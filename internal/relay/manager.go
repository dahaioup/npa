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
	ListNodes() ([]model.Node, error)
	ListDiscoveredNodes() ([]model.Node, error)
	ListRelayRoutes() ([]model.RelayRoute, error)
	SaveRelayRoute(route model.RelayRoute) error
}

type Manager struct {
	repo Repository
	ssh  *sshx.Manager
	mu   sync.Mutex
}

func NewManager(repo Repository, sshManager *sshx.Manager) *Manager {
	return &Manager{repo: repo, ssh: sshManager}
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
	if isDirect(ctx, ip) {
		return model.RelayRoute{TargetSubnet: ip.String() + "/32", ViaNodeID: "direct", Hops: []string{}, Healthy: true, LastChecked: time.Now().UTC()}, nil
	}
	nodes, err := m.repo.ListNodes()
	if err != nil {
		return model.RelayRoute{}, err
	}
	var selected *model.RelayRoute
	for _, node := range nodes {
		if !node.Trusted || !node.RelayEnabled || node.Status != "connected" {
			continue
		}
		for _, subnet := range node.ReachableSubnets {
			if !isPrivateSubnet(subnet) {
				continue
			}
			if !subnetContains(subnet, ip) {
				continue
			}
			route := model.RelayRoute{
				TargetSubnet: subnet, ViaNodeID: node.ID, Hops: []string{node.ID},
				Healthy: true, LastChecked: time.Now().UTC(),
			}
			if selected == nil || len(route.Hops) < len(selected.Hops) {
				candidate := route
				selected = &candidate
			}
		}
	}
	if selected == nil {
		return model.RelayRoute{}, apperrors.New(apperrors.TargetForbidden, ErrNoRoute.Error())
	}
	if err := m.repo.SaveRelayRoute(*selected); err != nil {
		return *selected, err
	}
	return *selected, nil
}

func (m *Manager) Dial(ctx context.Context, target string) (net.Conn, error) {
	route, err := m.Select(ctx, target)
	if err != nil {
		return nil, err
	}
	if route.ViaNodeID == "direct" {
		host, port, err := targetAddress(target)
		if err != nil {
			return nil, err
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	}
	client, err := m.ssh.Client(ctx, route.ViaNodeID)
	if err != nil {
		return nil, err
	}
	conn, err := client.Dial("tcp", target)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.TargetForbidden, "relay dial failed", err)
	}
	return conn, nil
}

func (m *Manager) Routes() ([]model.RelayRoute, error) {
	return m.repo.ListRelayRoutes()
}

func (m *Manager) Subnets(ctx context.Context) ([]model.SubnetInfo, error) {
	result := []model.SubnetInfo{}
	for _, subnet := range discovery.LocalSubnets() {
		result = append(result, model.SubnetInfo{Source: "local", Prefix: subnet})
	}
	managed, err := m.repo.ListNodes()
	if err != nil {
		return nil, err
	}
	for _, node := range managed {
		for _, subnet := range node.ReachableSubnets {
			if isPrivateSubnet(subnet) {
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
			if isPrivateSubnet(subnet) {
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
