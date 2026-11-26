// Command npa-obscura 是 NPA 的 obscura 无头浏览器插件进程。
//
// 它在两边做协议翻译：
//
//	NPA core  ──NPA 插件协议(JSON-RPC 2.0 + Content-Length, stdio)──▶  npa-obscura
//	npa-obscura ──MCP 协议(stdio)──▶  obscura mcp（引擎自带 MCP server）
//
// obscura 的 38 个 browser_* 工具因此原样出现在 NPA 的 MCP 面上：工具名、
// 参数 schema、元素引用（e1）、快照格式全部是引擎自己的语义，NPA 不做翻译，
// 只在每个工具上追加一个 node_id（决定在哪台机器上跑）。
//
// 用法：
//
//	npa-obscura --obscura /path/to/obscura [--stealth] [--proxy URL]
//	npa-obscura --obscura /path/to/obscura --user-agent "..." --allow-private-network
//
// 也可用环境变量 NPA_OBSCURA_PATH 指定 obscura 可执行文件位置。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/npa/npa/internal/browser"
	"github.com/npa/npa/internal/plugin"
)

const (
	pluginID      = "npa-obscura"
	pluginName    = "NPA Obscura Browser"
	pluginVersion = "1.0.0"
)

func main() {
	var (
		binaryPath       = flag.String("obscura", os.Getenv("NPA_OBSCURA_PATH"), "path to the obscura executable")
		stealth          = flag.Bool("stealth", false, "run obscura in stealth mode")
		proxy            = flag.String("proxy", "", "HTTP/SOCKS proxy for obscura")
		userAgent        = flag.String("user-agent", "", "custom user agent for obscura")
		allowPrivate     = flag.Bool("allow-private-network", false, "allow obscura to reach loopback/private addresses")
		obeyRobots       = flag.Bool("obey-robots", false, "obscura honours robots.txt")
		storageDir       = flag.String("storage-dir", "", "obscura storage directory")
		connectTimeout   = flag.Duration("connect-timeout", 30*time.Second, "obscura MCP connect timeout")
		callTimeout      = flag.Duration("call-timeout", 120*time.Second, "per-call timeout")
		shutdownWait     = flag.Duration("shutdown-wait", 5*time.Second, "grace period on shutdown")
		dataDir          = flag.String("data-dir", "", "NPA data directory (obscura is installed into <data-dir>/bin)")
		variant          = flag.String("variant", "", "obscura build variant to install: (default), stealth or no-render")
		httpPort         = flag.Int("http-port", 0, "serve the plugin page and MCP over HTTP on this port (0 disables)")
		httpBind         = flag.String("http-bind", "127.0.0.1", "address the plugin HTTP service binds to")
		printVersionOnly = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *printVersionOnly {
		fmt.Println(pluginID, pluginVersion)
		return
	}

	if *binaryPath != "" {
		if info, err := os.Stat(*binaryPath); err != nil || info.IsDir() {
			slog.Error("obscura executable not usable", "path", *binaryPath, "error", err)
			os.Exit(2)
		}
	}
	// 找不到 obscura 不在这里退出：插件进程要活着等 obscura 被装上（或配置改
	// 动后重启引擎）。真正的失败留到调用时报错，错误消息说清怎么装。

	config := browser.Config{
		BinaryPath:          *binaryPath,
		Stealth:             *stealth,
		AllowPrivateNetwork: *allowPrivate,
		Proxy:               *proxy,
		UserAgent:           *userAgent,
		Timeout:             *callTimeout,
	}

	// 装过的 obscura 在 <data-dir>/bin/obscura：默认就用它。
	if *binaryPath == "" && *dataDir != "" {
		candidate := filepath.Join(*dataDir, "bin", "obscura")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			*binaryPath = candidate
		}
	}

	engine := &engine{
		binaryPath:     *binaryPath,
		variant:        *variant,
		obeyRobots:     *obeyRobots,
		storageDir:     *storageDir,
		connectTimeout: *connectTimeout,
		callTimeout:    *callTimeout,
		config:         config,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := plugin.NewServer(os.Stdin, os.Stdout, engine.handle)
	engine.server = server

	var httpService *httpService
	if *httpPort > 0 {
		httpService = newHTTPService(engine, *dataDir, *httpPort, *httpBind)
		if err := httpService.start(ctx); err != nil {
			slog.Error("plugin http service failed", "error", err)
		}
	}
	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		<-signals
		cancel()
	}()

	if err := server.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("plugin server stopped", "error", err)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), *shutdownWait)
	defer shutdownCancel()
	if httpService != nil {
		httpService.close(shutdownCtx)
	}
	engine.close(shutdownCtx)
}

