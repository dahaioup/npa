// apply.go 实现节点侧「原子升级」：一条指令完成 校验 → fork guardian → 返回，
// 全程封装在 NPA 内部，调用方（MCP/上级）只提供包路径，不再逐步安排升级流程。
//
// 流程（全部在本节点进程内执行）：
//  1. 校验 package_url 指向的文件：平台头匹配、扩展名正确、可执行权限、版本号、大小。
//     有问题直接返回错误，不 fork guardian。
//  2. NPA 写 pidfile（~/.npa/npa.pid），单例互斥。
//  3. 复制自己为 guardian（原名+`-guardian`后缀），启动 --guardian --caller-pid <PID>。
//  4. NPA 继续正常服务，不自杀。guardian 接管后续升级。
package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultHealthURL 节点本机 Web 管理面（回环），guardian 用它判活。
const DefaultHealthURL = "http://127.0.0.1:56700/api/system"

// ApplyRequest 原子升级请求。
type ApplyRequest struct {
	// PackageURL 升级包地址：本地路径或 HTTP URL（zip 整包或单平台裸二进制）。
	PackageURL string `json:"package_url"`
	// SHA256 包的期望哈希（hex，可空；提供则强校验）。
	SHA256 string `json:"sha256,omitempty"`
	// ExpectedVersion 期望升级后的版本（可空）。
	ExpectedVersion string `json:"expected_version,omitempty"`
	// HealthURL 判活端点（可空，默认本机 56700 /api/system）。
	HealthURL string `json:"health_url,omitempty"`
}

// ApplyResult 原子升级受理结果。受理成功即返回；后续成败看 guardian.log。
type ApplyResult struct {
	Status      string `json:"status"` // accepted
	Message     string `json:"message"`
	Binary      string `json:"binary"` // 校验通过的新二进制路径
	GuardianPID int    `json:"guardian_pid"`
	PackageSize int64  `json:"package_size"`
}

// UpgradeApply 执行原子升级。返回时 guardian 已被拉起（或校验失败）。
// 不再下载——调用方应已通过文件通道上传新二进制到本节点。
func UpgradeApply(_ context.Context, req ApplyRequest) (*ApplyResult, error) {
	if strings.TrimSpace(req.PackageURL) == "" {
		return nil, fmt.Errorf("package_url is required")
	}

	// 1) 校验新二进制
	binPath, size, err := validatePackage(req.PackageURL, req.SHA256)
	if err != nil {
		return nil, err
	}

	// 2) NPA 写 pidfile
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	exePath, _ = filepath.Abs(exePath)
	if err := writePIDFile(os.Getpid()); err != nil {
		return nil, fmt.Errorf("write pidfile: %w", err)
	}

	// 3) 直接启动 package_url 校验过的文件为 guardian（设计定案）。
	// doUpgrade 的 copyBinary 只「读」自身、写 tmp 再覆盖目标，不写运行中文件，合法。
	guardianPath := binPath
	// 4) 启动 guardian
	pid, err := launchGuardian(guardianPath, os.Getpid())
	if err != nil {
		os.Remove(guardianPath)
		return nil, fmt.Errorf("launch guardian: %w", err)
	}

	return &ApplyResult{
		Status:      "accepted",
		Message:     fmt.Sprintf("guardian launched (pid %d); it will swap, restart and roll back on failure", pid),
		Binary:      binPath,
		GuardianPID: pid,
		PackageSize: size,
	}, nil
}

