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
	"net/url"
	"os"
	"path/filepath"
	"time"

	"ark/internal/ai"
	"ark/internal/apps"
	"ark/internal/docker"
	"ark/internal/jobs"
	"ark/internal/storage"
	"ark/internal/system"
)

//go:embed templates/* static/*
var embeddedFiles embed.FS

// JobView represents a background job rendered in the UI
type JobView struct {
	ID          string
	Type        string
	Target      string
	Status      string
	Progress    float64
	ProgressMsg string
	ErrorMsg    string
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

// SystemView holds formatted host telemetry for rendering on /system
type SystemView struct {
	CPUPercent  float64
	RAMPercent  float64
	RAMUsedGB   string
	RAMTotalGB  string
	DiskPercent float64
	DiskFreeGB  uint64
	DiskTotalGB uint64
	Hostname    string
	OS          string
	UptimeText  string
}

// ContainerView represents an Ark container on /system
type ContainerView struct {
	AppID      string
	Name       string
	Image      string
	Status     string
	CPUText    string
	MemoryText string
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
	HasActiveJobs      bool
	Settings           map[string]string
	RAGDocs            []ai.RAGDocument
	SystemMetrics      *SystemView
	Containers         []ContainerView
	DockerOnline       bool
	FlashMsg           string
}

// Server handles Ark web UI and API routing
type Server struct {
	db        *sql.DB
	dataDir   string
	templates map[string]*template.Template
	docker    *docker.Client
	proxy     *apps.ProxyManager
	queue     *jobs.QueueManager
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
		"system.html",
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
		proxy:     apps.NewProxyManager(),
	}
	return s, nil
}

// SetDockerClient attaches the Docker client used to manage app containers.
func (s *Server) SetDockerClient(d *docker.Client) {
	s.docker = d
}

// SetQueueManager attaches the background job queue manager.
func (s *Server) SetQueueManager(qm *jobs.QueueManager) {
	s.queue = qm
}

