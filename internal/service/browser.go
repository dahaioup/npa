package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/npa/npa/internal/audit"
	"github.com/npa/npa/internal/browser"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/policy"
)

// browserRequestTimeout 是一次浏览器工具调用的总预算（渲染型页面可能慢）。
const browserRequestTimeout = 120 * time.Second

// browserCallTimeout 是 core 侧等待一次浏览器调用的上限。
// 它要比引擎自己的预算宽裕一些（多 30s），这样引擎先超时、错误才能
// 原样传回来；用户把「单次调用超时」调得比 120s 还长时，以用户的为准。
func (s *Service) browserCallTimeout() time.Duration {
	timeout := browserRequestTimeout
	if configured := s.BrowserConfig().Timeout; configured > 0 {
		if candidate := configured + 30*time.Second; candidate > timeout {
			timeout = candidate
		}
	}
	return timeout
}

// BrowserConfig 从设置项读出 obscura 的运行配置。
func (s *Service) BrowserConfig() browser.Config {
	settings := s.Settings()
	timeout := 60
	if value := settings["browser_obscura_timeout"]; value != "" {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed > 0 {
			timeout = parsed
		}
	}
	return browser.Config{
		BinaryPath:          browser.ResolveBinary(settings["browser_obscura_path"], s.Store.DataDir()),
		Stealth:             parseBool(settings["browser_obscura_stealth"]),
		AllowPrivateNetwork: parseBool(settings["browser_obscura_allow_private"]),
		Proxy:               settings["browser_obscura_proxy"],
		UserAgent:           settings["browser_obscura_user_agent"],
		Timeout:             time.Duration(timeout) * time.Second,
	}
}

// BrowserTools 返回本机可用的浏览器工具清单。
// 插件在跑时以插件上报的实时清单为准（obscura 升级后新工具自动出现），
// 否则回落到内嵌目录——远端节点可能装了 obscura，工具必须先挂上。
func (s *Service) BrowserTools(ctx context.Context) ([]browser.Tool, error) {
	if s.Browser == nil {
		return browser.Catalog(), nil
	}
	tools, err := s.Browser.Tools(ctx)
	if err != nil || len(tools) == 0 {
		return browser.Catalog(), nil
	}
	return tools, nil
}

// BrowserCall 在指定节点执行一次浏览器工具调用。
// nodeID 为空或本机 ID 表示本机执行；否则派发到那台 NPA 节点的 mesh 面。
func (s *Service) BrowserCall(ctx context.Context, nodeID, tool string, arguments map[string]any) (browser.CallResult, error) {
	if err := s.Authorize(ctx, "browser.call", policy.CapBrowser); err != nil {
		return browser.CallResult{}, err
	}
	if strings.TrimSpace(tool) == "" {
		return browser.CallResult{}, apperrors.New(apperrors.InvalidArgument, "browser tool name is required")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, s.browserCallTimeout())
	defer cancel()
	request := browser.CallRequest{Tool: tool, Arguments: arguments}
	var (
		result browser.CallResult
		err    error
	)
	switch s.browserTargetKind(nodeID) {
	case "self":
		result, err = s.Browser.Call(ctx, request)
	case "agent":
		payload := map[string]any{"tool": tool, "arguments": arguments}
		err = s.browserPost(ctx, nodeID, "/api/browser/call", payload, &result)
	default:
		err = apperrors.New(apperrors.NotSupported, "browser call requires an NPA node (ssh nodes have no agent)")
	}
	s.Audit.Record(ctx, "browser.call", nodeID, policy.CapBrowser, audit.SafeParameters(map[string]string{
		"tool": tool,
	}), started, err)
	return result, err
}

// browserTargetKind 分类目标：self（本机）/ agent（NPA 节点）/ ""（不支持）。
func (s *Service) browserTargetKind(nodeID string) string {
	if s.isSelfNode(nodeID) {
		return "self"
	}
	if s.isPeerNode(nodeID) || s.meshBaseDirect(nodeID) != "" {
		return "agent"
	}
	return ""
}

