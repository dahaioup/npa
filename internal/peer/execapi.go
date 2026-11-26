package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/npa/npa/internal/selfupdate"
)

// ExecAPI 是节点侧的执行/升级入口：与 mesh 端口同生命周期，
// 供「经上级代理拨到本机 mesh 端口」的跨域场景执行命令、落地自升级。
//
// 文件面（/api/file/*）不在这里：它归文件插件所有，挂在插件自己的端口
// （默认 56751），mesh 端口（56701）不再提供文件端点。
type ExecAPI struct {
	gate   *ioGate
	selfID string
}

// NewExecAPI 构造节点执行 API（selfID 为空时不做目标校验，仅供测试）。
func NewExecAPI() *ExecAPI { return &ExecAPI{gate: newIOGate(maxConcurrentIO)} }

// NewExecAPIWithID 构造带节点身份的执行 API：请求显式指定了别的 node 时拒绝，
// 避免「参数被忽略、静默落在错误机器上」。
func NewExecAPIWithID(selfID string) *ExecAPI {
	return &ExecAPI{gate: newIOGate(maxConcurrentIO), selfID: selfID}
}

// Register 把执行与升级端点挂到 mux 上。
//
//	POST /api/exec           在本机执行命令，回传 JSON 结果
//	POST /api/upgrade/apply  下载包→校验→选平台二进制→守护拉起
func (f *ExecAPI) Register(mux *http.ServeMux) {
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if f.rejectForeign(w, r) {
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /api/exec", guard(f.handleExec))
	mux.HandleFunc("POST /api/upgrade/apply", guard(f.handleUpgradeApply))
}

// foreignTarget 检查请求是否指向其它节点。query 的 node/node_id 与 bodyNode
// （如 exec JSON 里的 node_id）任一非空即视为显式寻址。
func (f *ExecAPI) foreignTarget(r *http.Request, bodyNode string) string {
	if f.selfID == "" {
		return ""
	}
	target := r.URL.Query().Get("node")
	if target == "" {
		target = r.URL.Query().Get("node_id")
	}
	if target == "" {
		target = bodyNode
	}
	if target == "" || target == f.selfID {
		return ""
	}
	return target
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// rejectForeign 供各 handler 开头调用：目标不是本机时回 404。
func (f *ExecAPI) rejectForeign(w http.ResponseWriter, r *http.Request) bool {
	target := f.foreignTarget(r, "")
	if target == "" {
		return false
	}
	fileError(w, http.StatusNotFound,
		"node not found: request targets %q but this node is %q (mesh /api/* is node-local; address the target node's own port or route via its upper)",
		target, f.selfID)
	return true
}

type execRequest struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
	NodeID    string `json:"node_id"`
	Node      string `json:"node"`
}

const (
	// execOutputLimit 单条命令 stdout/stderr 的回传上限，超出截断并打
	// truncated 标志——防止 `cat 大文件` 之类把节点内存吃光。
	execOutputLimit = 2 << 20
	// maxConcurrentIO 单节点同时处理的 exec 数上限，超出直接
	// 拒绝（HTTP 429），避免多个上级/重试把节点打满。
	maxConcurrentIO = 2
)

// ioGate 按目标键（固定 "local"）做并发闸门。
type ioGate struct {
	mu    sync.Mutex
	busy  map[string]int
	limit int
}

func newIOGate(limit int) *ioGate { return &ioGate{busy: map[string]int{}, limit: limit} }

// acquire 占用一个槽位；占不到返回 false。
func (g *ioGate) acquire(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy[key] >= g.limit {
		return false
	}
	g.busy[key]++
	return true
}

func (g *ioGate) release(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.busy[key]--
}

// limitedBuffer 最多累计 limit 字节，超出丢弃并置 truncated。
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

// handleExec 在本机执行命令并回传 JSON 结果（对标反向链路 TypeExec 通道，
// 让"经上级代理拨到本机 mesh 端口"的跨域场景也能执行命令）。
func (f *ExecAPI) handleExec(w http.ResponseWriter, r *http.Request) {
	var request execRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
		fileError(w, http.StatusBadRequest, "bad exec request: %v", err)
		return
	}
	if target := f.foreignTarget(r, firstNonEmpty(request.NodeID, request.Node)); target != "" {
		fileError(w, http.StatusNotFound,
			"node not found: request targets %q but this node is %q (mesh /api/exec is node-local; address the target node's own port or route via its upper)",
			target, f.selfID)
		return
	}
	command := strings.TrimSpace(request.Command)
	if command == "" {
		fileError(w, http.StatusBadRequest, "command is required")
		return
	}
	timeout := time.Duration(request.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if !f.gate.acquire("io") {
		fileError(w, http.StatusTooManyRequests, "node busy: another exec is running (limit %d)", maxConcurrentIO)
		return
	}
	defer f.gate.release("io")
	ctx, cancel := context.WithTimeout(r.Context(), timeout+15*time.Second)
	defer cancel()
	var shell string
	var args []string
	if runtime.GOOS == "windows" {
		shell, args = "cmd", []string{"/c", command}
	} else {
		shell, args = "sh", []string{"-c", command}
	}
	cmd := exec.CommandContext(ctx, shell, args...)
	hideExecWindow(cmd)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	started := time.Now()
	if err := cmd.Start(); err != nil {
		writeFileJSON(w, map[string]any{"stdout": "", "stderr": err.Error(), "exit_code": 127, "duration_ms": 0})
		return
	}
	var outBuf, errBuf limitedBuffer
	outBuf.limit, errBuf.limit = execOutputLimit, execOutputLimit
	_, _ = io.Copy(&outBuf, stdout)
	_, _ = io.Copy(&errBuf, stderr)
	runErr := cmd.Wait()
	exitCode := 0
	if runErr != nil {
		exitCode = 1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"stdout":      outBuf.String(),
		"stderr":      errBuf.String(),
		"truncated":   outBuf.truncated || errBuf.truncated,
		"exit_code":   exitCode,
		"duration_ms": time.Since(started).Milliseconds(),
	})
}

// handleUpgradeApply 原子升级入口：下载包→校验→选平台二进制→守护拉起。
// 与 /api/exec 同级（mesh 端口），请求体 selfupdate.ApplyRequest。
func (f *ExecAPI) handleUpgradeApply(w http.ResponseWriter, r *http.Request) {
	var req selfupdate.ApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONValue(w, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	result, err := selfupdate.UpgradeApply(r.Context(), req)
	if err != nil {
		writeJSONValue(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSONValue(w, result)
}

func fileError(w http.ResponseWriter, status int, format string, args ...any) {
	http.Error(w, fmt.Sprintf(format, args...), status)
}

func writeFileJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}
