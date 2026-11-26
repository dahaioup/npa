package browser

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
)

// obscuraReleasesAPI 是 GitHub 上 obscura 的 release 接口（取最新版本与资产清单）。
// 做成变量是为了让测试能指向本地服务器。
var obscuraReleasesAPI = "https://api.github.com/repos/h4ckf0r0day/obscura/releases/latest"

// obscuraDownloadBase 是 release 资产的下载地址前缀。
const obscuraDownloadBase = "https://github.com/h4ckf0r0day/obscura/releases/download"

// Variants 是 obscura 官方发布包的可选形态：
// 空 = 完整渲染版；stealth = 隐匿版（指纹一致 + TLS 伪装 + 追踪器拦截）；
// no-render = 精简版（不渲染，只做抓取，最小体积）。
var Variants = map[string]bool{"": true, "stealth": true, "no-render": true}

// Installer 负责下载安装 obscura 二进制。
type Installer struct {
	// DataDir 是 NPA 数据目录，二进制装到 <DataDir>/bin。
	DataDir string
	// Variant 是发布包形态（见 Variants）。
	Variant string
	// Version 指定 release tag（空 = 自动取最新）。
	Version string
	// Client 是下载用的 HTTP 客户端（可注入，便于测试）。
	Client *http.Client
	// Progress 收到下载进度回调（已下载字节、总字节）。
	Progress func(downloaded, total int64)
}

// InstallState 记录一次安装的落地信息（写在二进制旁边的 sidecar 文件里）。
type InstallState struct {
	Version     string    `json:"version"`
	Variant     string    `json:"variant"`
	Asset       string    `json:"asset"`
	Size        int64     `json:"size"`
	InstalledAt time.Time `json:"installed_at"`
}

