package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/npa/npa/internal/browser"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/state"
)

// TestBrowserCallDispatchesToRemoteNode 验证「浏览器调用派发到远端 NPA 节点」
// 这条路：调用要真的走到节点的 mesh HTTP 端点，并把结果原样带回来。
//
// 这条路径不能只靠单测自洽：远端节点的 /api/browser/call 是注册在 peer 面
// 上的，路由名或端口约定错了，只有真发一次请求才会发现。
func TestBrowserCallDispatchesToRemoteNode(t *testing.T) {
	var received map[string]any
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/browser/call" {
			t.Errorf("remote path = %q, want /api/browser/call", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("remote body is not JSON: %v (%s)", err, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(browser.CallResult{
			Content: []browser.Content{{Type: "text", Text: "remote result"}},
		})
	}))
	defer remote.Close()

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	// 把目标节点登记成一个反向接入的 peer 节点，地址指向本地测试服务器。
	parsed, err := url.Parse(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	node := model.Node{
		ID:         "remote-node-1",
		Name:       "remote",
		Address:    parsed.Hostname(),
		Port:       port,
		Username:   "npa",
		AuthMethod: "peer",
		Protocol:   "peer",
		Status:     "online",
		PeerPort:   port,
		Registered: true,
	}
	if err := store.SaveNode(node); err != nil {
		t.Fatalf("save node: %v", err)
	}

	result, err := app.BrowserCall(context.Background(), node.ID, "browser_navigate", map[string]any{
		"url": "https://example.com",
	})
	if err != nil {
		t.Fatalf("browser call to a remote node: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "remote result" {
		t.Fatalf("result = %+v, want the remote node's result", result)
	}
	if received == nil {
		t.Fatal("the remote node did not receive the call")
	}
	if received["tool"] != "browser_navigate" {
		t.Errorf("remote received tool = %v", received["tool"])
	}
	arguments, _ := received["arguments"].(map[string]any)
	if arguments["url"] != "https://example.com" {
		t.Errorf("remote received arguments = %v", arguments)
	}
}

// TestBrowserCallRejectsUnknownTarget 不带 node_id、也不指向任何节点时，
// 调用要明确报错，而不是悄悄落到本机。
func TestBrowserCallRejectsUnknownTarget(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	if _, err := app.BrowserCall(context.Background(), "no-such-node", "browser_navigate", map[string]any{
		"url": "https://example.com",
	}); err == nil {
		t.Fatal("expected an error for an unknown node")
	}
}

// TestBrowserCallRequiresTool 工具名不能为空。
func TestBrowserCallRequiresTool(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	if _, err := app.BrowserCall(context.Background(), "", "  ", nil); err == nil {
		t.Fatal("expected an error for an empty tool name")
	}
}
