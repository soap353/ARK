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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ark/catalog"
	"ark/internal/ai"
	"ark/internal/apps"
	"ark/internal/docker"
	"ark/internal/fieldguide"
	"ark/internal/jobs"
	"ark/internal/kiwix"
	"ark/internal/storage"
	"ark/internal/system"
	"ark/internal/update"
	"ark/internal/usb"
)

var jobSeq atomic.Uint64

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
	FileName string
	RelPath  string
	Category    string
	Description string
	Stem        string
	Size        string
	Date        string
	OpenURL     string
	IconURL     string
}

// AppView represents an application module in the catalog or installed
type AppView struct {
	ID          string
	Name        string
	Category    string
	Description string
	Image       string
	Port        int
	Status      string // "not_installed", "running", "stopped", "error"
	ProxyMode   string
	OpenURL     string
	IsCustom    bool
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
	Title               string
	ActiveNav           string
	StorageFreeGB       uint64
	StorageTotalGB      uint64
	StorageUsedGB       uint64
	StorageUsedPercent  uint64
	StorageArkPercent   uint64
	StorageOtherPercent uint64
	ArkDataUsedText     string
	Jobs                []JobView
	Items               []LibraryItemView
	InstalledApps       []AppView
	AvailableApps       []AppView
	AIConnected         bool
	AIStatus            ai.ModelRuntimeStatus
	AIStatusJSON        template.JS
	AIContextMax        int
	AskPreloadZIM       string
	AskPreloadArticle   string
	AskPreloadTitle     string
	AskPreloadQuery     string
	HasActiveJobs       bool
	Settings            map[string]string
	RAGDocs             []ai.RAGDocument
	SystemMetrics       *SystemView
	Containers          []ContainerView
	DockerOnline        bool
	RuntimeLabel        string
	CanInstallRuntime   bool
	FlashMsg            string
	FlashErr            string
	USBDrives           []USBDriveView
	USBCategories       []USBCategoryView
	USBProfiles         []usb.ExportProfile
	ArkVersion          string
	RollbackAvailable   bool
	RollbackNotice      string
	MapArchives         []MapArchiveView
	MapArchivesJSON     template.JS
	SelectedMap         string
	MapsRunning         bool
	WikipediaOptions    []WikipediaOptionView
	NomadCollections    []NomadCollectionView
	MapRegions          []MapRegionView
	CreatorVideoPacks   []kiwix.VideoCreatorPack
	CountryMaps         []kiwix.CountryMapOption
	MapPins             []MapPinView
	MapPinsJSON         template.JS
	CustomOPDS          string
	InitialQuery        string
	SelectedCategory    string
	SelectedCondition   string
	SelectedGuideKind   string
	GuideConditions     []fieldguide.Condition
	GuideCategories     []string
	GuideRemedies       []fieldguide.Remedy
	LastDowntime        system.DowntimeInfo
	GitHubRepo          string
	SessionToken        string
	IsLocal             bool
	LANDropEnabled      bool
	OPDSEnabled         bool
	LANDropPosts        []LANDropPostView
	ConnectedDevices    []LANDropDeviceView
	LANShareURLs        []string
	LANAllowlist        []LANAllowlistEntry
	LANPasscodeSet      bool
}

// LANDropDeviceView summarizes a connected LAN/local device for /lan-drop.
type LANDropDeviceView struct {
	Addr      string `json:"addr"`
	Label     string `json:"label"`
	Online    bool   `json:"online"`
	IsRemote  bool   `json:"is_remote"`
	PostCount int    `json:"post_count"`
	LastSeen  string `json:"last_seen,omitempty"`
}

