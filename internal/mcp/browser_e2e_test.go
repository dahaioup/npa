package mcp_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	npamcp "github.com/npa/npa/internal/mcp"
	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

// TestBrowserCallEndToEnd 走完整链路：
//
//	MCP 客户端 ──▶ NPA MCP 面 ──▶ Service ──▶ browser.Router ──▶ 插件进程
//	              ──▶ obscura mcp ──▶ 真实网页
//
// 这条链路要能跑通，浏览器能力才算真的接上了（而不是只在单测里各环节自洽）。
// 需要一台真的 obscura 与网络；缺任一条件就跳过。
func TestBrowserCallEndToEnd(t *testing.T) {
	obscura := obscuraBinary(t)
	if obscura == "" {
		t.Skip("no obscura binary available")
	}
	pluginDir := buildBrowserPlugin(t)

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	// 把插件进程指到刚编出来的那份，并告诉它 obscura 在哪。
	app.BrowserPlugin.PreferredName = "npa-obscura"
	app.BrowserPlugin.SearchDirs = []string{pluginDir}
	app.BrowserPlugin.Args = []string{"--obscura", obscura}
	if err := app.BrowserPlugin.Start(context.Background()); err != nil {
		t.Fatalf("start the obscura plugin: %v", err)
	}
	defer app.BrowserPlugin.Close(context.Background())

	manager := npamcp.NewManager(app)
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	serverSession, err := manager.Server().Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := clientSession.CallTool(ctx, &sdk.CallToolParams{
		Name:      "browser_navigate",
		Arguments: map[string]any{"url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("browser_navigate: %v", err)
	}
	if result.IsError {
		t.Fatalf("browser_navigate reported an error: %+v", result.Content)
	}
	var builder strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			builder.WriteString(text.Text)
		}
	}
	if !strings.Contains(builder.String(), "Example Domain") {
		t.Errorf("page text missing expected content, got: %s", builder.String())
	}
}

// buildBrowserPlugin 编译 npa-obscura 插件进程并返回它所在的目录。
func buildBrowserPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := "npa-obscura"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	command := exec.Command("go", "build", "-o", filepath.Join(dir, name), "github.com/npa/npa/cmd/npa-obscura")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("cannot build the obscura plugin: %v\n%s", err, output)
	}
	return dir
}

// obscuraBinary 找一台真的 obscura；没有就返回空串。
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
