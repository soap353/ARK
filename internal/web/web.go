package web

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"ark/internal/apps"
	"ark/internal/storage"
)

//go:embed templates/* static/*
var embeddedFiles embed.FS

// JobView represents a background job rendered in the UI
type JobView struct {
	ID       string
	Type     string
	Target   string
	Status   string
	Progress float64
}

// LibraryItemView represents an offline media/zim entry
type LibraryItemView struct {
	Title    string
	Category string
	Size     string
	Date     string
	OpenURL  string
}

// AppView represents an application module in the catalog or installed
type AppView struct {
	ID          string
	Name        string
	Category    string
	Description string
	Image       string
	Status      string // "not_installed", "running", "stopped", "error"
	ProxyMode   string
	OpenURL     string
}

// PageData carries standard context for HTML templates
type PageData struct {
	Title              string
	ActiveNav          string
	StorageFreeGB      uint64
	StorageTotalGB     uint64
	StorageUsedGB      uint64
	StorageUsedPercent uint64
	Jobs               []JobView
	Items              []LibraryItemView
	InstalledApps      []AppView
	AvailableApps      []AppView
	AIConnected        bool
}

// Server handles Ark web UI and API routing
type Server struct {
	db        *sql.DB
	dataDir   string
	templates map[string]*template.Template
}

// NewServer initializes templates and returns a configured Server
func NewServer(db *sql.DB, dataDir string) (*Server, error) {
	pages := []string{
		"home.html",
		"library.html",
		"get_content.html",
		"apps.html",
		"ask.html",
		"jobs.html",
		"settings.html",
	}

	tmplMap := make(map[string]*template.Template)
	for _, page := range pages {
		tmpl, err := template.ParseFS(embeddedFiles, "templates/layout.html", "templates/"+page)
		if err != nil {
			return nil, fmt.Errorf("failed to parse template layout with %s: %w", page, err)
		}
		tmplMap[page] = tmpl
	}

	s := &Server{
		db:        db,
		dataDir:   dataDir,
		templates: tmplMap,
	}
	return s, nil
}

// RegisterRoutes binds all application routes to the given mux
func (s *Server) RegisterRoutes(mux *http.ServeMux) error {
	staticFS, err := fs.Sub(embeddedFiles, "static")
	if err != nil {
		return fmt.Errorf("failed to create static sub-filesystem: %w", err)
	}

	// Static assets
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Health check endpoint
	mux.HandleFunc("/healthz", s.handleHealthz)

	// Pages
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/library", s.handleLibrary)
	mux.HandleFunc("/get-content", s.handleGetContent)
	mux.HandleFunc("/apps", s.handleApps)
	mux.HandleFunc("/ask", s.handleAsk)
	mux.HandleFunc("/jobs", s.handleJobs)
	mux.HandleFunc("/settings", s.handleSettings)

	// API endpoints
	mux.HandleFunc("/api/jobs", s.handleAPIJobs)
	mux.HandleFunc("/api/apps/install", s.handleAPIAppInstall)
	mux.HandleFunc("/api/apps/start", s.handleAPIAppStart)
	mux.HandleFunc("/api/apps/stop", s.handleAPIAppStop)
	mux.HandleFunc("/api/apps/uninstall", s.handleAPIAppUninstall)

	return nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"app":    "ark",
	})
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	jobs := s.fetchRecentJobs(5)
	freeGB, totalGB, usedGB, pct := s.getStorageStats(r.Context())
	installedApps, _ := s.fetchApps()

	data := PageData{
		Title:              "Home",
		ActiveNav:          "home",
		StorageFreeGB:      freeGB,
		StorageTotalGB:     totalGB,
		StorageUsedGB:      usedGB,
		StorageUsedPercent: pct,
		Jobs:               jobs,
		InstalledApps:      installedApps,
	}
	s.render(w, "home.html", data)
}

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	items := s.fetchLibraryItems()
	data := PageData{
		Title:     "Library",
		ActiveNav: "library",
		Items:     items,
	}
	s.render(w, "library.html", data)
}

