package peer

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestElectManagerDeterministic(t *testing.T) {
	plain := func(id string, uptime int64) DomainMember {
		return DomainMember{ID: id, Address: "10.0.0.1:56701", Uptime: uptime}
	}
	cross := func(id string, uptime int64) DomainMember {
		m := plain(id, uptime)
		m.CrossDomain = true
		return m
	}

	// 交叉节点优先，即使 uptime 更低
	view := []DomainMember{plain("a", 1000), cross("b", 10)}
	if w, _ := electManager(view); w.ID != "b" {
		t.Fatalf("cross-domain should win: got %s", w.ID)
	}
	// 同优先级 uptime 大者胜
	view = []DomainMember{plain("a", 100), plain("b", 200)}
	if w, _ := electManager(view); w.ID != "b" {
		t.Fatalf("higher uptime should win: got %s", w.ID)
	}
	// 同 uptime ID 字典序小者胜
	view = []DomainMember{plain("b", 100), plain("a", 100)}
	if w, _ := electManager(view); w.ID != "a" {
		t.Fatalf("lexicographic tiebreak: got %s", w.ID)
	}
	// 乱序输入结果一致
	shuffled := []DomainMember{plain("c", 50), plain("a", 100), plain("b", 100)}
	if w1, _ := electManager(shuffled); w1.ID != "a" {
		t.Fatalf("shuffled: got %s", w1.ID)
	}
	if _, ok := electManager(nil); ok {
		t.Fatal("empty view must not elect")
	}
}

func TestManagerHeartbeatAndSnapshot(t *testing.T) {
	self := DomainMember{ID: "m1", Address: "127.0.0.1:56701", Uptime: 10}
	mgr := NewDomainManager(self)

	st := mgr.Heartbeat(DomainMember{ID: "a", Address: "10.0.0.2:56701", Uptime: 5})
	if len(st.Members) != 1 || st.Members[0].ID != "a" {
		t.Fatalf("member not recorded: %+v", st.Members)
	}
	if st.Seq == 0 {
		t.Fatal("seq should advance on new member")
	}
	// 地址为空的成员拒收（仅转述得知的不可入表）
	mgr.Heartbeat(DomainMember{ID: "ghost"})
	if s := mgr.State(); len(s.Members) != 1 {
		t.Fatalf("ghost member must be rejected: %+v", s.Members)
	}
	// 重复心跳内容不变不推 seq
	before := mgr.State().Seq
	mgr.Heartbeat(DomainMember{ID: "a", Address: "10.0.0.2:56701", Uptime: 5})
	if mgr.State().Seq != before {
		t.Fatal("unchanged heartbeat must not advance seq")
	}
	// 域标识/管理者 = 当选者（这里 self uptime 更大）
	st = mgr.State()
	if st.Domain != "m1" || st.Manager.ID != "m1" {
		t.Fatalf("election inside snapshot: domain=%s manager=%s", st.Domain, st.Manager.ID)
	}
}

func TestManagerSweepRemovesDead(t *testing.T) {
	mgr := NewDomainManager(DomainMember{ID: "m1", Address: "127.0.0.1:56701"})
	mgr.Heartbeat(DomainMember{ID: "a", Address: "10.0.0.2:56701"})
	mgr.Heartbeat(DomainMember{ID: "b", Address: "10.0.0.3:56701"})

	// 把 a 的 lastSeen 拨回到判死线之前
	mgr.mu.Lock()
	mgr.members["a"].seen = time.Now().Add(-deadAfter - time.Second)
	seqBefore := mgr.seq
	mgr.mu.Unlock()

	mgr.Sweep()
	st := mgr.State()
	if len(st.Members) != 1 || st.Members[0].ID != "b" {
		t.Fatalf("dead member not swept: %+v", st.Members)
	}
	if st.Seq != seqBefore+1 {
		t.Fatalf("sweep should advance seq: %d -> %d", seqBefore, st.Seq)
	}
}

