package mcp

import (
	"context"
	"net/http"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/policy"
	"github.com/npa/npa/internal/service"
)

type Manager struct {
	service *service.Service
	mu      sync.Mutex
	server  *mcpsdk.Server
}

func NewManager(app *service.Service) *Manager {
	manager := &Manager{service: app, server: mcpsdk.NewServer(&mcpsdk.Implementation{Name: "NPA", Version: service.Version}, nil)}
	manager.Refresh()
	return manager
}

func (m *Manager) Server() *mcpsdk.Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.server
}

func (m *Manager) Refresh() {
	m.mu.Lock()
	defer m.mu.Unlock()
	capabilities := m.service.MCPCapabilities()
	m.server.RemoveTools(
		"nodes.list", "ssh.exec", "ssh.session_info",
		"file.list", "file.read", "file.write", "file.remove",
		"tunnel.list", "tunnel.open", "tunnel.close", "service.open_web",
	)
	mcpCtx := func(ctx context.Context) context.Context { return policy.WithSource(ctx, policy.SourceMCP) }
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "nodes.list", Description: "List managed nodes"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		nodes, err := m.service.ListNodes()
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"nodes": nodes}, nil
	})
	if capabilities["ssh"] {
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "ssh.exec", Description: "Execute a command on a managed node"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			NodeID         string `json:"node_id" jsonschema:"managed node ID"`
			Command        string `json:"command" jsonschema:"shell command"`
			TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"command timeout in seconds"`
		}) (*mcpsdk.CallToolResult, any, error) {
			timeout := time.Duration(input.TimeoutSeconds) * time.Second
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			result, err := m.service.Exec(mcpCtx(ctx), input.NodeID, input.Command, timeout)
			if err != nil {
				return nil, nil, err
			}
			return nil, result, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "ssh.session_info", Description: "Get SSH connection status for a node"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			NodeID string `json:"node_id" jsonschema:"managed node ID"`
		}) (*mcpsdk.CallToolResult, any, error) {
			if err := m.service.Authorize(mcpCtx(ctx), "ssh.session_info", policy.CapSSH); err != nil {
				return nil, nil, err
			}
			node, err := m.service.GetNode(input.NodeID)
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"id": node.ID, "status": node.Status, "host_key_fingerprint": node.HostKeyFingerprint, "last_error": node.LastError}, nil
		})
	}
	if capabilities["file"] {
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "file.list", Description: "List a remote SFTP directory"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			NodeID string `json:"node_id" jsonschema:"managed node ID"`
			Path   string `json:"path" jsonschema:"remote directory"`
		}) (*mcpsdk.CallToolResult, any, error) {
			entries, err := m.service.FileList(mcpCtx(ctx), input.NodeID, input.Path)
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"entries": entries}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "file.read", Description: "Read a small remote file (4 MiB limit)"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			NodeID string `json:"node_id" jsonschema:"managed node ID"`
			Path   string `json:"path" jsonschema:"remote file path"`
		}) (*mcpsdk.CallToolResult, any, error) {
			data, err := m.service.FileRead(mcpCtx(ctx), input.NodeID, input.Path)
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"path": input.Path, "content": string(data), "size": len(data)}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "file.write", Description: "Write a small text file to a remote node"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			NodeID  string `json:"node_id" jsonschema:"managed node ID"`
			Path    string `json:"path" jsonschema:"remote file path"`
			Content string `json:"content" jsonschema:"file content"`
		}) (*mcpsdk.CallToolResult, any, error) {
			if err := m.service.FileWrite(mcpCtx(ctx), input.NodeID, input.Path, []byte(input.Content)); err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"path": input.Path, "bytes": len(input.Content)}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "file.remove", Description: "Remove a remote file or empty directory"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			NodeID string `json:"node_id" jsonschema:"managed node ID"`
			Path   string `json:"path" jsonschema:"remote path"`
		}) (*mcpsdk.CallToolResult, any, error) {
			if err := m.service.FileRemove(mcpCtx(ctx), input.NodeID, input.Path); err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"removed": input.Path}, nil
		})
	}
	if capabilities["tunnel"] {
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "tunnel.list", Description: "List SSH tunnels"}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			ctx := policy.WithSource(context.Background(), policy.SourceMCP)
			if err := m.service.Authorize(ctx, "tunnel.list", policy.CapTunnel); err != nil {
				return nil, nil, err
			}
			tunnels, err := m.service.ListTunnels()
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"tunnels": tunnels}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "tunnel.open", Description: "Start a saved tunnel"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			TunnelID string `json:"tunnel_id" jsonschema:"saved tunnel ID"`
		}) (*mcpsdk.CallToolResult, any, error) {
			tunnel, err := m.service.StartTunnel(mcpCtx(ctx), input.TunnelID)
			if err != nil {
				return nil, nil, err
			}
			return nil, tunnel, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "tunnel.close", Description: "Stop a running tunnel"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			TunnelID string `json:"tunnel_id" jsonschema:"saved tunnel ID"`
		}) (*mcpsdk.CallToolResult, any, error) {
			if err := m.service.StopTunnel(mcpCtx(ctx), input.TunnelID); err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"stopped": input.TunnelID}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "service.open_web", Description: "Open a node web service through the controlled proxy"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			NodeID     string `json:"node_id" jsonschema:"managed node ID"`
			TargetHost string `json:"target_host" jsonschema:"host reachable from the node"`
			TargetPort int    `json:"target_port" jsonschema:"target TCP port"`
		}) (*mcpsdk.CallToolResult, any, error) {
			path, err := m.service.OpenWeb(mcpCtx(ctx), input.NodeID, input.TargetHost, input.TargetPort)
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"path": path, "url": "http://127.0.0.1:56700" + path}, nil
		})
	}
}

func (m *Manager) Handler() http.Handler {
	return mcpsdk.NewStreamableHTTPHandler(func(_ *http.Request) *mcpsdk.Server {
		return m.Server()
	}, &mcpsdk.StreamableHTTPOptions{JSONResponse: false})
}

func (m *Manager) Status() map[string]any {
	settings := m.service.Settings()
	return map[string]any{
		"enabled":  settings["mcp_enabled"] == "true",
		"ssh":      settings["mcp_ssh"] == "true",
		"file":     settings["mcp_file"] == "true",
		"tunnel":   settings["mcp_tunnel"] == "true",
		"endpoint": "http://127.0.0.1:56701/mcp",
	}
}

var _ = model.Overview{}
