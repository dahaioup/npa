package browser

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestInstallerInstallsFromRelease 走完整安装流程：查 release → 选资产 → 下载 →
// 解包 → 自检 → 写 sidecar。服务器是本地的，不碰网络。
func TestInstallerInstallsFromRelease(t *testing.T) {
	assetName, err := AssetName(runtime.GOOS, runtime.GOARCH, "")
	if err != nil {
		t.Skipf("no obscura build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	archive := buildFakeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			payload := map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]any{{
					"name":                 assetName,
					"browser_download_url": "http://" + r.Host + "/download/" + assetName,
					"size":                 len(archive),
				}},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)
		case strings.HasSuffix(r.URL.Path, "/download/"+assetName):
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	installer := &Installer{DataDir: t.TempDir()}
	installer.Client = server.Client()
	// release 接口地址指向本地服务器。
	restore := obscuraReleasesAPI
	obscuraReleasesAPI = server.URL + "/releases/latest"
	defer func() { obscuraReleasesAPI = restore }()

	result, err := installer.Install(context.Background())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.Version != "9.9.9" {
		t.Errorf("version = %q, want 9.9.9", result.Version)
	}
	if result.Path != installer.BinaryPath() {
		t.Errorf("path = %q, want %q", result.Path, installer.BinaryPath())
	}
	info, err := os.Stat(installer.BinaryPath())
	if err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is not executable: %v", info.Mode())
	}
	// sidecar 要记下装的是哪个版本与包。
	state, ok := installer.State()
	if !ok {
		t.Fatal("install state was not written")
	}
	if state.Version != "9.9.9" || state.Asset != assetName {
		t.Errorf("state = %+v", state)
	}
	if !installer.Status().Installed {
		t.Error("status should report the binary as installed")
	}
}

// TestInstallerRejectsMissingAsset release 里没有对应平台的包时要明确报错。
func TestInstallerRejectsMissingAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{
			"tag_name": "v9.9.9",
			"assets":   []map[string]any{{"name": "obscura-sparc-solaris.tar.gz"}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()

	installer := &Installer{DataDir: t.TempDir()}
	installer.Client = server.Client()
	restore := obscuraReleasesAPI
	obscuraReleasesAPI = server.URL + "/releases/latest"
	defer func() { obscuraReleasesAPI = restore }()

	if _, err := installer.Install(context.Background()); err == nil {
		t.Fatal("expected an error when the release has no matching asset")
	} else if !strings.Contains(err.Error(), "obscura") {
		t.Errorf("error should mention obscura, got %v", err)
	}
}

// TestInstallerResumesInterruptedDownload 模拟 GitHub CDN 断流：
// 第一次请求只给一半就断，第二次带 Range 续传。安装必须自己恢复。
func TestInstallerResumesInterruptedDownload(t *testing.T) {
	assetName, err := AssetName(runtime.GOOS, runtime.GOARCH, "")
	if err != nil {
		t.Skipf("no obscura build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	archive := buildFakeArchive(t)
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			payload := map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]any{{
					"name":                 assetName,
					"browser_download_url": "http://" + r.Host + "/download/" + assetName,
					"size":                 len(archive),
				}},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)
		case strings.HasSuffix(r.URL.Path, "/download/"+assetName):
			attempts++
			offset := 0
			if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
				if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-", &offset); err != nil {
					t.Errorf("bad Range header %q", rangeHeader)
				}
			}
			if attempts == 1 {
				// 第一次：只给前一半就断。
				half := len(archive) / 2
				w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(archive[:half])
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				panic(http.ErrAbortHandler)
			}
			// 第二次：按 Range 续传。
			w.Header().Set("Content-Length", strconv.Itoa(len(archive)-offset))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(archive[offset:])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	installer := &Installer{DataDir: t.TempDir()}
	installer.Client = server.Client()
	restore := obscuraReleasesAPI
	obscuraReleasesAPI = server.URL + "/releases/latest"
	defer func() { obscuraReleasesAPI = restore }()

	result, err := installer.Install(context.Background())
	if err != nil {
		t.Fatalf("install should recover from an interrupted download: %v", err)
	}
	if result.Version != "9.9.9" {
		t.Errorf("version = %q, want 9.9.9", result.Version)
	}
	if attempts < 2 {
		t.Errorf("expected a retry after the interrupted download, attempts = %d", attempts)
	}
	if _, err := os.Stat(installer.BinaryPath()); err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}
}

// buildFakeArchive 造一个 tar.gz：里面放一个假的 obscura 与 obscura-worker。
func buildFakeArchive(t *testing.T) []byte {
	t.Helper()
	var buffer strings.Builder
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	files := map[string]string{
		"obscura":        "#!/bin/sh\nexit 0\n",
		"obscura-worker": "#!/bin/sh\nexit 0\n",
		"README.md":      "not the browser\n",
	}
	for name, content := range files {
		header := &tar.Header{
			Name:     "obscura-package/" + name,
			Mode:     0o755,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return []byte(buffer.String())
}

// TestInstallerExtractsOnlyBrowserBinaries 解包只落地 obscura 与 obscura-worker，
// 包里其它文件（README 等）不能被写进安装目录。
func TestInstallerExtractsOnlyBrowserBinaries(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "obscura.tar.gz")
	if err := os.WriteFile(archivePath, buildFakeArchive(t), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	installer := &Installer{DataDir: dir}
	if err := installer.extract(archivePath, "obscura.tar.gz"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, name := range []string{binaryName(), "obscura-worker"} {
		if _, err := os.Stat(filepath.Join(installer.InstallDir(), name)); err != nil {
			t.Errorf("%s should have been extracted: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(installer.InstallDir(), "README.md")); err == nil {
		t.Error("README.md should not have been extracted")
	}
}
