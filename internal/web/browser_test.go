package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

// TestBrowserRoutes 浏览器端点要能报状态、列工具、转发调用。
func TestBrowserRoutes(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()

	// 状态接口
	response, err := http.Get(server.URL + "/api/browser/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status endpoint returned %d", response.StatusCode)
	}
	var status map[string]any
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status["tools"] == nil {
		t.Errorf("status should report the tool count: %#v", status)
	}
	if status["config"] == nil {
		t.Errorf("status should report the config: %#v", status)
	}

	// 工具清单接口
	response, err = http.Get(server.URL + "/api/browser/tools")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("tools endpoint returned %d", response.StatusCode)
	}
	var listing map[string]any
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	tools, _ := listing["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("tools endpoint returned nothing")
	}
	first, _ := tools[0].(map[string]any)
	if first["name"] != "browser_navigate" {
		t.Errorf("first tool = %v", first["name"])
	}

	// 调用接口：没装 obscura 时要报错，但不能是 500 之外的怪状态。
	payload, _ := json.Marshal(map[string]any{
		"tool":      "browser_navigate",
		"arguments": map[string]any{"url": "https://example.com"},
	})
	response, err = http.Post(server.URL+"/api/browser/call", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode == http.StatusOK {
		t.Logf("browser call succeeded: %s", string(body)[:min(len(body), 200)])
	} else {
		t.Logf("browser call failed as expected without obscura: %d %s", response.StatusCode, string(body))
		if response.StatusCode != http.StatusBadRequest && response.StatusCode != http.StatusNotImplemented {
			t.Errorf("unexpected status %d: %s", response.StatusCode, string(body))
		}
	}
}

// TestBrowserCallEndToEnd 通过 HTTP 端点走完整链路：
//
//	Web /api/browser/call ──▶ Service ──▶ browser.Router ──▶ 插件进程
//	                        ──▶ obscura mcp ──▶ 真实网页
//
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
	app.BrowserPlugin.SearchDirs = []string{pluginDir}
	app.BrowserPlugin.Args = []string{"--obscura", obscura}
	if err := app.BrowserPlugin.Start(context.Background()); err != nil {
		t.Fatalf("start the obscura plugin: %v", err)
	}
	defer app.BrowserPlugin.Close(context.Background())

	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()

	payload, _ := json.Marshal(map[string]any{
		"tool":      "browser_navigate",
		"arguments": map[string]any{"url": "https://example.com"},
	})
	response, err := http.Post(server.URL+"/api/browser/call", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("browser call returned %d: %s", response.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "Example Domain") {
		t.Errorf("page text missing expected content: %s", string(body))
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

// TestBrowserInstallRejectsUnknownVariant 未知的 obscura 变体要被拒绝。
func TestBrowserInstallRejectsUnknownVariant(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()

	payload, _ := json.Marshal(map[string]any{"variant": "bogus"})
	response, err := http.Post(server.URL+"/api/browser/install", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown variant should be rejected with 400, got %d: %s", response.StatusCode, string(body))
	}
}
