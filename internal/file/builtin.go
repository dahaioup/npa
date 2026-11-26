package file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/plugin"
	sshx "github.com/npa/npa/internal/ssh"
)

const (
	maxReadSize  = 4 << 20
	maxWriteSize = 4 << 20
)

// Builtin 是文件能力的内置插件实现：纯 Go、随 core 编译进来，
// 按「节点 HTTP 文件面 → 跨域代理 → SFTP」三级选路把文件送到远端节点。
//
// 它实现 Provider 契约，与外部文件插件进程（PluginProvider）可互换：
// 外部插件在跑时接管全部文件操作，插件缺失时由它兜底。
type Builtin struct {
	SSH *sshx.Manager
	// IsPeer 判断节点是否为反向接入（peer）节点；
	// PeerFS 返回该节点上的 SFTP 会话。两者由 service 注入，
	// 文件页 / MCP file 工具因此无需感知底层是 SSH 还是反向链路。
	IsPeer func(nodeID string) bool
	PeerFS func(ctx context.Context, nodeID string) (*sftp.Client, func(), error)
	// PeerHTTPBase 返回 peer 节点的 HTTP 文件面基址（如 http://10.0.0.2:56701）。
	// 大文件上传/下载优先走它，失败再回退 SFTP-over-WS 数据通道。
	PeerHTTPBase func(nodeID string) string
	// RelayDial 跨域兜底：节点不在本机直连链路上时，经持有上级代理拨到
	// 它自己的 mesh 端口（127.0.0.1:56701），HTTP 文件面照样可用。
	RelayDial func(ctx context.Context, nodeID string) (net.Conn, error)
}

// NewBuiltin 创建内置文件插件实现。
func NewBuiltin(sshManager *sshx.Manager) *Builtin { return &Builtin{SSH: sshManager} }

// builtinMeta 是文件分发能力作为内置插件的自描述信息。
var builtinMeta = plugin.Builtin{
	ID:       "npa.file",
	Name:     "文件分发",
	Version:  "1",
	Protocol: "core/1",
	Capabilities: []string{
		"file.list", "file.stat", "file.read", "file.write", "file.upload",
		"file.download", "file.mkdir", "file.remove", "file.rename",
	},
	Permission: "core.file",
	// 内置插件也自己声明参数：端口是文件面 HTTP 服务的落点，
	// 由插件设置页统一渲染（core 不再单独摆一份表单）。
	Settings: []plugin.SettingSpec{
		{
			Key: "port", Label: "文件服务端口", Type: "number", Setting: "file_plugin_port",
			Default: "56751", Description: "文件插件 HTTP 服务监听的端口",
		},
	},
}

// Info 返回内置插件的元数据（与外部插件同形）。
func (b *Builtin) Info() plugin.Info { return builtinMeta.Info() }

// httpClient 仅控制拨号/响应头超时，整体传输时长由 ctx 控制（大文件不限总时长）。
var httpClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 5 * time.Second,
	},
}

func (s *Builtin) httpBase(nodeID string) string {
	if s.PeerHTTPBase == nil {
		return ""
	}
	return s.PeerHTTPBase(nodeID)
}

// relayHTTPClient 返回一个把所有连接都经 RelayDial 拨到节点 mesh 端口的
// HTTP 客户端。禁用 keep-alive：每个请求重拨一次，避免连接复用与单连接冲突。
func relayHTTPClient(ctx context.Context, nodeID string, dial func(context.Context, string) (net.Conn, error)) *http.Client {
	// 代理管道建一条要跨域协商一次（成本高），必须开 keep-alive 复用，
	// 否则每个 HTTP 请求都重新协商+挤占节点反向池，连续调用就会 take 超时。
	return &http.Client{
		Transport: &http.Transport{
			MaxConnsPerHost: 2,
			MaxIdleConns:    2,
			IdleConnTimeout: 30 * time.Second,
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return dial(ctx, nodeID)
			},
		},
	}
}

// httpTarget 解析出该节点可用的 HTTP 客户端与基址；直连基址优先，
// 其次经上级代理（跨域）。都没有返回 ok=false。
func (s *Builtin) httpTarget(ctx context.Context, nodeID string) (*http.Client, string, bool) {
	if base := s.httpBase(nodeID); base != "" {
		return httpClient, base, true
	}
	if s.RelayDial != nil {
		return relayHTTPClient(ctx, nodeID, s.RelayDial), "http://mesh", true
	}
	return nil, "", false
}

