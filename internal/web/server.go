package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
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
	mux.HandleFunc("POST /api/webproxy/open", s.handleOpenWeb)
	mux.HandleFunc("GET /api/settings", s.handleSettings)
	mux.HandleFunc("PUT /api/settings", s.handleSettingsSave)
	mux.HandleFunc("GET /api/mcp", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.mcp.Status()) })
	mux.HandleFunc("GET /api/discovery", s.handleDiscoveryNodes)
	mux.HandleFunc("POST /api/discovery/refresh", s.handleDiscoveryRefresh)
	mux.HandleFunc("GET /api/network/subnets", s.handleNetworkSubnets)
	mux.HandleFunc("GET /api/relay/routes", s.handleRelayRoutes)
	mux.HandleFunc("POST /api/relay/open", s.handleRelayOpen)
	mux.HandleFunc("GET /api/audit", s.handleAudit)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /proxy/{id}/", s.handleProxy)
	mux.HandleFunc("GET /api/nodes/{id}/pty", s.handlePTY)
	mux.HandleFunc("/", s.handleStatic)
	return securityHeaders(mux)
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
			return s.app.Tunnels.Dial(ctx, definition.NodeID, target)
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
