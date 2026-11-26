package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

// TestPluginRoutes 插件仓库端点要能用内置的插件管理器给出结果。
func TestPluginRoutes(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)

	// 指一个本地索引，别让测试去碰公网。
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":1,"plugins":[{"id":"npa-demo","version":"1.0.0","install":{"dir":"plugins/demo","executable":"npa-demo"}}]}`))
	}))
	defer registry.Close()
	if err := store.SaveSettings(map[string]string{"plugin_registry_url": registry.URL}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/plugins/status")
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
	if status["registry_url"] == nil {
		t.Errorf("status should report the registry url: %#v", status)
	}
	// 没有外部进程时，状态要说明是内置实现顶上。
	if status["plugin_status"] != "builtin" {
		t.Errorf("plugin_status = %v, want builtin", status["plugin_status"])
	}

	// 清单接口由内置实现来服务。
	response, err = http.Get(server.URL + "/api/plugins/list")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list endpoint returned %d", response.StatusCode)
	}
	var listing struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Plugins) != 1 || listing.Plugins[0]["id"] != "npa-demo" {
		t.Fatalf("plugins = %#v", listing.Plugins)
	}
}

// TestPluginManagerCannotBeDisabledViaWeb 内置的插件管理器不接受停用请求。
func TestPluginManagerCannotBeDisabledViaWeb(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()

	body, _ := json.Marshal(map[string]any{"slot": "manager", "enabled": false})
	request, err := http.NewRequest(http.MethodPut, server.URL+"/api/plugins/settings", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("disabling the plugin manager returned %d, want 400", response.StatusCode)
	}
	if !app.PluginEnabled(service.PluginSlotManager) {
		t.Fatal("the plugin manager must stay enabled")
	}
}

// TestPluginInstallValidatesPayload 安装接口要校验参数。
func TestPluginInstallValidatesPayload(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()

	payload, _ := json.Marshal(map[string]any{})
	response, err := http.Post(server.URL+"/api/plugins/install", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusInternalServerError {
		t.Fatalf("install endpoint returned %d", response.StatusCode)
	}
}

// TestPluginSettingsRoutes 插件设置接口要能读出插件视图，并接受启停与参数变更。
func TestPluginSettingsRoutes(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/plugins/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("plugin settings endpoint returned %d", response.StatusCode)
	}
	var payload struct {
		Plugins []service.PluginView `json:"plugins"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Plugins) != 4 {
		t.Fatalf("expected four plugin slots, got %d: %#v", len(payload.Plugins), payload.Plugins)
	}

	// 停用浏览器插件。
	body, _ := json.Marshal(map[string]any{"slot": "browser", "enabled": false})
	request, err := http.NewRequest(http.MethodPut, server.URL+"/api/plugins/settings", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("plugin settings update returned %d", response.StatusCode)
	}
	if got := app.Settings()["plugin.browser.enabled"]; got != "false" {
		t.Fatalf("enable state was not applied: %q", got)
	}

	// 插件没声明的参数要被拒绝。
	body, _ = json.Marshal(map[string]any{"slot": "browser", "values": map[string]string{"not_declared": "1"}})
	request, err = http.NewRequest(http.MethodPut, server.URL+"/api/plugins/settings", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("undeclared param must be rejected with 400, got %d", response.StatusCode)
	}
}
