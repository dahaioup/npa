package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// 分域 v2 核心（docs/architecture/npa-domain-v2.html）：
//
//   - 域 = 双向可达图的连通分量；同一子网内经 mDNS 互相发现的节点天然同域。
//   - 域内管理者按 (uptime, nodeID) 序确定性选举，交叉节点（同时属于其它域）
//     优先——它省一条域间长连接。各节点基于各自的视图独立计算，结果由心跳
//     收敛（非管理者向当选者发心跳；当选者收心跳即接管）。
//   - 活性 = 请求即活：成员每 heartbeatInterval 拉一次域状态（兼作心跳），
//     管理者 deadAfter 未见到即判死。管理者自身失联时，成员在本地重选——
//     缓存的成员表零信息损失。
//   - 域间增量：管理者间 push delta 为主（单调 (epoch,seq)），pull 兜底
//     （新邻域或 seq 断档时拉全量快照）。
//
// 鉴权（HMAC 签名）随权限体系统一重设计，当前所有端点无鉴权。

const (
	// heartbeatInterval 成员向管理者拉取域状态的周期（兼心跳）。
	heartbeatInterval = 20 * time.Second
	// deadAfter 管理者判死成员的静默阈值（3×heartbeatInterval）。
	deadAfter = 3 * heartbeatInterval
	// managerDeadAfter 成员判死管理者的静默阈值，超过即本地重选。
	managerDeadAfter = 3 * heartbeatInterval
	// maxDomainMembers 单域成员上限，防广播异常打爆内存。
	maxDomainMembers = 256
)

// DomainMember 域成员表条目。
type DomainMember struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"` // 可达入口 host:port
	Port    int    `json:"port"`
	Uptime  int64  `json:"uptime_seconds"`
	// CrossDomain 该成员同时是其它域的成员/管理者（选举优先）。
	CrossDomain bool   `json:"cross_domain"`
	LastSeen    int64  `json:"last_seen_unix"` // 管理者侧维护；成员本地视图里是 0
	Version     string `json:"version,omitempty"`
}

// DomainState 域全量快照（pull 兜底与成员同步共用）。
type DomainState struct {
	Epoch       uint64         `json:"epoch"`
	Seq         uint64         `json:"seq"`
	Domain      string         `json:"domain"` // 域标识 = 当选管理者 ID
	Manager     DomainMember   `json:"manager"`
	Members     []DomainMember `json:"members"`
	GeneratedAt int64          `json:"generated_at_unix"`
}

// DomainDelta 管理者间增量：单调 (epoch,seq)，跨域管理者转发给邻域。
type DomainDelta struct {
	From     string         `json:"from"`
	Epoch    uint64         `json:"epoch"`
	Seq      uint64         `json:"seq"`
	Upserts  []DomainMember `json:"upserts"`
	Removals []string       `json:"removals"`
}

// electionLess 确定性选举序：CrossDomain 优先，其次 uptime 大者，最后
// nodeID 字典序——所有成员对同一视图算出同一个管理者。
func electionLess(a, b DomainMember) bool {
	if a.CrossDomain != b.CrossDomain {
		return a.CrossDomain
	}
	if a.Uptime != b.Uptime {
		return a.Uptime > b.Uptime
	}
	return a.ID < b.ID
}

// electManager 从视图中选出管理者；空视图返回 false。
func electManager(members []DomainMember) (DomainMember, bool) {
	var best DomainMember
	found := false
	for _, m := range members {
		if !found || electionLess(m, best) {
			best, found = m, true
		}
	}
	return best, found
}

// domainEntry 管理者侧成员记录（含内部活性字段）。
type domainEntry struct {
	member DomainMember
	seen   time.Time
}

