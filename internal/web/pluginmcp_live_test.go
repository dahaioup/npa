package web

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

// buildObscuraPlugin 编出插件进程；编不出来就跳过。
func buildObscuraPlugin(t *testing.T) string {
	t.Helper()
	binary := t.TempDir() + "/npa-obscura"
	command := exec.Command("go", "build", "-o", binary, "github.com/npa/npa/cmd/npa-obscura")
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		t.Skipf("cannot build the obscura plugin: %v", err)
	}
	return binary
}

// freePort 找一个空闲端口。
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// TestPluginMCPProxyAgainstRealPlugin NPA 的代理要真能把插件自己的 MCP
// 转发出去：起一个真的插件进程（带 HTTP 服务），再从 NPA 的代理路径访问它。
func TestPluginMCPProxyAgainstRealPlugin(t *testing.T) {
	binary := buildObscuraPlugin(t)
	port := freePort(t)

	command := exec.Command(binary,
		"--http-port", strconv.Itoa(port),
		"--http-bind", "127.0.0.1",
		"--data-dir", t.TempDir(),
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()

	// 等插件的 HTTP 服务起来。
	ready := false
	for i := 0; i < 100; i++ {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("the plugin HTTP service did not come up on port %d", port)
	}

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	if err := store.SaveSettings(map[string]string{"browser_plugin_port": strconv.Itoa(port)}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(New(app, fakeMCP{}).Handler())
	defer server.Close()

	// MCP 握手。
	request, err := http.NewRequest(http.MethodPost, server.URL+"/plugins/browser/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	session := response.Header.Get("Mcp-Session-Id")
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initialize returned %d: %s", response.StatusCode, body)
	}
	if session == "" {
		t.Fatalf("the proxy must forward the mcp session id: %s", body)
	}

	// tools/list。
	request, err = http.NewRequest(http.MethodPost, server.URL+"/plugins/browser/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Session-Id", session)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("tools/list returned %d", response.StatusCode)
	}
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	if !strings.Contains(text, "browser_navigate") {
		t.Fatalf("the proxied tool list should contain browser_navigate: %s", text)
	}
	// SSE 一行一个事件，取出 data 行数一下工具个数。
	tools := 0
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var message struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &message); err != nil {
			continue
		}
		tools = len(message.Result.Tools)
	}
	if tools < 30 {
		t.Errorf("proxied tool count = %d, want the obscura catalog", tools)
	}
}
