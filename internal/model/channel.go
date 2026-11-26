package model

import "time"

// 通道类别。ssh 通道是纯 SSH 功能（不依赖 NPA，Tabby 也能建立）；
// npa 通道是 NPA 系统之间的通道（依赖 NPA：节点服务、插件 HTTP 服务）。
const (
	ChannelCategorySSH = "ssh"
	ChannelCategoryNPA = "npa"
)

// 通道子类。
const (
	// ssh 通道
	ChannelKindLocal  = "local"  // ssh -L
	ChannelKindRemote = "remote" // ssh -R
	ChannelKindSocks  = "socks"  // ssh -D
	// npa 通道
	ChannelKindMapping    = "mapping"     // 本机端口 → 目标地址
	ChannelKindTunnel     = "tunnel"      // 节点间临时/永久隧道
	ChannelKindPluginHTTP = "plugin-http" // 插件自带的 HTTP 服务
)

// 通道状态。
const (
	ChannelStatusStopped  = "stopped"
	ChannelStatusStarting = "starting"
	ChannelStatusRunning  = "running"
	ChannelStatusDegraded = "degraded"
	ChannelStatusExpired  = "expired"
)

// Channel 是统一的通道模型。隧道与端口映射只是它的两种实现：
// ssh 通道由 SSH 隧道管理器承载，npa 通道由端口映射管理器与节点隧道承载。
type Channel struct {
	// ID 是统一标识，形如 "ssh:<定义ID>" / "npa:<定义ID>"，前缀标明类别。
	ID   string `json:"id"`
	Name string `json:"name"`
	// Category 取值 ssh / npa，决定这条通道依赖什么。
	Category string `json:"category"`
	// Kind 是类别下的子类（local/remote/socks 或 mapping/tunnel/plugin-http）。
	Kind string `json:"kind"`

	// 监听端：本机开放的端口。RuntimePort 是实际分配到的端口（0 表示自动分配）。
	ListenAddress string `json:"listen_address"`
	ListenPort    int    `json:"listen_port"`
	RuntimePort   int    `json:"runtime_port,omitempty"`

	// 目标端：远端是什么决定了通道类别（SSH 服务 → ssh 通道；NPA 节点 → npa 通道）。
	TargetHost   string `json:"target_host,omitempty"`
	TargetPort   int    `json:"target_port,omitempty"`
	TargetNodeID string `json:"target_node_id,omitempty"`

	// 经由：npa 通道的转发路径（direct / relay:<id> / manager:<id>）。
	ViaNodeID string `json:"via_node_id,omitempty"`
	ViaRoute  string `json:"via_route,omitempty"`

	// NodeID 是承载该通道的节点（ssh 通道指执行 SSH 的节点）。
	NodeID string `json:"node_id,omitempty"`
	// SSHUser / SSHPort 仅 ssh 通道使用。
	SSHUser string `json:"ssh_user,omitempty"`
	SSHPort int    `json:"ssh_port,omitempty"`

	// AutoStart 标记为 true 时随服务启动自动恢复。
	AutoStart bool `json:"auto_start"`
	// Temporary 标记这条通道是按「临时」意图建立的（AI 手工建）。
	// 实际回收以 ExpiresAt / IdleTimeoutSec 或显式关闭为准。
	Temporary bool `json:"temporary"`
	// ExpiresAt 是通道的绝对过期时间（nil 表示不过期，即永久通道）。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// IdleTimeoutSec 是空闲回收时间（秒，0 表示不按空闲回收）。
	IdleTimeoutSec int `json:"idle_timeout_sec,omitempty"`
	// Origin 记录通道来源：web / ai / auto。
	Origin string `json:"origin,omitempty"`

	Status    string `json:"status"`
	LastError string `json:"last_error,omitempty"`
	BytesTx   int64  `json:"bytes_tx,omitempty"`
	BytesRx   int64  `json:"bytes_rx,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ChannelID 拼出统一标识。
func ChannelID(category, refID string) string { return category + ":" + refID }

// ChannelRef 拆出统一标识里的类别与底层定义 ID。
func ChannelRef(id string) (category, refID string, ok bool) {
	for _, c := range []string{ChannelCategorySSH, ChannelCategoryNPA} {
		if len(id) > len(c)+1 && id[:len(c)] == c && id[len(c)] == ':' {
			return c, id[len(c)+1:], true
		}
	}
	return "", "", false
}