// LANDropPostView represents a message or file drop on /lan-drop.
type LANDropPostView struct {
	ID         string `json:"id"`
	Author     string `json:"author"`
	Category   string `json:"category"`
	Message    string `json:"message"`
	FileName   string `json:"file_name,omitempty"`
	FileURL    string `json:"file_url,omitempty"`
	FileSize   string `json:"file_size,omitempty"`
	RemoteAddr string `json:"remote_addr,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// WikipediaOptionView decorates a NOMAD Wikipedia option with local install status.
type WikipediaOptionView struct {
	kiwix.WikipediaOption
	Installed bool
}

// NomadTierView decorates a NOMAD tier with exact 1-2 word bundle tags for the UI.
type NomadTierView struct {
	kiwix.NomadTier
	BundleTags    []string
	BundleTagsCSV string
}

// NomadCollectionView wraps a NOMAD collection and its 3 reactive tiers.
type NomadCollectionView struct {
	Slug        string
	Name        string
	Description string
	SizeRange   string
	Tiers       []NomadTierView
}

// MapRegionView decorates a NOMAD US map region with local download counts.
type MapRegionView struct {
	kiwix.MapRegion
	DownloadedCount int
	AllDownloaded   bool
}

// Server handles Ark web UI and API routing
type Server struct {
	db              *sql.DB
	dataDir         string
	templates       map[string]*template.Template
	docker          *docker.Client
	proxy           *apps.ProxyManager
	queue           *jobs.QueueManager
	sessionStart    time.Time
	lanClients      sync.Map
	instanceID      string
	discoveredPeers sync.Map
	peerByID        sync.Map
	onShutdown      func()
	lanRateLimiter  *ipRateLimiter
	pinFailLimiter  *ipRateLimiter
}

// SetOnShutdown registers a callback used by /settings/restart and /settings/shutdown to trigger graceful exit.
func (s *Server) SetOnShutdown(fn func()) {
	s.onShutdown = fn
}

// NewServer initializes templates and returns a configured Server
func NewServer(db *sql.DB, dataDir string) (*Server, error) {
	pages := []string{
		"home.html",
		"library.html",
		"kiwix.html",
		"get_content.html",
		"field_guide.html",
		"apps.html",
		"ask.html",
		"system.html",
		"settings.html",
		"maps.html",
		"mesh.html",
		"lan_drop.html",
		"tools.html",
	}

	funcMap := template.FuncMap{
		"appIcon": renderAppIconSVG,
	}

	tmplMap := make(map[string]*template.Template)
	for _, page := range pages {
		tmpl, err := template.New("layout.html").Funcs(funcMap).ParseFS(embeddedFiles, "templates/layout.html", "templates/"+page)
		if err != nil {
			return nil, fmt.Errorf("failed to parse template layout with %s: %w", page, err)
		}
		tmplMap[page] = tmpl
	}

	if db != nil {
		// Ensure removed catalog modules (qdrant, ollama, kiwix) are purged from app_states
		_, _ = db.Exec(`DELETE FROM app_states WHERE id IN ('qdrant', 'ollama', 'kiwix')`)
	}

	s := &Server{
		db:             db,
		dataDir:        dataDir,
		templates:      tmplMap,
		proxy:          apps.NewProxyManager(),
		sessionStart:   time.Now().UTC().Truncate(time.Second),
		instanceID:     generateInstanceID(),
		lanRateLimiter: newIPRateLimiter(),
		pinFailLimiter: newIPRateLimiter(),
	}
	s.seedDefaultApps()
	s.ensureLANDropTable()
	s.ensureMeshTables()
	s.ensureSecurityTables()
	return s, nil
}

func (s *Server) seedDefaultApps() {
	if s.db == nil {
		return
	}
	var seeded string
	_ = s.db.QueryRow(`SELECT value FROM settings WHERE key = 'apps_seeded'`).Scan(&seeded)
	if seeded == "1" {
		return
	}
	catDir := findCatalogDir()
	manifests, err := apps.LoadAllManifests(catDir)
	if err != nil || len(manifests) == 0 {
		manifests, _ = apps.LoadAllManifestsFS(catalog.FS)
	}
	for _, m := range manifests {
		if m.ID == "qdrant" || m.ID == "ollama" || m.ID == "kiwix" {
			continue
		}
		_, _ = s.db.Exec(`
			INSERT OR IGNORE INTO app_states (id, name, category, image, status, proxy_mode, auto_start, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'stopped', ?, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		`, m.ID, m.Name, m.Category, m.Image, m.Proxy)
	}
	_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('apps_seeded', '1')`)
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

	// Static assets (with graceful empty-protobuf fallback for unbundled map font ranges offline)
	staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/vendor/maps/fonts/") && strings.HasSuffix(r.URL.Path, ".pbf") {
			rel := strings.TrimPrefix(r.URL.Path, "/static/")
			if _, statErr := fs.Stat(staticFS, rel); statErr != nil {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("Cache-Control", "public, max-age=86400")
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		staticHandler.ServeHTTP(w, r)
	})
	if s.dataDir != "" {
		mux.Handle("/media/", http.StripPrefix("/media/", http.FileServer(http.Dir(filepath.Join(s.dataDir, "media")))))
	}

	// Health check & LAN Auth endpoints
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/lan-auth", s.handleLANAuth)

	// Pages
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/library", s.handleLibrary)
	mux.HandleFunc("/kiwix", s.handleKiwixCatalogPage)
	mux.HandleFunc("/get-content", s.handleGetContent)
	mux.HandleFunc("/field-guide", s.handleFieldGuidePage)
	mux.HandleFunc("/apps", s.handleApps)
	mux.HandleFunc("/ask", s.handleAsk)
	mux.HandleFunc("/jobs", s.localOnly(s.handleJobsRedirect))
	mux.HandleFunc("/system", s.localOnly(s.handleSystem))
	mux.HandleFunc("/settings", s.localOnly(s.handleSettings))
	mux.HandleFunc("/settings/general", s.localOnly(s.handleSettingsGeneral))
	mux.HandleFunc("/settings/lan-allowlist/remove", s.localOnly(s.handleSettingsAllowlistRemove))
	mux.HandleFunc("/settings/lan-allowlist/add", s.localOnly(s.handleSettingsAllowlistAdd))
	mux.HandleFunc("/settings/ai", s.localOnly(s.handleSettingsAI))
	mux.HandleFunc("/settings/downloads", s.localOnly(s.handleSettingsDownloads))
	mux.HandleFunc("/settings/usb-export", s.localOnly(s.handleSettingsUSBExport))
	mux.HandleFunc("/settings/usb-profile", s.localOnly(s.handleSettingsUSBProfile))
	mux.HandleFunc("/settings/update", s.localOnly(s.handleSettingsUpdate))
	mux.HandleFunc("/settings/update-github", s.localOnly(s.handleSettingsUpdateGitHub))
	mux.HandleFunc("/settings/rollback", s.localOnly(s.handleSettingsRollback))
	mux.HandleFunc("/settings/restart", s.localOnly(s.handleSettingsRestart))
	mux.HandleFunc("/settings/shutdown", s.localOnly(s.handleSettingsShutdown))
	mux.HandleFunc("/maps", s.handleMapsViewer)
	mux.HandleFunc("/mesh", s.handleMeshPage)
	mux.HandleFunc("/maps/files/", s.handleMapsFiles)
	mux.HandleFunc("/maps/tiles/", s.handleUnifiedMapTiles)
	mux.HandleFunc("/read/zim", s.handleZIMReader)
	mux.HandleFunc("/read/zim/", s.handleZIMReader)
	mux.HandleFunc("/tools", s.handleToolsPage)
	mux.HandleFunc("/lan-drop", s.handleLANDropPage)
	mux.HandleFunc("/opds", s.handleOPDSRouter)
	mux.HandleFunc("/opds/", s.handleOPDSRouter)

	// API endpoints
	mux.HandleFunc("/api/jobs", s.handleAPIJobs)
	mux.HandleFunc("/api/jobs/cancel", s.localOnly(s.handleAPIJobCancel))
	mux.HandleFunc("/api/jobs/retry", s.localOnly(s.handleAPIJobRetry))
	mux.HandleFunc("/api/jobs/delete", s.localOnly(s.handleAPIJobDelete))
	mux.HandleFunc("/api/jobs/status", s.handleAPIJobStatus)
	mux.HandleFunc("/api/kiwix/catalog", s.handleAPIKiwixCatalog)
	mux.HandleFunc("/api/library/delete", s.localOnly(s.handleAPILibraryDelete))
	mux.HandleFunc("/api/library/updates", s.handleAPILibraryUpdates)
	mux.HandleFunc("/api/library/icon", s.handleAPILibraryIcon)
	mux.HandleFunc("/api/storage/check-space", s.handleAPIStorageCheckSpace)
	mux.HandleFunc("/api/update/check", s.handleAPIUpdateCheck)
	mux.HandleFunc("/api/apps/install", s.localOnly(s.handleAPIAppInstall))
	mux.HandleFunc("/api/apps/start", s.localOnly(s.handleAPIAppStart))
	mux.HandleFunc("/api/apps/stop", s.localOnly(s.handleAPIAppStop))
	mux.HandleFunc("/api/apps/uninstall", s.localOnly(s.handleAPIAppUninstall))
	mux.HandleFunc("/api/apps/custom", s.localOnly(s.handleAPICustomAppSave))
	mux.HandleFunc("/api/apps/update", s.localOnly(s.handleAPIAppUpdateImage))
	mux.HandleFunc("/api/system/sse", s.localOnly(system.NewSSEFeed(s.dataDir).ServeHTTP))
	mux.HandleFunc("/api/system/runtime", s.localOnly(s.handleAPISystemRuntime))
	mux.HandleFunc("/api/system/time", s.localOnly(s.handleAPISystemTime))
	mux.HandleFunc("/api/system/install-runtime", s.localOnly(s.handleAPISystemInstallRuntime))
	mux.HandleFunc("/api/zim/suggest", s.handleAPIZIMSuggest)
	mux.HandleFunc("/api/zim/pages", s.handleAPIZIMPages)
	mux.HandleFunc("/api/lan-drop/posts", s.handleAPILANDropPosts)
	mux.HandleFunc("/api/lan-drop/post", s.handleAPILANDropPost)
	mux.HandleFunc("/api/lan-drop/delete", s.localOnly(s.handleAPILANDropDelete))
	mux.HandleFunc("/api/lan-drop/file/", s.handleAPILANDropFile)
	mux.HandleFunc("/api/containers/restart", s.localOnly(s.handleAPIContainerRestart))
	mux.HandleFunc("/api/containers/logs", s.localOnly(s.handleAPIContainerLogs))
	mux.HandleFunc("/api/ai/detect", s.handleAPIAIDetect)
	mux.HandleFunc("/api/ai/test", s.handleAPIAITest)
	mux.HandleFunc("/api/ai/status", s.handleAPIAIStatus)
	mux.HandleFunc("/api/ai/chat", s.handleAPIAIChat)
	mux.HandleFunc("/api/ai/sessions", s.handleAPIAISessions)
	mux.HandleFunc("/api/ai/sessions/get", s.handleAPIAISessionGet)
	mux.HandleFunc("/api/ai/sessions/pin", s.handleAPIAISessionPin)
	mux.HandleFunc("/api/ai/sessions/delete", s.localOnly(s.handleAPIAISessionDelete))
	mux.HandleFunc("/api/ai/index", s.localOnly(s.handleAPIAIIndex))
	mux.HandleFunc("/api/ai/index-article", s.localOnly(s.handleAPIAIIndexArticle))
	mux.HandleFunc("/api/ai/knowledge/remove", s.localOnly(s.handleAPIAIKnowledgeRemove))
	mux.HandleFunc("/api/ai/models", s.handleAPIAIModels)
	mux.HandleFunc("/api/ai/models/pull", s.localOnly(s.handleAPIAIModelPull))
	mux.HandleFunc("/api/ai/models/delete", s.localOnly(s.handleAPIAIModelDelete))
	mux.HandleFunc("/api/ai/models/select", s.localOnly(s.handleAPIAIModelSelect))
	mux.HandleFunc("/api/maps/extract", s.localOnly(s.handleAPIMapsExtract))
	mux.HandleFunc("/api/maps/pins", s.handleAPIMapPins)
	mux.HandleFunc("/api/maps/tilejson", s.handleAPIMapsTileJSON)
	mux.HandleFunc("/api/reader/theme", s.handleAPIReaderTheme)
	mux.HandleFunc("/api/peers/catalog", s.handleAPIPeersCatalog)
	mux.HandleFunc("/api/peers/download", s.handleAPIPeersDownload)
	mux.HandleFunc("/api/peers/list", s.handleAPIPeersList)
	mux.HandleFunc("/api/peers/pull", s.localOnly(s.handleAPIPeersPull))
	mux.HandleFunc("/api/mesh/state", s.handleAPIMeshState)
	mux.HandleFunc("/api/mesh/message", s.localOnly(s.handleAPIMeshMessage))
	mux.HandleFunc("/api/mesh/node", s.localOnly(s.handleAPIMeshNode))
	mux.HandleFunc("/api/mesh/clear", s.localOnly(s.handleAPIMeshClear))
	mux.HandleFunc("/peer/", s.handlePeerProxy)

	// App reverse proxy
	if s.proxy != nil {
		mux.Handle("/a/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/a/maps" || r.URL.Path == "/a/maps/" {
				http.Redirect(w, r, "/maps", http.StatusSeeOther)
				return
			}
			if r.URL.Path == "/a/meshtastic" || r.URL.Path == "/a/meshtastic/" || r.URL.Path == "/a/mesh" || r.URL.Path == "/a/mesh/" {
				http.Redirect(w, r, "/mesh", http.StatusSeeOther)
				return
			}
			if r.URL.Path == "/a/kiwix" || strings.HasPrefix(r.URL.Path, "/a/kiwix/") {
				http.Redirect(w, r, "/library", http.StatusSeeOther)
				return
			}
			s.proxy.ServeHTTP(w, r)
		}))
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
	s.trackLANClient(r)

	installedApps, _ := s.fetchApps()

	data := PageData{
		Title:         "Home",
		ActiveNav:     "home",
		InstalledApps: installedApps,
		FlashErr:      r.URL.Query().Get("err"),
	}
	s.render(w, r, "home.html", data)
}

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	cat := strings.TrimSpace(r.URL.Query().Get("cat"))
	items := s.scanDiskLibraryItems(q, cat)
	data := PageData{
		Title:            "Library",
		ActiveNav:        "library",
		Items:            items,
		InitialQuery:     q,
		SelectedCategory: cat,
		FlashMsg:         strings.TrimSpace(r.URL.Query().Get("msg")),
		FlashErr:         strings.TrimSpace(r.URL.Query().Get("err")),
	}
	s.render(w, r, "library.html", data)
}