// httpTargetList 按优先顺序给出全部可用目标：直连基址在前、上级代理兜底。
// 跨域节点带着注册地址（如 192.168.2.192）但本机不可达，直连失败后必须
// 还有代理档可用，否则会一路掉进 SFTP 回退（占通道池）。
func (s *Builtin) httpTargetList(ctx context.Context, nodeID string) []httpTargetEntry {
	var entries []httpTargetEntry
	if base := s.httpBase(nodeID); base != "" {
		entries = append(entries, httpTargetEntry{client: httpClient, base: base})
	}
	if s.RelayDial != nil {
		entries = append(entries, httpTargetEntry{client: relayHTTPClient(ctx, nodeID, s.RelayDial), base: "http://mesh"})
	}
	return entries
}

type httpTargetEntry struct {
	client *http.Client
	base   string
}

// httpTry 按优先顺序在全部 HTTP 目标上执行 fn，任一成功即返回 true；
// 全部失败返回 false，由调用方决定是否回退 SFTP 通道。
func (s *Builtin) httpTry(ctx context.Context, nodeID string, fn func(client *http.Client, base string) error) bool {
	var lastErr error
	for _, target := range s.httpTargetList(ctx, nodeID) {
		if err := fn(target.client, target.base); err == nil {
			return true
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		slog.Warn("peer http file op failed on all targets", "node", nodeID, "error", lastErr)
	}
	return false
}

// httpJSON 在节点的 HTTP 文件面上调用一次 JSON 接口。目标节点有直连基址
// （反向接入/同网段发现）或可经上级代理时返回 done=true；失败由调用方决定
// 是否回退 SSH 通道。
func (s *Builtin) httpJSON(ctx context.Context, nodeID, method, endpoint string, payload []byte, out any) (bool, error) {
	targets := s.httpTargetList(ctx, nodeID)
	if len(targets) == 0 {
		return false, nil
	}
	var lastErr error
	for _, target := range targets {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, target.base+endpoint, body)
		if err != nil {
			return true, err
		}
		resp, err := target.client.Do(req)
		if err != nil {
			lastErr = err
			continue // 直连失败换代理档再试
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			message, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			return true, apperrors.New(apperrors.NodeOffline, fmt.Sprintf("node file api %d: %s", resp.StatusCode, strings.TrimSpace(string(message))))
		}
		if out == nil {
			return true, nil
		}
		return true, json.NewDecoder(resp.Body).Decode(out)
	}
	return true, lastErr
}

// httpFileEntry 已移入 provider.go（fileEntry）：节点 HTTP 文件面与文件
// 插件协议共用同一条目结构。

func (s *Builtin) httpUploadWith(ctx context.Context, client *http.Client, base, remotePath string, source io.Reader) (int64, error) {
	target := base + "/api/file/upload?path=" + url.QueryEscape(remotePath)
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var sent atomic.Int64
	counting := &progressReader{r: source, n: &sent}
	stopWatch := watchProgress(&sent, ioIdleTimeout, cancel)
	defer stopWatch()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, target, counting)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return sent.Load(), err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return sent.Load(), fmt.Errorf("node returned %d: %s", resp.StatusCode, string(body))
	}
	// 大小回执校验：节点实际落盘字节数必须与发送量一致。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var ack struct {
		Bytes int64 `json:"bytes"`
	}
	_ = json.Unmarshal(body, &ack)
	if ack.Bytes > 0 && ack.Bytes != sent.Load() {
		return sent.Load(), fmt.Errorf("upload size mismatch: sent %d, node wrote %d", sent.Load(), ack.Bytes)
	}
	return sent.Load(), nil
}

// countingReader 统计已发给节点的字节数（上传报告要用真实发送量，
// 不能用响应体长度）。
type countingReader struct {
	io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += int64(n)
	return n, err
}

// ioIdleTimeout 客户端侧传输空闲上限：连续这么久没有任何字节进展就取消
// 请求——大文件不限总时长，但静默死连接（NAT 断流 / 节点消失）必须能发现。
const ioIdleTimeout = 60 * time.Second

