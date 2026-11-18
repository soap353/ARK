package system

import (
	"bufio"
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
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

func TestShutdownAndStartupDowntimeTracking(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "dt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("create settings: %v", err)
	}

	ctx := t.Context()
	t0 := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	if err := RecordShutdownTime(ctx, db, t0); err != nil {
		t.Fatalf("RecordShutdownTime: %v", err)
	}

	t1 := t0.Add(2*time.Hour + 15*time.Minute)
	dt := RecordStartupAndComputeDowntime(ctx, db, t1)
	if !dt.HasPrevious || dt.DowntimeHuman != "2h 15m" {
		t.Fatalf("expected 2h 15m downtime, got %+v", dt)
	}

	loaded := GetDowntimeInfo(ctx, db)
	if !loaded.HasPrevious || loaded.DowntimeHuman != "2h 15m" {
		t.Fatalf("expected persisted downtime 2h 15m, got %+v", loaded)
	}

	// Clock rollback safety: earlier timestamp should clamp to 0s without negative duration
	dtBack := RecordStartupAndComputeDowntime(ctx, db, t0.Add(-5*time.Minute))
	if !dtBack.HasPrevious || dtBack.Downtime != 0 {
		t.Errorf("expected clock rollback clamped to 0, got %+v", dtBack)
	}
}
