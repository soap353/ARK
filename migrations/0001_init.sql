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
