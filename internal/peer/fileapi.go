package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FileAPI 把本机文件系统以纯 HTTP 暴露给上级：大文件传输不再走
// SFTP-over-WS 数据通道，避免占用/拖垮通道池。
// 注意：按现阶段约定暂不挂任何鉴权（权限体系后续统一重设计）。
type FileAPI struct {
	gate *ioGate
}

// NewFileAPI 构造节点本机文件 API。
func NewFileAPI() *FileAPI { return &FileAPI{gate: newIOGate(maxConcurrentIO)} }

// Register 把文件端点挂到 mux 上。上级使用的入口：
//
//	GET  /api/file/list?path=      列目录
//	GET  /api/file/stat?path=      单条信息
//	GET  /api/file/read?path=      整读（上限 4MB，兼容 MCP file.read）
//	GET  /api/file/download?path=  流式下载（大文件主路径）
//	PUT  /api/file/upload?path=    流式上传（请求体即文件内容）
//	POST /api/file/write?path=     整写（小文本，上限 4MB）
//	POST /api/file/mkdir?path=     建目录（含父级）
//	POST /api/file/remove?path=    删除文件或目录
//	POST /api/file/rename?from=&to=
func (f *FileAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/file/list", f.handleList)
	mux.HandleFunc("GET /api/file/stat", f.handleStat)
	mux.HandleFunc("GET /api/file/read", f.handleRead)
	mux.HandleFunc("GET /api/file/download", f.handleDownload)
	mux.HandleFunc("PUT /api/file/upload", f.handleUpload)
	mux.HandleFunc("POST /api/file/write", f.handleWrite)
	mux.HandleFunc("POST /api/file/mkdir", f.handleMkdir)
	mux.HandleFunc("POST /api/file/remove", f.handleRemove)
	mux.HandleFunc("POST /api/file/rename", f.handleRename)
	mux.HandleFunc("POST /api/exec", f.handleExec)
}

type execRequest struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
}

const (
	// execOutputLimit 单条命令 stdout/stderr 的回传上限，超出截断并打
	// truncated 标志——防止 `cat 大文件` 之类把节点内存吃光。
	execOutputLimit = 2 << 20
	// ioIdleTimeout 传输空闲上限：连续这么久没有任何字节进展即断开。
	// 大文件不限总时长，但死连接（NAT 断流 / 对端消失）必须能被发现。
	ioIdleTimeout = 60 * time.Second
	// maxConcurrentIO 单节点同时处理的 upload / exec 数上限，超出直接
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

// idleBody 包装请求体：每次 Read 前把连接读截止时间延后 idle——连续
// idle 没有字节进展就返回错误（发现静默断掉的死连接），大文件总时长不限。
type idleBody struct {
	rc   *http.ResponseController
	body io.Reader
}

func (b idleBody) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(ioIdleTimeout))
	return b.body.Read(p)
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
func (f *FileAPI) handleExec(w http.ResponseWriter, r *http.Request) {
	var request execRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
		fileError(w, http.StatusBadRequest, "bad exec request: %v", err)
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
		fileError(w, http.StatusTooManyRequests, "node busy: another exec/upload is running (limit %d)", maxConcurrentIO)
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

func fileError(w http.ResponseWriter, status int, format string, args ...any) {
	http.Error(w, fmt.Sprintf(format, args...), status)
}

func cleanFileSystemPath(value string) string {
	if value == "" {
		return "."
	}
	return filepath.Clean(value)
}

type httpFileEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime int64  `json:"mod_time_unix"`
	IsDir   bool   `json:"is_dir"`
}

func (f *FileAPI) handleList(w http.ResponseWriter, r *http.Request) {
	dir := cleanFileSystemPath(r.URL.Query().Get("path"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot list %q: %v", dir, err)
		return
	}
	out := make([]httpFileEntry, 0, len(entries))
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		out = append(out, toHTTPEntry(dir, info))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	writeFileJSON(w, out)
}

func (f *FileAPI) handleStat(w http.ResponseWriter, r *http.Request) {
	target := cleanFileSystemPath(r.URL.Query().Get("path"))
	info, err := os.Stat(target)
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot stat %q: %v", target, err)
		return
	}
	writeFileJSON(w, toHTTPEntry(path.Dir(target), info))
}

