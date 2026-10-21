package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
}

type Manager struct {
	directory string
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
	entries, err := os.ReadDir(m.directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrRuntimeMissing
		}
		return "", err
	}
	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(m.directory, entry.Name())
		info, err := executableInfo(path)
		if err != nil || !IsExecutable(info) {
			continue
		}
		if entry.Name() == "npa-desktop" || entry.Name() == "npa-desktop.exe" {
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
	process, err := Start(ctx, path)
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
