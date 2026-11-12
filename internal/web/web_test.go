package web

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		CREATE TABLE jobs (id TEXT PRIMARY KEY, type TEXT, target TEXT, status TEXT, progress REAL, created_at DATETIME, updated_at DATETIME);
		CREATE TABLE catalog_items (id TEXT PRIMARY KEY, title TEXT, category TEXT, language TEXT, size_bytes INTEGER, date TEXT, flavour TEXT, url TEXT, is_downloaded BOOLEAN, file_path TEXT, created_at DATETIME);
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
		{"/", http.StatusOK, "Ark"},
		{"/library", http.StatusOK, "Offline Library"},
		{"/get-content", http.StatusOK, "Get Content"},
		{"/apps", http.StatusOK, "Installed App Modules"},
		{"/ask", http.StatusOK, "Ask Local AI"},
		{"/jobs", http.StatusOK, "Background Job Queue"},
		{"/settings", http.StatusOK, "Settings"},
		{"/static/ark.css", http.StatusOK, "--color-bg: #0B0D10;"},
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

func TestCSSWCAGPalette(t *testing.T) {
	cssData, err := embeddedFiles.ReadFile("static/ark.css")
	if err != nil {
		t.Fatalf("failed to read embedded ark.css: %v", err)
	}
	css := string(cssData)

	expectedTokens := []string{
		"#0B0D10", // Background
		"#F4F4F0", // Primary text (17.7:1 AAA)
		"#FFB000", // Accent (10.6:1 AAA)
		"#80868B", // Secondary text (5.3:1 AA)
		"#5C6168", // Decorative divider
		"#3DD68C", // Success
		"#E5484D", // Error
		"18px",    // Font base size
		"prefers-reduced-motion",
	}

	for _, token := range expectedTokens {
		if !strings.Contains(css, token) {
			t.Errorf("ark.css missing expected WCAG palette token: %s", token)
		}
	}
}
