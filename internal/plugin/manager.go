package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

type Info struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Protocol     string   `json:"protocol"`
	Capabilities []string `json:"capabilities"`
	// Permission 是该能力在权限模型里的命名（如 core.file）。外部插件可
	// 省略；内置插件用它自报权限归属。
	Permission string `json:"permission,omitempty"`
	// Settings 是插件自己声明的可配置参数。core 按这份声明渲染「插件设置」
	// 页面，用户改的值和插件自报的值都落回设置库。
	Settings []SettingSpec `json:"settings,omitempty"`
}

type Manager struct {
	directory string
	// SearchDirs 是额外的插件查找目录（按顺序）。directory 找不到时再逐个找。
	// 例如插件既可能放在工作目录的 plugins/ 下，也可能放在数据目录里。
	SearchDirs []string
	// PreferredName 是优先选用的可执行文件名（如 "npa-obscura"）。
	// 目录里可能有多个可执行文件时优先用它，否则取按名排序的第一个。
	PreferredName string
	// PreferredNames 是额外的优先文件名（按顺序）。发布包里插件常按
	// "<name>-<goos>-<goarch>" 命名，一个包里同时装着所有平台的产物，
	// 运行时需要挑出自己平台的那一个。
	PreferredNames []string
	// Args 是启动插件进程时附带的参数。
	Args []string
	// Env 是插件进程的环境变量（附加到 os.Environ() 之后）。
	Env []string
	// InitTimeout 是插件 initialize 握手的超时（默认 5s）。
	// 留成字段是为了在慢盘/高负载环境（比如并行跑测试）里能放宽。
	InitTimeout time.Duration
	mu          sync.Mutex
	process     *Process
	info        Info
	status      string
	lastError   string
}

func NewManager(directory string) *Manager {
	return &Manager{directory: directory, status: "detected"}
}

func (m *Manager) Status() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status, m.lastError
}

func (m *Manager) Info() Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.info
}

func (m *Manager) findExecutable() (string, error) {
	if path, err := m.findIn(m.directory); err == nil {
		return path, nil
	} else if !errors.Is(err, ErrRuntimeMissing) {
		return "", err
	}
	for _, dir := range m.SearchDirs {
		if dir == "" {
			continue
		}
		path, err := m.findIn(dir)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, ErrRuntimeMissing) {
			return "", err
		}
	}
	return "", ErrRuntimeMissing
}

// findIn 在一个目录里挑可执行文件：npa-desktop 优先，其次 PreferredName /
// PreferredNames，最后取按名排序的第一个。
func (m *Manager) findIn(directory string) (string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrRuntimeMissing
		}
		return "", err
	}
	preferred := m.preferredNames()
	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := executableInfo(path)
		if err != nil || !IsExecutable(info) {
			continue
		}
		if entry.Name() == "npa-desktop" || entry.Name() == "npa-desktop.exe" {
			return path, nil
		}
		if preferred[entry.Name()] {
			return path, nil
		}
		candidates = append(candidates, path)
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", ErrRuntimeMissing
	}
	return candidates[0], nil
}

// preferredNames 汇总优先文件名：固定的 PreferredName/PreferredNames，加上
// 发布包里按平台命名的组合（如 "npa-obscura-darwin-amd64"）。
func (m *Manager) preferredNames() map[string]bool {
	names := map[string]bool{}
	add := func(name string) {
		if name == "" {
			return
		}
		names[name] = true
		names[name+".exe"] = true
	}
	add(m.PreferredName)
	for _, name := range m.PreferredNames {
		add(name)
	}
	base := m.PreferredName
	if base == "" {
		base = "npa-plugin"
	}
	for _, platform := range platformTags() {
		add(base + "-" + platform)
	}
	return names
}

// platformTags 列出当前平台在发布包里可能用到的文件名后缀。
func platformTags() []string {
	tags := []string{runtime.GOOS + "-" + runtime.GOARCH}
	switch runtime.GOARCH {
	case "amd64":
		tags = append(tags, runtime.GOOS+"-x86_64")
	case "arm64":
		tags = append(tags, runtime.GOOS+"-aarch64")
	}
	return tags
}

func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.process != nil && m.process.Alive() {
		return nil
	}
	path, err := m.findExecutable()
	if err != nil {
		m.status, m.lastError = "runtime_missing", err.Error()
		return err
	}
	m.status, m.lastError = "probing", ""
	process, err := StartWithEnv(ctx, path, m.Env, m.Args...)
	if err != nil {
		m.status, m.lastError = "failed", err.Error()
		return err
	}
	m.process = process
	go func() {
		<-process.done
		m.mu.Lock()
		if m.process == process {
			m.process = nil
			m.status, m.lastError = "failed", ErrCrashed.Error()
		}
		m.mu.Unlock()
	}()
	initTimeout := m.InitTimeout
	if initTimeout <= 0 {
		initTimeout = 5 * time.Second
	}
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	var info Info
	if err := process.Request(initCtx, "plugin.initialize", map[string]any{"protocol_version": "1"}, &info); err != nil {
		_ = process.Close()
		m.process = nil
		m.status, m.lastError = "incompatible", err.Error()
		return err
	}
	m.info, m.status = info, "running"
	return nil
}

func (m *Manager) Invoke(ctx context.Context, method string, params any, result any) error {
	m.mu.Lock()
	process := m.process
	m.mu.Unlock()
	if process == nil || !process.Alive() {
		return ErrRuntimeMissing
	}
	return process.Request(ctx, method, params, result)
}

func (m *Manager) Health(ctx context.Context) error {
	var result json.RawMessage
	return m.Invoke(ctx, "plugin.health", struct{}{}, &result)
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	process := m.process
	m.process = nil
	m.status, m.lastError = "stopped", ""
	m.mu.Unlock()
	if process == nil {
		return nil
	}
	if err := process.Shutdown(ctx); err != nil && !errors.Is(err, ErrCrashed) {
		_ = process.Close()
		return err
	}
	return process.Close()
}
