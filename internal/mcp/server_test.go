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
	// 核心能力（SSH / 隧道 / 节点发现 / 中继）随 MCP 服务常开，不再逐项开关。
	if names := list(); !names["ssh.exec"] || !names["nodes.list"] || !names["network.subnets"] {
		t.Fatalf("core tools must be registered by default: %#v", names)
	}
	// 插件能力由插件启停决定：停用文件插件，文件工具要从 MCP 面上撤掉。
	if err := app.SaveSettings(map[string]string{"plugin.file.enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); names["file.list"] || names["file.read"] || names["file.write"] || names["file.remove"] {
		t.Fatalf("file tools must disappear when the file plugin is disabled: %#v", names)
	}
	if err := app.SaveSettings(map[string]string{"plugin.file.enabled": "true"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); !names["file.list"] || !names["file.read"] || !names["file.write"] || !names["file.remove"] {
		t.Fatalf("file tools were not registered after enabling the file plugin: %#v", names)
	}
	// 桌面插件同理。
	if err := app.SaveSettings(map[string]string{"plugin.desktop.enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); names["desktop.screens"] || names["desktop.screenshot"] || names["desktop.mouse_move"] || names["desktop.mouse_click"] || names["desktop.mouse_scroll"] {
		t.Fatalf("desktop tools must disappear when the desktop plugin is disabled: %#v", names)
	}
	if err := app.SaveSettings(map[string]string{"plugin.desktop.enabled": "true"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); !names["desktop.screens"] || !names["desktop.screenshot"] || !names["desktop.mouse_move"] || !names["desktop.mouse_click"] || !names["desktop.mouse_scroll"] {
		t.Fatalf("desktop tools were not registered after enabling the desktop plugin: %#v", names)
	}
	// 局域网开关只管暴露面（56701 是否对局域网开放），不影响本机工具面。
	if err := app.SaveSettings(map[string]string{"lan_enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); !names["discovery.nodes"] || !names["relay.routes"] {
		t.Fatalf("mesh tools must stay available while LAN exposure is off: %#v", names)
	}
	if err := app.SaveSettings(map[string]string{"lan_enabled": "true"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); !names["discovery.nodes"] || !names["relay.routes"] {
		t.Fatalf("mesh tools must stay available after turning LAN exposure back on: %#v", names)
	}
}
