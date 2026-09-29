# 《软件需求规格说明书》 - NPA (Node Proxy Agent)

---

## 1. 引言

### 1.1 项目标识

- **项目名称**：NPA (Node Proxy Agent)
- **中文定位**：局域网节点访问、调试与代理工具
- **文档类型**：软件需求规格说明书（Software Requirements Specification, SRS）
- **文档版本**：v2.1
- **适用产品版本**：NPA v1.x ~ v4.x
- **目标实现语言 / 技术栈**：Go 1.22+，主程序优先 `CGO_ENABLED=0`
- **核心原则**：单二进制、少配置、固定端口、标准协议优先、成熟开源项目优先、安全默认关闭高权限能力

### 1.2 编写目的

本说明书定义 NPA 的产品定位、功能范围、阶段规划、接口、权限、安全、数据与验收要求，作为后续架构设计、开发、测试和发布依据。

NPA 不以“自研网络协议”为目标。对于 MCP、SSH、SFTP、隧道、mDNS 等已有成熟协议和开源实现的能力，应优先复用成熟项目，仅在 NPA 产品编排、权限控制、状态管理、Web 管理和跨模块协同时编写必要的业务代码。

### 1.3 术语

| 术语 | 含义 |
| --- | --- |
| Control | NPA 本机控制面，提供 Web、MCP、权限、状态与任务编排 |
| Node | 运行 NPA 或可通过 SSH 访问的目标节点 |
| Tunnel | 本地转发、反向转发、SOCKS/代理等隧道能力 |
| File | 文件访问能力；一期以 SFTP 为主，传统 FTP 作为兼容适配器 |
| Discovery | 三期节点发现能力，优先使用标准 mDNS/DNS-SD |
| Relay | 通过一个 NPA 节点代理访问其可达子网 |
| Capability | MCP 可调用能力权限，如 SSH、File、Tunnel、Screenshot、Mouse |
| Managed Node | 已保存连接信息并纳入 NPA 管理的节点 |

---

## 2. 产品总体描述

### 2.1 产品定位

NPA 是面向开发、测试、运维和 AI Agent 的轻量级局域网节点访问工具，提供：

1. SSH 连接与命令执行；
2. SFTP/FTP 文件访问；
3. TCP/SSH 隧道与 Web 服务代理；
4. 本地 Web 管理界面；
5. MCP Server，使 AI Agent 在授权范围内调用上述能力；
6. 二期增加截图与鼠标操作；
7. 三期通过 mDNS 自动发现 NPA 节点，并通过 Relay 访问远端节点可达的子网。

### 2.2 核心设计目标

- 用户执行一个 NPA 二进制即可启动；
- 普通使用不要求编写 YAML/JSON/TOML 配置文件；
- 配置统一在 Web 中完成，由 NPA 内部持久化；
- Web 固定监听 `127.0.0.1:56700`；
- MCP HTTP 固定监听 `127.0.0.1:56701`；
- Discovery 不占用 NPA 私有发现端口，使用标准 mDNS/DNS-SD `5353/UDP`；
- 一期不依赖 Discovery；
- 默认只开放本机回环接口；
- MCP 高风险能力必须单独授权；
- 模块实现优先使用稳定开源项目，不重复实现成熟协议栈。

### 2.3 产品阶段

| 阶段 | 名称 | 主要能力 |
| --- | --- | --- |
| Phase 1 | NPA Core | SSH、SFTP/FTP、Tunnel、Web、MCP、权限、审计、状态持久化 |
| Phase 2 | Desktop + Plugin Runtime | 官方插件运行框架、Screenshot、Mouse Move/Click/Scroll、脚本降级 |
| Phase 3 | NPA Mesh | mDNS Discovery、节点能力/子网上报、Relay、自动路由、代理访问子网节点 |
| Phase 4 | Plugin Ecosystem | 通用第三方插件、零 Manifest、自描述、Web 加载/卸载、动态 Web/MCP 工具 |

### 2.4 运行形态

默认只提供一个 `npa` 主进程。Control、Gateway、MCP、Web、Tunnel 等均为同一进程内模块，不设计 Control/Gateway 拆进程模式。

目标平台：

- Windows amd64/arm64
- macOS amd64/arm64
- Linux amd64/arm64
- FreeBSD 作为可选支持平台

