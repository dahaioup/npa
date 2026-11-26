package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
)

// 通道的统一视图层：ssh 通道由 SSH 隧道管理器承载，npa 通道由端口映射管理器承载。
// 这里只做「统一形状 + 统一入口」，运行时仍由各自的管理器负责。

// ListChannels 返回本节点管理的全部通道（ssh + npa），按类别与名称排序。
func (s *Service) ListChannels(ctx context.Context) ([]model.Channel, error) {
	channels := make([]model.Channel, 0, 8)

	tunnels, err := s.Tunnels.List()
	if err != nil {
		return nil, err
	}
	for _, definition := range tunnels {
		channels = append(channels, s.channelFromTunnel(definition))
	}

	mappings, err := s.PortMappings.List()
	if err != nil {
		return nil, err
	}
	for _, definition := range mappings {
		channels = append(channels, s.channelFromMapping(definition))
	}

	// 登记表里的通道（S1 阶段是尚未由隧道/映射承载的记录，例如 S3 的临时隧道）
	// 不与 ssh/npa 两类投影重复。
	stored, err := s.Store.ListChannels()
	if err != nil {
		return nil, err
	}
	for _, definition := range stored {
		switch definition.Category {
		case model.ChannelCategorySSH, model.ChannelCategoryNPA:
			continue
		}
		channels = append(channels, definition)
	}

	sort.Slice(channels, func(i, j int) bool {
		if channels[i].Category != channels[j].Category {
			return channels[i].Category < channels[j].Category
		}
		return channels[i].ID < channels[j].ID
	})
	return channels, nil
}

// channelFromTunnel 把 SSH 隧道转成统一通道视图。
func (s *Service) channelFromTunnel(definition model.Tunnel) model.Channel {
	channel := model.Channel{
		ID:            model.ChannelID(model.ChannelCategorySSH, definition.ID),
		Name:          definition.Name,
		Category:      model.ChannelCategorySSH,
		Kind:          definition.Type,
		ListenAddress: definition.ListenAddress,
		ListenPort:    definition.ListenPort,
		RuntimePort:   s.Tunnels.RuntimePort(definition.ID),
		TargetHost:    definition.TargetHost,
		TargetPort:    definition.TargetPort,
		NodeID:        definition.NodeID,
		ViaNodeID:     definition.ViaNodeID,
		AutoStart:     definition.AutoStart,
		Status:        definition.Status,
		LastError:     definition.LastError,
		CreatedAt:     definition.CreatedAt,
		UpdatedAt:     definition.UpdatedAt,
	}
	if s.Tunnels.IsRunning(definition.ID) {
		channel.Status = model.ChannelStatusRunning
	}
	if channel.Kind == "" {
		channel.Kind = model.ChannelKindLocal
	}
	return channel
}

// channelFromMapping 把 NPA 端口映射转成统一通道视图。
func (s *Service) channelFromMapping(definition model.PortMapping) model.Channel {
	channel := model.Channel{
		ID:            model.ChannelID(model.ChannelCategoryNPA, definition.ID),
		Name:          definition.Name,
		Category:      model.ChannelCategoryNPA,
		Kind:          model.ChannelKindMapping,
		ListenAddress: definition.ListenAddress,
		ListenPort:    definition.ListenPort,
		RuntimePort:   definition.RuntimePort,
		TargetHost:    definition.TargetHost,
		TargetPort:    definition.TargetPort,
		ViaNodeID:     definition.ViaNodeID,
		ViaRoute:      definition.ViaRoute,
		AutoStart:     definition.AutoStart,
		Status:        definition.Status,
		LastError:     definition.LastError,
		CreatedAt:     definition.CreatedAt,
		UpdatedAt:     definition.UpdatedAt,
	}
	if runtimePort, via := s.PortMappings.RuntimeInfo(definition.ID); runtimePort > 0 {
		channel.RuntimePort = runtimePort
		if via != "" {
			channel.ViaRoute = via
		}
	}
	if s.PortMappings.IsRunning(definition.ID) {
		channel.Status = model.ChannelStatusRunning
	}
	return channel
}

