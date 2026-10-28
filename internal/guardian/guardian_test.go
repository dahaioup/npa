package guardian

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// withStubs 临时替换守护进程的可替换钩子，测试结束还原，避免污染其它用例。
func withStubs(t *testing.T, k func(int) error, s func(string, string) error, h func(string) (string, bool), c func(string, string) error, r func(string) error) {
	t.Helper()
	ok, os_, oh, oc, or := killPID, spawn, health, copyBinary, removeFile
	if k != nil {
		killPID = k
	}
	if s != nil {
		spawn = s
	}
	if h != nil {
		health = h
	}
	if c != nil {
		copyBinary = c
	}
	if r != nil {
		removeFile = r
	}
	t.Cleanup(func() {
		killPID, spawn, health, copyBinary, removeFile = ok, os_, oh, oc, or
	})
}

// stubSleep 让 killSettle 不真的睡 2 秒。
func stubSleep(t *testing.T) {
	t.Helper()
	orig := sleepFn
	sleepFn = func(time.Duration) {}
	t.Cleanup(func() { sleepFn = orig })
}

func TestLoadFileDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"main_path":"/bin/npa","new_path":"/tmp/npa.new","old_pid":123}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MainPath != "/bin/npa" || cfg.NewPath != "/tmp/npa.new" || cfg.OldPID != 123 {
		t.Fatalf("字段解析错误: %+v", cfg)
	}
	if cfg.Interval != DefaultInterval {
		t.Fatalf("Interval 默认值错误: %v", cfg.Interval)
	}
	if cfg.MaxRestarts != DefaultMaxRestarts {
		t.Fatalf("MaxRestarts 默认值错误: %v", cfg.MaxRestarts)
	}
	if cfg.SuccessChecks != DefaultSuccessChecks {
		t.Fatalf("SuccessChecks 默认值错误: %v", cfg.SuccessChecks)
	}
	if cfg.HealthURL != defaultHealthURL {
		t.Fatalf("HealthURL 默认值错误: %v", cfg.HealthURL)
	}
}

func TestRunSuccessAfterStableHealth(t *testing.T) {
	stubSleep(t)
	var spawns atomic.Int32
	var killed atomic.Int32
	// 永远健康且版本匹配 → 应在 SuccessChecks 个周期后成功。
	withStubs(t,
		func(pid int) error { killed.Add(1); return nil },
		func(string, string) error { spawns.Add(1); return nil },
		func(string) (string, bool) { return "0.4.1-999", true },
		func(string, string) error { return nil },
		func(string) error { return nil },
	)
	cfg := Config{
		MainPath:      "/bin/npa",
		NewPath:       "/tmp/npa.new",
		BackupPath:    "/tmp/npa.old",
		ExpectedVer:   "0.4.1-999",
		OldPID:        4242,
		Interval:      time.Millisecond,
		SuccessChecks: 2,
		MaxRestarts:   6,
	}
	out := Run(cfg)
	if out != OutcomeSuccess {
		t.Fatalf("期望 success，得到 %v", out)
	}
	if killed.Load() != 1 {
		t.Fatalf("应恰好停一次旧进程，得到 %d", killed.Load())
	}
	if spawns.Load() < 1 {
		t.Fatalf("应至少拉起一次主程序，得到 %d", spawns.Load())
	}
}

func TestRunRestartsThenSucceeds(t *testing.T) {
	stubSleep(t)
	var spawns atomic.Int32
	var calls atomic.Int32
	// 前几次不健康，触发重启；之后变健康 → 成功。
	withStubs(t,
		nil,
		func(string, string) error { spawns.Add(1); return nil },
		func(string) (string, bool) {
			if calls.Add(1) <= 4 {
				return "", false // 不健康，逼出重启
			}
			return "0.4.1-999", true
		},
		func(string, string) error { return nil },
		func(string) error { return nil },
	)
	cfg := Config{
		MainPath:      "/bin/npa",
		NewPath:       "/tmp/npa.new",
		ExpectedVer:   "0.4.1-999",
		Interval:      time.Millisecond,
		SuccessChecks: 1,
		MaxRestarts:   6,
	}
	out := Run(cfg)
	if out != OutcomeSuccess {
		t.Fatalf("期望 success，得到 %v", out)
	}
	if spawns.Load() < 2 {
		t.Fatalf("应重启过主程序（初始 1 次 + 重启），得到 %d", spawns.Load())
	}
}

func TestRunRollbackAfterMaxRestarts(t *testing.T) {
	stubSleep(t)
	var spawns atomic.Int32
	var copies []string
	// 永不健康 → 每次都重启，直到 MaxRestarts 后回滚。
	withStubs(t,
		nil,
		func(string, string) error { spawns.Add(1); return nil },
		func(string) (string, bool) { return "", false },
		func(src, dst string) error { copies = append(copies, src+">"+dst); return nil },
		func(string) error { return nil },
	)
	cfg := Config{
		MainPath:   "/bin/npa",
		NewPath:    "/tmp/npa.new",
		BackupPath: "/tmp/npa.old",
		Interval:   time.Millisecond,
		MaxRestarts: 3,
	}
	out := Run(cfg)
	if out != OutcomeRolledBack {
		t.Fatalf("期望 rolled-back，得到 %v", out)
	}
	// 回滚应把备份覆盖回主程序。
	found := false
	for _, c := range copies {
		if c == "/tmp/npa.old>/bin/npa" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未找到回滚复制 备份→主程序，copies=%v", copies)
	}
}

func TestRunVersionMismatchKeepsWaiting(t *testing.T) {
	stubSleep(t)
	var calls atomic.Int32
	// 一直健康但版本是旧的（回滚后/替换失败），不该误判成功。
	withStubs(t,
		nil,
		func(string, string) error { return nil },
		func(string) (string, bool) { calls.Add(1); return "0.4.1-old", true },
		func(string, string) error { return nil },
		func(string) error { return nil },
	)
	cfg := Config{
		MainPath:    "/bin/npa",
		ExpectedVer: "0.4.1-999", // 期望新版本
		Interval:    time.Millisecond,
		// MaxRestarts 很大，确保不会走到回滚，只验证「版本不符不算成功」
		MaxRestarts:   1 << 30,
		SuccessChecks: 2,
	}
	// 跑一小段时间后强停：如果误判成功会立刻返回 success。
	done := make(chan Outcome, 1)
	go func() { done <- Run(cfg) }()
	select {
	case out := <-done:
		t.Fatalf("版本不符时不应返回（得到 %v）", out)
	case <-time.After(50 * time.Millisecond):
		// 仍在等待，符合预期
	}
	if calls.Load() == 0 {
		t.Fatal("应至少巡检过一次")
	}
}

func TestRemoveSelfUsesDelayedDeleteOnFailure(t *testing.T) {
	var delayed atomic.Int32
	orig := spawnDelayedDelete
	spawnDelayedDelete = func(string) { delayed.Add(1) }
	t.Cleanup(func() { spawnDelayedDelete = orig })

	// 指向一个删不掉的路径（非空目录，os.Remove 不会删非空目录）→ remove 失败 → 走延迟删除。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeSelf(dir)
	if delayed.Load() != 1 {
		t.Fatalf("删除失败时应调用延迟删除，得到 %d", delayed.Load())
	}
}
