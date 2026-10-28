// Package guardian 实现远程升级的守护进程模式。
//
// 远程升级失败很危险：新版本可能起不来，而升级发起方的连接往往在杀掉旧进程的
// 那一刻就断了（节点唯一远程通道就是 NPA 自己），没人能把它拉回来。守护进程在
// 升级动手之前被拉起，独立于发起方存活，接管整个替换生命周期：
//
//	备份旧版 → 停旧进程 → 用新版覆盖主程序 → 拉起主程序 →
//	每 interval 巡检一次 → 挂了就重启 → 连续拉不起来就回滚到备份 →
//	确认新版本稳定服务后自我清理退出。
//
// 守护进程用 `--guardian-config <file.json>` 进入；配置从 JSON 文件读，避免
// 经 shell 传参时的引号转义问题（尤其 Windows）。
package guardian

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"log/slog"
	"time"
)

// Outcome 是守护进程的退出结论。
type Outcome int

const (
	OutcomeSuccess    Outcome = iota // 新版本已稳定服务，已清理退出
	OutcomeRolledBack                // 连续拉起失败，已回滚到旧版本
	OutcomeError                     // 守护进程自身出错（如覆盖主程序失败）
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomeRolledBack:
		return "rolled-back"
	default:
		return "error"
	}
}

const (
	// DefaultInterval 巡检间隔，默认 5s。
	DefaultInterval = 5 * time.Second
	// DefaultMaxRestarts 连续拉起失败多少次后回滚到旧版本，默认 6。
	DefaultMaxRestarts = 6
	// DefaultSuccessChecks 判定「升级成功」所需的连续健康次数，默认 2（即约 2 个巡检周期稳定）。
	DefaultSuccessChecks = 2
	// downTicksBeforeAct 连续 down 这么多个巡检周期才动手，避开启动期/瞬时抖动。
	downTicksBeforeAct = 2
	// defaultHealthURL 本机 Web 管理面（回环、无鉴权）的系统信息端点。
	defaultHealthURL = "http://127.0.0.1:56700/api/system"
	// killSettle 停掉旧进程后等它退干净的时间。
	killSettle = 2 * time.Second
)

// Config 是守护进程的运行期配置（由 FileConfig 解析并补默认值）。
type Config struct {
	MainPath      string        // 主程序路径（升级后应为新版本）
	NewPath       string        // 新版本二进制（用于覆盖 MainPath）
	BackupPath    string        // 升级前 MainPath 的备份（回滚用）
	DataDir       string        // 传给主程序的 --data-dir（空=默认）
	ExpectedVer   string        // 期望的新版本号；空=只看健康不校验版本
	HealthURL     string        // 健康检查地址
	OldPID        int           // 升级前主程序 PID；0=跳过停旧进程
	Interval      time.Duration // 巡检间隔
	MaxRestarts   int           // 连续拉起失败上限
	SuccessChecks int           // 判定成功所需连续健康次数
	TempSelf      string        // 守护进程自身路径（清理用；空=os.Executable）
}

// FileConfig 是 JSON 配置文件的形状；时长用整秒，避免 JSON 里出现纳秒整数。
type FileConfig struct {
	MainPath      string `json:"main_path"`
	NewPath       string `json:"new_path"`
	BackupPath    string `json:"backup_path"`
	DataDir       string `json:"data_dir"`
	ExpectedVer   string `json:"expected_version"`
	HealthURL     string `json:"health_url"`
	OldPID        int    `json:"old_pid"`
	IntervalSec   int    `json:"interval_sec"`
	MaxRestarts   int    `json:"max_restarts"`
	SuccessChecks int    `json:"success_checks"`
	TempSelf      string `json:"temp_self"`
}

// LoadFile 读取 JSON 配置并转成带默认值的 Config。
func LoadFile(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var file FileConfig
	if err := json.Unmarshal(raw, &file); err != nil {
		return Config{}, fmt.Errorf("解析守护配置 %s: %w", path, err)
	}
	return file.toConfig(), nil
}

func (f FileConfig) toConfig() Config {
	cfg := Config{
		MainPath:      f.MainPath,
		NewPath:       f.NewPath,
		BackupPath:    f.BackupPath,
		DataDir:       f.DataDir,
		ExpectedVer:   f.ExpectedVer,
		HealthURL:     f.HealthURL,
		OldPID:        f.OldPID,
		Interval:      time.Duration(f.IntervalSec) * time.Second,
		MaxRestarts:   f.MaxRestarts,
		SuccessChecks: f.SuccessChecks,
		TempSelf:      f.TempSelf,
	}
	cfg.applyDefaults()
	return cfg
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.MaxRestarts <= 0 {
		c.MaxRestarts = DefaultMaxRestarts
	}
	if c.SuccessChecks <= 0 {
		c.SuccessChecks = DefaultSuccessChecks
	}
	if c.HealthURL == "" {
		c.HealthURL = defaultHealthURL
	}
}

