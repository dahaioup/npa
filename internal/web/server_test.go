package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

type fakeMCP struct{}

func (fakeMCP) Status() map[string]any { return map[string]any{"enabled": false} }

func TestEmbeddedWebAndSettings(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()
	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", response.StatusCode)
	}
	buf := make([]byte, 512)
	n, _ := response.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "NPA") {
		t.Fatalf("embedded UI did not load: %q", string(buf[:n]))
	}
	settings := app.Settings()
	// 调试期默认：MCP 与 Discovery 能力全开、MCP 走局域网；Web 管理面仍固定回环。
	if settings["mcp_ssh"] != "true" || settings["mcp_file"] != "true" || settings["mcp_tunnel"] != "true" {
		t.Fatalf("MCP capabilities must default on for debugging: %#v", settings)
	}
	if settings["discovery_enabled"] != "true" || settings["mcp_discovery"] != "true" || settings["mcp_relay"] != "true" {
		t.Fatalf("mesh capabilities must default on for debugging: %#v", settings)
	}
	if settings["mcp_bind_scope"] != "lan" {
		t.Fatalf("MCP must default to lan scope for debugging: %#v", settings)
	}
	for _, key := range []string{"peer_enabled", "peer_bind_scope", "peer_port", "peer_pool_size"} {
		if settings[key] == "" {
			t.Fatalf("missing peer setting default %s: %#v", key, settings)
		}
	}
	for _, path := range []string{"/api/discovery", "/api/network/subnets", "/api/relay/routes"} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %d", path, response.StatusCode)
		}
		response.Body.Close()
	}
}

func TestPortMappingLifecycle(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()
	defer app.Close()

	body := `{"name":"RustDesk","target_host":"192.168.50.21","target_port":21115,"listen_address":"127.0.0.1","listen_port":0,"auto_start":true}`
	response, err := http.Post(server.URL+"/api/portmappings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("create returned %d", response.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("mapping id must be assigned")
	}
	started, err := http.Post(server.URL+"/api/portmappings/"+created.ID+"/start", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var running map[string]any
	if err := json.NewDecoder(started.Body).Decode(&running); err != nil {
		t.Fatal(err)
	}
	started.Body.Close()
	if started.StatusCode != http.StatusOK {
		t.Fatalf("start returned %d (%v)", started.StatusCode, running)
	}
	if running["status"] != "running" {
		t.Fatalf("mapping must be running: %#v", running)
	}
	listResponse, err := http.Get(server.URL + "/api/portmappings")
	if err != nil {
		t.Fatal(err)
	}
	var mappings []model.PortMapping
	if err := json.NewDecoder(listResponse.Body).Decode(&mappings); err != nil {
		t.Fatal(err)
	}
	listResponse.Body.Close()
	if len(mappings) != 1 || mappings[0].Status != "running" || mappings[0].RuntimePort == 0 {
		t.Fatalf("unexpected mappings: %#v", mappings)
	}
	stopped, err := http.Post(server.URL+"/api/portmappings/"+created.ID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped.Body.Close()
	if stopped.StatusCode != http.StatusOK {
		t.Fatalf("stop returned %d", stopped.StatusCode)
	}
	deleted, err := http.NewRequest(http.MethodDelete, server.URL+"/api/portmappings/"+created.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(deleted)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete returned %d", response.StatusCode)
	}
	if _, err := store.GetPortMapping(created.ID); err == nil {
		t.Fatal("mapping must be removed from the store")
	}
}

func TestPortMappingRejectsPublicTarget(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()
	defer app.Close()
	body := `{"name":"bad","target_host":"8.8.8.8","target_port":53,"listen_port":0}`
	response, err := http.Post(server.URL+"/api/portmappings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("public target must be rejected, got %d", response.StatusCode)
	}
}

// Web 退出服务：返回 stopping 并触发注入的优雅关闭钩子，同时留下审计。
func TestShutdownEndpointTriggersGracefulHook(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := New(app, fakeMCP{})
	called := make(chan struct{}, 1)
	server.ShutdownFunc = func() { called <- struct{}{} }
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	defer app.Close()

	response, err := http.Post(ts.URL+"/api/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("退出接口返回 %d", response.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["stopping"] != true {
		t.Fatalf("响应应先告知正在停止: %#v", payload)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("ShutdownFunc 没有被调用")
	}
	events, err := app.AuditEvents(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Operation == "service.shutdown" {
			found = true
		}
	}
	if !found {
		t.Fatalf("退出操作必须写审计: %#v", events)
	}
}

// 未注入钩子时应明确拒绝，而不是假装成功。
func TestShutdownEndpointWithoutHook(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	defer app.Close()
	ts := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer ts.Close()
	response, err := http.Post(ts.URL+"/api/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatal("没有关闭钩子时不应返回成功")
	}
}
