package pluginmanager

import (
	"context"

	"errors"

	"github.com/npa/npa/internal/plugin"
)

// Provider 是插件管理器能力的插件契约。
//
// 两种实现：
//   - Builtin：内置插件（纯 Go，随 core 编译进来），始终可用、不可禁用；
//   - PluginProvider：外部插件进程（plugins/manager 下的 npa-plugin-manager），
//     通过插件协议暴露同一套能力。
//
// 契约语义与底层链路无关：调用方（MCP plugin.* 工具、Web /api/plugins/*）
// 只依赖本接口，换实现不改变任何对外接口。
type Provider interface {
	// List 列出仓库里的插件与本机安装状态。
	List(ctx context.Context) (map[string]any, error)
	// Install 下载并安装一个插件及其依赖。
	Install(ctx context.Context, id, version string) (map[string]any, error)
	// Update 把已装插件更新到仓库里的最新版本。
	Update(ctx context.Context, id string) (map[string]any, error)
	// Remove 卸载一个插件。
	Remove(ctx context.Context, id string) (map[string]any, error)
	// Status 返回仓库地址与本机安装状态。
	Status(ctx context.Context) (map[string]any, error)
	// Info 返回插件元数据。
	Info() plugin.Info
}

var (
	_ Provider = (*Builtin)(nil)
	_ Provider = (*PluginProvider)(nil)
)

// Invoker 是外部插件进程的调用面（internal/plugin.Manager 满足它）。
type Invoker interface {
	Invoke(ctx context.Context, method string, params any, result any) error
	Status() (string, string)
	Info() plugin.Info
}

// PluginProvider 是外部插件进程实现。
type PluginProvider struct {
	invoker Invoker
}

// NewPluginProvider 用一个插件进程调用面创建外部实现。
func NewPluginProvider(invoker Invoker) *PluginProvider {
	return &PluginProvider{invoker: invoker}
}

// Available 报告外部插件进程是否在跑。
func (p *PluginProvider) Available() bool {
	if p == nil || p.invoker == nil {
		return false
	}
	status, _ := p.invoker.Status()
	return status == "running"
}

// Info 返回外部插件的元数据。
func (p *PluginProvider) Info() plugin.Info {
	if p == nil || p.invoker == nil {
		return plugin.Info{}
	}
	return p.invoker.Info()
}

func (p *PluginProvider) invoke(ctx context.Context, method string, params any, out any) error {
	return p.invoker.Invoke(ctx, method, params, out)
}

// List 列出仓库里的插件与本机安装状态。
func (p *PluginProvider) List(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := p.invoke(ctx, "manager.list", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Install 下载并安装一个插件及其依赖。
func (p *PluginProvider) Install(ctx context.Context, id, version string) (map[string]any, error) {
	var out map[string]any
	err := p.invoke(ctx, "manager.install", map[string]string{"id": id, "version": version}, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Update 把已装插件更新到仓库里的最新版本。
func (p *PluginProvider) Update(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	if err := p.invoke(ctx, "manager.update", map[string]string{"id": id}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Remove 卸载一个插件。
func (p *PluginProvider) Remove(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	if err := p.invoke(ctx, "manager.remove", map[string]string{"id": id}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Status 返回仓库地址与本机安装状态。
func (p *PluginProvider) Status(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := p.invoke(ctx, "manager.status", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Service 是插件管理器能力的门面：按插件优先级把每次调用路由到具体实现。
//
// 优先级（与 internal/file.Service 一致，见 SAD §8.7 Provider Router）：
//
//	外部插件进程（plugins/manager 下的 npa-plugin-manager，running 时接管）
//	  → 内置插件（Builtin：始终可用）
//
// 插件进程在调用途中消失（plugin.ErrRuntimeMissing）时回退内置实现——仓库
// 操作不该因为外部进程崩溃而整条链路断掉；其余错误一律上抛，不静默降级。
type Service struct {
	// Builtin 是内置插件实现，也是外部插件不可用时的兜底。
	Builtin *Builtin
	plugin  *PluginProvider
}

// NewService 创建插件管理器服务：内置实现立即可用，
// 外部插件进程稍后用 AttachPlugin 挂上。
func NewService(dataDir string, registryURL func() string) *Service {
	return &Service{Builtin: NewBuiltin(dataDir, registryURL)}
}

// AttachPlugin 挂上外部插件进程；传 nil 表示只用内置实现。
func (s *Service) AttachPlugin(provider *PluginProvider) {
	s.plugin = provider
}

// PluginInfo 返回当前生效的插件管理器元数据（内置实现或外部插件）。
func (s *Service) PluginInfo() plugin.Info {
	if s.plugin != nil && s.plugin.Available() {
		return s.plugin.Info()
	}
	return s.Builtin.Info()
}

// provider 选出这次调用该走哪个实现。
func (s *Service) provider() Provider {
	if s.plugin != nil && s.plugin.Available() {
		return s.plugin
	}
	return s.Builtin
}

// dispatch 执行一次仓库操作：外部插件优先，插件进程消失时回退内置实现。
func dispatch[T any](s *Service, fn func(Provider) (T, error)) (T, error) {
	provider := s.provider()
	external := s.plugin != nil && provider == Provider(s.plugin)
	result, err := fn(provider)
	if err != nil && external && errors.Is(err, plugin.ErrRuntimeMissing) {
		return fn(s.Builtin)
	}
	return result, err
}

// List 列出仓库里的插件与本机安装状态。
func (s *Service) List(ctx context.Context) (map[string]any, error) {
	return dispatch(s, func(provider Provider) (map[string]any, error) {
		return provider.List(ctx)
	})
}

// Install 下载并安装一个插件及其依赖。
func (s *Service) Install(ctx context.Context, id, version string) (map[string]any, error) {
	return dispatch(s, func(provider Provider) (map[string]any, error) {
		return provider.Install(ctx, id, version)
	})
}

// Update 把已装插件更新到仓库里的最新版本。
func (s *Service) Update(ctx context.Context, id string) (map[string]any, error) {
	return dispatch(s, func(provider Provider) (map[string]any, error) {
		return provider.Update(ctx, id)
	})
}

// Remove 卸载一个插件。
func (s *Service) Remove(ctx context.Context, id string) (map[string]any, error) {
	return dispatch(s, func(provider Provider) (map[string]any, error) {
		return provider.Remove(ctx, id)
	})
}

// Status 返回仓库地址与本机安装状态。
func (s *Service) Status(ctx context.Context) (map[string]any, error) {
	return dispatch(s, func(provider Provider) (map[string]any, error) {
		return provider.Status(ctx)
	})
}
