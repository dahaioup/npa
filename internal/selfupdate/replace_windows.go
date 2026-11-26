//go:build windows

package selfupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
)

// Apply Windows 不允许覆盖正在运行的 exe，因此：
// 旧文件改名（Windows 允许 rename 运行中的 exe）→ 写入新文件 → 拉起新进程。
// 调用方随后负责优雅退出旧进程。改名失败会自动回滚，避免服务起不来。
func Apply(newBinary string, args []string) (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	backup := fmt.Sprintf("%s.old-%d", executable, time.Now().Unix())
	if err := os.Rename(executable, backup); err != nil {
		return false, apperrors.Wrap(apperrors.Internal, "无法重命名正在运行的可执行文件", err)
	}
	data, err := os.ReadFile(newBinary)
	if err != nil {
		_ = os.Rename(backup, executable)
		return false, err
	}
	if err := os.WriteFile(executable, data, 0o755); err != nil {
		_ = os.Rename(backup, executable)
		return false, apperrors.Wrap(apperrors.Internal, "写入新版本失败", err)
	}
	_ = os.Remove(newBinary)

	command := exec.Command(executable, args[1:]...)
	// 与控制台解耦：关闭本窗口不会连带杀掉新进程
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 /* DETACHED_PROCESS */}
	if err := command.Start(); err != nil {
		return false, apperrors.Wrap(apperrors.Internal, "启动新版本失败", err)
	}
	return true, nil
}
