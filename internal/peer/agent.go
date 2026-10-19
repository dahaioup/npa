package peer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/sftp"

	"github.com/npa/npa/internal/model"
	sshx "github.com/npa/npa/internal/ssh"
)

// DialFunc 按 via 拨号：direct 直连目标，ssh:<nodeID> 经该节点的 SSH 客户端转发。
type DialFunc func(ctx context.Context, target, via string) (net.Conn, error)

// AgentOptions 由 service 注入，agent 只负责发现、注册、长连接与通道处理。
type AgentOptions struct {
	Version      string
	NodeID       func() string
	Name         func() string
	Address      func() string
	Subnets      func() []string
	Caps         func() []string
	PoolSize     func() int
	Seeds        func() string
	ProbeSubnets func() string
	SSHRoutes    func() []model.RemoteRoute
	Dial         DialFunc
	OnError      func(error)
}

// Status 是下级侧反向接入的运行状态。
type Status struct {
	Enabled     bool        `json:"enabled"`
	Running     bool        `json:"running"`
	Primary     string      `json:"primary,omitempty"`
	Candidates  []Candidate `json:"candidates,omitempty"`
	Registered  bool        `json:"registered"`
	PoolReady   int         `json:"pool_ready"`
	LastError   string      `json:"last_error,omitempty"`
	LastConnect time.Time   `json:"last_connect,omitempty"`
}

// Agent 是下级侧：发现上级、注册、保持长连接并维持空闲连接池。
type Agent struct {
	opts AgentOptions

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	status  Status
	// active 是当前主链路；Stop 时必须主动关闭它，
	// 否则阻塞在读控制帧上的 goroutine 不会因 ctx 取消而退出。
	active *websocket.Conn
}

func NewAgent(opts AgentOptions) *Agent {
	return &Agent{opts: opts}
}

func (a *Agent) call(fn func() string, fallback string) string {
	if fn == nil {
		return fallback
	}
	if value := fn(); value != "" {
		return value
	}
	return fallback
}

func (a *Agent) registration(source string) Registration {
	subnets := []string{}
	if a.opts.Subnets != nil {
		subnets = a.opts.Subnets()
	}
	caps := []string{}
	if a.opts.Caps != nil {
		caps = a.opts.Caps()
	}
	routes := []model.RemoteRoute{}
	if a.opts.SSHRoutes != nil {
		routes = a.opts.SSHRoutes()
	}
	return Registration{
		ID: a.call(a.opts.NodeID, "peer"), Name: a.call(a.opts.Name, "peer"),
		Address: a.call(a.opts.Address, ""), Subnets: subnets, Caps: caps,
		SSHRoutes: routes, Version: a.opts.Version, Source: source,
	}
}

func (a *Agent) set(patch func(*Status)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	patch(&a.status)
}

func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := a.status
	result.Candidates = append([]Candidate(nil), a.status.Candidates...)
	return result
}

// Start 启动发现与注册循环；重复调用直接返回。
func (a *Agent) Start(parent context.Context) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	a.running = true
	a.cancel = cancel
	a.mu.Unlock()
	go a.loop(ctx)
}

func (a *Agent) Stop() {
	a.mu.Lock()
	cancel := a.cancel
	conn := a.active
	a.running = false
	a.cancel = nil
	a.active = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func (a *Agent) setActive(conn *websocket.Conn) {
	a.mu.Lock()
	a.active = conn
	a.mu.Unlock()
	if conn != nil && a.cancel == nil {
		// Stop 与建立连接并发时，避免留下无人关闭的链路
		_ = conn.Close()
	}
}

func (a *Agent) clearActive(conn *websocket.Conn) {
	a.mu.Lock()
	if a.active == conn {
		a.active = nil
	}
	a.mu.Unlock()
}

func (a *Agent) loop(ctx context.Context) {
	defer func() {
		a.set(func(s *Status) {
			s.Running, s.Enabled = false, false
			s.Registered, s.Primary = false, ""
		})
	}()
	a.set(func(s *Status) { s.Running, s.Enabled = true, true })
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		candidates, err := Discover(ctx, a.seeds(), a.probeSubnets(), DefaultPort)
		a.set(func(s *Status) { s.Candidates = candidates })
		switch {
		case err != nil:
			a.record(err)
		case len(candidates) == 0:
			a.record(fmt.Errorf("没有可用的上级节点：请填写 peer_seeds（ip:port）或 peer_probe_subnets"))
			if !sleep(ctx, 5*time.Second) {
				return
			}
			continue
		default:
			// serveFirstReachable 内部已按候选逐条记录错误
			_ = a.serveFirstReachable(ctx, candidates)
			a.set(func(s *Status) { s.Registered = false; s.Primary = "" })
			backoff = time.Second
		}
		if !sleep(ctx, clampBackoff(backoff)) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func (a *Agent) seeds() string {
	if a.opts.Seeds == nil {
		return ""
	}
	return a.opts.Seeds()
}

func (a *Agent) probeSubnets() string {
	if a.opts.ProbeSubnets == nil {
		return ""
	}
	return a.opts.ProbeSubnets()
}

func (a *Agent) serveFirstReachable(ctx context.Context, candidates []Candidate) error {
	var lastErr error
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return nil
		}
		err := a.serve(ctx, candidate)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil // 链路正常结束，回到外层重新发现
		}
		lastErr = err
		a.record(fmt.Errorf("%s: %w", candidate.Address, err))
	}
	return lastErr
}

