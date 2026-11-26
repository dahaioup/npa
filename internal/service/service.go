package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/npa/npa/internal/audit"
	"github.com/npa/npa/internal/autostart"
	"github.com/npa/npa/internal/browser"
	"github.com/npa/npa/internal/desktop"
	"github.com/npa/npa/internal/discovery"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/file"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/peer"
	"github.com/npa/npa/internal/plugin"
	"github.com/npa/npa/internal/pluginmanager"
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
	Store            *state.Store
	SSH              *sshx.Manager
	Files            *file.Service
	Tunnels          *tunnel.Manager
	Audit            *audit.Logger
	Desktop          *desktop.Router
	Plugin           *plugin.Manager
	FilePlugin       *plugin.Manager
	filePluginMu     sync.Mutex
	fileServer       *file.Server // 文件插件自带的 HTTP 服务（节点文件面 + 插件前端页面）
	Browser          *browser.Router
	BrowserPlugin    *plugin.Manager
	BrowserInstaller *browser.Installer
	PluginManager    *plugin.Manager
	// PluginManagerService 是插件管理器能力的门面：内置实现始终可用，
	// 外部插件进程在跑时接管。
	PluginManagerService *pluginmanager.Service
	pluginManagerMu      sync.Mutex
	pluginManagerArgs    string
	Discovery            *discovery.Manager
	Relay                *relay.Manager
	PortMappings         *portmap.Manager
	Peer                 *peer.Hub
	PeerAgent            *peer.Agent
	Domain               *peer.DomainHub
	autoUppersMu         sync.Mutex
	autoUppers           []peer.Candidate // 上级网络自动发现的最近结果（缓存 10 分钟刷新）
	yieldMu              sync.Mutex       // 让位检查/双向让位仲裁串行化
	managerSyncedMu      sync.Mutex       // 保护 managerSyncedVer
	managerSyncedVer     int64            // 最近一次成功从管理节点同步到的数据版本（/api/info 暴露给前端）
	resourceDir          string
	started              time.Time
	stop                 chan struct{}
	stopOnce             sync.Once
	requireInflight      sync.Map // nodeID -> *sync.WaitGroup：同目标在途 require 去重
	holderCooldown       sync.Map // holderAddress -> time.Time：require 失败冷却
	screenTransMu        sync.Mutex
	screenTransports     map[string]*http.Transport // nodeID -> 跨域代理管道 keep-alive 传输层
	browserInstallMu     sync.Mutex                 // 串行化 obscura 安装动作
	browserPluginMu      sync.Mutex                 // 保护插件进程重启
	browserPluginArgs    string                     // 上次启动插件进程时用的参数（变了就重启插件）
	downloadTokens       map[string]downloadToken
	mu                   sync.RWMutex
	limiter              map[string]*rateWindow
	OnSettingsChanged    func()
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
		screenTransports: map[string]*http.Transport{},
	}
	// 桌面控制插件（npa-remote）：core 用 plugin 模式调它（stdio JSON-RPC），
	// 截图/键鼠/剪贴板因此走插件实现而不是内嵌脚本；serve 模式由 sidecar 另起
	// 一个进程给局域网浏览器用，两者互不冲突。
	app.Plugin = plugin.NewManager(filepath.Join("plugins", "desktop"))
	app.Plugin.PreferredName = "npa-remote"
	app.Plugin.SearchDirs = desktopPluginDirs(store.DataDir())
	app.Plugin.Args = []string{"plugin"}
	app.Plugin.InitTimeout = 30 * time.Second
	app.FilePlugin = plugin.NewManager(filepath.Join("plugins", "file"))
	// 文件能力做成内置插件：外部插件进程在跑时接管文件操作，
	// 插件缺失时由内置实现（Builtin）兜底。
	fileService.AttachPlugin(file.NewPluginProvider(app.FilePlugin))
	app.Desktop = desktop.NewRouterWithRoot(app.Plugin, resourceDir)
	// 浏览器能力：obscura 插件进程优先，缺失时回落到 obscura CLI。
	// 插件放在 plugins/browser 目录，二进制名 npa-obscura。
	app.BrowserPlugin = plugin.NewManager(filepath.Join("plugins", "browser"))
	app.BrowserPlugin.PreferredName = "npa-obscura"
	app.BrowserPlugin.SearchDirs = browserPluginDirs(store.DataDir())
	// 插件进程握手默认只有 5s，机器负载高时（磁盘慢、并发编译）会超时，
	// 这里给浏览器插件一个更宽的握手窗口。
	app.BrowserPlugin.InitTimeout = 30 * time.Second
	app.BrowserPlugin.Args = app.BrowserPluginArgs()
	app.BrowserInstaller = &browser.Installer{DataDir: store.DataDir()}
	app.Browser = browser.NewRouter(app.BrowserPlugin, app.BrowserConfig)
	// 插件管理器：内置插件（始终可用、不可禁用）+ 可选的外部覆盖进程。
	app.PluginManager = plugin.NewManager(filepath.Join("plugins", "manager"))
	app.PluginManager.PreferredName = "npa-plugin-manager"
	app.PluginManager.SearchDirs = pluginManagerDirs(store.DataDir())
	app.PluginManager.InitTimeout = 30 * time.Second
	app.PluginManager.Args = app.PluginManagerArgs()
	// 内置实现随 core 编译、始终可用；外部进程在跑时接管仓库操作。
	app.PluginManagerService = pluginmanager.NewService(store.DataDir(), app.PluginRegistryURL)
	app.PluginManagerService.Builtin.CallTimeout = pluginManagerTimeout
	app.PluginManagerService.AttachPlugin(pluginmanager.NewPluginProvider(app.PluginManager))
	app.Discovery = discovery.NewManager(store, Version)
	app.Relay = relay.NewManager(store, sshManager)
	app.PortMappings = portmap.NewManager(store, app.Relay)
	tunnelManager.DialTarget = app.DialNode // Local/SOCKS 隧道也能走 peer 反向链路
	tunnelManager.IsPeer = func(nodeID string) bool { return app.isPeerNode(nodeID) }
	fileService.Builtin.IsPeer = func(nodeID string) bool { return app.isPeerNode(nodeID) }
	// 必须用闭包延迟取值：wirePeer 在之后才创建 app.Peer
	fileService.Builtin.PeerFS = func(ctx context.Context, nodeID string) (*sftp.Client, func(), error) {
		return app.Peer.FS(ctx, nodeID)
	}
	// HTTP 文件面随文件插件迁到插件自己的端口（默认 56751）：
	// 反向接入节点用注册上报的地址，同网段只靠广播互见的节点用广播地址，
	// 都直接打对端的文件插件端口（大文件不走 SFTP-over-WS 通道）。
	fileService.Builtin.PeerHTTPBase = func(nodeID string) string { return app.fileBaseDirect(nodeID) }
	// 跨域兜底：经持有上级代理拨到节点自己的文件插件端口，HTTP 文件面照样可用。
	fileService.Builtin.RelayDial = func(ctx context.Context, id string) (net.Conn, error) {
		return app.dialNodeFile(ctx, id)
	}
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
	// uptime 从进程启动算起：/api/info 实时上报，选举前由对端获取比较
	domainStart := time.Now()
	s.Peer = peer.NewHub(s.Store, peer.Options{
		Version:   Version,
		Name:      hostname,
		Port:      func() int { return meshPort },
		FilePort:  func() int { return s.filePluginPort() },
		Subnets:   discovery.LocalSubnets,
		AutoTrust: func() bool { return parseBool(s.Settings()["peer_auto_trust"]) },
		// 拓扑增量视图（docs/plans/topology-sync.md）：本机作为管理者时，
		// 注册/心跳写进来，成员拉增量、域间批量推送从这里出。
		View: peer.NewTopologyView(model.NodeID()),
		EphemeralIdle: func() time.Duration {
			return time.Duration(intSetting(s.Settings()["peer_on_demand_idle"], 60)) * time.Second
		},
		OnRegister: func(node model.Node) {
			s.Audit.Record(context.Background(), "peer.register", node.ID, policy.CapBase,
				audit.SafeParameters(map[string]string{"address": node.Address, "source": node.PeerSource, "subnets": strings.Join(node.ReachableSubnets, ",")}),
				time.Now(), nil)
		},
		// 跨上级查拓扑时，别人问"你是谁、你下面有谁"
		Self: func() peer.TopoNode { return s.topologySelf() },
		// 本机运行时长——选举前对端实时获取
		Uptime: func() int64 { return int64(time.Since(domainStart) / time.Second) },
		// 选举出的管理节点——/api/info 暴露，注册时用于重定向
		ManagerInfo: func() (string, string) {
			if s.Domain == nil {
				return "", ""
			}
			return s.Domain.ManagerID(), s.Domain.ManagerAddress()
		},
		// 最近一次从管理节点同步到的数据版本——前端给自己显示 在线(Vn)
		SyncedVersion: s.getManagerSynced,
	})
	s.PeerAgent = peer.NewAgent(peer.AgentOptions{
		Version: Version,
		NodeID:  func() string { return model.NodeID() },
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
		FilePort:     func() int { return s.filePluginPort() },
		Dial:         s.peerDial,
		SSHRoutes:    s.sshRouteHints,
		OnError:      logPeerError,
	})
	// relay 通过 peer 数据面访问反向接入的节点；跨上级协调由 service 自己实现
	s.Relay.SetPeerDialer(s.Peer)
	s.Relay.SetUpperCoordinator(s)
	s.Peer.StartSweeper() // 临时链路空闲回收（peer_on_demand_idle）
	// 分域 v2：本机同时带管理者与成员两个角色
	s.Domain = peer.NewDomainHub(func() peer.DomainMember {
		return peer.DomainMember{
			ID:      model.NodeID(),
			Name:    hostname,
			Address: net.JoinHostPort(discovery.PrimaryIP(), strconv.Itoa(meshPort)),
			Port:    meshPort,
			Uptime:  int64(time.Since(domainStart) / time.Second),
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
	// 拓扑增量同步（docs/plans/topology-sync.md）：清扫/拉取/推送三个循环
	s.startTopologySync(context.Background())
	// npa-remote sidecar：插件二进制存在且 56750 空闲时拉起 serve 模式
	// （必须由交互会话里的 NPA 拉起，schtasks 会话拿不到 DXGI 桌面复制）。
	s.StartRemoteSidecar(context.Background())
	// 上级网络自动发现（peer_auto_discover_uppers，默认开）：无种子时走路由
	// TTL 探测找上游内网网段并受限探测；结果缓存供 candidateUppers/选举使用。
	go s.refreshAutoUppersLoop(context.Background())
	// 启动默认自己是管理节点；mDNS 发现新的同域节点时做让位检查
	// （checkYieldToNewNode：对方运行更久就把管理权限交给它），此后只在
	// 管理节点丢失时重选。
	if s.Domain != nil {
		s.Domain.SetElectedManager(peer.DomainMember{
			ID:      model.NodeID(),
			Address: net.JoinHostPort(discovery.PrimaryIP(), strconv.Itoa(meshPort)),
		})
		// 启动即自我认定为管理节点——出参打标 self 记录
		if view := s.Peer.View(); view != nil {
			view.SetSelfIsManager(true)
		}
	}
	// 新同域节点接入 → 管理节点让位检查（异步，不阻塞 mDNS 轮询）
	if s.Discovery != nil {
		s.Discovery.SetOnNodeDiscovered(func(nodeID string) {
			go s.checkYieldToNewNode(nodeID)
		})
	}
	// mDNS 广播 role/children/peer/file，便于同网段的其它上级发现我们并查询拓扑；
	// file= 让只靠广播互见的节点也能直连我们的文件插件端口。
	if s.Discovery != nil {
		s.Discovery.SetLocalInfo(func() int {
			if s.Settings()["peer_enabled"] != "true" {
				return 0
			}
			return meshPort
		}, func() int { return len(s.registeredChildren()) })
		s.Discovery.SetFilePort(func() int { return s.filePluginPort() })
	}
}

// startTopologySync 启动拓扑增量同步的三个循环（docs/plans/topology-sync.md）：
//  1. 清扫（仅管理者视角有效）：90s 判 offline、24h 墓碑剔除；
//  2. 成员拉取：每 30s 向当选管理者 GET /api/topology/sync?since=（兼心跳），
//     自己是管理者则跳过（本机 view 由 Hub 注册/心跳直接驱动）；
//  3. 域间推送：每 30s 把 view 增量推给上级候选/当选管理者（空批次也推 = 活性），
//     连续 3 次失败交由上层重注册（规则 8，重注册机制后续接）。
func (s *Service) startTopologySync(ctx context.Context) {
	view := s.Peer.View()
	if view == nil {
		return
	}
	// 本机先入视图：成员副本也要有 self 记录（全量快照不缺自己）
	nodeHostname, _ := os.Hostname()
	meshAddress := net.JoinHostPort(discovery.PrimaryIP(), strconv.Itoa(meshPort))
	view.SetSelfAddress(meshAddress)
	if s.Domain != nil {
		view.ManagerInfo = func() (string, string) {
			return s.Domain.ManagerID(), s.Domain.ManagerAddress()
		}
	}
	view.Upsert(peer.TopoNode{
		ID: model.NodeID(), Name: nodeHostname, Address: meshAddress, Source: "self",
	}, true)

	isManager := func() bool {
		if s.Domain == nil {
			return true
		}
		managerID := s.Domain.ManagerID()
		return managerID == "" || managerID == model.NodeID()
	}

	// 1. 清扫循环
	go func() {
		ticker := time.NewTicker(peer.TopoPullInterval)
		defer ticker.Stop()
		wasManager := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if removed := view.Sweep(); len(removed) > 0 {
					slog.Info("topology tombstone", "removed", removed)
				}
				// 升为管理节点的瞬间：把拉来的副本收编为本域自有——
				// 此后本机对本域成员的存活负责（Sweep 判死、增量下发），
				// 否则死节点的副本永远不被判死。
				mgr := isManager()
				if mgr && !wasManager {
					view.ClaimAll()
				}
				// 身份出参打标：每 tick 同步（变化时 view 内部 bump 推送）
				view.SetSelfIsManager(mgr)
				wasManager = mgr
				// 周期重选：选举只在 mDNS 新节点出现时触发，节点换 ID（升级后
				// 换成硬件派生 ID）或记录被清理后，管理节点会一直停留在旧值。
				// 每 tick 重算一次（选举是确定性的，各节点算出同一结果）。
				s.reelectDomainManager()
			}
		}
	}()

	// 2. 拉取循环（兼心跳）
	//
	//   普通节点：只向**本域管理节点**拉取本域拓扑表（增量）——不 push、不维护本域表；
	//   管理节点：向**自己的上级**拉取完整表（增量），本域表由本机维护。
	//
	// 拉取基线（since）在对方的版本空间里，所以每条关系各记一个游标：
	//   managerSince  本域管理节点的版本
	//   upstreamSince 上级的版本
	//
	// 管理者拉取失败连续 topoManagerFailLimit 次视为管理节点丢失 → 重新选举；
	// 管理者答复 NotManager → 改指新管理者。
	go func() {
		ticker := time.NewTicker(peer.TopoPullInterval)
		defer ticker.Stop()
		failures := 0
		var managerSince, upstreamSince int64
		upstream := ""    // 当前使用的上级地址（对方不是管理者时改指其管理者）
		upstreamFor := "" // upstreamSince 所属的上级地址：游标在对方的版本空间里，
		// 换了对方就必须清零重新全量，否则拿 A 的版本号向 B 要增量只会拿到空集
		// （158 视图冻结的根因）。
		lastManager := "" // managerSince 同理，随管理节点切换清零
		pulls := 0        // 拉取轮次：定期强制全量，兜住版本空间错位造成的永久缺口
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.Domain == nil {
					continue
				}
				pulls++
				// 定期全量重拉（每 12 轮 ≈ 6 分钟）：对方的增量窗口按它自己的
				// per-record 版本过滤，而本地的游标是对方的全局版本——两套版本
				// 空间不同源，某个节点一旦被窗口漏掉就永远补不回来（192 曾在
				// 165 上永久缺失）。定期全量保证最终收敛。
				forceFull := pulls%12 == 0
				if isManager() {
					// 管理节点：向上级拉取完整表（增量）
					targets := s.upstreamTopologyTargets()
					if upstream != "" {
						targets = append([]string{upstream}, targets...)
					}
					for _, target := range targets {
						since := upstreamSince
						if target != upstreamFor || forceFull {
							since = 0 // 换上级 = 换版本空间，重新全量
						}
						resp, err := fetchTopologySync(ctx, target, since,
							model.NodeID(), view.SelfAddress(), true)
						if err != nil {
							slog.Warn("topology upstream fetch failed", "upstream", target, "err", err)
							continue
						}
						if resp.NotManager {
							if resp.ManagerAddr != "" {
								slog.Info("upstream demoted, switching", "old", target, "new", resp.ManagerAddr)
								upstream = resp.ManagerAddr
							}
							continue
						}
						upstream = target
						upstreamFor = target
						upstreamSince = resp.Version
						view.ApplySync(resp)
						// 反熵校验：对方应答带 ID 清单，本地比对若发现
						// 「对方有、自己没有」的节点，说明增量窗口漏了它
						// （对方按 per-record 版本过滤，本地却拿对方的全局
						// 版本当游标——两套版本空间不同源）。漏掉的节点永远
						// 补不回来，必须把游标清零强制下一次全量。
						if missing := view.MissingIDs(resp.IDs); len(missing) > 0 {
							slog.Warn("upstream sync missing nodes, forcing full resync",
								"upstream", target, "missing", missing)
							upstreamSince = 0
						}
						// 一个 tick 只采用第一个成功响应的上游：多个上游交替
						// 全量会互相清表（后到的小全量清掉先到的大表），且单游标
						// 在多 target 间乒乓导致每轮都重置 since=0 反复全量。
						break
					}
					continue
				}
				// 普通节点：只向本域管理节点拉取
				manager := s.Domain.ManagerAddress()
				if manager == "" {
					continue
				}
				if manager != lastManager || forceFull {
					managerSince = 0 // 换管理节点 = 换版本空间，重新全量
					lastManager = manager
				}
				// 同域（mDNS 可见）拉取 → 与管理节点平级；跨域（seeds/重定向）
				// 拉取 → 挂到管理节点名下（注册下级语义）。
				attach := !s.sameDomainTarget(manager)
				resp, err := fetchTopologySync(ctx, manager, managerSince,
					model.NodeID(), view.SelfAddress(), attach)
				if err != nil {
					failures++
					slog.Warn("topology sync fetch failed", "manager", manager, "failures", failures, "err", err)
					if failures >= peer.TopoManagerFailLimit {
						slog.Warn("manager lost, re-electing", "manager", manager)
						failures = 0
						s.ElectDomainManager()
					}
					continue
				}
				failures = 0
				if resp.NotManager {
					slog.Info("manager demoted, switching", "old", manager, "new", resp.ManagerAddr)
					// 覆盖前捕获本机此前实际跟随的管理节点，供三方裁决用
					prevMgrID, prevMgrAddr := s.Domain.ManagerID(), s.Domain.ManagerAddress()
					s.Domain.SetElectedManager(peer.DomainMember{ID: resp.ManagerID, Address: resp.ManagerAddr})
					// 对方已把管理权让给我：三方裁决（自己 / 推送禅让的节点 /
					// 此前实际管理节点）取运行最久者，避免禅让被陈旧信念或
					// 字典序否决，也避免直接接受禅让时丢掉更长运行的现任管理者。
					if resp.ManagerID == model.NodeID() {
						s.arbitrateYieldTo(manager, prevMgrID, prevMgrAddr)
					}
					continue
				}
				managerSince = resp.Version
				s.setManagerSynced(resp.Version)
				view.ApplySyncFromManager(resp)
				// 反熵校验（见上游分支注释）：管理节点应答带 ID 清单，
				// 本地比对发现缺口就说明增量窗口漏了节点——把游标清零，
				// 下一轮强制全量补齐。缺口一旦形成就永远补不回来。
				if missing := view.MissingIDs(resp.IDs); len(missing) > 0 {
					slog.Warn("manager sync missing nodes, forcing full resync",
						"manager", manager, "missing", missing)
					managerSince = 0
				}
			}
		}
	}()

	// 3. 域间推送循环（规则 7）：只有管理节点向外推送，且只推**本域自有内容**
	//    （本域 + 本域所有下级域）；目标是上级（跨域），同域节点不互推。
	go func() {
		ticker := time.NewTicker(peer.TopoPushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !isManager() {
					continue // 只有管理者向外推送本域视图
				}
				for _, address := range s.upstreamTopologyTargets() {
					view.PushBatchTo(address, model.NodeID(), postTopologyPush)
				}
			}
		}
	}()
}

