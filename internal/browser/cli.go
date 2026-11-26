package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
)

// cliTextLimit 是 CLI 回退返回正文的截断长度（避免一次抓取把 MCP 上下文撑爆）。
const cliTextLimit = 100000

// CLIProvider 直接调用 obscura CLI（`obscura fetch`）的最小实现。
// 插件进程缺失时用它兜底：只覆盖"抓一次就完事"的工具，交互类工具（点击、
// 填表、多步会话）必须靠插件进程里的 obscura MCP server。
type CLIProvider struct {
	// Settings 返回当前 obscura 配置（路径、代理、隐匿模式……）。
	Settings func() Config
	// SearchDirs 是自动探测 obscura 可执行文件时依次查找的目录。
	SearchDirs []string

	mu      sync.Mutex
	current string // 最近一次访问的 URL：CLI 没有会话，靠它串起 navigate → evaluate
}

// NewCLIProvider 构造 CLI 回退实现。
func NewCLIProvider(settings func() Config) *CLIProvider {
	return &CLIProvider{Settings: settings}
}

func (p *CLIProvider) config() Config {
	if p.Settings == nil {
		return Config{Timeout: 60 * time.Second}
	}
	return p.Settings()
}

// binaryName 返回当前平台的 obscura 可执行文件名。
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "obscura.exe"
	}
	return "obscura"
}

// cliTools 是 CLI 回退真正能跑的工具。
// 值是 `obscura fetch --dump` 的模式；空串表示该工具走特殊分支（eval / 截图）。
var cliTools = map[string]string{
	"browser_navigate":   "markdown",
	"browser_snapshot":   "markdown",
	"browser_markdown":   "markdown",
	"browser_links":      "links",
	"browser_evaluate":   "",
	"browser_screenshot": "",
}

// Tools 返回 CLI 回退支持的工具清单（schema 沿用 obscura 的定义）。
func (p *CLIProvider) Tools(context.Context) ([]Tool, error) {
	tools := make([]Tool, 0, len(cliTools))
	for _, tool := range Catalog() {
		if _, ok := cliTools[tool.Name]; !ok {
			continue
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// Call 执行一次 CLI 抓取。
func (p *CLIProvider) Call(ctx context.Context, request CallRequest) (CallResult, error) {
	// 先看工具能不能跑、再看参数、最后才找二进制：
	// 报错要指向真正的拦路虎，而不是先抛一句"obscura 没装"。
	mode, ok := cliTools[request.Tool]
	if !ok {
		return CallResult{}, apperrors.New(apperrors.NotSupported,
			"tool "+request.Tool+" needs the interactive obscura plugin; the CLI fallback only supports fetch, snapshot, evaluate, screenshot, links and markdown")
	}
	url := p.currentURL(request)
	if url == "" {
		return CallResult{}, apperrors.New(apperrors.InvalidArgument,
			"url is required: the CLI fallback has no session, so call browser_navigate with a url first")
	}
	binary, err := p.binary()
	if err != nil {
		return CallResult{}, err
	}
	config := p.config()
	switch request.Tool {
	case "browser_evaluate":
		expression, _ := request.Arguments["expression"].(string)
		if strings.TrimSpace(expression) == "" {
			return CallResult{}, apperrors.New(apperrors.InvalidArgument, "expression is required")
		}
		output, err := p.run(ctx, binary, url, config, []string{"--eval", expression})
		if err != nil {
			return CallResult{}, err
		}
		return textResult(output), nil
	case "browser_screenshot":
		file, err := os.CreateTemp("", "npa-obscura-shot-*.png")
		if err != nil {
			return CallResult{}, apperrors.Wrap(apperrors.Internal, "create screenshot file", err)
		}
		path := file.Name()
		_ = file.Close()
		defer os.Remove(path)
		if _, err := p.run(ctx, binary, url, config, []string{"--screenshot", path}); err != nil {
			return CallResult{}, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return CallResult{}, apperrors.Wrap(apperrors.Internal, "read screenshot", err)
		}
		return CallResult{Content: []Content{{
			Type:     "image",
			Data:     base64.StdEncoding.EncodeToString(data),
			MimeType: "image/png",
			Name:     filepath.Base(path),
		}}}, nil
	default:
		output, err := p.run(ctx, binary, url, config, []string{"--dump", mode})
		if err != nil {
			return CallResult{}, err
		}
		return textResult(output), nil
	}
}

// currentURL 取本次请求的 URL；没给就沿用最近一次访问的地址。
func (p *CLIProvider) currentURL(request CallRequest) string {
	for _, key := range []string{"url", "URL"} {
		if value, ok := request.Arguments[key].(string); ok && strings.TrimSpace(value) != "" {
			p.mu.Lock()
			p.current = value
			p.mu.Unlock()
			return value
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

// binary 定位 obscura 可执行文件：设置项 → 数据目录 bin/ → PATH。
func (p *CLIProvider) binary() (string, error) {
	config := p.config()
	if config.BinaryPath != "" {
		if info, err := os.Stat(config.BinaryPath); err == nil && !info.IsDir() {
			return config.BinaryPath, nil
		}
		return "", apperrors.New(apperrors.InvalidArgument, "obscura binary not found: "+config.BinaryPath)
	}
	for _, dir := range p.SearchDirs {
		candidate := filepath.Join(dir, binaryName())
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath(binaryName()); err == nil {
		return path, nil
	}
	return "", apperrors.New(apperrors.NotSupported,
		"obscura 未安装：请在 Web 设置页点「下载安装 obscura」，或把 browser_obscura_path 指向可执行文件")
}

// run 执行一次 obscura fetch。
func (p *CLIProvider) run(ctx context.Context, binary, url string, config Config, extra []string) (string, error) {
	args := []string{"fetch", url}
	if config.AllowPrivateNetwork {
		args = append(args, "--allow-private-network")
	}
	if config.Stealth {
		args = append(args, "--stealth")
	}
	if config.Proxy != "" {
		args = append(args, "--proxy", config.Proxy)
	}
	if config.UserAgent != "" {
		args = append(args, "--user-agent", config.UserAgent)
	}
	if config.Timeout > 0 {
		seconds := int(config.Timeout.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		args = append(args, "--timeout", fmt.Sprintf("%d", seconds))
	}
	args = append(args, extra...)
	args = append(args, "--quiet")

	command := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", apperrors.New(apperrors.Internal, "obscura fetch failed: "+truncate(message, 2000))
	}
	return stdout.String(), nil
}

// textResult 把 CLI 输出包成文本内容块。
func textResult(output string) CallResult {
	return CallResult{Content: []Content{{Type: "text", Text: truncate(output, cliTextLimit)}}}
}

// truncate 截断长文本并标注被截掉的部分。
func truncate(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return text[:limit] + fmt.Sprintf("\n...[truncated, %d bytes total]", len(text))
}
