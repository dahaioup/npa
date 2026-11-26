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

// HolderForTarget 在拓扑视图里找"持有能到达该目标的节点"的跳板。
// 返回（持有 hub 地址, 节点 id）；本机自己持有的情况返回 ok=false（前面的选路级别已覆盖）。
func (s *Service) HolderForTarget(_ context.Context, target string) (string, string, bool) {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return "", "", false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", "", false
	}
	view := s.Peer.View()
	if view == nil {
		return "", "", false
	}
	selfID := view.SelfID()
	nodes := view.Snapshot()
	// 先按"目标 IP 就是该节点的地址"匹配：跨域节点的子网宣告常常缺失
	// （subnets 为空），只靠子网匹配会找不到持有者。
	for _, node := range nodes {
		if node.Source == "self" || node.ID == selfID {
			continue
		}
		if nodeHostIP(node.Address) != ip.String() {
			continue
		}
		if holder, ok := s.holderAddressFor(view, nodes, node); ok {
			return holder, node.ID, true
		}
	}
	for _, node := range nodes {
		if node.Source == "self" || node.ID == selfID {
			continue
		}
		// 本机自己持有的下级（holder=自己）：前面的选路级别已覆盖
		if node.Holder == "" || sameHostPort(node.Holder, view.SelfAddress()) {
			continue
		}
		holds := false
		for _, subnet := range node.Subnets {
			if prefix, err := netip.ParsePrefix(subnet); err == nil && prefix.Contains(ip) {
				holds = true
				break
			}
		}
		if !holds {
			continue
		}
		if holder, ok := s.holderAddressFor(view, nodes, node); ok {
			return holder, node.ID, true
		}
	}
	return "", "", false
}

// holderAddressFor 找真正持有该节点的 hub 地址：优先用父节点（拓扑里的 parent
// 指向持有它的管理节点），父节点缺失或就是本机时退回节点自带的 Holder 字段，
// 再退回本机注册到的上级 hub。
func (s *Service) holderAddressFor(view *peer.TopologyView, nodes []peer.TopoNode, node peer.TopoNode) (string, bool) {
	if node.Parent != "" && node.Parent != view.SelfID() {
		for _, candidate := range nodes {
			if candidate.ID != node.Parent {
				continue
			}
			if addr := normalizeMeshAddress(candidate.Address); addr != "" {
				return addr, true
			}
		}
	}
	if node.Holder != "" && !sameHostPort(node.Holder, view.SelfAddress()) {
		return normalizeMeshAddress(node.Holder), true
	}
	if primary := s.PrimaryHolder(); primary != "" {
		return normalizeMeshAddress(primary), true
	}
	return "", false
}

// PrimaryHolder 返回本机 agent 当前注册到的上级 hub 地址。
// 上级 hub 持有它的下级节点；拓扑里的 Holder 可能是节点的 NAT 映射地址
// （本机不可达），这时由上级 hub 代理拨号才能打通。
func (s *Service) PrimaryHolder() string {
	if s.PeerAgent == nil {
		return ""
	}
	return s.PeerAgent.Status().Primary
}

// nodeHostIP 取节点地址里的主机部分（无端口时原样返回）。
func nodeHostIP(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

// HoldersForNode 按**节点 ID**收集持有它的跳板地址（视图里同一节点只保留一行，
// 但为兼容残留注册，仍按 holder 去重后返回）。
func (s *Service) HoldersForNode(_ context.Context, nodeID string) []string {
	view := s.Peer.View()
	if view == nil {
		return nil
	}
	selfID := view.SelfID()
	selfAddr := view.SelfAddress()
	nodes := view.Snapshot()
	var addresses []string
	seen := map[string]bool{}
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || seen[raw] {
			return
		}
		seen[raw] = true
		addresses = append(addresses, normalizeMeshAddress(raw))
	}
	for _, node := range nodes {
		if node.ID != nodeID || node.Source == "self" {
			continue
		}
		// 真正持有该节点的是它的父节点（拓扑里的 parent 指向持有它的管理节点）；
		// 节点自带的 Holder 可能是 NAT 映射地址（本机连不上），放在后面兜底。
		if node.Parent != "" && node.Parent != selfID {
			for _, candidate := range nodes {
				if candidate.ID != node.Parent {
					continue
				}
				if addr := strings.TrimSpace(candidate.Address); addr != "" && !sameHostPort(addr, selfAddr) {
					add(addr)
				}
			}
		}
		if node.Holder != "" && !sameHostPort(node.Holder, selfAddr) {
			add(node.Holder)
		}
	}
	if len(addresses) == 0 {
		// 拓扑里查不到持有者：本机注册到的上级 hub 持有它的下级节点。
		if primary := s.PrimaryHolder(); primary != "" && !sameHostPort(primary, selfAddr) {
			add(primary)
		}
	}
	return addresses
}

// sameHostPort 比较两个地址的 host:port（允许缺端口缺省 meshPort）。
func sameHostPort(a, b string) bool {
	return normalizeMeshAddress(a) == normalizeMeshAddress(b)
}

// normalizeMeshAddress 补全缺省端口并归一格式。
func normalizeMeshAddress(address string) string {
	address = strings.TrimSpace(address)
	if _, _, err := net.SplitHostPort(address); err != nil {
		return net.JoinHostPort(address, strconv.Itoa(meshPort))
	}
	return address
}

// HolderForNode 返回最优候选（第一个），语义保持向后兼容。
func (s *Service) HolderForNode(ctx context.Context, nodeID string) (string, bool) {
	holders := s.HoldersForNode(ctx, nodeID)
	if len(holders) == 0 {
		return "", false
	}
	return holders[0], true
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