// engine 持有 obscura MCP 子进程与会话；配置变了就重启引擎。
type engine struct {
	binaryPath     string
	variant        string
	obeyRobots     bool
	storageDir     string
	connectTimeout time.Duration
	callTimeout    time.Duration

	mu      sync.Mutex
	config  browser.Config
	session *mcpsdk.ClientSession
	client  *mcpsdk.Client
	command *exec.Cmd
	server  *plugin.Server
	mcp     *mcpsdk.Server
}

func (e *engine) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "plugin.initialize":
		return plugin.Info{
			ID:           pluginID,
			Name:         pluginName,
			Version:      pluginVersion,
			Protocol:     "1",
			Capabilities: []string{"browser"},
			Permission:   "core.browser",
			Settings:     obscuraSettings(),
		}, nil
	case "plugin.health":
		return map[string]any{"engine": "obscura", "binary": e.binaryPath}, nil
	case "plugin.shutdown":
		e.close(ctx)
		// core 等的就是进程退出：收到 shutdown 就收工。
		if e.server != nil {
			e.server.Stop()
		}
		return map[string]any{"stopped": true}, nil
	case plugin.MethodSettingsDescribe:
		return plugin.SettingsSchema{Settings: obscuraSettings()}, nil
	case plugin.MethodSettingsGet:
		return plugin.SettingsValues{Values: e.settingValues()}, nil
	case plugin.MethodSettingsApply:
		return e.applySettings(params)
	case "browser.tools":
		return e.tools(ctx)
	case "browser.call":
		return e.call(ctx, params)
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

// obscuraSettings 是本插件声明的可配置参数：core 按这份声明渲染「插件设置」页。
//
// 落点沿用已有的扁平设置键（browser_obscura_*），老库里的值继续有效；
// Writable 一律为 false——这些值由用户在页面上改、core 负责重启插件生效，
// 插件自己不回写。
func obscuraSettings() []plugin.SettingSpec {
	return []plugin.SettingSpec{
		{
			Key: "path", Label: "obscura 路径", Type: "string", Setting: "browser_obscura_path",
			Description: "obscura 可执行文件位置；留空则用 PATH 里的 obscura",
		},
		{
			Key: "stealth", Label: "隐匿模式", Type: "bool", Setting: "browser_obscura_stealth",
			Description: "以 stealth 模式启动引擎",
		},
		{
			Key: "allow_private", Label: "允许访问内网", Type: "bool", Setting: "browser_obscura_allow_private",
			Description: "允许引擎访问回环与私有网段地址",
		},
		{
			Key: "proxy", Label: "代理", Type: "string", Setting: "browser_obscura_proxy",
			Description: "引擎使用的 HTTP/SOCKS 代理",
		},
		{
			Key: "user_agent", Label: "User-Agent", Type: "string", Setting: "browser_obscura_user_agent",
			Description: "引擎使用的 User-Agent",
		},
		{
			Key: "timeout", Label: "单次调用超时（秒）", Type: "number", Setting: "browser_obscura_timeout",
			Default: "60", Description: "一次浏览器工具调用的时间预算",
		},
		{
			Key: "variant", Label: "引擎形态", Type: "string", Setting: "browser_obscura_variant",
			Description: "下载安装时使用的 obscura 形态：默认 / stealth / no-render",
		},
	}
}

// settingValues 返回插件当前生效的参数值（来自启动参数）。
func (e *engine) settingValues() map[string]string {
	e.mu.Lock()
	config := e.config
	e.mu.Unlock()
	return map[string]string{
		"path":          config.BinaryPath,
		"stealth":       boolText(config.Stealth),
		"allow_private": boolText(config.AllowPrivateNetwork),
		"proxy":         config.Proxy,
		"user_agent":    config.UserAgent,
		"timeout":       strconv.Itoa(int(config.Timeout.Seconds())),
		"variant":       e.variant,
	}
}

