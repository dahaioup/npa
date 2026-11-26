# npa

NPA (Node Proxy Agent) 是一款专为局域网多机联调与 AI 自动化排障设计的轻量级、跨平台节点调试代理网关。

通过标准化的 MCP (Model Context Protocol) 协议，NPA 让外部 AI Agent 与开发终端能够无缝接管并观测局域网目标机；借助纯出站的 SSH 客户端隧道与动态端口映射，消除传统调试对公网 IP 和复杂网络配置的依赖。核心程序采用纯 Go 构建，零 CGO 污染，实现全平台“单机编译、一键分发”。

## 快速开始

1. 下 Release 里的 `npa-all_<os>_<arch>.zip`，解压后 `./run.sh`（Windows 用 `run.bat`），单二进制、无运行时依赖。
2. 打开 <http://127.0.0.1:56700> 管理面；MCP 端点是 <http://127.0.0.1:56701/mcp>（本机自用也可以走 <http://127.0.0.1:56700/mcp>）。
3. 右上角「**MCP**」是 MCP 服务的总开关，「**局域网**」决定 56701 是否对局域网开放（关掉后只听 `127.0.0.1`）；插件的启停与参数在「**插件设置**」页里改。

完整步骤（接 AI Agent、局域网多机、端口映射、常见问题）见 [docs/USAGE.md](docs/USAGE.md)。

## Phase 1 — NPA Core

当前仓库已实现一期核心能力：

- 单二进制启动，Web 固定监听 `127.0.0.1:56700`（本地面，也提供 `/mcp`）；MCP 与 mesh 监听 `56701`，右上角「局域网」开关决定它是 `0.0.0.0` 还是只听 `127.0.0.1`；
- 权限不再是逐项能力开关：核心能力（SSH / 隧道 / 中继）随 MCP 服务整体提供，节点发现跟随局域网开关，插件能力（文件 / 浏览器 / 桌面 / 插件管理）由插件启停决定；
- Web 管理概览、节点拓扑占位、SSH 交互式终端、Local/Remote/SOCKS5 隧道、插件仓库、插件设置、设置和审计；
- 节点页默认展示本地节点，不提供手工添加入口；三期 Discovery 接入后在此展示自动发现节点拓扑，点击节点查看详情；
- 节点 ID 从本机 MAC 地址稳定派生为 8 位 `0-9A-Z` 字符，并同步用于 mDNS TXT；
- SSH 页面只保留交互式终端，默认选择本地节点并在本地 shell 中执行命令；
- 节点、隧道、设置、审计和加密凭据保存在内部 bbolt 状态库，不维护用户配置文件；
- MCP 使用官方 Go SDK，工具面按「核心能力 + 已启用插件」动态挂载；
- MCP File Capability 继续提供 SFTP 工具：`file.list`、`file.read`、`file.write`、`file.remove`；大文件仍通过受控 Web 下载接口处理；
- SSH 使用 `x/crypto/ssh`，SFTP 使用 `pkg/sftp`，支持 TOFU Host Key 指纹、密钥/口令/Agent 认证和连接复用；
- 隧道复用 SSH `direct-tcpip`/`forwarded-tcpip`，带 KeepAlive、状态持久化和自动恢复；
- Web Proxy 通过 `/proxy/{tunnel-id}/` 走受控反向代理（对外暴露面由右上角的局域网药丸控制，不再单独限制公网目标）；
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
- 文件分发能力也走同一套插件机制：`internal/file` 抽出 Provider 契约，内置纯 Go 实现作为「内置插件」，外部插件进程（`plugins/file`）在运行时可接管；
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

### 跨多域的文件分发流程

A 要给任意深度的节点 N 传文件（或从 N 拉文件）时，按下面的顺序选路，逐级降级、调用方无感（`internal/file/service.go` 的 `httpTarget`）：