func (a *Agent) serve(ctx context.Context, candidate Candidate) error {
	endpoint := "ws://" + candidate.Address + "/api/peers/link?node=" + url.QueryEscape(a.call(a.opts.NodeID, "peer"))
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return err
	}
	a.setActive(conn)
	defer func() {
		a.clearActive(conn)
		_ = conn.Close()
	}()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	source := candidate.Source
	if source == "" {
		source = "manual"
	}
	if err := writeJSON(conn, Control{Type: CtrlRegister, Node: ptr(a.registration(source))}); err != nil {
		return err
	}
	var ack Control
	if err := readJSON(conn, &ack, time.Now().Add(10*time.Second)); err != nil {
		return err
	}
	if ack.Type != CtrlRegistered {
		if ack.Type == CtrlError {
			return fmt.Errorf("registration rejected: %s", ack.Message)
		}
		return fmt.Errorf("unexpected registration reply: %s", ack.Type)
	}

	a.set(func(s *Status) {
		s.Primary = candidate.Address
		s.Registered = true
		s.LastConnect = time.Now()
		s.LastError = ""
	})

	poolCtx, stopPool := context.WithCancel(ctx)
	var pool sync.WaitGroup
	go a.maintainPool(poolCtx, &pool, candidate.Address) // 必须异步，否则控制帧循环无法启动
	defer func() {
		stopPool()
		pool.Wait()
	}()

	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeatDone:
				return
			case <-ticker.C:
				if err := writeJSON(conn, Control{Type: CtrlPing}); err != nil {
					return
				}
			}
		}
	}()

	for {
		var control Control
		if err := readJSON(conn, &control, time.Now().Add(linkTimeout)); err != nil {
			return err
		}
		switch control.Type {
		case CtrlPong, CtrlRegistered:
			continue
		case CtrlError:
			return fmt.Errorf("peer hub error: %s", control.Message)
		}
	}
}

// maintainPool 持续补齐空闲数据连接（对标 frp 的 workConn 池模型）。
func (a *Agent) maintainPool(ctx context.Context, wg *sync.WaitGroup, base string) {
	var (
		mu    sync.Mutex
		count int
	)
	setCount := func(value int) {
		a.set(func(s *Status) { s.PoolReady = value })
	}
	add := func(delta int) {
		mu.Lock()
		count += delta
		current := count
		mu.Unlock()
		setCount(current)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		size := a.poolSize()
		mu.Lock()
		current := count
		mu.Unlock()
		for current < size {
			if ctx.Err() != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				a.openIdle(ctx, base, add)
			}()
			current++
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Agent) poolSize() int {
	size := 4
	if a.opts.PoolSize != nil {
		size = a.opts.PoolSize()
	}
	if size < 1 {
		size = 1
	}
	if size > 32 {
		size = 32
	}
	return size
}

func (a *Agent) openIdle(ctx context.Context, base string, add func(int)) {
	endpoint := "ws://" + base + "/api/peers/conn?node=" + url.QueryEscape(a.call(a.opts.NodeID, "peer"))
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return
	}
	add(1)
	claimed := false
	defer func() {
		if !claimed {
			add(-1)
		}
		_ = conn.Close()
	}()

	var header Header
	if err := readJSON(conn, &header, time.Time{}); err != nil {
		return // 上级尚未取用连接就断开了
	}
	claimed = true
	add(-1)
	a.serveChannel(ctx, conn, header)
}