// fetchTopologySync 成员侧拉取管理者增量。
// fetchTopologySync 成员侧拉取管理者增量。
// attach=true 表示跨域拉取（注册下级拉上级），触达时挂到目标名下；
// attach=false 同域拉取，触达时与管理节点同级。
func fetchTopologySync(ctx context.Context, manager string, since int64, selfID, selfAddr string, attach bool) (peer.TopologySyncResponse, error) {
	attachArg := ""
	if attach {
		attachArg = "&attach=1"
	}
	var resp peer.TopologySyncResponse
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://%s/api/topology/sync?since=%d&id=%s&addr=%s%s", manager, since,
			url.QueryEscape(selfID), url.QueryEscape(selfAddr), attachArg), nil)
	if err != nil {
		return resp, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return resp, err
	}
	defer response.Body.Close()
	err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&resp)
	return resp, err
}

// postTopologyPush 推送 POST 辅助（topology.PushBatchTo 的传输函数）。
func postTopologyPush(url string, payload []byte) ([]byte, int, error) {
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	return body, response.StatusCode, err
}

// ElectDomainManager 用当前候选视图跑一次分域选举。
// 候选 = mDNS 发现的邻居 + 已注册下级 + 自己。
// 选举前实时获取各候选的运行时长（GET /api/info），保证跨节点可比。
func (s *Service) ElectDomainManager() {
	if s.Domain == nil {
		slog.Warn("ElectDomainManager: Domain is nil")
		return
	}
	candidates := s.domainCandidates()
	s.fillCandidateUptimes(candidates)
	slog.Info("ElectDomainManager", "candidates", len(candidates))
	s.Domain.Elect(candidates)
	// 选出管理节点后，同步它的下级列表到本机拓扑
	go s.SyncManagerChildren(context.Background(), s.Domain.ManagerID(), s.Domain.ManagerAddress())
}