func (f *FileAPI) handleRead(w http.ResponseWriter, r *http.Request) {
	const maxRead = 4 << 20
	target := cleanFileSystemPath(r.URL.Query().Get("path"))
	file, err := os.Open(target)
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot open %q: %v", target, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, io.LimitReader(file, maxRead))
}

func (f *FileAPI) handleDownload(w http.ResponseWriter, r *http.Request) {
	target := cleanFileSystemPath(r.URL.Query().Get("path"))
	file, err := os.Open(target)
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot open %q: %v", target, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, file)
}

func (f *FileAPI) handleUpload(w http.ResponseWriter, r *http.Request) {
	target := cleanFileSystemPath(r.URL.Query().Get("path"))
	if !f.gate.acquire("io") {
		fileError(w, http.StatusTooManyRequests, "node busy: another exec/upload is running (limit %d)", maxConcurrentIO)
		return
	}
	defer f.gate.release("io")
	if dir := filepath.Dir(target); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	// 原子写：先写同目录临时文件，全部收完再 rename——中断不会留半截目标文件。
	tmp, err := os.CreateTemp(filepath.Dir(target), ".npa-upload-*")
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot create temp file: %v", err)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // 成功 rename 后这句是 no-op
	written, err := io.Copy(tmp, idleBody{rc: http.NewResponseController(w), body: r.Body})
	if err != nil {
		tmp.Close()
		fileError(w, http.StatusInternalServerError, "upload interrupted after %d bytes: %v", written, err)
		return
	}
	if err := tmp.Close(); err != nil {
		fileError(w, http.StatusInternalServerError, "cannot flush temp file: %v", err)
		return
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		fileError(w, http.StatusInternalServerError, "cannot chmod temp file: %v", err)
		return
	}
	if err := os.Rename(tmpPath, target); err != nil {
		fileError(w, http.StatusInternalServerError, "cannot move into place: %v", err)
		return
	}
	writeFileJSON(w, map[string]any{"path": target, "bytes": written})
}

func (f *FileAPI) handleWrite(w http.ResponseWriter, r *http.Request) {
	const maxWrite = 4 << 20
	target := cleanFileSystemPath(r.URL.Query().Get("path"))
	data, err := io.ReadAll(io.LimitReader(r.Body, maxWrite+1))
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot read body: %v", err)
		return
	}
	if len(data) > maxWrite {
		fileError(w, http.StatusBadRequest, "body exceeds %d bytes; use upload", maxWrite)
		return
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		fileError(w, http.StatusBadRequest, "cannot write %q: %v", target, err)
		return
	}
	writeFileJSON(w, map[string]any{"path": target, "bytes": len(data)})
}

func (f *FileAPI) handleMkdir(w http.ResponseWriter, r *http.Request) {
	target := cleanFileSystemPath(r.URL.Query().Get("path"))
	if err := os.MkdirAll(target, 0o755); err != nil {
		fileError(w, http.StatusBadRequest, "cannot mkdir %q: %v", target, err)
		return
	}
	writeFileJSON(w, map[string]any{"path": target})
}

func (f *FileAPI) handleRemove(w http.ResponseWriter, r *http.Request) {
	target := cleanFileSystemPath(r.URL.Query().Get("path"))
	if err := os.RemoveAll(target); err != nil {
		fileError(w, http.StatusBadRequest, "cannot remove %q: %v", target, err)
		return
	}
	writeFileJSON(w, map[string]any{"path": target})
}

func (f *FileAPI) handleRename(w http.ResponseWriter, r *http.Request) {
	from := cleanFileSystemPath(r.URL.Query().Get("from"))
	to := cleanFileSystemPath(r.URL.Query().Get("to"))
	if err := os.Rename(from, to); err != nil {
		fileError(w, http.StatusBadRequest, "cannot rename %q -> %q: %v", from, to, err)
		return
	}
	writeFileJSON(w, map[string]any{"from": from, "to": to})
}

func toHTTPEntry(dir string, info os.FileInfo) httpFileEntry {
	mode := uint32(info.Mode())
	return httpFileEntry{
		Name:    info.Name(),
		Path:    path.Join(filepath.ToSlash(dir), info.Name()),
		Size:    info.Size(),
		Mode:    strconv.FormatUint(uint64(mode), 8),
		ModTime: info.ModTime().Unix(),
		IsDir:   info.IsDir(),
	}
}

func writeFileJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}
