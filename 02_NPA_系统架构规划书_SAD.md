# 《系统架构规划书》 - NPA (Node Proxy Agent)

---

## 1. 引言

### 1.1 文档标识

- **文档类型**：系统架构规划书（Software Architecture Document, SAD）
- **文档版本**：v2.1
- **上游依据**：`01_NPA_软件需求规格说明书_SRS.md`
- **实现语言**：Go 1.22+
- **主程序目标**：单二进制、优先零 CGO、少配置、固定本地端口
- **固定端口**：
  - Web：`127.0.0.1:56700`
  - MCP HTTP：`127.0.0.1:56701`
  - Discovery：不定义 NPA 私有端口，Phase 3 使用标准 mDNS `5353/UDP`

### 1.2 架构原则

1. **单进程优先**：Control、Web、MCP、SSH、File、Tunnel、Policy 在同一 NPA 进程。
2. **协议复用**：MCP、SSH、SFTP、HTTP、mDNS 使用成熟协议。
3. **开源复用**：优先成熟 Go 项目，不重新实现完整协议栈。
4. **无用户配置文件**：业务配置通过 Web 修改，内部数据库持久化。
5. **Capability 权限**：MCP 权限按能力独立控制。
6. **阶段解耦**：Phase 1 不依赖 Desktop 或 Discovery。
7. **数据面复用**：Phase 3 Relay 复用 Tunnel/SSH，不创建新的私有数据面协议。
8. **安全默认值**：Web/MCP 回环监听，高权限 Capability 默认关闭。

---

## 2. 总体架构

### 2.1 Phase 1 部署视图

```text
                         AI / IDE / Agent
                              |
                       MCP Streamable HTTP
                         127.0.0.1:56701
                              |
                              v
+================================================================+
|                           NPA                                  |
|                                                                |
|  +-------------+       +---------------+                       |
|  | MCP Server  |------>| Capability /  |                       |
|  +-------------+       | Policy/Audit  |                       |
|                        +-------+-------+                       |
|                                |                               |
|  Browser                       v                               |
|     |                +---------+----------+                    |
|     | :56700         |   Service Layer   |                    |
|     v                +---------+----------+                    |
| +---------+                    |                               |
| | Web UI  |--------------------+                               |
| | REST API|                    |                               |
| +---------+          +---------+---------+---------+           |
|                      |                   |         |           |
|                      v                   v         v           |
|                 +---------+         +--------+ +--------+      |
|                 |   SSH   |         |  File  | | Tunnel |      |
|                 +----+----+         +---+----+ +---+----+      |
|                      |                  |          |            |
|                      +--------+---------+----------+            |
|                               | SSH session                     |
+===============================+================================+
                                |
                                v
                         Target SSH Server
                          SSH / SFTP / TCP
```

Phase 1 不包含自动发现。节点页默认展示本地节点；已管理节点由内部状态库承载，三期 Discovery 接入后再展示自动发现拓扑。

### 2.2 Phase 2 — Plugin Runtime + Desktop

```text
                       NPA Core
                 Pure Go / CGO=0
                         |
                  Provider Router
             +-----------+-----------+
             |                       |
      Pure-Go Provider         Plugin Manager
                                     |
                         JSON-RPC 2.0 / stdio
                                     |
                              Desktop Plugin
                            (独立原生进程)
                                     |
                    screen / screenshot / mouse

Plugin unavailable
        |
        v
Embedded Script Fallback
```

第二期建立官方 Plugin Runtime。Desktop 是第一个参考插件，用于验证插件生命周期、IPC、自描述、错误隔离和脚本降级。Core 不链接 Desktop 的 CGO/原生库。

### 2.3 Phase 3 Mesh

```text
                       mDNS / DNS-SD
                        5353/UDP
                 _npa._tcp.local.
                       /       \
                      /         \
                   NPA-A       NPA-B
                     |           |
                     |           +---- reachable_subnets
                     |                 10.10.0.0/24
                     |
                     +--- route decision
                              |
                              v
                         Tunnel / Relay
                              |
                            NPA-B
                              |
                              v
                        10.10.0.50:8080
```

Discovery 只负责“找到节点和能力入口”。真正的数据访问继续走认证后的 NPA/SSH/Tunnel 路径。


