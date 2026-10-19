package file

import (
	"context"
	"io"
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
}

func NewService(sshManager *sshx.Manager) *Service { return &Service{SSH: sshManager} }

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
