package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/npa/npa/internal/discovery"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	sshx "github.com/npa/npa/internal/ssh"
)

var ErrNoRoute = errors.New("no authorized relay route")

type Repository interface {
	GetNode(id string) (model.Node, error)
	ListNodes() ([]model.Node, error)
	ListDiscoveredNodes() ([]model.Node, error)
	ListRelayRoutes() ([]model.RelayRoute, error)
	SaveRelayRoute(route model.RelayRoute) error
}

// PeerDialer 是反向链路的数据面（由 peer.Hub 实现）；
// via 取 direct（对端直连目标）或 ssh:<nodeID>（经对端的 SSH 客户端转发）。
type PeerDialer interface {
	Dial(ctx context.Context, nodeID, target, via string) (net.Conn, error)
	Online(nodeID string) bool
}

// UpperCoordinator 由 service 注入，负责"目标节点不在本机时，去拓扑里找持有它的上级"。
type UpperCoordinator interface {
	// HolderForTarget 返回持有该目标的上级地址与节点 id
	HolderForTarget(ctx context.Context, target string) (holderAddress, nodeID string, ok bool)
	// EnsureDirect 让持有者通知该节点主动连到本机（按需直连）
	EnsureDirect(ctx context.Context, holderAddress, nodeID string) error
	// DialViaUpper 经持有上级代理拨号（A→B→N）
	DialViaUpper(ctx context.Context, holderAddress, nodeID, target, via string) (net.Conn, error)
	// RelayFallbackEnabled 是否允许走"经上级代理"这条兜底
	RelayFallbackEnabled() bool
}

type Manager struct {
	repo  Repository
	ssh   *sshx.Manager
	peer  PeerDialer
	upper UpperCoordinator
	mu    sync.Mutex
}

func NewManager(repo Repository, sshManager *sshx.Manager) *Manager {
	return &Manager{repo: repo, ssh: sshManager}
}

// SetPeerDialer 注入反向链路数据面；未注入时 peer 节点不可用（退化为 SSH/直连）。
func (m *Manager) SetPeerDialer(dialer PeerDialer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peer = dialer
}

func (m *Manager) peerDialer() PeerDialer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peer
}

// SetUpperCoordinator 注入跨上级协调能力（拓扑找持有者、按需直连、经上级代理）。
func (m *Manager) SetUpperCoordinator(coordinator UpperCoordinator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upper = coordinator
}

func (m *Manager) upperCoordinator() UpperCoordinator {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.upper
}

func targetAddress(target string) (string, int, error) {
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, apperrors.New(apperrors.InvalidArgument, "target must be host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, apperrors.New(apperrors.InvalidArgument, "invalid target port")
	}
	return host, port, nil
}

func targetIP(ctx context.Context, host string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip, nil
	}
	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return netip.Addr{}, apperrors.New(apperrors.TargetForbidden, "cannot resolve target address")
	}
	ip, ok := netip.AddrFromSlice(addresses[0])
	if !ok {
		return netip.Addr{}, apperrors.New(apperrors.TargetForbidden, "invalid target address")
	}
	return ip.Unmap(), nil
}

func subnetContains(subnet string, ip netip.Addr) bool {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		return false
	}
	return prefix.Contains(ip)
}

func isPrivateSubnet(subnet string) bool {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		return false
	}
	return prefix.Addr().IsPrivate() || prefix.Addr().IsLoopback() || prefix.Addr().IsLinkLocalUnicast()
}

func isDirect(ctx context.Context, ip netip.Addr) bool {
	for _, subnet := range discovery.LocalSubnets() {
		if subnetContains(subnet, ip) {
			return true
		}
	}
	return ip.IsLoopback()
}

