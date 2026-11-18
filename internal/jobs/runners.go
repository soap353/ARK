package jobs

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ark/internal/kiwix"
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
	transport := &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16,
		MaxConnsPerHost:     16,
		ReadBufferSize:      256 * 1024,
		WriteBufferSize:     256 * 1024,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
	}
	return &Aria2cRunner{
		Aria2cBin: binPath,
		HTTPClient: &http.Client{
			Timeout:   0, // No timeout for large downloads
			Transport: transport,
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

	targetURL := job.Target
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		if resources := kiwix.ResolvePresetResources(targetURL); len(resources) > 0 {
			return r.runResourcePack(ctx, job, meta, outputDir, resources, progress)
		}
		if progress != nil {
			progress(1.0, fmt.Sprintf("Resolving mirror for preset '%s'...", targetURL), 0, 0, 0)
		}
		resolved, err := r.resolvePresetURL(ctx, targetURL)
		if err != nil {
			return fmt.Errorf("resolve preset %q: %w", targetURL, err)
		}
		targetURL = resolved
		jobCopy := *job
		jobCopy.Target = targetURL
		job = &jobCopy
	}

	return r.downloadSingleURL(ctx, job, meta, outputDir, progress)
}

func (r *Aria2cRunner) runResourcePack(ctx context.Context, job *Job, meta JobMetadata, baseOutputDir string, resources []kiwix.ContentResource, progress ProgressFunc) error {
	total := len(resources)
	completedAny := false
	var lastErr error

	for idx, res := range resources {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		basePct := (float64(idx) / float64(total)) * 100.0
		spanPct := 100.0 / float64(total)

		destDir := baseOutputDir
		if res.Kind == "pmtiles" && filepath.Base(destDir) == "zim" {
			destDir = filepath.Join(filepath.Dir(destDir), "maps")
		}
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return fmt.Errorf("create output dir: %w", err)
		}

		dlURL := res.FallbackURL
		if res.KiwixQuery != "" {
			if progress != nil {
				progress(basePct, fmt.Sprintf("[%d/%d] Resolving %s...", idx+1, total, res.Title), 0, 0, 0)
			}
			if resolved, err := r.resolvePresetURL(ctx, res.KiwixQuery); err == nil && resolved != "" {
				dlURL = resolved
			}
		}
		if dlURL == "" {
			lastErr = fmt.Errorf("no download URL for %s", res.Title)
			continue
		}

		fileName := fileNameFromURL(dlURL)
		targetPath := filepath.Join(destDir, fileName)
		if fi, err := os.Stat(targetPath); err == nil && fi.Size() > 0 {
			completedAny = true
			if res.Kind == "zim" || strings.HasSuffix(strings.ToLower(fileName), ".zim") {
				r.registerDownloadedZIM(ctx, destDir, targetPath, res.Title, res.Description)
			}
			if progress != nil {
				progress(basePct+spanPct, fmt.Sprintf("[%d/%d] Already installed: %s", idx+1, total, res.Title), 0, 0, 0)
			}
			continue
		}

		itemJob := *job
		itemJob.Target = dlURL
		itemMeta := meta
		itemMeta.OutputDir = destDir
		itemMeta.FileName = fileName

		scaledProgress := func(pct float64, msg string, dlBytes, totBytes int64, eta int) {
			if progress == nil {
				return
			}
			overall := basePct + (pct/100.0)*spanPct
			label := fmt.Sprintf("[%d/%d] %s: %s", idx+1, total, res.Title, msg)
			progress(overall, label, dlBytes, totBytes, eta)
		}

		if err := r.downloadSingleURL(ctx, &itemJob, itemMeta, destDir, scaledProgress); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("%s: %w", res.Title, err)
			continue
		}
		completedAny = true
	}

	if !completedAny && lastErr != nil {
		return lastErr
	}
	if progress != nil {
		progress(100.0, fmt.Sprintf("Completed %d item(s)", total), 0, 0, 0)
	}
	return nil
}

