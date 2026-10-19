package tunnel

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	sshx "github.com/npa/npa/internal/ssh"
)

type Repository interface {
	ListTunnels() ([]model.Tunnel, error)
	GetTunnel(id string) (model.Tunnel, error)
	SaveTunnel(tunnel model.Tunnel) error
	UpdateTunnelStatus(id, status, lastError string) error
	DeleteTunnel(id string) error
}

type runtimeState struct {
	cancel   context.CancelFunc
	listener net.Listener
	port     int
}

type Manager struct {
	repo            Repository
	ssh             *sshx.Manager
	mu              sync.Mutex
	runtime         map[string]*runtimeState
	TargetValidator func(host string, port int) error
	// DialTarget 由 service 注入，用于让 peer 节点（反向链路）也能承载 Local/SOCKS 转发；
	// 未注入时退回 SSH direct-tcpip。
	DialTarget func(ctx context.Context, nodeID, address string) (net.Conn, error)
	// IsPeer 判断节点是否为反向接入（peer）节点：它的连接由它主动建立，
	// 没有 SSH 服务端，启动 local/socks 隧道时不应尝试建立 SSH 客户端。
	IsPeer func(nodeID string) bool
}

func NewManager(repo Repository, sshManager *sshx.Manager) *Manager {
	return &Manager{repo: repo, ssh: sshManager, runtime: map[string]*runtimeState{}}
}

func listenHostPort(address string, port int) string {
	if address == "" {
		address = "127.0.0.1"
	}
	return net.JoinHostPort(address, strconv.Itoa(port))
}

func (m *Manager) IsRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtime[id] != nil
}

func (m *Manager) RuntimePort(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.runtime[id]; state != nil {
		return state.port
	}
	return 0
}

func (m *Manager) List() ([]model.Tunnel, error) {
	tunnels, err := m.repo.ListTunnels()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	for i := range tunnels {
		if state := m.runtime[tunnels[i].ID]; state != nil {
			tunnels[i].Status = "running"
			if tunnels[i].ListenPort == 0 {
				tunnels[i].ListenPort = state.port
			}
		} else if tunnels[i].Status == "running" {
			tunnels[i].Status = "stopped"
		}
	}
	m.mu.Unlock()
	return tunnels, nil
}

func (m *Manager) Start(ctx context.Context, id string) (model.Tunnel, error) {
	tunnel, err := m.repo.GetTunnel(id)
	if err != nil {
		return tunnel, err
	}
	m.mu.Lock()
	if m.runtime[id] != nil {
		m.mu.Unlock()
		return tunnel, nil
	}
	m.mu.Unlock()
	isPeer := m.IsPeer != nil && m.IsPeer(tunnel.NodeID)
	var (
		sshClient interface {
			Dial(network, addr string) (net.Conn, error)
			Listen(network, laddr string) (net.Listener, error)
		}
		listener net.Listener
	)
	listenAddress := listenHostPort(tunnel.ListenAddress, tunnel.ListenPort)

	if tunnel.Type == "remote" {
		// 反向隧道要在**节点上**监听，需要节点侧有 SSH 服务端；peer 节点没有。
		if isPeer {
			err := apperrors.New(apperrors.NotSupported,
				"peer 节点没有 SSH 服务端，不支持反向隧道；请改用端口映射或 Local/SOCKS 隧道")
			_ = m.repo.UpdateTunnelStatus(id, "error", err.Error())
			return tunnel, err
		}
		client, clientErr := m.ssh.Client(ctx, tunnel.NodeID)
		if clientErr != nil {
			_ = m.repo.UpdateTunnelStatus(id, "error", clientErr.Error())
			return tunnel, clientErr
		}
		sshClient = client
		listener, err = client.Listen("tcp", listenAddress)
	} else {
		// Local / SOCKS 的数据面走 DialTarget（peer 节点经反向链路），
		// 但非 peer 节点仍先建立 SSH 连接，保持"启动即校验"的快速失败反馈。
		if !isPeer {
			client, clientErr := m.ssh.Client(ctx, tunnel.NodeID)
			if clientErr != nil {
				_ = m.repo.UpdateTunnelStatus(id, "error", clientErr.Error())
				return tunnel, clientErr
			}
			sshClient = client
		}
		listener, err = net.Listen("tcp", listenAddress)
	}
	if err != nil {
		_ = m.repo.UpdateTunnelStatus(id, "error", err.Error())
		if strings.Contains(err.Error(), "address already in use") {
			return tunnel, apperrors.New(apperrors.PortInUse, "local tunnel port is already in use")
		}
		return tunnel, apperrors.Wrap(apperrors.Internal, "cannot listen for tunnel", err)
	}
	actualPort := 0
	if address, ok := listener.Addr().(*net.TCPAddr); ok {
		actualPort = address.Port
	}
	runCtx, cancel := context.WithCancel(context.Background())
	state := &runtimeState{cancel: cancel, listener: listener, port: actualPort}
	m.mu.Lock()
	m.runtime[id] = state
	m.mu.Unlock()
	_ = m.repo.UpdateTunnelStatus(id, "running", "")
	go m.acceptLoop(runCtx, tunnel, listener, sshClient)
	return tunnel, nil
}