// validatePackage 校验新二进制并返回 (路径, 大小, error)。
// 支持本地路径和 HTTP URL。wantSHA256 非空时做 sha256 强校验。
func validatePackage(pkgURL, wantSHA256 string) (string, int64, error) {
	var path string
	var size int64
	var downloaded string // HTTP 下载的临时文件；校验失败时清理
	defer func() {
		if downloaded != "" && path != downloaded {
			_ = os.Remove(downloaded)
		}
	}()

	if strings.HasPrefix(pkgURL, "http://") || strings.HasPrefix(pkgURL, "https://") {
		// HTTP URL：下载到临时文件。
		// 后缀必须是 .exe：Windows 的 exec.LookPath 对无扩展名文件只尝试
		// file+PATHEXT（.exe/.com…），npa-upgrade-* 会被判「not found in %PATH%」，
		// 版本校验与 Start-Process 都会失败（Unix 忽略后缀，无影响）。
		tmp, err := os.CreateTemp("", "npa-upgrade-*.exe")
		if err != nil {
			return "", 0, err
		}
		resp, err := http.Get(pkgURL)
		if err != nil {
			tmp.Close()
			return "", 0, fmt.Errorf("download: %w", err)
		}
		defer resp.Body.Close()
		n, err := io.Copy(tmp, resp.Body)
		if err != nil {
			tmp.Close()
			return "", 0, err
		}
		tmp.Close()
		path = tmp.Name()
		downloaded = path
		size = n
	} else {
		// 本地路径
		info, err := os.Stat(pkgURL)
		if err != nil {
			return "", 0, fmt.Errorf("stat: %w", err)
		}
		if info.IsDir() {
			return "", 0, fmt.Errorf("not a file")
		}
		path = pkgURL
		size = info.Size()
	}

	// sha256 强校验（此前字段声明了却从未校验——补上：防下载损坏/被替换）
	if want := strings.ToLower(strings.TrimSpace(wantSHA256)); want != "" {
		f, err := os.Open(path)
		if err != nil {
			return "", 0, err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", 0, err
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != want {
			return "", 0, fmt.Errorf("sha256 mismatch: want %s got %s", want, got)
		}
	}

	// 平台校验
	if err := verifyBinaryHeader(path); err != nil {
		return "", 0, err
	}

	// 可执行权限（Unix）
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o755); err != nil {
			return "", 0, fmt.Errorf("chmod: %w", err)
		}
	}

	// 版本号校验
	ver, err := getBinaryVersion(path)
	if err != nil {
		return "", 0, fmt.Errorf("version check: %w", err)
	}
	if ver == "" {
		return "", 0, fmt.Errorf("empty version")
	}

	return path, size, nil
}

// getBinaryVersion 运行二进制 --version 获取版本号。
func getBinaryVersion(path string) (string, error) {
	// 30s：Windows 杀软对新写入的可执行文件常做首次深度扫描，
	// 5s 会被误杀（TerminateProcess 退出码恰为 1，报成 "exit status 1"）。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// launchGuardian 启动 guardian 进程，返回其 PID。
func launchGuardian(guardianPath string, callerPID int) (int, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// PowerShell Start-Process 分离启动
		psCmd := fmt.Sprintf(`Start-Process -FilePath '%s' -ArgumentList '--guardian','--caller-pid','%d' -WindowStyle Hidden -PassThru | Select-Object -ExpandProperty Id`,
			guardianPath, callerPID)
		out, err := exec.Command("powershell", "-NoProfile", "-Command", psCmd).Output()
		if err != nil {
			return 0, err
		}
		var pid int
		fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &pid)
		return pid, nil
	case "linux":
		cmd = exec.Command("setsid", "nohup", guardianPath, "--guardian", "--caller-pid", fmt.Sprint(callerPID))
	case "darwin":
		cmd = exec.Command("nohup", guardianPath, "--guardian", "--caller-pid", fmt.Sprint(callerPID))
	default:
		cmd = exec.Command(guardianPath, "--guardian", "--caller-pid", fmt.Sprint(callerPID))
	}
	applySysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

// writePIDFile 写 NPA 的 pidfile。
func writePIDFile(pid int) error {
	dir := os.TempDir()
	pidPath := filepath.Join(dir, "npa.pid")
	return os.WriteFile(pidPath, []byte(fmt.Sprint(pid)), 0o644)
}

// readPIDFile 读 NPA 的 pidfile。
func readPIDFile() (int, error) {
	pidPath := filepath.Join(os.TempDir(), "npa.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, err
	}
	var pid int
	_, err = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	return pid, err
}

