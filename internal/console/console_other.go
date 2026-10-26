//go:build !windows

package console

// Unix 侧没有"控制台窗口"概念，隐藏由 run.sh 的后台异步启动实现。
func hidePlatformConsole() bool { return false }
