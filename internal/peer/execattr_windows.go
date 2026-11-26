//go:build windows

package peer

import (
	"os/exec"
	"syscall"
)

// hideExecWindow prevents exec'd children (cmd /c) from allocating a console
// window. Without it every remote exec steals the desktop focus, so injected
// input from screen.type/screen.click lands in the ephemeral console instead
// of the user's foreground application.
func hideExecWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
