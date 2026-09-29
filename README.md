# npa

NPA (Node Proxy Agent) 是一款专为局域网多机联调与 AI 自动化排障设计的轻量级、跨平台节点调试代理网关。

通过标准化的 MCP (Model Context Protocol) 协议，NPA 让外部 AI Agent 与开发终端能够无缝接管并观测局域网目标机；借助纯出站的 SSH 客户端隧道与动态端口映射，消除传统调试对公网 IP 和复杂网络配置的依赖。核心程序采用纯 Go 构建，零 CGO 污染，实现全平台“单机编译、一键分发”。