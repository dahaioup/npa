package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/npa/npa/internal/audit"
	"github.com/npa/npa/internal/autostart"
	"github.com/npa/npa/internal/desktop"
	"github.com/npa/npa/internal/discovery"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/file"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/peer"
	"github.com/npa/npa/internal/plugin"
	"github.com/npa/npa/internal/policy"
	"github.com/npa/npa/internal/portmap"
	"github.com/npa/npa/internal/relay"
	"github.com/npa/npa/internal/selfupdate"
	sshx "github.com/npa/npa/internal/ssh"
	"github.com/npa/npa/internal/state"
	"github.com/npa/npa/internal/tunnel"
	"github.com/npa/npa/internal/version"

	"github.com/pkg/sftp"
)

// Version 是当前构建的版本号，形如 0.4.1-1793220000（-后面是构建时刻的
// Unix 秒级时间戳）。界面统一在版本号前加 v，所以这里不带 v 前缀。
// 发布构建由 scripts/build-release.sh 通过
//
//	-ldflags "-X github.com/npa/npa/internal/service.Version=0.4.1-<epoch 秒>"
//
// 注入；直接 go build / go test 时保留这个默认值。必须是 var（const 无法被
// ldflags 改写）。
var Version = "0.4.1-dev"

// meshPort 是节点间通信的固定端口：注册、控制长连接、数据池、协调 RPC
// 与 MCP 共用 56701。节点通信端口不再可配（peer_port 设置项已废弃，
// 旧库残留值一律忽略），见 cmd/npa/main.go 的 meshSupervisor。
const meshPort = 56701

// heartbeatBootstrapInterval 分域管理者引导选举的周期（与成员心跳同量级）。
const heartbeatBootstrapInterval = 20 * time.Second

type Service struct {
	Store              *state.Store
	SSH                *sshx.Manager
	Files              *file.Service
	Tunnels            *tunnel.Manager
	Audit              *audit.Logger
	Desktop            *desktop.Router
	Plugin             *plugin.Manager
	Discovery          *discovery.Manager
	Relay              *relay.Manager
	PortMappings       *portmap.Manager
	Peer               *peer.Hub
	PeerAgent          *peer.Agent
	Domain             *peer.DomainHub
	autoUppersMu       sync.Mutex
	autoUppers         []peer.Candidate // 上级网络自动发现的最近结果（缓存 10 分钟刷新）
	resourceDir        string
	started            time.Time
	stop               chan struct{}
	stopOnce           sync.Once
	requireInflight    sync.Map // nodeID -> *sync.WaitGroup：同目标在途 require 去重
	holderCooldown     sync.Map // holderAddress -> time.Time：require 失败冷却
	topologyCache      *topologyCacheEntry
	topologyRefreshing bool
	downloadTokens     map[string]downloadToken
	mu                 sync.RWMutex
	limiter            map[string]*rateWindow
	OnSettingsChanged  func()
}

type rateWindow struct {
	start time.Time
	count int
}

func New(store *state.Store) *Service {
	sshManager := sshx.NewManager(store)
	fileService := file.NewService(sshManager)
	tunnelManager := tunnel.NewManager(store, sshManager)
	resourceDir, _ := os.MkdirTemp("", "npa-resources-*")
	app := &Service{
		Store: store, SSH: sshManager, Files: fileService, Tunnels: tunnelManager,
		Audit: audit.New(store), started: time.Now(), limiter: map[string]*rateWindow{},
		resourceDir: resourceDir, stop: make(chan struct{}),
	}
	app.Plugin = plugin.NewManager(filepath.Join("plugins", "desktop"))
	app.Desktop = desktop.NewRouterWithRoot(app.Plugin, resourceDir)
	app.Discovery = discovery.NewManager(store, Version)
	app.Relay = relay.NewManager(store, sshManager)
	app.PortMappings = portmap.NewManager(store, app.Relay)
	tunnelManager.TargetValidator = app.validateTarget
	tunnelManager.DialTarget = app.DialNode // Local/SOCKS 隧道也能走 peer 反向链路
	tunnelManager.IsPeer = func(nodeID string) bool { return app.isPeerNode(nodeID) }
	fileService.IsPeer = func(nodeID string) bool { return app.isPeerNode(nodeID) }
	// 必须用闭包延迟取值：wirePeer 在之后才创建 app.Peer
	fileService.PeerFS = func(ctx context.Context, nodeID string) (*sftp.Client, func(), error) {
		return app.Peer.FS(ctx, nodeID)
	}
	// HTTP 文件面：反向接入节点用注册上报的地址，同网段只靠广播互见的节点用广播地址，
	// 都直接打对端 mesh 端口（大文件不走 SFTP-over-WS 通道）。
	fileService.PeerHTTPBase = func(nodeID string) string { return app.meshBaseDirect(nodeID) }
	// 跨域兜底：经持有上级代理拨到节点自己的 mesh 端口，HTTP 文件面照样可用。
	fileService.RelayDial = func(ctx context.Context, id string) (net.Conn, error) {
		return app.dialNodeMesh(ctx, id)
	}
	app.PortMappings.TargetValidator = app.validateTarget
	app.wirePeer()
	app.startDiscoveryJanitor()
	return app
}

// startDiscoveryJanitor 启动时清理一次，之后每 30 分钟扫一次：
// 离线超过 6 小时的 mDNS 发现节点会被移除（避免同一台机器换网卡后留下重复的陈旧记录）。
func (s *Service) startDiscoveryJanitor() {
	s.PruneDiscovered()
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				s.PruneDiscovered()
			}
		}
	}()
}

// PruneDiscovered 清理离线超过 6 小时的发现节点，返回清理数量。
func (s *Service) PruneDiscovered() int {
	if s.Discovery == nil {
		return 0
	}
	removed, err := s.Discovery.PruneStale(time.Now())
	if err != nil {
		slog.Warn("prune discovered nodes failed", "error", err)
	}
	return removed
}