---

## 3. 功能需求

## 3.1 Phase 1 — NPA Core

### FR-01 启动与本地 Web

- FR-01.1 执行 `npa` 应直接启动 NPA 服务，不要求子命令。
- FR-01.2 Web UI 与 REST API 应固定监听 `127.0.0.1:56700`。
- FR-01.3 Web 静态资源应嵌入 NPA 二进制。
- FR-01.4 Web 应作为唯一主要人机管理入口。
- FR-01.5 Web 至少包含：概览、节点拓扑、SSH 交互式终端、隧道、设置（含 MCP 权限）、日志/审计、关于；独立文件页面在当前界面中暂不展示。
- FR-01.6 Web 页面状态变化应通过 SSE、WebSocket 或短轮询自动更新，不要求整页刷新。
- FR-01.7 默认不得监听 `0.0.0.0`；如未来允许远程 Web，必须显式开启认证和 TLS/安全代理。

### FR-02 无用户配置文件设计

- FR-02.1 NPA 普通运行不得要求 `config.yaml`、`config.json`、`.toml` 等用户配置文件。
- FR-02.2 用户配置应通过 Web UI 修改。
- FR-02.3 配置、节点、权限、隧道定义和状态应存储于内部状态数据库。
- FR-02.4 内部状态数据库属于实现细节，不作为用户编辑接口。
- FR-02.5 应提供合理默认值，使首次启动无需任何配置即可打开 Web。
- FR-02.6 启动参数仅保留必要诊断类参数，例如 `--version`、`--help`、`--log-level`、`--data-dir`；不得要求用户通过大量 CLI 参数配置业务功能。
- FR-02.7 服务开关、MCP 权限、节点和隧道配置修改后应即时生效或明确提示需要重启。

### FR-03 节点管理

- FR-03.1 一期节点界面默认展示本地节点，不提供手工添加入口；已管理节点由内部状态库承载，三期 Discovery 接入后在节点拓扑中展示自动发现节点。
- FR-03.2 节点至少记录：`node_id`、名称、地址、SSH 端口、用户名、认证方式、标签、最近连接状态。
- FR-03.3 SSH 默认端口为 22，但允许按节点修改。
- FR-03.4 节点凭据不得明文展示或写入日志。
- FR-03.5 应支持 SSH 私钥认证；可选支持口令认证。
- FR-03.6 应记录 SSH Host Key 指纹并支持首次信任/变更告警。
- FR-03.7 节点连接失败不得影响其他节点。

### FR-04 SSH

- FR-04.1 Web SSH 页面只提供交互式终端，默认选择本地节点并在本地 shell 中执行命令。
- FR-04.2 应支持远程节点交互式终端/PTY，供 Web 人工操作。
- FR-04.3 应支持连接超时、命令超时、KeepAlive、断线状态检测。
- FR-04.4 同一节点应尽量复用 SSH 连接，避免每次操作重新握手。
- FR-04.5 SSH 实现应优先基于 Go `golang.org/x/crypto/ssh`。
- FR-04.6 MCP 的 SSH 能力关闭时，MCP 不得调用命令执行、终端等 SSH 工具，但 Web 人工功能可按本地管理员策略独立控制。

### FR-05 文件 / SFTP / FTP

- FR-05.1 一期文件能力应以 SFTP 为首选协议。
- FR-05.2 应支持目录列表、上传、下载、重命名、删除、新建目录和文件属性读取。
- FR-05.3 SFTP 应复用现有 SSH 会话。
- FR-05.4 应优先使用成熟 Go SFTP 实现（如 `github.com/pkg/sftp`）。
- FR-05.5 对仅支持传统 FTP 的设备，可提供 FTP Client Adapter；FTP 与 SFTP 在 Web 中统一呈现为“文件”能力，但协议类型必须明确显示。
- FR-05.6 大文件传输不得通过 MCP Base64 整体返回，应使用分块、临时资源或受控下载接口。
- FR-05.7 MCP File Capability 关闭时，MCP 不得执行文件读取、上传、删除等操作。

### FR-06 Tunnel

