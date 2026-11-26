// Package autostart 管理 NPA 的开机自启动项。
//
// 平台实现：
//   - Windows：HKCU\Software\Microsoft\Windows\CurrentVersion\Run 的 NPA 值（无需管理员）
//   - macOS：~/Library/LaunchAgents/com.npa.desktop.plist + launchctl bootstrap
//   - Linux：systemd user unit；没有 systemd 时回退 XDG autostart .desktop
package autostart

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/npa/npa/internal/state"
)

// ErrNotSupported 表示当前平台没有自启动实现。
var ErrNotSupported = errors.New("当前平台不支持开机自启动")

// Status 描述当前自启动项的真实状态（读取系统，而不是读设置）。
type Status struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	Method    string `json:"method"` // registry | launchd | systemd | xdg
	Path      string `json:"path"`
	Command   string `json:"command,omitempty"`
}

// Current 读取系统里真实的自启动状态（不是读设置）。
func Current() (Status, error) { return platformStatus() }

// Enable 写入开机自启动项；dataDir 非默认时会带上 --data-dir。
func Enable(dataDir string) error { return platformEnable(dataDir) }

// Disable 移除开机自启动项，幂等。
func Disable() error { return platformDisable() }

// executable 与 launchctl 是可替换的钩子，便于单测不污染宿主机。
var executable = os.Executable

// commandLine 生成启动项要执行的命令行；dataDir 非默认时显式带上 --data-dir，
// 否则自启动后会读到另一个状态库。
func commandLine(dataDir string) (string, error) {
	path, err := executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	args := []string{path}
	dataDir = strings.TrimSpace(dataDir)
	if dataDir != "" {
		if def, err := state.DefaultDataDir(); err == nil && filepath.Clean(dataDir) != filepath.Clean(def) {
			args = append(args, "--data-dir", dataDir)
		}
	}
	return quoteArgs(args), nil
}

// quoteArgs 把参数拼成 Windows/POSIX shell 都能接受的命令行。
func quoteArgs(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t\"") {
			arg = `"` + strings.ReplaceAll(arg, `"`, `\"`) + `"`
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}

// arguments 返回命令行的参数切片（写 plist / systemd unit 时需要逐项）。
func arguments(dataDir string) ([]string, error) {
	command, err := commandLine(dataDir)
	if err != nil {
		return nil, err
	}
	return splitCommandLine(command), nil
}

// splitCommandLine 只处理本包自己生成的引号形式，够用且不引依赖。
func splitCommandLine(command string) []string {
	var (
		result  []string
		current strings.Builder
		quoted  bool
	)
	for _, r := range command {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ' ' && !quoted:
			if current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}
