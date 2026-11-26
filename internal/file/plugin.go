package file

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/plugin"
)

// ErrPayloadTooLarge 表示插件通道的载荷超限（base64 JSON-RPC 通道上限 4 MiB）。
//
// 门面据此回退内置实现：内置走流式 HTTP/SFTP 通道，不受此限，
// 因此升级包推送这类大文件不会因为装了外部插件就发不出去。
// 插件实现方遇到超过通道上限的文件应直接报超限错，不要硬传。
var ErrPayloadTooLarge = errors.New("file plugin payload exceeds channel limit")

// Invoker 是插件调用面：internal/plugin.Manager 满足它，测试用替身实现。
type Invoker interface {
	Invoke(ctx context.Context, method string, params any, result any) error
	Status() (string, string)
	Info() plugin.Info
}

// PluginProvider 把外部文件插件进程适配成 Provider。
//
// 协议：JSON-RPC 2.0 over LSP 风格 framing（见 internal/plugin），
// 方法与参数：
//
//	file.list     {node_id, path}                     -> {entries: [fileEntry]}
//	file.stat     {node_id, path}                     -> fileEntry
//	file.read     {node_id, path}                     -> {content_base64}
//	file.download {node_id, path}                     -> {content_base64}
//	file.write    {node_id, path, content_base64}     -> {}
//	file.upload   {node_id, path, content_base64}     -> {bytes}
//	file.mkdir    {node_id, path}                     -> {}
//	file.remove   {node_id, path}                     -> {}
//	file.rename   {node_id, old_path, new_path}       -> {}
//	file.touch    {node_id, path}                     -> {}
//
// 字节载荷走 base64，单次载荷上限与内置实现一致（4 MiB）。
// 载荷超限（读回的响应或待写入的数据）一律报 ErrPayloadTooLarge，
// 门面据此回退内置实现：内置走流式通道，大文件照样能传。
// 插件实现方遇到超过通道上限的文件应直接报超限错，不要硬传。
type PluginProvider struct {
	invoker Invoker
}

// NewPluginProvider 用插件管理器（或任意 Invoker 替身）创建适配器。
func NewPluginProvider(invoker Invoker) *PluginProvider {
	return &PluginProvider{invoker: invoker}
}

// Available 报告插件进程是否处于运行状态。
func (p *PluginProvider) Available() bool {
	if p == nil || p.invoker == nil {
		return false
	}
	status, _ := p.invoker.Status()
	return status == "running"
}

// Info 返回插件自报的元数据；插件不在运行时返回内置插件元数据。
func (p *PluginProvider) Info() plugin.Info {
	if p == nil || p.invoker == nil {
		return builtinMeta.Info()
	}
	return p.invoker.Info()
}

// fileParams 是 file.* 方法的公共参数。
type fileParams struct {
	NodeID     string `json:"node_id"`
	Path       string `json:"path"`
	OldPath    string `json:"old_path,omitempty"`
	NewPath    string `json:"new_path,omitempty"`
	ContentB64 string `json:"content_base64,omitempty"`
}

func (p *PluginProvider) List(ctx context.Context, nodeID, remotePath string) ([]model.FileEntry, error) {
	var result struct {
		Entries []fileEntry `json:"entries"`
	}
	if err := p.invoke(ctx, "file.list", fileParams{NodeID: nodeID, Path: remotePath}, &result); err != nil {
		return nil, err
	}
	entries := make([]model.FileEntry, 0, len(result.Entries))
	for _, entry := range result.Entries {
		entries = append(entries, entry.model())
	}
	return entries, nil
}

func (p *PluginProvider) Stat(ctx context.Context, nodeID, remotePath string) (model.FileEntry, error) {
	var result fileEntry
	if err := p.invoke(ctx, "file.stat", fileParams{NodeID: nodeID, Path: remotePath}, &result); err != nil {
		return model.FileEntry{}, err
	}
	return result.model(), nil
}

func (p *PluginProvider) Read(ctx context.Context, nodeID, remotePath string) ([]byte, error) {
	var result struct {
		Content string `json:"content_base64"`
	}
	if err := p.invoke(ctx, "file.read", fileParams{NodeID: nodeID, Path: remotePath}, &result); err != nil {
		return nil, err
	}
	return decodePayload(result.Content, maxReadSize, "read")
}

