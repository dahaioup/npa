//go:build windows

package selfupdate

import (
	"os/exec"
	"syscall"
)

// detachedSysProcAttr Windows 上走 schtasks（见 apply.go launchDetached），这里占位。
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

// applySysProcAttr Windows 上隐藏窗口启动 guardian。
func applySysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = detachedSysProcAttr()
}
