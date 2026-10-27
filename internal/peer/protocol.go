package peer

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/npa/npa/internal/model"
)

// 通道类型：上级从连接池取一条连接后，用首帧（JSON 文本帧）声明要做什么。
const (
	TypeOpen      = "open"       // 反向 TCP（端口映射 / 隧道 / 代理）
	TypeExec      = "exec"       // 在下级执行命令
	TypePTY       = "pty"        // 下级本地交互终端
	TypeFS        = "fs"         // 标准 SFTP 字节流
	TypeRelayOpen = "relay_open" // 经持有上级代理到某个下级（A→B→N）
)

// 控制通道（长连接）帧类型。
const (
	CtrlRegister   = "register"
	CtrlRegistered = "registered"
	CtrlPing       = "ping"
	CtrlPong       = "pong"
	CtrlError      = "error"
	// 跨上级协调
	CtrlConnectTo    = "connect_to"    // 持有者 → 下级：去连 backto
	CtrlRelayFail    = "relay_fail"    // 下级 → 持有者：连不上
	CtrlRequire      = "require"       // 上级 → 持有者：把某下级接到我这里
	CtrlRequireRelay = "require_relay" // 上级 → 持有者：经你中继到某下级
)

const (
	protocolVersion      = 1
	writeTimeout         = 10 * time.Second
	ackOK           byte = 0
	ackError        byte = 1
)

// TopologyNode 是拓扑图里的一台 NPA；既能是上级也能是下级。
type TopologyNode struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Address    string         `json:"address"`
	Port       int            `json:"port,omitempty"` // mDNS / HTTP api 端口
	Version    string         `json:"version,omitempty"`
	Protocol   string         `json:"protocol,omitempty"`
	Online     bool           `json:"online"`
	Registered bool           `json:"registered,omitempty"`
	Children   []TopologyNode `json:"children,omitempty"`
	// Subnets 是该节点宣告的可达子网，选路时用来判断目标归它
	Subnets []string `json:"subnets,omitempty"`
	// Parent / Via / Unknown / Error 由 service 组装跨上级拓扑时填充
	Parent  string `json:"parent,omitempty"`
	Via     string `json:"via,omitempty"` // direct | relay:<upper>
	Unknown bool   `json:"unknown,omitempty"`
	Error   string `json:"error,omitempty"`
	// Domain 是分域 v2 的域标识（= 域内当选管理者 ID）；DomainSeq 单调推进。
	Domain    string `json:"domain,omitempty"`
	DomainSeq uint64 `json:"domain_seq,omitempty"`
}

// Topology 是一次拓扑查询的返回：自己 + 其直接下级 + 它的上级（可再向上递归）。
type Topology struct {
	Self     TopologyNode   `json:"self"`
	Parent   *TopologyNode  `json:"parent,omitempty"`
	Children []TopologyNode `json:"children"`
}

// Header 是每条数据连接的首帧，决定该连接承载哪种通道。
type Header struct {
	V      int    `json:"v"`
	Type   string `json:"type"`
	Target string `json:"target,omitempty"`
	// NodeID 用于 relay_open：指定要经哪条下级链路转发
	NodeID    string `json:"node_id,omitempty"`
	Via       string `json:"via,omitempty"` // direct | ssh:<nodeID>
	Command   string `json:"command,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}

// Registration 是下级向自报的节点身份信息。
type Registration struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Address string   `json:"address"`
	Port    int      `json:"port"`
	Subnets []string `json:"subnets"`
	Caps    []string `json:"caps"`
	Version string   `json:"version"`
	Source  string   `json:"source,omitempty"` // seed | probe | manual
	// SSHRoutes 是下级已纳管的 SSH 跳板（NPA 节点到 SSH 服务的映射段），供上级计算串联路径。
	SSHRoutes []model.RemoteRoute `json:"ssh_routes,omitempty"`
	// Ephemeral 标记这是按需建立的**临时**链路：不写入注册表、不改节点状态、
	// 空闲超时后由持有方主动断开，节点的持久归属（唯一上级）不受影响。
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// Control 是控制通道上的 JSON 帧。除注册/心跳外，还承载跨上级协调消息。
type Control struct {
	Type    string        `json:"type"`
	Node    *Registration `json:"node,omitempty"`
	Message string        `json:"message,omitempty"`
	// connect_to：要求下级主动连到 backto（按需直连）
	Address string `json:"address,omitempty"`
	// require / require_relay：上级向持有者发起的请求
	NodeID string `json:"node_id,omitempty"`
	BackTo string `json:"backto,omitempty"`
	Via    string `json:"via,omitempty"`
	// relay_fail：连不上时回报
	Reason string `json:"reason,omitempty"`
	// accepted：持有者应答"我已经通知下去了"
	Accepted bool `json:"accepted,omitempty"`
}

// ExecFrame 是 exec 通道上的输出帧：先按流回传 stdout/stderr，最后一帧带退出码。
type ExecFrame struct {
	Stream     string `json:"stream,omitempty"` // out | err
	Data       string `json:"data,omitempty"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
}