// progressReader 把读取进度记进原子计数器，供看门狗检测。
type progressReader struct {
	r io.Reader
	n *atomic.Int64
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n.Add(int64(n))
	return n, err
}

// watchProgress 启动看门狗：计数器连续 idle 无进展就调用 cancel 中断传输。
// 返回停止函数（同步等待看门狗退出，无泄漏、无竞态）。
func watchProgress(counter *atomic.Int64, idle time.Duration, cancel func()) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(idle / 2)
		defer ticker.Stop()
		last := counter.Load()
		lastChange := time.Now()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				cur := counter.Load()
				if cur != last {
					last = cur
					lastChange = time.Now()
					continue
				}
				if time.Since(lastChange) >= idle {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(stop); <-done }
}

// watchedBody 包住下载响应体：读进度喂给看门狗，Close 时同步停表。
type watchedBody struct {
	io.ReadCloser
	n    *atomic.Int64
	stop func()
}

func (w *watchedBody) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	w.n.Add(int64(n))
	return n, err
}

func (w *watchedBody) Close() error {
	w.stop()
	return w.ReadCloser.Close()
}

func (s *Builtin) httpOpenReadWith(ctx context.Context, client *http.Client, base, remotePath string) (io.ReadCloser, error) {
	target := base + "/api/file/download?path=" + url.QueryEscape(remotePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithCancel(ctx)
	req = req.WithContext(reqCtx)
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("node returned %d: %s", resp.StatusCode, string(body))
	}
	var progress atomic.Int64
	stopWatch := watchProgress(&progress, ioIdleTimeout, cancel)
	return &watchedBody{ReadCloser: resp.Body, n: &progress, stop: stopWatch}, nil
}

func cleanPath(value string) string {
	if value == "" {
		return "."
	}
	if path.IsAbs(value) {
		return path.Clean(value)
	}
	return path.Clean(value)
}

func (s *Builtin) client(ctx context.Context, nodeID string) (*sftp.Client, func(), error) {
	if s.IsPeer != nil && s.IsPeer(nodeID) {
		if s.PeerFS == nil {
			return nil, func() {}, apperrors.New(apperrors.NotSupported, "反向链路文件能力未启用")
		}
		return s.PeerFS(ctx, nodeID)
	}
	sshClient, err := s.SSH.Client(ctx, nodeID)
	if err != nil {
		return nil, func() {}, err
	}
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		return nil, func() {}, apperrors.Wrap(apperrors.NodeOffline, "cannot start SFTP", err)
	}
	return sftpClient, func() { _ = sftpClient.Close() }, nil
}

