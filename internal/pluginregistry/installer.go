package pluginregistry

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Installer installs registry entries into the NPA data directory.
type Installer struct {
	// DataDir is the NPA data directory, the root of every install path.
	DataDir string
	// Client downloads the registry document.
	Client *Client
	// Downloader downloads release assets.
	Downloader *Downloader
	// Progress reports download progress for the entry being installed.
	Progress func(id string, downloaded int64, total int64)
}

// InstallResult describes one installed entry.
type InstallResult struct {
	ID      string   `json:"id"`
	Version string   `json:"version"`
	Kind    string   `json:"kind"`
	Dir     string   `json:"dir"`
	Files   []string `json:"files"`
	OS      string   `json:"os"`
	Arch    string   `json:"arch"`
	Size    int64    `json:"size"`
}

// InstalledEntry is one entry of the installed state file.
type InstalledEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Version     string   `json:"version"`
	Kind        string   `json:"kind"`
	Dir         string   `json:"dir"`
	Files       []string `json:"files"`
	OS          string   `json:"os,omitempty"`
	Arch        string   `json:"arch,omitempty"`
	InstalledAt string   `json:"installed_at"`
}

type installState struct {
	Entries map[string]InstalledEntry `json:"entries"`
}

func (i *Installer) statePath() string {
	return filepath.Join(i.DataDir, "plugins", "installed.json")
}

// State returns every recorded installation.
func (i *Installer) State() ([]InstalledEntry, error) {
	data, err := os.ReadFile(i.statePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state installState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	entries := make([]InstalledEntry, 0, len(state.Entries))
	for _, entry := range state.Entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].ID < entries[b].ID })
	return entries, nil
}

// StateFor returns the recorded installation of one entry.
func (i *Installer) StateFor(id string) (InstalledEntry, bool) {
	entries, err := i.State()
	if err != nil {
		return InstalledEntry{}, false
	}
	for _, entry := range entries {
		if entry.ID == id {
			return entry, true
		}
	}
	return InstalledEntry{}, false
}

