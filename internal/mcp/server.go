package mcp

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/npa/npa/internal/desktop"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/policy"
	"github.com/npa/npa/internal/selfupdate"
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
		"desktop.screens", "desktop.screenshot", "desktop.mouse_move", "desktop.mouse_click", "desktop.mouse_scroll",
		"discovery.nodes", "discovery.refresh", "network.subnets", "relay.routes", "relay.open",
		"upgrade.guide",
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
	if capabilities["screenshot"] {
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "desktop.screens", Description: "List local screens and display geometry"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			screens, err := m.service.Screens(mcpCtx(ctx))
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"screens": screens}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "desktop.screenshot", Description: "Capture a PNG screenshot through the desktop provider"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			ScreenID string `json:"screen_id,omitempty" jsonschema:"screen ID; empty means primary screen"`
		}) (*mcpsdk.CallToolResult, any, error) {
			result, err := m.service.Screenshot(mcpCtx(ctx), desktop.CaptureRequest{ScreenID: input.ScreenID})
			if err != nil {
				return nil, nil, err
			}
			return nil, result, nil
		})
	}
	if capabilities["mouse"] {
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "desktop.mouse_move", Description: "Move the local mouse cursor"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			X        int  `json:"x" jsonschema:"absolute X coordinate"`
			Y        int  `json:"y" jsonschema:"absolute Y coordinate"`
			Relative bool `json:"relative,omitempty" jsonschema:"move relative to current position"`
		}) (*mcpsdk.CallToolResult, any, error) {
			if err := m.service.MoveMouse(mcpCtx(ctx), desktop.MoveRequest{X: input.X, Y: input.Y, Relative: input.Relative}); err != nil {
				return nil, nil, err
			}
			return nil, map[string]bool{"moved": true}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "desktop.mouse_click", Description: "Click a local mouse button"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			Button string `json:"button" jsonschema:"left, right or middle"`
			Action string `json:"action,omitempty" jsonschema:"click, down or up"`
		}) (*mcpsdk.CallToolResult, any, error) {
			action := input.Action
			if action == "" {
				action = "click"
			}
			if err := m.service.MouseButton(mcpCtx(ctx), desktop.ButtonRequest{Button: input.Button, Action: action}); err != nil {
				return nil, nil, err
			}
			return nil, map[string]bool{"clicked": true}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "desktop.mouse_scroll", Description: "Scroll the local mouse wheel"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			DeltaX int `json:"delta_x,omitempty" jsonschema:"horizontal scroll delta"`
			DeltaY int `json:"delta_y" jsonschema:"vertical scroll delta"`
		}) (*mcpsdk.CallToolResult, any, error) {
			if err := m.service.Scroll(mcpCtx(ctx), desktop.ScrollRequest{DeltaX: input.DeltaX, DeltaY: input.DeltaY}); err != nil {
				return nil, nil, err
			}
			return nil, map[string]bool{"scrolled": true}, nil
		})
	}
	if capabilities["discovery"] {
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "discovery.nodes", Description: "List nodes discovered through mDNS"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			nodes, err := m.service.DiscoveryNodes(mcpCtx(ctx))
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"nodes": nodes}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "discovery.refresh", Description: "Refresh the mDNS discovery browser"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			if err := m.service.DiscoveryRefresh(mcpCtx(ctx)); err != nil {
				return nil, nil, err
			}
			return nil, map[string]bool{"refreshed": true}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "network.subnets", Description: "List local and discovered reachable subnets"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			subnets, err := m.service.NetworkSubnets(mcpCtx(ctx))
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"subnets": subnets}, nil
		})
	}
	if capabilities["relay"] {
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "relay.routes", Description: "List persisted relay routes"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			routes, err := m.service.RelayRoutes(mcpCtx(ctx))
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"routes": routes}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "relay.open", Description: "Select an authorized one-hop relay route"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			Target           string `json:"target" jsonschema:"target host:port"`
			TargetCapability string `json:"target_capability,omitempty" jsonschema:"tunnel, ssh or file"`
		}) (*mcpsdk.CallToolResult, any, error) {
			capability := policy.CapTunnel
			switch strings.ToLower(input.TargetCapability) {
			case "", "tunnel":
				capability = policy.CapTunnel
			case "ssh":
				capability = policy.CapSSH
			case "file":
				capability = policy.CapFile
			default:
				return nil, nil, apperrors.New(apperrors.InvalidArgument, "unsupported target capability")
			}
			route, err := m.service.RelayOpen(mcpCtx(ctx), input.Target, capability)
			if err != nil {
				return nil, nil, err
			}
			return nil, route, nil
		})
	}

	// upgrade.guide：远程升级手册。不受能力开关限制——其它 AI 程序连上
	// 任意节点的 MCP 就能读到升级流程与坑，不需要翻仓库文档。
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "upgrade.guide", Description: "Full runbook for remotely upgrading NPA nodes (guardian mode): steps, endpoints and known pitfalls"}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		return nil, map[string]any{"runbook": selfupdate.Guide(), "version": service.Version}, nil
	})
}

func (m *Manager) Handler() http.Handler {
	return mcpsdk.NewStreamableHTTPHandler(func(_ *http.Request) *mcpsdk.Server {
		return m.Server()
	}, &mcpsdk.StreamableHTTPOptions{JSONResponse: false})
}

func (m *Manager) Status() map[string]any {
	settings := m.service.Settings()
	capabilities := m.service.MCPCapabilities()
	return map[string]any{
		"enabled":    settings["mcp_enabled"] == "true",
		"ssh":        capabilities["ssh"],
		"file":       capabilities["file"],
		"tunnel":     capabilities["tunnel"],
		"screenshot": capabilities["screenshot"],
		"mouse":      capabilities["mouse"],
		"discovery":  capabilities["discovery"],
		"relay":      capabilities["relay"],
		"bind_scope": settings["mcp_bind_scope"],
		"endpoint":   "http://127.0.0.1:56701/mcp",
	}
}

var _ = model.Overview{}