// wirePeer 构造反向接入的上级 Hub 与下级 Agent；闭包在运行期读取设置，保证热生效。
func (s *Service) wirePeer() {
	hostname, _ := os.Hostname()
	// 同一条错误可能每几秒重复（例如本机没有配置上级种子），
	// 只在错误变化时打印一次，之后最多每 5 分钟提醒一次；完整错误仍通过 /api/peers 暴露。
	var (
		logMu        sync.Mutex
		lastMessage  string
		lastLoggedAt time.Time
	)
	logPeerError := func(err error) {
		logMu.Lock()
		shouldLog := err.Error() != lastMessage || time.Since(lastLoggedAt) >= 5*time.Minute
		if shouldLog {
			lastMessage, lastLoggedAt = err.Error(), time.Now()
		}
		logMu.Unlock()
		if shouldLog {
			slog.Warn("peer agent", "error", err)
		}
	}
	s.Peer = peer.NewHub(s.Store, peer.Options{
		Version:   Version,
		Name:      hostname,
		Port:      func() int { return meshPort },
		Subnets:   discovery.LocalSubnets,
		AutoTrust: func() bool { return parseBool(s.Settings()["peer_auto_trust"]) },
		EphemeralIdle: func() time.Duration {
			return time.Duration(intSetting(s.Settings()["peer_on_demand_idle"], 60)) * time.Second
		},
		OnRegister: func(node model.Node) {
			s.Audit.Record(context.Background(), "peer.register", node.ID, policy.CapBase,
				audit.SafeParameters(map[string]string{"address": node.Address, "source": node.PeerSource, "subnets": strings.Join(node.ReachableSubnets, ",")}),
				time.Now(), nil)
		},
		// 跨上级查拓扑时，别人问"你是谁、你下面有谁"
		Self: func() peer.TopologyNode { return s.topologySelf() },
	})
	s.PeerAgent = peer.NewAgent(peer.AgentOptions{
		Version: Version,
		NodeID:  func() string { return model.NodeIDFromMAC() },
		Name:    func() string { host, _ := os.Hostname(); return host },
		Address: discovery.PrimaryIP,
		Subnets: discovery.LocalSubnets,
		Caps: func() []string {
			return []string{"ssh", "file", "tunnel", "screenshot", "mouse", "discovery", "relay"}
		},
		PoolSize:     func() int { return intSetting(s.Settings()["peer_pool_size"], 4) },
		Seeds:        func() string { return s.Settings()["peer_seeds"] },
		ProbeSubnets: func() string { return s.Settings()["peer_probe_subnets"] },
		AutoDiscover: func() bool { return s.Settings()["peer_auto_discover_uppers"] != "false" },
		Dial:         s.peerDial,
		SSHRoutes:    s.sshRouteHints,
		OnError:      logPeerError,
	})
	// relay 通过 peer 数据面访问反向接入的节点；跨上级协调由 service 自己实现
	s.Relay.SetPeerDialer(s.Peer)
	s.Relay.SetUpperCoordinator(s)
	s.Peer.StartSweeper() // 临时链路空闲回收（peer_on_demand_idle）
	// 分域 v2：本机同时带管理者与成员两个角色；uptime 从进程启动算起
	domainStart := time.Now()
	s.Domain = peer.NewDomainHub(func() peer.DomainMember {
		children := len(s.registeredChildren())
		return peer.DomainMember{
			ID:          model.NodeIDFromMAC(),
			Name:        hostname,
			Address:     net.JoinHostPort(discovery.PrimaryIP(), strconv.Itoa(meshPort)),
			Port:        meshPort,
			Uptime:      int64(time.Since(domainStart) / time.Second),
			CrossDomain: children > 0, // 有下级即天然处于多域交叉，选举优先
		}
	})
	s.Domain.Start(context.Background())
	s.Domain.SetNeighbors(func() []peer.DomainMember {
		// 邻域管理者候选：配置/发现的上级 + 已注册下级——
		// delta 沿树的上下方向双向 push
		result := []peer.DomainMember{}
		for _, address := range s.candidateUppers() {
			if host, port, err := net.SplitHostPort(address); err == nil {
				result = append(result, peer.DomainMember{Address: net.JoinHostPort(host, port)})
			}
		}
		for _, child := range s.registeredChildren() {
			result = append(result, peer.DomainMember{
				ID: child.ID, Address: net.JoinHostPort(child.Address, strconv.Itoa(meshPort)),
			})
		}
		return result
	})
	s.Domain.StartDeltaPush()
	// 上级网络自动发现（peer_auto_discover_uppers，默认开）：无种子时走路由
	// TTL 探测找上游内网网段并受限探测；结果缓存供 candidateUppers/选举使用。
	go s.refreshAutoUppersLoop(context.Background())
	// 管理者引导：周期性把「mDNS 发现的同网段节点 + 已注册下级」作为候选视图
	// 做确定性选举；当选者是别人就把心跳指过去，是自己就待命收心跳。
	go func() {
		ticker := time.NewTicker(heartbeatBootstrapInterval)
		defer ticker.Stop()
		for range ticker.C {
			if s.Domain == nil || s.Discovery == nil {
				continue
			}
			candidates := s.domainCandidates()
			if len(candidates) == 0 {
				continue
			}
			s.Domain.Elect(candidates)
		}
	}()
	// mDNS 广播 role/children/peer，便于同网段的其它上级发现我们并查询拓扑
	if s.Discovery != nil {
		s.Discovery.SetLocalInfo(func() int {
			if s.Settings()["peer_enabled"] != "true" {
				return 0
			}
			return meshPort
		}, func() int { return len(s.registeredChildren()) })
	}
}

// domainCandidates 分域管理者选举的候选视图：mDNS 发现的同网段 NPA + 已注册下级。
func (s *Service) domainCandidates() []peer.DomainMember {
	selfID := model.NodeIDFromMAC()
	result := []peer.DomainMember{}
	add := func(id, name, host string, port int, uptime int64) {
		if id == "" || id == selfID || host == "" {
			return
		}
		result = append(result, peer.DomainMember{
			ID: id, Name: name,
			Address: net.JoinHostPort(host, strconv.Itoa(port)),
			Port:    port, Uptime: uptime,
		})
	}
	if s.Discovery != nil {
		if nodes, err := s.Discovery.Nodes(); err == nil {
			for _, node := range nodes {
				port := node.PeerPort
				if port <= 0 {
					port = meshPort
				}
				add(node.ID, node.Name, node.Address, port, node.Uptime)
			}
		}
	}
	for _, node := range s.registeredChildren() {
		add(node.ID, node.Name, node.Address, meshPort, 0)
	}
	// 路由探测自动发现的上级（peer_auto_discover_uppers）：候选视图最后补上
	for _, candidate := range s.autoUpperCandidates() {
		host, portText, err := net.SplitHostPort(candidate.Address)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port <= 0 {
			port = meshPort
		}
		add(candidate.ID, candidate.Name, host, port, 0)
	}
	return result
}

// autoDiscoverInterval 自动发现结果的刷新周期；结果缓存在两次刷新之间复用。
const autoDiscoverInterval = 10 * time.Minute

