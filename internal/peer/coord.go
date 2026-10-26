package peer

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	apperrors "github.com/npa/npa/internal/errors"
)

const (
	// requireWaitTimeout 是"我已经通知下去了，等它连过来"的等待上限。
	requireWaitTimeout = 8 * time.Second
	// sweepInterval 是临时链路空闲扫描间隔。
	sweepInterval = 5 * time.Second
	// defaultEphemeralIdle 临时链路的默认空闲上限（设置项 peer_on_demand_idle 覆盖）。
	defaultEphemeralIdle = 60 * time.Second
)

// ephemeralIdleTTL 临时链路空闲多久就主动断开。
func (h *Hub) ephemeralIdleTTL() time.Duration {
	if h.opts.EphemeralIdle != nil {
		if ttl := h.opts.EphemeralIdle(); ttl > 0 {
			return ttl
		}
	}
	return defaultEphemeralIdle
}

// setEphemeral 记录一条按需建立的临时链路：不写注册表、不改节点状态。
func (h *Hub) setEphemeral(nodeID string, conn *websocket.Conn, backTo string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ephemeral[nodeID] = &ephemeralState{conn: conn, backTo: backTo, lastUsed: time.Now()}
}

func (h *Hub) clearEphemeral(nodeID string, conn *websocket.Conn) {
	h.mu.Lock()
	state, ok := h.ephemeral[nodeID]
	if ok && (conn == nil || state.conn == conn) {
		delete(h.ephemeral, nodeID)
	}
	h.mu.Unlock()
	if ok && conn != nil && (state.conn == conn) {
		_ = conn.Close()
	}
}

// Online 判断该节点当前是否可用：持久链路或按需临时链路都算。
func (h *Hub) Online(nodeID string) bool {
	h.mu.Lock()
	_, persistent := h.links[nodeID]
	_, ephemeral := h.ephemeral[nodeID]
	h.mu.Unlock()
	return persistent || ephemeral
}

// EphemeralReady 报告某个节点的按需临时链路是否已建立（供 service 等待）。
func (h *Hub) EphemeralReady(nodeID string) bool { return h.hasEphemeral(nodeID) }

func (h *Hub) hasEphemeral(nodeID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.ephemeral[nodeID]
	return ok
}

// touchEphemeral 记录临时链路最近一次被使用。
func (h *Hub) touchEphemeral(nodeID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if state, ok := h.ephemeral[nodeID]; ok {
		state.lastUsed = time.Now()
	}
}

// StartSweeper 启动临时链路的空闲回收：超过 TTL 没用过就主动断开，
// 让节点回到"只挂在它唯一上级"的状态。
func (h *Hub) StartSweeper() {
	go func() {
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-ticker.C:
				h.purgeEphemeral(time.Now())
			}
		}
	}()
}

func (h *Hub) purgeEphemeral(now time.Time) {
	ttl := h.ephemeralIdleTTL()
	h.mu.Lock()
	stale := map[string]*ephemeralState{}
	for nodeID, state := range h.ephemeral {
		if now.Sub(state.lastUsed) > ttl {
			stale[nodeID] = state
			delete(h.ephemeral, nodeID)
		}
	}
	idle := make(map[string][]*websocket.Conn, len(stale))
	for nodeID, state := range stale {
		idle[nodeID] = append(idle[nodeID], state.conn)
	}
	h.mu.Unlock()
	for _, connections := range idle {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}
}

// sendToChild 经持久链路给某个下级下发控制帧。
func (h *Hub) sendToChild(nodeID string, control Control) error {
	h.mu.Lock()
	state, ok := h.links[nodeID]
	h.mu.Unlock()
	if !ok {
		return apperrors.New(apperrors.NodeOffline, "该节点当前没有反向链路")
	}
	return state.send(control)
}

// HandleRequire 供其它上级调用：让本机持有的某个下级反向连到 backto。
// 成功返回 accepted=true；节点没在超时内连过来则返回 accepted=false（调用方转而走代理）。
func (h *Hub) HandleRequire(w http.ResponseWriter, r *http.Request) {
	var request Control
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil ||
		strings.TrimSpace(request.NodeID) == "" || strings.TrimSpace(request.BackTo) == "" {
		writePeerError(w, apperrors.New(apperrors.InvalidArgument, "需要 node_id 与 backto"))
		return
	}
	nodeID := strings.TrimSpace(request.NodeID)
	h.mu.Lock()
	_, held := h.links[nodeID]
	h.mu.Unlock()
	if !held {
		writePeerError(w, apperrors.New(apperrors.InvalidArgument, "本机没有持有该节点的反向链路"))
		return
	}
	if h.hasEphemeral(nodeID) {
		writeCoordJSON(w, map[string]any{"accepted": true})
		return
	}
	if err := h.sendToChild(nodeID, Control{Type: CtrlConnectTo, Address: strings.TrimSpace(request.BackTo)}); err != nil {
		writePeerError(w, err)
		return
	}
	// 只负责"通知下去"：临时链路建在请求方那边，这里看不到，
	// 由请求方自己等待建立；等不到就转走代理兜底。
	writeCoordJSON(w, map[string]any{"accepted": true, "notified": true})
}