// DomainManager 域管理者角色：收心跳、维护成员表、判死、发增量。
// 每个节点都内嵌一个（任何人随时可被推举为管理者），零成本故障转移。
type DomainManager struct {
	mu      sync.Mutex
	self    DomainMember
	epoch   uint64
	seq     uint64
	members map[string]*domainEntry
	// deltas 转发目标：邻域管理者地址 -> 上次成功推送的 seq（0=下次全量）。
	neighbors map[string]uint64
	onChange  func(DomainState) // 状态变化回调（可空）
}

// NewDomainManager 创建管理者角色；self.Uptime/CrossDomain 由调用方维护。
func NewDomainManager(self DomainMember) *DomainManager {
	return &DomainManager{
		self:      self,
		members:   map[string]*domainEntry{},
		neighbors: map[string]uint64{},
	}
}

// SetOnChange 注册状态变化回调（增量推送的触发源）。
func (m *DomainManager) SetOnChange(fn func(DomainState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = fn
}

// Heartbeat 处理成员心跳：upsert 成员、推进 seq，返回当前快照。
// 地址为空的成员（仅经转述得知）不会被添加——域成员必须直接可达。
func (m *DomainManager) Heartbeat(member DomainMember) DomainState {
	m.mu.Lock()
	if member.Address == "" {
		m.mu.Unlock()
		return m.snapshotLocked()
	}
	if len(m.members) >= maxDomainMembers {
		if _, ok := m.members[member.ID]; !ok {
			m.mu.Unlock()
			return m.snapshotLocked()
		}
	}
	now := time.Now()
	member.LastSeen = now.Unix()
	prev, existed := m.members[member.ID]
	m.members[member.ID] = &domainEntry{member: member, seen: now}
	if !existed || memberChanged(prev.member, member) {
		m.seq++
		snapshot := m.snapshotLocked()
		onChange := m.onChange
		m.mu.Unlock()
		m.notifyChange(snapshot, onChange)
		return snapshot
	}
	snapshot := m.snapshotLocked()
	m.mu.Unlock()
	return snapshot
}

// Sweep 判死：deadAfter 没见过的成员移除并推进 seq。
func (m *DomainManager) Sweep() {
	m.mu.Lock()
	deadline := time.Now().Add(-deadAfter)
	removed := false
	for id, entry := range m.members {
		if entry.seen.Before(deadline) {
			delete(m.members, id)
			removed = true
		}
	}
	if !removed {
		m.mu.Unlock()
		return
	}
	m.seq++
	snapshot := m.snapshotLocked()
	onChange := m.onChange
	m.mu.Unlock()
	m.notifyChange(snapshot, onChange)
}

// StartSweeper 周期判死，直到 ctx 取消。
func (m *DomainManager) StartSweeper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.Sweep()
			}
		}
	}()
}

// State 当前全量快照。
func (m *DomainManager) State() DomainState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// ApplyDelta 应用邻域管理者推来的增量；seq 断档返回 false（调用方应转 pull）。
// 邻域成员以「转述」形态并入本域视图：Address 保留（代理拨号要用），
// 但 LastSeen 不参与本域判死。
func (m *DomainManager) ApplyDelta(delta DomainDelta) (DomainState, bool) {
	if !m.acceptDelta(delta) {
		return m.State(), false // 旧主复活防护
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if delta.Seq == 0 || delta.Seq <= m.seq && len(delta.Upserts) == 0 && len(delta.Removals) == 0 {
		return m.snapshotLocked(), true
	}
	if delta.Seq > m.seq+1 && m.seq != 0 {
		return m.snapshotLocked(), false // 断档：调用方 pull 全量
	}
	changed := false
	for _, up := range delta.Upserts {
		prev, existed := m.members[up.ID]
		up.LastSeen = 0 // 转述成员不带本域活性
		now := time.Now()
		m.members[up.ID] = &domainEntry{member: up, seen: now}
		if !existed || memberChanged(prev.member, up) {
			changed = true
		}
	}
	for _, id := range delta.Removals {
		if _, ok := m.members[id]; ok {
			delete(m.members, id)
			changed = true
		}
	}
	if changed {
		m.seq = delta.Seq
	}
	return m.snapshotLocked(), true
}

// SnapshotForPull 邻域 pull 兜底：返回全量快照。
func (m *DomainManager) SnapshotForPull() DomainState { return m.State() }

func (m *DomainManager) snapshotLocked() DomainState {
	state := DomainState{
		Epoch: m.epoch, Seq: m.seq,
		Manager: m.self, GeneratedAt: time.Now().Unix(),
	}
	for _, entry := range m.members {
		state.Members = append(state.Members, entry.member)
	}
	sort.Slice(state.Members, func(i, j int) bool { return state.Members[i].ID < state.Members[j].ID })
	if candidate, ok := electManager(append([]DomainMember{m.self}, state.Members...)); ok {
		state.Domain = candidate.ID
		state.Manager = candidate
	} else {
		state.Domain = m.self.ID
		state.Manager = m.self
	}
	return state
}

func (m *DomainManager) notifyChange(snapshot DomainState, fn func(DomainState)) {
	if fn != nil {
		fn(snapshot)
	}
}

// BumpEpoch 升格为新主时调用：epoch+1，使旧主复活后的 delta/state 被识别为过期。
func (m *DomainManager) BumpEpoch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.epoch++
}

