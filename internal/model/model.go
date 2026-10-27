package model

import "time"

type Node struct {
	ID                 string   `json:"id"`
	DisplayID          string   `json:"display_id,omitempty"`
	Name               string   `json:"name"`
	Address            string   `json:"address"`
	Port               int      `json:"port"`
	Username           string   `json:"username"`
	AuthMethod         string   `json:"auth_method"`
	Protocol           string   `json:"protocol"`
	Tags               []string `json:"tags"`
	Status             string   `json:"status"`
	LastError          string   `json:"last_error,omitempty"`
	HostKeyFingerprint string   `json:"host_key_fingerprint,omitempty"`
	ReachableSubnets   []string `json:"reachable_subnets,omitempty"`
	Capabilities       []string `json:"capabilities,omitempty"`
	RelayEnabled       bool     `json:"relay_enabled"`
	Trusted            bool     `json:"trusted"`
	Discovered         bool     `json:"discovered"`
	// Registered 表示该节点是主动向上级注册的反向接入（peer）节点；
	// 它由子网侧主动建立长连接，上级通过该链路反向联系它。
	Registered bool   `json:"registered,omitempty"`
	PeerSource string `json:"peer_source,omitempty"` // seed | probe | manual
	// PeerRoutes 是该 peer 节点**对端**已纳管的 SSH 跳板，用于「NPA 映射 + SSH 映射」串联。
	PeerRoutes []RemoteRoute `json:"peer_routes,omitempty"`
	// Version 是对端自报的 NPA 版本（peer 注册、mDNS TXT 的 ver）；纯 SSH 节点通常为空。
	Version string `json:"version,omitempty"`
	// PeerPort 是对端广播的反向接入端口（跨上级查拓扑要用）。
	PeerPort int `json:"peer_port,omitempty"`
	// Uptime 是对端 mDNS 广播的进程运行秒数（分域管理者选举的比较项）。
	Uptime int64 `json:"uptime_seconds,omitempty"`
	// Online 由 ListNodes 计算：peer 看反向链路是否在线，其余节点做快速探测。
	Online bool `json:"online"`
	// UpdateAvailable 由 ListNodes 计算：该节点版本严格新于本机。
	UpdateAvailable bool      `json:"update_available,omitempty"`
	LastSeen        time.Time `json:"last_seen,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Credential struct {
	NodeID             string `json:"node_id"`
	Password           string `json:"password,omitempty"`
	PrivateKey         string `json:"private_key,omitempty"`
	PrivateKeyPassword string `json:"private_key_password,omitempty"`
}

type Tunnel struct {
	ID            string    `json:"id"`
	NodeID        string    `json:"node_id"`
	Name          string    `json:"name"`
	Type          string    `json:"type"`
	ListenAddress string    `json:"listen_address"`
	ListenPort    int       `json:"listen_port"`
	TargetHost    string    `json:"target_host,omitempty"`
	TargetPort    int       `json:"target_port,omitempty"`
	AutoStart     bool      `json:"auto_start"`
	Status        string    `json:"status"`
	LastError     string    `json:"last_error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// PortMapping 是 NPA 自带的端口映射：本机监听端口，把入站连接转发到子网内的目标 IP:端口，
// 转发路径由 relay 选路（直连或经已纳管节点中继），与 SSH 隧道相互独立。
type PortMapping struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	TargetHost    string    `json:"target_host"`
	TargetPort    int       `json:"target_port"`
	ListenAddress string    `json:"listen_address"`
	ListenPort    int       `json:"listen_port"`
	ViaNodeID     string    `json:"via_node_id,omitempty"`
	AutoStart     bool      `json:"auto_start"`
	Status        string    `json:"status"`
	LastError     string    `json:"last_error,omitempty"`
	RuntimePort   int       `json:"runtime_port,omitempty"`
	ViaRoute      string    `json:"via_route,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type AuditEvent struct {
	ID         int64     `json:"id"`
	Time       time.Time `json:"time"`
	Source     string    `json:"source"`
	Operation  string    `json:"operation"`
	NodeID     string    `json:"node_id,omitempty"`
	Capability string    `json:"capability,omitempty"`
	Parameters string    `json:"parameters,omitempty"`
	Result     string    `json:"result"`
	DurationMS int64     `json:"duration_ms"`
}

type FileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
	IsDir   bool      `json:"is_dir"`
}

type ExecResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
}

type Overview struct {
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	Nodes         int    `json:"nodes"`
	Connected     int    `json:"connected"`
	Tunnels       int    `json:"tunnels"`
	Running       int    `json:"running"`
	MCPEnabled    bool   `json:"mcp_enabled"`
}

type SubnetInfo struct {
	Source string `json:"source"`
	NodeID string `json:"node_id,omitempty"`
	Prefix string `json:"prefix"`
}

// RemoteRoute 描述对端可作为跳板的 SSH 节点及其宣告子网。
type RemoteRoute struct {
	NodeID       string   `json:"node_id"`
	Subnets      []string `json:"subnets"`
	Trusted      bool     `json:"trusted"`
	RelayEnabled bool     `json:"relay_enabled"`
}

type RelayRoute struct {
	TargetSubnet string    `json:"target_subnet"`
	ViaNodeID    string    `json:"via_node_id"`
	Hops         []string  `json:"hops"`
	RTTMS        int64     `json:"rtt_ms"`
	Healthy      bool      `json:"healthy"`
	LastChecked  time.Time `json:"last_checked"`
}
