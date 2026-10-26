package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"runtime/debug"
	"time"

	"github.com/gorilla/websocket"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/peer"
	"github.com/npa/npa/internal/policy"
	"github.com/npa/npa/internal/service"
)

//go:embed static/*
var static embed.FS

var staticFS fs.FS

func init() {
	var err error
	staticFS, err = fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
}

type MCPStatus interface{ Status() map[string]any }

type Server struct {
	app      *service.Service
	mcp      MCPStatus
	upgrader websocket.Upgrader
	// ShutdownFunc 由 main 注入：Web「退出服务」按钮触发与 Ctrl+C 相同的优雅关闭。
	ShutdownFunc func()
}

func New(app *service.Service, mcp MCPStatus) *Server {
	return &Server{app: app, mcp: mcp, upgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		parsed, err := url.Parse(origin)
		return err == nil && parsed.Host == r.Host
	}}}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	code := apperrors.CodeOf(err)
	status := http.StatusInternalServerError
	switch code {
	case apperrors.InvalidArgument, apperrors.Authentication, apperrors.TargetForbidden, apperrors.NotSupported:
		status = http.StatusBadRequest
	case apperrors.PermissionDenied:
		status = http.StatusForbidden
	case apperrors.RateLimited:
		status = http.StatusTooManyRequests
	case apperrors.Timeout:
		status = http.StatusGatewayTimeout
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": string(code), "message": apperrors.MessageOf(err)}})
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return apperrors.Wrap(apperrors.InvalidArgument, "invalid JSON request", err)
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.app.Overview()) })
	mux.HandleFunc("GET /api/system", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.app.SystemInfo()) })
	mux.HandleFunc("GET /api/nodes", s.handleNodes)
	mux.HandleFunc("POST /api/nodes", s.handleNodeSave)
	mux.HandleFunc("DELETE /api/nodes/{id}", s.handleNodeDelete)
	mux.HandleFunc("POST /api/nodes/{id}/connect", s.handleNodeConnect)
	mux.HandleFunc("POST /api/nodes/{id}/disconnect", s.handleNodeDisconnect)
	mux.HandleFunc("POST /api/nodes/{id}/exec", s.handleExec)
	mux.HandleFunc("GET /api/nodes/{id}/files", s.handleFileList)
	mux.HandleFunc("POST /api/nodes/{id}/files/read", s.handleFileRead)
	mux.HandleFunc("POST /api/nodes/{id}/files/write", s.handleFileWrite)
	mux.HandleFunc("POST /api/nodes/{id}/files/remove", s.handleFileRemove)
	mux.HandleFunc("POST /api/nodes/{id}/files/mkdir", s.handleFileMkdir)
	mux.HandleFunc("POST /api/nodes/{id}/files/rename", s.handleFileRename)
	mux.HandleFunc("POST /api/nodes/{id}/files/upload", s.handleFileUpload)
	mux.HandleFunc("GET /api/nodes/{id}/files/download", s.handleFileDownload)
	mux.HandleFunc("GET /api/tunnels", s.handleTunnels)
	mux.HandleFunc("POST /api/tunnels", s.handleTunnelSave)
	mux.HandleFunc("POST /api/tunnels/{id}/start", s.handleTunnelStart)
	mux.HandleFunc("POST /api/tunnels/{id}/stop", s.handleTunnelStop)
	mux.HandleFunc("DELETE /api/tunnels/{id}", s.handleTunnelDelete)
	mux.HandleFunc("GET /api/portmappings", s.handlePortMappings)
	mux.HandleFunc("POST /api/portmappings", s.handlePortMappingSave)
	mux.HandleFunc("POST /api/portmappings/{id}/start", s.handlePortMappingStart)
	mux.HandleFunc("POST /api/portmappings/{id}/stop", s.handlePortMappingStop)
	mux.HandleFunc("DELETE /api/portmappings/{id}", s.handlePortMappingDelete)
	mux.HandleFunc("POST /api/webproxy/open", s.handleOpenWeb)
	mux.HandleFunc("GET /api/settings", s.handleSettings)
	mux.HandleFunc("PUT /api/settings", s.handleSettingsSave)
	mux.HandleFunc("POST /api/shutdown", s.handleShutdown)
	mux.HandleFunc("POST /api/update/check", s.handleUpdateCheck)
	mux.HandleFunc("POST /api/update/apply", s.handleUpdateApply)
	mux.HandleFunc("POST /api/update/push", s.handleUpdatePush)
	mux.HandleFunc("GET /api/mcp", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.mcp.Status()) })
	mux.HandleFunc("GET /api/discovery", s.handleDiscoveryNodes)
	mux.HandleFunc("POST /api/discovery/refresh", s.handleDiscoveryRefresh)
	mux.HandleFunc("GET /api/network/subnets", s.handleNetworkSubnets)
	mux.HandleFunc("GET /api/relay/routes", s.handleRelayRoutes)
	mux.HandleFunc("POST /api/relay/open", s.handleRelayOpen)
	s.peerRoutes(mux)
	mux.HandleFunc("GET /api/audit", s.handleAudit)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /proxy/{id}/", s.handleProxy)
	mux.HandleFunc("GET /api/nodes/{id}/pty", s.handlePTY)
	mux.HandleFunc("/", s.handleStatic)
	return securityHeaders(RecoverMiddleware(mux))
}

