package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/npa/npa/internal/plugin"
	"github.com/npa/npa/internal/pluginmanager"
	"github.com/npa/npa/internal/pluginregistry"
)

// newTestManager 造一个进程内的管理器替身：逻辑与真实进程一致（复用内置实现）。
func newTestManager(t *testing.T, registry string) *manager {
	t.Helper()
	m := &manager{registry: registry}
	m.builtin = pluginmanager.NewBuiltin(t.TempDir(), m.registryURL)
	return m
}

func TestManagerInitializeReportsPluginInfo(t *testing.T) {
	manager := newTestManager(t, "")
	result, err := manager.handle(context.Background(), "plugin.initialize", nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	info, ok := result.(plugin.Info)
	if !ok {
		t.Fatalf("initialize returned %T, want plugin.Info", result)
	}
	if info.ID != pluginID {
		t.Fatalf("plugin id = %s, want %s", info.ID, pluginID)
	}
	if info.Protocol != "1" {
		t.Fatalf("protocol = %s, want 1", info.Protocol)
	}
	if len(info.Capabilities) != 1 || info.Capabilities[0] != "plugin" {
		t.Fatalf("capabilities = %v", info.Capabilities)
	}
}

func TestManagerRejectsUnknownMethod(t *testing.T) {
	manager := newTestManager(t, "")
	if _, err := manager.handle(context.Background(), "manager.nope", nil); err == nil {
		t.Fatal("expected an error for an unknown method")
	}
}

func TestManagerInstallRequiresID(t *testing.T) {
	manager := newTestManager(t, "")
	if _, err := manager.handle(context.Background(), "manager.install", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected an error when the plugin id is missing")
	}
}

// registryServer serves a small registry document with one plugin entry.
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
			"capabilities": ["demo"],
			"requires": ["demo-engine"],
			"install": {"dir": "plugins/demo", "executable": "npa-demo"},
			"artifacts": [{"os": %q, "arch": %q, "url": "http://example/npa-demo", "sha256": "aa", "size": 10, "format": "binary"}]
		}, {
			"id": "demo-engine",
			"name": "Demo Engine",
			"version": "0.1.0",
			"kind": "dependency",
			"install": {"dir": "bin", "executable": "demo"},
			"artifacts": [{"os": %q, "arch": %q, "url": "http://example/demo", "sha256": "bb", "size": 20, "format": "binary"}]
		}]
	}`, runtime.GOOS, runtime.GOARCH, runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(document))
	})
	return httptest.NewServer(mux)
}

func TestManagerListReportsRegistryEntries(t *testing.T) {
	server := registryServer(t)
	defer server.Close()

	manager := newTestManager(t, server.URL+"/registry.json")
	result, err := manager.handle(context.Background(), "manager.list", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("list returned %T", result)
	}
	plugins, ok := payload["plugins"].([]map[string]any)
	if !ok {
		t.Fatalf("plugins payload = %T", payload["plugins"])
	}
	if len(plugins) != 2 {
		t.Fatalf("plugins = %d, want 2", len(plugins))
	}
	if plugins[0]["id"] != "npa-demo" || plugins[0]["version"] != "1.2.3" {
		t.Fatalf("unexpected entry: %+v", plugins[0])
	}
	if plugins[0]["available"] != true {
		t.Fatalf("entry should be available for %s/%s: %+v", runtime.GOOS, runtime.GOARCH, plugins[0])
	}
	if plugins[0]["installed"] != false {
		t.Fatalf("entry should not be installed yet: %+v", plugins[0])
	}
	if payload["registry_url"] != server.URL+"/registry.json" {
		t.Fatalf("registry url = %v", payload["registry_url"])
	}
}

func TestManagerInstallRejectsUnknownPlugin(t *testing.T) {
	server := registryServer(t)
	defer server.Close()

	manager := newTestManager(t, server.URL+"/registry.json")
	if _, err := manager.handle(context.Background(), "manager.install", json.RawMessage(`{"id":"nope"}`)); err == nil {
		t.Fatal("expected an error for an unknown plugin")
	}
}

func TestSlotsOfDetectsPluginSlots(t *testing.T) {
	slots := pluginmanager.SlotsOf([]pluginregistry.InstallResult{
		{ID: "demo-engine", Dir: "bin"},
		{ID: "npa-demo", Dir: "plugins/demo"},
	})
	if len(slots) != 1 || slots[0] != "demo" {
		t.Fatalf("slots = %v, want [demo]", slots)
	}
}
