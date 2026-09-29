package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
)

var (
	ErrRuntimeMissing = errors.New("plugin runtime missing")
	ErrCrashed        = errors.New("plugin crashed")
)

type Process struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.Reader
	stderr  io.Reader
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan responseMessage
	closed  chan struct{}
	done    chan struct{}
	once    sync.Once
}

func Start(ctx context.Context, executable string, args ...string) (*Process, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	process := &Process{
		cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr,
		pending: map[int64]chan responseMessage{}, closed: make(chan struct{}), done: make(chan struct{}),
	}
	go process.readLoop()
	go process.readStderr()
	go func() {
		_ = cmd.Wait()
		close(process.done)
		process.closePending(ErrCrashed)
	}()
	return process, nil
}

func (p *Process) readLoop() {
	reader := bufio.NewReader(p.stdout)
	for {
		data, err := ReadMessage(reader)
		if err != nil {
			return
		}
		response, err := DecodeResponse(data)
		if err != nil {
			slog.Warn("invalid plugin response", "error", err)
			continue
		}
		var id int64
		if len(response.ID) > 0 {
			if err := json.Unmarshal(response.ID, &id); err != nil {
				slog.Warn("invalid plugin response id", "error", err)
				continue
			}
		}
		p.mu.Lock()
		ch := p.pending[id]
		delete(p.pending, id)
		p.mu.Unlock()
		if ch != nil {
			ch <- response
		}
	}
}

func (p *Process) readStderr() {
	scanner := bufio.NewScanner(p.stderr)
	for scanner.Scan() {
		slog.Info("plugin stderr", "message", scanner.Text())
	}
}

func (p *Process) closePending(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, ch := range p.pending {
		ch <- responseMessage{Error: &RPCError{Code: -32000, Message: err.Error()}}
		delete(p.pending, id)
	}
}

func (p *Process) Request(ctx context.Context, method string, params any, result any) error {
	select {
	case <-p.done:
		return ErrCrashed
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	id := p.nextID.Add(1)
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	message := requestMessage{JSONRPC: "2.0", ID: id, Method: method, Params: rawParams}
	ch := make(chan responseMessage, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()
	if err := WriteMessage(p.stdin, message); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return err
	}
	select {
	case response := <-ch:
		if response.Error != nil {
			return response.Error
		}
		if result != nil && len(response.Result) > 0 {
			return json.Unmarshal(response.Result, result)
		}
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return ctx.Err()
	case <-p.done:
		return ErrCrashed
	}
}

func (p *Process) Shutdown(ctx context.Context) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	_ = p.Request(ctx, "plugin.shutdown", struct{}{}, nil)
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Process) Close() error {
	var err error
	p.once.Do(func() {
		close(p.closed)
		if p.cmd.Process != nil {
			err = p.cmd.Process.Kill()
		}
		_ = p.stdin.Close()
	})
	return err
}

func (p *Process) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *Process) Wait(ctx context.Context) error {
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func executableInfo(path string) (os.FileInfo, error) { return os.Stat(path) }

func IsExecutable(info os.FileInfo) bool {
	if runtime.GOOS == "windows" {
		return info.Mode().IsRegular()
	}
	return info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

func FormatCrash(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("plugin failed: %v", err)
}
