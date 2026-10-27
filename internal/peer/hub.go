package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
)

const (
	// linkTimeout 是控制通道的读超时：超过该时间没有心跳就认为链路已断。
	linkTimeout = 45 * time.Second
	// takeTimeout 是上级等待下级空闲数据连接的最长时间。
	takeTimeout = 5 * time.Second
	// poolPollInterval 是 Take 轮询空闲连接的间隔。
	poolPollInterval = 50 * time.Millisecond
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // 服务端 Agent 没有 Origin
		}
		return strings.Contains(origin, r.Host)
	},
}

type Repository interface {
	GetNode(id string) (model.Node, error)
	ListNodes() ([]model.Node, error)
	SaveNode(node model.Node) error
	UpdateNodeStatus(id, status, lastError string) error
}

// Options 由 service 注入，避免 peer 依赖配置读取细节。
type Options struct {
	Version   string
	Name      string
	Port      func() int
	Subnets   func() []string
	AutoTrust func() bool
	// OnRegister 在节点成功注册后回调（用于审计）。
	OnRegister func(node model.Node)
	// Self 提供本机在拓扑中的自我描述（service 注入，hub 不依赖节点表）。
	Self func() TopologyNode
	// EphemeralIdle 临时链路的空闲上限（设置项 peer_on_demand_idle）。
	EphemeralIdle func() time.Duration
}

// LinkStatus 描述某条反向链路的当前状态。
type LinkStatus struct {
	NodeID   string    `json:"node_id"`
	Name     string    `json:"name"`
	Address  string    `json:"address"`
	Subnets  []string  `json:"subnets"`
	IdlePool int       `json:"idle_pool"`
	LastSeen time.Time `json:"last_seen"`
	Online   bool      `json:"online"`
}

type linkState struct {
	conn     *websocket.Conn
	lastSeen time.Time
	registry Registration
	writeMu  sync.Mutex // 控制帧只有一个写入者：读循环与 require 路径共用
}

func (l *linkState) send(control Control) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	return writeJSON(l.conn, control)
}

// ephemeralState 是按需建立的临时链路（上级之间 / 上级要求下级连过来）。
// 它不写入节点注册表，空闲超过 TTL 由持有方主动断开。
type ephemeralState struct {
	conn     *websocket.Conn
	backTo   string
	lastUsed time.Time
}

// Hub 是上级侧的反向接入服务端：接受注册、维护长连接与空闲连接池，
// 并在需要时从池里取连接下发请求（对标 frp 的控制连接 + workConn 池模型）。
type Hub struct {
	repo Repository
	opts Options

	mu    sync.Mutex
	links map[string]*linkState
	idle  map[string][]*websocket.Conn
	// ephemeral：按需建立的临时链路；relayChannels：经持有上级代理时对端推过来的数据通道
	ephemeral     map[string]*ephemeralState
	relayChannels map[string][]*websocket.Conn
	stop          chan struct{}
	closeOnce     sync.Once
	closed        bool
}

func NewHub(repo Repository, opts Options) *Hub {
	return &Hub{
		repo: repo, opts: opts,
		links:         map[string]*linkState{},
		idle:          map[string][]*websocket.Conn{},
		ephemeral:     map[string]*ephemeralState{},
		relayChannels: map[string][]*websocket.Conn{},
		stop:          make(chan struct{}),
	}
}

func (h *Hub) info() Info {
	port := 0
	if h.opts.Port != nil {
		port = h.opts.Port()
	}
	subnets := []string{}
	if h.opts.Subnets != nil {
		subnets = h.opts.Subnets()
	}
	info := Info{
		Product: "npa", Name: h.opts.Name, Version: h.opts.Version, PeerPort: port,
		Subnets: subnets, Protocols: []string{"register", "link", "conn", TypeOpen, TypeExec, TypePTY, TypeFS, TypeRelayOpen},
	}
	if h.opts.Self != nil {
		if self := h.opts.Self(); self.ID != "" {
			info.ID = self.ID
		}
	}
	return info
}

// ResetLinksOnStart 在本机启动（或重启）后调用：
// 之前标记为 connected 的反向节点此刻必然没有活链路，先置为 offline 等待重连，
// 避免"显示在线、实际断开"误导用户。
func (h *Hub) ResetLinksOnStart() int {
	nodes, err := h.repo.ListNodes()
	if err != nil {
		return 0
	}
	reset := 0
	for _, node := range nodes {
		if node.Protocol != "peer" || node.Status == "offline" {
			continue
		}
		if err := h.repo.UpdateNodeStatus(node.ID, "offline", "本机刚启动，等待节点重新接入"); err == nil {
			reset++
		}
	}
	return reset
}

