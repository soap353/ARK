package web

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"ark/catalog"
	"ark/internal/ai"
	"ark/internal/apps"
	"ark/internal/jobs"

	"github.com/klauspost/compress/zstd"
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
		{"/", http.StatusOK, "ConvertX"},
		{"/library", http.StatusOK, "Check ZIM Updates"},
		{"/kiwix", http.StatusOK, "Open Local ZIM Reader"},
		{"/get-content", http.StatusOK, "Open Local ZIM Reader"},
		{"/field-guide", http.StatusOK, "Search Remedy"},
		{"/apps", http.StatusOK, "<th>Module</th>"},
		{"/ask", http.StatusOK, "Knowledge Index"},
		{"/jobs", http.StatusSeeOther, ""},
		{"/system", http.StatusOK, "CPU Usage"},
		{"/settings", http.StatusOK, "<h2>General</h2>"},
		{"/a/kiwix/", http.StatusSeeOther, ""},
		{"/static/ark.css", http.StatusOK, "--color-bg: #F5F2EB;"},
		{"/static/htmx.min.js", http.StatusOK, "htmx"},
	}

	for _, rt := range routes {
		t.Run(rt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, rt.path, nil)
			req.RemoteAddr = "127.0.0.1:12345"
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
	if instApps, _ := server.fetchApps(); len(instApps) < 7 {
		t.Errorf("expected fresh DB to pre-install all 7 built-in apps, got %d", len(instApps))
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
	req.RemoteAddr = "127.0.0.1:12345"
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

	// 2. Start cyberchef -> status running and persisted auto_start = 1
	req = httptest.NewRequest(http.MethodPost, "/api/apps/start", strings.NewReader(form.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var autoStart int
	_ = db.QueryRow("SELECT status, COALESCE(auto_start, 0) FROM app_states WHERE id = 'cyberchef'").Scan(&status, &autoStart)
	if status != "running" || autoStart != 1 {
		t.Errorf("expected app status 'running' and auto_start=1, got %s, %d", status, autoStart)
	}
	if ids := server.appsToRestore(); len(ids) != 1 || ids[0] != "cyberchef" {
		t.Errorf("expected appsToRestore to return [cyberchef], got %v", ids)
	}

	// Simulate shutdown (runtime status becomes stopped, desired auto_start=1 remains)
	_, _ = db.Exec(`UPDATE app_states SET status = 'stopped' WHERE status = 'running'`)
	if ids := server.appsToRestore(); len(ids) != 1 || ids[0] != "cyberchef" {
		t.Errorf("expected cyberchef to persist for next startup after shutdown, got %v", ids)
	}

	// 3. Stop cyberchef -> status stopped and persisted auto_start = 0 (must not start on next Ark boot)
	req = httptest.NewRequest(http.MethodPost, "/api/apps/stop", strings.NewReader(form.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	_ = db.QueryRow("SELECT status, COALESCE(auto_start, 0) FROM app_states WHERE id = 'cyberchef'").Scan(&status, &autoStart)
	if status != "stopped" || autoStart != 0 {
		t.Errorf("expected app status 'stopped' and auto_start=0, got %s, %d", status, autoStart)
	}
	if ids := server.appsToRestore(); len(ids) != 0 {
		t.Errorf("expected stopped app not to be restored on next startup, got %v", ids)
	}

	// 4. Uninstall cyberchef
	req = httptest.NewRequest(http.MethodPost, "/api/apps/uninstall", strings.NewReader(form.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
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
		"#F5F2EB", // Warm parchment background
		"#FDFBF7", // Elevated almanac surface
		"#1B2E26", // Forest-pine sidebar
		"#1C1F1D", // Archival ink text
		"#C25E28", // Terracotta primary accent
		"#525B54", // Muted sage secondary text
		"#D6CFC2", // Hairline parchment border
		"#2E6F40", // Forest success
		"#B91C1C", // Crimson error
		"--sidebar-width",
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
	req.RemoteAddr = "127.0.0.1:12345"
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

	// 2. Verify System (/system) renders Cancel button for pending job and Home omits /jobs nav tab and Session Jobs
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	homeBody := w.Body.String()
	if strings.Contains(homeBody, `href="/jobs"`) {
		t.Errorf("expected /jobs link to be removed from navigation and Home card")
	}
	if strings.Contains(homeBody, "Session Jobs") {
		t.Errorf("expected Session Jobs to be moved off Home to /system")
	}

	req = httptest.NewRequest(http.MethodGet, "/system", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	sysBody := w.Body.String()
	if !strings.Contains(sysBody, "/api/jobs/cancel") {
		t.Errorf("expected /system page to render Cancel button for pending job")
	}

	// 3. Cancel job via /api/jobs/cancel
	cancelForm := url.Values{"id": {jobID}}
	req = httptest.NewRequest(http.MethodPost, "/api/jobs/cancel", strings.NewReader(cancelForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	_ = db.QueryRow("SELECT status FROM jobs WHERE id = ?", jobID).Scan(&jobStatus)
	if jobStatus != "cancelled" {
		t.Errorf("expected status 'cancelled' after cancel, got %q", jobStatus)
	}

	// 4. Retry job via /api/jobs/retry
	req = httptest.NewRequest(http.MethodPost, "/api/jobs/retry", strings.NewReader(cancelForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	_ = db.QueryRow("SELECT status FROM jobs WHERE id = ?", jobID).Scan(&jobStatus)
	if jobStatus != "pending" {
		t.Errorf("expected status 'pending' after retry, got %q", jobStatus)
	}

	// 5. Delete job via /api/jobs/delete
	req = httptest.NewRequest(http.MethodPost, "/api/jobs/delete", strings.NewReader(cancelForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
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
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"title\":\"Boil Water\",\"answer\":\"Boil water for 1 minute.\",\"references\":[{\"id\":1,\"quote\":\"rolling boil for 1 minute\"}]}"}}]}`))
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
	req.RemoteAddr = "127.0.0.1:12345"
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
	req.RemoteAddr = "127.0.0.1:12345"
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

func TestSettingsUSBAndUpdateRoutes(t *testing.T) {
	tmpDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmpDir, "zim"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "zim", "mini.zim"), []byte("zim-bytes"), 0644)

	dbPath := filepath.Join(tmpDir, "ark.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
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
		CREATE TABLE usb_profiles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			description TEXT,
			config_json TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		t.Fatalf("init schema: %v", err)
	}

	server, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	jobStore := jobs.NewJobStore(db)
	qm := jobs.NewQueueManager(jobStore, jobs.DefaultQueueConfig())
	server.SetQueueManager(qm)

	mux := http.NewServeMux()
	if err := server.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// 1. Save a USB profile
	form := url.Values{
		"profile_name":   {"field_kit"},
		"categories":     {"zim"},
		"include_config": {"1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/usb-profile", strings.NewReader(form.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 on /settings/usb-profile, got %d", w.Code)
	}

	// 2. Render /settings and verify Phase 3 USB + Updates cards and saved profile button appear
	req = httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "Start USB Export Job") || !strings.Contains(body, "field_kit") || !strings.Contains(body, "Updates &amp; Rollback") {
		t.Fatalf("expected Phase 3 Settings cards rendered, got status %d", w.Code)
	}

	// 3. Submit /settings/usb-export enqueues a usb job and redirects to /
	usbDest := filepath.Join(tmpDir, "usb-target")
	expForm := url.Values{
		"target_path": {"custom"},
		"custom_path": {usbDest},
		"categories":  {"zim"},
	}
	req = httptest.NewRequest(http.MethodPost, "/settings/usb-export", strings.NewReader(expForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/system" {
		t.Fatalf("expected redirect to /system from /settings/usb-export, got %d -> %s", w.Code, w.Header().Get("Location"))
	}
	var jobType string
	if err := db.QueryRow(`SELECT type FROM jobs LIMIT 1`).Scan(&jobType); err != nil || jobType != jobs.TypeUSB {
		t.Fatalf("expected queued job type %q, got %q (err=%v)", jobs.TypeUSB, jobType, err)
	}

	// 4. Reject invalid/missing update bundle cleanly
	updForm := url.Values{"update_source": {filepath.Join(tmpDir, "nonexistent-update")}}
	req = httptest.NewRequest(http.MethodPost, "/settings/update", strings.NewReader(updForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatalf("expected redirect with err= for missing update bundle, got %d -> %s", w.Code, w.Header().Get("Location"))
	}
}

func TestHomeSessionJobsLimitAndGetContentNOMAD(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(tmpDir, "test_nomad.db"))
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
		t.Fatalf("create jobs table: %v", err)
	}

	// Seed a job from a previous session (10 minutes ago) - must NOT appear on Home
	_, err = db.Exec(`
		INSERT INTO jobs (id, type, target, status, progress, progress_msg, created_at, updated_at)
		VALUES ('job-old', 'preset', 'old-session-target', 'done', 100.0, 'Done', datetime('now', '-10 minutes'), datetime('now', '-10 minutes'))
	`)
	if err != nil {
		t.Fatalf("insert old job: %v", err)
	}
	server, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	mux := http.NewServeMux()
	if err := server.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes failed: %v", err)
	}

	// Submit 6 active jobs in current session -> all 6 active jobs must appear on /system (unlimited active jobs)
	activeTargets := []string{"session-job-1", "session-job-2", "session-job-3", "session-job-4", "session-job-5", "session-job-6"}
	for _, tgt := range activeTargets {
		form := url.Values{"type": {"preset"}, "target": {tgt}}
		req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(form.Encode()))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/system" {
			t.Fatalf("expected 303 redirect to /system, got %d -> %s", w.Code, w.Header().Get("Location"))
		}
	}

	// Insert 6 completed ('done') jobs in current session -> only the 5 most recent completed jobs may appear
	doneTargets := []string{"done-job-1", "done-job-2", "done-job-3", "done-job-4", "done-job-5", "done-job-6"}
	for i, tgt := range doneTargets {
		_, err := db.Exec(`
			INSERT INTO jobs (id, type, target, status, progress, progress_msg, created_at, updated_at, completed_at)
			VALUES (?, 'download', ?, 'done', 100.0, 'Complete', datetime('now', '+' || ? || ' seconds'), CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		`, tgt, tgt, i+1)
		if err != nil {
			t.Fatalf("insert done job %s: %v", tgt, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/system", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if strings.Contains(body, "old-session-target") {
		t.Errorf("expected previous session completed job to be excluded from /system")
	}
	for _, act := range activeTargets {
		if !strings.Contains(body, act) {
			t.Errorf("expected /system to show all 6 active jobs, missing %q", act)
		}
	}
	if strings.Contains(body, "done-job-1") {
		t.Errorf("expected at most 5 completed jobs on /system, oldest completed job done-job-1 should be excluded")
	}
	for _, recentDone := range doneTargets[1:] {
		if !strings.Contains(body, recentDone) {
			t.Errorf("expected /system to include recent completed job %q", recentDone)
		}
	}

	// Verify /get-content includes all NOMAD options and Kiwix Library section
	req = httptest.NewRequest(http.MethodGet, "/get-content", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	gcBody := w.Body.String()
	for _, needle := range []string{
		"Quick Reference", "Popular Articles", "Complete Wikipedia (Compact)", "Complete Wikipedia (No Images)", "Complete Wikipedia (Full)",
		"Medicine", "Survival &amp; Preparedness", "Education &amp; Reference", "DIY &amp; Repair", "Agriculture &amp; Food", "Computing &amp; Technology",
		"Pacific Region", "Mountain Region", "West South Central", "East South Central", "South Atlantic",
		"West North Central", "East North Central", "Mid-Atlantic", "New England",
		"Kiwix Library Direct Access",
	} {
		if !strings.Contains(gcBody, needle) {
			t.Errorf("expected /get-content to render %q", needle)
		}
	}

	// Verify /api/kiwix/catalog parses OPDS feed cleanly
	opdsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <totalResults>1</totalResults>
  <entry>
    <id>urn:uuid:test-zim</id>
    <title>WikiMed English</title>
    <summary>Offline medical encyclopedia</summary>
    <language>eng</language>
    <category>medical</category>
    <flavour>maxi</flavour>
    <updated>2026-06-01T00:00:00Z</updated>
    <link rel="http://opds-spec.org/acquisition/open-access" type="application/x-zim" href="https://download.kiwix.org/zim/wikimed_en_all_maxi_2026-06.zim.meta4" length="1073741824"/>
  </entry>
</feed>`))
	}))
	defer opdsSrv.Close()
	t.Setenv("ARK_KIWIX_OPDS_URL", opdsSrv.URL)

	req = httptest.NewRequest(http.MethodGet, "/api/kiwix/catalog?q=wikimed", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /api/kiwix/catalog, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "WikiMed English") || !strings.Contains(w.Body.String(), "wikimed_en_all_maxi_2026-06.zim") {
		t.Errorf("unexpected /api/kiwix/catalog JSON: %s", w.Body.String())
	}

	// Verify disk library scan + safe delete
	zimDir := filepath.Join(tmpDir, "zim")
	_ = os.MkdirAll(zimDir, 0755)
	sampleZim := filepath.Join(zimDir, "wikipedia_en_mini_2026-06.zim")
	_ = os.WriteFile(sampleZim, []byte("zimbytes"), 0644)

	req = httptest.NewRequest(http.MethodGet, "/library", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "wikipedia_en_mini_2026-06.zim") {
		t.Errorf("expected /library to list on-disk .zim file")
	}

	// Reject path traversal delete outside ArkData
	delBad := url.Values{"rel_path": {"../outside.txt"}}
	req = httptest.NewRequest(http.MethodPost, "/api/library/delete", strings.NewReader(delBad.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Errorf("expected error redirect on traversal delete, got %s", w.Header().Get("Location"))
	}

	// Delete valid library ZIM
	delGood := url.Values{"rel_path": {"zim/wikipedia_en_mini_2026-06.zim"}}
	req = httptest.NewRequest(http.MethodPost, "/api/library/delete", strings.NewReader(delGood.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if _, err := os.Stat(sampleZim); !os.IsNotExist(err) {
		t.Errorf("expected deleted ZIM file to be removed from disk")
	}

	// Verify embedded catalog fallback works cleanly even outside repo root (standalone release binary)
	embeddedManifests, err := apps.LoadAllManifestsFS(catalog.FS)
	if err != nil || len(embeddedManifests) < 7 {
		t.Errorf("expected embedded catalog manifests to load cleanly, got count=%d err=%v", len(embeddedManifests), err)
	}
	origWD, _ := os.Getwd()
	_ = os.Chdir(t.TempDir())
	_, avail := server.fetchApps()
	_ = os.Chdir(origWD)
	if len(avail) < 7 {
		t.Errorf("expected fetchApps() to fall back to embedded catalog when disk catalog/ is absent, got %d apps", len(avail))
	}

	// Verify custom map pin save & read
	pinForm := url.Values{"title": {"Base Camp"}, "lat": {"45.5"}, "lng": {"-122.6"}, "category": {"shelter"}}
	req = httptest.NewRequest(http.MethodPost, "/api/maps/pins", strings.NewReader(pinForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	req = httptest.NewRequest(http.MethodGet, "/api/maps/pins", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "Base Camp") {
		t.Errorf("expected saved pin 'Base Camp' in /api/maps/pins JSON, got %s", w.Body.String())
	}

	// Verify custom Docker app save renders on /apps
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS app_states (id TEXT PRIMARY KEY, name TEXT, category TEXT, image TEXT, container_id TEXT, status TEXT, port INTEGER, proxy_mode TEXT, auto_start BOOLEAN, created_at DATETIME, updated_at DATETIME);`)
	customForm := url.Values{"name": {"My Wiki"}, "image": {"nginx:alpine"}, "port": {"80"}, "proxy_mode": {"path"}}
	req = httptest.NewRequest(http.MethodPost, "/api/apps/custom", strings.NewReader(customForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	req = httptest.NewRequest(http.MethodGet, "/apps", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "My Wiki") || !strings.Contains(w.Body.String(), "nginx:alpine") {
		t.Errorf("expected custom app on /apps page")
	}

	// Verify Qdrant removed, clickable app modal in /apps, no bottom bars/storage/jobs on Home, and Storage on /system
	req = httptest.NewRequest(http.MethodGet, "/apps", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	appsOut := w.Body.String()
	if strings.Contains(appsOut, "Qdrant") || strings.Contains(appsOut, "qdrant/qdrant") {
		t.Errorf("expected qdrant app option to be removed from /apps")
	}
	for _, want := range []string{`class="app-name-trigger"`, `id="app-modal"`, `openAppModal`} {
		if !strings.Contains(appsOut, want) {
			t.Errorf("expected /apps modal UI to include %q", want)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	homeOut := w.Body.String()
	for _, forbidden := range []string{`class="sidebar-footer"`, `<footer`, "Auto-updating every 2s", "Ark Archive:", "Session Jobs"} {
		if strings.Contains(homeOut, forbidden) {
			t.Errorf("expected Home UI not to contain %q", forbidden)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/system", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	sysOut := w.Body.String()
	if !strings.Contains(sysOut, "Ark Archive:") || !strings.Contains(sysOut, "Session Jobs") || !strings.Contains(sysOut, `action="/settings/shutdown"`) {
		t.Errorf("expected /system to host Storage (Ark Archive), Session Jobs, and Shut Down Ark controls")
	}

	// Verify /library card grid, origin icon endpoint, and AJAX Add to AI
	t.Setenv("ARK_OFFLINE_ICONS", "1")
	if err := os.MkdirAll(filepath.Join(tmpDir, "zim"), 0o755); err != nil {
		t.Fatalf("mkdir zim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "zim", "wikipedia_en_top_mini.zim"), []byte("zim-stub"), 0o644); err != nil {
		t.Fatalf("write stub zim: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/library", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	libOut := w.Body.String()
	for _, want := range []string{`class="library-grid"`, `class="library-tile"`, `id="lib-context-menu"`, `/api/library/icon?`} {
		if !strings.Contains(libOut, want) {
			t.Errorf("expected /library grid UI to contain %q", want)
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/api/library/icon?stem=wikipedia_en_top_mini&kind=zim", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "image/svg+xml") || !strings.Contains(w.Body.String(), "<svg") {
		t.Errorf("expected SVG fallback icon from /api/library/icon, got %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	server.queue = jobs.NewQueueManager(jobs.NewJobStore(db), jobs.QueueConfig{})
	aiForm := url.Values{"path": {"zim/wikipedia_en_top_mini.zim"}, "ajax": {"1"}}
	req = httptest.NewRequest(http.MethodPost, "/api/ai/index", strings.NewReader(aiForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("expected JSON ok:true from /api/ai/index ajax=1, got %d: %s", w.Code, w.Body.String())
	}
	var idxResp struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &idxResp)
	if idxResp.JobID == "" {
		t.Fatalf("expected job_id from /api/ai/index ajax=1")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/jobs/status?id="+url.QueryEscape(idxResp.JobID), nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"pending"`) {
		t.Errorf("expected pending job JSON from /api/jobs/status, got %d: %s", w.Code, w.Body.String())
	}

	// Verify /ask includes vendored Markdown + KaTeX offline assets
	req = httptest.NewRequest(http.MethodGet, "/ask", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	askOut := w.Body.String()
	for _, want := range []string{"/static/vendor/katex.min.css", "/static/vendor/marked.min.js", "/static/vendor/katex.min.js", "renderMarkdownAndMath"} {
		if !strings.Contains(askOut, want) {
			t.Errorf("expected /ask to include %q", want)
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/static/vendor/katex.min.js", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.Len() < 1000 {
		t.Errorf("expected /static/vendor/katex.min.js served offline, got %d len=%d", w.Code, w.Body.Len())
	}

	req = httptest.NewRequest(http.MethodGet, "/get-content", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	gcOut := w.Body.String()
	for _, want := range []string{"<li style=\"margin-bottom: 0.2rem;\">Medical Library</li>", "<li style=\"margin-bottom: 0.2rem;\">NHS Medicines</li>", "data-tier-tags="} {
		if !strings.Contains(gcOut, want) {
			t.Errorf("expected /get-content reactive 1-2 word bullet item %q", want)
		}
	}

	// Verify GitHub release check endpoint with mock API
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","name":"Ark v9.9.9","published_at":"2026-11-18T00:00:00Z","assets":[]}`))
	}))
	defer ghSrv.Close()
	t.Setenv("ARK_GITHUB_API_URL", ghSrv.URL)
	req = httptest.NewRequest(http.MethodGet, "/api/update/check?repo=soap353/ARK", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"has_update":true`) {
		t.Errorf("expected has_update:true from /api/update/check, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBuiltInNativeZIMReaderWithoutKiwixContainer(t *testing.T) {
	tmpDir := t.TempDir()
	zimDir := filepath.Join(tmpDir, "zim")
	if err := os.MkdirAll(zimDir, 0o755); err != nil {
		t.Fatalf("mkdir zim: %v", err)
	}

	// Build a minimal valid ZIM v6 (Zstd-compressed) archive with 2 HTML articles and mainPage=0
	article1 := []byte("<html><head><title>Penicillin</title></head><body><h1>Penicillin</h1><p>Beta-lactam antibiotic.</p></body></html>")
	article2 := []byte("<html><head><title>Solar Cell</title></head><body><h1>Solar Cell</h1><p>Photovoltaic diode.</p></body></html>")

	var blobData bytes.Buffer
	tableSize := uint32(3 * 4)
	offs := []uint32{tableSize, tableSize + uint32(len(article1)), tableSize + uint32(len(article1)+len(article2))}
	blobData.Write(article1)
	blobData.Write(article2)

	var uncompressed bytes.Buffer
	for _, off := range offs {
		_ = binary.Write(&uncompressed, binary.LittleEndian, off)
	}
	uncompressed.Write(blobData.Bytes())

	enc, _ := zstd.NewWriter(nil)
	defer enc.Close()
	var clusterBuf bytes.Buffer
	clusterBuf.WriteByte(5)
	clusterBuf.Write(enc.EncodeAll(uncompressed.Bytes(), nil))

	mimeList := []byte("text/html;raw=true\x00\x00")
	mimePos := uint64(80)
	dirStart := mimePos + uint64(len(mimeList))
	var dirBuf bytes.Buffer
	entries := []struct{ url, title string }{
		{"Main_Page", "Penicillin Main"},
		{"Solar_Cell", "Solar Cell"},
	}
	entryOffs := make([]uint64, len(entries))
	for i, e := range entries {
		entryOffs[i] = dirStart + uint64(dirBuf.Len())
		_ = binary.Write(&dirBuf, binary.LittleEndian, uint16(0))
		dirBuf.WriteByte(0)
		dirBuf.WriteByte('C')
		_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0))
		_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0))
		_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(i))
		dirBuf.WriteString(e.url + "\x00")
		dirBuf.WriteString(e.title + "\x00")
	}
	urlPtrPos := dirStart + uint64(dirBuf.Len())
	clusterPtrPos := urlPtrPos + uint64(len(entries)*8)
	clusterDataPos := clusterPtrPos + 8

	var zimBuf bytes.Buffer
	var hdr [80]byte
	binary.LittleEndian.PutUint32(hdr[0:4], ai.ZIMMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], 6)
	binary.LittleEndian.PutUint16(hdr[6:8], 3)
	binary.LittleEndian.PutUint32(hdr[24:28], 2)
	binary.LittleEndian.PutUint32(hdr[28:32], 1)
	binary.LittleEndian.PutUint64(hdr[32:40], urlPtrPos)
	binary.LittleEndian.PutUint64(hdr[40:48], urlPtrPos)
	binary.LittleEndian.PutUint64(hdr[48:56], clusterPtrPos)
	binary.LittleEndian.PutUint64(hdr[56:64], mimePos)
	binary.LittleEndian.PutUint32(hdr[64:68], 0) // MainPage = 0
	binary.LittleEndian.PutUint64(hdr[72:80], clusterDataPos+uint64(clusterBuf.Len()))
	zimBuf.Write(hdr[:])
	zimBuf.Write(mimeList)
	zimBuf.Write(dirBuf.Bytes())
	for _, off := range entryOffs {
		_ = binary.Write(&zimBuf, binary.LittleEndian, off)
	}
	_ = binary.Write(&zimBuf, binary.LittleEndian, clusterDataPos)
	zimBuf.Write(clusterBuf.Bytes())

	if err := os.WriteFile(filepath.Join(zimDir, "mini_wiki.zim"), zimBuf.Bytes(), 0o644); err != nil {
		t.Fatalf("write zim: %v", err)
	}

	server, err := NewServer(nil, tmpDir)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	mux := http.NewServeMux()
	if err := server.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes failed: %v", err)
	}

	// 1. /library opens local ZIMs via /read/zim/<stem>/ in a new tab instead of /a/kiwix/
	req := httptest.NewRequest(http.MethodGet, "/library", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	libBody := w.Body.String()
	if !strings.Contains(libBody, `href="/read/zim/mini_wiki/"`) || !strings.Contains(libBody, `target="_blank"`) {
		t.Errorf("expected /library ZIM card to link to /read/zim/mini_wiki/ with target=\"_blank\"")
	}
	if !strings.Contains(libBody, `action="/read/zim/mini_wiki/"`) {
		t.Errorf("expected /library ZIM card to include inline book search form")
	}

	// Verify GET /read/zim/mini_wiki/?q=penicillin and streaming v6.3 fallback scanTitlesStreaming both return hits
	req = httptest.NewRequest(http.MethodGet, "/read/zim/mini_wiki/?q=penicillin", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Penicillin") {
		t.Errorf("expected /read/zim/mini_wiki/?q=penicillin to render matching Penicillin result, got %d", w.Code)
	}
	if arc, aerr := getZIMArchive(filepath.Join(tmpDir, "zim", "mini_wiki.zim"), "mini_wiki"); aerr != nil {
		t.Fatalf("getZIMArchive: %v", aerr)
	} else if hits := arc.scanTitlesStreaming("penicillin", []string{"penicillin"}, 10); len(hits) == 0 || !strings.Contains(hits[0].title, "Penicillin") {
		t.Errorf("expected scanTitlesStreaming to find Penicillin, got %+v", hits)
	}

	// 2. /a/kiwix/ redirects cleanly to /library
	req = httptest.NewRequest(http.MethodGet, "/a/kiwix/", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/library" {
		t.Errorf("expected /a/kiwix/ to redirect 303 -> /library, got %d -> %s", w.Code, w.Header().Get("Location"))
	}

	// 3. /read/zim/mini_wiki/ opens the ZIM archive's built-in front page directly (302 -> /read/zim/mini_wiki/C/Main_Page)
	req = httptest.NewRequest(http.MethodGet, "/read/zim/mini_wiki/", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/read/zim/mini_wiki/C/Main_Page" {
		t.Fatalf("expected /read/zim/mini_wiki/ to redirect 302 -> /read/zim/mini_wiki/C/Main_Page, got %d -> %s", w.Code, w.Header().Get("Location"))
	}

	req = httptest.NewRequest(http.MethodGet, "/read/zim/mini_wiki/__main__", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/read/zim/mini_wiki/C/Main_Page" {
		t.Fatalf("expected 302 -> /read/zim/mini_wiki/C/Main_Page, got %d -> %s", w.Code, w.Header().Get("Location"))
	}

	req = httptest.NewRequest(http.MethodGet, "/read/zim/mini_wiki/C/Main_Page", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	pageOut := w.Body.String()
	for _, want := range []string{"Beta-lactam antibiotic.", `id="ark-proxy-nav"`, `id="ark-download-html"`, `id="ark-page-pager"`, `id="ark-prev-page"`, `id="ark-next-page"`, `id="ark-current-page-label"`, `/read/zim/mini_wiki/C/Solar_Cell`, `id="ark-theme-sel"`, `highlightAIQuote`, `+ Add to AI`} {
		if w.Code != http.StatusOK || !strings.Contains(pageOut, want) {
			if want == `id="ark-prev-page"` {
				continue // Main_Page may be first in sequence
			}
			t.Errorf("expected rendered ZIM article to contain %q, got %d", want, w.Code)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/read/zim/mini_wiki/C/Solar_Cell", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="ark-prev-page"`) || !strings.Contains(w.Body.String(), `/read/zim/mini_wiki/C/Main_Page`) {
		t.Errorf("expected Solar_Cell bottom pager to link back to Main_Page, got %d", w.Code)
	}

	// 4. In-ZIM title search (/read/zim/mini_wiki/?q=solar) finds Solar Cell
	req = httptest.NewRequest(http.MethodGet, "/read/zim/mini_wiki/?q=solar", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/read/zim/mini_wiki/C/Solar_Cell") {
		t.Errorf("expected ZIM title search result linking to Solar_Cell, got %d: %s", w.Code, w.Body.String())
	}

	// 5. ?download=1 returns standalone HTML attachment without reader chrome
	req = httptest.NewRequest(http.MethodGet, "/read/zim/mini_wiki/C/Main_Page?download=1", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	disp := w.Header().Get("Content-Disposition")
	if w.Code != http.StatusOK || !strings.Contains(disp, "attachment;") || !strings.Contains(disp, ".html") {
		t.Errorf("expected HTML attachment response, got %d disp=%q", w.Code, disp)
	}
	if !strings.Contains(w.Body.String(), "Beta-lactam antibiotic.") || strings.Contains(w.Body.String(), `id="ark-proxy-nav"`) {
		t.Errorf("expected clean standalone HTML without reader nav")
	}

	// 6. Media/video HTTP Range requests (206 Partial Content) for scrubbing
	vidDir := filepath.Join(tmpDir, "media", "video")
	_ = os.MkdirAll(vidDir, 0o755)
	_ = os.WriteFile(filepath.Join(vidDir, "clip.mp4"), []byte("0123456789ABCDEF"), 0o644)
	req = httptest.NewRequest(http.MethodGet, "/media/video/clip.mp4", nil)
	req.Header.Set("Range", "bytes=4-7")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "4567" {
		t.Errorf("expected 206 Partial Content '4567' for video Range request, got %d (%q)", w.Code, w.Body.String())
	}

	// 7. Reader theme persists in SQLite across pages and restarts via /api/reader/theme
	themeDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer themeDB.Close()
	_, _ = themeDB.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
	serverWithDB, err := NewServer(themeDB, tmpDir)
	if err != nil {
		t.Fatalf("NewServer with db: %v", err)
	}
	muxWithDB := http.NewServeMux()
	if err := serverWithDB.RegisterRoutes(muxWithDB); err != nil {
		t.Fatalf("RegisterRoutes with db: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/reader/theme", strings.NewReader(`{"theme":"sepia"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	muxWithDB.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"theme":"sepia"`) {
		t.Fatalf("expected /api/reader/theme POST to save sepia, got %d: %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/read/zim/mini_wiki/C/Main_Page", nil)
	w = httptest.NewRecorder()
	muxWithDB.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `var SERVER_THEME = "sepia";`) {
		t.Errorf("expected ZIM page to embed saved SERVER_THEME sepia, got %d", w.Code)
	}
}

func TestAskAttachmentsInlineLinksAndIndexedZIMToolbar(t *testing.T) {
	tmpDir := t.TempDir()
	zimDir := filepath.Join(tmpDir, "zim")
	if err := os.MkdirAll(zimDir, 0o755); err != nil {
		t.Fatalf("mkdir zim: %v", err)
	}
	zimPath := filepath.Join(zimDir, "mini_wiki.zim")

	// Minimal valid ZIM v6 (Zstd) with Albert Einstein article
	article := []byte("<html><head><title>Albert Einstein</title></head><body><p>His mass-energy equivalence formula E = mc^2 was published in 1905 during his miracle year.</p></body></html>")
	tableSize := uint32(2 * 4)
	offs := []uint32{tableSize, tableSize + uint32(len(article))}
	var uncompressed bytes.Buffer
	for _, off := range offs {
		_ = binary.Write(&uncompressed, binary.LittleEndian, off)
	}
	uncompressed.Write(article)
	enc, _ := zstd.NewWriter(nil)
	defer enc.Close()
	var clusterBuf bytes.Buffer
	clusterBuf.WriteByte(5)
	clusterBuf.Write(enc.EncodeAll(uncompressed.Bytes(), nil))

	mimeList := []byte("text/html;raw=true\x00\x00")
	mimePos := uint64(80)
	dirStart := mimePos + uint64(len(mimeList))
	var dirBuf bytes.Buffer
	entryOff := dirStart
	_ = binary.Write(&dirBuf, binary.LittleEndian, uint16(0))
	dirBuf.WriteByte(0)
	dirBuf.WriteByte('C')
	_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0))
	_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0))
	_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0))
	dirBuf.WriteString("Albert_Einstein\x00Albert Einstein\x00")
	urlPtrPos := dirStart + uint64(dirBuf.Len())
	clusterPtrPos := urlPtrPos + 8
	clusterDataPos := clusterPtrPos + 8
	var hdr [80]byte
	binary.LittleEndian.PutUint32(hdr[0:4], ai.ZIMMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], 6)
	binary.LittleEndian.PutUint16(hdr[6:8], 3)
	binary.LittleEndian.PutUint32(hdr[24:28], 1)
	binary.LittleEndian.PutUint32(hdr[28:32], 1)
	binary.LittleEndian.PutUint64(hdr[32:40], urlPtrPos)
	binary.LittleEndian.PutUint64(hdr[40:48], urlPtrPos)
	binary.LittleEndian.PutUint64(hdr[48:56], clusterPtrPos)
	binary.LittleEndian.PutUint64(hdr[56:64], mimePos)
	binary.LittleEndian.PutUint32(hdr[64:68], 0)
	binary.LittleEndian.PutUint64(hdr[72:80], clusterDataPos+uint64(clusterBuf.Len()))
	var zimBuf bytes.Buffer
	zimBuf.Write(hdr[:])
	zimBuf.Write(mimeList)
	zimBuf.Write(dirBuf.Bytes())
	_ = binary.Write(&zimBuf, binary.LittleEndian, entryOff)
	_ = binary.Write(&zimBuf, binary.LittleEndian, clusterDataPos)
	zimBuf.Write(clusterBuf.Bytes())
	if err := os.WriteFile(zimPath, zimBuf.Bytes(), 0o644); err != nil {
		t.Fatalf("write zim: %v", err)
	}

	db, err := sql.Open("sqlite", filepath.Join(tmpDir, "ark.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	_, _ = db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)

	var capturedPrompt string
	aiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = body.ReadFrom(r.Body)
		s := body.String()
		if !strings.Contains(s, "summarizing strictly the user's query below") {
			capturedPrompt = s
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"title\":\"Mass Energy Equivalence Note\",\"answer\":\"Einstein derived $E = mc^2$.\",\"references\":[{\"id\":1,\"quote\":\"mass-energy equivalence formula\"}]}"}}]}`))
	}))
	defer aiSrv.Close()

	_, _ = db.Exec(`INSERT INTO settings (key, value) VALUES ('ai_server_url', ?), ('ai_chat_model', 'test-vision')`, aiSrv.URL)

	ctx := t.Context()
	if _, err := ai.IndexSource(ctx, db, ai.Conn{}, nil, ai.DefaultCollection, "zim", zimPath, "", nil); err != nil {
		t.Fatalf("IndexSource zim: %v", err)
	}

	server, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	if err := server.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// 1. Indexed ZIM archive must NOT render '+ Add to AI' button on individual article pages
	req := httptest.NewRequest(http.MethodGet, "/read/zim/mini_wiki/C/Albert_Einstein", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "+ Add to AI") {
		t.Errorf("expected + Add to AI button to be suppressed for already-indexed ZIM archive, got %d", w.Code)
	}

	// 2. Ask UI renders large chat window + attach button + compact classes
	req = httptest.NewRequest(http.MethodGet, "/ask", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	askHTML := w.Body.String()
	for _, want := range []string{`id="ask-attach-btn"`, `id="ask-files"`, `user-msg-compact`, `ai-msg-open`, `ai-chat-card`} {
		if !strings.Contains(askHTML, want) {
			t.Errorf("expected /ask HTML to contain %q", want)
		}
	}

	// 3. Multipart upload with temporary text + image attachment sends both to AI without storing in rag_documents
	var mpBuf bytes.Buffer
	mw := multipart.NewWriter(&mpBuf)
	_ = mw.WriteField("question", "Explain mass-energy equivalence using the attached note")
	fw1, _ := mw.CreateFormFile("files", "note.txt")
	_, _ = fw1.Write([]byte("Temporary solar note snippet"))
	fw2, _ := mw.CreateFormFile("files", "diagram.png")
	_, _ = fw2.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	_ = mw.Close()

	req = httptest.NewRequest(http.MethodPost, "/api/ai/chat", &mpBuf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from multipart /api/ai/chat, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(capturedPrompt, "Temporary solar note snippet") || !strings.Contains(capturedPrompt, "data:image/png;base64,") {
		t.Errorf("expected prompt to include temporary text & base64 image, got %s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "[1] Title: Albert Einstein") {
		t.Errorf("expected RAG prompt to provide numbered context source [1], got %s", capturedPrompt)
	}
	if !strings.Contains(w.Body.String(), "/read/zim/mini_wiki/C/Albert_Einstein?ark_hl=") {
		t.Errorf("expected /api/ai/chat JSON response to return verified ZIM ?ark_hl= citation link, got %s", w.Body.String())
	}
	var ragCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM rag_documents`).Scan(&ragCount)
	if ragCount != 1 {
		t.Errorf("expected temporary uploads NOT to persist into rag_documents (count=%d)", ragCount)
	}

	// 4. Space-containing ZIM URL resolves to underscore entry cleanly
	req = httptest.NewRequest(http.MethodGet, "/read/zim/mini_wiki/C/Albert%20Einstein", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "mass-energy equivalence formula") {
		t.Errorf("expected space-to-underscore ZIM resolution, got %d", w.Code)
	}

	// 5. parseStructuredAIResponse parses AI JSON with LaTeX backslashes, strips inline links from answer, and maps numbered references
	rawJSON := "```json\n{\n  \"title\": \"Mass Energy Equivalence Query\",\n  \"answer\": \"Einstein derived $\\frac{E}{c^2} = m$ and $\\theta + \\nabla \\cdot \\mathbf{E}$ without [inline link](https://en.wikipedia.org/wiki/Test).\",\n  \"references\": [{\"id\": 1, \"quote\": \"mass-energy equivalence formula E = mc^2 was published in 1905\"}]\n}\n```"
	cleanAns, parsedTitle, refs := server.parseStructuredAIResponse(rawJSON, "Explain mass-energy equivalence", []chatCitation{{
		Num:        1,
		Title:      "Albert Einstein (mini_wiki)",
		CleanTitle: "Albert Einstein",
		Path:       zimPath + "#Albert_Einstein",
		URL:        "/read/zim/mini_wiki/C/Albert_Einstein?ark_hl=formula%20was%20published",
		Snippet:    "formula was published",
	}})
	if parsedTitle != "Mass Energy Equivalence Query" {
		t.Errorf("unexpected parsedTitle: %q", parsedTitle)
	}
	if strings.Contains(cleanAns, "https://") || strings.Contains(cleanAns, "[inline link]") ||
		!strings.Contains(cleanAns, `\frac{E}{c^2}`) || !strings.Contains(cleanAns, `\theta`) || !strings.Contains(cleanAns, `\nabla`) {
		t.Errorf("expected cleanAns to preserve LaTeX and strip inline links, got %q", cleanAns)
	}
	if len(refs) != 1 || refs[0].Num != 1 || !strings.Contains(refs[0].URL, "/read/zim/mini_wiki/C/Albert_Einstein?ark_hl=") {
		t.Errorf("expected 1 numbered reference with verified ZIM URL, got %+v", refs)
	}

	// When AI returns references: [], no citations should be attached even if candidates were retrieved
	emptyRefJSON := `{"title":"General Greeting","answer":"Hello! How can I help?","references":[]}`
	_, _, noRefs := server.parseStructuredAIResponse(emptyRefJSON, "Hello", []chatCitation{{
		Num:        1,
		Title:      "Albert Einstein (mini_wiki)",
		CleanTitle: "Albert Einstein",
		Path:       zimPath + "#Albert_Einstein",
		URL:        "/read/zim/mini_wiki/C/Albert_Einstein",
	}})
	if len(noRefs) != 0 {
		t.Errorf("expected 0 citations when AI returns empty references, got %+v", noRefs)
	}

	// 6. Home app tiles and Apps modal Open App open in new tabs (target="_blank")
	req = httptest.NewRequest(http.MethodGet, "/apps", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "openA.target = '_blank'") {
		t.Errorf("expected /apps modal Open App button to open in a new tab")
	}

	// 7. Ask UI session controls & full session persistence lifecycle
	for _, want := range []string{`id="ask-session-select"`, `id="ask-pin-btn"`, `id="ask-del-btn"`} {
		if !strings.Contains(askHTML, want) {
			t.Errorf("expected /ask HTML to include %q", want)
		}
	}

	// Create session via /api/ai/chat
	req = httptest.NewRequest(http.MethodPost, "/api/ai/chat", strings.NewReader(`{"question":"What is Einstein formula?"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var chatOut struct {
		SessionID     string `json:"session_id"`
		SessionTitle  string `json:"session_title"`
		SessionPinned bool   `json:"session_pinned"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &chatOut)
	if chatOut.SessionID == "" || chatOut.SessionTitle == "" || len(strings.Fields(chatOut.SessionTitle)) > 4 || chatOut.SessionPinned {
		t.Fatalf("unexpected session metadata from /api/ai/chat: %+v", chatOut)
	}

	// Pin session permanently
	pinForm := url.Values{"id": {chatOut.SessionID}, "pinned": {"1"}}
	req = httptest.NewRequest(http.MethodPost, "/api/ai/sessions/pin", strings.NewReader(pinForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"pinned":true`) {
		t.Fatalf("expected pinned:true, got %d: %s", w.Code, w.Body.String())
	}

	// Fetch session history & list sessions
	req = httptest.NewRequest(http.MethodGet, "/api/ai/sessions/get?id="+url.QueryEscape(chatOut.SessionID), nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "What is Einstein formula?") {
		t.Errorf("expected saved session history messages, got %d: %s", w.Code, w.Body.String())
	}

	// Delete session
	delForm := url.Values{"id": {chatOut.SessionID}}
	req = httptest.NewRequest(http.MethodPost, "/api/ai/sessions/delete", strings.NewReader(delForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("expected delete session ok:true, got %d: %s", w.Code, w.Body.String())
	}

	// 8. Verify literal app names in /apps, Home right-click start/stop menu, and no Remove button on completed jobs
	req = httptest.NewRequest(http.MethodGet, "/apps", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	for _, wantName := range []string{"ConvertX", "Data Chef", "Drug Labels", "Flatnotes", "Kolibri", "LibreTranslate", "Maps"} {
		if !strings.Contains(w.Body.String(), wantName) {
			t.Errorf("expected /apps to render literal app name %q", wantName)
		}
	}
	for _, hiddenName := range []string{"AI Assistant", `data-id="kiwix"`} {
		if strings.Contains(w.Body.String(), hiddenName) {
			t.Errorf("expected /apps not to render removed app %q", hiddenName)
		}
	}

	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS app_states (id TEXT PRIMARY KEY, name TEXT NOT NULL, category TEXT NOT NULL, image TEXT NOT NULL DEFAULT '', container_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'stopped', port INTEGER NOT NULL DEFAULT 0, proxy_mode TEXT NOT NULL DEFAULT 'path', auto_start INTEGER NOT NULL DEFAULT 0, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	instForm := url.Values{"id": {"convertx"}}
	req = httptest.NewRequest(http.MethodPost, "/api/apps/install", strings.NewReader(instForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `id="home-app-menu"`) || !strings.Contains(w.Body.String(), `id="app-ctx-start"`) || !strings.Contains(w.Body.String(), `id="app-ctx-stop"`) {
		t.Errorf("expected Home UI to include right-click start/stop app context menu")
	}
}

func TestOfflineMapsViewerAndPMTiles(t *testing.T) {
	tmpDir := t.TempDir()
	mapsDir := filepath.Join(tmpDir, "maps")
	if err := os.MkdirAll(mapsDir, 0o755); err != nil {
		t.Fatalf("mkdir maps: %v", err)
	}

	// Build a minimal valid PMTiles v3 file (127-byte header + raw JSON metadata)
	metaJSON := []byte(`{"name":"Test Region","vector_layers":[{"id":"roads"}]}`)
	buf := make([]byte, pmtilesHeaderLen+len(metaJSON)+16)
	copy(buf[0:7], "PMTiles")
	buf[7] = 3
	binary.LittleEndian.PutUint64(buf[24:32], uint64(pmtilesHeaderLen))
	binary.LittleEndian.PutUint64(buf[32:40], uint64(len(metaJSON)))
	buf[97] = 1 // no internal compression
	buf[99] = 1 // MVT
	buf[100] = 0 // minzoom
	buf[101] = 14
	putI32 := func(off int, v int32) {
		binary.LittleEndian.PutUint32(buf[off:off+4], uint32(v))
	}
	putI32(102, -755000000) // -75.5
	putI32(106, 388000000)  // 38.8
	putI32(110, -739000000) // -73.9
	putI32(114, 413000000)  // 41.3
	buf[118] = 6
	putI32(119, -747000000)
	putI32(123, 400500000)
	copy(buf[pmtilesHeaderLen:], metaJSON)

	pmPath := filepath.Join(mapsDir, "test_nj.pmtiles")
	if err := os.WriteFile(pmPath, buf, 0o644); err != nil {
		t.Fatalf("write pmtiles: %v", err)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	srv, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	if err := srv.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// 1. Native header parsing
	meta, err := ParsePMTilesMeta(pmPath)
	if err != nil {
		t.Fatalf("ParsePMTilesMeta: %v", err)
	}
	if meta.MaxZoom != 14 || meta.TileType != "mvt" || meta.Name != "Test Region" {
		t.Errorf("unexpected parsed pmtiles meta: %+v", meta)
	}

	// 2. Range request to /maps/files/test_nj.pmtiles returns 206 Partial Content
	req := httptest.NewRequest(http.MethodGet, "/maps/files/test_nj.pmtiles", nil)
	req.Header.Set("Range", "bytes=0-6")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "PMTiles" {
		t.Errorf("expected 206 Partial Content 'PMTiles', got %d (%q)", w.Code, w.Body.String())
	}

	// Traversal guard
	req = httptest.NewRequest(http.MethodGet, "/maps/files/..%2Fsecret.pmtiles", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 on path traversal, got %d", w.Code)
	}

	// 3. Native TileJSON endpoint works without container
	req = httptest.NewRequest(http.MethodGet, "/api/maps/tilejson?name=test_nj", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"maxzoom":14`) || !strings.Contains(w.Body.String(), `/maps/files/test_nj.pmtiles`) {
		t.Errorf("expected valid TileJSON from /api/maps/tilejson, got %d: %s", w.Code, w.Body.String())
	}

	// 4. Interactive /maps page includes unified MapLibre + /maps/tiles/ source without redundant page header
	req = httptest.NewRequest(http.MethodGet, "/maps?place=test_nj", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{"maplibre-gl.js", "basemaps.js", "/maps/tiles/{z}/{x}/{y}.mvt", "test_nj.pmtiles", "Go Here"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected /maps HTML to contain %q", want)
		}
	}

	// 4b. Verify Hilbert zxyToTileID and unified tile endpoint handle missing tiles cleanly with 204
	if zxyToTileID(0, 0, 0) != 0 || zxyToTileID(1, 0, 0) != 1 || zxyToTileID(1, 1, 0) != 4 {
		t.Errorf("unexpected zxyToTileID Hilbert values")
	}
	req = httptest.NewRequest(http.MethodGet, "/maps/tiles/0/0/0.mvt", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204 No Content when synthetic archive has empty root dir, got %d", w.Code)
	}

	// 5. Missing font PBF fallback returns 200 empty protobuf instead of 404
	req = httptest.NewRequest(http.MethodGet, "/static/vendor/maps/fonts/Noto%20Sans%20Regular/12800-13055.pbf", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/x-protobuf" {
		t.Errorf("expected 200 application/x-protobuf for missing font range, got %d (%q)", w.Code, w.Header().Get("Content-Type"))
	}

	// 6. Library does NOT include .pmtiles map files in the library items grid
	req = httptest.NewRequest(http.MethodGet, "/library", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "test_nj.pmtiles") || strings.Contains(w.Body.String(), "/maps?place=test_nj") {
		t.Errorf("expected /library not to include .pmtiles map files, got %d", w.Code)
	}

	// 7. Pins API supports JSON mode without HTML redirect
	pinForm := url.Values{"title": {"Trailhead"}, "lat": {"40.1"}, "lng": {"-74.5"}, "category": {"waypoint"}, "format": {"json"}}
	req = httptest.NewRequest(http.MethodPost, "/api/maps/pins", strings.NewReader(pinForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"Trailhead"`) {
		t.Errorf("expected 200 JSON pin response, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAskAIFederatedFeaturesAndStreaming(t *testing.T) {
	// Reset auto-index cooldown so this test runs syncAutoIndexedDirs deterministically
	autoIdxMu.Lock()
	autoIdxLast = time.Time{}
	autoIdxMu.Unlock()

	// Mock Ollama + OpenAI server reporting /api/ps context_length=16384 and streaming JSON answer
	aiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[{"name":"test-llm","model":"test-llm","context_length":16384}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			b1, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": `{"title":"Burn First Aid","answer":"Cool burn gently`}}}})
			b2, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": ` and cover.","references":[{"id":1,"quote":"Aloe"}],"map_actions":[{"title":"Clinic","lat":40.1,"lng":-74.5}]}`}}}})
			_, _ = w.Write([]byte("data: " + string(b1) + "\n\ndata: " + string(b2) + "\n\ndata: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer aiSrv.Close()

	tmpDir := t.TempDir()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	_, _ = db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
	_, _ = db.Exec(`INSERT INTO settings (key, value) VALUES ('ai_server_url', ?), ('ai_chat_model', 'test-llm')`, aiSrv.URL)

	// Create sample note (#4), sample ZIM (#1, #5, #6), and sample druglabels.db (#3)
	notesDir := filepath.Join(tmpDir, "notes")
	_ = os.MkdirAll(notesDir, 0o755)
	_ = os.WriteFile(filepath.Join(notesDir, "camp_checklist.md"), []byte("# Camp Water\nCarry two Sawyer squeeze filters and backup Aquamira drops."), 0o644)

	dlDir := filepath.Join(tmpDir, "druglabels")
	_ = os.MkdirAll(dlDir, 0o755)
	dlDB, err := sql.Open("sqlite", filepath.Join(dlDir, "druglabels.db"))
	if err != nil {
		t.Fatalf("open druglabels: %v", err)
	}
	_, _ = dlDB.Exec(`
		CREATE TABLE labels (set_id TEXT PRIMARY KEY, brand_name TEXT, generic_name TEXT, manufacturer TEXT, ndc TEXT, imprint TEXT, boxed_warning TEXT, indications TEXT, dosage TEXT, warnings TEXT);
		CREATE VIRTUAL TABLE labels_fts USING fts5(set_id UNINDEXED, brand_name, generic_name, ndc, imprint, indications, warnings, content='labels', content_rowid='rowid');
		INSERT INTO labels (set_id, brand_name, generic_name, manufacturer, ndc, imprint, boxed_warning, indications, dosage, warnings)
		VALUES ('set-1', 'Advil', 'Ibuprofen', 'Haleon', '0573-0164', 'I-2', '', 'Pain reliever and fever reducer.', '200 mg every 4 to 6 hours', 'Stomach bleeding warning.');
		INSERT INTO labels_fts(labels_fts) VALUES('rebuild');
	`)
	_ = dlDB.Close()

	zimDir := filepath.Join(tmpDir, "zim")
	_ = os.MkdirAll(zimDir, 0o755)
	zimPath := filepath.Join(zimDir, "mini_wiki.zim")
	{
		art := []byte("<html><head><title>Einstein</title></head><body><h1>Einstein</h1><p>Speed of light in vacuum is 299792458 m/s.</p></body></html>")
		var unc bytes.Buffer
		_ = binary.Write(&unc, binary.LittleEndian, uint32(8))
		_ = binary.Write(&unc, binary.LittleEndian, uint32(8+len(art)))
		unc.Write(art)
		enc, _ := zstd.NewWriter(nil)
		var cb bytes.Buffer
		cb.WriteByte(5)
		cb.Write(enc.EncodeAll(unc.Bytes(), nil))
		_ = enc.Close()

		mimeList := []byte("text/html;raw=true\x00\x00")
		mimePos := uint64(80)
		dirStart := mimePos + uint64(len(mimeList))
		var dir bytes.Buffer
		_ = binary.Write(&dir, binary.LittleEndian, uint16(0))
		dir.WriteByte(0)
		dir.WriteByte('C')
		_ = binary.Write(&dir, binary.LittleEndian, uint32(0))
		_ = binary.Write(&dir, binary.LittleEndian, uint32(0))
		_ = binary.Write(&dir, binary.LittleEndian, uint32(0))
		dir.WriteString("Main_Page\x00Einstein\x00")
		urlPtrPos := dirStart + uint64(dir.Len())
		clusterPtrPos := urlPtrPos + 8
		clusterDataPos := clusterPtrPos + 8
		var zb bytes.Buffer
		var hdr [80]byte
		binary.LittleEndian.PutUint32(hdr[0:4], ai.ZIMMagic)
		binary.LittleEndian.PutUint16(hdr[4:6], 6)
		binary.LittleEndian.PutUint16(hdr[6:8], 3)
		binary.LittleEndian.PutUint32(hdr[24:28], 1)
		binary.LittleEndian.PutUint32(hdr[28:32], 1)
		binary.LittleEndian.PutUint64(hdr[32:40], urlPtrPos)
		binary.LittleEndian.PutUint64(hdr[40:48], urlPtrPos)
		binary.LittleEndian.PutUint64(hdr[48:56], clusterPtrPos)
		binary.LittleEndian.PutUint64(hdr[56:64], mimePos)
		binary.LittleEndian.PutUint32(hdr[64:68], 0)
		binary.LittleEndian.PutUint64(hdr[72:80], clusterDataPos+uint64(cb.Len()))
		zb.Write(hdr[:])
		zb.Write(mimeList)
		zb.Write(dir.Bytes())
		_ = binary.Write(&zb, binary.LittleEndian, dirStart)
		_ = binary.Write(&zb, binary.LittleEndian, clusterDataPos)
		zb.Write(cb.Bytes())
		if err := os.WriteFile(zimPath, zb.Bytes(), 0o644); err != nil {
			t.Fatalf("write mini zim: %v", err)
		}
	}

	srv, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.ensureMapMarkersTable()
	_, _ = db.Exec(`INSERT INTO map_markers (id, title, lat, lng, category, note) VALUES ('p1', 'Spring Creek', 40.10, -74.50, 'water', 'Fresh spring')`)

	mux := http.NewServeMux()
	if err := srv.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// #1 On-the-fly ZIM lookup without prior indexing
	zimHits := srv.searchLiveZIMs("Einstein light speed", 2)
	if len(zimHits) == 0 || !strings.Contains(zimHits[0].URL, "/read/zim/mini_wiki/") {
		t.Errorf("expected live ZIM hit for Einstein, got %+v", zimHits)
	}

	// #2 Remedy guide lookup
	remHits := searchRemedyGuide("burn aloe", 2)
	if len(remHits) == 0 || !strings.Contains(remHits[0].URL, "/field-guide?") {
		t.Errorf("expected Remedy guide hit, got %+v", remHits)
	}

	// #3 Drug label & pill imprint lookup
	dlHits := srv.searchDrugLabels(t.Context(), "Advil I-2", 2)
	if len(dlHits) == 0 || !strings.Contains(dlHits[0].Text, "Ibuprofen") {
		t.Errorf("expected DrugLabel hit for Ibuprofen, got %+v", dlHits)
	}

	// #4 Auto-indexed note synced into rag_documents on /ask load
	req := httptest.NewRequest(http.MethodGet, "/ask?zim=mini_wiki&article=C/Main_Page", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	askHTML := w.Body.String()
	for _, want := range []string{`id="ark-ctx-gauge"`, `id="ark-ctx-ratio"`, `0/16384`, `id="ask-sources-only"`, `id="ask-protocol-mode"`, `id="ask-triage-bar"`, `ask-scope-pill`, `camp_checklist.md`} {
		if !strings.Contains(askHTML, want) {
			t.Errorf("expected /ask HTML to contain %q", want)
		}
	}

	// #5 & #6 ZIM reader includes #ark-ask-link and selection popover (Explain/Summarize/Translate -> :5000)
	req = httptest.NewRequest(http.MethodGet, "/read/zim/mini_wiki/C/Main_Page", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	zimHTML := w.Body.String()
	for _, want := range []string{`id="ark-ask-link"`, `ark-sel-popover`, `Explain`, `Summarize`, `Translate`, `:5000/?source=auto&target=en`} {
		if !strings.Contains(zimHTML, want) {
			t.Errorf("expected ZIM page HTML to contain %q", want)
		}
	}

	// #7 Map Haversine context & action validation
	mapCtx := srv.queryMapWaypointsContext("nearest water source to 40.12, -74.51")
	if !strings.Contains(mapCtx, "Spring Creek") || !strings.Contains(mapCtx, "km") {
		t.Errorf("expected Haversine distance in map context, got %q", mapCtx)
	}
	actions := validateMapActions([]chatMapAction{{Title: "Camp", Lat: 40.1, Lng: -74.5}})
	if len(actions) != 1 || !strings.Contains(actions[0].MapURL, "/maps?lat=40.100000&lng=-74.500000") {
		t.Errorf("unexpected validated map action: %+v", actions)
	}

	// #8 Strict Sources Only short-circuits when scope='attached' has no sources
	strictBody := `{"question":"Unknown query","scope":"attached","sources_only":true,"stream":false}`
	req = httptest.NewRequest(http.MethodPost, "/api/ai/chat", strings.NewReader(strictBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "No verified local source found") {
		t.Errorf("expected strict sources_only short-circuit, got %d: %s", w.Code, w.Body.String())
	}

	// #10 Deterministic offline calculator
	calcOut := evalAskMathTool("Give 15 mg/kg for 70 kg; also check 100Ah 12V battery at 50W and 146.52 MHz")
	if !strings.Contains(calcOut, "1050.00 mg") || !strings.Contains(calcOut, "1200.00 Wh") || !strings.Contains(calcOut, "24.00 hours") || !strings.Contains(calcOut, "MHz") {
		t.Errorf("expected exact calculations in evalAskMathTool, got %q", calcOut)
	}

	// #12 jsonAnswerStreamer extracts unescaped answer tokens cleanly while streaming JSON
	var st jsonAnswerStreamer
	part1 := st.feed(`{"title":"Test","answer":"Line 1\n\"quoted`)
	part2 := st.feed(`\" end","references":[]}`)
	if part1+part2 != "Line 1\n\"quoted\" end" {
		t.Errorf("unexpected streamed JSON answer decode: %q", part1+part2)
	}

	// #12 + #13 + #9 + #11 End-to-end SSE streaming chat with scope="remedy", protocol_mode, triage, and context_max=16384
	sseReqBody := `{"question":"burn care aloe","scope":"remedy","protocol_mode":true,"triage_type":"symptom","stream":true}`
	req = httptest.NewRequest(http.MethodPost, "/api/ai/chat", strings.NewReader(sseReqBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	sseOut := w.Body.String()
	for _, want := range []string{"event: token", "Cool burn gently", "event: done", `"context_max":16384`, `/maps?lat=40.100000`, `/field-guide?condition=burns`} {
		if !strings.Contains(sseOut, want) {
			t.Errorf("expected SSE stream to contain %q, got:\n%s", want, sseOut)
		}
	}
}

func TestAIModelStatusAndContextSetting(t *testing.T) {
	modelLoaded := false
	lmMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v0/models" {
			state := "not-loaded"
			loadedCtx := 0
			if modelLoaded {
				state = "loaded"
				loadedCtx = 32768
			}
			_, _ = fmt.Fprintf(w, `{"object":"list","data":[{"id":"gemma-vlm","type":"vlm","arch":"gemma3","quantization":"Q4_K_M","state":%q,"max_context_length":131072,"loaded_context_length":%d,"capabilities":["tool_use"]}]}`, state, loadedCtx)
			return
		}
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"gemma-vlm"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer lmMock.Close()

	_, _, mux, _ := func() (*Server, *sql.DB, *http.ServeMux, func()) {
		tmpDir := t.TempDir()
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		_, _ = db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
		srv, err := NewServer(db, tmpDir)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		m := http.NewServeMux()
		if err := srv.RegisterRoutes(m); err != nil {
			t.Fatalf("RegisterRoutes: %v", err)
		}
		return srv, db, m, func() {}
	}()

	// Save LM Studio URL + custom ai_context_length=16384 via /settings/ai
	form := url.Values{
		"ai_server_url":     {lmMock.URL},
		"ai_chat_model":     {"gemma-vlm"},
		"ai_context_length": {"16384"},
		"ai_embed_model":    {""},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/ai", strings.NewReader(form.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect from POST /settings/ai, got %d", w.Code)
	}

	// Unloaded state: /api/ai/status reports model_loaded: false; /ask hides #ark-ctx-gauge and shows No Model Loaded
	req = httptest.NewRequest(http.MethodGet, "/api/ai/status", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /api/ai/status, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"server_online":true`, `"model_loaded":false`, `"context_length":16384`, `"max_context_length":131072`} {
		if !strings.Contains(body, want) {
			t.Errorf("unloaded status missing %s: %s", want, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/ask", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	askUnloaded := w.Body.String()
	if !strings.Contains(askUnloaded, `badge badge-warning`) || !strings.Contains(askUnloaded, `>gemma-vlm<`) {
		t.Errorf("expected /ask to display yellow badge-warning with gemma-vlm when unloaded")
	}
	if !strings.Contains(askUnloaded, `id="ark-ctx-gauge"`) || !strings.Contains(askUnloaded, `display:none`) {
		t.Errorf("expected #ark-ctx-gauge to be hidden (display:none) when no model is loaded")
	}

	// Loaded state: /api/ai/status reports model_loaded: true, supports_vision: true; /ask shows green badge-success & gauge
	modelLoaded = true
	req = httptest.NewRequest(http.MethodGet, "/api/ai/status", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body = w.Body.String()
	for _, want := range []string{`"model_loaded":true`, `"loaded_model":"gemma-vlm"`, `"supports_vision":true`, `"supports_tools":true`, `"context_length":16384`} {
		if !strings.Contains(body, want) {
			t.Errorf("loaded status missing %s: %s", want, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/ask", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	askLoaded := w.Body.String()
	if !strings.Contains(askLoaded, `badge badge-success`) || !strings.Contains(askLoaded, `>gemma-vlm<`) {
		t.Errorf("expected /ask to display green badge-success with gemma-vlm when loaded")
	}
	if !strings.Contains(askLoaded, `id="ark-ctx-gauge"`) || !strings.Contains(askLoaded, `display:inline-flex`) {
		t.Errorf("expected #ark-ctx-gauge to be visible (display:inline-flex) when model is loaded")
	}

	// Offline/unreachable server: /ask shows red badge-error "No Model Available" and hides gauge
	lmMock.Close()
	req = httptest.NewRequest(http.MethodGet, "/ask", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	askOffline := w.Body.String()
	if !strings.Contains(askOffline, `badge badge-error`) || !strings.Contains(askOffline, "No Model Available") {
		t.Errorf("expected /ask to display red badge-error No Model Available when server offline")
	}
}

func TestSettingsChatModelDropdownAndSystemClock(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	_, _ = db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
	srv, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	if err := srv.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// 1. /settings renders <select id="ai_chat_model" ...> dropdown
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `<select id="ai_chat_model" name="ai_chat_model"`) {
		t.Errorf("expected /settings to render #ai_chat_model as a <select> dropdown")
	}

	// 2. /system renders #system-clock
	req = httptest.NewRequest(http.MethodGet, "/system", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /system status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `id="system-clock"`) {
		t.Errorf("expected /system to render #system-clock element")
	}

	// 3. /api/system/time returns source=device when offline and source=online when probe succeeds
	origProbe := onlineTimeProbeURL
	defer func() { onlineTimeProbeURL = origProbe }()

	onlineTimeProbeURL = ""
	req = httptest.NewRequest(http.MethodGet, "/api/system/time", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"source":"device"`) {
		t.Errorf("expected offline /api/system/time source=device, got %d: %s", w.Code, w.Body.String())
	}

	timeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Date(2026, 11, 19, 21, 55, 0, 0, time.UTC).Format(http.TimeFormat))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer timeSrv.Close()

	onlineTimeProbeURL = timeSrv.URL
	req = httptest.NewRequest(http.MethodGet, "/api/system/time", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"source":"online"`) {
		t.Errorf("expected online /api/system/time source=online, got %d: %s", w.Code, w.Body.String())
	}
}

func TestStorageSpacePrecheckAndBlocking(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	_, _ = db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
	_, _ = db.Exec(`CREATE TABLE jobs (id TEXT PRIMARY KEY, type TEXT, target TEXT, status TEXT, progress REAL, progress_msg TEXT, metadata TEXT, error TEXT, created_at DATETIME, updated_at DATETIME)`)

	srv, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	if err := srv.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// 1. Check /api/storage/check-space on starter_pack and map_extract
	req := httptest.NewRequest(http.MethodGet, "/api/storage/check-space?type=preset&target=starter_pack", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"known_size":true`) {
		t.Fatalf("expected known_size:true from /api/storage/check-space, got %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/storage/check-space?type=map_extract&target=united-states", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"known_size":true`) {
		t.Fatalf("expected known_size:true for united-states map extract, got %d: %s", w.Code, w.Body.String())
	}

	// 2. Oversized download is blocked cleanly by POST /api/jobs (JSON 507 + form redirect ?err=)
	form := url.Values{
		"type":       {"kiwix_zim"},
		"target":     {"https://example.invalid/huge.zim"},
		"size_bytes": {"99999999999999999"},
	}
	req = httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(form.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusInsufficientStorage || !strings.Contains(w.Body.String(), `"status":"insufficient"`) {
		t.Errorf("expected HTTP 507 insufficient for oversized download, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAskMultiTurnContextAndBottomSpacer(t *testing.T) {
	var lastChatReqBody string
	aiMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v0/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"test-llm","type":"llm","state":"loaded","max_context_length":16384,"loaded_context_length":16384}]}`))
		case "/v1/chat/completions":
			var b bytes.Buffer
			_, _ = b.ReadFrom(r.Body)
			lastChatReqBody = b.String()
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"title\":\"Camp Water\",\"answer\":\"Use Sawyer squeeze first.\",\"references\":[]}"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer aiMock.Close()

	tmpDir := t.TempDir()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	_, _ = db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
	_, _ = db.Exec(`INSERT INTO settings (key, value) VALUES ('ai_server_url', ?), ('ai_chat_model', 'test-llm')`, aiMock.URL)

	srv, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	if err := srv.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// Turn 1
	req1 := httptest.NewRequest(http.MethodPost, "/api/ai/chat", strings.NewReader(`{"question":"What filter is in my camp checklist?"}`))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("turn 1 status = %d: %s", w1.Code, w1.Body.String())
	}
	sessID := w1.Header().Get("X-Ark-Session-Id")
	if sessID == "" {
		t.Fatalf("expected X-Ark-Session-Id header on turn 1")
	}

	// Turn 2 using session_id -> must include Turn 1 question & answer in multi-turn context
	req2 := httptest.NewRequest(http.MethodPost, "/api/ai/chat", strings.NewReader(fmt.Sprintf(`{"question":"How do I backflush it?","session_id":%q}`, sessID)))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("turn 2 status = %d: %s", w2.Code, w2.Body.String())
	}
	for _, want := range []string{"Prior conversation history in this chat:", "What filter is in my camp checklist?", "Use Sawyer squeeze first."} {
		if !strings.Contains(lastChatReqBody, want) {
			t.Errorf("turn 2 LLM request missing prior context %q; got:\n%s", want, lastChatReqBody)
		}
	}

	// Also verify bottom whitespace CSS rules on #chat-messages
	cssReq := httptest.NewRequest(http.MethodGet, "/static/ark.css", nil)
	cssW := httptest.NewRecorder()
	mux.ServeHTTP(cssW, cssReq)
	if !strings.Contains(cssW.Body.String(), "#chat-messages::after") {
		t.Errorf("expected /static/ark.css to include #chat-messages::after bottom spacer")
	}
}

func TestSystemRuntimeZIMSearchAndAjaxDownloads(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	_, _ = db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
	_, _ = db.Exec(`CREATE TABLE jobs (id TEXT PRIMARY KEY, type TEXT, target TEXT, status TEXT, progress REAL, progress_msg TEXT, metadata TEXT, error TEXT, created_at DATETIME, updated_at DATETIME)`)

	srv, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	if err := srv.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// 1. /api/system/runtime returns Native Mode JSON cleanly when no container engine attached
	req := httptest.NewRequest(http.MethodGet, "/api/system/runtime", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"label":"Native Mode"`) {
		t.Errorf("expected Native Mode runtime JSON, got %d: %s", w.Code, w.Body.String())
	}

	// 2. /system omits removed "Live Host Telemetry" and "Live (2s SSE)" text
	req = httptest.NewRequest(http.MethodGet, "/system", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	sysBody := w.Body.String()
	for _, removed := range []string{"Live Host Telemetry", "Live (2s SSE)"} {
		if strings.Contains(sysBody, removed) {
			t.Errorf("expected /system not to contain %q", removed)
		}
	}

	// 3. POST /api/jobs with Accept: application/json returns JSON {ok:true} without redirect
	form := url.Values{
		"type":       {"doc"},
		"target":     {"https://example.com/guide.pdf"},
		"size_bytes": {"1024"},
		"ajax":       {"1"},
	}
	req = httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(form.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("expected JSON 200 ok:true for AJAX download, got %d: %s", w.Code, w.Body.String())
	}

	// 4. GET /api/zim/suggest responds cleanly
	req = httptest.NewRequest(http.MethodGet, "/api/zim/suggest?stem=missing&q=water", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"hits":[]`) {
		t.Errorf("expected JSON hits:[] on /api/zim/suggest, got %d: %s", w.Code, w.Body.String())
	}
}

func writeWikiTestZIM(t *testing.T, zimPath string) {
	t.Helper()
	type testEntry struct {
		url        string
		title      string
		html       string
		isRedirect bool
		targetIdx  uint32
	}
	entries := []testEntry{
		{url: "Main_Page", title: "Main Page", html: "<html><body><h1>Welcome</h1></body></html>"},
		{url: "United_States", title: "United States", html: "<html><body><h1>United States</h1><p>North American country.</p></body></html>"},
	}
	for i := 1; i <= 15; i++ {
		entries = append(entries, testEntry{
			url:   fmt.Sprintf("Water_compound_%02d", i),
			title: fmt.Sprintf("Water compound %02d", i),
			html:  fmt.Sprintf("<html><body><h1>Water compound %02d</h1></body></html>", i),
		})
	}
	// Exact match appears AFTER 15 prefix entries; Redirect USA, meta-refresh stub !Bang, and special-char URLs appear at end.
	entries = append(entries,
		testEntry{url: "Water", title: "Water", html: "<html><body><h1>Water</h1><p>H2O molecule.</p></body></html>"},
		testEntry{url: "USA", title: "USA", isRedirect: true, targetIdx: 1},
		testEntry{url: "!Bang", title: "!Bang", html: `<html><head><title>!Bang</title><meta http-equiv="refresh" content="0;URL='./United_States#Bangs'" /></head><body><a href="./United_States#Bangs">!Bang</a></body></html>`},
		testEntry{url: "5%_Nation", title: "5% Nation", html: "<html><body><h1>5% Nation</h1></body></html>"},
		testEntry{url: "What_Is_to_Be_Done?", title: "What Is to Be Done?", html: "<html><body><h1>What Is to Be Done?</h1></body></html>"},
		testEntry{url: "C#_(programming_language)", title: "C# (programming language)", html: "<html><body><h1>C#</h1></body></html>"},
	)

	var blobData bytes.Buffer
	var offs []uint32
	blobNum := uint32(0)
	entryBlobIdx := make([]uint32, len(entries))
	for i, e := range entries {
		if e.isRedirect {
			continue
		}
		entryBlobIdx[i] = blobNum
		offs = append(offs, uint32((len(entries)-2+1)*4)) // patched below
		_ = offs
		blobNum++
	}
	offs = make([]uint32, 0, blobNum+1)
	headerBytes := (blobNum + 1) * 4
	curBlobOff := headerBytes
	for _, e := range entries {
		if e.isRedirect {
			continue
		}
		offs = append(offs, uint32(curBlobOff))
		b := []byte(e.html)
		blobData.Write(b)
		curBlobOff += uint32(len(b))
	}
	offs = append(offs, uint32(curBlobOff))

	var uncompressed bytes.Buffer
	for _, off := range offs {
		_ = binary.Write(&uncompressed, binary.LittleEndian, off)
	}
	uncompressed.Write(blobData.Bytes())

	enc, _ := zstd.NewWriter(nil)
	defer enc.Close()
	var clusterBuf bytes.Buffer
	clusterBuf.WriteByte(5)
	clusterBuf.Write(enc.EncodeAll(uncompressed.Bytes(), nil))

	mimeList := []byte("text/html;raw=true\x00\x00")
	mimePos := uint64(80)
	dirStart := mimePos + uint64(len(mimeList))
	var dirBuf bytes.Buffer
	entryOffs := make([]uint64, len(entries))
	for i, e := range entries {
		entryOffs[i] = dirStart + uint64(dirBuf.Len())
		if e.isRedirect {
			_ = binary.Write(&dirBuf, binary.LittleEndian, uint16(0xffff))
			dirBuf.WriteByte(0)
			dirBuf.WriteByte('C')
			_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0))
			_ = binary.Write(&dirBuf, binary.LittleEndian, e.targetIdx)
		} else {
			_ = binary.Write(&dirBuf, binary.LittleEndian, uint16(0))
			dirBuf.WriteByte(0)
			dirBuf.WriteByte('C')
			_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0))
			_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0))
			_ = binary.Write(&dirBuf, binary.LittleEndian, entryBlobIdx[i])
		}
		dirBuf.WriteString(e.url + "\x00")
		dirBuf.WriteString(e.title + "\x00")
	}

	urlPtrPos := dirStart + uint64(dirBuf.Len())
	clusterPtrPos := urlPtrPos + uint64(len(entries)*8)
	clusterDataPos := clusterPtrPos + 8

	// Sort URLPtrPos indices by URL (as required by ZIM spec) while keeping physical entry offsets in insertion order.
	sortedIdxs := make([]int, len(entries))
	for i := range sortedIdxs {
		sortedIdxs[i] = i
	}
	sort.Slice(sortedIdxs, func(i, j int) bool {
		return entries[sortedIdxs[i]].url < entries[sortedIdxs[j]].url
	})
	mainPageSortedIdx := uint32(0)
	targetSortedIdx := uint32(0)
	for si, origIdx := range sortedIdxs {
		if origIdx == 0 {
			mainPageSortedIdx = uint32(si)
		}
		if origIdx == 1 {
			targetSortedIdx = uint32(si)
		}
	}
	// Patch redirect targetIdx to point to sorted index of United_States
	dirBuf.Reset()
	for i, e := range entries {
		entryOffs[i] = dirStart + uint64(dirBuf.Len())
		if e.isRedirect {
			_ = binary.Write(&dirBuf, binary.LittleEndian, uint16(0xffff))
			dirBuf.WriteByte(0)
			dirBuf.WriteByte('C')
			_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0))
			_ = binary.Write(&dirBuf, binary.LittleEndian, targetSortedIdx)
		} else {
			_ = binary.Write(&dirBuf, binary.LittleEndian, uint16(0))
			dirBuf.WriteByte(0)
			dirBuf.WriteByte('C')
			_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0))
			_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0))
			_ = binary.Write(&dirBuf, binary.LittleEndian, entryBlobIdx[i])
		}
		dirBuf.WriteString(e.url + "\x00")
		dirBuf.WriteString(e.title + "\x00")
	}

	var zimBuf bytes.Buffer
	var hdr [80]byte
	binary.LittleEndian.PutUint32(hdr[0:4], ai.ZIMMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], 6)
	binary.LittleEndian.PutUint16(hdr[6:8], 3)
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(len(entries)))
	binary.LittleEndian.PutUint32(hdr[28:32], 1)
	binary.LittleEndian.PutUint64(hdr[32:40], urlPtrPos)
	binary.LittleEndian.PutUint64(hdr[40:48], ^uint64(0)) // v6.3 style: no TitlePtrPos
	binary.LittleEndian.PutUint64(hdr[48:56], clusterPtrPos)
	binary.LittleEndian.PutUint64(hdr[56:64], mimePos)
	binary.LittleEndian.PutUint32(hdr[64:68], mainPageSortedIdx)
	binary.LittleEndian.PutUint64(hdr[72:80], clusterDataPos+uint64(clusterBuf.Len()))
	zimBuf.Write(hdr[:])
	zimBuf.Write(mimeList)
	zimBuf.Write(dirBuf.Bytes())
	for _, origIdx := range sortedIdxs {
		_ = binary.Write(&zimBuf, binary.LittleEndian, entryOffs[origIdx])
	}
	_ = binary.Write(&zimBuf, binary.LittleEndian, clusterDataPos)
	zimBuf.Write(clusterBuf.Bytes())

	if err := os.WriteFile(zimPath, zimBuf.Bytes(), 0o644); err != nil {
		t.Fatalf("write zim: %v", err)
	}
}