func (s *Builtin) List(ctx context.Context, nodeID, remotePath string) ([]model.FileEntry, error) {
	var listed []fileEntry
	if done, err := s.httpJSON(ctx, nodeID, http.MethodGet, "/api/file/list?path="+url.QueryEscape(remotePath), nil, &listed); done {
		if err == nil {
			entries := make([]model.FileEntry, 0, len(listed))
			for _, entry := range listed {
				entries = append(entries, entry.model())
			}
			return entries, nil
		}
		slog.Warn("node file api list failed; falling back to sftp", "node", nodeID, "error", err)
	}
	client, closeClient, err := s.client(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	defer closeClient()
	infos, err := client.ReadDir(cleanPath(remotePath))
	if err != nil {
		return nil, apperrors.Wrap(apperrors.NodeOffline, "cannot list remote directory", err)
	}
	entries := make([]model.FileEntry, 0, len(infos))
	for _, info := range infos {
		entryPath := path.Join(cleanPath(remotePath), info.Name())
		entries = append(entries, model.FileEntry{
			Name: info.Name(), Path: entryPath, Size: info.Size(), Mode: info.Mode().String(),
			ModTime: info.ModTime().UTC(), IsDir: info.IsDir(),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

func (s *Builtin) Stat(ctx context.Context, nodeID, remotePath string) (model.FileEntry, error) {
	var entry fileEntry
	if done, err := s.httpJSON(ctx, nodeID, http.MethodGet, "/api/file/stat?path="+url.QueryEscape(remotePath), nil, &entry); done {
		if err == nil {
			return entry.model(), nil
		}
		slog.Warn("node file api stat failed; falling back to sftp", "node", nodeID, "error", err)
	}
	client, closeClient, err := s.client(ctx, nodeID)
	if err != nil {
		return model.FileEntry{}, err
	}
	defer closeClient()
	info, err := client.Stat(cleanPath(remotePath))
	if err != nil {
		return model.FileEntry{}, apperrors.Wrap(apperrors.NodeOffline, "cannot stat remote path", err)
	}
	return model.FileEntry{
		Name: path.Base(cleanPath(remotePath)), Path: cleanPath(remotePath), Size: info.Size(),
		Mode: info.Mode().String(), ModTime: info.ModTime().UTC(), IsDir: info.IsDir(),
	}, nil
}

func (s *Builtin) Read(ctx context.Context, nodeID, remotePath string) ([]byte, error) {
	if reader, err := s.OpenRead(ctx, nodeID, remotePath); err == nil {
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, maxReadSize+1))
		if err != nil {
			return nil, apperrors.Wrap(apperrors.NodeOffline, "cannot read remote file", err)
		}
		if len(data) > maxReadSize {
			return nil, apperrors.New(apperrors.InvalidArgument, "file exceeds MCP read limit; use download")
		}
		return data, nil
	}
	client, closeClient, err := s.client(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	defer closeClient()
	file, err := client.Open(cleanPath(remotePath))
	if err != nil {
		return nil, apperrors.Wrap(apperrors.NodeOffline, "cannot open remote file", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxReadSize+1))
	if err != nil {
		return nil, apperrors.Wrap(apperrors.NodeOffline, "cannot read remote file", err)
	}
	if len(data) > maxReadSize {
		return nil, apperrors.New(apperrors.InvalidArgument, "file exceeds MCP read limit; use download")
	}
	return data, nil
}

func (s *Builtin) OpenRead(ctx context.Context, nodeID, remotePath string) (io.ReadCloser, error) {
	var result io.ReadCloser
	if s.httpTry(ctx, nodeID, func(client *http.Client, base string) error {
		reader, err := s.httpOpenReadWith(ctx, client, base, remotePath)
		if err != nil {
			return err
		}
		result = reader
		return nil
	}) {
		return result, nil
	} else if s.httpBase(nodeID) != "" {
		slog.Warn("peer http download failed; falling back to sftp channel", "node", nodeID)
	}
	client, closeClient, err := s.client(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	file, err := client.Open(cleanPath(remotePath))
	if err != nil {
		closeClient()
		return nil, apperrors.Wrap(apperrors.NodeOffline, "cannot open remote file", err)
	}
	return &readCloser{ReadCloser: file, closeAll: func() { file.Close(); closeClient() }}, nil
}

type readCloser struct {
	io.ReadCloser
	closeAll func()
	once     bool
}

func (r *readCloser) Close() error {
	if r.once {
		return nil
	}
	r.once = true
	r.closeAll()
	return nil
}

func (s *Builtin) Write(ctx context.Context, nodeID, remotePath string, data []byte) error {
	if len(data) > maxWriteSize {
		return apperrors.New(apperrors.InvalidArgument, "file exceeds write limit")
	}
	if s.httpTry(ctx, nodeID, func(client *http.Client, base string) error {
		target := base + "/api/file/write?path=" + url.QueryEscape(remotePath)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return apperrors.New(apperrors.NodeOffline, "node file api write status "+resp.Status)
		}
		return nil
	}) {
		return nil // HTTP 全部成功
	}
	// HTTP 不可用则继续走 SFTP 通道
	client, closeClient, err := s.client(ctx, nodeID)
	if err != nil {
		return err
	}
	defer closeClient()
	file, err := client.Create(cleanPath(remotePath))
	if err != nil {
		return apperrors.Wrap(apperrors.NodeOffline, "cannot create remote file", err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return apperrors.Wrap(apperrors.NodeOffline, "cannot write remote file", err)
	}
	return nil
}

func (s *Builtin) CreateReader(ctx context.Context, nodeID, remotePath string) (io.Reader, error) {
	reader, err := s.OpenRead(ctx, nodeID, remotePath)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func (s *Builtin) Upload(ctx context.Context, nodeID, remotePath string, source io.Reader) (int64, error) {
	// 先整读进内存再发：HTTP 失败回退 SFTP 时数据不会丢。
	if data, readErr := io.ReadAll(source); readErr == nil {
		var written int64
		if s.httpTry(ctx, nodeID, func(client *http.Client, base string) error {
			result, err := s.httpUploadWith(ctx, client, base, remotePath, bytes.NewReader(data))
			if err != nil {
				return err
			}
			written = result
			return nil
		}) {
			return written, nil
		}
		slog.Warn("peer http upload failed; falling back to sftp channel", "node", nodeID)
		source = bytes.NewReader(data)
	} else {
		slog.Warn("peer http upload skipped (source unreadable); using sftp channel", "node", nodeID, "error", readErr)
	}
	client, closeClient, err := s.client(ctx, nodeID)
	if err != nil {
		return 0, err
	}
	defer closeClient()
	file, err := client.Create(cleanPath(remotePath))
	if err != nil {
		return 0, apperrors.Wrap(apperrors.NodeOffline, "cannot create remote file", err)
	}
	defer file.Close()
	written, err := io.Copy(file, source)
	if err != nil {
		return written, apperrors.Wrap(apperrors.NodeOffline, "cannot upload remote file", err)
	}
	return written, nil
}

func (s *Builtin) Remove(ctx context.Context, nodeID, remotePath string) error {
	if done, err := s.httpJSON(ctx, nodeID, http.MethodPost, "/api/file/remove?path="+url.QueryEscape(remotePath), nil, nil); done {
		if err == nil {
			return nil
		}
		slog.Warn("node file api remove failed; falling back to sftp", "node", nodeID, "error", err)
	}
	client, closeClient, err := s.client(ctx, nodeID)
	if err != nil {
		return err
	}
	defer closeClient()
	info, err := client.Stat(cleanPath(remotePath))
	if err != nil {
		return apperrors.Wrap(apperrors.NodeOffline, "cannot stat remote path", err)
	}
	if info.IsDir() {
		err = client.RemoveDirectory(cleanPath(remotePath))
	} else {
		err = client.Remove(cleanPath(remotePath))
	}
	if err != nil {
		return apperrors.Wrap(apperrors.NodeOffline, "cannot remove remote path", err)
	}
	return nil
}

func (s *Builtin) Mkdir(ctx context.Context, nodeID, remotePath string) error {
	if done, err := s.httpJSON(ctx, nodeID, http.MethodPost, "/api/file/mkdir?path="+url.QueryEscape(remotePath), nil, nil); done {
		if err == nil {
			return nil
		}
		slog.Warn("node file api mkdir failed; falling back to sftp", "node", nodeID, "error", err)
	}
	client, closeClient, err := s.client(ctx, nodeID)
	if err != nil {
		return err
	}
	defer closeClient()
	if err := client.MkdirAll(cleanPath(remotePath)); err != nil {
		return apperrors.Wrap(apperrors.NodeOffline, "cannot create remote directory", err)
	}
	return nil
}

func (s *Builtin) Rename(ctx context.Context, nodeID, oldPath, newPath string) error {
	endpoint := "/api/file/rename?from=" + url.QueryEscape(oldPath) + "&to=" + url.QueryEscape(newPath)
	if done, err := s.httpJSON(ctx, nodeID, http.MethodPost, endpoint, nil, nil); done {
		if err == nil {
			return nil
		}
		slog.Warn("node file api rename failed; falling back to sftp", "node", nodeID, "error", err)
	}
	client, closeClient, err := s.client(ctx, nodeID)
	if err != nil {
		return err
	}
	defer closeClient()
	if err := client.Rename(cleanPath(oldPath), cleanPath(newPath)); err != nil {
		return apperrors.Wrap(apperrors.NodeOffline, "cannot rename remote path", err)
	}
	return nil
}

func (s *Builtin) Touch(ctx context.Context, nodeID, remotePath string) error {
	client, closeClient, err := s.client(ctx, nodeID)
	if err != nil {
		return err
	}
	defer closeClient()
	file, err := client.Create(cleanPath(remotePath))
	if err != nil {
		return apperrors.Wrap(apperrors.NodeOffline, "cannot create remote file", err)
	}
	return file.Close()
}

func (s *Builtin) TouchTime(_ context.Context, _ string, _ string, _ time.Time) error {
	return apperrors.New(apperrors.NotSupported, "setting file times is not supported in Phase 1")
}