// browserPost 经节点 mesh HTTP 调 /api/browser/*：先直连，失败走跨域代理管道。
func (s *Service) browserPost(ctx context.Context, nodeID, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return apperrors.Wrap(apperrors.InvalidArgument, "encode browser request", err)
	}
	if base := s.meshBaseDirect(nodeID); base != "" {
		if err := postBrowserJSON(ctx, base+path, body, out); err == nil {
			return nil
		}
	}
	transport := s.relayScreenTransport(nodeID)
	client := &http.Client{Transport: transport}
	return postWithClient(ctx, client, "http://mesh"+path, body, out)
}

// postBrowserJSON 直连节点 mesh 面发一次 POST。
func postBrowserJSON(ctx context.Context, url string, body []byte, out any) error {
	// 不给 client 单独设超时：ctx 上已经有 browserCallTimeout 的预算，
	// 再加一个更短的上限会把用户调长的超时又掐回去。
	client := &http.Client{}
	return postWithClient(ctx, client, url, body, out)
}

// BrowserStatus 返回浏览器能力的运行状态（插件进程 + obscura 安装情况）。
func (s *Service) BrowserStatus() map[string]any {
	tools := browser.Catalog()
	settings := s.Settings()
	status := map[string]any{
		"tools":         len(tools),
		"tools_summary": describeBrowserTools(tools),
		"config":        s.BrowserConfig(),
		"variant":       settings["browser_obscura_variant"],
		// 超时不进 browser.Config（Duration 序列化是纳秒，容易被误读），
		// 单独按设置项原样报给 Web 面回填表单。
		"timeout": settings["browser_obscura_timeout"],
	}
	if s.Browser != nil {
		pluginStatus, lastError := s.Browser.PluginStatus()
		status["plugin_status"] = pluginStatus
		if lastError != "" {
			status["plugin_error"] = lastError
		}
	}
	if s.BrowserInstaller != nil {
		install := s.BrowserInstaller.Status()
		status["installed"] = install.Installed
		status["install_path"] = install.Path
		status["install_version"] = install.Version
		status["install_variant"] = install.Variant
	}
	return status
}

// BrowserInstall 下载安装 obscura（Web 设置页的「下载安装」按钮）。
// variant 取值：""（完整渲染版）、"stealth"（隐匿版）、"no-render"（精简版）。
// 传空时用设置项 browser_obscura_variant 里的默认变体。
func (s *Service) BrowserInstall(ctx context.Context, variant string) (browser.InstallResult, error) {
	if s.BrowserInstaller == nil {
		return browser.InstallResult{}, apperrors.New(apperrors.NotSupported, "browser installer is not available")
	}
	if strings.TrimSpace(variant) == "" {
		variant = strings.TrimSpace(s.Settings()["browser_obscura_variant"])
	}
	if !browser.Variants[variant] {
		return browser.InstallResult{}, apperrors.New(apperrors.InvalidArgument, "unknown obscura variant: "+variant)
	}
	s.browserInstallMu.Lock()
	defer s.browserInstallMu.Unlock()
	s.BrowserInstaller.Variant = variant
	result, err := s.BrowserInstaller.Install(ctx)
	if err != nil {
		s.Audit.Record(ctx, "browser.install", "", policy.CapBrowser, audit.SafeParameters(map[string]string{
			"variant": variant,
		}), time.Now(), err)
		return browser.InstallResult{}, err
	}
	s.Audit.Record(ctx, "browser.install", "", policy.CapBrowser, audit.SafeParameters(map[string]string{
		"variant": variant, "version": result.Version,
	}), time.Now(), nil)
	slog.Info("obscura installed", "path", result.Path, "version", result.Version)
	// 装完立刻重启插件进程：新路径要进到插件的启动参数里。
	if s.BrowserPlugin != nil {
		s.closeBrowserPlugin(ctx)
		s.BrowserPlugin.Args = s.BrowserPluginArgs()
		go s.StartBrowserPlugin(context.Background())
	}
	return result, nil
}

// closeBrowserPlugin 关掉插件进程：给一个有界的等待窗口，
// 免得插件不退出时把调用方（Web 请求）一起挂住。
func (s *Service) closeBrowserPlugin(ctx context.Context) {
	if s.BrowserPlugin == nil {
		return
	}
	closeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = s.BrowserPlugin.Close(closeCtx)
}