- FR-06.1 应支持 SSH 本地端口转发（Local Forward，`-L` 语义）。
- FR-06.2 应支持 SSH 反向端口转发（Remote Forward，`-R` 语义）。
- FR-06.3 应支持 SOCKS5 动态转发（`-D` 语义）。
- FR-06.4 应支持将本机 HTTP/Web 请求代理至目标节点可达的 Web 服务。
- FR-06.5 应正确处理 WebSocket、HTTP chunked、Host Header 等常见 Web 场景。
- FR-06.6 隧道定义应持久化，进程重启后可按用户设置自动恢复。
- FR-06.7 隧道断开应支持指数退避重连和 KeepAlive。
- FR-06.8 Tunnel 实现应优先复用 `x/crypto/ssh`，并参考/复用 Chisel 等成熟项目的重连、KeepAlive、多端点设计，避免重复实现已验证机制。
- FR-06.9 动态分配的本地转发端口不要求固定范围；系统应检测端口冲突并可自动选择空闲端口。
- FR-06.10 MCP Tunnel Capability 关闭时，MCP 不得创建、修改或关闭隧道。

### FR-07 Web Proxy

- FR-07.1 `56700` 同时作为 Web 管理入口和受控 Web Proxy 入口。
- FR-07.2 管理界面建议位于 `/` 或 `/ui/`，代理入口使用独立路径/路由规则，避免与管理 API 冲突。
- FR-07.3 Web 应提供“一键打开目标 Web 服务”能力，由 NPA 自动建立必要转发。
- FR-07.4 代理规则必须经过权限和目标地址校验。
- FR-07.5 代理到公网目标默认禁止，除非用户明确开启。

### FR-08 MCP Server

- FR-08.1 MCP Server 应优先采用官方 `github.com/modelcontextprotocol/go-sdk`。
- FR-08.2 MCP Streamable HTTP 固定监听 `127.0.0.1:56701`。
- FR-08.3 可保留 stdio MCP 作为本地进程集成方式。
- FR-08.4 MCP HTTP 服务可在 Web 设置中启用/关闭。
- FR-08.5 MCP Server 必须准确声明当前可用 Tools 和 Capabilities。
- FR-08.6 被关闭的 Capability 对应工具应不可调用；实现可选择不注册该工具或返回明确的 `permission_denied`。
- FR-08.7 MCP 单次响应应有大小限制；大日志和大文件应分页/分块。
- FR-08.8 MCP HTTP 应启用适当的 Host/Origin/认证保护，防止本地服务被恶意网页跨站调用或 DNS Rebinding 利用。
- FR-08.9 MCP 与 Web 管理操作应复用同一业务服务层，不允许 MCP 绕过权限直接访问底层执行器。

### FR-09 MCP Capability 权限

MCP 权限不再使用笼统的 `read/write` 两档作为唯一模型，而采用显式 Capability。

一期至少包含：

| Capability | 默认 | 控制范围 |
| --- | --- | --- |
| `ssh` | OFF | 命令执行、终端、SSH 诊断 |
| `file` | OFF | SFTP/FTP 文件读写 |
| `tunnel` | OFF | 创建/修改/删除端口转发和代理 |

二期增加：

| Capability | 默认 | 控制范围 |
| --- | --- | --- |
| `screenshot` | OFF | 屏幕截图与屏幕信息 |
| `mouse` | OFF | 鼠标移动、点击、滚轮 |

三期可增加：

| Capability | 默认 | 控制范围 |
| --- | --- | --- |
| `discovery` | OFF | 查询/刷新自动发现信息 |
| `relay` | OFF | 通过其他 NPA 节点代理访问子网 |

要求：

- FR-09.1 所有 MCP Capability 默认关闭，首次使用由用户显式授权。
- FR-09.2 `screenshot` 与 `mouse` 必须独立授权。
- FR-09.3 `mouse` 属于高权限能力，开启时 Web 应醒目标识。
- FR-09.4 Capability 修改应写入审计日志。
- FR-09.5 Capability 检查必须位于所有 MCP 执行路径的统一入口。

### FR-10 审计与安全

- FR-10.1 MCP 发起的 SSH、文件写入、隧道修改、截图、鼠标操作和 Relay 应记录审计。
- FR-10.2 审计至少记录：时间、调用来源、工具、目标节点、参数摘要、结果、耗时。
- FR-10.3 密码、私钥、Token、敏感文件内容不得进入普通日志。
- FR-10.4 应提供并发和速率限制。
- FR-10.5 对目标地址执行私网/公网判断；默认禁止 MCP 建立面向公网的任意代理。
- FR-10.6 Web、MCP、内部服务均不得存在绕过统一 Policy 的执行入口。

