# NPA · Node Proxy Agent

通过代理节点，让你的 AI 操作局域网内的所有设备。

- 中文说明见下方；English summary at the bottom.
- 项目主页：https://github.com/dahaioup/npa
- 插件仓库：npa-remote（Web 远程控制 / 截图 / 输入 / 剪贴板），https://github.com/dahaioup/npa-remote
- 插件清单仓库：https://github.com/dahaioup/npa-plugins

## 功能概览

| 层 | 能力 |
|---|---|
| 接入层 | Web 管理面（127.0.0.1:56700）、Mesh 节点间通道（56701）、mDNS 局域网发现 |
| 编排层 | 服务编排、MCP 工具服务、插件槽位管理、节点升级与守护重启 |
| 能力层 | 节点管理、SSH/PTY、文件传输、统一通道（本地/远程端口转发、SOCKS）、屏幕截图与键鼠注入、剪贴板、远程桌面 |
| 基础设施 | 多节点互联、跨域中继、拓扑同步、审计日志 |

## 快速开始

1. 编译：`go build ./cmd/npa`
2. 运行：`./npa`（Web 界面默认监听回环 56700）
3. 通过 MCP 工具或 Web 界面添加节点、注册插件、远程操作。

## 统一通道

隧道与端口映射合并为统一的「通道」模型，Web 与 MCP 共用一套接口：

- 类型：本地端口转发、远程端口转发、动态端口转发（SOCKS 代理）；
- 链路中含 SSH 节点即 SSH 通道，否则为 NPA 通道；
- 远程端口转发支持经由节点跳板（先连跳板再在跳板上开 direct-tcpip 到目标）。

MCP 工具：`channel.list`、`channel.open`、`channel.close`。

## 插件仓库地址

插件仓库地址可在 Web 界面「插件仓库」页配置（留空则使用内置默认地址）。

## English

NPA (Node Proxy Agent) lets your AI operate every device on your LAN through proxy nodes.
It provides a local web console, an MCP tool surface, peer-to-peer mesh transport with
mDNS discovery, SSH/PTY, file transfer, unified channels (local/remote port forwarding and
SOCKS), remote screen control and auditing.

- Project: https://github.com/dahaioup/npa
- Remote control plugin: https://github.com/dahaioup/npa-remote
- Plugin registry: https://github.com/dahaioup/npa-plugins

## License

见仓库内 LICENSE 文件。
