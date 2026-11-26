//go:build !windows

package peer

import "os/exec"

func hideExecWindow(_ *exec.Cmd) {}
