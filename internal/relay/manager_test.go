package relay

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/npa/npa/internal/model"
)

type fakeRepository struct {
	nodes  []model.Node
	routes []model.RelayRoute
}

func (r *fakeRepository) GetNode(id string) (model.Node, error) {
	for _, node := range r.nodes {
		if node.ID == id {
			return node, nil
		}
	}
	return model.Node{}, errors.New("node not found")
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

type fakePeerDialer struct {
	online map[string]bool
	dialed []string
}

func (d *fakePeerDialer) Online(nodeID string) bool { return d.online[nodeID] }

func (d *fakePeerDialer) Dial(_ context.Context, nodeID, target, via string) (net.Conn, error) {
	d.dialed = append(d.dialed, nodeID+"|"+target+"|"+via)
	left, right := net.Pipe()
	_ = right.Close()
	return left, nil
}

// 反向接入后的选路：peer 声明 → 本机 SSH → peer+SSH 串联 → 私网兜底 → 离线不可选。
func TestSelectRoutesThroughPeerAndChain(t *testing.T) {
	peerNode := model.Node{
		ID: "peer-a", Protocol: "peer", Trusted: true, RelayEnabled: true, Status: "connected",
		ReachableSubnets: []string{"192.168.2.0/24"},
		PeerRoutes: []model.RemoteRoute{
			{NodeID: "ssh-b", Subnets: []string{"10.5.0.0/24"}, Trusted: true, RelayEnabled: true},
		},
	}
	repo := &fakeRepository{nodes: []model.Node{peerNode}}
	dialer := &fakePeerDialer{online: map[string]bool{"peer-a": true}}
	manager := NewManager(repo, nil)
	manager.SetPeerDialer(dialer)

	// 1) peer 宣告子网 → 单跳反向链路
	route, err := manager.Select(context.Background(), "192.168.2.192:21115")
	if err != nil {
		t.Fatal(err)
	}
	if len(route.Hops) != 1 || route.Hops[0] != "peer-a" {
		t.Fatalf("expected single peer hop: %#v", route)
	}

	// 2) peer 对端 SSH 跳板宣告子网 → NPA 映射 + SSH 映射串联
	route, err = manager.Select(context.Background(), "10.5.0.7:22")
	if err != nil {
		t.Fatal(err)
	}
	if len(route.Hops) != 2 || route.Hops[0] != "peer-a" || route.Hops[1] != "ssh-b" {
		t.Fatalf("expected chained hops: %#v", route)
	}
	conn, err := manager.dialHops(context.Background(), route, "10.5.0.7:22")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if len(dialer.dialed) != 1 || dialer.dialed[0] != "peer-a|10.5.0.7:22|ssh:ssh-b" {
		t.Fatalf("chain must pass via=ssh:<node>: %#v", dialer.dialed)
	}

	// 3) 私网兜底：没有声明匹配的子网，也交给在线 peer 试拨
	route, err = manager.Select(context.Background(), "172.20.9.9:445")
	if err != nil {
		t.Fatal(err)
	}
	if len(route.Hops) != 1 || route.Hops[0] != "peer-a" {
		t.Fatalf("expected blind peer fallback: %#v", route)
	}

	// 4) peer 离线时不参与选路
	dialer.online["peer-a"] = false
	if _, err := manager.Select(context.Background(), "192.168.2.192:21115"); err == nil {
		t.Fatal("offline peer must not be selected")
	}

	// 5) 未注入反向数据面时不可用
	manager.SetPeerDialer(nil)
	dialer.online["peer-a"] = true
	if _, err := manager.Select(context.Background(), "192.168.2.192:21115"); err == nil {
		t.Fatal("peer selection requires a wired peer dialer")
	}
}

// 回环与链路本地永远不会是中继目标，展示与 MCP 都不应把它们当成"节点可达网段"。
func TestSubnetsFiltersNonRelayPrefixes(t *testing.T) {
	repo := &fakeRepository{nodes: []model.Node{{
		ID:               "peer-a",
		Address:          "192.168.2.192",
		Protocol:         "peer",
		ReachableSubnets: []string{"192.168.2.0/24", "127.0.0.0/8", "fe80::/64", "::1/128"},
	}}}
	manager := NewManager(repo, nil)
	list, err := manager.Subnets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	foundUseful := false
	for _, item := range list {
		if item.Prefix == "127.0.0.0/8" || strings.HasPrefix(item.Prefix, "fe80:") || item.Prefix == "::1/128" {
			t.Fatalf("non-relay prefix must be filtered: %#v", item)
		}
		if item.NodeID == "peer-a" && item.Prefix == "192.168.2.0/24" {
			foundUseful = true
		}
	}
	if !foundUseful {
		t.Fatalf("useful subnet must survive filtering: %#v", list)
	}
}
