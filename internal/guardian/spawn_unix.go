//go:build !windows

package guardian

import (
	"os/exec"
	"syscall"
)

// defaultSpawn 以独立会话（Setsid）拉起主程序：守护进程退出后它不会被一起带走，
// 也没有控制台窗口跟随。输出丢弃，主程序自己写日志文件。
func defaultSpawn(path, dataDir string) error {
	args := []string{}
	if dataDir != "" {
		args = append(args, "--data-dir", dataDir)
	}
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	return cmd.Start()
}

// windowsProcessAlive 仅 Windows 编译；Unix 下不可达。
func windowsProcessAlive(pid int) bool { return false }
