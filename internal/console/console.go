// Package console 处理"启动时隐藏控制台窗口"。
//
// Windows 上 `run.bat` 用 `start /min` 独立启动后仍会留一个最小化窗口，
// 由本包在进程内把自己的控制台窗口藏掉；Unix 侧由 run.sh 的后台异步启动实现，
// 这里无需窗口操作，直接返回 false。
package console

// Hide 尝试隐藏当前进程的控制台窗口，返回是否真的隐藏了。
func Hide() bool { return hidePlatformConsole() }
