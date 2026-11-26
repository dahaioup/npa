package peer

// 拓扑增量同步（docs/plans/topology-sync.md，10 条规则的实现）：
//
//   - 管理者维护 view（全量节点 + 单调 version），节点每 30s 拉增量（兼心跳）；
//   - 90s 未见即 offline，24h 未见即物理剔除（墓碑经 removed 列表同步下级）；
//   - 域间：管理者每 30s 向上级/邻域批量 push（空批次 = 活性），push_id 幂等，
//     失败时响应带接收方实际 version，推送方据此重算增量；
//   - version 本地生成（max+1），不依赖 NTP 对齐（第 34 项）。

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// TopoPullInterval 节点向管理者拉增量的周期（兼心跳）。
	TopoPullInterval = 30 * time.Second
	// TopoManagerFailLimit 连续拉取管理者失败多少次视为管理节点丢失（规则 6 重选）。
	TopoManagerFailLimit = 3
	// TopoOfflineAfter 管理者判节点离线的静默阈值（3×pull）。
	TopoOfflineAfter = 90 * time.Second
	// TopoTombstoneAfter offline 超过该时长物理剔除（用户定案 24h）。
	TopoTombstoneAfter = 24 * time.Hour
	// topoMaxPushIDs push_id 幂等窗口（第 37 项）。
	topoMaxPushIDs = 100
	// topoPageSize 全量分页阈值（第 40 项：只留接口，现网不触发）。
	topoPageSize = 500
	// TopoPushInterval 管理者向上级/邻域批量推送的周期（空批次也推，规则 7）。
	TopoPushInterval = 30 * time.Second
	// topoPushTimeout 单次推送超时。
	topoPushTimeout = 5 * time.Second
)

// TopoNode 视图节点（规则 1）。复用 protocol.go 的 TopologyNode 太重
// （children 递归/调试字段），这里定义扁平增量专用形态。
type TopoNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Online  bool   `json:"online"`
	// Holder 直连持有该节点的 hub 的 mesh 地址（注册下级=管理者自身地址，
	// 下级域节点=该域管理者的推送地址）。跨域拨号按它找代理跳板。
	Holder     string `json:"holder,omitempty"`
	Parent     string `json:"parent,omitempty"`     // 域内直连上级；空=直连管理者
	DomainID   string `json:"domain_id,omitempty"`  // 所属域 = 管理者
	Discovered bool   `json:"discovered,omitempty"` // mDNS 同域可见（仅投影时填充） ID
	Version    int64  `json:"version"`              // 该记录最后变更版本（域记录序号，非 NPA 版本）
	// NodeVersion 是该节点的 NPA 版本号（对端自报），与上面的域记录序号严格分开。
	NodeVersion string   `json:"node_version,omitempty"`
	Source      string   `json:"source,omitempty"` // self | member | domain（下级域转述）
	Subnets     []string `json:"subnets,omitempty"`
	// IsManager：该节点是【本节点所属域】的管理节点。由本机出参时现算，
	// 绝不继承对端 payload 里的值——那是对方域里的身份，陈旧后会跨节点传播。
	IsManager bool `json:"is_manager,omitempty"`
	// ManagerVersion：本域管理节点的域数据版本（view.version），供徽章显示 V?。
	ManagerVersion int64 `json:"manager_version,omitempty"`
	// DomainManager：该节点是【它自己那个下级域】的管理节点（它把整域注册/
	// 推上来了：source=domain 且 domain_id == 自身 ID）。与 IsManager 分开，
	// 避免把「下级域的管理者」和「本域的管理者」混成一个标签。
	DomainManager bool `json:"domain_manager,omitempty"`
}

// TopologySyncResponse GET /api/topology/sync 的响应（规则 3/6）。
// NotManager=true 表示应答方已不是管理节点（已降级为普通节点）：
// 此时 Nodes 为空，ManagerID/ManagerAddr 告知请求方改指新管理者（规则 6）。
type TopologySyncResponse struct {
	Version     int64      `json:"version"`
	Full        bool       `json:"full"`
	Nodes       []TopoNode `json:"nodes"`
	Removed     []string   `json:"removed,omitempty"`
	Page        int        `json:"page,omitempty"`
	PageTotal   int        `json:"page_total,omitempty"`
	NotManager  bool       `json:"not_manager,omitempty"`
	ManagerID   string     `json:"manager_id,omitempty"`
	ManagerAddr string     `json:"manager_addr,omitempty"`
	// IDs 是应答方当前持有的全部节点 ID（不含自己）——反熵清单。
	// 请求方拿它和本地视图比对：发现对方有、自己没有的节点 → 说明
	// 增量窗口把它漏掉了（版本空间错位），下次拉取强制全量补齐。
	IDs []string `json:"ids,omitempty"`
}

