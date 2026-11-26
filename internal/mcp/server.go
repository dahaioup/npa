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
	// browserTools 记录本次注册挂上去的浏览器工具名，
	// 下次 Refresh 时按名字摘掉（工具清单来自 obscura，数量不定）。
	browserTools []string
	// face 区分这个 MCP 面：local（本机 56700）与 mesh（对外 56701）。
	// mesh 面会剔除被关掉 MCP 的插件能力。
	face service.MCPFace
}

// MCPFace 是 MCP 面的标识。
type MCPFace = service.MCPFace

const (
	// FaceLocal 是本地面（56700）。
	FaceLocal = service.MCPFaceLocal
	// FaceMesh 是 mesh 面（56701）。
	FaceMesh = service.MCPFaceMesh
)

func NewManager(app *service.Service) *Manager {
	return NewManagerFor(app, FaceLocal)
}

// NewManagerFor 建一个指定面的 MCP 管理器。
func NewManagerFor(app *service.Service, face MCPFace) *Manager {
	manager := &Manager{service: app, face: face, server: mcpsdk.NewServer(&mcpsdk.Implementation{Name: "NPA", Version: service.Version}, nil)}
	manager.Refresh()
	return manager
}

// Face 返回这个管理器的面。
func (m *Manager) Face() MCPFace { return m.face }

func (m *Manager) Server() *mcpsdk.Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.server
}

