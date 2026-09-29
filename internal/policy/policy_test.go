package policy

import (
	"context"
	"errors"
	"testing"
)

type capabilityStore map[string]bool

func (s capabilityStore) MCPCapabilities() map[string]bool { return s }

func TestCheckSourceAndCapability(t *testing.T) {
	store := capabilityStore{"ssh": false, "file": true}
	if err := Check(WithSource(context.Background(), SourceMCP), store, CapSSH); err == nil {
		t.Fatal("expected disabled MCP capability to be denied")
	} else if code := errors.Unwrap(err); code != nil {
		t.Fatalf("unexpected wrapped error: %v", code)
	}
	if err := Check(WithSource(context.Background(), SourceMCP), store, CapFile); err != nil {
		t.Fatalf("expected enabled MCP capability: %v", err)
	}
	if err := Check(context.Background(), store, CapSSH); err != nil {
		t.Fatalf("web source should bypass MCP capability: %v", err)
	}
	if err := Check(WithSource(context.Background(), SourceMCP), store, CapBase); err != nil {
		t.Fatalf("base tools should remain available: %v", err)
	}
}
