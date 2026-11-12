package web

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"time"
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

// PageData carries standard context for HTML templates
type PageData struct {
	Title              string
	ActiveNav          string
	StorageFreeGB      uint64
	StorageTotalGB     uint64
	StorageUsedPercent uint64
	Jobs               []JobView
	Items              []LibraryItemView
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
	data := PageData{
		Title:              "Home",
		ActiveNav:          "home",
		StorageFreeGB:      128,
		StorageTotalGB:     512,
		StorageUsedPercent: 75,
		Jobs:               jobs,
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
	data := PageData{
		Title:     "Apps",
		ActiveNav: "apps",
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
	data := PageData{
		Title:     "Settings",
		ActiveNav: "settings",
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
