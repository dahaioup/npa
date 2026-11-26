package plugin

// Builtin 描述一个随 core 内置的能力插件：没有独立进程，实现体就在 core
// 内部，但对外暴露与外部插件同形的元数据（ID / 能力 / 权限）。
//
// 内置插件与外部插件是同一个 Provider 契约的两种实现：外部插件按优先级
// 顶替内置实现（见 SAD §8.7 Provider Router 优先级）。因此内置能力同样
// 是「可替换」的，而调用方（MCP 工具、Web API）不需要感知差异。
type Builtin struct {
	ID           string
	Name         string
	Version      string
	Protocol     string
	Capabilities []string
	// Permission 是该能力在权限模型里的命名（如 core.file）。
	Permission string
	// Settings 是该内置插件自己声明的可配置参数。
	Settings []SettingSpec
}

// Info 返回与外部插件同形的元数据。
func (b Builtin) Info() Info {
	return Info{
		ID:           b.ID,
		Name:         b.Name,
		Version:      b.Version,
		Protocol:     b.Protocol,
		Capabilities: b.Capabilities,
		Permission:   b.Permission,
		Settings:     b.Settings,
	}
}