// peerRoutes 注册反向接入相关路由；同时挂到回环 Web 与独立的 peer 端口上。
func (s *Server) peerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/info", s.app.Peer.HandleInfo)
	mux.HandleFunc("POST /api/peers/register", s.handlePeerRegister)
	mux.HandleFunc("GET /api/peers/link", s.app.Peer.HandleLink)
	mux.HandleFunc("GET /api/peers/conn", s.app.Peer.HandleConn)
	mux.HandleFunc("GET /api/peers", s.handlePeerStatus)
	mux.HandleFunc("PUT /api/peers", s.handlePeerSettings)
	mux.HandleFunc("POST /api/peers/discover", s.handlePeerDiscover)
	mux.HandleFunc("GET /api/topology", s.handleTopology)
	mux.HandleFunc("POST /api/peers/require", s.app.Peer.HandleRequire)
	mux.HandleFunc("POST /api/peers/require_relay", s.app.Peer.HandleRequireRelay)
	mux.HandleFunc("GET /api/peers/relay", s.app.Peer.HandleRelay)
	peer.NewFileAPI().Register(mux)
}

// RecoverMiddleware 兜底任意 handler 的 panic，避免单请求打崩整个进程。
func RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("http handler panic", "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				http.Error(w, "internal panic", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// PeerHandler 是 peer 端口专用的处理器：只暴露反向接入入口，不含 Web 管理面。
func (s *Server) PeerHandler() http.Handler {
	mux := http.NewServeMux()
	s.peerRoutes(mux)
	return securityHeaders(RecoverMiddleware(mux))
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handlePeerRegister(w http.ResponseWriter, r *http.Request) {
	var registration peer.Registration
	if err := decodeJSON(r, &registration); err != nil {
		writeError(w, err)
		return
	}
	node, err := s.app.RegisterPeer(policy.WithSource(r.Context(), policy.SourceWeb), registration, remoteHost(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"registered": true, "id": node.ID})
}

// handleTopology 返回完整树状拓扑（本机 + 下级 + 上级链 + 其它上级的下级）。
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	payload, err := s.app.Topology(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, payload)
}

func (s *Server) handlePeerStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.app.PeerStatus())
}

func (s *Server) handlePeerSettings(w http.ResponseWriter, r *http.Request) {
	var values map[string]string
	if err := decodeJSON(r, &values); err != nil {
		writeError(w, err)
		return
	}
	for key, value := range values {
		if !strings.HasPrefix(key, "peer_") {
			writeError(w, apperrors.New(apperrors.InvalidArgument, "仅允许修改 peer_* 设置: "+key))
			return
		}
		if key == "peer_port" {
			// 节点通信端口已固定为 56701（与 MCP 共用），设置项废弃
			writeError(w, apperrors.New(apperrors.InvalidArgument,
				"节点通信端口已固定为 56701（与 MCP 服务共用），peer_port 设置已废弃"))
			return
		}
		switch key {
		case "peer_probe_subnets":
			if _, err := peer.ParseProbeSubnets(value); err != nil {
				writeError(w, apperrors.Wrap(apperrors.InvalidArgument, "invalid probe subnets", err))
				return
			}
		case "peer_pool_size":
			size, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || size < 1 || size > 32 {
				writeError(w, apperrors.New(apperrors.InvalidArgument, "peer_pool_size 必须在 1-32 之间"))
				return
			}
		}
	}
	if err := s.app.SaveSettings(values); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, s.app.PeerStatus())
}

