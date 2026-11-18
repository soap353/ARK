package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ark/internal/kiwix"
)

// MapExtractRunner extracts country or world PMTiles archives from Protomaps builds via go-pmtiles.
type MapExtractRunner struct {
	MetadataURL string
}

// NewMapExtractRunner creates a MapExtractRunner pointing at Protomaps public build metadata.
func NewMapExtractRunner() *MapExtractRunner {
	return &MapExtractRunner{
		MetadataURL: "https://build-metadata.protomaps.dev/builds.json",
	}
}

// Run resolves the latest Protomaps build key and extracts the requested country/world bbox into mapsDir.
func (r *MapExtractRunner) Run(ctx context.Context, job *Job, progress ProgressFunc) error {
	meta := ParseMetadata(job.Metadata)
	outDir := meta.OutputDir
	if outDir == "" {
		outDir = filepath.Join(".", "data", "maps")
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("create maps dir: %w", err)
	}

	opt := kiwix.FindCountryMapBySlug(job.Target)
	if opt == nil {
		return fmt.Errorf("unknown map extract slug: %s", job.Target)
	}

	if progress != nil {
		progress(5, "Resolving latest Protomaps build...", 0, 0, 0)
	}
	buildKey, err := r.latestBuildKey(ctx)
	if err != nil {
		return err
	}
	remoteURL := "https://build.protomaps.com/" + strings.TrimPrefix(buildKey, "/")
	outFile := opt.Slug + ".pmtiles"

	if progress != nil {
		progress(15, fmt.Sprintf("Extracting %s (%s) via go-pmtiles...", opt.Name, opt.SizeEstimate), 0, 0, 0)
	}

	// Try go-pmtiles locally first, else invoke Docker image ghcr.io/protomaps/go-pmtiles:latest
	absOutDir, _ := filepath.Abs(outDir)
	args := []string{
		"extract", remoteURL, outFile,
		"--bbox=" + opt.BBox,
		fmt.Sprintf("--maxzoom=%d", opt.MaxZoom),
	}

	var cmd *exec.Cmd
	if bin, lookErr := exec.LookPath("go-pmtiles"); lookErr == nil {
		args[1] = remoteURL
		args[2] = filepath.Join(absOutDir, outFile)
		cmd = exec.CommandContext(ctx, bin, args...)
	} else {
		dockerArgs := []string{
			"run", "--rm",
			"-v", absOutDir + ":/data",
			"ghcr.io/protomaps/go-pmtiles:latest",
			"extract", remoteURL, "/data/" + outFile,
			"--bbox=" + opt.BBox,
			fmt.Sprintf("--maxzoom=%d", opt.MaxZoom),
		}
		cmd = exec.CommandContext(ctx, "docker", dockerArgs...)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go-pmtiles extract failed: %v (%s)", err, strings.TrimSpace(string(output)))
	}

	if progress != nil {
		progress(100, "Saved "+outFile, 0, 0, 0)
	}
	return nil
}

func (r *MapExtractRunner) latestBuildKey(ctx context.Context) (string, error) {
	url := r.MetadataURL
	if url == "" {
		url = "https://build-metadata.protomaps.dev/builds.json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch protomaps builds: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("protomaps metadata status %d", resp.StatusCode)
	}

	var builds []struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&builds); err != nil {
		return "", err
	}
	if len(builds) == 0 {
		return "", fmt.Errorf("no protomaps builds returned")
	}
	return builds[len(builds)-1].Key, nil
}