// SpawnGuardian 为「重启服务」场景拉起一个最小守护：不复制自己，
// 直接用本体以 --guardian --restart --caller-pid 启动，guardian 杀掉 NPA 后重新拉起。
func SpawnGuardian() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	_, err = launchGuardianRestart(executable, os.Getpid())
	return err
}

// launchGuardianRestart 用本体以 --guardian --restart --caller-pid 启动。
func launchGuardianRestart(guardianPath string, callerPID int) (int, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		psCmd := fmt.Sprintf(`Start-Process -FilePath '%s' -ArgumentList '--guardian','--restart','--caller-pid','%d' -WindowStyle Hidden -PassThru | Select-Object -ExpandProperty Id`,
			guardianPath, callerPID)
		out, err := exec.Command("powershell", "-NoProfile", "-Command", psCmd).Output()
		if err != nil {
			return 0, err
		}
		var pid int
		fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &pid)
		return pid, nil
	case "linux":
		cmd = exec.Command("setsid", "nohup", guardianPath, "--guardian", "--restart", "--caller-pid", fmt.Sprint(callerPID))
	case "darwin":
		cmd = exec.Command("nohup", guardianPath, "--guardian", "--restart", "--caller-pid", fmt.Sprint(callerPID))
	default:
		cmd = exec.Command(guardianPath, "--guardian", "--restart", "--caller-pid", fmt.Sprint(callerPID))
	}
	applySysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func copyFile(src, dest string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, cpErr := io.Copy(out, in)
	if clErr := out.Close(); cpErr == nil {
		cpErr = clErr
	}
	return cpErr
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// verifyBinaryHeader 校验文件头与本节点平台一致。
func verifyBinaryHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF {
		return err
	}
	head = head[:n]
	goos, goarch := runtime.GOOS, runtime.GOARCH
	switch {
	case goos == "windows":
		if len(head) < 64 || head[0] != 'M' || head[1] != 'Z' {
			return fmt.Errorf("二进制不是 Windows PE（升级包可能传错了平台）：got % x", head[:min(2, len(head))])
		}
		off := int(binary.LittleEndian.Uint32(head[0x3c:0x40]))
		if off+6 > len(head) || head[off] != 'P' || head[off+1] != 'E' {
			return fmt.Errorf("PE 头缺失（升级包可能传错了平台）")
		}
		machine := binary.LittleEndian.Uint16(head[off+4 : off+6])
		want := map[string]uint16{"amd64": 0x8664, "arm64": 0xAA64, "386": 0x014c}[goarch]
		if machine != want {
			return fmt.Errorf("PE 架构不匹配：节点是 %s，包是 machine=0x%04x", goarch, machine)
		}
	case goos == "linux", goos == "darwin":
		if goos == "darwin" {
			wantMagic := []byte{0xcf, 0xfa, 0xed, 0xfe}
			if len(head) < 8 || !bytes.Equal(head[0:4], wantMagic) {
				return fmt.Errorf("二进制不是 macOS Mach-O（升级包可能传错了平台）：got % x", head[:min(4, len(head))])
			}
			// Mach-O cputype: x86_64 = CPU_TYPE_X86_64 | ABI64 = 0x01000007
			//                  arm64   = CPU_TYPE_ARM64   | ABI64 = 0x0100000C
			cputype := binary.LittleEndian.Uint32(head[4:8])
			wantCpu := map[string]uint32{"amd64": 0x01000007, "arm64": 0x0100000C}[goarch]
			if cputype != wantCpu {
				return fmt.Errorf("Mach-O 架构不匹配：节点是 %s，包是 cputype=0x%08x", goarch, cputype)
			}
			return nil
		}
		if len(head) < 20 || head[0] != 0x7f || string(head[1:4]) != "ELF" {
			return fmt.Errorf("二进制不是 ELF（升级包可能传错了平台）：got % x", head[:min(4, len(head))])
		}
		machine := binary.LittleEndian.Uint16(head[18:20])
		want := map[string]uint16{"amd64": 62, "arm64": 183, "386": 3}[goarch]
		if machine != want {
			return fmt.Errorf("ELF 架构不匹配：节点是 %s，包是 e_machine=%d", goarch, machine)
		}
	}
	return nil
}
