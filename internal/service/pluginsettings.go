package service

// 插件设置：启用/禁用 + 插件自己声明的参数。
//
// 权限模型重构后，设置页不再逐项列出 MCP 能力开关：
//   - 核心能力（SSH / 隧道 / 节点发现 / 中继）随 MCP 服务一起开；
//   - 插件能力（文件 / 浏览器 / 桌面 / 插件管理）由插件是否启用决定；
//   - 每个插件能改哪些参数、能改哪些值，由插件自己声明（plugin.SettingSpec）。
//
// 参数落点：plugin.<槽位>.<参数>；内置插件沿用已有的扁平设置键
// （如 browser_obscura_timeout），声明里用 Setting 字段指定，core 不搬家。

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/npa/npa/internal/audit"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/plugin"
	"github.com/npa/npa/internal/policy"
)

// 插件槽位名：core 内置四个槽位，外部插件按槽位顶替内置实现。
const (
	PluginSlotDesktop = "desktop"
	PluginSlotFile    = "file"
	PluginSlotBrowser = "browser"
	PluginSlotManager = "manager"
)

// pluginSlot 描述一个槽位在 core 里的位置与它提供的能力。
type pluginSlot struct {
	name         string
	title        string
	manager      *plugin.Manager
	builtin      func() plugin.Info
	builtinName  string
	capabilities []string
	// required 表示这个槽位是内置能力，不能被停用：
	// 插件管理器就是这种——它负责装别的插件，停用它等于把装插件的路堵死。
	required bool
	// httpPort 是这个槽位插件 HTTP 服务的默认端口（0 表示没有 HTTP 服务）。
	httpPort int
	// httpExposure 是默认开放度：auto / local / lan。
	httpExposure string
	// hasMCP 表示插件在 HTTP 上提供 MCP 服务（/mcp）。
	hasMCP bool
	// mcpTLS 表示插件的 MCP 走自签 HTTPS（代理要放行自签证书）。
	mcpTLS bool
}

// PluginParamView 是一个参数的展示形态。
type PluginParamView struct {
	Key         string   `json:"key"`
	Label       string   `json:"label,omitempty"`
	Type        string   `json:"type,omitempty"`
	Options     []string `json:"options,omitempty"`
	Description string   `json:"description,omitempty"`
	Value       string   `json:"value"`
	Default     string   `json:"default,omitempty"`
	// Writable 表示该参数允许插件自己改（插件自报的值会被 core 落库）。
	Writable bool `json:"writable,omitempty"`
	// Setting 是该参数在设置库里的落点键。
	Setting string `json:"setting,omitempty"`
}

// PluginView 是「插件设置」页里一个插件的展示形态。
type PluginView struct {
	Slot    string `json:"slot"`
	Title   string `json:"title"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	Enabled bool   `json:"enabled"`
	// Required 表示这是内置能力，页面不给启停开关。
	Required     bool              `json:"required,omitempty"`
	Builtin      bool              `json:"builtin"`
	BuiltinName  string            `json:"builtin_name,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Params       []PluginParamView `json:"params"`
	// HTTP 描述插件自己的 HTTP 服务（端口、开放度、页面地址、MCP 开关）。
	HTTP *PluginHTTPInfo `json:"http,omitempty"`
}

// PluginSettingsUpdate 是一次插件设置变更：启停开关与参数值。
type PluginSettingsUpdate struct {
	Slot    string            `json:"slot"`
	Enabled *bool             `json:"enabled,omitempty"`
	Values  map[string]string `json:"values,omitempty"`
}

