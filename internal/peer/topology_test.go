package peer

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestView(t *testing.T, selfID string) *TopologyView {
	t.Helper()
	return NewTopologyView(selfID)
}

// 规则 2：version 本地单调生成，不依赖时钟。
func TestTopologyVersionMonotonic(t *testing.T) {
	view := newTestView(t, "SELF")
	view.Upsert(TopoNode{ID: "A", Address: "1.1.1.1:56701"}, true)
	v1 := view.Version()
	view.Upsert(TopoNode{ID: "B", Address: "2.2.2.2:56701"}, true)
	v2 := view.Version()
	view.MarkSeen("A") // 在线翻转才会 bump；已在线 → 不变
	v3 := view.Version()
	if !(v1 < v2 && v2 == v3) {
		t.Fatalf("version 期望 v1<v2 且心跳不 bump：v1=%d v2=%d v3=%d", v1, v2, v3)
	}
	// offline 翻转要 bump
	view.Upsert(TopoNode{ID: "A", Address: "1.1.1.1:56701"}, false)
	if view.Version() <= v3 {
		t.Fatalf("offline 翻转应 bump version")
	}
}

// 规则 3/8：增量计算——since 之后的变化；since=0 全量。
func TestTopologyDelta(t *testing.T) {
	view := newTestView(t, "SELF")
	view.Upsert(TopoNode{ID: "A", Address: "a:1"}, true)
	base := view.Version()
	view.Upsert(TopoNode{ID: "B", Address: "b:1"}, true)

	delta := view.Delta(base)
	if delta.Full || len(delta.Nodes) != 1 || delta.Nodes[0].ID != "B" {
		t.Fatalf("增量应只含 B：full=%v nodes=%v", delta.Full, delta.Nodes)
	}
	full := view.Delta(0)
	if !full.Full || len(full.Nodes) != 2 {
		t.Fatalf("since=0 应全量含 A/B：full=%v nodes=%d", full.Full, len(full.Nodes))
	}
	// 断档（since > 当前版本，管理者重启归零场景）→ 全量
	gap := view.Delta(view.Version() + 100)
	if !gap.Full {
		t.Fatalf("since 断档应回退全量")
	}
}

// 规则 9：push_id 幂等——重复推送返回 dup 且不重复 bump。
func TestTopologyPushIdempotent(t *testing.T) {
	view := newTestView(t, "SELF")
	push := TopologyPush{
		PushID: "pid-1", FromID: "DOMAIN-A",
		Nodes: []TopoNode{{ID: "X", Name: "x", Address: "x:1", Online: true}},
	}
	first := view.ApplyPush(push, "pusher-a:56701")
	second := view.ApplyPush(push, "pusher-a:56701")
	if !first.OK || !second.OK || !second.Dup {
		t.Fatalf("重复推送应 dup 命中：first=%+v second=%+v", first, second)
	}
	if view.Version() != first.Version {
		t.Fatalf("重复推送不应 bump version：first=%d now=%d", first.Version, view.Version())
	}
	if len(view.Snapshot()) != 1 {
		t.Fatalf("重复推送不应重复入表：%v", view.Snapshot())
	}
}

// 规则 9：接收方版本落后（重启归零）→ 响应 OK=false + 实际版本，推送方重推全量。
func TestTopologyPushResync(t *testing.T) {
	member := newTestView(t, "MEMBER")
	member.Upsert(TopoNode{ID: "C1", Address: "c1:1"}, true)

	manager := newTestView(t, "MANAGER")

	var calls int
	post := func(url string, payload []byte) ([]byte, int, error) {
		calls++
		var push TopologyPush
		_ = json.Unmarshal(payload, &push)
		var resp TopologyPushResponse
		if calls == 1 {
			// 第一次模拟接收方刚重启：增量基线无效，返回失败+实际版本 0
			resp = TopologyPushResponse{OK: false, Version: 0}
		} else {
			resp = manager.ApplyPush(push, "member:56701") // 第二次按 since=0 重算出全量
		}
		body, _ := json.Marshal(resp)
		return body, http.StatusOK, nil
	}

	if !member.PushBatchTo("manager:56701", "MEMBER", post) {
		t.Fatalf("重推后应成功")
	}
	if calls != 2 {
		t.Fatalf("应重推一次：calls=%d", calls)
	}
	if len(manager.Snapshot()) != 1 || manager.Snapshot()[0].ID != "C1" {
		t.Fatalf("接收方应收到全量 C1：%v", manager.Snapshot())
	}
}

