//go:build windows

package service

import (
	"os/exec"
	"syscall"
)

const sidecarCreateNoWindow = 0x08000000 // CREATE_NO_WINDOW

func applySysProcAttrPlatform(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: sidecarCreateNoWindow,
	}
}
