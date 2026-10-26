package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

type fakeMCP struct{}

func (fakeMCP) Status() map[string]any { return map[string]any{"enabled": false} }

func TestEmbeddedWebAndSettings(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()
	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", response.StatusCode)
	}
	buf := make([]byte, 512)
	n, _ := response.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "NPA") {
		t.Fatalf("embedded UI did not load: %q", string(buf[:n]))
	}
	settings := app.Settings()
	// 调试期默认：MCP 与 Discovery 能力全开、MCP 走局域网；Web 管理面仍固定回环。
	if settings["mcp_ssh"] != "true" || settings["mcp_file"] != "true" || settings["mcp_tunnel"] != "true" {
		t.Fatalf("MCP capabilities must default on for debugging: %#v", settings)
	}
	if settings["discovery_enabled"] != "true" || settings["mcp_discovery"] != "true" || settings["mcp_relay"] != "true" {
		t.Fatalf("mesh capabilities must default on for debugging: %#v", settings)
	}
	if settings["mcp_bind_scope"] != "lan" {
		t.Fatalf("MCP must default to lan scope for debugging: %#v", settings)
	}
	for _, key := range []string{"peer_enabled", "peer_bind_scope", "peer_port", "peer_pool_size"} {
		if settings[key] == "" {
			t.Fatalf("missing peer setting default %s: %#v", key, settings)
		}
	}
	for _, path := range []string{"/api/discovery", "/api/network/subnets", "/api/relay/routes"} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %d", path, response.StatusCode)
		}
		response.Body.Close()
	}
}

