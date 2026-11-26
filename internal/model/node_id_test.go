package model

import "testing"

func TestNodeIDFromMACFormat(t *testing.T) {
	id := NodeIDFromMAC()
	if len(id) != 8 {
		t.Fatalf("expected 8 characters, got %q (%d)", id, len(id))
	}
	for _, char := range id {
		if !(char >= '0' && char <= '9') && !(char >= 'A' && char <= 'Z') {
			t.Fatalf("invalid node id character %q in %q", char, id)
		}
	}
}
