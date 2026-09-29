package model

import "time"

type Node struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Address            string    `json:"address"`
	Port               int       `json:"port"`
	Username           string    `json:"username"`
	AuthMethod         string    `json:"auth_method"`
	Protocol           string    `json:"protocol"`
	Tags               []string  `json:"tags"`
	Status             string    `json:"status"`
	LastError          string    `json:"last_error,omitempty"`
	HostKeyFingerprint string    `json:"host_key_fingerprint,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
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
