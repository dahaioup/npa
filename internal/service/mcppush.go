package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/selfupdate"

	"github.com/npa/npa/internal/discovery"
)

// MCP 自升级通道：对没有反向链路的局域网发现节点，改走它自己的 MCP
// （地址:56701/mcp，节点间通信收敛后 MCP 与注册是同一个端口）让它在
// 本机执行"下载新版本 + 自我替换重启"。前提只有一条：本机 56701 对它可达。

// downloadToken 是一次性下载凭据：一个 token 只能换一个本地文件，
// 用后即焚，5 分钟未用自动过期。
type downloadToken struct {
	path      string
	label     string
	expiresAt time.Time
}

const downloadTokenTTL = 5 * time.Minute

// errMCPUnavailable 表示目标节点的 MCP 通道不存在（不是 NPA 或不可达）。
var errMCPUnavailable = errors.New("mcp-unavailable")

func (s *Service) newDownloadToken(path, label string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buffer)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.downloadTokens == nil {
		s.downloadTokens = map[string]downloadToken{}
	}
	// 顺手清过期
	now := time.Now()
	for key, entry := range s.downloadTokens {
		if now.After(entry.expiresAt) {
			delete(s.downloadTokens, key)
		}
	}
	s.downloadTokens[token] = downloadToken{path: path, label: label, expiresAt: now.Add(downloadTokenTTL)}
	return token, nil
}

// TakeDownloadToken 换取文件并销毁 token（由 web 层的下载端点调用）。
func (s *Service) TakeDownloadToken(token string) (string, bool) {
	s.mu.Lock()
	entry, ok := s.downloadTokens[token]
	if ok {
		delete(s.downloadTokens, token)
	}
	s.mu.Unlock()
	if !ok || time.Now().After(entry.expiresAt) {
		return "", false
	}
	return entry.path, true
}

