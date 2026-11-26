package pluginmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/npa/npa/internal/plugin"
	"github.com/npa/npa/internal/pluginregistry"
)

// registryServer 提供一份最小索引，供内置实现拉取。
//
// 索引里的资产地址指向本服务器：装插件时不必真去公网。
func registryServer(t *testing.T) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/artifact.bin", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("demo-binary"))
	})
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, _ *http.Request) {
		document := fmt.Sprintf(`{
			"schema_version": 1,
			"updated_at": "2026-11-24T00:00:00Z",
			"plugins": [{
				"id": "npa-demo",
				"name": "Demo Plugin",
				"version": "1.2.3",
				"kind": "plugin",
				"capabilities": ["demo"],
				"install": {"dir": "plugins/demo", "executable": "npa-demo"},
				"artifacts": [{"os": %q, "arch": %q, "url": %q, "sha256": %q, "size": 11, "format": "binary"}]
			}]
		}`, runtime.GOOS, runtime.GOARCH, server.URL+"/artifact.bin", digestOf("demo-binary"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(document))
	})
	server = httptest.NewServer(mux)
	return server
}

// digestOf 算一段内容的 sha256（十六进制）。
func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// TestBuiltinInfoDeclaresPluginManager 内置实现要按插件的形状自报家门。
func TestBuiltinInfoDeclaresPluginManager(t *testing.T) {
	builtin := NewBuiltin(t.TempDir(), nil)
	info := builtin.Info()
	if info.ID != BuiltinID {
		t.Fatalf("id = %s, want %s", info.ID, BuiltinID)
	}
	if info.Protocol != "core/1" {
		t.Fatalf("protocol = %s, want core/1", info.Protocol)
	}
	if len(info.Capabilities) != 1 || info.Capabilities[0] != "plugin" {
		t.Fatalf("capabilities = %v", info.Capabilities)
	}
	specs := builtin.Settings()
	if len(specs) == 0 || specs[0].Setting != "plugin_registry_url" {
		t.Fatalf("settings = %+v, want the registry url parameter", specs)
	}
}

// TestBuiltinFallsBackToDefaultRegistry 没配地址时用默认公有仓库。
func TestBuiltinFallsBackToDefaultRegistry(t *testing.T) {
	builtin := NewBuiltin(t.TempDir(), func() string { return "" })
	if builtin.registryURL() != pluginregistry.DefaultRegistryURL {
		t.Fatalf("registry url = %q, want the default", builtin.registryURL())
	}
	builtin = NewBuiltin(t.TempDir(), func() string { return "http://example.test/registry.json" })
	if builtin.registryURL() != "http://example.test/registry.json" {
		t.Fatalf("registry url = %q, want the configured one", builtin.registryURL())
	}
}

// TestBuiltinListsRegistryEntries 内置实现能把仓库清单拉下来。
func TestBuiltinListsRegistryEntries(t *testing.T) {
	server := registryServer(t)
	defer server.Close()

	builtin := NewBuiltin(t.TempDir(), func() string { return server.URL + "/registry.json" })
	result, err := builtin.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	plugins, ok := result["plugins"].([]map[string]any)
	if !ok || len(plugins) != 1 {
		t.Fatalf("plugins = %+v", result["plugins"])
	}
	if plugins[0]["id"] != "npa-demo" {
		t.Fatalf("entry = %+v", plugins[0])
	}
	if result["registry_url"] != server.URL+"/registry.json" {
		t.Fatalf("registry url = %v", result["registry_url"])
	}
}

// TestBuiltinCachesRegistryAndInvalidates 索引会被缓存，失效后重新拉。
func TestBuiltinCachesRegistryAndInvalidates(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":1,"plugins":[{"id":"demo","version":"1.0.0","install":{"dir":"plugins/demo","executable":"demo"}}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	builtin := NewBuiltin(t.TempDir(), func() string { return server.URL + "/registry.json" })
	for i := 0; i < 3; i++ {
		if _, err := builtin.List(context.Background()); err != nil {
			t.Fatalf("list: %v", err)
		}
	}
	if hits != 1 {
		t.Fatalf("registry fetched %d times, want 1 (cached)", hits)
	}
	builtin.Invalidate()
	if _, err := builtin.List(context.Background()); err != nil {
		t.Fatalf("list: %v", err)
	}
	if hits != 2 {
		t.Fatalf("registry fetched %d times, want 2 after invalidate", hits)
	}
}