// acceptDelta 旧主防护：delta 的 epoch 低于本域当前 epoch 即拒收
// （升格后本域 epoch+1，旧主复活的推送由此被识别为过期）。
func (m *DomainManager) acceptDelta(delta DomainDelta) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return delta.Epoch >= m.epoch
}

func memberChanged(a, b DomainMember) bool {
	return a.Address != b.Address || a.Port != b.Port || a.Name != b.Name ||
		a.Uptime != b.Uptime || a.CrossDomain != b.CrossDomain
}

// HandleHeartbeat HTTP 入口：POST /api/domain/heartbeat。
func (m *DomainManager) HandleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var member DomainMember
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&member); err != nil {
		http.Error(w, "bad heartbeat: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSONValue(w, m.Heartbeat(member))
}

// HandleState HTTP 入口：GET /api/domain/state（pull 兜底全量）。
func (m *DomainManager) HandleState(w http.ResponseWriter, r *http.Request) {
	writeJSONValue(w, m.State())
}

// HandleDelta HTTP 入口：POST /api/domain/delta（邻域管理者 push）。
func (m *DomainManager) HandleDelta(w http.ResponseWriter, r *http.Request) {
	var delta DomainDelta
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&delta); err != nil {
		http.Error(w, "bad delta: "+err.Error(), http.StatusBadRequest)
		return
	}
	state, ok := m.ApplyDelta(delta)
	if !ok {
		// seq 断档：让对方下次直接全量 pull
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"resync": true, "seq": state.Seq})
		return
	}
	writeJSONValue(w, state)
}

func writeJSONValue(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// DomainClient 成员角色：每周期向管理者发心跳并收域状态；
// 连续 managerDeadAfter 拿不到管理者响应即在本地接管（重选收敛）。
type DomainClient struct {
	mu       sync.Mutex
	self     func() DomainMember
	manager  string // 当前认定的管理者地址（host:port）
	lastOK   time.Time
	state    DomainState // 最近一次收到的域状态（本地重选的成员表来源）
	deadline time.Duration
}

// NewDomainClient 创建成员角色；self 返回本机成员条目（uptime 持续更新）。
func NewDomainClient(self func() DomainMember) *DomainClient {
	return &DomainClient{self: self, deadline: managerDeadAfter}
}

// SetManager 设置/更新管理者地址（mDNS 发现或配置注入）。
func (c *DomainClient) SetManager(address string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if address != "" && address != c.manager {
		c.manager = address
		c.lastOK = time.Now()
	}
}

// Manager 当前认定的管理者地址。
func (c *DomainClient) Manager() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.manager
}

