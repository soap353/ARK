package apps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var validAppIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,40}$`)

// SanitizeCustomAppID converts user-provided names/IDs into a safe lowercase container ID slug.
func SanitizeCustomAppID(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, " ", "-")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-_")
	if !validAppIDRe.MatchString(out) {
		return ""
	}
	return out
}

// SaveCustomManifest stores a custom container definition under <dataDir>/custom_apps/<id>.json.
func SaveCustomManifest(dataDir string, m *AppManifest) error {
	if dataDir == "" || m == nil || m.ID == "" {
		return fmt.Errorf("invalid custom app manifest")
	}
	dir := filepath.Join(dataDir, "custom_apps")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, m.ID+".json"), raw, 0644)
}

// LoadCustomManifests reads all user-added custom container manifests from <dataDir>/custom_apps.
func LoadCustomManifests(dataDir string) []*AppManifest {
	if dataDir == "" {
		return nil
	}
	dir := filepath.Join(dataDir, "custom_apps")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []*AppManifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m AppManifest
		if err := json.Unmarshal(raw, &m); err != nil || m.ID == "" {
			continue
		}
		out = append(out, &m)
	}
	return out
}

// LoadCustomManifest loads one custom manifest by appID if it exists.
func LoadCustomManifest(dataDir, appID string) (*AppManifest, error) {
	if dataDir == "" || appID == "" {
		return nil, os.ErrNotExist
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "custom_apps", appID+".json"))
	if err != nil {
		return nil, err
	}
	var m AppManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// DeleteCustomManifest deletes a user-added custom container manifest from disk.
func DeleteCustomManifest(dataDir, appID string) error {
	if dataDir == "" || appID == "" {
		return nil
	}
	err := os.Remove(filepath.Join(dataDir, "custom_apps", appID+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
