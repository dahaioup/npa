// 剪贴板操作：仅经 desktop 插件（npa-remote）提供，无脚本回退。
package desktop

import (
	"context"
	"errors"
)

var (
	// ErrPluginRequired 剪贴板能力只有 desktop 插件提供。
	ErrPluginRequired = errors.New("clipboard requires the desktop plugin (npa-remote)")
	// ErrEmptyText 空文本不允许写入。
	ErrEmptyText = errors.New("clipboard text is required")
	// ErrTextTooLarge 剪贴板文本超限。
	ErrTextTooLarge = errors.New("clipboard text too large (max 1MiB)")
)

const maxClipboardText = 1 << 20 // 1MiB

type ClipboardResult struct {
	Text string `json:"text"`
}

// ClipboardGet 读取节点剪贴板文本。
func (r *Router) ClipboardGet(ctx context.Context) (ClipboardResult, error) {
	var result ClipboardResult
	if r.usePlugin() && r.supports("desktop.clipboard_get") {
		if err := r.Plugin.Invoke(ctx, "desktop.clipboard_get", struct{}{}, &result); err != nil {
			return ClipboardResult{}, err
		}
		return result, nil
	}
	return ClipboardResult{}, ErrPluginRequired
}

// ClipboardSet 写入节点剪贴板文本。
func (r *Router) ClipboardSet(ctx context.Context, text string) error {
	if text == "" {
		return ErrEmptyText
	}
	if len(text) > maxClipboardText {
		return ErrTextTooLarge
	}
	if r.usePlugin() && r.supports("desktop.clipboard_set") {
		return r.Plugin.Invoke(ctx, "desktop.clipboard_set", map[string]string{"text": text}, nil)
	}
	return ErrPluginRequired
}