func (m *Manager) Select(ctx context.Context, target string) (model.RelayRoute, error) {
	host, _, err := targetAddress(target)
	if err != nil {
		return model.RelayRoute{}, err
	}
	ip, err := targetIP(ctx, host)
	if err != nil {
		return model.RelayRoute{}, err
	}
	now := time.Now().UTC()
	if isDirect(ctx, ip) {
		return model.RelayRoute{
			TargetSubnet: ip.String() + "/32", ViaNodeID: "direct",
			Hops: []string{}, Healthy: true, LastChecked: now,
		}, nil
	}

	nodes, err := m.repo.ListNodes()
	if err != nil {
		return model.RelayRoute{}, err
	}
	dialer := m.peerDialer()

	var peerNodes, sshNodes []model.Node
	for _, node := range nodes {
		if !node.Trusted || !node.RelayEnabled {
			continue
		}
		if node.Protocol == "peer" {
			if dialer == nil || !dialer.Online(node.ID) {
				continue
			}
			peerNodes = append(peerNodes, node)
			continue
		}
		if node.Status == "connected" {
			sshNodes = append(sshNodes, node)
		}
	}

	declaredBy := func(subnets []string) (string, bool) {
		for _, subnet := range subnets {
			if isPrivateSubnet(subnet) && subnetContains(subnet, ip) {
				return subnet, true
			}
		}
		return "", false
	}
	route := func(subnet, via string, hops ...string) model.RelayRoute {
		selected := model.RelayRoute{TargetSubnet: subnet, ViaNodeID: via, Hops: hops, Healthy: true, LastChecked: now}
		_ = m.repo.SaveRelayRoute(selected) // 保留选路结果，供 relay.routes 展示
		return selected
	}

	// 1) peer 自己宣告该子网：走反向链路，由 peer 直接拨目标
	for _, node := range peerNodes {
		if subnet, ok := declaredBy(node.ReachableSubnets); ok {
			return route(subnet, node.ID, node.ID), nil
		}
	}
	// 2) 本机已纳管的 SSH 节点宣告该子网：不需要反向链路
	for _, node := range sshNodes {
		if subnet, ok := declaredBy(node.ReachableSubnets); ok {
			return route(subnet, node.ID, node.ID), nil
		}
	}
	// 3) peer 对端的 SSH 跳板宣告该子网：NPA 映射 + SSH 映射串联
	for _, node := range peerNodes {
		for _, remote := range node.PeerRoutes {
			if !remote.Trusted || !remote.RelayEnabled {
				continue
			}
			if subnet, ok := declaredBy(remote.Subnets); ok {
				return route(subnet, node.ID, node.ID, remote.NodeID), nil
			}
		}
	}
	// 4) 本机不持有该节点，但拓扑显示某个上级持有它：
	//    先让持有者通知节点主动连过来（按需直连），连不上就走"经上级代理"兜底
	if coordinator := m.upperCoordinator(); coordinator != nil {
		if holder, nodeID, ok := coordinator.HolderForTarget(ctx, target); ok {
			if err := coordinator.EnsureDirect(ctx, holder, nodeID); err == nil {
				// 节点已连到本机：它会进本机 peer 的连接池，走常规反向链路
				return route(ip.String()+"/32", nodeID, nodeID), nil
			}
			if coordinator.RelayFallbackEnabled() {
				return route(ip.String()+"/32", "upper-relay:"+holder, nodeID), nil
			}
		}
	}
	// 5) 兜底：私网目标但没有宣告匹配的子网，交给在线 peer 试拨（可达性以实际拨号为准）
	if ip.IsPrivate() {
		for _, node := range peerNodes {
			return route(ip.String()+"/32", node.ID, node.ID), nil
		}
	}
	return model.RelayRoute{}, apperrors.New(apperrors.TargetForbidden, ErrNoRoute.Error())
}

// DialRoute 选路并拨号到目标，同时返回所选路径，供需要展示"经由谁"的调用方使用。
func (m *Manager) DialRoute(ctx context.Context, target string) (net.Conn, model.RelayRoute, error) {
	route, err := m.Select(ctx, target)
	if err != nil {
		return nil, route, err
	}
	if route.ViaNodeID == "direct" {
		host, port, err := targetAddress(target)
		if err != nil {
			return nil, route, err
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return nil, route, apperrors.Wrap(apperrors.TargetForbidden, "direct dial failed", err)
		}
		return conn, route, nil
	}
	conn, err := m.dialHops(ctx, route, target)
	return conn, route, err
}

// dialHops 按跳数依次执行：首跳是 peer 节点就走反向链路，
// 第二跳若是对端 SSH 节点，则用 via=ssh:<nodeID> 让对端再经 SSH 转发（NPA 映射 + SSH 映射）。
func (m *Manager) dialHops(ctx context.Context, route model.RelayRoute, target string) (net.Conn, error) {
	if len(route.Hops) == 0 {
		return nil, apperrors.New(apperrors.TargetForbidden, ErrNoRoute.Error())
	}
	// 经持有上级代理：A → B → N
	if strings.HasPrefix(route.ViaNodeID, "upper-relay:") {
		coordinator := m.upperCoordinator()
		if coordinator == nil {
			return nil, apperrors.New(apperrors.NotSupported, "跨上级代理未启用")
		}
		return coordinator.DialViaUpper(ctx, strings.TrimPrefix(route.ViaNodeID, "upper-relay:"), route.Hops[0], target, "")
	}
	entry := route.Hops[0]
	if !m.isPeerNode(entry) {
		return m.dialNode(ctx, entry, target)
	}
	dialer := m.peerDialer()
	if dialer == nil {
		return nil, apperrors.New(apperrors.NotSupported, "反向链路未启用")
	}
	via := "direct"
	if len(route.Hops) >= 2 {
		via = "ssh:" + route.Hops[1]
	}
	return dialer.Dial(ctx, entry, target, via)
}

