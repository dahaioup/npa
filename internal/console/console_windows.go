//go:build windows

package console

import "golang.org/x/sys/windows"

// user32 的两个函数在 x/sys/windows 里没有封装，这里按需动态取。
var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procGetConsoleWindow = user32.NewProc("GetConsoleWindow")
	procShowWindow       = user32.NewProc("ShowWindow")
)

const swHide = 0 // SW_HIDE

// hidePlatformConsole 藏掉本进程的控制台窗口。
// 窗口只是隐藏、进程仍附着在该控制台上，因此不会因为"关窗口"被杀；
// 需要结束进程时用 Web 设置页的「退出服务」。
func hidePlatformConsole() bool {
	handle, _, _ := procGetConsoleWindow.Call()
	if handle == 0 {
		return false // 没有控制台（本身就是窗口进程），无需隐藏
	}
	procShowWindow.Call(handle, swHide)
	return true
}
