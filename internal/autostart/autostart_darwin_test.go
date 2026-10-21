//go:build darwin

package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinEnableWritesAndRemovesLaunchAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	withExecutable(t, "/opt/npa/bin/npa")
	var loaded [][]string
	original := launchctl
	launchctl = func(args ...string) error {
		loaded = append(loaded, append([]string(nil), args...))
		return nil
	}
	t.Cleanup(func() { launchctl = original })

	custom := filepath.Join(home, "state")
	if err := Enable(custom); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "Library", "LaunchAgents", darwinLabel+".plist")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("启动项文件未生成: %v", err)
	}
	content := string(raw)
	if len(content) < 100 {
		t.Fatalf("plist 内容异常（长度 %d）:\n%s", len(content), content)
	}
	for _, want := range []string{darwinLabel, "/opt/npa/bin/npa", "--data-dir", custom, "<runAtLoad>true</runAtLoad>", "<programArguments>"} {
		if !strings.Contains(content, want) {
			t.Fatalf("plist 缺少 %q:\n%s", want, content)
		}
	}

	status, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Supported || !status.Enabled || status.Method != darwinMethod {
		t.Fatalf("状态不对: %#v", status)
	}
	if len(loaded) == 0 {
		t.Fatal("Enable 应该调用 launchctl 加载启动项")
	}

	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Disable 应该删除 plist")
	}
	status, err = Current()
	if err != nil {
		t.Fatal(err)
	}
	if status.Enabled {
		t.Fatalf("Disable 之后应为未启用: %#v", status)
	}
	// 幂等：再关一次不应报错
	if err := Disable(); err != nil {
		t.Fatalf("Disable 必须幂等: %v", err)
	}
}
