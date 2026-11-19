package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"ark/internal/ai"
	"ark/internal/docker"
	"ark/internal/jobs"
	"ark/internal/system"
	"ark/internal/update"
	"ark/internal/usb"
	"ark/internal/web"
)

const initialMigrationSQL = `
-- 0001_init.sql
-- Ark SQLite Initial Schema

PRAGMA foreign_keys = ON;

-- System and application configuration
CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Seed default settings
INSERT OR IGNORE INTO settings (key, value) VALUES
    ('device_name', 'Ark'),
    ('port', '8080'),
    ('lan_enabled', 'true'),
    ('admin_password_hash', ''),
    ('data_dir', './ArkData'),
    ('video_quality', 'balanced'),
    ('music_bitrate', '96k'),
    ('max_concurrent_jobs', '3'),
    ('bandwidth_limit_kbps', '0'),
    ('ai_server_url', ''),
    ('ai_api_key', ''),
    ('ai_chat_model', ''),
    ('ai_embed_model', '');

-- SQLite-backed job queue for downloads, encoding, and indexing
CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL, -- 'download', 'encode', 'index', 'preset', 'zim'
    target TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending', -- 'pending', 'running', 'done', 'failed', 'paused'
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

CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_created_at ON jobs(created_at);

-- Docker application module states
CREATE TABLE IF NOT EXISTS app_states (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    category TEXT NOT NULL DEFAULT 'tools',
    image TEXT NOT NULL,
    container_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'stopped', -- 'running', 'stopped', 'starting', 'error', 'not_installed'
    port INTEGER NOT NULL DEFAULT 0,
    proxy_mode TEXT NOT NULL DEFAULT 'path', -- 'path' or 'port'
    auto_start BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Kiwix / Library catalog cache
CREATE TABLE IF NOT EXISTS catalog_items (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    category TEXT NOT NULL,
    language TEXT NOT NULL,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    date TEXT NOT NULL DEFAULT '',
    flavour TEXT NOT NULL DEFAULT '',
    url TEXT NOT NULL DEFAULT '',
    is_downloaded BOOLEAN NOT NULL DEFAULT 0,
    file_path TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_catalog_category ON catalog_items(category);
CREATE INDEX IF NOT EXISTS idx_catalog_language ON catalog_items(language);

-- Saved USB export profiles
CREATE TABLE IF NOT EXISTS usb_profiles (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    config_json TEXT NOT NULL DEFAULT '{}',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- RAG Knowledge Index tracking
CREATE TABLE IF NOT EXISTS rag_documents (
    id TEXT PRIMARY KEY,
    source_type TEXT NOT NULL, -- 'zim', 'pdf', 'note', 'web'
    source_path TEXT NOT NULL,
    title TEXT NOT NULL,
    indexed_pages INTEGER NOT NULL DEFAULT 0,
    qdrant_collection TEXT NOT NULL DEFAULT '',
    indexed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Set schema version
PRAGMA user_version = 1;
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "update":
			handleUpdate(os.Args[2:])
			return
		case "rollback":
			handleRollback(os.Args[2:])
			return
		case "usb":
			handleUSB(os.Args[2:])
			return
		case "move-data":
			handleMoveData(os.Args[2:])
			return
		case "uninstall":
			handleUninstall(os.Args[2:])
			return
		case "help", "-h", "--help":
			printUsage()
			return
		case "run":
			runServer(os.Args[2:])
			return
		}
	}

	runServer(os.Args[1:])
}

func printUsage() {
	fmt.Print(`Ark — Offline Internet Archive (Command Center)

Usage:
  ark [command] [options]

Commands:
  run          Start the Ark Command Center service (default)
  update       Update Ark binary and app containers
  rollback     Roll back to previous Ark binary and database snapshot
  usb          Export selected archives and containers to USB drive
  move-data    Relocate ArkData directory
  uninstall    Remove Ark containers, images, volumes, and service

Options for 'run':
  -port string
        HTTP service port (default "8080")
  -data-dir string
        Path to Ark data directory (auto-detects ./data or ./ArkData)
  -no-browser
        Do not automatically open the homepage in the default browser
  -foreground
        Keep running attached to the terminal window instead of detaching on Windows
  -no-container
        Run Ark web UI natively on host instead of inside the ark-core Docker container