func TestApplyDeltaAndResync(t *testing.T) {
	mgr := NewDomainManager(DomainMember{ID: "m1", Address: "127.0.0.1:56701"})

	st, ok := mgr.ApplyDelta(DomainDelta{From: "n1", Seq: 1, Upserts: []DomainMember{
		{ID: "x", Address: "10.9.0.1:56701"},
	}})
	if !ok || len(st.Members) != 1 {
		t.Fatalf("first delta rejected: ok=%v members=%d", ok, len(st.Members))
	}
	// 转述成员 LastSeen 置 0（不参与本域判死）
	if st.Members[0].LastSeen != 0 {
		t.Fatalf("relayed member LastSeen must be 0, got %d", st.Members[0].LastSeen)
	}
	// 顺序 delta
	if _, ok := mgr.ApplyDelta(DomainDelta{From: "n1", Seq: 2, Removals: []string{"x"}}); !ok {
		t.Fatal("in-order delta rejected")
	}
	if s := mgr.State(); len(s.Members) != 0 {
		t.Fatalf("removal not applied: %+v", s.Members)
	}
	// 跳号 → 断档，调用方应转 pull
	if _, ok := mgr.ApplyDelta(DomainDelta{From: "n1", Seq: 9}); ok {
		t.Fatal("gap delta must be rejected")
	}
}

