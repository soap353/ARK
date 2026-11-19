package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestJobStore(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Open in-memory db: %v", err)
	}
	defer db.Close()

	store := NewJobStore(db)
	if err := store.EnsureTable(ctx); err != nil {
		t.Fatalf("EnsureTable failed: %v", err)
	}

	job := &Job{
		ID:       "job-1",
		Type:     TypeDownload,
		Target:   "https://example.com/file.zim",
		Status:   StatusPending,
		Metadata: `{"file_name":"file.zim"}`,
	}

	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("Create job failed: %v", err)
	}

	retrieved, err := store.Get(ctx, "job-1")
	if err != nil {
		t.Fatalf("Get job failed: %v", err)
	}
	if retrieved.ID != "job-1" || retrieved.Status != StatusPending {
		t.Errorf("Unexpected job state: %+v", retrieved)
	}

	// Update progress
	if err := store.UpdateProgress(ctx, "job-1", 50.0, "halfway", 500, 1000, 10); err != nil {
		t.Fatalf("UpdateProgress failed: %v", err)
	}
	retrieved, _ = store.Get(ctx, "job-1")
	if retrieved.Progress != 50.0 || retrieved.BytesDownloaded != 500 {
		t.Errorf("Progress update mismatch: %+v", retrieved)
	}

	// Set status to done
	if err := store.SetStatus(ctx, "job-1", StatusDone, ""); err != nil {
		t.Fatalf("SetStatus failed: %v", err)
	}
	retrieved, _ = store.Get(ctx, "job-1")
	if retrieved.Status != StatusDone || retrieved.CompletedAt == nil {
		t.Errorf("Expected done status with CompletedAt set, got %+v", retrieved)
	}
}

func TestAcquireNextPending(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Open in-memory db: %v", err)
	}
	defer db.Close()

	store := NewJobStore(db)
	_ = store.EnsureTable(ctx)

	_ = store.Create(ctx, &Job{ID: "j1", Type: TypeDownload, Status: StatusPending})
	_ = store.Create(ctx, &Job{ID: "j2", Type: TypeEncode, Status: StatusPending})

	// Acquire download
	acquired, err := store.AcquireNextPending(ctx, []string{TypeDownload})
	if err != nil {
		t.Fatalf("AcquireNextPending failed: %v", err)
	}
	if acquired == nil || acquired.ID != "j1" || acquired.Status != StatusRunning {
		t.Fatalf("Expected j1 running, got %+v", acquired)
	}

	// Try acquiring download again (should return nil since j1 is running and no more downloads)
	again, err := store.AcquireNextPending(ctx, []string{TypeDownload})
	if err != nil {
		t.Fatalf("AcquireNextPending second time error: %v", err)
	}
	if again != nil {
		t.Fatalf("Expected nil, got %+v", again)
	}
}

type mockRunner struct {
	mu           sync.Mutex
	activeCount  int32
	maxActive    int32
	executedJobs []string
	delay        time.Duration
	doneCh       chan struct{}
}

func (m *mockRunner) Run(ctx context.Context, job *Job, progress ProgressFunc) error {
	cur := atomic.AddInt32(&m.activeCount, 1)
	m.mu.Lock()
	if cur > m.maxActive {
		m.maxActive = cur
	}
	m.executedJobs = append(m.executedJobs, job.ID)
	m.mu.Unlock()

	if progress != nil {
		progress(50.0, "processing", 50, 100, 1)
	}
	time.Sleep(m.delay)
	atomic.AddInt32(&m.activeCount, -1)
	if m.doneCh != nil {
		m.doneCh <- struct{}{}
	}
	return nil
}