// reelectDomainManager 周期重选管理节点（每 30s 一次）：选举只在 mDNS 新节点
// 出现时触发，节点升级换 ID 或记录被清理后，管理节点会一直停留在旧值。
// 选举本身是确定性的（CrossDomain > BootAt 早者胜 > NodeID），各节点算出同一结果。
// 只有管理节点真的变了才同步它的下级列表，避免每 tick 重复拉取。
func (s *Service) reelectDomainManager() {
	if s.Domain == nil {
		return
	}
	before := s.Domain.ManagerID()
	candidates := s.domainCandidates()
	// 必须实时拉候选的运行时长：mDNS TXT 的 up= 只是注册瞬间快照（恒≈0），
	// 不填的话候选全是 0，选举会永远选自己（自认管理者的分裂）。
	s.fillCandidateUptimes(candidates)
	s.Domain.Elect(candidates)
	after := s.Domain.ManagerID()
	if after != before {
		slog.Info("domain manager changed", "from", before, "to", after)
		go s.SyncManagerChildren(context.Background(), after, s.Domain.ManagerAddress())
	}
}

// fillCandidateUptimes 选举前并发拉取各候选 /api/info 取运行时长。
// 拉不到的保持原值（0），按选举序排后；self 的时长由 DomainHub 现算，不经此路。
func (s *Service) fillCandidateUptimes(candidates []peer.DomainMember) {
	if len(candidates) == 0 {
		return
	}
	client := &http.Client{Timeout: 2 * time.Second}
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Add(1)
		go func(c *peer.DomainMember) {
			defer wg.Done()
			resp, err := client.Get("http://" + c.Address + "/api/info")
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var info peer.Info
			if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info); err != nil {
				return
			}
			if info.Uptime > 0 {
				c.Uptime = info.Uptime
			}
		}(&candidates[i])
	}
	wg.Wait()
}