// HandleInfo 返回自描述信息，供下级探测识别 NPA。
func (h *Hub) HandleInfo(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(h.info())
}

// HandleTopology 返回"本机 + 其直接下级 + 本机的上级"，供其它上级拼出完整树。
// 只返回一层 children，递归由请求方按需拉取。
func (h *Hub) HandleTopology(w http.ResponseWriter, _ *http.Request) {
	topology := Topology{Children: []TopologyNode{}}
	if h.opts.Self != nil {
		topology.Self = h.opts.Self()
	}
	h.mu.Lock()
	online := make(map[string]bool, len(h.links))
	for id := range h.links {
		online[id] = true
	}
	h.mu.Unlock()

	if nodes, err := h.repo.ListNodes(); err == nil {
		for _, node := range nodes {
			if node.Protocol != "peer" || !node.Registered {
				continue
			}
			topology.Children = append(topology.Children, TopologyNode{
				ID: node.ID, Name: node.Name, Address: node.Address, Port: node.Port,
				Version: node.Version, Protocol: node.Protocol,
				Subnets: node.ReachableSubnets,
				Online:  online[node.ID], Registered: true,
			})
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(topology)
}

// HandleRegister 处理 HTTP 注册（便于探测与调试；长连接首帧同样可以注册）。
func (h *Hub) HandleRegister(w http.ResponseWriter, r *http.Request) {
	var registration Registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&registration); err != nil {
		writePeerError(w, apperrors.New(apperrors.InvalidArgument, "invalid registration"))
		return
	}
	node, err := h.Register(registration, remoteIP(r))
	if err != nil {
		writePeerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"registered": true, "id": node.ID, "info": h.info()})
}

// Register 登记或刷新一个反向接入节点。
func (h *Hub) Register(registration Registration, fallbackAddress string) (model.Node, error) {
	registration.ID = strings.TrimSpace(registration.ID)
	if registration.ID == "" {
		return model.Node{}, apperrors.New(apperrors.InvalidArgument, "node id is required")
	}
	address := strings.TrimSpace(registration.Address)
	if address == "" || address == "0.0.0.0" || address == "::" {
		address = fallbackAddress
	}
	if address == "" {
		return model.Node{}, apperrors.New(apperrors.InvalidArgument, "node address is required")
	}
	if registration.ID == model.NodeIDFromMAC() {
		return model.Node{}, apperrors.New(apperrors.InvalidArgument, "节点不能向自身注册反向链路")
	}
	now := time.Now().UTC()
	autoTrust := true
	if h.opts.AutoTrust != nil {
		autoTrust = h.opts.AutoTrust()
	}
	name := strings.TrimSpace(registration.Name)
	if name == "" {
		name = registration.ID
	}
	node := model.Node{
		ID: registration.ID, Name: name, Address: address, Port: registration.Port,
		Username: "peer", AuthMethod: "peer", Protocol: "peer",
		Tags:         []string{"peer", "reverse"},
		Status:       "connected",
		Capabilities: registration.Caps, ReachableSubnets: registration.Subnets,
		Trusted: autoTrust, RelayEnabled: true, Registered: true,
		PeerRoutes: registration.SSHRoutes,
		Version:    registration.Version,
		PeerSource: registration.Source, LastSeen: now, UpdatedAt: now,
	}
	if previous, err := h.repo.GetNode(registration.ID); err == nil {
		node.CreatedAt = previous.CreatedAt
		node.HostKeyFingerprint = previous.HostKeyFingerprint
		node.Trusted = previous.Trusted || autoTrust
	}
	if node.CreatedAt.IsZero() {
		node.CreatedAt = now
	}
	if err := h.repo.SaveNode(node); err != nil {
		return model.Node{}, err
	}
	if h.opts.OnRegister != nil {
		h.opts.OnRegister(node)
	}
	return node, nil
}