// TopologyPush POST /api/topology/push 的请求（规则 7）。
type TopologyPush struct {
	PushID string `json:"push_id"`
	FromID string `json:"from_id"`
	// Pusher 推送方 mesh 地址（ Holder 判定依据）。空则取 HTTP 来源地址。
	Pusher  string `json:"pusher,omitempty"`
	Version int64  `json:"version"` // 推送方当前版本（可观测用）
	// BaseVersion 是推送方认为接收方已经拥有的版本（= 上次推送成功后本方的版本）。
	// 接收方比对不上就说明中间漏过消息（推送方重启/接收方重启）→ 回 OK=false，
	// 推送方按全量重推，避免增量基线错位导致永远对不齐。
	BaseVersion int64      `json:"base_version,omitempty"`
	Nodes       []TopoNode `json:"nodes"`
	Removed     []string   `json:"removed,omitempty"`
}

// TopologyPushResponse 推送响应；OK=false 时 Version=接收方实际版本，
// 推送方按它重算增量（规则 7/8）。
type TopologyPushResponse struct {
	OK      bool  `json:"ok"`
	Version int64 `json:"version"`
	Dup     bool  `json:"dup,omitempty"`
}

type topoRecord struct {
	node      TopoNode
	ver       int64
	seen      time.Time
	offlineAt time.Time // zero = online
	// owned=true 表示这条记录属于本域自有内容（注册下级、下级域转述）；
	// owned=false 表示是从上级拉来的副本，只用于展示，绝不原样推回上级
	// （否则形成回环：上级→本域→上级）。
	owned bool
}

// tombstone 物理剔除记录：id → 剔除发生的版本（下级按 since 拉增量时补发）。
type tombstone struct {
	id  string
	ver int64
	at  time.Time
}

// clearTombstoneLocked 丢掉某节点的墓碑记录。
//
// 节点重新出现在视图里（重新注册、重新推送、成员触达）时，它之前的墓碑
// 就过期了——墓碑仍会被应答方带进 Removed 列表，接收端按它把刚收到的
// 节点立刻删掉，于是该节点永远补不回来（版本空间错位只让它更难重现）。
func (v *TopologyView) clearTombstoneLocked(id string) {
	if id == "" || len(v.tombs) == 0 {
		return
	}
	kept := v.tombs[:0]
	for _, t := range v.tombs {
		if t.id != id {
			kept = append(kept, t)
		}
	}
	v.tombs = kept
}

// TopologyView 管理者/副本共用的拓扑视图存储。管理者侧权威；成员侧作为
// 同步副本（manager 模式=false，ApplySync 整表替换）。
type TopologyView struct {
	// ManagerInfo 返回当前当选管理者（空串=未选出）。nil 时视应答方恒为管理者。
	ManagerInfo func() (string, string)
	mu          sync.RWMutex
	selfID      string
	selfAddr    string // 本机 mesh 地址（构建 Holder 用），service 注入
	version     int64
	nodes       map[string]*topoRecord
	tombs       []tombstone
	pushIDs     map[string]time.Time
	// pushers 下级域 ID → 最近一次推送来源地址（Holder 的来源）。
	pushers map[string]string
	// lastPushAck 邻域/上级地址 → 上次成功推送时本方的版本（规则 7 的增量基线）。
	// 记本方版本：推给上级的内容按本方的记录版本号比较。
	lastPushAck map[string]int64
	// pushAcks 推送方 ID → 本方已确认收到的该推送方版本（检测增量断档）。
	pushAcks map[string]int64
	// failCount 邻域地址 → 连续推送失败次数（规则 8：3 次触发重注册）。
	failCount map[string]int
	// selfIsManager 本机当前是否本域管理节点（出参打标 self 记录用）。
	selfIsManager bool
	onChange      func() // 变更回调（触发批量推送等），可空
}

// NewTopologyView 创建视图。
func NewTopologyView(selfID string) *TopologyView {
	return &TopologyView{
		selfID:      selfID,
		nodes:       map[string]*topoRecord{},
		pushIDs:     map[string]time.Time{},
		pushers:     map[string]string{},
		lastPushAck: map[string]int64{},
		pushAcks:    map[string]int64{},
		failCount:   map[string]int{},
	}
}

// SetSelfAddress 注入本机 mesh 地址（本机直连持有节点时的 Holder）。
func (v *TopologyView) SetSelfAddress(address string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.selfAddr = address
}

// SetSelfIsManager 记录本机的管理节点身份（每 tick 由 service 调用）。
// 身份变化时 bump self 记录——既让出参打标立即生效，也把变更推给邻居。
func (v *TopologyView) SetSelfIsManager(mgr bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.selfIsManager == mgr {
		return
	}
	v.selfIsManager = mgr
	if rec, ok := v.nodes[v.selfID]; ok {
		if rec.node.IsManager != mgr {
			rec.node.IsManager = mgr
			rec.node.Version = v.bumpLocked()
			rec.ver = rec.node.Version
			v.fireLocked()
		}
	}
}

