# npa

NPA (Node Proxy Agent) 是一款专为局域网多机联调与 AI 自动化排障设计的轻量级、跨平台节点调试代理网关。

通过标准化的 MCP (Model Context Protocol) 协议，NPA 让外部 AI Agent 与开发终端能够无缝接管并观测局域网目标机；借助纯出站的 SSH 客户端隧道与动态端口映射，消除传统调试对公网 IP 和复杂网络配置的依赖。核心程序采用纯 Go 构建，零 CGO 污染，实现全平台“单机编译、一键分发”。

## Phase 1 — NPA Core

当前仓库已实现一期核心能力：

- 单二进制启动，Web 固定监听 `127.0.0.1:56700`；MCP 默认监听 `127.0.0.1:56701`，可在设置中切换为局域网范围；
- MCP 监听范围位于权限层级中；仅本机范围时，节点发现和中继 MCP 权限不可开启。
- Web 管理概览、节点拓扑占位、SSH 交互式终端、Local/Remote/SOCKS5 隧道、设置（含 MCP 权限）和审计；
- 节点页默认展示本地节点，不提供手工添加入口；三期 Discovery 接入后在此展示自动发现节点拓扑，点击节点查看详情；
- 节点 ID 从本机 MAC 地址稳定派生为 8 位 `0-9A-Z` 字符，并同步用于 mDNS TXT；
- SSH 页面只保留交互式终端，默认选择本地节点并在本地 shell 中执行命令；
- 节点、隧道、设置、审计和加密凭据保存在内部 bbolt 状态库，不维护用户配置文件；
- MCP 使用官方 Go SDK，默认只注册 `nodes.list`；SSH、File、Tunnel Capability 默认关闭并可在设置页即时开关；
- MCP File Capability 继续提供 SFTP 工具：`file.list`、`file.read`、`file.write`、`file.remove`；大文件仍通过受控 Web 下载接口处理；
- SSH 使用 `x/crypto/ssh`，SFTP 使用 `pkg/sftp`，支持 TOFU Host Key 指纹、密钥/口令/Agent 认证和连接复用；
- 隧道复用 SSH `direct-tcpip`/`forwarded-tcpip`，带 KeepAlive、状态持久化和自动恢复；
- Web Proxy 通过 `/proxy/{tunnel-id}/` 走受控反向代理，默认拒绝公网目标；
- 所有 MCP 调用经过统一 Capability、速率限制和脱敏审计入口。

文件浏览 API 和 SFTP 适配器仍保留在服务层，当前 Web 导航暂不展示独立“文件”页面。

## Phase 2 — Desktop + Plugin Runtime

当前已加入二期基础能力：

- 独立 Desktop Plugin 进程，使用 JSON-RPC 2.0 + `Content-Length` framing + stdio；
- Plugin Manager 支持初始化、健康检查、调用、超时、崩溃隔离和优雅关闭；
- 插件缺失或不可用时自动回退到嵌入式系统命令脚本；
- `desktop.screens`、`desktop.screenshot` MCP 工具；
- `desktop.mouse_move`、`desktop.mouse_click`、`desktop.mouse_scroll` MCP 工具；
- Screenshot 与 Mouse 为独立 Capability，默认关闭，并统一经过审计；
- 截图资源由 Core 分配和清理，不把大图直接塞入 MCP JSON；
- Core 继续保持 `CGO_ENABLED=0`，平台能力通过插件或脚本回退隔离。

macOS 截图需要系统“屏幕录制”权限；未授权时工具会返回明确错误，不会导致 Core 崩溃。

## Phase 3 — NPA Mesh

当前已加入三期基础能力：

- 标准 mDNS/DNS-SD `_npa._tcp.local.` 发现，固定使用 `5353/UDP`，不增加 NPA 私有发现端口；
- Discovery 可在设置中独立开关，关闭时不广播；
- TXT 仅发布节点 ID、名称、版本、入口、Capability 和可达子网摘要，不包含凭据；
- 自动汇总 RFC1918、回环和链路本地 `reachable_subnets`；
- 单跳 Relay 路由选择，优先直连，再选择已信任且显式启用 Relay 的健康节点；
- Relay 数据面复用 SSH `direct-tcpip`，不创建私有协议；
- Web 节点拓扑显示本地、已管理与已发现节点，并展示可达子网和 Relay 路径；
- MCP 新增 `discovery.nodes`、`discovery.refresh`、`network.subnets`、`relay.routes`、`relay.open`；
- Discovery 与 Relay MCP Capability 默认关闭并独立审计；
- 端口映射（独立于 SSH 隧道）：把子网目标 IP:端口映射到本机端口，经直连或 Relay 转发，定义持久化并可自动恢复；
- 端口映射默认仅监听 `127.0.0.1`，可按单条映射显式开启局域网监听，创建与启停均进入审计。

### 用 RustDesk 远程子网设备

本机无法直连子网设备、但子网内的 NPA 节点可以时：

1. 在 **端口映射** 页新建映射：子网目标 IP（如 `192.168.50.21`）、目标端口（RustDesk 默认 `21115`）、本机端口（填 `0` 自动分配，或指定一个空闲端口）；
2. 保存并启动，列表显示 `本机端口 → 目标 IP:端口` 以及当前经由（直连或某节点）；
3. 在本机 RustDesk 客户端连接 `127.0.0.1:<本机端口>`（或本机 IP:端口，前提是该映射已开启局域网监听），即可远程子网设备；
4. 需要固定走某个中继节点时，在映射的「经由节点」里选择它；不选则由 Relay 自动选路。

