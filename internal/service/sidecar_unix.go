//go:build !windows

package service

import (
	"os/exec"
	"syscall"
)

func applySysProcAttrPlatform(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
