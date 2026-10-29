# NPA — Node Proxy Agent

[English](README_EN.md) | 简体中文

NPA 是一个单二进制、跨平台的内网节点管理代理：通过**反向接入**把分散在内网的 Windows/Linux/macOS 机器聚合成一张可管理的网格，提供远程执行、文件分发、隧道映射、桌面控制与 MCP 工具面，并支持**守护式自动升级**。

- 单二进制，无外部依赖（CGO_ENABLED=0）
- 数据面只走下级出站连接，NAT/防火墙友好
- 内置 MCP Server，可被任意 AI 程序直接驱动
- 版本号：`v0.4.1-<Unix构建时间戳>`

## 架构一览

```
浏览器/AI ──► Web 管理面 127.0.0.1:56700
AI (MCP) ──► Mesh 入口 :56701  ─┬─ /mcp（MCP Streamable HTTP）
                                ├─ /api/file/* /api/exec（节点文件/执行面）
                                ├─ /api/topology /api/info（拓扑与发现）
                                └─ peer 协议（register/link/conn/open/exec/pty/relay_open）
节点 ──出站──► 上级 :56701（反向 WS 长连接：控制 + 按需数据通道）
跨域 ──► 上级的上级代理链（peer_relay_fallback，逐跳 require 拼管）
升级 ──► guardian 守护进程（备份/巡检/重启/回滚/自清理）
```

## 已实现的功能 ✅

### 节点接入与发现
- ✅ **反向接入**：下级主动出站连接上级，控制链路 + 按需数据通道（exec/PTY/文件/反向 TCP/relay）
- ✅ **mDNS 自动发现**（`_npa._tcp.local.`）：广播 ID/名称/版本/peer 端口/uptime/可达子网；轮询式解析器（QU 单播应答），不受 Windows 组播出口/防火墙影响
- ✅ **上级网络自动发现**：无种子时用 traceroute 探测域外网段，自动找到相邻 NPA 并接入（`peer_auto_discover_uppers`，默认开）
- ✅ **跨域拓扑**：`/api/topology` 带 parent/missing，stale-while-revalidate 缓存 + 并发逐级查询
- ✅ **跨域访问**：目标不可直连时经持有上级逐跳代理（`peer_relay_fallback`），在途协商去重 + 失败冷却

### 远程执行与文件
- ✅ **远程执行**：`ssh.exec`（SSH 节点）与 peer 节点 HTTP `/api/exec`（stdout/stderr 截断 2MiB，超时可控）
- ✅ **节点文件面**：`/api/file/{list,stat,read,download,upload,write,mkdir,remove,rename}`，全流式不限大小（read/write 4MiB 内存上限），临时文件原子落盘，并发 IO 闸门（≤2），空闲超时看门狗
- ✅ **跨多域文件分发**：三级选路——直连 HTTP → 上级代理链 → SFTP-over-WS 回退；传输不占控制通道
- ✅ **SSH 管理**：凭据加密存储（AES-GCM）、SFTP、交互式 PTY（Web 终端）
- ✅ **隧道与端口映射**：SSH 隧道恢复、本地端口映射（默认只绑回环）

### 升级与运维
- ✅ **守护式自动升级**：新二进制以 `--guardian-config` 运行守护——备份旧版→停旧→覆盖→拉起→每 5s 巡检→挂了重启→连续 6 次失败自动回滚→稳定后自清理退出；全程原地同名替换
- ✅ **升级手册内置**：MCP 工具 `upgrade.guide`，任何 AI 程序连上节点即可获取完整升级流程与坑清单
- ✅ **自升级推送**：升级包推送 + 一次性下载 token + 重启前检测
- ✅ **自启动**：Windows 注册表（PowerShell 隐藏窗口拉起，登录不弹控制台）、macOS launchd、Linux systemd/XDG
- ✅ **Web 管理面**：节点/SSH/隧道/端口映射/拓扑/审计/设置页，SSE 事件推送
- ✅ **审计日志**：MCP 操作审计，保留期可配
- ✅ **崩溃防护**：mesh 入口 recover 中间件，单请求 panic 不拖垮进程

### AI 集成
- ✅ **MCP Server**（Streamable HTTP，`:56701/mcp`）：`nodes.list`、`ssh.exec`、`file.*`、`tunnel.*`、`desktop.*`、`discovery.*`、`network.subnets`、`relay.*`、`upgrade.guide` 等 20+ 工具，能力位开关独立控制
- ✅ AI 程序可通过 MCP 组合完成全自动的「发现节点→部署→升级→验证」闭环

## 规划中的功能 📋

- 📋 **分域 v2 完整版**：域管理者选举、心跳判死、epoch/seq 增量同步核心已落地（`internal/peer/domain.go`，含单测）；待补主动探测图、真实多进程端到端测试
- 📋 **数据面健壮性**：通道池用后归还、数据连接 keepalive（NAT 静默断流防护）
- 📋 **权限体系重新设计**：当前为稳定性优先已搁置——文件面/exec 暂无鉴权，仅建议内网使用；稳定后整体重做
- 📋 **升级事务性**：重启前 drain 在途请求 + 新版本回报确认闭环
- 📋 **断点续传**：大文件传输分块 + SHA256 + Range 续传
- 📋 **拓扑 missing 消费**：DialNode 失败目标进入持有者冷却，避免反复协商
- 📋 **升级包差量分发**：多下级并发拉包时的上级缓存/去重

> 设计决策记录：分域间**直连协商已明确放弃**（2026-10-26），跨域访问一律走代理链；触发重评条件为持续大流量或跨公网链路。

## 快速开始

```bash
# 构建（版本号注入构建时间戳）
scripts/build-release.sh          # 7 平台交叉编译 + zip

# 运行（默认数据目录 ~/.npa）
./npa                             # Web 管理面 http://127.0.0.1:56700
./npa --version                   # v0.4.1-<Unix秒>
```

节点加入网格：在新机器运行 NPA 后，在上级 Web「节点」页批准注册，或配置 `peer_uppers`/mDNS 自动发现即可。
