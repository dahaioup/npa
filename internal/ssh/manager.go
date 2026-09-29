package sshx

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
)

type Repository interface {
	GetNode(id string) (model.Node, error)
	GetCredential(nodeID string) (model.Credential, error)
	UpdateNodeStatus(id, status, lastError string) error
	SaveNode(node model.Node) error
}

type Manager struct {
	repo           Repository
	mu             sync.Mutex
	clients        map[string]*ssh.Client
	keepAlives     map[string]context.CancelFunc
	ConnectTimeout time.Duration
}

func NewManager(repo Repository) *Manager {
	return &Manager{
		repo:           repo,
		clients:        map[string]*ssh.Client{},
		keepAlives:     map[string]context.CancelFunc{},
		ConnectTimeout: 10 * time.Second,
	}
}

func fingerprint(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func (m *Manager) hostKeyCallback(nodeID string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		node, err := m.repo.GetNode(nodeID)
		if err != nil {
			return err
		}
		current := fingerprint(key)
		if node.HostKeyFingerprint != "" && node.HostKeyFingerprint != current {
			return apperrors.Wrap(apperrors.HostKeyChanged, "SSH host key changed", fmt.Errorf("stored %s, received %s", node.HostKeyFingerprint, current))
		}
		if node.HostKeyFingerprint == "" {
			node.HostKeyFingerprint = current
			if err := m.repo.SaveNode(node); err != nil {
				return err
			}
		}
		return nil
	}
}

func (m *Manager) authMethods(node model.Node, credential model.Credential) ([]ssh.AuthMethod, error) {
	switch node.AuthMethod {
	case "password":
		if credential.Password == "" {
			return nil, apperrors.New(apperrors.InvalidArgument, "password credential is missing")
		}
		return []ssh.AuthMethod{ssh.Password(credential.Password)}, nil
	case "key":
		if credential.PrivateKey == "" {
			return nil, apperrors.New(apperrors.InvalidArgument, "private key credential is missing")
		}
		var signer ssh.Signer
		var err error
		if credential.PrivateKeyPassword != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(credential.PrivateKey), []byte(credential.PrivateKeyPassword))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(credential.PrivateKey))
		}
		if err != nil {
			return nil, apperrors.Wrap(apperrors.Authentication, "invalid private key", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	case "agent":
		socket := os.Getenv("SSH_AUTH_SOCK")
		if socket == "" {
			return nil, apperrors.New(apperrors.Authentication, "SSH_AUTH_SOCK is not set")
		}
		conn, err := net.Dial("unix", socket)
		if err != nil {
			return nil, apperrors.Wrap(apperrors.Authentication, "cannot connect to SSH agent", err)
		}
		client := agent.NewClient(conn)
		return []ssh.AuthMethod{ssh.PublicKeysCallback(client.Signers)}, nil
	default:
		return nil, apperrors.New(apperrors.InvalidArgument, "unsupported authentication method")
	}
}

func mapConnectError(err error) error {
	var appErr *apperrors.AppError
	if errors.As(err, &appErr) {
		return err
	}
	if strings.Contains(err.Error(), "unable to authenticate") || strings.Contains(err.Error(), "no supported methods") {
		return apperrors.Wrap(apperrors.Authentication, "SSH authentication failed", err)
	}
	return apperrors.Wrap(apperrors.NodeOffline, "SSH connection failed", err)
}

func (m *Manager) Connect(ctx context.Context, nodeID string) (*ssh.Client, error) {
	m.mu.Lock()
	if client, ok := m.clients[nodeID]; ok {
		m.mu.Unlock()
		return client, nil
	}
	m.mu.Unlock()

	node, err := m.repo.GetNode(nodeID)
	if err != nil {
		return nil, err
	}
	credential, err := m.repo.GetCredential(nodeID)
	if err != nil {
		return nil, err
	}
	auth, err := m.authMethods(node, credential)
	if err != nil {
		_ = m.repo.UpdateNodeStatus(nodeID, "error", err.Error())
		return nil, err
	}
	config := &ssh.ClientConfig{
		User:            node.Username,
		Auth:            auth,
		HostKeyCallback: m.hostKeyCallback(nodeID),
		Timeout:         m.ConnectTimeout,
		ClientVersion:   "SSH-2.0-NPA_1",
	}
	dialer := net.Dialer{Timeout: m.ConnectTimeout, KeepAlive: 30 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(node.Address, fmt.Sprintf("%d", node.Port)))
	if err != nil {
		_ = m.repo.UpdateNodeStatus(nodeID, "offline", err.Error())
		return nil, mapConnectError(err)
	}
	_ = raw.SetDeadline(time.Now().Add(m.ConnectTimeout))
	connection, channels, requests, err := ssh.NewClientConn(raw, net.JoinHostPort(node.Address, fmt.Sprintf("%d", node.Port)), config)
	if err != nil {
		raw.Close()
		_ = m.repo.UpdateNodeStatus(nodeID, "error", err.Error())
		return nil, mapConnectError(err)
	}
	_ = raw.SetDeadline(time.Time{})
	client := ssh.NewClient(connection, channels, requests)

	m.mu.Lock()
	if existing, ok := m.clients[nodeID]; ok {
		m.mu.Unlock()
		client.Close()
		return existing, nil
	}
	m.clients[nodeID] = client
	runCtx, cancel := context.WithCancel(context.Background())
	m.keepAlives[nodeID] = cancel
	m.mu.Unlock()
	_ = m.repo.UpdateNodeStatus(nodeID, "connected", "")
	go m.keepAlive(nodeID, client, runCtx)
	return client, nil
}

