//go:build darwin

package autostart

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	darwinLabel  = "com.npa.desktop"
	darwinMethod = "launchd"
)

type launchdPlist struct {
	XMLName          xml.Name `xml:"plist"`
	Version          string   `xml:"version,attr"`
	Label            string   `xml:"label"`
	ProgramArguments struct {
		Items []string `xml:"string"`
	} `xml:"programArguments"`
	RunAtLoad bool `xml:"runAtLoad"`
	KeepAlive bool `xml:"keepAlive"`
}

// launchctl 是可替换钩子，单测不会真的动系统。
var launchctl = func(args ...string) error {
	output, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func darwinPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", darwinLabel+".plist"), nil
}

func darwinDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func platformStatus() (Status, error) {
	path, err := darwinPlistPath()
	if err != nil {
		return Status{Supported: true, Method: darwinMethod}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Status{Supported: true, Method: darwinMethod, Path: path}, nil
		}
		return Status{Supported: true, Method: darwinMethod, Path: path}, err
	}
	status := Status{Supported: true, Method: darwinMethod, Path: path}
	if info != nil {
		status.Enabled = true
	}
	if raw, readErr := os.ReadFile(path); readErr == nil {
		status.Command = strings.Join(launchdProgramArgs(string(raw)), " ")
	}
	return status, nil
}

// launchdProgramArgs 从 plist 文本里取出 ExecStart，仅用于状态展示。
func launchdProgramArgs(content string) []string {
	var parsed launchdPlist
	if err := xml.Unmarshal([]byte(content), &parsed); err != nil {
		return nil
	}
	return parsed.ProgramArguments.Items
}

func platformEnable(dataDir string) error {
	args, err := arguments(dataDir)
	if err != nil {
		return err
	}
	path, err := darwinPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var document launchdPlist
	document.Version = "1.0"
	document.Label = darwinLabel
	document.ProgramArguments.Items = args
	document.RunAtLoad = true
	document.KeepAlive = false

	raw, err := xml.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append([]byte(xml.Header), raw...), 0o644); err != nil {
		return err
	}
	// 先按新机制 bootstrap；老系统回退 load -w
	if err := launchctl("bootstrap", darwinDomain(), path); err != nil {
		if fallbackErr := launchctl("load", "-w", path); fallbackErr != nil {
			return fmt.Errorf("写入启动项成功但加载失败: %v / %v", err, fallbackErr)
		}
	}
	return nil
}

func platformDisable() error {
	path, err := darwinPlistPath()
	if err != nil {
		return err
	}
	// 未加载时 bootout 会报错，忽略即可；文件不存在也直接成功
	_ = launchctl("bootout", darwinDomain()+"/"+darwinLabel)
	_ = launchctl("unload", "-w", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
