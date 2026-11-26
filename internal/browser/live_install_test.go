package browser

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// TestLiveInstallAgainstGitHub 是一次性的人工验证脚本（默认跳过）：
// 真的从 GitHub release 下载 obscura，验证「查 release → 选资产 → 下载 →
// 解包 → 自检 → 写 sidecar」整条链路。跑法：
//
//	NPA_LIVE_INSTALL=1 go test ./internal/browser/ -run TestLiveInstallAgainstGitHub -v
func TestLiveInstallAgainstGitHub(t *testing.T) {
	if os.Getenv("NPA_LIVE_INSTALL") != "1" {
		t.Skip("set NPA_LIVE_INSTALL=1 to run the live download")
	}
	dir := t.TempDir()
	installer := &Installer{DataDir: dir, Variant: "no-render"}
	installer.Progress = func(downloaded, total int64) {
		if total > 0 {
			t.Logf("downloaded %d/%d bytes", downloaded, total)
		}
	}
	result, err := installer.Install(context.Background())
	if err != nil {
		t.Fatalf("live install: %v", err)
	}
	t.Logf("installed %s at %s", result.Version, result.Path)
	info, err := os.Stat(result.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	t.Logf("size: %d bytes", info.Size())
	out, err := exec.Command(result.Path, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("obscura --help: %v (%s)", err, out)
	}
	if len(out) == 0 {
		t.Fatal("obscura --help produced no output")
	}
	t.Logf("obscura --help ok, %d bytes of output", len(out))
}
