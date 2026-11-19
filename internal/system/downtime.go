package system

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	settingLastShutdownAt    = "last_shutdown_at"
	settingLastStartedAt     = "last_started_at"
	settingLastDowntimeSec   = "last_downtime_sec"
	settingLastDowntimeHuman = "last_downtime_human"
)

// DowntimeInfo summarizes how long Ark was offline before the current run.
type DowntimeInfo struct {
	HasPrevious    bool
	LastShutdownAt string
	LastStartedAt  string
	Downtime       time.Duration
	DowntimeHuman  string
}

// RecordShutdownTime saves the current system UTC timestamp to SQLite settings before shutdown.
func RecordShutdownTime(ctx context.Context, db *sql.DB, now time.Time) error {
	if db == nil {
		return nil
	}
	ts := normalizeUTC(now).Format(time.RFC3339)
	return upsertSetting(ctx, db, settingLastShutdownAt, ts)
}

// RecordStartupAndComputeDowntime compares current system time against the saved shutdown time
// and stores the elapsed downtime in settings for Telemetry, AI, and retention checks.
func RecordStartupAndComputeDowntime(ctx context.Context, db *sql.DB, now time.Time) DowntimeInfo {
	now = normalizeUTC(now)
	startedStr := now.Format(time.RFC3339)
	info := DowntimeInfo{LastStartedAt: startedStr}
	if db == nil {
		return info
	}

	var prevStr string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, settingLastShutdownAt).Scan(&prevStr)
	if err == nil && strings.TrimSpace(prevStr) != "" {
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
	pairs := [][2]string{
		{settingLastStartedAt, startedStr},
		{settingLastDowntimeSec, secStr},
		{settingLastDowntimeHuman, info.DowntimeHuman},
	}
	for _, p := range pairs {
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

// GetDowntimeInfo reads the computed startup downtime metadata from SQLite settings.
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
