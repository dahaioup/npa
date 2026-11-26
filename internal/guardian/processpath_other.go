//go:build !windows

package guardian

import "fmt"

// getProcessPathWindows 仅在 Windows 构建中实现（processpath_windows.go）。
// 非 Windows 构建中不会走到这个分支（getProcessPath 先按 GOOS 分派）。
func getProcessPathWindows(pid int) (string, error) {
	return "", fmt.Errorf("windows only")
}