// ServeDownload 是 mesh 端口上的一次性下载端点：GET /download/{token}。
func (s *Service) ServeDownload(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/download/")
	path, ok := s.TakeDownloadToken(token)
	if !ok {
		http.Error(w, "download link is invalid or expired", http.StatusNotFound)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		http.Error(w, "artifact is gone", http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "artifact is gone", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Artifact-Name", strings.TrimSpace(sanitizeHeader(filepathBase(path))))
	_, _ = io.Copy(w, file)
}

func filepathBase(path string) string {
	index := strings.LastIndexAny(path, "/\\")
	if index < 0 {
		return path
	}
	return path[index+1:]
}

func sanitizeHeader(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if char >= 0x20 && char < 0x7f {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

// mcpEndpoint 探测该节点是否是一个可达的 NPA：GET 它的 /api/info。
// 返回它的版本与本机名（探测失败一律 ok=false，调用方回落到原报错）。
func (s *Service) probeNodeMesh(ctx context.Context, address string) (model.Node, bool) {
	info, ok := probeInfoOnce(ctx, address)
	if !ok {
		return model.Node{}, false
	}
	return info, true
}

// probeInfoOnce 对单个 host:port 做一次 /api/info 探测。
func probeInfoOnce(ctx context.Context, target string) (model.Node, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+target+"/api/info", nil)
	if err != nil {
		return model.Node{}, false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return model.Node{}, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return model.Node{}, false
	}
	var info struct {
		Product string `json:"product"`
		Version string `json:"version"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&info); err != nil {
		return model.Node{}, false
	}
	if info.Product != "npa" {
		return model.Node{}, false
	}
	return model.Node{Name: info.Name, Version: info.Version}, true
}

// resolveMeshEndpoint 确认目标主机是一个局域网可达的 NPA，并选出可用的
// MCP 端点。探测 /api/info：自报端口（旧版 56710）或 56701（新版）任一应答即可。
// MCP 端点：新版两个功能都在 56701；旧版 MCP 只在 56701（自报的 56710 是纯
// peer 口）。因此顺序是——先验证它是 NPA，再返回 host:56701；
// 但若自报端口本身就在响应 MCP（如测试桩/同端口部署），优先自报端口。
func (s *Service) resolveMeshEndpoint(ctx context.Context, host string, selfReported int) (string, bool) {
	candidates := []string{}
	if selfReported > 0 {
		candidates = append(candidates, net.JoinHostPort(host, strconv.Itoa(selfReported)))
	}
	candidates = append(candidates, net.JoinHostPort(host, strconv.Itoa(meshPort)))
	found := false
	selfReportedReachable := ""
	for _, candidate := range candidates {
		if _, ok := probeInfoOnce(ctx, candidate); ok {
			found = true
			if selfReportedReachable == "" && selfReported > 0 && selfReported != meshPort {
				selfReportedReachable = candidate
			}
			break
		}
	}
	if !found {
		return "", false
	}
	mesh := net.JoinHostPort(host, strconv.Itoa(meshPort))
	// 自报端口若也提供 MCP（initialize 应答 200），优先它（测试桩/同端口部署）
	if selfReportedReachable != "" {
		client := &mcpClient{base: "http://" + selfReportedReachable + "/mcp"}
		if err := client.initialize(ctx); err == nil {
			return selfReportedReachable, true
		}
	}
	return mesh, true
}

// mcpSelfUpgrade 对无通道的局域网 NPA 节点执行自升级：
//  1. 一次性 token 端点把本机二进制挂到本机 mesh 端口；
//  2. 经它自己的 MCP 在它本机执行「下载 → 校验 → 替换 → 重启」，
//     下载目录用节点自己的临时目录（无需预先探测可写目录）。
func (s *Service) mcpSelfUpgrade(ctx context.Context, peer model.Node, goos, goarch, binary, peerAddress string) (map[string]any, error) {
	endpoint, err := s.selfMeshEndpoint()
	if err != nil {
		return nil, err
	}
	token, err := s.newDownloadToken(binary, "npa "+goos+"/"+goarch)
	if err != nil {
		return nil, err
	}
	downloadURL := "http://" + endpoint + "/download/" + token
	dir := mcpDownloadDir(goos)
	report := map[string]any{
		"channel":           "mcp",
		"mcp_node":          peer.Name,
		"mcp_node_version":  peer.Version,
		"mcp_endpoint":      peerAddress,
		"download_url_host": endpoint,
	}
	if err := s.mcpExecLocal(ctx, peerAddress, mcpUpgradeScript(goos, downloadURL, dir), goos); err != nil {
		return nil, err
	}
	report["note"] = "已通过该节点自己的 MCP 下发自升级脚本；它将在本机下载新版本并替换重启"
	return report, nil
}

// mcpDownloadDir 是节点侧的下载目录：Windows 用 PowerShell 的 $env:TEMP，
// Unix 用 /tmp。注意 Windows 脚本整体走 PowerShell，不能用 cmd 的 %TEMP%。
func mcpDownloadDir(goos string) string {
	if goos == "windows" {
		return "$env:TEMP"
	}
	return "/tmp"
}

// selfMeshEndpoint 是本机在 mesh 端口上对局域网通告的地址。
func (s *Service) selfMeshEndpoint() (string, error) {
	if value := strings.TrimSpace(s.Settings()["peer_advertise"]); value != "" {
		if _, _, err := net.SplitHostPort(value); err == nil {
			return value, nil
		}
	}
	address := discovery.PrimaryIP()
	if address == "" || address == "127.0.0.1" {
		return "", apperrors.New(apperrors.NodeOffline,
			"本机没有局域网地址，目标节点无法从本机下载更新包")
	}
	return net.JoinHostPort(address, strconv.Itoa(meshPort)), nil
}

// mcpUpgradeScript 生成节点本机执行的自升级脚本（下载 → 魔数校验 → 替换 → 重启）。
// Windows 返回**纯 PowerShell 语句**（powershell 外壳由 mcpExecLocal 统一加，
// 避免 powershell 套 powershell）；Unix 返回完整 sh 命令。
func mcpUpgradeScript(goos, downloadURL, dir string) string {
	if goos == "windows" {
		// 全部用 PowerShell 变量与语句（单引号里的 $env:TEMP 不展开，
		// 这正是此前脚本静默失败的原因）；dir 参数仅用于 Unix。
		return strings.Join([]string{
			"$ErrorActionPreference='Stop'",
			"$out = Join-Path $env:TEMP 'npa-new.exe'",
			"Invoke-WebRequest -UseBasicParsing -Uri '" + downloadURL + "' -OutFile $out",
			"$b=[IO.File]::ReadAllBytes($out)[0..1]",
			"if(-not ($b[0]-eq 0x4D -and $b[1]-eq 0x5A)){ throw 'bad artifact' }",
			// 进程名是 npa-windows-amd64 之类（带平台后缀），必须用通配符
			"$p=(Get-Process npa* -ErrorAction Stop | Select-Object -First 1).Path",
			"$dir=Split-Path $p",
			"if(Test-Path ($dir+'/npa.old')){Remove-Item -Force ($dir+'/npa.old')}",
			"Rename-Item -Force $p 'npa.old'",
			"Copy-Item -Force $out $p",
			"Start-Process $p",
			"Stop-Process -Name 'npa*' -Force -ErrorAction SilentlyContinue",
		}, "; ")
	}
	name := "npa-new.bin"
	return strings.Join([]string{
		"sh -c '",
		"curl -fsS -o " + dir + "/" + name + " " + downloadURL + " &&",
		"head -c2 " + dir + "/" + name + " | grep -q ELF &&",
		"p=$(readlink /proc/$PPID/exe 2>/dev/null || which npa) &&",
		"cp -f " + dir + "/" + name + " $p.new && chmod +x $p.new && mv -f $p.new $p &&",
		"setsid nohup $p >/dev/null 2>&1 & sleep 1; kill -TERM $PPID 2>/dev/null; true",
		"'",
	}, " ")
}

// pushViaMCP 是 PushPackage 的 MCP 回落：节点上执行不了命令时，
// 探测它是否是局域网可达的 NPA（地址:56701 /api/info），是则经它自己的
// MCP 下发自升级。pkg 为空时自行解析本机更新包。
func (s *Service) pushViaMCP(ctx context.Context, nodeID, pkg string) (map[string]any, error) {
	// 发现节点只存在于 discovered 表；纳管节点在主表。两处都试。
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		if listed, listErr := s.Store.ListDiscoveredNodes(); listErr == nil {
			for _, candidate := range listed {
				if candidate.ID == nodeID || discoveredFallbackID(candidate) == nodeID {
					node = candidate
					err = nil
					break
				}
			}
		}
	}
	if err != nil || node.Address == "" {
		return nil, apperrors.New(apperrors.NotSupported, "节点没有地址，无法经 MCP 推送")
	}
	address, ok := s.resolveMeshEndpoint(ctx, node.Address, node.PeerPort)
	if !ok {
		// 目标的 NPA 服务（自报端口与 56701）都探不到：它不是可经
		// MCP 自升级的节点，返回哨兵让调用方回落到原始错误（通道不可用）。
		return nil, errMCPUnavailable
	}
	peerInfo, _ := s.probeNodeMesh(ctx, address)
	if pkg == "" {
		resolved, err := s.resolvePushPackage()
		if err != nil {
			return nil, err
		}
		pkg = resolved
	}
	// 平台探测也要经它的 MCP：在它本机执行 uname / echo %OS%
	goos, goarch, err := s.mcpDetectPlatform(ctx, address)
	if err != nil {
		return nil, err
	}
	workDir, err := selfupdate.ExtractPlatform(pkg, filepath.Join(s.Store.DataDir(), "push", fmt.Sprintf("%d", time.Now().UnixNano())), goos, goarch)
	if err != nil {
		return nil, err
	}
	if err := selfupdate.VerifyArtifactFor(workDir, goos); err != nil {
		return nil, err
	}
	digest, _ := selfupdate.FileSHA256(workDir)
	report, err := s.mcpSelfUpgrade(ctx, peerInfo, goos, goarch, workDir, address)
	if err == nil {
		report["sha256"] = digest
		report["package"] = pkg
		report["node_id"] = nodeID
	}
	return report, err
}

// mcpDetectPlatform 经目标节点的 MCP 在它本机识别平台。
func (s *Service) mcpDetectPlatform(ctx context.Context, address string) (string, string, error) {
	client := &mcpClient{base: "http://" + meshEndpointOf(address) + "/mcp"}
	if err := client.initialize(ctx); err != nil {
		return "", "", apperrors.Wrap(apperrors.NodeOffline, "无法连接该节点的 MCP 服务", err)
	}
	nodeID, err := client.findLocalNode(ctx)
	if err != nil {
		return "", "", err
	}
	if result, err := client.callTool(ctx, "ssh.exec", map[string]any{"node_id": nodeID, "command": "uname -s -m"}); err == nil {
		if fields := strings.Fields(strings.TrimSpace(result["stdout"].(string))); len(fields) >= 2 {
			switch strings.ToLower(fields[0]) {
			case "darwin", "linux":
				return strings.ToLower(fields[0]), normalizeArch(strings.ToLower(fields[1])), nil
			}
		}
	}
	// 注意不能带 `|`：cmd 会当管道解析（历史 bug），只读一个环境变量
	result, err := client.callTool(ctx, "ssh.exec", map[string]any{"node_id": nodeID, "command": "echo %PROCESSOR_ARCHITECTURE%"})
	if err != nil {
		return "", "", apperrors.Wrap(apperrors.NotSupported, "经该节点的 MCP 识别平台失败", err)
	}
	switch arch := strings.ToUpper(strings.TrimSpace(stringFieldValue(result["stdout"]))); arch {
	case "AMD64":
		return "windows", "amd64", nil
	case "ARM64":
		return "windows", "arm64", nil
	case "X86", "386":
		return "windows", "386", nil
	}
	return "", "", apperrors.New(apperrors.NotSupported, "无法识别该节点的平台")
}

func meshEndpointOf(address string) string {
	if _, _, err := net.SplitHostPort(address); err != nil {
		return net.JoinHostPort(address, strconv.Itoa(meshPort))
	}
	return address
}

func stringFieldValue(value any) string {
	text, _ := value.(string)
	return text
}

// mcpExecLocal 经目标节点自己的 MCP（地址:56701/mcp）在它本机执行命令。
// 只依赖标准 MCP 协议（initialize → tools/call ssh.exec），对旧版 NPA 同样可用。
func (s *Service) mcpExecLocal(ctx context.Context, address, command, goos string) error {
	target := address
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, strconv.Itoa(meshPort))
	}
	client := &mcpClient{base: "http://" + target + "/mcp"}
	if err := client.initialize(ctx); err != nil {
		return apperrors.Wrap(apperrors.NodeOffline, "无法连接该节点的 MCP 服务", err)
	}
	nodeID, err := client.findLocalNode(ctx)
	if err != nil {
		return err
	}
	// Windows：脚本已是纯 PowerShell 语句，统一走 EncodedCommand 绕开引号转译；
	// Unix：命令是完整 sh 行，原样下发
	execCommand := command
	if goos == "windows" {
		execCommand = encodePowerShell(command)
	}
	// 大下载（13MB）+ 替换：给足 120 秒；callTool 的 HTTP 客户端也放宽
	result, err := client.callTool(ctx, "ssh.exec", map[string]any{
		"node_id": nodeID, "command": execCommand, "timeout": 120,
	})
	if err != nil {
		return apperrors.Wrap(apperrors.NodeOffline, "经该节点的 MCP 执行自升级失败", err)
	}
	if code, _ := result["exit_code"].(float64); code != 0 {
		return apperrors.New(apperrors.NotSupported,
			"节点上的自升级脚本执行失败（exit "+strconv.Itoa(int(code))+"）："+stringFieldValue(result["stderr"]))
	}
	return nil
}

func encodePowerShell(script string) string {
	encoded := psEncode(script)
	return "powershell -NoProfile -EncodedCommand " + encoded
}

// meshHTTPClient 是节点间 HTTP（含 MCP 调用）的共享客户端：
// 不设总超时（由调用方 ctx 控制），连接池参数面向长响应。
var meshHTTPClient = &http.Client{}

// ---- 最小 MCP 客户端：只实现本通道需要的三个调用 ----

type mcpClient struct {
	base    string
	session string
	id      int
}

func (c *mcpClient) rpc(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.id++
	payload := map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		request.Header.Set("Mcp-Session-Id", c.session)
	}
	// 长命令（自升级下载 13MB）在 HTTP 层没有总超时，靠 ctx 控制上限；
	// DefaultClient 没有响应头超时限制问题，但为避免连接复用半死，用专用 client
	response, err := meshHTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if session := response.Header.Get("Mcp-Session-Id"); session != "" {
		c.session = session
	}
	if response.StatusCode >= 400 {
		return nil, fmt.Errorf("mcp http %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	text := string(raw)
	// Streamable HTTP 可能返回 SSE 帧
	if index := strings.Index(text, "data:"); index >= 0 {
		if lineEnd := strings.Index(text[index:], "\n"); lineEnd > 0 {
			text = text[index+5 : index+lineEnd]
		} else {
			text = text[index+5:]
		}
		text = strings.TrimSpace(text)
	}
	var reply struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &reply); err != nil {
		return nil, err
	}
	if reply.Error != nil {
		return nil, fmt.Errorf("mcp error: %s", reply.Error.Message)
	}
	return reply.Result, nil
}

func (c *mcpClient) initialize(ctx context.Context) error {
	_, err := c.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "npa-self-upgrade", "version": Version},
	})
	return err
}

// findLocalNode 经目标节点的 nodes.list 找到"它本机"对应的节点记录
// （auth_method=local），自升级要在它本机执行。
func (c *mcpClient) findLocalNode(ctx context.Context) (string, error) {
	result, err := c.callTool(ctx, "nodes.list", map[string]any{})
	if err != nil {
		return "", apperrors.Wrap(apperrors.NodeOffline, "无法读取该节点的节点列表", err)
	}
	text, _ := result["text"].(string)
	if text == "" {
		return "", apperrors.New(apperrors.NotSupported, "该节点的 MCP 没有返回节点列表")
	}
	var parsed struct {
		Nodes []struct {
			ID         string `json:"id"`
			AuthMethod string `json:"auth_method"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		// 也可能是裸数组
		var list []struct {
			ID         string `json:"id"`
			AuthMethod string `json:"auth_method"`
		}
		if err2 := json.Unmarshal([]byte(text), &list); err2 != nil {
			return "", apperrors.New(apperrors.NotSupported, "该节点的 MCP 节点列表格式无法解析")
		}
		for _, item := range list {
			if item.AuthMethod == "local" {
				return item.ID, nil
			}
		}
		return "", apperrors.New(apperrors.NotSupported, "该节点的 MCP 节点列表里没有本机节点")
	}
	for _, item := range parsed.Nodes {
		if item.AuthMethod == "local" {
			return item.ID, nil
		}
	}
	return "", apperrors.New(apperrors.NotSupported, "该节点的 MCP 节点列表里没有本机节点")
}

func (c *mcpClient) callTool(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	result, err := c.rpc(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, err
	}
	content, _ := result["content"].([]any)
	out := map[string]any{}
	for _, item := range content {
		entry, _ := item.(map[string]any)
		if entry == nil || entry["type"] != "text" {
			continue
		}
		if text, ok := entry["text"].(string); ok {
			out["text"] = text
			// tools/call 的 text 里通常还有一层 JSON（结构化结果），尽力解析
			var inner map[string]any
			if json.Unmarshal([]byte(text), &inner) == nil {
				for key, value := range inner {
					out[key] = value
				}
			}
			break
		}
	}
	if structured, ok := result["structuredContent"].(map[string]any); ok {
		for key, value := range structured {
			out[key] = value
		}
	}
	return out, nil
}

// psEncode 是 PowerShell -EncodedCommand 要求的 UTF-16LE base64。
func psEncode(script string) string {
	units := utf16.Encode([]rune(script))
	bytes := make([]byte, 0, len(units)*2+2)
	for _, unit := range units {
		bytes = append(bytes, byte(unit), byte(unit>>8))
	}
	return base64.StdEncoding.EncodeToString(bytes)
}
