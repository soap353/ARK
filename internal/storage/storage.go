// Package storage manages Ark's local data directory (`ArkData`):
// computing disk usage by category, validating free space before downloads,
// auditing database/disk integrity, and cascading item deletions cleanly.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"ark/internal/kiwix"
	"github.com/shirou/gopsutil/v4/disk"
)

// Supported storage category constants.
const (
	CategoryZim        = "zim"
	CategoryMaps       = "maps"
	CategoryVideo      = "video"
	CategoryMusic      = "music"
	CategoryBooks      = "books"
	CategoryDocs       = "docs"
	CategoryDrugLabels = "druglabels"
	CategoryNotes      = "notes"
	CategoryKolibri    = "kolibri"
	CategoryAIModels   = "ai_models"
	CategoryOther      = "other"
)

const (
	// SpaceSafetyBufferBytes reserves 1 GiB so downloads never completely fill the OS/DB disk.
	SpaceSafetyBufferBytes int64 = 1 << 30
	// SpacePortionThreshold warns when a download would consume >= 25% of remaining free space.
	SpacePortionThreshold = 0.25
	// LowDiskUnknownSizeBytes warns on unknown-size downloads when free space is under 2 GiB.
	LowDiskUnknownSizeBytes int64 = 2 << 30
)

var sharedProbeClient = &http.Client{Timeout: 1500 * time.Millisecond}

// CategoryFolderMap maps category names to relative paths within ArkData.
var CategoryFolderMap = map[string]string{
	CategoryZim:        "zim",
	CategoryMaps:       "maps",
	CategoryVideo:      filepath.Join("media", "video"),
	CategoryMusic:      filepath.Join("media", "music"),
	CategoryBooks:      filepath.Join("media", "books"),
	CategoryDocs:       filepath.Join("media", "docs"),
	CategoryDrugLabels: "druglabels",
	CategoryNotes:      "notes",
	CategoryKolibri:    "kolibri",
	CategoryAIModels:   "ollama",
}

