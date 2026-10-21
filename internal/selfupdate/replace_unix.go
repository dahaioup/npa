//go:build !windows

package selfupdate

import (
	"os"
	"path/filepath"
	"syscall"

	apperrors "github.com/npa/npa/internal/errors"
)

// Apply 用新二进制替换正在运行的可执行文件，并原地 exec 重启。
// Unix 上 rename 覆盖运行中的可执行文件是安全的（内核继续持有旧 inode 的映射），
// syscall.Exec 直接替换当前进程，端口无需释放再抢。
func Apply(newBinary string, args []string) (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	if err := os.Chmod(newBinary, 0o755); err != nil {
		return false, err
	}
	if err := os.Rename(newBinary, executable); err != nil {
		return false, apperrors.Wrap(apperrors.Internal, "替换本机二进制失败", err)
	}
	if len(args) == 0 {
		args = []string{executable}
	}
	if err := syscall.Exec(executable, args, os.Environ()); err != nil {
		return false, apperrors.Wrap(apperrors.Internal, "重启到新版本失败", err)
	}
	// 正常情况下 exec 不会返回
	return false, nil
}
