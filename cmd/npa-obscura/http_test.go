package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestHTTPServicePageAndStatus 插件自己的 HTTP 服务要提供管理页与状态。
func TestHTTPServicePageAndStatus(t *testing.T) {
	engine := &engine{callTimeout: 30 * time.Second}
	service := newHTTPService(engine, t.TempDir(), 0, "127.0.0.1")
	server := httptest.NewServer(service.handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("page returned %d, want 200", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Errorf("page content type = %q, want text/html", contentType)
	}

	response, err = http.Get(server.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status returned %d, want 200", response.StatusCode)
	}
	var status statusPayload
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Engine != "obscura" {
		t.Errorf("engine = %q, want obscura", status.Engine)
	}
	if status.MCPPath != "/mcp" {
		t.Errorf("mcp path = %q, want /mcp", status.MCPPath)
	}
	if status.ProxyPath != "/plugins/browser/mcp" {
		t.Errorf("proxy path = %q, want /plugins/browser/mcp", status.ProxyPath)
	}
	if status.Tools == 0 {
		t.Errorf("status should report the tool count")
	}
}

// TestHTTPServiceInstallRejectsUnknownVariant 安装形态不认识时要明确报错。
func TestHTTPServiceInstallRejectsUnknownVariant(t *testing.T) {
	engine := &engine{callTimeout: 30 * time.Second}
	service := newHTTPService(engine, t.TempDir(), 0, "127.0.0.1")
	server := httptest.NewServer(service.handler())
	defer server.Close()

	response, err := http.Post(server.URL+"/api/install", "application/json", strings.NewReader(`{"variant":"bogus"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("install returned %d, want 400", response.StatusCode)
	}
}

// TestHTTPServicePreviewNeedsURL 预览必须给网址。
func TestHTTPServicePreviewNeedsURL(t *testing.T) {
	engine := &engine{callTimeout: 30 * time.Second}
	service := newHTTPService(engine, t.TempDir(), 0, "127.0.0.1")
	server := httptest.NewServer(service.handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/preview")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("preview returned %d, want 400", response.StatusCode)
	}
}

// TestHTTPServiceServesMCP 插件自己的 /mcp 要能用 MCP 客户端连上，
// 并把 obscura 的工具挂出来（引擎没装时用内嵌目录兜底）。
func TestHTTPServiceServesMCP(t *testing.T) {
	engine := &engine{callTimeout: 30 * time.Second}
	service := newHTTPService(engine, t.TempDir(), 0, "127.0.0.1")
	server := httptest.NewServer(service.handler())
	defer server.Close()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) < 30 {
		t.Errorf("tool count = %d, want the obscura catalog", len(list.Tools))
	}
	found := false
	for _, tool := range list.Tools {
		if tool != nil && tool.Name == "browser_navigate" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("browser_navigate should be exposed by the plugin MCP")
	}
}
