package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ark/internal/jobs"

	_ "modernc.org/sqlite"
)

func TestNewServerAndRoutes(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ark-web-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test_ark.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()

	// Initialize basic schema for testing
	_, err = db.Exec(`
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT);
		CREATE TABLE jobs (id TEXT PRIMARY KEY, type TEXT, target TEXT, status TEXT, progress REAL, progress_msg TEXT DEFAULT '', error_msg TEXT DEFAULT '', bytes_downloaded INTEGER DEFAULT 0, total_bytes INTEGER DEFAULT 0, eta_seconds INTEGER DEFAULT 0, metadata TEXT DEFAULT '{}', created_at DATETIME, updated_at DATETIME, completed_at DATETIME);
		CREATE TABLE catalog_items (id TEXT PRIMARY KEY, title TEXT, category TEXT, language TEXT, size_bytes INTEGER, date TEXT, flavour TEXT, url TEXT, is_downloaded BOOLEAN, file_path TEXT, created_at DATETIME);
		CREATE TABLE app_states (id TEXT PRIMARY KEY, name TEXT, category TEXT, image TEXT, container_id TEXT, status TEXT, port INTEGER, proxy_mode TEXT, auto_start BOOLEAN, created_at DATETIME, updated_at DATETIME);
	`)
	if err != nil {
		t.Fatalf("failed to init schema: %v", err)
	}

	server, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	mux := http.NewServeMux()
	if err := server.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes failed: %v", err)
	}

	routes := []struct {
		path         string
		expectedCode int
		contains     string
	}{
		{"/healthz", http.StatusOK, `"status":"ok"`},
		{"/", http.StatusOK, "Zero Pre-installed Apps"},
		{"/library", http.StatusOK, "Offline Library"},
		{"/get-content", http.StatusOK, "Get Content"},
		{"/apps", http.StatusOK, "Modular App Catalog"},
		{"/ask", http.StatusOK, "Ask Local AI"},
		{"/jobs", http.StatusOK, "Job Queue"},
		{"/system", http.StatusOK, "Live Host Telemetry"},
		{"/settings", http.StatusOK, "Settings"},
		{"/a/kiwix/", http.StatusNotFound, "App Not Active"},
		{"/static/ark.css", http.StatusOK, "--color-bg: #F4F6F9;"},
		{"/static/htmx.min.js", http.StatusOK, "htmx"},
	}

	for _, rt := range routes {
		t.Run(rt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, rt.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != rt.expectedCode {
				t.Errorf("GET %s expected status %d, got %d", rt.path, rt.expectedCode, w.Code)
			}
			body := w.Body.String()
			if !strings.Contains(body, rt.contains) {
				t.Errorf("GET %s expected body to contain %q, got %q", rt.path, rt.contains, body)
			}
		})
	}
}