func TestPortMappingLifecycle(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()
	defer app.Close()

	body := `{"name":"RustDesk","target_host":"192.168.50.21","target_port":21115,"listen_address":"127.0.0.1","listen_port":0,"auto_start":true}`
	response, err := http.Post(server.URL+"/api/portmappings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("create returned %d", response.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("mapping id must be assigned")
	}
	started, err := http.Post(server.URL+"/api/portmappings/"+created.ID+"/start", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var running map[string]any
	if err := json.NewDecoder(started.Body).Decode(&running); err != nil {
		t.Fatal(err)
	}
	started.Body.Close()
	if started.StatusCode != http.StatusOK {
		t.Fatalf("start returned %d (%v)", started.StatusCode, running)
	}
	if running["status"] != "running" {
		t.Fatalf("mapping must be running: %#v", running)
	}
	listResponse, err := http.Get(server.URL + "/api/portmappings")
	if err != nil {
		t.Fatal(err)
	}
	var mappings []model.PortMapping
	if err := json.NewDecoder(listResponse.Body).Decode(&mappings); err != nil {
		t.Fatal(err)
	}
	listResponse.Body.Close()
	if len(mappings) != 1 || mappings[0].Status != "running" || mappings[0].RuntimePort == 0 {
		t.Fatalf("unexpected mappings: %#v", mappings)
	}
	stopped, err := http.Post(server.URL+"/api/portmappings/"+created.ID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped.Body.Close()
	if stopped.StatusCode != http.StatusOK {
		t.Fatalf("stop returned %d", stopped.StatusCode)
	}
	deleted, err := http.NewRequest(http.MethodDelete, server.URL+"/api/portmappings/"+created.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(deleted)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete returned %d", response.StatusCode)
	}
	if _, err := store.GetPortMapping(created.ID); err == nil {
		t.Fatal("mapping must be removed from the store")
	}
}

func TestPortMappingRejectsPublicTarget(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()
	defer app.Close()
	body := `{"name":"bad","target_host":"8.8.8.8","target_port":53,"listen_port":0}`
	response, err := http.Post(server.URL+"/api/portmappings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("public target must be rejected, got %d", response.StatusCode)
	}
}

// Web 退出服务：返回 stopping 并触发注入的优雅关闭钩子，同时留下审计。
func TestShutdownEndpointTriggersGracefulHook(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	server := New(app, fakeMCP{})
	called := make(chan struct{}, 1)
	server.ShutdownFunc = func() { called <- struct{}{} }
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	defer app.Close()

	response, err := http.Post(ts.URL+"/api/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("退出接口返回 %d", response.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["stopping"] != true {
		t.Fatalf("响应应先告知正在停止: %#v", payload)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("ShutdownFunc 没有被调用")
	}
	events, err := app.AuditEvents(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Operation == "service.shutdown" {
			found = true
		}
	}
	if !found {
		t.Fatalf("退出操作必须写审计: %#v", events)
	}
}

// 未注入钩子时应明确拒绝，而不是假装成功。
func TestShutdownEndpointWithoutHook(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	defer app.Close()
	ts := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer ts.Close()
	response, err := http.Post(ts.URL+"/api/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatal("没有关闭钩子时不应返回成功")
	}
}

// 节点在线状态：本机恒在线；discovered 节点靠快速端口探测判断，不再永远停在 "discovered"。
func TestListNodesReportsOnline(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	defer app.Close()

	// 在线节点：指向一个真实监听的端口
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	onlinePort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	// 离线节点：先占一个端口再放开，确保它此刻没人监听
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()

	if err := store.SaveDiscoveredNode(model.Node{
		ID: "disc-on", Name: "在线节点", Address: host, Port: onlinePort,
		AuthMethod: "discovered", Protocol: "discovered", Status: "discovered",
		LastSeen: time.Now().Add(-10 * time.Minute), // 让 mDNS 新鲜度兜底失效，只考验端口探测
	}); err != nil {
		t.Fatal(err)
	}
	// 用 TEST-NET-1 里不可路由的地址，避免探测误中本机自己监听的 56701
	if err := store.SaveDiscoveredNode(model.Node{
		ID: "disc-off", Name: "离线节点", Address: "192.0.2.1", Port: closedPort,
		AuthMethod: "discovered", Protocol: "discovered", Status: "discovered",
		LastSeen: time.Now().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	nodes, err := app.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]model.Node{}
	for _, node := range nodes {
		found[node.ID] = node
	}
	localOnline := false
	for _, node := range nodes {
		if node.AuthMethod == "local" {
			localOnline = node.Online
		}
	}
	if !localOnline {
		t.Fatal("本机节点必须显示在线")
	}
	_ = model.Node{}
	if !found["disc-on"].Online {
		t.Fatalf("可达的 discovered 节点应显示在线: %#v", found["disc-on"])
	}
	if found["disc-off"].Online {
		t.Fatalf("不可达的 discovered 节点应显示离线: %#v", found["disc-off"])
	}
}

// 树状拓扑：本机 + 其注册的下级（带 parent），跨上级查询失败的分支进 missing。
func TestTopologyReportsTree(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	defer app.Close()
	if err := store.SaveNode(model.Node{
		ID: "child-1", Name: "子网节点", Address: "192.168.2.192", Port: 1,
		Username: "peer", AuthMethod: "peer", Protocol: "peer",
		Registered: true, Trusted: true, RelayEnabled: true, Version: "0.4.0-phase3",
	}); err != nil {
		t.Fatal(err)
	}
	// 不属于本机的节点不应出现在本机的下级里
	if err := store.SaveNode(model.Node{
		ID: "ssh-1", Name: "纯 SSH 节点", Address: "192.168.1.9", Port: 22,
		Username: "dev", AuthMethod: "key", Protocol: "sftp",
	}); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/topology")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload struct {
		SelfID  string           `json:"self_id"`
		Nodes   []map[string]any `json:"nodes"`
		Missing []map[string]any `json:"missing"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.SelfID == "" {
		t.Fatal("必须返回本机 id")
	}
	byID := map[string]map[string]any{}
	for _, node := range payload.Nodes {
		byID[fmt.Sprint(node["id"])] = node
	}
	if _, ok := byID[payload.SelfID]; !ok {
		t.Fatalf("拓扑必须包含本机: %#v", payload.Nodes)
	}
	child, ok := byID["child-1"]
	if !ok {
		t.Fatalf("应包含注册到本机的下级: %#v", payload.Nodes)
	}
	if child["parent"] != payload.SelfID {
		t.Fatalf("下级的 parent 应指向本机: %#v", child)
	}
	if child["online"] != true && child["registered"] != true {
		t.Fatalf("下级应带 registered: %#v", child)
	}
	if _, ok := byID["ssh-1"]; ok {
		t.Fatal("纯 SSH 节点不该出现在拓扑下级里")
	}
}