func (i *Installer) saveState(entries map[string]InstalledEntry) error {
	path := i.statePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(installState{Entries: entries}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Install downloads the entry with the given id together with every
// dependency it needs. Dependencies are installed first.
func (i *Installer) Install(ctx context.Context, registry *Registry, id string) ([]InstallResult, error) {
	if registry == nil {
		return nil, fmt.Errorf("plugin registry is empty")
	}
	ordered, err := registry.Resolve(id)
	if err != nil {
		return nil, err
	}
	downloader := i.Downloader
	if downloader == nil {
		downloader = &Downloader{}
	}
	results := make([]InstallResult, 0, len(ordered))
	for _, entry := range ordered {
		result, err := i.installEntry(ctx, entry, downloader)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (i *Installer) installEntry(ctx context.Context, entry Plugin, downloader *Downloader) (InstallResult, error) {
	artifact, ok := entry.ArtifactFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return InstallResult{}, fmt.Errorf("plugin %s has no build for %s/%s", entry.ID, runtime.GOOS, runtime.GOARCH)
	}
	targetDir := filepath.Join(i.DataDir, filepath.FromSlash(entry.Install.Dir))
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return InstallResult{}, err
	}
	archivePath := filepath.Join(os.TempDir(), fmt.Sprintf("npa-plugin-%s-%s%s", entry.ID, runtime.GOOS, archiveSuffix(artifact)))
	defer os.Remove(archivePath)

	progress := downloader.Progress
	if i.Progress != nil {
		progress = func(downloaded int64, total int64) { i.Progress(entry.ID, downloaded, total) }
	}
	local := &Downloader{HTTP: downloader.HTTP, Progress: progress}
	if err := local.Download(ctx, artifact.URL, archivePath, artifact.Size); err != nil {
		return InstallResult{}, fmt.Errorf("download plugin %s: %w", entry.ID, err)
	}
	if err := verifyDigest(archivePath, artifact.SHA256); err != nil {
		return InstallResult{}, fmt.Errorf("verify plugin %s: %w", entry.ID, err)
	}
	files, err := i.unpack(entry, artifact, archivePath, targetDir)
	if err != nil {
		return InstallResult{}, fmt.Errorf("unpack plugin %s: %w", entry.ID, err)
	}
	entryState := InstalledEntry{
		ID:          entry.ID,
		Name:        entry.Name,
		Version:     entry.Version,
		Kind:        entry.Kind,
		Dir:         filepath.ToSlash(entry.Install.Dir),
		Files:       files,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	entries, err := i.State()
	if err != nil {
		return InstallResult{}, err
	}
	state := installState{Entries: map[string]InstalledEntry{}}
	for _, item := range entries {
		state.Entries[item.ID] = item
	}
	state.Entries[entry.ID] = entryState
	if err := i.saveState(state.Entries); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{
		ID:      entry.ID,
		Version: entry.Version,
		Kind:    entry.Kind,
		Dir:     entryState.Dir,
		Files:   files,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Size:    artifact.Size,
	}, nil
}

// unpack extracts the artifact into the target directory and returns the
// installed files relative to the data directory.
func (i *Installer) unpack(entry Plugin, artifact Artifact, archivePath string, targetDir string) ([]string, error) {
	format := artifact.FormatOrDefault()
	switch format {
	case FormatBinary:
		name := i.fileName(entry, artifact, "")
		if err := installFile(archivePath, filepath.Join(targetDir, name), 0o755); err != nil {
			return nil, err
		}
		return []string{filepath.ToSlash(filepath.Join(entry.Install.Dir, name))}, nil
	case FormatTarGz:
		return i.unpackTarGz(entry, artifact, archivePath, targetDir)
	case FormatZip:
		return i.unpackZip(entry, artifact, archivePath, targetDir)
	default:
		return nil, fmt.Errorf("unsupported artifact format %q", format)
	}
}

func (i *Installer) unpackTarGz(entry Plugin, artifact Artifact, archivePath string, targetDir string) ([]string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	var installed []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(filepath.Clean(header.Name))
		if !keepFile(artifact, base) {
			continue
		}
		name := i.fileName(entry, artifact, base)
		destination := filepath.Join(targetDir, name)
		if err := writeStream(reader, destination, 0o755); err != nil {
			return nil, err
		}
		installed = append(installed, filepath.ToSlash(filepath.Join(entry.Install.Dir, name)))
	}
	return installed, nil
}

func (i *Installer) unpackZip(entry Plugin, artifact Artifact, archivePath string, targetDir string) ([]string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var installed []string
	for _, member := range reader.File {
		if member.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(filepath.Clean(member.Name))
		if !keepFile(artifact, base) {
			continue
		}
		name := i.fileName(entry, artifact, base)
		source, err := member.Open()
		if err != nil {
			return nil, err
		}
		err = writeStream(source, filepath.Join(targetDir, name), 0o755)
		source.Close()
		if err != nil {
			return nil, err
		}
		installed = append(installed, filepath.ToSlash(filepath.Join(entry.Install.Dir, name)))
	}
	return installed, nil
}

// fileName returns the name the artifact member is stored under. The entry
// point of a plugin is stored per platform so several builds can live in the
// same directory, dependencies keep their plain name.
func (i *Installer) fileName(entry Plugin, artifact Artifact, base string) string {
	primary := entry.Install.Executable
	if base != "" && !strings.EqualFold(strings.TrimSuffix(base, ".exe"), primary) {
		return base
	}
	name := primary
	if entry.Kind == KindPlugin {
		name = fmt.Sprintf("%s-%s-%s", primary, runtime.GOOS, runtime.GOARCH)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		name += ".exe"
	}
	return name
}

func keepFile(artifact Artifact, base string) bool {
	if base == "." || base == "" || base == ".." {
		return false
	}
	if len(artifact.Files) == 0 {
		return true
	}
	for _, wanted := range artifact.Files {
		// Windows archives carry the .exe suffix, the registry lists the plain name.
		if strings.EqualFold(strings.TrimSuffix(wanted, ".exe"), strings.TrimSuffix(base, ".exe")) {
			return true
		}
	}
	return false
}

func archiveSuffix(artifact Artifact) string {
	switch artifact.FormatOrDefault() {
	case FormatTarGz:
		return ".tar.gz"
	case FormatZip:
		return ".zip"
	default:
		return ".bin"
	}
}

// Remove deletes an installed entry and the files it owns.
func (i *Installer) Remove(id string) error {
	entries, err := i.State()
	if err != nil {
		return err
	}
	state := installState{Entries: map[string]InstalledEntry{}}
	var target InstalledEntry
	found := false
	for _, entry := range entries {
		if entry.ID == id {
			target = entry
			found = true
			continue
		}
		state.Entries[entry.ID] = entry
	}
	if !found {
		return fmt.Errorf("plugin %s is not installed", id)
	}
	for _, name := range target.Files {
		path := filepath.Join(i.DataDir, filepath.FromSlash(name))
		if !withinDataDir(i.DataDir, path) {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if target.Dir != "" {
		dir := filepath.Join(i.DataDir, filepath.FromSlash(target.Dir))
		_ = os.Remove(dir)
	}
	return i.saveState(state.Entries)
}

func withinDataDir(dataDir string, path string) bool {
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return false
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func installFile(source string, destination string, mode os.FileMode) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	return writeStream(file, destination, mode)
}

func writeStream(source io.Reader, destination string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	partial := destination + ".part"
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, source); err != nil {
		file.Close()
		os.Remove(partial)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(partial)
		return err
	}
	if err := os.Chmod(partial, mode); err != nil {
		os.Remove(partial)
		return err
	}
	return os.Rename(partial, destination)
}

func verifyDigest(path string, expected string) error {
	if expected == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(digest.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", actual, expected)
	}
	return nil
}

// Digest returns the sha256 digest of a file.
func Digest(path string) (string, error) {
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
