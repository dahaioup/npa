package file

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/plugin"
	sshx "github.com/npa/npa/internal/ssh"
)

// Service 是文件能力的门面：按插件优先级把每次调用路由到具体实现。
//
// 优先级（与 internal/desktop.Router 一致，见 SAD §8.7 Provider Router）：
//
//	外部文件插件（plugins/file 下的插件进程，running 时接管）
//	  → 内置插件（Builtin：节点 HTTP 文件面 → 跨域代理 → SFTP 三级选路）
//
// 插件缺失 / 进程不在运行 → 走内置实现；插件在跑但调用失败 → 错误上抛，
// 不静默降级；只有插件进程在调用途中消失（plugin.ErrRuntimeMissing）
// 或插件通道载荷超限（ErrPayloadTooLarge）才回退内置实现，保证文件操作
// 不会因为插件崩溃或大文件超限而整条链路断掉。
//
// 对外接口不变：MCP 的 file.* 工具与 Web 的 /api/nodes/{id}/files/*
// 仍然调用本类型的方法，底层换成插件路由。
type Service struct {
	// Builtin 是内置插件实现，也是外部插件不可用时的兜底。
	Builtin *Builtin
	plugin  *PluginProvider
}

// NewService 创建文件服务：内置实现立即可用，外部插件稍后用 AttachPlugin 挂上。
func NewService(sshManager *sshx.Manager) *Service {
	return &Service{Builtin: NewBuiltin(sshManager)}
}

// AttachPlugin 挂上外部文件插件；传 nil 表示只用内置实现。
func (s *Service) AttachPlugin(provider *PluginProvider) {
	s.plugin = provider
}

// PluginInfo 返回当前生效的文件能力插件元数据（内置插件或外部插件）。
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

// dispatch 执行一次文件操作：外部插件优先，插件进程消失时回退内置实现。
//
// 两种情况回退内置实现：
//
//  1. 插件进程在调用途中消失（plugin.ErrRuntimeMissing）——文件操作不该因为
//     插件崩溃而整条链路断掉；
//  2. 插件通道载荷超限（ErrPayloadTooLarge）——插件通道是 base64 JSON-RPC，
//     有 4 MiB 上限，而内置实现走流式 HTTP/SFTP 不受限；不回退的话，装了外部
//     插件就会把升级包推送这类大文件操作打断。
//
// 其余插件错误一律上抛，不静默降级。
func dispatch[T any](s *Service, fn func(Provider) (T, error)) (T, error) {
	provider := s.provider()
	external := s.plugin != nil && provider == Provider(s.plugin)
	result, err := fn(provider)
	if err != nil && external && (errors.Is(err, plugin.ErrRuntimeMissing) || errors.Is(err, ErrPayloadTooLarge)) {
		return fn(s.Builtin)
	}
	return result, err
}

// dispatchErr 是 dispatch 的无返回值版本。
func dispatchErr(s *Service, fn func(Provider) error) error {
	_, err := dispatch(s, func(provider Provider) (struct{}, error) {
		return struct{}{}, fn(provider)
	})
	return err
}

func (s *Service) List(ctx context.Context, nodeID, remotePath string) ([]model.FileEntry, error) {
	return dispatch(s, func(provider Provider) ([]model.FileEntry, error) {
		return provider.List(ctx, nodeID, remotePath)
	})
}

func (s *Service) Stat(ctx context.Context, nodeID, remotePath string) (model.FileEntry, error) {
	return dispatch(s, func(provider Provider) (model.FileEntry, error) {
		return provider.Stat(ctx, nodeID, remotePath)
	})
}

func (s *Service) Read(ctx context.Context, nodeID, remotePath string) ([]byte, error) {
	return dispatch(s, func(provider Provider) ([]byte, error) {
		return provider.Read(ctx, nodeID, remotePath)
	})
}

func (s *Service) OpenRead(ctx context.Context, nodeID, remotePath string) (io.ReadCloser, error) {
	return dispatch(s, func(provider Provider) (io.ReadCloser, error) {
		return provider.OpenRead(ctx, nodeID, remotePath)
	})
}

func (s *Service) Write(ctx context.Context, nodeID, remotePath string, data []byte) error {
	return dispatchErr(s, func(provider Provider) error {
		return provider.Write(ctx, nodeID, remotePath, data)
	})
}

func (s *Service) Upload(ctx context.Context, nodeID, remotePath string, source io.Reader) (int64, error) {
	return dispatch(s, func(provider Provider) (int64, error) {
		return provider.Upload(ctx, nodeID, remotePath, source)
	})
}

func (s *Service) Remove(ctx context.Context, nodeID, remotePath string) error {
	return dispatchErr(s, func(provider Provider) error {
		return provider.Remove(ctx, nodeID, remotePath)
	})
}

func (s *Service) Mkdir(ctx context.Context, nodeID, remotePath string) error {
	return dispatchErr(s, func(provider Provider) error {
		return provider.Mkdir(ctx, nodeID, remotePath)
	})
}

func (s *Service) Rename(ctx context.Context, nodeID, oldPath, newPath string) error {
	return dispatchErr(s, func(provider Provider) error {
		return provider.Rename(ctx, nodeID, oldPath, newPath)
	})
}

func (s *Service) Touch(ctx context.Context, nodeID, remotePath string) error {
	return dispatchErr(s, func(provider Provider) error {
		return provider.Touch(ctx, nodeID, remotePath)
	})
}

func (s *Service) TouchTime(ctx context.Context, nodeID, remotePath string, modTime time.Time) error {
	return dispatchErr(s, func(provider Provider) error {
		return provider.TouchTime(ctx, nodeID, remotePath, modTime)
	})
}

func (s *Service) CreateReader(ctx context.Context, nodeID, remotePath string) (io.Reader, error) {
	return dispatch(s, func(provider Provider) (io.Reader, error) {
		return provider.CreateReader(ctx, nodeID, remotePath)
	})
}
