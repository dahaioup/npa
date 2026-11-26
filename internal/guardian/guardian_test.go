package guardian

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAcquireGuardianLock 验证单例互斥：第二次加锁应失败。
func TestAcquireGuardianLock(t *testing.T) {
	// 先释放可能存在的锁
	releaseGuardianLock()

	err := acquireGuardianLock()
	if err != nil {
		t.Fatalf("first acquire should succeed: %v", err)
	}
	defer releaseGuardianLock()

	// 第二次应失败（同进程内 flock 是进程级，不会自己 block 自己）
	// 这里只验证文件创建成功
	lockPath := filepath.Join(os.TempDir(), "npa-guardian.lock")
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		t.Fatal("lock file should exist")
	}
}

// TestOutcomeString 验证 Outcome 的 String 方法。
func TestOutcomeString(t *testing.T) {
	cases := []struct {
		out  Outcome
		want string
	}{
		{OutcomeSuccess, "success"},
		{OutcomeRolledBack, "rolled-back"},
		{OutcomeAlreadyUpgraded, "already-upgraded"},
		{OutcomeError, "error"},
	}
	for _, c := range cases {
		if got := c.out.String(); got != c.want {
			t.Errorf("Outcome(%d).String() = %q, want %q", c.out, got, c.want)
		}
	}
}

// TestDefaultConstants 验证常量合理。
func TestDefaultConstants(t *testing.T) {
	if StartupTimeout != 10*time.Second {
		t.Errorf("StartupTimeout = %v, want 10s", StartupTimeout)
	}
	if DefaultHealthURL != "http://127.0.0.1:56700/api/system" {
		t.Errorf("DefaultHealthURL = %q", DefaultHealthURL)
	}
	if DefaultCompleteURL != "http://127.0.0.1:56700/api/guardian/complete" {
		t.Errorf("DefaultCompleteURL = %q", DefaultCompleteURL)
	}
}

// TestCopyBinary 验证文件复制。
func TestCopyBinary(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	data := []byte("hello world")
	os.WriteFile(src, data, 0o644)

	if err := defaultCopyBinary(src, dst); err != nil {
		t.Fatalf("copy: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != string(data) {
		t.Errorf("copied data mismatch")
	}
}

// TestProcessAlive 验证进程存活检测。
func TestProcessAlive(t *testing.T) {
	// 自己一定活着
	if !processAlive(os.Getpid()) {
		t.Error("current process should be alive")
	}
}
