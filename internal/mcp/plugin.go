package mcp

import (
	"context"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/npa/npa/internal/policy"
)

// registerPluginTools 挂上插件仓库相关的 MCP 工具：
// plugin.list / plugin.install / plugin.remove / plugin.update。
// 真正的下载与安装由插件管理器完成——它是内置能力（实现体在 core 里），
// 外部的 npa-plugin-manager 进程在跑时接管，不在时内置实现顶上。
// core 这一层只做鉴权与审计。
func (m *Manager) registerPluginTools(capabilities map[string]bool) {
	if !capabilities["plugin"] {
		return
	}
	mcpCtx := func(ctx context.Context) context.Context { return policy.WithSource(ctx, policy.SourceMCP) }
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "plugin.list", Description: "List plugins published in the public plugin repository and their local install state"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		if err := m.service.Authorize(mcpCtx(ctx), "plugin.list", policy.CapPlugin); err != nil {
			return nil, nil, err
		}
		result, err := m.service.PluginList(mcpCtx(ctx))
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "plugin.install", Description: "Install a plugin and its dependencies from the plugin repository"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		ID      string `json:"id" jsonschema:"plugin id from the registry"`
		Version string `json:"version,omitempty" jsonschema:"optional exact version"`
	}) (*mcpsdk.CallToolResult, any, error) {
		if err := m.service.Authorize(mcpCtx(ctx), "plugin.install", policy.CapPlugin); err != nil {
			return nil, nil, err
		}
		result, err := m.service.PluginInstall(mcpCtx(ctx), input.ID, input.Version)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "plugin.remove", Description: "Remove an installed plugin and the files it owns (the built-in plugin manager cannot be removed)"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		ID string `json:"id" jsonschema:"plugin id"`
	}) (*mcpsdk.CallToolResult, any, error) {
		if err := m.service.Authorize(mcpCtx(ctx), "plugin.remove", policy.CapPlugin); err != nil {
			return nil, nil, err
		}
		result, err := m.service.PluginRemove(mcpCtx(ctx), input.ID)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{Name: "plugin.update", Description: "Update an installed plugin to the version published in the repository"}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input struct {
		ID string `json:"id" jsonschema:"plugin id"`
	}) (*mcpsdk.CallToolResult, any, error) {
		if err := m.service.Authorize(mcpCtx(ctx), "plugin.update", policy.CapPlugin); err != nil {
			return nil, nil, err
		}
		result, err := m.service.PluginUpdate(mcpCtx(ctx), input.ID)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})
}
