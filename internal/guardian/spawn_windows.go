//go:build windows

package guardian

import (
	"fmt"
	"os/exec"
	"syscall"
)

// CREATE_NO_WINDOW 让控制台程序在没有可见窗口的情况下运行。
const createNoWindow = 0x08000000

// defaultSpawn 以隐藏窗口 + 独立进程的方式拉起主程序：不弹控制台窗口，
// 守护进程退出后它继续存活。
func defaultSpawn(path, dataDir string) error {
	args := []string{}
	if dataDir != "" {
		args = append(args, "--data-dir", dataDir)
	}
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return cmd.Start()
}

// defaultSpawnDelayedDelete Windows 上运行中的自身删不掉：起一个独立的 cmd，
// 等本进程退出后再删。ping 起延时作用（约 3 秒）。
func defaultSpawnDelayedDelete(path string) {
	script := fmt.Sprintf("ping -n 4 127.0.0.1 >nul & del /f /q %q", path)
	cmd := exec.Command("cmd", "/c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	_ = cmd.Start()
}