func (r *Aria2cRunner) downloadSingleURL(ctx context.Context, job *Job, meta JobMetadata, outputDir string, progress ProgressFunc) error {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	var err error
	usedAria := false
	// If aria2c is available on host, use it
	if r.Aria2cBin != "" {
		if _, lookErr := exec.LookPath(r.Aria2cBin); lookErr == nil {
			if err = r.runAria2c(ctx, job, meta, outputDir, progress); err == nil {
				usedAria = true
			}
		}
	}
	if !usedAria {
		if err = r.runHTTPFallback(ctx, job, meta, outputDir, progress); err != nil {
			return err
		}
	}

	fileName := meta.FileName
	if fileName == "" {
		fileName = fileNameFromURL(job.Target)
	}
	if strings.HasSuffix(strings.ToLower(fileName), ".zim") {
		r.registerDownloadedZIM(ctx, outputDir, filepath.Join(outputDir, fileName), "", "")
	}
	return nil
}

func fileNameFromURL(rawURL string) string {
	parts := strings.Split(rawURL, "/")
	fileName := parts[len(parts)-1]
	if idx := strings.Index(fileName, "?"); idx != -1 {
		fileName = fileName[:idx]
	}
	if fileName == "" {
		fileName = "downloaded.zim"
	}
	return fileName
}

func (r *Aria2cRunner) registerDownloadedZIM(ctx context.Context, dir, zimPath, title, desc string) {
	lm := kiwix.NewLibraryManager(filepath.Join(dir, "library.xml"), "")
	var meta *kiwix.LibraryBook
	if title != "" {
		meta = &kiwix.LibraryBook{
			ID:          filepath.Base(zimPath),
			Path:        filepath.Base(zimPath),
			Title:       title,
			Description: desc,
		}
	}
	_ = lm.AddZIM(ctx, zimPath, meta)
}

