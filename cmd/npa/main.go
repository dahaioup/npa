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

	"github.com/npa/npa/internal/guardian"
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

// meshAddress 返回节点通信端口（56701）的监听地址：MCP 与节点间通信（peer）
// 共用这一个端口，按路径分发。scope 为 lan 时全网卡，否则仅回环。
func meshAddress(scope string) string {
	if scope == "lan" {
		return "0.0.0.0:56701"
	}
	return "127.0.0.1:56701"
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
	guardianConfig := flag.String("guardian-config", "", "run in guardian mode (remote-upgrade watchdog) with the given JSON config file, then exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	// 守护模式：只做远程升级的看门狗，不启动完整服务。放在最前面，
	// 避免为守护进程打开状态库/监听端口。
	if *guardianConfig != "" {
		setupGuardianLogger(*dataDir)
		cfg, err := guardian.LoadFile(*guardianConfig)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guardian config:", err)
			os.Exit(2)
		}
		os.Exit(int(guardian.Run(cfg)))
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
	webHandler := web.New(app, mcpManager)
	peerHandler := webHandler.PeerHandler()
	// 56701 是节点间通信与 MCP 的公共入口：/mcp 走 MCP，/api/peers/*、/api/info、
	// /api/topology 走 peer。两个功能各自受自己的开关门控，端口本身按
	// mcp_bind_scope ∪ peer_bind_scope 取更宽的范围监听。
	meshSupervisor := &listenerSupervisor{name: "mesh", address: meshAddress, handler: web.RecoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			if app.Settings()["mcp_enabled"] != "true" {
				http.Error(w, "MCP service is disabled", http.StatusForbidden)
				return
			}
			mcpHTTPHandler.ServeHTTP(w, r)
			return
		}
		// 一次性下载端点：MCP 自升级通道用（token 一次性、5 分钟过期）。
		// 它必须在 peer 开关门控之外：下载发生在 peer 被关的节点上也该可用。
		if strings.HasPrefix(r.URL.Path, "/download/") {
			app.ServeDownload(w, r)
			return
		}
		if app.Settings()["peer_enabled"] != "true" {
			http.Error(w, "peer service is disabled", http.StatusForbidden)
			return
		}
		peerHandler.ServeHTTP(w, r)
	}))}
	webHandler.ShutdownFunc = requestShutdown // Web「退出服务」按钮走这条优雅关闭路径
	app.OnSettingsChanged = func() {
		mcpManager.Refresh()
		if err := meshSupervisor.restart(meshScope(app.Settings())); err != nil {
			slog.Error("cannot restart mesh listener", "error", err)
		}
	}
	app.ApplyRuntimeSettings(context.Background())
	if err := meshSupervisor.restart(meshScope(app.Settings())); err != nil {
		slog.Error("cannot start mesh listener", "error", err)
	}
	// 本机重启后旧的反向节点必然没活链路，先置 offline 等它们重新接入
	if reset := app.Peer.ResetLinksOnStart(); reset > 0 {
		slog.Info("已把未重连的反向节点标记为离线", "count", reset)
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
	_ = meshSupervisor.shutdown(ctx)
	_ = webServer.Shutdown(ctx)
	app.Close()
}

// meshScope 是 56701 的监听范围：MCP 与 peer 谁要求更宽就取谁。
// 两者都关时返回空，监听器整体下线。
func meshScope(settings map[string]string) string {
	mcp := settings["mcp_enabled"] == "true"
	peer := settings["peer_enabled"] == "true"
	switch {
	case mcp && peer:
		if settings["mcp_bind_scope"] == "lan" || settings["peer_bind_scope"] == "lan" {
			return "lan"
		}
		return "local"
	case mcp:
		return settings["mcp_bind_scope"]
	case peer:
		return settings["peer_bind_scope"]
	default:
		return ""
	}
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

// setupGuardianLogger 让守护进程把日志写到独立文件（guardian.log），
// 与主程序的 npa.log 分开，便于事后排查升级过程。
func setupGuardianLogger(dataDirFlag string) {
	dir := dataDirPath(dataDirFlag)
	full := filepath.Join(dir, "guardian.log")
	file, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	var w io.Writer = os.Stderr
	if err == nil {
		w = io.MultiWriter(os.Stderr, file)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})))
}

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
