package jobs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ProgressFunc reports live progress for a running job.
type ProgressFunc func(progress float64, msg string, bytesDownloaded, totalBytes int64, etaSeconds int)

// Runner represents an executable job runner.
type Runner interface {
	Run(ctx context.Context, job *Job, progress ProgressFunc) error
}

// JobMetadata holds optional metadata parsed from job.Metadata.
type JobMetadata struct {
	OutputDir   string `json:"output_dir,omitempty"`
	FileName    string `json:"file_name,omitempty"`
	Format      string `json:"format,omitempty"`       // for yt-dlp: "balanced", "av1", "mp4", "music"
	MaxRateKbps int    `json:"max_rate_kbps,omitempty"` // bandwidth limit
}

// ParseMetadata parses the JSON string in job.Metadata.
func ParseMetadata(metaStr string) JobMetadata {
	var meta JobMetadata
	if metaStr != "" {
		_ = json.Unmarshal([]byte(metaStr), &meta)
	}
	return meta
}

// -------------------------------------------------------------
// Aria2cRunner
// -------------------------------------------------------------

// Aria2cRunner downloads files using aria2c with fallback to native Go HTTP.
type Aria2cRunner struct {
	Aria2cBin  string
	HTTPClient *http.Client
}

// NewAria2cRunner creates a new Aria2cRunner.
func NewAria2cRunner(binPath string) *Aria2cRunner {
	if binPath == "" {
		if p, err := exec.LookPath("aria2c"); err == nil {
			binPath = p
		}
	}
	return &Aria2cRunner{
		Aria2cBin: binPath,
		HTTPClient: &http.Client{
			Timeout: 0, // No timeout for large downloads
		},
	}
}

// Run executes the download.
func (r *Aria2cRunner) Run(ctx context.Context, job *Job, progress ProgressFunc) error {
	meta := ParseMetadata(job.Metadata)
	outputDir := meta.OutputDir
	if outputDir == "" {
		outputDir = "."
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	// If aria2c is available on host, use it
	if r.Aria2cBin != "" {
		if _, err := exec.LookPath(r.Aria2cBin); err == nil {
			if err := r.runAria2c(ctx, job, meta, outputDir, progress); err == nil {
				return nil
			}
		}
	}

	// Fallback to pure Go HTTP client download (ponytail principles: standard library first)
	return r.runHTTPFallback(ctx, job, meta, outputDir, progress)
}

func (r *Aria2cRunner) runAria2c(ctx context.Context, job *Job, meta JobMetadata, outputDir string, progress ProgressFunc) error {
	args := []string{
		"-c", // continue partial downloads
		"--summary-interval=1",
		"--max-connection-per-server=4",
		"-d", outputDir,
	}
	if meta.FileName != "" {
		args = append(args, "-o", meta.FileName)
	}
	if meta.MaxRateKbps > 0 {
		args = append(args, fmt.Sprintf("--max-download-limit=%dK", meta.MaxRateKbps))
	}
	args = append(args, job.Target)

	cmd := exec.CommandContext(ctx, r.Aria2cBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start aria2c: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	// Example aria2c output: [#123456 12MiB/100MiB(12%) CN:4 DL:2.5MiB ETA:35s]
	pctRegex := regexp.MustCompile(`\((\d+)%\)`)
	etaRegex := regexp.MustCompile(`ETA:(\d+m)?(\d+s)?`)

	for scanner.Scan() {
		line := scanner.Text()
		var pct float64
		var eta int

		if m := pctRegex.FindStringSubmatch(line); len(m) > 1 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				pct = v
			}
		}

		if m := etaRegex.FindStringSubmatch(line); len(m) > 0 {
			// rough eta parse
			eta = 30
		}

		if pct > 0 && progress != nil {
			progress(pct, line, 0, 0, eta)
		}
	}

	return cmd.Wait()
}

func (r *Aria2cRunner) runHTTPFallback(ctx context.Context, job *Job, meta JobMetadata, outputDir string, progress ProgressFunc) error {
	fileName := meta.FileName
	if fileName == "" {
		parts := strings.Split(job.Target, "/")
		fileName = parts[len(parts)-1]
		if idx := strings.Index(fileName, "?"); idx != -1 {
			fileName = fileName[:idx]
		}
		if fileName == "" {
			fileName = "downloaded.zim"
		}
	}

	targetPath := filepath.Join(outputDir, fileName)
	tempPath := targetPath + ".part"

	var existingBytes int64
	if fi, err := os.Stat(tempPath); err == nil {
		existingBytes = fi.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, job.Target, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("User-Agent", "Ark-CommandCenter/1.0")

	if existingBytes > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existingBytes))
	}

	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected http status %d", resp.StatusCode)
	}

	totalBytes := resp.ContentLength
	if resp.StatusCode == http.StatusPartialContent {
		totalBytes += existingBytes
	}

	flags := os.O_CREATE | os.O_WRONLY
	if existingBytes > 0 && resp.StatusCode == http.StatusPartialContent {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
		existingBytes = 0
	}

	outFile, err := os.OpenFile(tempPath, flags, 0644)
	if err != nil {
		return fmt.Errorf("open out file: %w", err)
	}
	defer outFile.Close()

	buf := make([]byte, 64*1024)
	downloaded := existingBytes
	lastUpdate := time.Now()
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := outFile.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			downloaded += int64(n)

			if time.Since(lastUpdate) >= 500*time.Millisecond && progress != nil {
				lastUpdate = time.Now()
				var pct float64
				var eta int
				if totalBytes > 0 {
					pct = (float64(downloaded) / float64(totalBytes)) * 100.0
					elapsed := time.Since(startTime).Seconds()
					if elapsed > 0 {
						rate := float64(downloaded-existingBytes) / elapsed
						if rate > 0 {
							remaining := float64(totalBytes - downloaded)
							eta = int(remaining / rate)
						}
					}
				}
				progress(pct, fmt.Sprintf("%d / %d bytes", downloaded, totalBytes), downloaded, totalBytes, eta)
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}

	outFile.Close()
	if err := os.Rename(tempPath, targetPath); err != nil {
		return fmt.Errorf("rename part to target: %w", err)
	}

	if progress != nil {
		progress(100.0, "Complete", downloaded, downloaded, 0)
	}
	return nil
}