### 2.4 Phase 4 — Third-party Plugin Ecosystem

```text
plugins/
├── universal.py
├── tools.js
├── windows/
│   └── native.exe
├── macos/
│   └── native
└── linux/
    └── native

             |
             v
       Plugin Scanner
             |
       Plugin Manager
             |
  JSON-RPC 2.0 / stdio
             |
        Tool Registry
        /          \
       v            v
      Web          MCP
```

第三方插件可以由 Go、Python、JavaScript、C/C++、Rust、C# 等任意可执行技术实现。Core 只依赖 NPA Plugin Protocol。

---

## 3. 端口与网络边界

### 3.1 固定端口

| 端口 | 地址 | 用途 |
| --- | --- | --- |
| `56700/tcp` | `127.0.0.1` | Web UI + REST API + Web Proxy |
| `56701/tcp` | `127.0.0.1`（默认）或局域网范围 | MCP Streamable HTTP；范围由 Web 设置控制 |
| `5353/udp` | mDNS 标准组播 | Phase 3 Discovery；非 NPA 私有端口 |

NPA **不预留发现端口范围**。

### 3.2 动态端口

SSH Local Forward / SOCKS 等本地监听端口：

- 由用户指定；或
- 由系统通过 `:0`/空闲端口选择动态分配。

不再固定 `18000-18999` 等端口池。

### 3.3 网络边界

- Web 和 MCP 默认只绑定 loopback。
- SSH/SFTP/Tunnel 为主动出站连接。
- Phase 3 mDNS 仅在 Discovery 开启时加入组播并发布服务。
- 访问公网地址的代理/Relay 默认禁止或需要显式授权。
- MCP HTTP 必须实施 Host/Origin/认证等本地 HTTP 安全措施。

---

## 4. 进程与模块规划

### 4.1 单进程

```text
cmd/npa
  |
  +-- app
      |
      +-- web
      +-- mcp
      +-- service
      +-- policy
      +-- audit
      +-- state
      +-- nodes
      +-- ssh
      +-- file
      +-- tunnel
      +-- plugin        # Phase 2 runtime / Phase 4 ecosystem
      |
      +-- desktop       # Phase 2 provider abstraction
      |
      +-- discovery     # Phase 3
      +-- relay         # Phase 3
```

v1~v3 不设计 Control/Gateway 多进程拆分。未来如出现强需求再通过 ADR 重新评估。

### 4.2 分层

```text
Presentation
  Web / REST / MCP
        |
        v
Application Service
  NodeService
  SSHService
  FileService
  TunnelService
  DesktopService
  DiscoveryService
  RelayService
        |
        v
Policy + Audit
        |
        v
Infrastructure
  SSH Client
  SFTP/FTP Adapter
  Tunnel Engine
  State DB
  OS Desktop Adapter
  mDNS Adapter
```

Web 和 MCP 不直接调用底层协议模块。

---

## 5. Phase 1 模块

### 5.1 `internal/web`

职责：

- Web UI
- REST API
- SSE/WebSocket 状态推送
- Web Proxy
- 登录/本地会话（如启用）
- 调用 Application Service

实现：

- `net/http`
- `httputil.ReverseProxy`
- `embed.FS`
- 前端优先轻量 HTML + HTMX + 少量 JavaScript

不建议一期引入大型前端框架和独立 Node.js 运行时。

### 5.2 `internal/mcp`

首选依赖：

`github.com/modelcontextprotocol/go-sdk`

职责：

- MCP Server
- Streamable HTTP `:56701`
- 可选 stdio Transport
- Tool 注册
- 参数 Schema
- Capability 到 Tool 的映射
- 错误转换

MCP 模块只做协议适配，不实现 SSH/File/Tunnel 业务。

```text
MCP Tool
   |
Capability.Check
   |
Application Service
   |
Audit
```

依赖版本必须锁定到经过安全修复的稳定版本，并在升级时运行协议兼容测试。

### 5.3 `internal/policy`

权限不再以单一 `read/write` 为核心。

```go
type Capability uint64

const (
    CapSSH Capability = 1 << iota
    CapFile
    CapTunnel
    CapScreenshot
    CapMouse
    CapDiscovery
    CapRelay
)
```

