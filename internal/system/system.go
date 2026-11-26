// Package system collects host telemetry (CPU, RAM, disk, uptime),
// streams live metrics via Server-Sent Events (SSE), and tracks Ark's
// offline downtime between restarts using SQLite settings.
package system

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

const (
	settingLastShutdownAt    = "last_shutdown_at"
	settingLastStartedAt     = "last_started_at"
	settingLastDowntimeSec   = "last_downtime_sec"
	settingLastDowntimeHuman = "last_downtime_human"
)

var sharedHostMetricsClient = &http.Client{Timeout: 1200 * time.Millisecond}

// Metrics represents a snapshot of host CPU, memory, disk, and uptime statistics.
type Metrics struct {
	CPUPercent  float64 `json:"cpu_percent"`
	RAMTotal    uint64  `json:"ram_total"`
	RAMUsed     uint64  `json:"ram_used"`
	RAMFree     uint64  `json:"ram_free"`
	RAMPercent  float64 `json:"ram_percent"`
	DiskTotal   uint64  `json:"disk_total"`
	DiskUsed    uint64  `json:"disk_used"`
	DiskFree    uint64  `json:"disk_free"`
	DiskPercent float64 `json:"disk_percent"`
	Uptime      uint64  `json:"uptime"`
	Hostname    string  `json:"hostname"`
	OS          string  `json:"os"`
	Timestamp   int64   `json:"timestamp"`
}

// CollectMetrics gathers current CPU, memory, disk, and host metrics.
// If diskPath is empty, the host root drive is queried.
func CollectMetrics(ctx context.Context, diskPath string) (*Metrics, error) {
	// When running inside Docker, ARK_HOST_METRICS_URL delegates to the host sidecar.
	if metricsURL := strings.TrimSpace(os.Getenv("ARK_HOST_METRICS_URL")); metricsURL != "" {
		reqCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, metricsURL, nil)
		if err == nil {
			resp, rerr := sharedHostMetricsClient.Do(req)
			if rerr == nil {
				var hostM Metrics
				derr := json.NewDecoder(resp.Body).Decode(&hostM)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK && derr == nil && hostM.Hostname != "" {
					cancel()
					return &hostM, nil
				}
			}
		}
		cancel()
	}

	m := &Metrics{
		OS:        runtime.GOOS,
		Timestamp: time.Now().Unix(),
	}

	if hInfo, err := host.InfoWithContext(ctx); err == nil && hInfo != nil {
		m.Hostname = hInfo.Hostname
		m.Uptime = hInfo.Uptime
	} else if hostName, err := os.Hostname(); err == nil {
		m.Hostname = hostName
	}

	if cpuPercents, err := cpu.PercentWithContext(ctx, 100*time.Millisecond, false); err == nil && len(cpuPercents) > 0 {
		m.CPUPercent = cpuPercents[0]
	}

	if vMem, err := mem.VirtualMemoryWithContext(ctx); err == nil && vMem != nil {
		m.RAMTotal = vMem.Total
		m.RAMUsed = vMem.Used
		m.RAMFree = vMem.Available
		m.RAMPercent = vMem.UsedPercent
	}

	targetPath := diskPath
	if targetPath == "" {
		if runtime.GOOS == "windows" {
			targetPath = "C:\\"
		} else {
			targetPath = "/"
		}
	}

	if dUsage, err := disk.UsageWithContext(ctx, targetPath); err == nil && dUsage != nil {
		m.DiskTotal = dUsage.Total
		m.DiskUsed = dUsage.Used
		m.DiskFree = dUsage.Free
		m.DiskPercent = dUsage.UsedPercent
	}

	return m, nil
}

// SSEFeed serves live system metrics over Server-Sent Events (SSE).
type SSEFeed struct {
	DiskPath string
	Interval time.Duration
}

// NewSSEFeed creates a new SSE metrics feed polling every 2 seconds.
func NewSSEFeed(diskPath string) *SSEFeed {
	return &SSEFeed{
		DiskPath: diskPath,
		Interval: 2 * time.Second,
	}
}

// ServeHTTP streams periodic "metrics" events until the client disconnects.
func (s *SSEFeed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}
	// Clear the HTTP server's write deadline so long-lived SSE streams stay open.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	_ = sendMetricsFrame(ctx, w, flusher, s.DiskPath)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sendMetricsFrame(ctx, w, flusher, s.DiskPath); err != nil {
				return
			}
		}
	}
}