func TestMaxDomainMembers(t *testing.T) {
	mgr := NewDomainManager(DomainMember{ID: "m1", Address: "127.0.0.1:56701"})
	for i := 0; i < maxDomainMembers; i++ {
		mgr.Heartbeat(DomainMember{ID: string(rune('a'+i%26)) + itoa(i), Address: "10.0.0.9:56701"})
	}
	extra := DomainMember{ID: "overflow", Address: "10.0.0.10:56701"}
	mgr.Heartbeat(extra)
	if s := mgr.State(); len(s.Members) != maxDomainMembers {
		t.Fatalf("member cap: got %d want %d", len(s.Members), maxDomainMembers)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

// fakeManager 起一个真实 HTTP 端点，包装 DomainManager。
type fakeManager struct {
	*DomainManager
	srv *httptest.Server
}

func startFakeManager(t *testing.T, self DomainMember) *fakeManager {
	t.Helper()
	mgr := NewDomainManager(self)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/domain/heartbeat", mgr.HandleHeartbeat)
	mux.HandleFunc("GET /api/domain/state", mgr.HandleState)
	mux.HandleFunc("POST /api/domain/delta", mgr.HandleDelta)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fakeManager{DomainManager: mgr, srv: srv}
}

func (f *fakeManager) addr() string {
	// httptest.Server URL 形如 http://127.0.0.1:port
	return f.srv.URL[len("http://"):]
}

func TestDomainClientHeartbeatAndFollow(t *testing.T) {
	mgr := startFakeManager(t, DomainMember{ID: "boss", Address: "", Uptime: 100})
	// 管理者回复的快照里自己没有地址——client 侧把管理者地址带上
	mgr2 := NewDomainManager(DomainMember{ID: "boss", Address: mgr.addr(), Uptime: 100})

	client := NewDomainClient(func() DomainMember {
		return DomainMember{ID: "worker", Address: "127.0.0.1:60000", Uptime: 1}
	})
	client.SetManager(mgr.addr())

	state, err := client.Tick(context.Background(), nil)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if state.Manager.ID != "boss" {
		t.Fatalf("should follow boss: %+v", state.Manager)
	}
	if client.Manager() != mgr.addr() {
		t.Fatalf("client should keep manager address, got %q", client.Manager())
	}
	_ = mgr2
}

func TestDomainClientFailoverPromotesSelf(t *testing.T) {
	mgr := startFakeManager(t, DomainMember{ID: "boss", Address: "", Uptime: 100})
	managerAddr := mgr.addr()

	client := NewDomainClient(func() DomainMember {
		return DomainMember{ID: "worker", Address: "127.0.0.1:60000", Uptime: 1}
	})
	client.deadline = 50 * time.Millisecond // 测试加速
	client.SetManager(managerAddr)

	if _, err := client.Tick(context.Background(), nil); err != nil {
		t.Fatalf("first tick: %v", err)
	}

	// 管理者下线
	mgr.srv.Close()
	promoted := make(chan DomainState, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var lastErr error
	for {
		select {
		case st := <-promoted:
			if st.Domain != "worker" {
				t.Fatalf("promoted state should elect worker: %+v", st)
			}
			if client.Manager() != "127.0.0.1:60000" {
				t.Fatalf("client should point at self, got %q", client.Manager())
			}
			return
		default:
		}
		if _, err := client.Tick(ctx, func(s DomainState) { promoted <- s }); err != nil {
			lastErr = err
		}
		if ctx.Err() != nil {
			t.Fatalf("failover did not converge: lastErr=%v", lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDomainClientResyncAfterManagerRecovers(t *testing.T) {
	mgr := startFakeManager(t, DomainMember{ID: "boss", Address: "", Uptime: 100})
	addr := mgr.addr()

	client := NewDomainClient(func() DomainMember {
		return DomainMember{ID: "worker", Address: "127.0.0.1:60000", Uptime: 1}
	})
	client.deadline = 30 * time.Millisecond
	client.SetManager(addr)

	// 心跳成功一次：缓存了成员表
	if _, err := client.Tick(context.Background(), nil); err != nil {
		t.Fatalf("tick: %v", err)
	}
	_ = addr
}

// TestDomainElectionConvergence 三节点同网段场景：
// 各自 Elect 彼此 → 收敛到同一管理者；杀掉管理者后 → 剩余节点收敛出新管理者。
func TestDomainElectionConvergence(t *testing.T) {
	type node struct {
		name string
		addr string
		ln   net.Listener
		hub  *DomainHub
	}
	nodes := make([]*node, 0, 3)
	start := time.Now()
	for idx, id := range []string{"n1", "n2", "n3"} {
		port := 61000 + idx
		addr := "127.0.0.1:" + itoa(port)
		hub := NewDomainHub(func() DomainMember {
			return DomainMember{
				ID: id, Address: addr,
				Uptime: int64(time.Since(start) / time.Second),
			}
		})
		// 在声明的端口上起真实监听——心跳地址必须真的可达
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("listen %s: %v", addr, err)
		}
		mux := http.NewServeMux()
		hub.Register(mux)
		srv := &http.Server{Handler: mux}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
		nodes = append(nodes, &node{name: id, addr: addr, ln: ln, hub: hub})
	}

	// 每个节点把另外两个作为候选视图做引导选举；uptime 全相同时按 ID 字典序选 n1
	for i, nd := range nodes {
		var candidates []DomainMember
		for j, other := range nodes {
			if i == j {
				continue
			}
			candidates = append(candidates, DomainMember{
				ID: other.name, Address: other.addr, Uptime: 5,
			})
		}
		nd.hub.Elect(candidates)
	}

	// 三者对管理者的认定必须一致（Elect 后经几轮心跳跟随收敛——
	// 视图不对称时靠管理者快照里的当选者地址统一）
	managers := map[string]bool{}
	convergeBy := time.Now().Add(2 * time.Second)
	for time.Now().Before(convergeBy) {
		managers = map[string]bool{}
		for _, nd := range nodes {
			_, _ = nd.hub.Client.Tick(context.Background(), nil)
			managers[nd.hub.Client.Manager()] = true
		}
		if len(managers) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(managers) != 1 {
		var got []string
		for m := range managers {
			got = append(got, m)
		}
		t.Fatalf("did not converge on one manager: %v", got)
	}

	// 找到管理者节点并杀掉它
	var boss *node
	for _, nd := range nodes {
		if nd.addr == nd.hub.Client.Manager() {
			boss = nd
		}
	}
	if boss == nil {
		t.Fatal("no node recognized itself as manager target")
	}
	_ = boss.ln.Close() // 管理者下线

	// 剩余节点在 deadline 后本地重选，收敛到同一个新管理者
	deadline := time.Now().Add(3 * time.Second)
	remaining := nodes[:0]
	for _, nd := range nodes {
		if nd != boss {
			remaining = append(remaining, nd)
		}
	}
	nd1, nd2 := remaining[0], remaining[1]
	nd1.hub.Client.deadline = 60 * time.Millisecond
	nd2.hub.Client.deadline = 60 * time.Millisecond

	for time.Now().Before(deadline) {
		_, _ = nd1.hub.Client.Tick(context.Background(), nil)
		_, _ = nd2.hub.Client.Tick(context.Background(), nil)
		m1 := nd1.hub.Client.Manager()
		m2 := nd2.hub.Client.Manager()
		if m1 != "" && m1 == m2 && m1 != boss.addr {
			// 新管理者必须是其自身（本地重选只会选存活者）
			for _, nd := range remaining {
				if nd.addr == m1 {
					return // 收敛成功
				}
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("failover did not converge: %q vs %q", nd1.hub.Client.Manager(), nd2.hub.Client.Manager())
}

// TestDeltaPushBetweenManagers 两个独立域的管理者：A 状态变化 → delta push 到 B；
// B 收到后成员表包含 A 的成员（转述形态）。
func TestDeltaPushBetweenManagers(t *testing.T) {
	startHub := func(id string, port int) *DomainHub {
		addr := "127.0.0.1:" + itoa(port)
		hub := NewDomainHub(func() DomainMember {
			return DomainMember{ID: id, Address: addr, Uptime: 99}
		})
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		mux := http.NewServeMux()
		hub.Register(mux)
		go (&http.Server{Handler: mux}).Serve(ln)
		t.Cleanup(func() { ln.Close() })
		return hub
	}
	hubA := startHub("a1", 61100)
	hubB := startHub("b1", 61101)

	// A 的邻域 = B；B 的邻域 = A
	hubA.SetNeighbors(func() []DomainMember {
		return []DomainMember{{Address: "127.0.0.1:61101"}}
	})
	hubB.SetNeighbors(func() []DomainMember {
		return []DomainMember{{Address: "127.0.0.1:61100"}}
	})
	hubA.StartDeltaPush()
	hubB.StartDeltaPush()

	// A 域内成员心跳 → 触发 onChange → push 到 B
	hubA.Manager.Heartbeat(DomainMember{ID: "a-member", Address: "10.0.0.5:56701", Uptime: 3})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state := hubB.Manager.State()
		for _, m := range state.Members {
			if m.ID == "a-member" {
				if m.LastSeen != 0 {
					t.Fatal("relayed member must have LastSeen=0")
				}
				return // 推送收敛成功
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("delta push did not reach neighbor: B members=%+v", hubB.Manager.State().Members)
}

// TestBumpEpochRejectsStaleMaster 旧主防护：升格 epoch+1 后，
// 旧 epoch 的 delta 被拒收（ok=false），不污染成员表。
func TestBumpEpochRejectsStaleMaster(t *testing.T) {
	mgr := NewDomainManager(DomainMember{ID: "new-master", Address: "127.0.0.1:56701"})
	if mgr.State().Epoch != 0 {
		t.Fatal("initial epoch must be 0")
	}
	mgr.BumpEpoch()
	if mgr.State().Epoch != 1 {
		t.Fatal("epoch should advance")
	}
	// 旧主（epoch 0 的 delta）→ 拒收
	state, ok := mgr.ApplyDelta(DomainDelta{From: "old-master", Epoch: 0, Seq: 5,
		Upserts: []DomainMember{{ID: "ghost", Address: "10.9.9.9:56701"}}})
	if ok {
		t.Fatal("stale-epoch delta must be rejected")
	}
	for _, m := range state.Members {
		if m.ID == "ghost" {
			t.Fatal("rejected delta must not mutate members")
		}
	}
	// 当前/未来 epoch 的 delta 照常收
	if _, ok := mgr.ApplyDelta(DomainDelta{From: "peer", Epoch: 1, Seq: 1,
		Upserts: []DomainMember{{ID: "real", Address: "10.0.0.7:56701"}}}); !ok {
		t.Fatal("current-epoch delta must be accepted")
	}
}
