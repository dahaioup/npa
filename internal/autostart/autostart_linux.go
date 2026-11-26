//go:build linux

package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	linuxMethodSystemd = "systemd"
	linuxMethodXDG     = "xdg"
	linuxUnitName      = "npa.service"
	linuxDesktopName   = "npa.desktop"
)

const linuxUnitTemplate = `[Unit]
Description=NPA Node Proxy Agent
After=network.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure

[Install]
WantedBy=default.target
`

const linuxDesktopTemplate = `[Desktop Entry]
Type=Application
Name=NPA
Comment=NPA Node Proxy Agent
Exec=%s
Terminal=false
X-GNOME-Autostart-enabled=true
`

// systemctl 是可替换钩子，单测不会真的改系统服务。
// 作用域与 unit 路径对齐：普通用户的 unit 在 ~/.config/systemd/user，必须带
// --user；root 写 /etc/systemd/system，用系统级 systemctl（root 的用户级
// unit 依赖图形会话的 D-Bus，开机早于登录时根本不生效）。
var systemctl = func(args ...string) error {
	if os.Getuid() != 0 {
		args = append([]string{"--user"}, args...)
	}
	output, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func hasSystemctl() bool {
	path, err := exec.LookPath("systemctl")
	return err == nil && path != ""
}

func linuxConfigHome() (string, error) {
	if value := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); value != "" {
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config"), nil
}

func linuxUnitPath() (string, error) {
	// root 用系统级 unit：用户级 unit 对 root 的开机流程不可见（依赖登录会话），
	// 而且系统级 systemctl enable 找不到 ~/.config/systemd/user 下的文件。
	if os.Getuid() == 0 {
		return filepath.Join("/etc/systemd/system", linuxUnitName), nil
	}
	config, err := linuxConfigHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "systemd", "user", linuxUnitName), nil
}

func linuxDesktopPath() (string, error) {
	config, err := linuxConfigHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "autostart", linuxDesktopName), nil
}

func extractCommand(path, prefix string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func platformStatus() (Status, error) {
	if hasSystemctl() {
		path, err := linuxUnitPath()
		if err != nil {
			return Status{Supported: true, Method: linuxMethodSystemd}, err
		}
		if _, statErr := os.Stat(path); statErr == nil {
			return Status{
				Supported: true, Enabled: true, Method: linuxMethodSystemd, Path: path,
				Command: extractCommand(path, "ExecStart="),
			}, nil
		}
	}
	path, err := linuxDesktopPath()
	if err != nil {
		return Status{Supported: true, Method: linuxMethodXDG}, err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		return Status{
			Supported: true, Enabled: true, Method: linuxMethodXDG, Path: path,
			Command: extractCommand(path, "Exec="),
		}, nil
	}
	method := linuxMethodXDG
	if hasSystemctl() {
		method = linuxMethodSystemd
	}
	return Status{Supported: true, Method: method}, nil
}

func platformEnable(dataDir string) error {
	command, err := commandLine(dataDir)
	if err != nil {
		return err
	}
	if hasSystemctl() {
		path, err := linuxUnitPath()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(fmt.Sprintf(linuxUnitTemplate, command)), 0o644); err != nil {
			return err
		}
		// 用户级 systemd 实例可能不可用（无 D-Bus），此时回退到 XDG autostart。
		// enable 不带 --now：设置接口被调用时 NPA 必然在运行，--now 会再拉起
		// 一个实例抢端口；自启动在下次开机时生效即可。
		if reloadErr := systemctl("daemon-reload"); reloadErr == nil {
			if enableErr := systemctl("enable", linuxUnitName); enableErr != nil {
				return fmt.Errorf("写入 systemd unit 成功但 enable 失败: %w", enableErr)
			}
			return nil
		}
	}
	return writeDesktopEntry(command)
}

func writeDesktopEntry(command string) error {
	path, err := linuxDesktopPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(fmt.Sprintf(linuxDesktopTemplate, command)), 0o644)
}

// Disable 幂等：systemd unit 与 .desktop 都尝试清理。
func platformDisable() error {
	var firstErr error
	if hasSystemctl() {
		// 同样不带 --now：禁用自启动不应顺手杀掉正在运行的 NPA。
		if err := systemctl("disable", linuxUnitName); err != nil {
			firstErr = err
		}
		if path, err := linuxUnitPath(); err == nil {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
				firstErr = err
			}
		}
		_ = systemctl("daemon-reload")
	}
	if path, err := linuxDesktopPath(); err == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
