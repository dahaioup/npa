package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/npa/npa/internal/audit"
	"github.com/npa/npa/internal/desktop"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/file"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/plugin"
	"github.com/npa/npa/internal/policy"
	sshx "github.com/npa/npa/internal/ssh"
	"github.com/npa/npa/internal/state"
	"github.com/npa/npa/internal/tunnel"
)

const Version = "0.2.0-phase2"

type Service struct {
	Store             *state.Store
	SSH               *sshx.Manager
	Files             *file.Service
	Tunnels           *tunnel.Manager
	Audit             *audit.Logger
	Desktop           *desktop.Router
	Plugin            *plugin.Manager
	resourceDir       string
	started           time.Time
	mu                sync.RWMutex
	limiter           map[string]*rateWindow
	OnSettingsChanged func()
}

type rateWindow struct {
	start time.Time
	count int
}

func New(store *state.Store) *Service {
	sshManager := sshx.NewManager(store)
	fileService := file.NewService(sshManager)
	tunnelManager := tunnel.NewManager(store, sshManager)
	resourceDir, _ := os.MkdirTemp("", "npa-resources-*")
	app := &Service{
		Store: store, SSH: sshManager, Files: fileService, Tunnels: tunnelManager,
		Audit: audit.New(store), started: time.Now(), limiter: map[string]*rateWindow{}, resourceDir: resourceDir,
	}
	app.Plugin = plugin.NewManager(filepath.Join("plugins", "desktop"))
	app.Desktop = desktop.NewRouterWithRoot(app.Plugin, resourceDir)
	tunnelManager.TargetValidator = app.validateTarget
	return app
}

func (s *Service) Close() {
	s.Tunnels.Close()
	_ = s.SSH.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.Plugin.Close(ctx)
	cancel()
	if s.resourceDir != "" {
		_ = os.RemoveAll(s.resourceDir)
	}
	_ = s.Store.Close()
}

func (s *Service) MCPCapabilities() map[string]bool {
	settings, _ := s.Store.Settings()
	return map[string]bool{
		"ssh": parseBool(settings["mcp_ssh"]), "file": parseBool(settings["mcp_file"]),
		"tunnel": parseBool(settings["mcp_tunnel"]), "screenshot": parseBool(settings["mcp_screenshot"]),
		"mouse": parseBool(settings["mcp_mouse"]),
	}
}

func parseBool(value string) bool { return strings.EqualFold(value, "true") || value == "1" }

func (s *Service) Settings() map[string]string {
	settings, _ := s.Store.Settings()
	return settings
}

func (s *Service) SaveSettings(values map[string]string) error {
	if err := s.Store.SaveSettings(values); err != nil {
		return err
	}
	if s.OnSettingsChanged != nil {
		s.OnSettingsChanged()
	}
	return nil
}

func (s *Service) Authorize(ctx context.Context, operation string, capability policy.Capability) error {
	if policy.SourceFrom(ctx) == policy.SourceMCP {
		if err := s.rateLimit(operation); err != nil {
			return err
		}
	}
	return policy.Check(ctx, s, capability)
}

func (s *Service) rateLimit(operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(policy.SourceMCP) + ":" + operation
	now := time.Now()
	window := s.limiter[key]
	if window == nil || now.Sub(window.start) >= time.Second {
		s.limiter[key] = &rateWindow{start: now, count: 1}
		return nil
	}
	if window.count >= 30 {
		return apperrors.New(apperrors.RateLimited, "MCP operation rate limit exceeded")
	}
	window.count++
	return nil
}

func localNode() model.Node {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	userName := os.Getenv("USER")
	if userName == "" {
		userName = os.Getenv("USERNAME")
	}
	if userName == "" {
		userName = "local"
	}
	return model.Node{
		ID: "local", Name: "本地节点", Address: host, Port: 0, Username: userName,
		AuthMethod: "local", Protocol: "local", Tags: []string{"local"},
		Status: "connected", CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Now().UTC(),
	}
}

func (s *Service) ListNodes() ([]model.Node, error) {
	nodes, err := s.Store.ListNodes()
	if err != nil {
		return nil, err
	}
	return append([]model.Node{localNode()}, nodes...), nil
}

func (s *Service) GetNode(id string) (model.Node, error) {
	if id == "local" {
		return localNode(), nil
	}
	return s.Store.GetNode(id)
}