func (m *Manager) acceptLoop(ctx context.Context, definition model.Tunnel, listener net.Listener, sshClient interface {
	Dial(network, addr string) (net.Conn, error)
	Listen(network, laddr string) (net.Listener, error)
}) {
	backoff := time.Second
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
				time.Sleep(backoff)
				backoff *= 2
				continue
			}
		}
		backoff = time.Second
		go m.handleConnection(ctx, definition, conn, sshClient)
	}
}

func (m *Manager) handleConnection(ctx context.Context, definition model.Tunnel, inbound net.Conn, sshClient interface {
	Dial(network, addr string) (net.Conn, error)
	Listen(network, laddr string) (net.Listener, error)
}) {
	defer inbound.Close()
	if definition.Type == "socks" {
		m.handleSOCKS(ctx, definition, inbound, sshClient)
		return
	}
	if definition.Type == "remote" {
		local, err := net.DialTimeout("tcp", net.JoinHostPort(definition.TargetHost, strconv.Itoa(definition.TargetPort)), 5*time.Second)
		if err != nil {
			return
		}
		defer local.Close()
		pipe(inbound, local)
		return
	}
	remote, err := m.dialTarget(ctx, definition.NodeID, net.JoinHostPort(definition.TargetHost, strconv.Itoa(definition.TargetPort)))
	if err != nil {
		_ = m.repo.UpdateTunnelStatus(definition.ID, "error", err.Error())
		m.ssh.Close(definition.NodeID)
		return
	}
	defer remote.Close()
	pipe(inbound, remote)
}

func (m *Manager) handleSOCKS(ctx context.Context, definition model.Tunnel, conn net.Conn, sshClient interface {
	Dial(network, addr string) (net.Conn, error)
}) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil || request[0] != 5 || request[1] != 1 {
		return
	}
	var host string
	switch request[3] {
	case 1:
		raw := make([]byte, 4)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return
		}
		raw := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = string(raw)
	case 4:
		raw := make([]byte, 16)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	default:
		_, _ = conn.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	rawPort := make([]byte, 2)
	if _, err := io.ReadFull(conn, rawPort); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(rawPort)
	if m.TargetValidator != nil {
		if err := m.TargetValidator(host, int(port)); err != nil {
			_, _ = conn.Write([]byte{5, 2, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
	}
	remote, err := m.dialTarget(ctx, definition.NodeID, net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer remote.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	pipe(conn, remote)
}

// dialTarget 经指定节点拨号；注入的 DialTarget 让 peer 节点同样可用。
func (m *Manager) dialTarget(ctx context.Context, nodeID, address string) (net.Conn, error) {
	if m.DialTarget != nil {
		return m.DialTarget(ctx, nodeID, address)
	}
	client, err := m.ssh.Client(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	return client.Dial("tcp", address)
}

func pipe(left, right net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(right, left); _ = right.Close(); done <- struct{}{} }()
	go func() { _, _ = io.Copy(left, right); _ = left.Close(); done <- struct{}{} }()
	<-done
}

func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	state := m.runtime[id]
	delete(m.runtime, id)
	m.mu.Unlock()
	if state == nil {
		return m.repo.UpdateTunnelStatus(id, "stopped", "")
	}
	state.cancel()
	err := state.listener.Close()
	if updateErr := m.repo.UpdateTunnelStatus(id, "stopped", ""); err == nil {
		err = updateErr
	}
	return err
}

func (m *Manager) Delete(id string) error {
	_ = m.Stop(id)
	return m.repo.DeleteTunnel(id)
}

func (m *Manager) Restore(ctx context.Context) error {
	settingsTunnels, err := m.repo.ListTunnels()
	if err != nil {
		return err
	}
	for _, tunnel := range settingsTunnels {
		if !tunnel.AutoStart {
			continue
		}
		_, _ = m.Start(ctx, tunnel.ID)
	}
	return nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.runtime))
	for id := range m.runtime {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Stop(id)
	}
}

func (m *Manager) Dial(ctx context.Context, nodeID, address string) (net.Conn, error) {
	client, err := m.ssh.Client(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	conn, err := client.Dial("tcp", address)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.NodeOffline, "cannot dial through SSH", err)
	}
	return conn, nil
}

func ValidateTarget(target string) error {
	host, portText, err := net.SplitHostPort(target)
	if err != nil || host == "" || portText == "" {
		return apperrors.New(apperrors.InvalidArgument, "target must be host:port")
	}
	if _, err := strconv.Atoi(portText); err != nil {
		return apperrors.New(apperrors.InvalidArgument, "invalid target port")
	}
	return nil
}

func FormatTarget(host string, port int) string { return fmt.Sprintf("%s:%d", host, port) }