func (p *PluginProvider) OpenRead(ctx context.Context, nodeID, remotePath string) (io.ReadCloser, error) {
	var result struct {
		Content string `json:"content_base64"`
	}
	if err := p.invoke(ctx, "file.download", fileParams{NodeID: nodeID, Path: remotePath}, &result); err != nil {
		return nil, err
	}
	data, err := decodePayload(result.Content, maxReadSize, "download")
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (p *PluginProvider) Write(ctx context.Context, nodeID, remotePath string, data []byte) error {
	if len(data) > maxWriteSize {
		return apperrors.Wrap(apperrors.InvalidArgument, "file exceeds write limit", ErrPayloadTooLarge)
	}
	params := fileParams{NodeID: nodeID, Path: remotePath, ContentB64: base64.StdEncoding.EncodeToString(data)}
	return p.invoke(ctx, "file.write", params, nil)
}

func (p *PluginProvider) Upload(ctx context.Context, nodeID, remotePath string, source io.Reader) (int64, error) {
	data, err := io.ReadAll(io.LimitReader(source, maxWriteSize+1))
	if err != nil {
		return 0, apperrors.Wrap(apperrors.Internal, "cannot read upload source", err)
	}
	if len(data) > maxWriteSize {
		return 0, apperrors.Wrap(apperrors.InvalidArgument, "file exceeds upload limit", ErrPayloadTooLarge)
	}
	var result struct {
		Bytes int64 `json:"bytes"`
	}
	params := fileParams{NodeID: nodeID, Path: remotePath, ContentB64: base64.StdEncoding.EncodeToString(data)}
	if err := p.invoke(ctx, "file.upload", params, &result); err != nil {
		return 0, err
	}
	if result.Bytes <= 0 {
		return int64(len(data)), nil
	}
	return result.Bytes, nil
}

func (p *PluginProvider) Remove(ctx context.Context, nodeID, remotePath string) error {
	return p.invoke(ctx, "file.remove", fileParams{NodeID: nodeID, Path: remotePath}, nil)
}

func (p *PluginProvider) Mkdir(ctx context.Context, nodeID, remotePath string) error {
	return p.invoke(ctx, "file.mkdir", fileParams{NodeID: nodeID, Path: remotePath}, nil)
}

func (p *PluginProvider) Rename(ctx context.Context, nodeID, oldPath, newPath string) error {
	params := fileParams{NodeID: nodeID, OldPath: oldPath, NewPath: newPath}
	return p.invoke(ctx, "file.rename", params, nil)
}

func (p *PluginProvider) Touch(ctx context.Context, nodeID, remotePath string) error {
	return p.invoke(ctx, "file.touch", fileParams{NodeID: nodeID, Path: remotePath}, nil)
}

// TouchTime 不支持：文件插件协议里没有改时间戳的方法，与内置实现一致。
func (p *PluginProvider) TouchTime(_ context.Context, _ string, _ string, _ time.Time) error {
	return apperrors.New(apperrors.NotSupported, "setting file times is not supported by file plugins")
}

func (p *PluginProvider) CreateReader(ctx context.Context, nodeID, remotePath string) (io.Reader, error) {
	reader, err := p.OpenRead(ctx, nodeID, remotePath)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

// invoke 调用一次插件方法。插件进程不在时返回 plugin.ErrRuntimeMissing，
// 门面据此回退内置实现。
func (p *PluginProvider) invoke(ctx context.Context, method string, params any, result any) error {
	if p == nil || p.invoker == nil {
		return plugin.ErrRuntimeMissing
	}
	return p.invoker.Invoke(ctx, method, params, result)
}

// decodePayload 解插件返回的 base64 载荷，并卡住大小上限。
func decodePayload(value string, limit int, kind string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, apperrors.New(apperrors.Internal, fmt.Sprintf("file plugin returned invalid base64 %s payload", kind))
	}
	if len(data) > limit {
		return nil, apperrors.Wrap(apperrors.InvalidArgument, fmt.Sprintf("file plugin %s payload exceeds limit", kind), ErrPayloadTooLarge)
	}
	return data, nil
}