// stampManager 出参打标：**只按本机掌握的事实**计算管理身份，
// 绝不继承对端 payload 里的 is_manager（那是对方域里的身份，陈旧后会
// 跨节点传播——例如某节点早已不是管理者，旧标记却被上级原样继承，
// 最后每个节点都显示「管理」）。
//
// 判定：
//   - 本机记录 → 本机现算身份（selfIsManager）；
//   - 本域管理节点（ManagerInfo 给出的 mid）→ IsManager=true；
//   - 下级域的管理节点（它把整域注册/推上来了：source=domain 且
//     domain_id == 自身 ID）→ DomainManager=true（它确实是「它自己那个域」
//     的管理者，但那是下级域的身份，不能冒充本域的管理者）。
func (v *TopologyView) stampManager(node *TopoNode) {
	node.IsManager = false
	node.ManagerVersion = 0
	node.DomainManager = false
	if node.ID == v.selfID {
		node.IsManager = v.selfIsManager
		if v.selfIsManager {
			node.ManagerVersion = v.version
		}
		return
	}
	if v.ManagerInfo != nil {
		if mid, _ := v.ManagerInfo(); mid != "" && node.ID == mid {
			node.IsManager = true
			node.ManagerVersion = v.version
		}
	}
	if node.Source == "domain" && node.DomainID != "" && node.DomainID == node.ID {
		// 下级域的管理节点：它以整域身份注册到上级域管理者
		node.DomainManager = true
	}
}

// SelfID 本机节点 ID。
func (v *TopologyView) SelfID() string { return v.selfID }

// SelfAddress 本机 mesh 地址（未注入时为空）。
func (v *TopologyView) SelfAddress() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.selfAddr
}

// NodeByID 按节点 ID 查视图副本。
func (v *TopologyView) NodeByID(id string) (TopoNode, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if rec, ok := v.nodes[id]; ok {
		return rec.node, true
	}
	return TopoNode{}, false
}

// MissingIDs 返回「对方有、本地没有」的节点 ID（反熵差集）。
//
// 用途：对方应答里带 ID 清单（TopologySyncResponse.IDs），本地比对后若发现
// 缺口，说明增量窗口把这个节点漏掉了——两套版本空间不同源（对方按
// per-record 版本过滤，请求方却拿对方的全局版本当游标），缺口一旦形成
// 就永远补不回来。调用方据此把游标清零，强制下一次全量拉取。
func (v *TopologyView) MissingIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	var missing []string
	for _, id := range ids {
		if id == "" || id == v.selfID {
			continue
		}
		if _, ok := v.nodes[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}

// PusherOf 返回下级域最近一次推送来源地址；无记录返回空。
func (v *TopologyView) PusherOf(domainID string) string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.pushers[domainID]
}

// SetOnChange 注册变更回调（批量推送的触发源之一）。
func (v *TopologyView) SetOnChange(fn func()) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.onChange = fn
}

func (v *TopologyView) bumpLocked() int64 {
	v.version++
	return v.version
}