func (s *Server) handlePeerDiscover(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RefreshPeer(policy.WithSource(r.Context(), policy.SourceWeb)); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"refreshed": true})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws://127.0.0.1:56700; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.ServeFileFS(w, r, staticFS, "index.html")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/static/") {
		http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))).ServeHTTP(w, r)
		return
	}
	http.ServeFileFS(w, r, staticFS, "index.html")
}

func (s *Server) handleNodes(w http.ResponseWriter, _ *http.Request) {
	nodes, err := s.app.ListNodes()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, nodes)
}

type nodeRequest struct {
	Node       model.Node       `json:"node"`
	Credential model.Credential `json:"credential"`
}

func (s *Server) handleNodeSave(w http.ResponseWriter, r *http.Request) {
	var request nodeRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	if request.Node.ID == "" {
		request.Node.ID = fmt.Sprintf("node-%d", time.Now().UnixNano())
	}
	if request.Credential.NodeID == "" {
		request.Credential.NodeID = request.Node.ID
	}
	if old, err := s.app.GetNode(request.Node.ID); err == nil && request.Credential.Password == "" && request.Credential.PrivateKey == "" {
		previous, _ := s.app.Store.GetCredential(old.ID)
		request.Credential, request.Credential.NodeID = previous, old.ID
	}
	if err := s.app.SaveNode(r.Context(), request.Node, request.Credential); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, request.Node)
}

func (s *Server) handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteNode(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}

func (s *Server) handleNodeConnect(w http.ResponseWriter, r *http.Request) {
	if err := s.app.ConnectNode(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"connected": true})
}

func (s *Server) handleNodeDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DisconnectNode(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"disconnected": true})
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	timeout := time.Duration(request.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	result, err := s.app.Exec(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"), request.Command, timeout)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	entries, err := s.app.FileList(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, entries)
}

func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	data, err := s.app.FileRead(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"), request.Path)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"path": request.Path, "content": string(data)})
}

func (s *Server) handleFileWrite(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	if err := s.app.FileWrite(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"), request.Path, []byte(request.Content)); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"saved": true})
}

func (s *Server) handleFileRemove(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	if err := s.app.FileRemove(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"), request.Path); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"removed": true})
}

func (s *Server) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	if err := s.app.FileMkdir(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"), request.Path); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"created": true})
}

func (s *Server) handleFileRename(w http.ResponseWriter, r *http.Request) {
	var request struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	if err := s.app.FileRename(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"), request.OldPath, request.NewPath); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"renamed": true})
}

func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(128 << 20); err != nil {
		writeError(w, apperrors.Wrap(apperrors.InvalidArgument, "invalid multipart upload", err))
		return
	}
	path := r.FormValue("path")
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, apperrors.Wrap(apperrors.InvalidArgument, "file field is required", err))
		return
	}
	defer file.Close()
	written, err := s.app.FileUpload(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"), path, file)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"path": path, "name": header.Filename, "bytes": written})
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	reader, err := s.app.FileDownload(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"), path)
	if err != nil {
		writeError(w, err)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", pathBase(path)))
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, reader)
}

func pathBase(value string) string {
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[index+1:]
	}
	return value
}

func (s *Server) handleTunnels(w http.ResponseWriter, _ *http.Request) {
	tunnels, err := s.app.ListTunnels()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, tunnels)
}

func (s *Server) handleTunnelSave(w http.ResponseWriter, r *http.Request) {
	var definition model.Tunnel
	if err := decodeJSON(r, &definition); err != nil {
		writeError(w, err)
		return
	}
	if definition.ID == "" {
		definition.ID = fmt.Sprintf("tunnel-%d", time.Now().UnixNano())
	}
	if err := s.app.SaveTunnel(policy.WithSource(r.Context(), policy.SourceWeb), definition); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, definition)
}

