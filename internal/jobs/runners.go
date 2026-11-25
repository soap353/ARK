package jobs

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
	Format      string `json:"format,omitempty"`        // for yt-dlp: "balanced", "av1", "mp4", "music"
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
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		MaxConnsPerHost:       32,
		ReadBufferSize:        512 * 1024,
		WriteBufferSize:       512 * 1024,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
		// Disable HTTP/2 single-connection multiplexing so concurrent Range workers
		// open independent parallel TCP connections across hosts/mirrors.
		ForceAttemptHTTP2: false,
		TLSNextProto:      make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
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
		if dlURL != "" {
			if freshURL := r.resolveLatestKiwixMirrorURL(ctx, dlURL); freshURL != "" {
				dlURL = freshURL
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
		q = strings.ReplaceAll(preset, "_", " ")
	}

	opdsURL := "https://library.kiwix.org/catalog/v2/entries?count=5&q=" + url.QueryEscape(q)
	if strings.Contains(strings.ToLower(preset), "_en_") || strings.HasSuffix(strings.ToLower(preset), "_en") {
		opdsURL += "&lang=eng"
	}
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

	presetLow := strings.ToLower(strings.TrimSpace(preset))
	firstHref := ""
	for _, entry := range feed.Entries {
		for _, l := range entry.Links {
			if strings.Contains(l.Rel, "acquisition") && l.Href != "" {
				href := strings.TrimSuffix(l.Href, ".meta4")
				if firstHref == "" {
					firstHref = href
				}
				if presetLow != "" && strings.Contains(strings.ToLower(href), presetLow) {
					return href, nil
				}
			}
		}
	}
	if firstHref != "" {
		return firstHref, nil
	}
	return "", fmt.Errorf("no download link found in catalog for %s", q)
}

var kiwixDatedFileRe = regexp.MustCompile(`^(.+?)_(\d{4}-\d{2}[a-z]?)\.zim$`)

// resolveLatestKiwixMirrorURL resolves dated download.kiwix.org/zim/<cat>/<stem>_YYYY-MM.zim URLs
// against the live directory index so monthly date rotations on Kiwix mirrors never 404.
func (r *Aria2cRunner) resolveLatestKiwixMirrorURL(ctx context.Context, rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || !strings.Contains(strings.ToLower(u.Host), "download.kiwix.org") {
		return rawURL
	}
	base := pathBase(u.Path)
	m := kiwixDatedFileRe.FindStringSubmatch(base)
	if len(m) != 3 {
		return rawURL
	}
	stemPrefix := m[1]
	dirPath := strings.TrimSuffix(u.Path, base)
	dirURL := u.Scheme + "://" + u.Host + dirPath

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, dirURL, nil)
	if err != nil {
		return rawURL
	}
	req.Header.Set("User-Agent", "Ark-CommandCenter/1.0")
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return rawURL
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return rawURL
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return rawURL
	}

	linkRe := regexp.MustCompile(`href="(` + regexp.QuoteMeta(stemPrefix) + `_\d{4}-\d{2}[a-z]?\.zim)"`)
	matches := linkRe.FindAllStringSubmatch(string(bodyBytes), -1)
	if len(matches) == 0 {
		return rawURL
	}
	names := make([]string, 0, len(matches))
	for _, mm := range matches {
		if len(mm) == 2 {
			names = append(names, mm[1])
		}
	}
	sort.Strings(names)
	latest := names[len(names)-1]
	return dirURL + latest
}

func pathBase(p string) string {
	p = strings.TrimRight(p, "/")
	if idx := strings.LastIndex(p, "/"); idx != -1 {
		return p[idx+1:]
	}
	return p
}

