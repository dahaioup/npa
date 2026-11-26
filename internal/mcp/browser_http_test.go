package mcp_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	npamcp "github.com/npa/npa/internal/mcp"
	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

// TestBrowserToolsOverStreamableHTTP 走生产上真实的那条路：
// HTTP + Streamable HTTP 传输（56701/mcp 用的就是它）。
//
// in-memory 传输测不出「HTTP 面上工具到底挂没挂上、参数 schema 有没有原样传出去」
// 这类问题，所以这里起一个真的 HTTP server，用 SDK 的 HTTP 客户端连上去。
func TestBrowserToolsOverStreamableHTTP(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)

	manager := npamcp.NewManager(app)
	server := httptest.NewServer(manager.Handler())
	defer server.Close()

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: server.URL,
	}, nil)
	if err != nil {
		t.Fatalf("connect over streamable HTTP: %v", err)
	}
	defer session.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	listing, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	var browserTools int
	var navigate *sdk.Tool
	for _, tool := range listing.Tools {
		if !strings.HasPrefix(tool.Name, "browser_") {
			continue
		}
		browserTools++
		if tool.Name == "browser_navigate" {
			navigate = tool
		}
	}
	if browserTools < 30 {
		t.Errorf("browser tools over HTTP = %d, want the obscura catalogue (38)", browserTools)
	}
	if navigate == nil {
		t.Fatal("browser_navigate is missing from the HTTP tool list")
	}
	// node_id 必须出现在 HTTP 面上：远端节点派发全靠它。
	schema, _ := navigate.InputSchema.(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["node_id"]; !ok {
		t.Errorf("browser_navigate schema over HTTP has no node_id property: %v", schema)
	}
	if _, ok := properties["url"]; !ok {
		t.Errorf("browser_navigate schema over HTTP lost obscura's own url property: %v", schema)
	}
}

// TestBrowserCallOverStreamableHTTP 再进一步：真的通过 HTTP 面调一次浏览器工具，
// 确认参数能从 HTTP 传输里进来、结果也能回去。
func TestBrowserCallOverStreamableHTTP(t *testing.T) {
	obscura := obscuraBinary(t)
	if obscura == "" {
		t.Skip("no obscura binary available")
	}
	pluginDir := buildBrowserPlugin(t)

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	app.BrowserPlugin.PreferredName = "npa-obscura"
	app.BrowserPlugin.SearchDirs = []string{pluginDir}
	app.BrowserPlugin.Args = []string{"--obscura", obscura}
	if err := app.BrowserPlugin.Start(context.Background()); err != nil {
		t.Fatalf("start the obscura plugin: %v", err)
	}
	defer app.BrowserPlugin.Close(context.Background())

	manager := npamcp.NewManager(app)
	server := httptest.NewServer(manager.Handler())
	defer server.Close()

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: server.URL,
	}, nil)
	if err != nil {
		t.Fatalf("connect over streamable HTTP: %v", err)
	}
	defer session.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "browser_navigate",
		Arguments: map[string]any{"url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("browser_navigate over HTTP: %v", err)
	}
	if result.IsError {
		t.Fatalf("browser_navigate reported an error: %+v", result.Content)
	}
	var builder strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			builder.WriteString(text.Text)
		}
	}
	if !strings.Contains(builder.String(), "Example Domain") {
		t.Errorf("page text missing expected content, got: %s", builder.String())
	}
}
