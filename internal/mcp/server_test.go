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
	if err := app.SaveSettings(map[string]string{"mcp_ssh": "false"}); err != nil {
		t.Fatal(err)
	}
	manager.Refresh()
	if names := list(); names["ssh.exec"] {
		t.Fatalf("SSH tool remained registered after disabling capability: %#v", names)
	}
}