```text
A 发起 FileUpload / OpenRead / Write
  │
  ├─ ① 直连 HTTP：节点 N 在本机反向链路上（有直连基址）
  │     A ──HTTP PUT /api/file/upload（流式）──▶ N:56751
  │
  ├─ ② 跨域代理：N 不在直连链路（跨域，只存在于拓扑）
  │     A ──上级代理链 A→B→N──▶ N 自己的 127.0.0.1:56751
  │     （DialNode 直连失败 → 按 N.ID 在拓扑找持有上级 B → 经 B 中转）
  │
  └─ ③ SFTP 回退：以上 HTTP 全部失败（如旧版本节点）
        走 SFTP-over-WS 数据通道（会占用共享通道池，逐步淘汰）
```

要点：

- **全链流式**：①② 都是纯 HTTP 流式传输，不限文件大小、不整读进内存；升级包十几 MB 一样直过。整读进内存的只有 `file.read`/`file.write` 小文件接口（4 MiB 上限，给 MCP JSON 通道保命）。
- **传输不占控制面**：代理链是按需拨、传完即关的独立连接，不占用心跳/exec/PTY 共享的通道池。
- **多跳同理**：A→B→C→N 三跳时，② 的代理回退逐跳递归（`RequireChild`），A 不需要知道 C 的存在。
- **超时语义**：拨号 3s、响应头 5s；总时长由调用方 ctx 控制，大文件不限总时长。代理链路上每个请求新建连接（禁 keep-alive），避免与单连接数据面冲突。
- **典型应用——跨域升级推送**：`/api/update/push` 对跨域节点 = ② 传升级包（流式 HTTP）+ `/api/exec` 探测与重启，一条代理链走通；端到端验证见 `TestCrossUpperPushAndTunnel`。

架构细节见 [docs/architecture/ARCHITECTURE.md](docs/architecture/ARCHITECTURE.md)（含整体分层、Mesh 数据面与分域演进三张图）。

### 文件能力：内置插件，外部插件可接管

文件分发已经改造成 NPA 内置插件（`internal/file`）。对外接口一个都没动——MCP 的 `file.*` 工具、Web 的 `/api/nodes/{id}/files/*`、跨域升级包推送的调用点与返回结构都保持原样，底层换成插件路由：

```text
调用方 ──▶ file.Service（门面 + 路由）
             │
             ├─ ① 外部文件插件：plugins/file 下的插件进程在运行（JSON-RPC 2.0 + stdio）
             │     方法：file.list / stat / read / download / write / upload / mkdir / remove / rename / touch
             │
             └─ ② 内置插件（纯 Go，随 core 编译）：插件缺失或进程崩溃时接管
                   直连 HTTP 文件面 → 跨域代理 → SFTP 三级选路（与上面同一套选路）
```

要点：

- **内置实现零 CGO、单二进制**：内置插件就是 core 里的纯 Go 实现，随 NPA 一起发布，不需要额外装东西；
- **插件缺失不影响功能**：`plugins/file` 目录为空、或插件进程崩了，文件操作自动落到内置实现，行为与改造前一致；
- **插件报错不静默降级**：插件在跑但某次调用失败时错误直接上抛，不悄悄改走内置实现，避免插件的问题被盖掉；只有插件进程消失、或插件通道载荷超限（base64 通道 4 MiB 上限，而内置走流式通道）才回退内置实现；
- **协议**：JSON-RPC 2.0 + `Content-Length` framing + stdio（与 Desktop 插件同款运行时），字节载荷走 base64，单次上限 4 MiB；
- **内置插件自报身份**：ID `npa.file`、权限 `core.file`，与外部插件同形元数据。

### 文件面与文件页面：都归内置插件（56751）

文件分发通道整体搬进了内置插件，节点侧文件面不再挂在 mesh 上：

