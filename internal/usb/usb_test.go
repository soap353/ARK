package usb

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	schema := `
	CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	CREATE TABLE usb_profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		description TEXT,
		config_json TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestProfilesCRUDAndScanCategories(t *testing.T) {
	db := openTestDB(t)
	cfg := ExportConfig{
		Categories:          []string{"zim", "druglabels"},
		IncludeDockerImages: true,
		IncludeConfig:       true,
	}
	if err := SaveProfile(db, "starter_pack", "Wikipedia + FDA Drug Labels", cfg); err != nil {
		t.Fatalf("SaveProfile failed: %v", err)
	}
	cfg.Categories = []string{"zim", "maps", "druglabels"}
	if err := SaveProfile(db, "starter_pack", "Updated Starter Pack", cfg); err != nil {
		t.Fatalf("SaveProfile update failed: %v", err)
	}

	profiles, err := ListProfiles(db)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d (err=%v)", len(profiles), err)
	}
	got, err := GetProfile(db, "starter_pack")
	if err != nil {
		t.Fatalf("GetProfile failed: %v", err)
	}
	if len(got.Config.Categories) != 3 || got.Description != "Updated Starter Pack" {
		t.Fatalf("unexpected profile data: %+v", got)
	}

	dataDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dataDir, "zim"), 0755)
	_ = os.WriteFile(filepath.Join(dataDir, "zim", "wiki.zim"), []byte("zim-data"), 0644)
	cats, err := ScanCategories(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("ScanCategories failed: %v", err)
	}
	var foundZim bool
	for _, c := range cats {
		if c.Key == "zim" && c.FileCount == 1 && c.SizeBytes == 8 {
			foundZim = true
		}
	}
	if !foundZim {
		t.Fatalf("expected zim category stats in %+v", cats)
	}
}

func TestIncrementalExportToUSBAndVerifyManifest(t *testing.T) {
	dataDir := t.TempDir()
	usbDir := t.TempDir()

	_ = os.MkdirAll(filepath.Join(dataDir, "zim"), 0755)
	_ = os.MkdirAll(filepath.Join(dataDir, "druglabels"), 0755)
	_ = os.WriteFile(filepath.Join(dataDir, "zim", "mini.zim"), []byte("hello-zim"), 0644)
	_ = os.WriteFile(filepath.Join(dataDir, "library.xml"), []byte("<library/>"), 0644)
	_ = os.WriteFile(filepath.Join(dataDir, "druglabels", "druglabels.db"), []byte("fda-db-content"), 0644)

	// Create ark.db with sensitive settings to verify sanitization on export.
	srcDBPath := filepath.Join(dataDir, "ark.db")
	srcDB, err := sql.Open("sqlite", srcDBPath)
	if err != nil {
		t.Fatalf("open src db: %v", err)
	}
	_, _ = srcDB.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO settings VALUES ('lan_mode', 'read'), ('admin_password_hash', 'secret-hash'), ('ai_api_key', 'sk-secret');`)
	_ = srcDB.Close()

	saver := func(ctx context.Context, imageNames []string, out io.Writer) error {
		_, err := out.Write([]byte("fake-docker-tar"))
		return err
	}

	cfg := ExportConfig{
		TargetPath:          usbDir,
		Categories:          []string{"zim", "druglabels"},
		IncludeConfig:       true,
		IncludeDockerImages: true,
		DockerImageNames:    []string{"ark/druglabels:latest"},
	}

	res1, err := ExportToUSB(context.Background(), dataDir, cfg, saver, nil)
	if err != nil {
		t.Fatalf("first ExportToUSB failed: %v", err)
	}
	if res1.FilesCopied != 5 || res1.FilesSkipped != 0 {
		t.Fatalf("expected 5 copied (zim, library.xml, druglabels, ark.db, docker tar) & 0 skipped, got %+v", res1)
	}

	verified, err := VerifyManifest(usbDir)
	if err != nil || verified != 5 {
		t.Fatalf("expected 5 verified files, got %d (err=%v)", verified, err)
	}

	// Verify exported ark.db stripped admin_password_hash and ai_api_key.
	expDB, err := sql.Open("sqlite", filepath.Join(usbDir, "config", "ark.db"))
	if err != nil {
		t.Fatalf("open exported ark.db: %v", err)
	}
	var secretCount int
	_ = expDB.QueryRow(`SELECT COUNT(*) FROM settings WHERE key IN ('admin_password_hash', 'ai_api_key')`).Scan(&secretCount)
	_ = expDB.Close()
	if secretCount != 0 {
		t.Fatalf("expected 0 sensitive keys in exported DB, got %d", secretCount)
	}

	// Second run without Docker tar regeneration should skip all unchanged content files.
	cfg.IncludeDockerImages = false
	res2, err := ExportToUSB(context.Background(), dataDir, cfg, nil, nil)
	if err != nil {
		t.Fatalf("second ExportToUSB failed: %v", err)
	}
	if res2.FilesCopied != 0 || res2.FilesSkipped != 4 {
		t.Fatalf("expected 0 copied and 4 skipped on unchanged incremental re-run, got %+v", res2)
	}

	// Corrupt a file on USB -> VerifyManifest must fail, then re-export fixes only that file.
	_ = os.WriteFile(filepath.Join(usbDir, "data", "zim", "mini.zim"), []byte("corrupt"), 0644)
	if _, err := VerifyManifest(usbDir); err == nil {
		t.Fatalf("expected VerifyManifest error on corrupted USB file")
	}

	res3, err := ExportToUSB(context.Background(), dataDir, cfg, nil, nil)
	if err != nil {
		t.Fatalf("third ExportToUSB failed: %v", err)
	}
	if res3.FilesCopied != 1 || res3.FilesSkipped != 3 {
		t.Fatalf("expected 1 re-copied and 3 skipped, got %+v", res3)
	}
}

func TestMoveDataDir(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "NewArkData")

	_ = os.MkdirAll(filepath.Join(src, "zim"), 0755)
	_ = os.WriteFile(filepath.Join(src, "zim", "test.zim"), []byte("payload-123"), 0644)
	_ = os.WriteFile(filepath.Join(src, "ark.db"), []byte("db-bytes"), 0644)

	files, bytes, err := MoveDataDir(src, dst)
	if err != nil {
		t.Fatalf("MoveDataDir failed: %v", err)
	}
	if files != 2 || bytes != int64(len("payload-123")+len("db-bytes")) {
		t.Fatalf("unexpected move stats: files=%d bytes=%d", files, bytes)
	}
	got, err := os.ReadFile(filepath.Join(dst, "zim", "test.zim"))
	if err != nil || string(got) != "payload-123" {
		t.Fatalf("expected moved file contents, got %q (err=%v)", string(got), err)
	}

	if _, _, err := MoveDataDir(src, filepath.Join(src, "nested")); err == nil {
		t.Fatalf("expected error moving directory into itself")
	}
}
