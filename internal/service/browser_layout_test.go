package service

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/npa/npa/internal/state"
)

// TestReleaseLayoutPicksOwnPlatformPlugin 验证发布包里的插件摆放方式真的能被找到。
//
// 发布包的结构是：
//
//	npa/bin/npa-<goos>-<goarch>          ← 主程序
//	npa/plugins/browser/npa-obscura-<goos>-<goarch>   ← 插件进程（一个包里装着所有平台）
//
// 运行时要挑出「自己平台」那一份，而不是随便拿一个能执行的。
func TestReleaseLayoutPicksOwnPlatformPlugin(t *testing.T) {
	packageDir := t.TempDir()
	pluginDir := filepath.Join(packageDir, "plugins", "browser")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 造三个平台的假插件，只有自己平台那个是真的（会正常响应 initialize）。
	// 每个假插件启动时把自己的文件名记到 marker 里，用来验「挑中了哪一个」。
	marker := filepath.Join(packageDir, "picked.txt")
	for _, tag := range []string{"darwin-amd64", "linux-amd64", "windows-amd64"} {
		name := "npa-obscura-" + tag
		if tag == "windows-amd64" {
			name += ".exe"
		}
		path := filepath.Join(pluginDir, name)
		script := "#!/bin/sh\necho " + name + " > " + marker + "\nexit 1\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ownTag := runtime.GOOS + "-" + runtime.GOARCH
	own := filepath.Join(pluginDir, "npa-obscura-"+ownTag)
	if runtime.GOOS == "windows" {
		own += ".exe"
	}
	script := "#!/bin/sh\necho " + filepath.Base(own) + " > " + marker + "\nexit 1\n"
	if err := os.WriteFile(own, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	// 模拟发布包：插件目录就是包里的 plugins/browser。
	app.BrowserPlugin.SearchDirs = []string{pluginDir}
	app.BrowserPlugin.Args = []string{"--obscura", "/tmp/obscura-dist/obscura"}
	// 假插件起不来是预期的（它们不是真的插件进程）；这里要验的是「挑中了哪一个」。
	if err := app.BrowserPlugin.Start(context.Background()); err == nil {
		defer app.BrowserPlugin.Close(context.Background())
	}
	picked, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatalf("no plugin was launched: %v", readErr)
	}
	if strings.TrimSpace(string(picked)) != filepath.Base(own) {
		t.Errorf("picked plugin = %q, want %q", strings.TrimSpace(string(picked)), filepath.Base(own))
	}
}

// TestBrowserPluginDirsCoverReleaseLayout 确认查找目录覆盖了发布包的两种摆法：
// 插件与主程序同级目录，以及插件在上一级目录（发布包的 plugins/browser）。
func TestBrowserPluginDirsCoverReleaseLayout(t *testing.T) {
	dirs := browserPluginDirs("/data")
	if len(dirs) < 2 {
		t.Fatalf("browserPluginDirs returned %v, want the data dir plus the release layout", dirs)
	}
	if dirs[0] != filepath.Join("/data", "plugins", "browser") {
		t.Errorf("first search dir = %q", dirs[0])
	}
	executable, err := os.Executable()
	if err != nil {
		t.Skip("no executable path available")
	}
	binDir := filepath.Dir(executable)
	want := []string{
		filepath.Join(binDir, "plugins", "browser"),
		filepath.Join(filepath.Dir(binDir), "plugins", "browser"),
	}
	for _, expected := range want {
		found := false
		for _, dir := range dirs {
			if dir == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("search dirs %v do not cover %q", dirs, expected)
		}
	}
}
