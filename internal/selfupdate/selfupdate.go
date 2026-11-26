// Package selfupdate 从已连接的 NPA 节点拉取新版本并替换本机二进制。
//
// 安全前提（与 SRS/ADR 一致）：只由回环 Web 手动触发、只连局域网已纳管节点、
// 下载后校验可执行魔数与体积、必要时比对节点侧 sha256，全程写审计。
package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/file"
	"github.com/npa/npa/internal/model"
)

const (
	// maxArtifactSize 限制从节点拉取的产物大小，防失控下载。
	maxArtifactSize = 256 << 20
	// minBinarySize 是可执行文件的体积下限，用来挡住截断/空文件。
	minBinarySize = 1 << 20
)

// Candidate 是某个节点上可以用来升级的产物。
type Candidate struct {
	NodeID      string `json:"node_id"`
	NodeName    string `json:"node_name"`
	NodeVersion string `json:"node_version"`
	Artifact    string `json:"artifact"`
	Size        int64  `json:"size"`
	IsArchive   bool   `json:"is_archive"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// Result 是一次升级应用的结果。
type Result struct {
	NodeID     string `json:"node_id"`
	Artifact   string `json:"artifact"`
	LocalPath  string `json:"local_path"`
	BinaryPath string `json:"binary_path,omitempty"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Version    string `json:"version,omitempty"`
	Restarted  bool   `json:"restarted"` // unix 上 syscall.Exec 不会返回
}

// BinaryName 是指定平台二进制在包里的名字。
func BinaryName(goos, goarch string) string {
	name := fmt.Sprintf("npa-%s-%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// binaryName 是本平台二进制在包里的名字。
func binaryName() string { return BinaryName(runtime.GOOS, runtime.GOARCH) }

// IsCandidateArtifact 判断文件名是否是可升级产物。
// 打包产物形如 npa-all_20261019_2016.zip，单平台二进制形如 npa-darwin-amd64。
func IsCandidateArtifact(name string) bool {
	base := filepath.Base(name)
	if strings.HasSuffix(base, ".sha256") || strings.HasSuffix(base, ".old") {
		return false
	}
	if rest, ok := strings.CutPrefix(base, "npa-all"); ok {
		if (strings.HasPrefix(rest, "-") || strings.HasPrefix(rest, "_")) && strings.HasSuffix(base, ".zip") {
			return true
		}
	}
	return strings.Contains(base, binaryName())
}

// artifactDirs 返回候选搜索目录（按顺序）。
func artifactDirs(hint string) []string {
	dirs := []string{}
	if value := strings.TrimSpace(hint); value != "" {
		dirs = append(dirs, value)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Downloads"), home)
	}
	dirs = append(dirs, "/tmp")
	return dirs
}

// FindArtifact 在节点上定位最新产物：hint 是文件就用它，是目录或留空则在候选目录里找最新的。
func FindArtifact(ctx context.Context, files *file.Service, nodeID, hint string) (model.FileEntry, error) {
	hint = strings.TrimSpace(hint)
	if hint != "" && !strings.HasSuffix(strings.TrimRight(hint, "/\\"), "/") {
		// 看起来像文件：直接确认存在
		entries, err := files.List(ctx, nodeID, filepath.ToSlash(filepath.Dir(hint)))
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir && entry.Name == filepath.Base(hint) {
					entry.Path = hint
					return entry, nil
				}
			}
		}
	}
	var best model.FileEntry
	for _, dir := range artifactDirs(hint) {
		entries, err := files.List(ctx, nodeID, filepath.ToSlash(dir))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir || !IsCandidateArtifact(entry.Name) {
				continue
			}
			entry.Path = filepath.ToSlash(filepath.Join(dir, entry.Name))
			if best.Name == "" || entry.ModTime.After(best.ModTime) {
				best = entry
			}
		}
		if best.Name != "" {
			return best, nil
		}
	}
	if best.Name == "" {
		return model.FileEntry{}, apperrors.New(apperrors.InvalidArgument,
			"在该节点的候选目录里没有找到 npa-all_*.zip 或 "+binaryName())
	}
	return best, nil
}