// pluginSlots 返回 core 管理的插件槽位。
func (s *Service) pluginSlots() []pluginSlot {
	slots := []pluginSlot{
		{
			name: PluginSlotDesktop, title: "桌面控制",
			manager: s.Plugin, capabilities: []string{"screenshot", "mouse"},
			// 远控插件本来就是给局域网里其它设备访问的：默认监听所有网卡。
			// 它的 MCP 端点与网页同一个端口，走自签 HTTPS（浏览器要 secure
			// context 才能用 WebCodecs），代理放行自签证书即可。
			httpPort: 56750, httpExposure: PluginExposureLAN, hasMCP: true, mcpTLS: true,
		},
		{
			name: PluginSlotFile, title: "文件分发",
			manager: s.FilePlugin, capabilities: []string{"file"},
			// 文件面默认跟随 NPA 的局域网开关。
			httpPort: 56751, httpExposure: PluginExposureAuto,
		},
		{
			name: PluginSlotBrowser, title: "无头浏览器",
			manager: s.BrowserPlugin, capabilities: []string{"browser"},
			// obscura 只需要本机访问：默认只监听回环，MCP 由 NPA 代理出去。
			httpPort: 56753, httpExposure: PluginExposureLocal, hasMCP: true,
		},
		{
			// 插件管理器是内置能力：随 core 编译、始终可用、不可停用。
			// plugins/manager 下的外部进程只是可选的覆盖实现。
			name: PluginSlotManager, title: "插件管理器",
			manager: s.PluginManager, capabilities: []string{"plugin"},
			required: true,
		},
	}
	// 文件槽位有内置实现（core 内置的文件分发插件），插件缺失时兜底。
	if s.Files != nil && s.Files.Builtin != nil {
		slots[1].builtin = s.Files.Builtin.Info
		slots[1].builtinName = "内置文件实现"
	}
	// 插件管理器同样有内置实现。
	if s.PluginManagerService != nil && s.PluginManagerService.Builtin != nil {
		slots[3].builtin = s.PluginManagerService.Builtin.Info
		slots[3].builtinName = "内置插件管理器"
	}
	return slots
}

// findPluginSlot 按名字找槽位。
func (s *Service) findPluginSlot(name string) (pluginSlot, bool) {
	name = strings.TrimSpace(name)
	for _, slot := range s.pluginSlots() {
		if slot.name == name {
			return slot, true
		}
	}
	return pluginSlot{}, false
}

// PluginEnabled 判断某个插件槽位是否启用（默认启用）。
//
// 内置能力（插件管理器）恒为启用：它不能被停用。
func (s *Service) PluginEnabled(slot string) bool {
	if found, ok := s.findPluginSlot(slot); ok && found.required {
		return true
	}
	settings, _ := s.Store.Settings()
	return pluginEnabledIn(settings, slot)
}

// slotEnabled 判断槽位是否启用（内置能力恒为启用）。
func (s *Service) slotEnabled(slot pluginSlot) bool {
	if slot.required {
		return true
	}
	settings, _ := s.Store.Settings()
	return pluginEnabledIn(settings, slot.name)
}

// pluginEnabledIn 按设置表判断槽位是否启用（缺省启用）。
func pluginEnabledIn(settings map[string]string, slot string) bool {
	value, ok := settings[plugin.PluginEnabledKey(slot)]
	if !ok {
		return true
	}
	return parseBool(value)
}

// PluginViews 返回「插件设置」页要渲染的全部插件。
func (s *Service) PluginViews() []PluginView {
	views := make([]PluginView, 0, 4)
	for _, slot := range s.pluginSlots() {
		views = append(views, s.pluginView(slot))
	}
	return views
}

// pluginView 组装单个插件的展示数据。
func (s *Service) pluginView(slot pluginSlot) PluginView {
	view := PluginView{
		Slot:         slot.name,
		Title:        slot.title,
		Enabled:      s.slotEnabled(slot),
		Required:     slot.required,
		Capabilities: slot.capabilities,
		Params:       []PluginParamView{},
	}
	info := plugin.Info{}
	status := "not_installed"
	if slot.manager != nil {
		current, lastError := slot.manager.Status()
		status, view.Error = current, lastError
		info = slot.manager.Info()
	}
	if info.ID == "" && slot.builtin != nil {
		info = slot.builtin()
		view.Builtin = true
		view.BuiltinName = slot.builtinName
		// 内置实现接管该槽位：外部插件缺失是预期状态，不再当成错误暴露。
		status = "builtin"
		view.Error = ""
	}
	view.ID, view.Name, view.Version = info.ID, info.Name, info.Version
	view.Status = status
	view.HTTP = s.PluginHTTP(slot.name)
	settings := s.Settings()
	for _, spec := range s.pluginSlotSpecs(slot) {
		view.Params = append(view.Params, PluginParamView{
			Key:         spec.Key,
			Label:       spec.Label,
			Type:        spec.Type,
			Options:     spec.Options,
			Description: spec.Description,
			Value:       settings[spec.SettingKey(slot.name)],
			Default:     spec.Default,
			Writable:    spec.Writable,
			Setting:     spec.SettingKey(slot.name),
		})
	}
	return view
}