建议同时保留 Operation Risk：

```text
Read
Write
Control
Network
```

Capability 决定“能不能进入某类模块”，Risk 用于审计和后续细粒度策略。

例如：

```text
file.list      -> CapFile + Read
file.write     -> CapFile + Write
mouse.click    -> CapMouse + Control
tunnel.open    -> CapTunnel + Network
```

### 5.4 `internal/ssh`

首选：

`golang.org/x/crypto/ssh`

职责：

- SSH Client
- Host Key 校验
- 密钥/密码认证
- KeepAlive
- Session Pool
- Exec
- PTY
- `direct-tcpip`
- `forwarded-tcpip`
- 供 SFTP/Tunnel/Relay 复用底层连接

建议接口：

```go
type Manager interface {
    Connect(ctx context.Context, node Node) (*Client, error)
    Client(nodeID string) (*Client, bool)
    Exec(ctx context.Context, nodeID string, req ExecRequest) (ExecResult, error)
    Shell(ctx context.Context, nodeID string, opts PTYOptions) (PTYSession, error)
    Close(nodeID string) error
}
```

### 5.5 `internal/file`

采用 Adapter：

```text
FileService
   |
   +-- SFTPAdapter
   |
   +-- FTPAdapter (兼容)
```

#### SFTP

首选：

`github.com/pkg/sftp`

直接复用 `internal/ssh` 的 SSH Client。

#### FTP

传统 FTP 不复用 SSH。仅在确有兼容需求时启用 FTP Adapter。

FTP Adapter 必须：

- 明确显示 FTP 为明文协议风险；
- 支持 PASV；
- 不与 SFTP 凭据混用；
- 默认不自动尝试 FTP。

UI 统一叫“文件”，连接类型显示：

```text
SFTP
FTP
```

### 5.6 `internal/tunnel`

职责：

- Local Forward
- Remote Forward
- SOCKS5
- Web Proxy Dial
- KeepAlive
- Retry
- Tunnel Definition
- Traffic counters
- Lifecycle

数据面优先：

`golang.org/x/crypto/ssh`

可靠性设计参考 Chisel：

- exponential backoff
- keepalive
- dead connection detection
- multiple endpoints/session reuse
- reconnect lifecycle

不建议直接 fork Chisel 形成第二个独立进程；优先复用可抽取思想/组件，使 NPA 的节点、权限和状态模型保持统一。

接口建议：

```go
type TunnelManager interface {
    Open(ctx context.Context, spec Spec) (Tunnel, error)
    Close(id string) error
    List() []Tunnel
    Restore(ctx context.Context) error
}
```

### 5.7 `internal/state`

目标：替代用户配置文件。

对上提供 Repository：

```go
type Store interface {
    Settings() SettingsRepository
    Nodes() NodeRepository
    Tunnels() TunnelRepository
    Permissions() PermissionRepository
    Audit() AuditRepository
}
```

实现候选：

- pure-Go SQLite
- bbolt

选择标准：

- Windows/macOS/Linux 一致性
- 零 CGO
- 崩溃恢复
- 并发读写
- 体积
- 许可证

状态数据库不得要求用户手工编辑。

### 5.8 `internal/audit`

建议结构化事件：

```json
{
  "time": "...",
  "source": "mcp",
  "operation": "ssh.exec",
  "node_id": "...",
  "capability": "ssh",
  "result": "success",
  "duration_ms": 123
}
```

参数必须先脱敏。

---

## 6. Web UI 信息架构

```text
Overview
Nodes
  ├─ mDNS Topology
  ├─ Node Detail (click to inspect)
  └─ Reachable Subnets / Relay Routes
SSH
Tunnels
Audit
Settings
  ├─ MCP Service Scope
  ├─ MCP Service ON/OFF
  ├─ Discovery Service ON/OFF
  ├─ Discovery Permission
  ├─ Relay Permission
  ├─ SSH Permission
  ├─ File Permission
  └─ Tunnel Permission
About
```

### 6.1 Settings

不显示配置文件概念。

```text
General
  Start on boot
  Log level
  Update

Services
  MCP              ON/OFF
  Discovery         ON/OFF   # Phase 3

MCP Permissions
  SSH               ON/OFF
  File              ON/OFF
  Tunnel            ON/OFF
  Screenshot        ON/OFF
  Mouse             ON/OFF
  Discovery         ON/OFF
  Relay             ON/OFF

Security
  Credential management
  Host key trust
  Audit retention
```