// Download 把产物流式拉到本机 destDir，返回本地路径与大小。
func Download(ctx context.Context, files *file.Service, nodeID, remotePath, destDir string) (string, int64, error) {
	reader, err := files.OpenRead(ctx, nodeID, remotePath)
	if err != nil {
		return "", 0, apperrors.Wrap(apperrors.NodeOffline, "打开节点上的产物失败", err)
	}
	defer reader.Close()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", 0, err
	}
	local := filepath.Join(destDir, filepath.Base(remotePath))
	target, err := os.Create(local)
	if err != nil {
		return "", 0, err
	}
	defer target.Close()
	written, err := io.Copy(target, io.LimitReader(reader, maxArtifactSize+1))
	if err != nil {
		return "", 0, apperrors.Wrap(apperrors.NodeOffline, "下载产物失败", err)
	}
	if written > maxArtifactSize {
		return "", 0, apperrors.New(apperrors.InvalidArgument, "产物超过大小上限")
	}
	return local, written, nil
}

// FileSHA256 计算本地文件的 sha256（十六进制小写）。
func FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// VerifyArtifact 校验本平台二进制：体积下限 + 可执行魔数，挡住截断或错文件。
func VerifyArtifact(path string) error { return VerifyArtifactFor(path, runtime.GOOS) }

// VerifyArtifactFor 校验指定平台的二进制（推送升级到别的平台节点时用）。
func VerifyArtifactFor(path, goos string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() < minBinarySize {
		return fmt.Errorf("二进制体积异常（%d 字节），可能是截断或空文件", info.Size())
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(file, head); err != nil {
		return err
	}
	switch goos {
	case "windows":
		if head[0] != 'M' || head[1] != 'Z' {
			return fmt.Errorf("不是 Windows 可执行文件（缺少 MZ 魔数）")
		}
	case "linux":
		if string(head[:4]) != "\x7fELF" {
			return fmt.Errorf("不是 ELF 可执行文件")
		}
	case "darwin":
		magic := uint32(head[0])<<24 | uint32(head[1])<<16 | uint32(head[2])<<8 | uint32(head[3])
		switch magic {
		case 0xFEEDFACE, 0xFEEDFACF, 0xCEFAEDFE, 0xCFFAEDFE, 0xCAFEBABE:
		default:
			return fmt.Errorf("不是 Mach-O 可执行文件（魔数 %x）", magic)
		}
	}
	return nil
}

// Extract 从 zip 里取出本平台二进制；传进来的是裸二进制则原样返回。
func Extract(archivePath, dir string) (string, error) {
	return ExtractPlatform(archivePath, dir, runtime.GOOS, runtime.GOARCH)
}

// ExtractPlatform 从 zip 里取出**指定平台**的二进制（推送升级到别的平台节点时用）。
// 返回的路径在 dir 下，已具备可执行权限（unix）。
func ExtractPlatform(archivePath, dir, goos, goarch string) (string, error) {
	if !strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return archivePath, nil
	}
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", apperrors.Wrap(apperrors.InvalidArgument, "打开更新包失败", err)
	}
	defer reader.Close()

	name := BinaryName(goos, goarch)
	var target *zip.File
	for i := range reader.File {
		entry := reader.File[i]
		if entry.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(entry.Name)
		if base == name || strings.HasSuffix(entry.Name, "/bin/"+name) {
			target = entry
			break
		}
	}
	if target == nil {
		return "", fmt.Errorf("更新包里没有本平台的二进制 %s", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	output := filepath.Join(dir, name)
	source, err := target.Open()
	if err != nil {
		return "", err
	}
	defer source.Close()
	targetFile, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	defer targetFile.Close()
	if _, err := io.Copy(targetFile, io.LimitReader(source, maxArtifactSize)); err != nil {
		return "", err
	}
	if err := targetFile.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(output, 0o755); err != nil {
		return "", err
	}
	return output, nil
}

// CleanStale 备份残留（Windows 上次升级留下的 .old-<ts>）与过期下载，保持目录干净。
func CleanStale(dir string, keep time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-keep)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.Contains(name, ".old-") && !strings.HasSuffix(name, ".zip") && !strings.HasSuffix(name, ".exe") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}
