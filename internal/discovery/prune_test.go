package discovery

import (
	"testing"
	"time"

	"github.com/npa/npa/internal/model"
)

type fakeRepo struct {
	nodes map[string]model.Node
}

func newFakeRepo(nodes ...model.Node) *fakeRepo {
	repo := &fakeRepo{nodes: map[string]model.Node{}}
	for _, node := range nodes {
		repo.nodes[node.ID] = node
	}
	return repo
}

func (r *fakeRepo) ListDiscoveredNodes() ([]model.Node, error) {
	result := make([]model.Node, 0, len(r.nodes))
	for _, node := range r.nodes {
		result = append(result, node)
	}
	return result, nil
}

func (r *fakeRepo) SaveDiscoveredNode(node model.Node) error { r.nodes[node.ID] = node; return nil }

func (r *fakeRepo) DeleteDiscoveredNode(id string) error { delete(r.nodes, id); return nil }

// 离线超过 6 小时的发现节点应被清理，新鲜的、以及从未见过的（用 UpdatedAt 兜底）保留。
func TestPruneStaleRemovesExpiredDiscoveredNodes(t *testing.T) {
	now := time.Now()
	repo := newFakeRepo(
		model.Node{ID: "stale", Name: "7 小时前", Discovered: true, LastSeen: now.Add(-7 * time.Hour)},
		model.Node{ID: "fresh", Name: "10 分钟前", Discovered: true, LastSeen: now.Add(-10 * time.Minute)},
		model.Node{ID: "no-seen-but-updated", Name: "无 LastSeen", Discovered: true, UpdatedAt: now.Add(-time.Hour)},
	)
	manager := NewManager(repo, "0.4.0")
	removed, err := manager.PruneStale(now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("应只清理 1 个过期节点，实际 %d", removed)
	}
	if _, exists := repo.nodes["stale"]; exists {
		t.Fatal("离线超过 6 小时的节点应被移除")
	}
	if _, exists := repo.nodes["fresh"]; !exists {
		t.Fatal("新鲜的节点不应被移除")
	}
	if _, exists := repo.nodes["no-seen-but-updated"]; !exists {
		t.Fatal("缺 LastSeen 时应按 UpdatedAt 判断，不应误删")
	}
}

// 阈值边界：正好 6 小时不算过期。
func TestPruneStaleThresholdBoundary(t *testing.T) {
	now := time.Now()
	repo := newFakeRepo(
		model.Node{ID: "edge", Discovered: true, LastSeen: now.Add(-StaleAfter)},
		model.Node{ID: "just-over", Discovered: true, LastSeen: now.Add(-StaleAfter - time.Second)},
	)
	manager := NewManager(repo, "0.4.0")
	removed, err := manager.PruneStale(now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("边界判断不对，实际清理 %d", removed)
	}
	if _, exists := repo.nodes["edge"]; !exists {
		t.Fatal("恰好 6 小时的节点不应被移除")
	}
}
