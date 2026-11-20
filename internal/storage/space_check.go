package storage

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
)

const (
	// SpaceSafetyBufferBytes reserves 1 GiB so downloads never completely fill the OS/DB disk.
	SpaceSafetyBufferBytes int64 = 1 << 30
	// SpacePortionThreshold warns when a download would consume >= 25% of remaining free space.
	SpacePortionThreshold = 0.25
	// LowDiskUnknownSizeBytes warns on unknown-size downloads when free space is under 2 GiB.
	LowDiskUnknownSizeBytes int64 = 2 << 30
)

// SpaceCheckResult summarizes available disk space vs. estimated download size.
type SpaceCheckResult struct {
	FreeBytes      uint64  `json:"free_bytes"`
	TotalBytes     uint64  `json:"total_bytes"`
	EstimatedBytes int64   `json:"estimated_bytes"`
	KnownSize      bool    `json:"known_size"`
	PercentOfFree  float64 `json:"percent_of_free"`
	Status         string  `json:"status"` // "ok", "warn", "insufficient"
	Message        string  `json:"message"`
}

// EvaluateSpace computes whether a download of estimatedBytes fits safely in freeBytes.
func EvaluateSpace(freeBytes, totalBytes uint64, estimatedBytes int64) SpaceCheckResult {
	res := SpaceCheckResult{
		FreeBytes:      freeBytes,
		TotalBytes:     totalBytes,
		EstimatedBytes: estimatedBytes,
		KnownSize:      estimatedBytes > 0,
		Status:         "ok",
	}

	if estimatedBytes <= 0 {
		if freeBytes < uint64(LowDiskUnknownSizeBytes) {
			res.Status = "warn"
			res.Message = fmt.Sprintf("Low disk space: only %s free (download size unknown).", FormatBytes(int64(freeBytes)))
			return res
		}
		res.Message = fmt.Sprintf("%s free on disk.", FormatBytes(int64(freeBytes)))
		return res
	}

	if freeBytes > 0 {
		res.PercentOfFree = (float64(estimatedBytes) / float64(freeBytes)) * 100.0
	} else {
		res.PercentOfFree = 100.0
	}

	if uint64(estimatedBytes) >= freeBytes || freeBytes < uint64(estimatedBytes)+uint64(SpaceSafetyBufferBytes) {
		res.Status = "insufficient"
		res.Message = fmt.Sprintf(
			"Not enough disk space: download needs ~%s (plus 1.0 GB safety buffer), only %s free.",
			FormatBytes(estimatedBytes),
			FormatBytes(int64(freeBytes)),
		)
		return res
	}

	if float64(estimatedBytes) >= float64(freeBytes)*SpacePortionThreshold {
		res.Status = "warn"
		res.Message = fmt.Sprintf(
			"Large download warning: ~%s will use %.0f%% of your %s remaining free space.",
			FormatBytes(estimatedBytes),
			res.PercentOfFree,
			FormatBytes(int64(freeBytes)),
		)
		return res
	}

	res.Message = fmt.Sprintf("%s free (~%s needed).", FormatBytes(int64(freeBytes)), FormatBytes(estimatedBytes))
	return res
}

// CheckDiskSpaceForPath queries filesystem free space for targetPath and evaluates estimatedBytes.
func CheckDiskSpaceForPath(ctx context.Context, targetPath string, estimatedBytes int64) (SpaceCheckResult, error) {
	checkPath := strings.TrimSpace(targetPath)
	if checkPath == "" {
		checkPath = "."
	}
	for {
		if _, err := os.Stat(checkPath); err == nil {
			break
		}
		parent := filepath.Dir(checkPath)
		if parent == checkPath {
			checkPath = "."
			break
		}
		checkPath = parent
	}
	usage, err := disk.UsageWithContext(ctx, checkPath)
	if err != nil {
		return SpaceCheckResult{}, err
	}
	return EvaluateSpace(usage.Free, usage.Total, estimatedBytes), nil
}

// ParseSizeText parses human size strings like "~3.5 GB", "620 MB", or "27 GB" into bytes.
func ParseSizeText(s string) int64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "~")
	s = strings.ReplaceAll(s, ",", "")
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	numStr := fields[0]
	unit := ""
	if len(fields) > 1 {
		unit = fields[1]
	} else {
		for i, r := range numStr {
			if (r < '0' || r > '9') && r != '.' {
				unit = numStr[i:]
				numStr = numStr[:i]
				break
			}
		}
	}
	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil || val <= 0 {
		return 0
	}
	switch {
	case strings.HasPrefix(unit, "TB") || unit == "T":
		return int64(val * 1024 * 1024 * 1024 * 1024)
	case strings.HasPrefix(unit, "GB") || unit == "G":
		return int64(val * 1024 * 1024 * 1024)
	case strings.HasPrefix(unit, "MB") || unit == "M":
		return int64(val * 1024 * 1024)
	case strings.HasPrefix(unit, "KB") || unit == "K":
		return int64(val * 1024)
	default:
		return int64(val)
	}
}

// ProbeRemoteSize issues a quick HTTP HEAD request to inspect Content-Length when available.
func ProbeRemoteSize(ctx context.Context, rawURL string) int64 {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return 0
	}
	probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0
	}
	resp, err := (&http.Client{Timeout: 1500 * time.Millisecond}).Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	if resp.ContentLength > 0 {
		return resp.ContentLength
	}
	return 0
}

// FormatBytes formats a byte count as human-readable MB or GB.
func FormatBytes(b int64) string {
	if b <= 0 {
		return "0 MB"
	}
	const gb = 1024 * 1024 * 1024
	const mb = 1024 * 1024
	if b >= gb {
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	}
	return fmt.Sprintf("%.0f MB", float64(b)/float64(mb))
}
