package browser

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// toolsJSON 是 obscura v0.2.7 的 MCP server 实际暴露的工具目录（38 个
// browser_* 工具，含参数 schema 与描述），由 obscura mcp 的 tools/list 导出。
//
// 这份快照的作用：即使本机没装 obscura，NPA 也要把工具挂到 MCP 上——
// 因为浏览器调用可以派发到远端 NPA 节点执行，那台节点才需要 obscura。
// 插件在跑时以插件上报的实时清单为准（obscur 升级后新增的工具会自动出现）。
//
//go:embed tools.json
var toolsJSON []byte

var (
	catalogOnce sync.Once
	catalog     []Tool
)

// Catalog 返回内嵌的 obscura 工具目录。
func Catalog() []Tool {
	catalogOnce.Do(func() {
		if err := json.Unmarshal(toolsJSON, &catalog); err != nil {
			return
		}
	})
	return catalog
}

// ToolNames 返回目录里的全部工具名。
func ToolNames() []string {
	tools := Catalog()
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

// nodeIDProperty 是 NPA 加在每个 obscura 工具上的参数：指定目标节点。
const nodeIDProperty = "node_id"

// NodeIDDescription 是 node_id 参数的说明文案。
const NodeIDDescription = "目标 NPA 节点 ID；留空或填本机 ID 表示在本机执行浏览器"

// WithNodeID 在 obscura 的工具 schema 上补出 node_id 参数。
// obscura 自己的 schema 一个参数都不认识，NPA 在这层统一加节点路由能力：
// 工具语义完全不变，只是多一个"在谁的机器上跑"的选择。
func WithNodeID(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	var document map[string]any
	if err := json.Unmarshal(schema, &document); err != nil {
		return schema
	}
	if document == nil {
		document = map[string]any{}
	}
	document["type"] = "object"
	properties, _ := document["properties"].(map[string]any)
	if properties == nil {
		properties = map[string]any{}
	}
	properties[nodeIDProperty] = map[string]any{
		"type":        "string",
		"description": NodeIDDescription,
	}
	document["properties"] = properties
	encoded, err := json.Marshal(document)
	if err != nil {
		return schema
	}
	return encoded
}

// SplitNodeID 从参数里摘出 node_id，返回剩下的工具参数。
func SplitNodeID(arguments map[string]any) (string, map[string]any) {
	nodeID, _ := arguments[nodeIDProperty].(string)
	delete(arguments, nodeIDProperty)
	return nodeID, arguments
}
