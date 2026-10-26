package peer_test

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	seeds := peer.ParseSeeds("192.168.0.165, 10.0.0.5:56701 10.0.0.5", 56701)
	if len(seeds) != 2 || seeds[0] != "192.168.0.165:56701" || seeds[1] != "10.0.0.5:56701" {
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
	candidates, err := peer.ProbeSubnets(context.Background(), nil, 56701)
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
		_, _ = w.Write([]byte(`{"product":"npa","name":"upper","version":"test","peer_port":56701}`))
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

// 更新全流程（走反向链路）：下级节点自报更高版本并"放着"一个更新包，
// 上级应能发现候选、下载、解出本平台二进制并完成校验。
// 不执行替换那一步 —— 那会覆盖正在跑的测试二进制。
func TestUpdatePipelineOverReverseLink(t *testing.T) {
	storeUpper, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storeUpper.Close()
	upper := service.New(storeUpper)
	defer upper.Close()
	upperServer := httptest.NewServer(web.New(upper, fakeMCP{}).PeerHandler())
	defer upperServer.Close()

	// 下级侧准备"新版"产物：全平台 zip，内含本平台二进制（魔数 + 足够体积）
	artifactDir := t.TempDir()
	archivePath := filepath.Join(artifactDir, "npa-all_20990101_0000.zip")
	if err := writeFakePackage(archivePath); err != nil {
		t.Fatal(err)
	}

	seed := strings.TrimPrefix(upperServer.URL, "http://")
	lower := peer.NewAgent(peer.AgentOptions{
		Version:  "9.9.9", // 明显高于本机
		NodeID:   func() string { return "LOWER-SUBNET-NODE" },
		Name:     func() string { return "新版节点" },
		Address:  func() string { return "192.168.2.192" },
		Subnets:  func() []string { return []string{"192.168.2.0/24"} },
		PoolSize: func() int { return 2 },
		Seeds:    func() string { return seed },
		Dial: func(ctx context.Context, target, via string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", startEcho(t))
		},
	})
	lower.Start(context.Background())
	defer lower.Stop()
	waitFor(t, 10*time.Second, func() bool { return len(upper.Peer.Links()) > 0 },
		"下级没有在超时内向上级注册")

	if err := upper.SaveSettings(map[string]string{"update_artifact": artifactDir}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	check, err := upper.UpdateCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list, ok := check["updates"].([]interface{}); ok {
		if len(list) != 1 {
			t.Fatalf("应发现 1 个更新候选: %#v", check)
		}
		entry := list[0].(map[string]interface{})
		if entry["node_version"] != "9.9.9" {
			t.Fatalf("候选版本不对: %#v", entry)
		}
		if !strings.Contains(entry["artifact"].(string), "npa-all_20990101_0000.zip") {
			t.Fatalf("候选产物不对: %#v", entry)
		}
	}
	prepared, err := upper.UpdatePrepare(ctx, "LOWER-SUBNET-NODE", "")
	if err != nil {
		t.Fatal(err)
	}
	if prepared["node_version"] != "9.9.9" {
		t.Fatalf("下载结果版本不对: %#v", prepared)
	}
	if digest, _ := prepared["sha256"].(string); digest == "" {
		t.Fatalf("缺少 sha256: %#v", prepared)
	}
	binaryPath, _ := prepared["binary_path"].(string)
	if binaryPath == "" {
		t.Fatalf("缺少解出的二进制路径: %#v", prepared)
	}
	if _, err := os.Stat(binaryPath); err != nil {
		t.Fatalf("解出的二进制不存在: %v", err)
	}
}

// writeFakePackage 生成一个和发布包同结构的 zip，内含本平台二进制。
func writeFakePackage(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(file)
	name := "npa/bin/npa-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	entry, err := writer.Create(name)
	if err != nil {
		return err
	}
	payload := make([]byte, 2<<20)
	copy(payload, fakeMagic())
	if _, err := entry.Write(payload); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return file.Close()
}

func fakeMagic() []byte {
	switch runtime.GOOS {
	case "windows":
		return []byte{'M', 'Z', 0x90, 0x00}
	case "linux":
		return []byte{0x7f, 'E', 'L', 'F'}
	default:
		return []byte{0xcf, 0xfa, 0xed, 0xfe}
	}
}

// 未配置上级时不该产生任何错误输出：本机只作为上级运行时，
// 反复打"没有可用的上级节点"纯属噪音（用户明确要求去掉）。
func TestAgentStaysSilentWithoutConfiguredUpper(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	defer app.Close()
	if err := app.SaveSettings(map[string]string{
		"peer_enabled": "true", "peer_seeds": "", "peer_probe_subnets": "",
	}); err != nil {
		t.Fatal(err)
	}
	defer app.PeerAgent.Stop()
	// 覆盖至少两次重试周期
	time.Sleep(1500 * time.Millisecond)
	if got := app.PeerAgent.Status().LastError; got != "" {
		t.Fatalf("未配置上级时不应记录错误: %q", got)
	}
	if !app.PeerAgent.Status().Running {
		t.Fatal("agent 应保持运行（配置随时可能补上）")
	}
}

// 推送升级（走反向链路）：本机按节点平台从包里取出二进制并上传到节点。
// 不执行"替换并重启"那一步 —— 那会覆盖正在跑的测试二进制。
func TestPushPackageToNodeOverReverseLink(t *testing.T) {
	storeUpper, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storeUpper.Close()
	upper := service.New(storeUpper)
	defer upper.Close()
	upperServer := httptest.NewServer(web.New(upper, fakeMCP{}).PeerHandler())
	defer upperServer.Close()

	// 本机侧准备一个"发布包"，内含各平台二进制
	packageDir := t.TempDir()
	archivePath := filepath.Join(packageDir, "npa-all_20990101_0000.zip")
	if err := writeFakePackage(archivePath); err != nil {
		t.Fatal(err)
	}
	if err := upper.SaveSettings(map[string]string{"push_package": archivePath}); err != nil {
		t.Fatal(err)
	}

	seed := strings.TrimPrefix(upperServer.URL, "http://")
	lower := peer.NewAgent(peer.AgentOptions{
		Version:  "0.3.0",
		NodeID:   func() string { return "LOWER-SUBNET-NODE" },
		Name:     func() string { return "子网节点" },
		Address:  func() string { return "192.168.2.192" },
		Subnets:  func() []string { return []string{"192.168.2.0/24"} },
		PoolSize: func() int { return 2 },
		Seeds:    func() string { return seed },
		Dial: func(ctx context.Context, target, via string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", startEcho(t))
		},
	})
	lower.Start(context.Background())
	defer lower.Stop()
	waitFor(t, 10*time.Second, func() bool { return len(upper.Peer.Links()) > 0 },
		"下级没有在超时内向上级注册")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := upper.PushPackage(ctx, "LOWER-SUBNET-NODE", "")
	if err != nil {
		t.Fatal(err)
	}
	goos, _ := report["goos"].(string)
	remoteBinary, _ := report["remote_binary"].(string)
	digest, _ := report["sha256"].(string)
	bytes, _ := report["bytes"].(int64)
	if goos == "" || remoteBinary == "" || digest == "" {
		t.Fatalf("推送报告不完整: %#v", report)
	}
	if bytes <= 1<<20 {
		t.Fatalf("上传字节数异常: %d", bytes)
	}
	// 节点与本机同机（测试），文件应该真实落盘
	if _, err := os.Stat(remoteBinary); err != nil {
		t.Fatalf("节点上没有收到文件: %v", err)
	}
	if executable, _ := report["node_executable"].(string); executable == "" {
		t.Logf("警告：未能确定节点上运行的 NPA 路径（推送仍成功）: %#v", report["node_executable_error"])
	}
}

// 跨上级协调端到端：
//
//	A（本机 service） ──需要访问──> N，但 N 注册在 B 上
//	1) A 经拓扑发现 N 在 B（peer_uppers）
//	2) A 请求 B：让 N 主动连到 A（按需直连）
//	3) A 通过临时链路访问 N
//	4) 直连不通时，A 经 B 代理访问 N（A→B→N）
func TestCrossUpperOnDemandAndRelay(t *testing.T) {
	// B：持有者。用纯 hub 实例，避免与 A 撞同一个 MAC 派生的节点 ID。
	storeB, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	// 持有者在拓扑里自报的地址必须指向本测试实例，否则协调请求会打到真实主机上；
	// 地址自带端口时 HolderForTarget 直接采用（生产里端口收敛后通常省略，由 meshPort 兜底）
	holderAddr := "127.0.0.1:0"
	hubB := peer.NewHub(storeB, peer.Options{
		Name: "holder-b",
		Port: func() int { return 56701 },
		Self: func() peer.TopologyNode {
			return peer.TopologyNode{ID: "HOLDER-B", Name: "holder-b", Address: holderAddr, Port: 56700, Protocol: "local", Online: true}
		},
	})
	hubB.StartSweeper()
	muxB := http.NewServeMux()
	muxB.HandleFunc("GET /api/topology", hubB.HandleTopology)
	muxB.HandleFunc("GET /api/peers/link", hubB.HandleLink)
	muxB.HandleFunc("GET /api/peers/conn", hubB.HandleConn)
	muxB.HandleFunc("POST /api/peers/require", hubB.HandleRequire)
	muxB.HandleFunc("POST /api/peers/require_relay", hubB.HandleRequireRelay)
	muxB.HandleFunc("GET /api/peers/relay", hubB.HandleRelay)
	serverB := httptest.NewServer(muxB)
	defer serverB.Close()
	// 持有者在拓扑里自报的地址必须指向本测试实例，否则协调请求会打到真实主机上
	_, portText, _ := net.SplitHostPort(strings.TrimPrefix(serverB.URL, "http://"))
	holderPort, _ := strconv.Atoi(portText)
	holderAddr = net.JoinHostPort("127.0.0.1", portText)

	// N：下级，注册到 B，自报 192.168.2.192 / 192.168.2.0/24
	echo := startEcho(t)
	holder := strings.TrimPrefix(serverB.URL, "http://")
	node := peer.NewAgent(peer.AgentOptions{
		Version:  "0.4.0",
		NodeID:   func() string { return "SUBNET-NODE" },
		Name:     func() string { return "子网节点" },
		Address:  func() string { return "192.168.2.192" },
		Subnets:  func() []string { return []string{"192.168.2.0/24"} },
		PoolSize: func() int { return 2 },
		Seeds:    func() string { return holder },
		Dial: func(ctx context.Context, target, via string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", echo)
		},
	})
	node.Start(context.Background())
	defer node.Stop()
	waitFor(t, 10*time.Second, func() bool { return len(hubB.Links()) > 0 }, "下级没有注册到持有者")

	// A：本机视角
	storeA, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	appA := service.New(storeA)
	defer appA.Close()
	serverA := httptest.NewServer(web.New(appA, fakeMCP{}).Handler())
	defer serverA.Close()
	// A 的通告地址指向 A 自己的测试监听，否则协调请求与回调都会打到真实主机上
	if err := appA.SaveSettings(map[string]string{
		"peer_uppers": holder, "peer_relay_fallback": "true",
		"peer_advertise": strings.TrimPrefix(serverA.URL, "http://"),
	}); err != nil {
		t.Fatal(err)
	}

	// 1) 拓扑里能看到持有者与它的下级
	payload, err := appA.Topology(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	nodes, _ := payload["nodes"].([]peer.TopologyNode)
	var found *peer.TopologyNode
	for i := range nodes {
		if nodes[i].ID == "SUBNET-NODE" {
			found = &nodes[i]
		}
	}
	if found == nil {
		t.Fatalf("拓扑里应能看到持有者的下级: %#v", nodes)
	}
	if found.Parent != "HOLDER-B" {
		t.Fatalf("下级的 parent 应是持有者: %#v", found)
	}
	if len(found.Subnets) != 1 || found.Subnets[0] != "192.168.2.0/24" {
		t.Fatalf("拓扑节点应带可达子网（选路要用）: %#v", found.Subnets)
	}

	// 2) 找到持有者
	holderAddress, nodeID, ok := appA.HolderForTarget(context.Background(), "192.168.2.192:21115")
	// 持有者地址 = 它在拓扑里自报的地址 + 反向接入端口
	if !ok || nodeID != "SUBNET-NODE" || holderAddress != net.JoinHostPort("127.0.0.1", strconv.Itoa(holderPort)) {
		t.Fatalf("应找到持有者: ok=%v holder=%q node=%q", ok, holderAddress, nodeID)
	}

	// 3) 按需直连：持有者通知 N 连到 A
	if err := appA.EnsureDirect(context.Background(), holderAddress, nodeID); err != nil {
		t.Fatalf("按需直连失败: %v", err)
	}
	if !appA.Peer.EphemeralReady(nodeID) {
		t.Fatal("临时链路应已建立")
	}

	// 4) 自动选路应命中这条临时链路
	conn, route, err := appA.Relay.DialRoute(context.Background(), "192.168.2.192:21115")
	if err != nil {
		t.Fatalf("经临时链路拨号失败: %v", err)
	}
	if len(route.Hops) != 1 || route.Hops[0] != "SUBNET-NODE" {
		t.Fatalf("选路应命中临时链路: %#v", route)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("on-demand\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "echo:on-demand" {
		t.Fatalf("临时链路数据不对: %q", line)
	}

	// 5) 直连不通时经持有上级代理（A→B→N）
	relayed, err := appA.DialViaUpper(context.Background(), holderAddress, nodeID, "192.168.2.192:21115", "")
	if err != nil {
		t.Fatalf("经上级代理失败: %v", err)
	}
	defer relayed.Close()
	if _, err := relayed.Write([]byte("via-upper\n")); err != nil {
		t.Fatal(err)
	}
	_ = relayed.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err = bufio.NewReader(relayed).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "echo:via-upper" {
		t.Fatalf("代理链路数据不对: %q", line)
	}
}

// MCP 自升级通道端到端：
//
// 目标节点是局域网里的独立 NPA（没有向本机注册、没有反向链路），
// 本机对它执行不了命令 → 自动回落到「它自己的 MCP」：
// 1) 本机探测到它 56701 的 /api/info 是 NPA
// 2) 本机把二进制挂到一次性 token 下载端点
// 3) 经它 MCP 的 ssh.exec 在它本机执行「下载 → 校验」（不执行替换重启）
// 4) 下载端点 token 用后即焚
func TestPushViaNodeOwnMCP(t *testing.T) {
	storeUpper, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storeUpper.Close()
	upper := service.New(storeUpper)
	defer upper.Close()
	// 镜像生产的 mesh mux：/download/ 一次性端点 + 全量 Web（token 端点在
	// 生产挂在 56701 上，与 Web 面分开，这里合在一个测试监听里等价验证）
	meshMux := http.NewServeMux()
	meshMux.HandleFunc("/download/", upper.ServeDownload)
	meshMux.Handle("/", web.New(upper, fakeMCP{}).Handler())
	upperServer := httptest.NewServer(meshMux)
	defer upperServer.Close()

	// 本机通告地址指向测试监听，避免打到真实主机
	if err := upper.SaveSettings(map[string]string{
		"peer_advertise": strings.TrimPrefix(upperServer.URL, "http://"),
	}); err != nil {
		t.Fatal(err)
	}

	// 目标节点：一个假的独立 NPA——暴露 /api/info 与最小 MCP，
	// 它的 ssh.exec 只做一件事：真正执行下发的下载命令（curl/Invoke-WebRequest
	// 打回本机的一次性下载端点），并把命令写进日志供断言。
	var mu sync.Mutex
	var executedCommands []string
	targetRoot := t.TempDir()
	targetMux := http.NewServeMux()
	targetMux.HandleFunc("GET /api/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"product":"npa","name":"standalone","version":"0.4.0-phase3","peer_port":56701,"protocols":["register","link","conn"]}`))
	})
	targetMux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
				Args struct {
					Command string `json:"command"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeRPC := func(result map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "TESTSESSION")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": payload.ID, "result": result})
		}
		switch payload.Method {
		case "initialize":
			writeRPC(map[string]any{"capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "NPA", "version": "0.4.0-phase3"}})
		case "tools/call":
			if payload.Params.Name == "nodes.list" {
				writeRPC(map[string]any{"content": []any{map[string]any{"type": "text", "text": `[{"id":"LOCAL","auth_method":"local"}]`}}})
				return
			}
			if payload.Params.Name == "ssh.exec" {
				mu.Lock()
				executedCommands = append(executedCommands, payload.Params.Args.Command)
				mu.Unlock()
				// 平台探测命令返回本机平台，让 ExtractPlatform 解出匹配魔数的假二进制
				if payload.Params.Args.Command == "uname -s -m" {
					writeRPC(map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"stdout":"` + platformUname() + `","stderr":"","exit_code":0}`}}})
					return
				}
				if strings.HasPrefix(payload.Params.Args.Command, "echo %OS%") {
					writeRPC(map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"stdout":"","stderr":"","exit_code":1}`}}})
					return
				}
				// 在目标"本机"（同一个测试进程的临时目录）真的跑一遍下载段，
				// 验证下载 URL 与 token 有效。Windows 段是 powershell 单行，
				// 这里只验证 URL 可达（HEAD 语义），不真跑 PowerShell。
				downloadURL := extractURL(payload.Params.Args.Command)
				if downloadURL != "" {
					resp, err := http.Get(downloadURL)
					if err != nil {
						writeRPC(map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"stdout":"","stderr":"` + err.Error() + `"}`}}})
						return
					}
					defer resp.Body.Close()
					downloaded, _ := io.ReadAll(resp.Body)
					if len(downloaded) == 0 {
						writeRPC(map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"stdout":"","stderr":"empty download"}`}}})
						return
					}
					// 下载内容应是伪装修的假二进制（含魔数），且 token 已被焚毁
					writeRPC(map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf(`{"stdout":"downloaded %d bytes","stderr":"","exit_code":0}`, len(downloaded))}}})
					return
				}
				writeRPC(map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"stdout":"","stderr":"","exit_code":0}`}}})
				return
			}
			writeRPC(map[string]any{"content": []any{map[string]any{"type": "text", "text": `{}`}}})
		default:
			http.Error(w, "unexpected method", 400)
		}
	})
	targetServer := httptest.NewServer(targetMux)
	defer targetServer.Close()
	_, targetPort, _ := net.SplitHostPort(strings.TrimPrefix(targetServer.URL, "http://"))

	// 本机节点表里登记它：discovered、无凭据、无链路（这是 MCP 回落的前提）
	if err := upper.Store.SaveDiscoveredNode(model.Node{
		ID: "STANDALONE", Name: "standalone", Address: "127.0.0.1", Port: atoi(targetPort),
		Username:   "nobody",
		AuthMethod: "discovered", Protocol: "discovered", Status: "discovered",
		Discovered: true, PeerPort: atoi(targetPort),
		LastSeen: time.Now().UTC(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// 本机侧准备"发布包"
	packageDir := t.TempDir()
	archivePath := filepath.Join(packageDir, "npa-all_20990101_0000.zip")
	if err := writeFakePackage(archivePath); err != nil {
		t.Fatal(err)
	}
	if err := upper.SaveSettings(map[string]string{"push_package": archivePath}); err != nil {
		t.Fatal(err)
	}

	// 平台探测先在它"本机"执行 uname/echo —— 测试桩对 uname 返回当前平台，
	// 使 ExtractPlatform 能解出匹配魔数的二进制（fakeMagic 与本机平台一致）。
	mu.Lock()
	executedCommands = nil
	mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := upper.PushPackage(ctx, "STANDALONE", "")
	if err != nil {
		t.Fatal(err)
	}
	if report["channel"] != "mcp" {
		t.Fatalf("应走 MCP 通道: %#v", report)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(executedCommands) == 0 {
		t.Fatal("MCP ssh.exec 应至少下发一次自升级命令")
	}
	downloadURL := ""
	for _, command := range executedCommands {
		if url := extractURL(command); url != "" {
			downloadURL = url
		}
	}
	if downloadURL == "" {
		t.Fatalf("自升级命令里应包含下载 URL: %#v", executedCommands)
	}
	// token 用后即焚：再次下载同一 URL 必须 404
	resp, err := http.Get(downloadURL)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("一次性 token 应已失效，实际 %d", resp.StatusCode)
		}
	}
	_ = targetRoot
}

// extractURL 从自升级命令里取出 http(s) URL（测试辅助）。
func extractURL(command string) string {
	for _, field := range strings.Fields(command) {
		if strings.HasPrefix(field, "http://") || strings.HasPrefix(field, "https://") {
			return strings.Trim(field, "'\"")
		}
	}
	return ""
}

func atoi(text string) int {
	value, _ := strconv.Atoi(text)
	return value
}

// platformUname 是平台探测桩：让 mcpDetectPlatform 认出当前测试平台。
func platformUname() string {
	switch runtime.GOOS {
	case "darwin":
		return "Darwin " + runtime.GOARCH
	case "linux":
		return "Linux " + runtime.GOARCH
	default:
		return runtime.GOOS + " " + runtime.GOARCH
	}
}

// 跨域两条主线路的端到端验证（A → 持有者 B → N，N 不与 A 直连）：
//  1. exec：A 对 N 执行命令，自动经上级代理拨 N 的 mesh 端口走 HTTP /api/exec
//  2. 文件：A 向 N 上传/下载（HTTP 文件面经同一代理通道），升级包推送以此为基础
//  3. 隧道：DialNode 直连失败后自动回退到跨上级代理（RustDesk 场景的数据面）
func TestCrossUpperPushAndTunnel(t *testing.T) {
	storeB, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	holderAddr := "127.0.0.1:0"
	hubB := peer.NewHub(storeB, peer.Options{
		Name: "holder-b",
		Port: func() int { return 56701 },
		Self: func() peer.TopologyNode {
			return peer.TopologyNode{ID: "HOLDER-B", Name: "holder-b", Address: holderAddr, Port: 56700, Protocol: "local", Online: true}
		},
	})
	hubB.StartSweeper()
	muxB := http.NewServeMux()
	muxB.HandleFunc("GET /api/topology", hubB.HandleTopology)
	muxB.HandleFunc("GET /api/peers/link", hubB.HandleLink)
	muxB.HandleFunc("GET /api/peers/conn", hubB.HandleConn)
	muxB.HandleFunc("POST /api/peers/require", hubB.HandleRequire)
	muxB.HandleFunc("POST /api/peers/require_relay", hubB.HandleRequireRelay)
	muxB.HandleFunc("GET /api/peers/relay", hubB.HandleRelay)
	serverB := httptest.NewServer(muxB)
	defer serverB.Close()
	holderAddr = strings.TrimPrefix(serverB.URL, "http://")
	holder := holderAddr

	// N：下级。它的 mesh 端口用一个假 HTTP 服务模拟（就是 FileAPI 本身），
	// Agent 的 Dial 忽略目标端口全部拨过去 —— 等价于"节点自己 127.0.0.1:56701"。
	nodeFS := t.TempDir()
	fileAPI := peer.NewFileAPI()
	nodeMux := http.NewServeMux()
	fileAPI.Register(nodeMux)
	nodeServer := httptest.NewServer(nodeMux)
	defer nodeServer.Close()

	echo := startEcho(t)
	node := peer.NewAgent(peer.AgentOptions{
		Version:  "0.4.0",
		NodeID:   func() string { return "SUBNET-NODE" },
		Name:     func() string { return "子网节点" },
		Address:  func() string { return "192.168.2.192" },
		Subnets:  func() []string { return []string{"192.168.2.0/24"} },
		PoolSize: func() int { return 2 },
		Seeds:    func() string { return holder },
		Dial: func(ctx context.Context, target, via string) (net.Conn, error) {
			var dialer net.Dialer
			// 56701 是节点自己的 mesh 端口（HTTP 文件面/exec），其它目标是普通 TCP 服务
			if strings.HasSuffix(target, ":56701") {
				return dialer.DialContext(ctx, "tcp", strings.TrimPrefix(nodeServer.URL, "http://"))
			}
			return dialer.DialContext(ctx, "tcp", echo)
		},
	})
	node.Start(context.Background())
	defer node.Stop()
	waitFor(t, 10*time.Second, func() bool { return len(hubB.Links()) > 0 }, "下级没有注册到持有者")

	storeA, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	appA := service.New(storeA)
	defer appA.Close()
	serverA := httptest.NewServer(web.New(appA, fakeMCP{}).Handler())
	defer serverA.Close()
	if err := appA.SaveSettings(map[string]string{
		"peer_uppers": holder, "peer_relay_fallback": "true",
		"peer_advertise": strings.TrimPrefix(serverA.URL, "http://"),
	}); err != nil {
		t.Fatal(err)
	}

	// 1) 跨域 exec：直连链路不存在 → 自动经上级代理 → N 的 /api/exec
	result, err := appA.Exec(context.Background(), "SUBNET-NODE", "echo cross-domain-ok", 10*time.Second)
	if err != nil {
		t.Fatalf("跨域 exec 失败: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "cross-domain-ok" {
		t.Fatalf("跨域 exec 输出不对: %#v", result)
	}

	// 2) 跨域文件上传/下载（升级包推送的底座）
	payload := []byte("fake-npa-binary-" + strings.Repeat("x", 4096))
	remotePath := strings.ReplaceAll(nodeFS, string(filepath.Separator), "/") + "/npa-new.bin"
	written, err := appA.Files.Upload(context.Background(), "SUBNET-NODE", remotePath, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("跨域上传失败: %v", err)
	}
	if written != int64(len(payload)) {
		t.Fatalf("上传字节数不对: %d", written)
	}
	onDisk, err := os.ReadFile(filepath.Join(nodeFS, "npa-new.bin"))
	if err != nil || !bytes.Equal(onDisk, payload) {
		t.Fatalf("节点侧文件内容不对: err=%v len=%d", err, len(onDisk))
	}
	reader, err := appA.Files.OpenRead(context.Background(), "SUBNET-NODE", remotePath)
	if err != nil {
		t.Fatalf("跨域下载失败: %v", err)
	}
	downloaded, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(downloaded, payload) {
		t.Fatalf("回读内容不对: err=%v len=%d", err, len(downloaded))
	}

	// 3) 跨域隧道数据面：DialNode 直连失败 → 上级代理 → N 拨到 echo 服务
	tunnelConn, err := appA.DialNode(context.Background(), "SUBNET-NODE", "192.168.2.192:21115")
	if err != nil {
		t.Fatalf("跨域 DialNode 失败: %v", err)
	}
	defer tunnelConn.Close()
	if _, err := tunnelConn.Write([]byte("rustdesk\n")); err != nil {
		t.Fatal(err)
	}
	_ = tunnelConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(tunnelConn).ReadString('\n')
	if err != nil {
		t.Fatalf("跨域隧道回包失败: %v", err)
	}
	if strings.TrimSpace(line) != "echo:rustdesk" {
		t.Fatalf("跨域隧道数据不对: %q", line)
	}
}
