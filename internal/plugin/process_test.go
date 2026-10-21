package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("NPA_PLUGIN_HELPER") == "1" {
		runPluginHelper()
		return
	}
	os.Exit(m.Run())
}

func runPluginHelper() {
	for {
		data, err := ReadMessage(os.Stdin)
		if err != nil {
			return
		}
		var request struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(data, &request) != nil {
			return
		}
		if request.Method == "plugin.crash" {
			os.Exit(9)
		}
		var result any
		switch request.Method {
		case "plugin.initialize":
			result = Info{ID: "test", Name: "test", Version: "1", Protocol: "1"}
		case "plugin.health":
			result = map[string]bool{"ok": true}
		case "plugin.shutdown":
			result = map[string]bool{"stopped": true}
		default:
			result = map[string]bool{"ok": true}
		}
		_ = WriteMessage(os.Stdout, struct {
			JSONRPC string `json:"jsonrpc"`
			ID      int64  `json:"id"`
			Result  any    `json:"result"`
		}{JSONRPC: "2.0", ID: request.ID, Result: result})
		if request.Method == "plugin.shutdown" {
			return
		}
	}
}

func TestProcessCrashDoesNotPanicCore(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "npa-desktop")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NPA_PLUGIN_HELPER", "1")
	manager := NewManager(directory)
	// 这个用例要复制整个测试二进制再拉起来，并行跑全量时磁盘压力大，放宽握手超时
	manager.InitTimeout = 60 * time.Second
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var health map[string]bool
	if err := manager.Invoke(context.Background(), "plugin.health", struct{}{}, &health); err != nil {
		t.Fatal(err)
	}
	if !health["ok"] {
		t.Fatal("unexpected health response")
	}
	err = manager.Invoke(context.Background(), "plugin.crash", struct{}{}, nil)
	if !errors.Is(err, ErrCrashed) {
		t.Fatalf("expected crash isolation error, got %v", err)
	}
}