func (s *Server) handleGetContent(w http.ResponseWriter, r *http.Request) {
	data := PageData{
		Title:     "Get Content",
		ActiveNav: "get-content",
	}
	s.render(w, "get_content.html", data)
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	installedApps, availableApps := s.fetchApps()
	data := PageData{
		Title:         "Apps",
		ActiveNav:     "apps",
		InstalledApps: installedApps,
		AvailableApps: availableApps,
	}
	s.render(w, "apps.html", data)
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var aiURL string
	if s.db != nil {
		_ = s.db.QueryRow("SELECT value FROM settings WHERE key = 'ai_server_url'").Scan(&aiURL)
	}

	data := PageData{
		Title:       "Ask AI",
		ActiveNav:   "ask",
		AIConnected: aiURL != "",
	}
	s.render(w, "ask.html", data)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs := s.fetchRecentJobs(50)
	data := PageData{
		Title:     "Job Queue",
		ActiveNav: "jobs",
		Jobs:      jobs,
	}
	s.render(w, "jobs.html", data)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	freeGB, totalGB, usedGB, pct := s.getStorageStats(r.Context())
	data := PageData{
		Title:              "Settings",
		ActiveNav:          "settings",
		StorageFreeGB:      freeGB,
		StorageTotalGB:     totalGB,
		StorageUsedGB:      usedGB,
		StorageUsedPercent: pct,
	}
	s.render(w, "settings.html", data)
}

func (s *Server) handleAPIJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	jobType := r.FormValue("type")
	target := r.FormValue("target")
	if jobType == "" || target == "" {
		http.Error(w, "Missing type or target", http.StatusBadRequest)
		return
	}

	if s.db != nil {
		jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
		_, err := s.db.Exec(`
			INSERT INTO jobs (id, type, target, status, progress, created_at, updated_at)
			VALUES (?, ?, ?, 'pending', 0.0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		`, jobID, jobType, target)
		if err != nil {
			log.Printf("Failed to insert job: %v", err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, "/jobs", http.StatusSeeOther)
}

func findCatalogDir() string {
	candidates := []string{"catalog", "../../catalog", "../catalog"}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}
	return "catalog"
}