// ProxyManager returns the reverse proxy manager for /a/<id>/ routes.
func (s *Server) ProxyManager() *apps.ProxyManager {
	return s.proxy
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
	mux.HandleFunc("/system", s.handleSystem)
	mux.HandleFunc("/settings", s.handleSettings)
	mux.HandleFunc("/settings/general", s.handleSettingsGeneral)
	mux.HandleFunc("/settings/ai", s.handleSettingsAI)
	mux.HandleFunc("/settings/downloads", s.handleSettingsDownloads)

	// API endpoints
	mux.HandleFunc("/api/jobs", s.handleAPIJobs)
	mux.HandleFunc("/api/jobs/cancel", s.handleAPIJobCancel)
	mux.HandleFunc("/api/jobs/retry", s.handleAPIJobRetry)
	mux.HandleFunc("/api/jobs/delete", s.handleAPIJobDelete)
	mux.HandleFunc("/api/apps/install", s.handleAPIAppInstall)
	mux.HandleFunc("/api/apps/start", s.handleAPIAppStart)
	mux.HandleFunc("/api/apps/stop", s.handleAPIAppStop)
	mux.HandleFunc("/api/apps/uninstall", s.handleAPIAppUninstall)
	mux.Handle("/api/system/sse", system.NewSSEFeed(s.dataDir))
	mux.HandleFunc("/api/containers/restart", s.handleAPIContainerRestart)
	mux.HandleFunc("/api/containers/logs", s.handleAPIContainerLogs)
	mux.HandleFunc("/api/ai/detect", s.handleAPIAIDetect)
	mux.HandleFunc("/api/ai/test", s.handleAPIAITest)
	mux.HandleFunc("/api/ai/chat", s.handleAPIAIChat)
	mux.HandleFunc("/api/ai/index", s.handleAPIAIIndex)
	mux.HandleFunc("/api/ai/index-article", s.handleAPIAIIndexArticle)
	mux.HandleFunc("/api/ai/knowledge/remove", s.handleAPIAIKnowledgeRemove)

	// App reverse proxy
	if s.proxy != nil {
		mux.Handle("/a/", s.proxy)
	}

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

func hasActive(jobsList []JobView) bool {
	for _, j := range jobsList {
		if j.Status == "pending" || j.Status == "running" {
			return true
		}
	}
	return false
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	jobsList := s.fetchRecentJobs(5)
	freeGB, totalGB, usedGB, pct := s.getStorageStats(r.Context())
	installedApps, _ := s.fetchApps()

	data := PageData{
		Title:              "Home",
		ActiveNav:          "home",
		StorageFreeGB:      freeGB,
		StorageTotalGB:     totalGB,
		StorageUsedGB:      usedGB,
		StorageUsedPercent: pct,
		Jobs:               jobsList,
		InstalledApps:      installedApps,
		HasActiveJobs:      hasActive(jobsList),
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
	settingsMap := s.fetchSettings()
	ragDocs, _ := ai.ListDocuments(r.Context(), s.db)

	data := PageData{
		Title:       "Ask AI",
		ActiveNav:   "ask",
		AIConnected: settingsMap["ai_server_url"] != "",
		RAGDocs:     ragDocs,
	}
	s.render(w, "ask.html", data)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobsList := s.fetchRecentJobs(50)
	data := PageData{
		Title:         "Job Queue",
		ActiveNav:     "jobs",
		Jobs:          jobsList,
		HasActiveJobs: hasActive(jobsList),
	}
	s.render(w, "jobs.html", data)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	freeGB, totalGB, usedGB, pct := s.getStorageStats(r.Context())
	flash := ""
	if r.URL.Query().Get("saved") == "1" {
		flash = "Settings saved."
	}
	data := PageData{
		Title:              "Settings",
		ActiveNav:          "settings",
		StorageFreeGB:      freeGB,
		StorageTotalGB:     totalGB,
		StorageUsedGB:      usedGB,
		StorageUsedPercent: pct,
		Settings:           s.fetchSettings(),
		FlashMsg:           flash,
	}
	s.render(w, "settings.html", data)
}

func (s *Server) handleAPIJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	rawType := r.FormValue("type")
	target := r.FormValue("target")
	if rawType == "" || target == "" {
		http.Error(w, "Missing type or target", http.StatusBadRequest)
		return
	}

	jobType := rawType
	meta := jobs.JobMetadata{
		OutputDir: filepath.Join(s.dataDir, "zim"),
	}

	switch rawType {
	case "preset":
		jobType = jobs.TypePreset
		meta.OutputDir = filepath.Join(s.dataDir, "zim")
	case "doc", "download":
		jobType = jobs.TypeDownload
		meta.OutputDir = filepath.Join(s.dataDir, "media", "docs")
	case "video":
		jobType = jobs.TypeEncode
		meta.Format = "av1"
		meta.OutputDir = filepath.Join(s.dataDir, "media", "video")
	case "video_h264":
		jobType = jobs.TypeEncode
		meta.Format = "mp4"
		meta.OutputDir = filepath.Join(s.dataDir, "media", "video")
	case "music":
		jobType = jobs.TypeEncode
		meta.Format = "music"
		meta.OutputDir = filepath.Join(s.dataDir, "media", "music")
	case "zim":
		jobType = jobs.TypeZim
		meta.OutputDir = filepath.Join(s.dataDir, "zim")
	}

	metaBytes, _ := json.Marshal(meta)
	jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())

	if s.queue != nil {
		job := &jobs.Job{
			ID:          jobID,
			Type:        jobType,
			Target:      target,
			Status:      jobs.StatusPending,
			Progress:    0.0,
			ProgressMsg: "Queued",
			Metadata:    string(metaBytes),
		}
		if err := s.queue.Submit(r.Context(), job); err != nil {
			log.Printf("Failed to submit job: %v", err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
	} else if s.db != nil {
		_, err := s.db.Exec(`
			INSERT INTO jobs (id, type, target, status, progress, progress_msg, metadata, created_at, updated_at)
			VALUES (?, ?, ?, 'pending', 0.0, 'Queued', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		`, jobID, jobType, target, string(metaBytes))
		if err != nil {
			log.Printf("Failed to insert job: %v", err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, "/jobs", http.StatusSeeOther)
}

func (s *Server) handleAPIJobCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	jobID := r.FormValue("id")
	if jobID != "" {
		if s.queue != nil {
			_ = s.queue.Cancel(r.Context(), jobID)
		} else if s.db != nil {
			_, _ = s.db.Exec(`UPDATE jobs SET status = 'cancelled', error_msg = 'Cancelled by user', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobID)
		}
	}
	redirect := r.FormValue("redirect")
	if redirect == "" {
		redirect = "/jobs"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (s *Server) handleAPIJobRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	jobID := r.FormValue("id")
	if jobID != "" {
		if s.queue != nil {
			_ = s.queue.Retry(r.Context(), jobID)
		} else if s.db != nil {
			_, _ = s.db.Exec(`UPDATE jobs SET status = 'pending', progress = 0, error_msg = '', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobID)
		}
	}
	http.Redirect(w, r, "/jobs", http.StatusSeeOther)
}

func (s *Server) handleAPIJobDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	jobID := r.FormValue("id")
	if jobID != "" {
		if s.queue != nil {
			_ = s.queue.Remove(r.Context(), jobID)
		} else if s.db != nil {
			_, _ = s.db.Exec(`DELETE FROM jobs WHERE id = ?`, jobID)
		}
	}
	redirect := r.FormValue("redirect")
	if redirect == "" {
		redirect = "/jobs"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
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

	if m.ID == "druglabels" && s.queue != nil {
		dbFile := filepath.Join(s.dataDir, "druglabels", "druglabels.db")
		if _, statErr := os.Stat(dbFile); os.IsNotExist(statErr) {
			metaBytes, _ := json.Marshal(jobs.JobMetadata{
				OutputDir: filepath.Join(s.dataDir, "druglabels"),
			})
			_ = s.queue.Submit(r.Context(), &jobs.Job{
				ID:          fmt.Sprintf("job-druglabels-%d", time.Now().UnixNano()),
				Type:        jobs.TypeDrugLabels,
				Target:      "openFDA Drug Labels Database",
				Status:      jobs.StatusPending,
				Progress:    0.0,
				ProgressMsg: "Queued FDA drug labels database download",
				Metadata:    string(metaBytes),
			})
		}
	}

	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

func (s *Server) startAppContainer(ctx context.Context, appID string) error {
	if s.docker == nil || !s.docker.IsAvailable(ctx) {
		return nil
	}
	catDir := findCatalogDir()
	m, err := apps.LoadManifest(filepath.Join(catDir, appID, "app.yaml"))
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	if err := s.docker.Pull(ctx, m.Image); err != nil {
		log.Printf("[Ark] Pull warning for %s (%s): %v", appID, m.Image, err)
	}
	info, err := s.docker.EnsureRunning(ctx, m, s.dataDir)
	if err != nil {
		return fmt.Errorf("ensure running: %w", err)
	}
	if s.proxy != nil && info.HostPort > 0 {
		target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", info.HostPort))
		if err == nil {
			stripPrefix := m.ID != "kiwix"
			s.proxy.Register(m.ID, target, stripPrefix)
		}
	}
	if s.db != nil {
		_, _ = s.db.Exec("UPDATE app_states SET container_id = ?, port = ?, status = 'running', updated_at = CURRENT_TIMESTAMP WHERE id = ?", info.ID, info.HostPort, appID)
	}
	return nil
}

// RestoreRunningApps re-registers proxies for any apps marked running in SQLite.
func (s *Server) RestoreRunningApps(ctx context.Context) {
	if s.db == nil {
		return
	}
	rows, err := s.db.Query("SELECT id FROM app_states WHERE status = 'running'")
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	for _, id := range ids {
		if err := s.startAppContainer(ctx, id); err != nil {
			log.Printf("[Ark] Failed to restore app %s: %v", id, err)
		}
	}
}

func (s *Server) handleAPIAppStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	appID := r.FormValue("id")
	if appID != "" {
		if err := s.startAppContainer(r.Context(), appID); err != nil {
			log.Printf("[Ark] Failed to start app %s: %v", appID, err)
			if s.db != nil {
				_, _ = s.db.Exec("UPDATE app_states SET status = 'error', updated_at = CURRENT_TIMESTAMP WHERE id = ?", appID)
			}
			http.Error(w, "Failed to start app container: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if s.docker == nil && s.db != nil {
			_, _ = s.db.Exec("UPDATE app_states SET status = 'running', updated_at = CURRENT_TIMESTAMP WHERE id = ?", appID)
		}
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

func (s *Server) handleAPIAppStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	appID := r.FormValue("id")
	if appID != "" {
		if s.docker != nil && s.docker.IsAvailable(r.Context()) {
			if err := s.docker.Stop(r.Context(), appID, 10); err != nil {
				log.Printf("[Ark] Failed to stop container %s: %v", appID, err)
			}
		}
		if s.proxy != nil {
			s.proxy.Unregister(appID)
		}
		if s.db != nil {
			_, _ = s.db.Exec("UPDATE app_states SET status = 'stopped', updated_at = CURRENT_TIMESTAMP WHERE id = ?", appID)
		}
	}
	redirect := r.FormValue("redirect")
	if redirect == "" {
		redirect = "/apps"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (s *Server) handleAPIAppUninstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	appID := r.FormValue("id")
	if appID != "" {
		if s.docker != nil && s.docker.IsAvailable(r.Context()) {
			_ = s.docker.Remove(r.Context(), appID, true)
		}
		if s.proxy != nil {
			s.proxy.Unregister(appID)
		}
		if s.db != nil {
			_, _ = s.db.Exec("DELETE FROM app_states WHERE id = ?", appID)
		}
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
		openURL := fmt.Sprintf("/a/%s/", m.ID)
		if m.Proxy == "port" && m.Port > 0 {
			openURL = fmt.Sprintf("http://localhost:%d/", m.Port)
		}
		app := AppView{
			ID:        m.ID,
			Name:      m.Name,
			Category:  m.Category,
			Image:     m.Image,
			Status:    "not_installed",
			ProxyMode: m.Proxy,
			OpenURL:   openURL,
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
		SELECT id, type, target, status, progress, COALESCE(progress_msg, ''), COALESCE(error_msg, '')
		FROM jobs
		ORDER BY created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var jobsList []JobView
	for rows.Next() {
		var j JobView
		if err := rows.Scan(&j.ID, &j.Type, &j.Target, &j.Status, &j.Progress, &j.ProgressMsg, &j.ErrorMsg); err == nil {
			jobsList = append(jobsList, j)
		}
	}
	return jobsList
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
