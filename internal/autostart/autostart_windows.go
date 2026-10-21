//go:build windows

package autostart

import (
	"errors"
	"strings"

	"golang.org/x/sys/windows/registry"
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
	command, err := commandLine(dataDir)
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
