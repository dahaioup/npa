package service

import (
	"context"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/peer"
)

// 拓扑（新版，docs/plans/topology-sync.md）：
// 视图（peer.TopologyView）是唯一数据源——注册/心跳/增量同步实时维护，
// 这里只做投影，不再向上递归拉取（旧 buildTopology/fetchTopology 已删除）。

// Topology 返回扁平拓扑 {self_id, nodes:[...], missing:[], generated_at}，
// 供 Web 端按 parent/holder 渲染。节点集合 = 本机 + 视图（注册下级、
// 域成员、下级域转述节点）。
func (s *Service) Topology(_ context.Context) (map[string]any, error) {
	self := s.topologySelf()
	nodes := []peer.TopoNode{{
		ID: self.ID, Name: self.Name, Address: self.Address,
		Version: self.Version, Source: "self", Online: true,
		NodeVersion: self.NodeVersion,
	}}
	if view := s.Peer.View(); view != nil {
		selfParent := ""
		for _, n := range view.Snapshot() {
			if n.ID == self.ID {
				// 视图里的本机记录带权威父指针（本机挂在哪），
				// 成员的父指针继承自它——投影必须透传，否则本机屏幕上的
				// 自己与成员对不齐。
				selfParent = n.Parent
				continue // 本机用上面的记录，跳过视图里的重复
			}
			nodes = append(nodes, n)
		}
		nodes[0].Parent = selfParent
		// 视图成员记录（心跳自报）只有 id/addr，名字/版本从 mDNS 发现记录补齐
		if found, err := s.Discovery.Nodes(); err == nil {
			byID := map[string]model.Node{}
			for _, n := range found {
				byID[n.ID] = n
			}
			for i := range nodes {
				discovered, ok := byID[nodes[i].ID]
				if !ok {
					continue
				}
				if nodes[i].Name == "" && discovered.Name != "" {
					nodes[i].Name = discovered.Name
				}
				if nodes[i].NodeVersion == "" && discovered.Version != "" {
					nodes[i].NodeVersion = discovered.Version
				}
				nodes[i].Discovered = true // mDNS 可见 = 同域成员
			}
		}
	}
	// store 里注册过但还没有活跃链路的下级也要出现（offline）——
	// 视图只收活跃注册，历史注册记录在 nodes 页仍可见。
	inView := map[string]bool{self.ID: true}
	for _, node := range nodes {
		inView[node.ID] = true
	}
	for _, child := range s.registeredChildren() {
		if inView[child.ID] {
			continue
		}
		nodes = append(nodes, peer.TopoNode{
			ID: child.ID, Name: child.Name, Address: child.Address,
			Parent: self.ID, Source: "hub", Online: false,
			NodeVersion: child.Version,
		})
	}
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	payload := map[string]any{
		"self_id":      self.ID,
		"nodes":        nodes,
		"missing":      []map[string]string{},
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	return payload, nil
}

// topologySelf 描述本机在拓扑中的样子。
func (s *Service) topologySelf() peer.TopoNode {
	local := localNode()
	// 名字用主机名，别叫「本地节点」：拓扑合并后每个节点都在描述"自己"，
	// 全叫本地节点会让列表里四台机器无法分辨。
	name := local.Name
	host, _ := os.Hostname()
	if host != "" && host != "localhost" {
		name = host
	} else if user := os.Getenv("USER"); user != "" {
		name = "npa (" + user + ")"
	} else if user := os.Getenv("USERNAME"); user != "" {
		name = "npa (" + user + ")"
	}
	self := peer.TopoNode{
		ID: local.ID, Name: name, Address: local.Address,
		Version: 0, Source: "self", Online: true,
		NodeVersion: Version,
	}
	if s.Domain != nil {
		state := s.Domain.Manager.State()
		self.DomainID = state.Domain
	}
	return self
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

// upstreamTopologyTargets 是本机拓扑同步的「上级」候选（跨域）：
//
//	peer_uppers 配置 + 注册到的上级（agent Primary）+ 路由探测自动发现的上级。
//
// mDNS 发现的同域节点不在此列——同域的拓扑表只由该域管理节点维护，
// 同域成员之间不互推（互相推送会把归属改写成环）。
func (s *Service) upstreamTopologyTargets() []string {
	sameDomain := map[string]bool{}
	if discovered, err := s.Store.ListDiscoveredNodes(); err == nil {
		for _, node := range discovered {
			if !node.Discovered || node.Address == "" {
				continue
			}
			port := peer.NormalizePort(node.PeerPort)
			if port <= 0 {
				port = meshPort
			}
			sameDomain[net.JoinHostPort(node.Address, strconv.Itoa(port))] = true
		}
	}
	seen := map[string]bool{}
	result := []string{}
	add := func(address string) {
		address = strings.TrimSpace(address)
		if address == "" || seen[address] || sameDomain[address] {
			return
		}
		seen[address] = true
		result = append(result, address)
	}
	for _, address := range peer.ParseSeeds(s.Settings()["peer_uppers"], meshPort) {
		add(peer.NormalizeAddress(address))
	}
	// 注册到的上级（本机作为下级挂上去的那台）——跨域时它就是上级域入口
	if s.PeerAgent != nil {
		if st := s.PeerAgent.Status(); st.Primary != "" {
			add(peer.NormalizeAddress(st.Primary))
		}
	}
	for _, candidate := range s.autoUpperCandidates() {
		if candidate.Address != "" {
			add(candidate.Address)
		}
	}
	const maxCandidates = 8
	if len(result) > maxCandidates {
		result = result[:maxCandidates]
	}
	return result
}

// sameDomainTarget 判断目标是否本域 mDNS 可见：可见=同域（成员与管理节点
// 平级），不可见（seeds/跨网段/重定向到域外）=跨域（挂靠到管理节点名下）。
func (s *Service) sameDomainTarget(addr string) bool {
	if s.Discovery == nil || addr == "" {
		return false
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	nodes, err := s.Discovery.Nodes()
	if err != nil {
		return false
	}
	for _, n := range nodes {
		nh := n.Address
		if h, _, err := net.SplitHostPort(n.Address); err == nil {
			nh = h
		}
		if nh == host {
			return true
		}
	}
	return false
}

// candidateUppers 是本机可以推送/拉取拓扑的其它管理者：配置的 peer_uppers + mDNS 发现的 NPA。
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
	// 路由探测自动发现的上级（peer_auto_discover_uppers，默认开）
	for _, candidate := range s.autoUpperCandidates() {
		if candidate.Address != "" {
			add(candidate.Address)
		}
	}
	const maxCandidates = 8
	if len(result) > maxCandidates {
		result = result[:maxCandidates]
	}
	return result
}
