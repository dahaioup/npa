package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/npa/npa/internal/audit"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/policy"
	"github.com/npa/npa/internal/selfupdate"
)

// PushPackage 把本机的新版本上传到指定节点（只负责"送到"，不负责重启）。
// remoteDir 为空时会在节点上探测可写目录（Windows 的用户 Downloads、/tmp 等）。
func (s *Service) PushPackage(ctx context.Context, nodeID, remoteDir string) (map[string]any, error) {
	if err := s.Authorize(ctx, "update.push", policy.CapFile); err != nil {
		return nil, err
	}
	started := time.Now()
	pkg, err := s.resolvePushPackage()
	if err != nil {
		s.Audit.Record(ctx, "update.push", nodeID, policy.CapFile, "", started, err)
		return nil, err
	}
	goos, goarch, err := s.detectNodePlatform(ctx, nodeID)
	if err != nil {
		// 节点上执行不了命令（没有反向链路/SSH）：若它是局域网上 56701 可达的
		// NPA（mDNS 发现的独立节点），改走它自己的 MCP 让它自升级。
		if report, mcpErr := s.pushViaMCP(ctx, nodeID, pkg); mcpErr == nil {
			s.Audit.Record(ctx, "update.push", nodeID, policy.CapFile,
				audit.SafeParameters(map[string]string{"channel": "mcp"}), started, nil)
			return report, nil
		} else if mcpErr.Error() != "mcp-unavailable" {
			s.Audit.Record(ctx, "update.push", nodeID, policy.CapFile, "", started, mcpErr)
			return nil, mcpErr
		}
		s.Audit.Record(ctx, "update.push", nodeID, policy.CapFile, "", started, err)
		return nil, err
	}
	workDir := filepath.Join(s.Store.DataDir(), "push", fmt.Sprintf("%d", time.Now().UnixNano()))
	binary, err := selfupdate.ExtractPlatform(pkg, workDir, goos, goarch)
	if err != nil {
		s.Audit.Record(ctx, "update.push", nodeID, policy.CapFile, "", started, err)
		return nil, err
	}
	if err := selfupdate.VerifyArtifactFor(binary, goos); err != nil {
		s.Audit.Record(ctx, "update.push", nodeID, policy.CapFile, "", started, err)
		return nil, err
	}
	digest, _ := selfupdate.FileSHA256(binary)
	info, _ := os.Stat(binary)

	dir, err := s.resolvePushDir(ctx, nodeID, remoteDir)
	if err != nil {
		s.Audit.Record(ctx, "update.push", nodeID, policy.CapFile, "", started, err)
		return nil, err
	}
	remoteBinary := filepath.ToSlash(filepath.Join(dir, selfupdate.BinaryName(goos, goarch)))
	source, err := os.Open(binary)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	written, err := s.Files.Upload(ctx, nodeID, remoteBinary, source)
	if err != nil {
		s.Audit.Record(ctx, "update.push", nodeID, policy.CapFile,
			audit.SafeParameters(map[string]string{"remote": remoteBinary}), started, err)
		return nil, err
	}
	executable, execErr := s.nodeExecutable(ctx, nodeID)

	report := map[string]any{
		"node_id":         nodeID,
		"package":         pkg,
		"goos":            goos,
		"goarch":          goarch,
		"remote_binary":   remoteBinary,
		"remote_dir":      dir,
		"binary_path":     binary,
		"bytes":           written,
		"sha256":          digest,
		"node_executable": executable,
	}
	if info != nil {
		report["local_bytes"] = info.Size()
	}
	if execErr != nil {
		report["node_executable_error"] = execErr.Error()
	}
	s.Audit.Record(ctx, "update.push", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{
		"remote": remoteBinary, "bytes": fmt.Sprintf("%d", written), "sha256": digest,
	}), started, nil)
	return report, nil
}

// PushUpdate 推送并让节点自己替换重启（供非 Web 场景调用）。
func (s *Service) PushUpdate(ctx context.Context, nodeID, remoteDir string) (map[string]any, error) {
	report, err := s.PushPackage(ctx, nodeID, remoteDir)
	if err != nil {
		return nil, err
	}
	return s.PushRestart(ctx, nodeID, report)
}