// SaveChannel 按通道形态把统一通道定义落到对应的管理器上：
//   - local（本地端口转发）→ NPA 端口映射：本机监听 → 经由节点 → 远程服务
//   - remote（远程端口转发）→ SSH 反向隧道：远程监听 → 本地服务
//   - socks（动态端口转发）→ SSH SOCKS 代理
func (s *Service) SaveChannel(ctx context.Context, definition model.Channel) (model.Channel, error) {
	category, refID, ok := model.ChannelRef(definition.ID)
	if !ok || category == "" {
		category = definition.Category
	}
	switch definition.Kind {
	case model.ChannelKindLocal, model.ChannelKindMapping:
		return s.saveNPAChannel(ctx, definition, refID)
	case model.ChannelKindRemote, model.ChannelKindSocks:
		return s.saveSSHChannel(ctx, definition, refID)
	default:
		switch category {
		case model.ChannelCategorySSH:
			return s.saveSSHChannel(ctx, definition, refID)
		case model.ChannelCategoryNPA:
			return s.saveNPAChannel(ctx, definition, refID)
		default:
			return model.Channel{}, apperrors.New(apperrors.InvalidArgument,
				fmt.Sprintf("unknown channel kind %q (want %q, %q or %q)", definition.Kind,
					model.ChannelKindLocal, model.ChannelKindRemote, model.ChannelKindSocks))
		}
	}
}

// sshCapableNode 判断节点是否录了 SSH 凭据（能作为 SSH 服务端被连接）。
func (s *Service) sshCapableNode(nodeID string) (model.Node, error) {
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		return model.Node{}, err
	}
	switch node.AuthMethod {
	case "key", "password", "agent":
		return node, nil
	default:
		return node, apperrors.New(apperrors.InvalidArgument,
			fmt.Sprintf("节点 %s 没有 SSH 凭据，无法作为 SSH 服务端使用", node.Name))
	}
}

func (s *Service) saveSSHChannel(ctx context.Context, definition model.Channel, refID string) (model.Channel, error) {
	kind := definition.Kind
	if kind == "" {
		kind = model.ChannelKindRemote
	}
	if definition.NodeID == "" {
		return model.Channel{}, apperrors.New(apperrors.InvalidArgument, "请选择远程服务 / 代理服务")
	}
	if _, err := s.sshCapableNode(definition.NodeID); err != nil {
		return model.Channel{}, err
	}
	tunnel := model.Tunnel{
		ID:            refID,
		NodeID:        definition.NodeID,
		ViaNodeID:     definition.ViaNodeID,
		Name:          definition.Name,
		Type:          kind,
		ListenAddress: definition.ListenAddress,
		ListenPort:    definition.ListenPort,
		TargetHost:    definition.TargetHost,
		TargetPort:    definition.TargetPort,
		AutoStart:     definition.AutoStart,
	}
	if kind == model.ChannelKindRemote {
		// 远程端口转发：监听在远程机器上，流量回落到本机的本地服务。
		if tunnel.ListenPort <= 0 || tunnel.ListenPort > 65535 {
			return model.Channel{}, apperrors.New(apperrors.InvalidArgument, "远程端口必须有效")
		}
		if strings.TrimSpace(tunnel.TargetHost) == "" {
			tunnel.TargetHost = "127.0.0.1"
		}
		if tunnel.TargetPort <= 0 || tunnel.TargetPort > 65535 {
			return model.Channel{}, apperrors.New(apperrors.InvalidArgument, "本地端口必须有效")
		}
		if strings.TrimSpace(tunnel.ListenAddress) == "" {
			tunnel.ListenAddress = "127.0.0.1"
		}
	}
	if tunnel.ID == "" {
		tunnel.ID = fmt.Sprintf("tunnel-%d", time.Now().UnixNano())
	}
	if err := s.SaveTunnel(ctx, tunnel); err != nil {
		return model.Channel{}, err
	}
	saved, err := s.Store.GetTunnel(tunnel.ID)
	if err != nil {
		return model.Channel{}, err
	}
	return s.channelFromTunnel(saved), nil
}