func (s *Server) handleAPIAppInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	appID := r.FormValue("id")
	if appID == "" || s.db == nil {
		http.Error(w, "Invalid app ID", http.StatusBadRequest)
		return
	}

	catDir := findCatalogDir()
	m, err := apps.LoadManifest(filepath.Join(catDir, appID, "app.yaml"))
	if err != nil {
		http.Error(w, "Manifest not found: "+err.Error(), http.StatusNotFound)
		return
	}

	_, err = s.db.Exec(`
		INSERT INTO app_states (id, name, category, image, status, proxy_mode, auto_start, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'stopped', ?, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET status = 'stopped', updated_at = CURRENT_TIMESTAMP
	`, m.ID, m.Name, m.Category, m.Image, m.Proxy)
	if err != nil {
		log.Printf("Failed to install app %s: %v", appID, err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

func (s *Server) handleAPIAppStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	appID := r.FormValue("id")
	if appID != "" && s.db != nil {
		_, _ = s.db.Exec("UPDATE app_states SET status = 'running', updated_at = CURRENT_TIMESTAMP WHERE id = ?", appID)
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

func (s *Server) handleAPIAppStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	appID := r.FormValue("id")
	if appID != "" && s.db != nil {
		_, _ = s.db.Exec("UPDATE app_states SET status = 'stopped', updated_at = CURRENT_TIMESTAMP WHERE id = ?", appID)
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

func (s *Server) handleAPIAppUninstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	appID := r.FormValue("id")
	if appID != "" && s.db != nil {
		_, _ = s.db.Exec("DELETE FROM app_states WHERE id = ?", appID)
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

func (s *Server) fetchApps() ([]AppView, []AppView) {
	catDir := findCatalogDir()
	manifests, _ := apps.LoadAllManifests(catDir)

	installedMap := make(map[string]AppView)
	if s.db != nil {
		rows, err := s.db.Query(`
			SELECT id, name, category, image, status, proxy_mode
			FROM app_states
		`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id, name, category, image, status, proxyMode string
				if err := rows.Scan(&id, &name, &category, &image, &status, &proxyMode); err == nil {
					openURL := "#"
					if proxyMode == "path" {
						openURL = fmt.Sprintf("/a/%s/", id)
					}
					installedMap[id] = AppView{
						ID:        id,
						Name:      name,
						Category:  category,
						Image:     image,
						Status:    status,
						ProxyMode: proxyMode,
						OpenURL:   openURL,
					}
				}
			}
		}
	}

	var installed []AppView
	var available []AppView

	for _, m := range manifests {
		app := AppView{
			ID:        m.ID,
			Name:      m.Name,
			Category:  m.Category,
			Image:     m.Image,
			Status:    "not_installed",
			ProxyMode: m.Proxy,
			OpenURL:   fmt.Sprintf("/a/%s/", m.ID),
		}

		if state, found := installedMap[m.ID]; found {
			app.Status = state.Status
			if state.Status == "running" || state.Status == "stopped" {
				installed = append(installed, app)
			}
		}
		available = append(available, app)
	}

	return installed, available
}

func (s *Server) getStorageStats(ctx context.Context) (uint64, uint64, uint64, uint64) {
	if s.dataDir == "" {
		return 0, 0, 0, 0
	}
	report, err := storage.GetDiskUsage(ctx, s.dataDir)
	if err != nil || report == nil {
		return 0, 0, 0, 0
	}
	usedGB := uint64(report.TotalUsedBytes / (1024 * 1024 * 1024))
	freeGB := uint64(report.TotalFreeBytes / (1024 * 1024 * 1024))
	totalGB := uint64(report.DiskTotalBytes / (1024 * 1024 * 1024))
	var pct uint64
	if totalGB > 0 {
		pct = (usedGB * 100) / totalGB
	}
	return freeGB, totalGB, usedGB, pct
}

func (s *Server) fetchRecentJobs(limit int) []JobView {
	if s.db == nil {
		return nil
	}

	rows, err := s.db.Query(`
		SELECT id, type, target, status, progress
		FROM jobs
		ORDER BY created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var jobs []JobView
	for rows.Next() {
		var j JobView
		if err := rows.Scan(&j.ID, &j.Type, &j.Target, &j.Status, &j.Progress); err == nil {
			jobs = append(jobs, j)
		}
	}
	return jobs
}

func (s *Server) fetchLibraryItems() []LibraryItemView {
	if s.db == nil {
		return nil
	}

	rows, err := s.db.Query(`
		SELECT title, category, size_bytes, date
		FROM catalog_items
		WHERE is_downloaded = 1
		ORDER BY title ASC
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var items []LibraryItemView
	for rows.Next() {
		var title, category, date string
		var sizeBytes int64
		if err := rows.Scan(&title, &category, &sizeBytes, &date); err == nil {
			items = append(items, LibraryItemView{
				Title:    title,
				Category: category,
				Size:     fmt.Sprintf("%.1f GB", float64(sizeBytes)/(1024*1024*1024)),
				Date:     date,
				OpenURL:  "#",
			})
		}
	}
	return items
}

func (s *Server) render(w http.ResponseWriter, pageTemplate string, data PageData) {
	tmpl, ok := s.templates[pageTemplate]
	if !ok {
		log.Printf("Template not found: %s", pageTemplate)
		http.Error(w, "Page Not Found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("Template render error (%s): %v", pageTemplate, err)
		http.Error(w, "Failed to render page", http.StatusInternalServerError)
	}
}
