package peer

import (
	"context"
	"encoding/json"
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
}

// Hub 是上级侧的反向接入服务端：接受注册、维护长连接与空闲连接池，
// 并在需要时从池里取连接下发请求（对标 frp 的控制连接 + workConn 池模型）。
type Hub struct {
	repo Repository
	opts Options

	mu     sync.Mutex
	links  map[string]*linkState
	idle   map[string][]*websocket.Conn
	closed bool
}

func NewHub(repo Repository, opts Options) *Hub {
	return &Hub{repo: repo, opts: opts, links: map[string]*linkState{}, idle: map[string][]*websocket.Conn{}}
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
	return Info{
		Product: "npa", Name: h.opts.Name, Version: h.opts.Version, PeerPort: port,
		Subnets: subnets, Protocols: []string{"register", "link", "conn", TypeOpen, TypeExec, TypePTY, TypeFS},
	}
}

// HandleInfo 返回自描述信息，供下级探测识别 NPA。
func (h *Hub) HandleInfo(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(h.info())
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

	if err := writeJSON(conn, Control{Type: CtrlRegistered, Node: &registration}); err != nil {
		return
	}
	for {
		var control Control
		if err := readJSON(conn, &control, time.Now().Add(linkTimeout)); err != nil {
			return
		}
		switch control.Type {
		case CtrlPing:
			h.touch(node.ID)
			if err := writeJSON(conn, Control{Type: CtrlPong}); err != nil {
				return
			}
		case CtrlRegister:
			if control.Node != nil {
				if _, err := h.Register(*control.Node, remoteIP(r)); err != nil {
					return
				}
				h.touch(node.ID)
			}
		default:
			h.touch(node.ID)
		}
	}
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
		lastErr = apperrors.New(apperrors.NodeOffline, "peer 没有可用的空闲数据连接（链路未就绪）")
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

// Online 报告某条反向链路是否在线。
func (h *Hub) Online(nodeID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.links[nodeID]
	return ok
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
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	links := h.links
	idle := h.idle
	h.links = map[string]*linkState{}
	h.idle = map[string][]*websocket.Conn{}
	h.mu.Unlock()
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
