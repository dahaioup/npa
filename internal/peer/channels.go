package peer

import (
	"context"
	"strings"
	"time"

	"github.com/pkg/sftp"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	sshx "github.com/npa/npa/internal/ssh"
)

// execReadTimeout 是等待下级回传执行结果的上限。
func execReadTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return timeout + 15*time.Second
}

// Exec 经反向链路在 peer 节点上执行命令（不依赖任何 SSH 服务端）。
func (h *Hub) Exec(ctx context.Context, nodeID, command string, timeout time.Duration) (model.ExecResult, error) {
	result := model.ExecResult{}
	if strings.TrimSpace(command) == "" {
		return result, apperrors.New(apperrors.InvalidArgument, "command is required")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	header := NewHeader(TypeExec)
	header.Command = command
	header.TimeoutMS = int(timeout.Milliseconds())
	conn, err := h.take(ctx, nodeID, header)
	if err != nil {
		return result, err
	}
	defer conn.Close()

	var stdout, stderr strings.Builder
	deadline := time.Now().Add(execReadTimeout(timeout))
	for {
		var frame ExecFrame
		if err := readJSON(conn, &frame, deadline); err != nil {
			return result, apperrors.Wrap(apperrors.Timeout, "peer exec did not return", err)
		}
		switch frame.Stream {
		case "out":
			stdout.WriteString(frame.Data)
		case "err":
			stderr.WriteString(frame.Data)
		default:
			result.Stdout = stdout.String()
			result.Stderr = stderr.String()
			result.ExitCode = frame.ExitCode
			result.DurationMS = frame.DurationMS
			return result, nil
		}
	}
}

// OpenPTY 打开 peer 节点上的交互式终端，返回与 SSH 会话同形的 Shell。
func (h *Hub) OpenPTY(ctx context.Context, nodeID string, cols, rows int) (*sshx.Shell, error) {
	header := NewHeader(TypePTY)
	header.Cols = cols
	header.Rows = rows
	conn, err := h.take(ctx, nodeID, header)
	if err != nil {
		return nil, err
	}
	stream := NewWSConn(conn)
	return sshx.NewShell(stream, stream, func() error { return conn.Close() }), nil
}

// FS 在 peer 节点上打开标准 SFTP 会话（SFTP 协议跑在反向 WS 通道上，
// 复用 pkg/sftp 的 client/server 实现，不引入 SSH 服务端）。
func (h *Hub) FS(ctx context.Context, nodeID string) (*sftp.Client, func(), error) {
	conn, err := h.take(ctx, nodeID, NewHeader(TypeFS))
	if err != nil {
		return nil, nil, err
	}
	stream := NewWSConn(conn)
	client, err := sftp.NewClientPipe(stream, stream)
	if err != nil {
		_ = conn.Close()
		return nil, nil, apperrors.Wrap(apperrors.NodeOffline, "cannot start sftp over reverse link", err)
	}
	cleanup := func() {
		_ = client.Close() // 会关闭写端，进而关闭底层 WS
		_ = conn.Close()
	}
	return client, cleanup, nil
}
