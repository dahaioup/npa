package file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"time"

	"github.com/pkg/sftp"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	sshx "github.com/npa/npa/internal/ssh"
)

const (
	maxReadSize  = 4 << 20
	maxWriteSize = 4 << 20
)

type Service struct {
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

func NewService(sshManager *sshx.Manager) *Service { return &Service{SSH: sshManager} }

// httpClient 仅控制拨号/响应头超时，整体传输时长由 ctx 控制（大文件不限总时长）。
var httpClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 5 * time.Second,
	},
}

func (s *Service) httpBase(nodeID string) string {
	if s.PeerHTTPBase == nil {
		return ""
	}
	return s.PeerHTTPBase(nodeID)
}

// relayHTTPClient 返回一个把所有连接都经 RelayDial 拨到节点 mesh 端口的
// HTTP 客户端。禁用 keep-alive：每个请求重拨一次，避免连接复用与单连接冲突。
func relayHTTPClient(ctx context.Context, nodeID string, dial func(context.Context, string) (net.Conn, error)) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			MaxConnsPerHost:   1,
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return dial(ctx, nodeID)
			},
		},
	}
}

// httpTarget 解析出该节点可用的 HTTP 客户端与基址；直连基址优先，
// 其次经上级代理（跨域）。都没有返回 ok=false。
func (s *Service) httpTarget(ctx context.Context, nodeID string) (*http.Client, string, bool) {
	if base := s.httpBase(nodeID); base != "" {
		return httpClient, base, true
	}
	if s.RelayDial != nil {
		return relayHTTPClient(ctx, nodeID, s.RelayDial), "http://mesh", true
	}
	return nil, "", false
}

func (s *Service) httpUploadWith(ctx context.Context, client *http.Client, base, remotePath string, source io.Reader) (int64, error) {
	target := base + "/api/file/upload?path=" + url.QueryEscape(remotePath)
	counting := &countingReader{Reader: source}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, counting)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return counting.n, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return counting.n, fmt.Errorf("node returned %d: %s", resp.StatusCode, string(body))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return counting.n, nil
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

func (s *Service) httpOpenReadWith(ctx context.Context, client *http.Client, base, remotePath string) (io.ReadCloser, error) {
	target := base + "/api/file/download?path=" + url.QueryEscape(remotePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		return nil, fmt.Errorf("node returned %d: %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
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

func (s *Service) client(ctx context.Context, nodeID string) (*sftp.Client, func(), error) {
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

func (s *Service) List(ctx context.Context, nodeID, remotePath string) ([]model.FileEntry, error) {
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

func (s *Service) Stat(ctx context.Context, nodeID, remotePath string) (model.FileEntry, error) {
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

func (s *Service) Read(ctx context.Context, nodeID, remotePath string) ([]byte, error) {
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

func (s *Service) OpenRead(ctx context.Context, nodeID, remotePath string) (io.ReadCloser, error) {
	if client, base, ok := s.httpTarget(ctx, nodeID); ok {
		if reader, err := s.httpOpenReadWith(ctx, client, base, remotePath); err == nil {
			return reader, nil
		} else {
			slog.Warn("peer http download failed; falling back to sftp channel", "node", nodeID, "error", err)
		}
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

func (s *Service) Write(ctx context.Context, nodeID, remotePath string, data []byte) error {
	if len(data) > maxWriteSize {
		return apperrors.New(apperrors.InvalidArgument, "file exceeds write limit")
	}
	if client, base, ok := s.httpTarget(ctx, nodeID); ok {
		target := base + "/api/file/write?path=" + url.QueryEscape(remotePath)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
		if err == nil {
			if resp, err := client.Do(req); err == nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		// HTTP 不可用则继续走 SFTP 通道
	}
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

func (s *Service) CreateReader(ctx context.Context, nodeID, remotePath string) (io.Reader, error) {
	reader, err := s.OpenRead(ctx, nodeID, remotePath)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func (s *Service) Upload(ctx context.Context, nodeID, remotePath string, source io.Reader) (int64, error) {
	if client, base, ok := s.httpTarget(ctx, nodeID); ok {
		// 先整读进内存再发：HTTP 失败回退 SFTP 时数据不会丢。
		if data, readErr := io.ReadAll(source); readErr == nil {
			if written, err := s.httpUploadWith(ctx, client, base, remotePath, bytes.NewReader(data)); err == nil {
				return written, nil
			} else {
				slog.Warn("peer http upload failed; falling back to sftp channel", "node", nodeID, "error", err)
				source = bytes.NewReader(data)
			}
		} else {
			slog.Warn("peer http upload skipped (source unreadable); using sftp channel", "node", nodeID, "error", readErr)
		}
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

func (s *Service) Remove(ctx context.Context, nodeID, remotePath string) error {
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

func (s *Service) Mkdir(ctx context.Context, nodeID, remotePath string) error {
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

func (s *Service) Rename(ctx context.Context, nodeID, oldPath, newPath string) error {
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

func (s *Service) Touch(ctx context.Context, nodeID, remotePath string) error {
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

func (s *Service) TouchTime(_ context.Context, _ string, _ string, _ time.Time) error {
	return apperrors.New(apperrors.NotSupported, "setting file times is not supported in Phase 1")
}