// applySettings 应用 core 下发的参数变更：更新内存里的配置，
// 下一次调用按新配置重启引擎（core 也会按参数变化重启插件进程）。
func (e *engine) applySettings(params json.RawMessage) (map[string]any, error) {
	var apply plugin.SettingsApply
	if len(params) > 0 {
		if err := json.Unmarshal(params, &apply); err != nil {
			return nil, fmt.Errorf("invalid plugin.settings.apply params: %w", err)
		}
	}
	e.mu.Lock()
	config := e.config
	for key, value := range apply.Values {
		switch key {
		case "path":
			config.BinaryPath = value
		case "stealth":
			config.Stealth = parseBoolText(value)
		case "allow_private":
			config.AllowPrivateNetwork = parseBoolText(value)
		case "proxy":
			config.Proxy = value
		case "user_agent":
			config.UserAgent = value
		case "timeout":
			if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
				config.Timeout = time.Duration(seconds) * time.Second
			}
		}
	}
	e.config = config
	if variant, ok := apply.Values["variant"]; ok {
		e.variant = strings.TrimSpace(variant)
	}
	e.mu.Unlock()
	return map[string]any{"applied": len(apply.Values)}, nil
}

// boolText 把布尔值转成设置库里的字符串。
func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// parseBoolText 解析设置库里的布尔值。
func parseBoolText(value string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	return trimmed == "true" || trimmed == "1"
}

// tools 返回 obscura MCP server 当前暴露的工具清单；引擎还没起来时用内嵌目录兜底。
func (e *engine) tools(ctx context.Context) ([]browser.Tool, error) {
	session, err := e.sessionFor(ctx, e.config)
	if err != nil {
		// 引擎没起来也要让 NPA 挂上工具：远端节点可能装了 obscura。
		return browser.Catalog(), nil
	}
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		return browser.Catalog(), nil
	}
	tools := make([]browser.Tool, 0, len(result.Tools))
	for _, tool := range result.Tools {
		if tool == nil {
			continue
		}
		tools = append(tools, browser.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: marshalSchema(tool.InputSchema),
		})
	}
	if len(tools) == 0 {
		return browser.Catalog(), nil
	}
	return tools, nil
}

// call 把一次浏览器工具调用转发给 obscura MCP server。
func (e *engine) call(ctx context.Context, params json.RawMessage) (browser.CallResult, error) {
	var request browser.CallParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &request); err != nil {
			return browser.CallResult{}, fmt.Errorf("invalid browser.call params: %w", err)
		}
	}
	if strings.TrimSpace(request.Tool) == "" {
		return browser.CallResult{}, errors.New("browser.call requires a tool name")
	}
	// 调用里带了配置就以它为准（配置变了要重启引擎，让新参数真正生效）。
	config := e.currentConfig()
	if request.Config != (browser.Config{}) {
		config = request.Config
	}
	return e.callWithConfig(ctx, request.Tool, request.Arguments, config)
}

// callTool 按当前配置调用一个浏览器工具。
func (e *engine) callTool(ctx context.Context, name string, arguments map[string]any) (browser.CallResult, error) {
	return e.callWithConfig(ctx, name, arguments, e.currentConfig())
}

// callWithConfig 用指定配置调用一个浏览器工具。
func (e *engine) callWithConfig(ctx context.Context, name string, arguments map[string]any, config browser.Config) (browser.CallResult, error) {
	if strings.TrimSpace(name) == "" {
		return browser.CallResult{}, errors.New("browser.call requires a tool name")
	}
	session, err := e.sessionFor(ctx, config)
	if err != nil {
		return browser.CallResult{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, e.callTimeout)
	defer cancel()
	result, err := session.CallTool(callCtx, &mcpsdk.CallToolParams{
		Name:      name,
		Arguments: arguments,
	})
	if err != nil {
		return browser.CallResult{}, err
	}
	converted := browser.CallResult{IsError: result.IsError}
	for _, content := range result.Content {
		converted.Content = append(converted.Content, convertContent(content))
	}
	return converted, nil
}

// currentConfig 返回当前配置（线程安全）。
func (e *engine) currentConfig() browser.Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.config
}

// setBinaryPath 换掉引擎可执行文件并让引擎下次调用时重启。
func (e *engine) setBinaryPath(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	e.mu.Lock()
	e.config.BinaryPath = path
	e.binaryPath = path
	e.closeLocked(context.Background())
	e.mu.Unlock()
}

// settingValue 读取一个插件参数的当前值。
func (e *engine) settingValue(key string) string {
	return e.settingValues()[key]
}

