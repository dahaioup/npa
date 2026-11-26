//go:build windows

package guardian

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	kernel32DLL    = syscall.NewLazyDLL("kernel32.dll")
	lockFileExProc = kernel32DLL.NewProc("LockFileEx")
	unlockFileProc = kernel32DLL.NewProc("UnlockFile")
)

const (
	lockfileExclusiveLock = 0x2
	lockfileFailImmediately = 0x1
)

func guardianLockPath() string {
	return filepath.Join(os.TempDir(), "npa-guardian.lock")
}

func flockLock(f *os.File) error {
	var overlapped syscall.Overlapped
	r1, _, err := lockFileExProc.Call(
		f.Fd(),
		lockfileExclusiveLock|lockfileFailImmediately,
		0, 0, 0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

func flockUnlock(f *os.File) {
	unlockFileProc.Call(f.Fd(), 0, 0, 0, 0)
}
