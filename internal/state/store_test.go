package state

import (
	"os"
	"strings"
	"testing"

	"github.com/npa/npa/internal/model"
)

func TestStorePersistsEncryptedCredentials(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	node := model.Node{ID: "node-1", Name: "dev", Address: "127.0.0.1", Port: 22, Username: "root", AuthMethod: "password"}
	if err := store.SaveNode(node); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCredential(model.Credential{NodeID: node.ID, Password: "super-secret"}); err != nil {
		t.Fatal(err)
	}
	tunnel := model.Tunnel{ID: "tunnel-1", NodeID: node.ID, Name: "local", Type: "local", ListenAddress: "127.0.0.1", ListenPort: 18080, TargetHost: "127.0.0.1", TargetPort: 8080, AutoStart: true}
	if err := store.SaveTunnel(tunnel); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dir + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret") {
		t.Fatal("credential was persisted in plaintext")
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.GetCredential(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "super-secret" {
		t.Fatalf("credential mismatch: %q", got.Password)
	}
	savedNode, err := reopened.GetNode(node.ID)
	if err != nil || savedNode.Name != node.Name {
		t.Fatalf("node was not restored: %#v, %v", savedNode, err)
	}
	savedTunnel, err := reopened.GetTunnel(tunnel.ID)
	if err != nil || !savedTunnel.AutoStart || savedTunnel.TargetPort != 8080 {
		t.Fatalf("tunnel was not restored: %#v, %v", savedTunnel, err)
	}
}