func (m *Manager) keepAlive(nodeID string, client *ssh.Client, ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				m.Close(nodeID)
				_ = m.repo.UpdateNodeStatus(nodeID, "offline", err.Error())
				return
			}
		}
	}
}

func (m *Manager) Client(ctx context.Context, nodeID string) (*ssh.Client, error) {
	return m.Connect(ctx, nodeID)
}

func (m *Manager) Close(nodeID string) error {
	m.mu.Lock()
	client := m.clients[nodeID]
	stop := m.keepAlives[nodeID]
	delete(m.clients, nodeID)
	delete(m.keepAlives, nodeID)
	m.mu.Unlock()
	if stop != nil {
		stop()
	}
	if client != nil {
		return client.Close()
	}
	return nil
}

func (m *Manager) CloseAll() error {
	m.mu.Lock()
	ids := make([]string, 0, len(m.clients))
	for id := range m.clients {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	var first error
	for _, id := range ids {
		if err := m.Close(id); err != nil && first == nil {
			first = err
		}
	}
	return first
}

type ExecRequest struct {
	Command string
	Timeout time.Duration
}

func localShellCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/Q"}
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return shell, nil
}

func (m *Manager) localExec(ctx context.Context, request ExecRequest) (model.ExecResult, error) {
	if request.Timeout <= 0 {
		request.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	started := time.Now()
	shell, args := localShellCommand()
	var command []string
	if runtime.GOOS == "windows" {
		command = append([]string{"/C"}, request.Command)
	} else {
		command = append([]string{"-lc"}, request.Command)
	}
	cmd := exec.CommandContext(ctx, shell, append(args, command...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			err = nil
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = apperrors.New(apperrors.Timeout, "command timed out")
		} else {
			err = apperrors.Wrap(apperrors.Internal, "local command failed", err)
		}
	}
	return model.ExecResult{
		Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode,
		DurationMS: time.Since(started).Milliseconds(),
	}, err
}

func (m *Manager) Exec(ctx context.Context, nodeID string, request ExecRequest) (model.ExecResult, error) {
	if nodeID == "local" {
		return m.localExec(ctx, request)
	}
	client, err := m.Client(ctx, nodeID)
	if err != nil {
		return model.ExecResult{}, err
	}
	if request.Timeout <= 0 {
		request.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	started := time.Now()
	session, err := client.NewSession()
	if err != nil {
		return model.ExecResult{}, mapConnectError(err)
	}
	defer session.Close()
	var stdout, stderr strings.Builder
	session.Stdout = &stdout
	session.Stderr = &stderr
	err = session.Start(request.Command)
	if err == nil {
		done := make(chan error, 1)
		go func() { done <- session.Wait() }()
		select {
		case <-ctx.Done():
			_ = session.Signal(ssh.SIGKILL)
			err = ctx.Err()
		case err = <-done:
		}
	}
	exitCode := 0
	if err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitStatus()
			err = nil
		} else if errors.Is(err, context.DeadlineExceeded) {
			err = apperrors.New(apperrors.Timeout, "command timed out")
		} else if err != nil {
			err = mapConnectError(err)
		}
	}
	return model.ExecResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ExitCode:   exitCode,
		DurationMS: time.Since(started).Milliseconds(),
	}, err
}

type Shell struct {
	Session   *ssh.Session
	Stdin     net.Conn
	Stdout    net.Conn
	closeFn   func() error
	closeOnce sync.Once
}

func (s *Shell) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.closeFn != nil {
			err = s.closeFn()
		} else if s.Session != nil {
			err = s.Session.Close()
		}
		s.Stdin.Close()
		s.Stdout.Close()
	})
	return err
}

func (m *Manager) openLocalShell() (*Shell, error) {
	shell, args := localShellCommand()
	cmd := exec.Command(shell, args...)
	stdinReader, stdinWriter := net.Pipe()
	stdoutWriter, stdoutReader := net.Pipe()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinReader, stdoutWriter, stdoutWriter
	if err := cmd.Start(); err != nil {
		stdinReader.Close()
		stdinWriter.Close()
		stdoutReader.Close()
		stdoutWriter.Close()
		return nil, apperrors.Wrap(apperrors.Internal, "cannot start local shell", err)
	}
	go func() {
		_ = cmd.Wait()
		_ = stdinReader.Close()
		_ = stdoutWriter.Close()
	}()
	return &Shell{
		Stdin: stdinWriter, Stdout: stdoutReader,
		closeFn: func() error {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = stdinWriter.Close()
			_ = stdoutWriter.Close()
			return nil
		},
	}, nil
}

func (m *Manager) OpenShell(ctx context.Context, nodeID string, cols, rows int) (*Shell, error) {
	if nodeID == "local" {
		return m.openLocalShell()
	}
	client, err := m.Client(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, mapConnectError(err)
	}
	if cols <= 0 {
		cols = 100
	}
	if rows <= 0 {
		rows = 32
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := session.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		session.Close()
		return nil, apperrors.Wrap(apperrors.Internal, "cannot request PTY", err)
	}
	stdin, stdinWriter := net.Pipe()
	stdout, stdoutReader := net.Pipe()
	session.Stdin = stdin
	session.Stdout = stdout
	session.Stderr = stdout
	if err := session.Shell(); err != nil {
		session.Close()
		stdin.Close()
		stdout.Close()
		return nil, apperrors.Wrap(apperrors.Internal, "cannot start shell", err)
	}
	go func() { _ = session.Wait() }()
	return &Shell{Session: session, Stdin: stdinWriter, Stdout: stdoutReader}, nil
}
