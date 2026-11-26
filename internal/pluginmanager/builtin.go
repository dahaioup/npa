// Package pluginmanager 是插件管理器能力的实现体。
//
// 插件管理器是 NPA 的内置插件：实现体随 core 编译、始终可用、不可禁用。
// 它把公有插件仓库（registry.json 索引 + Release 资产）里的插件及其依赖
// 下载到本机：
//
//	npa core（内置）──HTTP──▶ 插件仓库（registry.json + Release 资产）
//
// 外部插件进程（plugins/manager 下的 npa-plugin-manager）是可选的覆盖实现：
// 它在跑时接管调用，不在时由内置实现兜底（见 Service 的分派规则）。
// 两条实现共用本文件里的逻辑，行为一致。
package pluginmanager

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/npa/npa/internal/plugin"
	"github.com/npa/npa/internal/pluginregistry"
)

// BuiltinID 是内置插件管理器的插件 ID（外部覆盖实现用 npa-plugin-manager）。
const BuiltinID = "npa.plugin-manager"

// builtinMeta 是插件管理器作为内置插件的自描述信息。
//
// 参数声明沿用已有的扁平设置键（plugin_registry_url），老库里的值继续有效；
// Writable 为 false——仓库地址由用户改，插件自己不回写。
var builtinMeta = plugin.Builtin{
	ID:           BuiltinID,
	Name:         "插件管理器",
	Version:      "1",
	Protocol:     "core/1",
	Capabilities: []string{"plugin"},
	Permission:   "core.plugin",
	Settings: []plugin.SettingSpec{
		{
			Key: "registry_url", Label: "插件仓库地址", Type: "string", Setting: "plugin_registry_url",
			Description: "拉取插件清单与 Release 资产的仓库地址；留空用默认公有仓库",
		},
	},
}

// Builtin 是插件管理器的内置实现体。
type Builtin struct {
	// DataDir 是 NPA 数据目录：插件装在 <DataDir>/plugins/<槽位>/，
	// 依赖（引擎之类）装在 <DataDir>/bin/。
	DataDir string
	// RegistryURL 返回当前生效的仓库地址；core 每次调用前给最新值。
	RegistryURL func() string
	// CallTimeout 是一次仓库操作（下载可能很大）的预算。
	CallTimeout time.Duration

	mu     sync.Mutex
	cached *pluginregistry.Registry
}

// NewBuiltin 创建内置插件管理器。registryURL 传 nil 时用默认公有仓库。
func NewBuiltin(dataDir string, registryURL func() string) *Builtin {
	return &Builtin{
		DataDir:     dataDir,
		RegistryURL: registryURL,
		CallTimeout: 10 * time.Minute,
	}
}

// Info 返回内置插件的元数据（与外部插件同形）。
func (b *Builtin) Info() plugin.Info { return builtinMeta.Info() }

// Settings 返回该内置插件声明的可配置参数。
func (b *Builtin) Settings() []plugin.SettingSpec { return builtinMeta.Settings }

// registryURL 返回仓库地址，没有配置时用默认的公有仓库。
func (b *Builtin) registryURL() string {
	if b.RegistryURL != nil {
		if url := strings.TrimSpace(b.RegistryURL()); url != "" {
			return url
		}
	}
	return pluginregistry.DefaultRegistryURL
}

func (b *Builtin) client() *pluginregistry.Client {
	return pluginregistry.NewClient(b.registryURL())
}

func (b *Builtin) installer() *pluginregistry.Installer {
	return &pluginregistry.Installer{
		DataDir: b.DataDir,
		Client:  b.client(),
	}
}

// callTimeout 返回本次调用的预算。
func (b *Builtin) callTimeout() time.Duration {
	if b.CallTimeout > 0 {
		return b.CallTimeout
	}
	return 10 * time.Minute
}

