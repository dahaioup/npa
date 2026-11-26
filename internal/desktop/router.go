package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/npa/npa/internal/plugin"
)

// ErrPluginUnavailable 表示桌面插件没有运行：本体的桌面能力全部由插件提供，
// 插件没起来就没有截图/键鼠/剪贴板能力（不再回落到内嵌脚本）。
var ErrPluginUnavailable = errors.New("desktop plugin is not running")

// Router 把桌面能力（截图、键鼠、剪贴板、屏幕信息）转发给桌面插件。
//
// NPA 本体只保留协议与目录管理，具体实现由插件通过 plugin.initialize 注册进来：
// 插件声明自己支持哪些 desktop.* 方法，Router 据此调用。
type Router struct {
	Plugin       *plugin.Manager
	resourceRoot string
}

func NewRouter(manager *plugin.Manager) *Router {
	return &Router{Plugin: manager}
}

func NewRouterWithRoot(manager *plugin.Manager, root string) *Router {
	return &Router{Plugin: manager, resourceRoot: root}
}

func (r *Router) usePlugin() bool {
	if r.Plugin == nil {
		return false
	}
	status, _ := r.Plugin.Status()
	return status == "running"
}

// supports 判断插件是否声明支持某个方法。
//
// 插件通过 plugin.initialize 的 capabilities 报自己的能力；插件没实现的能力
// （比如 npa-remote 没有 OCR，desktop.read）要明确报不支持，而不是把
// 「方法不存在」的 RPC 错误抛给调用方。插件没报能力表时按老规矩直接调用。
func (r *Router) supports(method string) bool {
	if r.Plugin == nil {
		return false
	}
	capabilities := r.Plugin.Info().Capabilities
	if len(capabilities) == 0 {
		return true
	}
	for _, capability := range capabilities {
		if capability == method {
			return true
		}
	}
	return false
}

// call 调用插件方法：插件没运行返回 ErrPluginUnavailable；
// 插件没声明该能力返回「不支持」错误。
func (r *Router) call(ctx context.Context, method string, params any, out any) error {
	if !r.usePlugin() {
		return fmt.Errorf("%w: %s", ErrPluginUnavailable, method)
	}
	if !r.supports(method) {
		return fmt.Errorf("desktop plugin does not support %s", method)
	}
	return r.Plugin.Invoke(ctx, method, params, out)
}

func (r *Router) Screens(ctx context.Context) ([]Screen, error) {
	var result []Screen
	if err := r.call(ctx, "desktop.screens", struct{}{}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *Router) Screenshot(ctx context.Context, request CaptureRequest) (CaptureResult, error) {
	if err := request.Region.validate(); err != nil {
		return CaptureResult{}, err
	}
	var result CaptureResult
	if err := r.call(ctx, "desktop.screenshot", request, &result); err != nil {
		return CaptureResult{}, err
	}
	if r.resourceRoot != "" {
		// 插件把截图写在自己的临时目录里，路径不在 core 的资源根下。
		// 把它搬进资源根再对外给路径，路径安全检查才有意义。
		stored, storeErr := r.storeResource(result.Resource.Path)
		if storeErr != nil {
			return CaptureResult{}, storeErr
		}
		result.Resource.Path = stored
	}
	if result.Resource.MimeType == "" {
		result.Resource.MimeType = mimeTypeForPath(result.Resource.Path)
	}
	return result, nil
}

// storeResource 把插件产出的文件搬进 core 的资源根，返回新路径。
func (r *Router) storeResource(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("plugin returned an empty screenshot path")
	}
	if _, err := SafeResourcePath(r.resourceRoot, path); err == nil {
		// 已经在资源根里，直接用。
		return path, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read plugin screenshot: %w", err)
	}
	if err := os.MkdirAll(r.resourceRoot, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(r.resourceRoot, fmt.Sprintf("plugin-shot-%d%s", time.Now().UnixNano(), filepath.Ext(path)))
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return "", err
	}
	return target, nil
}

// mimeTypeForPath 按扩展名猜 MIME 类型，插件没报 mime_type 时兜底。
func mimeTypeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	default:
		return ""
	}
}

func (r *Router) MoveMouse(ctx context.Context, request MoveRequest) error {
	return r.call(ctx, "desktop.mouse_move", request, nil)
}

func (r *Router) MouseButton(ctx context.Context, request ButtonRequest) error {
	return r.call(ctx, "desktop.mouse_click", request, nil)
}

func (r *Router) Scroll(ctx context.Context, request ScrollRequest) error {
	return r.call(ctx, "desktop.mouse_scroll", request, nil)
}

// ReadScreen 读屏幕文字（OCR）：插件支持 desktop.read 时走插件。
func (r *Router) ReadScreen(ctx context.Context, request ReadRequest) (ReadResult, error) {
	if err := request.Region.validate(); err != nil {
		return ReadResult{}, err
	}
	var result ReadResult
	if err := r.call(ctx, "desktop.read", request, &result); err != nil {
		return ReadResult{}, err
	}
	return result, nil
}

// TypeText 键盘输入：走插件的 desktop.type（物理按键，兼容中文输入法）。
func (r *Router) TypeText(ctx context.Context, request TypeRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return r.call(ctx, "desktop.type", request, nil)
}

// Hotkey 组合键：走插件的 desktop.hotkey。
func (r *Router) Hotkey(ctx context.Context, request HotkeyRequest) error {
	if err := request.validate(); err != nil {
		return err
	}
	return r.call(ctx, "desktop.hotkey", request, nil)
}

// Info 屏幕基础信息（鼠标位置/分辨率/前台窗口矩形）：走插件的 desktop.info。
func (r *Router) Info(ctx context.Context, request InfoRequest) (InfoResult, error) {
	var result InfoResult
	if err := r.call(ctx, "desktop.info", request, &result); err != nil {
		return InfoResult{}, err
	}
	return result, nil
}