func (s *Service) SaveNode(ctx context.Context, node model.Node, credential model.Credential) error {
	if err := s.Authorize(ctx, "node.write", policy.CapBase); err != nil {
		return err
	}
	if err := s.Store.SaveNode(node); err != nil {
		return err
	}
	credential.NodeID = node.ID
	if err := s.Store.SaveCredential(credential); err != nil {
		return err
	}
	s.Audit.Record(ctx, "node.write", node.ID, policy.CapBase, audit.SafeParameters(map[string]string{"name": node.Name, "address": node.Address}), time.Now(), nil)
	return nil
}

func (s *Service) DeleteNode(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "node.delete", policy.CapBase); err != nil {
		return err
	}
	err := s.Store.DeleteNode(id)
	if err == nil {
		s.Audit.Record(ctx, "node.delete", id, policy.CapBase, "", time.Now(), nil)
	}
	return err
}

func (s *Service) ConnectNode(ctx context.Context, id string) error {
	if id == "local" {
		return nil
	}
	if err := s.Authorize(ctx, "ssh.connect", policy.CapSSH); err != nil {
		return err
	}
	started := time.Now()
	_, err := s.SSH.Client(ctx, id)
	if err != nil {
		s.Audit.Record(ctx, "ssh.connect", id, policy.CapSSH, "", started, err)
		return err
	}
	s.Audit.Record(ctx, "ssh.connect", id, policy.CapSSH, "", started, nil)
	return nil
}

func (s *Service) DisconnectNode(ctx context.Context, id string) error {
	if id == "local" {
		return nil
	}
	if err := s.Authorize(ctx, "ssh.disconnect", policy.CapSSH); err != nil {
		return err
	}
	err := s.SSH.Close(id)
	s.Audit.Record(ctx, "ssh.disconnect", id, policy.CapSSH, "", time.Now(), err)
	return err
}

func (s *Service) Exec(ctx context.Context, nodeID, command string, timeout time.Duration) (model.ExecResult, error) {
	if err := s.Authorize(ctx, "ssh.exec", policy.CapSSH); err != nil {
		return model.ExecResult{}, err
	}
	if strings.TrimSpace(command) == "" {
		return model.ExecResult{}, apperrors.New(apperrors.InvalidArgument, "command is required")
	}
	started := time.Now()
	result, err := s.SSH.Exec(ctx, nodeID, sshx.ExecRequest{Command: command, Timeout: timeout})
	s.Audit.Record(ctx, "ssh.exec", nodeID, policy.CapSSH, audit.SafeParameters(map[string]string{"command": command}), started, err)
	return result, err
}

func (s *Service) OpenShell(ctx context.Context, nodeID string, cols, rows int) (*sshx.Shell, error) {
	if err := s.Authorize(ctx, "ssh.shell", policy.CapSSH); err != nil {
		return nil, err
	}
	shell, err := s.SSH.OpenShell(ctx, nodeID, cols, rows)
	s.Audit.Record(ctx, "ssh.shell", nodeID, policy.CapSSH, "", time.Now(), err)
	return shell, err
}

func (s *Service) Screens(ctx context.Context) ([]desktop.Screen, error) {
	if err := s.Authorize(ctx, "desktop.screens", policy.CapScreenshot); err != nil {
		return nil, err
	}
	started := time.Now()
	screens, err := s.Desktop.Screens(ctx)
	s.Audit.Record(ctx, "desktop.screens", "", policy.CapScreenshot, "", started, err)
	return screens, err
}

func (s *Service) Screenshot(ctx context.Context, request desktop.CaptureRequest) (desktop.CaptureResult, error) {
	if err := s.Authorize(ctx, "desktop.screenshot", policy.CapScreenshot); err != nil {
		return desktop.CaptureResult{}, err
	}
	started := time.Now()
	result, err := s.Desktop.Screenshot(ctx, request)
	s.Audit.Record(ctx, "desktop.screenshot", "", policy.CapScreenshot, audit.SafeParameters(map[string]string{"screen": request.ScreenID}), started, err)
	return result, err
}

func (s *Service) MoveMouse(ctx context.Context, request desktop.MoveRequest) error {
	if err := s.Authorize(ctx, "desktop.mouse_move", policy.CapMouse); err != nil {
		return err
	}
	started := time.Now()
	err := s.Desktop.MoveMouse(ctx, request)
	s.Audit.Record(ctx, "desktop.mouse_move", "", policy.CapMouse, audit.SafeParameters(map[string]string{"x": fmt.Sprintf("%d", request.X), "y": fmt.Sprintf("%d", request.Y)}), started, err)
	return err
}

