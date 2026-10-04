package relay

import (
	"context"
	"testing"

	"github.com/npa/npa/internal/model"
)

type fakeRepository struct {
	nodes  []model.Node
	routes []model.RelayRoute
}

func (r *fakeRepository) ListNodes() ([]model.Node, error)             { return r.nodes, nil }
func (r *fakeRepository) ListDiscoveredNodes() ([]model.Node, error)   { return nil, nil }
func (r *fakeRepository) ListRelayRoutes() ([]model.RelayRoute, error) { return r.routes, nil }
func (r *fakeRepository) SaveRelayRoute(route model.RelayRoute) error {
	r.routes = append(r.routes, route)
	return nil
}

func TestSelectRequiresTrustedEnabledRelay(t *testing.T) {
	repo := &fakeRepository{nodes: []model.Node{{
		ID: "relay-b", Status: "connected", Trusted: true, RelayEnabled: true,
		ReachableSubnets: []string{"10.99.88.0/24"},
	}}}
	manager := NewManager(repo, nil)
	route, err := manager.Select(context.Background(), "10.99.88.7:8080")
	if err != nil {
		t.Fatal(err)
	}
	if route.ViaNodeID != "relay-b" || len(route.Hops) != 1 || !route.Healthy {
		t.Fatalf("unexpected route: %#v", route)
	}

	repo.nodes[0].Trusted = false
	if _, err := manager.Select(context.Background(), "10.99.88.7:8080"); err == nil {
		t.Fatal("untrusted relay must not be selected")
	}
	repo.nodes[0].Trusted = true
	repo.nodes[0].RelayEnabled = false
	if _, err := manager.Select(context.Background(), "10.99.88.7:8080"); err == nil {
		t.Fatal("relay must be explicitly enabled")
	}
}
