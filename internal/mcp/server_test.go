package mcp_test

import (
	"context"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	npamcp "github.com/npa/npa/internal/mcp"
	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

func TestCapabilityToolsRefresh(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
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
	list := func() map[string]bool {
		result, err := clientSession.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, tool := range result.Tools {
			names[tool.Name] = true
		}
		return names
	}
	// 调试期默认能力全开；此处先显式关闭，验证能力开关确实能收窄 MCP 工具面。
	if err := app.SaveSettings(map[string]string{
		"mcp_bind_scope": "loopback",
		"mcp_ssh":        "false", "mcp_file": "false", "mcp_tunnel": "false",
		"mcp_screenshot": "false", "mcp_mouse": "false",
	}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); !names["nodes.list"] || names["ssh.exec"] {
		t.Fatalf("unexpected default tools: %#v", names)
	}
	if err := app.SaveSettings(map[string]string{"mcp_ssh": "true"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); !names["ssh.exec"] {
		t.Fatalf("SSH tool was not registered after enabling capability: %#v", names)
	}
	if err := app.SaveSettings(map[string]string{"mcp_file": "true"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); !names["file.list"] || !names["file.read"] || !names["file.write"] || !names["file.remove"] {
		t.Fatalf("SFTP tools were not registered after enabling File capability: %#v", names)
	}
	if err := app.SaveSettings(map[string]string{"mcp_screenshot": "true", "mcp_mouse": "true"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); !names["desktop.screens"] || !names["desktop.screenshot"] || !names["desktop.mouse_move"] || !names["desktop.mouse_click"] || !names["desktop.mouse_scroll"] {
		t.Fatalf("desktop tools were not registered after enabling capabilities: %#v", names)
	}
	if err := app.SaveSettings(map[string]string{"mcp_bind_scope": "lan", "mcp_discovery": "true", "mcp_relay": "true"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); !names["discovery.nodes"] || !names["discovery.refresh"] || !names["network.subnets"] || !names["relay.routes"] || !names["relay.open"] {
		t.Fatalf("mesh tools were not registered after enabling capabilities: %#v", names)
	}
	if err := app.SaveSettings(map[string]string{"mcp_bind_scope": "loopback"}); err != nil {
		t.Fatal(err)
	}
	if settings := app.Settings(); settings["mcp_discovery"] != "false" || settings["mcp_relay"] != "false" {
		t.Fatalf("loopback scope must disable mesh permissions: %#v", settings)
	}
	manager.Refresh()
	if names := list(); names["discovery.nodes"] || names["relay.routes"] {
		t.Fatalf("mesh tools must be unavailable in loopback scope: %#v", names)
	}
	if err := app.SaveSettings(map[string]string{"mcp_discovery": "true"}); err == nil {
		t.Fatal("loopback scope must reject enabling discovery permission")
	}
	if err := app.SaveSettings(map[string]string{"mcp_ssh": "false"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); names["ssh.exec"] {
		t.Fatalf("SSH tool remained registered after disabling capability: %#v", names)
	}
}