// State 最近一次域状态。
func (c *DomainClient) State() DomainState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Tick 执行一次心跳拉取。管理者可达 → 更新状态并跟随快照里的当选者；
// 超过 deadline 拿不到响应 → 本地重选：视图 = 缓存成员 + 自己，
// 当选者是自己就升格为管理者（调 onPromoted），否则指向当选者地址。
func (c *DomainClient) Tick(ctx context.Context, onPromoted func(DomainState)) (DomainState, error) {
	c.mu.Lock()
	manager := c.manager
	lastOK := c.lastOK
	c.mu.Unlock()

	if manager != "" {
		if state, err := postHeartbeat(ctx, manager, c.self()); err == nil {
			c.mu.Lock()
			c.state = state
			c.lastOK = time.Now()
			// 跟随当选者（可能不是原管理者）；快照里当选者无地址则保持现值
			if state.Manager.Address != "" {
				c.manager = state.Manager.Address
			}
			c.mu.Unlock()
			return state, nil
		}
	}
	if manager != "" && time.Since(lastOK) < c.deadline {
		return c.State(), nil
	}
	// 本地重选：视图 = 缓存成员 + 自己，但排除刚失联的管理者——
	// 否则死管理者永远留在视图里，选举会反复指向它
	self := c.self()
	view := []DomainMember{self}
	c.mu.Lock()
	for _, m := range c.state.Members {
		if m.ID != self.ID && m.Address != "" && m.Address != manager {
			view = append(view, m)
		}
	}
	c.mu.Unlock()
	winner, ok := electManager(view)
	if !ok {
		return c.State(), nil
	}
	if winner.ID == self.ID {
		slog.Info("domain: promoted to manager after losing previous manager", "domain_member", self.ID)
		c.mu.Lock()
		c.manager = self.Address
		c.lastOK = time.Now()
		// 升格状态：以缓存视图构造本域快照（自己当选），供调用方播种其 DomainManager
		promoted := DomainState{
			Epoch: 1, Seq: 1, Domain: self.ID, Manager: self,
			GeneratedAt: time.Now().Unix(),
		}
		promoted.Members = append(promoted.Members, view...)
		sort.Slice(promoted.Members, func(i, j int) bool { return promoted.Members[i].ID < promoted.Members[j].ID })
		c.state = promoted
		c.mu.Unlock()
		if onPromoted != nil {
			onPromoted(promoted)
		}
		return promoted, nil
	}
	c.SetManager(winner.Address)
	return c.State(), nil
}

// Start 成员主循环：每 heartbeatInterval 一次 Tick，直到 ctx 取消。
func (c *DomainClient) Start(ctx context.Context, onPromoted func(DomainState)) {
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = c.Tick(ctx, onPromoted)
			}
		}
	}()
}

func postHeartbeat(ctx context.Context, manager string, self DomainMember) (DomainState, error) {
	var state DomainState
	target := "http://" + manager + "/api/domain/heartbeat"
	payload, err := json.Marshal(self)
	if err != nil {
		return state, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return state, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return state, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return state, io.ErrUnexpectedEOF
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&state)
	return state, err
}

// DomainHub 单节点的分域总成：管理者角色 + 成员角色并存（任何人随时可能
// 当选管理者，所以两个角色都常驻）。Register 挂载 HTTP 入口，Start 启动
// 判死清扫与成员心跳循环。
type DomainHub struct {
	Manager *DomainManager
	Client  *DomainClient
	// neighbors 返回邻域管理者候选（交叉成员所在域），delta push 的目标。
	neighbors func() []DomainMember
}

// NewDomainHub 创建总成；self 返回本机成员条目（uptime/CrossDomain 由调用方动态维护）。
func NewDomainHub(self func() DomainMember) *DomainHub {
	manager := NewDomainManager(self())
	client := NewDomainClient(self)
	return &DomainHub{Manager: manager, Client: client}
}

// Register 挂载分域 HTTP 入口（无鉴权；与 file/exec 同策略，权限后置统一重设计）。
func (h *DomainHub) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/domain/heartbeat", h.Manager.HandleHeartbeat)
	mux.HandleFunc("GET /api/domain/state", h.Manager.HandleState)
	mux.HandleFunc("POST /api/domain/delta", h.Manager.HandleDelta)
}

