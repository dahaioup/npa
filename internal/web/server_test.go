package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	if settings["mcp_ssh"] != "false" || settings["mcp_file"] != "false" || settings["mcp_tunnel"] != "false" {
		t.Fatalf("MCP capabilities must default off: %#v", settings)
	}
	if settings["discovery_enabled"] != "false" || settings["mcp_discovery"] != "false" || settings["mcp_relay"] != "false" {
		t.Fatalf("mesh capabilities must default off: %#v", settings)
	}
	if settings["mcp_bind_scope"] != "loopback" {
		t.Fatalf("MCP must default to loopback scope: %#v", settings)
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
