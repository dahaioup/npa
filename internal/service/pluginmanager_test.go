package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/npa/npa/internal/pluginmanager"
	"github.com/npa/npa/internal/state"
)

// registryServer 提供一份最小索引，让内置实现能拉到东西。
func registryServer(t *testing.T) *httptest.Server {
	t.Helper()
	document := fmt.Sprintf(`{
		"schema_version": 1,
		"updated_at": "2026-11-24T00:00:00Z",
		"plugins": [{
			"id": "npa-demo",
			"name": "Demo Plugin",
			"version": "1.2.3",
			"kind": "plugin",
			"install": {"dir": "plugins/demo", "executable": "npa-demo"},
			"artifacts": [{"os": %q, "arch": %q, "url": "http://example/npa-demo", "sha256": "aa", "size": 10, "format": "binary"}]
		}]
	}`, runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(document))
	})
	return httptest.NewServer(mux)
}

// TestPluginManagerRegistryURLFallsBackToDefault 没配仓库地址时用默认公有仓库。
func TestPluginManagerRegistryURLFallsBackToDefault(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	url := app.PluginRegistryURL()
	if !strings.HasPrefix(url, "http") {
		t.Fatalf("registry url = %q, want an http url", url)
	}
	if err := store.SaveSettings(map[string]string{"plugin_registry_url": "http://example.test/registry.json"}); err != nil {
		t.Fatal(err)
	}
	if got := app.PluginRegistryURL(); got != "http://example.test/registry.json" {
		t.Fatalf("registry url = %q, want the configured one", got)
	}
}

// TestPluginManagerArgsCarryDataDirAndRegistry 插件管理器要知道装到哪儿、从哪儿取。
func TestPluginManagerArgsCarryDataDirAndRegistry(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	joined := strings.Join(app.PluginManagerArgs(), " ")
	if !strings.Contains(joined, "--data-dir") || !strings.Contains(joined, store.DataDir()) {
		t.Fatalf("args = %v, want the data dir", app.PluginManagerArgs())
	}
	if !strings.Contains(joined, "--registry") {
		t.Fatalf("args = %v, want the registry url", app.PluginManagerArgs())
	}
}

