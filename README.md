# npa

NPA (Node Proxy Agent) 是一款专为局域网多机联调与 AI 自动化排障设计的轻量级、跨平台节点调试代理网关。

通过标准化的 MCP (Model Context Protocol) 协议，NPA 让外部 AI Agent 与开发终端能够无缝接管并观测局域网目标机；借助纯出站的 SSH 客户端隧道与动态端口映射，消除传统调试对公网 IP 和复杂网络配置的依赖。核心程序采用纯 Go 构建，零 CGO 污染，实现全平台“单机编译、一键分发”。

## Phase 1 — NPA Core

当前仓库已实现一期核心能力：

- 单二进制启动，Web 固定监听 `127.0.0.1:56700`，MCP 固定监听 `127.0.0.1:56701`；
- Web 管理概览、节点拓扑占位、SSH 交互式终端、Local/Remote/SOCKS5 隧道、设置（含 MCP 权限）和审计；
- 节点页默认展示本地节点，不提供手工添加入口；三期 Discovery 接入后在此展示自动发现节点拓扑，点击节点查看详情；
- SSH 页面只保留交互式终端，默认选择本地节点并在本地 shell 中执行命令；
- 节点、隧道、设置、审计和加密凭据保存在内部 bbolt 状态库，不维护用户配置文件；
- MCP 使用官方 Go SDK，默认只注册 `nodes.list`；SSH、File、Tunnel Capability 默认关闭并可在设置页即时开关；
- SSH 使用 `x/crypto/ssh`，SFTP 使用 `pkg/sftp`，支持 TOFU Host Key 指纹、密钥/口令/Agent 认证和连接复用；
- 隧道复用 SSH `direct-tcpip`/`forwarded-tcpip`，带 KeepAlive、状态持久化和自动恢复；
- Web Proxy 通过 `/proxy/{tunnel-id}/` 走受控反向代理，默认拒绝公网目标；
- 所有 MCP 调用经过统一 Capability、速率限制和脱敏审计入口。

文件浏览 API 和 SFTP 适配器仍保留在服务层，当前 Web 导航暂不展示独立“文件”页面。

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