func (s *Server) handleGetContent(w http.ResponseWriter, r *http.Request) {
	zimNames := s.listFileNamesLower(filepath.Join(s.dataDir, "zim"))
	mapNames := s.listFileNamesLower(filepath.Join(s.dataDir, "maps"))

	var wikiViews []WikipediaOptionView
	for _, opt := range kiwix.GetWikipediaOptions() {
		installed := false
		for _, fn := range zimNames {
			if strings.HasPrefix(fn, strings.ToLower(opt.MatchPrefix)) {
				installed = true
				break
			}
		}
		wikiViews = append(wikiViews, WikipediaOptionView{
			WikipediaOption: opt,
			Installed:       installed,
		})
	}

	var colViews []NomadCollectionView
	for _, col := range kiwix.GetNomadCollections() {
		cv := NomadCollectionView{
			Slug:        col.Slug,
			Name:        col.Name,
			Description: col.Description,
			SizeRange:   col.SizeRange,
		}
		for _, t := range col.Tiers {
			tags := kiwix.CumulativeTierLabels(t.Slug)
			cv.Tiers = append(cv.Tiers, NomadTierView{
				NomadTier:     t,
				BundleTags:    tags,
				BundleTagsCSV: strings.Join(tags, "|"),
			})
		}
		colViews = append(colViews, cv)
	}

	var regViews []MapRegionView
	for _, reg := range kiwix.GetNomadMapRegions() {
		dlCount := 0
		for _, res := range reg.Resources {
			prefix := strings.ToLower(res.ID) + "_"
			for _, fn := range mapNames {
				if strings.HasPrefix(fn, prefix) && strings.HasSuffix(fn, ".pmtiles") {
					dlCount++
					break
				}
			}
		}
		regViews = append(regViews, MapRegionView{
			MapRegion:       reg,
			DownloadedCount: dlCount,
			AllDownloaded:   reg.ItemCount > 0 && dlCount >= reg.ItemCount,
		})
	}

	data := PageData{
		Title:             "Get Content",
		ActiveNav:         "get-content",
		WikipediaOptions:  wikiViews,
		NomadCollections:  colViews,
		MapRegions:        regViews,
		CreatorVideoPacks: kiwix.GetVideoCreatorPacks(),
	}
	s.render(w, r, "get_content.html", data)
}