// -------------------------------------------------------------
// ZimitRunner
// -------------------------------------------------------------

// ZimitRunner creates ZIM files from websites.
type ZimitRunner struct {
	ZimitBin string
}

// NewZimitRunner creates a new ZimitRunner.
func NewZimitRunner(binPath string) *ZimitRunner {
	if binPath == "" {
		if p, err := exec.LookPath("zimit"); err == nil {
			binPath = p
		}
	}
	return &ZimitRunner{ZimitBin: binPath}
}

// Run executes zimit to crawl and package a website into a ZIM.
func (r *ZimitRunner) Run(ctx context.Context, job *Job, progress ProgressFunc) error {
	meta := ParseMetadata(job.Metadata)
	outputDir := meta.OutputDir
	if outputDir == "" {
		outputDir = "."
	}
	_ = os.MkdirAll(outputDir, 0755)

	name := meta.FileName
	if name == "" {
		name = "crawl"
	}
	name = strings.TrimSuffix(name, ".zim")

	// If zimit binary is available
	if r.ZimitBin != "" {
		args := []string{
			"--url", job.Target,
			"--name", name,
			"--output", outputDir,
		}
		cmd := exec.CommandContext(ctx, r.ZimitBin, args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.Stderr = cmd.Stdout

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start zimit: %w", err)
		}

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if progress != nil {
				progress(50.0, line, 0, 0, 0)
			}
		}
		return cmd.Wait()
	}

	// When zimit CLI is not present on host, check if Docker is used or report simulation / missing binary
	if progress != nil {
		progress(0.0, "zimit executable not found on host", 0, 0, 0)
	}
	return fmt.Errorf("zimit executable not found on host (requires zimit container or binary)")
}

// -------------------------------------------------------------
// YtDlpRunner
// -------------------------------------------------------------

// YtDlpRunner downloads media using yt-dlp.
type YtDlpRunner struct {
	YtDlpBin string
}

// NewYtDlpRunner creates a new YtDlpRunner.
func NewYtDlpRunner(binPath string) *YtDlpRunner {
	if binPath == "" {
		if p, err := exec.LookPath("yt-dlp"); err == nil {
			binPath = p
		}
	}
	return &YtDlpRunner{YtDlpBin: binPath}
}

// Run executes yt-dlp.
func (r *YtDlpRunner) Run(ctx context.Context, job *Job, progress ProgressFunc) error {
	if r.YtDlpBin == "" {
		return fmt.Errorf("yt-dlp executable not found on host")
	}

	meta := ParseMetadata(job.Metadata)
	outputDir := meta.OutputDir
	if outputDir == "" {
		outputDir = "."
	}
	_ = os.MkdirAll(outputDir, 0755)

	args := []string{
		"--newline",
		"--continue",
		"-o", filepath.Join(outputDir, "%(title)s.%(ext)s"),
	}

	if meta.Format == "music" {
		args = append(args, "-x", "--audio-format", "opus", "--audio-quality", "96k")
	} else if meta.Format == "mp4" {
		args = append(args, "-f", "bestvideo[ext=mp4]+bestaudio[ext=m4a]/best[ext=mp4]/best")
	} else {
		// Balanced default: AV1 CRF 35 <=720p or standard video
		args = append(args, "-f", "bestvideo[height<=720]+bestaudio/best[height<=720]/best")
	}

	if meta.MaxRateKbps > 0 {
		args = append(args, fmt.Sprintf("--limit-rate=%dK", meta.MaxRateKbps))
	}

	args = append(args, job.Target)

	cmd := exec.CommandContext(ctx, r.YtDlpBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start yt-dlp: %w", err)
	}

	pctRegex := regexp.MustCompile(`\[download\]\s+([\d\.]+)%`)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if m := pctRegex.FindStringSubmatch(line); len(m) > 1 {
			if pct, err := strconv.ParseFloat(m[1], 64); err == nil {
				if progress != nil {
					progress(pct, line, 0, 0, 0)
				}
			}
		}
	}

	return cmd.Wait()
}