func (s *Service) MouseButton(ctx context.Context, request desktop.ButtonRequest) error {
	if err := s.Authorize(ctx, "desktop.mouse_click", policy.CapMouse); err != nil {
		return err
	}
	started := time.Now()
	err := s.Desktop.MouseButton(ctx, request)
	s.Audit.Record(ctx, "desktop.mouse_click", "", policy.CapMouse, audit.SafeParameters(map[string]string{"button": request.Button, "action": request.Action}), started, err)
	return err
}

func (s *Service) Scroll(ctx context.Context, request desktop.ScrollRequest) error {
	if err := s.Authorize(ctx, "desktop.mouse_scroll", policy.CapMouse); err != nil {
		return err
	}
	started := time.Now()
	err := s.Desktop.Scroll(ctx, request)
	s.Audit.Record(ctx, "desktop.mouse_scroll", "", policy.CapMouse, audit.SafeParameters(map[string]string{"dx": fmt.Sprintf("%d", request.DeltaX), "dy": fmt.Sprintf("%d", request.DeltaY)}), started, err)
	return err
}

func (s *Service) FileList(ctx context.Context, nodeID, path string) ([]model.FileEntry, error) {
	if err := s.Authorize(ctx, "file.list", policy.CapFile); err != nil {
		return nil, err
	}
	started := time.Now()
	result, err := s.Files.List(ctx, nodeID, path)
	s.Audit.Record(ctx, "file.list", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return result, err
}

func (s *Service) FileRead(ctx context.Context, nodeID, path string) ([]byte, error) {
	if err := s.Authorize(ctx, "file.read", policy.CapFile); err != nil {
		return nil, err
	}
	started := time.Now()
	result, err := s.Files.Read(ctx, nodeID, path)
	s.Audit.Record(ctx, "file.read", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return result, err
}

func (s *Service) FileWrite(ctx context.Context, nodeID, path string, data []byte) error {
	if err := s.Authorize(ctx, "file.write", policy.CapFile); err != nil {
		return err
	}
	started := time.Now()
	err := s.Files.Write(ctx, nodeID, path, data)
	s.Audit.Record(ctx, "file.write", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return err
}

func (s *Service) FileRemove(ctx context.Context, nodeID, path string) error {
	if err := s.Authorize(ctx, "file.remove", policy.CapFile); err != nil {
		return err
	}
	started := time.Now()
	err := s.Files.Remove(ctx, nodeID, path)
	s.Audit.Record(ctx, "file.remove", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return err
}

func (s *Service) FileMkdir(ctx context.Context, nodeID, path string) error {
	if err := s.Authorize(ctx, "file.mkdir", policy.CapFile); err != nil {
		return err
	}
	started := time.Now()
	err := s.Files.Mkdir(ctx, nodeID, path)
	s.Audit.Record(ctx, "file.mkdir", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return err
}

func (s *Service) FileRename(ctx context.Context, nodeID, oldPath, newPath string) error {
	if err := s.Authorize(ctx, "file.rename", policy.CapFile); err != nil {
		return err
	}
	started := time.Now()
	err := s.Files.Rename(ctx, nodeID, oldPath, newPath)
	s.Audit.Record(ctx, "file.rename", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"old": oldPath, "new": newPath}), started, err)
	return err
}

func (s *Service) FileUpload(ctx context.Context, nodeID, path string, reader interface{ Read([]byte) (int, error) }) (int64, error) {
	if err := s.Authorize(ctx, "file.write", policy.CapFile); err != nil {
		return 0, err
	}
	started := time.Now()
	written, err := s.Files.Upload(ctx, nodeID, path, reader)
	s.Audit.Record(ctx, "file.upload", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
	return written, err
}

func (s *Service) FileDownload(ctx context.Context, nodeID, path string) (interface {
	Read([]byte) (int, error)
	Close() error
}, error) {
	if err := s.Authorize(ctx, "file.read", policy.CapFile); err != nil {
		return nil, err
	}
	started := time.Now()
	reader, err := s.Files.OpenRead(ctx, nodeID, path)
	if err != nil {
		s.Audit.Record(ctx, "file.download", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, err)
		return nil, err
	}
	s.Audit.Record(ctx, "file.download", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{"path": path}), started, nil)
	return reader, nil
}

func (s *Service) ListTunnels() ([]model.Tunnel, error) { return s.Tunnels.List() }

func (s *Service) SaveTunnel(ctx context.Context, definition model.Tunnel) error {
	if err := s.Authorize(ctx, "tunnel.write", policy.CapTunnel); err != nil {
		return err
	}
	if err := s.Store.SaveTunnel(definition); err != nil {
		return err
	}
	s.Audit.Record(ctx, "tunnel.write", definition.NodeID, policy.CapTunnel, audit.SafeParameters(map[string]string{"type": definition.Type, "listen": fmt.Sprintf("%s:%d", definition.ListenAddress, definition.ListenPort)}), time.Now(), nil)
	return nil
}

func (s *Service) StartTunnel(ctx context.Context, id string) (model.Tunnel, error) {
	if err := s.Authorize(ctx, "tunnel.open", policy.CapTunnel); err != nil {
		return model.Tunnel{}, err
	}
	definition, err := s.Store.GetTunnel(id)
	if err != nil {
		return definition, err
	}
	if definition.Type != "socks" {
		if err := s.validateTarget(definition.TargetHost, definition.TargetPort); err != nil {
			return definition, err
		}
	}
	started := time.Now()
	result, err := s.Tunnels.Start(ctx, id)
	s.Audit.Record(ctx, "tunnel.open", definition.NodeID, policy.CapTunnel, audit.SafeParameters(map[string]string{"type": definition.Type}), started, err)
	return result, err
}

func (s *Service) StopTunnel(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "tunnel.close", policy.CapTunnel); err != nil {
		return err
	}
	err := s.Tunnels.Stop(id)
	s.Audit.Record(ctx, "tunnel.close", id, policy.CapTunnel, "", time.Now(), err)
	return err
}

func (s *Service) DeleteTunnel(ctx context.Context, id string) error {
	if err := s.Authorize(ctx, "tunnel.delete", policy.CapTunnel); err != nil {
		return err
	}
	err := s.Tunnels.Delete(id)
	s.Audit.Record(ctx, "tunnel.delete", id, policy.CapTunnel, "", time.Now(), err)
	return err
}

func (s *Service) validateTarget(host string, port int) error {
	settings := s.Settings()
	if parseBool(settings["allow_public_targets"]) {
		return nil
	}
	if host == "localhost" || strings.HasSuffix(strings.ToLower(host), ".local") || strings.HasSuffix(strings.ToLower(host), ".lan") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		return nil
	}
	if ip == nil {
		addresses, lookupErr := net.LookupIP(host)
		if lookupErr != nil {
			return apperrors.New(apperrors.TargetForbidden, "cannot verify target address; public target is disabled")
		}
		for _, address := range addresses {
			if address.IsLoopback() || address.IsPrivate() {
				return nil
			}
		}
		return apperrors.New(apperrors.TargetForbidden, "public target is disabled by default")
	}
	return apperrors.New(apperrors.TargetForbidden, "public target is disabled by default")
}

func (s *Service) OpenWeb(ctx context.Context, nodeID, targetHost string, targetPort int) (string, error) {
	if err := s.Authorize(ctx, "service.open_web", policy.CapTunnel); err != nil {
		return "", err
	}
	if err := s.validateTarget(targetHost, targetPort); err != nil {
		return "", err
	}
	definition := model.Tunnel{
		ID: fmt.Sprintf("web-%d", time.Now().UnixNano()), NodeID: nodeID,
		Name: fmt.Sprintf("Web %s:%d", targetHost, targetPort), Type: "local",
		ListenAddress: "127.0.0.1", ListenPort: 0, TargetHost: targetHost, TargetPort: targetPort,
		AutoStart: false, Status: "stopped",
	}
	if err := s.Store.SaveTunnel(definition); err != nil {
		return "", err
	}
	started := time.Now()
	_, err := s.Tunnels.Start(ctx, definition.ID)
	s.Audit.Record(ctx, "service.open_web", nodeID, policy.CapTunnel, audit.SafeParameters(map[string]string{"target": fmt.Sprintf("%s:%d", targetHost, targetPort)}), started, err)
	if err != nil {
		return "", err
	}
	return "/proxy/" + definition.ID + "/", nil
}

func (s *Service) Overview() model.Overview {
	nodes, _ := s.Store.ListNodes()
	tunnels, _ := s.Tunnels.List()
	connected := 0
	for _, node := range nodes {
		if node.Status == "connected" {
			connected++
		}
	}
	running := 0
	for _, definition := range tunnels {
		if definition.Status == "running" {
			running++
		}
	}
	return model.Overview{
		Version: Version, UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Nodes: len(nodes), Connected: connected, Tunnels: len(tunnels), Running: running,
		MCPEnabled: parseBool(s.Settings()["mcp_enabled"]),
	}
}

func (s *Service) AuditEvents(limit, offset int) ([]model.AuditEvent, error) {
	return s.Audit.List(limit, offset)
}

func (s *Service) SetHostTrust(nodeID, fingerprint string) error {
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		return err
	}
	node.HostKeyFingerprint = fingerprint
	return s.Store.SaveNode(node)
}

func JSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