func (m *Manager) Refresh() {
	m.mu.Lock()
	defer m.mu.Unlock()
	capabilities := m.service.MCPCapabilitiesFor(m.face)
	m.server.RemoveTools(
		"nodes.list", "ssh.exec", "ssh.session_info",
		"file.list", "file.read", "file.write", "file.remove",
		"channel.list", "channel.open", "channel.close", "service.open_web",
		"desktop.screens", "desktop.screenshot", "desktop.mouse_move", "desktop.mouse_click", "desktop.mouse_scroll",
		"discovery.nodes", "discovery.refresh", "network.subnets", "relay.routes", "relay.open",
		"screen.read", "screen.type", "screen.click", "screen.hotkey", "screen.info", "screen.move",
		"clipboard.get", "clipboard.set", "upgrade.apply",
		"upgrade.guide",
		"plugin.list", "plugin.install", "plugin.remove", "plugin.update",
	)
	// 浏览器工具数量不定（跟着 obscura 的版本走），按上次注册的名字摘掉。
	if len(m.browserTools) > 0 {
		m.server.RemoveTools(m.browserTools...)
		m.browserTools = nil
	}
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
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "channel.list", Description: "List channels (ssh tunnels and npa port mappings)"}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			ctx := policy.WithSource(context.Background(), policy.SourceMCP)
			if err := m.service.Authorize(ctx, "channel.list", policy.CapTunnel); err != nil {
				return nil, nil, err
			}
			channels, err := m.service.ListChannels(ctx)
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"channels": channels}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "channel.open", Description: "Start a saved channel"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			ChannelID string `json:"channel_id" jsonschema:"saved channel ID (ssh:<id> or npa:<id>)"`
		}) (*mcpsdk.CallToolResult, any, error) {
			channel, err := m.service.StartChannel(mcpCtx(ctx), input.ChannelID)
			if err != nil {
				return nil, nil, err
			}
			return nil, channel, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "channel.close", Description: "Stop a running channel"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			ChannelID string `json:"channel_id" jsonschema:"saved channel ID (ssh:<id> or npa:<id>)"`
		}) (*mcpsdk.CallToolResult, any, error) {
			if err := m.service.StopChannel(mcpCtx(ctx), input.ChannelID); err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"stopped": input.ChannelID}, nil
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
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "desktop.screens", Description: "List local screens and display geometry (desktop plugin)"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			screens, err := m.service.Screens(mcpCtx(ctx))
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"screens": screens}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "desktop.screenshot", Description: "Capture a screenshot through the desktop plugin (npa-remote). The plugin must be running"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
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
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "desktop.mouse_move", Description: "Move the local mouse cursor (desktop plugin)"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			X        int  `json:"x" jsonschema:"absolute X coordinate"`
			Y        int  `json:"y" jsonschema:"absolute Y coordinate"`
			Relative bool `json:"relative,omitempty" jsonschema:"move relative to current position"`
		}) (*mcpsdk.CallToolResult, any, error) {
			if err := m.service.MoveMouse(mcpCtx(ctx), desktop.MoveRequest{X: input.X, Y: input.Y, Relative: input.Relative}); err != nil {
				return nil, nil, err
			}
			return nil, map[string]bool{"moved": true}, nil
		})
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "desktop.mouse_click", Description: "Click a local mouse button (desktop plugin)"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
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
		mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "desktop.mouse_scroll", Description: "Scroll the local mouse wheel (desktop plugin)"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
			DeltaX int `json:"delta_x,omitempty" jsonschema:"horizontal scroll delta"`
			DeltaY int `json:"delta_y" jsonschema:"vertical scroll delta"`
		}) (*mcpsdk.CallToolResult, any, error) {
			if err := m.service.Scroll(mcpCtx(ctx), desktop.ScrollRequest{DeltaX: input.DeltaX, DeltaY: input.DeltaY}); err != nil {
				return nil, nil, err
			}
			return nil, map[string]bool{"scrolled": true}, nil
		})
	}
	// screen.* 工具：任意 NPA 节点（含本机，node_id 传空或本机 ID）的读屏/输入/点击。
	// 权限沿用 screenshot（读）与 mouse（写）；不依赖 desktop 能力开关，因为走的是节点 mesh 面。
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "screen.read", Description: "Read text from a node's screen via the desktop plugin's OCR (the plugin must declare desktop.read). Always returns status and the screenshot file path, even when OCR fails. Combine with screen.click using returned line coordinates."}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		NodeID   string `json:"node_id" jsonschema:"target node ID; empty or self ID means this machine"`
		ScreenID string `json:"screen_id,omitempty" jsonschema:"screen ID; empty means primary screen"`
		Region   *struct {
			X int `json:"x"`
			Y int `json:"y"`
			W int `json:"w"`
			H int `json:"h"`
		} `json:"region,omitempty" jsonschema:"pixel region to capture; empty means full screen"`
		Lang string `json:"lang,omitempty" jsonschema:"OCR language hint, e.g. zh-Hans, en-US, chi_sim+eng"`
	}) (*mcpsdk.CallToolResult, any, error) {
		request := desktop.ReadRequest{ScreenID: input.ScreenID, Lang: input.Lang}
		if input.Region != nil {
			request.Region = &desktop.Region{X: input.Region.X, Y: input.Region.Y, W: input.Region.W, H: input.Region.H}
		}
		result, err := m.service.ReadScreen(mcpCtx(ctx), input.NodeID, request)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "screen.type", Description: "Type text into a node's keyboard through the desktop plugin (physical key events, IME-friendly)"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		NodeID string `json:"node_id" jsonschema:"target node ID; empty or self ID means this machine"`
		Text   string `json:"text" jsonschema:"text to type (max 4000 chars per call)"`
	}) (*mcpsdk.CallToolResult, any, error) {
		if err := m.service.TypeText(mcpCtx(ctx), input.NodeID, input.Text); err != nil {
			return nil, nil, err
		}
		return nil, map[string]bool{"typed": true}, nil
	})
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "screen.click", Description: "Move the node's mouse to coordinates and click via the desktop plugin (use coordinates returned by screen.read)"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		NodeID string `json:"node_id" jsonschema:"target node ID; empty or self ID means this machine"`
		X      int    `json:"x" jsonschema:"absolute X coordinate"`
		Y      int    `json:"y" jsonschema:"absolute Y coordinate"`
		Button string `json:"button,omitempty" jsonschema:"left (default), right or middle"`
	}) (*mcpsdk.CallToolResult, any, error) {
		if err := m.service.ClickScreen(mcpCtx(ctx), input.NodeID, input.X, input.Y, input.Button); err != nil {
			return nil, nil, err
		}
		return nil, map[string]bool{"clicked": true}, nil
	})
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "screen.hotkey", Description: "Send a key combo to a node via the desktop plugin (ctrl+v, ctrl+shift+esc, alt+f4 ...). Pair with clipboard.set to paste"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		NodeID string `json:"node_id" jsonschema:"target node ID; empty or self ID means this machine"`
		Combo  string `json:"combo" jsonschema:"modifier+key, e.g. ctrl+v or ctrl+shift+esc"`
	}) (*mcpsdk.CallToolResult, any, error) {
		if err := m.service.HotkeyScreen(mcpCtx(ctx), input.NodeID, input.Combo); err != nil {
			return nil, nil, err
		}
		return nil, map[string]bool{"sent": true}, nil
	})
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "clipboard.get", Description: "Read the node's clipboard text (requires the npa-remote desktop plugin on the node)"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		NodeID string `json:"node_id" jsonschema:"target node ID; empty or self ID means this machine"`
	}) (*mcpsdk.CallToolResult, any, error) {
		result, err := m.service.ClipboardGetScreen(mcpCtx(ctx), input.NodeID)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "clipboard.set", Description: "Write text to the node's clipboard (requires the npa-remote desktop plugin on the node). Pair with screen.hotkey ctrl+v to paste"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		NodeID string `json:"node_id" jsonschema:"target node ID; empty or self ID means this machine"`
		Text   string `json:"text" jsonschema:"text to place on the clipboard"`
	}) (*mcpsdk.CallToolResult, any, error) {
		if err := m.service.ClipboardSetScreen(mcpCtx(ctx), input.NodeID, input.Text); err != nil {
			return nil, nil, err
		}
		return nil, map[string]bool{"written": true}, nil
	})
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "screen.info", Description: "Query a node's screen basics: mouse position, screen resolutions, foreground window rectangle+title. Use before screen.click to pick valid coordinates"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		NodeID string `json:"node_id" jsonschema:"target node ID; empty or self ID means this machine"`
	}) (*mcpsdk.CallToolResult, any, error) {
		result, err := m.service.ScreenInfo(mcpCtx(ctx), input.NodeID)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "screen.move", Description: "Move a node's mouse to coordinates without clicking"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		NodeID   string `json:"node_id" jsonschema:"target node ID; empty or self ID means this machine"`
		X        int    `json:"x" jsonschema:"target x in pixels"`
		Y        int    `json:"y" jsonschema:"target y in pixels"`
		Relative bool   `json:"relative,omitempty" jsonschema:"move relative to current position"`
	}) (*mcpsdk.CallToolResult, any, error) {
		if err := m.service.MoveScreen(mcpCtx(ctx), input.NodeID, input.X, input.Y, input.Relative); err != nil {
			return nil, nil, err
		}
		return nil, map[string]bool{"moved": true}, nil
	})
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "upgrade.apply", Description: "Atomic remote upgrade: tell the node where the new package is (http url, zip or raw binary); the node downloads, verifies sha256, picks the platform binary, launches its guardian to swap+restart+rollback. All steps run inside NPA"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		NodeID          string `json:"node_id" jsonschema:"target node ID; empty or self ID means this machine"`
		PackageURL      string `json:"package_url" jsonschema:"http(s) url of the upgrade package (npa-all_*.zip or npa-<os>-<arch> binary)"`
		SHA256          string `json:"sha256" jsonschema:"expected sha256 hex of the package (recommended)"`
		ExpectedVersion string `json:"expected_version" jsonschema:"version the node should report after upgrade, e.g. 0.4.1-1793587576"`
	}) (*mcpsdk.CallToolResult, any, error) {
		result, err := m.service.UpgradeApply(mcpCtx(ctx), input.NodeID, input.PackageURL, input.SHA256, input.ExpectedVersion)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})
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

	// browser.* 工具：obscura 无头浏览器的工具原样挂上来（工具名与 schema 都不改），
	// 每个工具多一个 node_id 参数，用来决定这次调用在哪台 NPA 节点上跑。
	m.registerBrowserTools(capabilities)
	m.registerPluginTools(capabilities)
}

