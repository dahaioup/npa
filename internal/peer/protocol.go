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
	TypeOpen = "open" // 反向 TCP（端口映射 / 隧道 / 代理）
	TypeExec = "exec" // 在下级执行命令
	TypePTY  = "pty"  // 下级本地交互终端
	TypeFS   = "fs"   // 标准 SFTP 字节流
)

// 控制通道（长连接）帧类型。
const (
	CtrlRegister   = "register"
	CtrlRegistered = "registered"
	CtrlPing       = "ping"
	CtrlPong       = "pong"
	CtrlError      = "error"
)

const (
	protocolVersion      = 1
	writeTimeout         = 10 * time.Second
	ackOK           byte = 0
	ackError        byte = 1
)

// Header 是每条数据连接的首帧，决定该连接承载哪种通道。
type Header struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	Target    string `json:"target,omitempty"`
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
}

// Control 是控制通道上的 JSON 帧。
type Control struct {
	Type    string        `json:"type"`
	Node    *Registration `json:"node,omitempty"`
	Message string        `json:"message,omitempty"`
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