// PushRestart 让节点用刚上传的新版本替换自身并重启。
// MCP 通道（channel=mcp）不需要：它的自升级脚本已包含替换与重启。
// 节点侧脚本会覆盖正在运行的可执行文件并重启进程，
// 因此这条 exec 以"进程被杀"收尾是**预期结果**，不是失败。
func (s *Service) PushRestart(ctx context.Context, nodeID string, report map[string]any) (map[string]any, error) {
	if channel, _ := report["channel"].(string); channel == "mcp" {
		return report, nil
	}
	goos, _ := report["goos"].(string)
	remoteBinary, _ := report["remote_binary"].(string)
	executable, _ := report["node_executable"].(string)
	if strings.TrimSpace(executable) == "" {
		return report, apperrors.New(apperrors.NotSupported,
			"无法确定节点上正在运行的 NPA 路径，包已上传但未执行替换；请在节点上手动升级")
	}
	remoteScript := remoteScriptPath(remoteBinary, "npa-update.sh")
	command := "sh " + strconv.Quote(remoteScript)
	if goos == "windows" {
		remoteScript = remoteScriptPath(remoteBinary, "npa-update.cmd")
		command = "cmd /c " + remoteScript
	}
	script := pushRestartScript(goos, remoteBinary, executable)
	if err := s.Files.Write(ctx, nodeID, remoteScript, []byte(script)); err != nil {
		report["restart_error"] = err.Error()
		return report, err
	}

	started := time.Now()
	result, execErr := s.Exec(ctx, nodeID, command, 60*time.Second)
	report["restart_command"] = command
	report["restart_script"] = remoteScript
	if execErr != nil {
		report["restart_error"] = execErr.Error() // 进程被替换/杀掉时属预期
	}
	if output := strings.TrimSpace(result.Stdout + "\n" + result.Stderr); output != "" {
		report["restart_output"] = output
	}
	report["note"] = "节点正在用新版本替换自身；几秒后它的反向链路会以新版本重连"
	s.Audit.Record(ctx, "update.restart", nodeID, policy.CapFile, audit.SafeParameters(map[string]string{
		"command": command, "script": remoteScript,
	}), started, execErr)
	return report, nil
}

func remoteScriptPath(remoteBinary, name string) string {
	return filepath.ToSlash(filepath.Join(filepath.Dir(remoteBinary), name))
}

// resolvePushPackage 定位本机要推送的包：设置项优先，其次数据目录、仓库 dist、当前目录。
func (s *Service) resolvePushPackage() (string, error) {
	candidates := []string{}
	if hint := strings.TrimSpace(s.Settings()["push_package"]); hint != "" {
		candidates = append(candidates, hint)
	}
	dataDir := s.Store.DataDir()
	candidates = append(candidates, filepath.Join(dataDir, "update"), filepath.Join(".", "dist"), ".")

	var (
		best     string
		bestTime time.Time
	)
	for _, candidate := range candidates {
		stat, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if !stat.IsDir() {
			if isPackageFile(candidate) && (best == "" || stat.ModTime().After(bestTime)) {
				best, bestTime = candidate, stat.ModTime()
			}
			continue
		}
		entries, err := os.ReadDir(candidate)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(candidate, entry.Name())
			if !isPackageFile(path) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if best == "" || info.ModTime().After(bestTime) {
				best, bestTime = path, info.ModTime()
			}
		}
	}
	if best == "" {
		return "", apperrors.New(apperrors.InvalidArgument,
			"没有找到本机的更新包（npa-all_*.zip）；可在设置里指定「更新包路径」")
	}
	return best, nil
}

func isPackageFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(filepath.Base(path)), ".zip") ||
		selfupdate.IsCandidateArtifact(filepath.Base(path))
}

// detectNodePlatform 识别节点平台：先按 Unix 试，失败再读 Windows 环境变量。
// 注意 Windows 探测命令里不能有 `|`：cmd 会把它当管道解析（展开后变成
// `echo Windows_NT | AMD64`，试图执行名为 AMD64 的命令），这正是历史 bug。
func (s *Service) detectNodePlatform(ctx context.Context, nodeID string) (string, string, error) {
	if result, err := s.Exec(ctx, nodeID, "uname -s -m", 15*time.Second); err == nil {
		fields := strings.Fields(strings.TrimSpace(result.Stdout))
		if len(fields) >= 2 {
			switch strings.ToLower(fields[0]) {
			case "darwin", "linux":
				return strings.ToLower(fields[0]), normalizeArch(strings.ToLower(fields[1])), nil
			}
		}
	}
	result, err := s.Exec(ctx, nodeID, "echo %PROCESSOR_ARCHITECTURE%", 15*time.Second)
	if err == nil {
		switch arch := strings.ToUpper(strings.TrimSpace(result.Stdout)); arch {
		case "AMD64":
			return "windows", "amd64", nil
		case "ARM64":
			return "windows", "arm64", nil
		case "X86", "386":
			return "windows", "386", nil
		}
	}
	return "", "", apperrors.New(apperrors.NotSupported,
		"无法识别节点平台：在节点上执行命令失败（通常是反向链路未建立）。请先确认该节点的 NPA 正在运行，并能访问本机的反向接入端口")
}

