package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	npamcp "github.com/npa/npa/internal/mcp"
	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

// TestBrowserToolsRegisteredWithNodeID 浏览器能力开启时，obscura 的工具要挂到
// MCP 面上，并且每个工具都带上 node_id 参数（决定在哪台机器上跑）。
func TestBrowserToolsRegisteredWithNodeID(t *testing.T) {
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

	list, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*sdk.Tool{}
	for _, tool := range list.Tools {
		tools[tool.Name] = tool
	}
	if len(tools) == 0 {
		t.Fatal("no tools registered")
	}
	navigate, ok := tools["browser_navigate"]
	if !ok {
		t.Fatal("browser_navigate was not registered")
	}
	schema, err := json.Marshal(navigate.InputSchema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(schema, &document); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	properties, _ := document["properties"].(map[string]any)
	if properties == nil {
		t.Fatal("schema has no properties")
	}
	if _, ok := properties["node_id"]; !ok {
		t.Error("browser_navigate schema is missing the node_id property")
	}
	if _, ok := properties["url"]; !ok {
		t.Error("browser_navigate schema lost the original url property")
	}
	// obscura 的工具清单要整体挂上，不是只挂一两个。
	browserTools := 0
	for name := range tools {
		if len(name) > 8 && name[:8] == "browser_" {
			browserTools++
		}
	}
	if browserTools < 30 {
		t.Errorf("expected the full obscura tool list, got %d browser tools", browserTools)
	}
}

// TestBrowserToolsFollowCapabilitySwitch 停用浏览器插件后，工具要从 MCP 面上撤掉。
func TestBrowserToolsFollowCapabilitySwitch(t *testing.T) {
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

	if err := app.SaveSettings(map[string]string{"plugin.browser.enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	result, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range result.Tools {
		if len(tool.Name) > 8 && tool.Name[:8] == "browser_" {
			t.Fatalf("browser tool %q stayed registered after the browser plugin was disabled", tool.Name)
		}
	}
}
