package desktop

import (
	"fmt"
	"os"
	"path/filepath"
)

// 本文件只保留与插件之间的数据契约（请求/响应结构）以及资源路径的
// 安全检查。桌面能力的实现全部在插件里（npa-remote），本体不再内置
// PowerShell/xdotool 脚本实现。

type Screen struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	X      int     `json:"x"`
	Y      int     `json:"y"`
	Scale  float64 `json:"scale"`
}

type Resource struct {
	Type     string `json:"type"`
	Path     string `json:"path"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

type CaptureRequest struct {
	ScreenID string  `json:"screen_id,omitempty"`
	Region   *Region `json:"region,omitempty"`
}

type CaptureResult struct {
	Resource Resource `json:"resource"`
}

type MoveRequest struct {
	X        int  `json:"x"`
	Y        int  `json:"y"`
	Relative bool `json:"relative"`
}

type ButtonRequest struct {
	Button string `json:"button"`
	Action string `json:"action"`
}

type ScrollRequest struct {
	DeltaX int `json:"delta_x"`
	DeltaY int `json:"delta_y"`
}

// TempResourceDirectory 返回插件资源（截图等）的临时目录。
func TempResourceDirectory() (string, error) {
	dir, err := os.MkdirTemp("", "npa-resources-")
	if err != nil {
		return "", fmt.Errorf("create resource directory: %w", err)
	}
	return dir, nil
}

// SafeResourcePath 校验路径必须落在 root 之内，防止插件给出越界路径。
func SafeResourcePath(root, path string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("resource root is not configured")
	}
	if path == "" {
		return "", fmt.Errorf("resource path is empty")
	}
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve resource root: %w", err)
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve resource path: %w", err)
	}
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil {
		return "", fmt.Errorf("resolve resource path: %w", err)
	}
	if rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("resource path escapes root")
	}
	return cleanPath, nil
}