func TestZIMSearchRedirectsPaginationAndTOC(t *testing.T) {
	tmpDir := t.TempDir()
	zimDir := filepath.Join(tmpDir, "zim")
	if err := os.MkdirAll(zimDir, 0o755); err != nil {
		t.Fatalf("mkdir zim: %v", err)
	}
	zimPath := filepath.Join(zimDir, "wiki_test.zim")
	writeWikiTestZIM(t, zimPath)

	arc, err := getZIMArchive(zimPath, "wiki_test")
	if err != nil {
		t.Fatalf("getZIMArchive: %v", err)
	}

	// Exact title "Water" placed after 15 prefix entries must rank first even with limit=5
	if hits := arc.searchTitles("water", 5); len(hits) == 0 || hits[0].title != "Water" {
		t.Errorf("searchTitles(\"water\", 5) expected exact \"Water\" first, got %+v", hits)
	}
	if hits := arc.scanTitlesStreaming("water", []string{"water"}, 5); len(hits) == 0 || hits[0].title != "Water" {
		t.Errorf("scanTitlesStreaming(\"water\", 5) expected exact \"Water\" first, got %+v", hits)
	}
	// Redirect entry USA (mimeID == 0xffff) must also be searchable
	if hits := arc.searchTitles("usa", 10); len(hits) == 0 || hits[0].url != "USA" {
		t.Errorf("searchTitles(\"usa\") expected redirect \"USA\", got %+v", hits)
	}
	if hits := arc.scanTitlesStreaming("usa", []string{"usa"}, 10); len(hits) == 0 || hits[0].url != "USA" {
		t.Errorf("scanTitlesStreaming(\"usa\") expected redirect \"USA\", got %+v", hits)
	}

	srv, err := NewServer(nil, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	if err := srv.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// /api/zim/pages pagination + filter (must exclude binary redirects like USA and meta-refresh stubs like !Bang)
	req := httptest.NewRequest(http.MethodGet, "/api/zim/pages?stem=wiki_test&offset=0&limit=50", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/zim/pages status %d: %s", w.Code, w.Body.String())
	}
	var pgResp struct {
		Pages      []struct{ Title, URL string } `json:"pages"`
		Total      int                           `json:"total"`
		NextOffset int                           `json:"next_offset"`
		HasMore    bool                          `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pgResp); err != nil {
		t.Fatalf("decode /api/zim/pages: %v", err)
	}
	if pgResp.Total != 21 || len(pgResp.Pages) != 21 {
		t.Errorf("expected 21 real visitable pages in /api/zim/pages (excluding USA and !Bang), got total=%d len=%d", pgResp.Total, len(pgResp.Pages))
	}
	for _, p := range pgResp.Pages {
		if p.Title == "!Bang" || p.Title == "USA" {
			t.Errorf("expected redirect/stub %q to be excluded from /api/zim/pages", p.Title)
		}
		// Every single URL returned in /api/zim/pages must resolve to HTTP 200 OK (including %, ?, # titles)
		pReq := httptest.NewRequest(http.MethodGet, p.URL, nil)
		pW := httptest.NewRecorder()
		mux.ServeHTTP(pW, pReq)
		if pW.Code != http.StatusOK {
			t.Errorf("TOC page %q URL %q returned status %d instead of 200", p.Title, p.URL, pW.Code)
		}
	}
	// Also verify unescaped trailing '?' fallback resolves cleanly
	qReq := httptest.NewRequest(http.MethodGet, "/read/zim/wiki_test/C/What_Is_to_Be_Done?", nil)
	qW := httptest.NewRecorder()
	mux.ServeHTTP(qW, qReq)
	if qW.Code != http.StatusOK {
		t.Errorf("expected unescaped trailing '?' article URL to resolve 200 OK, got %d", qW.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/zim/pages?stem=wiki_test&q=united", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "United States") {
		t.Errorf("expected filtered /api/zim/pages to include United States, got %d: %s", w.Code, w.Body.String())
	}

	// Rendered article has collapsible left TOC panel defaulted collapsed with per-item localStorage key
	req = httptest.NewRequest(http.MethodGet, "/read/zim/wiki_test/C/Main_Page", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{`id="ark-toc-toggle"`, `id="ark-toc-panel"`, `data-stem="wiki_test"`, `ark_zim_toc_open_`, `translateX(-100%)`} {
		if w.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("expected ZIM page HTML to contain %q, status=%d", want, w.Code)
		}
	}
}

func TestRealWikipediaZIMSearchIfPresent(t *testing.T) {
	wikiPath := filepath.Join("..", "..", "data", "zim", "wikipedia_en_top_mini_2026-06.zim")
	if _, err := os.Stat(wikiPath); err != nil {
		t.Skipf("wikipedia mini zim not present locally: %v", err)
	}
	arc, err := getZIMArchive(wikiPath, "wikipedia_en_top_mini_2026-06")
	if err != nil {
		t.Fatalf("open wikipedia zim: %v", err)
	}
	for _, q := range []string{"water", "albert einstein", "usa"} {
		hits := arc.searchTitles(q, 10)
		if len(hits) == 0 {
			t.Errorf("expected hits for %q in %s, got 0", q, wikiPath)
		}
	}
	pages, total, hasMore := arc.listPages(0, 20, "")
	if len(pages) != 20 || total < 1000 || !hasMore {
		t.Errorf("unexpected wikipedia listPages(0,20): len=%d total=%d hasMore=%v", len(pages), total, hasMore)
	}
	zimGlob, _ := filepath.Glob(filepath.Join("..", "..", "data", "zim", "*.zim"))
	for _, zp := range zimGlob {
		stem := strings.TrimSuffix(filepath.Base(zp), ".zim")
		a, aerr := getZIMArchive(zp, stem)
		if aerr != nil {
			t.Errorf("getZIMArchive(%s): %v", stem, aerr)
			continue
		}
		pgs, tot, hm := a.listPages(0, 15, "")
		if len(pgs) == 0 || tot == 0 {
			t.Errorf("ZIM %s returned 0 pages from listPages (count=%d, hasMore=%v)", stem, a.hdr.ArticleCount, hm)
		}
		var sample []string
		for i := 0; i < len(pgs) && i < 8; i++ {
			sample = append(sample, fmt.Sprintf("%q (%s)", pgs[i].title, pgs[i].url))
		}
		t.Logf("ZIM %-34s totalPages=%-6d sample=%v", stem, tot, sample)
		if strings.Contains(stem, "wikipedia") {
			for _, p := range pgs {
				if p.title == "!Bang" || p.title == "%CONFIG%" || p.title == "%APPEND%" || strings.Contains(p.title, "\"Crazy\"") {
					t.Errorf("unexpected redirect stub in wikipedia TOC pages: %+v", p)
				}
			}
		}
	}
}

func TestAskAIQueryNeedsLocalKnowledge(t *testing.T) {
	noToolCases := []string{"hi", "Hello!", "thanks!", "My name is Gabe", "tell me a joke", "what is 25 * 4?", "how are you?"}
	for _, q := range noToolCases {
		hasCalc := evalAskMathTool(q) != ""
		if queryNeedsLocalKnowledge(q, "all", "", "", false, hasCalc) {
			t.Errorf("expected queryNeedsLocalKnowledge(%q) == false", q)
		}
	}
	needToolCases := []string{"water purification", "dosage of ibuprofen", "search wikipedia for einstein", "cite local notes on welding"}
	for _, q := range needToolCases {
		hasCalc := evalAskMathTool(q) != ""
		if !queryNeedsLocalKnowledge(q, "all", "", "", false, hasCalc) {
			t.Errorf("expected queryNeedsLocalKnowledge(%q) == true", q)
		}
	}
}

func TestFeaturesLANDropOPDSAndCalculators(t *testing.T) {
	tmpDir := t.TempDir()
	for _, sub := range []string{"zim", "media", "docs"} {
		if err := os.MkdirAll(filepath.Join(tmpDir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	writeWikiTestZIM(t, filepath.Join(tmpDir, "zim", "wiki_test.zim"))
	if err := os.WriteFile(filepath.Join(tmpDir, "media", "survival_manual.epub"), []byte("FAKE_EPUB_BYTES"), 0o644); err != nil {
		t.Fatalf("write epub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "docs", "water_guide.pdf"), []byte("%PDF-1.4\n"), 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	_, _ = db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
	_, _ = db.Exec(`CREATE TABLE jobs (id TEXT PRIMARY KEY, type TEXT, target TEXT, status TEXT, progress REAL, progress_msg TEXT, metadata TEXT, error TEXT, created_at DATETIME, updated_at DATETIME)`)

	srv, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	if err := srv.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// 1. Calculators page (#35) renders offline with all 5 tabs and zero CDN links
	req := httptest.NewRequest(http.MethodGet, "/tools", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	toolsHTML := w.Body.String()
	for _, want := range []string{"Graphing Calculator", `id="gc-canvas"`, `id="gc-settings-pop"`, `id="gc-keypad"`, "Weight-Based Dosage", "Solar &amp; Battery", "Radio Frequency", "Coordinates, UTM", "Unit Converter", "<strong>Inputs:</strong>", "<strong>Outputs:</strong>"} {
		if w.Code != http.StatusOK || !strings.Contains(toolsHTML, want) {
			t.Errorf("expected /tools (status=%d) to contain %q", w.Code, want)
		}
	}
	if strings.Contains(toolsHTML, "cdn.jsdelivr.net") || strings.Contains(toolsHTML, "unpkg.com") {
		t.Errorf("/tools must not reference external CDNs")
	}
	if strings.Contains(toolsHTML, "r=sin(3") || strings.Contains(toolsHTML, "<title>Field Calculators — Ark Offline") {
		t.Errorf("/tools must omit old hint text and Offline in page title")
	}
	logoReq := httptest.NewRequest(http.MethodGet, "/static/ark-logo.png", nil)
	logoW := httptest.NewRecorder()
	mux.ServeHTTP(logoW, logoReq)
	if logoW.Code != http.StatusOK || logoW.Body.Len() < 100 {
		t.Errorf("expected /static/ark-logo.png to serve PNG, got %d len=%d", logoW.Code, logoW.Body.Len())
	}
	if !strings.Contains(toolsHTML, `src="/static/ark-logo.png"`) {
		t.Errorf("expected layout to include /static/ark-logo.png next to Ark brand")
	}

	// 2. Settings renders toggles for LAN Drop and OPDS Feed
	req = httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="lan_drop_enabled"`) || !strings.Contains(w.Body.String(), `name="opds_enabled"`) {
		t.Errorf("expected /settings to render lan_drop_enabled and opds_enabled toggles")
	}

	// 3. LAN Drop (#41): post text + multipart file, fetch list, download file, delete post
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("author", "Radio Desk")
	_ = mw.WriteField("category", "Alert")
	_ = mw.WriteField("message", "Battery bank charged 100%")
	fw, _ := mw.CreateFormFile("file", "../notes.txt")
	_, _ = fw.Write([]byte("checklist-ok"))
	_ = mw.Close()

	req = httptest.NewRequest(http.MethodPost, "/api/lan-drop/post?ajax=1", &body)
	req.RemoteAddr = "192.168.1.42:54321"
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/lan-drop/post failed %d: %s", w.Code, w.Body.String())
	}
	var postResp struct {
		OK   bool            `json:"ok"`
		Post LANDropPostView `json:"post"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &postResp); err != nil || !postResp.OK || postResp.Post.FileURL == "" {
		t.Fatalf("unexpected LAN drop post response: %v %s", err, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, postResp.Post.FileURL, nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "checklist-ok" {
		t.Errorf("expected uploaded file bytes, got %d: %q", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/lan-drop/posts?device=remote", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Battery bank charged 100%") || !strings.Contains(w.Body.String(), "192.168.1.42") {
		t.Errorf("expected posted message and connected device in /api/lan-drop/posts?device=remote, got %d: %s", w.Code, w.Body.String())
	}

	delForm := url.Values{"id": {postResp.Post.ID}}
	req = httptest.NewRequest(http.MethodPost, "/api/lan-drop/delete?ajax=1", strings.NewReader(delForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("delete LAN drop post failed: %d", w.Code)
	}

	// 4. OPDS Feed (#42): root, media, zim, search, opensearch, and direct file acquisition
	for _, ep := range []string{"/opds", "/opds/media", "/opds/all", "/opds/zim", "/opds/zim/wiki_test", "/opds/search?q=survival", "/opds/opensearch.xml"} {
		req = httptest.NewRequest(http.MethodGet, ep, nil)
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("OPDS endpoint %s returned %d: %s", ep, w.Code, w.Body.String())
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/opds/media", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "/opds/files/media/survival_manual.epub") || !strings.Contains(w.Body.String(), "/opds/files/docs/water_guide.pdf") {
		t.Errorf("expected /opds/media to list EPUB and PDF acquisition links, got: %s", w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/opds/files/media/survival_manual.epub", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "FAKE_EPUB_BYTES" {
		t.Errorf("expected /opds/files download to return EPUB bytes, got %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/opds/files/media/..%2Fzim%2Fwiki_test.zim", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Errorf("expected path traversal via /opds/files to be rejected")
	}

	// 5. Disable both LAN Drop and OPDS via Settings and confirm enforcement
	offForm := url.Values{
		"device_name": {"Ark"},
		"port":        {"8080"},
		"lan_enabled": {"on"},
	}
	req = httptest.NewRequest(http.MethodPost, "/settings/general", strings.NewReader(offForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/general returned %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/lan-drop/posts", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for disabled /api/lan-drop/posts, got %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/lan-drop", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "LAN Drop is currently turned off") {
		t.Errorf("expected /lan-drop disabled notice, got %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/opds", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for disabled /opds, got %d", w.Code)
	}
}

func TestRemoteAccessBlocksSettingsAndSystemAndHidesCommandNav(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	_, _ = db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
	_, _ = db.Exec(`CREATE TABLE jobs (id TEXT PRIMARY KEY, type TEXT, target TEXT, status TEXT, progress REAL, progress_msg TEXT, metadata TEXT, error TEXT, created_at DATETIME, updated_at DATETIME)`)

	server, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	if err := server.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	blockedRoutes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/system"},
		{http.MethodGet, "/settings"},
		{http.MethodGet, "/jobs"},
		{http.MethodPost, "/settings/general"},
		{http.MethodPost, "/settings/ai"},
		{http.MethodPost, "/settings/downloads"},
		{http.MethodPost, "/settings/restart"},
		{http.MethodPost, "/settings/shutdown"},
		{http.MethodGet, "/api/system/runtime"},
		{http.MethodGet, "/api/system/time"},
		{http.MethodGet, "/api/system/sse"},
		{http.MethodPost, "/api/system/install-runtime"},
	}
	for _, rt := range blockedRoutes {
		req := httptest.NewRequest(rt.method, rt.path, nil)
		req.RemoteAddr = "192.168.1.42:54321"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("remote %s %s expected 403 Forbidden, got %d", rt.method, rt.path, w.Code)
		}
	}

	// Remote requests to regular pages hide the Command sidebar section and its /system + /settings links.
	for _, path := range []string{"/", "/library", "/ask", "/lan-drop"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "192.168.1.42:54321"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("remote GET %s expected 200, got %d", path, w.Code)
		}
		body := w.Body.String()
		if strings.Contains(body, `<div class="nav-section-label">Command</div>`) || strings.Contains(body, `href="/system"`) || strings.Contains(body, `href="/settings"`) {
			t.Errorf("remote GET %s should hide Command sidebar links (/system, /settings)", path)
		}
	}

	// Local requests (IPv4 and IPv6 loopback) can access /system and /settings and see the Command sidebar section.
	for _, addr := range []string{"127.0.0.1:54321", "[::1]:54321"} {
		req := httptest.NewRequest(http.MethodGet, "/library", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		body := w.Body.String()
		if !strings.Contains(body, `<div class="nav-section-label">Command</div>`) || !strings.Contains(body, `href="/system"`) || !strings.Contains(body, `href="/settings"`) {
			t.Errorf("local GET /library (%s) should show Command sidebar links", addr)
		}
		for _, path := range []string{"/system", "/settings"} {
			req = httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = addr
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("local GET %s (%s) expected 200, got %d", path, addr, w.Code)
			}
		}
	}
}

func TestRemoteDeleteAndDownloadRestrictionsAndPeerDiscovery(t *testing.T) {
	// Peer B (remote Ark instance on LAN with a real document)
	peerBDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(peerBDir, "media", "docs"), 0o755)
	peerFile := filepath.Join(peerBDir, "media", "docs", "field_manual.pdf")
	_ = os.WriteFile(peerFile, []byte("%PDF-1.4 peer manual bytes"), 0o644)

	dbB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open dbB: %v", err)
	}
	defer dbB.Close()
	_, _ = dbB.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
	_, _ = dbB.Exec(`INSERT INTO settings (key, value) VALUES ('device_name', 'Workshop-PC')`)
	srvB, err := NewServer(dbB, peerBDir)
	if err != nil {
		t.Fatalf("NewServer B: %v", err)
	}
	muxB := http.NewServeMux()
	_ = srvB.RegisterRoutes(muxB)
	tsB := httptest.NewServer(muxB)
	defer tsB.Close()

	// Peer A (this computer)
	peerADir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(peerADir, "media", "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(peerADir, "media", "docs", "local_guide.pdf"), []byte("%PDF-1.4 local"), 0o644)

	dbA, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open dbA: %v", err)
	}
	defer dbA.Close()
	_, _ = dbA.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
	_, _ = dbA.Exec(`CREATE TABLE jobs (id TEXT PRIMARY KEY, type TEXT, target TEXT, status TEXT, progress REAL, progress_msg TEXT, error_msg TEXT, bytes_downloaded INTEGER, total_bytes INTEGER, eta_seconds INTEGER, metadata TEXT, created_at DATETIME, updated_at DATETIME, completed_at DATETIME)`)
	_, _ = dbA.Exec(`INSERT INTO settings (key, value) VALUES ('peer_manual_hosts', ?)`, tsB.URL)

	srvA, err := NewServer(dbA, peerADir)
	if err != nil {
		t.Fatalf("NewServer A: %v", err)
	}
	srvA.SetQueueManager(jobs.NewQueueManager(jobs.NewJobStore(dbA), jobs.QueueConfig{}))
	muxA := http.NewServeMux()
	_ = srvA.RegisterRoutes(muxA)

	// 1. Remote devices are blocked from ALL delete & download endpoints, while still able to download installed media
	remoteBlocked := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/jobs", "type=doc&target=https://example.com/a.pdf"},
		{http.MethodPost, "/api/library/delete", "rel_path=media/docs/local_guide.pdf"},
		{http.MethodPost, "/api/lan-drop/delete", "id=1"},
		{http.MethodPost, "/api/maps/pins", "action=delete&id=1"},
		{http.MethodPost, "/api/ai/sessions/delete", "id=1"},
		{http.MethodPost, "/api/ai/knowledge/remove", "id=1"},
		{http.MethodPost, "/api/apps/uninstall", "id=convertx"},
		{http.MethodPost, "/api/peers/pull", "peer_base_url=" + url.QueryEscape(tsB.URL) + "&rel_path=media/docs/field_manual.pdf"},
	}
	for _, tc := range remoteBlocked {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.RemoteAddr = "192.168.1.99:55555"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		muxA.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("remote %s %s expected 403, got %d: %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	// Remote device can still download already-installed media
	req := httptest.NewRequest(http.MethodGet, "/media/docs/local_guide.pdf", nil)
	req.RemoteAddr = "192.168.1.99:55555"
	w := httptest.NewRecorder()
	muxA.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "local") {
		t.Errorf("expected remote GET /media/docs/local_guide.pdf to succeed, got %d", w.Code)
	}

	// Remote /library hides #ctx-delete and omits category filter presets; remote /get-content disables download buttons
	req = httptest.NewRequest(http.MethodGet, "/library", nil)
	req.RemoteAddr = "192.168.1.99:55555"
	w = httptest.NewRecorder()
	muxA.ServeHTTP(w, req)
	libRemoteBody := w.Body.String()
	if strings.Contains(libRemoteBody, `id="ctx-delete"`) {
		t.Errorf("expected remote /library to hide #ctx-delete")
	}
	if strings.Contains(libRemoteBody, "ZIM Archives") || strings.Contains(libRemoteBody, "/library?cat=Music") {
		t.Errorf("expected /library to omit category filter preset buttons")
	}
	req = httptest.NewRequest(http.MethodGet, "/get-content", nil)
	req.RemoteAddr = "192.168.1.99:55555"
	w = httptest.NewRecorder()
	muxA.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `disabled title="Downloads are only available on the host computer"`) {
		t.Errorf("expected remote /get-content to grey out download buttons")
	}

	// 2. Peer A discovers Peer B via /api/peers/list (both local and remote devices) and proxies peer content via /peer/<id>/...
	req = httptest.NewRequest(http.MethodGet, "/api/peers/list", nil)
	req.RemoteAddr = "192.168.1.99:55555"
	w = httptest.NewRecorder()
	muxA.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Workshop-PC") || !strings.Contains(w.Body.String(), "field_manual.pdf") {
		t.Fatalf("expected remote /api/peers/list to return Workshop-PC with field_manual.pdf, got %d: %s", w.Code, w.Body.String())
	}
	var peerListResp struct {
		Peers []PeerLibraryView `json:"peers"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &peerListResp)
	if len(peerListResp.Peers) != 1 || peerListResp.Peers[0].ID == "" {
		t.Fatalf("expected 1 peer with ID, got %+v", peerListResp.Peers)
	}
	peerProxyReq := httptest.NewRequest(http.MethodGet, "/peer/"+url.PathEscape(peerListResp.Peers[0].ID)+"/media/docs/field_manual.pdf", nil)
	peerProxyReq.RemoteAddr = "192.168.1.99:55555"
	peerProxyW := httptest.NewRecorder()
	muxA.ServeHTTP(peerProxyW, peerProxyReq)
	if peerProxyW.Code != http.StatusOK || !strings.Contains(peerProxyW.Body.String(), "peer manual bytes") {
		t.Errorf("expected /peer/<id>/media/docs/field_manual.pdf to proxy peer file, got %d: %s", peerProxyW.Code, peerProxyW.Body.String())
	}

	// 3. Peer A pulls field_manual.pdf from Peer B via POST /api/peers/pull
	pullForm := url.Values{
		"peer_base_url": {tsB.URL},
		"rel_path":      {"media/docs/field_manual.pdf"},
		"title":         {"Field Manual"},
	}
	req = httptest.NewRequest(http.MethodPost, "/api/peers/pull", strings.NewReader(pullForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	muxA.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), `"job_id":"peer-`) {
		t.Fatalf("expected /api/peers/pull to queue job, got %d: %s", w.Code, w.Body.String())
	}

	// If file already exists locally, /api/peers/pull skips it
	_ = os.WriteFile(filepath.Join(peerADir, "media", "docs", "field_manual.pdf"), []byte("already here"), 0o644)
	req = httptest.NewRequest(http.MethodPost, "/api/peers/pull", strings.NewReader(pullForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	muxA.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"skipped":true`) {
		t.Errorf("expected /api/peers/pull to skip existing file, got %d: %s", w.Code, w.Body.String())
	}

	// 4. Peer B disallows downloads -> /api/peers/download returns 403
	_, _ = dbB.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('peer_allow_downloads', 'false')`)
	resp, err := http.Get(tsB.URL + "/api/peers/download?rel=media/docs/field_manual.pdf")
	if err != nil {
		t.Fatalf("GET peer download: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 when peer_allow_downloads=false, got %d", resp.StatusCode)
	}

	// 5. Peer B disables discoverability -> /api/peers/list omits Workshop-PC
	_, _ = dbB.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('peer_discoverable', 'false')`)
	req = httptest.NewRequest(http.MethodGet, "/api/peers/list", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	muxA.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "Workshop-PC") {
		t.Errorf("expected /api/peers/list to omit non-discoverable peer, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMeshRadioBridgeAndCelestialTracker(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	_, _ = db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`)
	_, _ = db.Exec(`CREATE TABLE app_states (id TEXT PRIMARY KEY, name TEXT, category TEXT, image TEXT, container_id TEXT, status TEXT, port INTEGER, proxy_mode TEXT, auto_start BOOLEAN, created_at DATETIME, updated_at DATETIME)`)

	srv, err := NewServer(db, tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	if err := srv.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// 1. /apps lists Meshtastic / LoRa Bridge natively
	req := httptest.NewRequest(http.MethodGet, "/apps", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Meshtastic / LoRa Bridge") || !strings.Contains(w.Body.String(), `data-open-url="/mesh"`) {
		t.Errorf("expected /apps to include native Meshtastic / LoRa Bridge app")
	}

	// 2. /mesh renders USB + Bluetooth + Meshtastic/MeshCore UI offline
	req = httptest.NewRequest(http.MethodGet, "/mesh", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	meshHTML := w.Body.String()
	for _, want := range []string{"Meshtastic / LoRa Radio Bridge", "Connect USB", "Connect Bluetooth", "MeshCore", `id="mesh-msg-feed"`, `id="mesh-nodes-tbody"`} {
		if w.Code != http.StatusOK || !strings.Contains(meshHTML, want) {
			t.Errorf("expected /mesh (status=%d) to contain %q", w.Code, want)
		}
	}
	if strings.Contains(meshHTML, "cdn.jsdelivr.net") || strings.Contains(meshHTML, "unpkg.com") {
		t.Errorf("expected /mesh to have zero external CDN references")
	}

	// 3. Mesh API persistence (local write, remote read-only)
	nodeForm := url.Values{"node_id": {"!a1b2c3d4"}, "short_name": {"BASE"}, "long_name": {"Base Camp"}, "protocol": {"meshtastic"}, "lat": {"45.5"}, "lng": {"-122.6"}, "battery": {"92"}, "snr": {"8.5"}}
	req = httptest.NewRequest(http.MethodPost, "/api/mesh/node", strings.NewReader(nodeForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/mesh/node status=%d: %s", w.Code, w.Body.String())
	}

	msgForm := url.Values{"protocol": {"meshtastic"}, "direction": {"rx"}, "channel": {"0"}, "from_id": {"!a1b2c3d4"}, "from_name": {"Base Camp"}, "to_id": {"^all"}, "body": {"Radio check loud and clear"}}
	req = httptest.NewRequest(http.MethodPost, "/api/mesh/message", strings.NewReader(msgForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/mesh/message status=%d: %s", w.Code, w.Body.String())
	}

	// Remote read allowed, remote write blocked
	req = httptest.NewRequest(http.MethodGet, "/api/mesh/state", nil)
	req.RemoteAddr = "192.168.1.99:55555"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Radio check loud and clear") || !strings.Contains(w.Body.String(), "Base Camp") {
		t.Errorf("expected remote GET /api/mesh/state to return persisted node and message, got %d: %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/mesh/clear", nil)
	req.RemoteAddr = "192.168.1.99:55555"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected remote POST /api/mesh/clear to return 403, got %d", w.Code)
	}

	// 4. /tools includes Celestial & Stars tracker tab with moon phase and compass
	req = httptest.NewRequest(http.MethodGet, "/tools", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	toolsHTML := w.Body.String()
	for _, want := range []string{"Celestial &amp; Stars", `id="tab-celestial"`, `id="cel-canvas"`, `id="cel-moon-canvas"`, `id="cel-heading"`, `id="cel-lat"`, `id="cel-lon"`, `id="cel-target-card"`, `id="cel-opt-mw"`, `id="cel-opt-dso"`, `id="cel-opt-ecliptic"`} {
		if w.Code != http.StatusOK || !strings.Contains(toolsHTML, want) {
			t.Errorf("expected /tools to contain %q", want)
		}
	}
	if strings.Contains(toolsHTML, `id="cel-ephem-tbody"`) {
		t.Errorf("expected /tools not to include removed cel-ephem-tbody table")
	}
}
