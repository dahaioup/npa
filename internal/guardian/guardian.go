// Package guardian 实现远程升级的守护进程模式。
//
// 远程升级失败很危险：新版本可能起不来，而升级发起方的连接往往在杀掉旧进程的
// 那一刻就断了（节点唯一远程通道就是 NPA 自己），没人能把它拉回来。守护进程在
// 升级动手之前被拉起，独立于发起方存活，接管整个替换生命周期：
//
//	校验是否已升级 → 备份旧版 → 停旧进程 → 用 guardian 自己覆盖主程序 →
//	拉起主程序 → 10 秒健康检查 → 成功/回滚 → POST 56700 通知 NPA 清理 → 退出
//
// 守护进程用 `--guardian --caller-pid <pid>` 启动；不需要配置文件。
// 启动参数只需要调用者 PID，guardian 自己获取自身路径和 NPA 路径。
package guardian

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Outcome 是守护进程的退出结论。
type Outcome int

const (
	OutcomeSuccess    Outcome = iota // 新版本已稳定服务，NPA 已清理 guardian
	OutcomeRolledBack                // 新 NPA 启动失败，已回滚到旧版本
	OutcomeError                     // 守护进程自身出错
	OutcomeAlreadyUpgraded           // guardian 和 NPA 是同一文件，不需要升级
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomeRolledBack:
		return "rolled-back"
	case OutcomeAlreadyUpgraded:
		return "already-upgraded"
	default:
		return "error"
	}
}

const (
	// DefaultHealthURL 本机 Web 管理面（回环、无鉴权）的系统信息端点。
	DefaultHealthURL = "http://127.0.0.1:56700/api/system"
	// DefaultCompleteURL guardian 完成任务后通知 NPA 的端点。
	DefaultCompleteURL = "http://127.0.0.1:56700/api/guardian/complete"
	// StartupTimeout 给 NPA 的启动超时时间。
	StartupTimeout = 10 * time.Second
	// killSettle 停掉旧进程后等它退干净的时间。
	killSettle = 2 * time.Second
	// healthCheckInterval 启动后的健康检查间隔。
	healthCheckInterval = 500 * time.Millisecond
)


// setupGuardianLog 把 slog 输出重定向到 <dir>/guardian.log（追加）。
// Windows 分离启动下 stderr 丢失，文件日志是唯一的排障手段。
// 打开失败则保持 stderr 输出（Unix 场景仍然可见）。
func setupGuardianLog(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, "guardian.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
}

// RunByCallerPID 是新的 guardian 入口：只用调用者 PID 启动。
// callerPID 是当前 NPA 进程的 PID。
// restart=true：杀 NPA 后重新拉起（服务重启场景）；restart=false：升级场景。
func RunByCallerPID(callerPID int, restart bool) Outcome {
	if callerPID <= 0 {
		slog.Error("guardian: caller-pid is required")
		return OutcomeError
	}

	// 单例互斥：guardian.pid 文件锁
	if err := acquireGuardianLock(); err != nil {
		slog.Error("guardian: another instance is running", "error", err)
		return OutcomeError
	}
	defer releaseGuardianLock()

	// 获取自己的路径
	selfPath, err := os.Executable()
	if err != nil {
		slog.Error("guardian: cannot get self path", "error", err)
		return OutcomeError
	}
	selfPath, _ = filepath.Abs(selfPath)
	selfPath, _ = filepath.EvalSymlinks(selfPath)

	// 日志写独立 guardian.log（与 NPA 同目录）——Windows 分离启动下
	// stderr 会丢失，没有文件日志排查不了失败原因。需要在拿到 npPath 后才能定位目录。
	npPath, err := getProcessPath(callerPID)
	if err != nil {
		setupGuardianLog(os.TempDir())
		slog.Error("guardian: cannot get NPA path from pid", "pid", callerPID, "error", err)
		return OutcomeError
	}
	npPath, _ = filepath.Abs(npPath)
	npPath, _ = filepath.EvalSymlinks(npPath)
	setupGuardianLog(filepath.Dir(npPath))

	slog.Info("guardian: started", "self", selfPath, "npa", npPath, "caller_pid", callerPID, "restart", restart)

	// 重启模式：不需要升级，直接杀 NPA → 重新拉起
	if restart {
		return doRestart(npPath, callerPID)
	}

	// 对比：如果 guardian 和 NPA 是同一文件，说明已经升级过了
	if selfPath == npPath {
		slog.Info("guardian: already upgraded (same file), exiting")
		notifyNPAComplete()
		return OutcomeAlreadyUpgraded
	}

	// 开始升级
	return doUpgrade(selfPath, npPath, callerPID)
}

