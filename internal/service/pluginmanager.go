package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/npa/npa/internal/audit"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/plugin"
	"github.com/npa/npa/internal/pluginmanager"
	"github.com/npa/npa/internal/pluginregistry"
	"github.com/npa/npa/internal/policy"
)

// pluginManagerTimeout 是一次插件仓库操作（下载可能很大）的 core 侧预算。
const pluginManagerTimeout = 15 * time.Minute

// PluginRegistryURL 返回插件仓库地址：设置项优先，否则用默认公有仓库。
func (s *Service) PluginRegistryURL() string {
	if configured := strings.TrimSpace(s.Settings()["plugin_registry_url"]); configured != "" {
		return configured
	}
	return pluginregistry.DefaultRegistryURL
}

// PluginManagerArgs 返回插件管理器进程的启动参数。
func (s *Service) PluginManagerArgs() []string {
	return []string{
		"--data-dir", s.Store.DataDir(),
		"--registry", s.PluginRegistryURL(),
	}
}

// StartPluginManager 启动插件管理器插件进程。
//
// 插件管理器是内置能力：这个进程是可选的覆盖实现——不在 plugins/manager
// 目录里也没关系，内置实现照常工作。
func (s *Service) StartPluginManager(ctx context.Context) {
	if s.PluginManager == nil {
		return
	}
	if err := s.PluginManager.Start(ctx); err != nil {
		if errors.Is(err, plugin.ErrRuntimeMissing) {
			// 正常的形态：外部覆盖没装，用内置实现。
			slog.Info("plugin manager override not installed, using the built-in implementation")
			return
		}
		slog.Warn("plugin manager override not started", "error", err)
		return
	}
	s.pluginManagerMu.Lock()
	s.pluginManagerArgs = strings.Join(s.PluginManager.Args, " ")
	s.pluginManagerMu.Unlock()
	slog.Info("plugin manager override started", "id", s.PluginManager.Info().ID)
}

// closePluginManager 关掉插件管理器进程（带超时，免得把 Web 请求挂住）。
func (s *Service) closePluginManager(ctx context.Context) {
	if s.PluginManager == nil {
		return
	}
	closeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = s.PluginManager.Close(closeCtx)
}

// refreshPluginManager 参数变了（仓库地址改了）就重启插件管理器进程。
func (s *Service) refreshPluginManager(ctx context.Context) {
	if s.PluginManager == nil {
		return
	}
	s.pluginManagerMu.Lock()
	current := s.pluginManagerArgs
	s.pluginManagerMu.Unlock()
	args := s.PluginManagerArgs()
	if strings.Join(args, " ") == current {
		return
	}
	status, _ := s.PluginManager.Status()
	if status == "running" {
		s.closePluginManager(ctx)
	}
	s.PluginManager.Args = args
	go s.StartPluginManager(context.Background())
	// 内置实现也要换地址：它每次调用都现取 core 的设置。
	if s.PluginManagerService != nil {
		s.PluginManagerService.Builtin.Invalidate()
	}
}

// PluginManagerStatus 返回插件管理器自身的状态（仓库地址、已装插件）。
func (s *Service) PluginManagerStatus(ctx context.Context) (map[string]any, error) {
	result, err := s.PluginManagerService.Status(ctx)
	if err != nil {
		return nil, err
	}
	// 生效的是内置实现还是外部覆盖进程，页面上要看得出来。
	if s.PluginManager != nil {
		status, lastError := s.PluginManager.Status()
		if status == "running" {
			result["plugin_status"] = status
			result["plugin_id"] = s.PluginManager.Info().ID
		} else {
			result["plugin_status"] = "builtin"
			result["plugin_id"] = pluginmanager.BuiltinID
		}
		if lastError != "" {
			result["plugin_error"] = lastError
		}
	} else {
		result["plugin_status"] = "builtin"
		result["plugin_id"] = pluginmanager.BuiltinID
	}
	return result, nil
}

// PluginList 返回仓库里的插件清单与本机安装状态。
func (s *Service) PluginList(ctx context.Context) (map[string]any, error) {
	return s.PluginManagerService.List(ctx)
}

// PluginInstall 从仓库下载并安装一个插件及其依赖。
func (s *Service) PluginInstall(ctx context.Context, id string, version string) (map[string]any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, apperrors.New(apperrors.InvalidArgument, "plugin id is required")
	}
	started := time.Now()
	result, err := s.PluginManagerService.Install(ctx, id, version)
	s.Audit.Record(ctx, "plugin.install", "", policy.CapPlugin, audit.SafeParameters(map[string]string{
		"id": id, "version": version,
	}), started, err)
	if err != nil {
		return nil, err
	}
	slog.Info("plugin installed", "id", id)
	s.restartPluginSlots(ctx, slotsOfResult(result))
	return result, nil
}