// Upsert 管理者侧登记/刷新节点（注册、心跳、下级域推送共用）。
// online 由调用方给（心跳/注册=在线）；地址为空的转述节点不接受（与域成员
// 同规则：必须直接可达才能进视图）。
func (v *TopologyView) Upsert(node TopoNode, online bool) bool {
	if node.ID == "" || node.Address == "" {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	node.Online = online
	if !online && node.Source == "" {
		node.Source = "domain"
	}
	prev, existed := v.nodes[node.ID]
	wasOnline := existed && prev.offlineAt.IsZero()
	if existed {
		// 合并保留归属：成员心跳（注册/心跳自报只有 id/addr）不能把域推送来的
		// DomainID/Parent 冲掉，否则归属在节点间来回改写最终成环。
		if node.DomainID == "" && prev.node.DomainID != "" {
			node.DomainID = prev.node.DomainID
		}
		if node.Parent == "" && prev.node.Parent != "" {
			node.Parent = prev.node.Parent
		}
		if node.Holder == "" && prev.node.Holder != "" {
			node.Holder = prev.node.Holder
		}
		if node.NodeVersion == "" && prev.node.NodeVersion != "" {
			node.NodeVersion = prev.node.NodeVersion
		}
	}
	changed := !existed ||
		topoNodeChanged(prev.node, node) ||
		wasOnline != online
	if changed {
		node.Version = v.bumpLocked()
	} else {
		// 内容没变（重复注册/心跳刷新）：沿用原版本号，不虚增
		node.Version = prev.node.Version
	}
	// owned=true：本域自有内容（注册下级 + 本机 self）——属于「本域」，
	// 可以继续向上级推送。
	v.nodes[node.ID] = &topoRecord{node: node, ver: node.Version, seen: now, owned: true}
	// 节点回来了 → 它的旧墓碑作废（否则应答方仍会把它写进 Removed，
	// 接收端会当场删掉刚收到的这条记录）。
	v.clearTombstoneLocked(node.ID)
	if !online {
		if existed {
			v.nodes[node.ID].offlineAt = prev.offlineAt
		} else {
			v.nodes[node.ID].offlineAt = now
		}
	}
	if changed {
		v.fireLocked()
	}
	return changed
}

// MarkSeen 心跳触达：刷新 seen，在线状态翻转才 bump（30s 心跳不产生增量噪音）。
func (v *TopologyView) MarkSeen(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	rec, ok := v.nodes[id]
	if !ok {
		return
	}
	rec.seen = time.Now()
	if !rec.offlineAt.IsZero() {
		rec.offlineAt = time.Time{}
		rec.node.Online = true
		rec.node.Version = v.bumpLocked()
		rec.ver = rec.node.Version
		v.fireLocked()
	}
}

// TouchMember 成员 pull 触达（请求即活，规则 6）：刷新存活，并补三件事——
//   - 父指针：同域拉取（attach=false）与管理节点**同级**——继承管理节点自己
//     的父指针（管理节点是根则成员也是根）；跨域拉取（attach=true，如注册下级
//     拉上级）挂到本节点名下。域内平级、域间挂靠全靠这条规则。
//   - 来源纠偏：摄入时抄进来的外来 source="self" 改回 member；
//   - 收编：副本转为本域自有（owned），管理节点从此对它的存活负责。
//
// 内容有变化才 bump（30s 一次的纯心跳不产生增量噪音）。
func (v *TopologyView) TouchMember(id, addr string, attach bool) {
	if id == "" || id == v.selfID {
		return
	}
	// 目标父指针：attach → 挂本节点；否则与本节点同级（继承本节点的父指针）。
	wantParent := ""
	if attach {
		wantParent = v.selfID
	} else if self, ok := v.nodes[v.selfID]; ok {
		wantParent = self.node.Parent
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	rec, existed := v.nodes[id]
	if !existed {
		if addr == "" {
			return // 地址为空的转述节点不接受（与 Upsert 同规则）
		}
		node := TopoNode{ID: id, Address: addr, Parent: wantParent, Source: "member"}
		node.Version = v.bumpLocked()
		node.Online = true
		v.nodes[id] = &topoRecord{node: node, ver: node.Version, seen: now, owned: true}
		v.clearTombstoneLocked(id)
		v.fireLocked()
		return
	}
	changed := false
	rec.seen = now
	if !rec.offlineAt.IsZero() {
		rec.offlineAt = time.Time{}
		rec.node.Online = true
		changed = true
	}
	if rec.node.Parent != wantParent {
		rec.node.Parent = wantParent
		changed = true
	}
	if rec.node.Source == "self" {
		rec.node.Source = "member"
		changed = true
	}
	if addr != "" && rec.node.Address != addr {
		rec.node.Address = addr
		changed = true
	}
	rec.owned = true // 收编：本地标记，不进 wire 内容，无需 bump
	if changed {
		rec.node.Version = v.bumpLocked()
		rec.ver = rec.node.Version
		v.fireLocked()
	}
}

// ClaimAll 升为管理节点时收编视图：把从管理节点拉来的副本转为本域自有，
// 此后本机对本域成员的存活负责（Sweep 判死 + 增量下发）。owned 是本地
// 标记不进 JSON，无需 bump。
func (v *TopologyView) ClaimAll() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for id, rec := range v.nodes {
		if id == v.selfID || rec.owned {
			continue
		}
		rec.owned = true
	}
}

// Sweep 判死 + 墓碑（规则 3 离线判定 / 规则 9 剔除）。
// 返回本次物理剔除的节点 ID。
func (v *TopologyView) Sweep() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	offlineDeadline := now.Add(-TopoOfflineAfter)
	tombDeadline := now.Add(-TopoTombstoneAfter)
	var removed []string
	changed := false
	for id, rec := range v.nodes {
		if id == v.selfID {
			continue // 本机恒在线，不参与判死
		}
		if !rec.owned {
			// 从管理节点拉来的副本：存活由管理节点判定、经增量/全量下发，
			// 本地按 seen 判死会把仍活着的成员误标离线（30s 拉取不带增量时）。
			continue
		}
		if rec.offlineAt.IsZero() {
			if rec.seen.Before(offlineDeadline) {
				rec.offlineAt = now
				rec.node.Online = false
				rec.node.Version = v.bumpLocked()
				rec.ver = rec.node.Version
				changed = true
			}
			continue
		}
		if rec.offlineAt.Before(tombDeadline) {
			ver := v.bumpLocked()
			v.tombs = append(v.tombs, tombstone{id: id, ver: ver, at: now})
			delete(v.nodes, id)
			removed = append(removed, id)
			changed = true
		}
	}
	// 墓碑自身 7 天后清理（下级早已追平）
	kept := v.tombs[:0]
	for _, t := range v.tombs {
		if t.at.After(now.Add(-7 * 24 * time.Hour)) {
			kept = append(kept, t)
		}
	}
	v.tombs = kept
	if changed {
		v.fireLocked()
	}
	return removed
}