// 规则 9/10：推送失败计数与恢复清零。
func TestTopologyPushFailureCount(t *testing.T) {
	view := newTestView(t, "SELF")
	view.Upsert(TopoNode{ID: "A", Address: "a:1"}, true)

	failPost := func(url string, payload []byte) ([]byte, int, error) {
		return nil, 0, errTestNetwork
	}
	view.PushBatchTo("bad:56701", "SELF", failPost)
	view.PushBatchTo("bad:56701", "SELF", failPost)
	if got := view.PushFailures("bad:56701"); got != 2 {
		t.Fatalf("失败计数应为 2：%d", got)
	}
	view.ResetPushState("bad:56701")
	if got := view.PushFailures("bad:56701"); got != 0 {
		t.Fatalf("重置后应为 0：%d", got)
	}
}

// 规则 11：墓碑——offline 超过 24h 物理剔除，removed 进增量。
func TestTopologyTombstone(t *testing.T) {
	view := newTestView(t, "SELF")
	view.Upsert(TopoNode{ID: "DEAD", Address: "d:1"}, true)
	base := view.Version()
	// 直接把 offlineAt 拨回 25 小时前（同包测试可操作内部状态）
	view.mu.Lock()
	if rec, ok := view.nodes["DEAD"]; ok {
		rec.offlineAt = time.Now().Add(-25 * time.Hour)
	}
	view.mu.Unlock()

	removed := view.Sweep()
	if len(removed) != 1 || removed[0] != "DEAD" {
		t.Fatalf("25h offline 应被剔除：%v", removed)
	}
	delta := view.Delta(base)
	if len(delta.Removed) != 1 || delta.Removed[0] != "DEAD" {
		t.Fatalf("removed 应进增量：%v", delta.Removed)
	}
	if len(view.Snapshot()) != 0 {
		t.Fatalf("剔除后快照应为空：%v", view.Snapshot())
	}
}

// 规则 3：90s 未见判 offline。
func TestTopologySweepOffline(t *testing.T) {
	view := newTestView(t, "SELF")
	view.Upsert(TopoNode{ID: "GONE", Address: "g:1"}, true)
	view.mu.Lock()
	view.nodes["GONE"].seen = time.Now().Add(-2 * TopoOfflineAfter)
	view.mu.Unlock()

	view.Sweep()
	nodes := view.Snapshot()
	if len(nodes) != 1 || nodes[0].Online {
		t.Fatalf("超时节点应 offline：%v", nodes)
	}
}

// HTTP 入口：sync 增量与 push 幂等走真实 mux。
func TestTopologyHTTPHandlers(t *testing.T) {
	view := newTestView(t, "SELF")
	view.Upsert(TopoNode{ID: "A", Address: "a:1"}, true)
	mux := http.NewServeMux()
	view.RegisterRoutes(mux)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/topology/sync?since=0")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	defer resp.Body.Close()
	var sync TopologySyncResponse
	if err := json.NewDecoder(resp.Body).Decode(&sync); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !sync.Full || len(sync.Nodes) != 1 {
		t.Fatalf("sync 应全量：%+v", sync)
	}

	pushBody, _ := json.Marshal(TopologyPush{PushID: "p1", FromID: "D", Nodes: []TopoNode{{ID: "B", Address: "b:1", Online: true}}})
	pr, _ := http.Post(srv.URL+"/api/topology/push", "application/json", bytes.NewReader(pushBody))
	var pushResp TopologyPushResponse
	_ = json.NewDecoder(pr.Body).Decode(&pushResp)
	pr.Body.Close()
	if !pushResp.OK {
		t.Fatalf("push 应成功：%+v", pushResp)
	}
}

type testNetworkError struct{}

func (testNetworkError) Error() string { return "network down" }

var errTestNetwork error = testNetworkError{}