func (r *Aria2cRunner) resolvePresetURL(ctx context.Context, preset string) (string, error) {
	queryMap := map[string]string{
		"wikipedia_en_top100": "wikipedia_en_top",
		"wikipedia_en_mini":   "wikipedia_en_all_mini",
		"wikipedia_en_nopic":  "wikipedia_en_all_nopic",
		"wikipedia_en_full":   "wikipedia_en_all_maxi",
		"wikimed":             "wikimed_en_all",
		"survival_repair":     "ifixit_en_all",
		"gutenberg":           "gutenberg_en_all",
		"starter_pack":        "wikimed_en_all",
		"ray_charles":         "wikipedia_en_ray_charles",
	}
	q := queryMap[preset]
	if q == "" {
		q = preset
	}

	opdsURL := "https://library.kiwix.org/catalog/v2/entries?count=1&q=" + url.QueryEscape(q)
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, opdsURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Ark-CommandCenter/1.0")

	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("kiwix catalog status %d", resp.StatusCode)
	}

	var feed struct {
		Entries []struct {
			Links []struct {
				Rel  string `xml:"rel,attr"`
				Href string `xml:"href,attr"`
				Type string `xml:"type,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return "", fmt.Errorf("decode kiwix opds: %w", err)
	}
	if len(feed.Entries) == 0 {
		return "", fmt.Errorf("no catalog entry found for %s", q)
	}

	for _, l := range feed.Entries[0].Links {
		if strings.Contains(l.Rel, "acquisition") && l.Href != "" {
			href := strings.TrimSuffix(l.Href, ".meta4")
			return href, nil
		}
	}
	return "", fmt.Errorf("no download link found in catalog for %s", q)
}

func (r *Aria2cRunner) runAria2c(ctx context.Context, job *Job, meta JobMetadata, outputDir string, progress ProgressFunc) error {
	args := []string{
		"-c", // continue partial downloads
		"--summary-interval=1",
		"--max-connection-per-server=8",
		"--split=8",
		"--min-split-size=1M",
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

	// Fast path: when starting fresh on servers supporting Accept-Ranges >= 2 MB, split across 6 concurrent HTTP streams.
	if existingBytes == 0 && meta.MaxRateKbps == 0 {
		if totalLen, supportsRange := r.probeRangeSupport(ctx, job.Target); supportsRange && totalLen >= 2*1024*1024 {
			if err := r.downloadSegmentedHTTP(ctx, job.Target, fileName, tempPath, targetPath, totalLen, 6, progress); err == nil {
				return nil
			}
		}
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

	if progress != nil {
		var initPct float64 = 1.0
		if totalBytes > 0 && existingBytes > 0 {
			initPct = (float64(existingBytes) / float64(totalBytes)) * 100.0
		}
		progress(initPct, fmt.Sprintf("Downloading %s...", fileName), existingBytes, totalBytes, 0)
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

	buf := make([]byte, 1024*1024) // 1 MB buffer for high throughput
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
				var pct, rateMBs float64
				var eta int
				elapsed := time.Since(startTime).Seconds()
				if elapsed > 0 {
					rate := float64(downloaded-existingBytes) / elapsed
					rateMBs = rate / (1024 * 1024)
					if totalBytes > 0 {
						pct = (float64(downloaded) / float64(totalBytes)) * 100.0
						if rate > 0 {
							eta = int(float64(totalBytes-downloaded) / rate)
						}
					}
				}
				progress(pct, fmt.Sprintf("%.1f / %.1f MB (%.1f MB/s)", float64(downloaded)/(1024*1024), float64(totalBytes)/(1024*1024), rateMBs), downloaded, totalBytes, eta)
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

func (r *Aria2cRunner) probeRangeSupport(ctx context.Context, rawURL string) (int64, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("User-Agent", "Ark-CommandCenter/1.0")
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return 0, false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 {
		return 0, false
	}
	acceptRanges := strings.ToLower(resp.Header.Get("Accept-Ranges"))
	return resp.ContentLength, strings.Contains(acceptRanges, "bytes")
}

func (r *Aria2cRunner) downloadSegmentedHTTP(ctx context.Context, rawURL, fileName, tempPath, targetPath string, totalBytes int64, workers int, progress ProgressFunc) error {
	if workers < 2 {
		workers = 4
	}
	outFile, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if err := outFile.Truncate(totalBytes); err != nil {
		outFile.Close()
		return err
	}

	segCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var downloaded atomic.Int64
	var errMu sync.Mutex
	var firstErr error
	chunkSize := totalBytes / int64(workers)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := int64(w) * chunkSize
		end := start + chunkSize - 1
		if w == workers-1 {
			end = totalBytes - 1
		}
		wg.Add(1)
		go func(s, e int64) {
			defer wg.Done()
			req, err := http.NewRequestWithContext(segCtx, http.MethodGet, rawURL, nil)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				cancel()
				return
			}
			req.Header.Set("User-Agent", "Ark-CommandCenter/1.0")
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", s, e))
			resp, err := r.HTTPClient.Do(req)
			if err != nil || resp.StatusCode != http.StatusPartialContent {
				if resp != nil {
					resp.Body.Close()
				}
				errMu.Lock()
				if firstErr == nil {
					if err != nil {
						firstErr = err
					} else {
						firstErr = fmt.Errorf("expected 206 Partial Content, got %d", resp.StatusCode)
					}
				}
				errMu.Unlock()
				cancel()
				return
			}
			defer resp.Body.Close()

			buf := make([]byte, 512*1024)
			offset := s
			for {
				n, rErr := resp.Body.Read(buf)
				if n > 0 {
					if _, wErr := outFile.WriteAt(buf[:n], offset); wErr != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = wErr
						}
						errMu.Unlock()
						cancel()
						return
					}
					offset += int64(n)
					downloaded.Add(int64(n))
				}
				if rErr != nil {
					if rErr == io.EOF {
						break
					}
					errMu.Lock()
					if firstErr == nil {
						firstErr = rErr
					}
					errMu.Unlock()
					cancel()
					return
				}
			}
		}(start, end)
	}

	// Progress reporter until workers finish
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		wg.Wait()
	}()

	startTime := time.Now()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-doneCh:
			break loop
		case <-ticker.C:
			if progress != nil {
				dl := downloaded.Load()
				pct := (float64(dl) / float64(totalBytes)) * 100.0
				elapsed := time.Since(startTime).Seconds()
				var rateMBs float64
				var eta int
				if elapsed > 0 {
					rate := float64(dl) / elapsed
					rateMBs = rate / (1024 * 1024)
					if rate > 0 {
						eta = int(float64(totalBytes-dl) / rate)
					}
				}
				progress(pct, fmt.Sprintf("%s (6x parallel): %.1f / %.1f MB (%.1f MB/s)", fileName, float64(dl)/(1024*1024), float64(totalBytes)/(1024*1024), rateMBs), dl, totalBytes, eta)
			}
		}
	}

	outFile.Close()
	if firstErr != nil {
		_ = os.Remove(tempPath)
		return firstErr
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		return err
	}
	if progress != nil {
		progress(100.0, "Complete", totalBytes, totalBytes, 0)
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