端口映射与「隧道」页的 SSH Local/Remote/SOCKS 相互独立，删除隧道不影响已建立的映射。

### 让子网节点反向上级（跨网段）

mDNS 是链路本地组播，跨不了三层网段，所以上级（本机 `192.168.0.x`）主动发现不到子网节点（如 `192.168.2.192`）。NPA 的做法是**方向反转**：由子网上的 NPA 主动发现上级、注册并保持长连接。

1. **上级**：节点页确认「反向接入」卡显示本机入口 `192.168.0.165:56701`（节点通信与 MCP 共用 56701，默认监听局域网；Web 管理面 `56700` 仍只在回环）。
2. **子网节点**：解压 `npa-all_*.zip` 后 `run.bat` / `./run.sh` 启动，在节点页填**上级种子** `192.168.0.165:56701`（也可填上级探测网段，如 `192.168.0.0/24`，只扫显式配置的网段）。
3. 上级「已注册到本机的子网节点」里出现该节点，状态 `在线`，并显示它宣告的网段与空闲连接池。
4. 之后上级侧即可反向使用它：端口映射、Local/SOCKS 隧道、Web 代理、命令执行、交互终端、文件读写——全部能力与 SSH 节点等价（`-R` 反向隧道除外，它需要 SSH 服务端语义）。
5. 回到上面的 RustDesk 步骤，把目标填成子网 IP，NPA 会自动经反向链路转发。

几点说明：

- **所有连接都由子网侧发起**，上级不要求子网开放任何入站端口；
- 子网节点的命令/终端走 NPA 自有通道；文件能力走节点自身的 **HTTP 文件面**（`/api/file/*`，纯 HTTP、流式传输），原 SFTP-over-WS 通道降级为回退——大文件不再挤占 exec/终端/隧道共享的通道池；
- 上级对节点的远程命令也新增 HTTP 直连路径（`/api/exec`），失败自动回退自有通道；
- 当节点无法直达 SSH 服务时，会自动串成 **NPA 映射 + SSH 映射**（上级 → 反向链路 → 对端 SSH `direct-tcpip` → 目标）；
- 反向接入、节点文件面与 `/api/exec` **当前均不鉴权**（调试期，权限体系明确后置）：同网段任何人都能注册、读写节点文件、执行命令，正式使用前必须收紧。

### 跨域协同（上级的上级）

节点 N 注册在上级 B、B 又注册在更上层 A 时，拓扑会把整条链汇总到 A。A 对 N 的操作不再要求直连：

- **拓扑透传**：`/api/topology` 返回全链节点（含 `parent` 归属与 `missing` 断链提示），缓存 15s；
- **跨域代理回退**：顶层开启 `peer_relay_fallback=true`（默认关）后，A 对 N 的直连失败会自动经 B 中转（A→B→N 代理链），命令执行、文件上传/下载、隧道与端口映射全部适用；
- **跨域升级推送**：「版本与更新」的节点推送升级对跨域节点同样可用（探测 → 传包 → 写脚本 → 执行替换重启，一条代理链走通）；端到端验证见 `internal/peer/e2e_test.go` 的 `TestCrossUpperPushAndTunnel`；
- **使用前提**：中间持有者（B）的地址必须能被 A 访问；NAT/多网卡环境在 B 上设置 `peer_advertise` 显式通告入口。

架构细节见 [docs/architecture/ARCHITECTURE.md](docs/architecture/ARCHITECTURE.md)（含整体分层、Mesh 数据面与分域演进三张图）。

## 本机管理（设置页）

- **节点版本**：节点详情显示对端自报版本；比本机新时带「有新版本」标识。
- **开机自启动**：Windows 写注册表 Run，macOS 写 LaunchAgent，Linux 写 systemd user（无则回退 XDG autostart）；开关失败会报错，`/api/system` 显示的是系统里的真实状态。
- **隐藏控制台**：开启后由 `run.sh` / `run.bat` 转为后台异步启动，输出只进 `<数据目录>/npa.log`，控制台可直接关闭；关闭后回到前台（控制台 + 日志双输出）。
- **退出服务**：设置页「退出服务」按钮，与 Ctrl+C 同一条优雅关闭路径（隐藏控制台时唯一的正常退出手段）。
- **版本与更新**：「检查更新」比对各节点自报版本 → 从该节点下载 zip → 解出本平台二进制并校验 → 替换本机二进制并重启。

| 场景 | `run.sh` / `run.bat` 行为 |
| --- | --- |
| 默认 | 前台阻塞执行，输出同时进控制台与日志 |
| 设置里开启「隐藏控制台」 | 后台异步启动，输出只进日志，控制台可关闭 |

## 构建与运行

```bash
go test ./...
go build -o npa ./cmd/npa
./npa
```

启动后访问 `http://127.0.0.1:56700`。常用诊断参数：

```text
--data-dir DIR       内部状态目录（默认 ~/.npa）
--log-level LEVEL    debug/info/warn/error
--version            输出版本
```

MCP Streamable HTTP 端点为 `http://127.0.0.1:56701/mcp`。首次连接完成 `initialize` 和 `notifications/initialized` 后即可调用 `tools/list`。