- **端口**：默认 `56751`（设置项 `file_plugin_port` 可改），绑定范围跟随右上角的「局域网」开关（`lan_enabled`：关掉只听 `127.0.0.1`，打开对局域网开放）；节点间通过 `Registration.FilePort` / mDNS TXT 的 `file=` 互相告知端口，跨域时在目标节点拨 `127.0.0.1:<filePort>`。
- **文件面**：`http://<节点>:56751/api/file/{list,stat,read,download,upload,write,mkdir,remove,rename}`（原在 56701，现已迁走；56701 只保留 `/api/exec` 与 `/api/upgrade/apply`）。
- **自带页面**：打开 <http://127.0.0.1:56751/> 就是文件浏览器——选节点、浏览目录、上传、下载、新建目录、重命名、删除；Web 管理面概览页的「文件插件」按钮直达。
- **注意**：这是纯迁移——旧版 NPA 节点没有 56751 文件面，新旧混跑的网格里双方无法互相传文件或推升级包，升级请整网一起升。

架构细节见 [docs/architecture/ARCHITECTURE.md](docs/architecture/ARCHITECTURE.md) 第 5 节。

## Phase 4 — Obscura 无头浏览器

NPA 之前处理网页只有两条路：`ssh.exec` 跑 `curl`（拿不到 JS 渲染后的页面），或者干脆不碰网页。Phase 4 把 [obscura](https://github.com/h4ckf0r0day/obscura) 接进来：它是 Rust 自研的无头浏览器引擎，V8 真跑 JS、自建 DOM，30MB 常驻内存，比 headless Chrome 轻得多，还自带反检测。

- **obscura 的 38 个 `browser_*` 工具整体挂到 NPA 的 MCP 面上**：工具名、参数 schema、元素引用（`e1`）、快照格式全部是引擎自己的语义，NPA 不做翻译——上游 obscura 升级时 NPA 这边零改动；
- **执行位置可选本机或远端 NPA 节点**：每个工具的 schema 上多一个 `node_id` 参数，留空就是本机跑，填节点 ID 就派发到那台 NPA 节点（走 mesh 面 `/api/browser/*`，先直连后跨域代理，与屏幕能力同一套选路）；
- **插件进程 + CLI 回退双路径**：`plugins/browser/npa-obscura` 插件进程在时走 JSON-RPC over stdio 桥接 obscura 自带的 MCP server；插件缺失时回退到 `obscura fetch` 命令行，覆盖抓取类工具；
- **一键下载安装**：浏览器插件自带一个管理页（插件设置页 →「打开插件页面」），页面上点「下载安装」即可——插件自己查 GitHub release、选平台资产、下载解包落到 `<数据目录>/bin/`，三种形态可选（完整版 / 隐匿版 stealth / 精简版 no-render），同一个页面还能**预览任意网页截图**；
- **obscura 没装也能挂工具**：工具清单来自内嵌的 obscura v0.2.7 快照，所以本机没装 obscura、但远端节点装了，调用照样派发得过去；
- **配置改动即生效**：隐匿模式、代理、UA、是否允许访问内网地址等都是 obscura 的启动参数，NPA 在设置变更后重启插件进程与引擎，无需重启 NPA。
- **插件只绑本机，MCP 由 NPA 代理**：插件的 HTTP 服务（管理页 + 自己的 MCP 端点）默认只监听 `127.0.0.1`（端口 56753）；NPA 在 `56700` 与 `56701` 都挂 `/plugins/browser/mcp` 转发过去——插件不用自己对外开放。端口与开放度（`auto` / `local` / `lan`）在「插件设置」页里改；把插件的 MCP 关掉后，`56701` 就不再提供它，只有 `56700` 本地面能用。

```text
AI Agent ──MCP──▶ NPA core (56701 mesh 面 / 56700 本地面)
                    │
                    ├─ 本机：browser.Router（插件优先 → CLI 回退）
                    │        └─ npa-obscura 插件进程 ──MCP(stdio)──▶ obscura mcp
                    │
                    └─ 远端节点：mesh /api/browser/call ──▶ 节点侧同一条 browser.Router
```

要点：

- **obscura 二进制不是 NPA 的一部分**，装不装由用户决定；不装时 CLI 回退路径仍能抓静态页面；
- **obscura 整体是独立插件，不内置**：插件进程 `plugins/browser/npa-obscura` 和引擎二进制都从插件仓库装（`npa-obscura` + `obscura-engine`），core 里只有契约层与路由。浏览器是可停用、可卸载的普通插件槽位——要「内置、不可禁用」的只有插件管理器自己；
- **obscura 默认禁止访问回环与内网地址**（自带 SSRF 防护），要抓内网站点需在设置里显式打开「允许访问内网地址」；
- 每次调用都经过统一的 Capability 校验、速率限制与脱敏审计（操作名 `browser.call`）。

## Phase 5 — 插件仓库与插件管理器

插件以前只能「拷进 `plugins/` 目录」才能用。Phase 5 加了一条完整的分发链路：一个**公有插件仓库** + 一个**插件管理器插件**。

- **公有仓库**：[dahaioup/npa-plugins](https://github.com/dahaioup/npa-plugins)（GitHub，匿名可读）。git 里只放一份索引 `registry.json`，二进制作为 Release 资产发布——仓库不膨胀，第三方只下自己平台那一份；
- **`registry.json` 索引**：描述每个插件/依赖的版本、平台产物、sha256、依赖关系。插件和它的依赖（例如 obscura 引擎）都写在同一份索引里，`requires` 描述依赖关系；
- **插件管理器是 NPA 的内置插件**：实现体随 core 编译（`internal/pluginmanager`），**始终可用、不可停用、不可卸载**——它就是装插件的那条路本身，停用它等于把路堵死。「插件设置」页里它显示为「内置 · 不可停用」，没有开关；
- **外部实现只是可选覆盖**：仓库里的 `npa-plugin-manager` 是同一个管理器的独立进程版本（走标准插件协议 stdio JSON-RPC）。它在跑时接管调用，不在时内置实现顶上——两条实现共用同一份逻辑，行为一致；
- **两个入口**：Web 新增「插件仓库」页（看清单、装/更/卸、换仓库地址）与「插件设置」页（启停插件、调插件自己声明的参数）；MCP 面上新增四个工具 `plugin.list` / `plugin.install` / `plugin.remove` / `plugin.update`（`plugin` 能力恒开）；
- **自举**：`scripts/build-release.sh` 把 `npa-plugin-manager` 直接打进 NPA 发布包的 `plugins/manager/`，所以新装 NPA 就有一个可用的外部实现；不装也不影响——内置实现在 core 里；
- **装完即用**：装到 `browser` 槽位的插件会被 core 自动重启加载，装完就能在 MCP 面上看到它提供的能力。

```text
AI Agent ──MCP(plugin.install)──▶ NPA core ──▶ 内置插件管理器（默认）
                                     │                  └──HTTP──▶ dahaioup/npa-plugins
                                     └──插件协议──▶ npa-plugin-manager（可选覆盖）  （registry.json + Release 资产）
```

要点：

- 下载走的是**断点续传 + 重试**的下载器（公共镜像经常中途断流），sha256 不匹配直接拒绝安装；
- 安装/更新/卸载都会写审计日志（操作名 `plugin.install` / `plugin.update` / `plugin.remove`，能力位 `plugin`）；
- 仓库地址是设置项 `plugin_registry_url`，留空用默认公有仓库；
- 插件管理器是**本机动作**（往自己机器上装东西），所以只挂在回环 Web 面上，不进 mesh/peer 面。

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

发布包用 `scripts/build-release.sh` 生成：交叉编译 darwin/linux/windows 共 7 个平台，
版本号按构建时刻注入为 `v0.4.1-<Unix 秒>`（`-ldflags -X .../internal/service.Version=0.4.1-<epoch>`，界面统一加 `v`），
产物在 `dist/npa-all_<时间戳>/`，同时打出带 UTF-8 文件名的 zip。
直接 `go build` 的版本是 `v0.4.1-dev`。

启动后访问 `http://127.0.0.1:56700`。常用诊断参数：

```text
--data-dir DIR       内部状态目录（默认 ~/.npa）
--log-level LEVEL    debug/info/warn/error
--version            输出版本
```

MCP Streamable HTTP 端点为 `http://127.0.0.1:56701/mcp`。首次连接完成 `initialize` 和 `notifications/initialized` 后即可调用 `tools/list`。
