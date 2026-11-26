//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"

	"github.com/npa/npa/internal/state"
)

const (
	windowsRunKey    = `Software\Microsoft\Windows\CurrentVersion\Run`
	windowsValueName = "NPA"
	windowsMethod    = "registry"
)

// Status 读取 HKCU 下的 Run 值，判断是否已配置开机自启（无需管理员权限）。
func platformStatus() (Status, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKey, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return Status{Supported: true, Method: windowsMethod}, nil
		}
		return Status{Supported: true, Method: windowsMethod}, err
	}
	defer key.Close()
	command, _, err := key.GetStringValue(windowsValueName)
	if errors.Is(err, registry.ErrNotExist) {
		return Status{Supported: true, Method: windowsMethod}, nil
	}
	if err != nil {
		return Status{Supported: true, Method: windowsMethod}, err
	}
	return Status{
		Supported: true,
		Enabled:   strings.TrimSpace(command) != "",
		Method:    windowsMethod,
		Path:      `HKCU\` + windowsRunKey + `\` + windowsValueName,
		Command:   command,
	}, nil
}

func platformEnable(dataDir string) error {
	command, err := hiddenCommandLine(dataDir)
	if err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, windowsRunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue(windowsValueName, command)
}

// hiddenCommandLine 生成开机自启动要执行的命令行：用 PowerShell 的
// Start-Process -WindowStyle Hidden 拉起本体，避免登录时弹出控制台窗口。
func hiddenCommandLine(dataDir string) (string, error) {
	path, err := executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	psArgs := make([]string, 0, 2)
	dataDir = strings.TrimSpace(dataDir)
	if dataDir != "" {
		if def, err := state.DefaultDataDir(); err == nil && filepath.Clean(dataDir) != filepath.Clean(def) {
			psArgs = append(psArgs, psQuote("--data-dir"), psQuote(dataDir))
		}
	}
	ps := "Start-Process -FilePath " + psQuote(path)
	if len(psArgs) > 0 {
		ps += " -ArgumentList " + strings.Join(psArgs, ",")
	}
	ps += " -WindowStyle Hidden"
	return fmt.Sprintf(`powershell.exe -NoProfile -WindowStyle Hidden -Command "%s"`, ps), nil
}

// psQuote 用单引号包住 PowerShell 字符串，内部单引号翻倍。
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Disable 幂等：没配置过也返回成功。
func platformDisable() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKey, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	defer key.Close()
	if err := key.DeleteValue(windowsValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}