---

## 3.2 Phase 2 — Desktop + Plugin Runtime

### FR-11 Plugin Runtime

- FR-11.1 NPA Core 必须继续保持纯 Go，默认发布构建要求 `CGO_ENABLED=0`。
- FR-11.2 无法可靠通过纯 Go 跨平台实现、依赖 CGO、系统 SDK 或平台原生库的能力，应封装为独立插件进程，不得污染 NPA Core 的交叉编译能力。
- FR-11.3 插件与 Core 采用轻量、语言无关的本地 IPC；当前协议定义为 **JSON-RPC 2.0 + stdio**。
- FR-11.4 JSON-RPC 消息采用明确的长度分帧（推荐 LSP 风格 `Content-Length` framing），不得依赖“一行一个 JSON”作为长期协议边界。
- FR-11.5 `stdin` 用于 Core → Plugin RPC，`stdout` 专用于 Plugin → Core RPC/Notification，`stderr` 专用于插件日志，插件不得向 stdout 输出非协议文本。
- FR-11.6 第二期插件框架首先服务于 NPA 官方插件，不要求开放任意第三方插件安装。
- FR-11.7 插件应支持初始化、自描述、健康检查、调用、流式事件、取消和优雅关闭。
- FR-11.8 插件进程崩溃不得导致 NPA Core 崩溃；Plugin Manager 应记录状态并可按策略重启。
- FR-11.9 插件缺失或不可用时，如果该功能存在安全、可维护的跨平台系统命令/脚本实现，应回退到 NPA 内嵌 Script Fallback。
- FR-11.10 Fallback 脚本应通过 `go:embed` 等方式随 Core 发布，不要求用户维护脚本文件。
- FR-11.11 大截图、大文件等二进制数据不得默认 Base64 塞入 JSON-RPC；应优先通过受控临时文件/资源引用传递，并由 Core 管理生命周期。
- FR-11.12 插件只是能力执行器；所有来自 MCP/Web 的调用仍必须经过 Core 的 Policy、Capability 和 Audit，不允许插件绕过权限层。

Provider 优先级原则：

```text
Pure-Go Core Provider
        ↓ 不支持
Platform-specific Plugin
        ↓ 不存在/不可用
Universal Plugin
        ↓ 不存在/不可用
Embedded Script Fallback
        ↓ 不支持
not_supported
```

### FR-12 Screenshot


- FR-12.1 应支持获取屏幕列表、分辨率、缩放比例等信息。
- FR-12.2 应支持全屏和指定屏幕截图。
- FR-12.3 截图输出优先 PNG；MCP 可返回资源引用或受大小限制的图像内容。
- FR-12.4 Screenshot Capability 必须单独授权。
- FR-12.5 跨平台实现优先采用操作系统原生接口或稳定开源封装；平台相关实现应隔离，不得破坏主程序核心模块。

### FR-13 Mouse Control

- FR-13.1 应支持鼠标绝对/相对移动。
- FR-13.2 应支持左/右/中键点击与按下/释放。
- FR-13.3 应支持滚轮。
- FR-13.4 Mouse Capability 默认关闭。
- FR-13.5 鼠标控制必须记录审计。
- FR-13.6 鼠标工具必须有参数边界检查，避免异常坐标或无限操作序列。
- FR-13.7 后续键盘控制若加入，应作为新的独立 Capability，不自动继承 Mouse 权限。

---

## 3.3 Phase 3 — NPA Mesh

### FR-14 Discovery

- FR-16.1 Discovery 可在 Web 中独立开启/关闭。
- FR-16.2 Discovery 不占用 NPA 自定义发现端口。
- FR-16.3 局域网发现优先使用标准 mDNS/DNS-SD `5353/UDP`。
- FR-16.4 服务类型建议使用 `_npa._tcp.local.`。
- FR-16.5 mDNS 广播仅携带发现所需的最小信息，不传输凭据。
- FR-16.6 广播信息至少包含：节点 ID、名称、版本、NPA 服务端口/入口、Capabilities 摘要。
- FR-16.7 详细节点信息应在发现后通过受认证的 NPA Node API 获取。
- FR-16.8 一期手工节点管理必须继续可用；Discovery 只是增强能力，不能成为 SSH/SFTP/Tunnel 的强依赖。
- FR-16.9 可保留受限主动探测作为可选补充，但不得默认进行大范围网段扫描。