// Start 启动判死清扫与成员心跳循环，直到 ctx 取消。
func (h *DomainHub) Start(ctx context.Context) {
	h.Manager.StartSweeper(ctx)
	h.Client.Start(ctx, func(state DomainState) {
		h.Manager.BumpEpoch() // 升格即递增世代，旧主复活的推送从此被拒收
		for _, m := range state.Members {
			if m.ID != state.Manager.ID {
				h.Manager.Heartbeat(m)
			}
		}
	})
}

// Elect 管理者引导：对候选视图（mDNS 发现的同网段节点）与本机做一次确定性
// 选举。当选者是自己就原地待命（别人会来心跳）；否则把成员角色的心跳指向
// 当选者。可周期调用，视图变化自动跟随。
func (h *DomainHub) Elect(candidates []DomainMember) {
	self := h.Client.self()
	view := make([]DomainMember, 0, len(candidates)+1)
	for _, c := range candidates {
		if c.ID != self.ID && c.Address != "" {
			view = append(view, c)
		}
	}
	view = append(view, self)
	winner, ok := electManager(view)
	if !ok {
		return
	}
	if winner.ID != self.ID {
		h.Client.SetManager(winner.Address)
	}
}

// ---------- 管理者间增量同步（push 为主，pull 兜底） ----------

// pushTimeout 单次向邻域管理者推送/拉取的超时。
const pushTimeout = 3 * time.Second

// SetNeighbors 注入邻域管理者候选（交叉成员所在域的管理者）；变化回调时
// 逐个 push delta，收到 409（seq 断档）就退化为全量 pull。
func (h *DomainHub) SetNeighbors(fn func() []DomainMember) { h.neighbors = fn }

// StartDeltaPush 把管理者状态变化接到邻域推送；独立于 Start 便于单独测试。
func (h *DomainHub) StartDeltaPush() {
	h.Manager.SetOnChange(func(state DomainState) {
		if h.neighbors == nil {
			return
		}
		for _, peer := range h.neighbors() {
			if peer.Address == "" || peer.Address == h.Manager.State().Manager.Address {
				continue
			}
			h.pushDeltaTo(peer, state)
		}
	})
}

// pushDeltaTo 向一个邻域管理者推送增量；409 → 全量 pull 并本地吸收。
func (h *DomainHub) pushDeltaTo(peerDomain DomainMember, state DomainState) {
	delta := DomainDelta{
		From:  state.Manager.ID,
		Epoch: state.Epoch,
		Seq:   state.Seq,
		Upserts: append([]DomainMember{state.Manager},
			state.Members...),
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+peerDomain.Address+"/api/domain/delta", bytes.NewReader(mustJSON(delta)))
	if err != nil {
		return
	}
	client := &http.Client{Timeout: pushTimeout}
	response, err := client.Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		// seq 断档：pull 全量快照兜底
		if snapReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://"+peerDomain.Address+"/api/domain/state", nil); err == nil {
			if snapResp, err := client.Do(snapReq); err == nil {
				var snapshot DomainState
				if json.NewDecoder(io.LimitReader(snapResp.Body, 4<<20)).Decode(&snapshot) == nil {
					h.Manager.ApplySnapshot(snapshot)
				}
				snapResp.Body.Close()
			}
		}
	}
}

// ApplySnapshot 邻域 pull 兜底：整表吸收（seq 直接对齐，不再校验连续性）。
func (m *DomainManager) ApplySnapshot(snapshot DomainState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, up := range snapshot.Members {
		up.LastSeen = 0
		m.members[up.ID] = &domainEntry{member: up, seen: time.Now()}
	}
	if snapshot.Seq > m.seq {
		m.seq = snapshot.Seq
	}
}

func mustJSON(value any) []byte {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return payload
}
