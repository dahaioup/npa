package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

// fakePluginMCP 造一个假的插件 MCP 服务：插件只绑本机，NPA 通过代理访问它。
func fakePluginMCP(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		hits++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"from": "plugin", "echo": string(body)},
		})
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

// TestPluginMCPProxy NPA 要能把插件自己的 MCP 代理出去。
//
// 插件只绑 127.0.0.1，别的机器要用它的 MCP 就走 NPA 的代理路径：
// /plugins/<槽位>/mcp。
func TestPluginMCPProxy(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)

	plugin, hits := fakePluginMCP(t)
	port := portOf(t, plugin.URL)
	if err := store.SaveSettings(map[string]string{"browser_plugin_port": strconv.Itoa(port)}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()

	response, err := http.Post(server.URL+"/plugins/browser/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("proxy returned %d, want 200", response.StatusCode)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "plugin") {
		t.Errorf("proxy body = %s, want the plugin's answer", body)
	}
	if *hits != 1 {
		t.Errorf("plugin hits = %d, want 1", *hits)
	}
}

// TestPluginMCPProxyDisabledOnMeshFace 插件的 MCP 关掉之后：mesh 面（56701）
// 不再提供它，本地面（56700）照旧能用。
func TestPluginMCPProxyDisabledOnMeshFace(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)

	plugin, hits := fakePluginMCP(t)
	port := portOf(t, plugin.URL)
	if err := store.SaveSettings(map[string]string{
		"browser_plugin_port": strconv.Itoa(port),
		"browser_plugin_mcp":  "false",
	}); err != nil {
		t.Fatal(err)
	}

	// mesh 面：直接拒绝。
	mesh := httptest.NewServer(PluginMCPHandler(app, "browser", service.MCPFaceMesh))
	defer mesh.Close()
	response, err := http.Post(mesh.URL+"/plugins/browser/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("mesh face returned %d, want 403", response.StatusCode)
	}
	if *hits != 0 {
		t.Errorf("the plugin must not be reached from the mesh face (hits = %d)", *hits)
	}

	// 本地面：照旧代理。
	local := httptest.NewServer(PluginMCPHandler(app, "browser", service.MCPFaceLocal))
	defer local.Close()
	response, err = http.Post(local.URL+"/plugins/browser/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("local face returned %d, want 200", response.StatusCode)
	}
	if *hits != 1 {
		t.Errorf("plugin hits = %d, want 1", *hits)
	}
}

// TestPluginMCPUnknownSlot 没有 MCP 的槽位要明确回 404。
func TestPluginMCPUnknownSlot(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)

	server := httptest.NewServer(PluginMCPHandler(app, "file", service.MCPFaceLocal))
	defer server.Close()
	response, err := http.Post(server.URL+"/plugins/file/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("file slot returned %d, want 404", response.StatusCode)
	}
}

// TestPluginMCPPath 解析 /plugins/<槽位>/mcp 形式的路径。
func TestPluginMCPPath(t *testing.T) {
	slot, ok := PluginMCPPath("/plugins/browser/mcp")
	if !ok || slot != "browser" {
		t.Errorf("path parse = %q, %v; want browser, true", slot, ok)
	}
	if _, ok := PluginMCPPath("/plugins/browser/page"); ok {
		t.Errorf("a non-MCP path must not match")
	}
	if _, ok := PluginMCPPath("/mcp"); ok {
		t.Errorf("the core MCP path must not match")
	}
}

// portOf 从地址里取端口。
func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	index := strings.LastIndex(rawURL, ":")
	if index < 0 {
		t.Fatalf("cannot parse url %q", rawURL)
	}
	port, err := strconv.Atoi(rawURL[index+1:])
	if err != nil {
		t.Fatalf("cannot parse url %q: %v", rawURL, err)
	}
	return port
}