### FR-15 子网能力上报

- FR-16.1 NPA 节点应能读取本机接口和路由信息，形成 `reachable_subnets`。
- FR-16.2 节点可向已认证的其他 NPA 节点上报自己可访问的私有子网。
- FR-16.3 `reachable_subnets` 仅代表网络可达性声明，不代表目标设备已经授权。
- FR-16.4 默认只允许 RFC1918、链路本地等私有范围作为自动 Relay 子网。
- FR-16.5 Web 应展示“本节点可达子网”和“其他节点宣告的可达子网”。

### FR-16 Relay / 子网代理

- FR-16.1 当控制端无法直连目标地址，但某已认证 NPA 节点声明可达目标子网时，可经该节点 Relay。
- FR-16.2 Relay 应复用现有 Tunnel/SSH 能力，不自造新的数据面协议。
- FR-16.3 对上层 SSH、File、Tunnel、Web Proxy 应尽量透明。
- FR-16.4 Relay 节点必须显式允许 `relay` 能力。
- FR-16.5 MCP 使用 Relay 还必须同时拥有目标操作对应 Capability。
- FR-16.6 Relay 路径必须进入审计日志。
- FR-16.7 多个 Relay 可达同一子网时，应基于直连优先、跳数、健康状态和延迟选择路径。
- FR-16.8 初始版本建议最多 1 个中继跳；多跳 Mesh 作为后续增强，避免三期首次实现过度复杂。
- FR-16.9 Relay 断线应自动恢复；无法恢复时应明确返回目标不可达，而不是静默切换到未经授权节点。

---


## 3.4 Phase 4 — General Third-party Plugin Ecosystem

### FR-17 Zero-Manifest Plugin

- FR-17.1 第三方插件不得强制要求 `plugin.json`、YAML、TOML、XML 等旁路 Manifest。
- FR-17.2 插件文件本身是唯一必须交付的插件实体；插件身份、版本、作者、说明、权限、工具、参数 Schema、示例和帮助均通过运行时自描述接口获取。
- FR-17.3 支持至少以下插件形态：
  - Python：`.py`
  - JavaScript：`.js` / `.mjs`
  - Windows Native：`.exe`
  - Unix/macOS/Linux 可执行文件与 Shebang 脚本
- FR-17.4 NPA 应根据扩展名、可执行属性和 Shebang 选择 Launcher；缺少 Python/Node 等运行时时应在 Web 中显示 `Runtime Missing`，不得导致 Core 启动失败。
- FR-17.5 插件协议必须保持语言无关；第三方开发者不使用官方 SDK也能按协议实现插件。
- FR-17.6 官方可提供 Go、Python、JavaScript、C++、Rust 等轻量 SDK，但 SDK 不是协议的一部分，也不是运行插件的强制依赖。

### FR-18 插件目录与平台覆盖

默认插件根目录结构：

```text
plugins/
├── hello.py
├── network_tools.js
├── git_plugin
├── windows/
│   └── desktop.exe
├── macos/
│   └── desktop
├── linux/
│   └── desktop
└── freebsd/
    └── system_tools
```

- FR-18.1 `plugins/` 根目录直接放置**通用平台插件**。
- FR-18.2 `plugins/windows/`、`plugins/macos/`、`plugins/linux/`、`plugins/freebsd/` 放置对应平台插件。
- FR-18.3 NPA 只扫描根目录与当前 OS 对应目录；不得执行其他平台目录内容。
- FR-18.4 第一版不递归扫描任意深度目录，避免把插件依赖脚本误识别为插件。
- FR-18.5 后续可扩展 `<os>/<arch>/` 层级；优先级为 `OS+ARCH > OS > Universal`。
- FR-18.6 若多个候选插件自描述后具有相同 `plugin_id`，应按平台具体度选择唯一实现，不得同时注册冲突工具。
- FR-18.7 插件路径只是候选来源；最终插件身份必须来自插件自描述的稳定 `plugin_id`，不能依赖文件名作为永久 ID。