func normalizeArch(value string) string {
	switch value {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "i386", "i686", "x86", "386":
		return "386"
	default:
		return value
	}
}

// probeWritable 用一次 1 字节写入+删除试探目录是否可写，避免上传几十 MB 后才发现权限不够。
func (s *Service) probeWritable(ctx context.Context, nodeID, dir string) bool {
	probe := filepath.ToSlash(filepath.Join(dir, fmt.Sprintf(".npa-write-probe-%d", time.Now().UnixNano())))
	if err := s.Files.Write(ctx, nodeID, probe, []byte("npa")); err != nil {
		return false
	}
	_ = s.Files.Remove(ctx, nodeID, probe)
	return true
}

// resolvePushDir 在节点上挑一个可写目录：先看用户指定的，再自动探测。
func (s *Service) resolvePushDir(ctx context.Context, nodeID, hint string) (string, error) {
	hint = strings.TrimSpace(hint)
	if hint != "" {
		if s.probeWritable(ctx, nodeID, hint) {
			return filepath.ToSlash(hint), nil
		}
		return "", apperrors.New(apperrors.InvalidArgument, "节点上的目录不可写: "+hint)
	}
	candidates := []string{}
	if users, err := s.Files.List(ctx, nodeID, "C:/Users"); err == nil {
		for _, user := range users {
			if user.IsDir {
				candidates = append(candidates, "C:/"+user.Path+"/Downloads")
			}
		}
	}
	candidates = append(candidates, "Downloads", "/tmp", "C:/temp")
	for _, candidate := range candidates {
		if s.probeWritable(ctx, nodeID, candidate) {
			return filepath.ToSlash(candidate), nil
		}
	}
	return "", apperrors.New(apperrors.InvalidArgument,
		"节点上没有可写目录用于放置升级包；可以指定 remote_dir（例如 C:/Users/<用户>/Downloads）")
}

// nodeExecutable 尽力确定节点上正在运行的 NPA 可执行文件路径。
func (s *Service) nodeExecutable(ctx context.Context, nodeID string) (string, error) {
	if result, err := s.Exec(ctx, nodeID, `powershell -NoProfile -Command "(Get-Process npa).Path"`, 20*time.Second); err == nil {
		if path := strings.TrimSpace(result.Stdout); path != "" && !strings.Contains(path, "%") && !strings.Contains(strings.ToLower(path), "cannot") {
			return path, nil
		}
	}
	for _, command := range []string{
		`readlink /proc/$PPID/exe`,
		`ps -o comm= -p $PPID | tail -1`,
	} {
		result, err := s.Exec(ctx, nodeID, command, 15*time.Second)
		if err != nil {
			continue
		}
		if path := strings.TrimSpace(result.Stdout); strings.HasPrefix(path, "/") {
			return path, nil
		}
	}
	return "", apperrors.New(apperrors.NotSupported, "无法确定节点上正在运行的 NPA 路径")
}

// pushRestartScript 生成节点侧的替换脚本：把新版本覆盖到当前可执行文件并重启。
func pushRestartScript(goos, newBinary, executable string) string {
	if goos == "windows" {
		return strings.Join([]string{
			"@echo off",
			"setlocal",
			"set NEW=" + newBinary,
			"set CUR=" + executable,
			`if not exist "%NEW%" exit /b 3`,
			`if exist "npa.old" del "npa.old" >nul 2>&1`,
			`ren "%CUR%" "npa.old" >nul 2>&1`,
			`copy /y "%NEW%" "%CUR%" >nul`,
			`start "" "%CUR%"`,
			"taskkill /f /im " + filepath.Base(executable) + " >nul 2>&1",
			"exit /b 0",
		}, "\r\n")
	}
	return strings.Join([]string{
		"#!/bin/sh",
		"NEW='" + newBinary + "'",
		"CUR='" + executable + "'",
		`[ -f "$NEW" ] || exit 3`,
		`cp -f "$NEW" "$CUR.new" && chmod +x "$CUR.new" && mv -f "$CUR.new" "$CUR"`,
		`kill -TERM "$PPID" 2>/dev/null || true`,
		"sleep 1",
		`setsid nohup "$CUR" >/dev/null 2>&1 &`,
		"exit 0",
	}, "\n")
}