// fetchPeerInfo 拉取对端 /api/info（让位检查/选举前取运行时长用）。
func fetchPeerInfo(addr string) (peer.Info, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/api/info")
	if err != nil {
		return peer.Info{}, err
	}
	defer resp.Body.Close()
	var info peer.Info
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info); err != nil {
		return peer.Info{}, err
	}
	return info, nil
}

// checkYieldToNewNode 管理节点让位检查：mDNS 发现新的同域节点时，实时获取
// 对方运行时长与本机对比，对方更久就把管理权限交给对方——之后成员向本机
// pull 会收到 NotManager + 新管理者地址，成员侧已有路径自动跟随。
func (s *Service) checkYieldToNewNode(nodeID string) {
	if s.Domain == nil || s.Discovery == nil {
		return
	}
	s.yieldMu.Lock()
	defer s.yieldMu.Unlock()
	selfID := model.NodeID()
	if nodeID == "" || nodeID == selfID || s.Domain.ManagerID() != selfID {
		return // 本机不是管理节点，不参与让位
	}
	// 「选举前获取所有节点的运行时长，按运行最久当选」：发现新节点时对全部
	// 已知同域节点重新评估取最长者——先到先让会把管理权交给第一个被发现的
	// 节点而非最长者（实测：158 启动让给了 1048s 的 39，而不是 3975s 的 165，
	// 随后链式回弹全员停在 uptime 最短的 158）。
	candidates := make([]peer.DomainMember, 0, 8)
	discovered, err := s.Discovery.Nodes()
	if err != nil {
		slog.Warn("yield check: list discovered failed", "err", err)
		return
	}
	for _, node := range discovered {
		if node.ID == "" || node.ID == selfID || node.Address == "" {
			continue
		}
		port := node.PeerPort
		if port <= 0 {
			port = meshPort
		}
		candidates = append(candidates, peer.DomainMember{
			ID:      node.ID,
			Name:    node.Name,
			Address: net.JoinHostPort(node.Address, strconv.Itoa(port)),
		})
	}
	if len(candidates) == 0 {
		return
	}
	s.fillCandidateUptimes(candidates) // 并发 GET /api/info 取实时运行时长
	var best peer.DomainMember
	found := false
	for _, c := range candidates {
		if !found || peer.ElectionBeats(c, best) {
			best, found = c, true
		}
	}
	if !found {
		return
	}
	s.maybeYieldManager(best.ID, best.Address, best.Uptime)
}

