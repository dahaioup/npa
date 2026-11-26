package main

// 插件自己的 HTTP 服务。
//
//	GET    /                    管理页：下载安装 obscura、预览页面
//	GET    /api/status          引擎状态（是否装好、版本、工具数……）
//	POST   /api/install         下载安装 obscura（{"variant":"stealth"}）
//	GET    /api/preview?url=... 页面预览（image/png）
//	POST/GET/DELETE /mcp        插件自己的 MCP 服务（obscura 的 38 个工具）
//
// 插件默认只绑 127.0.0.1：别的机器要用它的 MCP，走 NPA 的代理——
// NPA 56701 /plugins/browser/mcp → http://127.0.0.1:<port>/mcp。

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/npa/npa/internal/browser"
)

//go:embed page.html
var pageHTML []byte

// httpService 是插件的 HTTP 服务。
type httpService struct {
	engine    *engine
	installer *browser.Installer
	port      int
	bind      string

	mu         sync.Mutex
	installing bool
	lastError  string

	server   *http.Server
	listener net.Listener
}

// newHTTPService 造插件的 HTTP 服务。
func newHTTPService(engine *engine, dataDir string, port int, bind string) *httpService {
	if bind == "" {
		bind = "127.0.0.1"
	}
	return &httpService{
		engine:    engine,
		installer: &browser.Installer{DataDir: dataDir},
		port:      port,
		bind:      bind,
	}
}

// addr 是监听地址。
func (s *httpService) addr() string {
	return net.JoinHostPort(s.bind, strconv.Itoa(s.port))
}

// start 监听并开始服务。
func (s *httpService) start(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.addr())
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr(), err)
	}
	s.listener = listener
	s.server = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("plugin http server stopped", "error", err)
		}
	}()
	slog.Info("plugin http service started", "addr", s.addr())
	return nil
}

// close 停掉 HTTP 服务。
func (s *httpService) close(ctx context.Context) {
	if s.server == nil {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = s.server.Shutdown(shutdownCtx)
	s.server = nil
	s.listener = nil
}

// handler 组装路由。
func (s *httpService) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/install", s.handleInstall)
	mux.HandleFunc("GET /api/preview", s.handlePreview)
	// 插件自己的 MCP：NPA 通过反向代理把它挂到自己的面上。
	mux.Handle("/mcp", s.engine.mcpHandler())
	return mux
}

// handlePage 返回管理页。
func (s *httpService) handlePage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(pageHTML)
}

// statusPayload 是管理页读到的状态。
type statusPayload struct {
	Engine     string `json:"engine"`
	Installed  bool   `json:"installed"`
	Version    string `json:"version"`
	Path       string `json:"path"`
	Variant    string `json:"variant"`
	Asset      string `json:"asset,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Installing bool   `json:"installing"`
	LastError  string `json:"last_error,omitempty"`
	Tools      int    `json:"tools"`
	Bind       string `json:"bind"`
	Port       int    `json:"port"`
	MCPPath    string `json:"mcp_path"`
	ProxyPath  string `json:"proxy_path"`
}

// handleStatus 返回引擎与安装状态。
func (s *httpService) handleStatus(w http.ResponseWriter, _ *http.Request) {
	payload := s.status()
	writeJSON(w, http.StatusOK, payload)
}

// status 组装状态。
func (s *httpService) status() statusPayload {
	install := s.installer.Status()
	s.mu.Lock()
	installing := s.installing
	lastError := s.lastError
	s.mu.Unlock()
	if lastError == "" {
		lastError = install.LastError
	}
	tools := 0
	if list := s.engine.toolNames(); list != nil {
		tools = len(list)
	}
	return statusPayload{
		Engine:     "obscura",
		Installed:  install.Installed,
		Version:    install.Version,
		Path:       install.Path,
		Variant:    install.Variant,
		Asset:      install.Asset,
		Size:       install.Size,
		Installing: installing,
		LastError:  lastError,
		Tools:      tools,
		Bind:       s.bind,
		Port:       s.port,
		MCPPath:    "/mcp",
		ProxyPath:  "/plugins/browser/mcp",
	}
}

// handleInstall 下载安装 obscura。
func (s *httpService) handleInstall(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Variant string `json:"variant"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	variant := request.Variant
	if variant == "" {
		variant = s.engine.settingValue("variant")
	}
	if !browser.Variants[variant] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown variant: " + variant})
		return
	}

	s.mu.Lock()
	if s.installing {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "安装正在进行中"})
		return
	}
	s.installing = true
	s.lastError = ""
	s.mu.Unlock()

	s.installer.Variant = variant
	result, err := s.installer.Install(r.Context())

	s.mu.Lock()
	s.installing = false
	if err != nil {
		s.lastError = err.Error()
	}
	s.mu.Unlock()

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 装完立刻让引擎用上新二进制。
	s.engine.setBinaryPath(result.Path)
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    result.Path,
		"version": result.Version,
		"asset":   result.Asset,
		"size":    result.Size,
	})
}