// refreshAutoUppersLoop 周期性执行上级网络自动发现并缓存结果。
// 规则：关了开关、关了 peer、或配了种子（种子优先）就清空/跳过缓存。
func (s *Service) refreshAutoUppersLoop(ctx context.Context) {
	refresh := func() {
		if s.Settings()["peer_enabled"] != "true" ||
			s.Settings()["peer_auto_discover_uppers"] == "false" ||
			strings.TrimSpace(s.Settings()["peer_seeds"]) != "" {
			s.setAutoUppers(nil)
			return
		}
		found, err := peer.AutoDiscoverUppers(ctx, meshPort, discovery.LocalSubnets())
		if err != nil {
			return // traceroute 不可用/超时：保留上次结果，下轮再试
		}
		s.setAutoUppers(found)
	}
	refresh()
	ticker := time.NewTicker(autoDiscoverInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

func (s *Service) setAutoUppers(candidates []peer.Candidate) {
	s.autoUppersMu.Lock()
	s.autoUppers = candidates
	s.autoUppersMu.Unlock()
}

func (s *Service) autoUpperCandidates() []peer.Candidate {
	s.autoUppersMu.Lock()
	defer s.autoUppersMu.Unlock()
	return append([]peer.Candidate{}, s.autoUppers...)
}

// sshRouteHints 把本机已授权的 SSH 跳板上报给上级，
// 使上级能在「节点无法直达 SSH 服务」时计算出 NPA 映射 + SSH 映射的串联路径。
func (s *Service) sshRouteHints() []model.RemoteRoute {
	nodes, err := s.Store.ListNodes()
	if err != nil {
		return nil
	}
	routes := []model.RemoteRoute{}
	for _, node := range nodes {
		if node.Protocol == "peer" || !node.Trusted || !node.RelayEnabled {
			continue
		}
		if len(node.ReachableSubnets) == 0 {
			continue
		}
		routes = append(routes, model.RemoteRoute{
			NodeID: node.ID, Subnets: node.ReachableSubnets,
			Trusted: node.Trusted, RelayEnabled: node.RelayEnabled,
		})
	}
	return routes
}

// DialNode 是统一的目标拨号入口：peer 节点走反向链路，SSH 节点走 direct-tcpip。
// 直连失败且允许回退时，经持有上级代理（A→B→N）兜底——跨域隧道/映射因此可用。
// 端口映射、隧道、Web 代理共用它，从而对上层透明。
func (s *Service) DialNode(ctx context.Context, nodeID, target string) (net.Conn, error) {
	if isLocalNodeID(nodeID) {
		return nil, apperrors.New(apperrors.NotSupported, "本地节点不需要拨号")
	}
	conn, _, err := s.Relay.DialNode(ctx, nodeID, target)
	if err == nil {
		return conn, nil
	}
	if conn2, relayErr := s.dialNodeViaUpper(ctx, nodeID, target); relayErr == nil {
		return conn2, nil
	}
	return nil, err
}

// holderCooldownWindow require 失败后该持有者的冷却时长：期间对它的代理
// 请求直接失败，不再每次撞墙（拓扑里持有者断链是常态，等它自己刷新）。
const holderCooldownWindow = 30 * time.Second

// dialNodeViaUpper 是跨域兜底：按节点 ID 找到持有它的上级，请它代理转发。
// 同一目标节点的在途协商自动共享（后来的等待复用结果），避免重试风暴；
// 持有者协商失败后进入 30s 冷却，期间直接报错不再反复敲它。
func (s *Service) dialNodeViaUpper(ctx context.Context, nodeID, target string) (net.Conn, error) {
	if !s.RelayFallbackEnabled() {
		return nil, apperrors.New(apperrors.NodeOffline, "跨上级代理回退未启用（peer_relay_fallback）")
	}
	if inflight, busy := s.requireInflight.LoadOrStore(nodeID, &sync.WaitGroup{}); busy {
		wg := inflight.(*sync.WaitGroup)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// 首个协商已结束：直接再试一次（缓存效应在 hub 侧），
		// 失败也如实返回，不再叠加第二轮协商。
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	s.requireInflight.Store(nodeID, wg)
	defer func() {
		wg.Done()
		s.requireInflight.Delete(nodeID)
	}()
	holder, ok := s.HolderForNode(ctx, nodeID)
	if !ok {
		return nil, apperrors.New(apperrors.NodeOffline, "拓扑里找不到该节点的持有上级")
	}
	if raw, cooling := s.holderCooldown.Load(holder); cooling {
		if until, _ := raw.(time.Time); time.Now().Before(until) {
			return nil, apperrors.New(apperrors.NodeOffline, fmt.Sprintf("持有者 %s 协商刚失败，冷却中（%.0fs 后重试）", holder, holderCooldownWindow.Seconds()))
		}
		s.holderCooldown.Delete(holder)
	}
	conn, err := s.DialViaUpper(ctx, holder, nodeID, target, "")
	if err != nil {
		s.holderCooldown.Store(holder, time.Now().Add(holderCooldownWindow))
		return nil, err
	}
	s.holderCooldown.Delete(holder)
	return conn, nil
}

// dialNodeMesh 经反向链路或跨上级代理拨到节点自己的 mesh 端口（127.0.0.1:56701）。
// 拿到的是节点 HTTP 文件面/exec 端口的原始 TCP 连接。
func (s *Service) dialNodeMesh(ctx context.Context, nodeID string) (net.Conn, error) {
	return s.DialNode(ctx, nodeID, net.JoinHostPort("127.0.0.1", strconv.Itoa(meshPort)))
}

func intSetting(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

// meshBaseDirect 返回节点自己 mesh HTTP 端口的直连基址。
// 反向接入节点用注册上报的地址；同网段只靠广播互见的节点没有握手注册，
// 用广播里的地址直接访问它的 /api/file/* 与 /api/exec；跨域节点不在直连视图里返回空。
func (s *Service) meshBaseDirect(nodeID string) string {
	if node, err := s.Store.GetNode(nodeID); err == nil {
		// 反向接入节点用注册地址；mDNS 只看到、没注册的节点也能用它的广播地址，
		// 端口优先取它自报的 peer_port（默认 56701 只是兜底）。
		if node.Protocol != "peer" && !node.Discovered {
			return ""
		}
		if strings.TrimSpace(node.Address) == "" {
			return ""
		}
		return meshBaseURL(node.Address, node.PeerPort, node.Port)
	}
	if s.Discovery == nil {
		return ""
	}
	if node, ok := s.Discovery.Find(nodeID); ok && strings.TrimSpace(node.Address) != "" {
		return meshBaseURL(node.Address, node.PeerPort, node.Port)
	}
	return ""
}

// meshBaseURL 拼节点的 mesh 基址，端口按 peer_port → port → 默认端口 依次兜底。
func meshBaseURL(address string, peerPort, port int) string {
	switch {
	case peerPort > 0:
		port = peerPort
	case port > 0:
	default:
		port = peer.DefaultPort
	}
	return "http://" + net.JoinHostPort(address, strconv.Itoa(port))
}

// isPeerNode 判断目标是否为反向接入节点（连接由它主动建立）。
// 主表没有时再查完整拓扑：跨域节点（挂在别的上级下）只出现在拓扑里，
// 它们同样走 peer 通道（直连失败自动经持有上级代理）。
func (s *Service) isPeerNode(nodeID string) bool {
	if node, err := s.Store.GetNode(nodeID); err == nil {
		return node.Protocol == "peer"
	}
	payload, err := s.Topology(context.Background())
	if err != nil {
		return false
	}
	nodes, ok := payload["nodes"].([]peer.TopologyNode)
	if !ok {
		return false
	}
	for _, node := range nodes {
		if node.ID == nodeID && node.Protocol == "peer" {
			return true
		}
	}
	return false
}

// execViaNodeMesh 在跨域节点的 mesh 端口上执行命令：拨到 127.0.0.1:56701 后
// POST /api/exec（节点 HTTP 面，与文件传输同一条代理通道）。
func (s *Service) execViaNodeMesh(ctx context.Context, nodeID, command string, timeout time.Duration) (model.ExecResult, error) {
	conn, err := s.dialNodeMesh(ctx, nodeID)
	if err != nil {
		return model.ExecResult{}, err
	}
	return s.execWithConn(ctx, conn, command, timeout)
}

// execOverBase 直接对节点的 mesh HTTP 端口执行命令（同网段发现节点与反向接入节点）。
func (s *Service) execOverBase(ctx context.Context, base, command string, timeout time.Duration) (model.ExecResult, error) {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	transport := &http.Transport{
		DisableKeepAlives: true,
		MaxConnsPerHost:   1,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://"))
		},
	}
	return s.execWithTransport(ctx, transport, "http://mesh", command, timeout)
}

func (s *Service) execWithConn(ctx context.Context, conn net.Conn, command string, timeout time.Duration) (model.ExecResult, error) {
	transport := &http.Transport{
		DisableKeepAlives: true,
		MaxConnsPerHost:   1,
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return conn, nil
		},
	}
	return s.execWithTransport(ctx, transport, "http://mesh", command, timeout)
}

func (s *Service) execWithTransport(ctx context.Context, transport *http.Transport, url, command string, timeout time.Duration) (model.ExecResult, error) {
	result := model.ExecResult{}
	client := &http.Client{Transport: transport}
	defer client.CloseIdleConnections()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"command": command, "timeout_ms": timeout.Milliseconds()})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(url, "/")+"/api/exec", bytes.NewReader(payload))
	if err != nil {
		return result, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 256))
		return result, apperrors.New(apperrors.NodeOffline, "node exec endpoint: "+string(body))
	}
	err = json.NewDecoder(response.Body).Decode(&result)
	return result, err
}