func (v *TopologyView) fireLocked() {
	if v.onChange != nil {
		fn := v.onChange
		v.mu.Unlock()
		fn()
		v.mu.Lock()
	}
}

// Delta 计算 since 之后的增量；since 无效（0 / 断档 / 管理者重启归零）→ 全量。
func (v *TopologyView) Delta(since int64) TopologySyncResponse {
	v.mu.RLock()
	defer v.mu.RUnlock()
	full := since <= 0 || since > v.version
	resp := TopologySyncResponse{Version: v.version, Full: full}
	if full {
		since = 0
	}
	for _, rec := range v.nodes {
		if full || rec.ver > since {
			node := rec.node
			if node.ID != v.selfID && node.Holder == "" {
				if node.Source == "domain" {
					node.Holder = v.pushers[node.DomainID]
				} else {
					node.Holder = v.selfAddr
				}
			}
			v.stampManager(&node)
			resp.Nodes = append(resp.Nodes, node)
		}
	}
	for _, t := range v.tombs {
		if _, alive := v.nodes[t.id]; alive {
			// 节点已回到视图里 → 这条墓碑过期（否则同一份应答既带它又删它）
			continue
		}
		if full || t.ver > since {
			resp.Removed = append(resp.Removed, t.id)
		}
	}
	// 反熵清单：应答方持有的全部节点 ID（不含自己）。请求方拿它和本地视图
	// 比对——发现对方有、自己没有的节点，说明增量窗口把它漏了（版本空间
	// 错位：记录的 per-record 版本可能低于请求方已推进的游标），下次拉取
	// 必须强制全量才能补齐。
	for id := range v.nodes {
		if id != v.selfID {
			resp.IDs = append(resp.IDs, id)
		}
	}
	sort.Strings(resp.IDs)
	sortNodesByID(resp.Nodes)
	// 分页（第 40 项）：只有全量才可能超限；增量天然小
	if len(resp.Nodes) > topoPageSize {
		resp.PageTotal = (len(resp.Nodes) + topoPageSize - 1) / topoPageSize
		resp.Page = 1
		resp.Nodes = resp.Nodes[:topoPageSize]
	}
	if resp.Nodes == nil {
		resp.Nodes = []TopoNode{}
	}
	return resp
}

// ApplySync 把上级的响应并入本地副本。
//
// owned 语义（分域拓扑的归属规则）：
//   - owned=true  本域自有内容（注册下级、本机 self、下级域转述）——权威，可向上级推送；
//   - owned=false 从上级拉来的副本——只用于展示，绝不原样推回上级（否则成环）。
//
// preserveOwned=true（管理节点向上级拉取）：全量只清副本、合并不覆盖本域自有
// 内容——否则管理节点每向上级拉一次就把自己的域表清空。
func (v *TopologyView) ApplySync(resp TopologySyncResponse) {
	v.applySync(resp, true)
}

// ApplySyncFromManager 把管理者的响应并入本地视图（成员侧拉取）。
// 管理者对本域成员是权威：本域自有旧记录（如让位前当管理者时留下的）
// 也允许被覆盖/清空，否则前管理者永远留着陈旧的 parent/online。
func (v *TopologyView) ApplySyncFromManager(resp TopologySyncResponse) {
	v.applySync(resp, false)
}