// toolNames 返回插件暴露的工具名清单。
func (e *engine) toolNames() []string {
	return browser.ToolNames()
}

// convertContent 把 MCP 内容块转成 NPA 的中立表示。
func convertContent(content mcpsdk.Content) browser.Content {
	switch typed := content.(type) {
	case *mcpsdk.TextContent:
		return browser.Content{Type: "text", Text: typed.Text}
	case *mcpsdk.ImageContent:
		return browser.Content{
			Type:     "image",
			Data:     encodeBase64(typed.Data),
			MimeType: typed.MIMEType,
		}
	case *mcpsdk.EmbeddedResource:
		resource := typed.Resource
		if resource == nil {
			return browser.Content{Type: "text"}
		}
		return browser.Content{
			Type:     "resource",
			URI:      resource.URI,
			MimeType: resource.MIMEType,
			Text:     resource.Text,
			Data:     encodeBase64(resource.Blob),
		}
	default:
		return browser.Content{Type: "text", Text: fmt.Sprintf("%v", content)}
	}
}

// sessionFor 返回可用的 obscura MCP 会话；配置变化时重启引擎。
func (e *engine) sessionFor(ctx context.Context, config browser.Config) (*mcpsdk.ClientSession, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session != nil && configKey(e.config) == configKey(config) {
		return e.session, nil
	}
	if e.session != nil {
		e.closeLocked(ctx)
	}
	session, err := e.startLocked(ctx, config)
	if err != nil {
		return nil, err
	}
	e.config = config
	e.session = session
	return session, nil
}

// startLocked 拉起 obscura mcp 子进程并建立 MCP 会话。
func (e *engine) startLocked(ctx context.Context, config browser.Config) (*mcpsdk.ClientSession, error) {
	binary := config.BinaryPath
	if binary == "" {
		binary = e.binaryPath
	}
	if binary == "" {
		// obscura 可能是后装的：这时才去找 PATH。
		if found, err := exec.LookPath("obscura"); err == nil {
			binary = found
		}
	}
	if binary == "" {
		return nil, errors.New("obscura 未安装：请在 NPA Web 设置页点「下载安装 obscura」，或用 --obscura / NPA_OBSCURA_PATH 指定可执行文件")
	}
	args := []string{"mcp"}
	if config.Stealth {
		args = append(args, "--stealth")
	}
	if config.Proxy != "" {
		args = append(args, "--proxy", config.Proxy)
	}
	if config.UserAgent != "" {
		args = append(args, "--user-agent", config.UserAgent)
	}
	if config.AllowPrivateNetwork {
		args = append(args, "--allow-private-network")
	}
	if e.obeyRobots {
		args = append(args, "--obey-robots")
	}
	if e.storageDir != "" {
		args = append(args, "--storage-dir", e.storageDir)
	}

	command := exec.Command(binary, args...)
	command.Stderr = os.Stderr
	transport := &mcpsdk.CommandTransport{Command: command, TerminateDuration: 3 * time.Second}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "npa-obscura", Version: pluginVersion}, nil)
	connectCtx, cancel := context.WithTimeout(ctx, e.connectTimeout)
	defer cancel()
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect obscura mcp: %w", err)
	}
	e.client = client
	e.command = command
	return session, nil
}

// close 关掉引擎子进程。
func (e *engine) close(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closeLocked(ctx)
}

// closeLocked 关掉引擎子进程（调用方持锁）。
func (e *engine) closeLocked(_ context.Context) {
	if e.session != nil {
		_ = e.session.Close()
		e.session = nil
	}
	if e.command != nil && e.command.Process != nil {
		_ = e.command.Process.Kill()
		e.command = nil
	}
	e.client = nil
}

// configKey 是配置指纹：变了就重启引擎。
func configKey(config browser.Config) string {
	return strings.Join([]string{
		config.BinaryPath,
		fmt.Sprintf("%t", config.Stealth),
		config.Proxy,
		config.UserAgent,
		fmt.Sprintf("%t", config.AllowPrivateNetwork),
	}, "|")
}

// marshalSchema 把 MCP 的 InputSchema（任意可 JSON 化的值）转成原始 JSON。
func marshalSchema(schema any) json.RawMessage {
	if schema == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	if raw, ok := schema.(json.RawMessage); ok {
		return raw
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return encoded
}

// encodeBase64 把字节编码成 base64（内容块里统一用字符串传输二进制）。
func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