func (s *Server) listFileNamesLower(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, strings.ToLower(e.Name()))
		}
	}
	return out
}

func (s *Server) runtimeStatus(ctx context.Context) (online bool, label string, canInstall bool) {
	if s.docker != nil && s.docker.IsAvailable(ctx) {
		rt := s.docker.RuntimeName()
		if rt == "" || rt == "None" {
			rt = "Docker"
		}
		return true, rt + " Online", false
	}
	return false, "Native Mode", docker.CanInstallPodmanRuntime()
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	installedApps, availableApps := s.fetchApps()
	online, label, canInstall := s.runtimeStatus(r.Context())
	data := PageData{
		Title:             "Apps",
		ActiveNav:         "apps",
		InstalledApps:     installedApps,
		AvailableApps:     availableApps,
		DockerOnline:      online,
		RuntimeLabel:      label,
		CanInstallRuntime: canInstall,
		FlashMsg:          strings.TrimSpace(r.URL.Query().Get("msg")),
		FlashErr:          strings.TrimSpace(r.URL.Query().Get("err")),
	}
	s.render(w, r, "apps.html", data)
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	settingsMap := s.fetchSettings()
	conn := ai.LoadConnFromDB(r.Context(), s.db)
	if conn.BaseURL == "" {
		conn.BaseURL = settingsMap["ai_server_url"]
	}
	if conn.ChatModel == "" {
		conn.ChatModel = settingsMap["ai_chat_model"]
	}
	if s.db != nil && s.dataDir != "" {
		s.syncAutoIndexedDirs(r.Context(), conn)
	}
	ragDocs, _ := ai.ListDocuments(r.Context(), s.db)

	status := conn.Status(r.Context())
	statusBytes, _ := json.Marshal(status)

	preZIM := strings.TrimSpace(r.URL.Query().Get("zim"))
	preArt := strings.TrimSpace(r.URL.Query().Get("article"))
	preQ := strings.TrimSpace(r.URL.Query().Get("q"))
	var preTitle string
	if preZIM != "" && preArt != "" {
		if t, _, _, ok := s.readZIMArticlePlainText(preZIM, preArt, 200); ok {
			preTitle = t
		}
	}

	data := PageData{
		Title:             "Ask AI",
		ActiveNav:         "ask",
		AIConnected:       settingsMap["ai_server_url"] != "",
		AIStatus:          status,
		AIStatusJSON:      template.JS(statusBytes),
		AIContextMax:      status.ContextLength,
		AskPreloadZIM:     preZIM,
		AskPreloadArticle: preArt,
		AskPreloadTitle:   preTitle,
		AskPreloadQuery:   preQ,
		RAGDocs:           ragDocs,
		Settings:          settingsMap,
		FlashMsg:          strings.TrimSpace(r.URL.Query().Get("msg")),
		FlashErr:          strings.TrimSpace(r.URL.Query().Get("err")),
	}
	s.render(w, r, "ask.html", data)
}

