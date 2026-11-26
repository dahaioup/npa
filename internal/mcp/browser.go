package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/npa/npa/internal/browser"
	"github.com/npa/npa/internal/policy"
)

// registerBrowserTools 把 obscura 的 browser_* 工具挂到 NPA 的 MCP 面上。
//
// 工具名、参数 schema、语义全部保持 obscura 原样（上游升级时 NPA 零改动），
// NPA 只做两件事：
//
//  1. 给每个工具的 schema 追加 node_id —— 决定这次调用在哪台机器上跑；
//  2. 把 obscura 的内容块转成 MCP 标准内容块。
//
// 工具清单来自内嵌目录（obscura v0.2.7 的快照）：即使本机没装 obscura，
// 工具也要挂着——调用可以派发到装了 obscura 的远端 NPA 节点。
func (m *Manager) registerBrowserTools(capabilities map[string]bool) {
	if !capabilities["browser"] {
		return
	}
	tools := browser.Catalog()
	if len(tools) == 0 {
		return
	}
	for _, tool := range tools {
		tool := tool
		m.server.AddTool(&mcpsdk.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: browser.WithNodeID(tool.InputSchema),
		}, func(ctx context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return m.callBrowserTool(ctx, tool.Name, request)
		})
		m.browserTools = append(m.browserTools, tool.Name)
	}
}

// callBrowserTool 执行一次浏览器工具调用，并把结果转成 MCP 内容块。
func (m *Manager) callBrowserTool(ctx context.Context, tool string, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	arguments := map[string]any{}
	if request != nil && request.Params != nil && len(request.Params.Arguments) > 0 {
		if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
			return nil, err
		}
	}
	nodeID, arguments := browser.SplitNodeID(arguments)
	result, err := m.service.BrowserCall(policy.WithSource(ctx, policy.SourceMCP), nodeID, tool, arguments)
	if err != nil {
		return nil, err
	}
	return browserResult(result), nil
}

// browserResult 把 NPA 的中立内容块转成 MCP 内容块。
func browserResult(result browser.CallResult) *mcpsdk.CallToolResult {
	out := &mcpsdk.CallToolResult{IsError: result.IsError}
	for _, content := range result.Content {
		switch content.Type {
		case "image":
			out.Content = append(out.Content, &mcpsdk.ImageContent{
				Data:     decodeBase64(content.Data),
				MIMEType: content.MimeType,
			})
		case "resource":
			out.Content = append(out.Content, &mcpsdk.EmbeddedResource{
				Resource: &mcpsdk.ResourceContents{
					URI:      content.URI,
					MIMEType: content.MimeType,
					Text:     content.Text,
					Blob:     decodeBase64(content.Data),
				},
			})
		default:
			out.Content = append(out.Content, &mcpsdk.TextContent{Text: content.Text})
		}
	}
	return out
}

// decodeBase64 解 base64 文本（解码失败时返回 nil，内容块按空处理）。
func decodeBase64(data string) []byte {
	if data == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil
	}
	return decoded
}