// StartBrowserPlugin 拉起 obscura 插件进程（缺失时只警告，不影响其它能力）。
func (s *Service) StartBrowserPlugin(ctx context.Context) {
	if s.BrowserPlugin == nil {
		return
	}
	if !s.PluginEnabled(PluginSlotBrowser) {
		return
	}
	if err := s.BrowserPlugin.Start(ctx); err != nil {
		slog.Warn("obscura plugin not started", "error", err)
		return
	}
	s.browserPluginMu.Lock()
	s.browserPluginArgs = strings.Join(s.BrowserPlugin.Args, " ")
	s.browserPluginMu.Unlock()
	slog.Info("obscura plugin started", "id", s.BrowserPlugin.Info().ID)
}

// refreshBrowserPlugin 在设置变化后重启插件进程：
// obscura 的路径/隐匿/代理等参数是进程启动参数，改了必须重启才生效。
func (s *Service) refreshBrowserPlugin(ctx context.Context) {
	if s.BrowserPlugin == nil {
		return
	}
	if !s.PluginEnabled(PluginSlotBrowser) {
		return
	}
	s.browserPluginMu.Lock()
	current := s.browserPluginArgs
	s.browserPluginMu.Unlock()
	args := s.BrowserPluginArgs()
	if strings.Join(args, " ") == current {
		return
	}
	status, _ := s.BrowserPlugin.Status()
	if status == "running" {
		s.closeBrowserPlugin(ctx)
	}
	s.BrowserPlugin.Args = args
	go s.StartBrowserPlugin(context.Background())
}

// BrowserPluginArgs 返回启动插件进程时附带的参数（obscura 路径与运行配置）。
func (s *Service) BrowserPluginArgs() []string {
	config := s.BrowserConfig()
	args := []string{}
	if config.BinaryPath != "" {
		args = append(args, "--obscura", config.BinaryPath)
	}
	if config.Stealth {
		args = append(args, "--stealth")
	}
	if config.AllowPrivateNetwork {
		args = append(args, "--allow-private-network")
	}
	if config.Proxy != "" {
		args = append(args, "--proxy", config.Proxy)
	}
	if config.UserAgent != "" {
		args = append(args, "--user-agent", config.UserAgent)
	}
	// 单次调用预算要跟着设置走：插件进程侧的 --call-timeout 才是真正
	// 卡住 obscura 引擎的那个超时，CLI 回退用的是 Config.Timeout。
	if config.Timeout > 0 {
		args = append(args, "--call-timeout", config.Timeout.String())
	}
	// 数据目录：插件把 obscura 装到 <data-dir>/bin，也用它放自己的状态。
	if dir := s.Store.DataDir(); dir != "" {
		args = append(args, "--data-dir", dir)
	}
	// 插件自己的 HTTP 服务：管理页 + 插件的 MCP。端口与开放度由 core 统一管，
	// 插件只管照做——默认只绑本机，对外由 NPA 代理。
	if port := s.PluginHTTPPort(PluginSlotBrowser); port > 0 {
		args = append(args, "--http-port", strconv.Itoa(port))
		args = append(args, "--http-bind", s.PluginBindHost(PluginSlotBrowser))
	}
	if variant := strings.TrimSpace(s.Settings()["browser_obscura_variant"]); variant != "" {
		args = append(args, "--variant", variant)
	}
	return args
}

// browserPluginDirs 列出浏览器插件进程的查找目录（按顺序）：
//   - 数据目录下的 plugins/browser（用户自装插件的标准位置）
//   - 发布包里的 plugins/browser（可执行文件同级的上一级）
//   - 可执行文件同级的 plugins/browser（开发期直接在仓库里跑）
func browserPluginDirs(dataDir string) []string {
	dirs := []string{filepath.Join(dataDir, "plugins", "browser")}
	if executable, err := os.Executable(); err == nil {
		binDir := filepath.Dir(executable)
		dirs = append(dirs,
			filepath.Join(binDir, "plugins", "browser"),
			filepath.Join(filepath.Dir(binDir), "plugins", "browser"))
	}
	return dirs
}

// describeBrowserTools 把工具清单压成一行摘要（状态接口用）。
func describeBrowserTools(tools []browser.Tool) string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return fmt.Sprintf("%d tools: %s", len(names), strings.Join(names, ", "))
}
