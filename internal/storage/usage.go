package storage

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
)

// Supported storage category constants
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

// CategoryUsage details storage consumption of a single category.
type CategoryUsage struct {
	Category  string `json:"category"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	FileCount int    `json:"file_count"`
}

// DiskUsageReport provides an aggregated summary of ArkData disk usage and free space.
type DiskUsageReport struct {
	DataDir        string          `json:"data_dir"`
	Categories     []CategoryUsage `json:"categories"`
	TotalUsedBytes int64           `json:"total_used_bytes"`
	TotalFreeBytes int64           `json:"total_free_bytes"`
	DiskTotalBytes int64           `json:"disk_total_bytes"`
}

// FileItem represents an individual file in a category.
type FileItem struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Category  string    `json:"category"`
	Path      string    `json:"path"`
	SizeBytes int64     `json:"size_bytes"`
	ModTime   time.Time `json:"mod_time"`
}

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

// GetDiskUsage scans ArkData and computes disk usage by category plus filesystem free space.
func GetDiskUsage(ctx context.Context, dataDir string) (*DiskUsageReport, error) {
	report := &DiskUsageReport{
		DataDir:    dataDir,
		Categories: make([]CategoryUsage, 0, len(CategoryFolderMap)),
	}

	for cat, subPath := range CategoryFolderMap {
		fullPath := filepath.Join(dataDir, subPath)
		size, count := dirStats(fullPath)

		cu := CategoryUsage{
			Category:  cat,
			Path:      fullPath,
			SizeBytes: size,
			FileCount: count,
		}
		report.Categories = append(report.Categories, cu)
		report.TotalUsedBytes += size
	}

	// Sort categories by size descending
	sort.Slice(report.Categories, func(i, j int) bool {
		return report.Categories[i].SizeBytes > report.Categories[j].SizeBytes
	})

	// Get underlying disk filesystem capacity
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

// ListCategoryFiles enumerates all files stored in a specific category.
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

	// Sort items largest first
	sort.Slice(items, func(i, j int) bool {
		return items[i].SizeBytes > items[j].SizeBytes
	})

	return items, nil
}

func dirStats(path string) (int64, int) {
	var totalSize int64
	var fileCount int

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return 0, 0
	}

	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
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