---

## 7. MCP 架构

### 7.1 工具注册

Tool Registry 由 Capability 驱动：

```text
Tool Definition
   |
   +-- name
   +-- schema
   +-- required capability
   +-- risk level
   +-- handler
```

调用链：

```text
MCP Request
   |
Schema Validation
   |
Capability Check
   |
Rate Limit
   |
Application Service
   |
Audit
   |
MCP Response
```

### 7.2 Phase 1 Tool Mapping

```text
nodes.list             Base
ssh.exec               SSH
ssh.session_info       SSH

file.list              File
file.read              File
file.write             File
file.remove            File

tunnel.list            Tunnel
tunnel.open            Tunnel
tunnel.close           Tunnel
service.open_web        Tunnel
```

### 7.3 大数据

以下内容不得直接无限制放入 MCP Response：

- 大文件
- 大日志
- 超大截图
- 长时间终端输出

统一采用：

```text
limit
offset/cursor
resource reference
temporary local URL
```

等方式控制。

---


## 8. Plugin Runtime 架构（Phase 2 基础设施）

### 8.1 Pure-Go Core 边界

Core 正式构建必须保持：

```text
CGO_ENABLED=0
```

以下能力优先进入插件：

- 必须 CGO；
- 必须链接平台 SDK/C/C++ 库；
- 无法稳定跨平台交叉编译；
- 会显著增加 Core 平台耦合。

插件为独立进程，因此其实现语言和构建方式不影响 Core。

### 8.2 IPC

正式 Plugin Protocol：

```text
JSON-RPC 2.0
    +
Content-Length framing
    +
stdio
```

进程通道：

```text
NPA -> plugin stdin      RPC Request / Notification
NPA <- plugin stdout     RPC Response / Notification
NPA <- plugin stderr     Human-readable plugin logs
```

不分配固定 TCP 端口，不使用 Unix Socket/Named Pipe 作为第一版强制接口，也不要求 gRPC/Protobuf。

Core 可主要使用：

```text
os/exec
encoding/json
bufio
io
context
```

从而保持依赖轻量。

### 8.3 Framing

推荐：

```text
Content-Length: <bytes>\r\n
\r\n
<JSON-RPC message bytes>
```

禁止插件向 stdout 写调试日志；调试输出必须走 stderr。

### 8.4 基础协议

建议最小方法集：

```text
plugin.initialize
plugin.getInfo
plugin.health
plugin.shutdown

tool.list
tool.invoke
tool.cancel
```

通知：

```text
stream.data
stream.progress
stream.error
stream.end

plugin.log          # 可选结构化日志
plugin.status       # 可选状态变化
```

`plugin.initialize` 完成：

- 协议版本协商；
- 插件身份；
- Capability；
- Permission；
- Tool Schema；
- 帮助与示例；
- Runtime 信息。

### 8.5 二进制/大数据

JSON-RPC 作为控制面，不承担大型二进制数据面。

截图/大文件返回：

```json
{
  "resource": {
    "type": "file",
    "path": "<controlled-temp-path>",
    "mimeType": "image/png",
    "size": 123456
  }
}
```

临时文件必须位于 Core 分配/认可的工作目录，Core 负责校验、读取和清理，避免插件通过任意路径引用越权文件。

### 8.6 Plugin Manager

职责：

```text
scan
launch
initialize
negotiate protocol
register tools
health
stop
restart
timeout
crash isolation
stderr capture
resource cleanup
```

状态：

```text
Detected -> Probing -> Loaded -> Running
                 \-> Incompatible
                 \-> RuntimeMissing
                 \-> Failed
```

### 8.7 Provider Router

```text
Pure-Go Core
    >
Platform Plugin
    >
Universal Plugin
    >
Embedded Script Fallback
```