// maybeYieldManager 按选举序对比：对方胜过本机（运行更久；同长则 NodeID
// 字典序更小）才让位。
func (s *Service) maybeYieldManager(otherID, otherAddr string, otherUptime int64) {
	selfID := model.NodeID()
	if otherID == "" || otherID == selfID || otherAddr == "" {
		return
	}
	mine := int64(time.Since(s.started) / time.Second)
	other := peer.DomainMember{ID: otherID, Address: otherAddr, Uptime: otherUptime}
	self := peer.DomainMember{ID: selfID, Uptime: mine}
	if !peer.ElectionBeats(other, self) {
		return
	}
	slog.Info("yielding manager role", "to", otherID, "addr", otherAddr,
		"other_uptime", otherUptime, "my_uptime", mine)
	s.Domain.SetElectedManager(other)
}

// arbitrateYieldTo 三方裁决：本机收到 NotManager 且指向自己（即对方把管理权
// 禅让给我）时，不直接接受，而是在三方之间按选举序取最优——
//  1. 自己；
//  2. 推送禅让的节点（刚向本机 pull 的对方）；
//  3. 本机此前实际跟随的管理节点（覆盖前捕获，防止接受禅让时丢掉一个
//     比双方都运行更久的现任管理者）。
//
// 选举序 = 运行更久者胜，同长 NodeID 字典序兜底（与 DomainHub.Elect 同规则，
// 双方实测同样两个数字，裁决一致不会来回翻）。不能只按字典序：陈旧信念会
// 指着刚重启的短 uptime 节点，字典序仲裁会把刚做出的 uptime 让位否决掉
// （实测 158 让位给 37761s 的 125 后被字典序弹回，全员钉死在最短 uptime 上）。
// 当前流程里禅让方与此前管理节点是同一对象（pull 的就是当前信念），ID 相同
// 去重后退化为两方；两者不同时三方各占一席。
func (s *Service) arbitrateYieldTo(yielderAddr, prevMgrID, prevMgrAddr string) {
	selfID := model.NodeID()
	mine := int64(time.Since(s.started) / time.Second)
	candidates := []peer.DomainMember{{ID: selfID, Uptime: mine}}
	seen := map[string]bool{selfID: true}
	add := func(id, addr string, uptime int64) {
		if id == "" || addr == "" || seen[id] {
			return
		}
		seen[id] = true
		candidates = append(candidates, peer.DomainMember{ID: id, Address: addr, Uptime: uptime})
	}
	// 禅让方：实时取它的 ID 与运行时长
	if info, err := fetchPeerInfo(yielderAddr); err == nil && info.ID != "" {
		add(info.ID, yielderAddr, info.Uptime)
	}
	// 此前实际管理节点（与禅让方不同才单独取；拉不到按 0 排后）
	if prevMgrID != "" && !seen[prevMgrID] && prevMgrAddr != "" {
		uptime := int64(0)
		if info, err := fetchPeerInfo(prevMgrAddr); err == nil && info.Uptime > 0 {
			uptime = info.Uptime
		}
		add(prevMgrID, prevMgrAddr, uptime)
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if peer.ElectionBeats(c, best) {
			best = c
		}
	}
	if best.ID == selfID {
		slog.Info("arbitrate: keep self as manager", "yielder", yielderAddr,
			"prev_manager", prevMgrID, "candidates", len(candidates), "my_uptime", mine)
		return
	}
	slog.Info("arbitrate: yield to winner", "to", best.ID, "addr", best.Address,
		"uptime", best.Uptime, "my_uptime", mine, "candidates", len(candidates))
	s.Domain.SetElectedManager(best)
}

