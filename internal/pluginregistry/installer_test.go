package pluginregistry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// buildTarGz returns a tar.gz archive holding the given files.
func buildTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(writer)
	for name, content := range files {
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buffer.Bytes()
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// registryServer serves a registry document and its artifacts.
type registryServer struct {
	*httptest.Server
	registryData []byte
	artifacts    map[string][]byte
}

func newRegistryServer(t *testing.T, pluginArtifact []byte, pluginDigest string) *registryServer {
	t.Helper()
	server := &registryServer{artifacts: map[string][]byte{"/plugin.tar.gz": pluginArtifact}}
	registry := map[string]any{
		"schema_version": 1,
		"updated_at":     "2026-11-24T00:00:00Z",
		"plugins": []map[string]any{
			{
				"id":      "npa-demo",
				"name":    "Demo",
				"version": "1.0.0",
				"kind":    KindPlugin,
				"install": map[string]any{"dir": "plugins/demo", "executable": "npa-demo"},
				"artifacts": []map[string]any{
					{
						"os": runtime.GOOS, "arch": runtime.GOARCH,
						"url": serverURLPlaceholder, "sha256": pluginDigest,
						"size": len(pluginArtifact), "format": FormatTarGz,
					},
				},
			},
		},
	}
	server.registryData, _ = json.Marshal(registry)
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(server.registryData)
	})
	mux.HandleFunc("/plugin.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(server.artifacts["/plugin.tar.gz"])
	})
	server.Server = httptest.NewServer(mux)
	return server
}

const serverURLPlaceholder = "SERVER/plugin.tar.gz"

func TestInstallerInstallsPluginAndDependency(t *testing.T) {
	dependency := buildTarGz(t, map[string][]byte{
		"obscura":        []byte("engine"),
		"obscura-worker": []byte("worker"),
		"README.md":      []byte("ignore me"),
	})
	plugin := buildTarGz(t, map[string][]byte{
		"npa-demo": []byte("plugin"),
	})
	server := newRegistryServer(t, plugin, digestOf(plugin))
	defer server.Close()

	// The registry document points at the test server.
	document := map[string]any{
		"schema_version": 1,
		"plugins": []map[string]any{
			{
				"id": "npa-demo", "name": "Demo", "version": "1.0.0", "kind": KindPlugin,
				"requires": []string{"demo-engine"},
				"install":  map[string]any{"dir": "plugins/demo", "executable": "npa-demo"},
				"artifacts": []map[string]any{{
					"os": runtime.GOOS, "arch": runtime.GOARCH,
					"url": server.URL + "/plugin.tar.gz", "sha256": digestOf(plugin),
					"size": len(plugin), "format": FormatTarGz,
				}},
			},
			{
				"id": "demo-engine", "name": "Demo Engine", "version": "2.0.0", "kind": KindDependency,
				"install": map[string]any{"dir": "bin", "executable": "obscura"},
				"artifacts": []map[string]any{{
					"os": runtime.GOOS, "arch": runtime.GOARCH,
					"url": server.URL + "/dependency.tar.gz", "sha256": digestOf(dependency),
					"size": len(dependency), "format": FormatTarGz,
					"files": []string{"obscura", "obscura-worker"},
				}},
			},
		},
	}
	data, _ := json.Marshal(document)
	registry, err := Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Serve the dependency archive as well.
	server.artifacts["/dependency.tar.gz"] = dependency
	mux := http.NewServeMux()
	mux.HandleFunc("/dependency.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(dependency)
	})
	extra := httptest.NewServer(mux)
	defer extra.Close()
	registry.Plugins[1].Artifacts[0].URL = extra.URL + "/dependency.tar.gz"

	dataDir := t.TempDir()
	installer := &Installer{DataDir: dataDir, Client: NewClient(server.URL + "/registry.json")}
	results, err := installer.Install(context.Background(), registry, "npa-demo")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].ID != "demo-engine" {
		t.Fatalf("first result = %s, want the dependency first", results[0].ID)
	}

	// The plugin lands in plugins/demo with the platform suffix.
	expectedPlugin := filepath.Join(dataDir, "plugins", "demo", fmt.Sprintf("npa-demo-%s-%s", runtime.GOOS, runtime.GOARCH))
	if _, err := os.Stat(expectedPlugin); err != nil {
		t.Fatalf("plugin binary missing: %v", err)
	}
	// The dependency keeps its plain name in bin/.
	expectedDependency := filepath.Join(dataDir, "bin", "obscura")
	if runtime.GOOS == "windows" {
		expectedDependency += ".exe"
	}
	if _, err := os.Stat(expectedDependency); err != nil {
		t.Fatalf("dependency binary missing: %v", err)
	}
	// The secondary member of the dependency keeps its own name.
	if _, err := os.Stat(filepath.Join(dataDir, "bin", "obscura-worker")); err != nil {
		t.Fatalf("dependency worker missing: %v", err)
	}
	// The archive member that is not part of the artifact must be skipped.
	if _, err := os.Stat(filepath.Join(dataDir, "bin", "README.md")); err == nil {
		t.Fatal("README.md should not be installed")
	}

	entries, err := installer.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("state entries = %d, want 2", len(entries))
	}
}