func (r *Aria2cRunner) runAria2c(ctx context.Context, job *Job, meta JobMetadata, outputDir string, progress ProgressFunc) error {
	args := []string{
		"-c", // continue partial downloads
		"--summary-interval=1",
		"--max-connection-per-server=16",
		"--split=16",
		"--min-split-size=1M",
		"--file-allocation=none",
		"--disk-cache=64M",
		"-d", outputDir,
	}
	if meta.FileName != "" {
		args = append(args, "-o", filepath.Base(meta.FileName))
	}
	if meta.MaxRateKbps > 0 {
		args = append(args, fmt.Sprintf("--max-download-limit=%dK", meta.MaxRateKbps))
	}
	if strings.HasPrefix(job.ID, "peer-") && strings.HasPrefix(job.Target, "https://") {
		args = append(args, "--check-certificate=false")
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
	fileName := filepath.Base(meta.FileName)
	if fileName == "" || fileName == "." || fileName == ".." {
		parts := strings.Split(job.Target, "/")
		fileName = parts[len(parts)-1]
		if idx := strings.Index(fileName, "?"); idx != -1 {
			fileName = fileName[:idx]
		}
		fileName = filepath.Base(fileName)
		if fileName == "" || fileName == "." || fileName == ".." {
			fileName = "downloaded.zim"
		}
	}

	targetPath := filepath.Join(outputDir, fileName)
	tempPath := targetPath + ".part"

	var existingBytes int64
	if fi, err := os.Stat(tempPath); err == nil {
		existingBytes = fi.Size()
	}

	// Fast path: when starting fresh on servers supporting Byte-Range requests >= 1 MB,
	// resolve Metalink (.meta4) mirrors + direct post-redirect URLs and split across 12 parallel TCP workers.
	if existingBytes == 0 && meta.MaxRateKbps == 0 {
		if totalLen, mirrors, supportsRange := r.resolveMirrorsAndRange(ctx, job.Target); supportsRange && totalLen >= 1024*1024 {
			if err := r.downloadSegmentedHTTP(ctx, mirrors, fileName, tempPath, targetPath, totalLen, 12, progress); err == nil {
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

	httpClient := r.HTTPClient
	if strings.HasPrefix(job.ID, "peer-") && strings.HasPrefix(job.Target, "https://") {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		httpClient = &http.Client{Transport: tr}
	}
	resp, err := httpClient.Do(req)
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

func (r *Aria2cRunner) probeSingleMirrorRange(ctx context.Context, rawURL string, timeout time.Duration) (int64, string, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 1. Try HEAD first
	req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, rawURL, nil)
	if err == nil {
		req.Header.Set("User-Agent", "Ark-CommandCenter/1.0")
		if resp, err := r.HTTPClient.Do(req); err == nil {
			finalURL := rawURL
			if resp.Request != nil && resp.Request.URL != nil {
				finalURL = resp.Request.URL.String()
			}
			cl := resp.ContentLength
			acceptRanges := strings.ToLower(resp.Header.Get("Accept-Ranges"))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && cl > 0 && strings.Contains(acceptRanges, "bytes") {
				return cl, finalURL, true
			}
		}
	}

	// 2. Fallback: Try 1-byte Range GET (many mirrors/CDNs omit Accept-Ranges on HEAD or block HEAD)
	reqGet, err := http.NewRequestWithContext(probeCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, rawURL, false
	}
	reqGet.Header.Set("User-Agent", "Ark-CommandCenter/1.0")
	reqGet.Header.Set("Range", "bytes=0-0")
	resp, err := r.HTTPClient.Do(reqGet)
	if err != nil {
		return 0, rawURL, false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))

	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	if resp.StatusCode == http.StatusPartialContent {
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if slash := strings.LastIndex(cr, "/"); slash != -1 {
				if total, err := strconv.ParseInt(strings.TrimSpace(cr[slash+1:]), 10, 64); err == nil && total > 0 {
					return total, finalURL, true
				}
			}
		}
	}
	return 0, finalURL, false
}

func (r *Aria2cRunner) fetchMeta4Mirrors(ctx context.Context, rawURL string) ([]string, int64) {
	lower := strings.ToLower(rawURL)
	if !strings.HasSuffix(lower, ".zim") && !strings.Contains(lower, "kiwix.org") {
		return nil, 0
	}
	metaURL := rawURL
	if !strings.HasSuffix(lower, ".meta4") {
		metaURL = rawURL + ".meta4"
	}

	mCtx, cancel := context.WithTimeout(ctx, 3500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(mCtx, http.MethodGet, metaURL, nil)
	if err != nil {
		return nil, 0
	}
	req.Header.Set("User-Agent", "Ark-CommandCenter/1.0")
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return nil, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0
	}

	var ml struct {
		Files []struct {
			Size int64 `xml:"size"`
			URLs []struct {
				Priority int    `xml:"priority,attr"`
				URL      string `xml:",chardata"`
			} `xml:"url"`
		} `xml:"file"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 512*1024)).Decode(&ml); err != nil || len(ml.Files) == 0 {
		return nil, 0
	}
	f := ml.Files[0]
	sort.SliceStable(f.URLs, func(i, j int) bool {
		pi, pj := f.URLs[i].Priority, f.URLs[j].Priority
		if pi <= 0 {
			pi = 999
		}
		if pj <= 0 {
			pj = 999
		}
		return pi < pj
	})
	var urls []string
	seen := make(map[string]bool)
	for _, u := range f.URLs {
		clean := strings.TrimSpace(u.URL)
		if (strings.HasPrefix(clean, "https://") || strings.HasPrefix(clean, "http://")) && !seen[clean] {
			seen[clean] = true
			urls = append(urls, clean)
			if len(urls) >= 6 {
				break
			}
		}
	}
	return urls, f.Size
}

func (r *Aria2cRunner) resolveMirrorsAndRange(ctx context.Context, rawURL string) (int64, []string, bool) {
	metaMirrors, _ := r.fetchMeta4Mirrors(ctx, rawURL)
	candidates := make([]string, 0, len(metaMirrors)+1)
	seen := make(map[string]bool)
	for _, m := range metaMirrors {
		if !seen[m] {
			seen[m] = true
			candidates = append(candidates, m)
		}
	}
	if !seen[rawURL] {
		candidates = append(candidates, rawURL)
	}

	if len(candidates) == 1 {
		total, finalURL, ok := r.probeSingleMirrorRange(ctx, rawURL, 8*time.Second)
		if !ok {
			return 0, nil, false
		}
		return total, []string{finalURL}, true
	}

	type mirrorResult struct {
		url     string
		size    int64
		latency time.Duration
	}
	var mu sync.Mutex
	var results []mirrorResult
	var wg sync.WaitGroup

	for _, cand := range candidates {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			t0 := time.Now()
			sz, finalURL, ok := r.probeSingleMirrorRange(ctx, u, 3*time.Second)
			if !ok || sz <= 0 {
				return
			}
			mu.Lock()
			results = append(results, mirrorResult{
				url:     finalURL,
				size:    sz,
				latency: time.Since(t0),
			})
			mu.Unlock()
		}(cand)
	}
	wg.Wait()

	if len(results) == 0 {
		total, finalURL, ok := r.probeSingleMirrorRange(ctx, rawURL, 8*time.Second)
		if !ok {
			return 0, nil, false
		}
		return total, []string{finalURL}, true
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].latency < results[j].latency
	})
	primarySize := results[0].size
	var fastest []string
	seenFinal := make(map[string]bool)
	for _, res := range results {
		if res.size == primarySize && !seenFinal[res.url] {
			seenFinal[res.url] = true
			fastest = append(fastest, res.url)
			if len(fastest) >= 4 {
				break
			}
		}
	}
	return primarySize, fastest, len(fastest) > 0
}

type byteChunk struct {
	index int
	start int64
	end   int64
}

func (r *Aria2cRunner) downloadSegmentedHTTP(ctx context.Context, mirrors []string, fileName, tempPath, targetPath string, totalBytes int64, workers int, progress ProgressFunc) error {
	if len(mirrors) == 0 {
		return fmt.Errorf("no download mirrors available")
	}
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

	// Build dynamic work-stealing chunks (8 MB target chunk size, or totalBytes/workers for smaller files)
	const targetChunkBytes = int64(8 * 1024 * 1024)
	const minChunkBytes = int64(256 * 1024)
	chunkSize := targetChunkBytes
	if totalBytes/int64(workers) < chunkSize {
		chunkSize = totalBytes / int64(workers)
		if chunkSize < minChunkBytes {
			chunkSize = minChunkBytes
		}
	}

	var chunks []byteChunk
	var idx int
	for pos := int64(0); pos < totalBytes; pos += chunkSize {
		end := pos + chunkSize - 1
		if end >= totalBytes {
			end = totalBytes - 1
		}
		chunks = append(chunks, byteChunk{index: idx, start: pos, end: end})
		idx++
	}
	if workers > len(chunks) {
		workers = len(chunks)
	}

	segCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var nextChunk atomic.Int64
	var downloaded atomic.Int64
	var errMu sync.Mutex
	var firstErr error

	downloadOneChunk := func(ch byteChunk) error {
		expectedLen := ch.end - ch.start + 1
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if segCtx.Err() != nil {
				return segCtx.Err()
			}
			mirrorURL := mirrors[(ch.index+attempt)%len(mirrors)]
			req, err := http.NewRequestWithContext(segCtx, http.MethodGet, mirrorURL, nil)
			if err != nil {
				lastErr = err
				continue
			}
			req.Header.Set("User-Agent", "Ark-CommandCenter/1.0")
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", ch.start, ch.end))
			resp, err := r.HTTPClient.Do(req)
			if err != nil {
				lastErr = err
				continue
			}
			if resp.StatusCode != http.StatusPartialContent {
				resp.Body.Close()
				lastErr = fmt.Errorf("expected 206 Partial Content, got %d", resp.StatusCode)
				continue
			}

			readBuf := make([]byte, 1024*1024)       // 1 MB read buffer
			writeBuf := make([]byte, 0, 2*1024*1024) // 2 MB batched disk write buffer
			writeStart := ch.start
			var chunkRead int64
			var rwErr error

			for {
				n, rErr := resp.Body.Read(readBuf)
				if n > 0 {
					writeBuf = append(writeBuf, readBuf[:n]...)
					chunkRead += int64(n)
					downloaded.Add(int64(n))

					if len(writeBuf) >= 2*1024*1024 {
						if _, wErr := outFile.WriteAt(writeBuf, writeStart); wErr != nil {
							rwErr = wErr
							break
						}
						writeStart += int64(len(writeBuf))
						writeBuf = writeBuf[:0]
					}
				}
				if rErr != nil {
					if rErr == io.EOF {
						break
					}
					rwErr = rErr
					break
				}
			}
			resp.Body.Close()

			if rwErr == nil && len(writeBuf) > 0 {
				if _, wErr := outFile.WriteAt(writeBuf, writeStart); wErr != nil {
					rwErr = wErr
				}
			}
			if rwErr == nil && chunkRead == expectedLen {
				return nil
			}
			// Roll back partial progress before retrying chunk
			if chunkRead > 0 {
				downloaded.Add(-chunkRead)
			}
			if rwErr != nil {
				lastErr = rwErr
			} else {
				lastErr = fmt.Errorf("short chunk read: got %d of %d bytes", chunkRead, expectedLen)
			}
		}
		return lastErr
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(nextChunk.Add(1) - 1)
				if i >= len(chunks) {
					return
				}
				if err := downloadOneChunk(chunks[i]); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					cancel()
					return
				}
			}
		}()
	}

	// Progress reporter until workers finish
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		wg.Wait()
	}()

	startTime := time.Now()
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-doneCh:
			break loop
		case <-ticker.C:
			if progress != nil {
				dl := downloaded.Load()
				if dl > totalBytes {
					dl = totalBytes
				}
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
				progress(pct, fmt.Sprintf("%s: %.1f / %.1f MB (%.1f MB/s)", fileName, float64(dl)/(1024*1024), float64(totalBytes)/(1024*1024), rateMBs), dl, totalBytes, eta)
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
