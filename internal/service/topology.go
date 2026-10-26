package service

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/peer"
)

const (
	topologyCacheTTL = 15 * time.Second
	topologyTimeout  = 2 * time.Second
	// maxTopologyDepth 向上递归的层数，防止环
	maxTopologyDepth = 4
	// maxUpperCandidates 单次最多向多少个上级查询，避免广播风暴
	maxUpperCandidates = 8
)

type topologyCacheEntry struct {
	at      time.Time
	payload map[string]any
}

// Topology 返回完整树状拓扑：本机、本机的下级、本机的上级链，
// 以及其它已知上级（mDNS 发现的 / peer_uppers）的下级。
//
// 返回扁平结构 {self_id, nodes:[{...,"parent":上级id}], missing:[{address,error}]}，
// 由前端按 parent 组树；拉取失败的分支放进 missing，显示为"未知"而不是静默丢弃。
func (s *Service) Topology(ctx context.Context) (map[string]any, error) {
	s.mu.Lock()
	if entry := s.topologyCache; entry != nil && time.Since(entry.at) < topologyCacheTTL {
		payload := entry.payload
		s.mu.Unlock()
		return payload, nil
	}
	s.mu.Unlock()

	self := s.topologySelf()
	nodes := []peer.TopologyNode{self}
	missing := []map[string]string{}
	seen := map[string]bool{self.ID: true}

	// 1) 本机直接持有的下级
	for _, child := range s.registeredChildren() {
		nodes = append(nodes, peer.TopologyNode{
			ID: child.ID, Name: child.Name, Address: child.Address, Port: child.Port,
			Version: child.Version, Protocol: child.Protocol, Registered: true,
			Online: s.probeNode(child), Parent: self.ID,
		})
		seen[child.ID] = true
	}

	// 2) 本机的上级链：从 agent.primary 向上走，逐层拉 children
	parentAddress := strings.TrimSpace(s.PeerAgent.Status().Primary)
	for depth := 0; depth < maxTopologyDepth && parentAddress != "" && !seen["@"+parentAddress]; depth++ {
		seen["@"+parentAddress] = true
		topology, err := s.fetchTopology(ctx, parentAddress)
		if err != nil {
			missing = append(missing, map[string]string{"address": parentAddress, "error": err.Error()})
			break
		}
		upperID := topology.Self.ID
		if upperID == "" {
			upperID = parentAddress
		}
		parentOfUpper := ""
		if topology.Parent != nil {
			parentOfUpper = topology.Parent.ID
			parentAddress = topology.Parent.Address
			if parentOfUpper == "" {
				break
			}
		} else {
			parentAddress = ""
		}
		nodes = append(nodes, withParent(topology.Self, parentOfUpper, upperID, seen))
		for _, child := range topology.Children {
			nodes = append(nodes, withParent(child, upperID, child.ID, seen))
		}
	}

	// 3) 其它已知上级：mDNS 发现的 NPA + peer_uppers 配置
	for _, address := range s.candidateUppers() {
		if len(nodes) > 200 {
			break
		}
		key := "@" + address
		if seen[key] {
			continue
		}
		seen[key] = true
		topology, err := s.fetchTopology(ctx, address)
		if err != nil {
			missing = append(missing, map[string]string{"address": address, "error": err.Error()})
			continue
		}
		upperID := topology.Self.ID
		if upperID == "" {
			upperID = address
		}
		nodes = append(nodes, withParent(topology.Self, "", upperID, seen))
		for _, child := range topology.Children {
			nodes = append(nodes, withParent(child, upperID, child.ID, seen))
		}
	}

	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	payload := map[string]any{
		"self_id":      self.ID,
		"nodes":        nodes,
		"missing":      missing,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	s.mu.Lock()
	s.topologyCache = &topologyCacheEntry{at: time.Now(), payload: payload}
	s.mu.Unlock()
	return payload, nil
}

// withParent 去重后补齐 parent，避免同一节点被多个上级重复列出。
func withParent(node peer.TopologyNode, parent, id string, seen map[string]bool) peer.TopologyNode {
	if id != "" && seen[id] {
		node.Unknown = true
		node.Error = "该节点已在树中出现，未重复展开"
	}
	if id != "" {
		seen[id] = true
	}
	node.Parent = parent
	return node
}

// fetchTopology 向指定上级查询它的拓扑（只查一层）。
func (s *Service) fetchTopology(ctx context.Context, address string) (peer.Topology, error) {
	var topology peer.Topology
	target := address
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, strconv.Itoa(meshPort))
	}
	ctx, cancel := context.WithTimeout(ctx, topologyTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+target+"/api/topology", nil)
	if err != nil {
		return topology, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return topology, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return topology, apperrors.New(apperrors.NodeOffline, "拓扑查询返回 "+strconv.Itoa(response.StatusCode))
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&topology); err != nil {
		return topology, err
	}
	return topology, nil
}

// topologySelf 描述本机在拓扑中的样子。
func (s *Service) topologySelf() peer.TopologyNode {
	local := localNode()
	return peer.TopologyNode{
		ID: local.ID, Name: local.Name, Address: local.Address, Port: 56700,
		Version: local.Version, Protocol: "local", Online: true,
	}
}

// registeredChildren 是本机持有的反向接入下级。
func (s *Service) registeredChildren() []model.Node {
	stored, err := s.Store.ListNodes()
	if err != nil {
		return nil
	}
	result := []model.Node{}
	for _, node := range stored {
		if node.Protocol == "peer" && node.Registered {
			result = append(result, node)
		}
	}
	return result
}

// candidateUppers 是本机可以查询拓扑的其它上级：配置的 peer_uppers + mDNS 发现的 NPA。
func (s *Service) candidateUppers() []string {
	seen := map[string]bool{}
	result := []string{}
	add := func(address string) {
		address = strings.TrimSpace(address)
		if address == "" || seen[address] {
			return
		}
		seen[address] = true
		result = append(result, address)
	}
	for _, address := range peer.ParseSeeds(s.Settings()["peer_uppers"], meshPort) {
		add(peer.NormalizeAddress(address))
	}
	discovered, err := s.Store.ListDiscoveredNodes()
	if err != nil {
		return result
	}
	for _, node := range discovered {
		if !node.Discovered || node.Address == "" {
			continue
		}
		// 旧版本节点自报 56710：端口收敛后指向同一个服务，归一到 56701
		port := peer.NormalizePort(node.PeerPort)
		if port <= 0 {
			port = meshPort
		}
		add(net.JoinHostPort(node.Address, strconv.Itoa(port)))
	}
	if len(result) > maxUpperCandidates {
		result = result[:maxUpperCandidates]
	}
	return result
}