func (v *TopologyView) applySync(resp TopologySyncResponse, preserveOwned bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if resp.Full {
		// 差量式全量：只删「对端已经没有」的记录，payload 里有的走下面的
		// 合并逻辑原地更新——内容没变就不 bump。否则先清表再逐条重加，
		// 每轮全量都把整张表重盖一遍版本号（单节点域版本虚增的主因）。
		keep := make(map[string]bool, len(resp.Nodes))
		for _, node := range resp.Nodes {
			if node.ID != "" {
				keep[node.ID] = true
			}
		}
		for id, rec := range v.nodes {
			if id == v.selfID || keep[id] {
				continue
			}
			if preserveOwned && rec.owned {
				continue
			}
			delete(v.nodes, id)
		}
		v.tombs = nil
	}
	for _, node := range resp.Nodes {
		if node.ID == "" {
			continue
		}
		if node.ID == v.selfID {
			// 本机记录不整条覆盖，但采纳对端给的父指针——它描述「本机挂在哪」
			// （成员拉管理者=与管理节点同级；管理节点拉上级=挂上级），
			// 本机成员的父指针继承链条从这里开始。
			if rec, ok := v.nodes[v.selfID]; ok && node.Parent != rec.node.Parent {
				rec.node.Parent = node.Parent
				rec.node.Version = v.bumpLocked()
				rec.ver = rec.node.Version
				v.fireLocked()
			}
			continue
		}
		// 对端 payload 里的 self 是「对方自己」，对本机只是普通节点——
		// 不改写的话，前管理者让位后视图里会留下外来 src=self 的脏记录。
		if node.Source == "self" {
			node.Source = "member"
		}
		// 管理身份一律由本机出参时现算（见 stampManager），合并时清掉
		// 对端带来的标记——否则陈旧标记会被当成内容变化反复 bump，
		// 且跨节点传播（某节点早已不是管理者却仍显示「管理」）。
		node.IsManager = false
		node.ManagerVersion = 0
		node.DomainManager = false
		prev, existed := v.nodes[node.ID]
		if preserveOwned && existed && prev.owned {
			// 本域自有内容优先：上级副本不覆盖（否则归属在上/下级之间来回改写）
			continue
		}
		if existed && !topoNodeChanged(prev.node, node) {
			// 内容没变：只刷新存活时间，不产生增量（避免每 30s 拉一次就全表重发）
			prev.seen = now
			if node.Online {
				prev.offlineAt = time.Time{}
			} else if prev.offlineAt.IsZero() {
				prev.offlineAt = now
			}
			continue
		}
		// 用本地版本号重新盖戳：副本进入本节点的版本空间，增量基线才可比
		node.Version = v.bumpLocked()
		rec := &topoRecord{node: node, ver: node.Version, seen: now, owned: false}
		if !node.Online {
			rec.offlineAt = now
		}
		v.nodes[node.ID] = rec
		// 这次应答里又带回了它 → 之前把它判死的墓碑作废
		v.clearTombstoneLocked(node.ID)
	}
	// Removed 是应答方的墓碑列表；但同一份应答的 payload 里如果又带着这个
	// 节点，说明它已经复活（应答方的墓碑没跟着清理）——payload 是当前事实，
	// 不能被同一份应答里的旧墓碑当场删掉，否则该节点永远补不回来。
	payload := make(map[string]bool, len(resp.Nodes))
	for _, node := range resp.Nodes {
		payload[node.ID] = true
	}
	for _, id := range resp.Removed {
		rec, ok := v.nodes[id]
		if !ok || id == v.selfID {
			continue
		}
		if payload[id] {
			continue // 本次应答仍带着它 → 不删
		}
		if preserveOwned && rec.owned {
			continue
		}
		delete(v.nodes, id)
	}
	// 注意：不把 resp.Version 并入本地版本号——本地版本号只服务本地记录，
	// 拉取基线（since）由调用方按「对方版本空间」单独跟踪（见 service 的游标）。
}

// topoNodeChanged 比较两条拓扑记录的内容是否变化（版本号不比）。
func topoNodeChanged(a, b TopoNode) bool {
	return a.Address != b.Address ||
		a.Name != b.Name ||
		a.Online != b.Online ||
		a.Parent != b.Parent ||
		a.DomainID != b.DomainID ||
		a.Holder != b.Holder ||
		a.Source != b.Source ||
		a.NodeVersion != b.NodeVersion ||
		a.IsManager != b.IsManager ||
		a.ManagerVersion != b.ManagerVersion
}

// localVersion 成员侧当前已同步到的版本。
func (v *TopologyView) localVersion() int64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.version
}

