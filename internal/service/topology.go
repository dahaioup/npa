package service

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/peer"
)

const (
	topologyCacheTTL = 15 * time.Second
	// topologyTimeout 单次向对端取拓扑的上限：对端现在是"先给缓存、后台刷新"，
	// 正常响应在毫秒级，这里只兜住对端卡死的情况。
	topologyTimeout = 800 * time.Millisecond
	// topologyBuildBudget 冷启动拼整棵树的预算。拓扑是一层一层问上去的，
	// 串行叠加会让上层超时（界面就显示"拓扑不可达"），所以整树必须有硬预算。
	topologyBuildBudget = 3 * time.Second
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
	entry := s.topologyCache
	s.mu.Unlock()
	if entry != nil {
		if time.Since(entry.at) >= topologyCacheTTL {
			// 先给旧值、后台刷新。拓扑要逐级向上问，同步聚合会把延迟叠加给上一层，
			// 上层一旦超时就把它显示成"拓扑不可达"。
			s.refreshTopologyAsync()
		}
		return entry.payload, nil
	}
	// 冷启动：并发查询，整树给一个硬预算，超时也返回已经拼出来的部分。
	buildCtx, cancel := context.WithTimeout(ctx, topologyBuildBudget)
	defer cancel()
	return s.buildTopology(buildCtx)
}

// refreshTopologyAsync 后台刷新拓扑缓存，同一时刻只允许一个刷新在跑。
func (s *Service) refreshTopologyAsync() {
	s.mu.Lock()
	if s.topologyRefreshing {
		s.mu.Unlock()
		return
	}
	s.topologyRefreshing = true
	s.mu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// 后台刷新 panic 不能带走整个进程：保留上一份缓存继续服务。
				slog.Error("topology refresh panic", "panic", r, "stack", string(debug.Stack()))
			}
			s.mu.Lock()
			s.topologyRefreshing = false
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), topologyBuildBudget)
		defer cancel()
		_, _ = s.buildTopology(ctx)
	}()
}

// buildTopology 真正拼拓扑：本机、下级、上级链、其它已知上级。结果写入缓存。
func (s *Service) buildTopology(ctx context.Context) (map[string]any, error) {
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
		if topology.Self.ID != "" {
			nodes = append(nodes, withParent(topology.Self, parentOfUpper, upperID, seen))
		}
		for _, child := range topology.Children {
			nodes = append(nodes, withParent(child, upperID, child.ID, seen))
		}
	}

	// 3) 其它已知上级：mDNS 发现的 NPA + peer_uppers 配置。
	// 并发查询，逐台串行会被不可达的节点拖满预算。
	candidates := make([]string, 0, maxUpperCandidates)
	for _, address := range s.candidateUppers() {
		if len(candidates) >= maxUpperCandidates || len(nodes) > 200 {
			break
		}
		key := "@" + address
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, address)
	}
	type candidateResult struct {
		address  string
		topology peer.Topology
		err      error
	}
	results := make([]candidateResult, len(candidates))
	var wg sync.WaitGroup
	for i, address := range candidates {
		wg.Add(1)
		go func(i int, address string) {
			defer wg.Done()
			topology, err := s.fetchTopology(ctx, address)
			results[i] = candidateResult{address: address, topology: topology, err: err}
		}(i, address)
	}
	wg.Wait()
	for _, result := range results {
		if result.err != nil {
			missing = append(missing, map[string]string{"address": result.address, "error": result.err.Error()})
			continue
		}
		upperID := result.topology.Self.ID
		if upperID == "" {
			upperID = result.address
		}
		if result.topology.Self.ID != "" {
			nodes = append(nodes, withParent(result.topology.Self, "", upperID, seen))
		}
		for _, child := range result.topology.Children {
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
	// 对端 /api/topology 返回的是扁平结构 {self_id, nodes:[{...,parent}], missing}，
	// 不是 peer.Topology 的嵌套形状；直接按嵌套结构解码会得到一个空节点，
	// 界面上就多出「无名节点」并把上级树整段丢掉。这里按 parent 关系还原。
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return topology, err
	}
	// 两种形状都要认：
	//   peer 面（hub.HandleTopology）返回嵌套的 {self, parent, children}；
	//   Web 面返回扁平的 {self_id, nodes:[{...,parent}], missing}。
	// 同一个地址 56701 上挂哪个取决于 mux 注册顺序，所以按内容判断。
	if err := json.Unmarshal(raw, &topology); err == nil && topology.Self.ID != "" {
		return topology, nil
	}
	var payload struct {
		SelfID string              `json:"self_id"`
		Nodes  []peer.TopologyNode `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return topology, err
	}
	byID := make(map[string]peer.TopologyNode, len(payload.Nodes))
	for _, node := range payload.Nodes {
		if node.ID != "" {
			byID[node.ID] = node
		}
	}
	self, ok := byID[payload.SelfID]
	if !ok {
		return topology, nil
	}
	topology.Self = self
	for _, node := range payload.Nodes {
		if node.ID == "" || node.ID == payload.SelfID || node.Parent != payload.SelfID {
			continue
		}
		topology.Children = append(topology.Children, node)
	}
	if self.Parent != "" {
		if parent, found := byID[self.Parent]; found {
			topology.Parent = &parent
		}
	}
	return topology, nil
}

// topologySelf 描述本机在拓扑中的样子。
func (s *Service) topologySelf() peer.TopologyNode {
	local := localNode()
	self := peer.TopologyNode{
		ID: local.ID, Name: local.Name, Address: local.Address, Port: 56700,
		Version: local.Version, Protocol: "local", Online: true,
	}
	if s.Domain != nil {
		state := s.Domain.Manager.State()
		self.Domain = state.Domain
		self.DomainSeq = state.Seq
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
	// 路由探测自动发现的上级（peer_auto_discover_uppers，默认开）
	for _, candidate := range s.autoUpperCandidates() {
		if candidate.Address != "" {
			add(candidate.Address)
		}
	}
	if len(result) > maxUpperCandidates {
		result = result[:maxUpperCandidates]
	}
	return result
}
