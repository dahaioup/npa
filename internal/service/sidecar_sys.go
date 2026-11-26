package service

import "os/exec"

// applySysProcAttr 让 sidecar 脱离 NPA 会话独立存活且不弹窗口。
func applySysProcAttr(cmd *exec.Cmd) {
	applySysProcAttrPlatform(cmd)
}
