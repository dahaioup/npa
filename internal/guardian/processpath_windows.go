//go:build windows

package guardian

import (
	"fmt"
	"syscall"
	"unsafe"
)

// getProcessPathWindows 用 QueryFullProcessImageNameW 查进程可执行文件路径。
// 不依赖 wmic（新版 Windows 24H2+ 已默认移除）或 PowerShell 子进程。
func getProcessPathWindows(pid int) (string, error) {
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return "", fmt.Errorf("open process %d: %w", pid, err)
	}
	defer syscall.CloseHandle(h)

	queryProc := syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	r1, _, callErr := queryProc.Call(
		uintptr(h),
		0, // dwFlags=0 → Win32 路径格式
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r1 == 0 {
		return "", fmt.Errorf("query image name: %v", callErr)
	}
	return syscall.UTF16ToString(buf[:size]), nil
}
