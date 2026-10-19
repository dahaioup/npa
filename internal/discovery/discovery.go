package discovery

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"

	"github.com/npa/npa/internal/model"
)

const serviceType = "_npa._tcp"

type Repository interface {
	ListDiscoveredNodes() ([]model.Node, error)
	SaveDiscoveredNode(node model.Node) error
	DeleteDiscoveredNode(id string) error
}

type Manager struct {
	repo    Repository
	version string
	mu      sync.Mutex
	enabled bool
	server  *zeroconf.Server
	cancel  context.CancelFunc
	status  string
	lastErr string
	localID string
}

func NewManager(repo Repository, version string) *Manager {
	return &Manager{repo: repo, version: version, status: "disabled"}
}

// LocalIPs 返回本机私网单播地址（IPv4 优先，排序稳定），用于节点自报可达地址。
func LocalIPs() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return []string{}
	}
	v4 := map[string]struct{}{}
	v6 := map[string]struct{}{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			prefix, err := netip.ParsePrefix(addr.String())
			if err != nil {
				continue
			}
			ip := prefix.Addr().Unmap()
			if !ip.IsPrivate() || ip.IsLoopback() {
				continue
			}
			if ip.Is4() {
				v4[ip.String()] = struct{}{}
			} else {
				v6[ip.String()] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(v4)+len(v6))
	for ip := range v4 {
		result = append(result, ip)
	}
	for ip := range v6 {
		result = append(result, ip)
	}
	sort.Strings(result)
	return result
}

// PrimaryIP 返回本机首选私网地址，取不到时退回回环地址。
func PrimaryIP() string {
	addresses := LocalIPs()
	if len(addresses) == 0 {
		return "127.0.0.1"
	}
	return addresses[0]
}

func LocalSubnets() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return []string{}
	}
	set := map[string]struct{}{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			prefix, err := netip.ParsePrefix(addr.String())
			if err != nil {
				continue
			}
			ip := prefix.Addr()
			if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				set[prefix.Masked().String()] = struct{}{}
			}
		}
	}
	subnets := make([]string, 0, len(set))
	for subnet := range set {
		subnets = append(subnets, subnet)
	}
	sort.Strings(subnets)
	return subnets
}

func (m *Manager) txtRecord(nodeID, name string, subnets []string, capabilities []string) []string {
	return []string{
		"id=" + nodeID,
		"name=" + name,
		"ver=" + strings.TrimPrefix(m.version, "v"),
		"api=56700",
		"caps=" + strings.Join(capabilities, ","),
		"subnets=" + strings.Join(subnets, ","),
	}
}

func localDiscoveryID(host string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(host) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('-')
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		slug = "node"
	}
	return "node-" + slug
}

func (m *Manager) SetEnabled(ctx context.Context, enabled bool) error {
	m.mu.Lock()
	if m.enabled == enabled {
		m.mu.Unlock()
		return nil
	}
	m.enabled = enabled
	if !enabled {
		cancel := m.cancel
		server := m.server
		m.cancel, m.server = nil, nil
		m.status, m.lastErr = "disabled", ""
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if server != nil {
			server.Shutdown()
		}
		return nil
	}
	m.mu.Unlock()
	if err := m.start(ctx); err != nil {
		m.mu.Lock()
		m.enabled = false
		m.status, m.lastErr = "failed", err.Error()
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *Manager) start(ctx context.Context) error {
	m.mu.Lock()
	if m.server != nil {
		m.mu.Unlock()
		return nil
	}
	m.status, m.lastErr = "starting", ""
	m.mu.Unlock()
	host, _ := os.Hostname()
	if host == "" {
		host = "npa"
	}
	instance := "NPA-" + host
	m.localID = model.NodeIDFromMAC()
	server, err := zeroconf.Register(instance, serviceType, "local.", 56700, m.txtRecord(m.localID, host, LocalSubnets(), []string{"ssh", "file", "tunnel", "screenshot", "mouse", "discovery", "relay"}), nil)
	if err != nil {
		m.mu.Lock()
		m.status, m.lastErr = "failed", err.Error()
		m.mu.Unlock()
		return err
	}
	browseCtx, cancel := context.WithCancel(context.Background())
	entries := make(chan *zeroconf.ServiceEntry, 16)
	resolver, err := zeroconf.NewResolver()
	if err == nil {
		err = resolver.Browse(browseCtx, serviceType, "local.", entries)
	}
	if err != nil {
		cancel()
		server.Shutdown()
		m.mu.Lock()
		m.status, m.lastErr = "failed", err.Error()
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	m.server, m.cancel, m.status = server, cancel, "running"
	m.mu.Unlock()
	go m.consume(entries, browseCtx)
	if runtime.GOOS == "darwin" {
		go m.browseWithDnsSD(browseCtx)
	}
	slog.Info("mDNS discovery started", "instance", instance, "service", serviceType+".local.")
	_ = ctx
	return nil
}

func (m *Manager) browseWithDnsSD(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "dns-sd", "-B", serviceType, "local.")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		slog.Debug("dns-sd fallback unavailable", "error", err)
		return
	}
	seen := map[string]struct{}{}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[1] != "Add" {
			continue
		}
		instance := fields[len(fields)-1]
		if instance == "" {
			continue
		}
		if _, exists := seen[instance]; exists {
			continue
		}
		seen[instance] = struct{}{}
		go m.resolveWithDnsSD(ctx, instance)
	}
	_ = cmd.Wait()
}

