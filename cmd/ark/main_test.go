package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitDatabase(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ark-db-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "ark.db")
	db, err := initDatabase(dbPath)
	if err != nil {
		t.Fatalf("initDatabase failed: %v", err)
	}
	defer db.Close()

	// Verify PRAGMA user_version is 1
	var version int
	if err := db.QueryRow("PRAGMA user_version;").Scan(&version); err != nil {
		t.Fatalf("failed to query pragma user_version: %v", err)
	}
	if version != 1 {
		t.Errorf("expected user_version 1, got %d", version)
	}

	// Verify settings table exists and default settings seeded
	var deviceName string
	err = db.QueryRow("SELECT value FROM settings WHERE key = 'device_name'").Scan(&deviceName)
	if err != nil {
		t.Fatalf("failed to query device_name from settings: %v", err)
	}
	if deviceName != "Ark" {
		t.Errorf("expected device_name 'Ark', got '%s'", deviceName)
	}

	// Verify jobs table exists
	_, err = db.Exec("INSERT INTO jobs (id, type, target) VALUES ('test-1', 'download', 'http://example.com')")
	if err != nil {
		t.Errorf("failed to insert into jobs table: %v", err)
	}

	// Verify app_states table exists
	_, err = db.Exec("INSERT INTO app_states (id, name, image) VALUES ('kiwix', 'Library', 'ghcr.io/kiwix/kiwix-serve')")
	if err != nil {
		t.Errorf("failed to insert into app_states table: %v", err)
	}
}