func TestQueueSemaphores(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbFile := filepath.Join(t.TempDir(), "queue_test.db")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()

	_, _ = db.Exec("PRAGMA journal_mode = WAL;")
	_, _ = db.Exec("PRAGMA busy_timeout = 5000;")

	store := NewJobStore(db)
	if err := store.EnsureTable(ctx); err != nil {
		t.Fatalf("EnsureTable failed: %v", err)
	}

	cfg := QueueConfig{
		MaxDownloads: 2,
		MaxEncodes:   1,
		MaxIndexes:   1,
	}
	qm := NewQueueManager(store, cfg)

	mockDl := &mockRunner{
		delay:  15 * time.Millisecond,
		doneCh: make(chan struct{}, 4),
	}
	qm.RegisterRunner(TypeDownload, mockDl)

	// Submit 4 download jobs
	for i := 1; i <= 4; i++ {
		if err := qm.Submit(ctx, &Job{
			ID:     fmt.Sprintf("dl-%d", i),
			Type:   TypeDownload,
			Target: "test",
		}); err != nil {
			t.Fatalf("Submit failed: %v", err)
		}
	}

	qm.Start(ctx)

	// Wait for all 4 jobs to complete
	for i := 0; i < 4; i++ {
		select {
		case <-mockDl.doneCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("Timed out waiting for job %d to finish", i+1)
		}
	}

	// Give a moment for completion status writes
	time.Sleep(50 * time.Millisecond)
	qm.Stop()

	// Verify semaphore limit: maxActive should not exceed MaxDownloads (2)
	mockDl.mu.Lock()
	maxConcurrent := mockDl.maxActive
	totalExecuted := len(mockDl.executedJobs)
	mockDl.mu.Unlock()

	if maxConcurrent > 2 {
		t.Errorf("Semaphore violation: max concurrent %d exceeded limit 2", maxConcurrent)
	}
	if totalExecuted != 4 {
		t.Errorf("Expected 4 executed jobs, got %d", totalExecuted)
	}

	counts, err := store.CountByStatus(context.Background())
	if err != nil {
		t.Fatalf("CountByStatus error: %v", err)
	}
	if counts[StatusDone] != 4 {
		t.Errorf("Expected 4 done jobs, got counts: %+v", counts)
	}
}

func TestHTTPFallbackDownload(t *testing.T) {
	content := "The quick brown fox jumps over the lazy dog."
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		_, _ = w.Write([]byte(content))
	}))
	defer ts.Close()

	tempDir := t.TempDir()
	runner := NewAria2cRunner("non-existent-binary-path")

	job := &Job{
		ID:       "test-http",
		Type:     TypeDownload,
		Target:   ts.URL,
		Metadata: fmt.Sprintf(`{"output_dir":"%s","file_name":"output.txt"}`, filepath.ToSlash(tempDir)),
	}

	var progressCalled bool
	err := runner.Run(context.Background(), job, func(progress float64, msg string, bytesDown, totalBytes int64, eta int) {
		progressCalled = true
	})

	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if !progressCalled {
		t.Errorf("Expected progress callback to be called")
	}

	outFile := filepath.Join(tempDir, "output.txt")
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("Read output file: %v", err)
	}
	if string(data) != content {
		t.Errorf("Output content mismatch: got %q, want %q", string(data), content)
	}
}

func TestAria2cRunnerParallelRangeDownload(t *testing.T) {
	// 2.5 MB payload triggers 6x parallel segmented Range download path (>= 2 MB)
	payload := bytes.Repeat([]byte("ARK-RANGE-TEST-BLOCK-0123456789!"), (2*1024*1024+512*1024)/32)
	wantHash := sha256.Sum256(payload)

	var rangeReqs atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			rangeReqs.Add(1)
		}
		http.ServeContent(w, r, "big.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer ts.Close()

	tempDir := t.TempDir()
	runner := NewAria2cRunner("non-existent-binary-path")
	job := &Job{
		ID:       "test-range-6x",
		Type:     TypeDownload,
		Target:   ts.URL + "/big.bin",
		Metadata: fmt.Sprintf(`{"output_dir":"%s","file_name":"big.bin"}`, filepath.ToSlash(tempDir)),
	}
	if err := runner.Run(context.Background(), job, nil); err != nil {
		t.Fatalf("parallel Range download failed: %v", err)
	}
	if rangeReqs.Load() < 2 {
		t.Errorf("expected parallel Range requests >= 2, got %d", rangeReqs.Load())
	}

	gotBytes, err := os.ReadFile(filepath.Join(tempDir, "big.bin"))
	if err != nil {
		t.Fatalf("read big.bin: %v", err)
	}
	if sha256.Sum256(gotBytes) != wantHash {
		t.Fatalf("parallel range file SHA-256 mismatch (got %d bytes, want %d)", len(gotBytes), len(payload))
	}
}

