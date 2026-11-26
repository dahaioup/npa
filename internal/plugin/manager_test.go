package plugin

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeExecutable 在目录里放一个可执行文件（模拟发布包里的插件进程）。
func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestFindExecutablePrefersNamedPlugin 目录里有多个可执行文件时，优先挑
// PreferredName 指定的那个。
func TestFindExecutablePrefersNamedPlugin(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, dir, "aaa-other-plugin")
	want := writeExecutable(t, dir, "npa-obscura")

	manager := NewManager(dir)
	manager.PreferredName = "npa-obscura"
	got, err := manager.findExecutable()
	if err != nil {
		t.Fatalf("findExecutable: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestFindExecutableMatchesPlatformName 发布包里的插件按
// "<name>-<goos>-<goarch>" 命名，一个包里装着所有平台的产物，
// 运行时必须挑出自己平台的那一个。
func TestFindExecutableMatchesPlatformName(t *testing.T) {
	dir := t.TempDir()
	// 造两个别的平台的产物，确认它们不会被选中。
	for _, tag := range []string{"plan9-amd64", "windows-amd64"} {
		writeExecutable(t, dir, "npa-obscura-"+tag)
	}
	want := writeExecutable(t, dir, "npa-obscura-"+runtime.GOOS+"-"+runtime.GOARCH)

	manager := NewManager(dir)
	manager.PreferredName = "npa-obscura"
	got, err := manager.findExecutable()
	if err != nil {
		t.Fatalf("findExecutable: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestFindExecutableSearchesExtraDirs 主目录里没有插件时，去 SearchDirs 里找。
func TestFindExecutableSearchesExtraDirs(t *testing.T) {
	primary := t.TempDir()
	extra := t.TempDir()
	want := writeExecutable(t, extra, "npa-obscura")

	manager := NewManager(primary)
	manager.PreferredName = "npa-obscura"
	manager.SearchDirs = []string{primary, extra}
	got, err := manager.findExecutable()
	if err != nil {
		t.Fatalf("findExecutable: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestFindExecutableMissingRuntime 目录不存在或为空时报告 ErrRuntimeMissing，
// 上层据此回退到内置实现。
func TestFindExecutableMissingRuntime(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "does-not-exist"))
	manager.PreferredName = "npa-obscura"
	if _, err := manager.findExecutable(); err != ErrRuntimeMissing {
		t.Errorf("err = %v, want ErrRuntimeMissing", err)
	}
}
