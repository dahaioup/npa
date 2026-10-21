package autostart

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/npa/npa/internal/state"
)

func withExecutable(t *testing.T, path string) {
	t.Helper()
	original := executable
	executable = func() (string, error) { return path, nil }
	t.Cleanup(func() { executable = original })
}

func TestCommandLineAddsDataDirOnlyWhenCustom(t *testing.T) {
	withExecutable(t, "/opt/npa/bin/npa")

	defaultDir, err := state.DefaultDataDir()
	if err != nil {
		t.Fatal(err)
	}
	command, err := commandLine(defaultDir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(command, "--data-dir") {
		t.Fatalf("默认数据目录不应出现在启动项里: %s", command)
	}

	custom := filepath.Join(t.TempDir(), "npa-state")
	command, err = commandLine(custom)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "--data-dir") || !strings.Contains(command, custom) {
		t.Fatalf("自定义数据目录必须写进启动项: %s", command)
	}
}

func TestCommandLineQuotesPathsWithSpaces(t *testing.T) {
	withExecutable(t, "/opt/npa dir/with space/npa")
	command, err := commandLine("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, `"/opt/npa dir/with space/npa"`) {
		t.Fatalf("含空格的路径需要加引号: %s", command)
	}
	// 拆回来必须还是同一个可执行文件
	parts := splitCommandLine(command)
	if len(parts) != 1 || parts[0] != "/opt/npa dir/with space/npa" {
		t.Fatalf("拆分还原失败: %#v", parts)
	}
}

func TestSplitCommandLineKeepsQuotedArgument(t *testing.T) {
	parts := splitCommandLine(`/opt/npa --data-dir "/tmp/a b"`)
	if len(parts) != 3 || parts[1] != "--data-dir" || parts[2] != "/tmp/a b" {
		t.Fatalf("拆分结果不对: %#v", parts)
	}
}
