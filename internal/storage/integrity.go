package storage

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// IntegrityReport provides findings from checking consistency between disk and SQLite.
type IntegrityReport struct {
	OrphanFiles    []string `json:"orphan_files"`     // Files on disk not tracked in catalog_items
	MissingFiles   []string `json:"missing_files"`    // Catalog items marked downloaded but absent on disk
	OrphanRAGDocs  []string `json:"orphan_rag_docs"`  // RAG documents pointing to non-existent disk files
}

// resolveDataPath resolves a relative path against dataDir if it is not already absolute.
func resolveDataPath(dataDir, p string) string {
	if p == "" || filepath.IsAbs(p) || dataDir == "" {
		return p
	}
	return filepath.Join(dataDir, p)
}

// CheckIntegrity audits ArkData storage against database records to find discrepancies.
func CheckIntegrity(ctx context.Context, dataDir string, db *sql.DB) (*IntegrityReport, error) {
	report := &IntegrityReport{
		OrphanFiles:   make([]string, 0),
		MissingFiles:  make([]string, 0),
		OrphanRAGDocs: make([]string, 0),
	}

	// 1. Gather all files marked downloaded in catalog_items
	catalogMap := make(map[string]string) // path -> id
	rows, err := db.QueryContext(ctx, "SELECT id, file_path FROM catalog_items WHERE is_downloaded = 1")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, path string
			if err := rows.Scan(&id, &path); err == nil && path != "" {
				cleanP := filepath.Clean(path)
				slashP := filepath.ToSlash(cleanP)
				catalogMap[cleanP] = id
				catalogMap[slashP] = id

				// Check if file exists on disk
				checkPath := resolveDataPath(dataDir, cleanP)
				if _, err := os.Stat(checkPath); os.IsNotExist(err) {
					report.MissingFiles = append(report.MissingFiles, slashP)
				}
			}
		}
	}

	// 2. Check RAG documents
	ragRows, err := db.QueryContext(ctx, "SELECT id, source_path FROM rag_documents")
	if err == nil {
		defer ragRows.Close()
		for ragRows.Next() {
			var id, sourcePath string
			if err := ragRows.Scan(&id, &sourcePath); err == nil && sourcePath != "" {
				cleanP := filepath.Clean(sourcePath)
				slashP := filepath.ToSlash(cleanP)
				checkPath := resolveDataPath(dataDir, cleanP)
				if _, err := os.Stat(checkPath); os.IsNotExist(err) {
					report.OrphanRAGDocs = append(report.OrphanRAGDocs, slashP)
				}
			}
		}
	}

	// 3. Scan disk for ZIM files not registered in catalog_items
	zimDir := filepath.Join(dataDir, "zim")
	if _, err := os.Stat(zimDir); err == nil {
		_ = filepath.WalkDir(zimDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			if ext == ".zim" {
				relPath, _ := filepath.Rel(dataDir, p)
				cleanRel := filepath.Clean(relPath)
				slashRel := filepath.ToSlash(cleanRel)
				cleanAbs := filepath.Clean(p)

				_, foundRel := catalogMap[cleanRel]
				_, foundSlash := catalogMap[slashRel]
				_, foundAbs := catalogMap[cleanAbs]
				if !foundRel && !foundSlash && !foundAbs {
					report.OrphanFiles = append(report.OrphanFiles, slashRel)
				}
			}
			return nil
		})
	}

	return report, nil
}

// PruneOrphans remedies integrity discrepancies by reconciling DB state and optionally cleaning files.
func PruneOrphans(ctx context.Context, dataDir string, db *sql.DB, report *IntegrityReport, removeOrphanDiskFiles bool) error {
	// 1. Reset missing catalog_items
	for _, missing := range report.MissingFiles {
		cleanP := filepath.Clean(missing)
		slashP := filepath.ToSlash(cleanP)
		_, _ = db.ExecContext(ctx, "UPDATE catalog_items SET is_downloaded = 0, file_path = '' WHERE file_path = ? OR file_path = ?", cleanP, slashP)
	}

	// 2. Remove orphan RAG docs
	for _, orphanRAG := range report.OrphanRAGDocs {
		cleanP := filepath.Clean(orphanRAG)
		slashP := filepath.ToSlash(cleanP)
		_, _ = db.ExecContext(ctx, "DELETE FROM rag_documents WHERE source_path = ? OR source_path = ?", cleanP, slashP)
	}

	// 3. Delete orphan files if requested
	if removeOrphanDiskFiles {
		for _, orphanFile := range report.OrphanFiles {
			_ = os.Remove(resolveDataPath(dataDir, orphanFile))
		}
	}

	return nil
}