// CategoryUsage details storage consumption of a single category.
type CategoryUsage struct {
	Category  string `json:"category"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	FileCount int    `json:"file_count"`
}

// DiskUsageReport summarizes ArkData disk usage by category and filesystem free space.
type DiskUsageReport struct {
	DataDir        string          `json:"data_dir"`
	Categories     []CategoryUsage `json:"categories"`
	TotalUsedBytes int64           `json:"total_used_bytes"`
	TotalFreeBytes int64           `json:"total_free_bytes"`
	DiskTotalBytes int64           `json:"disk_total_bytes"`
}

// FileItem represents an individual file inside a storage category.
type FileItem struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Category  string    `json:"category"`
	Path      string    `json:"path"`
	SizeBytes int64     `json:"size_bytes"`
	ModTime   time.Time `json:"mod_time"`
}

// SpaceCheckResult summarizes available disk space vs. estimated download size.
type SpaceCheckResult struct {
	FreeBytes      uint64  `json:"free_bytes"`
	TotalBytes     uint64  `json:"total_bytes"`
	EstimatedBytes int64   `json:"estimated_bytes"`
	KnownSize      bool    `json:"known_size"`
	PercentOfFree  float64 `json:"percent_of_free"`
	Status         string  `json:"status"` // "ok", "warn", or "insufficient"
	Message        string  `json:"message"`
}

// IntegrityReport lists discrepancies between files on disk and SQLite records.
type IntegrityReport struct {
	OrphanFiles   []string `json:"orphan_files"`    // Files on disk not tracked in catalog_items
	MissingFiles  []string `json:"missing_files"`   // Catalog items marked downloaded but absent on disk
	OrphanRAGDocs []string `json:"orphan_rag_docs"` // RAG documents pointing to non-existent disk files
}

// DeleteTarget identifies an item to remove cleanly across disk, XML, and SQLite.
type DeleteTarget struct {
	ItemID   string `json:"item_id"`
	FilePath string `json:"file_path"` // relative to DataDir or absolute
	Category string `json:"category"`
}

// DeleteCascadeResult reports counts and byte savings from CascadeDelete.
type DeleteCascadeResult struct {
	FileDeleted            bool  `json:"file_deleted"`
	BytesFreed             int64 `json:"bytes_freed"`
	LibraryXMLEntryRemoved bool  `json:"library_xml_entry_removed"`
	USBProfileRefsRemoved  int   `json:"usb_profile_refs_removed"`
	DBRecordsRemoved       int   `json:"db_records_removed"`
}

// DeleteCascadeConfig supplies the data directory, library.xml path, and DB connection for deletion.
type DeleteCascadeConfig struct {
	DataDir        string
	LibraryXMLPath string
	KiwixManageBin string
	DB             *sql.DB
}

// GetDiskUsage scans ArkData and computes usage by category plus underlying disk free space.
func GetDiskUsage(ctx context.Context, dataDir string) (*DiskUsageReport, error) {
	report := &DiskUsageReport{
		DataDir:    dataDir,
		Categories: make([]CategoryUsage, 0, len(CategoryFolderMap)),
	}

	for cat, subPath := range CategoryFolderMap {
		fullPath := filepath.Join(dataDir, subPath)
		size, count := dirStats(fullPath)
		report.Categories = append(report.Categories, CategoryUsage{
			Category:  cat,
			Path:      fullPath,
			SizeBytes: size,
			FileCount: count,
		})
		report.TotalUsedBytes += size
	}

	sort.Slice(report.Categories, func(i, j int) bool {
		return report.Categories[i].SizeBytes > report.Categories[j].SizeBytes
	})

	probePath := dataDir
	if probePath == "" {
		if runtime.GOOS == "windows" {
			probePath = "C:\\"
		} else {
			probePath = "/"
		}
	}
	if dUsage, err := disk.UsageWithContext(ctx, probePath); err == nil && dUsage != nil {
		report.TotalFreeBytes = int64(dUsage.Free)
		report.DiskTotalBytes = int64(dUsage.Total)
	}

	return report, nil
}

// ListCategoryFiles enumerates all files stored in a specific category, largest first.
func ListCategoryFiles(dataDir, category string) ([]FileItem, error) {
	subPath, ok := CategoryFolderMap[category]
	if !ok {
		subPath = category
	}
	catDir := filepath.Join(dataDir, subPath)

	var items []FileItem
	if _, err := os.Stat(catDir); os.IsNotExist(err) {
		return items, nil
	}

	err := filepath.WalkDir(catDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		relPath, _ := filepath.Rel(dataDir, path)
		cleanID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		items = append(items, FileItem{
			ID:        cleanID,
			Name:      filepath.Base(path),
			Category:  category,
			Path:      filepath.ToSlash(relPath),
			SizeBytes: info.Size(),
			ModTime:   info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].SizeBytes > items[j].SizeBytes
	})
	return items, nil
}

func dirStats(path string) (int64, int) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return 0, 0
	}
	var totalSize int64
	var fileCount int
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			totalSize += info.Size()
			fileCount++
		}
		return nil
	})
	return totalSize, fileCount
}

// EvaluateSpace checks whether estimatedBytes fits safely inside freeBytes (keeping a 1 GiB buffer).
func EvaluateSpace(freeBytes, totalBytes uint64, estimatedBytes int64) SpaceCheckResult {
	res := SpaceCheckResult{
		FreeBytes:      freeBytes,
		TotalBytes:     totalBytes,
		EstimatedBytes: estimatedBytes,
		KnownSize:      estimatedBytes > 0,
		Status:         "ok",
	}

	if estimatedBytes <= 0 {
		if freeBytes < uint64(LowDiskUnknownSizeBytes) {
			res.Status = "warn"
			res.Message = fmt.Sprintf("Low disk space: only %s free (download size unknown).", FormatBytes(int64(freeBytes)))
			return res
		}
		res.Message = fmt.Sprintf("%s free on disk.", FormatBytes(int64(freeBytes)))
		return res
	}

	if freeBytes > 0 {
		res.PercentOfFree = (float64(estimatedBytes) / float64(freeBytes)) * 100.0
	} else {
		res.PercentOfFree = 100.0
	}

	if uint64(estimatedBytes) >= freeBytes || freeBytes < uint64(estimatedBytes)+uint64(SpaceSafetyBufferBytes) {
		res.Status = "insufficient"
		res.Message = fmt.Sprintf(
			"Not enough disk space: download needs ~%s (plus 1.0 GB safety buffer), only %s free.",
			FormatBytes(estimatedBytes),
			FormatBytes(int64(freeBytes)),
		)
		return res
	}

	if float64(estimatedBytes) >= float64(freeBytes)*SpacePortionThreshold {
		res.Status = "warn"
		res.Message = fmt.Sprintf(
			"Large download warning: ~%s will use %.0f%% of your %s remaining free space.",
			FormatBytes(estimatedBytes),
			res.PercentOfFree,
			FormatBytes(int64(freeBytes)),
		)
		return res
	}

	res.Message = fmt.Sprintf("%s free (~%s needed).", FormatBytes(int64(freeBytes)), FormatBytes(estimatedBytes))
	return res
}

// CheckDiskSpaceForPath queries filesystem free space for targetPath and evaluates estimatedBytes.
func CheckDiskSpaceForPath(ctx context.Context, targetPath string, estimatedBytes int64) (SpaceCheckResult, error) {
	checkPath := strings.TrimSpace(targetPath)
	if checkPath == "" {
		checkPath = "."
	}
	// Walk up until an existing ancestor directory is found (target file may not exist yet).
	for {
		if _, err := os.Stat(checkPath); err == nil {
			break
		}
		parent := filepath.Dir(checkPath)
		if parent == checkPath {
			checkPath = "."
			break
		}
		checkPath = parent
	}
	usage, err := disk.UsageWithContext(ctx, checkPath)
	if err != nil {
		return SpaceCheckResult{}, err
	}
	return EvaluateSpace(usage.Free, usage.Total, estimatedBytes), nil
}

// ParseSizeText parses human size strings like "~3.5 GB", "620 MB", or "27 GB" into bytes.
func ParseSizeText(s string) int64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "~")
	s = strings.ReplaceAll(s, ",", "")
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	numStr := fields[0]
	unit := ""
	if len(fields) > 1 {
		unit = fields[1]
	} else {
		for i, r := range numStr {
			if (r < '0' || r > '9') && r != '.' {
				unit = numStr[i:]
				numStr = numStr[:i]
				break
			}
		}
	}
	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil || val <= 0 {
		return 0
	}
	switch {
	case strings.HasPrefix(unit, "TB") || unit == "T":
		return int64(val * 1024 * 1024 * 1024 * 1024)
	case strings.HasPrefix(unit, "GB") || unit == "G":
		return int64(val * 1024 * 1024 * 1024)
	case strings.HasPrefix(unit, "MB") || unit == "M":
		return int64(val * 1024 * 1024)
	case strings.HasPrefix(unit, "KB") || unit == "K":
		return int64(val * 1024)
	default:
		return int64(val)
	}
}

// ProbeRemoteSize issues a quick HTTP HEAD request to inspect Content-Length when available.
func ProbeRemoteSize(ctx context.Context, rawURL string) int64 {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return 0
	}
	probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0
	}
	resp, err := sharedProbeClient.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	if resp.ContentLength > 0 {
		return resp.ContentLength
	}
	return 0
}

// FormatBytes formats a byte count as human-readable MB or GB.
func FormatBytes(b int64) string {
	if b <= 0 {
		return "0 MB"
	}
	const gb = 1024 * 1024 * 1024
	const mb = 1024 * 1024
	if b >= gb {
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	}
	return fmt.Sprintf("%.0f MB", float64(b)/float64(mb))
}

func resolveDataPath(dataDir, p string) string {
	if p == "" || filepath.IsAbs(p) || dataDir == "" {
		return p
	}
	return filepath.Join(dataDir, p)
}

// CheckIntegrity audits ArkData files against SQLite records to find missing or orphan entries.
func CheckIntegrity(ctx context.Context, dataDir string, db *sql.DB) (*IntegrityReport, error) {
	report := &IntegrityReport{
		OrphanFiles:   make([]string, 0),
		MissingFiles:  make([]string, 0),
		OrphanRAGDocs: make([]string, 0),
	}

	catalogMap := make(map[string]string)
	if rows, err := db.QueryContext(ctx, "SELECT id, file_path FROM catalog_items WHERE is_downloaded = 1"); err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, path string
			if err := rows.Scan(&id, &path); err == nil && path != "" {
				cleanP := filepath.Clean(path)
				slashP := filepath.ToSlash(cleanP)
				catalogMap[cleanP] = id
				catalogMap[slashP] = id
				if _, err := os.Stat(resolveDataPath(dataDir, cleanP)); os.IsNotExist(err) {
					report.MissingFiles = append(report.MissingFiles, slashP)
				}
			}
		}
	}

	if ragRows, err := db.QueryContext(ctx, "SELECT id, source_path FROM rag_documents"); err == nil {
		defer ragRows.Close()
		for ragRows.Next() {
			var id, sourcePath string
			if err := ragRows.Scan(&id, &sourcePath); err == nil && sourcePath != "" {
				slashP := filepath.ToSlash(filepath.Clean(sourcePath))
				if _, err := os.Stat(resolveDataPath(dataDir, sourcePath)); os.IsNotExist(err) {
					report.OrphanRAGDocs = append(report.OrphanRAGDocs, slashP)
				}
			}
		}
	}

	zimDir := filepath.Join(dataDir, "zim")
	if _, err := os.Stat(zimDir); err == nil {
		_ = filepath.WalkDir(zimDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() || strings.ToLower(filepath.Ext(p)) != ".zim" {
				return nil
			}
			relPath, _ := filepath.Rel(dataDir, p)
			cleanRel := filepath.Clean(relPath)
			slashRel := filepath.ToSlash(cleanRel)
			cleanAbs := filepath.Clean(p)
			if catalogMap[cleanRel] == "" && catalogMap[slashRel] == "" && catalogMap[cleanAbs] == "" {
				report.OrphanFiles = append(report.OrphanFiles, slashRel)
			}
			return nil
		})
	}

	return report, nil
}

// PruneOrphans reconciles database discrepancies found by CheckIntegrity and optionally deletes orphan files.
func PruneOrphans(ctx context.Context, dataDir string, db *sql.DB, report *IntegrityReport, removeOrphanDiskFiles bool) error {
	for _, missing := range report.MissingFiles {
		cleanP := filepath.Clean(missing)
		slashP := filepath.ToSlash(cleanP)
		_, _ = db.ExecContext(ctx, "UPDATE catalog_items SET is_downloaded = 0, file_path = '' WHERE file_path = ? OR file_path = ?", cleanP, slashP)
	}

	for _, orphanRAG := range report.OrphanRAGDocs {
		cleanP := filepath.Clean(orphanRAG)
		slashP := filepath.ToSlash(cleanP)
		_, _ = db.ExecContext(ctx, "DELETE FROM rag_documents WHERE source_path = ? OR source_path = ?", cleanP, slashP)
	}

	if removeOrphanDiskFiles {
		for _, orphanFile := range report.OrphanFiles {
			_ = os.Remove(resolveDataPath(dataDir, orphanFile))
		}
	}

	return nil
}

// CascadeDelete removes a target file along with its library.xml entry, USB profile refs, and DB rows.
func CascadeDelete(ctx context.Context, cfg DeleteCascadeConfig, target DeleteTarget) (*DeleteCascadeResult, error) {
	res := &DeleteCascadeResult{}
	absFilePath := resolveDataPath(cfg.DataDir, target.FilePath)

	if absFilePath != "" {
		if fi, err := os.Stat(absFilePath); err == nil {
			res.BytesFreed = fi.Size()
			if err := os.Remove(absFilePath); err == nil {
				res.FileDeleted = true
			}
		}
		_ = os.Remove(absFilePath + ".part")
		_ = os.Remove(absFilePath + ".aria2")
	}

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

	if cfg.DB != nil {
		refsRemoved, _ := purgeUSBProfileRefs(ctx, cfg.DB, target, absFilePath)
		res.USBProfileRefsRemoved = refsRemoved
		res.DBRecordsRemoved = cleanDBRecords(ctx, cfg.DB, target, absFilePath)
	}

	return res, nil
}

func purgeUSBProfileRefs(ctx context.Context, db *sql.DB, target DeleteTarget, absPath string) (int, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, config_json FROM usb_profiles")
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type profileRow struct {
		id, configJSON string
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
		items, ok := cfgMap["items"].([]any)
		if !ok {
			continue
		}

		modified := false
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
			newJSON, _ := json.Marshal(cfgMap)
			if _, err := db.ExecContext(ctx, "UPDATE usb_profiles SET config_json = ? WHERE id = ?", string(newJSON), p.id); err == nil {
				updatedCount++
			}
		}
	}
	return updatedCount, nil
}

func cleanDBRecords(ctx context.Context, db *sql.DB, target DeleteTarget, absPath string) int {
	totalCleaned := 0

	if res, err := db.ExecContext(ctx, `
		UPDATE catalog_items
		SET is_downloaded = 0, file_path = ''
		WHERE id = ? OR file_path = ? OR file_path = ?
	`, target.ItemID, target.FilePath, absPath); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			totalCleaned += int(n)
		}
	}

	// Remove FTS5/SQLite chunks belonging to any matching rag_documents entry first.
	_, _ = db.ExecContext(ctx, `
		DELETE FROM rag_chunks
		WHERE doc_id IN (SELECT id FROM rag_documents WHERE id = ? OR source_path = ? OR source_path = ?)
	`, target.ItemID, target.FilePath, absPath)

	if res, err := db.ExecContext(ctx, `
		DELETE FROM rag_documents
		WHERE id = ? OR source_path = ? OR source_path = ?
	`, target.ItemID, target.FilePath, absPath); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			totalCleaned += int(n)
		}
	}

	return totalCleaned
}