// doRestart 重启 NPA：杀掉 → 等退出 → 重新拉起。
func doRestart(npaPath string, callerPID int) Outcome {
	for attempt := 1; attempt <= 3; attempt++ {
		if !processAlive(callerPID) {
			break
		}
		if err := killPID(callerPID); err != nil {
			slog.Warn("guardian: kill NPA failed", "pid", callerPID, "attempt", attempt, "error", err)
		}
		time.Sleep(killSettle)
	}
	if processAlive(callerPID) {
		slog.Error("guardian: cannot kill NPA, abort", "pid", callerPID)
		return OutcomeError
	}
	slog.Info("guardian: NPA stopped, restarting")

	if err := spawn(npaPath, ""); err != nil {
		slog.Error("guardian: restart spawn failed", "error", err)
		return OutcomeError
	}
	slog.Info("guardian: NPA restarted", "path", npaPath)
	return OutcomeSuccess
}

// doUpgrade 执行升级：备份→杀旧→覆盖→启动→检查→通知。
func doUpgrade(guardianPath, npaPath string, callerPID int) Outcome {
	dir := filepath.Dir(npaPath)
	base := filepath.Base(npaPath)
	backupPath := filepath.Join(dir, "old-"+base)

	// 1. 备份旧版
	if err := copyBinary(npaPath, backupPath); err != nil {
		slog.Error("guardian: backup failed", "error", err)
		return OutcomeError
	}
	slog.Info("guardian: backed up", "backup", backupPath)

	// 2. 杀掉旧 NPA 进程（最多 3 轮）
	dead := false
	for attempt := 1; attempt <= 3; attempt++ {
		if !processAlive(callerPID) {
			dead = true
			break
		}
		if err := killPID(callerPID); err != nil {
			slog.Warn("guardian: kill NPA failed", "pid", callerPID, "attempt", attempt, "error", err)
		}
		time.Sleep(killSettle)
	}
	if !dead && processAlive(callerPID) {
		slog.Error("guardian: cannot kill NPA, abort", "pid", callerPID)
		_ = os.Remove(backupPath)
		notifyNPAComplete()
		return OutcomeError
	}
	slog.Info("guardian: NPA stopped")

	// 3. 用 guardian 自己覆盖 NPA 文件（保持 NPA 原名）
	if err := copyBinary(guardianPath, npaPath); err != nil {
		slog.Error("guardian: overwrite NPA failed", "error", err)
		// 回滚
		_ = copyBinary(backupPath, npaPath)
		_ = spawn(npaPath, "")
		notifyNPAComplete()
		return OutcomeRolledBack
	}
	slog.Info("guardian: overwritten NPA", "path", npaPath)

	// 4. 启动新 NPA
	if err := spawn(npaPath, ""); err != nil {
		slog.Error("guardian: spawn NPA failed", "error", err)
		_ = copyBinary(backupPath, npaPath)
		_ = spawn(npaPath, "")
		notifyNPAComplete()
		return OutcomeRolledBack
	}

	// 5. 等 10 秒，健康检查
	if waitForHealthy(StartupTimeout) {
		slog.Info("guardian: NPA is healthy, upgrade success")
		notifyNPAComplete()
		return OutcomeSuccess
	}

	// 6. 启动失败，回滚
	slog.Warn("guardian: NPA failed to start in 10s, rolling back")
	// 找到并杀掉新 NPA（通过找监听 56700 的进程）
	if pid := findPIDByPort(56700); pid > 0 {
		_ = killPID(pid)
		time.Sleep(killSettle)
	}
	_ = copyBinary(backupPath, npaPath)
	if err := spawn(npaPath, ""); err != nil {
		slog.Error("guardian: rollback spawn failed", "error", err)
		notifyNPAComplete()
		return OutcomeError
	}
	if waitForHealthy(StartupTimeout) {
		slog.Info("guardian: rollback successful, old NPA is running")
	} else {
		slog.Error("guardian: rollback also failed")
	}
	notifyNPAComplete()
	return OutcomeRolledBack
}

// notifyNPAComplete 通过 56700 通知 NPA 升级完成（或回滚完成），附带自己的 PID。
func notifyNPAComplete() {
	pid := os.Getpid()
	body, _ := json.Marshal(map[string]int{"pid": pid})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(DefaultCompleteURL, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Warn("guardian: notify NPA complete failed", "error", err)
		return
	}
	resp.Body.Close()
	slog.Info("guardian: notified NPA complete", "pid", pid)
}

// waitForHealthy 等 NPA 健康检查通过，最多 timeout。
func waitForHealthy(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, healthy := checkHealth(DefaultHealthURL); healthy {
			return true
		}
		time.Sleep(healthCheckInterval)
	}
	return false
}