func TestAria2cRunnerMultiMirrorMeta4AndClearCompleted(t *testing.T) {
	payload := bytes.Repeat([]byte("ARK-MULTI-MIRROR-TURBO-CHUNK-01!"), (3*1024*1024)/32)
	wantHash := sha256.Sum256(payload)

	var m1Reqs, m2Reqs atomic.Int32
	mirror1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			m1Reqs.Add(1)
		}
		http.ServeContent(w, r, "wiki.zim", time.Now(), bytes.NewReader(payload))
	}))
	defer mirror1.Close()

	mirror2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			m2Reqs.Add(1)
		}
		http.ServeContent(w, r, "wiki.zim", time.Now(), bytes.NewReader(payload))
	}))
	defer mirror2.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wiki.zim.meta4" {
			w.Header().Set("Content-Type", "application/metalink4+xml")
			_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<metalink xmlns="urn:ietf:params:xml:ns:metalink">
  <file name="wiki.zim">
    <size>%d</size>
    <url priority="1">%s/wiki.zim</url>
    <url priority="2">%s/wiki.zim</url>
  </file>
</metalink>`, len(payload), mirror1.URL, mirror2.URL)
			return
		}
		http.ServeContent(w, r, "wiki.zim", time.Now(), bytes.NewReader(payload))
	}))
	defer origin.Close()

	tempDir := t.TempDir()
	runner := NewAria2cRunner("non-existent-binary-path")
	job := &Job{
		ID:       "test-meta4-turbo",
		Type:     TypeDownload,
		Target:   origin.URL + "/wiki.zim",
		Metadata: fmt.Sprintf(`{"output_dir":"%s","file_name":"wiki.zim"}`, filepath.ToSlash(tempDir)),
	}
	if err := runner.Run(context.Background(), job, nil); err != nil {
		t.Fatalf("multi-mirror meta4 download failed: %v", err)
	}
	if m1Reqs.Load() == 0 || m2Reqs.Load() == 0 {
		t.Errorf("expected both mirrors to serve parallel chunks (m1=%d, m2=%d)", m1Reqs.Load(), m2Reqs.Load())
	}
	gotBytes, err := os.ReadFile(filepath.Join(tempDir, "wiki.zim"))
	if err != nil {
		t.Fatalf("read wiki.zim: %v", err)
	}
	if sha256.Sum256(gotBytes) != wantHash {
		t.Fatalf("multi-mirror file SHA-256 mismatch")
	}

	// Verify ClearCompleted removes only 'done' jobs while preserving 'pending'/'running'
	ctx := context.Background()
	db, _ := sql.Open("sqlite", ":memory:")
	defer db.Close()
	store := NewJobStore(db)
	_ = store.EnsureTable(ctx)
	_ = store.Create(ctx, &Job{ID: "d1", Type: TypeDownload, Status: StatusDone})
	_ = store.Create(ctx, &Job{ID: "p1", Type: TypeDownload, Status: StatusPending})
	if err := store.ClearCompleted(ctx); err != nil {
		t.Fatalf("ClearCompleted: %v", err)
	}
	counts, _ := store.CountByStatus(ctx)
	if counts[StatusDone] != 0 || counts[StatusPending] != 1 {
		t.Errorf("expected done=0 and pending=1 after ClearCompleted, got %+v", counts)
	}
}
