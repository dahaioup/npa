package peer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wsPair(t *testing.T) *WSConn {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ready")); err != nil {
			return
		}
		remote := NewWSConn(conn)
		buf := make([]byte, 4096)
		for {
			n, err := remote.Read(buf)
			if n > 0 {
				if _, writeErr := remote.Write(buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	client := websocket.Dialer{}
	conn, _, err := client.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// 吃掉服务端的 ready 文本帧，确保后续 payload 从干净状态开始
	_, _, _ = conn.ReadMessage()
	return NewWSConn(conn)
}

func TestWSConnIsNetConnAndEchoes(t *testing.T) {
	local := wsPair(t)
	if err := local.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	n, err := local.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	got := make([]byte, 0, len(payload))
	buf := make([]byte, 8192)
	for len(got) < len(payload) {
		read, err := local.Read(buf)
		if read > 0 {
			got = append(got, buf[:read]...)
		}
		if err != nil {
			t.Fatalf("read after %d bytes: %v", len(got), err)
		}
	}
	if string(got) != string(payload) {
		t.Fatalf("payload mismatch: got %d bytes", len(got))
	}
}

func TestWSConnReadDeadline(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		<-r.Context().Done()
	}))
	defer server.Close()

	client := websocket.Dialer{}
	conn, _, err := client.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream := NewWSConn(conn)
	if err := stream.SetReadDeadline(time.Now().Add(80 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if _, err := stream.Read(buf); err == nil {
		t.Fatal("expected read deadline error")
	} else if err != io.ErrClosedPipe && !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("unexpected error: %v", err)
	}
}
