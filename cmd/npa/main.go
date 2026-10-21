package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/npa/npa/internal/mcp"
	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
	"github.com/npa/npa/internal/web"
)

var version = service.Version

// listenerSupervisor 管理一个可按设置热切换监听范围的 HTTP 端口（MCP 与 peer 复用）。
// scope 为空字符串表示关闭该监听。
type listenerSupervisor struct {
	name    string
	handler http.Handler
	address func(scope string) string
	mu      sync.Mutex
	server  *http.Server
	scope   string
}

func mcpAddress(scope string) string {
	if scope == "lan" {
		return "0.0.0.0:56701"
	}
	return "127.0.0.1:56701"
}

func peerAddress(settings map[string]string) string {
	if settings["peer_enabled"] != "true" {
		return ""
	}
	port := settings["peer_port"]
	if port == "" {
		port = "56710"
	}
	host := "127.0.0.1"
	if settings["peer_bind_scope"] == "lan" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, port)
}

func (s *listenerSupervisor) currentScope() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scope
}

func (s *listenerSupervisor) restart(scope string) error {
	s.mu.Lock()
	if s.scope == scope && (s.server != nil || scope == "") {
		s.mu.Unlock()
		return nil
	}
	old := s.server
	s.server = nil
	s.scope = scope
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	if scope == "" {
		slog.Info(s.name + " listener stopped")
		return nil
	}
	address := s.address(scope)
	if address == "" {
		return nil
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second}
	s.mu.Lock()
	s.server = server
	s.mu.Unlock()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error(s.name+" server stopped", "address", address, "error", err)
		}
	}()
	slog.Info(s.name+" server listening", "address", address)
	return nil
}

func (s *listenerSupervisor) shutdown(ctx context.Context) error {
	s.mu.Lock()
	server := s.server
	s.server = nil
	s.scope = ""
	s.mu.Unlock()
	if server == nil {
		return nil
	}
	return server.Shutdown(ctx)
}

func main() {
	dataDir := flag.String("data-dir", "", "internal state directory (default ~/.npa)")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	level := slog.LevelInfo
	switch strings.ToLower(*logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	// 日志同时写控制台与文件：前台模式两者都有，隐藏控制台（后台）模式只剩文件
	slog.SetDefault(slog.New(slog.NewTextHandler(logWriter(dataDirPath(*dataDir)), &slog.HandlerOptions{Level: level})))

	store, err := state.Open(*dataDir)
	if err != nil {
		slog.Error("cannot open state database", "error", err)
		os.Exit(1)
	}
	app := service.New(store)
	mcpManager := mcp.NewManager(app)
	mcpHTTPHandler := mcpManager.Handler()
	mcpSupervisor := &listenerSupervisor{name: "MCP", address: mcpAddress, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.Settings()["mcp_enabled"] != "true" {
			http.Error(w, "MCP service is disabled", http.StatusForbidden)
			return
		}
		mcpHTTPHandler.ServeHTTP(w, r)
	})}
	webHandler := web.New(app, mcpManager)
	webHandler.ShutdownFunc = requestShutdown // Web「退出服务」按钮走这条优雅关闭路径
	peerSupervisor := &listenerSupervisor{
		name:    "peer",
		handler: webHandler.PeerHandler(),
		address: func(string) string { return peerAddress(app.Settings()) },
	}
	app.OnSettingsChanged = func() {
		mcpManager.Refresh()
		if err := mcpSupervisor.restart(app.Settings()["mcp_bind_scope"]); err != nil {
			slog.Error("cannot restart MCP listener", "scope", app.Settings()["mcp_bind_scope"], "error", err)
		}
		if err := peerSupervisor.restart(peerScope(app.Settings())); err != nil {
			slog.Error("cannot restart peer listener", "scope", app.Settings()["peer_bind_scope"], "error", err)
		}
	}
	app.ApplyRuntimeSettings(context.Background())
	if err := peerSupervisor.restart(peerScope(app.Settings())); err != nil {
		slog.Error("cannot start peer listener", "scope", app.Settings()["peer_bind_scope"], "error", err)
	}
	go func() {
		if err := app.Plugin.Start(context.Background()); err != nil {
			slog.Warn("desktop plugin unavailable; using embedded fallback", "error", err)
		}
	}()
	if days := app.Settings()["audit_retention_days"]; days != "" {
		_ = app.Audit.Prune(parseInt(days))
	}

	webServer := &http.Server{
		Addr:              "127.0.0.1:56700",
		Handler:           webHandler.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	webErr := make(chan error, 1)
	go func() {
		slog.Info("Web server listening", "address", "http://127.0.0.1:56700")
		webErr <- webServer.ListenAndServe()
	}()

	if app.Settings()["restore_tunnels"] == "true" {
		go func() {
			if err := app.Tunnels.Restore(context.Background()); err != nil {
				slog.Warn("tunnel restore failed", "error", err)
			}
		}()
	}
	if app.Settings()["restore_port_mappings"] == "true" {
		go func() {
			if err := app.PortMappings.Restore(context.Background()); err != nil {
				slog.Warn("port mapping restore failed", "error", err)
			}
		}()
	}
	slog.Info("NPA ready", "version", version)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case <-stop:
		slog.Info("shutdown requested by signal")
	case <-stopSignal:
		slog.Info("shutdown requested from web")
	case err := <-webErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("web server stopped", "error", err)
		}
	}
	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = mcpSupervisor.shutdown(ctx)
	_ = peerSupervisor.shutdown(ctx)
	_ = webServer.Shutdown(ctx)
	app.Close()
}

func peerScope(settings map[string]string) string {
	if settings["peer_enabled"] != "true" {
		return ""
	}
	return settings["peer_bind_scope"]
}

// stopSignal 让 Web「退出服务」与 Ctrl+C 走同一条优雅关闭路径。
var stopSignal = make(chan struct{}, 1)

func requestShutdown() {
	select {
	case stopSignal <- struct{}{}:
	default: // 已经在关闭流程里
	}
}

const logRotateSize = 10 << 20

func dataDirPath(flagValue string) string {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value
	}
	dir, err := state.DefaultDataDir()
	if err != nil {
		return "."
	}
	return dir
}

// logWriter 同时输出到控制台与日志文件：前台模式两者都有，
// 隐藏控制台（后台）模式只剩文件，所以文件日志必须始终存在。
func logWriter(dataDir string) io.Writer {
	full := filepath.Join(dataDir, "npa.log")
	rotateIfNeeded(full)
	file, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Error("cannot open log file, console only", "path", full, "error", err)
		return os.Stderr
	}
	return io.MultiWriter(os.Stderr, file)
}

func rotateIfNeeded(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < logRotateSize {
		return
	}
	_ = os.Remove(path + ".1")
	_ = os.Rename(path, path+".1")
}

func parseInt(value string) int {
	var result int
	_, _ = fmt.Sscanf(value, "%d", &result)
	return result
}
