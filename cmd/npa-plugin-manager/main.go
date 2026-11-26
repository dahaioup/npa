// Command npa-plugin-manager 是 NPA 插件管理器插件的外部进程形态。
//
// 插件管理器本身是 NPA 的内置插件（internal/pluginmanager），随 core 编译、
// 始终可用、不可禁用。这个可执行文件是它的可选覆盖实现：装到
// plugins/manager 目录后，core 会优先用它接管仓库操作；不装就用内置实现。
// 两者共用同一份逻辑（internal/pluginmanager.Builtin）。
//
// 它通过 NPA 插件协议（JSON-RPC 2.0 + Content-Length framing + stdio）与 core
// 通信，负责把公有插件仓库里的插件及其依赖下载到本机：
//
//	npa core ──NPA 插件协议──▶ npa-plugin-manager ──HTTP──▶ 插件仓库（registry.json + Release 资产）
//
// 仓库结构：git 里只放索引 registry.json（描述插件/依赖的版本、平台产物、
// sha256、依赖关系），二进制放在 Release 资产里，既不让仓库膨胀，又能让
// 第三方只下载自己需要的平台产物。
//
// 用法：
//
//	npa-plugin-manager --data-dir /path/to/.npa [--registry URL]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/npa/npa/internal/plugin"
	"github.com/npa/npa/internal/pluginmanager"
	"github.com/npa/npa/internal/pluginregistry"
)

const (
	pluginID      = "npa-plugin-manager"
	pluginName    = "NPA Plugin Manager"
	pluginVersion = "1.0.0"
)

func main() {
	var (
		dataDir          = flag.String("data-dir", os.Getenv("NPA_DATA_DIR"), "NPA data directory")
		registryURL      = flag.String("registry", os.Getenv("NPA_PLUGIN_REGISTRY"), "plugin registry URL")
		callTimeout      = flag.Duration("call-timeout", 10*time.Minute, "per-call timeout")
		shutdownWait     = flag.Duration("shutdown-wait", 5*time.Second, "grace period on shutdown")
		printVersionOnly = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *printVersionOnly {
		fmt.Println(pluginID, pluginVersion)
		return
	}

	if strings.TrimSpace(*dataDir) == "" {
		slog.Error("npa-plugin-manager requires --data-dir")
		os.Exit(2)
	}

	manager := &manager{
		registry:    *registryURL,
		callTimeout: *callTimeout,
	}
	// 内置实现与进程共享同一份仓库地址：core 改地址后这里跟着变。
	manager.builtin = pluginmanager.NewBuiltin(*dataDir, manager.registryURL)
	manager.builtin.CallTimeout = *callTimeout

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := plugin.NewServer(os.Stdin, os.Stdout, manager.handle)
	manager.server = server
	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		<-signals
		cancel()
	}()

	if err := server.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("plugin server stopped", "error", err)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), *shutdownWait)
	defer shutdownCancel()
	manager.close(shutdownCtx)
}

// manager 是插件管理器进程的状态：逻辑复用内置实现，进程只负责协议适配。
type manager struct {
	builtin     *pluginmanager.Builtin
	registry    string
	callTimeout time.Duration

	server *plugin.Server
}

func (m *manager) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "plugin.initialize":
		return plugin.Info{
			ID:           pluginID,
			Name:         pluginName,
			Version:      pluginVersion,
			Protocol:     "1",
			Capabilities: []string{"plugin"},
			Permission:   "core.plugin",
			Settings:     m.builtin.Settings(),
		}, nil
	case "plugin.health":
		return map[string]any{"manager": "npa-plugin-manager", "registry": m.registryURL()}, nil
	case "plugin.shutdown":
		m.close(ctx)
		if m.server != nil {
			m.server.Stop()
		}
		return map[string]any{"stopped": true}, nil
	case plugin.MethodSettingsDescribe:
		return plugin.SettingsSchema{Settings: m.builtin.Settings()}, nil
	case plugin.MethodSettingsGet:
		return plugin.SettingsValues{Values: m.settingValues()}, nil
	case plugin.MethodSettingsApply:
		return m.applySettings(params)
	case "manager.list":
		return m.builtin.List(ctx)
	case "manager.install":
		return m.install(ctx, params)
	case "manager.remove":
		return m.remove(ctx, params)
	case "manager.update":
		return m.install(ctx, params)
	case "manager.status":
		return m.builtin.Status(ctx)
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

// settingValues 返回插件当前生效的参数值。
func (m *manager) settingValues() map[string]string {
	return map[string]string{"registry_url": m.registryURL()}
}

// applySettings 应用 core 下发的参数变更：换仓库地址就丢掉缓存的索引，
// 下次调用重新拉。
func (m *manager) applySettings(params json.RawMessage) (map[string]any, error) {
	var apply plugin.SettingsApply
	if len(params) > 0 {
		if err := json.Unmarshal(params, &apply); err != nil {
			return nil, fmt.Errorf("invalid plugin.settings.apply params: %w", err)
		}
	}
	for key, value := range apply.Values {
		if key == "registry_url" {
			m.registry = value
		}
	}
	m.builtin.Invalidate()
	return map[string]any{"applied": len(apply.Values)}, nil
}

// registryURL 返回仓库地址，没有配置时用默认的公有仓库。
func (m *manager) registryURL() string {
	if strings.TrimSpace(m.registry) == "" {
		return pluginregistry.DefaultRegistryURL
	}
	return m.registry
}

// install 解析参数后交给内置实现执行安装。
func (m *manager) install(ctx context.Context, params json.RawMessage) (map[string]any, error) {
	var request struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, fmt.Errorf("invalid manager.install params: %w", err)
		}
	}
	return m.builtin.Install(ctx, request.ID, request.Version)
}

// remove 解析参数后交给内置实现执行卸载。
func (m *manager) remove(ctx context.Context, params json.RawMessage) (map[string]any, error) {
	var request struct {
		ID string `json:"id"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, fmt.Errorf("invalid manager.remove params: %w", err)
		}
	}
	return m.builtin.Remove(ctx, request.ID)
}

// close 释放资源（当前没有常驻子进程，留着给后续扩展）。
func (m *manager) close(_ context.Context) {}