// ApplyPush 管理者侧接收下级域推送（规则 7）：push_id 幂等；合并节点；
// removed 物理删。pusher 为推送来源 mesh 地址（Holder 判定依据）。
// 返回响应（失败时 Version=当前实际版本供对方重算）。
func (v *TopologyView) ApplyPush(push TopologyPush, pusher string) TopologyPushResponse {
	v.mu.Lock()
	defer v.mu.Unlock()
	resp := TopologyPushResponse{Version: v.version}
	// 幂等（第 37 项）：最近 topoMaxPushIDs 个 push_id 命中即返回成功
	if push.PushID != "" {
		if _, dup := v.pushIDs[push.PushID]; dup {
			resp.OK, resp.Dup = true, true
			return resp
		}
		v.pushIDs[push.PushID] = time.Now()
		if len(v.pushIDs) > topoMaxPushIDs {
			oldest, oldestAt := "", time.Now().Add(time.Hour)
			for id, at := range v.pushIDs {
				if at.Before(oldestAt) {
					oldest, oldestAt = id, at
				}
			}
			delete(v.pushIDs, oldest)
		}
	}
	now := time.Now()
	changed := false
	// 增量断档检测：推送方自报「我认为你已有的版本」（BaseVersion），
	// 与本方记录的不一致说明中间漏过消息 → 回 OK=false 让对方按全量重推。
	if push.BaseVersion > 0 {
		if ack, ok := v.pushAcks[push.FromID]; ok && ack != push.BaseVersion {
			resp.OK = false
			resp.Version = v.version
			return resp
		}
	}
	if pusher = strings.TrimSpace(pusher); pusher != "" {
		v.pushers[push.FromID] = pusher
	} else if v.pushers[push.FromID] == "" {
		// 无显式来源时退化为第一个节点的地址（同一域总有一个可达地址）
		for _, node := range push.Nodes {
			if node.Address != "" {
				v.pushers[push.FromID] = node.Address
				break
			}
		}
	}
	// 推送者自身的 ID：按 pusher 地址匹配已有成员记录，作为下级域节点的 Parent

	for _, node := range push.Nodes {
		if node.ID == "" || node.ID == v.selfID {
			continue
		}
		// 归属只补不覆盖：已有 DomainID/Parent 时保留，避免互推时归属来回改写成环。
		prev, existed := v.nodes[node.ID]
		if node.DomainID == "" {
			if existed && prev.node.DomainID != "" {
				node.DomainID = prev.node.DomainID
			} else {
				node.DomainID = push.FromID
			}
		}
		node.Source = "domain"
		// 管理身份由出参现算，合并时清掉对端标记（见 stampManager）。
		node.IsManager = false
		node.ManagerVersion = 0
		node.DomainManager = false
		if node.Holder == "" {
			node.Holder = v.pushers[push.FromID]
		}
		if node.Parent == "" {
			if existed && prev.node.Parent != "" {
				node.Parent = prev.node.Parent
			} else {
				// 下级域推送的节点挂在接收域管理者下（NAT 场景 pusherID 匹配不到也成立）
				node.Parent = v.selfID
			}
		}
		if existed && !topoNodeChanged(prev.node, node) {
			// 内容没变：沿用原版本号，只刷新存活（重复推送不虚增版本号）
			node.Version = prev.node.Version
		} else {
			node.Version = v.bumpLocked()
			changed = true
		}
		// owned=true：下级域转述内容属于本域子树，可继续向上级推送
		rec := &topoRecord{node: node, ver: node.Version, seen: now, owned: true}
		if !node.Online {
			if existed && !prev.offlineAt.IsZero() {
				rec.offlineAt = prev.offlineAt // 保留原判死起点，不因重复推送重置
			} else {
				rec.offlineAt = now
			}
		}
		v.nodes[node.ID] = rec
		// 节点回来了 → 旧墓碑作废
		v.clearTombstoneLocked(node.ID)
	}
	// 同一次推送里 payload 与 removed 撞车时，payload 是当前事实。
	pushIDs := make(map[string]bool, len(push.Nodes))
	for _, node := range push.Nodes {
		pushIDs[node.ID] = true
	}
	for _, id := range push.Removed {
		if id == v.selfID {
			continue
		}
		if pushIDs[id] {
			continue // 本次推送仍带着它 → 不删
		}
		if _, ok := v.nodes[id]; ok {
			ver := v.bumpLocked()
			v.tombs = append(v.tombs, tombstone{id: id, ver: ver, at: now})
			delete(v.nodes, id)
			changed = true
		}
	}
	if changed {
		v.fireLocked()
	}
	// 记录本方已确认收到的该推送方版本（下次推送方带 BaseVersion 来比对）
	if push.Version > 0 {
		v.pushAcks[push.FromID] = push.Version
	}
	resp.Version = v.version
	resp.OK = true
	return resp
}

// DeltaOwned 只算「本域自有内容」（注册下级 + 本机 self + 下级域转述）的增量。
//
// 管理节点向上级推送时用它：推给上级的内容 = 本域 + 本域的所有下级域，
// 从上级拉来的副本（owned=false）绝不原样推回去，否则形成回环
// 上级 → 本域 → 上级。
func (v *TopologyView) DeltaOwned(since int64) TopologySyncResponse {
	v.mu.RLock()
	defer v.mu.RUnlock()
	full := since <= 0 || since > v.version
	resp := TopologySyncResponse{Version: v.version, Full: full}
	if full {
		since = 0
	}
	for _, rec := range v.nodes {
		if !rec.owned {
			continue
		}
		if full || rec.ver > since {
			node := rec.node
			if node.ID != v.selfID && node.Holder == "" {
				if node.Source == "domain" {
					node.Holder = v.pushers[node.DomainID]
				} else {
					node.Holder = v.selfAddr
				}
			}
			v.stampManager(&node)
			resp.Nodes = append(resp.Nodes, node)
		}
	}
	sortNodesByID(resp.Nodes)
	if resp.Nodes == nil {
		resp.Nodes = []TopoNode{}
	}
	return resp
}

// PushBatchTo 向一个上级/邻域管理者推送增量（规则 7/8）。
// 成功：记录对方 ack 版本；失败：失败计数 +1 并返回 false。
// prevAck 为上次确认版本——增量 = prevAck 之后的变化；空增量也推（活性）。
//
// 推的是「本域自有内容」（DeltaOwned）：本域 + 本域所有下级域；
// 从上级拉来的副本不推回去，避免成环。
func (v *TopologyView) PushBatchTo(address string, fromID string, post func(url string, payload []byte) ([]byte, int, error)) bool {
	return v.pushBatchTo(address, fromID, post, 0)
}