func sendMetricsFrame(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, diskPath string) error {
	metrics, err := CollectMetrics(ctx, diskPath)
	if err != nil {
		return nil
	}
	data, err := json.Marshal(metrics)
	if err != nil {
		return nil
	}
	if _, err := fmt.Fprintf(w, "event: metrics\ndata: %s\n\n", data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// DowntimeInfo summarizes how long Ark was offline before the current run.
type DowntimeInfo struct {
	HasPrevious    bool
	LastShutdownAt string
	LastStartedAt  string
	Downtime       time.Duration
	DowntimeHuman  string
}

// RecordShutdownTime saves the current UTC timestamp to SQLite settings before shutdown.
func RecordShutdownTime(ctx context.Context, db *sql.DB, now time.Time) error {
	if db == nil {
		return nil
	}
	return upsertSetting(ctx, db, settingLastShutdownAt, normalizeUTC(now).Format(time.RFC3339))
}

// RecordStartupAndComputeDowntime compares current time against the last recorded shutdown
// and stores elapsed downtime in settings for Telemetry and retention checks.
func RecordStartupAndComputeDowntime(ctx context.Context, db *sql.DB, now time.Time) DowntimeInfo {
	now = normalizeUTC(now)
	startedStr := now.Format(time.RFC3339)
	info := DowntimeInfo{LastStartedAt: startedStr}
	if db == nil {
		return info
	}

	var prevStr string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, settingLastShutdownAt).Scan(&prevStr); err == nil && strings.TrimSpace(prevStr) != "" {
		if prevTime, perr := time.Parse(time.RFC3339, strings.TrimSpace(prevStr)); perr == nil {
			elapsed := now.Sub(prevTime)
			if elapsed < 0 {
				elapsed = 0
			}
			info.HasPrevious = true
			info.LastShutdownAt = prevTime.Local().Format("2006-01-02 15:04:05")
			info.Downtime = elapsed
			info.DowntimeHuman = formatElapsedDuration(elapsed)
		}
	}

	secStr := strconv.FormatInt(int64(info.Downtime.Seconds()), 10)
	for _, p := range [][2]string{
		{settingLastStartedAt, startedStr},
		{settingLastDowntimeSec, secStr},
		{settingLastDowntimeHuman, info.DowntimeHuman},
	} {
		_ = upsertSetting(ctx, db, p[0], p[1])
	}
	return info
}

func normalizeUTC(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func upsertSetting(ctx context.Context, db *sql.DB, key, value string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	return err
}

// GetDowntimeInfo reads saved startup downtime metadata from SQLite settings.
func GetDowntimeInfo(ctx context.Context, db *sql.DB) DowntimeInfo {
	var info DowntimeInfo
	if db == nil {
		return info
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN (?, ?, ?, ?)`,
		settingLastShutdownAt, settingLastStartedAt, settingLastDowntimeSec, settingLastDowntimeHuman)
	if err != nil {
		return info
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			continue
		}
		switch k {
		case settingLastShutdownAt:
			if t, perr := time.Parse(time.RFC3339, strings.TrimSpace(v)); perr == nil {
				info.HasPrevious = true
				info.LastShutdownAt = t.Local().Format("2006-01-02 15:04:05")
			}
		case settingLastStartedAt:
			if t, perr := time.Parse(time.RFC3339, strings.TrimSpace(v)); perr == nil {
				info.LastStartedAt = t.Local().Format("2006-01-02 15:04:05")
			}
		case settingLastDowntimeSec:
			if sec, perr := strconv.ParseInt(v, 10, 64); perr == nil && sec >= 0 {
				info.Downtime = time.Duration(sec) * time.Second
			}
		case settingLastDowntimeHuman:
			info.DowntimeHuman = v
		}
	}
	return info
}

func formatElapsedDuration(d time.Duration) string {
	sec := int64(d.Seconds())
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	days := sec / 86400
	hrs := (sec % 86400) / 3600
	mins := (sec % 3600) / 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hrs, mins)
	}
	if hrs > 0 {
		return fmt.Sprintf("%dh %dm", hrs, mins)
	}
	return fmt.Sprintf("%dm %ds", mins, sec%60)
}