// pluginSlotSpecs 返回某个槽位的参数声明：
//  1. 插件进程在跑 → 用插件自己报的（握手里的 Info.Settings，再用
//     plugin.settings.describe 补齐）；
//  2. 插件没在跑 → 用上次落库的声明（plugin.<槽位>.schema）；
//  3. 还没有落过声明 → 用 core 的兜底声明（只有内置槽位有）。
func (s *Service) pluginSlotSpecs(slot pluginSlot) []plugin.SettingSpec {
	return mergePluginHTTPSpecs(slot, s.pluginSlotBaseSpecs(slot))
}

// pluginSlotBaseSpecs 取插件自己（或兜底）声明的参数表。
func (s *Service) pluginSlotBaseSpecs(slot pluginSlot) []plugin.SettingSpec {
	if slot.manager != nil {
		if status, _ := slot.manager.Status(); status == "running" {
			if specs := s.describePluginSpecs(slot.manager); len(specs) > 0 {
				return specs
			}
		}
	}
	if specs := s.storedPluginSpecs(slot.name); len(specs) > 0 {
		return specs
	}
	// 内置插件自己声明的参数（文件槽位的端口就是内置文件插件声明的）。
	if slot.builtin != nil {
		if specs := slot.builtin().Settings; len(specs) > 0 {
			return specs
		}
	}
	return fallbackSlotSpecs(slot.name)
}

// fallbackSlotSpecs 是 core 自带的兜底参数声明：插件进程没装、也没落过声明时，
// 页面至少还能配置这个槽位的关键参数。目前只有浏览器槽位——引擎路径、代理、
// 超时这些在插件装上之前就该能改。外部插件一律以插件自己的声明为准。
func fallbackSlotSpecs(slot string) []plugin.SettingSpec {
	switch slot {
	case PluginSlotBrowser:
		return []plugin.SettingSpec{
			{
				Key: "path", Label: "obscura 路径", Type: "string", Setting: "browser_obscura_path",
				Description: "obscura 可执行文件位置；留空自动探测（数据目录 bin/ 或 PATH）",
			},
			{
				Key: "stealth", Label: "隐匿模式", Type: "bool", Setting: "browser_obscura_stealth",
				Description: "以 stealth 模式启动引擎（一致的指纹与 TLS 伪装）",
			},
			{
				Key: "allow_private", Label: "允许访问内网", Type: "bool", Setting: "browser_obscura_allow_private",
				Description: "允许引擎访问回环与私有网段地址（默认关闭，防 SSRF）",
			},
			{
				Key: "proxy", Label: "代理", Type: "string", Setting: "browser_obscura_proxy",
				Description: "引擎使用的 HTTP/SOCKS 代理",
			},
			{
				Key: "user_agent", Label: "User-Agent", Type: "string", Setting: "browser_obscura_user_agent",
				Description: "引擎使用的 User-Agent；留空用引擎默认",
			},
			{
				Key: "timeout", Label: "单次调用超时（秒）", Type: "number", Setting: "browser_obscura_timeout",
				Default: "60", Description: "一次浏览器工具调用的时间预算",
			},
		}
	}
	return nil
}

// describePluginSpecs 问插件要它的参数声明；插件没实现 describe 时回落到
// initialize 握手里带来的 Info.Settings。
func (s *Service) describePluginSpecs(manager *plugin.Manager) []plugin.SettingSpec {
	var schema plugin.SettingsSchema
	if err := manager.Invoke(context.Background(), plugin.MethodSettingsDescribe, struct{}{}, &schema); err == nil && len(schema.Settings) > 0 {
		return schema.Settings
	}
	return manager.Info().Settings
}

// storedPluginSpecs 读上次落库的参数声明。
func (s *Service) storedPluginSpecs(slot string) []plugin.SettingSpec {
	raw := s.Settings()[plugin.PluginSchemaKey(slot)]
	if raw == "" {
		return nil
	}
	var specs []plugin.SettingSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		return nil
	}
	return specs
}

