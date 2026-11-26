//go:build windows

package guardian

import (
	"os/exec"
	"syscall"
	"unsafe"
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

// windowsProcessAlive 用 kernel32 OpenProcess+GetExitCodeProcess 判断存活。
func windowsProcessAlive(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	openProc := kernel32.NewProc("OpenProcess")
	getExit := kernel32.NewProc("GetExitCodeProcess")
	closeHandle := kernel32.NewProc("CloseHandle")
	h, _, _ := openProc.Call(uintptr(processQueryLimitedInformation), uintptr(0), uintptr(pid))
	if h == 0 {
		return false
	}
	defer closeHandle.Call(h)
	const stillActive = 259
	var code uint32
	getExit.Call(h, uintptr(unsafe.Pointer(&code)))
	return code == stillActive
}
