// Package pluginregistry implements the client side of the public NPA plugin
// repository. The repository keeps the index document (registry.json) in git
// and every binary artifact as a release asset, so the git history stays small
// while the assets remain downloadable anonymously.
//
// The index describes plugins as well as the dependencies they need, for every
// supported platform, together with the sha256 digest of each artifact. The
// installer downloads an artifact, verifies the digest, unpacks it into the
// NPA data directory and records what was installed.
package pluginregistry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SchemaVersion is the version of the registry document format understood by
// this package.
const SchemaVersion = 1

// Kinds of entries published in a registry.
const (
	// KindPlugin is a standalone NPA plugin process.
	KindPlugin = "plugin"
	// KindDependency is an executable a plugin depends on, such as the
	// obscura browser engine.
	KindDependency = "dependency"
)

// Archive formats of an artifact.
const (
	FormatBinary = "binary"
	FormatTarGz  = "tar.gz"
	FormatZip    = "zip"
)

// Registry is the root document published by the plugin repository.
type Registry struct {
	SchemaVersion int      `json:"schema_version"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
	Plugins       []Plugin `json:"plugins"`
}

// Plugin describes one entry of the registry: an installable plugin or one of
// its dependencies.
type Plugin struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Version      string     `json:"version"`
	Description  string     `json:"description,omitempty"`
	Kind         string     `json:"kind,omitempty"`
	Capabilities []string   `json:"capabilities,omitempty"`
	Homepage     string     `json:"homepage,omitempty"`
	Install      Install    `json:"install"`
	Requires     []string   `json:"requires,omitempty"`
	Artifacts    []Artifact `json:"artifacts"`
}

// Install describes where an entry is installed inside the NPA data
// directory and which file is the entry point.
type Install struct {
	// Dir is relative to the NPA data directory, for example
	// "plugins/browser" or "bin".
	Dir string `json:"dir"`
	// Executable is the base name of the entry point. Plugin kind entries
	// are stored as <executable>-<os>-<arch>, dependency kind entries keep
	// the plain name.
	Executable string `json:"executable"`
}

// Artifact is one downloadable platform specific build of an entry.
type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	// Format is binary, tar.gz or zip.
	Format string `json:"format,omitempty"`
	// Files lists the archive members to extract. When empty every member
	// of the archive is extracted.
	Files []string `json:"files,omitempty"`
}

// Parse decodes a registry document and validates its structure.
func Parse(data []byte) (*Registry, error) {
	var registry Registry
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("parse plugin registry: %w", err)
	}
	if registry.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("unsupported plugin registry schema version %d", registry.SchemaVersion)
	}
	if len(registry.Plugins) == 0 {
		return nil, fmt.Errorf("plugin registry is empty")
	}
	for i := range registry.Plugins {
		entry := &registry.Plugins[i]
		if entry.ID == "" {
			return nil, fmt.Errorf("plugin registry entry %d has no id", i)
		}
		if entry.Version == "" {
			return nil, fmt.Errorf("plugin %s has no version", entry.ID)
		}
		if entry.Install.Dir == "" {
			return nil, fmt.Errorf("plugin %s has no install directory", entry.ID)
		}
		if entry.Install.Executable == "" {
			return nil, fmt.Errorf("plugin %s has no install executable", entry.ID)
		}
		if entry.Kind == "" {
			entry.Kind = KindPlugin
		}
	}
	return &registry, nil
}

// Find returns the entry with the given id.
func (r *Registry) Find(id string) (Plugin, bool) {
	for _, entry := range r.Plugins {
		if entry.ID == id {
			return entry, true
		}
	}
	return Plugin{}, false
}

// Resolve returns the entry with the given id together with all of its
// dependencies, dependencies first, so the list can be installed in order.
func (r *Registry) Resolve(id string) ([]Plugin, error) {
	entry, ok := r.Find(id)
	if !ok {
		return nil, fmt.Errorf("plugin %s is not published in the registry", id)
	}
	visited := map[string]bool{}
	var ordered []Plugin
	var walk func(Plugin) error
	walk = func(current Plugin) error {
		if visited[current.ID] {
			return nil
		}
		if visited["cycle:"+current.ID] {
			return fmt.Errorf("dependency cycle detected at plugin %s", current.ID)
		}
		visited["cycle:"+current.ID] = true
		for _, dependency := range current.Requires {
			next, ok := r.Find(dependency)
			if !ok {
				return fmt.Errorf("plugin %s requires %s which is not published", current.ID, dependency)
			}
			if err := walk(next); err != nil {
				return err
			}
		}
		delete(visited, "cycle:"+current.ID)
		visited[current.ID] = true
		ordered = append(ordered, current)
		return nil
	}
	if err := walk(entry); err != nil {
		return nil, err
	}
	return ordered, nil
}

// ArtifactFor returns the artifact built for the given platform.
func (p Plugin) ArtifactFor(goos, goarch string) (Artifact, bool) {
	wantOS, wantArch := normalizeOS(goos), normalizeArch(goarch)
	for _, artifact := range p.Artifacts {
		if normalizeOS(artifact.OS) == wantOS && normalizeArch(artifact.Arch) == wantArch {
			return artifact, true
		}
	}
	return Artifact{}, false
}

// PlatformTags returns the platform identifiers of one artifact entry.
func (a Artifact) PlatformTags() []string {
	return []string{normalizeOS(a.OS), normalizeArch(a.Arch)}
}

// Format returns the artifact format, defaulting to a plain binary.
func (a Artifact) FormatOrDefault() string {
	if a.Format == "" {
		return FormatBinary
	}
	return a.Format
}

func normalizeOS(value string) string {
	switch strings.ToLower(value) {
	case "macos", "mac", "osx", "darwin":
		return "darwin"
	case "windows", "win":
		return "windows"
	default:
		return strings.ToLower(value)
	}
}

func normalizeArch(value string) string {
	switch strings.ToLower(value) {
	case "x86_64", "x64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "i386", "386":
		return "386"
	default:
		return strings.ToLower(value)
	}
}

// SortedIDs returns the ids of every published entry in alphabetical order.
func (r *Registry) SortedIDs() []string {
	ids := make([]string, 0, len(r.Plugins))
	for _, entry := range r.Plugins {
		ids = append(ids, entry.ID)
	}
	sort.Strings(ids)
	return ids
}