// HandleRequireRelay 供其它上级调用：本机持有该节点，请把它经本机代理给请求方。
// 做法是**本机主动连到请求方**的 relay 通道入口（请求方是上级，也开着 peer 端口）。
func (h *Hub) HandleRequireRelay(w http.ResponseWriter, r *http.Request) {
	var request Control
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil ||
		strings.TrimSpace(request.NodeID) == "" || strings.TrimSpace(request.BackTo) == "" {
		writePeerError(w, apperrors.New(apperrors.InvalidArgument, "需要 node_id 与 backto"))
		return
	}
	nodeID := strings.TrimSpace(request.NodeID)
	backTo := strings.TrimSpace(request.BackTo)
	h.mu.Lock()
	_, held := h.links[nodeID]
	h.mu.Unlock()
	if !held {
		writePeerError(w, apperrors.New(apperrors.InvalidArgument, "本机没有持有该节点的反向链路"))
		return
	}
	// 异步建立：openRelayChannel 会一直服务到代理会话结束，不能阻塞本请求
	go func() {
		if err := h.openRelayChannel(context.Background(), backTo, nodeID); err != nil {
			slog.Warn("代理通道建立失败", "node", nodeID, "backto", backTo, "error", err)
		}
	}()
	writeCoordJSON(w, map[string]any{"accepted": true, "notified": true, "via": h.opts.Name})
}

// HandleRelay 是请求方侧的数据通道入口：持有方主动连过来，等待首帧决定转发什么。
func (h *Hub) HandleRelay(w http.ResponseWriter, r *http.Request) {
	nodeID := strings.TrimSpace(r.URL.Query().Get("node"))
	if nodeID == "" {
		writePeerError(w, apperrors.New(apperrors.InvalidArgument, "node is required"))
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.relayChannels[nodeID] = append(h.relayChannels[nodeID], conn)
	h.mu.Unlock()
	// 连接由 takeRelay 取走并驱动；若一直没被取用，由调用方超时后关闭
}

// takeRelay 从对端推过来的代理通道里取一条，写首帧并等应答。
func (h *Hub) takeRelay(nodeID string, header Header) (net.Conn, error) {
	deadline := time.Now().Add(takeTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn := h.popRelay(nodeID)
		if conn == nil {
			time.Sleep(poolPollInterval)
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
		clearDeadlines(conn)
		return NewWSConn(conn), nil
	}
	if lastErr == nil {
		lastErr = apperrors.New(apperrors.NodeOffline, "持有方没有提供可用的代理通道")
	}
	return nil, apperrors.Wrap(apperrors.NodeOffline, "cannot take relay channel", lastErr)
}

func (h *Hub) popRelay(nodeID string) *websocket.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	channels := h.relayChannels[nodeID]
	if len(channels) == 0 {
		return nil
	}
	conn := channels[len(channels)-1]
	h.relayChannels[nodeID] = channels[:len(channels)-1]
	return conn
}

// openRelayChannel 由持有方调用：连到请求方的 relay 通道入口，等它下发转发指令。
// 本机既是上级（对下级持有链路）也能作为下级连到别的上级，二者互不冲突。
func (h *Hub) openRelayChannel(ctx context.Context, address, nodeID string) error {
	endpoint := "ws://" + address + "/api/peers/relay?node=" + nodeID
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return err
	}
	var header Header
	if err := readJSON(conn, &header, time.Time{}); err != nil {
		_ = conn.Close()
		return err
	}
	if header.Type != TypeRelayOpen || header.Target == "" {
		_ = conn.Close()
		return apperrors.New(apperrors.InvalidArgument, "代理通道首帧不对")
	}
	// 用本机与该下级之间的持久链路转发到目标
	remote, err := h.take(ctx, header.NodeID, Header{Type: TypeOpen, Target: header.Target, Via: header.Via})
	if err != nil {
		_ = writeAck(conn, err)
		_ = conn.Close()
		return err
	}
	if err := writeAck(conn, nil); err != nil {
		_ = remote.Close()
		_ = conn.Close()
		return err
	}
	pipe(NewWSConn(conn), NewWSConn(remote))
	return nil
}

// DialUpper 经持有上级代理拨号 A→B→N：要求 B 先建代理通道，再从通道取一条下发 open。
func (h *Hub) DialUpper(ctx context.Context, holderAddress, nodeID, target, via string) (net.Conn, error) {
	deadline := time.Now().Add(requireWaitTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if conn, err := h.takeRelay(nodeID, Header{Type: TypeRelayOpen, NodeID: nodeID, Target: target, Via: via}); err == nil {
			return conn, nil
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = apperrors.New(apperrors.NodeOffline, "持有方未提供代理通道")
	}
	return nil, lastErr
}

// RequireChild 通知本机持有的下级反向连到 backto（本机是"持有者"时被调用方使用）。
func (h *Hub) RequireChild(ctx context.Context, nodeID, backTo string) error {
	if !h.hasEphemeral(nodeID) {
		if err := h.sendToChild(nodeID, Control{Type: CtrlConnectTo, Address: backTo}); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(requireWaitTimeout)
	for time.Now().Before(deadline) {
		if h.hasEphemeral(nodeID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return apperrors.New(apperrors.NodeOffline, "节点未在超时内连过来（多半是直连不上请求方）")
}

// writeCoordJSON 写协调接口的响应（本包的 writeJSON 是 WebSocket 帧，不能复用）。
func writeCoordJSON(w http.ResponseWriter, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}
