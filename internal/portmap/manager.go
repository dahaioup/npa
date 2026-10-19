// Package portmap 实现 NPA 自带的端口映射：在本机监听一个端口，把入站连接转发到
// 子网内的目标 IP:端口。转发路径由 relay 选路（本机可达时直连，否则经已纳管且授权的
// 节点中继），因此它与 SSH 隧道相互独立，不依赖任何 SSH 隧道定义。
//
// 典型场景：本机无法直接访问子网设备，但子网内的 NPA 节点可以。把子网设备的
// RustDesk 服务端口映射到本机端口后，本机的 RustDesk 客户端连接本机端口即可远程该设备。
package portmap

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
)

const dialTimeout = 5 * time.Second

type Repository interface {
	ListPortMappings() ([]model.PortMapping, error)
	GetPortMapping(id string) (model.PortMapping, error)
	SavePortMapping(mapping model.PortMapping) error
	UpdatePortMappingStatus(id, status, lastError string) error
	DeletePortMapping(id string) error
}

// Dialer 建立到目标的转发路径；生产实现为 relay.Manager。
type Dialer interface {
	DialRoute(ctx context.Context, target string) (net.Conn, model.RelayRoute, error)
	DialNode(ctx context.Context, nodeID, target string) (net.Conn, model.RelayRoute, error)
}

type runtimeState struct {
	cancel   context.CancelFunc
	listener net.Listener
	port     int
	via      string
}

type Manager struct {
	repo            Repository
	dial            Dialer
	mu              sync.Mutex
	runtime         map[string]*runtimeState
	TargetValidator func(host string, port int) error
}

func NewManager(repo Repository, dialer Dialer) *Manager {
	return &Manager{repo: repo, dial: dialer, runtime: map[string]*runtimeState{}}
}

func listenAddress(mapping model.PortMapping) string {
	if strings.TrimSpace(mapping.ListenAddress) == "" {
		return "127.0.0.1"
	}
	return mapping.ListenAddress
}

func targetAddress(mapping model.PortMapping) string {
	return net.JoinHostPort(mapping.TargetHost, strconv.Itoa(mapping.TargetPort))
}

func (m *Manager) IsRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtime[id] != nil
}

// RuntimeInfo 返回映射的实际监听端口与当前经由路径（未运行时返回 0 与空串）。
func (m *Manager) RuntimeInfo(id string) (int, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.runtime[id]
	if state == nil {
		return 0, ""
	}
	return state.port, state.via
}

func (m *Manager) setVia(id, via string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.runtime[id]; state != nil {
		state.via = via
	}
}

func (m *Manager) List() ([]model.PortMapping, error) {
	mappings, err := m.repo.ListPortMappings()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	for i := range mappings {
		if state := m.runtime[mappings[i].ID]; state != nil {
			mappings[i].Status = "running"
			mappings[i].RuntimePort = state.port
			mappings[i].ViaRoute = state.via
		} else if mappings[i].Status == "running" {
			mappings[i].Status = "stopped"
			mappings[i].RuntimePort = 0
			mappings[i].ViaRoute = ""
		}
	}
	m.mu.Unlock()
	return mappings, nil
}

func (m *Manager) Start(ctx context.Context, id string) (model.PortMapping, error) {
	mapping, err := m.repo.GetPortMapping(id)
	if err != nil {
		return mapping, err
	}
	m.mu.Lock()
	if m.runtime[id] != nil {
		m.mu.Unlock()
		return mapping, nil
	}
	m.mu.Unlock()

	if mapping.TargetHost == "" || mapping.TargetPort <= 0 || mapping.TargetPort > 65535 {
		return mapping, apperrors.New(apperrors.InvalidArgument, "目标主机与端口必须有效")
	}
	if m.TargetValidator != nil {
		if err := m.TargetValidator(mapping.TargetHost, mapping.TargetPort); err != nil {
			_ = m.repo.UpdatePortMappingStatus(id, "error", apperrors.MessageOf(err))
			return mapping, err
		}
	}

	address := net.JoinHostPort(listenAddress(mapping), strconv.Itoa(mapping.ListenPort))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			_ = m.repo.UpdatePortMappingStatus(id, "error", apperrors.MessageOf(err))
			return mapping, apperrors.New(apperrors.PortInUse, "本机映射端口已被占用: "+address)
		}
		_ = m.repo.UpdatePortMappingStatus(id, "error", apperrors.MessageOf(err))
		return mapping, apperrors.Wrap(apperrors.Internal, "cannot listen for port mapping", err)
	}
	actualPort := 0
	if tcpAddress, ok := listener.Addr().(*net.TCPAddr); ok {
		actualPort = tcpAddress.Port
	}
	runCtx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.runtime[id] = &runtimeState{cancel: cancel, listener: listener, port: actualPort}
	m.mu.Unlock()
	_ = m.repo.UpdatePortMappingStatus(id, "running", "")
	mapping.Status = "running"
	mapping.RuntimePort = actualPort
	go m.acceptLoop(runCtx, mapping, listener)
	return mapping, nil
}

func (m *Manager) acceptLoop(ctx context.Context, mapping model.PortMapping, listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		go m.handleConnection(ctx, mapping, conn)
	}
}

func (m *Manager) handleConnection(ctx context.Context, mapping model.PortMapping, inbound net.Conn) {
	defer inbound.Close()
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var (
		remote net.Conn
		route  model.RelayRoute
		err    error
	)
	if mapping.ViaNodeID != "" {
		remote, route, err = m.dial.DialNode(dialCtx, mapping.ViaNodeID, targetAddress(mapping))
	} else {
		remote, route, err = m.dial.DialRoute(dialCtx, targetAddress(mapping))
	}
	if err != nil {
		_ = m.repo.UpdatePortMappingStatus(mapping.ID, "error", apperrors.MessageOf(err))
		return
	}
	defer remote.Close()
	m.setVia(mapping.ID, route.ViaNodeID)
	pipe(inbound, remote)
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
		return m.repo.UpdatePortMappingStatus(id, "stopped", "")
	}
	state.cancel()
	err := state.listener.Close()
	if updateErr := m.repo.UpdatePortMappingStatus(id, "stopped", ""); err == nil {
		err = updateErr
	}
	return err
}

func (m *Manager) Delete(id string) error {
	_ = m.Stop(id)
	return m.repo.DeletePortMapping(id)
}

func (m *Manager) Restore(ctx context.Context) error {
	mappings, err := m.repo.ListPortMappings()
	if err != nil {
		return err
	}
	for _, mapping := range mappings {
		if !mapping.AutoStart {
			continue
		}
		if _, err := m.Start(ctx, mapping.ID); err != nil {
			_ = m.repo.UpdatePortMappingStatus(mapping.ID, "error", apperrors.MessageOf(err))
		}
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

func FormatTarget(host string, port int) string { return fmt.Sprintf("%s:%d", host, port) }
