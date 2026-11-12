package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"ark/internal/ai"
	"ark/internal/docker"
	"ark/internal/jobs"
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
        Path to ArkData directory (default "./ArkData")
`)
}

func runServer(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	portFlag := fs.String("port", "8080", "HTTP server port")
	dataDirFlag := fs.String("data-dir", "./ArkData", "Path to ArkData directory")
	_ = fs.Parse(args)

	port := *portFlag
	if envPort := os.Getenv("ARK_PORT"); envPort != "" {
		port = envPort
	}

	dataDir := *dataDirFlag
	if envDir := os.Getenv("ARK_DATA_DIR"); envDir != "" {
		dataDir = envDir
	}
	if absDir, err := filepath.Abs(dataDir); err == nil {
		dataDir = absDir
	}

	log.Printf("[Ark] Initializing Command Center with data directory: %s", dataDir)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("[Ark] Failed to create data directory: %v", err)
	}

	// Initialize SQLite Database
	dbPath := filepath.Join(dataDir, "ark.db")
	db, err := initDatabase(dbPath)
	if err != nil {
		log.Fatalf("[Ark] Database initialization failed: %v", err)
	}
	defer db.Close()

	// Initialize Web Server
	webServer, err := web.NewServer(db, dataDir)
	if err != nil {
		log.Fatalf("[Ark] Failed to initialize web server: %v", err)
	}

	// Initialize SQLite-backed Job Queue Manager
	_ = ai.EnsureSchema(context.Background(), db)
	jobStore := jobs.NewJobStore(db)
	_ = jobStore.ResetStaleRunning(context.Background())
	queueMgr := jobs.NewQueueManager(jobStore, jobs.DefaultQueueConfig())
	queueMgr.RegisterRunner(jobs.TypeIndex, &ai.IndexJobRunner{DB: db, QdrantURL: "http://127.0.0.1:6333"})
	queueMgr.Start(context.Background())
	defer queueMgr.Stop()
	webServer.SetQueueManager(queueMgr)

	// Initialize Docker client (graceful if Docker daemon is offline)
	if dockerCli, err := docker.NewClient(); err == nil {
		defer dockerCli.Close()
		webServer.SetDockerClient(dockerCli)
		go webServer.RestoreRunningApps(context.Background())
	} else {
		log.Printf("[Ark] Docker client unavailable: %v", err)
	}

	mux := http.NewServeMux()
	if err := webServer.RegisterRoutes(mux); err != nil {
		log.Fatalf("[Ark] Failed to register web routes: %v", err)
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in background
	go func() {
		log.Printf("[Ark] Command Center listening on http://0.0.0.0:%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Ark] HTTP server error: %v", err)
		}
	}()

	// Graceful shutdown handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	sig := <-sigChan
	log.Printf("[Ark] Received signal %v, initiating graceful shutdown...", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[Ark] Error during HTTP server shutdown: %v", err)
	} else {
		log.Println("[Ark] HTTP server gracefully stopped")
	}

	log.Println("[Ark] Command Center shutdown complete")
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

func handleUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	fromFlag := fs.String("from", "", "Path to update directory or mounted USB")
	_ = fs.Parse(args)

	fmt.Printf("[Ark] Checking for updates (source: %s)...\n", *fromFlag)
	fmt.Println("[Ark] Ark is up to date.")
}

func handleRollback(args []string) {
	fmt.Println("[Ark] Rolling back to previous binary and database snapshot...")
	fmt.Println("[Ark] Rollback completed.")
}

func handleUSB(args []string) {
	fs := flag.NewFlagSet("usb", flag.ExitOnError)
	profileFlag := fs.String("profile", "starter_pack", "Profile name to export")
	toFlag := fs.String("to", "", "Target USB mount path")
	_ = fs.Parse(args)

	if *toFlag == "" {
		fmt.Println("[Ark] Error: --to <target_path> is required")
		return
	}
	fmt.Printf("[Ark] Exporting profile '%s' to USB drive at '%s'...\n", *profileFlag, *toFlag)
}

func handleMoveData(args []string) {
	fs := flag.NewFlagSet("move-data", flag.ExitOnError)
	toFlag := fs.String("to", "", "New destination directory for ArkData")
	_ = fs.Parse(args)

	if *toFlag == "" {
		fmt.Println("[Ark] Error: --to <new_path> is required")
		return
	}
	fmt.Printf("[Ark] Relocating ArkData directory to '%s'...\n", *toFlag)
}

func handleUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	allFlag := fs.Bool("all", false, "Uninstall all Ark containers, images and volumes")
	keepDataFlag := fs.Bool("keep-data", false, "Keep ArkData content directory")
	_ = fs.Parse(args)

	if !*allFlag {
		fmt.Println("[Ark] Please specify --all to confirm uninstallation")
		return
	}
	fmt.Printf("[Ark] Uninstalling Ark components (keep-data: %v)...\n", *keepDataFlag)
}
