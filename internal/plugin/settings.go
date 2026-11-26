package plugin

import "strings"

// SettingSpec 描述插件自己声明的一个参数。
//
// 插件在 initialize 握手（Info.Settings）或 plugin.settings.describe 里声明
// 自己有哪些参数、每个参数是什么意思、哪些参数允许插件自己改。core 按这份
// 声明渲染「插件设置」页，并把插件自报的值落回设置库——参数归谁管，由插件
// 自己说了算。
type SettingSpec struct {
	// Key 是插件内部的参数名（在插件自己的命名空间里唯一）。
	Key string `json:"key"`
	// Label 是给人看的名字。
	Label string `json:"label,omitempty"`
	// Type 是取值类型：string / bool / number / password / enum。留空表示 string。
	Type string `json:"type,omitempty"`
	// Options 是枚举参数的候选值（Type 为 enum 时生效）。页面渲染成下拉框。
	Options []string `json:"options,omitempty"`
	// Default 是插件给出的默认值。
	Default string `json:"default,omitempty"`
	// Description 是补充说明。
	Description string `json:"description,omitempty"`
	// Setting 是该参数在 NPA 设置库里的落点（扁平设置键）。留空表示落在
	// plugin.<槽位>.<Key>。内置插件沿用已有的扁平键，避免老库里的值失效。
	Setting string `json:"setting,omitempty"`
	// Writable 表示该参数允许插件自己修改：插件自报的值会被 core 落库并
	// 覆盖用户填的值。没声明可写的参数，core 一律不采纳插件自报的值。
	Writable bool `json:"writable,omitempty"`
}

// SettingsSchema 是插件声明的参数表。
type SettingsSchema struct {
	Settings []SettingSpec `json:"settings"`
}

// SettingsValues 是插件自报的参数值（key → 字符串值）。
type SettingsValues struct {
	Values map[string]string `json:"values"`
}

// SettingsApply 是 core 下发给插件的参数变更。
type SettingsApply struct {
	Values map[string]string `json:"values"`
}

// 插件协议里与参数声明相关的方法名。
const (
	// MethodSettingsDescribe 取参数声明（插件自报有哪些参数）。
	MethodSettingsDescribe = "plugin.settings.describe"
	// MethodSettingsGet 取插件当前生效的参数值。
	MethodSettingsGet = "plugin.settings.get"
	// MethodSettingsApply 把 core 侧的参数变更推给插件。
	MethodSettingsApply = "plugin.settings.apply"
)

// SettingKey 返回该参数在 NPA 设置库里的落点键。
// slot 是插件槽位（desktop / file / browser / manager …）。
func (s SettingSpec) SettingKey(slot string) string {
	if setting := strings.TrimSpace(s.Setting); setting != "" {
		return setting
	}
	return PluginSettingKey(slot, s.Key)
}

// PluginSettingKey 返回插件参数在设置库里的默认落点键。
func PluginSettingKey(slot, key string) string {
	return "plugin." + strings.TrimSpace(slot) + "." + strings.TrimSpace(key)
}

// PluginEnabledKey 返回插件启用状态在设置库里的落点键。
func PluginEnabledKey(slot string) string {
	return "plugin." + strings.TrimSpace(slot) + ".enabled"
}

// PluginSchemaKey 返回插件参数声明在设置库里的落点键。
func PluginSchemaKey(slot string) string {
	return "plugin." + strings.TrimSpace(slot) + ".schema"
}

// IsPluginSettingKey 判断一个设置键是否属于插件参数命名空间。
func IsPluginSettingKey(key string) bool {
	return strings.HasPrefix(key, "plugin.")
}
