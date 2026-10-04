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
	discovered := model.Node{ID: "peer-1", Name: "peer", Address: "192.168.1.20", Port: 56700, AuthMethod: "discovered", Protocol: "discovered", Status: "discovered", ReachableSubnets: []string{"192.168.2.0/24"}}
	if err := store.SaveDiscoveredNode(discovered); err != nil {
		t.Fatal(err)
	}
	route := model.RelayRoute{TargetSubnet: "192.168.2.0/24", ViaNodeID: "peer-1", Hops: []string{"peer-1"}, Healthy: true}
	if err := store.SaveRelayRoute(route); err != nil {
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
	discoveredNodes, err := reopened.ListDiscoveredNodes()
	if err != nil || len(discoveredNodes) != 1 || !discoveredNodes[0].Discovered {
		t.Fatalf("discovered node was not restored: %#v, %v", discoveredNodes, err)
	}
	routes, err := reopened.ListRelayRoutes()
	if err != nil || len(routes) != 1 || routes[0].ViaNodeID != "peer-1" {
		t.Fatalf("relay route was not restored: %#v, %v", routes, err)
	}
}
