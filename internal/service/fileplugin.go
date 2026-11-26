package service

import (
	"context"
	"log/slog"

	"github.com/npa/npa/internal/file"
	"github.com/npa/npa/internal/model"
)

// 文件插件的服务端生命周期。
//
// 文件能力做成 NPA 内置插件：节点文件面（/api/file/*）与插件自己的前端页面
// 都在插件的 HTTP 服务里，端口取设置项 file_plugin_port（默认 56751），
// 绑定范围跟随局域网服务开关 lan_enabled。插件随 NPA 进程内运行，不编译为独立进程。

// StartFilePlugin 启动文件插件的 HTTP 服务。已在运行时是空操作。
func (s *Service) StartFilePlugin(ctx context.Context) error {
	s.filePluginMu.Lock()
	defer s.filePluginMu.Unlock()
	if s.fileServer != nil {
		return nil
	}
	if !s.PluginEnabled(PluginSlotFile) {
		return nil
	}
	server := file.NewServer(model.NodeID(), s.Files, s.filePluginPort())
	server.SetScope(s.filePluginScope())
	// 前端页面用它列节点；本机节点由插件自己补在首位。
	server.Nodes = func() []model.Node {
		nodes, err := s.ListNodes()
		if err != nil {
			return nil
		}
		return nodes
	}
	if err := server.Start(ctx); err != nil {
		return err
	}
	s.fileServer = server
	slog.Info("file plugin server listening", "addr", server.Addr())
	return nil
}

// StopFilePlugin 停止文件插件的 HTTP 服务。
func (s *Service) StopFilePlugin(ctx context.Context) error {
	s.filePluginMu.Lock()
	defer s.filePluginMu.Unlock()
	if s.fileServer == nil {
		return nil
	}
	server := s.fileServer
	s.fileServer = nil
	return server.Close(ctx)
}

// RestartFilePlugin 按当前设置重启文件插件的 HTTP 服务。
// 端口或绑定范围变了才重启；其它设置变化不影响文件插件。
func (s *Service) RestartFilePlugin(ctx context.Context) error {
	port := s.filePluginPort()
	scope := s.filePluginScope()
	s.filePluginMu.Lock()
	running := s.fileServer
	s.filePluginMu.Unlock()
	if running != nil && running.Port() == port && running.Scope() == scope {
		return nil
	}
	if err := s.StopFilePlugin(ctx); err != nil {
		return err
	}
	return s.StartFilePlugin(ctx)
}
