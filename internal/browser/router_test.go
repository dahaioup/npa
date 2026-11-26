package browser

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/npa/npa/internal/plugin"
)

// buildPlugin 编译 npa-obscura 插件进程，放到插件目录里。
// 编不出来（没装 Go 工具链等）就跳过用例。
func buildPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := "npa-obscura"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(dir, name)
	command := exec.Command("go", "build", "-o", binary, "github.com/npa/npa/cmd/npa-obscura")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("cannot build the obscura plugin: %v\n%s", err, output)
	}
	return dir
}

// obscuraBinary 找一台真的 obscura；没有就跳过。
func obscuraBinary(t *testing.T) string {
	t.Helper()
	candidates := []string{}
	if path := os.Getenv("NPA_OBSCURA_PATH"); path != "" {
		candidates = append(candidates, path)
	}
	if found, err := exec.LookPath("obscura"); err == nil {
		candidates = append(candidates, found)
	}
	candidates = append(candidates,
		filepath.Join("/tmp", "obscura-dist", "obscura"),
		filepath.Join(os.TempDir(), "obscura-dist", "obscura"))
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// TestRouterUsesPluginProcess 端到端：NPA 路由 → 插件进程 → obscura → 真实网页。
// 这条链路是浏览器能力的主路径（CLI 回退只是兜底）。
func TestRouterUsesPluginProcess(t *testing.T) {
	obscura := obscuraBinary(t)
	if obscura == "" {
		t.Skip("no obscura binary available")
	}
	pluginDir := buildPlugin(t)

	manager := plugin.NewManager(pluginDir)
	manager.PreferredName = "npa-obscura"
	manager.Args = []string{"--obscura", obscura}
	// 机器负载高时插件握手会慢，给足握手窗口。
	manager.InitTimeout = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start plugin: %v", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = manager.Close(closeCtx)
	}()

	router := NewRouter(manager, func() Config { return Config{BinaryPath: obscura, Timeout: 60 * time.Second} })

	// 工具清单要来自插件上报的实时清单。
	tools, err := router.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("plugin reported no tools")
	}

	// 真的抓一个页面。
	result, err := router.Call(context.Background(), CallRequest{
		Tool:      "browser_navigate",
		Arguments: map[string]any{"url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("browser_navigate: %v", err)
	}
	if result.IsError {
		t.Fatalf("browser_navigate reported an error: %+v", result.Content)
	}
	text := resultText(result)
	if !strings.Contains(text, "Example Domain") {
		t.Errorf("page text missing expected content, got: %s", text)
	}
}

// TestRouterFallsBackToCLI 插件缺失时要落到 CLI 回退，而不是直接报错。
func TestRouterFallsBackToCLI(t *testing.T) {
	obscura := obscuraBinary(t)
	if obscura == "" {
		t.Skip("no obscura binary available")
	}
	// 插件目录是空的 → 插件运行时缺失 → 路由必须回退。
	manager := plugin.NewManager(t.TempDir())
	router := NewRouter(manager, func() Config { return Config{BinaryPath: obscura, Timeout: 60 * time.Second} })

	result, err := router.Call(context.Background(), CallRequest{
		Tool:      "browser_navigate",
		Arguments: map[string]any{"url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("browser_navigate through the CLI fallback: %v", err)
	}
	// CLI 回退拿的是正文 markdown（不含 H1 标题），断言正文内容即可。
	text := resultText(result)
	if !strings.Contains(text, "documentation examples") {
		t.Errorf("CLI fallback returned unexpected content: %s", text)
	}
}

// resultText 把内容块拼成一段文本。
func resultText(result CallResult) string {
	var builder strings.Builder
	for _, content := range result.Content {
		if content.Type == "text" {
			builder.WriteString(content.Text)
			builder.WriteString("\n")
		}
	}
	return builder.String()
}