func (a *Agent) serveChannel(ctx context.Context, conn *websocket.Conn, header Header) {
	defer conn.Close()
	switch header.Type {
	case TypeOpen:
		a.serveOpen(ctx, conn, header)
	case TypeExec:
		a.serveExec(ctx, conn, header)
	case TypePTY:
		a.servePTY(conn, header)
	case TypeFS:
		a.serveFS(conn)
	default:
		_ = writeAck(conn, fmt.Errorf("unknown channel type %q", header.Type))
	}
}

// serveExec 在本机执行命令并回传 stdout/stderr 与退出码。
func (a *Agent) serveExec(ctx context.Context, conn *websocket.Conn, header Header) {
	if strings.TrimSpace(header.Command) == "" {
		_ = writeAck(conn, fmt.Errorf("command is required"))
		return
	}
	if err := writeAck(conn, nil); err != nil {
		return
	}
	timeout := time.Duration(header.TimeoutMS) * time.Millisecond
	runCtx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	result, err := sshx.LocalExec(runCtx, sshx.ExecRequest{Command: header.Command, Timeout: timeout})
	cancel()
	stdout, stderr := result.Stdout, result.Stderr
	if err != nil {
		stderr += err.Error()
		result.ExitCode = -1
	}
	_ = writeJSON(conn, ExecFrame{Stream: "out", Data: stdout})
	_ = writeJSON(conn, ExecFrame{Stream: "err", Data: stderr})
	_ = writeJSON(conn, ExecFrame{ExitCode: result.ExitCode, DurationMS: result.DurationMS})
}

// servePTY 把本机交互式 shell 接到该通道上（与 SSH 会话同形的双工流）。
func (a *Agent) servePTY(conn *websocket.Conn, _ Header) {
	shell, err := sshx.LocalShell()
	if err != nil {
		_ = writeAck(conn, err)
		return
	}
	if err := writeAck(conn, nil); err != nil {
		_ = shell.Close()
		return
	}
	stream := NewWSConn(conn)
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(shell.Stdin, stream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(stream, shell.Stdout)
		done <- struct{}{}
	}()
	<-done
	_ = shell.Close()
}

// serveFS 在本机运行标准 SFTP 服务端，协议直接跑在该通道上（不涉及 SSH 服务端）。
func (a *Agent) serveFS(conn *websocket.Conn) {
	if err := writeAck(conn, nil); err != nil {
		return
	}
	stream := NewWSConn(conn)
	server, err := sftp.NewServer(stream)
	if err != nil {
		return
	}
	defer server.Close()
	if err := server.Serve(); err != nil && err != io.EOF {
		slog.Debug("sftp over reverse link ended", "error", err)
	}
}

// serveOpen 按 via 拨号到目标：direct 由本机直连，ssh:<node> 经本机纳管的 SSH 客户端转发。
func (a *Agent) serveOpen(ctx context.Context, conn *websocket.Conn, header Header) {
	if header.Target == "" {
		_ = writeAck(conn, fmt.Errorf("target is required"))
		return
	}
	via := header.Via
	if via == "" {
		via = "direct"
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	remote, err := a.opts.Dial(dialCtx, header.Target, via)
	cancel()
	if err != nil {
		_ = writeAck(conn, err)
		return
	}
	if err := writeAck(conn, nil); err != nil {
		_ = remote.Close()
		return
	}
	pipe(NewWSConn(conn), remote)
}

func (a *Agent) record(err error) {
	if err == nil {
		return
	}
	if a.opts.OnError != nil {
		a.opts.OnError(err)
	}
	a.set(func(s *Status) { s.LastError = err.Error() })
}

func clampBackoff(backoff time.Duration) time.Duration {
	if backoff < time.Second {
		return time.Second
	}
	if backoff > time.Minute {
		return time.Minute
	}
	return backoff
}

func nextBackoff(backoff time.Duration) time.Duration {
	backoff *= 2
	if backoff > time.Minute {
		return time.Minute
	}
	return backoff
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func pipe(left, right net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(right, left)
		_ = right.Close()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(left, right)
		_ = left.Close()
		done <- struct{}{}
	}()
	<-done
}

func ptr[T any](value T) *T { return &value }