func (s *Server) handleJobsRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/system", http.StatusSeeOther)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	freeGB, totalGB, usedGB, pct := s.getStorageStats(r.Context())
	flash := strings.TrimSpace(r.URL.Query().Get("msg"))
	if r.URL.Query().Get("saved") == "1" && flash == "" {
		flash = "Settings saved."
	}
	settingsMap := s.fetchSettings()
	data := PageData{
		Title:              "Settings",
		ActiveNav:          "settings",
		StorageFreeGB:      freeGB,
		StorageTotalGB:     totalGB,
		StorageUsedGB:      usedGB,
		StorageUsedPercent: pct,
		Settings:           settingsMap,
		LANAllowlist:       s.listAllowlistEntries(),
		LANPasscodeSet:     settingsMap["lan_passcode_hash"] != "",
		FlashMsg:           flash,
		FlashErr:           strings.TrimSpace(r.URL.Query().Get("err")),
		ArkVersion:         update.ArkVersion,
		GitHubRepo:         s.resolveGitHubRepo(),
	}
	s.populatePhase3Settings(r.Context(), &data)
	s.render(w, r, "settings.html", data)
}

func (s *Server) handleAPIJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		jobsList := s.fetchRecentSessionJobs(50)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobs": jobsList})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden: downloads are only allowed from the local host"})
		return
	}

	rawType := r.FormValue("type")
	target := r.FormValue("target")
	if rawType == "" || target == "" {
		http.Error(w, "Missing type or target", http.StatusBadRequest)
		return
	}

	var sizeHint int64
	if sb := strings.TrimSpace(r.FormValue("size_bytes")); sb != "" {
		_, _ = fmt.Sscanf(sb, "%d", &sizeHint)
	}
	if spaceRes, ok := s.checkDownloadSpace(r.Context(), rawType, target, sizeHint); ok && spaceRes.Status == "insufficient" {
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInsufficientStorage)
			_ = json.NewEncoder(w).Encode(spaceRes)
			return
		}
		redir := sanitizeLocalRedirect(r.FormValue("redirect"))
		sep := "?"
		if strings.Contains(redir, "?") {
			sep = "&"
		}
		http.Redirect(w, r, redir+sep+"err="+url.QueryEscape(spaceRes.Message), http.StatusSeeOther)
		return
	}

	jobType := rawType
	meta := jobs.JobMetadata{
		OutputDir: filepath.Join(s.dataDir, "zim"),
	}

	switch rawType {
	case "preset":
		jobType = jobs.TypePreset
		if strings.HasPrefix(strings.ToLower(target), "map-") || strings.HasPrefix(strings.ToLower(target), "maps-") {
			meta.OutputDir = filepath.Join(s.dataDir, "maps")
		} else {
			meta.OutputDir = filepath.Join(s.dataDir, "zim")
		}
	case "kiwix_zim":
		jobType = jobs.TypeDownload
		meta.OutputDir = filepath.Join(s.dataDir, "zim")
	case "map_pmtiles":
		jobType = jobs.TypeDownload
		meta.OutputDir = filepath.Join(s.dataDir, "maps")
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
	jobID := fmt.Sprintf("job-%d-%d", time.Now().UnixNano(), jobSeq.Add(1))

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

	if strings.Contains(r.Header.Get("Accept"), "application/json") || r.FormValue("ajax") == "1" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"job_id": jobID,
			"target": target,
			"status": "pending",
		})
		return
	}

	redirect := sanitizeLocalRedirect(r.FormValue("redirect"))
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func sanitizeLocalRedirect(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/system"
	}
	return raw
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
	http.Redirect(w, r, sanitizeLocalRedirect(r.FormValue("redirect")), http.StatusSeeOther)
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
	http.Redirect(w, r, sanitizeLocalRedirect(r.FormValue("redirect")), http.StatusSeeOther)
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
	http.Redirect(w, r, sanitizeLocalRedirect(r.FormValue("redirect")), http.StatusSeeOther)
}

func (s *Server) handleAPIJobStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	jobID := strings.TrimSpace(r.URL.Query().Get("id"))
	if jobID == "" || s.db == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "job id is required"})
		return
	}
	var st, msg, errMsg string
	var prog float64
	err := s.db.QueryRowContext(r.Context(), `
		SELECT status, progress, COALESCE(progress_msg, ''), COALESCE(error_msg, '')
		FROM jobs WHERE id = ?
	`, jobID).Scan(&st, &prog, &msg, &errMsg)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "job not found"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"id":           jobID,
		"status":       st,
		"progress":     prog,
		"progress_msg": msg,
		"error_msg":    errMsg,
	})
}