// peerDial 实现「NPA 映射 + SSH 映射」的分段拨号：direct 由 NPA 自己转发，
// ssh:<nodeID> 则经该节点的 SSH 客户端（direct-tcpip）转发。
func (s *Service) peerDial(ctx context.Context, target, via string) (net.Conn, error) {
	if via == "" || via == "direct" {
		dialer := net.Dialer{Timeout: 10 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", target)
		if err != nil {
			return nil, apperrors.Wrap(apperrors.TargetForbidden, "peer direct dial failed", err)
		}
		return conn, nil
	}
	if strings.HasPrefix(via, "ssh:") {
		nodeID := strings.TrimPrefix(via, "ssh:")
		client, err := s.SSH.Client(ctx, nodeID)
		if err != nil {
			return nil, err
		}
		conn, err := client.Dial("tcp", target)
		if err != nil {
			return nil, apperrors.Wrap(apperrors.TargetForbidden, "peer ssh relay dial failed", err)
		}
		return conn, nil
	}
	return nil, apperrors.New(apperrors.InvalidArgument, "unsupported via: "+via)
}

// RegisterPeer 登记一个主动注册的反向接入节点（HTTP 注册入口）。
func (s *Service) RegisterPeer(ctx context.Context, registration peer.Registration, remoteAddr string) (model.Node, error) {
	if err := s.Authorize(ctx, "peer.register", policy.CapBase); err != nil {
		return model.Node{}, err
	}
	return s.Peer.Register(registration, remoteAddr)
}

// PeerStatus 返回下级 agent 状态、上级链路状态与相关设置。
func (s *Service) PeerStatus() map[string]any {
	settings := s.Settings()
	status := map[string]any{
		"enabled":       parseBool(settings["peer_enabled"]),
		"bind_scope":    settings["peer_bind_scope"],
		"port":          meshPort,
		"seeds":         settings["peer_seeds"],
		"probe_subnets": settings["peer_probe_subnets"],
		"auto_discover": settings["peer_auto_discover_uppers"] != "false",
		"pool_size":     intSetting(settings["peer_pool_size"], 4),
		"auto_trust":    parseBool(settings["peer_auto_trust"]),
		"address":       discovery.PrimaryIP(),
		"advertise":     strings.TrimSpace(s.Settings()["peer_advertise"]),
		"agent":         s.PeerAgent.Status(),
		"links":         s.Peer.Links(),
		"local_subnets": discovery.LocalSubnets(),
	}
	return status
}

// RefreshPeer 重启下级发现循环（立即重新发现并选主）。
func (s *Service) RefreshPeer(ctx context.Context) error {
	if err := s.Authorize(ctx, "peer.refresh", policy.CapBase); err != nil {
		return err
	}
	if !parseBool(s.Settings()["peer_enabled"]) {
		return apperrors.New(apperrors.NotSupported, "反向接入未开启")
	}
	s.PeerAgent.Stop()
	s.PeerAgent.Start(context.Background())
	s.Audit.Record(ctx, "peer.refresh", "", policy.CapBase, "", time.Now(), nil)
	return nil
}

func (s *Service) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
	if s.PeerAgent != nil {
		s.PeerAgent.Stop()
	}
	if s.Peer != nil {
		s.Peer.Close()
	}
	s.Tunnels.Close()
	if s.PortMappings != nil {
		s.PortMappings.Close()
	}
	_ = s.SSH.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.Plugin.Close(ctx)
	cancel()
	if s.resourceDir != "" {
		_ = os.RemoveAll(s.resourceDir)
	}
	if s.Discovery != nil {
		s.Discovery.Close()
	}
	_ = s.Store.Close()
}

func (s *Service) MCPCapabilities() map[string]bool {
	settings, _ := s.Store.Settings()
	loopback := settings["mcp_bind_scope"] != "lan"
	return map[string]bool{
		"ssh": parseBool(settings["mcp_ssh"]), "file": parseBool(settings["mcp_file"]),
		"tunnel": parseBool(settings["mcp_tunnel"]), "screenshot": parseBool(settings["mcp_screenshot"]),
		"mouse":     parseBool(settings["mcp_mouse"]),
		"discovery": !loopback && parseBool(settings["mcp_discovery"]),
		"relay":     !loopback && parseBool(settings["mcp_relay"]),
	}
}

func parseBool(value string) bool { return strings.EqualFold(value, "true") || value == "1" }

func (s *Service) Settings() map[string]string {
	settings, _ := s.Store.Settings()
	return settings
}

func (s *Service) SaveSettings(values map[string]string) error {
	current := s.Settings()
	scope := values["mcp_bind_scope"]
	if scope == "" {
		scope = current["mcp_bind_scope"]
	}
	if scope != "lan" {
		explicitDiscovery := values["mcp_discovery"] == "true"
		explicitRelay := values["mcp_relay"] == "true"
		if explicitDiscovery || explicitRelay {
			return apperrors.New(apperrors.InvalidArgument, "仅本机 MCP 范围不允许开启节点发现或中继权限")
		}
		if _, changingScope := values["mcp_bind_scope"]; changingScope || current["mcp_discovery"] == "true" || current["mcp_relay"] == "true" {
			values["mcp_discovery"] = "false"
			values["mcp_relay"] = "false"
		}
	}
	// 开机自启动：先真正写/删系统的启动项，成功才落库，失败原样报错（不静默）
	if raw, ok := values["autostart"]; ok {
		var err error
		if parseBool(raw) {
			err = autostart.Enable(s.Store.DataDir())
		} else {
			err = autostart.Disable()
		}
		if err != nil {
			return apperrors.Wrap(apperrors.Internal, "设置开机自启动失败", err)
		}
	}
	// 隐藏控制台：run.sh / run.bat 读这个标记决定前台阻塞还是后台异步启动
	if raw, ok := values["hide_console"]; ok {
		if parseBool(raw) {
			if err := os.WriteFile(s.HideConsoleMarker(), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644); err != nil {
				return apperrors.Wrap(apperrors.Internal, "写入隐藏控制台标记失败", err)
			}
		} else if err := os.Remove(s.HideConsoleMarker()); err != nil && !os.IsNotExist(err) {
			return apperrors.Wrap(apperrors.Internal, "删除隐藏控制台标记失败", err)
		}
	}
	if err := s.Store.SaveSettings(values); err != nil {
		return err
	}
	s.ApplyRuntimeSettings(context.Background())
	return nil
}

// HideConsoleMarkerPath 是 run.sh / run.bat 与 Go 侧共同依据的标记文件：
// 存在 → 后台/隐藏模式；删除 → 立刻回到前台可见模式（不依赖数据库，便于离线恢复）。
func HideConsoleMarkerPath(dataDir string) string { return filepath.Join(dataDir, "hide_console") }

// HideConsoleMarker 是当前实例的隐藏控制台标记位置。
func (s *Service) HideConsoleMarker() string { return HideConsoleMarkerPath(s.Store.DataDir()) }

// LogFilePath 是日志文件位置，前台与后台模式都会写。
func (s *Service) LogFilePath() string { return filepath.Join(s.Store.DataDir(), "npa.log") }

// SystemInfo 返回本机版本、平台、路径与自启动等系统状态，供设置页展示。
func (s *Service) SystemInfo() map[string]any {
	settings := s.Settings()
	status, err := autostart.Current()
	info := map[string]any{
		"version":                Version,
		"goos":                   runtime.GOOS,
		"goarch":                 runtime.GOARCH,
		"executable":             executablePath(),
		"data_dir":               s.Store.DataDir(),
		"log_file":               s.LogFilePath(),
		"hide_console":           parseBool(settings["hide_console"]),
		"hide_console_supported": true, // 由 run 脚本后台化实现，跨平台可用
		"hide_console_marker":    s.HideConsoleMarker(),
		"update_artifact":        settings["update_artifact"],
		"autostart":              status,
	}
	if err != nil {
		info["autostart_error"] = err.Error()
	}
	return info
}

func executablePath() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		return resolved
	}
	return path
}

func (s *Service) ApplyRuntimeSettings(ctx context.Context) {
	if s.Discovery != nil {
		_ = s.Discovery.SetEnabled(ctx, parseBool(s.Settings()["discovery_enabled"]))
	}
	if s.PeerAgent != nil {
		if parseBool(s.Settings()["peer_enabled"]) {
			s.PeerAgent.Start(context.Background())
		} else {
			s.PeerAgent.Stop()
		}
	}
	if s.OnSettingsChanged != nil {
		s.OnSettingsChanged()
	}
}

func (s *Service) Authorize(ctx context.Context, operation string, capability policy.Capability) error {
	if policy.SourceFrom(ctx) == policy.SourceMCP {
		if err := s.rateLimit(operation); err != nil {
			return err
		}
	}
	return policy.Check(ctx, s, capability)
}

func (s *Service) rateLimit(operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(policy.SourceMCP) + ":" + operation
	now := time.Now()
	window := s.limiter[key]
	if window == nil || now.Sub(window.start) >= time.Second {
		s.limiter[key] = &rateWindow{start: now, count: 1}
		return nil
	}
	if window.count >= 30 {
		return apperrors.New(apperrors.RateLimited, "MCP operation rate limit exceeded")
	}
	window.count++
	return nil
}