“>`”表示同一能力存在多个 Provider 时的默认优先级。用户可在未来高级设置中覆盖，但第一版无需暴露复杂路由配置。


## 9. Phase 2 Desktop 架构

### 9.1 Adapter

```go
type Desktop interface {
    Screens(ctx context.Context) ([]Screen, error)
    Screenshot(ctx context.Context, req CaptureRequest) (CaptureResult, error)
    MoveMouse(ctx context.Context, req MoveRequest) error
    MouseButton(ctx context.Context, req ButtonRequest) error
    Scroll(ctx context.Context, req ScrollRequest) error
}
```

平台实现：

```text
desktop_windows.go
desktop_darwin.go
desktop_linux.go
```

优先系统原生 API 或成熟轻量封装。

### 9.2 权限隔离

```text
Screenshot
    |
 CapScreenshot

Mouse
    |
 CapMouse
```

绝不使用一个 `desktop` 总开关替代二者。

### 9.3 CGO

主程序仍以零 CGO 为目标。

若某平台可靠的截图/鼠标方案必须 CGO：

1. 优先寻找系统命令/系统 API 的纯 Go 方案；
2. 无法满足时通过构建标签隔离；
3. 不应因此让 SSH/MCP/Tunnel 核心强制依赖 CGO。

---

## 10. Phase 3 Discovery 架构

### 10.1 mDNS

首选：

`github.com/grandcat/zeroconf`

服务：

```text
_npa._tcp.local.
```

建议 mDNS Instance：

```text
NPA-<hostname>-<short-node-id>
```

TXT 只放最小元数据：

```text
id=<node-id>
ver=<version>
api=56701
caps=ssh,file,tunnel,relay
```

注意：`api=56701` 只是服务元信息示例；实际 Node-to-Node API 是否与 MCP HTTP 共用端口，应在 Phase 3 开发前通过 ADR 最终确定。Discovery 本身不因此增加端口。

### 10.2 Discovery 状态

```text
Unknown
   |
Discovered
   |
Trusted
   |
Managed
   |
Offline
```

发现 ≠ 信任。

新发现节点不得自动获得 SSH、Relay 或 MCP 权限。

### 10.3 详细能力获取

mDNS 仅负责定位。

```text
mDNS
  |
  v
Found Node
  |
  v
Authenticated Node Metadata
  |
  +-- version
  +-- capabilities
  +-- interfaces
  +-- reachable_subnets
  +-- health
```

详细信息不塞入 TXT Record。

### 10.4 跨网段

不重新引入“大范围扫描优先”的旧方案。

优先顺序：

1. 当前广播域 mDNS；
2. 已管理/已信任节点上报其可达子网和已知 NPA；
3. 用户手工添加种子节点；
4. 必要时再提供受限主动探测作为高级选项。

这样可以避免扫描 `/16`、`/8` 等网络。

---

## 11. Phase 3 Relay 架构

### 11.1 Route Model

```go
type Route struct {
    TargetSubnet netip.Prefix
    ViaNodeID    string
    Hops         []string
    RTT          time.Duration
    Healthy      bool
}
```

三期初版：

```text
max_hops = 1
```

即：

```text
NPA-A -> NPA-B -> Target
```

多跳：

```text
A -> B -> C -> Target
```

作为后续增强，不作为三期首版硬要求。

### 11.2 路由选择

优先级：

```text
Direct
  >
Explicit User Route
  >
Healthy Trusted Relay
```

同级 Relay 再比较：

- hop count
- health
- RTT
- recent failures

### 11.3 Relay 数据面

禁止再造：

```text
NPA Relay Protocol v1
```

数据面复用：

- SSH `direct-tcpip`
- Tunnel Dialer
- HTTP Proxy
- 已建立的认证 NPA 通道

最终让上层看到统一接口：

```go
type Dialer interface {
    DialContext(ctx context.Context, network, address string) (net.Conn, error)
}
```

直连：

```text
DirectDialer
```

经 Relay：

```text
RelayDialer
```

SSH/File/Web/Tunnel 不需要理解路由细节。

---


## 12. Phase 4 Third-party Plugin Ecosystem

### 12.1 Zero-Manifest

不定义强制 `plugin.json`。

插件文件启动后通过 `plugin.initialize` 返回全部元信息。NPA 内部数据库只保存用户对插件的状态，例如：

```text
enabled
auto_start
trusted
mcp_access
tool_permissions
preferred_provider
```

这些状态不写到插件旁边。

### 12.2 插件目录

第一版：

```text
plugins/
├── foo.py                 # Universal
├── bar.js                 # Universal
├── tool                   # Universal executable/shebang
├── windows/
│   └── tool.exe
├── macos/
│   └── tool
├── linux/
│   └── tool
└── freebsd/
    └── tool