// PluginUpdate 把已装插件更新到仓库里的最新版本。
func (s *Service) PluginUpdate(ctx context.Context, id string) (map[string]any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, apperrors.New(apperrors.InvalidArgument, "plugin id is required")
	}
	started := time.Now()
	result, err := s.PluginManagerService.Update(ctx, id)
	s.Audit.Record(ctx, "plugin.update", "", policy.CapPlugin, audit.SafeParameters(map[string]string{
		"id": id,
	}), started, err)
	if err != nil {
		return nil, err
	}
	slog.Info("plugin updated", "id", id)
	s.restartPluginSlots(ctx, slotsOfResult(result))
	return result, nil
}

// PluginRemove 卸载一个插件。
//
// 插件管理器是内置能力，它自己不可卸载——否则 core 就失去了装插件的能力。
func (s *Service) PluginRemove(ctx context.Context, id string) (map[string]any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, apperrors.New(apperrors.InvalidArgument, "plugin id is required")
	}
	if isBuiltinPluginManager(id) {
		return nil, apperrors.New(apperrors.InvalidArgument, "插件管理器是内置能力，不能卸载")
	}
	started := time.Now()
	result, err := s.PluginManagerService.Remove(ctx, id)
	s.Audit.Record(ctx, "plugin.remove", "", policy.CapPlugin, audit.SafeParameters(map[string]string{
		"id": id,
	}), started, err)
	if err != nil {
		return nil, err
	}
	slog.Info("plugin removed", "id", id)
	return result, nil
}

// isBuiltinPluginManager 判断一个插件 ID 指的是不是内置的插件管理器。
//
// 只有内置实现（点号 ID）不可卸载。仓库里的 "npa-plugin-manager" 是外部
// 覆盖进程，卸掉它不会失去装插件的能力——内置实现立刻顶上。
func isBuiltinPluginManager(id string) bool {
	return strings.TrimSpace(id) == pluginmanager.BuiltinID
}

// slotsOfResult 从安装结果里取出受影响的插件槽位。
func slotsOfResult(result map[string]any) []string {
	raw, ok := result["slots"].([]any)
	if !ok {
		return nil
	}
	slots := make([]string, 0, len(raw))
	for _, item := range raw {
		if value, ok := item.(string); ok && value != "" {
			slots = append(slots, value)
		}
	}
	return slots
}

// restartPluginSlots 重启被安装/卸载影响的插件槽位。
func (s *Service) restartPluginSlots(ctx context.Context, slots []string) {
	for _, slot := range slots {
		switch slot {
		case "browser":
			if s.BrowserPlugin != nil {
				s.closeBrowserPlugin(ctx)
				s.BrowserPlugin.Args = s.BrowserPluginArgs()
				go s.StartBrowserPlugin(context.Background())
			}
		case "desktop":
			if s.Plugin != nil {
				_ = s.Plugin.Close(ctx)
				go func() { _ = s.Plugin.Start(context.Background()) }()
			}
		case "file":
			if s.FilePlugin != nil {
				_ = s.FilePlugin.Close(ctx)
				go func() { _ = s.FilePlugin.Start(context.Background()) }()
			}
		case "manager":
			// 管理器进程正在处理本次调用，等它回完再重启。
			go func() {
				time.Sleep(time.Second)
				s.refreshPluginManager(context.Background())
			}()
		}
	}
}

// pluginManagerDirs 列出插件管理器进程的搜索目录。
func pluginManagerDirs(dataDir string) []string {
	dirs := []string{filepath.Join(dataDir, "plugins", "manager")}
	if executable, err := os.Executable(); err == nil {
		binDir := filepath.Dir(executable)
		dirs = append(dirs,
			filepath.Join(binDir, "plugins", "manager"),
			filepath.Join(filepath.Dir(binDir), "plugins", "manager"))
	}
	return dirs
}

// DescribePluginSlots 返回各槽位当前的插件状态，给 Web 面板用。
func (s *Service) DescribePluginSlots() []map[string]any {
	slots := []struct {
		name    string
		manager *plugin.Manager
		builtin func() plugin.Info
	}{
		{"desktop", s.Plugin, nil},
		{"file", s.FilePlugin, nil},
		{"browser", s.BrowserPlugin, nil},
		{"manager", s.PluginManager, s.pluginManagerBuiltinInfo},
	}
	described := make([]map[string]any, 0, len(slots))
	for _, slot := range slots {
		if slot.manager == nil {
			continue
		}
		status, lastError := slot.manager.Status()
		info := slot.manager.Info()
		builtin := false
		if info.ID == "" && slot.builtin != nil {
			if fallback := slot.builtin(); fallback.ID != "" {
				info = fallback
				builtin = true
				status = "builtin"
				lastError = ""
			}
		}
		described = append(described, map[string]any{
			"slot":    slot.name,
			"status":  status,
			"error":   lastError,
			"id":      info.ID,
			"name":    info.Name,
			"version": info.Version,
			"builtin": builtin,
		})
	}
	return described
}

// pluginManagerBuiltinInfo 返回内置插件管理器的元信息。
func (s *Service) pluginManagerBuiltinInfo() plugin.Info {
	if s.PluginManagerService == nil || s.PluginManagerService.Builtin == nil {
		return plugin.Info{}
	}
	return s.PluginManagerService.Builtin.Info()
}
