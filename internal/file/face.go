package file

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Face 是文件插件的「本机文件面」：把本机文件系统以纯 HTTP 暴露出去，
// 上级（或其它节点）据此拉取/推送文件，不再占用 SFTP-over-WS 通道池。
//
// 它随内置插件一起跑在 NPA 进程内，监听插件自己的端口（默认 56751）；
// mesh 端口（56701）不再提供文件面。
//
// 注意：按现阶段约定暂不挂任何鉴权（权限体系后续统一重设计）。
type Face struct {
	gate   *ioGate
	selfID string
}

// NewFace 构造本机文件面（selfID 为空时不做目标校验，仅供测试）。
func NewFace() *Face { return &Face{gate: newIOGate(maxConcurrentIO)} }

// NewFaceWithID 构造带节点身份的文件面：请求显式指定了别的 node 时拒绝，
// 避免「参数被忽略、静默落在错误机器上」。
func NewFaceWithID(selfID string) *Face {
	return &Face{gate: newIOGate(maxConcurrentIO), selfID: selfID}
}

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
func (f *Face) Register(mux *http.ServeMux) {
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if f.rejectForeign(w, r) {
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /api/file/list", guard(f.handleList))
	mux.HandleFunc("GET /api/file/stat", guard(f.handleStat))
	mux.HandleFunc("GET /api/file/read", guard(f.handleRead))
	mux.HandleFunc("GET /api/file/download", guard(f.handleDownload))
	mux.HandleFunc("PUT /api/file/upload", guard(f.handleUpload))
	mux.HandleFunc("POST /api/file/write", guard(f.handleWrite))
	mux.HandleFunc("POST /api/file/mkdir", guard(f.handleMkdir))
	mux.HandleFunc("POST /api/file/remove", guard(f.handleRemove))
	mux.HandleFunc("POST /api/file/rename", guard(f.handleRename))
}

// foreignTarget 检查请求是否指向其它节点。query 的 node/node_id 任一非空
// 即视为显式寻址。
func (f *Face) foreignTarget(r *http.Request) string {
	if f.selfID == "" {
		return ""
	}
	target := r.URL.Query().Get("node")
	if target == "" {
		target = r.URL.Query().Get("node_id")
	}
	if target == "" || target == f.selfID {
		return ""
	}
	return target
}

// rejectForeign 供各 handler 开头调用：目标不是本机时回 404。
func (f *Face) rejectForeign(w http.ResponseWriter, r *http.Request) bool {
	target := f.foreignTarget(r)
	if target == "" {
		return false
	}
	fileError(w, http.StatusNotFound,
		"node not found: request targets %q but this node is %q (file api is node-local; address the target node's own port or route via its upper)",
		target, f.selfID)
	return true
}

const (
	// maxConcurrentIO 单节点同时处理的 upload 数上限，超出直接
	// 拒绝（HTTP 429），避免多个上级/重试把节点打满。
	maxConcurrentIO = 2
)

// ioGate 按目标键做并发闸门。
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
// 空闲上限与客户端侧共用 ioIdleTimeout。
type idleBody struct {
	rc   *http.ResponseController
	body io.Reader
}

func (b idleBody) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(ioIdleTimeout))
	return b.body.Read(p)
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

func (f *Face) handleList(w http.ResponseWriter, r *http.Request) {
	dir := cleanFileSystemPath(r.URL.Query().Get("path"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot list %q: %v", dir, err)
		return
	}
	out := make([]fileEntry, 0, len(entries))
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

func (f *Face) handleStat(w http.ResponseWriter, r *http.Request) {
	target := cleanFileSystemPath(r.URL.Query().Get("path"))
	info, err := os.Stat(target)
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot stat %q: %v", target, err)
		return
	}
	writeFileJSON(w, toHTTPEntry(path.Dir(target), info))
}

func (f *Face) handleRead(w http.ResponseWriter, r *http.Request) {
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

func (f *Face) handleDownload(w http.ResponseWriter, r *http.Request) {
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

func (f *Face) handleUpload(w http.ResponseWriter, r *http.Request) {
	target := cleanFileSystemPath(r.URL.Query().Get("path"))
	if !f.gate.acquire("io") {
		fileError(w, http.StatusTooManyRequests, "node busy: another upload is running (limit %d)", maxConcurrentIO)
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

func (f *Face) handleWrite(w http.ResponseWriter, r *http.Request) {
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

func (f *Face) handleMkdir(w http.ResponseWriter, r *http.Request) {
	target := cleanFileSystemPath(r.URL.Query().Get("path"))
	if err := os.MkdirAll(target, 0o755); err != nil {
		fileError(w, http.StatusBadRequest, "cannot mkdir %q: %v", target, err)
		return
	}
	writeFileJSON(w, map[string]any{"path": target})
}

func (f *Face) handleRemove(w http.ResponseWriter, r *http.Request) {
	target := cleanFileSystemPath(r.URL.Query().Get("path"))
	if err := os.RemoveAll(target); err != nil {
		fileError(w, http.StatusBadRequest, "cannot remove %q: %v", target, err)
		return
	}
	writeFileJSON(w, map[string]any{"path": target})
}

func (f *Face) handleRename(w http.ResponseWriter, r *http.Request) {
	from := cleanFileSystemPath(r.URL.Query().Get("from"))
	to := cleanFileSystemPath(r.URL.Query().Get("to"))
	if err := os.Rename(from, to); err != nil {
		fileError(w, http.StatusBadRequest, "cannot rename %q -> %q: %v", from, to, err)
		return
	}
	writeFileJSON(w, map[string]any{"from": from, "to": to})
}

func toHTTPEntry(dir string, info os.FileInfo) fileEntry {
	return fileEntry{
		Name:        info.Name(),
		Path:        path.Join(filepath.ToSlash(dir), info.Name()),
		Size:        info.Size(),
		Mode:        info.Mode().String(),
		ModTimeUnix: info.ModTime().Unix(),
		IsDir:       info.IsDir(),
	}
}

func writeFileJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}
