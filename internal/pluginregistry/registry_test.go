package pluginregistry

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleRegistry() []byte {
	registry := map[string]any{
		"schema_version": 1,
		"updated_at":     "2026-11-24T00:00:00Z",
		"plugins": []map[string]any{
			{
				"id":           "npa-obscura",
				"name":         "NPA Obscura Browser",
				"version":      "1.0.0",
				"kind":         KindPlugin,
				"capabilities": []string{"browser"},
				"requires":     []string{"obscura-engine"},
				"install":      map[string]any{"dir": "plugins/browser", "executable": "npa-obscura"},
				"artifacts": []map[string]any{
					{
						"os": "darwin", "arch": "amd64", "url": "http://example/npa-obscura-darwin-amd64",
						"sha256": strings.Repeat("a", 64), "size": 1234, "format": FormatBinary,
					},
					{
						"os": "linux", "arch": "arm64", "url": "http://example/npa-obscura-linux-arm64",
						"sha256": strings.Repeat("b", 64), "size": 2345, "format": FormatBinary,
					},
				},
			},
			{
				"id":      "obscura-engine",
				"name":    "Obscura Engine",
				"version": "0.2.7",
				"kind":    KindDependency,
				"install": map[string]any{"dir": "bin", "executable": "obscura"},
				"artifacts": []map[string]any{
					{
						"os": "darwin", "arch": "amd64", "url": "http://example/obscura.tar.gz",
						"sha256": strings.Repeat("c", 64), "size": 3456, "format": FormatTarGz,
						"files": []string{"obscura", "obscura-worker"},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(registry)
	return data
}

func TestParseReadsEntries(t *testing.T) {
	registry, err := Parse(sampleRegistry())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if registry.SchemaVersion != 1 {
		t.Fatalf("schema version = %d, want 1", registry.SchemaVersion)
	}
	if len(registry.Plugins) != 2 {
		t.Fatalf("plugins = %d, want 2", len(registry.Plugins))
	}
	entry, ok := registry.Find("npa-obscura")
	if !ok {
		t.Fatal("npa-obscura not found")
	}
	if entry.Install.Dir != "plugins/browser" || entry.Install.Executable != "npa-obscura" {
		t.Fatalf("unexpected install info: %+v", entry.Install)
	}
	if len(entry.Requires) != 1 || entry.Requires[0] != "obscura-engine" {
		t.Fatalf("unexpected requires: %v", entry.Requires)
	}
}

func TestParseRejectsIncompleteEntries(t *testing.T) {
	cases := map[string]map[string]any{
		"missing schema version": {"plugins": []map[string]any{}},
		"missing id": {
			"schema_version": 1,
			"plugins": []map[string]any{{
				"version": "1.0.0",
				"install": map[string]any{"dir": "plugins/x", "executable": "x"},
			}},
		},
		"missing install dir": {
			"schema_version": 1,
			"plugins": []map[string]any{{
				"id":      "x",
				"version": "1.0.0",
				"install": map[string]any{"executable": "x"},
			}},
		},
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(document)
			if _, err := Parse(data); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestResolveReturnsDependenciesFirst(t *testing.T) {
	registry, err := Parse(sampleRegistry())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolved, err := registry.Resolve("npa-obscura")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved = %d entries, want 2", len(resolved))
	}
	if resolved[0].ID != "obscura-engine" {
		t.Fatalf("first entry = %s, want the dependency first", resolved[0].ID)
	}
	if resolved[1].ID != "npa-obscura" {
		t.Fatalf("second entry = %s, want the plugin", resolved[1].ID)
	}
}

func TestResolveDetectsCycles(t *testing.T) {
	document := map[string]any{
		"schema_version": 1,
		"plugins": []map[string]any{
			{
				"id": "a", "version": "1", "requires": []string{"b"},
				"install": map[string]any{"dir": "plugins/a", "executable": "a"},
			},
			{
				"id": "b", "version": "1", "requires": []string{"a"},
				"install": map[string]any{"dir": "plugins/b", "executable": "b"},
			},
		},
	}
	data, _ := json.Marshal(document)
	registry, err := Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := registry.Resolve("a"); err == nil {
		t.Fatal("expected a cycle error")
	}
}

func TestResolveUnknownPlugin(t *testing.T) {
	registry, err := Parse(sampleRegistry())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := registry.Resolve("nope"); err == nil {
		t.Fatal("expected an error for an unknown plugin")
	}
}

func TestArtifactForNormalizesOSAndArch(t *testing.T) {
	registry, err := Parse(sampleRegistry())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	entry, _ := registry.Find("obscura-engine")
	artifact, ok := entry.ArtifactFor("macos", "x86_64")
	if !ok {
		t.Fatal("macos/x86_64 should match the darwin/amd64 artifact")
	}
	if artifact.URL != "http://example/obscura.tar.gz" {
		t.Fatalf("unexpected artifact: %+v", artifact)
	}
	if _, ok := entry.ArtifactFor("windows", "arm64"); ok {
		t.Fatal("windows/arm64 has no artifact")
	}
}

func TestArtifactFormatDefaultsToBinary(t *testing.T) {
	artifact := Artifact{}
	if artifact.FormatOrDefault() != FormatBinary {
		t.Fatalf("default format = %s, want %s", artifact.FormatOrDefault(), FormatBinary)
	}
}
