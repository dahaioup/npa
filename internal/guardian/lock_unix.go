//go:build !windows

package guardian

import (
	"os"
	"path/filepath"
	"syscall"
)

func guardianLockPath() string {
	return filepath.Join(os.TempDir(), "npa-guardian.lock")
}

func flockLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func flockUnlock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