func (s *Server) handleAPIKiwixCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	lang := strings.TrimSpace(r.URL.Query().Get("lang"))
	if lang == "" {
		lang = "eng"
	}
	if lang == "all" {
		lang = ""
	}
	category := strings.TrimSpace(r.URL.Query().Get("category"))
	if category == "all" {
		category = ""
	}
	flavour := strings.TrimSpace(r.URL.Query().Get("flavour"))
	if flavour == "all" {
		flavour = ""
	}
	offset := 0
	if rawOffset := strings.TrimSpace(r.URL.Query().Get("offset")); rawOffset != "" {
		var parsed int
		if _, err := fmt.Sscanf(rawOffset, "%d", &parsed); err == nil && parsed >= 0 {
			offset = parsed
		}
	}
	limit := 24
	if rawCount := strings.TrimSpace(r.URL.Query().Get("count")); rawCount != "" {
		var parsed int
		if _, err := fmt.Sscanf(rawCount, "%d", &parsed); err == nil && parsed > 0 && parsed <= 50 {
			limit = parsed
		}
	}

	baseURL := kiwix.DefaultCatalogURL
	if customFeed := strings.TrimSpace(r.URL.Query().Get("feed_url")); customFeed != "" && (strings.HasPrefix(customFeed, "http://") || strings.HasPrefix(customFeed, "https://")) {
		baseURL = customFeed
		if s.db != nil {
			_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('custom_opds_url', ?)`, customFeed)
		}
	} else if override := strings.TrimSpace(os.Getenv("ARK_KIWIX_OPDS_URL")); override != "" {
		baseURL = override
	}

	client := &http.Client{Timeout: 12 * time.Second}
	feed, err := kiwix.FetchOPDSFeed(r.Context(), client, baseURL, kiwix.FilterParams{
		Query:    q,
		Language: lang,
		Category: category,
		Flavour:  flavour,
		Offset:   offset,
		Limit:    limit,
	})
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "Could not reach library.kiwix.org: " + err.Error(),
			"items": []any{},
		})
		return
	}

	installedFiles := make(map[string]bool)
	for _, fn := range s.listFileNamesLower(filepath.Join(s.dataDir, "zim")) {
		installedFiles[fn] = true
	}

	type kiwixEntryJSON struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Summary     string `json:"summary"`
		Language    string `json:"language"`
		Category    string `json:"category"`
		Flavour     string `json:"flavour"`
		Updated     string `json:"updated"`
		SizeBytes   int64  `json:"size_bytes"`
		SizeText    string `json:"size_text"`
		DownloadURL string `json:"download_url"`
		Installed   bool   `json:"installed"`
	}

	items := make([]kiwixEntryJSON, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		dlURL := e.DirectZimURL()
		if dlURL == "" {
			continue
		}
		sz := e.SizeBytes()
		szText := "Unknown"
		if sz > 0 {
			szText = formatHumanBytes(sz)
		}
		base := strings.ToLower(filepath.Base(dlURL))
		items = append(items, kiwixEntryJSON{
			ID:          e.ID,
			Title:       e.Title,
			Summary:     e.Summary,
			Language:    e.Language,
			Category:    e.Category,
			Flavour:     e.Flavour,
			Updated:     strings.Split(e.Updated, "T")[0],
			SizeBytes:   sz,
			SizeText:    szText,
			DownloadURL: dlURL,
			Installed:   installedFiles[base],
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"total":    feed.TotalResults,
		"offset":   offset,
		"limit":    limit,
		"has_more": offset+len(items) < feed.TotalResults,
		"items":    items,
	})
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

	m, err := s.loadAnyManifest(appID)
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
		// Container startup creates an empty ~36 KB schema file; only skip if a populated DB (>1 MB) exists.
		if info, statErr := os.Stat(dbFile); statErr != nil || info.Size() < 1<<20 {
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
	if s.docker == nil {
		return nil
	}
	if !s.docker.IsAvailable(ctx) {
		return fmt.Errorf("Docker engine is not running — start Docker Desktop first")
	}
	m, err := s.loadAnyManifest(appID)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	if appID == "kiwix" {
		zimDir := filepath.Join(s.dataDir, "zim")
		_ = os.MkdirAll(zimDir, 0755)
		libFile := filepath.Join(zimDir, "library.xml")
		if _, statErr := os.Stat(libFile); os.IsNotExist(statErr) {
			_ = os.WriteFile(libFile, []byte("<library version=\"20110515\"/>\n"), 0644)
		}
	}
	info, err := s.docker.EnsureRunning(ctx, m, s.dataDir)
	if err != nil {
		return fmt.Errorf("ensure running: %w", err)
	}
	if s.proxy != nil {
		var proxyTarget string
		if os.Getenv("ARK_IN_CONTAINER") == "1" && m.Proxy == "path" {
			proxyTarget = fmt.Sprintf("http://ark-%s:%d", appID, m.Port)
		} else if info.HostPort > 0 {
			proxyTarget = fmt.Sprintf("http://127.0.0.1:%d", info.HostPort)
		}
		if proxyTarget != "" {
			if target, err := url.Parse(proxyTarget); err == nil {
				stripPrefix := m.ID != "kiwix"
				s.proxy.Register(m.ID, target, stripPrefix)
			}
		}
	}
	if s.db != nil {
		_, _ = s.db.Exec("UPDATE app_states SET image = ?, container_id = ?, port = ?, proxy_mode = ?, status = 'running', auto_start = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?", m.Image, info.ID, info.HostPort, m.Proxy, appID)
		if appID == "ollama" && info.HostPort > 0 {
			var curAI string
			_ = s.db.QueryRow(`SELECT value FROM settings WHERE key = 'ai_server_url'`).Scan(&curAI)
			if strings.TrimSpace(curAI) == "" {
				_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('ai_server_url', ?)`, fmt.Sprintf("http://127.0.0.1:%d", info.HostPort))
			}
		}
	}
	return nil
}

// RestoreRunningApps starts installed apps whose persisted desired state is running (auto_start = 1) and wires their proxies.
func (s *Server) RestoreRunningApps(ctx context.Context) {
	ids := s.appsToRestore()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(appID string) {
			defer wg.Done()
			if err := s.startAppContainer(ctx, appID); err != nil {
				log.Printf("[Ark] Failed to restore app %s: %v", appID, err)
			}
		}(id)
	}
	wg.Wait()
}