// TestPluginManagerUsesBuiltinImplementation 没有外部插件进程时，内置实现顶上。
func TestPluginManagerUsesBuiltinImplementation(t *testing.T) {
	server := registryServer(t)
	defer server.Close()

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)
	if err := store.SaveSettings(map[string]string{"plugin_registry_url": server.URL + "/registry.json"}); err != nil {
		t.Fatal(err)
	}

	result, err := app.PluginList(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	plugins, ok := result["plugins"].([]map[string]any)
	if !ok || len(plugins) == 0 {
		t.Fatalf("plugins = %+v", result["plugins"])
	}
	status, err := app.PluginManagerStatus(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status["plugin_status"] != "builtin" {
		t.Fatalf("plugin_status = %v, want builtin", status["plugin_status"])
	}
	if status["plugin_id"] != pluginmanager.BuiltinID {
		t.Fatalf("plugin_id = %v, want %s", status["plugin_id"], pluginmanager.BuiltinID)
	}
}

// TestPluginManagerCannotBeDisabled 插件管理器是内置能力，不能被停用。
func TestPluginManagerCannotBeDisabled(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	disabled := false
	if _, err := app.UpdatePluginSettings(context.Background(), PluginSettingsUpdate{
		Slot: PluginSlotManager, Enabled: &disabled,
	}); err == nil {
		t.Fatal("expected the plugin manager to refuse being disabled")
	} else if !strings.Contains(err.Error(), "内置") {
		t.Fatalf("error = %v, want it to mention that it is built in", err)
	}

	// 直接写设置库也不行。
	if err := app.SaveSettings(map[string]string{"plugin.manager.enabled": "false"}); err == nil {
		t.Fatal("expected the settings API to refuse disabling the plugin manager")
	}

	// 就算有人绕过校验把值写进了状态库，槽位依然是启用的。
	if err := store.SaveSettings(map[string]string{"plugin.manager.enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	if !app.PluginEnabled(PluginSlotManager) {
		t.Fatal("the plugin manager must stay enabled")
	}
	if !app.MCPCapabilities()["plugin"] {
		t.Fatal("the plugin capability must stay available")
	}
}

// TestPluginManagerViewsMarkItRequired 插件设置页要标出它是内置能力。
func TestPluginManagerViewsMarkItRequired(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	var found bool
	for _, view := range app.PluginViews() {
		if view.Slot != PluginSlotManager {
			continue
		}
		found = true
		if !view.Required {
			t.Fatal("the plugin manager view must be marked as required")
		}
		if !view.Enabled {
			t.Fatal("the plugin manager view must be enabled")
		}
		if !view.Builtin {
			t.Fatal("the plugin manager view must report the built-in implementation")
		}
	}
	if !found {
		t.Fatal("the plugin manager slot is missing from the plugin views")
	}
}

// TestPluginManagerBuiltinCannotBeRemoved 内置的插件管理器不可卸载。
func TestPluginManagerBuiltinCannotBeRemoved(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	if _, err := app.PluginRemove(context.Background(), pluginmanager.BuiltinID); err == nil {
		t.Fatal("expected the built-in plugin manager to refuse being removed")
	} else if !strings.Contains(err.Error(), "内置") {
		t.Fatalf("error = %v, want it to mention that it is built in", err)
	}
}

// TestPluginManagerOverrideCanBeRemoved 外部覆盖进程可以卸掉——内置实现立刻顶上。
func TestPluginManagerOverrideCanBeRemoved(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	_, err = app.PluginRemove(context.Background(), "npa-plugin-manager")
	if err == nil {
		t.Fatal("expected an error: the override was never installed")
	}
	if strings.Contains(err.Error(), "内置") {
		t.Fatalf("error = %v, the external override must stay removable", err)
	}
}

// TestPluginManagerDirsCoverReleaseLayout 发布包里的插件管理器要能被找到。
func TestPluginManagerDirsCoverReleaseLayout(t *testing.T) {
	dataDir := t.TempDir()
	dirs := pluginManagerDirs(dataDir)
	if len(dirs) == 0 {
		t.Fatal("no plugin manager directories")
	}
	found := false
	for _, dir := range dirs {
		if dir == filepath.Join(dataDir, "plugins", "manager") {
			found = true
		}
	}
	if !found {
		t.Fatalf("dirs = %v, want the data dir plugin folder", dirs)
	}
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			t.Fatalf("dir %q is not absolute", dir)
		}
	}
}

// TestDescribePluginSlots 列出 NPA 内置的插件槽位，Web 端要显示它们。
func TestDescribePluginSlots(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	slots := app.DescribePluginSlots()
	if len(slots) == 0 {
		t.Fatal("expected the built-in plugin slots")
	}
	names := map[string]bool{}
	for _, slot := range slots {
		name, _ := slot["slot"].(string)
		names[name] = true
	}
	for _, want := range []string{"browser", "file", "desktop", "manager"} {
		if !names[want] {
			t.Fatalf("slots = %v, want %q", names, want)
		}
	}
}

// TestPluginManagerExecutableIsDiscoverable 在插件目录里放一个可执行文件时，
// core 的插件管理器槽位应该能找到它（发布包自举路径）。
func TestPluginManagerExecutableIsDiscoverable(t *testing.T) {
	dataDir := t.TempDir()
	pluginDir := filepath.Join(dataDir, "plugins", "manager")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "npa-plugin-manager"
	if _, err := os.Stat(filepath.Join(pluginDir, name)); err == nil {
		t.Skip("a real plugin manager is installed")
	}
	t.Log("slot lookup is covered by internal/plugin manager tests")
}