// Info 是上级返回给下级的自描述信息，用于探测识别 NPA。
type Info struct {
	Product   string   `json:"product"`
	ID        string   `json:"id,omitempty"` // 节点自报标识，供分域选举比较
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	PeerPort  int      `json:"peer_port"`
	Subnets   []string `json:"subnets"`
	Protocols []string `json:"protocols"`
}

func NewHeader(channelType string) Header {
	return Header{V: protocolVersion, Type: channelType}
}

func writeJSON(conn *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return conn.WriteMessage(websocket.TextMessage, data)
}

// readJSON 读取一个 JSON 文本帧；超时或类型不符都返回错误，避免把数据帧误当控制帧。
func readJSON(conn *websocket.Conn, target any, deadline time.Time) error {
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	if messageType != websocket.TextMessage {
		return fmt.Errorf("expected json frame, got message type %d", messageType)
	}
	return json.Unmarshal(data, target)
}

// readRegistration 读取注册首帧：既接受 Control{type:"register",node:{...}}，
// 也接受裸的 Registration 对象，避免两端写法耦合。
func readRegistration(conn *websocket.Conn, deadline time.Time) (Registration, error) {
	var registration Registration
	if err := conn.SetReadDeadline(deadline); err != nil {
		return registration, err
	}
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		return registration, err
	}
	if messageType != websocket.TextMessage {
		return registration, fmt.Errorf("expected registration frame, got message type %d", messageType)
	}
	var control Control
	if err := json.Unmarshal(data, &control); err == nil && control.Node != nil {
		return *control.Node, nil
	}
	if err := json.Unmarshal(data, &registration); err != nil {
		return registration, fmt.Errorf("invalid registration frame: %w", err)
	}
	if registration.ID == "" && control.Type == CtrlRegister && control.Node != nil {
		return *control.Node, nil
	}
	return registration, nil
}

// clearDeadlines 清除握手期设置的读写截止时间。
// 握手用短超时是为了快速失败，但进入数据阶段后连接可能是长寿命的
// （SFTP 传输、RustDesk 会话、交互终端），必须清零，否则会在 5s/10s 后被切断。
func clearDeadlines(conn *websocket.Conn) {
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})
}

// writeAck 写应答帧：二进制首字节为状态，失败时后续字节为错误文本。
// 应答写出即进入数据阶段，因此写完立刻清掉握手期的超时。
func writeAck(conn *websocket.Conn, cause error) error {
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	var err error
	if cause == nil {
		err = conn.WriteMessage(websocket.BinaryMessage, []byte{ackOK})
	} else {
		err = conn.WriteMessage(websocket.BinaryMessage, append([]byte{ackError}, cause.Error()...))
	}
	clearDeadlines(conn)
	return err
}

func readAck(conn *websocket.Conn, deadline time.Time) error {
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	if messageType != websocket.BinaryMessage || len(data) == 0 {
		return fmt.Errorf("invalid ack frame")
	}
	if data[0] == ackOK {
		return nil
	}
	return fmt.Errorf("%s", strings.TrimSpace(string(data[1:])))
}
