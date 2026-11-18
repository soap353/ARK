package update

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultGitHubRepo is the upstream GitHub repository checked for self-updates.
const DefaultGitHubRepo = "soap353/ARK"

// GitHubAsset mirrors a release asset returned by the GitHub Releases API.
type GitHubAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// GitHubReleaseInfo summarizes the latest GitHub release relative to currentVersion.
type GitHubReleaseInfo struct {
	Repo          string `json:"repo"`
	CurrentVer    string `json:"current_version"`
	TagName       string `json:"tag_name"`
	Version       string `json:"version"`
	Name          string `json:"name"`
	Body          string `json:"body"`
	HTMLURL       string `json:"html_url"`
	PublishedAt   string `json:"published_at"`
	HasUpdate     bool   `json:"has_update"`
	AssetName     string `json:"asset_name,omitempty"`
	AssetURL      string `json:"asset_url,omitempty"`
	AssetSize     int64  `json:"asset_size,omitempty"`
	ChecksumsURL  string `json:"checksums_url,omitempty"`
	ManifestURL   string `json:"manifest_url,omitempty"`
}

// ExpectedAssetName returns the canonical release binary filename for the current OS/arch.
func ExpectedAssetName() string {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("ark-%s-%s%s", runtime.GOOS, runtime.GOARCH, ext)
}

// IsNewerVersion returns true if candidate semver (e.g. "v0.4.0") is strictly newer than current.
func IsNewerVersion(candidate, current string) bool {
	cParts := parseSemver(candidate)
	curParts := parseSemver(current)
	for i := 0; i < 3; i++ {
		if cParts[i] > curParts[i] {
			return true
		}
		if cParts[i] < curParts[i] {
			return false
		}
	}
	return false
}

func parseSemver(raw string) [3]int {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "v")
	if idx := strings.IndexAny(s, "-+"); idx != -1 {
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err == nil && n >= 0 {
			out[i] = n
		}
	}
	return out
}