// HandleLink 处理长连接：首帧注册，之后心跳与状态维护。
func (h *Hub) HandleLink(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	closeWith := func(message string) {
		_ = writeJSON(conn, Control{Type: CtrlError, Message: message})
		_ = conn.Close()
	}
	registration, err := readRegistration(conn, time.Now().Add(10*time.Second))
	if err != nil {
		closeWith("invalid registration frame: " + err.Error())
		return
	}
	if registration.ID == "" {
		closeWith("node id is required")
		return
	}
	requested := strings.TrimSpace(r.URL.Query().Get("node"))
	if requested != "" && requested != strings.TrimSpace(registration.ID) {
		closeWith("node id mismatch")
		return
	}
	// 按需直连：临时链路不写注册表、不改节点状态，空闲超时后自动断开
	if registration.Ephemeral {
		h.setEphemeral(registration.ID, conn, requested)
		defer h.clearEphemeral(registration.ID, conn)
		// 必须回执，否则对端会等到超时并退出（链路建了却没有连接池）
		if err := writeJSON(conn, Control{Type: CtrlRegistered, Node: &registration}); err != nil {
			return
		}
		h.serveControl(conn, registration.ID, true, func(control Control) error {
			return writeJSON(conn, control)
		})
		return
	}

	node, err := h.Register(registration, remoteIP(r))
	if err != nil {
		closeWith(apperrors.MessageOf(err))
		return
	}
	if err := h.setLink(node.ID, conn, registration); err != nil {
		closeWith(err.Error())
		return
	}
	defer h.clearLink(node.ID, conn)

	// 首帧也必须走发送器：链路已登记，require 路径可能同时在写
	if err := h.linkSender(node.ID)(Control{Type: CtrlRegistered, Node: &registration}); err != nil {
		return
	}
	h.serveControl(conn, node.ID, false, h.linkSender(node.ID))
}

// linkSender 返回经互斥保护的控制帧发送器：读循环的 pong 与 require 下发共用，
// 避免 gorilla 连接被并发写（数据竞争）。
func (h *Hub) linkSender(nodeID string) func(Control) error {
	return func(control Control) error {
		h.mu.Lock()
		state, ok := h.links[nodeID]
		h.mu.Unlock()
		if !ok {
			return apperrors.New(apperrors.NodeOffline, "反向链路已断开")
		}
		return state.send(control)
	}
}

// serveControl 处理控制通道：心跳、重新注册，以及下级主动上报的中继失败。
func (h *Hub) serveControl(conn *websocket.Conn, nodeID string, ephemeral bool, send func(Control) error) {
	touch := func() {
		if ephemeral {
			h.touchEphemeral(nodeID)
			return
		}
		h.touch(nodeID)
	}
	for {
		var control Control
		if err := readJSON(conn, &control, time.Now().Add(linkTimeout)); err != nil {
			return
		}
		switch control.Type {
		case CtrlPing:
			touch()
			if err := send(Control{Type: CtrlPong}); err != nil {
				return
			}
		case CtrlRegister:
			if control.Node != nil {
				if _, err := h.Register(*control.Node, remoteIP2(conn)); err != nil && !ephemeral {
					return
				}
				touch()
			}
		case CtrlRelayFail:
			// 下级连不上请求方：保持原样，请求方会按超时转走代理
			touch()
		default:
			touch()
		}
	}
}

func remoteIP2(conn *websocket.Conn) string {
	if addr := conn.RemoteAddr(); addr != nil {
		if host, _, err := net.SplitHostPort(addr.String()); err == nil {
			return host
		}
	}
	return ""
}

// HandleConn 处理空闲数据连接：交给连接池，等待上级取用。
func (h *Hub) HandleConn(w http.ResponseWriter, r *http.Request) {
	nodeID := strings.TrimSpace(r.URL.Query().Get("node"))
	if nodeID == "" {
		writePeerError(w, apperrors.New(apperrors.InvalidArgument, "node is required"))
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h.offer(nodeID, conn)
}

func (h *Hub) setLink(nodeID string, conn *websocket.Conn, registration Registration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return apperrors.New(apperrors.NodeOffline, "peer hub is closed")
	}
	if existing, ok := h.links[nodeID]; ok && existing.conn != nil && existing.conn != conn {
		_ = existing.conn.Close()
	}
	h.links[nodeID] = &linkState{conn: conn, lastSeen: time.Now(), registry: registration}
	_ = h.repo.UpdateNodeStatus(nodeID, "connected", "")
	return nil
}

func (h *Hub) touch(nodeID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if state, ok := h.links[nodeID]; ok {
		state.lastSeen = time.Now()
	}
}

