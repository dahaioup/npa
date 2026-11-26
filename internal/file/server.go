package file

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/npa/npa/internal/model"
)

// DefaultPort 是文件插件自带 HTTP 服务的默认端口。
// 它落在官方插件端口段（56750–56799，见 docs/plugin-ports.md）。
const DefaultPort = 56751

// Server 是文件插件自带的 HTTP 服务：
//
//   - 本机文件面：/api/file/*，把本机文件系统暴露给其它节点（上级拉取 / 推送升级包）
//   - 远端转发：node 参数非空时，调用 Provider 把请求转发到目标节点
//   - 前端页面：/ ，插件自带的文件浏览器（与 NPA 控制面分离）
//
// 它随内置插件跑在 NPA 进程内（不编译为独立进程），默认监听 56751，
// 绑定范围跟随局域网服务开关 lan_enabled（见 docs/plugin-ports.md 规则 2）。
type Server struct {
	face   *Face
	remote Provider
	selfID string
	port   int
	scope  string
	// Nodes 供前端页面列节点；为空时页面只显示本机。
	Nodes func() []model.Node

	srv *http.Server
	ln  net.Listener
}

// NewServer 创建文件插件的 HTTP 服务。
//
// remote 是远端节点文件操作的 Provider（跨节点文件分发用）；
// 为 nil 时只服务本机文件面。
func NewServer(selfID string, remote Provider, port int) *Server {
	if port <= 0 {
		port = DefaultPort
	}
	return &Server{
		face:   NewFaceWithID(selfID),
		remote: remote,
		selfID: selfID,
		port:   port,
	}
}

// SetScope 设置监听范围："lan" 监听所有网卡，"local" 只监听回环。
func (s *Server) SetScope(scope string) { s.scope = scope }

// Scope 返回当前绑定范围（"lan" 或 "local"）。
func (s *Server) Scope() string { return s.scope }

// Port 返回服务端口。
func (s *Server) Port() int { return s.port }

// Addr 返回实际监听地址（服务未启动时返回配置地址）。
func (s *Server) Addr() string {
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return net.JoinHostPort(s.listenHost(), strconv.Itoa(s.port))
}

func (s *Server) listenHost() string {
	if s.scope == "local" {
		return "127.0.0.1"
	}
	return "0.0.0.0"
}

// Handler 返回插件的 HTTP 路由（与监听无关，方便测试直接挂 httptest）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/nodes", s.handleNodes)
	mux.HandleFunc("GET /api/file/list", s.handleList)
	mux.HandleFunc("GET /api/file/stat", s.handleStat)
	mux.HandleFunc("GET /api/file/read", s.handleRead)
	mux.HandleFunc("GET /api/file/download", s.handleDownload)
	mux.HandleFunc("PUT /api/file/upload", s.handleUpload)
	mux.HandleFunc("POST /api/file/upload", s.handleUpload)
	mux.HandleFunc("POST /api/file/write", s.handleWrite)
	mux.HandleFunc("POST /api/file/mkdir", s.handleMkdir)
	mux.HandleFunc("POST /api/file/remove", s.handleRemove)
	mux.HandleFunc("POST /api/file/rename", s.handleRename)
	return mux
}

// Start 监听端口并开始服务；已在运行时返回错误。
func (s *Server) Start(ctx context.Context) error {
	if s.srv != nil {
		return errors.New("file plugin server already started")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(s.listenHost(), strconv.Itoa(s.port)))
	if err != nil {
		return fmt.Errorf("file plugin listen %s: %w", s.Addr(), err)
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	slog.Info("file plugin listening", "address", "http://"+ln.Addr().String(), "self", s.selfID)
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("file plugin server stopped", "error", err)
		}
	}()
	return nil
}

// Close 关闭服务。
func (s *Server) Close(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	err := s.srv.Shutdown(ctx)
	s.srv = nil
	s.ln = nil
	return err
}

// ---- 路由处理 ----

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeFileJSON(w, map[string]any{
		"ok":     true,
		"plugin": builtinMeta.ID,
		"self":   s.selfID,
		"port":   s.port,
	})
}

// nodeOption 是前端页面用的节点条目。
type nodeOption struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	Self    bool   `json:"self,omitempty"`
}

func (s *Server) handleNodes(w http.ResponseWriter, _ *http.Request) {
	options := []nodeOption{{ID: s.selfID, Name: "本机", Address: "", Self: true}}
	if s.Nodes != nil {
		for _, node := range s.Nodes() {
			if node.ID != "" && node.ID == s.selfID {
				continue // 本机已在首位，别重复列一遍
			}
			name := node.Name
			if name == "" {
				name = node.ID
			}
			options = append(options, nodeOption{ID: node.ID, Name: name, Address: node.Address})
		}
	}
	writeFileJSON(w, options)
}

// target 解析请求的目标节点与路径：node 为空或等于本机即本机。
func (s *Server) target(r *http.Request) (nodeID, targetPath string, local bool) {
	query := r.URL.Query()
	nodeID = strings.TrimSpace(query.Get("node"))
	targetPath = query.Get("path")
	local = nodeID == "" || nodeID == s.selfID
	return nodeID, targetPath, local
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	nodeID, targetPath, local := s.target(r)
	if local {
		s.face.handleList(w, r)
		return
	}
	if s.remote == nil {
		fileError(w, http.StatusServiceUnavailable, "remote file provider is not available")
		return
	}
	entries, err := s.remote.List(r.Context(), nodeID, targetPath)
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot list %q on %s: %v", targetPath, nodeID, err)
		return
	}
	writeFileJSON(w, entriesToJSON(entries))
}

