package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/npa/npa/internal/state"
)

// buildPluginManager 交叉编译出一个本平台的插件管理器可执行文件。
func buildPluginManager(t *testing.T, target string) {
	t.Helper()
	command := exec.Command("go", "build", "-o", target, "github.com/npa/npa/cmd/npa-plugin-manager")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("cannot build the plugin manager: %v\n%s", err, output)
	}
}

// TestLiveBuiltinInstallsFromPublicRepository 走内置实现的完整链路：
//
//	core → 内置插件管理器 → HTTP → 公有仓库
//
// 不放任何外部插件进程，验证「不装 npa-plugin-manager 也能装插件」。
//
// 需要外网，默认跳过；本地验证时用 NPA_LIVE_REGISTRY=1 打开。
func TestLiveBuiltinInstallsFromPublicRepository(t *testing.T) {
	if os.Getenv("NPA_LIVE_REGISTRY") != "1" {
		t.Skip("set NPA_LIVE_REGISTRY=1 to run against the public repository")
	}
	dataDir := t.TempDir()

	store, err := state.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)
	defer app.Close()

	// 确认没有外部覆盖进程：这一步走的就是内置实现。
	status, err := app.PluginManagerStatus(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status["plugin_status"] != "builtin" {
		t.Fatalf("plugin_status = %v, want builtin", status["plugin_status"])
	}
	t.Logf("registry url = %v", status["registry_url"])

	listing, err := app.PluginList(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	plugins, _ := listing["plugins"].([]map[string]any)
	if len(plugins) == 0 {
		t.Fatal("the public repository returned no plugins")
	}
	t.Logf("plugins = %d", len(plugins))

	result, err := app.PluginInstall(context.Background(), "npa-obscura", "")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Logf("install result: %+v", result)

	plugin := filepath.Join(dataDir, "plugins", "browser", "npa-obscura-"+runtime.GOOS+"-"+runtime.GOARCH)
	if runtime.GOOS == "windows" {
		plugin += ".exe"
	}
	if _, err := os.Stat(plugin); err != nil {
		t.Fatalf("plugin binary missing: %v", err)
	}
	engine := filepath.Join(dataDir, "bin", "obscura")
	if runtime.GOOS == "windows" {
		engine += ".exe"
	}
	if _, err := os.Stat(engine); err != nil {
		t.Fatalf("engine binary missing: %v", err)
	}
}

// TestLivePluginManagerInstallsFromPublicRepository 走外部覆盖实现的完整链路：
//
//	core → 插件协议(stdio JSON-RPC) → npa-plugin-manager → HTTP → 公有仓库
//
// 装一个插件及其依赖，验证索引、下载、校验、解包、落盘都通。
//
// 需要外网，默认跳过；本地验证时用 NPA_LIVE_REGISTRY=1 打开。
func TestLivePluginManagerInstallsFromPublicRepository(t *testing.T) {
	if os.Getenv("NPA_LIVE_REGISTRY") != "1" {
		t.Skip("set NPA_LIVE_REGISTRY=1 to run against the public repository")
	}
	dataDir := t.TempDir()
	pluginDir := filepath.Join(dataDir, "plugins", "manager")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "npa-plugin-manager"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	buildPluginManager(t, filepath.Join(pluginDir, name))

	store, err := state.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store)
	app.StartPluginManager(context.Background())
	defer app.Close()

	listing, err := app.PluginList(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	plugins, _ := listing["plugins"].([]any)
	if len(plugins) == 0 {
		t.Fatal("the public repository returned no plugins")
	}
	t.Logf("registry url = %v, plugins = %d", listing["registry_url"], len(plugins))

	result, err := app.PluginInstall(context.Background(), "npa-obscura", "")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Logf("install result: %+v", result)

	plugin := filepath.Join(dataDir, "plugins", "browser", "npa-obscura-"+runtime.GOOS+"-"+runtime.GOARCH)
	if runtime.GOOS == "windows" {
		plugin += ".exe"
	}
	if _, err := os.Stat(plugin); err != nil {
		t.Fatalf("plugin binary missing: %v", err)
	}
	engine := filepath.Join(dataDir, "bin", "obscura")
	if runtime.GOOS == "windows" {
		engine += ".exe"
	}
	if _, err := os.Stat(engine); err != nil {
		t.Fatalf("engine binary missing: %v", err)
	}
}
