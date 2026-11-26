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
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/selfupdate"
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

// resolveNodeID 决定本机节点 ID：
//  1. --node-id 显式指定（最高优先，同时固化到库里）
//  2. 库里已固化的值（第一次启动派生之后就固定下来，不再变）
//  3. 按硬件标识派生，并写回库里
func resolveNodeID(explicit string, store *state.Store) string {
	explicit = strings.TrimSpace(explicit)
	stored, err := store.LocalNodeID()
	if err != nil {
		slog.Warn("cannot read persisted node id", "error", err)
	}
	stored = strings.TrimSpace(stored)
	if explicit != "" {
		if explicit != stored {
			if err := store.SetLocalNodeID(explicit); err != nil {
				slog.Warn("cannot persist node id", "error", err)
			}
		}
		return explicit
	}
	if stored != "" {
		return stored
	}
	derived := model.NodeID()
	if derived == "" {
		return ""
	}
	if err := store.SetLocalNodeID(derived); err != nil {
		slog.Warn("cannot persist node id", "error", err)
	}
	return derived
}

func main() {
	dataDir := flag.String("data-dir", "", "internal state directory (default ~/.npa)")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print version and exit")
	guardianMode := flag.Bool("guardian", false, "run in guardian mode (remote-upgrade watchdog or restart helper)")
	callerPID := flag.Int("caller-pid", 0, "caller NPA process PID (guardian mode)")
	restartMode := flag.Bool("restart", false, "guardian mode: kill and restart NPA instead of upgrading")
	guardianConfig := flag.String("guardian-config", "", "legacy: run in guardian mode with JSON config (deprecated, use --guardian --caller-pid)")
	nodeID := flag.String("node-id", "", "fix this node's ID (default: derived from hardware identifiers and persisted)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	// 守护模式：只做远程升级的看门狗，不启动完整服务。放在最前面。
	if *guardianMode {
		setupGuardianLogger(*dataDir)
		os.Exit(int(guardian.RunByCallerPID(*callerPID, *restartMode)))
	}
	// 旧版 JSON 配置模式（兼容）
	if *guardianConfig != "" {
		setupGuardianLogger(*dataDir)
		fmt.Fprintln(os.Stderr, "guardian: --guardian-config is deprecated, use --guardian --caller-pid")
		os.Exit(2)
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
	// 节点身份：--node-id 显式指定 > 落库的固化值 > 硬件标识派生。
	// 固化值优先于派生值，保证换了网卡、插拔硬盘之后身份不变。
	if id := resolveNodeID(*nodeID, store); id != "" {
		model.SetNodeIDOverride(id)
	}
	app := service.New(store)
	// 两个 MCP 面：本地面（56700）与 mesh 面（56701）。
	// 插件把自己的 MCP 服务关掉时，mesh 面不再提供它——禁用的 MCP 只能
	// 通过 56700 访问。
	localMCP := mcp.NewManagerFor(app, mcp.FaceLocal)
	meshMCP := mcp.NewManagerFor(app, mcp.FaceMesh)
	mcpHTTPHandler := meshMCP.Handler()
	webHandler := web.New(app, localMCP)
	peerHandler := webHandler.PeerHandler()
	// 56701 是节点间通信与 MCP 的公共入口：/mcp 走 MCP，/api/peers/*、/api/info、
	// /api/topology 走 peer。两个功能各自受自己的开关门控，端口本身按
	// 局域网服务开关（lan_enabled）取监听范围：关掉就只听回环。
	meshSupervisor := &listenerSupervisor{name: "mesh", address: meshAddress, handler: web.RecoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			if app.Settings()["mcp_enabled"] != "true" {
				http.Error(w, "MCP service is disabled", http.StatusForbidden)
				return
			}
			mcpHTTPHandler.ServeHTTP(w, r)
			return
		}
		// 插件自己的 MCP 由 NPA 代理：插件只绑本机，跨机器一律走这里。
		if slot, ok := web.PluginMCPPath(r.URL.Path); ok {
			if app.Settings()["mcp_enabled"] != "true" {
				http.Error(w, "MCP service is disabled", http.StatusForbidden)
				return
			}
			web.PluginMCPHandler(app, slot, service.MCPFaceMesh).ServeHTTP(w, r)
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
	webHandler.RestartFunc = requestRestart   // Web「重启服务」按钮走守护进程拉起路径
	app.OnSettingsChanged = func() {
		localMCP.Refresh()
		meshMCP.Refresh()
		if err := meshSupervisor.restart(meshScope(app.Settings())); err != nil {
			slog.Error("cannot restart mesh listener", "error", err)
		}
		// 文件插件的端口或绑定范围变了才重启它（其它设置变化不影响）。
		if err := app.RestartFilePlugin(context.Background()); err != nil {
			slog.Error("cannot restart file plugin listener", "error", err)
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
			return
		}
		app.SyncPluginSettings(context.Background(), service.PluginSlotDesktop, app.Plugin)
	}()
	go func() {
		if err := app.FilePlugin.Start(context.Background()); err != nil {
			slog.Warn("file plugin unavailable; using built-in file provider", "error", err)
			return
		}
		app.SyncPluginSettings(context.Background(), service.PluginSlotFile, app.FilePlugin)
	}()
	// 文件插件的 HTTP 服务：节点文件面（/api/file/*）与插件前端页面都在这里，
	// 端口取设置项 file_plugin_port（默认 56751），绑定范围跟随局域网开关。
	go func() {
		if err := app.StartFilePlugin(context.Background()); err != nil {
			slog.Error("cannot start file plugin listener", "error", err)
		}
	}()
	go func() {
		// obscura 插件进程缺失不算故障：浏览器能力会回落到 obscura CLI，
		// CLI 也没有时调用时报错提示去装 obscura。
		app.StartBrowserPlugin(context.Background())
		app.SyncPluginSettings(context.Background(), service.PluginSlotBrowser, app.BrowserPlugin)
	}()
	go func() {
		// 插件管理器缺失不算故障：插件仓库面板会提示插件进程没装。
		app.StartPluginManager(context.Background())
		app.SyncPluginSettings(context.Background(), service.PluginSlotManager, app.PluginManager)
	}()
	if days := app.Settings()["audit_retention_days"]; days != "" {
		_ = app.Audit.Prune(parseInt(days))
	}

	// 56700 是本地面：除了 Web 管理界面，也提供 MCP（本机自用）。
	webMux := http.NewServeMux()
	webMux.Handle("/mcp", localMCP.Handler())
	webMux.Handle("/", webHandler.Handler())
	webServer := &http.Server{
		Addr:              "127.0.0.1:56700",
		Handler:           webMux,
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

// meshScope 是 56701 的监听范围：局域网服务开着就监听所有网卡，
// 关掉只监听回环（MCP 与节点互联都不暴露到局域网）。
// 两个功能都关时返回空，监听器整体下线。
func meshScope(settings map[string]string) string {
	mcp := settings["mcp_enabled"] == "true"
	peer := settings["peer_enabled"] == "true"
	if !mcp && !peer {
		return ""
	}
	lan := settings["lan_enabled"]
	if lan == "" {
		lan = "true" // 老库没有这个键：默认按开着处理，与 store 的默认值一致
	}
	if lan == "true" {
		return "lan"
	}
	return "local"
}

// stopSignal 让 Web「退出服务」与 Ctrl+C 走同一条优雅关闭路径。
var stopSignal = make(chan struct{}, 1)

func requestShutdown() {
	select {
	case stopSignal <- struct{}{}:
	default: // 已经在关闭流程里
	}
}

// requestRestart 先让守护进程就位，再走优雅关闭；守护进程发现主程序退出后拉起它。
func requestRestart() {
	if err := selfupdate.SpawnGuardian(); err != nil {
		slog.Error("restart: spawn guardian failed", "error", err)
	}
	requestShutdown()
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
