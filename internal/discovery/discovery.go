package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
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
	// startedAt 进程启动时刻；mDNS TXT 广播 uptime= 运行秒数，供分域管理者选举。
	startedAt time.Time
	// peerPort / children 用于 mDNS TXT（role、children、peer），由 service 注入
	peerPort func() int
	children func() int
}

// SetLocalInfo 注入本机的反向接入端口与已注册下级数量，用于广播 role/children/peer。
func (m *Manager) SetLocalInfo(peerPort func() int, children func() int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peerPort, m.children = peerPort, children
}

func (m *Manager) localPeerPort() int {
	m.mu.Lock()
	fn := m.peerPort
	m.mu.Unlock()
	if fn != nil {
		if port := fn(); port > 0 {
			return port
		}
	}
	return 56701
}

func (m *Manager) localChildren() int {
	m.mu.Lock()
	fn := m.children
	m.mu.Unlock()
	if fn == nil {
		return 0
	}
	return fn()
}

// localUptime 本机进程运行秒数（分域管理者选举广播）。
func (m *Manager) localUptime() int64 { return int64(time.Since(m.startedAt) / time.Second) }

func NewManager(repo Repository, version string) *Manager {
	return &Manager{repo: repo, version: version, status: "disabled", startedAt: time.Now()}
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

// onMDNSInstance 把轮询器解析出的实例写成发现节点。
func (m *Manager) onMDNSInstance(instance mdnsInstance) {
	values := map[string]string{}
	for _, field := range instance.TXT {
		key, value, ok := strings.Cut(field, "=")
		if ok {
			values[strings.ToLower(key)] = value
		}
	}
	id := values["id"]
	if id == "" {
		id = instance.Instance
	}
	if id == m.localID {
		return
	}
	if len(instance.Addrs) == 0 {
		return
	}
	name := values["name"]
	if name == "" {
		name = instance.Instance
	}
	now := time.Now().UTC()
	node := model.Node{
		ID: id, DisplayID: values["id"], Name: name,
		Address: instance.Addrs[0].String(), Port: instance.Port,
		AuthMethod: "discovered", Protocol: "discovered", Status: "discovered",
		Capabilities: splitCSV(values["caps"]), ReachableSubnets: splitCSV(values["subnets"]),
		Version: strings.TrimSpace(values["ver"]),
		// 旧版本节点在 mDNS TXT 里广播的是 56710：端口收敛后指向同一个服务
		PeerPort:   normalizeLegacyPort(atoiOr(values["peer"], 0)),
		Uptime:     int64(atoiOr(values["up"], 0)),
		Discovered: true, LastSeen: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := m.repo.SaveDiscoveredNode(node); err != nil {
		slog.Warn("cannot persist mdns discovered node", "id", node.ID, "error", err)
		return
	}
	slog.Info("mDNS entry received", "id", node.ID, "name", node.Name, "address", node.Address, "peer_port", node.PeerPort)
}

// PrimaryIP 返回本机首选私网地址，取不到时退回回环地址。
func PrimaryIP() string {
	// 先问内核“去公网会走哪张网卡”，拿到出口地址：虚拟机/VPN 的虚拟网卡
	// （172.31.x、192.168.110.1 之类）不会出现在这条路径上。
	// UDP 的 Dial 不发包，只是让内核选路。
	if conn, err := net.Dial("udp4", "223.5.5.5:53"); err == nil {
		if local, ok := conn.LocalAddr().(*net.UDPAddr); ok && local.IP != nil {
			ip := local.IP
			if parsed, ok := netip.AddrFromSlice(ip.To4()); ok && !parsed.IsLoopback() && parsed.IsPrivate() {
				conn.Close()
				return parsed.String()
			}
		}
		conn.Close()
	}
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

// txtRecord 广播本机信息；role=upper / children=N 让同网段的其它 NPA 一眼看出谁是"上级"，
// 从而去它那里查拓扑；peer= 是反向接入端口，跨上级查询要用。
func (m *Manager) txtRecord(nodeID, name string, subnets []string, capabilities []string, peerPort, children int) []string {
	role := "node"
	if children > 0 {
		role = "upper"
	}
	return []string{
		"id=" + nodeID,
		"name=" + name,
		"ver=" + strings.TrimPrefix(m.version, "v"),
		"api=56700",
		"peer=" + strconv.Itoa(peerPort),
		"role=" + role,
		"children=" + strconv.Itoa(children),
		"up=" + strconv.FormatInt(m.localUptime(), 10),
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
	server, err := zeroconf.Register(instance, serviceType, "local.", 56700, m.txtRecord(m.localID, host, LocalSubnets(), []string{"ssh", "file", "tunnel", "screenshot", "mouse", "discovery", "relay"}, m.localPeerPort(), m.localChildren()), nil)
	if err != nil {
		m.mu.Lock()
		m.status, m.lastErr = "failed", err.Error()
		m.mu.Unlock()
		return err
	}
	// 广播用 zeroconf，接收用自己的轮询器：库的 Browse 只在单个报文里凑齐
	// PTR+SRV+TXT+A 才产出条目，NPA 应答 PTR 时只带 PTR，于是永远收不到。
	browseCtx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.server, m.cancel, m.status = server, cancel, "running"
	m.mu.Unlock()
	go newMDNSPoller(m.onMDNSInstance).start(browseCtx)
	slog.Info("mDNS discovery started", "instance", instance, "service", serviceType+".local.")
	_ = ctx
	return nil
}

func atoiOr(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// normalizeLegacyPort 把端口收敛前的 56710 归一为 56701。
func normalizeLegacyPort(port int) int {
	if port == 56710 {
		return 56701
	}
	return port
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

// Find 按节点 ID 查一个发现记录。同网段节点只靠广播互见、没有握手注册，
// 上层（exec/file）要靠它拿到地址，直接走对端的 mesh HTTP 端口。
func (m *Manager) Find(id string) (model.Node, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return model.Node{}, false
	}
	nodes, err := m.repo.ListDiscoveredNodes()
	if err != nil {
		return model.Node{}, false
	}
	for _, node := range nodes {
		if node.ID == id {
			return node, true
		}
	}
	return model.Node{}, false
}

// StaleAfter 是发现节点的清理阈值：离线（超过该时长没有再收到它的广播）就从列表移除。
const StaleAfter = 6 * time.Hour

// pruneInterval 是后台清理的扫描间隔。
const pruneInterval = 30 * time.Minute

// PruneStale 清理过期的已发现节点，返回清理数量。
// 只处理 mDNS 发现的节点；手工添加与反向接入（peer）的节点不会被自动删除。
func (m *Manager) PruneStale(now time.Time) (int, error) {
	nodes, err := m.repo.ListDiscoveredNodes()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, node := range nodes {
		seen := node.LastSeen
		if seen.IsZero() {
			seen = node.UpdatedAt
		}
		// "超过 6 小时" 是严格大于：正好 6 小时仍保留
		if seen.IsZero() || now.Sub(seen) <= StaleAfter {
			continue
		}
		if err := m.repo.DeleteDiscoveredNode(node.ID); err != nil {
			continue
		}
		removed++
		slog.Info("清理离线超过 6 小时的发现节点",
			"id", node.ID, "name", node.Name, "last_seen", seen.Format(time.RFC3339))
	}
	return removed, nil
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
