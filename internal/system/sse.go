package system

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SSEFeed serves live system metrics over Server-Sent Events.
type SSEFeed struct {
	DiskPath string
	Interval time.Duration
}

// NewSSEFeed creates a new SSE metrics feed.
func NewSSEFeed(diskPath string) *SSEFeed {
	return &SSEFeed{
		DiskPath: diskPath,
		Interval: 2 * time.Second,
	}
}

// ServeHTTP implements http.Handler for SSE streaming.
func (s *SSEFeed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ctx := r.Context()
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	// Send an immediate first frame
	if metrics, err := CollectMetrics(ctx, s.DiskPath); err == nil {
		if data, err := json.Marshal(metrics); err == nil {
			fmt.Fprintf(w, "event: metrics\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			metrics, err := CollectMetrics(ctx, s.DiskPath)
			if err != nil {
				continue
			}

			data, err := json.Marshal(metrics)
			if err != nil {
				continue
			}

			if _, err := fmt.Fprintf(w, "event: metrics\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
