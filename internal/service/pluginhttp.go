package service

import (
	"strconv"
	"strings"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/plugin"
)

// 插件 HTTP 服务的通用机制。
//
// 插件可以带自己的 HTTP 服务（自带页面、自己的 MCP 端点）。core 统一管三件事：
//
//   - 端口：<槽位>_plugin_port（默认取槽位登记的端口）；
//   - 开放度：<槽位>_plugin_exposure，auto / local / lan。
//     auto 表示跟随 NPA 自己的局域网开关（lan_enabled）；local 只监听回环；
//     lan 监听所有网卡；
//   - MCP 开关：<槽位>_plugin_mcp。关掉之后插件的 MCP 只在 56700 本地面
//     可用，56701（mesh 面）不再对外提供——插件只管自己本机，跨机器一律
//     走 NPA 代理。
//
// 外部插件由 core 传 --http-port / --http-bind 让它自己绑定；
// 内置插件（文件分发）由 core 直接起监听。

const (
	// PluginExposureAuto 跟随 NPA 的局域网开关。
	PluginExposureAuto = "auto"
	// PluginExposureLocal 只监听本机回环。
	PluginExposureLocal = "local"
	// PluginExposureLAN 监听所有网卡。
	PluginExposureLAN = "lan"
)

// PluginHTTPInfo 描述某个插件槽位的 HTTP 服务现状。
type PluginHTTPInfo struct {
	// Slot 是槽位名（desktop / file / browser …）。
	Slot string `json:"slot"`
	// Port 是插件 HTTP 服务的端口；0 表示这个槽位没有 HTTP 服务。
	Port int `json:"port"`
	// Exposure 是开放度原值（auto / local / lan）。
	Exposure string `json:"exposure,omitempty"`
	// Scope 是解析后的绑定范围（local / lan）。
	Scope string `json:"scope,omitempty"`
	// HasMCP 表示插件在 HTTP 上提供 MCP 服务。
	HasMCP bool `json:"has_mcp,omitempty"`
	// MCPEnabled 表示插件的 MCP 服务是否启用。
	MCPEnabled bool `json:"mcp_enabled"`
	// PageURL 是插件自带页面的地址（127.0.0.1 视角）。
	PageURL string `json:"page_url,omitempty"`
	// MCPPath 是 core 代理插件 MCP 的路径前缀。
	MCPPath string `json:"mcp_path,omitempty"`
}

// PluginHTTP 返回某个槽位的 HTTP 服务信息；槽位不存在或没有 HTTP 服务时
// 返回 nil。
func (s *Service) PluginHTTP(slot string) *PluginHTTPInfo {
	found, ok := s.findPluginSlot(slot)
	if !ok || found.httpPort <= 0 {
		return nil
	}
	info := &PluginHTTPInfo{
		Slot:       found.name,
		Port:       s.PluginHTTPPort(found.name),
		Exposure:   s.PluginHTTPExposure(found.name),
		Scope:      s.PluginScope(found.name),
		HasMCP:     found.hasMCP,
		MCPEnabled: s.PluginMCPEnabled(found.name),
		MCPPath:    "/plugins/" + found.name + "/mcp",
	}
	info.PageURL = "http://127.0.0.1:" + strconv.Itoa(info.Port) + "/"
	return info
}

// PluginHTTPPort 返回插件 HTTP 服务的端口。
func (s *Service) PluginHTTPPort(slot string) int {
	found, ok := s.findPluginSlot(slot)
	if !ok || found.httpPort <= 0 {
		return 0
	}
	value := strings.TrimSpace(s.Settings()[slot+"_plugin_port"])
	if value == "" {
		return found.httpPort
	}
	port, err := strconv.Atoi(value)
	if err != nil || port <= 0 || port > 65535 {
		return found.httpPort
	}
	return port
}

// PluginHTTPExposure 返回开放度原值（auto / local / lan）；没配或配错时
// 退回槽位的默认值。
func (s *Service) PluginHTTPExposure(slot string) string {
	found, ok := s.findPluginSlot(slot)
	if !ok {
		return PluginExposureAuto
	}
	value := strings.ToLower(strings.TrimSpace(s.Settings()[slot+"_plugin_exposure"]))
	switch value {
	case PluginExposureAuto, PluginExposureLocal, PluginExposureLAN:
		return value
	}
	if found.httpExposure != "" {
		return found.httpExposure
	}
	return PluginExposureAuto
}

// PluginScope 把开放度解析成实际绑定范围：auto 跟随 NPA 的局域网开关，
// local 永远只听回环，lan 永远听所有网卡。
func (s *Service) PluginScope(slot string) string {
	switch s.PluginHTTPExposure(slot) {
	case PluginExposureLocal:
		return "local"
	case PluginExposureLAN:
		return "lan"
	}
	if parseBool(s.Settings()["lan_enabled"]) {
		return "lan"
	}
	return "local"
}

