package pluginregistry

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestLiveRegistryFetchAgainstPublicRepository 真实连一次公有仓库，拉下索引。
//
// 需要外网，默认跳过；本地验证时用 NPA_LIVE_REGISTRY=1 打开。
func TestLiveRegistryFetchAgainstPublicRepository(t *testing.T) {
	if os.Getenv("NPA_LIVE_REGISTRY") != "1" {
		t.Skip("set NPA_LIVE_REGISTRY=1 to run against the public repository")
	}
	client := NewClient(DefaultRegistryURL)
	registry, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch registry: %v", err)
	}
	if len(registry.Plugins) == 0 {
		t.Fatal("registry has no entries")
	}
	t.Logf("registry has %d entries: %v", len(registry.Plugins), registry.SortedIDs())
	for _, entry := range registry.Plugins {
		artifact, ok := entry.ArtifactFor(runtime.GOOS, runtime.GOARCH)
		if !ok {
			t.Logf("%s has no build for %s/%s", entry.ID, runtime.GOOS, runtime.GOARCH)
			continue
		}
		if artifact.SHA256 == "" {
			t.Errorf("%s artifact for %s/%s has no sha256", entry.ID, runtime.GOOS, runtime.GOARCH)
		}
		if artifact.URL == "" {
			t.Errorf("%s artifact for %s/%s has no url", entry.ID, runtime.GOOS, runtime.GOARCH)
		}
	}
}

// TestLiveInstallFromPublicRepository 从真实公有仓库装一个插件及其依赖，
// 验证「索引 → 下载 → 校验 sha256 → 解包 → 落盘」整条链路。
//
// 需要外网，默认跳过；本地验证时用 NPA_LIVE_REGISTRY=1 打开。
func TestLiveInstallFromPublicRepository(t *testing.T) {
	if os.Getenv("NPA_LIVE_REGISTRY") != "1" {
		t.Skip("set NPA_LIVE_REGISTRY=1 to run against the public repository")
	}
	client := NewClient(DefaultRegistryURL)
	registry, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch registry: %v", err)
	}
	dataDir := t.TempDir()
	installer := &Installer{DataDir: dataDir, Client: client}
	results, err := installer.Install(context.Background(), registry, "npa-obscura")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	for _, result := range results {
		t.Logf("installed %s %s -> %s (%v)", result.ID, result.Version, result.Dir, result.Files)
	}
	// 插件入口落在 plugins/browser/<名字>-<os>-<arch>
	expected := filepath.Join(dataDir, "plugins", "browser", "npa-obscura-"+runtime.GOOS+"-"+runtime.GOARCH)
	if runtime.GOOS == "windows" {
		expected += ".exe"
	}
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("plugin binary missing: %v", err)
	}
	// 依赖落在 bin/
	engine := filepath.Join(dataDir, "bin", "obscura")
	if runtime.GOOS == "windows" {
		engine += ".exe"
	}
	if _, err := os.Stat(engine); err != nil {
		t.Fatalf("engine binary missing: %v", err)
	}
	entries, err := installer.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("state entries = %d, want 2", len(entries))
	}
}
