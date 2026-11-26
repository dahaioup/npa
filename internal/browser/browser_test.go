package browser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCatalogKeepsObscuraToolNames 内嵌目录必须与 obscura 的工具名一致：
// NPA 只加 node_id，不改工具语义。
func TestCatalogKeepsObscuraToolNames(t *testing.T) {
	tools := Catalog()
	if len(tools) == 0 {
		t.Fatal("catalog is empty")
	}
	names := ToolNames()
	for _, want := range []string{
		"browser_navigate", "browser_snapshot", "browser_click", "browser_fill",
		"browser_type", "browser_press_key", "browser_select_option", "browser_evaluate",
		"browser_wait_for", "browser_wait_for_text", "browser_wait_for_function",
		"browser_network_requests", "browser_console_messages", "browser_close",
		"browser_screenshot", "browser_pdf", "browser_markdown", "browser_links",
		"browser_tab_new", "browser_tab_list", "browser_tab_switch", "browser_tab_close",
	} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("catalog is missing tool %q", want)
		}
	}
	for _, tool := range tools {
		if !strings.HasPrefix(tool.Name, "browser_") {
			t.Errorf("unexpected tool name %q", tool.Name)
		}
	}
}

// TestWithNodeIDKeepsOriginalProperties 注入 node_id 不能动 obscura 自己的参数。
func TestWithNodeIDKeepsOriginalProperties(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`)
	updated := WithNodeID(schema)
	var document map[string]any
	if err := json.Unmarshal(updated, &document); err != nil {
		t.Fatalf("unmarshal updated schema: %v", err)
	}
	properties, _ := document["properties"].(map[string]any)
	if properties == nil {
		t.Fatal("properties missing")
	}
	if _, ok := properties["url"]; !ok {
		t.Error("original property url was dropped")
	}
	node, ok := properties[nodeIDProperty].(map[string]any)
	if !ok {
		t.Fatalf("%s property missing", nodeIDProperty)
	}
	if node["type"] != "string" {
		t.Errorf("%s should be a string, got %v", nodeIDProperty, node["type"])
	}
	if document["type"] != "object" {
		t.Errorf("type should stay object, got %v", document["type"])
	}
}

// TestWithNodeIDOnEmptySchema 空 schema 也要能注入。
func TestWithNodeIDOnEmptySchema(t *testing.T) {
	updated := WithNodeID(nil)
	var document map[string]any
	if err := json.Unmarshal(updated, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if document["type"] != "object" {
		t.Errorf("type should be object, got %v", document["type"])
	}
}

// TestSplitNodeID 摘出 node_id 后，剩下的参数才是工具自己的参数。
func TestSplitNodeID(t *testing.T) {
	arguments := map[string]any{"node_id": "node-1", "url": "https://example.com"}
	nodeID, rest := SplitNodeID(arguments)
	if nodeID != "node-1" {
		t.Errorf("node id = %q, want node-1", nodeID)
	}
	if _, ok := rest["node_id"]; ok {
		t.Error("node_id should be removed from the tool arguments")
	}
	if rest["url"] != "https://example.com" {
		t.Errorf("url argument lost: %v", rest)
	}
}

// TestResolveBinaryPrefersConfiguredPath 显式配置优先，其次数据目录 bin/，最后 PATH。
func TestResolveBinaryPrefersConfiguredPath(t *testing.T) {
	dir := t.TempDir()
	configured := filepath.Join(dir, "configured-obscura")
	writeExecutable(t, configured)
	dataDir := filepath.Join(dir, "data")
	installed := filepath.Join(dataDir, "bin", binaryName())
	writeExecutable(t, installed)

	if got := ResolveBinary(configured, dataDir); got != configured {
		t.Errorf("configured path should win, got %q", got)
	}
	if got := ResolveBinary("", dataDir); got != installed {
		t.Errorf("data dir binary should win over PATH, got %q", got)
	}
	if got := ResolveBinary(filepath.Join(dir, "missing"), dataDir); got != "" {
		t.Errorf("missing configured binary should resolve to empty, got %q", got)
	}
	if got := ResolveBinary("", filepath.Join(dir, "empty-data")); got != "" && got != binaryName() {
		// PATH 里可能恰好装了 obscura，这里只保证返回的是绝对路径或空。
		if !filepath.IsAbs(got) {
			t.Errorf("resolved path should be absolute, got %q", got)
		}
	}
}

// TestCLIProviderToolList CLI 回退只暴露真正能跑的工具。
func TestCLIProviderToolList(t *testing.T) {
	provider := NewCLIProvider(func() Config { return Config{} })
	tools, err := provider.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("CLI provider exposes no tools")
	}
	allowed := map[string]bool{}
	for name := range cliTools {
		allowed[name] = true
	}
	for _, tool := range tools {
		if !allowed[tool.Name] {
			t.Errorf("CLI fallback should not expose %q", tool.Name)
		}
	}
	if !allowed["browser_navigate"] || !allowed["browser_evaluate"] {
		t.Error("CLI fallback must cover navigate and evaluate")
	}
}

// TestCLIProviderRejectsInteractiveTools 交互类工具在 CLI 回退里必须明确报错，
// 不能假装成功。
func TestCLIProviderRejectsInteractiveTools(t *testing.T) {
	provider := NewCLIProvider(func() Config { return Config{BinaryPath: "/nonexistent/obscura"} })
	_, err := provider.Call(context.Background(), CallRequest{Tool: "browser_click"})
	if err == nil {
		t.Fatal("expected an error for an interactive tool")
	}
	if !strings.Contains(err.Error(), "browser_click") {
		t.Errorf("error should name the tool, got %v", err)
	}
}

// TestCLIProviderNeedsURL CLI 没有会话，没有 URL 就得报缺参数。
func TestCLIProviderNeedsURL(t *testing.T) {
	provider := NewCLIProvider(func() Config { return Config{BinaryPath: "/nonexistent/obscura"} })
	_, err := provider.Call(context.Background(), CallRequest{Tool: "browser_snapshot"})
	if err == nil {
		t.Fatal("expected an error when no url is known")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Errorf("error should mention the missing url, got %v", err)
	}
}

// TestAssetName 平台与形态的组合必须映射到官方发布包的名字。
func TestAssetName(t *testing.T) {
	cases := []struct {
		goos    string
		goarch  string
		variant string
		want    string
		wantErr bool
	}{
		{goos: "darwin", goarch: "amd64", want: "obscura-x86_64-macos.tar.gz"},
		{goos: "darwin", goarch: "arm64", want: "obscura-aarch64-macos.tar.gz"},
		{goos: "linux", goarch: "amd64", want: "obscura-x86_64-linux.tar.gz"},
		{goos: "linux", goarch: "arm64", want: "obscura-aarch64-linux.tar.gz"},
		{goos: "windows", goarch: "amd64", want: "obscura-x86_64-windows.zip"},
		{goos: "darwin", goarch: "amd64", variant: "stealth", want: "obscura-x86_64-macos-stealth.tar.gz"},
		{goos: "darwin", goarch: "amd64", variant: "no-render", want: "obscura-x86_64-macos-no-render.tar.gz"},
		{goos: "windows", goarch: "arm64", wantErr: true},
		{goos: "plan9", goarch: "amd64", wantErr: true},
		{goos: "darwin", goarch: "riscv64", wantErr: true},
		{goos: "darwin", goarch: "amd64", variant: "bogus", wantErr: true},
	}
	for _, test := range cases {
		got, err := AssetName(test.goos, test.goarch, test.variant)
		if test.wantErr {
			if err == nil {
				t.Errorf("AssetName(%s/%s,%s) should fail", test.goos, test.goarch, test.variant)
			}
			continue
		}
		if err != nil {
			t.Errorf("AssetName(%s/%s,%s): %v", test.goos, test.goarch, test.variant, err)
			continue
		}
		if got != test.want {
			t.Errorf("AssetName(%s/%s,%s) = %q, want %q", test.goos, test.goarch, test.variant, got, test.want)
		}
	}
}

// TestInstallerStatusReflectsInstalledBinary 安装状态要跟着磁盘上的实际情况走。
func TestInstallerStatusReflectsInstalledBinary(t *testing.T) {
	dir := t.TempDir()
	installer := &Installer{DataDir: dir}
	if installer.Status().Installed {
		t.Error("a fresh data dir must not look installed")
	}
	if err := os.MkdirAll(installer.InstallDir(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeExecutable(t, installer.BinaryPath())
	status := installer.Status()
	if !status.Installed {
		t.Error("installed binary should be reported as installed")
	}
	if status.Path != installer.BinaryPath() {
		t.Errorf("path = %q, want %q", status.Path, installer.BinaryPath())
	}
}

// TestInstallerStateRoundTrip sidecar 状态文件要能读回来。
func TestInstallerStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	installer := &Installer{DataDir: dir}
	if err := os.MkdirAll(installer.InstallDir(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	encoded, err := json.Marshal(InstallState{Version: "0.2.7", Variant: "stealth", Asset: "obscura-x86_64-macos-stealth.tar.gz"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(installer.statePath(), encoded, 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
	state, ok := installer.State()
	if !ok {
		t.Fatal("state should be readable")
	}
	if state.Version != "0.2.7" || state.Variant != "stealth" {
		t.Errorf("state = %+v", state)
	}
	status := installer.Status()
	if status.Version != "0.2.7" || status.Variant != "stealth" {
		t.Errorf("status = %+v", status)
	}
}

// TestTruncate 截断要标出被截掉的长度。
func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short text should pass through, got %q", got)
	}
	got := truncate(strings.Repeat("a", 100), 10)
	if !strings.HasPrefix(got, strings.Repeat("a", 10)) {
		t.Errorf("truncated text should keep the head, got %q", got)
	}
	if !strings.Contains(got, "100 bytes total") {
		t.Errorf("truncated text should report the original size, got %q", got)
	}
}

// writeExecutable 造一个可执行文件（内容无意义，只要存在且可执行）。
func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
}
