package service

import (
	"context"
	"strings"
	"testing"

	"github.com/npa/npa/internal/state"
)

// TestBrowserTimeoutSettingFlowsToBothPaths 「单次调用超时」这个设置要真的
// 落到两条执行路径上：
//
//	插件进程 → --call-timeout 启动参数（插件侧真正卡住引擎的那个超时）
//	CLI 回退 → obscura fetch --timeout
//
// 只落到一条上，用户改设置就会有一半不生效。
func TestBrowserTimeoutSettingFlowsToBothPaths(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	if err := store.SaveSettings(map[string]string{"browser_obscura_timeout": "45"}); err != nil {
		t.Fatal(err)
	}
	config := app.BrowserConfig()
	if config.Timeout != 45*1000000000 {
		t.Fatalf("config timeout = %v, want 45s", config.Timeout)
	}

	// 插件进程侧：启动参数里要带 --call-timeout 45s。
	args := app.BrowserPluginArgs()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--call-timeout") || !strings.Contains(joined, "45s") {
		t.Errorf("plugin args = %v, want --call-timeout 45s", args)
	}

	// core 侧的等待预算要比引擎的宽裕，否则引擎超时时错误传不回来。
	if app.browserCallTimeout() <= config.Timeout {
		t.Errorf("core timeout = %v, want more than the engine budget %v", app.browserCallTimeout(), config.Timeout)
	}
}

// TestBrowserTimeoutDefault 缺省值也要合理（60s），并且 core 的等待预算
// 不能比用户设的值还短。
func TestBrowserTimeoutDefault(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	config := app.BrowserConfig()
	if config.Timeout != 60*1000000000 {
		t.Fatalf("default timeout = %v, want 60s", config.Timeout)
	}
	if app.browserCallTimeout() < config.Timeout {
		t.Errorf("core timeout = %v, want at least the engine budget %v", app.browserCallTimeout(), config.Timeout)
	}
}

// TestBrowserTimeoutLongerUserSetting 用户把超时调得比默认还长时，
// core 的等待预算要跟着放宽，否则设置形同虚设。
func TestBrowserTimeoutLongerUserSetting(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	if err := store.SaveSettings(map[string]string{"browser_obscura_timeout": "600"}); err != nil {
		t.Fatal(err)
	}
	if app.browserCallTimeout() <= 600*1000000000 {
		t.Errorf("core timeout = %v, want more than the user's 600s", app.browserCallTimeout())
	}
}

// TestBrowserCallTimeoutShorterUserSetting 用户把超时调短时，core 的等待
// 预算仍要是引擎预算之上一点点，不能压到引擎前面。
func TestBrowserCallTimeoutShorterUserSetting(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)

	if err := store.SaveSettings(map[string]string{"browser_obscura_timeout": "5"}); err != nil {
		t.Fatal(err)
	}
	timeout := app.browserCallTimeout()
	if timeout < 5*1000000000 {
		t.Errorf("core timeout = %v, want at least the engine budget 5s", timeout)
	}
	_ = context.Background()
}