// SyncManagerChildren 从管理节点拉取下级列表，把下下级节点标记为 discovered。
// 同级节点调用——让拓扑看到「管理者的下级」。
func (s *Service) SyncManagerChildren(ctx context.Context, managerID, managerAddr string) {
	// 跳过自己——自己是管理节点时不需要拉
	selfID := model.NodeID()
	if managerID == "" || managerID == selfID || managerAddr == "" {
		return
	}
	url := fmt.Sprintf("http://%s/api/nodes", managerAddr)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	var nodes []model.Node
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return
	}
	for _, child := range nodes {
		if child.ID == "" || child.ID == selfID {
			continue
		}
		// 已有记录（store 里的）跳过
		if _, err := s.GetNode(child.ID); err == nil {
			continue
		}
		// 写入 discovered 节点
		child.Protocol = "discovered"
		child.Discovered = true
		_ = s.SaveNode(context.Background(), child, model.Credential{})
		slog.Info("synced manager child", "manager", managerID, "child", child.ID, "child_name", child.Name)
	}
}

// domainCandidates 分域管理者选举的候选视图：mDNS 发现的同网段 NPA + 已注册下级。
func (s *Service) domainCandidates() []peer.DomainMember {
	selfID := model.NodeID()
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
	holders := s.HoldersForNode(ctx, nodeID)
	if len(holders) == 0 {
		return nil, apperrors.New(apperrors.NodeOffline, "拓扑里找不到该节点的持有上级")
	}
	var lastErr error
	tried := 0
	for _, holder := range holders {
		if raw, cooling := s.holderCooldown.Load(holder); cooling {
			if until, _ := raw.(time.Time); time.Now().Before(until) {
				continue // 冷却中的候选直接跳过，试下一个
			}
			s.holderCooldown.Delete(holder)
		}
		tried++
		conn, err := s.DialViaUpper(ctx, holder, nodeID, target, "")
		if err == nil {
			s.holderCooldown.Delete(holder)
			return conn, nil
		}
		s.holderCooldown.Store(holder, time.Now().Add(holderCooldownWindow))
		lastErr = err
	}
	if tried == 0 {
		return nil, apperrors.New(apperrors.NodeOffline, "所有候选持有者都在协商冷却中")
	}
	return nil, lastErr
}

// dialNodeMesh 经反向链路或跨上级代理拨到节点自己的 mesh 端口（127.0.0.1:56701）。
// 拿到的是节点 exec/协调端口的原始 TCP 连接。
func (s *Service) dialNodeMesh(ctx context.Context, nodeID string) (net.Conn, error) {
	return s.DialNode(ctx, nodeID, net.JoinHostPort("127.0.0.1", strconv.Itoa(meshPort)))
}

// dialNodeFile 经反向链路或跨上级代理拨到节点自己的文件插件端口
// （127.0.0.1:<file_port>）。文件面整体迁到文件插件后，跨域代理也走这个端口。
func (s *Service) dialNodeFile(ctx context.Context, nodeID string) (net.Conn, error) {
	return s.DialNode(ctx, nodeID, net.JoinHostPort("127.0.0.1", strconv.Itoa(s.filePortOf(nodeID))))
}