// CheckGitHubRelease queries GitHub Releases API (default https://api.github.com) for the latest release.
func CheckGitHubRelease(ctx context.Context, client *http.Client, baseAPIURL, repo, currentVersion string) (*GitHubReleaseInfo, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if strings.TrimSpace(baseAPIURL) == "" {
		baseAPIURL = "https://api.github.com"
	}
	if strings.TrimSpace(repo) == "" {
		repo = DefaultGitHubRepo
	}

	endpoint := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(baseAPIURL, "/"), strings.Trim(repo, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Ark-CommandCenter/"+currentVersion)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact GitHub Releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no releases published yet on github.com/%s", repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API status %d", resp.StatusCode)
	}

	var payload struct {
		TagName     string        `json:"tag_name"`
		Name        string        `json:"name"`
		Body        string        `json:"body"`
		HTMLURL     string        `json:"html_url"`
		PublishedAt string        `json:"published_at"`
		Assets      []GitHubAsset `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode GitHub release: %w", err)
	}

	ver := strings.TrimPrefix(strings.TrimSpace(payload.TagName), "v")
	info := &GitHubReleaseInfo{
		Repo:        repo,
		CurrentVer:  currentVersion,
		TagName:     payload.TagName,
		Version:     ver,
		Name:        payload.Name,
		Body:        payload.Body,
		HTMLURL:     payload.HTMLURL,
		PublishedAt: strings.Split(payload.PublishedAt, "T")[0],
		HasUpdate:   IsNewerVersion(ver, currentVersion),
	}

	wantAsset := strings.ToLower(ExpectedAssetName())
	for _, a := range payload.Assets {
		low := strings.ToLower(a.Name)
		switch {
		case low == wantAsset:
			info.AssetName = a.Name
			info.AssetURL = a.BrowserDownloadURL
			info.AssetSize = a.Size
		case low == "checksums.txt" || low == "sha256sums.txt":
			info.ChecksumsURL = a.BrowserDownloadURL
		case low == "update-manifest.json":
			info.ManifestURL = a.BrowserDownloadURL
		}
	}
	// Fallback match if asset name includes version (e.g. ark-v0.4.0-windows-amd64.exe)
	if info.AssetURL == "" {
		osArch := strings.ToLower(runtime.GOOS + "-" + runtime.GOARCH)
		for _, a := range payload.Assets {
			low := strings.ToLower(a.Name)
			if strings.Contains(low, osArch) && !strings.HasSuffix(low, ".sha256") && !strings.HasSuffix(low, ".sig") {
				info.AssetName = a.Name
				info.AssetURL = a.BrowserDownloadURL
				info.AssetSize = a.Size
				break
			}
		}
	}

	return info, nil
}

// DownloadAndApplyGitHubRelease downloads the release asset, verifies Ed25519 manifest or SHA-256 checksum,
// and stages the binary update with automatic DB backup.
func DownloadAndApplyGitHubRelease(ctx context.Context, client *http.Client, rel *GitHubReleaseInfo, exePath, dataDir string) (string, error) {
	if rel == nil || rel.AssetURL == "" {
		return "", fmt.Errorf("no release binary found for %s/%s in %s", runtime.GOOS, runtime.GOARCH, func() string {
			if rel != nil {
				return rel.TagName
			}
			return "release"
		}())
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}

	stageDir, err := os.MkdirTemp("", "ark-gh-update-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stageDir)

	cleanAsset := filepath.Base(rel.AssetName)
	if cleanAsset == "" || cleanAsset == "." {
		cleanAsset = ExpectedAssetName()
	}
	binStagePath := filepath.Join(stageDir, cleanAsset)
	if err := downloadURLToFile(ctx, client, rel.AssetURL, binStagePath); err != nil {
		return "", fmt.Errorf("download release binary: %w", err)
	}

	verifiedVia := "unverified binary size check"
	verified := false

	// 1. Prefer signed update-manifest.json when present
	if rel.ManifestURL != "" {
		manPath := filepath.Join(stageDir, "update-manifest.json")
		if err := downloadURLToFile(ctx, client, rel.ManifestURL, manPath); err == nil {
			if pubKey, pkErr := DefaultPublicKey(); pkErr == nil {
				if _, _, verErr := LoadUpdateFromDir(stageDir, pubKey); verErr == nil {
					verified = true
					verifiedVia = "Ed25519 signature + SHA-256"
				}
			}
		}
	}

	// 2. Fallback to checksums.txt verification for standard GitHub Releases
	if !verified && rel.ChecksumsURL != "" {
		csPath := filepath.Join(stageDir, "checksums.txt")
		if err := downloadURLToFile(ctx, client, rel.ChecksumsURL, csPath); err != nil {
			return "", fmt.Errorf("download checksums.txt: %w", err)
		}
		expectedHex, err := lookupChecksumInFile(csPath, cleanAsset)
		if err != nil {
			return "", err
		}
		if err := VerifyFileSHA256(binStagePath, expectedHex); err != nil {
			return "", err
		}
		verified = true
		verifiedVia = "SHA-256 (checksums.txt)"
	}

	if !verified {
		fi, err := os.Stat(binStagePath)
		if err != nil || fi.Size() < 1024 {
			return "", errors.New("downloaded binary is invalid or empty")
		}
	}

	if err := ApplyUpdate(exePath, binStagePath, dataDir, ArkVersion, rel.Version); err != nil {
		return "", err
	}
	return verifiedVia, nil
}

func downloadURLToFile(ctx context.Context, client *http.Client, rawURL, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Ark-CommandCenter/"+ArkVersion)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

func lookupChecksumInFile(path, assetName string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			fname := filepath.Base(strings.TrimPrefix(fields[1], "*"))
			if strings.EqualFold(fname, assetName) {
				return fields[0], nil
			}
		}
	}
	return "", fmt.Errorf("checksum entry not found for %s in checksums.txt", assetName)
}