// 可替换的钩子：单测里换成假实现就能验证状态机，不碰真实进程/文件/网络。
var (
	killPID    = defaultKillPID
	spawn      = defaultSpawn
	health     = checkHealth
	copyBinary = defaultCopyBinary
	removeFile = os.Remove
	sleepFn    = time.Sleep
	// spawnDelayedDelete 在 Windows 上运行中的自身删不掉时，起一个延迟删除进程。
	spawnDelayedDelete = defaultSpawnDelayedDelete
)

// Run 执行守护逻辑，阻塞到成功/回滚/出错，返回结论。
func Run(cfg Config) Outcome {
	cfg.applyDefaults()

	// 1. 备份旧主程序（回滚的依据）。主程序不存在（首次安装）就没有备份。
	if cfg.MainPath != "" && cfg.BackupPath != "" {
		if info, err := os.Stat(cfg.MainPath); err == nil && !info.IsDir() {
			if err := copyBinary(cfg.MainPath, cfg.BackupPath); err != nil {
				slog.Warn("守护：备份旧主程序失败", "main", cfg.MainPath, "error", err)
			}
		}
	}

	// 2. 停掉升级前的旧进程，否则 Windows 上覆盖不了主程序。
	if cfg.OldPID > 0 {
		if err := killPID(cfg.OldPID); err != nil {
			slog.Warn("守护：停旧进程失败", "pid", cfg.OldPID, "error", err)
		}
		sleepFn(killSettle)
	}

	// 3. 用新版本覆盖主程序。
	if cfg.NewPath != "" {
		if err := copyBinary(cfg.NewPath, cfg.MainPath); err != nil {
			slog.Error("守护：覆盖主程序失败", "error", err)
			return OutcomeError
		}
	}

	// 4. 拉起主程序，进入巡检。
	_ = spawn(cfg.MainPath, cfg.DataDir)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	var downTicks, restarts, successTick int
	for range ticker.C {
		version, served := health(cfg.HealthURL)
		want := served && (cfg.ExpectedVer == "" || version == cfg.ExpectedVer)
		if want {
			downTicks = 0
			successTick++
			if successTick >= cfg.SuccessChecks {
				cleanup(cfg)
				slog.Info("守护：新版本已稳定服务，清理并退出", "version", version)
				return OutcomeSuccess
			}
			continue
		}
		// 不健康（没服务，或版本还没到期望的新版本）。
		successTick = 0
		downTicks++
		if downTicks < downTicksBeforeAct {
			continue
		}
		// 连续多个周期都没服务：先看是不是该回滚了。
		if restarts >= cfg.MaxRestarts {
			slog.Warn("守护：连续拉起失败，回滚到旧版本", "restarts", restarts)
			if cfg.BackupPath != "" {
				_ = copyBinary(cfg.BackupPath, cfg.MainPath)
			}
			_ = spawn(cfg.MainPath, cfg.DataDir)
			// 回滚后不再守护：节点已回到旧版本，交给系统自启动兜底。
			cleanupAfterRollback(cfg)
			return OutcomeRolledBack
		}
		slog.Warn("守护：主程序未服务，重新拉起", "restarts", restarts, "version", version, "served", served)
		_ = spawn(cfg.MainPath, cfg.DataDir)
		restarts++
		downTicks = 0
	}
	return OutcomeError
}

// cleanup 升级成功后的清理：删掉没用上的新版本副本、旧版本备份，以及守护进程自身。
func cleanup(cfg Config) {
	_ = removeFile(cfg.NewPath)
	_ = removeFile(cfg.BackupPath)
	removeSelf(cfg.TempSelf)
}

// cleanupAfterRollback 回滚后的清理：删掉没用上的新版本副本与守护进程自身，
// 保留备份（它此刻就是当前主程序的内容，留着无害）。
func cleanupAfterRollback(cfg Config) {
	_ = removeFile(cfg.NewPath)
	removeSelf(cfg.TempSelf)
}

// removeSelf 删掉守护进程自身的可执行文件。Windows 上运行中的自身删不掉，
// 此时起一个延迟删除进程，等本进程退出后再删。
func removeSelf(path string) {
	if path == "" {
		if exe, err := os.Executable(); err == nil {
			path = exe
		}
	}
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil {
		spawnDelayedDelete(path)
	}
}

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

// defaultKillPID 结束指定进程。os.Process.Kill 在 Unix 发 SIGKILL、在 Windows 调 TerminateProcess。
func defaultKillPID(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

// defaultCopyBinary 把 src 覆盖到 dst：先写临时文件再改名，避免中途失败留下半截文件。
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
	// Windows 上 os.Rename 会替换已存在的目标（Go 用 MoveFileEx REPLACE_EXISTING）。
	return os.Rename(tmp, dst)
}