func localNode() model.Node {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	userName := os.Getenv("USER")
	if userName == "" {
		userName = os.Getenv("USERNAME")
	}
	if userName == "" {
		userName = "local"
	}
	// 地址必须是本机真实 LAN IP：hostname 可能是 "localhost"，对反向接入对端没有意义。
	return model.Node{
		ID: model.NodeIDFromMAC(), DisplayID: model.NodeIDFromMAC(), Name: "本地节点",
		Address: discovery.PrimaryIP(), Port: 0, Username: userName, Version: Version,
		AuthMethod: "local", Protocol: "local", Tags: []string{"local", "host:" + host},
		Status: "connected", CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Now().UTC(),
	}
}

func isLocalNodeID(id string) bool {
	return id == "local" || id == model.NodeIDFromMAC()
}

func discoveredFallbackID(node model.Node) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(node.Name + "-" + node.Address) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('-')
		}
	}
	id := strings.Trim(builder.String(), "-")
	if id == "" {
		id = "node"
	}
	return "discovered-" + id
}

func (s *Service) ListNodes() ([]model.Node, error) {
	nodes, err := s.Store.ListNodes()
	if err != nil {
		return nil, err
	}
	result := append([]model.Node{localNode()}, nodes...)
	known := map[string]struct{}{}
	for _, node := range result {
		known[node.ID] = struct{}{}
	}
	discovered, err := s.Store.ListDiscoveredNodes()
	if err != nil {
		return nil, err
	}
	for _, node := range discovered {
		candidate := node
		if _, exists := known[candidate.ID]; exists {
			candidate.ID = discoveredFallbackID(candidate)
			if _, exists := known[candidate.ID]; exists {
				candidate.ID += "-" + fmt.Sprintf("%x", time.Now().UnixNano())
			}
		}
		result = append(result, candidate)
		known[candidate.ID] = struct{}{}
	}
	for i := range result {
		if isLocalNodeID(result[i].ID) {
			result[i].Online = true
			continue
		}
		result[i].Online = s.probeNode(result[i])
		// 对端自报版本比本机新时标记出来，Web 与 MCP 都能看到
		result[i].UpdateAvailable = version.IsNewer(result[i].Version, Version)
	}
	return result, nil
}

type nodeHealth struct {
	online    bool
	checkedAt time.Time
}

const (
	healthProbeTimeout = 600 * time.Millisecond
	healthCacheTTL     = 5 * time.Second
	// mDNS 广播新鲜度兜底：探测不到端口但广播还在，说明进程活着
	discoveredFreshWindow = 3 * time.Minute
)

var healthCache sync.Map // nodeID -> nodeHealth

// probeNode 判断节点是否在线：
//   - peer 节点看反向链路（权威，无需探测）
//   - 其余节点并发探测它可能监听的端口（SSH 端口、peer 端口、MCP 端口），结果缓存 5s
//   - mDNS 发现的节点在广播新鲜时也视为在线（Web 默认只监听回环，端口探测可能失败）
func (s *Service) probeNode(node model.Node) bool {
	if node.Protocol == "peer" {
		return s.Peer != nil && s.Peer.Online(node.ID)
	}
	if cached, ok := healthCache.Load(node.ID); ok {
		if entry, valid := cached.(nodeHealth); valid && time.Since(entry.checkedAt) < healthCacheTTL {
			return entry.online
		}
	}
	online := s.tcpProbe(node)
	if !online && node.Discovered && !node.LastSeen.IsZero() && time.Since(node.LastSeen) < discoveredFreshWindow {
		online = true
	}
	healthCache.Store(node.ID, nodeHealth{online: online, checkedAt: time.Now()})
	return online
}

// tcpProbe 并发探测该节点可能监听的端口，任一可达即认为在线。
func (s *Service) tcpProbe(node model.Node) bool {
	ports := map[int]struct{}{}
	if node.Port > 0 {
		ports[node.Port] = struct{}{}
	}
	ports[meshPort] = struct{}{}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		reach bool
	)
	for port := range ports {
		port := port
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(node.Address, strconv.Itoa(port)), healthProbeTimeout)
			if err != nil {
				return
			}
			_ = conn.Close()
			mu.Lock()
			reach = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	return reach
}

func (s *Service) GetNode(id string) (model.Node, error) {
	if isLocalNodeID(id) {
		return localNode(), nil
	}
	return s.Store.GetNode(id)
}

func (s *Service) SaveNode(ctx context.Context, node model.Node, credential model.Credential) error {
	if err := s.Authorize(ctx, "node.write", policy.CapBase); err != nil {
		return err
	}
	if err := s.Store.SaveNode(node); err != nil {
		return err
	}
	credential.NodeID = node.ID
	if err := s.Store.SaveCredential(credential); err != nil {
		return err
	}
	s.Audit.Record(ctx, "node.write", node.ID, policy.CapBase, audit.SafeParameters(map[string]string{"name": node.Name, "address": node.Address}), time.Now(), nil)
	return nil
}

func (s *Service) DeleteNode(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "node.delete", policy.CapBase); err != nil {
		return err
	}
	err := s.Store.DeleteNode(id)
	if err == nil {
		s.Audit.Record(ctx, "node.delete", id, policy.CapBase, "", time.Now(), nil)
	}
	return err
}

func (s *Service) ConnectNode(ctx context.Context, id string) error {
	if isLocalNodeID(id) {
		return nil
	}
	if err := s.Authorize(ctx, "ssh.connect", policy.CapSSH); err != nil {
		return err
	}
	started := time.Now()
	_, err := s.SSH.Client(ctx, id)
	if err != nil {
		s.Audit.Record(ctx, "ssh.connect", id, policy.CapSSH, "", started, err)
		return err
	}
	s.Audit.Record(ctx, "ssh.connect", id, policy.CapSSH, "", started, nil)
	return nil
}

func (s *Service) DisconnectNode(ctx context.Context, id string) error {
	if isLocalNodeID(id) {
		return nil
	}
	if err := s.Authorize(ctx, "ssh.disconnect", policy.CapSSH); err != nil {
		return err
	}
	err := s.SSH.Close(id)
	s.Audit.Record(ctx, "ssh.disconnect", id, policy.CapSSH, "", time.Now(), err)
	return err
}

func (s *Service) Exec(ctx context.Context, nodeID, command string, timeout time.Duration) (model.ExecResult, error) {
	if err := s.Authorize(ctx, "ssh.exec", policy.CapSSH); err != nil {
		return model.ExecResult{}, err
	}
	if strings.TrimSpace(command) == "" {
		return model.ExecResult{}, apperrors.New(apperrors.InvalidArgument, "command is required")
	}
	started := time.Now()
	base := s.meshBaseDirect(nodeID)
	if s.isPeerNode(nodeID) || base != "" {
		result, err := s.Peer.Exec(ctx, nodeID, command, timeout)
		if err == nil {
			s.Audit.Record(ctx, "peer.exec", nodeID, policy.CapSSH, audit.SafeParameters(map[string]string{"command": command}), started, nil)
			return result, nil
		}
		// 同网段/反向接入：直连它的 mesh 端口走 HTTP exec。
		if base != "" {
			if directResult, directErr := s.execOverBase(ctx, base, command, timeout); directErr == nil {
				s.Audit.Record(ctx, "peer.exec", nodeID, policy.CapSSH, audit.SafeParameters(map[string]string{"command": command, "via": "mesh-http"}), started, nil)
				return directResult, nil
			}
		}
		// 跨域节点不在本机链路上：经持有上级代理拨它的 mesh 端口走 HTTP exec。
		if relayResult, relayErr := s.execViaNodeMesh(ctx, nodeID, command, timeout); relayErr == nil {
			s.Audit.Record(ctx, "peer.exec", nodeID, policy.CapSSH, audit.SafeParameters(map[string]string{"command": command, "via": "upper-relay"}), started, nil)
			return relayResult, nil
		}
		s.Audit.Record(ctx, "peer.exec", nodeID, policy.CapSSH, audit.SafeParameters(map[string]string{"command": command}), started, err)
		return result, err
	}
	result, err := s.SSH.Exec(ctx, nodeID, sshx.ExecRequest{Command: command, Timeout: timeout})
	s.Audit.Record(ctx, "ssh.exec", nodeID, policy.CapSSH, audit.SafeParameters(map[string]string{"command": command}), started, err)
	return result, err
}

