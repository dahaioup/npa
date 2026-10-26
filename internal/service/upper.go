package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/npa/npa/internal/discovery"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/peer"
)

const (
	requireTimeout   = 10 * time.Second
	ephemeralWaitFor = 8 * time.Second
)

// HolderForTarget 在完整拓扑里找"持有能到达该目标的节点"的上级。
// 返回（上级地址, 节点 id）；本机自己持有的情况返回 ok=false（前面的选路级别已覆盖）。
func (s *Service) HolderForTarget(ctx context.Context, target string) (string, string, bool) {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return "", "", false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", "", false
	}
	payload, err := s.Topology(ctx)
	if err != nil {
		return "", "", false
	}
	nodes, ok := payload["nodes"].([]peer.TopologyNode)
	if !ok {
		return "", "", false
	}
	selfID, _ := payload["self_id"].(string)
	byID := map[string]peer.TopologyNode{}
	for _, node := range nodes {
		byID[node.ID] = node
	}
	for _, node := range nodes {
		if node.Protocol == "local" || node.ID == selfID {
			continue
		}
		// 本机自己持有的下级：父节点就是本机
		if node.Parent == selfID {
			continue
		}
		holds := false
		for _, subnet := range node.Subnets {
			if prefix, err := netip.ParsePrefix(subnet); err == nil && prefix.Contains(ip) {
				holds = true
				break
			}
		}
		if !holds || node.Parent == "" {
			continue
		}
		holder, exists := byID[node.Parent]
		if !exists || holder.Address == "" {
			continue
		}
		address := holder.Address
		if _, _, splitErr := net.SplitHostPort(address); splitErr != nil {
			address = net.JoinHostPort(address, strconv.Itoa(meshPort))
		}
		return address, node.ID, true
	}
	return "", "", false
}

// HolderForNode 在完整拓扑里按**节点 ID**找持有它的上级（跨域推送/执行用：
// 目标不是"某个 IP"，而是拓扑里的另一台 NPA 节点）。
func (s *Service) HolderForNode(ctx context.Context, nodeID string) (string, bool) {
	payload, err := s.Topology(ctx)
	if err != nil {
		return "", false
	}
	nodes, ok := payload["nodes"].([]peer.TopologyNode)
	if !ok {
		return "", false
	}
	selfID, _ := payload["self_id"].(string)
	byID := map[string]peer.TopologyNode{}
	for _, node := range nodes {
		byID[node.ID] = node
	}
	node, exists := byID[nodeID]
	if !exists || node.Protocol == "local" || node.Parent == "" || node.Parent == selfID {
		return "", false
	}
	holder, ok := byID[node.Parent]
	if !ok || holder.Address == "" {
		return "", false
	}
	address := holder.Address
	if _, _, splitErr := net.SplitHostPort(address); splitErr != nil {
		address = net.JoinHostPort(address, strconv.Itoa(meshPort))
	}
	return address, true
}

// EnsureDirect 让持有者通知它的下级主动连到本机，并等待临时链路建立。
func (s *Service) EnsureDirect(ctx context.Context, holderAddress, nodeID string) error {
	backTo := s.advertisedAddress()
	payload, _ := json.Marshal(map[string]string{"node_id": nodeID, "backto": backTo})
	ctx, cancel := context.WithTimeout(ctx, requireTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+holderAddress+"/api/peers/require", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return apperrors.Wrap(apperrors.NodeOffline, "联系持有上级失败", err)
	}
	defer response.Body.Close()
	var reply struct {
		Accepted bool   `json:"accepted"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
		return err
	}
	if !reply.Accepted {
		return apperrors.New(apperrors.NodeOffline, "持有上级未能让节点直连本机："+reply.Reason)
	}
	deadline := time.Now().Add(ephemeralWaitFor)
	for time.Now().Before(deadline) {
		if s.Peer.EphemeralReady(nodeID) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return apperrors.New(apperrors.NodeOffline, "节点未在超时内连到本机")
}

// DialViaUpper 经持有上级代理拨号（A→B→N）：先请持有方建代理通道，再从通道取连接转发。
func (s *Service) DialViaUpper(ctx context.Context, holderAddress, nodeID, target, via string) (net.Conn, error) {
	if err := s.requireRelayChannel(ctx, holderAddress, nodeID); err != nil {
		return nil, err
	}
	return s.Peer.DialUpper(ctx, holderAddress, nodeID, target, via)
}

// requireRelayChannel 请求持有方把该节点经它代理给本机。
func (s *Service) requireRelayChannel(ctx context.Context, holderAddress, nodeID string) error {
	payload, _ := json.Marshal(map[string]string{"node_id": nodeID, "backto": s.advertisedAddress()})
	ctx, cancel := context.WithTimeout(ctx, requireTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+holderAddress+"/api/peers/require_relay", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return apperrors.Wrap(apperrors.NodeOffline, "联系持有上级失败", err)
	}
	defer response.Body.Close()
	var reply struct {
		Accepted bool   `json:"accepted"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
		return err
	}
	if !reply.Accepted {
		return apperrors.New(apperrors.NodeOffline, "持有上级未提供代理："+reply.Reason)
	}
	return nil
}

// advertisedAddress 是本机在协调时对外通告的地址：节点要按它主动连过来。
// 默认取 LAN IP + 反向接入端口；多网卡或 NAT 时用设置项 peer_advertise 显式指定。
func (s *Service) advertisedAddress() string {
	port := meshPort
	if value := strings.TrimSpace(s.Settings()["peer_advertise"]); value != "" {
		if _, _, err := net.SplitHostPort(value); err == nil {
			return value
		}
		return net.JoinHostPort(value, strconv.Itoa(port))
	}
	return net.JoinHostPort(discovery.PrimaryIP(), strconv.Itoa(port))
}

// RelayFallbackEnabled 是否允许"直连失败后经上级代理"这条兜底。
func (s *Service) RelayFallbackEnabled() bool { return parseBool(s.Settings()["peer_relay_fallback"]) }