func (m *Manager) Handler() http.Handler {
	return mcpsdk.NewStreamableHTTPHandler(func(_ *http.Request) *mcpsdk.Server {
		return m.Server()
	}, &mcpsdk.StreamableHTTPOptions{JSONResponse: false})
}

func (m *Manager) Status() map[string]any {
	settings := m.service.Settings()
	capabilities := m.service.MCPCapabilitiesFor(m.face)
	return map[string]any{
		"enabled":        settings["mcp_enabled"] == "true",
		"ssh":            capabilities["ssh"],
		"file":           capabilities["file"],
		"tunnel":         capabilities["tunnel"],
		"screenshot":     capabilities["screenshot"],
		"mouse":          capabilities["mouse"],
		"discovery":      capabilities["discovery"],
		"relay":          capabilities["relay"],
		"browser":        capabilities["browser"],
		"plugin":         capabilities["plugin"],
		"bind_scope":     meshBindScope(settings),
		"endpoint":       "http://127.0.0.1:56701/mcp",
		"local_endpoint": "http://127.0.0.1:56700/mcp",
		"face":           string(m.face),
	}
}

// meshBindScope 报告 56701 的监听范围：跟随局域网服务开关。
func meshBindScope(settings map[string]string) string {
	if settings["lan_enabled"] == "false" {
		return "local"
	}
	return "lan"
}

var _ = model.Overview{}