// 单例互斥：用 pidfile + flock（平台实现见 lock_unix.go / lock_windows.go）
var guardianLockFile *os.File

func acquireGuardianLock() error {
	lockPath := guardianLockPath()
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := flockLock(f); err != nil {
		f.Close()
		return fmt.Errorf("another guardian is running")
	}
	guardianLockFile = f
	return nil
}

func releaseGuardianLock() {
	if guardianLockFile != nil {
		flockUnlock(guardianLockFile)
		guardianLockFile.Close()
		guardianLockFile = nil
	}
}

// getProcessPath 通过 PID 获取进程的可执行文件路径。
func getProcessPath(pid int) (string, error) {
	// Linux: /proc/<pid>/exe
	if runtime.GOOS == "linux" {
		return os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	}
	// macOS: /proc 不存在，用 sysctl
	if runtime.GOOS == "darwin" {
		return getProcessPathDarwin(pid)
	}
	// Windows: 用 wmic 或 PowerShell
	if runtime.GOOS == "windows" {
		return getProcessPathWindows(pid)
	}
	return "", fmt.Errorf("unsupported platform")
}

func getProcessPathDarwin(pid int) (string, error) {
	// 用 lsof 或 proc_pidinfo
	out, err := exec.Command("lsof", "-p", fmt.Sprint(pid), "-Fn", "-a", "-d", "txt").Output()
	if err != nil {
		return "", err
	}
	// 解析 lsof 输出找 txt 段的第一行
	lines := bytes.Split(out, []byte("\n"))
	for _, line := range lines {
		if len(line) > 1 && line[0] == 'n' {
			return string(line[1:]), nil
		}
	}
	return "", fmt.Errorf("cannot find process path")
}



// findPIDByPort 通过监听端口找进程 PID（用于杀掉新启动的 NPA）。
func findPIDByPort(port int) int {
	// Linux: 用 ss 或 netstat 找监听端口的进程
	if runtime.GOOS == "linux" {
		out, err := exec.Command("ss", "-tlnp", fmt.Sprintf("sport = :%d", port)).Output()
		if err == nil {
			// 解析 ss 输出找 pid
			if pid := parseSSPid(string(out)); pid > 0 {
				return pid
			}
		}
		// 回退 netstat
		out, err = exec.Command("netstat", "-tlnp").Output()
		if err == nil {
			if pid := parseNetstatPid(string(out), port); pid > 0 {
				return pid
			}
		}
	}
	// macOS: 用 lsof
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("lsof", "-i", fmt.Sprintf(":%d", port), "-sTCP:LISTEN", "-t").Output()
		if err == nil {
			var pid int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &pid); err == nil {
				return pid
			}
		}
	}
	return 0
}

func parseSSPid(out string) int {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "pid=") {
			start := strings.Index(line, "pid=") + 4
			end := start
			for end < len(line) && line[end] >= '0' && line[end] <= '9' {
				end++
			}
			var pid int
			if _, err := fmt.Sscanf(line[start:end], "%d", &pid); err == nil {
				return pid
			}
		}
	}
	return 0
}

func parseNetstatPid(out string, port int) int {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, fmt.Sprintf(":%d", port)) && strings.Contains(line, "LISTEN") {
			// 最后一列是 pid/process
			fields := strings.Fields(line)
			if len(fields) > 0 {
				lastField := fields[len(fields)-1]
				if idx := strings.Index(lastField, "/"); idx > 0 {
					var pid int
					if _, err := fmt.Sscanf(lastField[:idx], "%d", &pid); err == nil {
						return pid
					}
				}
			}
		}
	}
	return 0
}

// 可替换的钩子：单测里换成假实现就能验证状态机，不碰真实进程/文件/网络。
var (
	killPID      = defaultKillPID
	processAlive = defaultProcessAlive
	spawn        = defaultSpawn
	health       = checkHealth
	copyBinary   = defaultCopyBinary
)

// checkHealth 拉取本机系统信息端点，返回 (版本号, 是否健康)。
func checkHealth(url string) (string, bool) {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", false
	}
	return body.Version, true
}

// defaultKillPID 结束指定进程。
func defaultKillPID(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

// defaultProcessAlive 探测 pid 是否还活着。
func defaultProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		_ = process.Release()
		return windowsProcessAlive(pid)
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// defaultCopyBinary 把 src 覆盖到 dst：先写临时文件再改名。
func defaultCopyBinary(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmpcopy"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if lastErr = os.Rename(tmp, dst); lastErr == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return fmt.Errorf("rename %s -> %s after retries: %w", tmp, dst, lastErr)
}