func (h *Hub) clearLink(nodeID string, conn *websocket.Conn) {
	h.mu.Lock()
	state, ok := h.links[nodeID]
	if ok && state.conn == conn {
		delete(h.links, nodeID)
		// 状态必须先于链路表落库：否则 Links() 已经看不到这条链路，
		// 而节点还显示 connected，观察者（Web、测试）会看到不一致的中间态。
		_ = h.repo.UpdateNodeStatus(nodeID, "offline", "reverse link closed")
	}
	// 链路断开时清掉该节点的空闲连接，避免上级拿到死连接。
	orphans := h.idle[nodeID]
	delete(h.idle, nodeID)
	h.mu.Unlock()
	for _, orphan := range orphans {
		_ = orphan.Close()
	}
}

func (h *Hub) offer(nodeID string, conn *websocket.Conn) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.Close()
		return
	}
	h.idle[nodeID] = append(h.idle[nodeID], conn)
	h.mu.Unlock()
}

func (h *Hub) pop(nodeID string) *websocket.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	connections := h.idle[nodeID]
	if len(connections) == 0 {
		return nil
	}
	conn := connections[len(connections)-1]
	h.idle[nodeID] = connections[:len(connections)-1]
	return conn
}

// take 从连接池取一条空闲连接并完成「写首帧 + 读应答」，失败则换下一条重试。
func (h *Hub) take(ctx context.Context, nodeID string, header Header) (*websocket.Conn, error) {
	deadline := time.Now().Add(takeTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn := h.pop(nodeID)
		if conn == nil {
			select {
			case <-ctx.Done():
				return nil, apperrors.Wrap(apperrors.Timeout, "reverse dial cancelled", ctx.Err())
			case <-time.After(poolPollInterval):
			}
			continue
		}
		if err := writeJSON(conn, header); err != nil {
			lastErr = err
			_ = conn.Close()
			continue
		}
		if err := readAck(conn, time.Now().Add(takeTimeout)); err != nil {
			lastErr = err
			_ = conn.Close()
			continue
		}
		// 握手完成即进入长寿命数据阶段：清掉 5s 读超时，否则传输中途会被切断
		clearDeadlines(conn)
		return conn, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("链路未就绪：节点 %s 当前没有反向连接，请确认该节点的 NPA 正在运行并能访问本机的反向接入端口", nodeID)
	}
	return nil, apperrors.Wrap(apperrors.NodeOffline, "cannot take reverse connection", lastErr)
}

// Dial 通过反向链路拨号到目标（对标 NPA 自己的端口映射数据面）。
func (h *Hub) Dial(ctx context.Context, nodeID, target, via string) (net.Conn, error) {
	header := NewHeader(TypeOpen)
	header.Target = target
	header.Via = via
	conn, err := h.take(ctx, nodeID, header)
	if err != nil {
		return nil, err
	}
	return NewWSConn(conn), nil
}

// Call 打开一条请求-响应通道，返回已应答的连接；调用方负责关闭。
func (h *Hub) Call(ctx context.Context, nodeID string, header Header) (*websocket.Conn, error) {
	return h.take(ctx, nodeID, header)
}

// Links 返回所有在线链路状态（含空闲连接池数量）。
func (h *Hub) Links() []LinkStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]LinkStatus, 0, len(h.links))
	for nodeID, state := range h.links {
		result = append(result, LinkStatus{
			NodeID: nodeID, Name: state.registry.Name, Address: state.registry.Address,
			Subnets: state.registry.Subnets, IdlePool: len(h.idle[nodeID]),
			LastSeen: state.lastSeen, Online: true,
		})
	}
	return result
}

func (h *Hub) Close() {
	h.closeOnce.Do(func() { close(h.stop) })
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	links := h.links
	idle := h.idle
	ephemeral := h.ephemeral
	relayChannels := h.relayChannels
	h.links = map[string]*linkState{}
	h.idle = map[string][]*websocket.Conn{}
	h.ephemeral = map[string]*ephemeralState{}
	h.relayChannels = map[string][]*websocket.Conn{}
	h.mu.Unlock()
	for _, state := range ephemeral {
		if state.conn != nil {
			_ = state.conn.Close()
		}
	}
	for _, channels := range relayChannels {
		for _, conn := range channels {
			_ = conn.Close()
		}
	}
	for _, state := range links {
		if state.conn != nil {
			_ = state.conn.Close()
		}
	}
	for _, connections := range idle {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writePeerError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if apperrors.CodeOf(err) == apperrors.NodeOffline {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"code": string(apperrors.CodeOf(err)), "message": apperrors.MessageOf(err),
	}})
}