// handlePreview 抓一个页面并返回截图。
func (s *httpService) handlePreview(w http.ResponseWriter, r *http.Request) {
	url := r.URL.Query().Get("url")
	if url == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 url 参数"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.engine.callTimeout)
	defer cancel()
	if _, err := s.engine.callTool(ctx, "browser_navigate", map[string]any{"url": url}); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	result, err := s.engine.callTool(ctx, "browser_screenshot", map[string]any{"full_page": false})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	for _, content := range result.Content {
		if content.Type != "image" || content.Data == "" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(content.Data)
		if err != nil {
			continue
		}
		mime := content.MimeType
		if mime == "" {
			mime = "image/png"
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": "引擎没有返回截图"})
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// mcpHandler 返回插件自己的 MCP 处理器（Streamable HTTP）。
func (e *engine) mcpHandler() http.Handler {
	return mcpsdk.NewStreamableHTTPHandler(func(r *http.Request) *mcpsdk.Server {
		return e.mcpServer(r.Context())
	}, nil)
}

// mcpServer 造（或复用）插件自己的 MCP server。
//
// 工具清单优先用引擎当前暴露的（跟着 obscura 版本走），引擎没起来就用内嵌目录。
func (e *engine) mcpServer(ctx context.Context) *mcpsdk.Server {
	e.mu.Lock()
	server := e.mcp
	e.mu.Unlock()
	if server != nil {
		return server
	}
	tools := browser.Catalog()
	if session, err := e.sessionFor(ctx, e.currentConfig()); err == nil {
		if list, err := session.ListTools(ctx, nil); err == nil && len(list.Tools) > 0 {
			tools = toolsFromSession(list.Tools)
		}
	}
	server = e.buildMCPServer(tools)
	e.mu.Lock()
	e.mcp = server
	e.mu.Unlock()
	return server
}

// buildMCPServer 按工具清单注册 MCP 工具。
func (e *engine) buildMCPServer(tools []browser.Tool) *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: pluginID, Version: pluginVersion}, nil)
	for _, tool := range tools {
		current := tool
		server.AddTool(&mcpsdk.Tool{
			Name:        current.Name,
			Description: current.Description,
			InputSchema: current.InputSchema,
		}, func(ctx context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			arguments := map[string]any{}
			if len(request.Params.Arguments) > 0 {
				if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
					return nil, fmt.Errorf("invalid arguments for %s: %w", current.Name, err)
				}
			}
			result, err := e.callTool(ctx, current.Name, arguments)
			if err != nil {
				return nil, err
			}
			content := make([]mcpsdk.Content, 0, len(result.Content))
			for _, item := range result.Content {
				content = append(content, convertToMCP(item))
			}
			return &mcpsdk.CallToolResult{Content: content, IsError: result.IsError}, nil
		})
	}
	return server
}

// toolsFromSession 把 MCP 的工具清单转成 NPA 的中立表示。
func toolsFromSession(list []*mcpsdk.Tool) []browser.Tool {
	tools := make([]browser.Tool, 0, len(list))
	for _, tool := range list {
		if tool == nil {
			continue
		}
		tools = append(tools, browser.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: marshalSchema(tool.InputSchema),
		})
	}
	return tools
}

// convertToMCP 把 NPA 的内容块转成 MCP 内容块。
func convertToMCP(content browser.Content) mcpsdk.Content {
	switch content.Type {
	case "image":
		data, err := base64.StdEncoding.DecodeString(content.Data)
		if err != nil {
			return &mcpsdk.TextContent{Text: "image decode failed: " + err.Error()}
		}
		mime := content.MimeType
		if mime == "" {
			mime = "image/png"
		}
		return &mcpsdk.ImageContent{Data: data, MIMEType: mime}
	case "resource":
		blob, _ := base64.StdEncoding.DecodeString(content.Data)
		return &mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{
			URI:      content.URI,
			MIMEType: content.MimeType,
			Text:     content.Text,
			Blob:     blob,
		}}
	default:
		return &mcpsdk.TextContent{Text: content.Text}
	}
}