func (s *Service) saveNPAChannel(ctx context.Context, definition model.Channel, refID string) (model.Channel, error) {
	mapping := model.PortMapping{
		ID:            refID,
		Name:          definition.Name,
		TargetHost:    strings.TrimSpace(definition.TargetHost),
		TargetPort:    definition.TargetPort,
		ListenAddress: definition.ListenAddress,
		ListenPort:    definition.ListenPort,
		ViaNodeID:     definition.ViaNodeID,
		AutoStart:     definition.AutoStart,
	}
	if mapping.TargetPort <= 0 || mapping.TargetPort > 65535 {
		return model.Channel{}, apperrors.New(apperrors.InvalidArgument, "远程端口必须有效")
	}
	if mapping.TargetHost == "" {
		// 远程服务留空 = 指向经由节点自身。
		if mapping.ViaNodeID == "" {
			return model.Channel{}, apperrors.New(apperrors.InvalidArgument,
				"经由节点为「自动」时必须选择远程服务（某个 NPA 节点）")
		}
		node, err := s.Store.GetNode(mapping.ViaNodeID)
		if err != nil {
			return model.Channel{}, err
		}
		mapping.TargetHost = node.Address
	}
	if mapping.ID == "" {
		mapping.ID = fmt.Sprintf("map-%d", time.Now().UnixNano())
	}
	if err := s.SavePortMapping(ctx, mapping); err != nil {
		return model.Channel{}, err
	}
	saved, err := s.Store.GetPortMapping(mapping.ID)
	if err != nil {
		return model.Channel{}, err
	}
	return s.channelFromMapping(saved), nil
}

// StartChannel 启动一条通道。
func (s *Service) StartChannel(ctx context.Context, id string) (model.Channel, error) {
	category, refID, ok := model.ChannelRef(id)
	if !ok {
		return model.Channel{}, apperrors.New(apperrors.InvalidArgument, fmt.Sprintf("invalid channel id %q", id))
	}
	switch category {
	case model.ChannelCategorySSH:
		definition, err := s.StartTunnel(ctx, refID)
		if err != nil {
			return model.Channel{}, err
		}
		return s.channelFromTunnel(definition), nil
	case model.ChannelCategoryNPA:
		definition, err := s.StartPortMapping(ctx, refID)
		if err != nil {
			return model.Channel{}, err
		}
		return s.channelFromMapping(definition), nil
	default:
		return model.Channel{}, apperrors.New(apperrors.InvalidArgument, fmt.Sprintf("unknown channel category %q", category))
	}
}

// StopChannel 停止一条通道。
func (s *Service) StopChannel(ctx context.Context, id string) error {
	category, refID, ok := model.ChannelRef(id)
	if !ok {
		return apperrors.New(apperrors.InvalidArgument, fmt.Sprintf("invalid channel id %q", id))
	}
	switch category {
	case model.ChannelCategorySSH:
		return s.StopTunnel(ctx, refID)
	case model.ChannelCategoryNPA:
		return s.StopPortMapping(ctx, refID)
	default:
		return apperrors.New(apperrors.InvalidArgument, fmt.Sprintf("unknown channel category %q", category))
	}
}

// DeleteChannel 删除一条通道定义（并停止运行中的实例）。
func (s *Service) DeleteChannel(ctx context.Context, id string) error {
	category, refID, ok := model.ChannelRef(id)
	if !ok {
		return apperrors.New(apperrors.InvalidArgument, fmt.Sprintf("invalid channel id %q", id))
	}
	switch category {
	case model.ChannelCategorySSH:
		return s.DeleteTunnel(ctx, refID)
	case model.ChannelCategoryNPA:
		return s.DeletePortMapping(ctx, refID)
	default:
		return apperrors.New(apperrors.InvalidArgument, fmt.Sprintf("unknown channel category %q", category))
	}
}

// GetChannel 按统一 ID 取一条通道。
func (s *Service) GetChannel(ctx context.Context, id string) (model.Channel, error) {
	category, refID, ok := model.ChannelRef(id)
	if !ok {
		return model.Channel{}, apperrors.New(apperrors.InvalidArgument, fmt.Sprintf("invalid channel id %q", id))
	}
	switch category {
	case model.ChannelCategorySSH:
		definition, err := s.Store.GetTunnel(refID)
		if err != nil {
			return model.Channel{}, err
		}
		return s.channelFromTunnel(definition), nil
	case model.ChannelCategoryNPA:
		definition, err := s.Store.GetPortMapping(refID)
		if err != nil {
			return model.Channel{}, err
		}
		return s.channelFromMapping(definition), nil
	default:
		return model.Channel{}, apperrors.New(apperrors.InvalidArgument, fmt.Sprintf("unknown channel category %q", category))
	}
}

// channelAuditName 供审计使用的通道显示名。
func channelAuditName(channel model.Channel) string {
	name := strings.TrimSpace(channel.Name)
	if name == "" {
		name = channel.ID
	}
	return name
}