// storePluginSpecs 把参数声明落库（插件下次不在运行时页面仍有内容）。
func (s *Service) storePluginSpecs(slot string, specs []plugin.SettingSpec) {
	if len(specs) == 0 {
		return
	}
	encoded, err := json.Marshal(specs)
	if err != nil {
		return
	}
	if err := s.Store.SaveSettings(map[string]string{plugin.PluginSchemaKey(slot): string(encoded)}); err != nil {
		slog.Warn("plugin schema not stored", "slot", slot, "error", err)
	}
}

// SyncPluginSettings 在插件起来后同步它的参数声明与自报值。
//
// 插件报的参数表落库（页面下次打开就有内容）；插件自报的值也落库——
// 但只认它声明过「可写」的参数：不可写的参数归用户管，插件说了也不算。
func (s *Service) SyncPluginSettings(ctx context.Context, slot string, manager *plugin.Manager) {
	if manager == nil {
		return
	}
	specs := s.describePluginSpecs(manager)
	if len(specs) == 0 {
		return
	}
	s.storePluginSpecs(slot, specs)
	writable := map[string]plugin.SettingSpec{}
	for _, spec := range specs {
		if spec.Writable {
			writable[spec.Key] = spec
		}
	}
	if len(writable) == 0 {
		return
	}
	var values plugin.SettingsValues
	if err := manager.Invoke(ctx, plugin.MethodSettingsGet, struct{}{}, &values); err != nil {
		return
	}
	stored := map[string]string{}
	for key, value := range values.Values {
		spec, ok := writable[key]
		if !ok {
			continue
		}
		stored[spec.SettingKey(slot)] = value
	}
	if len(stored) == 0 {
		return
	}
	if err := s.Store.SaveSettings(stored); err != nil {
		slog.Warn("plugin values not stored", "slot", slot, "error", err)
	}
}

// UpdatePluginSettings 应用插件设置变更：落库 → 推给插件 → 重启受影响的插件。
func (s *Service) UpdatePluginSettings(ctx context.Context, update PluginSettingsUpdate) ([]PluginView, error) {
	slot, ok := s.findPluginSlot(update.Slot)
	if !ok {
		return nil, apperrors.New(apperrors.InvalidArgument, "unknown plugin slot: "+update.Slot)
	}
	specs := s.pluginSlotSpecs(slot)
	declared := map[string]plugin.SettingSpec{}
	for _, spec := range specs {
		declared[spec.Key] = spec
	}
	stored := map[string]string{}
	if update.Enabled != nil {
		if slot.required && !*update.Enabled {
			return nil, apperrors.New(apperrors.InvalidArgument, "内置能力不能停用："+slot.title)
		}
		stored[plugin.PluginEnabledKey(slot.name)] = boolSetting(*update.Enabled)
	}
	for key, value := range update.Values {
		spec, ok := declared[key]
		if !ok {
			return nil, apperrors.New(apperrors.InvalidArgument, "插件未声明该参数: "+key)
		}
		if err := validatePluginParamValue(spec, value); err != nil {
			return nil, err
		}
		stored[spec.SettingKey(slot.name)] = value
	}
	if len(stored) > 0 {
		if err := s.Store.SaveSettings(stored); err != nil {
			return nil, err
		}
	}
	started := time.Now()
	if update.Enabled != nil {
		s.applyPluginEnabled(ctx, slot)
	}
	if len(update.Values) > 0 {
		s.pushPluginValues(ctx, slot, update.Values)
	}
	// obscura 的路径/代理等是进程启动参数，改了必须重启插件才生效。
	s.refreshBrowserPlugin(ctx)
	s.refreshPluginManager(ctx)
	// 文件插件的端口等参数改了，文件面要换端口重开。
	if err := s.RestartFilePlugin(ctx); err != nil {
		slog.Warn("file plugin not restarted", "error", err)
	}
	// 启停之后同步一次声明与自报值。
	if slot.manager != nil {
		s.SyncPluginSettings(ctx, slot.name, slot.manager)
	}
	s.Audit.Record(ctx, "plugin.settings.update", "", policy.CapPlugin, audit.SafeParameters(map[string]string{
		"slot": slot.name,
	}), started, nil)
	return s.PluginViews(), nil
}