// PluginBindHost 返回插件 HTTP 服务应该绑定的地址。
func (s *Service) PluginBindHost(slot string) string {
	if s.PluginScope(slot) == "local" {
		return "127.0.0.1"
	}
	return "0.0.0.0"
}

// PluginHasMCP 判断该槽位的插件是否在 HTTP 上提供 MCP 服务。
func (s *Service) PluginHasMCP(slot string) bool {
	found, ok := s.findPluginSlot(slot)
	return ok && found.hasMCP
}

// PluginMCPEnabled 判断插件的 MCP 服务是否启用。
//
// 只有提供了 MCP 服务的槽位才有这个开关；关掉之后插件的 MCP 只在 56700
// 本地面可用，不再经 56701 对外提供。
func (s *Service) PluginMCPEnabled(slot string) bool {
	found, ok := s.findPluginSlot(slot)
	if !ok || !found.hasMCP {
		return true
	}
	value := strings.TrimSpace(s.Settings()[slot+"_plugin_mcp"])
	if value == "" {
		return true
	}
	return parseBool(value)
}

// PluginMCPAllowed 判断某个面（local / mesh）上是否允许访问该插件的 MCP。
//
// local 面（56700）永远允许——插件的 MCP 只能在本机用；
// mesh 面（56701）要求插件的 MCP 开关是开的。
func (s *Service) PluginMCPAllowed(slot, face string) bool {
	if !s.PluginHasMCP(slot) {
		return false
	}
	if !s.PluginEnabled(slot) {
		return false
	}
	if face == "mesh" && !s.PluginMCPEnabled(slot) {
		return false
	}
	return true
}

// pluginHTTPSpecs 返回 core 自己管的插件 HTTP 参数声明。
//
// 插件自己声明的同名参数优先（文件插件就自己声明了 port），
// core 只补它没声明的那些。
func pluginHTTPSpecs(slot pluginSlot) []plugin.SettingSpec {
	if slot.httpPort <= 0 {
		return nil
	}
	specs := []plugin.SettingSpec{
		{
			Key: "port", Label: "HTTP 端口", Type: "number",
			Setting:     slot.name + "_plugin_port",
			Default:     strconv.Itoa(slot.httpPort),
			Description: "插件自己 HTTP 服务的端口；NPA 通过它代理插件的 MCP",
		},
		{
			Key: "exposure", Label: "端口开放度", Type: "enum",
			Options:     []string{PluginExposureAuto, PluginExposureLocal, PluginExposureLAN},
			Setting:     slot.name + "_plugin_exposure",
			Default:     slot.httpExposure,
			Description: "auto=跟随 NPA 局域网开关；local=只监听本机；lan=监听所有网卡",
		},
	}
	if slot.hasMCP {
		specs = append(specs, plugin.SettingSpec{
			Key: "mcp", Label: "MCP 服务", Type: "bool",
			Setting:     slot.name + "_plugin_mcp",
			Default:     "true",
			Description: "关掉后插件的 MCP 只在 56700 本地面可用，不再经 56701 对外提供",
		})
	}
	return specs
}

// mergePluginHTTPSpecs 把 core 的 HTTP 参数声明并进插件自己的声明里
// （同名以插件为准）。
func mergePluginHTTPSpecs(slot pluginSlot, specs []plugin.SettingSpec) []plugin.SettingSpec {
	declared := map[string]bool{}
	for _, spec := range specs {
		declared[spec.Key] = true
	}
	for _, spec := range pluginHTTPSpecs(slot) {
		if declared[spec.Key] {
			continue
		}
		specs = append(specs, spec)
	}
	return specs
}

// validatePluginExposure 校验开放度取值。
func validatePluginExposure(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case PluginExposureAuto, PluginExposureLocal, PluginExposureLAN:
		return true
	}
	return false
}

// validatePluginParamValue 校验 core 自己管的 HTTP 参数取值：
// 端口要是合法端口号，开放度只能是 auto / local / lan。
func validatePluginParamValue(spec plugin.SettingSpec, value string) error {
	switch spec.Key {
	case "exposure":
		if !validatePluginExposure(value) {
			return apperrors.New(apperrors.InvalidArgument, "端口开放度只能是 auto / local / lan: "+value)
		}
	case "port":
		port, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || port <= 0 || port > 65535 {
			return apperrors.New(apperrors.InvalidArgument, "端口号不合法: "+value)
		}
	}
	return nil
}
