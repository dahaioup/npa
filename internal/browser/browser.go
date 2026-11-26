// Package browser 把 obscura 无头浏览器引擎接进 NPA。
//
// 两条执行路径，插件优先：
//
//  1. 插件进程（cmd/npa-obscura）：NPA 插件协议（JSON-RPC over stdio）↔
//     obscura 自带的 MCP server（stdio）。obscura 的 38 个 browser_* 工具
//     原样透传，语义（元素引用 e1、快照格式等）完全交给上游引擎。
//  2. CLI 回退：插件缺失时直接调用 `obscura fetch`，覆盖抓取类工具
//     （navigate/snapshot/evaluate/screenshot/links/markdown/text）。
//
// 工具名与 obscura 保持一致（browser_navigate、browser_snapshot……），
// 以便上游 obscura 升级时 NPA 这边零改动。
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/plugin"
)

// Tool 描述一个可调用的浏览器工具，字段与 obscura MCP server 暴露的一致。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// Content 是工具返回的内容块，语义对齐 MCP 的 content block。
// Type 取值：text / image / resource。
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"` // image：base64 编码的原始字节
	MimeType string `json:"mime_type,omitempty"`
	URI      string `json:"uri,omitempty"` // resource：本地文件路径
	Name     string `json:"name,omitempty"`
}

// CallResult 是一次工具调用的结果。
type CallResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"is_error,omitempty"`
}

// CallRequest 是一次工具调用的入参。
type CallRequest struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// CallParams 是插件进程收到的调用参数：请求 + 引擎配置。
// 配置随调用传下去，插件发现配置变了就重启 obscura 引擎
// （例如用户在设置页勾上隐匿模式，无需重启 NPA）。
type CallParams struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Config    Config         `json:"config,omitempty"`
}

// Config 是 obscura 的运行配置，来自 NPA 设置项。
type Config struct {
	BinaryPath          string `json:"binary_path"`   // obscura 可执行文件路径（空 = 自动探测）
	Stealth             bool   `json:"stealth"`       // 隐匿模式（一致的指纹 + TLS 伪装）
	AllowPrivateNetwork bool   `json:"allow_private"` // 允许访问回环/内网地址（关掉 obscura 的 SSRF 防护）
	Proxy               string `json:"proxy"`         // HTTP/SOCKS 代理
	UserAgent           string `json:"user_agent"`    // 自定义 UA
	// Timeout 是单次调用的总预算。它只在本机用（CLI 回退的 --timeout 参数），
	// 不跨进程传输：time.Duration 序列化出来是纳秒，容易被误读。
	// 跨进程的调用预算由 core 的请求超时与插件的 --call-timeout 控制。
	Timeout time.Duration `json:"-"`
}

// Provider 是浏览器能力的抽象：插件进程与 CLI 回退都实现它。
type Provider interface {
	// Tools 返回当前实现真正能跑的工具清单。
	Tools(ctx context.Context) ([]Tool, error)
	// Call 执行一次工具调用。
	Call(ctx context.Context, request CallRequest) (CallResult, error)
}

// ResolveBinary 按「显式配置 → 数据目录 bin/ → PATH」的顺序定位 obscura。
// 找不到时返回空串（调用方据此给出安装提示）。
func ResolveBinary(configured, dataDir string) string {
	if configured != "" {
		if info, err := os.Stat(configured); err == nil && !info.IsDir() {
			return configured
		}
		return ""
	}
	if dataDir != "" {
		candidate := filepath.Join(dataDir, "bin", binaryName())
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	if path, err := exec.LookPath(binaryName()); err == nil {
		return path
	}
	return ""
}

// Router 按「插件优先、CLI 回退」的顺序派发浏览器调用。
// 插件进程崩溃或缺失时自动落到 CLI；插件在跑但工具报错时原样返回错误，
// 不做静默降级（避免把"引擎出错了"伪装成"能力不支持"）。
type Router struct {
	Plugin       *plugin.Manager
	Fallback     Provider
	Settings     func() Config
	resourceRoot string
}

// NewRouter 构造浏览器路由。
func NewRouter(manager *plugin.Manager, settings func() Config) *Router {
	return &Router{Plugin: manager, Fallback: NewCLIProvider(settings), Settings: settings}
}

// NewRouterWithRoot 构造带资源目录的浏览器路由（截图落盘用）。
func NewRouterWithRoot(manager *plugin.Manager, settings func() Config, root string) *Router {
	router := NewRouter(manager, settings)
	router.resourceRoot = root
	return router
}

// usePlugin 插件进程是否可用。
func (r *Router) usePlugin() bool {
	if r.Plugin == nil {
		return false
	}
	status, _ := r.Plugin.Status()
	return status == "running"
}

// Tools 返回本机当前可用的工具清单：插件在跑就问插件，否则问 CLI 回退。
func (r *Router) Tools(ctx context.Context) ([]Tool, error) {
	if r.usePlugin() {
		var tools []Tool
		if err := r.Plugin.Invoke(ctx, "browser.tools", struct{}{}, &tools); err == nil {
			return tools, nil
		} else if !errors.Is(err, plugin.ErrRuntimeMissing) {
			slog.Warn("browser plugin tools failed", "error", err)
		}
	}
	if r.Fallback == nil {
		return nil, nil
	}
	return r.Fallback.Tools(ctx)
}

// Call 执行一次浏览器工具调用。
func (r *Router) Call(ctx context.Context, request CallRequest) (CallResult, error) {
	request.Tool = strings.TrimSpace(request.Tool)
	if request.Tool == "" {
		return CallResult{}, apperrors.New(apperrors.InvalidArgument, "browser tool name is required")
	}
	if r.usePlugin() {
		var result CallResult
		params := CallParams{Tool: request.Tool, Arguments: request.Arguments}
		if r.Settings != nil {
			params.Config = r.Settings()
		}
		err := r.Plugin.Invoke(ctx, "browser.call", params, &result)
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, plugin.ErrRuntimeMissing) {
			return CallResult{}, err
		}
	}
	if r.Fallback == nil {
		return CallResult{}, apperrors.New(apperrors.NotSupported, "obscura is not available")
	}
	return r.Fallback.Call(ctx, request)
}

// PluginStatus 返回插件进程状态与最近一次错误（Web 面展示用）。
func (r *Router) PluginStatus() (string, string) {
	if r.Plugin == nil {
		return "runtime_missing", "plugin manager not configured"
	}
	return r.Plugin.Status()
}