func (v *TopologyView) pushBatchTo(address string, fromID string, post func(url string, payload []byte) ([]byte, int, error), attempt int) bool {
	v.mu.RLock()
	prevAck := v.lastPushAck[address]
	resp := v.DeltaOwned(prevAck)
	fromID2, version := fromID, v.version
	v.mu.RUnlock()

	payload := mustJSON(TopologyPush{
		PushID:      pushID(),
		FromID:      fromID2,
		Version:     version,
		BaseVersion: prevAck,
		Nodes:       resp.Nodes,
		Removed:     resp.Removed,
	})
	body, status, err := post("http://"+address+"/api/topology/push", payload)
	if err != nil || status != http.StatusOK {
		v.mu.Lock()
		v.failCount[address]++
		failures := v.failCount[address]
		v.mu.Unlock()
		slog.Warn("topology push failed", "to", address, "failures", failures, "err", err, "status", status)
		return false
	}
	var pushResp TopologyPushResponse
	if err := json.Unmarshal(body, &pushResp); err != nil {
		return false
	}
	v.mu.Lock()
	if !pushResp.OK {
		// 接收方说增量对不上（它重启 / 中间漏过消息）→ 清空基线按全量重推一次
		v.lastPushAck[address] = 0
		v.failCount[address] = 0
		v.mu.Unlock()
		if attempt >= 1 {
			slog.Warn("topology push resync failed, giving up", "to", address)
			return false
		}
		return v.pushBatchTo(address, fromID2, post, attempt+1)
	}
	// 基线记本方版本：下次增量按本方记录版本号比较（推的是本域自有内容）
	v.lastPushAck[address] = version
	v.failCount[address] = 0
	v.mu.Unlock()
	return true
}

// PushFailures 返回某地址的连续失败次数（规则 8 的重注册触发依据）。
func (v *TopologyView) PushFailures(address string) int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.failCount[address]
}

// ResetPushState 清空某地址的推送状态（重注册成功后调用）。
func (v *TopologyView) ResetPushState(address string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.lastPushAck, address)
	delete(v.failCount, address)
}

// HandleSync HTTP 入口：GET /api/topology/sync?since=<int64>。
func (v *TopologyView) HandleSync(w http.ResponseWriter, r *http.Request) {
	// 请求即活（规则 6）：成员拉增量时自报 id/addr/attach，据此触达/入视图
	// （刷新存活 + 父指针 + 收编，见 TouchMember）。
	if id := r.URL.Query().Get("id"); id != "" && id != v.selfID {
		v.TouchMember(id, r.URL.Query().Get("addr"), r.URL.Query().Get("attach") == "1")
	}
	// 管理者降级告知（规则 6）：应答方已不是当选管理者时，拒绝增量并告知新管理者。
	if v.ManagerInfo != nil && v.selfID != "" {
		if id, addr := v.ManagerInfo(); id != "" && id != v.selfID {
			writeJSONValue(w, TopologySyncResponse{NotManager: true, ManagerID: id, ManagerAddr: addr})
			return
		}
	}
	since := parseSince(r.URL.Query().Get("since"))
	writeJSONValue(w, v.Delta(since))
}

// HandlePush HTTP 入口：POST /api/topology/push。
func (v *TopologyView) HandlePush(w http.ResponseWriter, r *http.Request) {
	var push TopologyPush
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&push); err != nil {
		http.Error(w, "bad push: "+err.Error(), http.StatusBadRequest)
		return
	}
	pusher := push.Pusher
	if pusher == "" {
		// RemoteAddr 带临时端口；Holder 需要的是对端 mesh 地址，取 host + 默认端口
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			pusher = net.JoinHostPort(host, "56701")
		} else {
			pusher = r.RemoteAddr
		}
	}
	writeJSONValue(w, v.ApplyPush(push, pusher))
}

// RegisterRoutes 挂载增量同步入口。
func (v *TopologyView) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/topology/sync", v.HandleSync)
	mux.HandleFunc("POST /api/topology/push", v.HandlePush)
}

// Snapshot 返回全量节点（Web UI / 旧拓扑接口读缓存用）。
func (v *TopologyView) Snapshot() []TopoNode {
	v.mu.RLock()
	defer v.mu.RUnlock()
	nodes := make([]TopoNode, 0, len(v.nodes))
	for _, rec := range v.nodes {
		node := rec.node
		if node.ID != v.selfID && node.Holder == "" {
			if node.Source == "domain" {
				node.Holder = v.pushers[node.DomainID]
			} else {
				node.Holder = v.selfAddr
			}
		}
		v.stampManager(&node)
		nodes = append(nodes, node)
	}
	sortNodesByID(nodes)
	return nodes
}

// Version 当前版本号。
func (v *TopologyView) Version() int64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.version
}

func sortNodesByID(nodes []TopoNode) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
}

func parseSince(raw string) int64 {
	var since int64
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0
		}
		since = since*10 + int64(c-'0')
	}
	return since
}

var topoPushCounter uint64

func pushID() string {
	topoPushCounter++
	return time.Now().UTC().Format("20060102150405.000000000") +
		"-" + strconv.FormatUint(topoPushCounter, 10)
}