func (m *Manager) isPeerNode(nodeID string) bool {
	if node, err := m.repo.GetNode(nodeID); err == nil && node.Protocol == "peer" {
		return true
	}
	// 本机没有该节点的记录，但它已按需连过来（临时链路）——同样走反向数据面
	dialer := m.peerDialer()
	return dialer != nil && dialer.Online(nodeID)
}

// DialNode 经指定节点转发到目标：peer 节点走反向链路，其余走 SSH direct-tcpip。
// 返回值携带所经节点，供端口映射等调用方展示"经由谁"。
func (m *Manager) DialNode(ctx context.Context, nodeID, target string) (net.Conn, model.RelayRoute, error) {
	route := model.RelayRoute{ViaNodeID: nodeID, Hops: []string{nodeID}, Healthy: true, LastChecked: time.Now().UTC()}
	if m.isPeerNode(nodeID) {
		dialer := m.peerDialer()
		if dialer == nil {
			return nil, route, apperrors.New(apperrors.NotSupported, "反向链路未启用")
		}
		conn, err := dialer.Dial(ctx, nodeID, target, "direct")
		if err != nil {
			return nil, route, err
		}
		return conn, route, nil
	}
	conn, err := m.dialNode(ctx, nodeID, target)
	if err != nil {
		return nil, route, err
	}
	return conn, route, nil
}

func (m *Manager) dialNode(ctx context.Context, nodeID, target string) (net.Conn, error) {
	if m.isPeerNode(nodeID) {
		dialer := m.peerDialer()
		if dialer == nil {
			return nil, apperrors.New(apperrors.NotSupported, "反向链路未启用")
		}
		return dialer.Dial(ctx, nodeID, target, "direct")
	}
	nodes, err := m.repo.ListNodes()
	if err != nil {
		return nil, err
	}
	var selected *model.Node
	for i := range nodes {
		if nodes[i].ID == nodeID {
			selected = &nodes[i]
			break
		}
	}
	if selected == nil {
		return nil, apperrors.New(apperrors.InvalidArgument, "relay node is not managed: "+nodeID)
	}
	if !selected.Trusted || !selected.RelayEnabled {
		return nil, apperrors.New(apperrors.TargetForbidden, "relay node is not authorized: "+nodeID)
	}
	client, err := m.ssh.Client(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	conn, err := client.Dial("tcp", target)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.TargetForbidden, "relay dial failed", err)
	}
	return conn, nil
}

func (m *Manager) Dial(ctx context.Context, target string) (net.Conn, error) {
	conn, _, err := m.DialRoute(ctx, target)
	return conn, err
}

func (m *Manager) Routes() ([]model.RelayRoute, error) {
	return m.repo.ListRelayRoutes()
}

// relayTargetPrefix 判断该前缀是否可能作为中继目标：
// 回环（127/8、::1）与链路本地（fe80::/10、169.254/16）永远不是跨设备转发对象，
// 展示与 MCP 都应过滤，避免把本机地址误读成"节点 IP"。
func relayTargetPrefix(prefix string) bool {
	parsed, err := netip.ParsePrefix(prefix)
	if err != nil {
		return false
	}
	addr := parsed.Addr()
	return !addr.IsLoopback() && !addr.IsLinkLocalUnicast() && !addr.IsLinkLocalMulticast()
}

func (m *Manager) Subnets(ctx context.Context) ([]model.SubnetInfo, error) {
	result := []model.SubnetInfo{}
	for _, subnet := range discovery.LocalSubnets() {
		if !relayTargetPrefix(subnet) {
			continue
		}
		result = append(result, model.SubnetInfo{Source: "local", Prefix: subnet})
	}
	managed, err := m.repo.ListNodes()
	if err != nil {
		return nil, err
	}
	for _, node := range managed {
		for _, subnet := range node.ReachableSubnets {
			if isPrivateSubnet(subnet) && relayTargetPrefix(subnet) {
				result = append(result, model.SubnetInfo{Source: "managed", NodeID: node.ID, Prefix: subnet})
			}
		}
	}
	nodes, err := m.repo.ListDiscoveredNodes()
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		for _, subnet := range node.ReachableSubnets {
			if isPrivateSubnet(subnet) && relayTargetPrefix(subnet) {
				result = append(result, model.SubnetInfo{Source: "discovered", NodeID: node.ID, Prefix: subnet})
			}
		}
	}
	return result, nil
}

func FormatTarget(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

func ValidateSubnet(value string) error {
	if _, err := netip.ParsePrefix(value); err != nil {
		return apperrors.New(apperrors.InvalidArgument, fmt.Sprintf("invalid subnet: %s", strings.TrimSpace(value)))
	}
	return nil
}
