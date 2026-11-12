package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ark/internal/kiwix"
)

// DeleteTarget specifies what is being targeted for cascading deletion.
type DeleteTarget struct {
	ItemID   string `json:"item_id"`
	FilePath string `json:"file_path"` // relative to DataDir or absolute
	Category string `json:"category"`
}

// DeleteCascadeResult details the items and counts purged across subsystems.
type DeleteCascadeResult struct {
	FileDeleted            bool  `json:"file_deleted"`
	BytesFreed             int64 `json:"bytes_freed"`
	LibraryXMLEntryRemoved bool  `json:"library_xml_entry_removed"`
	QdrantPointsDeleted    int   `json:"qdrant_points_deleted"`
	USBProfileRefsRemoved  int   `json:"usb_profile_refs_removed"`
	DBRecordsRemoved       int   `json:"db_records_removed"`
}

// DeleteCascadeConfig provides necessary dependencies for cascading cleanup.
type DeleteCascadeConfig struct {
	DataDir        string
	LibraryXMLPath string
	KiwixManageBin string
	QdrantURL      string // e.g. "http://127.0.0.1:6333"
	HTTPClient     *http.Client
	DB             *sql.DB
}

// CascadeDelete removes the target across all subsystems leaving zero orphans.
func CascadeDelete(ctx context.Context, cfg DeleteCascadeConfig, target DeleteTarget) (*DeleteCascadeResult, error) {
	res := &DeleteCascadeResult{}

	// Resolve absolute file path
	absFilePath := target.FilePath
	if absFilePath != "" && !filepath.IsAbs(absFilePath) && cfg.DataDir != "" {
		absFilePath = filepath.Join(cfg.DataDir, target.FilePath)
	}

	// 1. Remove File from Disk
	if absFilePath != "" {
		if fi, err := os.Stat(absFilePath); err == nil {
			res.BytesFreed = fi.Size()
			if err := os.Remove(absFilePath); err == nil {
				res.FileDeleted = true
			}
		}
		// Also clean up any potential download part or sidecar files
		_ = os.Remove(absFilePath + ".part")
		_ = os.Remove(absFilePath + ".aria2")
	}

	// 2. Remove entry from library.xml (via kiwix-manage or pure-Go XML updater)
	if cfg.LibraryXMLPath != "" {
		lm := kiwix.NewLibraryManager(cfg.LibraryXMLPath, cfg.KiwixManageBin)
		identifier := target.ItemID
		if identifier == "" {
			identifier = target.FilePath
		}
		if err := lm.RemoveZIM(ctx, identifier); err == nil {
			res.LibraryXMLEntryRemoved = true
		}
		if absFilePath != "" {
			_ = lm.RemoveZIM(ctx, absFilePath)
		}
	}

	// 3. Remove Qdrant vector points
	if cfg.QdrantURL != "" && cfg.DB != nil {
		deletedPoints := purgeQdrantPoints(ctx, cfg, target, absFilePath)
		res.QdrantPointsDeleted = deletedPoints
	}

	// 4. Remove USB Profile references
	if cfg.DB != nil {
		refsRemoved, _ := purgeUSBProfileRefs(ctx, cfg.DB, target, absFilePath)
		res.USBProfileRefsRemoved = refsRemoved
	}

	// 5. Clean SQLite database records (catalog_items and rag_documents)
	if cfg.DB != nil {
		cleanedDB := cleanDBRecords(ctx, cfg.DB, target, absFilePath)
		res.DBRecordsRemoved = cleanedDB
	}

	return res, nil
}

func purgeQdrantPoints(ctx context.Context, cfg DeleteCascadeConfig, target DeleteTarget, absPath string) int {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	// Find associated collections in rag_documents
	rows, err := cfg.DB.QueryContext(ctx, `
		SELECT qdrant_collection FROM rag_documents
		WHERE id = ? OR source_path = ? OR source_path = ?
	`, target.ItemID, target.FilePath, absPath)
	if err != nil {
		return 0
	}
	defer rows.Close()

	collections := make(map[string]bool)
	for rows.Next() {
		var coll string
		if err := rows.Scan(&coll); err == nil && coll != "" {
			collections[coll] = true
		}
	}

	if len(collections) == 0 {
		collections["ark_knowledge"] = true
	}

	totalPointsDeleted := 0
	for coll := range collections {
		filterPayload := map[string]any{
			"filter": map[string]any{
				"should": []map[string]any{
					{"key": "source_path", "match": map[string]any{"value": target.FilePath}},
					{"key": "source_path", "match": map[string]any{"value": absPath}},
					{"key": "item_id", "match": map[string]any{"value": target.ItemID}},
				},
			},
		}

		bodyBytes, err := json.Marshal(filterPayload)
		if err != nil {
			continue
		}

		deleteURL := fmt.Sprintf("%s/collections/%s/points/delete", strings.TrimRight(cfg.QdrantURL, "/"), coll)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, deleteURL, bytes.NewReader(bodyBytes))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				totalPointsDeleted++
			}
		}
	}

	return totalPointsDeleted
}

func purgeUSBProfileRefs(ctx context.Context, db *sql.DB, target DeleteTarget, absPath string) (int, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, config_json FROM usb_profiles")
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type profileRow struct {
		id         string
		configJSON string
	}
	var profiles []profileRow
	for rows.Next() {
		var p profileRow
		if err := rows.Scan(&p.id, &p.configJSON); err == nil {
			profiles = append(profiles, p)
		}
	}

	terms := []string{target.ItemID, target.FilePath}
	if absPath != "" {
		terms = append(terms, filepath.Base(absPath))
	}

	updatedCount := 0
	for _, p := range profiles {
		var cfgMap map[string]any
		if err := json.Unmarshal([]byte(p.configJSON), &cfgMap); err != nil {
			continue
		}

		modified := false
		if items, ok := cfgMap["items"].([]any); ok {
			var newItems []any
			for _, item := range items {
				itemStr := fmt.Sprintf("%v", item)
				matched := false
				for _, term := range terms {
					if term != "" && strings.Contains(itemStr, term) {
						matched = true
						modified = true
						break
					}
				}
				if !matched {
					newItems = append(newItems, item)
				}
			}
			if modified {
				cfgMap["items"] = newItems
			}
		}

		if modified {
			newJSON, _ := json.Marshal(cfgMap)
			_, err := db.ExecContext(ctx, "UPDATE usb_profiles SET config_json = ? WHERE id = ?", string(newJSON), p.id)
			if err == nil {
				updatedCount++
			}
		}
	}

	return updatedCount, nil
}

func cleanDBRecords(ctx context.Context, db *sql.DB, target DeleteTarget, absPath string) int {
	totalCleaned := 0

	// 1. catalog_items
	res1, err := db.ExecContext(ctx, `
		UPDATE catalog_items
		SET is_downloaded = 0, file_path = ''
		WHERE id = ? OR file_path = ? OR file_path = ?
	`, target.ItemID, target.FilePath, absPath)
	if err == nil {
		if n, _ := res1.RowsAffected(); n > 0 {
			totalCleaned += int(n)
		}
	}

	// 2. rag_documents
	res2, err := db.ExecContext(ctx, `
		DELETE FROM rag_documents
		WHERE id = ? OR source_path = ? OR source_path = ?
	`, target.ItemID, target.FilePath, absPath)
	if err == nil {
		if n, _ := res2.RowsAffected(); n > 0 {
			totalCleaned += int(n)
		}
	}

	return totalCleaned
}
