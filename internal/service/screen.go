package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/npa/npa/internal/audit"
	"github.com/npa/npa/internal/desktop"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/policy"
)

// screenRequestTimeout 覆盖截图+OCR 的总预算（OCR 引擎首跑可能加载语言包较慢）。
const screenRequestTimeout = 60 * time.Second

// isSelfNode 目标是否本机。
func (s *Service) isSelfNode(nodeID string) bool {
	return nodeID == "" || nodeID == model.NodeID()
}

// screenTargetKind 分类目标：self（本机桌面）/ agent（NPA 节点，HTTP 文件面同源）/ ""（不支持，如纯 SSH 节点）。
func (s *Service) screenTargetKind(nodeID string) string {
	if s.isSelfNode(nodeID) {
		return "self"
	}
	if s.isPeerNode(nodeID) || s.meshBaseDirect(nodeID) != "" {
		return "agent"
	}
	return ""
}

// screenPost 经节点 mesh HTTP 调 /api/screen/*：先直连，失败经持有上级代理拨 127.0.0.1:56701。
func (s *Service) screenPost(ctx context.Context, nodeID, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return apperrors.Wrap(apperrors.InvalidArgument, "encode screen request", err)
	}
	if base := s.meshBaseDirect(nodeID); base != "" {
		if err := postScreenJSON(ctx, base+path, body, out); err == nil {
			return nil
		}
	}
	// 代理管道：协商成本高，keep-alive 复用（每节点缓存一条管道上的传输层）。
	transport := s.relayScreenTransport(nodeID)
	client := &http.Client{Transport: transport}
	return postWithClient(ctx, client, "http://mesh"+path, body, out)
}

// relayScreenTransport 返回该节点跨域代理管道上的 keep-alive 传输层（复用缓存）。
func (s *Service) relayScreenTransport(nodeID string) *http.Transport {
	s.screenTransMu.Lock()
	defer s.screenTransMu.Unlock()
	if t, ok := s.screenTransports[nodeID]; ok {
		return t
	}
	t := &http.Transport{
		MaxConnsPerHost:  2,
		MaxIdleConns:     2,
		IdleConnTimeout:  30 * time.Second,
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return s.dialNodeMesh(context.Background(), nodeID)
		},
	}
	s.screenTransports[nodeID] = t
	return t
}

func postScreenJSON(ctx context.Context, url string, body []byte, out any) error {
	client := &http.Client{Timeout: screenRequestTimeout}
	return postWithClient(ctx, client, url, body, out)
}

