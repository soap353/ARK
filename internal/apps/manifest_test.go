package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseManifest_Valid(t *testing.T) {
	yamlData := `
version: 1
id: kiwix
name: Library
category: knowledge
image: ghcr.io/kiwix/kiwix-serve:3.7.0
command: ["--urlRootLocation", "/a/kiwix", "/data/zim"]
port: 8080
proxy: path
volumes: ["zim:/data/zim:ro"]
healthcheck: /
lan: read
`
	m, err := ParseManifest(strings.NewReader(yamlData))
	if err != nil {
		t.Fatalf("unexpected error parsing valid manifest: %v", err)
	}
	if m.ID != "kiwix" || m.Name != "Library" || m.Category != "knowledge" {
		t.Errorf("unexpected manifest fields: %+v", m)
	}
	if m.Port != 8080 {
		t.Errorf("expected port 8080, got %d", m.Port)
	}
	if m.Proxy != "path" || m.Lan != "read" {
		t.Errorf("expected proxy: path, lan: read, got proxy: %s, lan: %s", m.Proxy, m.Lan)
	}
}

func TestParseManifest_Defaults(t *testing.T) {
	yamlData := `
version: 1
id: minimal-app
name: Minimal
category: tools
image: alpine:latest
port: 80
`
	m, err := ParseManifest(strings.NewReader(yamlData))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Proxy != "path" {
		t.Errorf("expected default proxy 'path', got %q", m.Proxy)
	}
	if m.Lan != "read" {
		t.Errorf("expected default lan 'read', got %q", m.Lan)
	}
}

func TestParseManifest_Invalid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"invalid version", `version: 0\nid: test\nname: Test\ncategory: tools\nimage: img\nport: 80`},
		{"missing id", `version: 1\nname: Test\ncategory: tools\nimage: img\nport: 80`},
		{"invalid id", `version: 1\nid: "Invalid ID!"\nname: Test\ncategory: tools\nimage: img\nport: 80`},
		{"missing name", `version: 1\nid: test\ncategory: tools\nimage: img\nport: 80`},
		{"invalid category", `version: 1\nid: test\nname: Test\ncategory: unknown_cat\nimage: img\nport: 80`},
		{"missing image", `version: 1\nid: test\nname: Test\ncategory: tools\nport: 80`},
		{"invalid port", `version: 1\nid: test\nname: Test\ncategory: tools\nimage: img\nport: 70000`},
		{"invalid proxy", `version: 1\nid: test\nname: Test\ncategory: tools\nimage: img\nport: 80\nproxy: invalid`},
		{"invalid lan", `version: 1\nid: test\nname: Test\ncategory: tools\nimage: img\nport: 80\nlan: secret`},
		{"invalid volume mode", `version: 1\nid: test\nname: Test\ncategory: tools\nimage: img\nport: 80\nvolumes: ["a:b:invalid"]`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest(strings.NewReader(strings.ReplaceAll(tc.yaml, `\n`, "\n")))
			if err == nil {
				t.Errorf("expected error for case %q, but got nil", tc.name)
			}
		})
	}
}

func TestResolveBinds(t *testing.T) {
	tmpDir := t.TempDir()
	m := &AppManifest{
		Volumes: []string{
			"zim:/data/zim:ro",
			"notes:/data:rw",
			"ark-qdrant:/qdrant/storage:rw",
		},
	}
	binds := m.ResolveBinds(tmpDir)
	if len(binds) != 3 {
		t.Fatalf("expected 3 binds, got %d", len(binds))
	}

	expectedZim := filepath.Join(tmpDir, "zim") + ":/data/zim:ro"
	if binds[0] != expectedZim {
		t.Errorf("expected bind %q, got %q", expectedZim, binds[0])
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "zim")); os.IsNotExist(err) {
		t.Errorf("expected data subdirectory to be created")
	}

	if binds[2] != "ark-qdrant:/qdrant/storage:rw" {
		t.Errorf("named volume should be preserved as-is, got %q", binds[2])
	}
}

func TestCatalogManifests(t *testing.T) {
	catalogDir := filepath.Join("..", "..", "catalog")
	manifests, err := LoadAllManifests(catalogDir)
	if err != nil {
		t.Fatalf("failed to load catalog manifests: %v", err)
	}
	if len(manifests) < 7 {
		t.Errorf("expected at least 7 catalog apps, found %d", len(manifests))
	}

	expectedApps := map[string]bool{
		"maps":           false,
		"cyberchef":      false,
		"flatnotes":      false,
		"convertx":       false,
		"druglabels":     false,
		"kolibri":        false,
		"libretranslate": false,
	}

	for _, m := range manifests {
		expectedApps[m.ID] = true
	}

	for id, found := range expectedApps {
		if !found {
			t.Errorf("expected catalog app %q was not found or failed validation", id)
		}
	}
}
