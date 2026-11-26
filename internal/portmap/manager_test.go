package portmap

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
)

type fakeRepo struct {
	mu       sync.Mutex
	mappings map[string]model.PortMapping
}

func newFakeRepo(mappings ...model.PortMapping) *fakeRepo {
	repo := &fakeRepo{mappings: map[string]model.PortMapping{}}
	for _, mapping := range mappings {
		repo.mappings[mapping.ID] = mapping
	}
	return repo
}

func (r *fakeRepo) ListPortMappings() ([]model.PortMapping, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]model.PortMapping, 0, len(r.mappings))
	for _, mapping := range r.mappings {
		result = append(result, mapping)
	}
	return result, nil
}

func (r *fakeRepo) GetPortMapping(id string) (model.PortMapping, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mapping, ok := r.mappings[id]
	if !ok {
		return model.PortMapping{}, apperrors.New(apperrors.InvalidArgument, "port mapping not found")
	}
	return mapping, nil
}

func (r *fakeRepo) SavePortMapping(mapping model.PortMapping) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mappings[mapping.ID] = mapping
	return nil
}

func (r *fakeRepo) UpdatePortMappingStatus(id, status, lastError string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	mapping, ok := r.mappings[id]
	if !ok {
		return apperrors.New(apperrors.InvalidArgument, "port mapping not found")
	}
	mapping.Status = status
	mapping.LastError = lastError
	r.mappings[id] = mapping
	return nil
}

func (r *fakeRepo) DeletePortMapping(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.mappings, id)
	return nil
}

type fakeDialer struct {
	echoAddress string
	via         string
}

func (d fakeDialer) DialRoute(context.Context, string) (net.Conn, model.RelayRoute, error) {
	return d.dial()
}

func (d fakeDialer) DialNode(_ context.Context, nodeID, _ string) (net.Conn, model.RelayRoute, error) {
	conn, err := net.Dial("tcp", d.echoAddress)
	if err != nil {
		return nil, model.RelayRoute{}, err
	}
	return conn, model.RelayRoute{ViaNodeID: nodeID, Healthy: true}, nil
}

func (d fakeDialer) dial() (net.Conn, model.RelayRoute, error) {
	conn, err := net.Dial("tcp", d.echoAddress)
	if err != nil {
		return nil, model.RelayRoute{}, err
	}
	via := d.via
	if via == "" {
		via = "direct"
	}
	return conn, model.RelayRoute{ViaNodeID: via, Healthy: true}, nil
}

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

func mapping(id, host string, port int) model.PortMapping {
	return model.PortMapping{ID: id, Name: id, TargetHost: host, TargetPort: port, ListenAddress: "127.0.0.1", ListenPort: 0, Status: "stopped"}
}

func TestManagerRelaysConnectionsAndTracksViaNode(t *testing.T) {
	echo := startEcho(t)
	repo := newFakeRepo(mapping("map-1", "192.168.50.20", 21115))
	manager := NewManager(repo, fakeDialer{echoAddress: echo, via: "node-edge"})
	started, err := manager.Start(context.Background(), "map-1")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if started.RuntimePort == 0 {
		t.Fatal("runtime port must be resolved")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(started.RuntimePort)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "echo:ping" {
		t.Fatalf("unexpected relayed response: %q", line)
	}
	port, via := manager.RuntimeInfo("map-1")
	if port != started.RuntimePort || via != "node-edge" {
		t.Fatalf("runtime info mismatch: port=%d via=%q", port, via)
	}
	list, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != "running" || list[0].ViaRoute != "node-edge" {
		t.Fatalf("unexpected list result: %#v", list)
	}
	if err := manager.Stop("map-1"); err != nil {
		t.Fatal(err)
	}
	if manager.IsRunning("map-1") {
		t.Fatal("mapping must be stopped")
	}
	stopped, _ := repo.GetPortMapping("map-1")
	if stopped.Status != "stopped" {
		t.Fatalf("stored status must be stopped, got %q", stopped.Status)
	}
}

func TestManagerReportsPortInUse(t *testing.T) {
	echo := startEcho(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	definition := mapping("map-2", "192.168.50.21", 21115)
	definition.ListenPort = port
	repo := newFakeRepo(definition)
	manager := NewManager(repo, fakeDialer{echoAddress: echo})
	defer manager.Close()
	if _, err := manager.Start(context.Background(), "map-2"); err == nil {
		t.Fatal("expected port in use error")
	} else if apperrors.CodeOf(err) != apperrors.PortInUse {
		t.Fatalf("expected port_in_use, got %v", err)
	}
}

func TestManagerUsesPinnedRelayNodeAndRestores(t *testing.T) {
	echo := startEcho(t)
	definition := mapping("map-3", "192.168.50.22", 21115)
	definition.ViaNodeID = "node-edge"
	definition.AutoStart = true
	repo := newFakeRepo(definition)
	manager := NewManager(repo, fakeDialer{echoAddress: echo})
	if err := manager.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if !manager.IsRunning("map-3") {
		t.Fatal("auto-start mapping must be restored")
	}
	port, _ := manager.RuntimeInfo("map-3")
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || strings.TrimSpace(line) != "echo:hello" {
		t.Fatalf("unexpected response %q err=%v", line, err)
	}
	_, via := manager.RuntimeInfo("map-3")
	if via != "node-edge" {
		t.Fatalf("pinned node must be reported, got %q", via)
	}
	if err := manager.Delete("map-3"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetPortMapping("map-3"); err == nil {
		t.Fatal("mapping must be removed")
	}
}
