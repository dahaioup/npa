//go:build !windows

package selfupdate

import (
	"os/exec"
	"syscall"
)

// detachedSysProcAttr 让守护进程进入独立会话：发起方进程退出/连接断开都不牵连它。
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// applySysProcAttr 给 guardian 启动命令加 Setsid（Unix）。
func applySysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = detachedSysProcAttr()
}