### FR-19 插件自描述协议

插件启动后必须完成 `plugin.initialize` 握手，返回至少：

- `protocol_version`
- `plugin_id`
- `name`
- `version`
- `description`
- `author`（可选）
- `runtime`
- `capabilities`
- `permissions`
- `tools`
- `help/examples`（可选）

工具描述至少包含：

- `name`
- `title`
- `description`
- `inputSchema`（JSON Schema）
- `outputSchema`（可选）
- `permissions`
- `risk`
- `examples`（可选）

- FR-19.1 Web 应根据自描述信息自动展示插件功能和用法。
- FR-19.2 MCP 可根据已授权工具描述动态注册 Tool。
- FR-19.3 安装/加载插件不得自动授予 MCP 权限。
- FR-19.4 插件协议必须支持版本协商；无兼容协议版本时显示 `PLUGIN_PROTOCOL_INCOMPATIBLE`。
- FR-19.5 插件可通过 JSON-RPC Notification 提供 `stream.data`、`stream.progress`、`stream.error`、`stream.end` 等流式事件。

### FR-20 Web 插件管理

Web 增加 `Plugins` 页面，至少支持：

- Detect / Refresh
- Load
- Unload
- Start
- Stop
- Restart
- Enable / Disable
- Auto Start
- 查看插件日志
- 查看功能、工具、参数、示例和帮助
- 配置插件 MCP Access
- 按工具配置 MCP 允许/禁止

要求：

- FR-20.1 Web 上传插件的本质是将文件复制到正确插件目录并触发 Detect。
- FR-20.2 用户也可以直接将插件文件放入目录，NPA 应自动或手动刷新识别。
- FR-20.3 `Stop` 只停止插件进程；`Unload` 从当前 Plugin Registry 移除；删除文件属于卸载插件实体。
- FR-20.4 插件 Enabled/AutoStart/MCP 权限等用户设置存储在 NPA 内部状态库，不生成插件旁路配置文件。
- FR-20.5 Web 应显示插件来源：Universal/Platform、脚本/Native、运行时、文件路径、版本和当前状态。

### FR-21 第三方插件安全

- FR-21.1 将文件放入插件目录只代表“发现候选插件”，不得自动授予 MCP Tool 权限。
- FR-21.2 首次加载未知插件时应显示来源、请求权限和风险信息，由用户显式信任。
- FR-21.3 由于 Probe 本身会执行第三方代码，NPA 不得把 Probe 描述成安全沙箱；未知插件必须明确提示“运行第三方本机代码”的风险。
- FR-21.4 后续可支持官方签名、发布者签名和哈希校验；官方插件可显示 Verified 状态。
- FR-21.5 插件 stdout 协议异常、超时、崩溃、无限输出必须被 Core 隔离和限流。
- FR-21.6 插件动态权限采用字符串命名空间，例如 `plugin.serial.device`，不得要求每增加第三方插件就修改 Core 的 Capability 枚举。
- FR-21.7 插件工具仍叠加 Risk Level（Read/Write/Control/Network），用于 Web 提示、策略和审计。


## 4. MCP 工具建议

### 4.1 Phase 1

| 工具 | Capability | 说明 |
| --- | --- | --- |
| `nodes.list` | 基础只读 | 已管理节点 |
| `ssh.exec` | ssh | 执行命令 |
| `ssh.session_info` | ssh | SSH 会话状态 |
| `file.list` | file | 列目录 |
| `file.read` | file | 读取小文件/分块 |
| `file.write` | file | 写入/上传 |
| `file.remove` | file | 删除 |
| `tunnel.list` | tunnel | 查询隧道 |
| `tunnel.open` | tunnel | 建立隧道 |
| `tunnel.close` | tunnel | 关闭隧道 |
| `service.open_web` | tunnel | 建立目标 Web 访问入口 |

### 4.2 Phase 2

| 工具 | Capability |
| --- | --- |
| `desktop.screens` | screenshot |
| `desktop.screenshot` | screenshot |
| `desktop.mouse_move` | mouse |
| `desktop.mouse_click` | mouse |
| `desktop.mouse_scroll` | mouse |

### 4.3 Phase 3