func (s *Service) OpenShell(ctx context.Context, nodeID string, cols, rows int) (*sshx.Shell, error) {
	if err := s.Authorize(ctx, "ssh.shell", policy.CapSSH); err != nil {
		return nil, err
	}
	started := time.Now()
	if s.isPeerNode(nodeID) {
		shell, err := s.Peer.OpenPTY(ctx, nodeID, cols, rows)
		s.Audit.Record(ctx, "peer.shell", nodeID, policy.CapSSH, "", started, err)
		return shell, err
	}
	shell, err := s.SSH.OpenShell(ctx, nodeID, cols, rows)
	s.Audit.Record(ctx, "ssh.shell", nodeID, policy.CapSSH, "", started, err)
	return shell, err
}

func (s *Service) Screens(ctx context.Context) ([]desktop.Screen, error) {
	if err := s.Authorize(ctx, "desktop.screens", policy.CapScreenshot); err != nil {
		return nil, err
	}
	started := time.Now()
	screens, err := s.Desktop.Screens(ctx)
	s.Audit.Record(ctx, "desktop.screens", "", policy.CapScreenshot, "", started, err)
	return screens, err
}

func (s *Service) Screenshot(ctx context.Context, request desktop.CaptureRequest) (desktop.CaptureResult, error) {
	if err := s.Authorize(ctx, "desktop.screenshot", policy.CapScreenshot); err != nil {
		return desktop.CaptureResult{}, err
	}
	started := time.Now()
	result, err := s.Desktop.Screenshot(ctx, request)
	s.Audit.Record(ctx, "desktop.screenshot", "", policy.CapScreenshot, audit.SafeParameters(map[string]string{"screen": request.ScreenID}), started, err)
	return result, err
}

func (s *Service) MoveMouse(ctx context.Context, request desktop.MoveRequest) error {
	if err := s.Authorize(ctx, "desktop.mouse_move", policy.CapMouse); err != nil {
		return err
	}
	started := time.Now()
	err := s.Desktop.MoveMouse(ctx, request)
	s.Audit.Record(ctx, "desktop.mouse_move", "", policy.CapMouse, audit.SafeParameters(map[string]string{"x": fmt.Sprintf("%d", request.X), "y": fmt.Sprintf("%d", request.Y)}), started, err)
	return err
}

func (s *Service) MouseButton(ctx context.Context, request desktop.ButtonRequest) error {
	if err := s.Authorize(ctx, "desktop.mouse_click", policy.CapMouse); err != nil {
		return err
	}
	started := time.Now()
	err := s.Desktop.MouseButton(ctx, request)
	s.Audit.Record(ctx, "desktop.mouse_click", "", policy.CapMouse, audit.SafeParameters(map[string]string{"button": request.Button, "action": request.Action}), started, err)
	return err
}

func (s *Service) Scroll(ctx context.Context, request desktop.ScrollRequest) error {
	if err := s.Authorize(ctx, "desktop.mouse_scroll", policy.CapMouse); err != nil {
		return err
	}
	started := time.Now()
	err := s.Desktop.Scroll(ctx, request)
	s.Audit.Record(ctx, "desktop.mouse_scroll", "", policy.CapMouse, audit.SafeParameters(map[string]string{"dx": fmt.Sprintf("%d", request.DeltaX), "dy": fmt.Sprintf("%d", request.DeltaY)}), started, err)
	return err
}

func (s *Service) DiscoveryNodes(ctx context.Context) ([]model.Node, error) {
	if err := s.Authorize(ctx, "discovery.nodes", policy.CapDiscovery); err != nil {
		return nil, err
	}
	started := time.Now()
	nodes, err := s.Discovery.Nodes()
	s.Audit.Record(ctx, "discovery.nodes", "", policy.CapDiscovery, "", started, err)
	return nodes, err
}

func (s *Service) DiscoveryRefresh(ctx context.Context) error {
	if err := s.Authorize(ctx, "discovery.refresh", policy.CapDiscovery); err != nil {
		return err
	}
	started := time.Now()
	err := s.Discovery.Refresh(ctx)
	s.Audit.Record(ctx, "discovery.refresh", "", policy.CapDiscovery, "", started, err)
	return err
}

func (s *Service) NetworkSubnets(ctx context.Context) ([]model.SubnetInfo, error) {
	if err := s.Authorize(ctx, "network.subnets", policy.CapDiscovery); err != nil {
		return nil, err
	}
	started := time.Now()
	subnets, err := s.Relay.Subnets(ctx)
	s.Audit.Record(ctx, "network.subnets", "", policy.CapDiscovery, "", started, err)
	return subnets, err
}

func (s *Service) RelayRoutes(ctx context.Context) ([]model.RelayRoute, error) {
	if err := s.Authorize(ctx, "relay.routes", policy.CapRelay); err != nil {
		return nil, err
	}
	started := time.Now()
	routes, err := s.Relay.Routes()
	s.Audit.Record(ctx, "relay.routes", "", policy.CapRelay, "", started, err)
	return routes, err
}

func (s *Service) RelayOpen(ctx context.Context, target string, targetCapability policy.Capability) (model.RelayRoute, error) {
	if err := s.Authorize(ctx, "relay.open", policy.CapRelay); err != nil {
		return model.RelayRoute{}, err
	}
	if targetCapability != policy.CapBase {
		if err := s.Authorize(ctx, "relay.open", targetCapability); err != nil {
			return model.RelayRoute{}, err
		}
	}
	started := time.Now()
	route, err := s.Relay.Select(ctx, target)
	s.Audit.Record(ctx, "relay.open", route.ViaNodeID, policy.CapRelay, audit.SafeParameters(map[string]string{"target": target}), started, err)
	return route, err
}

func (s *Service) RelayDial(ctx context.Context, target string) (net.Conn, error) {
	if err := s.Authorize(ctx, "relay.dial", policy.CapRelay); err != nil {
		return nil, err
	}
	if err := s.Authorize(ctx, "relay.dial", policy.CapTunnel); err != nil {
		return nil, err
	}
	started := time.Now()
	conn, err := s.Relay.Dial(ctx, target)
	if err == nil {
		s.Audit.Record(ctx, "relay.dial", "", policy.CapRelay, audit.SafeParameters(map[string]string{"target": target}), started, nil)
	} else {
		s.Audit.Record(ctx, "relay.dial", "", policy.CapRelay, audit.SafeParameters(map[string]string{"target": target}), started, err)
	}
	return conn, err
}