func postWithClient(ctx context.Context, client *http.Client, url string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("screen endpoint %s returned %d", url, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	// 远端（peer）接口的错误形态是 {"error": "..."}：直接解码到目标结构会「成功」
	// 但字段全空，调用方只看到空结果（静默失败，排查时毫无线索）。先显式识别它。
	var failure struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &failure); err == nil && strings.TrimSpace(failure.Error) != "" {
		return fmt.Errorf("remote node returned error: %s", failure.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// ReadScreen 读取节点屏幕文字：本机直接调桌面路由，NPA 节点经 mesh HTTP。
// 无论 OCR 成败都返回 status 与截图文件位置。
func (s *Service) ReadScreen(ctx context.Context, nodeID string, request desktop.ReadRequest) (desktop.ReadResult, error) {
	if err := s.Authorize(ctx, "screen.read", policy.CapScreenshot); err != nil {
		return desktop.ReadResult{}, err
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, screenRequestTimeout)
	defer cancel()
	var (
		result desktop.ReadResult
		err    error
	)
	switch s.screenTargetKind(nodeID) {
	case "self":
		result, err = s.Desktop.ReadScreen(ctx, request)
	case "agent":
		err = s.screenPost(ctx, nodeID, "/api/screen/read", request, &result)
	default:
		err = apperrors.New(apperrors.NotSupported, "screen read requires an NPA node (ssh nodes have no agent)")
	}
	s.Audit.Record(ctx, "screen.read", nodeID, policy.CapScreenshot, audit.SafeParameters(map[string]string{
		"lang": request.Lang, "region": request.Region.String(),
	}), started, err)
	return result, err
}

// TypeText 向节点键盘输入文字（脚本原生：SendInput/keystroke/xdotool）。
func (s *Service) TypeText(ctx context.Context, nodeID, text string) error {
	if err := s.Authorize(ctx, "screen.type", policy.CapMouse); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return apperrors.New(apperrors.InvalidArgument, "text is required")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, screenRequestTimeout)
	defer cancel()
	request := desktop.TypeRequest{Text: text}
	var err error
	switch s.screenTargetKind(nodeID) {
	case "self":
		err = s.Desktop.TypeText(ctx, request)
	case "agent":
		var out map[string]string
		err = s.screenPost(ctx, nodeID, "/api/screen/type", request, &out)
	default:
		err = apperrors.New(apperrors.NotSupported, "screen type requires an NPA node (ssh nodes have no agent)")
	}
	s.Audit.Record(ctx, "screen.type", nodeID, policy.CapMouse, audit.SafeParameters(map[string]string{
		"chars": fmt.Sprintf("%d", len([]rune(text))),
	}), started, err)
	return err
}

// ClickScreen 把鼠标移到坐标并单击（用于配合 screen.read 返回的文字坐标）。
func (s *Service) ClickScreen(ctx context.Context, nodeID string, x, y int, button string) error {
	if err := s.Authorize(ctx, "screen.click", policy.CapMouse); err != nil {
		return err
	}
	if x < 0 || y < 0 || x > 100000 || y > 100000 {
		return apperrors.New(apperrors.InvalidArgument, "coordinates out of bounds")
	}
	if button == "" {
		button = "left"
	}
	switch button {
	case "left", "right", "middle":
	default:
		return apperrors.New(apperrors.InvalidArgument, "button must be left/right/middle")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, screenRequestTimeout)
	defer cancel()
	payload := map[string]any{"x": x, "y": y, "button": button}
	var err error
	switch s.screenTargetKind(nodeID) {
	case "self":
		if err = s.Desktop.MoveMouse(ctx, desktop.MoveRequest{X: x, Y: y}); err == nil {
			err = s.Desktop.MouseButton(ctx, desktop.ButtonRequest{Button: button, Action: "click"})
		}
	case "agent":
		var out map[string]string
		err = s.screenPost(ctx, nodeID, "/api/screen/click", payload, &out)
	default:
		err = apperrors.New(apperrors.NotSupported, "screen click requires an NPA node (ssh nodes have no agent)")
	}
	s.Audit.Record(ctx, "screen.click", nodeID, policy.CapMouse, audit.SafeParameters(map[string]string{
		"x": fmt.Sprintf("%d", x), "y": fmt.Sprintf("%d", y), "button": button,
	}), started, err)
	return err
}

// HotkeyScreen 向节点发送组合键（ctrl+v、ctrl+shift+esc 等），配合剪贴板实现粘贴。
func (s *Service) HotkeyScreen(ctx context.Context, nodeID, combo string) error {
	if err := s.Authorize(ctx, "screen.hotkey", policy.CapMouse); err != nil {
		return err
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, screenRequestTimeout)
	defer cancel()
	request := desktop.HotkeyRequest{Combo: combo}
	var err error
	switch s.screenTargetKind(nodeID) {
	case "self":
		err = s.Desktop.Hotkey(ctx, request)
	case "agent":
		var out map[string]string
		err = s.screenPost(ctx, nodeID, "/api/screen/hotkey", request, &out)
	default:
		err = apperrors.New(apperrors.NotSupported, "screen hotkey requires an NPA node (ssh nodes have no agent)")
	}
	s.Audit.Record(ctx, "screen.hotkey", nodeID, policy.CapMouse, audit.SafeParameters(map[string]string{
		"combo": combo,
	}), started, err)
	return err
}

// ScreenInfo 查询节点屏幕基础信息（鼠标位置/分辨率/前台窗口矩形）。
func (s *Service) ScreenInfo(ctx context.Context, nodeID string) (desktop.InfoResult, error) {
	if err := s.Authorize(ctx, "screen.info", policy.CapScreenshot); err != nil {
		return desktop.InfoResult{}, err
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, screenRequestTimeout)
	defer cancel()
	var (
		result desktop.InfoResult
		err    error
	)
	switch s.screenTargetKind(nodeID) {
	case "self":
		result, err = s.Desktop.Info(ctx, desktop.InfoRequest{})
	case "agent":
		err = s.screenPost(ctx, nodeID, "/api/screen/info", desktop.InfoRequest{}, &result)
	default:
		err = apperrors.New(apperrors.NotSupported, "screen info requires an NPA node (ssh nodes have no agent)")
	}
	s.Audit.Record(ctx, "screen.info", nodeID, policy.CapScreenshot, audit.SafeParameters(nil), started, err)
	return result, err
}

// MoveScreen 移动节点鼠标到指定坐标（不点击）。
func (s *Service) MoveScreen(ctx context.Context, nodeID string, x, y int, relative bool) error {
	if err := s.Authorize(ctx, "screen.move", policy.CapMouse); err != nil {
		return err
	}
	if x < -100000 || x > 100000 || y < -100000 || y > 100000 {
		return apperrors.New(apperrors.InvalidArgument, "coordinates out of bounds")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, screenRequestTimeout)
	defer cancel()
	request := desktop.MoveRequest{X: x, Y: y, Relative: relative}
	var err error
	switch s.screenTargetKind(nodeID) {
	case "self":
		err = s.Desktop.MoveMouse(ctx, request)
	case "agent":
		var out map[string]string
		err = s.screenPost(ctx, nodeID, "/api/screen/move", request, &out)
	default:
		err = apperrors.New(apperrors.NotSupported, "screen move requires an NPA node (ssh nodes have no agent)")
	}
	s.Audit.Record(ctx, "screen.move", nodeID, policy.CapMouse, audit.SafeParameters(map[string]string{
		"x": fmt.Sprintf("%d", x), "y": fmt.Sprintf("%d", y), "relative": fmt.Sprintf("%t", relative),
	}), started, err)
	return err
}

// ClipboardGetScreen 读取节点剪贴板文本（需 desktop 插件）。
func (s *Service) ClipboardGetScreen(ctx context.Context, nodeID string) (desktop.ClipboardResult, error) {
	if err := s.Authorize(ctx, "screen.clipboard", policy.CapMouse); err != nil {
		return desktop.ClipboardResult{}, err
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, screenRequestTimeout)
	defer cancel()
	var (
		result desktop.ClipboardResult
		err    error
	)
	switch s.screenTargetKind(nodeID) {
	case "self":
		result, err = s.Desktop.ClipboardGet(ctx)
	case "agent":
		err = s.screenPost(ctx, nodeID, "/api/screen/clipboard/get", desktop.ClipboardResult{}, &result)
	default:
		err = apperrors.New(apperrors.NotSupported, "clipboard requires an NPA node with the desktop plugin")
	}
	s.Audit.Record(ctx, "screen.clipboard_get", nodeID, policy.CapMouse, audit.SafeParameters(nil), started, err)
	return result, err
}

// ClipboardSetScreen 写入节点剪贴板文本（需 desktop 插件）。
func (s *Service) ClipboardSetScreen(ctx context.Context, nodeID, text string) error {
	if err := s.Authorize(ctx, "screen.clipboard", policy.CapMouse); err != nil {
		return err
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, screenRequestTimeout)
	defer cancel()
	var err error
	switch s.screenTargetKind(nodeID) {
	case "self":
		err = s.Desktop.ClipboardSet(ctx, text)
	case "agent":
		var out map[string]string
		err = s.screenPost(ctx, nodeID, "/api/screen/clipboard/set", desktop.ClipboardResult{Text: text}, &out)
	default:
		err = apperrors.New(apperrors.NotSupported, "clipboard requires an NPA node with the desktop plugin")
	}
	s.Audit.Record(ctx, "screen.clipboard_set", nodeID, policy.CapMouse, audit.SafeParameters(map[string]string{
		"bytes": fmt.Sprintf("%d", len(text)),
	}), started, err)
	return err
}

// GetNodeResources 查询远程节点的 CPU/内存/磁盘占用。
// 直连优先（meshBaseDirect），失败走跨域代理管道（relayScreenTransport）。
func (s *Service) GetNodeResources(ctx context.Context, nodeID string) (map[string]any, error) {
	// 本机直接返回：self 不走 mesh/relay（前端点拓扑树上的自己也走这里）
	if s.isSelfNode(nodeID) {
		return s.NodeResources(), nil
	}
	out := map[string]any{}
	base := s.meshBaseDirect(nodeID)
	if base != "" {
		// 有直连地址但查询失败（对端版本旧无 /api/node/resources）：返回明确错误
		if err := getScreenJSON(ctx, base+"/api/node/resources", &out); err != nil {
			return nil, fmt.Errorf("query node resources: %w", err)
		}
		return out, nil
	}
	// meshBaseDirect 返回空（跨域节点如 192）：走代理管道
	transport := s.relayScreenTransport(nodeID)
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://mesh/api/node/resources", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, apperrors.New(apperrors.NodeOffline, "resources endpoint returned "+strconv.Itoa(resp.StatusCode))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func getScreenJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: screenRequestTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return apperrors.New(apperrors.NodeOffline, "resources endpoint returned "+strconv.Itoa(resp.StatusCode))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
