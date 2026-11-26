package service_test

import (
	"context"
	"testing"

	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

// hasParam 判断参数表里有没有某个键，并且落到预期的设置键上。
func hasParam(params []service.PluginParamView, key, setting string) bool {
	for _, param := range params {
		if param.Key == key && param.Setting == setting {
			return true
		}
	}
	return false
}

// TestPluginViewsExposeDeclaredParams 插件设置页要能看到每个槽位自己声明的参数。
func TestPluginViewsExposeDeclaredParams(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)

	views := app.PluginViews()
	if len(views) != 4 {
		t.Fatalf("expected four plugin slots, got %d: %#v", len(views), views)
	}
	bySlot := map[string]service.PluginView{}
	for _, view := range views {
		bySlot[view.Slot] = view
	}
	// 文件槽位的参数由内置文件插件自己声明。
	file := bySlot[service.PluginSlotFile]
	if file.ID != "npa.file" || !file.Builtin {
		t.Fatalf("file slot must fall back to the builtin plugin: %#v", file)
	}
	if !hasParam(file.Params, "port", "file_plugin_port") {
		t.Fatalf("file slot must expose its own port param: %#v", file.Params)
	}
	// 浏览器插件没装时，用 core 的兜底声明，参数照样能改。
	browser := bySlot[service.PluginSlotBrowser]
	if !hasParam(browser.Params, "timeout", "browser_obscura_timeout") {
		t.Fatalf("browser slot must expose the fallback params: %#v", browser.Params)
	}
	if !hasParam(browser.Params, "path", "browser_obscura_path") {
		t.Fatalf("browser slot must expose the fallback path param: %#v", browser.Params)
	}
	// 默认全部启用。
	for _, view := range views {
		if !view.Enabled {
			t.Fatalf("slot %s must be enabled by default", view.Slot)
		}
	}
}

// TestUpdatePluginSettingsRejectsUndeclaredParam 插件没声明的参数不能落库。
func TestUpdatePluginSettingsRejectsUndeclaredParam(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)

	if _, err := app.UpdatePluginSettings(context.Background(), service.PluginSettingsUpdate{
		Slot:   service.PluginSlotBrowser,
		Values: map[string]string{"not_declared": "1"},
	}); err == nil {
		t.Fatal("undeclared params must be rejected")
	}
	if _, err := app.UpdatePluginSettings(context.Background(), service.PluginSettingsUpdate{
		Slot:   service.PluginSlotBrowser,
		Values: map[string]string{"timeout": "90"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := app.Settings()["browser_obscura_timeout"]; got != "90" {
		t.Fatalf("declared param was not stored: %q", got)
	}
	// 直接往设置里塞未声明的插件参数，同样要被挡住。
	if err := app.SaveSettings(map[string]string{"plugin.browser.not_declared": "1"}); err == nil {
		t.Fatal("settings save must reject undeclared plugin params")
	}
}

// TestPluginEnabledDrivesCapabilities 停用插件要把对应能力从 MCP 面上撤掉。
func TestPluginEnabledDrivesCapabilities(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)

	disabled := false
	if _, err := app.UpdatePluginSettings(context.Background(), service.PluginSettingsUpdate{
		Slot:    service.PluginSlotBrowser,
		Enabled: &disabled,
	}); err != nil {
		t.Fatal(err)
	}
	if app.Settings()["plugin.browser.enabled"] != "false" {
		t.Fatalf("enable state was not stored: %#v", app.Settings())
	}
	capabilities := app.MCPCapabilities()
	if capabilities["browser"] {
		t.Fatal("browser capability must be off while the browser plugin is disabled")
	}
	if !capabilities["ssh"] {
		t.Fatal("ssh capability must stay on: it belongs to core, not to a plugin")
	}
	if !capabilities["file"] {
		t.Fatal("file capability must stay on: another plugin is disabled")
	}

	enabled := true
	if _, err := app.UpdatePluginSettings(context.Background(), service.PluginSettingsUpdate{
		Slot:    service.PluginSlotBrowser,
		Enabled: &enabled,
	}); err != nil {
		t.Fatal(err)
	}
	capabilities = app.MCPCapabilities()
	if !capabilities["browser"] {
		t.Fatal("browser capability must come back after enabling the browser plugin")
	}

	// 文件能力跟着文件插件的启停走。
	if err := app.SaveSettings(map[string]string{"plugin.file.enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	if app.MCPCapabilities()["file"] {
		t.Fatal("file capability must be off while the file plugin is disabled")
	}
}

// TestPluginViewsReflectEnableState 停用后页面要能看出插件是关的。
func TestPluginViewsReflectEnableState(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)

	disabled := false
	if _, err := app.UpdatePluginSettings(context.Background(), service.PluginSettingsUpdate{
		Slot:    service.PluginSlotBrowser,
		Enabled: &disabled,
	}); err != nil {
		t.Fatal(err)
	}
	for _, view := range app.PluginViews() {
		if view.Slot == service.PluginSlotBrowser && view.Enabled {
			t.Fatal("browser slot must report as disabled")
		}
	}
}

// TestBrowserSlotIsNotBuiltIn obscura 必须保持独立插件：core 里没有它的内置实现，
// 它是普通槽位——可以停用、可以卸载。内置且不可停用的只有插件管理器。
func TestBrowserSlotIsNotBuiltIn(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)

	views := app.PluginViews()
	bySlot := map[string]service.PluginView{}
	for _, view := range views {
		bySlot[view.Slot] = view
	}
	browser, ok := bySlot[service.PluginSlotBrowser]
	if !ok {
		t.Fatal("browser slot must exist")
	}
	if browser.Builtin {
		t.Fatalf("obscura must stay an independent plugin, not a builtin: %#v", browser)
	}
	if browser.Required {
		t.Fatalf("browser slot must be disable-able: %#v", browser)
	}
	manager, ok := bySlot[service.PluginSlotManager]
	if !ok {
		t.Fatal("manager slot must exist")
	}
	if !manager.Builtin || !manager.Required {
		t.Fatalf("the plugin manager must be builtin and required: %#v", manager)
	}

	// 浏览器槽位可以停用，停用后 browser 能力位随之关闭。
	disabled := false
	if _, err := app.UpdatePluginSettings(context.Background(), service.PluginSettingsUpdate{
		Slot:    service.PluginSlotBrowser,
		Enabled: &disabled,
	}); err != nil {
		t.Fatalf("disabling the browser slot must be allowed: %v", err)
	}
	if app.PluginEnabled(service.PluginSlotBrowser) {
		t.Fatal("browser slot must report disabled")
	}
	if app.MCPCapabilities()["browser"] {
		t.Fatal("the browser capability must follow the slot state")
	}
	// 插件管理器则拒绝停用。
	off := false
	if _, err := app.UpdatePluginSettings(context.Background(), service.PluginSettingsUpdate{
		Slot:    service.PluginSlotManager,
		Enabled: &off,
	}); err == nil {
		t.Fatal("the plugin manager must not be disable-able")
	}
	if !app.MCPCapabilities()["plugin"] {
		t.Fatal("the plugin capability must always be on")
	}
}