func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	nodeID, targetPath, local := s.target(r)
	if local {
		s.face.handleStat(w, r)
		return
	}
	if s.remote == nil {
		fileError(w, http.StatusServiceUnavailable, "remote file provider is not available")
		return
	}
	entry, err := s.remote.Stat(r.Context(), nodeID, targetPath)
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot stat %q on %s: %v", targetPath, nodeID, err)
		return
	}
	writeFileJSON(w, entryToJSON(entry))
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	nodeID, targetPath, local := s.target(r)
	if local {
		s.face.handleRead(w, r)
		return
	}
	if s.remote == nil {
		fileError(w, http.StatusServiceUnavailable, "remote file provider is not available")
		return
	}
	data, err := s.remote.Read(r.Context(), nodeID, targetPath)
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot read %q on %s: %v", targetPath, nodeID, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	nodeID, targetPath, local := s.target(r)
	if local {
		s.face.handleDownload(w, r)
		return
	}
	if s.remote == nil {
		fileError(w, http.StatusServiceUnavailable, "remote file provider is not available")
		return
	}
	reader, err := s.remote.OpenRead(r.Context(), nodeID, targetPath)
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot open %q on %s: %v", targetPath, nodeID, err)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(filepath.ToSlash(targetPath))))
	_, _ = io.Copy(w, reader)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	nodeID, targetPath, local := s.target(r)
	if local {
		s.face.handleUpload(w, r)
		return
	}
	if s.remote == nil {
		fileError(w, http.StatusServiceUnavailable, "remote file provider is not available")
		return
	}
	written, err := s.remote.Upload(r.Context(), nodeID, targetPath, r.Body)
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot upload %q on %s: %v", targetPath, nodeID, err)
		return
	}
	writeFileJSON(w, map[string]any{"path": targetPath, "bytes": written})
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	nodeID, targetPath, local := s.target(r)
	if local {
		s.face.handleWrite(w, r)
		return
	}
	if s.remote == nil {
		fileError(w, http.StatusServiceUnavailable, "remote file provider is not available")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxWriteSize+1))
	if err != nil {
		fileError(w, http.StatusBadRequest, "cannot read body: %v", err)
		return
	}
	if len(data) > maxWriteSize {
		fileError(w, http.StatusBadRequest, "body exceeds %d bytes; use upload", maxWriteSize)
		return
	}
	if err := s.remote.Write(r.Context(), nodeID, targetPath, data); err != nil {
		fileError(w, http.StatusBadRequest, "cannot write %q on %s: %v", targetPath, nodeID, err)
		return
	}
	writeFileJSON(w, map[string]any{"path": targetPath, "bytes": len(data)})
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	nodeID, targetPath, local := s.target(r)
	if local {
		s.face.handleMkdir(w, r)
		return
	}
	if s.remote == nil {
		fileError(w, http.StatusServiceUnavailable, "remote file provider is not available")
		return
	}
	if err := s.remote.Mkdir(r.Context(), nodeID, targetPath); err != nil {
		fileError(w, http.StatusBadRequest, "cannot mkdir %q on %s: %v", targetPath, nodeID, err)
		return
	}
	writeFileJSON(w, map[string]any{"path": targetPath})
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	nodeID, targetPath, local := s.target(r)
	if local {
		s.face.handleRemove(w, r)
		return
	}
	if s.remote == nil {
		fileError(w, http.StatusServiceUnavailable, "remote file provider is not available")
		return
	}
	if err := s.remote.Remove(r.Context(), nodeID, targetPath); err != nil {
		fileError(w, http.StatusBadRequest, "cannot remove %q on %s: %v", targetPath, nodeID, err)
		return
	}
	writeFileJSON(w, map[string]any{"path": targetPath})
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	nodeID, _, local := s.target(r)
	query := r.URL.Query()
	from := query.Get("from")
	to := query.Get("to")
	if local {
		s.face.handleRename(w, r)
		return
	}
	if s.remote == nil {
		fileError(w, http.StatusServiceUnavailable, "remote file provider is not available")
		return
	}
	if err := s.remote.Rename(r.Context(), nodeID, from, to); err != nil {
		fileError(w, http.StatusBadRequest, "cannot rename %q -> %q on %s: %v", from, to, nodeID, err)
		return
	}
	writeFileJSON(w, map[string]any{"from": from, "to": to})
}

// entriesToJSON 把 model.FileEntry 转成与本机文件面一致的 JSON 形状。
func entriesToJSON(entries []model.FileEntry) []fileEntry {
	out := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entryToJSON(entry))
	}
	return out
}

func entryToJSON(entry model.FileEntry) fileEntry {
	return fileEntry{
		Name:        entry.Name,
		Path:        entry.Path,
		Size:        entry.Size,
		Mode:        entry.Mode,
		ModTimeUnix: entry.ModTime.Unix(),
		IsDir:       entry.IsDir,
	}
}