func (m *Manager) resolveWithDnsSD(ctx context.Context, instance string) {
	resolveCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(resolveCtx, "dns-sd", "-L", instance, serviceType, "local.")
	output, _ := cmd.Output()
	lines := strings.Split(string(output), "\n")
	var hostPort, txtLine string
	for _, line := range lines {
		if strings.Contains(line, "can be reached at ") {
			hostPort = strings.TrimSpace(line[strings.Index(line, "can be reached at ")+len("can be reached at "):])
			if index := strings.Index(hostPort, " (interface"); index >= 0 {
				hostPort = strings.TrimSpace(hostPort[:index])
			}
		} else if strings.HasPrefix(strings.TrimSpace(line), "id=") {
			txtLine = strings.TrimSpace(line)
		}
	}
	if hostPort == "" || txtLine == "" {
		return
	}
	host, portText, err := net.SplitHostPort(hostPort)
	if err != nil {
		return
	}
	host = strings.TrimSuffix(host, ".")
	addresses, err := net.LookupIP(host)
	if err != nil || len(addresses) == 0 {
		return
	}
	selectedAddress := addresses[0]
	for _, address := range addresses {
		if address.To4() != nil {
			selectedAddress = address
			break
		}
	}
	values := map[string]string{}
	for _, field := range strings.Fields(txtLine) {
		if key, value, ok := strings.Cut(field, "="); ok {
			values[strings.ToLower(key)] = value
		}
	}
	id := values["id"]
	if id == "" {
		id = instance
	}
	if id == m.localID {
		return
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		return
	}
	node := model.Node{
		ID: id, DisplayID: values["id"], Name: values["name"], Address: selectedAddress.String(), Port: port,
		AuthMethod: "discovered", Protocol: "discovered", Status: "discovered",
		Capabilities: splitCSV(values["caps"]), ReachableSubnets: splitCSV(values["subnets"]),
		Discovered: true, LastSeen: time.Now().UTC(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if node.Name == "" {
		node.Name = instance
	}
	if err := m.repo.SaveDiscoveredNode(node); err != nil {
		slog.Warn("cannot persist dns-sd discovered node", "id", node.ID, "error", err)
		return
	}
	slog.Info("dns-sd entry received", "id", node.ID, "name", node.Name, "address", node.Address)
}

func (m *Manager) consume(entries <-chan *zeroconf.ServiceEntry, ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-entries:
			if !ok {
				return
			}
			slog.Debug("mDNS raw entry", "instance", entry.Instance, "service", entry.Service, "ipv4", len(entry.AddrIPv4), "ipv6", len(entry.AddrIPv6), "text", entry.Text)
			node, ok := parseEntry(entry)
			if !ok || node.ID == m.localID {
				slog.Debug("mDNS entry skipped", "instance", entry.Instance, "parsed", ok, "local_id", m.localID)
				continue
			}
			slog.Info("mDNS entry received", "id", node.ID, "name", node.Name, "address", node.Address)
			if err := m.repo.SaveDiscoveredNode(node); err != nil {
				slog.Warn("cannot persist discovered node", "id", node.ID, "error", err)
			}
		}
	}
}

func parseEntry(entry *zeroconf.ServiceEntry) (model.Node, bool) {
	if entry == nil {
		return model.Node{}, false
	}
	values := map[string]string{}
	for _, text := range entry.Text {
		key, value, found := strings.Cut(text, "=")
		if found {
			values[strings.ToLower(key)] = value
		}
	}
	id := values["id"]
	if id == "" {
		id = entry.Instance
	}
	address := ""
	if len(entry.AddrIPv4) > 0 {
		address = entry.AddrIPv4[0].String()
	} else if len(entry.AddrIPv6) > 0 {
		address = entry.AddrIPv6[0].String()
	}
	if address == "" {
		return model.Node{}, false
	}
	name := values["name"]
	if name == "" {
		name = entry.Instance
	}
	capabilities := splitCSV(values["caps"])
	subnets := splitCSV(values["subnets"])
	return model.Node{
		ID: id, DisplayID: values["id"], Name: name, Address: address, Port: entry.Port, Username: "", AuthMethod: "discovered",
		Protocol: "discovered", Status: "discovered", Capabilities: capabilities, ReachableSubnets: subnets,
		Discovered: true, LastSeen: time.Now().UTC(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}, true
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return []string{}
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func (m *Manager) Refresh(ctx context.Context) error {
	m.mu.Lock()
	enabled := m.enabled
	m.mu.Unlock()
	if !enabled {
		return nil
	}
	return m.start(ctx)
}

func (m *Manager) Nodes() ([]model.Node, error) {
	return m.repo.ListDiscoveredNodes()
}

func (m *Manager) Status() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status, m.lastErr
}

func (m *Manager) Close() {
	m.SetEnabled(context.Background(), false)
}

func InstanceName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "npa"
	}
	return fmt.Sprintf("NPA-%s", host)
}