func (s *Server) appsToRestore() []string {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.Query("SELECT id FROM app_states WHERE auto_start = 1")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Server) handleAPIAppStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	appID := r.FormValue("id")
	redirect := r.FormValue("redirect")
	if redirect == "" {
		redirect = "/apps"
	} else {
		redirect = sanitizeLocalRedirect(redirect)
	}
	if appID != "" {
		if err := s.startAppContainer(context.Background(), appID); err != nil {
			log.Printf("[Ark] Failed to start app %s: %v", appID, err)
			if s.db != nil {
				_, _ = s.db.Exec("UPDATE app_states SET status = 'stopped', updated_at = CURRENT_TIMESTAMP WHERE id = ?", appID)
			}
			sep := "?"
			if strings.Contains(redirect, "?") {
				sep = "&"
			}
			http.Redirect(w, r, redirect+sep+"err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
		if s.docker == nil && s.db != nil {
			_, _ = s.db.Exec("UPDATE app_states SET status = 'running', auto_start = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?", appID)
		}
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (s *Server) handleAPIAppStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	appID := r.FormValue("id")
	if appID != "" {
		if s.docker != nil && s.docker.IsAvailable(r.Context()) {
			if err := s.docker.Stop(r.Context(), appID, 2); err != nil {
				log.Printf("[Ark] Failed to stop container %s: %v", appID, err)
			}
		}
		if s.proxy != nil {
			s.proxy.Unregister(appID)
		}
		if s.db != nil {
			_, _ = s.db.Exec("UPDATE app_states SET status = 'stopped', auto_start = 0, updated_at = CURRENT_TIMESTAMP WHERE id = ?", appID)
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
		if r.FormValue("delete_custom") == "1" {
			_ = apps.DeleteCustomManifest(s.dataDir, appID)
		}
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

func (s *Server) fetchApps() ([]AppView, []AppView) {
	catDir := findCatalogDir()
	manifests, err := apps.LoadAllManifests(catDir)
	if err != nil || len(manifests) == 0 {
		manifests, _ = apps.LoadAllManifestsFS(catalog.FS)
	}
	customs := apps.LoadCustomManifests(s.dataDir)
	customSet := make(map[string]bool)
	for _, cm := range customs {
		customSet[cm.ID] = true
		manifests = append(manifests, cm)
	}

	type appStateRow struct {
		status   string
		hostPort int
	}
	installedMap := make(map[string]appStateRow)
	if s.db != nil {
		rows, err := s.db.Query(`SELECT id, status, COALESCE(port, 0) FROM app_states`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id, status string
				var hostPort int
				if err := rows.Scan(&id, &status, &hostPort); err == nil {
					installedMap[id] = appStateRow{status: status, hostPort: hostPort}
				}
			}
		}
	}

	// Reconcile DB statuses and ports against live Docker containers when a Docker client is attached
	liveChecked := false
	liveMap := make(map[string]docker.ContainerInfo)
	if s.docker != nil {
		dCtx, dCancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
		defer dCancel()
		if s.docker.IsAvailable(dCtx) {
			if list, err := s.docker.ListArkContainers(dCtx); err == nil {
				liveChecked = true
				for _, c := range list {
					liveMap[c.AppID] = c
				}
			}
		} else {
			// Docker engine offline -> no container can currently be running
			liveChecked = true
		}
	}

	var installed []AppView
	var available []AppView

	for _, m := range manifests {
		if m.ID == "qdrant" || m.ID == "ollama" || m.ID == "kiwix" {
			continue
		}
		state, found := installedMap[m.ID]
		effectiveStatus := state.status
		effectivePort := state.hostPort
		if found && liveChecked {
			if c, ok := liveMap[m.ID]; ok && c.Status == "running" {
				effectiveStatus = "running"
				if c.HostPort > 0 {
					effectivePort = c.HostPort
				}
				if s.proxy != nil {
					if _, has := s.proxy.GetRoute(m.ID); !has {
						var proxyTarget string
						if os.Getenv("ARK_IN_CONTAINER") == "1" && m.Proxy == "path" {
							proxyTarget = fmt.Sprintf("http://ark-%s:%d", m.ID, m.Port)
						} else if effectivePort > 0 {
							proxyTarget = fmt.Sprintf("http://127.0.0.1:%d", effectivePort)
						}
						if proxyTarget != "" {
							if target, perr := url.Parse(proxyTarget); perr == nil {
								s.proxy.Register(m.ID, target, m.ID != "kiwix")
							}
						}
					}
				}
			} else if state.status == "running" {
				effectiveStatus = "stopped"
				if s.db != nil {
					_, _ = s.db.Exec(`UPDATE app_states SET status = 'stopped', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, m.ID)
				}
			}
		}

		app := AppView{
			ID:          m.ID,
			Name:        m.Name,
			Category:    m.Category,
			Description: m.Description,
			Image:       m.Image,
			Port:        m.Port,
			Status:      "not_installed",
			ProxyMode:   m.Proxy,
			OpenURL:     resolveAppOpenURL(m, effectivePort),
			IsCustom:    customSet[m.ID],
		}

		if m.ID == "meshtastic" {
			app.Status = "running"
			app.ProxyMode = "native"
			app.OpenURL = "/mesh"
			installed = append(installed, app)
		} else if found {
			app.Status = effectiveStatus
			if effectiveStatus == "running" || effectiveStatus == "stopped" {
				installed = append(installed, app)
			}
		}
		available = append(available, app)
	}

	return installed, available
}

func resolveAppOpenURL(m *apps.AppManifest, hostPort int) string {
	if m.OpenURL != "" {
		return m.OpenURL
	}
	suffix := m.OpenPath
	if suffix == "" {
		suffix = "/"
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	if m.Proxy == "port" {
		p := hostPort
		if p <= 0 {
			p = m.Port
		}
		if p > 0 {
			return fmt.Sprintf("http://localhost:%d%s", p, suffix)
		}
	}
	return fmt.Sprintf("/a/%s%s", m.ID, suffix)
}

func (s *Server) getStorageStats(ctx context.Context) (uint64, uint64, uint64, uint64) {
	freeGB, totalGB, usedGB, pct, _, _, _ := s.getStorageBreakdown(ctx)
	return freeGB, totalGB, usedGB, pct
}

func (s *Server) getStorageBreakdown(ctx context.Context) (freeGB, totalGB, driveUsedGB, usedPct, arkPct, otherPct uint64, arkText string) {
	if s.dataDir == "" {
		return 0, 0, 0, 0, 0, 0, "0 B"
	}
	report, err := storage.GetDiskUsage(ctx, s.dataDir)
	if err != nil || report == nil {
		return 0, 0, 0, 0, 0, 0, "0 B"
	}
	const gb = 1024 * 1024 * 1024
	freeGB = uint64(report.TotalFreeBytes / gb)
	totalGB = uint64(report.DiskTotalBytes / gb)

	var driveUsedBytes int64
	if report.DiskTotalBytes > report.TotalFreeBytes {
		driveUsedBytes = report.DiskTotalBytes - report.TotalFreeBytes
	}
	driveUsedGB = uint64(driveUsedBytes / gb)
	arkText = formatHumanBytes(report.TotalUsedBytes)

	if report.DiskTotalBytes > 0 {
		usedPct = uint64((driveUsedBytes * 100) / report.DiskTotalBytes)
		if usedPct > 100 {
			usedPct = 100
		}
		arkPct = uint64((report.TotalUsedBytes * 100) / report.DiskTotalBytes)
		if report.TotalUsedBytes > 0 && arkPct == 0 {
			arkPct = 1
		}
		if arkPct > usedPct {
			arkPct = usedPct
		}
		otherPct = usedPct - arkPct
	}
	return freeGB, totalGB, driveUsedGB, usedPct, arkPct, otherPct, arkText
}

func renderAppIconSVG(appID string) template.HTML {
	var body string
	switch appID {
	case "kiwix":
		body = `<path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/><path d="M9 7h7M9 11h5"/>`
	case "convertx":
		body = `<polyline points="17 1 21 5 17 9"/><path d="M3 11V9a4 4 0 0 1 4-4h14"/><polyline points="7 23 3 19 7 15"/><path d="M21 13v2a4 4 0 0 1-4 4H3"/>`
	case "cyberchef":
		body = `<path d="M9 3h6v4l4.5 9A2 2 0 0 1 17.7 19H6.3a2 2 0 0 1-1.8-3L9 7V3z"/><path d="M6 14h12"/>`
	case "druglabels":
		body = `<path d="M10.5 20.5 3.5 13.5a4.95 4.95 0 1 1 7-7l7 7a4.95 4.95 0 1 1-7 7Z"/><line x1="8.5" y1="8.5" x2="15.5" y2="15.5"/>`
	case "flatnotes":
		body = `<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="8" y1="13" x2="16" y2="13"/><line x1="8" y1="17" x2="13" y2="17"/>`
	case "kolibri":
		body = `<path d="M22 10v6M2 10l10-5 10 5-10 5z"/><path d="M6 12v5c3 3 9 3 12 0v-5"/>`
	case "maps":
		body = `<polygon points="1 6 1 22 8 18 16 22 23 18 23 2 16 6 8 2 1 6"/><line x1="8" y1="2" x2="8" y2="18"/><line x1="16" y1="6" x2="16" y2="22"/>`
	case "meshtastic":
		body = `<path d="M4.9 19.1C1 15.2 1 8.8 4.9 4.9"/><path d="M7.8 16.2c-2.3-2.3-2.3-6.1 0-8.5"/><circle cx="12" cy="12" r="2"/><path d="M16.2 7.8c2.3 2.3 2.3 6.1 0 8.5"/><path d="M19.1 4.9C23 8.8 23 15.2 19.1 19.1"/><line x1="12" y1="14" x2="12" y2="22"/>`
	case "ollama":
		body = `<path d="M12 2a4 4 0 0 1 4 4v1a3 3 0 0 1 2.5 3 3 3 0 0 1-1 5.2V18a2 2 0 0 1-2 2H8.5a2 2 0 0 1-2-2v-2.8A3 3 0 0 1 5.5 10 3 3 0 0 1 8 7V6a4 4 0 0 1 4-4z"/><circle cx="9.5" cy="10.5" r="1"/><circle cx="14.5" cy="10.5" r="1"/>`
	case "libretranslate":
		body = `<path d="m5 8 6 6"/><path d="m4 14 6-6 2-3"/><path d="M2 5h12"/><path d="M7 2h1"/><path d="m22 22-5-10-5 10"/><path d="M14 18h6"/>`
	default:
		body = `<path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/><polyline points="3.27 6.96 12 12.01 20.73 6.96"/><line x1="12" y1="22.08" x2="12" y2="12"/>`
	}
	return template.HTML(`<svg class="tile-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">` + body + `</svg>`)
}

func (s *Server) fetchRecentSessionJobs(maxCompleted int) []JobView {
	if s.db == nil {
		return nil
	}
	if maxCompleted <= 0 {
		maxCompleted = 5
	}

	since := s.sessionStart.UTC().Format("2006-01-02 15:04:05")
	rows, err := s.db.Query(`
		SELECT id, type, target, status, progress, COALESCE(progress_msg, ''), COALESCE(error_msg, '')
		FROM jobs
		WHERE (status != 'done' AND datetime(created_at) >= datetime(?))
		   OR status IN ('running', 'pending')
		   OR (status = 'done' AND datetime(COALESCE(completed_at, updated_at, created_at)) >= datetime(?))
		ORDER BY CASE WHEN status IN ('running', 'pending') THEN 0 ELSE 1 END, created_at DESC, rowid DESC
	`, since, since)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var jobsList []JobView
	completedCount := 0
	for rows.Next() {
		var j JobView
		if err := rows.Scan(&j.ID, &j.Type, &j.Target, &j.Status, &j.Progress, &j.ProgressMsg, &j.ErrorMsg); err == nil {
			if j.Status == "done" {
				if completedCount >= maxCompleted {
					continue
				}
				completedCount++
			}
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

func isLocalRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	host := clientHostFromReq(r)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (s *Server) localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLocalRequest(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"ok":false,"error":"forbidden: local host access only"}`))
				return
			}
			http.Error(w, "Forbidden: Settings and System pages are only accessible from the local host.", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, pageTemplate string, data PageData) {
	data.IsLocal = isLocalRequest(r)
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
