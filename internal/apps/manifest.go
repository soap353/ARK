package apps

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidCategories defines the allowed category values for Ark modules.
var ValidCategories = map[string]bool{
	"knowledge": true,
	"maps":      true,
	"media":     true,
	"ai":        true,
	"tools":     true,
	"education": true,
	"medical":   true,
}

var idRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// AppManifest represents an app.yaml specification for an Ark module.
type AppManifest struct {
	Version     int               `yaml:"version"`
	ID          string            `yaml:"id"`
	Name        string            `yaml:"name"`
	Category    string            `yaml:"category"`
	Description string            `yaml:"description,omitempty"`
	Icon        string            `yaml:"icon,omitempty"`
	Image       string            `yaml:"image"`
	Command     []string          `yaml:"command,omitempty"`
	Port        int               `yaml:"port"`
	Proxy       string            `yaml:"proxy"` // "path" (default) or "port"
	OpenPath    string            `yaml:"open_path,omitempty"`
	OpenURL     string            `yaml:"open_url,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"`
	Healthcheck string            `yaml:"healthcheck,omitempty"`
	Lan         string            `yaml:"lan"` // "read" (default) or "admin"
	Compose     string            `yaml:"compose,omitempty"`

	// Dir is set to the directory containing the app.yaml file
	Dir string `yaml:"-"`
}

// ParseManifest parses an AppManifest from an io.Reader and validates it.
func ParseManifest(r io.Reader) (*AppManifest, error) {
	dec := yaml.NewDecoder(r)
	var m AppManifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("failed to decode app manifest yaml: %w", err)
	}

	// Apply defaults
	if m.Proxy == "" {
		m.Proxy = "path"
	}
	if m.Lan == "" {
		m.Lan = "read"
	}

	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadManifest loads and validates an AppManifest from a file path.
func LoadManifest(filePath string) (*AppManifest, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open manifest file: %w", err)
	}
	defer f.Close()

	m, err := ParseManifest(f)
	if err != nil {
		return nil, fmt.Errorf("invalid manifest at %s: %w", filePath, err)
	}
	m.Dir = filepath.Dir(filePath)
	return m, nil
}

// LoadAllManifests scans a directory (e.g. catalog/ or ArkData/apps/) for subdirectories with app.yaml.
func LoadAllManifests(baseDir string) ([]*AppManifest, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", baseDir, err)
	}

	var manifests []*AppManifest
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		yamlPath := filepath.Join(baseDir, entry.Name(), "app.yaml")
		if _, err := os.Stat(yamlPath); err == nil {
			m, err := LoadManifest(yamlPath)
			if err != nil {
				return nil, err
			}
			manifests = append(manifests, m)
		}
	}
	return manifests, nil
}

// Validate checks that all required fields are present and valid according to Ark specs.
func (m *AppManifest) Validate() error {
	if m.Version <= 0 {
		return errors.New("field 'version' must be a positive integer")
	}

	if strings.TrimSpace(m.ID) == "" {
		return errors.New("field 'id' is required")
	}
	if !idRegex.MatchString(m.ID) {
		return fmt.Errorf("field 'id' (%q) must be alphanumeric with lowercase letters, numbers, hyphens, or underscores", m.ID)
	}

	if strings.TrimSpace(m.Name) == "" {
		return errors.New("field 'name' is required")
	}

	if strings.TrimSpace(m.Category) == "" {
		return errors.New("field 'category' is required")
	}
	if !ValidCategories[m.Category] {
		return fmt.Errorf("field 'category' (%q) must be one of: knowledge, maps, media, ai, tools, education, medical", m.Category)
	}

	if strings.TrimSpace(m.Image) == "" && strings.TrimSpace(m.Compose) == "" {
		return errors.New("either 'image' or 'compose' is required")
	}

	if m.Port < 1 || m.Port > 65535 {
		return fmt.Errorf("field 'port' (%d) must be between 1 and 65535", m.Port)
	}

	if m.Proxy != "path" && m.Proxy != "port" {
		return fmt.Errorf("field 'proxy' (%q) must be 'path' or 'port'", m.Proxy)
	}

	if m.Lan != "read" && m.Lan != "admin" {
		return fmt.Errorf("field 'lan' (%q) must be 'read' or 'admin'", m.Lan)
	}

	for _, v := range m.Volumes {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return fmt.Errorf("invalid volume specification %q: expected format host:container[:ro|rw]", v)
		}
		if len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw" {
			return fmt.Errorf("invalid volume mode %q in %q: must be 'ro' or 'rw'", parts[2], v)
		}
	}

	return nil
}

// ResolveBinds converts volume specs (e.g. "zim:/data/zim:ro") into host absolute paths relative to dataDir.
func (m *AppManifest) ResolveBinds(dataDir string) []string {
	binds := make([]string, 0, len(m.Volumes))
	for _, v := range m.Volumes {
		parts := strings.Split(v, ":")
		if len(parts) < 2 {
			continue
		}
		hostPart := parts[0]
		containerPart := parts[1]
		mode := "rw"
		if len(parts) == 3 {
			mode = parts[2]
		}

		// If hostPart is already an absolute path or named volume
		var resolvedHost string
		if filepath.IsAbs(hostPart) || strings.HasPrefix(hostPart, "/") || strings.HasPrefix(hostPart, "\\") {
			resolvedHost = hostPart
		} else if strings.HasPrefix(hostPart, "ark-") {
			// Named docker volume like ark-qdrant
			resolvedHost = hostPart
		} else {
			// Subfolder inside ArkData (must be absolute for Docker bind mounts)
			joined := filepath.Join(dataDir, hostPart)
			if abs, err := filepath.Abs(joined); err == nil {
				resolvedHost = abs
			} else {
				resolvedHost = joined
			}
			_ = os.MkdirAll(resolvedHost, 0755)
		}

		binds = append(binds, fmt.Sprintf("%s:%s:%s", resolvedHost, containerPart, mode))
	}
	return binds
}