`)
}

func runServer(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	portFlag := fs.String("port", "8080", "HTTP server port")
	dataDirFlag := fs.String("data-dir", "./ArkData", "Path to Ark data directory")
	noBrowserFlag := fs.Bool("no-browser", false, "Do not automatically open the homepage in the default browser")
	foregroundFlag := fs.Bool("foreground", false, "Keep running attached to the terminal window")
	noContainerFlag := fs.Bool("no-container", false, "Run natively on host instead of launching ark-core container")
	backgroundChildFlag := fs.Bool("background-child", false, "Internal flag for detached background service")
	_ = fs.Parse(args)

	dataDirExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "data-dir" {
			dataDirExplicit = true
		}
	})

	exePath, _ := os.Executable()
	if exePath != "" {
		exeDir := filepath.Dir(exePath)
		if _, err := os.Stat("catalog"); err != nil {
			if _, err2 := os.Stat(filepath.Join(exeDir, "catalog")); err2 == nil {
				_ = os.Chdir(exeDir)
			}
		}
	}

	port := *portFlag
	if envPort := os.Getenv("ARK_PORT"); envPort != "" {
		port = envPort
	}

	openUI := !*noBrowserFlag
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("ARK_NO_BROWSER"))); v == "1" || v == "true" || v == "yes" {
		openUI = false
	}

	fg := *foregroundFlag || *backgroundChildFlag
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("ARK_FOREGROUND"))); v == "1" || v == "true" || v == "yes" {
		fg = true
	}
	if !fg && maybeDetachBackground(port, args, openUI) {
		return
	}

	dataDir := resolveDefaultDataDir(exePath, *dataDirFlag, dataDirExplicit)

	log.Printf("[Ark] Initializing Command Center v%s with data directory: %s", update.ArkVersion, dataDir)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("[Ark] Failed to create data directory: %v", err)
	}

	if *backgroundChildFlag {
		if logFile, lerr := os.OpenFile(filepath.Join(dataDir, "ark.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); lerr == nil {
			log.SetOutput(logFile)
			defer logFile.Close()
		}
	}

	inContainer := os.Getenv("ARK_IN_CONTAINER") == "1"
	useContainerHost := !inContainer && !*noContainerFlag && os.Getenv("ARK_NO_CONTAINER") != "1"
	if useContainerHost {
		if runContainerizedHost(port, dataDir, openUI) {
			return
		}
		log.Printf("[Ark] Falling back to native host web server mode")
	}

	if rolledBack, err := update.MarkBootingOrRecover(exePath, dataDir); err != nil {
		log.Printf("[Ark] Warning: update watchdog check failed: %v", err)
	} else if rolledBack {
		log.Printf("[Ark] Previous update crashed before confirming health; rolled back binary and DB snapshot.")
	}

	// Initialize SQLite Database
	dbPath := filepath.Join(dataDir, "ark.db")
	db, err := initDatabase(dbPath)
	if err != nil {
		if _, rbErr := update.CheckAndRecoverOnStartup(exePath, dataDir); rbErr == nil {
			log.Printf("[Ark] DB init failed after update; rolled back to previous snapshot.")
		}
		log.Fatalf("[Ark] Database initialization failed: %v", err)
	}
	defer db.Close()

	// Clear any stale 'running' flags from an ungraceful previous exit until live containers start.
	_, _ = db.Exec(`UPDATE app_states SET status = 'stopped', updated_at = CURRENT_TIMESTAMP WHERE status = 'running'`)

	// Initialize Web Server
	webServer, err := web.NewServer(db, dataDir)
	if err != nil {
		log.Fatalf("[Ark] Failed to initialize web server: %v", err)
	}

	// Initialize Docker client (graceful if Docker daemon is offline)
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	restoreDone := make(chan struct{})
	var dockerCli *docker.Client
	if cli, err := docker.NewClient(); err == nil {
		dockerCli = cli
		defer dockerCli.Close()
		webServer.SetDockerClient(dockerCli)
		go func() {
			defer close(restoreDone)
			if err := dockerCli.EnsureDaemonRunning(runCtx); err != nil {
				log.Printf("[Ark] Docker engine unavailable at startup: %v", err)
				return
			}
			webServer.RestoreRunningApps(runCtx)
		}()
	} else {
		close(restoreDone)
		log.Printf("[Ark] Docker client unavailable: %v", err)
	}

	// Initialize SQLite-backed Job Queue Manager
	_ = ai.EnsureSchema(context.Background(), db)
	_ = ai.EnsureChatSchema(context.Background(), db)
	if pruned, _ := ai.PruneExpiredSessions(context.Background(), db, time.Now()); pruned > 0 {
		log.Printf("[Ark] Expired %d temporary AI chat session(s) older than 2 days", pruned)
	}
	if dt := system.RecordStartupAndComputeDowntime(context.Background(), db, time.Now()); dt.HasPrevious {
		log.Printf("[Ark] Resumed from last shutdown (%s); offline for %s", dt.LastShutdownAt, dt.DowntimeHuman)
	}
	jobStore := jobs.NewJobStore(db)
	_ = jobStore.ResetStaleRunning(context.Background())
	_ = jobStore.ClearCompleted(context.Background())
	queueMgr := jobs.NewQueueManager(jobStore, jobs.DefaultQueueConfig())
	queueMgr.RegisterRunner(jobs.TypeIndex, &ai.IndexJobRunner{DB: db, QdrantURL: "http://127.0.0.1:6333"})
	queueMgr.RegisterRunner(jobs.TypeOllamaPull, &ai.OllamaPullRunner{})
	queueMgr.RegisterRunner(jobs.TypeUSB, jobs.NewUSBRunner(dataDir, func(ctx context.Context, imageNames []string, out io.Writer) error {
		if dockerCli == nil {
			return fmt.Errorf("docker unavailable to export images")
		}
		return dockerCli.ImageSave(ctx, imageNames, out)
	}))
	queueMgr.Start(context.Background())
	webServer.SetQueueManager(queueMgr)

	mux := http.NewServeMux()
	if err := webServer.RegisterRoutes(mux); err != nil {
		log.Fatalf("[Ark] Failed to register web routes: %v", err)
	}

	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		_, _ = update.CheckAndRecoverOnStartup(exePath, dataDir)
		log.Fatalf("[Ark] Failed to listen on port %s: %v", port, err)
	}
	_ = update.ConfirmHealthy(dataDir)

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in background
	go func() {
		log.Printf("[Ark] Command Center listening on http://0.0.0.0:%s", port)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Ark] HTTP server error: %v", err)
		}
	}()

	if openUI {
		homeURL := fmt.Sprintf("http://localhost:%s/", port)
		go func() {
			time.Sleep(150 * time.Millisecond)
			log.Printf("[Ark] Opening %s in your default browser...", homeURL)
			openBrowser(homeURL)
		}()
	}

	// Graceful shutdown handling (signals + Windows console close 'X' button + System Tray)
	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	shutdownDone := make(chan struct{})
	triggerStop := func() {
		select {
		case sigChan <- syscall.SIGTERM:
		default:
		}
	}
	registerConsoleCloseHandler(triggerStop, shutdownDone)
	stopTray := startTrayIcon(port, triggerStop)
	defer stopTray()

	sig := <-sigChan
	stopTray()
	log.Printf("[Ark] Received signal %v, initiating graceful shutdown...", sig)
	runCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer shutdownCancel()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("[Ark] Error during HTTP server shutdown: %v", err)
		} else {
			log.Println("[Ark] HTTP server gracefully stopped")
		}
		queueMgr.Stop()
	}()

	go func() {
		defer wg.Done()
		select {
		case <-restoreDone:
		case <-time.After(500 * time.Millisecond):
		}
		if dockerCli == nil {
			if cli, err := docker.NewClient(); err == nil {
				dockerCli = cli
				defer dockerCli.Close()
			}
		}
		if dockerCli != nil && dockerCli.IsAvailable(shutdownCtx) {
			stopped, err := dockerCli.StopAll(shutdownCtx, 2)
			if err != nil {
				log.Printf("[Ark] Warning: error stopping some Docker containers: %v", err)
			} else {
				log.Printf("[Ark] Stopped %d Ark Docker container(s)", stopped)
			}
		}
		_, _ = db.Exec(`UPDATE app_states SET status = 'stopped', updated_at = CURRENT_TIMESTAMP WHERE status = 'running'`)
	}()

	wg.Wait()
	_ = jobStore.ClearCompleted(context.Background())
	if err := system.RecordShutdownTime(context.Background(), db, time.Now()); err != nil {
		log.Printf("[Ark] Warning: failed to record shutdown timestamp: %v", err)
	}
	log.Println("[Ark] Command Center shutdown complete")
	close(shutdownDone)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[Ark] Could not open browser automatically: %v (open %s manually)", err, url)
	}
}

func resolveDefaultDataDir(exePath string, flagVal string, flagExplicit bool) string {
	if flagExplicit && strings.TrimSpace(flagVal) != "" {
		if abs, err := filepath.Abs(flagVal); err == nil {
			return abs
		}
		return flagVal
	}
	if envDir := strings.TrimSpace(os.Getenv("ARK_DATA_DIR")); envDir != "" {
		if abs, err := filepath.Abs(envDir); err == nil {
			return abs
		}
		return envDir
	}

	var candidates []string
	if exePath != "" {
		exeDir := filepath.Dir(exePath)
		candidates = append(candidates,
			filepath.Join(exeDir, "data"),
			filepath.Join(exeDir, "ArkData"),
		)
	}
	candidates = append(candidates, "./data", "./ArkData")

	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "ark.db")); err == nil {
			if abs, err := filepath.Abs(c); err == nil {
				return abs
			}
			return c
		}
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			if abs, err := filepath.Abs(c); err == nil {
				return abs
			}
			return c
		}
	}

	if abs, err := filepath.Abs(flagVal); err == nil {
		return abs
	}
	return flagVal
}

// runContainerizedHost starts Docker (if offline), launches the containerized ark-core
// web server (which restores all installed app containers), opens the homepage in the browser,
// and keeps a System Tray supervisor alive to stop all containers on shutdown.
// Returns false only if Docker cannot be initialized so the caller can fall back to native host mode.
func runContainerizedHost(port, dataDir string, openUI bool) bool {
	cli, err := docker.NewClient()
	if err != nil {
		log.Printf("[Ark] Docker client unavailable: %v", err)
		return false
	}
	defer cli.Close()

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer bootCancel()

	log.Printf("[Ark] Ensuring Docker daemon is running...")
	if err := cli.EnsureDaemonRunning(bootCtx); err != nil {
		log.Printf("[Ark] Failed to start Docker daemon: %v", err)
		return false
	}

	log.Printf("[Ark] Starting containerized Ark Command Center (%s)...", docker.CoreContainerName)
	if err := cli.EnsureCoreContainer(bootCtx, port, dataDir); err != nil {
		log.Printf("[Ark] Failed to start %s container: %v", docker.CoreContainerName, err)
		return false
	}

	// Wait until ark-core responds on /healthz before opening the browser
	healthClient := &http.Client{Timeout: 800 * time.Millisecond}
	healthURL := fmt.Sprintf("http://127.0.0.1:%s/healthz", port)
	healthy := false
	for i := 0; i < 60; i++ {
		if resp, err := healthClient.Get(healthURL); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	if !healthy {
		log.Printf("[Ark] Timed out waiting for %s HTTP healthz on :%s", docker.CoreContainerName, port)
		return false
	}

	if openUI {
		homeURL := fmt.Sprintf("http://localhost:%s/", port)
		log.Printf("[Ark] Opening %s in your default browser...", homeURL)
		openBrowser(homeURL)
	}

	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	shutdownDone := make(chan struct{})
	triggerStop := func() {
		select {
		case sigChan <- syscall.SIGTERM:
		default:
		}
	}
	registerConsoleCloseHandler(triggerStop, shutdownDone)
	stopTray := startTrayIcon(port, triggerStop)
	defer stopTray()

	// Also watch if user triggered Shut Down Ark inside the web UI (ark-core stopped itself cleanly)
	coreExitCh := make(chan struct{}, 1)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-shutdownDone:
				return
			case <-ticker.C:
				checkCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
				up := cli.IsCoreRunning(checkCtx)
				cancel()
				if !up {
					select {
					case coreExitCh <- struct{}{}:
					default:
					}
					return
				}
			}
		}
	}()

	select {
	case sig := <-sigChan:
		log.Printf("[Ark] Received shutdown signal (%v); stopping all Ark containers...", sig)
	case <-coreExitCh:
		log.Printf("[Ark] %s container exited; ensuring all Ark containers are stopped...", docker.CoreContainerName)
	}

	stopTray()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer stopCancel()
	if stopped, err := cli.StopAll(stopCtx, 3); err != nil {
		log.Printf("[Ark] Warning: error stopping containers: %v", err)
	} else {
		log.Printf("[Ark] Stopped %d Ark container(s)", stopped)
	}
	close(shutdownDone)
	return true
}

func initDatabase(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Pragmas for performance and data safety
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA busy_timeout = 5000;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			log.Printf("[Ark] Warning: executing %s: %v", p, err)
		}
	}

	// Check user_version for migrations
	var userVersion int
	if err := db.QueryRow("PRAGMA user_version;").Scan(&userVersion); err != nil {
		return nil, fmt.Errorf("failed to read pragma user_version: %w", err)
	}

	if userVersion == 0 {
		log.Printf("[Ark] Applying initial migration (0001_init.sql)...")
		migrationSQL := initialMigrationSQL
		// Check if external migration file exists
		if content, err := os.ReadFile("migrations/0001_init.sql"); err == nil {
			migrationSQL = string(content)
		}

		if _, err := db.Exec(migrationSQL); err != nil {
			return nil, fmt.Errorf("failed to execute initial migration: %w", err)
		}
		log.Printf("[Ark] Migration 0001_init.sql applied successfully (user_version = 1)")
	}

	return db, nil
}

func resolveDataDir(flagVal string) string {
	dir := flagVal
	if envDir := os.Getenv("ARK_DATA_DIR"); envDir != "" && dir == "./ArkData" {
		dir = envDir
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func handleUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fromFlag := fs.String("from", "", "Path to update directory or mounted USB")
	dataDirFlag := fs.String("data-dir", "./ArkData", "Path to ArkData directory")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	src := strings.TrimSpace(*fromFlag)
	if src == "" {
		fmt.Println("[Ark] Current version:", update.ArkVersion)
		fmt.Println("[Ark] Provide --from <path> pointing to an offline update bundle directory.")
		return
	}

	dataDir := resolveDataDir(*dataDirFlag)
	pubKey, err := update.DefaultPublicKey()
	if err != nil {
		log.Fatalf("[Ark] Update key error: %v", err)
	}
	m, newBin, err := update.LoadUpdateFromDir(src, pubKey)
	if err != nil {
		log.Fatalf("[Ark] Update verification failed: %v", err)
	}
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("[Ark] Cannot determine current executable path: %v", err)
	}
	if err := update.ApplyUpdate(exePath, newBin, dataDir, update.ArkVersion, m.Version); err != nil {
		log.Fatalf("[Ark] Failed to apply update: %v", err)
	}
	fmt.Printf("[Ark] Verified Ed25519 signature and staged v%s (DB snapshot saved). Restart Ark to activate.\n", m.Version)
}

func handleRollback(args []string) {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	dataDirFlag := fs.String("data-dir", "./ArkData", "Path to ArkData directory")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	dataDir := resolveDataDir(*dataDirFlag)
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("[Ark] Cannot determine current executable path: %v", err)
	}
	if err := update.Rollback(exePath, dataDir); err != nil {
		log.Fatalf("[Ark] Rollback failed: %v", err)
	}
	fmt.Println("[Ark] Rolled back previous binary and database snapshot. Restart Ark to run the previous version.")
}

func handleUSB(args []string) {
	fs := flag.NewFlagSet("usb", flag.ContinueOnError)
	profileFlag := fs.String("profile", "", "Saved profile name to load (optional)")
	toFlag := fs.String("to", "", "Target USB mount or bundle directory path")
	dataDirFlag := fs.String("data-dir", "./ArkData", "Path to ArkData directory")
	catsFlag := fs.String("categories", "zim,druglabels", "Comma-separated content categories to export")
	imagesFlag := fs.Bool("images", false, "Include installed Docker app images (.tar)")
	configFlag := fs.Bool("config", true, "Include sanitized ark.db settings/catalog")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if strings.TrimSpace(*toFlag) == "" {
		fmt.Println("[Ark] Error: --to <target_path> is required")
		os.Exit(2)
	}

	dataDir := resolveDataDir(*dataDirFlag)
	cfg := usb.ExportConfig{
		TargetPath:          strings.TrimSpace(*toFlag),
		IncludeDockerImages: *imagesFlag,
		IncludeConfig:       *configFlag,
	}
	for _, c := range strings.Split(*catsFlag, ",") {
		if trimmed := strings.TrimSpace(c); trimmed != "" {
			cfg.Categories = append(cfg.Categories, trimmed)
		}
	}

	dbPath := filepath.Join(dataDir, "ark.db")
	if _, err := os.Stat(dbPath); err == nil {
		if db, err := sql.Open("sqlite", dbPath); err == nil {
			defer db.Close()
			if *profileFlag != "" {
				if prof, err := usb.GetProfile(db, *profileFlag); err == nil {
					cfg = prof.Config
					cfg.TargetPath = strings.TrimSpace(*toFlag)
				} else {
					log.Fatalf("[Ark] Profile %q not found: %v", *profileFlag, err)
				}
			}
			if cfg.IncludeDockerImages && len(cfg.DockerImageNames) == 0 {
				rows, err := db.Query(`SELECT DISTINCT image FROM app_states WHERE COALESCE(image, '') != ''`)
				if err == nil {
					for rows.Next() {
						var img string
						if rows.Scan(&img) == nil {
							cfg.DockerImageNames = append(cfg.DockerImageNames, img)
						}
					}
					rows.Close()
				}
			}
		}
	}

	var saver usb.ImageSaver
	if cfg.IncludeDockerImages && len(cfg.DockerImageNames) > 0 {
		if dockerCli, err := docker.NewClient(); err == nil {
			defer dockerCli.Close()
			saver = dockerCli.ImageSave
		}
	}

	res, err := usb.ExportToUSB(context.Background(), dataDir, cfg, saver, func(pct float64, msg string) {
		fmt.Printf("[Ark] USB %.0f%% - %s\n", pct, msg)
	})
	if err != nil {
		log.Fatalf("[Ark] USB export failed: %v", err)
	}
	fmt.Printf("[Ark] USB export complete: %d copied, %d unchanged skipped, manifest: %s\n",
		res.FilesCopied, res.FilesSkipped, res.ManifestPath)
}

func handleMoveData(args []string) {
	fs := flag.NewFlagSet("move-data", flag.ContinueOnError)
	toFlag := fs.String("to", "", "New destination directory for ArkData")
	dataDirFlag := fs.String("data-dir", "./ArkData", "Current ArkData directory")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if strings.TrimSpace(*toFlag) == "" {
		fmt.Println("[Ark] Error: --to <new_path> is required")
		os.Exit(2)
	}

	srcDir := resolveDataDir(*dataDirFlag)
	dstDir := resolveDataDir(*toFlag)
	files, bytes, err := usb.MoveDataDir(srcDir, dstDir)
	if err != nil {
		log.Fatalf("[Ark] Move data failed: %v", err)
	}
	if db, err := sql.Open("sqlite", filepath.Join(dstDir, "ark.db")); err == nil {
		_, _ = db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('data_dir', ?)`, dstDir)
		_ = db.Close()
	}
	fmt.Printf("[Ark] Relocated %d file(s) (%d bytes) to %s with SHA-256 verification.\n", files, bytes, dstDir)
	fmt.Printf("[Ark] Update ARK_DATA_DIR or launch with: ark run --data-dir %s\n", dstDir)
}

func handleUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	allFlag := fs.Bool("all", false, "Uninstall all Ark containers, images and volumes")
	keepDataFlag := fs.Bool("keep-data", false, "Keep ArkData content directory")
	dataDirFlag := fs.String("data-dir", "./ArkData", "Path to ArkData directory")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if !*allFlag {
		fmt.Println("[Ark] Please specify --all to confirm uninstallation")
		return
	}

	if dockerCli, err := docker.NewClient(); err == nil {
		defer dockerCli.Close()
		removed, err := dockerCli.UninstallAll(context.Background())
		if err != nil {
			log.Printf("[Ark] Warning during Docker cleanup: %v", err)
		} else {
			fmt.Printf("[Ark] Removed %d Ark container(s), images, ark-qdrant volume, and ark-net.\n", removed)
		}
	} else {
		fmt.Printf("[Ark] Docker daemon unavailable (%v); skipping container cleanup.\n", err)
	}

	if !*keepDataFlag {
		dataDir := resolveDataDir(*dataDirFlag)
		if err := os.RemoveAll(dataDir); err != nil {
			log.Fatalf("[Ark] Failed to remove ArkData at %s: %v", dataDir, err)
		}
		fmt.Printf("[Ark] Removed ArkData directory: %s\n", dataDir)
	} else {
		fmt.Println("[Ark] Kept ArkData content directory (--keep-data).")
	}
}