// TestBuiltinStatusReportsInstallState 状态里要能看到仓库地址与已装插件。
func TestBuiltinStatusReportsInstallState(t *testing.T) {
	builtin := NewBuiltin(t.TempDir(), func() string { return "http://example.test/registry.json" })
	result, err := builtin.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if result["registry_url"] != "http://example.test/registry.json" {
		t.Fatalf("registry url = %v", result["registry_url"])
	}
	installed, ok := result["installed"].([]pluginregistry.InstalledEntry)
	if !ok {
		t.Fatalf("installed = %T", result["installed"])
	}
	if len(installed) != 0 {
		t.Fatalf("installed = %+v, want empty", installed)
	}
}

// TestBuiltinRemoveUnknownPlugin 卸载没装过的插件要报错。
func TestBuiltinRemoveUnknownPlugin(t *testing.T) {
	builtin := NewBuiltin(t.TempDir(), nil)
	if _, err := builtin.Remove(context.Background(), "nope"); err == nil {
		t.Fatal("expected an error when removing an unknown plugin")
	}
	if _, err := builtin.Remove(context.Background(), "  "); err == nil {
		t.Fatal("expected an error when the plugin id is empty")
	}
}

// TestBuiltinInstallUnknownPlugin 仓库里没有的插件不能装。
func TestBuiltinInstallUnknownPlugin(t *testing.T) {
	server := registryServer(t)
	defer server.Close()

	builtin := NewBuiltin(t.TempDir(), func() string { return server.URL + "/registry.json" })
	if _, err := builtin.Install(context.Background(), "nope", ""); err == nil {
		t.Fatal("expected an error for an unknown plugin")
	}
	if _, err := builtin.Install(context.Background(), "npa-demo", "9.9.9"); err == nil {
		t.Fatal("expected an error for an unpublished version")
	}
}

// TestSlotHelpers 槽位名从安装目录推出来。
func TestSlotHelpers(t *testing.T) {
	if got := SlotOf("plugins/demo"); got != "demo" {
		t.Fatalf("slot = %q, want demo", got)
	}
	if got := SlotOf("bin"); got != "" {
		t.Fatalf("slot = %q, want empty", got)
	}
	slots := SlotsOf([]pluginregistry.InstallResult{
		{ID: "demo-engine", Dir: "bin"},
		{ID: "npa-demo", Dir: "plugins/demo"},
		{ID: "npa-demo2", Dir: "plugins/demo"},
	})
	if len(slots) != 1 || slots[0] != "demo" {
		t.Fatalf("slots = %v, want [demo]", slots)
	}
}

// TestServiceFallsBackToBuiltin 没有外部插件进程时，内置实现顶上。
func TestServiceFallsBackToBuiltin(t *testing.T) {
	server := registryServer(t)
	defer server.Close()

	service := NewService(t.TempDir(), func() string { return server.URL + "/registry.json" })
	result, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	plugins, ok := result["plugins"].([]map[string]any)
	if !ok || len(plugins) != 1 {
		t.Fatalf("plugins = %+v", result["plugins"])
	}
	if service.PluginInfo().ID != BuiltinID {
		t.Fatalf("plugin info = %+v", service.PluginInfo())
	}
}

// TestServiceInstallThroughBuiltin 内置实现能装插件并给出槽位。
func TestServiceInstallThroughBuiltin(t *testing.T) {
	server := registryServer(t)
	defer server.Close()

	dataDir := t.TempDir()
	service := NewService(dataDir, func() string { return server.URL + "/registry.json" })
	if _, err := service.Install(context.Background(), "nope", ""); err == nil {
		t.Fatal("expected an error for an unknown plugin")
	}
	result, err := service.Install(context.Background(), "npa-demo", "")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	slots, ok := result["slots"].([]string)
	if !ok || len(slots) != 1 || slots[0] != "demo" {
		t.Fatalf("slots = %+v, want [demo]", result["slots"])
	}
	installed := filepath.Join(dataDir, "plugins", "demo", "npa-demo-"+runtime.GOOS+"-"+runtime.GOARCH)
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("plugin not installed: %v", err)
	}
}

// TestServicePluginInfoMatchesBuiltin 门面报的是内置插件的元信息。
func TestServicePluginInfoMatchesBuiltin(t *testing.T) {
	service := NewService(t.TempDir(), nil)
	var info plugin.Info = service.PluginInfo()
	if info.ID != BuiltinID {
		t.Fatalf("id = %s, want %s", info.ID, BuiltinID)
	}
}
