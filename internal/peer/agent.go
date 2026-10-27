package peer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"runtime/debug"
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
	// AutoDiscover 无种子时的上级网络自动发现开关（peer_auto_discover_uppers，
	// 默认开）：traceroute 找上游内网网段 → 受限探测 56701。有种子只用种子。
	AutoDiscover func() bool
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
	// dialRequests 是"上级要求本机连过去"的派发队列：读循环只投递地址（值拷贝），
	// 由 Start 起的长生命周期消费者处理，避免 go 语句按引用捕获局部变量产生竞态。
	dialRequests chan string
}

func NewAgent(opts AgentOptions) *Agent {
	return &Agent{opts: opts, dialRequests: make(chan string, 8)}
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
	go a.consumeDialRequests(ctx)
}

// consumeDialRequests 处理"按需直连"派发。
// 本 goroutine 会等子任务全部结束才返回，避免它们读到已释放的栈槽。
func (a *Agent) consumeDialRequests(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case address := <-a.dialRequests:
			wg.Add(1)
			go func(addr string) { defer wg.Done(); a.connectEphemeral(addr) }(address)
		}
	}
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
		// 规则（2026-10-26）：配了种子就只用种子；没种子且没配探测网段、
		// 且自动发现开着，才走路由 TTL 探测自动找上级网络。
		autoMode := strings.TrimSpace(a.seeds()) == "" &&
			strings.TrimSpace(a.probeSubnets()) == "" && a.autoDiscover()
		var candidates []Candidate
		var err error
		if autoMode {
			subnets := []string{}
			if a.opts.Subnets != nil {
				subnets = a.opts.Subnets()
			}
			candidates, err = AutoDiscoverUppers(ctx, DefaultPort, subnets)
		} else {
			candidates, err = Discover(ctx, a.seeds(), a.probeSubnets(), DefaultPort)
		}
		a.set(func(s *Status) { s.Candidates = candidates })
		switch {
		case err != nil:
			a.record(err)
		case len(candidates) == 0:
			// 既没配种子也没配探测网段（或自动发现扑空）：本机多半只是在
			// 当"上级"运行，这不是错误，不记录、不打日志，只保持中性状态。
			if autoMode || strings.TrimSpace(a.seeds()) == "" && strings.TrimSpace(a.probeSubnets()) == "" {
				a.set(func(s *Status) { s.LastError = "" })
				if !sleep(ctx, 5*time.Second) {
					return
				}
				continue
			}
			// 配置了却找不到，才是需要暴露的问题
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

// controlSession 把控制通道的长寿命资源放在堆上，并保证**所有子任务在会话关闭前结束**。
// 否则后台 goroutine 会读到已返回函数的栈槽（go 语句按引用捕获局部变量），产生数据竞争。
type controlSession struct {
	conn       *websocket.Conn
	done       chan struct{}
	pool       *sync.WaitGroup
	wg         sync.WaitGroup
	address    string
	cancelPool context.CancelFunc
}

func newControlSession(conn *websocket.Conn, address string) *controlSession {
	return &controlSession{
		conn: conn, done: make(chan struct{}), pool: &sync.WaitGroup{}, address: address,
	}
}

// startPool 记录连接池维护所需的取消函数；goroutine 必须在**调用方**里启动并挂到 wg 上，
// 因为 go 语句按引用捕获局部变量，短命的辅助函数返回后子任务会读到已释放的栈槽。
func (c *controlSession) withPool(ctx context.Context) context.Context {
	poolCtx, cancel := context.WithCancel(ctx)
	c.cancelPool = cancel
	return poolCtx
}

// stop 关闭会话并**等待全部子任务退出**，之后才允许调用方返回（栈安全 + 干净关停）。
func (c *controlSession) stop() {
	close(c.done)
	if c.cancelPool != nil {
		c.cancelPool()
	}
	c.wg.Wait()
	c.pool.Wait()
}

// heartbeat 在控制通道上定期发 ping，直到 done 关闭或写失败。
// 参数按值传递，避免闭包捕获局部变量带来的竞态。
func heartbeat(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := writeJSON(conn, Control{Type: CtrlPing}); err != nil {
				return
			}
		}
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

// autoDiscover 缺省开启：只有设置显式写 "false" 才关。
func (a *Agent) autoDiscover() bool {
	return a.opts.AutoDiscover == nil || a.opts.AutoDiscover()
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

	session := newControlSession(conn, candidate.Address)
	defer session.stop()
	poolCtx := session.withPool(ctx)
	session.wg.Add(1)
	go func() { defer session.wg.Done(); a.maintainPool(poolCtx, session.pool, session.address) }()
	session.wg.Add(1)
	go func() { defer session.wg.Done(); heartbeat(session.conn, session.done) }()

	for {
		var control Control
		if err := readJSON(conn, &control, time.Now().Add(linkTimeout)); err != nil {
			return err
		}
		switch control.Type {
		case CtrlPong, CtrlRegistered:
			continue
		case CtrlConnectTo:
			// 持有者要求本机主动连到另一个上级（按需直连）。
			// 只把地址投进队列，不在这里起 goroutine：go 语句按引用捕获局部变量，
			// 而本函数随后就返回，会与仍在运行的 goroutine 产生数据竞争。
			if target := strings.TrimSpace(control.Address); target != "" {
				select {
				case a.dialRequests <- target:
				default:
					slog.Debug("按需直连队列已满，忽略该请求", "address", target)
				}
			}
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

// connectEphemeral 按上级要求临时连到另一个上级：它只是一条临时链路，
// 不改本机的主上级归属（Primary 不变），由对方在空闲超时后主动断开。
func (a *Agent) connectEphemeral(address string) {
	endpoint := "ws://" + address + "/api/peers/link?node=" + url.QueryEscape(a.call(a.opts.NodeID, "peer"))
	conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), endpoint, nil)
	if err != nil {
		slog.Warn("按需连到上级失败", "address", address, "error", err)
		return
	}
	defer conn.Close()

	registration := a.registration("on-demand")
	registration.Ephemeral = true
	if err := writeJSON(conn, Control{Type: CtrlRegister, Node: &registration}); err != nil {
		return
	}
	var ack Control
	if err := readJSON(conn, &ack, time.Now().Add(10*time.Second)); err != nil {
		slog.Warn("按需直连未被接受", "address", address, "error", err)
		return
	}
	if ack.Type != CtrlRegistered {
		slog.Warn("按需直连被拒绝", "address", address, "reply", ack.Type, "message", ack.Message)
		return
	}
	slog.Info("已按需连到上级", "address", address)

	// 临时会话期间维持心跳与数据连接池；对方空闲超时后会关闭，这里随之退出
	session := newControlSession(conn, address)
	defer session.stop()
	poolCtx := session.withPool(context.Background())
	session.wg.Add(1)
	go func() { defer session.wg.Done(); a.maintainPool(poolCtx, session.pool, session.address) }()
	session.wg.Add(1)
	go func() { defer session.wg.Done(); heartbeat(session.conn, session.done) }()
	for {
		var control Control
		if err := readJSON(conn, &control, time.Now().Add(linkTimeout)); err != nil {
			return // 对方断开：临时会话结束
		}
		if control.Type == CtrlError {
			return
		}
	}
}

func (a *Agent) serveChannel(ctx context.Context, conn *websocket.Conn, header Header) {
	defer conn.Close()
	defer func() {
		// 单条通道的 panic 只断这条连接，不能带崩整个 agent。
		if rec := recover(); rec != nil {
			slog.Error("peer channel panic", "type", header.Type, "panic", rec, "stack", string(debug.Stack()))
			_ = writeAck(conn, fmt.Errorf("channel panic: %v", rec))
		}
	}()
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