func (s *Service) FileList(ctx context.Context, nodeID, path string) ([]model.FileEntry, error) {
	if isLocalNodeID(nodeID) {
		return nil, apperrors.New(apperrors.NotSupported, "local file browsing is not enabled")
	}
	if err := s.Authorize(ctx, "file.list", policy.CapFile); err != nil {
		return nil, err
	}
	started := time.Now()
	result, err := s.Files.List(ctx, nodeID, path)
	s.Audit.Record(ctx, "file.list", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return result, err
}

func (s *Service) FileRead(ctx context.Context, nodeID, path string) ([]byte, error) {
	if isLocalNodeID(nodeID) {
		return nil, apperrors.New(apperrors.NotSupported, "local file browsing is not enabled")
	}
	if err := s.Authorize(ctx, "file.read", policy.CapFile); err != nil {
		return nil, err
	}
	started := time.Now()
	result, err := s.Files.Read(ctx, nodeID, path)
	s.Audit.Record(ctx, "file.read", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return result, err
}

func (s *Service) FileWrite(ctx context.Context, nodeID, path string, data []byte) error {
	if isLocalNodeID(nodeID) {
		return apperrors.New(apperrors.NotSupported, "local file browsing is not enabled")
	}
	if err := s.Authorize(ctx, "file.write", policy.CapFile); err != nil {
		return err
	}
	started := time.Now()
	err := s.Files.Write(ctx, nodeID, path, data)
	s.Audit.Record(ctx, "file.write", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return err
}

func (s *Service) FileRemove(ctx context.Context, nodeID, path string) error {
	if isLocalNodeID(nodeID) {
		return apperrors.New(apperrors.NotSupported, "local file browsing is not enabled")
	}
	if err := s.Authorize(ctx, "file.remove", policy.CapFile); err != nil {
		return err
	}
	started := time.Now()
	err := s.Files.Remove(ctx, nodeID, path)
	s.Audit.Record(ctx, "file.remove", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return err
}

func (s *Service) FileMkdir(ctx context.Context, nodeID, path string) error {
	if isLocalNodeID(nodeID) {
		return apperrors.New(apperrors.NotSupported, "local file browsing is not enabled")
	}
	if err := s.Authorize(ctx, "file.mkdir", policy.CapFile); err != nil {
		return err
	}
	started := time.Now()
	err := s.Files.Mkdir(ctx, nodeID, path)
	s.Audit.Record(ctx, "file.mkdir", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return err
}

func (s *Service) FileRename(ctx context.Context, nodeID, oldPath, newPath string) error {
	if isLocalNodeID(nodeID) {
		return apperrors.New(apperrors.NotSupported, "local file browsing is not enabled")
	}
	if err := s.Authorize(ctx, "file.rename", policy.CapFile); err != nil {
		return err
	}
	started := time.Now()
	err := s.Files.Rename(ctx, nodeID, oldPath, newPath)
	s.Audit.Record(ctx, "file.rename", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"old": oldPath, "new": newPath}), started, err)
	return err
}

func (s *Service) FileUpload(ctx context.Context, nodeID, path string, reader interface{ Read([]byte) (int, error) }) (int64, error) {
	if isLocalNodeID(nodeID) {
		return 0, apperrors.New(apperrors.NotSupported, "local file browsing is not enabled")
	}
	if err := s.Authorize(ctx, "file.write", policy.CapFile); err != nil {
		return 0, err
	}
	started := time.Now()
	written, err := s.Files.Upload(ctx, nodeID, path, reader)
	s.Audit.Record(ctx, "file.upload", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return written, err
}

func (s *Service) FileDownload(ctx context.Context, nodeID, path string) (interface {
	Read([]byte) (int, error)
	Close() error
}, error) {
	if isLocalNodeID(nodeID) {
		return nil, apperrors.New(apperrors.NotSupported, "local file browsing is not enabled")
	}
	if err := s.Authorize(ctx, "file.read", policy.CapFile); err != nil {
		return nil, err
	}
	started := time.Now()
	reader, err := s.Files.OpenRead(ctx, nodeID, path)
	if err != nil {
		s.Audit.Record(ctx, "file.download", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
		return nil, err
	}
	s.Audit.Record(ctx, "file.download", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, nil)
	return reader, nil
}

// UpdateCheck 遍历已连接节点，找出版本比本机新的，并在该节点上定位可用产物。
func (s *Service) UpdateCheck(ctx context.Context) (map[string]any, error) {
	if err := s.Authorize(ctx, "update.check", policy.CapBase); err != nil {
		return nil, err
	}
	started := time.Now()
	hint := s.Settings()["update_artifact"]
	nodes, err := s.ListNodes()
	if err != nil {
		return nil, err
	}
	updates := []selfupdate.Candidate{}
	considered := 0
	for _, node := range nodes {
		if isLocalNodeID(node.ID) || strings.TrimSpace(node.Version) == "" {
			continue
		}
		considered++
		if !version.IsNewer(node.Version, Version) {
			continue
		}
		entry, findErr := selfupdate.FindArtifact(ctx, s.Files, node.ID, hint)
		if findErr != nil {
			continue
		}
		updates = append(updates, selfupdate.Candidate{
			NodeID: node.ID, NodeName: node.Name, NodeVersion: node.Version,
			Artifact: entry.Path, Size: entry.Size,
			IsArchive: strings.HasSuffix(strings.ToLower(entry.Path), ".zip"),
			UpdatedAt: entry.ModTime.UTC().Format(time.RFC3339),
		})
	}
	s.Audit.Record(ctx, "update.check", "", policy.CapBase,
		audit.SafeParameters(map[string]string{"found": strconv.Itoa(len(updates)), "considered": strconv.Itoa(considered)}),
		started, nil)
	return map[string]any{
		"self_version": Version,
		"updates":      updates,
		"considered":   considered,
		"hint":         hint,
	}, nil
}

// UpdatePrepare 从节点下载产物并解出本平台二进制，完成校验；不替换、不重启。
// 返回的 binary_path 交给 UpdateApplyBinary 使用（调用方应在响应发出后再调用）。
func (s *Service) UpdatePrepare(ctx context.Context, nodeID, artifact string) (map[string]any, error) {
	if err := s.Authorize(ctx, "update.download", policy.CapBase); err != nil {
		return nil, err
	}
	started := time.Now()
	node, err := s.GetNode(nodeID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(node.Version) == "" || !version.IsNewer(node.Version, Version) {
		return nil, apperrors.New(apperrors.InvalidArgument, "该节点的版本不比本机新")
	}
	if strings.TrimSpace(artifact) == "" {
		entry, findErr := selfupdate.FindArtifact(ctx, s.Files, nodeID, s.Settings()["update_artifact"])
		if findErr != nil {
			s.Audit.Record(ctx, "update.download", nodeID, policy.CapBase, "", started, findErr)
			return nil, findErr
		}
		artifact = entry.Path
	}
	destDir := filepath.Join(s.Store.DataDir(), "update")
	local, size, err := selfupdate.Download(ctx, s.Files, nodeID, artifact, destDir)
	if err != nil {
		s.Audit.Record(ctx, "update.download", nodeID, policy.CapBase, audit.SafeParameters(map[string]string{"artifact": artifact}), started, err)
		return nil, err
	}
	digest, _ := selfupdate.FileSHA256(local)
	binary, err := selfupdate.Extract(local, destDir)
	if err != nil {
		s.Audit.Record(ctx, "update.download", nodeID, policy.CapBase, audit.SafeParameters(map[string]string{"artifact": artifact}), started, err)
		return nil, err
	}
	if err := selfupdate.VerifyArtifact(binary); err != nil {
		s.Audit.Record(ctx, "update.download", nodeID, policy.CapBase, audit.SafeParameters(map[string]string{"artifact": artifact}), started, err)
		return nil, err
	}
	executable := executablePath()
	s.Audit.Record(ctx, "update.download", nodeID, policy.CapBase,
		audit.SafeParameters(map[string]string{"artifact": artifact, "sha256": digest, "bytes": strconv.FormatInt(size, 10)}), started, nil)
	return map[string]any{
		"node_id":      nodeID,
		"node_version": node.Version,
		"artifact":     artifact,
		"local_path":   local,
		"binary_path":  binary,
		"size":         size,
		"sha256":       digest,
		"executable":   executable,
	}, nil
}

// UpdateApplyBinary 替换本机二进制并重启。
// Unix 上用 syscall.Exec 原地替换（不会返回）；Windows 上先改名旧文件、写入新文件、
// 拉起新进程，然后由调用方关闭旧进程。
func (s *Service) UpdateApplyBinary(binaryPath string) error {
	executable := executablePath()
	args := append([]string{executable}, os.Args[1:]...)
	s.Audit.Record(context.Background(), "update.apply", "", policy.CapBase,
		audit.SafeParameters(map[string]string{"binary": binaryPath, "executable": executable}), time.Now(), nil)
	restarted, err := selfupdate.Apply(binaryPath, args)
	if err != nil {
		return err
	}
	if restarted {
		slog.Info("新版本已启动，本进程即将退出", "executable", executable)
	}
	return nil
}

func (s *Service) ListTunnels() ([]model.Tunnel, error) { return s.Tunnels.List() }

func (s *Service) SaveTunnel(ctx context.Context, definition model.Tunnel) error {
	if isLocalNodeID(definition.NodeID) {
		return apperrors.New(apperrors.NotSupported, "local node cannot create SSH tunnels")
	}
	if err := s.Authorize(ctx, "tunnel.write", policy.CapTunnel); err != nil {
		return err
	}
	if err := s.Store.SaveTunnel(definition); err != nil {
		return err
	}
	s.Audit.Record(ctx, "tunnel.write", definition.NodeID, policy.CapTunnel, audit.SafeParameters(map[string]string{"type": definition.Type, "listen": fmt.Sprintf("%s:%d", definition.ListenAddress, definition.ListenPort)}), time.Now(), nil)
	return nil
}

func (s *Service) StartTunnel(ctx context.Context, id string) (model.Tunnel, error) {
	if err := s.Authorize(ctx, "tunnel.open", policy.CapTunnel); err != nil {
		return model.Tunnel{}, err
	}
	definition, err := s.Store.GetTunnel(id)
	if err != nil {
		return definition, err
	}
	if isLocalNodeID(definition.NodeID) {
		return definition, apperrors.New(apperrors.NotSupported, "local node cannot create SSH tunnels")
	}
	if definition.Type == "remote" {
		if node, err := s.Store.GetNode(definition.NodeID); err == nil && node.Protocol == "peer" {
			return definition, apperrors.New(apperrors.NotSupported,
				"peer 节点没有 SSH 服务端，不支持反向隧道；请改用端口映射或 Local/SOCKS 隧道")
		}
	}
	if definition.Type != "socks" {
		if err := s.validateTarget(definition.TargetHost, definition.TargetPort); err != nil {
			return definition, err
		}
	}
	started := time.Now()
	result, err := s.Tunnels.Start(ctx, id)
	s.Audit.Record(ctx, "tunnel.open", definition.NodeID, policy.CapTunnel, audit.SafeParameters(map[string]string{"type": definition.Type}), started, err)
	return result, err
}

func (s *Service) StopTunnel(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "tunnel.close", policy.CapTunnel); err != nil {
		return err
	}
	err := s.Tunnels.Stop(id)
	s.Audit.Record(ctx, "tunnel.close", id, policy.CapTunnel, "", time.Now(), err)
	return err
}

func (s *Service) DeleteTunnel(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "tunnel.delete", policy.CapTunnel); err != nil {
		return err
	}
	err := s.Tunnels.Delete(id)
	s.Audit.Record(ctx, "tunnel.delete", id, policy.CapTunnel, "", time.Now(), err)
	return err
}

// ListPortMappings 列出 NPA 自带的端口映射（与 SSH 隧道独立）。
func (s *Service) ListPortMappings() ([]model.PortMapping, error) { return s.PortMappings.List() }

func portMappingParameters(definition model.PortMapping) map[string]string {
	return map[string]string{
		"target": fmt.Sprintf("%s:%d", definition.TargetHost, definition.TargetPort),
		"listen": fmt.Sprintf("%s:%d", definition.ListenAddress, definition.ListenPort),
		"via":    definition.ViaNodeID,
	}
}

func (s *Service) SavePortMapping(ctx context.Context, definition model.PortMapping) error {
	if err := s.Authorize(ctx, "portmap.write", policy.CapTunnel); err != nil {
		return err
	}
	if err := s.validateTarget(definition.TargetHost, definition.TargetPort); err != nil {
		return err
	}
	if err := s.Store.SavePortMapping(definition); err != nil {
		return err
	}
	s.Audit.Record(ctx, "portmap.write", "", policy.CapTunnel, audit.SafeParameters(portMappingParameters(definition)), time.Now(), nil)
	return nil
}

func (s *Service) StartPortMapping(ctx context.Context, id string) (model.PortMapping, error) {
	if err := s.Authorize(ctx, "portmap.open", policy.CapTunnel); err != nil {
		return model.PortMapping{}, err
	}
	definition, err := s.Store.GetPortMapping(id)
	if err != nil {
		return definition, err
	}
	if err := s.validateTarget(definition.TargetHost, definition.TargetPort); err != nil {
		return definition, err
	}
	started := time.Now()
	result, err := s.PortMappings.Start(ctx, id)
	result.RuntimePort, result.ViaRoute = s.PortMappings.RuntimeInfo(id)
	s.Audit.Record(ctx, "portmap.open", "", policy.CapTunnel, audit.SafeParameters(portMappingParameters(result)), started, err)
	return result, err
}

func (s *Service) StopPortMapping(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "portmap.close", policy.CapTunnel); err != nil {
		return err
	}
	err := s.PortMappings.Stop(id)
	s.Audit.Record(ctx, "portmap.close", id, policy.CapTunnel, "", time.Now(), err)
	return err
}

func (s *Service) DeletePortMapping(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "portmap.delete", policy.CapTunnel); err != nil {
		return err
	}
	err := s.PortMappings.Delete(id)
	s.Audit.Record(ctx, "portmap.delete", id, policy.CapTunnel, "", time.Now(), err)
	return err
}

func (s *Service) validateTarget(host string, port int) error {
	settings := s.Settings()
	if parseBool(settings["allow_public_targets"]) {
		return nil
	}
	if host == "localhost" || strings.HasSuffix(strings.ToLower(host), ".local") || strings.HasSuffix(strings.ToLower(host), ".lan") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		return nil
	}
	if ip == nil {
		addresses, lookupErr := net.LookupIP(host)
		if lookupErr != nil {
			return apperrors.New(apperrors.TargetForbidden, "cannot verify target address; public target is disabled")
		}
		for _, address := range addresses {
			if address.IsLoopback() || address.IsPrivate() {
				return nil
			}
		}
		return apperrors.New(apperrors.TargetForbidden, "public target is disabled by default")
	}
	return apperrors.New(apperrors.TargetForbidden, "public target is disabled by default")
}

func (s *Service) OpenWeb(ctx context.Context, nodeID, targetHost string, targetPort int) (string, error) {
	if isLocalNodeID(nodeID) {
		return "", apperrors.New(apperrors.NotSupported, "local node cannot open SSH web proxy")
	}
	if err := s.Authorize(ctx, "service.open_web", policy.CapTunnel); err != nil {
		return "", err
	}
	if err := s.validateTarget(targetHost, targetPort); err != nil {
		return "", err
	}
	definition := model.Tunnel{
		ID: fmt.Sprintf("web-%d", time.Now().UnixNano()), NodeID: nodeID,
		Name: fmt.Sprintf("Web %s:%d", targetHost, targetPort), Type: "local",
		ListenAddress: "127.0.0.1", ListenPort: 0, TargetHost: targetHost, TargetPort: targetPort,
		AutoStart: false, Status: "stopped",
	}
	if err := s.Store.SaveTunnel(definition); err != nil {
		return "", err
	}
	started := time.Now()
	_, err := s.Tunnels.Start(ctx, definition.ID)
	s.Audit.Record(ctx, "service.open_web", nodeID, policy.CapTunnel, audit.SafeParameters(map[string]string{"target": fmt.Sprintf("%s:%d", targetHost, targetPort)}), started, err)
	if err != nil {
		return "", err
	}
	return "/proxy/" + definition.ID + "/", nil
}

func (s *Service) Overview() model.Overview {
	nodes, _ := s.ListNodes()
	tunnels, _ := s.Tunnels.List()
	connected := 0
	for _, node := range nodes {
		if node.Online {
			connected++
		}
	}
	running := 0
	for _, definition := range tunnels {
		if definition.Status == "running" {
			running++
		}
	}
	return model.Overview{
		Version: Version, UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Nodes: len(nodes), Connected: connected, Tunnels: len(tunnels), Running: running,
		MCPEnabled: parseBool(s.Settings()["mcp_enabled"]),
	}
}

func (s *Service) AuditEvents(limit, offset int) ([]model.AuditEvent, error) {
	return s.Audit.List(limit, offset)
}

func (s *Service) SetHostTrust(nodeID, fingerprint string) error {
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		return err
	}
	node.HostKeyFingerprint = fingerprint
	return s.Store.SaveNode(node)
}

func JSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