// boolSetting 把布尔值转成设置库里的字符串。
func boolSetting(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// applyPluginEnabled 按启用状态启停插件进程。
func (s *Service) applyPluginEnabled(ctx context.Context, slot pluginSlot) {
	if s.PluginEnabled(slot.name) {
		s.startSlotPlugin(ctx, slot)
		if slot.name == PluginSlotFile {
			// 文件面的 HTTP 服务跟着启停走。
			if err := s.StartFilePlugin(ctx); err != nil {
				slog.Warn("file plugin not started", "error", err)
			}
		}
		return
	}
	s.stopSlotPlugin(ctx, slot)
}

// startSlotPlugin 拉起某个槽位的插件进程。
func (s *Service) startSlotPlugin(ctx context.Context, slot pluginSlot) {
	switch slot.name {
	case PluginSlotDesktop:
		if s.Plugin != nil {
			go func() { _ = s.Plugin.Start(context.Background()) }()
		}
	case PluginSlotFile:
		if s.FilePlugin != nil {
			go func() { _ = s.FilePlugin.Start(context.Background()) }()
		}
	case PluginSlotBrowser:
		go s.StartBrowserPlugin(context.Background())
	case PluginSlotManager:
		go s.StartPluginManager(context.Background())
	}
}

// stopSlotPlugin 停掉某个槽位的插件。
func (s *Service) stopSlotPlugin(ctx context.Context, slot pluginSlot) {
	switch slot.name {
	case PluginSlotDesktop:
		if s.Plugin != nil {
			_ = s.Plugin.Close(ctx)
		}
	case PluginSlotFile:
		if s.FilePlugin != nil {
			_ = s.FilePlugin.Close(ctx)
		}
		if err := s.StopFilePlugin(ctx); err != nil {
			slog.Warn("file plugin not stopped", "error", err)
		}
	case PluginSlotBrowser:
		s.closeBrowserPlugin(ctx)
	case PluginSlotManager:
		s.closePluginManager(ctx)
	}
}

// pushPluginValues 把用户改的参数推给插件（插件不在线时忽略）。
func (s *Service) pushPluginValues(ctx context.Context, slot pluginSlot, values map[string]string) {
	if slot.manager == nil {
		return
	}
	if status, _ := slot.manager.Status(); status != "running" {
		return
	}
	apply := plugin.SettingsApply{Values: values}
	if err := slot.manager.Invoke(ctx, plugin.MethodSettingsApply, apply, nil); err != nil {
		if !errors.Is(err, plugin.ErrRuntimeMissing) {
			slog.Warn("plugin settings not applied", "slot", slot.name, "error", err)
		}
	}
}

// validatePluginSettings 校验设置里的插件参数：只放行插件声明过的参数，
// 以及 core 自己管的键（启停状态与参数声明本身）。
//
// 内置能力的启停键不接受「停用」——插件管理器是内置的，停用它等于把
// 装插件的路堵死。
func (s *Service) validatePluginSettings(values map[string]string) error {
	allowed := s.managedPluginSettingKeys()
	for key := range values {
		if !plugin.IsPluginSettingKey(key) {
			continue
		}
		if allowed[key] {
			continue
		}
		return apperrors.New(apperrors.InvalidArgument, "插件未声明该参数: "+key)
	}
	for _, slot := range s.pluginSlots() {
		if !slot.required {
			continue
		}
		key := plugin.PluginEnabledKey(slot.name)
		raw, ok := values[key]
		if !ok {
			continue
		}
		if !parseBool(raw) {
			return apperrors.New(apperrors.InvalidArgument, "内置能力不能停用："+slot.title)
		}
	}
	return nil
}

// managedPluginSettingKeys 列出插件命名空间里 core 允许直接写入的键。
func (s *Service) managedPluginSettingKeys() map[string]bool {
	allowed := map[string]bool{}
	for _, slot := range s.pluginSlots() {
		allowed[plugin.PluginEnabledKey(slot.name)] = true
		allowed[plugin.PluginSchemaKey(slot.name)] = true
		for _, spec := range s.pluginSlotSpecs(slot) {
			allowed[spec.SettingKey(slot.name)] = true
		}
	}
	return allowed
}

// applyPluginStates 按启用状态对齐各槽位的插件进程（启动时与设置变更后调用）。
func (s *Service) applyPluginStates(ctx context.Context) {
	for _, slot := range s.pluginSlots() {
		if !s.slotEnabled(slot) {
			s.stopSlotPlugin(ctx, slot)
			continue
		}
		s.startSlotPlugin(ctx, slot)
	}
}
