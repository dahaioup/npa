package peer_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/peer"
	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
	"github.com/npa/npa/internal/web"
)

type fakeMCP struct{}

func (fakeMCP) Status() map[string]any { return map[string]any{"enabled": false} }

func startEcho(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					if _, err := c.Write([]byte("echo:" + scanner.Text() + "\n")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return listener.Addr().String()
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(message)
}

func TestParseSeedsAndProbeSubnets(t *testing.T) {
	seeds := peer.ParseSeeds("192.168.0.165, 10.0.0.5:56710 10.0.0.5", 56710)
	if len(seeds) != 2 || seeds[0] != "192.168.0.165:56710" || seeds[1] != "10.0.0.5:56710" {
		t.Fatalf("unexpected seeds: %#v", seeds)
	}
	prefixes, err := peer.ParseProbeSubnets("192.168.0.128/25,10.1.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes) != 2 || prefixes[0].String() != "192.168.0.128/25" {
		t.Fatalf("unexpected prefixes: %#v", prefixes)
	}
	if _, err := peer.ParseProbeSubnets("not-a-cidr/x"); err == nil {
		t.Fatal("invalid cidr must be rejected")
	}
	// 未配置网段时不做任何扫描
	candidates, err := peer.ProbeSubnets(context.Background(), nil, 56710)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("empty probe must be a no-op: %v %#v", err, candidates)
	}
}

func TestProbeFindsNPAByInfoEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"product":"npa","name":"upper","version":"test","peer_port":56710}`))
	})
	go func() { _ = http.Serve(listener, mux) }()
	_, portText, _ := net.SplitHostPort(listener.Addr().String())

	candidates, err := peer.ProbeSubnets(context.Background(), mustPrefix(t, "127.0.0.1/32"), mustPort(t, portText))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Source != "probe" {
		t.Fatalf("probe must find the NPA info endpoint: %#v", candidates)
	}
}

func mustPrefix(t *testing.T, value string) []netip.Prefix {
	t.Helper()
	prefixes, err := peer.ParseProbeSubnets(value)
	if err != nil {
		t.Fatal(err)
	}
	return prefixes
}

func mustPort(t *testing.T, value string) int {
	t.Helper()
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 {
		t.Fatalf("invalid port %q", value)
	}
	return port
}

// 反向接入端到端：下级用种子找到上级、注册、建立长连接与连接池，
// 上级经反向链路拨号到下级侧服务（数据确实由下级拨出）。
//
// 下级刻意自报 192.168.2.192 / 192.168.2.0/24，复刻"上级无法直达子网"的真实拓扑：
// 同机跑两个实例时 MAC 相同会撞 ID，所以这里用显式节点 ID 的裸 Agent。
func TestReverseRegistrationAndDataPath(t *testing.T) {
	storeUpper, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storeUpper.Close()
	upper := service.New(storeUpper)
	defer upper.Close()
	upperServer := httptest.NewServer(web.New(upper, fakeMCP{}).PeerHandler())
	defer upperServer.Close()

	// 服务只存在于下级侧，用来证明字节确实由下级拨出。
	target := startEcho(t)

	seed := strings.TrimPrefix(upperServer.URL, "http://")
	lower := peer.NewAgent(peer.AgentOptions{
		Version:      "test",
		NodeID:       func() string { return "LOWER-SUBNET-NODE" },
		Name:         func() string { return "子网节点" },
		Address:      func() string { return "192.168.2.192" },
		Subnets:      func() []string { return []string{"192.168.2.0/24"} },
		Caps:         func() []string { return []string{"tunnel", "ssh"} },
		PoolSize:     func() int { return 3 },
		Seeds:        func() string { return seed },
		ProbeSubnets: func() string { return "" },
		Dial: func(ctx context.Context, target, via string) (net.Conn, error) {
			if via != "" && via != "direct" {
				return nil, fmt.Errorf("unexpected via %q", via)
			}
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", target)
		},
	})
	lower.Start(context.Background())
	defer lower.Stop()

	waitFor(t, 10*time.Second, func() bool { return len(upper.Peer.Links()) > 0 },
		"下级没有在超时内向上级注册")

	links := upper.Peer.Links()
	if !links[0].Online || links[0].NodeID != "LOWER-SUBNET-NODE" {
		t.Fatalf("unexpected link state: %#v", links[0])
	}
	nodeID := links[0].NodeID

	node, err := upper.Store.GetNode(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if node.Protocol != "peer" || !node.Registered || node.Status != "connected" {
		t.Fatalf("unexpected peer node state: %#v", node)
	}
	if node.Address != "192.168.2.192" {
		t.Fatalf("registered address must come from the lower node: %#v", node.Address)
	}
	if len(node.ReachableSubnets) != 1 || node.ReachableSubnets[0] != "192.168.2.0/24" {
		t.Fatalf("registered subnets must come from the lower node: %#v", node.ReachableSubnets)
	}

	// 空闲连接池建立后，经反向链路拨号，回声必须来自下级侧。
	waitFor(t, 5*time.Second, func() bool { return upper.Peer.Links()[0].IdlePool > 0 },
		"空闲数据连接池没有建立")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := upper.Peer.Dial(ctx, nodeID, target, "direct")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "echo:ping" {
		t.Fatalf("unexpected relayed response: %q", line)
	}

	// 下级停机后，上级必须把链路与节点标记为离线。
	lower.Stop()
	waitFor(t, 10*time.Second, func() bool { return len(upper.Peer.Links()) == 0 },
		"下级停止后链路没有断开")
	offline, err := upper.Store.GetNode(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if offline.Status == "connected" {
		t.Fatalf("node must be marked offline after link loss: %#v", offline)
	}
}

// service 侧接线：设置变更应启停下级 agent，并把状态暴露给 Web。
func TestServiceWiresPeerAgent(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	defer app.Close()
	if app.Peer == nil || app.PeerAgent == nil {
		t.Fatal("peer hub/agent must be wired by service.New")
	}
	if err := app.SaveSettings(map[string]string{
		"peer_seeds": "127.0.0.1:65500", "peer_enabled": "true", "peer_pool_size": "2",
	}); err != nil {
		t.Fatal(err)
	}
	status := app.PeerStatus()
	if status["enabled"] != true || status["pool_size"] != 2 || status["address"] == "" {
		t.Fatalf("unexpected peer status: %#v", status)
	}
	if err := app.SaveSettings(map[string]string{"peer_probe_subnets": "192.168.2.0/24"}); err != nil {
		t.Fatal(err)
	}
	if app.PeerStatus()["probe_subnets"] != "192.168.2.0/24" {
		t.Fatalf("probe subnets must persist: %#v", app.PeerStatus())
	}
	app.PeerAgent.Stop()
}

func TestHubRejectsMissingPool(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	defer app.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if _, err := app.Peer.Dial(ctx, "missing-node", "127.0.0.1:1", "direct"); err == nil {
		t.Fatal("expected error when no reverse link exists")
	}
}

// 端口映射走反向链路（RustDesk 场景）：上级把子网设备 192.168.2.192:21115 映射到本机端口，
// 本机连映射端口即可访问该设备 —— 实际拨号由下级完成，上级根本到不了 192.168.2.0/24。
func TestPortMappingOverReverseLink(t *testing.T) {
	storeUpper, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storeUpper.Close()
	upper := service.New(storeUpper)
	defer upper.Close()
	upperServer := httptest.NewServer(web.New(upper, fakeMCP{}).PeerHandler())
	defer upperServer.Close()

	// 下级侧的"子网设备"：上级同网段没有这个端口的监听
	subnetDevice := startEcho(t)
	seed := strings.TrimPrefix(upperServer.URL, "http://")

	lower := peer.NewAgent(peer.AgentOptions{
		Version:      "test",
		NodeID:       func() string { return "LOWER-SUBNET-NODE" },
		Name:         func() string { return "子网节点" },
		Address:      func() string { return "192.168.2.192" },
		Subnets:      func() []string { return []string{"192.168.2.0/24"} },
		PoolSize:     func() int { return 3 },
		Seeds:        func() string { return seed },
		ProbeSubnets: func() string { return "" },
		Dial: func(ctx context.Context, target, via string) (net.Conn, error) {
			if via != "" && via != "direct" {
				return nil, fmt.Errorf("unexpected via %q", via)
			}
			// 真实场景中下级能直达自己子网的设备；这里把目标重定向到本机回声服务
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", subnetDevice)
		},
	})
	lower.Start(context.Background())
	defer lower.Stop()

	waitFor(t, 10*time.Second, func() bool { return len(upper.Peer.Links()) > 0 },
		"下级没有在超时内向上级注册")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mapping := model.PortMapping{
		ID: "map-peer", Name: "RustDesk", TargetHost: "192.168.2.192", TargetPort: 21115,
		ListenAddress: "127.0.0.1", ListenPort: 0, AutoStart: true,
	}
	if err := upper.SavePortMapping(ctx, mapping); err != nil {
		t.Fatal(err)
	}
	started, err := upper.StartPortMapping(ctx, mapping.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upper.DeletePortMapping(context.Background(), mapping.ID) }()
	if started.RuntimePort == 0 {
		t.Fatal("mapping must resolve a runtime port")
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(started.RuntimePort)), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("rustdesk\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "echo:rustdesk" {
		t.Fatalf("unexpected response through reverse mapping: %q", line)
	}

	// 经由信息必须显示反向链路节点
	waitFor(t, 3*time.Second, func() bool {
		list, err := upper.ListPortMappings()
		return err == nil && len(list) == 1 && list[0].ViaRoute == "LOWER-SUBNET-NODE"
	}, "端口映射没有记录反向链路经由")
}

// peer 节点的全部能力：命令执行、交互终端、文件读写 —— 都经反向链路，不依赖 SSH 服务端。
func TestPeerExecPTYAndFiles(t *testing.T) {
	storeUpper, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storeUpper.Close()
	upper := service.New(storeUpper)
	defer upper.Close()
	upperServer := httptest.NewServer(web.New(upper, fakeMCP{}).PeerHandler())
	defer upperServer.Close()

	echo := startEcho(t)
	seed := strings.TrimPrefix(upperServer.URL, "http://")
	lower := peer.NewAgent(peer.AgentOptions{
		Version:  "test",
		NodeID:   func() string { return "LOWER-SUBNET-NODE" },
		Name:     func() string { return "子网节点" },
		Address:  func() string { return "192.168.2.192" },
		Subnets:  func() []string { return []string{"192.168.2.0/24"} },
		PoolSize: func() int { return 4 },
		Seeds:    func() string { return seed },
		Dial: func(ctx context.Context, target, via string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", echo)
		},
	})
	lower.Start(context.Background())
	defer lower.Stop()
	waitFor(t, 10*time.Second, func() bool { return len(upper.Peer.Links()) > 0 },
		"下级没有在超时内向上级注册")
	nodeID := upper.Peer.Links()[0].NodeID

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1) 命令执行
	result, err := upper.Exec(ctx, nodeID, "echo peer-exec-ok", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "peer-exec-ok") {
		t.Fatalf("unexpected exec result: %#v", result)
	}

	// 2) 交互终端
	shell, err := upper.OpenShell(ctx, nodeID, 120, 34)
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	if _, err := shell.Stdin.Write([]byte("echo peer-pty-ok\n")); err != nil {
		t.Fatal(err)
	}
	_ = shell.Stdout.SetReadDeadline(time.Now().Add(10 * time.Second))
	var output strings.Builder
	buffer := make([]byte, 512)
	for !strings.Contains(output.String(), "peer-pty-ok") && output.Len() < 8192 {
		n, err := shell.Stdout.Read(buffer)
		if n > 0 {
			output.Write(buffer[:n])
		}
		if err != nil {
			t.Fatalf("pty read failed: %v (output=%q)", err, output.String())
		}
	}

	// 3) 文件能力（SFTP over 反向链路）
	remoteDir := t.TempDir()
	remoteFile := filepath.Join(remoteDir, "peer-note.txt")
	if err := upper.FileWrite(ctx, nodeID, remoteFile, []byte("reverse-fs-ok")); err != nil {
		t.Fatal(err)
	}
	data, err := upper.FileRead(ctx, nodeID, remoteFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "reverse-fs-ok" {
		t.Fatalf("unexpected file content: %q", data)
	}
	entries, err := upper.FileList(ctx, nodeID, remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "peer-note.txt" {
		t.Fatalf("unexpected directory listing: %#v", entries)
	}
	if err := upper.FileRemove(ctx, nodeID, remoteFile); err != nil {
		t.Fatal(err)
	}
}

// 回归：握手期的 5s 读超时与 10s 写超时不能被带进数据阶段。
// 真实场景（34 MB SFTP 上传、RustDesk 长会话、交互终端）都远超握手窗口，
// 一旦超时未清除，连接会在 5s/10s 后被切断 —— 表现为 "connection lost"。
func TestReverseChannelSurvivesBeyondHandshakeTimeout(t *testing.T) {
	storeUpper, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storeUpper.Close()
	upper := service.New(storeUpper)
	defer upper.Close()
	upperServer := httptest.NewServer(web.New(upper, fakeMCP{}).PeerHandler())
	defer upperServer.Close()

	echo := startEcho(t)
	seed := strings.TrimPrefix(upperServer.URL, "http://")
	lower := peer.NewAgent(peer.AgentOptions{
		Version:  "test",
		NodeID:   func() string { return "LOWER-SUBNET-NODE" },
		Name:     func() string { return "子网节点" },
		Address:  func() string { return "192.168.2.192" },
		Subnets:  func() []string { return []string{"192.168.2.0/24"} },
		PoolSize: func() int { return 2 },
		Seeds:    func() string { return seed },
		Dial: func(ctx context.Context, target, via string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", echo)
		},
	})
	lower.Start(context.Background())
	defer lower.Stop()
	waitFor(t, 10*time.Second, func() bool { return len(upper.Peer.Links()) > 0 },
		"下级没有在超时内向上级注册")
	nodeID := upper.Peer.Links()[0].NodeID

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := upper.Peer.Dial(ctx, nodeID, "192.168.2.192:21115", "direct")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 越过握手窗口（takeTimeout=5s）之后再传数据
	time.Sleep(6500 * time.Millisecond)

	if _, err := conn.Write([]byte("late\n")); err != nil {
		t.Fatalf("握手超时未清除，写失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("握手超时未清除，读失败: %v", err)
	}
	if strings.TrimSpace(line) != "echo:late" {
		t.Fatalf("unexpected response after handshake window: %q", line)
	}
}
