package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
)

func TestLocalPTYThroughWebSocket(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(New(service.New(store), fakeMCP{}).Handler())
	defer server.Close()
	header := http.Header{}
	header.Set("Origin", server.URL)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/nodes/local/pty", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte("echo npa-web-pty-ok\n")); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for !strings.Contains(output.String(), "npa-web-pty-ok") {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read PTY output: %v, output=%q", err, output.String())
		}
		output.Write(data)
	}
}