func TestAppLifecycleEndpoints(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ark-web-app-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test_ark.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE app_states (id TEXT PRIMARY KEY, name TEXT, category TEXT, image TEXT, container_id TEXT, status TEXT, port INTEGER, proxy_mode TEXT, auto_start BOOLEAN, created_at DATETIME, updated_at DATETIME);
	`)
	if err != nil {
		t.Fatalf("failed to init schema: %v", err)
	}

	server, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	mux := http.NewServeMux()
	if err := server.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes failed: %v", err)
	}

	// 1. Install cyberchef
	form := url.Values{"id": {"cyberchef"}}
	req := httptest.NewRequest(http.MethodPost, "/api/apps/install", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("install expected redirect 303, got %d: %s", w.Code, w.Body.String())
	}

	var status string
	err = db.QueryRow("SELECT status FROM app_states WHERE id = 'cyberchef'").Scan(&status)
	if err != nil {
		t.Fatalf("failed to query app_states after install: %v", err)
	}
	if status != "stopped" {
		t.Errorf("expected installed app status 'stopped', got %s", status)
	}

	// 2. Start cyberchef
	req = httptest.NewRequest(http.MethodPost, "/api/apps/start", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	_ = db.QueryRow("SELECT status FROM app_states WHERE id = 'cyberchef'").Scan(&status)
	if status != "running" {
		t.Errorf("expected app status 'running', got %s", status)
	}

	// 3. Stop cyberchef
	req = httptest.NewRequest(http.MethodPost, "/api/apps/stop", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	_ = db.QueryRow("SELECT status FROM app_states WHERE id = 'cyberchef'").Scan(&status)
	if status != "stopped" {
		t.Errorf("expected app status 'stopped', got %s", status)
	}

	// 4. Uninstall cyberchef
	req = httptest.NewRequest(http.MethodPost, "/api/apps/uninstall", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM app_states WHERE id = 'cyberchef'").Scan(&count)
	if count != 0 {
		t.Errorf("expected app_states count 0 after uninstall, got %d", count)
	}
}

func TestCSSWCAGPalette(t *testing.T) {
	cssData, err := embeddedFiles.ReadFile("static/ark.css")
	if err != nil {
		t.Fatalf("failed to read embedded ark.css: %v", err)
	}
	css := string(cssData)

	expectedTokens := []string{
		"#F4F6F9", // Bright background
		"#FFFFFF", // Surface
		"#0F172A", // Primary text (17.4:1 AAA)
		"#0369A1", // Accent (5.9:1 AA+)
		"#475569", // Secondary text (7.5:1 AAA)
		"#CBD5E1", // Border
		"#15803D", // Success
		"#B91C1C", // Error
		"18px",    // Font base size
		"data:image/svg+xml", // Stencil art background
		"prefers-reduced-motion",
	}

	for _, token := range expectedTokens {
		if !strings.Contains(css, token) {
			t.Errorf("ark.css missing expected WCAG palette token: %s", token)
		}
	}
}

func TestJobQueueAndCancelEndpoints(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ark-web-jobs-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test_ark.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE jobs (
			id TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			target TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			progress REAL NOT NULL DEFAULT 0.0,
			progress_msg TEXT NOT NULL DEFAULT '',
			error_msg TEXT NOT NULL DEFAULT '',
			bytes_downloaded INTEGER NOT NULL DEFAULT 0,
			total_bytes INTEGER NOT NULL DEFAULT 0,
			eta_seconds INTEGER NOT NULL DEFAULT 0,
			metadata TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			completed_at DATETIME
		);
	`)
	if err != nil {
		t.Fatalf("failed to init schema: %v", err)
	}

	server, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	jobStore := jobs.NewJobStore(db)
	qm := jobs.NewQueueManager(jobStore, jobs.DefaultQueueConfig())
	server.SetQueueManager(qm)

	mux := http.NewServeMux()
	if err := server.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes failed: %v", err)
	}

	// 1. Submit preset job via /api/jobs
	form := url.Values{"type": {"preset"}, "target": {"wikimed"}}
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect on job creation, got %d", w.Code)
	}

	var jobID, jobStatus string
	err = db.QueryRow("SELECT id, status FROM jobs LIMIT 1").Scan(&jobID, &jobStatus)
	if err != nil {
		t.Fatalf("failed to find inserted job: %v", err)
	}
	if jobStatus != "pending" {
		t.Errorf("expected initial status 'pending', got %q", jobStatus)
	}

	// 2. Verify /jobs renders Cancel button for pending job
	req = httptest.NewRequest(http.MethodGet, "/jobs", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "/api/jobs/cancel") {
		t.Errorf("expected /jobs page to render Cancel button for pending job")
	}

	// 3. Cancel job via /api/jobs/cancel
	cancelForm := url.Values{"id": {jobID}}
	req = httptest.NewRequest(http.MethodPost, "/api/jobs/cancel", strings.NewReader(cancelForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	_ = db.QueryRow("SELECT status FROM jobs WHERE id = ?", jobID).Scan(&jobStatus)
	if jobStatus != "cancelled" {
		t.Errorf("expected status 'cancelled' after cancel, got %q", jobStatus)
	}

	// 4. Retry job via /api/jobs/retry
	req = httptest.NewRequest(http.MethodPost, "/api/jobs/retry", strings.NewReader(cancelForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	_ = db.QueryRow("SELECT status FROM jobs WHERE id = ?", jobID).Scan(&jobStatus)
	if jobStatus != "pending" {
		t.Errorf("expected status 'pending' after retry, got %q", jobStatus)
	}

	// 5. Delete job via /api/jobs/delete
	req = httptest.NewRequest(http.MethodPost, "/api/jobs/delete", strings.NewReader(cancelForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM jobs WHERE id = ?", jobID).Scan(&count)
	if count != 0 {
		t.Errorf("expected job to be deleted, count = %d", count)
	}
}

func TestPhase2SettingsAndAIEndpoints(t *testing.T) {
	aiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"llama3.2:3b"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Boil water for 1 minute [Water Guide]."}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer aiSrv.Close()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_ark.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT);`)
	if err != nil {
		t.Fatalf("init schema: %v", err)
	}

	server, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	mux := http.NewServeMux()
	if err := server.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes failed: %v", err)
	}

	// 1. Save AI settings
	form := url.Values{
		"ai_server_url":  {aiSrv.URL},
		"ai_chat_model":  {"llama3.2:3b"},
		"ai_embed_model": {"nomic-embed-text"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/ai", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 from /settings/ai, got %d", w.Code)
	}

	// 2. Test AI connection endpoint
	req = httptest.NewRequest(http.MethodPost, "/api/ai/test", strings.NewReader(url.Values{"ai_server_url": {aiSrv.URL}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var testResp struct {
		OK     bool     `json:"ok"`
		Models []string `json:"models"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &testResp)
	if !testResp.OK || len(testResp.Models) != 1 || testResp.Models[0] != "llama3.2:3b" {
		t.Fatalf("unexpected /api/ai/test response: %s", w.Body.String())
	}

	// 3. Index a sample document synchronously (QueueManager nil in unit test)
	sampleDoc := filepath.Join(tmpDir, "water.txt")
	if err := os.WriteFile(sampleDoc, []byte("To purify water in emergencies, bring clear water to a rolling boil for 1 minute."), 0644); err != nil {
		t.Fatalf("write sample doc: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/ai/index", strings.NewReader(url.Values{"path": {sampleDoc}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 from /api/ai/index, got %d (%s)", w.Code, w.Body.String())
	}

	// 4. Ask AI RAG chat endpoint should retrieve chunk citation and return LLM answer
	req = httptest.NewRequest(http.MethodPost, "/api/ai/chat", strings.NewReader(`{"question":"How long should I boil water?"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Boil water for 1 minute") || !strings.Contains(w.Body.String(), "water.txt") {
		t.Fatalf("unexpected /api/ai/chat response (%d): %s", w.Code, w.Body.String())
	}
}
