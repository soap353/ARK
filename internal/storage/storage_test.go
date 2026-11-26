package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestStorageDB(t *testing.T, dbFile string) *sql.DB {
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("Open test db: %v", err)
	}

	schema := `
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

	CREATE TABLE IF NOT EXISTS usb_profiles (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		config_json TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS rag_documents (
		id TEXT PRIMARY KEY,
		source_type TEXT NOT NULL,
		source_path TEXT NOT NULL,
		title TEXT NOT NULL,
		indexed_pages INTEGER NOT NULL DEFAULT 0,
		qdrant_collection TEXT NOT NULL DEFAULT '',
		indexed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("Init schema: %v", err)
	}

	return db
}

func TestDiskUsage(t *testing.T) {
	dataDir := t.TempDir()

	// Create sample folders and files
	zimDir := filepath.Join(dataDir, "zim")
	mediaVideoDir := filepath.Join(dataDir, "media", "video")
	_ = os.MkdirAll(zimDir, 0755)
	_ = os.MkdirAll(mediaVideoDir, 0755)

	_ = os.WriteFile(filepath.Join(zimDir, "wiki.zim"), make([]byte, 1024), 0644)
	_ = os.WriteFile(filepath.Join(mediaVideoDir, "doc.webm"), make([]byte, 2048), 0644)

	report, err := GetDiskUsage(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("GetDiskUsage failed: %v", err)
	}

	if report.TotalUsedBytes != 3072 {
		t.Errorf("Expected TotalUsedBytes 3072, got %d", report.TotalUsedBytes)
	}

	files, err := ListCategoryFiles(dataDir, CategoryZim)
	if err != nil {
		t.Fatalf("ListCategoryFiles failed: %v", err)
	}
	if len(files) != 1 || files[0].Name != "wiki.zim" {
		t.Errorf("Unexpected category files: %+v", files)
	}
}

func TestCascadeDelete(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "ArkData")
	zimDir := filepath.Join(dataDir, "zim")
	_ = os.MkdirAll(zimDir, 0755)

	zimFile := filepath.Join(zimDir, "med.zim")
	_ = os.WriteFile(zimFile, []byte("fake medical zim contents"), 0644)

	libraryXMLPath := filepath.Join(dataDir, "library.xml")
	db := setupTestStorageDB(t, filepath.Join(tempDir, "test.db"))
	defer db.Close()

	// Seed catalog_items
	_, _ = db.Exec(`
		INSERT INTO catalog_items (id, title, category, language, is_downloaded, file_path)
		VALUES ('med-123', 'WikiMed', 'medical', 'eng', 1, ?)
	`, "zim/med.zim")

	// Seed usb_profiles
	_, _ = db.Exec(`
		INSERT INTO usb_profiles (id, name, config_json)
		VALUES ('usb-1', 'Survival USB', '{"items":["med-123","maps/offline.pmtiles"]}')
	`)

	// Seed rag_documents
	_, _ = db.Exec(`
		INSERT INTO rag_documents (id, source_type, source_path, title, qdrant_collection)
		VALUES ('rag-1', 'zim', ?, 'WikiMed Articles', '')
	`, "zim/med.zim")

	cfg := DeleteCascadeConfig{
		DataDir:        dataDir,
		LibraryXMLPath: libraryXMLPath,
		DB:             db,
	}

	target := DeleteTarget{
		ItemID:   "med-123",
		FilePath: "zim/med.zim",
		Category: CategoryZim,
	}

	result, err := CascadeDelete(ctx, cfg, target)
	if err != nil {
		t.Fatalf("CascadeDelete failed: %v", err)
	}

	// 1. File on disk must be gone
	if !result.FileDeleted {
		t.Errorf("Expected FileDeleted true")
	}
	if _, err := os.Stat(zimFile); !os.IsNotExist(err) {
		t.Errorf("Expected zim file to be removed from disk")
	}

	// 2. USB profile must have removed med-123
	var updatedConfig string
	_ = db.QueryRow("SELECT config_json FROM usb_profiles WHERE id = 'usb-1'").Scan(&updatedConfig)
	if fmt.Sprintf("%v", updatedConfig) == `{"items":["med-123","maps/offline.pmtiles"]}` {
		t.Errorf("USB profile was not cleaned of med-123 reference: %s", updatedConfig)
	}

	// 4. catalog_items must be marked not downloaded
	var isDownloaded bool
	_ = db.QueryRow("SELECT is_downloaded FROM catalog_items WHERE id = 'med-123'").Scan(&isDownloaded)
	if isDownloaded {
		t.Errorf("catalog_items still marked downloaded")
	}

	// 5. rag_documents entry must be deleted
	var ragCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM rag_documents WHERE id = 'rag-1'").Scan(&ragCount)
	if ragCount != 0 {
		t.Errorf("Expected rag_documents entry to be removed, count=%d", ragCount)
	}
}

func TestIntegrityAndPrune(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "ArkData")
	zimDir := filepath.Join(dataDir, "zim")
	_ = os.MkdirAll(zimDir, 0755)

	db := setupTestStorageDB(t, filepath.Join(tempDir, "test.db"))
	defer db.Close()

	// 1. Create an orphan file on disk (not in catalog_items)
	orphanFile := filepath.Join(zimDir, "orphan.zim")
	_ = os.WriteFile(orphanFile, []byte("orphan"), 0644)

	// 2. Create a catalog_item that is missing on disk
	_, _ = db.Exec(`
		INSERT INTO catalog_items (id, title, category, language, is_downloaded, file_path)
		VALUES ('ghost-zim', 'Ghost ZIM', 'knowledge', 'eng', 1, 'zim/ghost.zim')
	`)

	// 3. Create a rag_document pointing to a missing file
	_, _ = db.Exec(`
		INSERT INTO rag_documents (id, source_type, source_path, title, qdrant_collection)
		VALUES ('rag-ghost', 'zim', 'zim/ghost.zim', 'Ghost', 'coll')
	`)

	report, err := CheckIntegrity(ctx, dataDir, db)
	if err != nil {
		t.Fatalf("CheckIntegrity failed: %v", err)
	}

	if len(report.OrphanFiles) != 1 || report.OrphanFiles[0] != "zim/orphan.zim" {
		t.Errorf("Expected 1 orphan file 'zim/orphan.zim', got %v", report.OrphanFiles)
	}
	if len(report.MissingFiles) != 1 {
		t.Errorf("Expected 1 missing file, got %v", report.MissingFiles)
	}
	if len(report.OrphanRAGDocs) != 1 {
		t.Errorf("Expected 1 orphan RAG doc, got %v", report.OrphanRAGDocs)
	}

	// Prune orphans
	err = PruneOrphans(ctx, dataDir, db, report, true)
	if err != nil {
		t.Fatalf("PruneOrphans failed: %v", err)
	}

	// Verify disk orphan was removed
	if _, err := os.Stat(orphanFile); !os.IsNotExist(err) {
		t.Errorf("Expected orphan file on disk to be removed")
	}

	// Re-check integrity -> zero orphans!
	postReport, _ := CheckIntegrity(ctx, dataDir, db)
	if len(postReport.OrphanFiles) != 0 || len(postReport.MissingFiles) != 0 || len(postReport.OrphanRAGDocs) != 0 {
		t.Errorf("Expected zero orphans after prune, got: %+v", postReport)
	}
}

func TestEvaluateSpaceAndParseSizeText(t *testing.T) {
	const gb = uint64(1024 * 1024 * 1024)

	// 1. OK (< 25% of free space and > 1 GB buffer)
	okRes := EvaluateSpace(20*gb, 100*gb, int64(2*gb))
	if okRes.Status != "ok" {
		t.Errorf("expected ok for 2 GB on 20 GB free, got %+v", okRes)
	}

	// 2. Warn (>= 25% of free space)
	warnRes := EvaluateSpace(20*gb, 100*gb, int64(6*gb))
	if warnRes.Status != "warn" {
		t.Errorf("expected warn for 6 GB on 20 GB free, got %+v", warnRes)
	}

	// 3. Insufficient (would violate 1 GB safety buffer or exceed free)
	insRes := EvaluateSpace(5*gb, 100*gb, int64(4500*1024*1024))
	if insRes.Status != "insufficient" {
		t.Errorf("expected insufficient for 4.5 GB on 5 GB free, got %+v", insRes)
	}

	// 4. Unknown size on critically low disk (< 2 GB free) -> warn
	lowRes := EvaluateSpace(1*gb, 100*gb, 0)
	if lowRes.Status != "warn" {
		t.Errorf("expected warn on 1 GB free with unknown size, got %+v", lowRes)
	}

	if got := ParseSizeText("~3.5 GB"); got != int64(3.5*float64(gb)) {
		t.Errorf("ParseSizeText(~3.5 GB) = %d", got)
	}
	if got := ParseSizeText("280 MB"); got != 280*1024*1024 {
		t.Errorf("ParseSizeText(280 MB) = %d", got)
	}
}