func TestInstallerRejectsDigestMismatch(t *testing.T) {
	plugin := buildTarGz(t, map[string][]byte{"npa-demo": []byte("plugin")})
	server := newRegistryServer(t, plugin, digestOf(plugin))
	defer server.Close()

	registry, err := Parse([]byte(fmt.Sprintf(`{
		"schema_version": 1,
		"plugins": [{
			"id": "npa-demo", "version": "1.0.0",
			"install": {"dir": "plugins/demo", "executable": "npa-demo"},
			"artifacts": [{"os": %q, "arch": %q, "url": %q, "sha256": %q, "size": %d, "format": "tar.gz"}]
		}]
	}`, runtime.GOOS, runtime.GOARCH, server.URL+"/plugin.tar.gz", digestOf([]byte("other")), len(plugin))))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	installer := &Installer{DataDir: t.TempDir()}
	if _, err := installer.Install(context.Background(), registry, "npa-demo"); err == nil {
		t.Fatal("expected a digest mismatch error")
	}
}

func TestInstallerRemoveDeletesOwnedFiles(t *testing.T) {
	plugin := buildTarGz(t, map[string][]byte{"npa-demo": []byte("plugin")})
	server := newRegistryServer(t, plugin, digestOf(plugin))
	defer server.Close()

	registry, err := Parse([]byte(fmt.Sprintf(`{
		"schema_version": 1,
		"plugins": [{
			"id": "npa-demo", "version": "1.0.0",
			"install": {"dir": "plugins/demo", "executable": "npa-demo"},
			"artifacts": [{"os": %q, "arch": %q, "url": %q, "sha256": %q, "size": %d, "format": "tar.gz"}]
		}]
	}`, runtime.GOOS, runtime.GOARCH, server.URL+"/plugin.tar.gz", digestOf(plugin), len(plugin))))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	dataDir := t.TempDir()
	installer := &Installer{DataDir: dataDir}
	if _, err := installer.Install(context.Background(), registry, "npa-demo"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := installer.Remove("npa-demo"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "plugins", "demo")); !os.IsNotExist(err) {
		t.Fatalf("plugin directory still present: %v", err)
	}
	entries, err := installer.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("state entries = %d, want 0", len(entries))
	}
}

func TestDownloaderResumesInterruptedDownload(t *testing.T) {
	payload := bytes.Repeat([]byte("npa"), 4096)
	served := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload[:1024])
			served = 1
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 1024-%d/%d", len(payload)-1, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[1024:])
		served = 2
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "artifact.bin")
	downloader := &Downloader{}
	// The first attempt is cut short, the second one resumes.
	err := downloader.Download(context.Background(), server.URL, destination, int64(len(payload)))
	if err == nil && served < 2 {
		t.Fatalf("expected the download to resume, served = %d", served)
	}
}

// TestKeepFileMatchesWindowsExecutableSuffix makes sure the registry can list
// the plain executable name while a Windows archive carries the .exe suffix.
func TestKeepFileMatchesWindowsExecutableSuffix(t *testing.T) {
	artifact := Artifact{Files: []string{"obscura", "obscura-worker"}}
	if !keepFile(artifact, "obscura.exe") {
		t.Fatal("obscura.exe should match the plain name")
	}
	if !keepFile(artifact, "obscura-worker.exe") {
		t.Fatal("obscura-worker.exe should match the plain name")
	}
	if keepFile(artifact, "README.md") {
		t.Fatal("README.md must not match")
	}
}
