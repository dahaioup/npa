package service

import (
	"testing"

	"github.com/npa/npa/internal/plugin"
	"github.com/npa/npa/internal/state"
)

// newPluginHTTPTestService 起一个干净的 service（临时数据目录）。
func newPluginHTTPTestService(t *testing.T) (*Service, *state.Store) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return New(store), store
}

// TestPluginHTTPDefaults 每个带 HTTP 的插件槽位都要有默认端口与默认开放度：
//
//	浏览器插件  56753 / local（只需要本机访问，MCP 由 NPA 代理出去）
//	文件插件    56751 / auto（跟随 NPA 局域网开关）
//	远控插件    56750 / lan（本来就是给局域网其它设备用的）
func TestPluginHTTPDefaults(t *testing.T) {
	app, _ := newPluginHTTPTestService(t)

	cases := []struct {
		slot     string
		port     int
		exposure string
		hasMCP   bool
	}{
		{PluginSlotBrowser, 56753, PluginExposureLocal, true},
		{PluginSlotFile, 56751, PluginExposureAuto, false},
		{PluginSlotDesktop, 56750, PluginExposureLAN, false},
	}
	for _, item := range cases {
		info := app.PluginHTTP(item.slot)
		if info == nil {
			t.Fatalf("slot %s has no HTTP service", item.slot)
		}
		if info.Port != item.port {
			t.Errorf("slot %s port = %d, want %d", item.slot, info.Port, item.port)
		}
		if info.Exposure != item.exposure {
			t.Errorf("slot %s exposure = %q, want %q", item.slot, info.Exposure, item.exposure)
		}
		if info.HasMCP != item.hasMCP {
			t.Errorf("slot %s has_mcp = %v, want %v", item.slot, info.HasMCP, item.hasMCP)
		}
		if info.MCPPath != "/plugins/"+item.slot+"/mcp" {
			t.Errorf("slot %s mcp path = %q", item.slot, info.MCPPath)
		}
	}
	// 插件管理器是内置能力，没有自己的 HTTP 服务。
	if app.PluginHTTP(PluginSlotManager) != nil {
		t.Errorf("manager slot should not have an HTTP service")
	}
}

// TestPluginHTTPExposureAutoFollowsLan 开放度 auto 要跟随 NPA 的局域网开关。
func TestPluginHTTPExposureAutoFollowsLan(t *testing.T) {
	app, store := newPluginHTTPTestService(t)

	// lan_enabled 缺省是开的，所以 auto 先解析成 lan。
	if scope := app.PluginScope(PluginSlotFile); scope != "lan" {
		t.Errorf("auto exposure with lan enabled = %q, want lan", scope)
	}
	if err := store.SaveSettings(map[string]string{"lan_enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	if scope := app.PluginScope(PluginSlotFile); scope != "local" {
		t.Errorf("auto exposure with lan disabled = %q, want local", scope)
	}
	// local / lan 是硬指定，不受局域网开关影响。
	if scope := app.PluginScope(PluginSlotBrowser); scope != "local" {
		t.Errorf("browser slot scope = %q, want local", scope)
	}
	if host := app.PluginBindHost(PluginSlotBrowser); host != "127.0.0.1" {
		t.Errorf("browser bind host = %q, want 127.0.0.1", host)
	}
	if host := app.PluginBindHost(PluginSlotDesktop); host != "0.0.0.0" {
		t.Errorf("desktop bind host = %q, want 0.0.0.0", host)
	}
}

// TestPluginHTTPPortOverride 端口可以改，改坏了要退回默认值。
func TestPluginHTTPPortOverride(t *testing.T) {
	app, store := newPluginHTTPTestService(t)

	if err := store.SaveSettings(map[string]string{"browser_plugin_port": "56999"}); err != nil {
		t.Fatal(err)
	}
	if port := app.PluginHTTPPort(PluginSlotBrowser); port != 56999 {
		t.Errorf("port = %d, want 56999", port)
	}
	if err := store.SaveSettings(map[string]string{"browser_plugin_port": "not-a-port"}); err != nil {
		t.Fatal(err)
	}
	if port := app.PluginHTTPPort(PluginSlotBrowser); port != 56753 {
		t.Errorf("port = %d, want the default 56753", port)
	}
}

// TestPluginMCPAllowedPerFace 插件的 MCP 关掉之后只能走本地面（56700），
// mesh 面（56701）不再提供它。
func TestPluginMCPAllowedPerFace(t *testing.T) {
	app, store := newPluginHTTPTestService(t)

	if !app.PluginMCPEnabled(PluginSlotBrowser) {
		t.Fatalf("browser MCP should be enabled by default")
	}
	if !app.PluginMCPAllowed(PluginSlotBrowser, "mesh") {
		t.Errorf("mesh face should be allowed by default")
	}

	if err := store.SaveSettings(map[string]string{"browser_plugin_mcp": "false"}); err != nil {
		t.Fatal(err)
	}
	if app.PluginMCPEnabled(PluginSlotBrowser) {
		t.Errorf("browser MCP should be disabled")
	}
	if app.PluginMCPAllowed(PluginSlotBrowser, "mesh") {
		t.Errorf("mesh face must not serve a disabled plugin MCP")
	}
	if !app.PluginMCPAllowed(PluginSlotBrowser, "local") {
		t.Errorf("local face must keep serving the plugin MCP")
	}
}

// TestPluginMCPDisabledRemovesCapabilityFromMeshFace 关掉插件的 MCP 之后，
// mesh 面上就不该再有这个插件的工具；本地面照旧。
func TestPluginMCPDisabledRemovesCapabilityFromMeshFace(t *testing.T) {
	app, store := newPluginHTTPTestService(t)

	if !app.MCPCapabilitiesFor(MCPFaceMesh)["browser"] {
		t.Fatalf("mesh face should expose the browser capability by default")
	}
	if err := store.SaveSettings(map[string]string{"browser_plugin_mcp": "false"}); err != nil {
		t.Fatal(err)
	}
	if app.MCPCapabilitiesFor(MCPFaceMesh)["browser"] {
		t.Errorf("mesh face must not expose the browser capability when its MCP is off")
	}
	if !app.MCPCapabilitiesFor(MCPFaceLocal)["browser"] {
		t.Errorf("local face must keep the browser capability")
	}
}

// TestValidatePluginParamValue 开放度与端口的取值要在写设置时就被拦住。
func TestValidatePluginParamValue(t *testing.T) {
	exposure := plugin.SettingSpec{Key: "exposure"}
	if err := validatePluginParamValue(exposure, "lan"); err != nil {
		t.Errorf("lan should be a valid exposure: %v", err)
	}
	if err := validatePluginParamValue(exposure, "public"); err == nil {
		t.Errorf("an unknown exposure must be rejected")
	}
	port := plugin.SettingSpec{Key: "port"}
	if err := validatePluginParamValue(port, "56753"); err != nil {
		t.Errorf("56753 should be a valid port: %v", err)
	}
	if err := validatePluginParamValue(port, "70000"); err == nil {
		t.Errorf("an out-of-range port must be rejected")
	}
}
