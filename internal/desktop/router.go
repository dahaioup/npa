package desktop

import (
	"context"
	"errors"

	"github.com/npa/npa/internal/plugin"
)

type Router struct {
	Plugin       *plugin.Manager
	Fallback     Provider
	resourceRoot string
}

func NewRouter(manager *plugin.Manager) *Router {
	return &Router{Plugin: manager, Fallback: NewSystemProvider()}
}

func NewRouterWithRoot(manager *plugin.Manager, root string) *Router {
	return &Router{Plugin: manager, Fallback: NewSystemProviderWithRoot(root), resourceRoot: root}
}

func (r *Router) usePlugin() bool {
	if r.Plugin == nil {
		return false
	}
	status, _ := r.Plugin.Status()
	return status == "running"
}

func (r *Router) Screens(ctx context.Context) ([]Screen, error) {
	if r.usePlugin() {
		var result []Screen
		if err := r.Plugin.Invoke(ctx, "desktop.screens", struct{}{}, &result); err == nil {
			return result, nil
		} else if !errors.Is(err, plugin.ErrRuntimeMissing) {
			return nil, err
		}
	}
	return r.Fallback.Screens(ctx)
}

func (r *Router) Screenshot(ctx context.Context, request CaptureRequest) (CaptureResult, error) {
	if r.usePlugin() {
		var result CaptureResult
		if err := r.Plugin.Invoke(ctx, "desktop.screenshot", request, &result); err == nil {
			if r.resourceRoot != "" {
				if _, pathErr := SafeResourcePath(r.resourceRoot, result.Resource.Path); pathErr != nil {
					return CaptureResult{}, pathErr
				}
			}
			return result, nil
		} else if !errors.Is(err, plugin.ErrRuntimeMissing) {
			return result, err
		}
	}
	return r.Fallback.Screenshot(ctx, request)
}

func (r *Router) MoveMouse(ctx context.Context, request MoveRequest) error {
	if r.usePlugin() {
		if err := r.Plugin.Invoke(ctx, "desktop.mouse_move", request, nil); err == nil {
			return nil
		} else if !errors.Is(err, plugin.ErrRuntimeMissing) {
			return err
		}
	}
	return r.Fallback.MoveMouse(ctx, request)
}

func (r *Router) MouseButton(ctx context.Context, request ButtonRequest) error {
	if r.usePlugin() {
		if err := r.Plugin.Invoke(ctx, "desktop.mouse_click", request, nil); err == nil {
			return nil
		} else if !errors.Is(err, plugin.ErrRuntimeMissing) {
			return err
		}
	}
	return r.Fallback.MouseButton(ctx, request)
}

func (r *Router) Scroll(ctx context.Context, request ScrollRequest) error {
	if r.usePlugin() {
		if err := r.Plugin.Invoke(ctx, "desktop.mouse_scroll", request, nil); err == nil {
			return nil
		} else if !errors.Is(err, plugin.ErrRuntimeMissing) {
			return err
		}
	}
	return r.Fallback.Scroll(ctx, request)
}
