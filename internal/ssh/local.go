package sshx

import (
	"context"
	"net"

	"github.com/npa/npa/internal/model"
)

// LocalExec 在本机执行命令。反向链路（peer）的对端复用它，
// 避免在 peer 包里重复实现进程执行、超时与退出码语义。
func LocalExec(ctx context.Context, request ExecRequest) (model.ExecResult, error) {
	return (&Manager{}).localExec(ctx, request)
}

// LocalShell 打开本机交互式 shell。
func LocalShell() (*Shell, error) { return (&Manager{}).openLocalShell() }

// NewShell 用外部输入输出流构造 Shell：反向链路对端把 WS 通道接到本地 shell。
func NewShell(stdin, stdout net.Conn, closeFn func() error) *Shell {
	return &Shell{Stdin: stdin, Stdout: stdout, closeFn: closeFn}
}