func (s *Server) handleTunnelStart(w http.ResponseWriter, r *http.Request) {
	definition, err := s.app.StartTunnel(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	definition.ListenPort = s.app.Tunnels.RuntimePort(definition.ID)
	writeJSON(w, 200, definition)
}

func (s *Server) handleTunnelStop(w http.ResponseWriter, r *http.Request) {
	if err := s.app.StopTunnel(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"stopped": true})
}

func (s *Server) handleTunnelDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteTunnel(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}

func (s *Server) handleOpenWeb(w http.ResponseWriter, r *http.Request) {
	var request struct {
		NodeID     string `json:"node_id"`
		TargetHost string `json:"target_host"`
		TargetPort int    `json:"target_port"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	path, err := s.app.OpenWeb(policy.WithSource(r.Context(), policy.SourceWeb), request.NodeID, request.TargetHost, request.TargetPort)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"path": path, "url": "http://127.0.0.1:56700" + path})
}

func (s *Server) handlePortMappings(w http.ResponseWriter, _ *http.Request) {
	mappings, err := s.app.ListPortMappings()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, mappings)
}

func (s *Server) handlePortMappingSave(w http.ResponseWriter, r *http.Request) {
	var definition model.PortMapping
	if err := decodeJSON(r, &definition); err != nil {
		writeError(w, err)
		return
	}
	if definition.ID == "" {
		definition.ID = fmt.Sprintf("map-%d", time.Now().UnixNano())
	}
	if err := s.app.SavePortMapping(policy.WithSource(r.Context(), policy.SourceWeb), definition); err != nil {
		writeError(w, err)
		return
	}
	saved, _ := s.app.Store.GetPortMapping(definition.ID)
	writeJSON(w, 200, saved)
}

func (s *Server) handlePortMappingStart(w http.ResponseWriter, r *http.Request) {
	definition, err := s.app.StartPortMapping(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, definition)
}

func (s *Server) handlePortMappingStop(w http.ResponseWriter, r *http.Request) {
	if err := s.app.StopPortMapping(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"stopped": true})
}

func (s *Server) handlePortMappingDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeletePortMapping(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}

// handleUpdateCheck 找出版本比本机新的节点及其上的可用产物。
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	result, err := s.app.UpdateCheck(policy.WithSource(r.Context(), policy.SourceWeb))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

// handleUpdateApply 先把下载与校验结果发出去，再异步替换二进制并重启：
// Unix 上替换用 exec 直接接管当前进程（不返回），Windows 上拉起新进程后旧进程优雅退出。
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	var request struct {
		NodeID   string `json:"node_id"`
		Artifact string `json:"artifact"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	result, err := s.app.UpdatePrepare(policy.WithSource(r.Context(), policy.SourceWeb), request.NodeID, request.Artifact)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, result)
	binaryPath, _ := result["binary_path"].(string)
	go func() {
		time.Sleep(300 * time.Millisecond) // 让响应先发出去
		if err := s.app.UpdateApplyBinary(binaryPath); err != nil {
			slog.Error("apply update failed", "error", err)
			return
		}
		if s.ShutdownFunc != nil {
			s.ShutdownFunc()
		}
	}()
}

