//go:build !windows

package guardian

import (
	"fmt"
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

// defaultSpawnDelayedDelete 兜底：Unix 上运行中的自身其实可以直接删，
// 走到这里说明删除失败，用一个独立的 rm 延迟再删一次。
func defaultSpawnDelayedDelete(path string) {
	script := fmt.Sprintf("sleep 2; rm -f %q", path)
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Start()
}