```

扫描范围严格限制为：

```text
plugins/*
plugins/<current-os>/*
```

后续允许：

```text
plugins/<current-os>/<current-arch>/*
```

优先级：

```text
OS+ARCH > OS > Universal
```

平台目录名称对用户采用：

```text
windows
macos
linux
freebsd
```

内部映射：

```text
macos <-> GOOS darwin
```

### 12.3 Launcher

```text
.py        -> python3 / python
.js/.mjs   -> node
.exe       -> direct exec on Windows
Unix executable -> direct exec
shebang    -> OS/interpreter exec
```

运行时缺失：

```text
RuntimeMissing
```

只影响该插件。

### 12.4 同 ID 覆盖

文件名不是插件 ID。

候选启动并自描述后按 `plugin_id` 分组：

```text
platform/arch implementation
        >
platform implementation
        >
universal implementation
```

同优先级出现重复 ID 时标记冲突并要求用户处理，避免随机选择。

### 12.5 动态 Tool Registry

```text
Plugin
  |
plugin.initialize / tool.list
  |
  v
Plugin Tool Registry
  |               |
  v               v
Web Dynamic UI   MCP Adapter
```

Tool 使用 JSON Schema 描述输入，尽量与 MCP Tool Schema 保持一致。

Web 可自动生成：

- 功能列表；
- 参数表单；
- 默认值；
- 帮助；
- 示例；
- 权限与风险提示。

MCP Adapter 只注册用户授权的 Tool。

### 12.6 动态权限

Core 内置能力可继续使用稳定名称：

```text
core.ssh
core.file
core.tunnel
desktop.screenshot
desktop.mouse
```

第三方：

```text
plugin.<plugin-id>.<permission>
```

例如：

```text
plugin.serial.device
plugin.docker.control
```

每个 Tool 同时声明：

```text
Read
Write
Control
Network
```

等 Risk Level。

### 12.7 Web 生命周期

```text
Detect
  -> Probe
  -> Trust
  -> Load
  -> Start

Stop
Unload
Restart
Disable
```

含义：

- `Detect`：发现候选文件；
- `Probe`：启动并尝试协议握手；
- `Trust`：用户接受第三方代码及其权限请求；
- `Load`：注册插件元数据/Tools；
- `Start`：保持插件运行；
- `Stop`：结束进程但保留已安装信息；
- `Unload`：从 Registry 移除；
- `Disable`：后续不自动加载。

Web “Install Plugin”仅提供文件复制便利；直接复制到插件目录效果一致。

### 12.8 安全边界

Probe 会执行代码，因此不是沙箱。

必须：

- 对未知插件提示本机代码执行风险；
- 插件 MCP Access 默认 OFF；
- Tool 权限默认 OFF 或要求显式授权；
- 限制握手超时、消息大小、并发和 stdout 输出速率；
- 限制临时资源目录；
- 捕获崩溃；
- 记录插件来源与哈希；
- 后续支持签名/Publisher Verification。


## 13. 依赖规划

| 能力 | 推荐实现 | 策略 |
| --- | --- | --- |
| MCP | `modelcontextprotocol/go-sdk` | 直接使用官方 SDK |
| SSH | `golang.org/x/crypto/ssh` | 直接使用 |
| SFTP | `github.com/pkg/sftp` | 直接使用 |
| FTP | 成熟 Go FTP client adapter | 仅兼容需求启用 |
| Tunnel | `x/crypto/ssh` + Chisel 可靠性设计 | 尽量复用成熟机制 |
| HTTP | `net/http` / `httputil` | 标准库 |
| mDNS | `grandcat/zeroconf` | Phase 3 |
| State | pure-Go SQLite / bbolt | POC 后选型 |
| Logging | `log/slog` | 标准库 |
| Frontend | embed + HTMX/轻 JS | 无运行时 Node.js 依赖 |
| Plugin IPC | JSON-RPC 2.0 + stdio | 语言无关、无固定端口、Core 可仅标准库 |
| Plugin Tool Schema | JSON Schema | 与 Web 动态 UI / MCP 映射共用 |

### 13.1 第三方依赖原则

- 不以“依赖越少”为唯一目标，以稳定、安全、维护成本低为目标；
- 不重复实现 SSH/SFTP/MCP/mDNS 协议；
- 对 Chisel/GOST 类大型项目，先评估是否能复用库级组件；
- 若只能通过复制大量源码才能集成，应优先保持 NPA 自己的小型抽象并复用底层标准库；
- 每次发布生成依赖与许可证清单；
- 固定版本，不使用不可重复的 `latest` 构建。

---

## 14. 建议目录结构

```text
npa/
├── cmd/
│   └── npa/
├── internal/
│   ├── app/
│   ├── web/
│   ├── mcp/
│   ├── service/
│   ├── policy/
│   ├── audit/
│   ├── state/
│   ├── nodes/
│   ├── ssh/
│   ├── file/
│   │   ├── sftp/
│   │   └── ftp/
│   ├── tunnel/
│   ├── proxy/
│   ├── credentials/
│   ├── desktop/          # Phase 2
│   ├── discovery/        # Phase 3
│   └── relay/            # Phase 3
├── plugins/                 # 运行时插件目录（不属于 Core 源码依赖）
│   ├── windows/
│   ├── macos/
│   ├── linux/
│   └── freebsd/
├── web/
│   ├── templates/
│   └── static/
├── docs/
│   ├── 01_NPA_软件需求规格说明书_SRS.md
│   └── 02_NPA_系统架构规划书_SAD.md
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

---

## 15. 启动生命周期

```text
main
 |
 +-- init logging
 |
 +-- open state DB
 |
 +-- load settings
 |
 +-- init credential provider
 |
 +-- init policy/audit
 |
 +-- init SSH/File/Tunnel
 |
 +-- init Plugin Manager
 +-- scan/load enabled official plugins
 |
 +-- start Web :56700
 |
 +-- if MCP enabled
 |      start MCP :56701
 |
 +-- if Phase3 && Discovery enabled
 |      start mDNS
 |
 +-- restore enabled tunnels
 |
 +-- ready
```

退出：

```text
stop accepting new requests
 -> stop MCP
 -> stop Web
 -> stop Discovery
 -> close tunnels
 -> close SSH sessions
 -> flush audit/state
 -> exit
```

---

## 16. 错误模型

统一业务错误：

```text
invalid_argument
permission_denied
authentication_failed
host_key_changed
node_offline
timeout
rate_limited
port_in_use
target_forbidden
not_supported
plugin_runtime_missing
plugin_protocol_incompatible
plugin_timeout
plugin_crashed
plugin_conflict
internal_error
```

Web REST 和 MCP 分别映射成对应协议错误，但业务层使用同一错误类型。

---

## 17. 测试策略

### 17.1 Unit

重点：

- Capability
- Route selection
- Tunnel state
- Host key
- State repository
- Input validation
- Secret redaction

### 17.2 Integration

使用容器/测试 SSH Server 验证：

- SSH Exec
- PTY
- SFTP
- Local Forward
- Remote Forward
- SOCKS
- reconnect
- WebSocket Proxy

### 17.3 MCP

- 使用官方 SDK/协议兼容测试；
- 测试所有 Capability OFF/ON 组合；
- 测试畸形输入和大响应；
- 测试 HTTP Host/Origin/认证保护。

### 17.4 Phase 3

至少构造：

```text
Subnet A
  NPA-A

Subnet A + B
  NPA-B (dual-homed)

Subnet B
  target
```

验证：

```text
A discovers B
B advertises subnet B
A cannot direct-connect target
A -> B -> target succeeds
```

---


### 17.5 Plugin Runtime

验证：

- JSON-RPC framing 粘包/拆包；
- stdout 非法输出；
- stderr 日志；
- initialize 超时；
- 插件崩溃；
- 消息大小限制；
- resource path 校验；
- Script Fallback；
- Core 在无插件情况下正常运行；
- `CGO_ENABLED=0` 交叉编译矩阵。

### 17.6 Phase 4 Cross-language Plugins

至少准备：

```text
Python reference plugin
JavaScript reference plugin
Go/native reference plugin
```

验证三者：

- 无 Manifest 被识别；
- 自描述一致；
- Web 自动显示功能；
- MCP 动态 Tool；
- RuntimeMissing；
- Universal/OS override；
- 重复 plugin_id 冲突；
- Load/Unload/Start/Stop；
- Tool 权限。

## 18. 演进路线

### Phase 1 — NPA Core

必须完成：

- Web `56700`
- MCP `56701`
- Node management
- SSH Exec / PTY
- SFTP
- FTP compatibility adapter（如确有需求）
- Local/Remote/SOCKS Tunnel
- Web Proxy
- Capability Policy
- Audit
- Internal State DB
- Cross-platform build

### Phase 2 — Desktop + Plugin Runtime

必须完成：

- Pure-Go Core 边界
- JSON-RPC 2.0 + stdio Plugin Protocol v1
- Plugin Manager 生命周期
- 官方 Desktop Plugin
- Embedded Script Fallback
- Screen info
- Screenshot
- Mouse move/click/scroll
- Screenshot / Mouse 独立 Capability
- 插件崩溃隔离与日志

### Phase 3 — NPA Mesh

必须完成：

- mDNS Discovery，无私有发现端口
- Node trust
- Capability metadata
- reachable_subnets
- Relay route
- 单跳子网代理
- Web 拓扑/路由视图
- MCP Discovery/Relay 权限


### Phase 4 — General Plugin Ecosystem

必须完成：

- Zero-Manifest 单文件插件
- Universal + OS-specific 插件目录
- Python / JavaScript / Native Launcher
- 插件运行时自描述
- 协议版本协商
- Web Detect/Load/Unload/Start/Stop/Restart
- 插件功能、参数、示例、帮助自动展示
- 动态 Tool Registry
- MCP 动态 Tool 映射
- 插件/Tool 独立 MCP 权限
- Runtime Missing/Conflict/Crash 状态
- 第三方插件 Trust 流程
- 插件哈希与后续签名机制基础

后续再考虑：

- `<os>/<arch>/` 目录正式启用
- 插件仓库/在线市场
- Publisher 签名体系
- 更强的 OS Sandbox
- 复杂插件包/多文件包格式

后续版本再考虑：

- 多跳 Mesh
- 主动网段探测增强
- UDP Relay
- 键盘控制
- 多用户/团队共享网关
- 远程 Web/MCP 安全部署模式

---

## 19. 关键架构决策

| ID | 决策 |
| --- | --- |
| ADR-01 | Web 固定 `56700` |
| ADR-02 | MCP HTTP 固定 `56701` |
| ADR-03 | Discovery 不使用 NPA 私有端口，采用 mDNS 5353 |
| ADR-04 | 普通用户不维护配置文件，使用 Web + 内部状态库 |
| ADR-05 | Control/Gateway 合并为单进程 |
| ADR-06 | MCP 使用官方 Go SDK |
| ADR-07 | SSH 使用 `x/crypto/ssh` |
| ADR-08 | SFTP 使用 `pkg/sftp` |
| ADR-09 | Tunnel 数据面复用 SSH，可靠性设计参考 Chisel |
| ADR-10 | MCP 权限使用 Capability 模型 |
| ADR-11 | Screenshot 与 Mouse 权限分离 |
| ADR-12 | Phase 3 Relay 复用现有 Dialer/Tunnel，不创建私有数据面协议 |
| ADR-13 | Phase 3 初版 Relay 最多一跳，多跳延后 |
| ADR-14 | NPA Core 保持 `CGO_ENABLED=0`；原生/CGO 能力通过独立插件隔离 |
| ADR-15 | Plugin Protocol 使用 JSON-RPC 2.0 + stdio + Content-Length framing |
| ADR-16 | 插件不要求 Manifest，身份与工具通过运行时自描述获取 |
| ADR-17 | 插件目录采用 Universal 根目录 + OS 专用目录，平台实现优先于通用实现 |
| ADR-18 | 大型二进制数据通过受控资源引用传递，不默认 Base64 塞入 JSON-RPC |
| ADR-19 | Phase 2 只开放官方插件；Phase 4 开放语言无关第三方插件生态 |

---

**文档结束**