// handleUpdatePush 把本机的新版本推给节点：默认只上传，restart=true 时再让节点自我替换重启。
// 替换会中断节点与本机的连接，因此上传先回响应，替换放到异步执行。
func (s *Server) handleUpdatePush(w http.ResponseWriter, r *http.Request) {
	var request struct {
		NodeID    string `json:"node_id"`
		RemoteDir string `json:"remote_dir"`
		Restart   bool   `json:"restart"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	if strings.TrimSpace(request.NodeID) == "" {
		writeError(w, apperrors.New(apperrors.InvalidArgument, "必须指定目标节点"))
		return
	}
	report, err := s.app.PushPackage(policy.WithSource(r.Context(), policy.SourceWeb), request.NodeID, request.RemoteDir)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, report)
	if !request.Restart {
		return
	}
	nodeID := request.NodeID
	go func() {
		time.Sleep(300 * time.Millisecond) // 先让响应发出去
		if _, err := s.app.PushRestart(context.Background(), nodeID, report); err != nil {
			slog.Error("push restart failed", "node", nodeID, "error", err)
		}
	}()
}

func (s *Server) handleSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.app.Settings())
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	var values map[string]string
	if err := decodeJSON(r, &values); err != nil {
		writeError(w, err)
		return
	}
	if err := s.app.SaveSettings(values); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, s.app.Settings())
}

// handleShutdown 让 Web 能正常退出服务，避免只能任务管理器强杀：
// 先把响应发出去，再异步走与 Ctrl+C 完全相同的优雅关闭路径。
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if s.ShutdownFunc == nil {
		writeError(w, apperrors.New(apperrors.NotSupported, "当前实例不支持从 Web 退出"))
		return
	}
	s.app.Audit.Record(policy.WithSource(r.Context(), policy.SourceWeb), "service.shutdown", "",
		policy.CapBase, "", time.Now(), nil)
	writeJSON(w, 200, map[string]any{"stopping": true})
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.ShutdownFunc()
	}()
}

func (s *Server) handleDiscoveryNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.app.DiscoveryNodes(policy.WithSource(r.Context(), policy.SourceWeb))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, nodes)
}

func (s *Server) handleDiscoveryRefresh(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DiscoveryRefresh(policy.WithSource(r.Context(), policy.SourceWeb)); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"refreshed": true})
}

func (s *Server) handleNetworkSubnets(w http.ResponseWriter, r *http.Request) {
	subnets, err := s.app.NetworkSubnets(policy.WithSource(r.Context(), policy.SourceWeb))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, subnets)
}

func (s *Server) handleRelayRoutes(w http.ResponseWriter, r *http.Request) {
	routes, err := s.app.RelayRoutes(policy.WithSource(r.Context(), policy.SourceWeb))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, routes)
}

func (s *Server) handleRelayOpen(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Target           string `json:"target"`
		TargetCapability string `json:"target_capability"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	capability := policy.CapTunnel
	switch strings.ToLower(request.TargetCapability) {
	case "", "tunnel":
		capability = policy.CapTunnel
	case "ssh":
		capability = policy.CapSSH
	case "file":
		capability = policy.CapFile
	default:
		writeError(w, apperrors.New(apperrors.InvalidArgument, "unsupported target capability"))
		return
	}
	route, err := s.app.RelayOpen(policy.WithSource(r.Context(), policy.SourceWeb), request.Target, capability)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, route)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	events, err := s.app.AuditEvents(limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, events)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, apperrors.New(apperrors.NotSupported, "SSE is not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			raw, _ := json.Marshal(s.app.Overview())
			_, _ = fmt.Fprintf(w, "event: overview\ndata: %s\n\n", raw)
			flusher.Flush()
		}
	}
}

func (s *Server) handlePTY(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	shell, err := s.app.OpenShell(policy.WithSource(r.Context(), policy.SourceWeb), r.PathValue("id"), 120, 34)
	if err != nil {
		_ = conn.WriteJSON(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	defer shell.Close()
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		buffer := make([]byte, 4096)
		for {
			n, err := shell.Stdout.Read(buffer)
			if n > 0 {
				if writeErr := conn.WriteMessage(websocket.TextMessage, buffer[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if _, err := shell.Stdin.Write(data); err != nil {
			return
		}
		select {
		case <-outputDone:
			return
		default:
		}
	}
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	definition, err := s.app.Store.GetTunnel(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if definition.Type == "remote" || definition.Type == "socks" {
		writeError(w, apperrors.New(apperrors.NotSupported, "proxy route requires a local tunnel definition"))
		return
	}
	if err := s.app.Authorize(policy.WithSource(r.Context(), policy.SourceWeb), "proxy", policy.CapTunnel); err != nil {
		writeError(w, err)
		return
	}
	target := fmt.Sprintf("%s:%d", definition.TargetHost, definition.TargetPort)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return s.app.DialNode(ctx, definition.NodeID, target)
		},
		ForceAttemptHTTP2: false,
	}
	proxy := &httputil.ReverseProxy{Transport: transport, Director: func(req *http.Request) {
		req.URL.Scheme, req.URL.Host, req.Host = "http", definition.TargetHost, definition.TargetHost
		prefix := "/proxy/" + id + "/"
		if strings.HasPrefix(req.URL.Path, prefix) {
			req.URL.Path = "/" + strings.TrimPrefix(req.URL.Path, prefix)
		}
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) { writeError(w, err) }}
	proxy.ServeHTTP(w, r)
}

func ListenAndServe(address string, handler http.Handler) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	return server.Serve(listener)
}
