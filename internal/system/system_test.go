package system

import (
	"bufio"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCollectMetrics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	metrics, err := CollectMetrics(ctx, "")
	if err != nil {
		t.Fatalf("CollectMetrics failed: %v", err)
	}

	if metrics.RAMTotal == 0 {
		t.Errorf("Expected RAMTotal > 0, got %d", metrics.RAMTotal)
	}

	if metrics.DiskTotal == 0 {
		t.Errorf("Expected DiskTotal > 0, got %d", metrics.DiskTotal)
	}

	if metrics.OS == "" {
		t.Errorf("Expected non-empty OS string")
	}
}

func TestSSEFeed(t *testing.T) {
	feed := &SSEFeed{
		DiskPath: "",
		Interval: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/api/system/live", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	feed.ServeHTTP(rec, req)

	res := rec.Result()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Expected Content-Type text/event-stream, got %s", ct)
	}

	body := rec.Body.String()
	scanner := bufio.NewScanner(strings.NewReader(body))
	foundEvent := false
	foundData := false

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: metrics") {
			foundEvent = true
		}
		if strings.HasPrefix(line, "data: {") {
			foundData = true
		}
	}

	if !foundEvent || !foundData {
		t.Fatalf("Did not find expected SSE stream format. Body:\n%s", body)
	}
}
