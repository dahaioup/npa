package plugin

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestFramingRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	message := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugin.health"}
	if err := WriteMessage(&buffer, message); err != nil {
		t.Fatal(err)
	}
	data, err := ReadMessage(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["method"] != "plugin.health" {
		t.Fatalf("unexpected message: %#v", decoded)
	}
}

func TestMissingRuntime(t *testing.T) {
	manager := NewManager(t.TempDir())
	if err := manager.Start(t.Context()); err != ErrRuntimeMissing {
		t.Fatalf("expected runtime missing, got %v", err)
	}
	status, _ := manager.Status()
	if status != "runtime_missing" {
		t.Fatalf("unexpected status: %s", status)
	}
}
