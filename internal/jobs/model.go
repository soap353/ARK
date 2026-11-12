package jobs

import (
	"time"
)

// Standard Job Types
const (
	TypeDownload = "download"
	TypeEncode   = "encode"
	TypeIndex    = "index"
	TypeZim      = "zim"
	TypePreset   = "preset"
)

// Standard Job Statuses
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusPaused  = "paused"
)

// Job represents a work unit in the SQLite queue.
type Job struct {
	ID              string     `json:"id"`
	Type            string     `json:"type"`
	Target          string     `json:"target"`
	Status          string     `json:"status"`
	Progress        float64    `json:"progress"` // 0.0 to 100.0
	ProgressMsg     string     `json:"progress_msg"`
	ErrorMsg        string     `json:"error_msg"`
	BytesDownloaded int64      `json:"bytes_downloaded"`
	TotalBytes      int64      `json:"total_bytes"`
	ETASeconds      int        `json:"eta_seconds"`
	Metadata        string     `json:"metadata"` // JSON payload
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}
