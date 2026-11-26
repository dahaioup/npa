package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/npa/npa/internal/browser"
	"github.com/npa/npa/internal/plugin"
)

// obscuraBinary 找一台真的 obscura：环境变量优先，其次 PATH，最后本机常见位置。
// 找不到就跳过用例（不装 obscura 的机器也必须能跑测试）。
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
		filepath.Join("/tmp", "obscura-dist", "obscura.exe"),
		filepath.Join(os.TempDir(), "obscura-dist", "obscura"),
		filepath.Join(os.TempDir(), "obscura-dist", "obscura.exe"))
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// TestEngineInitialize 插件握手要报出浏览器能力。
func TestEngineInitialize(t *testing.T) {
	engine := &engine{}
	info, err := engine.handle(context.Background(), "plugin.initialize", nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	typed, ok := info.(plugin.Info)
	if !ok {
		t.Fatalf("initialize returned %T", info)
	}
	if typed.ID != pluginID || typed.Protocol != "1" {
		t.Errorf("info = %+v", typed)
	}
	if len(typed.Capabilities) == 0 || typed.Capabilities[0] != "browser" {
		t.Errorf("capabilities = %v", typed.Capabilities)
	}
}

// TestEngineToolsWithoutEngine 引擎没起来时也要给得出工具清单，
// 否则远端节点装了 obscura 也没法通过 NPA 调用。
func TestEngineToolsWithoutEngine(t *testing.T) {
	engine := &engine{binaryPath: filepath.Join(os.TempDir(), "no-such-obscura")}
	tools, err := engine.handle(context.Background(), "browser.tools", nil)
	if err != nil {
		t.Fatalf("browser.tools: %v", err)
	}
	list, ok := tools.([]browser.Tool)
	if !ok {
		t.Fatalf("browser.tools returned %T", tools)
	}
	if len(list) == 0 {
		t.Fatal("tool list is empty")
	}
	if list[0].Name != "browser_navigate" {
		t.Errorf("first tool = %q", list[0].Name)
	}
}

// TestEngineRejectsUnknownMethod 未知方法要明确报错。
func TestEngineRejectsUnknownMethod(t *testing.T) {
	engine := &engine{}
	if _, err := engine.handle(context.Background(), "browser.nope", nil); err == nil {
		t.Fatal("expected an error for an unknown method")
	}
}

// TestEngineCallRequiresTool 缺工具名要报错。
func TestEngineCallRequiresTool(t *testing.T) {
	engine := &engine{}
	if _, err := engine.handle(context.Background(), "browser.call", []byte(`{}`)); err == nil {
		t.Fatal("expected an error for a missing tool name")
	}
}

// TestEngineRealFetch 端到端：插件进程 → obscura MCP → 真实网页。
// 需要一台真的 obscura 与网络；缺任一条件就跳过。
func TestEngineRealFetch(t *testing.T) {
	binary := obscuraBinary(t)
	if binary == "" {
		t.Skip("no obscura binary available")
	}
	engine := &engine{
		binaryPath:     binary,
		connectTimeout: 30 * time.Second,
		callTimeout:    60 * time.Second,
	}
	defer engine.close(context.Background())

	result, err := engine.handle(context.Background(), "browser.call", []byte(`{"tool":"browser_navigate","arguments":{"url":"https://example.com"}}`))
	if err != nil {
		t.Fatalf("browser_navigate: %v", err)
	}
	call, ok := result.(browser.CallResult)
	if !ok {
		t.Fatalf("browser.call returned %T", result)
	}
	if call.IsError {
		t.Fatalf("browser_navigate reported an error: %+v", call.Content)
	}
	text := contentText(call)
	if !strings.Contains(text, "Example Domain") {
		t.Errorf("page text missing expected content, got: %s", truncateForTest(text, 400))
	}
}

// contentText 把内容块拼成一段文本。
func contentText(result browser.CallResult) string {
	var builder strings.Builder
	for _, content := range result.Content {
		if content.Type == "text" {
			builder.WriteString(content.Text)
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

// truncateForTest 截断长文本用于报错信息。
func truncateForTest(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