func intSetting(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

// filePluginPort 返回本机文件插件 HTTP 服务的端口（设置项 file_plugin_port，
// 默认 56751，见 docs/plugin-ports.md）。
func (s *Service) filePluginPort() int {
	return s.PluginHTTPPort(PluginSlotFile)
}

// filePluginScope 返回文件插件的绑定范围：跟随插件自己的端口开放度
// （file_plugin_exposure：auto 跟随局域网开关、local 只听回环、lan 听所有网卡），
// 与 docs/plugin-ports.md 规则 2 一致。
func (s *Service) filePluginScope() string {
	return s.PluginScope(PluginSlotFile)
}

// meshBindScope 返回节点通信端口（56701）的监听范围：跟随局域网服务开关。
func (s *Service) meshBindScope() string {
	if !parseBool(s.Settings()["lan_enabled"]) {
		return "local"
	}
	return "lan"
}

// filePortOf 返回目标节点的文件插件端口：对端自报的文件端口优先，
// 缺失时回落到本机配置的默认端口（两端配置一致时成立）。
func (s *Service) filePortOf(nodeID string) int {
	if node, ok := s.lookupNode(nodeID); ok && node.FilePort > 0 {
		return node.FilePort
	}
	return s.filePluginPort()
}

// lookupNode 查节点：先主表（注册过的反向接入节点），再发现表（mDNS 互见但没注册的）。
func (s *Service) lookupNode(nodeID string) (model.Node, bool) {
	if node, err := s.Store.GetNode(nodeID); err == nil {
		return node, true
	}
	if s.Discovery != nil {
		if node, ok := s.Discovery.Find(nodeID); ok {
			return node, true
		}
	}
	return model.Node{}, false
}

// meshBaseDirect 返回节点自己 mesh HTTP 端口的直连基址。
// 反向接入节点用注册上报的地址；同网段只靠广播互见的节点没有握手注册，
// 用广播里的地址直接访问它的 /api/exec；跨域节点不在直连视图里返回空。
func (s *Service) meshBaseDirect(nodeID string) string {
	node, ok := s.lookupNode(nodeID)
	if !ok {
		return ""
	}
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

// fileBaseDirect 返回节点文件插件 HTTP 端口的直连基址（节点文件面 /api/file/*）。
// 与 meshBaseDirect 同源（注册地址 / 广播地址），只是端口换成对方的文件插件端口。
func (s *Service) fileBaseDirect(nodeID string) string {
	node, ok := s.lookupNode(nodeID)
	if !ok {
		return ""
	}
	if node.Protocol != "peer" && !node.Discovered {
		return ""
	}
	if strings.TrimSpace(node.Address) == "" {
		return ""
	}
	return fileBaseURL(node.Address, node.FilePort, s.filePluginPort())
}

// fileBaseURL 拼节点文件插件的基址：端口按对端自报 file_port → 本机默认端口 依次兜底。
func fileBaseURL(address string, filePort, fallbackPort int) string {
	port := filePort
	if port <= 0 {
		port = fallbackPort
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
	// 本机 store 没有 → 查拓扑视图（域间推送来的跨域节点）
	if view := s.Peer.View(); view != nil {
		if _, ok := view.NodeByID(nodeID); ok {
			return true
		}
	}
	return false
}

// execViaNodeMesh 在跨域节点的 mesh 端口上执行命令：POST /api/exec 到
// 节点 HTTP 面（与文件传输同一条代理通道，keep-alive 复用代理管道）。
func (s *Service) execViaNodeMesh(ctx context.Context, nodeID, command string, timeout time.Duration) (model.ExecResult, error) {
	return s.execViaRelay(ctx, nodeID, command, timeout)
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

// execViaRelay 经上级代理管道执行命令；管道协商成本高，必须 keep-alive 复用。
func (s *Service) execViaRelay(ctx context.Context, nodeID, command string, timeout time.Duration) (model.ExecResult, error) {
	transport := &http.Transport{
		MaxConnsPerHost: 2,
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return s.dialNodeMesh(ctx, nodeID)
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
		"bind_scope":    s.meshBindScope(),
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
	_ = s.StopFilePlugin(ctx)
	_ = s.Plugin.Close(ctx)
	_ = s.FilePlugin.Close(ctx)
	if s.BrowserPlugin != nil {
		_ = s.BrowserPlugin.Close(ctx)
	}
	cancel()
	if s.resourceDir != "" {
		_ = os.RemoveAll(s.resourceDir)
	}
	if s.Discovery != nil {
		s.Discovery.Close()
	}
	_ = s.Store.Close()
}

// MCPCapabilities 返回 MCP 侧当前可用的能力（本地面 56700）。
//
// 权限模型重构后不再逐项开关：核心能力（SSH / 隧道 / 节点发现 / 中继）随
// MCP 服务一起开；插件能力由插件是否启用决定——插件禁了，它提供的能力就没了。
func (s *Service) MCPCapabilities() map[string]bool {
	return s.MCPCapabilitiesFor(MCPFaceLocal)
}

// MCPFaceLocal 是本地面（56700）：本机自己用的 MCP。
// MCPFaceMesh 是 mesh 面（56701）：可能暴露给局域网其它设备。
type MCPFace string

const (
	MCPFaceLocal MCPFace = "local"
	MCPFaceMesh  MCPFace = "mesh"
)

// MCPCapabilitiesFor 返回某个面上可用的能力。
//
// 两个面的差别只有一处：插件把自己的 MCP 服务关掉时（<槽位>_plugin_mcp），
// mesh 面不再提供它——禁用的 MCP 只能通过 56700 本地面访问。
func (s *Service) MCPCapabilitiesFor(face MCPFace) map[string]bool {
	capabilities := map[string]bool{
		"ssh": true, "tunnel": true, "discovery": true, "relay": true,
	}
	for _, slot := range s.pluginSlots() {
		enabled := s.slotEnabled(slot)
		if face == MCPFaceMesh && slot.hasMCP && !s.PluginMCPEnabled(slot.name) {
			enabled = false
		}
		for _, capability := range slot.capabilities {
			capabilities[capability] = enabled
		}
	}
	return capabilities
}

func parseBool(value string) bool { return strings.EqualFold(value, "true") || value == "1" }

func (s *Service) Settings() map[string]string {
	settings, _ := s.Store.Settings()
	return settings
}

func (s *Service) SaveSettings(values map[string]string) error {
	// 插件参数由插件自己声明：设置接口只放行已声明的参数与 core 自己管的键。
	if err := s.validatePluginSettings(values); err != nil {
		return err
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
		// 节点发现跟随局域网开关：不对局域网开放时不做 mDNS 发现。
		_ = s.Discovery.SetEnabled(ctx, parseBool(s.Settings()["lan_enabled"]))
	}
	if s.PeerAgent != nil {
		if parseBool(s.Settings()["peer_enabled"]) {
			s.PeerAgent.Start(context.Background())
		} else {
			s.PeerAgent.Stop()
		}
	}
	// 插件的启停状态就是能力开关：禁用的插件进程要停下，启用的要拉起来。
	s.applyPluginStates(ctx)
	s.refreshBrowserPlugin(ctx)
	s.refreshPluginManager(ctx)
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
		ID: model.NodeID(), DisplayID: model.NodeID(), Name: "本地节点",
		Address: discovery.PrimaryIP(), Port: 0, Username: userName, Version: Version,
		AuthMethod: "local", Protocol: "local", Tags: []string{"local", "host:" + host},
		Status: "connected", CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Now().UTC(),
	}
}

func isLocalNodeID(id string) bool {
	return id == "local" || id == model.NodeID()
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
		// 发现节点（mDNS 记录）存在另一个 bucket：只删注册记录的话，
		// 同一条旧记录仍会以 discovered 身份留在列表里（同一节点出现两条）。
		if derr := s.Store.DeleteDiscoveredNode(id); derr != nil {
			s.Audit.Record(ctx, "node.delete", id, policy.CapBase, "", time.Now(), derr)
			return derr
		}
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
		relayResult, relayErr := s.execViaNodeMesh(ctx, nodeID, command, timeout)
		if relayErr == nil {
			s.Audit.Record(ctx, "peer.exec", nodeID, policy.CapSSH, audit.SafeParameters(map[string]string{"command": command, "via": "upper-relay"}), started, nil)
			return relayResult, nil
		}
		s.Audit.Record(ctx, "peer.exec", nodeID, policy.CapSSH, audit.SafeParameters(map[string]string{"command": command}), started, err)
		return result, apperrors.Wrap(apperrors.NodeOffline, "relay exec also failed: "+relayErr.Error(), err)
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
	if err := s.Authorize(ctx, "channel.write", policy.CapTunnel); err != nil {
		return err
	}
	if err := s.Store.SaveTunnel(definition); err != nil {
		return err
	}
	s.Audit.Record(ctx, "channel.write", definition.NodeID, policy.CapTunnel, audit.SafeParameters(map[string]string{"type": definition.Type, "listen": fmt.Sprintf("%s:%d", definition.ListenAddress, definition.ListenPort)}), time.Now(), nil)
	return nil
}

func (s *Service) StartTunnel(ctx context.Context, id string) (model.Tunnel, error) {
	if err := s.Authorize(ctx, "channel.open", policy.CapTunnel); err != nil {
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
	started := time.Now()
	result, err := s.Tunnels.Start(ctx, id)
	s.Audit.Record(ctx, "channel.open", definition.NodeID, policy.CapTunnel, audit.SafeParameters(map[string]string{"type": definition.Type}), started, err)
	return result, err
}

func (s *Service) StopTunnel(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "channel.close", policy.CapTunnel); err != nil {
		return err
	}
	err := s.Tunnels.Stop(id)
	s.Audit.Record(ctx, "channel.close", id, policy.CapTunnel, "", time.Now(), err)
	return err
}

func (s *Service) DeleteTunnel(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "channel.delete", policy.CapTunnel); err != nil {
		return err
	}
	err := s.Tunnels.Delete(id)
	s.Audit.Record(ctx, "channel.delete", id, policy.CapTunnel, "", time.Now(), err)
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
	if err := s.Authorize(ctx, "channel.write", policy.CapTunnel); err != nil {
		return err
	}
	if err := s.Store.SavePortMapping(definition); err != nil {
		return err
	}
	s.Audit.Record(ctx, "channel.write", "", policy.CapTunnel, audit.SafeParameters(portMappingParameters(definition)), time.Now(), nil)
	return nil
}

func (s *Service) StartPortMapping(ctx context.Context, id string) (model.PortMapping, error) {
	if err := s.Authorize(ctx, "channel.open", policy.CapTunnel); err != nil {
		return model.PortMapping{}, err
	}
	definition, err := s.Store.GetPortMapping(id)
	if err != nil {
		return definition, err
	}
	started := time.Now()
	result, err := s.PortMappings.Start(ctx, id)
	result.RuntimePort, result.ViaRoute = s.PortMappings.RuntimeInfo(id)
	s.Audit.Record(ctx, "channel.open", "", policy.CapTunnel, audit.SafeParameters(portMappingParameters(result)), started, err)
	return result, err
}

func (s *Service) StopPortMapping(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "channel.close", policy.CapTunnel); err != nil {
		return err
	}
	err := s.PortMappings.Stop(id)
	s.Audit.Record(ctx, "channel.close", id, policy.CapTunnel, "", time.Now(), err)
	return err
}

func (s *Service) DeletePortMapping(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "channel.delete", policy.CapTunnel); err != nil {
		return err
	}
	err := s.PortMappings.Delete(id)
	s.Audit.Record(ctx, "channel.delete", id, policy.CapTunnel, "", time.Now(), err)
	return err
}

func (s *Service) OpenWeb(ctx context.Context, nodeID, targetHost string, targetPort int) (string, error) {
	if isLocalNodeID(nodeID) {
		return "", apperrors.New(apperrors.NotSupported, "local node cannot open SSH web proxy")
	}
	if err := s.Authorize(ctx, "service.open_web", policy.CapTunnel); err != nil {
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
	// 节点数与拓扑页一致：用 topology 的数据源（含跨域节点），按 address 去重
	payload, _ := s.Topology(context.Background())
	topoNodes, _ := payload["nodes"].([]peer.TopoNode)
	byAddr := make(map[string]peer.TopoNode)
	for _, n := range topoNodes {
		addr := strings.TrimSpace(n.Address)
		if addr == "" {
			addr = n.ID
		}
		if existing, ok := byAddr[addr]; ok && n.Online && !existing.Online {
			byAddr[addr] = n
			continue
		}
		if _, ok := byAddr[addr]; !ok {
			byAddr[addr] = n
		}
	}
	connected := 0
	for _, node := range byAddr {
		if node.Online {
			connected++
		}
	}
	channels, _ := s.ListChannels(context.Background())
	running := 0
	for _, channel := range channels {
		if channel.Status == model.ChannelStatusRunning {
			running++
		}
	}
	return model.Overview{
		Version: Version, UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Nodes: len(byAddr), Connected: connected, Channels: len(channels), Running: running,
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

// NodeResources 返回本机 CPU/内存/磁盘占用百分比 + 总量。
// setManagerSynced 记录最近一次成功从管理节点同步到的数据版本。
func (s *Service) setManagerSynced(v int64) {
	s.managerSyncedMu.Lock()
	s.managerSyncedVer = v
	s.managerSyncedMu.Unlock()
}

// getManagerSynced 读最近同步到的管理节点数据版本（/api/info → 前端 在线(Vn)）。
func (s *Service) getManagerSynced() int64 {
	s.managerSyncedMu.Lock()
	defer s.managerSyncedMu.Unlock()
	return s.managerSyncedVer
}

func (s *Service) NodeResources() map[string]any {
	result := map[string]any{"cpu": 0, "memory": 0, "memory_total_gb": 0, "disk": 0, "disk_total_gb": 0,
		"uptime_seconds": int64(time.Since(s.started) / time.Second),
		"goos":           runtime.GOOS, "goarch": runtime.GOARCH}
	if pct, err := cpu.Percent(0, false); err == nil && len(pct) > 0 {
		result["cpu"] = math.Round(pct[0]*10) / 10
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		result["memory"] = math.Round(float64(vm.UsedPercent)*10) / 10
		result["memory_total_gb"] = math.Round(float64(vm.Total)/(1024*1024*1024)*10) / 10
	}
	if du, err := disk.Usage("/"); err == nil {
		result["disk"] = math.Round(du.UsedPercent*10) / 10
		result["disk_total_gb"] = math.Round(float64(du.Total)/(1024*1024*1024)*10) / 10
	}
	return result
}
