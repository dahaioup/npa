package tunnel

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	sshx "github.com/npa/npa/internal/ssh"
)

type fakeRepo struct {
	mu       sync.Mutex
	tunnels  map[string]model.Tunnel
	statuses map[string]string
}

func newFakeRepo(tunnels ...model.Tunnel) *fakeRepo {
	repo := &fakeRepo{tunnels: map[string]model.Tunnel{}, statuses: map[string]string{}}
	for _, tunnel := range tunnels {
		repo.tunnels[tunnel.ID] = tunnel
	}
	return repo
}

func (r *fakeRepo) ListTunnels() ([]model.Tunnel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]model.Tunnel, 0, len(r.tunnels))
	for _, tunnel := range r.tunnels {
		result = append(result, tunnel)
	}
	return result, nil
}

func (r *fakeRepo) GetTunnel(id string) (model.Tunnel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tunnel, ok := r.tunnels[id]
	if !ok {
		return model.Tunnel{}, apperrors.New(apperrors.InvalidArgument, "tunnel not found")
	}
	return tunnel, nil
}

func (r *fakeRepo) SaveTunnel(tunnel model.Tunnel) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tunnels[tunnel.ID] = tunnel
	return nil
}

func (r *fakeRepo) UpdateTunnelStatus(id, status, lastError string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses[id] = status + "|" + lastError
	return nil
}

func (r *fakeRepo) DeleteTunnel(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tunnels, id)
	return nil
}

// SSH 仓库：peer 节点不应走到这里；一旦走到就返回错误，用来断言"没碰 SSH"。
type refusingSSHRepo struct{}

func (refusingSSHRepo) GetNode(string) (model.Node, error) {
	return model.Node{}, apperrors.New(apperrors.NotSupported, "ssh must not be used for peer tunnels")
}
func (refusingSSHRepo) GetCredential(string) (model.Credential, error) {
	return model.Credential{}, apperrors.New(apperrors.NotSupported, "ssh must not be used for peer tunnels")
}
func (refusingSSHRepo) UpdateNodeStatus(string, string, string) error { return nil }
func (refusingSSHRepo) SaveNode(model.Node) error                     { return nil }

func localTunnel(id, nodeID string) model.Tunnel {
	return model.Tunnel{
		ID: id, NodeID: nodeID, Name: "rustdesk", Type: "local",
		ListenAddress: "127.0.0.1", ListenPort: 0,
		TargetHost: "192.168.2.192", TargetPort: 21115, Status: "stopped",
	}
}

// startEcho 起一个本机回声服务，用来断言隧道真的把字节转出去了。
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

// 回归：peer 节点（反向接入，没有 SSH 服务端）启动 Local 隧道时不得建立 SSH 客户端。
// 否则会因 auth_method=peer 报 "unsupported authentication method"。
func TestStartLocalTunnelOnPeerNodeSkipsSSH(t *testing.T) {
	repo := newFakeRepo(localTunnel("t-peer", "peer-node"))
	manager := NewManager(repo, sshx.NewManager(refusingSSHRepo{}))
	manager.IsPeer = func(nodeID string) bool { return nodeID == "peer-node" }

	// 数据面走注入的拨号器（真实实现是 service.DialNode → 反向链路）
	echo := startEcho(t)
	manager.DialTarget = func(ctx context.Context, nodeID, address string) (net.Conn, error) {
		if nodeID != "peer-node" || address != "192.168.2.192:21115" {
			return nil, apperrors.New(apperrors.InvalidArgument, "unexpected dial: "+nodeID+"/"+address)
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", echo)
	}
	defer manager.Close()

	started, err := manager.Start(context.Background(), "t-peer")
	if err != nil {
		t.Fatalf("peer 节点的 Local 隧道启动失败（不应触发 SSH）: %v", err)
	}
	if started.ID != "t-peer" || !manager.IsRunning("t-peer") {
		t.Fatalf("tunnel must be running: %#v", started)
	}
	if port := manager.RuntimePort("t-peer"); port == 0 {
		t.Fatal("runtime port must be resolved")
	}

	// 转发链路本身也要通
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(manager.RuntimePort("t-peer"))), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("through-peer\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "echo:through-peer" {
		t.Fatalf("unexpected forwarded response: %q", line)
	}
}

// peer 节点不支持反向隧道（-R 需要节点侧有 SSH 服务端），应给出明确错误。
func TestStartRemoteTunnelOnPeerNodeIsRejected(t *testing.T) {
	tunnel := localTunnel("t-remote", "peer-node")
	tunnel.Type = "remote"
	repo := newFakeRepo(tunnel)
	manager := NewManager(repo, sshx.NewManager(refusingSSHRepo{}))
	manager.IsPeer = func(nodeID string) bool { return nodeID == "peer-node" }
	defer manager.Close()

	_, err := manager.Start(context.Background(), "t-remote")
	if err == nil {
		t.Fatal("peer 节点的反向隧道必须被拒绝")
	}
	if apperrors.CodeOf(err) != apperrors.NotSupported {
		t.Fatalf("expected not_supported, got %v", err)
	}
	if !strings.Contains(err.Error(), "端口映射") {
		t.Fatalf("错误信息应引导改用端口映射: %v", err)
	}
}

// 非 peer 节点保持原行为：启动时先校验 SSH（快速失败）。
func TestStartLocalTunnelOnSSHNodeStillValidatesSSH(t *testing.T) {
	repo := newFakeRepo(localTunnel("t-ssh", "ssh-node"))
	manager := NewManager(repo, sshx.NewManager(refusingSSHRepo{}))
	manager.IsPeer = func(nodeID string) bool { return false }
	defer manager.Close()

	_, err := manager.Start(context.Background(), "t-ssh")
	if err == nil {
		t.Fatal("SSH 节点启动隧道时应先校验 SSH 连接")
	}
	if !strings.Contains(err.Error(), "ssh must not be used") {
		t.Fatalf("expected ssh validation failure, got: %v", err)
	}
}