// loadRegistry 取回并缓存仓库索引。
func (b *Builtin) loadRegistry(ctx context.Context) (*pluginregistry.Registry, error) {
	b.mu.Lock()
	cached := b.cached
	b.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	fetched, err := b.client().Fetch(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.cached = fetched
	b.mu.Unlock()
	return fetched, nil
}

// Invalidate 丢掉缓存的索引：仓库地址改了或装完插件后，下次调用重新拉。
func (b *Builtin) Invalidate() {
	b.mu.Lock()
	b.cached = nil
	b.mu.Unlock()
}

// List 返回仓库里的全部条目以及本机的安装状态。
func (b *Builtin) List(ctx context.Context) (map[string]any, error) {
	registry, err := b.loadRegistry(ctx)
	if err != nil {
		return nil, err
	}
	installer := b.installer()
	installed, err := installer.State()
	if err != nil {
		return nil, err
	}
	installedByID := map[string]pluginregistry.InstalledEntry{}
	for _, entry := range installed {
		installedByID[entry.ID] = entry
	}
	entries := make([]map[string]any, 0, len(registry.Plugins))
	for _, entry := range registry.Plugins {
		artifacts := make([]map[string]any, 0, len(entry.Artifacts))
		for _, artifact := range entry.Artifacts {
			artifacts = append(artifacts, map[string]any{
				"os":     artifact.OS,
				"arch":   artifact.Arch,
				"size":   artifact.Size,
				"sha256": artifact.SHA256,
				"format": artifact.FormatOrDefault(),
			})
		}
		_, available := entry.ArtifactFor(runtime.GOOS, runtime.GOARCH)
		state, isInstalled := installedByID[entry.ID]
		item := map[string]any{
			"id":           entry.ID,
			"name":         entry.Name,
			"version":      entry.Version,
			"description":  entry.Description,
			"kind":         entry.Kind,
			"capabilities": entry.Capabilities,
			"requires":     entry.Requires,
			"install":      map[string]any{"dir": entry.Install.Dir, "executable": entry.Install.Executable},
			"artifacts":    artifacts,
			"available":    available,
			"installed":    isInstalled,
		}
		if isInstalled {
			item["installed_version"] = state.Version
			item["installed_at"] = state.InstalledAt
			item["files"] = state.Files
		}
		entries = append(entries, item)
	}
	return map[string]any{
		"registry_url": b.registryURL(),
		"updated_at":   registry.UpdatedAt,
		"os":           runtime.GOOS,
		"arch":         runtime.GOARCH,
		"plugins":      entries,
	}, nil
}

// Install 下载并安装一个插件及其依赖。
func (b *Builtin) Install(ctx context.Context, id, version string) (map[string]any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("plugin id is required")
	}
	callCtx, cancel := context.WithTimeout(ctx, b.callTimeout())
	defer cancel()
	registry, err := b.loadRegistry(callCtx)
	if err != nil {
		return nil, err
	}
	entry, ok := registry.Find(id)
	if !ok {
		return nil, fmt.Errorf("plugin %s is not published in the registry", id)
	}
	if version != "" && version != entry.Version {
		return nil, fmt.Errorf("plugin %s version %s is not published (registry has %s)", id, version, entry.Version)
	}
	installer := b.installer()
	results, err := installer.Install(callCtx, registry, id)
	if err != nil {
		return map[string]any{"installed": results, "error": err.Error()}, err
	}
	// 索引可能已经变了，下次调用重新拉。
	b.Invalidate()
	return map[string]any{
		"installed": results,
		"slots":     SlotsOf(results),
	}, nil
}

// Update 把已装插件更新到仓库里的最新版本。
func (b *Builtin) Update(ctx context.Context, id string) (map[string]any, error) {
	return b.Install(ctx, id, "")
}

// Remove 卸载一个插件及其记录的文件。
func (b *Builtin) Remove(ctx context.Context, id string) (map[string]any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("plugin id is required")
	}
	installer := b.installer()
	if err := installer.Remove(id); err != nil {
		return nil, err
	}
	return map[string]any{"removed": id}, nil
}

// Status 返回仓库地址与本机安装状态。
func (b *Builtin) Status(ctx context.Context) (map[string]any, error) {
	installer := b.installer()
	installed, err := installer.State()
	if err != nil {
		return nil, err
	}
	if installed == nil {
		installed = []pluginregistry.InstalledEntry{}
	}
	return map[string]any{
		"registry_url": b.registryURL(),
		"data_dir":     b.DataDir,
		"os":           runtime.GOOS,
		"arch":         runtime.GOARCH,
		"installed":    installed,
	}, nil
}

// SlotsOf 从安装结果里推出受影响的插件槽位（plugins/<槽位>/…）。
func SlotsOf(results []pluginregistry.InstallResult) []string {
	seen := map[string]bool{}
	var slots []string
	for _, result := range results {
		slot := SlotOf(result.Dir)
		if slot == "" || seen[slot] {
			continue
		}
		seen[slot] = true
		slots = append(slots, slot)
	}
	return slots
}

// SlotOf 从安装目录里推出槽位名。
func SlotOf(dir string) string {
	parts := strings.Split(strings.Trim(strings.ReplaceAll(dir, "\\", "/"), "/"), "/")
	if len(parts) >= 2 && parts[0] == "plugins" {
		return parts[1]
	}
	return ""
}
