package system

import (
	"context"
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