| 工具 | Capability |
| --- | --- |
| `discovery.nodes` | discovery |
| `discovery.refresh` | discovery |
| `network.subnets` | discovery |
| `relay.routes` | relay |
| `relay.open` | relay + 对应目标能力 |

### 4.4 Phase 4

第三方插件 Tool 不在 Core 中硬编码。插件自描述后进入 Tool Registry；只有用户显式开启该插件/Tool 的 MCP Access 后，才映射为 MCP Tool。Tool 名称必须稳定且避免与 Core Tool 冲突。

---

## 5. 网络端口约定

| 端口 | 协议 | 默认监听 | 用途 | 阶段 |
| --- | --- | --- | --- | --- |
| **56700** | TCP/HTTP | `127.0.0.1` | Web UI、REST API、受控 Web Proxy | Phase 1 |
| **56701** | TCP/HTTP | `127.0.0.1` | MCP Streamable HTTP | Phase 1 |
| **5353** | UDP/mDNS | 标准 mDNS 组播 | NPA Discovery；不是 NPA 私有端口 | Phase 3 |
| 22 | TCP/SSH | 目标节点 | SSH/SFTP/SSH Tunnel | 外部标准服务 |

约束：

1. NPA 不预留 `56702+` 作为 Discovery 端口。
2. Discovery 默认完全复用 mDNS/DNS-SD。
3. 若未来确实需要跨广播域发现，应优先通过已发现 NPA 节点汇总/Relay、用户指定种子节点或现有网络基础设施解决，不提前占用一组私有端口。
4. Tunnel 的临时本地端口按需动态分配，不使用固定的大范围保留端口。

---

## 6. 数据需求

### 6.1 内部状态

建议至少包含：

- settings
- managed_nodes
- credentials_ref
- ssh_host_keys
- tunnel_definitions
- mcp_capabilities
- audit_index
- discovered_nodes（Phase 3）
- relay_routes（Phase 3）

### 6.2 凭据

凭据存储应采用以下优先级：

1. 操作系统安全存储/Keychain/Credential Manager/Secret Service（若实现条件允许）；
2. 本地加密存储；
3. 不允许明文持久化密码和私钥口令。

---

## 7. 非功能需求

| 编号 | 类别 | 目标 |
| --- | --- | --- |
| NFR-01 | 启动 | 本机启动后 Web/MCP 应快速可用 |
| NFR-02 | 分发 | 主程序尽量单二进制 |
| NFR-03 | 依赖 | 优先标准库和成熟开源项目 |
| NFR-04 | 配置 | 普通用户无需手工编辑配置文件 |
| NFR-05 | 安全 | Web/MCP 默认仅监听回环 |
| NFR-06 | 权限 | MCP 高权限能力默认关闭 |
| NFR-07 | 隔离 | 单节点/单隧道故障不得影响其他会话 |
| NFR-08 | 恢复 | Tunnel 支持 KeepAlive 与自动重连 |
| NFR-09 | 可观测 | 关键操作有结构化日志和审计 |
| NFR-10 | 兼容 | Windows/macOS/Linux 为主要目标平台 |
| NFR-11 | 稳定 | 畸形 MCP/REST 参数不得导致进程 panic |
| NFR-12 | 发现 | Discovery 关闭时不得产生 mDNS 广播 |
| NFR-13 | 隐私 | Discovery 广播不得包含密码、Token、用户名凭据等敏感信息 |
| NFR-14 | 可演进 | Phase 1 不依赖 Phase 2/3/4 模块 |
| NFR-15 | 纯 Go Core | 官方 Core 构建保持 `CGO_ENABLED=0`，平台原生依赖通过独立插件隔离 |
| NFR-16 | 插件隔离 | 插件崩溃、缺失运行时或协议错误不得导致 Core 崩溃 |
| NFR-17 | 插件兼容 | 第三方插件协议语言无关，不要求 Go 工具链或 Protobuf/gRPC |
| NFR-18 | 零 Manifest | 第三方单文件插件无需旁路 Manifest 即可被识别和自描述 |

---

## 8. 开源复用要求

优先评估并锁定稳定版本：