// InstallStatus 是给 Web 面看的安装状态。
type InstallStatus struct {
	Installed  bool   `json:"installed"`
	Path       string `json:"path"`
	Version    string `json:"version"`
	Variant    string `json:"variant"`
	Asset      string `json:"asset,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Installing bool   `json:"installing"`
	LastError  string `json:"last_error,omitempty"`
}

// InstallResult 是一次安装的结果。
type InstallResult struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Asset   string `json:"asset"`
	Size    int64  `json:"size"`
}

func (i *Installer) client() *http.Client {
	if i.Client != nil {
		return i.Client
	}
	return &http.Client{Timeout: 0}
}

// InstallDir 返回二进制安装目录。
func (i *Installer) InstallDir() string {
	return filepath.Join(i.DataDir, "bin")
}

// BinaryPath 返回安装后的 obscura 路径。
func (i *Installer) BinaryPath() string {
	return filepath.Join(i.InstallDir(), binaryName())
}

// statePath 返回 sidecar 状态文件路径。
func (i *Installer) statePath() string {
	return filepath.Join(i.InstallDir(), "obscura-install.json")
}

// State 读取已安装信息。
func (i *Installer) State() (InstallState, bool) {
	var state InstallState
	data, err := os.ReadFile(i.statePath())
	if err != nil {
		return state, false
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, false
	}
	return state, true
}

// Status 返回当前安装状态。
func (i *Installer) Status() InstallStatus {
	status := InstallStatus{Path: i.BinaryPath(), Variant: i.Variant}
	if info, err := os.Stat(status.Path); err == nil && !info.IsDir() {
		status.Installed = true
		status.Size = info.Size()
	}
	if state, ok := i.State(); ok {
		status.Version = state.Version
		if state.Variant != "" {
			status.Variant = state.Variant
		}
		status.Asset = state.Asset
	}
	return status
}

// release 是 GitHub release 接口的最小映射。
type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

// Install 下载并安装 obscura。流程：查 release → 选资产 → 下载 → 解包 → 落地 → 自检。
func (i *Installer) Install(ctx context.Context) (InstallResult, error) {
	release, err := i.latestRelease(ctx)
	if err != nil {
		return InstallResult{}, err
	}
	asset, err := i.pickAsset(release)
	if err != nil {
		return InstallResult{}, err
	}
	url := asset.BrowserDownloadURL
	if url == "" {
		url = fmt.Sprintf("%s/%s/%s", obscuraDownloadBase, release.TagName, asset.Name)
	}
	if err := os.MkdirAll(i.InstallDir(), 0o755); err != nil {
		return InstallResult{}, apperrors.Wrap(apperrors.Internal, "create install dir", err)
	}
	archivePath := filepath.Join(i.InstallDir(), "."+asset.Name+".part")
	// 下载失败时保留 .part，下次安装可以断点续传；成功解包后才删。
	installed := false
	defer func() {
		if installed {
			os.Remove(archivePath)
		}
	}()
	if err := i.download(ctx, url, archivePath, asset.Size); err != nil {
		return InstallResult{}, err
	}
	if err := i.extract(archivePath, asset.Name); err != nil {
		return InstallResult{}, err
	}
	if err := i.verify(i.BinaryPath()); err != nil {
		return InstallResult{}, err
	}
	installed = true
	state := InstallState{
		Version:     strings.TrimPrefix(release.TagName, "v"),
		Variant:     i.Variant,
		Asset:       asset.Name,
		Size:        asset.Size,
		InstalledAt: time.Now(),
	}
	encoded, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(i.statePath(), encoded, 0o644); err != nil {
		return InstallResult{}, apperrors.Wrap(apperrors.Internal, "write install state", err)
	}
	return InstallResult{Path: i.BinaryPath(), Version: state.Version, Asset: asset.Name, Size: asset.Size}, nil
}

// latestRelease 取 release 清单：指定了版本就直接拼 URL，否则查 GitHub API。
func (i *Installer) latestRelease(ctx context.Context) (release, error) {
	if i.Version != "" {
		return release{TagName: i.Version}, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, obscuraReleasesAPI, nil)
	if err != nil {
		return release{}, apperrors.Wrap(apperrors.Internal, "build release request", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "npa-obscura-installer")
	response, err := i.client().Do(request)
	if err != nil {
		return release{}, apperrors.Wrap(apperrors.Internal, "query obscura releases", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return release{}, apperrors.New(apperrors.Internal, "obscura release lookup returned "+response.Status)
	}
	var result release
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return release{}, apperrors.Wrap(apperrors.Internal, "decode release info", err)
	}
	if result.TagName == "" {
		return release{}, apperrors.New(apperrors.Internal, "obscura release info has no tag")
	}
	return result, nil
}

// pickAsset 按平台与形态挑资产；形态缺失时退回完整版。
func (i *Installer) pickAsset(release release) (struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}, error) {
	name, err := AssetName(runtime.GOOS, runtime.GOARCH, i.Variant)
	if err != nil {
		return struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Size               int64  `json:"size"`
		}{}, err
	}
	for _, asset := range release.Assets {
		if asset.Name == name {
			return asset, nil
		}
	}
	if i.Variant != "" {
		// 变体包不存在时退回完整版（release 列表通常是齐的，但别因为缺一个变体就装不了）。
		base, err := AssetName(runtime.GOOS, runtime.GOARCH, "")
		if err == nil {
			for _, asset := range release.Assets {
				if asset.Name == base {
					return asset, nil
				}
			}
		}
	}
	return struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	}{}, apperrors.New(apperrors.NotSupported, "obscura release has no asset "+name)
}

// AssetName 按平台与形态算出发布包文件名。
func AssetName(goos, goarch, variant string) (string, error) {
	if !Variants[variant] {
		return "", apperrors.New(apperrors.InvalidArgument, "unknown obscura variant: "+variant)
	}
	var osName, archName, extension string
	switch goos {
	case "darwin":
		osName = "macos"
	case "linux":
		osName = "linux"
	case "windows":
		osName = "windows"
	default:
		return "", apperrors.New(apperrors.NotSupported, "obscura has no build for "+goos)
	}
	switch goarch {
	case "amd64":
		archName = "x86_64"
	case "arm64":
		archName = "aarch64"
	default:
		return "", apperrors.New(apperrors.NotSupported, "obscura has no build for "+goarch)
	}
	if goos == "windows" && goarch == "arm64" {
		return "", apperrors.New(apperrors.NotSupported, "obscura has no windows arm64 build")
	}
	if goos == "windows" {
		extension = "zip"
	} else {
		extension = "tar.gz"
	}
	name := "obscura-" + archName + "-" + osName
	if variant != "" {
		name += "-" + variant
	}
	return name + "." + extension, nil
}

// downloadAttempts 是下载重试次数。GitHub 的 release 资产走 CDN，
// 大文件（70MB+）在弱网下经常中途断流，必须能续传重试。
const downloadAttempts = 5

// download 下载资产到本地文件。
//
// 断点续传：每次失败都保留已下载的部分，下次带 Range 头接着下；
// 服务器不支持续传（回 200 而不是 206）时从头重下。
func (i *Installer) download(ctx context.Context, url, destination string, total int64) error {
	var lastErr error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		offset := int64(0)
		if info, err := os.Stat(destination); err == nil {
			offset = info.Size()
		}
		if total > 0 && offset >= total {
			// 已经下完了（上次尝试写完但没来得及校验）。
			return nil
		}
		written, err := i.downloadOnce(ctx, url, destination, offset, total)
		if err == nil {
			// 服务端可能悄悄少给字节，收尾时核对一遍大小。
			if info, statErr := os.Stat(destination); statErr == nil && total > 0 && info.Size() != total {
				lastErr = fmt.Errorf("download incomplete: got %d bytes, want %d", info.Size(), total)
				slog.Warn("obscura download incomplete, retrying", "attempt", attempt, "error", lastErr)
				select {
				case <-ctx.Done():
					return apperrors.Wrap(apperrors.Internal, "download obscura", ctx.Err())
				case <-time.After(time.Duration(attempt) * time.Second):
				}
				continue
			}
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		slog.Warn("obscura download interrupted, retrying", "attempt", attempt, "written", written, "error", err)
		// 退避一下再试，别把网络打爆。
		select {
		case <-ctx.Done():
			return apperrors.Wrap(apperrors.Internal, "download obscura", ctx.Err())
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	return apperrors.Wrap(apperrors.Internal, "download obscura", lastErr)
}

// downloadOnce 发起一次下载请求，从 offset 处接着写。返回本次写入的字节数。
func (i *Installer) downloadOnce(ctx context.Context, url, destination string, offset, total int64) (int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, apperrors.Wrap(apperrors.Internal, "build download request", err)
	}
	request.Header.Set("User-Agent", "npa-obscura-installer")
	if offset > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	response, err := i.client().Do(request)
	if err != nil {
		return 0, apperrors.Wrap(apperrors.Internal, "download obscura", err)
	}
	defer response.Body.Close()
	flags := os.O_WRONLY | os.O_CREATE
	switch response.StatusCode {
	case http.StatusOK:
		// 服务器忽略了 Range（或本来就是从头下）：重置文件。
		flags |= os.O_TRUNC
		offset = 0
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	default:
		return 0, apperrors.New(apperrors.Internal, "obscura download returned "+response.Status)
	}
	file, err := os.OpenFile(destination, flags, 0o644)
	if err != nil {
		return 0, apperrors.Wrap(apperrors.Internal, "create download file", err)
	}
	defer file.Close()
	writer := io.Writer(file)
	if i.Progress != nil {
		writer = &progressWriter{writer: file, total: total, callback: i.Progress, base: offset}
	}
	written, err := io.Copy(writer, response.Body)
	if err != nil {
		return written, apperrors.Wrap(apperrors.Internal, "save obscura download", err)
	}
	return written, nil
}

// progressWriter 在下载过程中回调进度。
type progressWriter struct {
	writer     io.Writer
	total      int64
	downloaded int64
	base       int64
	callback   func(downloaded, total int64)
}

func (w *progressWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.downloaded += int64(n)
	if w.callback != nil {
		w.callback(w.base+w.downloaded, w.total)
	}
	return n, err
}

// extract 解包并把 obscura（以及渲染用的 obscura-worker）落地到安装目录。
func (i *Installer) extract(archivePath, assetName string) error {
	if err := os.MkdirAll(i.InstallDir(), 0o755); err != nil {
		return apperrors.Wrap(apperrors.Internal, "create install dir", err)
	}
	if strings.HasSuffix(assetName, ".zip") {
		return i.extractZip(archivePath)
	}
	return i.extractTarGz(archivePath)
}

// extractTarGz 解 tar.gz。
func (i *Installer) extractTarGz(archivePath string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return apperrors.Wrap(apperrors.Internal, "open archive", err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return apperrors.Wrap(apperrors.Internal, "open gzip stream", err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return apperrors.Wrap(apperrors.Internal, "read archive", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Base(filepath.Clean(header.Name))
		if name != binaryName() && name != "obscura-worker" {
			continue
		}
		if err := i.writeBinary(name, reader); err != nil {
			return err
		}
	}
	return nil
}

// extractZip 解 zip。
func (i *Installer) extractZip(archivePath string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return apperrors.Wrap(apperrors.Internal, "open zip archive", err)
	}
	defer reader.Close()
	for _, entry := range reader.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(filepath.Clean(entry.Name))
		if name != binaryName() && name != "obscura-worker" {
			continue
		}
		source, err := entry.Open()
		if err != nil {
			return apperrors.Wrap(apperrors.Internal, "open zip entry", err)
		}
		err = i.writeBinary(name, source)
		source.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeBinary 把一个可执行文件写进安装目录（先写临时文件再改名，避免半截文件）。
func (i *Installer) writeBinary(name string, source io.Reader) error {
	target := filepath.Join(i.InstallDir(), name)
	temporary := target + ".part"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return apperrors.Wrap(apperrors.Internal, "write "+name, err)
	}
	if _, err := io.Copy(file, source); err != nil {
		file.Close()
		os.Remove(temporary)
		return apperrors.Wrap(apperrors.Internal, "write "+name, err)
	}
	if err := file.Close(); err != nil {
		os.Remove(temporary)
		return apperrors.Wrap(apperrors.Internal, "close "+name, err)
	}
	if err := os.Rename(temporary, target); err != nil {
		os.Remove(temporary)
		return apperrors.Wrap(apperrors.Internal, "install "+name, err)
	}
	return os.Chmod(target, 0o755)
}

// verify 跑一次 `obscura --help` 确认二进制可用。
func (i *Installer) verify(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--help")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return apperrors.New(apperrors.Internal, "installed obscura failed to start: "+err.Error())
	}
	return nil
}
