package service

// npa-remote 插件 sidecar：NPA 在交互桌面会话里运行，由 NPA 拉起 npa-remote serve
// 才能拿到 DXGI 桌面复制（schtasks 计划任务会话拿不到，见 158 的 "no display" 教训）。

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"log/slog"

	"github.com/npa/npa/internal/peer"
)

const (
	remotePluginPort    = peer.RemotePluginPort
	sidecarCheckPeriod  = 30 * time.Second
	sidecarSpawnBackoff = 10 * time.Second
)

// RemotePluginPath 返回 sidecar 可执行文件的查找位置：
// 1) 设置 remote_plugin_path（绝对路径，优先）
// 2) <dataDir>/plugins/npa-remote[.exe]
func (s *Service) RemotePluginPath() string {
	if p := s.Settings()["remote_plugin_path"]; p != "" {
		return p
	}
	name := "npa-remote"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(s.Store.DataDir(), "plugins", name)
}

func remotePluginAlive(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// spawnRemotePlugin 拉起 serve 模式，日志写到插件同目录 npa-remote.log。
func (s *Service) spawnRemotePlugin() error {
	bin := s.RemotePluginPath()
	if _, err := os.Stat(bin); err != nil {
		return err
	}
	bind := "127.0.0.1"
	if parseBool(s.Settings()["lan_enabled"]) {
		bind = "0.0.0.0"
	}
	logPath := filepath.Join(filepath.Dir(bin), "npa-remote.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(bin, "serve", "--bind", bind, "--port", strconv.Itoa(remotePluginPort))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Linux：插件目录可能带私有共享库（如新版 libvpx），让 sidecar 能找到
	if runtime.GOOS == "linux" {
		pluginDir := filepath.Dir(bin)
		cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+pluginDir+":"+os.Getenv("LD_LIBRARY_PATH"))
	}
	applySysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	slog.Info("remote plugin sidecar started", "bin", bin, "bind", bind, "port", remotePluginPort, "pid", pid)
	return nil
}

var sidecarStarted atomic.Bool

// StartRemoteSidecar 启动 sidecar 及监控循环（进程消失自动拉起）。
func (s *Service) StartRemoteSidecar(ctx context.Context) {
	if !sidecarStarted.CompareAndSwap(false, true) {
		return
	}
	go func() {
		nextSpawn := time.Time{} // 失败退避；成功后下一 tick 仅做存活检查
		ticker := time.NewTicker(sidecarCheckPeriod)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(s.RemotePluginPath()); err == nil {
				// 进程死了（端口不通）且不在退避期内 → 重新拉起。
				// 注意存活检查必须每 tick 都做，不能被 nextSpawn 门禁挡住，
				// 否则插件崩了要等 24h（真实事故：158 上手动 kill 后再没拉起）。
				if !remotePluginAlive(remotePluginPort) && time.Now().After(nextSpawn) {
					if err := s.spawnRemotePlugin(); err == nil {
						nextSpawn = time.Time{}
					} else {
						nextSpawn = time.Now().Add(sidecarSpawnBackoff)
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