| 模块 | 首选 |
| --- | --- |
| MCP | `github.com/modelcontextprotocol/go-sdk` |
| SSH | `golang.org/x/crypto/ssh` |
| SFTP | `github.com/pkg/sftp` |
| Tunnel | `x/crypto/ssh`；参考/复用 Chisel 的可靠性设计 |
| HTTP/Web | Go `net/http` |
| Discovery | `github.com/grandcat/zeroconf` |
| 日志 | Go `log/slog` |
| 前端 | 内嵌静态资源；优先轻量 HTML/HTMX/少量 JS |
| Plugin IPC | JSON-RPC 2.0 + stdio + Content-Length framing；Core 可仅依赖 Go 标准库 |
| Plugin Schema | JSON Schema；尽量与 MCP Tool Schema 对齐 |
| 状态库 | pure-Go SQLite 或 bbolt，最终实现前做许可证/体积/并发评估 |

要求：

- 固定依赖版本；
- 维护第三方许可证清单；
- 发布前检查依赖安全公告；
- 不直接复制无法兼容本项目许可证的代码；
- 对 Chisel/GOST 等项目优先借鉴成熟架构或复用可独立引入的包，不为了“复用”而强行引入整套复杂框架。

---

## 9. 验收里程碑

### Phase 1 验收

1. `npa` 启动后可访问 `http://127.0.0.1:56700`。
2. MCP 开启后 `127.0.0.1:56701` 可正常完成 MCP 连接。
3. 节点页默认显示本地节点并可点击查看节点信息；Discovery 拓扑位置预留至三期。
4. Web SSH 页面提供交互式终端，默认选择本地节点并在本地 shell 执行命令；远程节点支持 PTY。
5. 文件浏览、上传和下载能力保留在 SFTP 服务层与 API 中，当前 Web 导航不展示独立文件页面。
6. 可创建 Local/Remote/SOCKS Tunnel。
7. 可通过 NPA 打开目标节点 Web 服务。
8. MCP 的 SSH/File/Tunnel 权限可独立开启/关闭。
9. 禁止的 MCP 工具无法绕过权限调用。
10. 重启后节点和隧道定义可恢复。
11. 不存在必须由用户维护的 YAML/JSON/TOML 配置文件。

### Phase 2 验收

1. NPA Core 仍可在单一开发平台以 `CGO_ENABLED=0` 交叉编译主要目标平台。
2. 官方 Desktop Plugin 以独立进程运行，通过 JSON-RPC 2.0/stdin/stdout 与 Core 通信。
3. Desktop Plugin 缺失时，存在可用系统命令的能力可回退到内嵌 Script Fallback。
4. 插件崩溃不会导致 Core 崩溃。
5. 可截图并正确识别多屏信息。
6. 可执行鼠标移动、点击和滚轮。
7. Screenshot 与 Mouse 权限独立。
8. Mouse 默认关闭且操作有审计。

### Phase 3 验收

1. 开启 Discovery 后，同广播域 NPA 可通过 mDNS 自动发现。
2. 关闭 Discovery 后不发送 NPA mDNS 广播。
3. 不使用任何 NPA 私有 Discovery 端口。
4. 节点可上报可达私有子网。
5. A 无法直连目标子网时，可经已授权 B Relay 访问。
6. Web 可展示发现节点、可达子网和 Relay 路径。
7. Relay 仍受 MCP Capability 和审计控制。


### Phase 4 验收

1. 将 `.py`、`.js`、`.exe` 或当前系统可执行插件直接放入约定目录即可被识别，不要求 `plugin.json`。
2. 根目录通用插件和当前平台目录插件均能识别，其他平台目录不会被执行。
3. 相同 `plugin_id` 存在通用和平台专用实现时，平台专用实现优先。
4. 插件名称、版本、功能、权限、Tool Schema、示例和帮助均通过 IPC 自描述获取。
5. Web 可 Load/Unload/Start/Stop/Restart/Enable 插件并查看功能和用法。
6. Python/Node 运行时缺失时 Web 能明确提示，不影响 Core。
7. 第三方插件 Tool 可动态映射到 MCP，但默认无 MCP 权限。
8. 用户可按插件或 Tool 配置 MCP Access。
9. 插件异常退出、超时和非法协议输出被隔离。
10. 不同语言插件可在不修改 NPA Core 的情况下实现同一 NPA Plugin Protocol。

---

**文档结束**
