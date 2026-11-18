package usb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	"ark/internal/storage"
)

const fat32MaxFileBytes int64 = 4*1024*1024*1024 - 1

// DriveInfo summarizes a mounted filesystem candidate for USB export.
type DriveInfo struct {
	MountPoint   string `json:"mount_point"`
	Device       string `json:"device"`
	FSType       string `json:"fs_type"`
	TotalBytes   uint64 `json:"total_bytes"`
	FreeBytes    uint64 `json:"free_bytes"`
	FAT32Warning bool   `json:"fat32_warning"`
}

// CategoryInfo describes an exportable ArkData category.
type CategoryInfo struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	RelPath   string `json:"rel_path"`
	FileCount int    `json:"file_count"`
	SizeBytes int64  `json:"size_bytes"`
}

// ExportConfig configures a USB offline export bundle.
type ExportConfig struct {
	TargetPath          string   `json:"target_path"`
	Categories          []string `json:"categories"`
	IncludeDockerImages bool     `json:"include_docker_images"`
	DockerImageNames    []string `json:"docker_image_names,omitempty"`
	IncludeConfig       bool     `json:"include_config"`
	IncludeSensitive    bool     `json:"include_sensitive,omitempty"`
	BinaryPath          string   `json:"binary_path,omitempty"`
}

// ExportProfile stores a named USB export configuration in SQLite.
type ExportProfile struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Config      ExportConfig `json:"config"`
	CreatedAt   string       `json:"created_at"`
}

// ManifestFile describes a single file tracked in USB manifest.json.
type ManifestFile struct {
	RelPath string `json:"rel_path"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// ExportManifest records all files and Docker images in a USB bundle.
type ExportManifest struct {
	Version      string         `json:"version"`
	ExportedAt   time.Time      `json:"exported_at"`
	Categories   []string       `json:"categories"`
	DockerImages []string       `json:"docker_images,omitempty"`
	Files        []ManifestFile `json:"files"`
}

// ExportResult reports incremental copy statistics for an export run.
type ExportResult struct {
	TargetPath   string `json:"target_path"`
	FilesCopied  int    `json:"files_copied"`
	FilesSkipped int    `json:"files_skipped"`
	BytesCopied  int64  `json:"bytes_copied"`
	ManifestPath string `json:"manifest_path"`
}

// ImageSaver writes Docker images as a tarball to out.
type ImageSaver func(ctx context.Context, imageNames []string, out io.Writer) error

var categoryLabels = map[string]string{
	storage.CategoryZim:        "Wikipedia & ZIM Libraries",
	storage.CategoryMaps:       "Offline Maps",
	storage.CategoryVideo:      "Video Media",
	storage.CategoryMusic:      "Music & Audio",
	storage.CategoryBooks:      "Books & Reference",
	storage.CategoryDocs:       "Documents",
	storage.CategoryDrugLabels: "FDA Drug Labels",
	storage.CategoryNotes:      "Notes",
	storage.CategoryKolibri:    "Kolibri Education",
	storage.CategoryAIModels:   "AI Models (Ollama)",
}

var ignoredFSTypes = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "squashfs": true, "overlay": true,
	"proc": true, "sysfs": true, "cgroup": true, "cgroup2": true,
}

// ListDrives enumerates mounted filesystems suitable as export targets and flags FAT32 drives.
func ListDrives(ctx context.Context) ([]DriveInfo, error) {
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return nil, err
	}
	var drives []DriveInfo
	for _, p := range parts {
		fsLower := strings.ToLower(strings.TrimSpace(p.Fstype))
		if ignoredFSTypes[fsLower] || p.Mountpoint == "" {
			continue
		}
		if runtime.GOOS != "windows" && strings.HasPrefix(p.Mountpoint, "/snap") {
			continue
		}
		usage, err := disk.UsageWithContext(ctx, p.Mountpoint)
		if err != nil || usage.Total == 0 {
			continue
		}
		drives = append(drives, DriveInfo{
			MountPoint:   p.Mountpoint,
			Device:       p.Device,
			FSType:       p.Fstype,
			TotalBytes:   usage.Total,
			FreeBytes:    usage.Free,
			FAT32Warning: strings.Contains(strings.ToUpper(p.Fstype), "FAT32"),
		})
	}
	sort.Slice(drives, func(i, j int) bool {
		return drives[i].MountPoint < drives[j].MountPoint
	})
	return drives, nil
}

// ScanCategories inspects ArkData and returns category sizes and file counts.
func ScanCategories(ctx context.Context, dataDir string) ([]CategoryInfo, error) {
	report, err := storage.GetDiskUsage(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	out := make([]CategoryInfo, 0, len(report.Categories))
	for _, c := range report.Categories {
		label := categoryLabels[c.Category]
		if label == "" {
			label = c.Category
		}
		out = append(out, CategoryInfo{
			Key:       c.Category,
			Label:     label,
			RelPath:   storage.CategoryFolderMap[c.Category],
			FileCount: c.FileCount,
			SizeBytes: c.SizeBytes,
		})
	}
	return out, nil
}

// SaveProfile inserts or updates a named USB export profile in SQLite.
func SaveProfile(db *sql.DB, name, description string, cfg ExportConfig) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("profile name is required")
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	res, err := db.Exec(`UPDATE usb_profiles SET description = ?, config_json = ? WHERE name = ?`,
		description, string(raw), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = db.Exec(`INSERT INTO usb_profiles (name, description, config_json) VALUES (?, ?, ?)`,
		name, description, string(raw))
	return err
}

// ListProfiles returns all saved USB export profiles ordered by name.
func ListProfiles(db *sql.DB) ([]ExportProfile, error) {
	rows, err := db.Query(`SELECT id, name, COALESCE(description, ''), config_json, created_at FROM usb_profiles ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []ExportProfile
	for rows.Next() {
		var p ExportProfile
		var raw string
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &raw, &p.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &p.Config)
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

// GetProfile looks up a single saved USB export profile by name.
func GetProfile(db *sql.DB, name string) (*ExportProfile, error) {
	var p ExportProfile
	var raw string
	err := db.QueryRow(
		`SELECT id, name, COALESCE(description, ''), config_json, created_at FROM usb_profiles WHERE name = ?`,
		strings.TrimSpace(name),
	).Scan(&p.ID, &p.Name, &p.Description, &raw, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &p.Config); err != nil {
		return nil, err
	}
	return &p, nil
}

type copyItem struct {
	srcAbs  string
	relDest string
	size    int64
	mode    os.FileMode
}

// ExportToUSB copies selected ArkData categories, sanitized ark.db, binary, and Docker images to targetPath.
// Unchanged files matching both manifest.json SHA-256 and destination digest are skipped cleanly.
func ExportToUSB(ctx context.Context, dataDir string, cfg ExportConfig, imageSaver ImageSaver, onProgress func(pct float64, msg string)) (*ExportResult, error) {
	target := strings.TrimSpace(cfg.TargetPath)
	if target == "" {
		return nil, errors.New("target path is required")
	}
	if err := os.MkdirAll(target, 0755); err != nil {
		return nil, fmt.Errorf("create target directory: %w", err)
	}

	isFAT32 := detectFAT32(ctx, target)
	prevManifest := readExistingManifest(filepath.Join(target, "manifest.json"))

	var items []copyItem
	var tempCleanups []string
	defer func() {
		for _, p := range tempCleanups {
			_ = os.RemoveAll(p)
		}
	}()

	for _, cat := range cfg.Categories {
		sub, ok := storage.CategoryFolderMap[cat]
		if !ok {
			return nil, fmt.Errorf("unknown USB export category: %s", cat)
		}
		srcCatDir := filepath.Join(dataDir, sub)
		if info, err := os.Stat(srcCatDir); err == nil && info.IsDir() {
			err := filepath.WalkDir(srcCatDir, func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil || d.IsDir() {
					return walkErr
				}
				fi, err := d.Info()
				if err != nil {
					return err
				}
				relInCat, err := filepath.Rel(dataDir, path)
				if err != nil {
					return err
				}
				items = append(items, copyItem{
					srcAbs:  path,
					relDest: filepath.ToSlash(filepath.Join("data", relInCat)),
					size:    fi.Size(),
					mode:    0644,
				})
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("scan category %s: %w", cat, err)
			}
		}
		if cat == storage.CategoryZim {
			libPath := filepath.Join(dataDir, "library.xml")
			if fi, err := os.Stat(libPath); err == nil && !fi.IsDir() {
				items = append(items, copyItem{
					srcAbs:  libPath,
					relDest: "data/library.xml",
					size:    fi.Size(),
					mode:    0644,
				})
			}
		}
	}

	if cfg.IncludeConfig {
		dbSrc := filepath.Join(dataDir, "ark.db")
		if _, err := os.Stat(dbSrc); err == nil {
			tmpDir, err := os.MkdirTemp("", "ark-usb-db-*")
			if err != nil {
				return nil, err
			}
			tempCleanups = append(tempCleanups, tmpDir)
			sanitizedDB := filepath.Join(tmpDir, "ark.db")
			if err := exportSanitizedDB(dbSrc, sanitizedDB, cfg.IncludeSensitive); err != nil {
				return nil, fmt.Errorf("sanitize config database: %w", err)
			}
			if fi, err := os.Stat(sanitizedDB); err == nil {
				items = append(items, copyItem{
					srcAbs:  sanitizedDB,
					relDest: "config/ark.db",
					size:    fi.Size(),
					mode:    0600,
				})
			}
		}
	}

	if cfg.BinaryPath != "" {
		if fi, err := os.Stat(cfg.BinaryPath); err == nil && !fi.IsDir() {
			items = append(items, copyItem{
				srcAbs:  cfg.BinaryPath,
				relDest: filepath.ToSlash(filepath.Join("bin", filepath.Base(cfg.BinaryPath))),
				size:    fi.Size(),
				mode:    0755,
			})
		}
	}

	for _, it := range items {
		if isFAT32 && it.size > fat32MaxFileBytes {
			return nil, fmt.Errorf("file %s (%d bytes) exceeds FAT32 4 GB file limit; format USB drive as exFAT, NTFS, or ext4", it.relDest, it.size)
		}
	}

	// Copy static content + config files incrementally using SHA-256 manifest verification.
	res := &ExportResult{
		TargetPath:   target,
		ManifestPath: filepath.Join(target, "manifest.json"),
	}
	manifest := ExportManifest{
		Version:    "1",
		ExportedAt: time.Now().UTC(),
		Categories: append([]string(nil), cfg.Categories...),
		Files:      make([]ManifestFile, 0, len(items)+1),
	}

	totalSteps := len(items)
	if cfg.IncludeDockerImages && len(cfg.DockerImageNames) > 0 && imageSaver != nil {
		totalSteps++
	}
	if totalSteps == 0 {
		totalSteps = 1
	}

	for idx, it := range items {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		srcHash, err := fileSHA256(it.srcAbs)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", it.srcAbs, err)
		}

		dstAbs := filepath.Join(target, filepath.FromSlash(it.relDest))
		if prev, ok := prevManifest[it.relDest]; ok && prev.SHA256 == srcHash && prev.Size == it.size {
			if dstFI, err := os.Stat(dstAbs); err == nil && dstFI.Size() == it.size {
				if dstHash, err := fileSHA256(dstAbs); err == nil && dstHash == srcHash {
					res.FilesSkipped++
					manifest.Files = append(manifest.Files, prev)
					if onProgress != nil {
						onProgress(float64(idx+1)/float64(totalSteps)*100, "Verified unchanged: "+it.relDest)
					}
					continue
				}
			}
		}

		if err := ensureFreeSpace(ctx, target, it.size); err != nil {
			return nil, err
		}
		if err := copyFileAtomic(it.srcAbs, dstAbs, it.mode); err != nil {
			return nil, fmt.Errorf("copy %s: %w", it.relDest, err)
		}
		res.FilesCopied++
		res.BytesCopied += it.size
		manifest.Files = append(manifest.Files, ManifestFile{
			RelPath: it.relDest,
			Size:    it.size,
			SHA256:  srcHash,
		})
		if onProgress != nil {
			onProgress(float64(idx+1)/float64(totalSteps)*100, "Copied "+it.relDest)
		}
	}

	// Optional Docker images bundle (.tar)
	if cfg.IncludeDockerImages && len(cfg.DockerImageNames) > 0 && imageSaver != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		relTar := "images/ark-images.tar"
		dstTar := filepath.Join(target, filepath.FromSlash(relTar))
		if err := os.MkdirAll(filepath.Dir(dstTar), 0755); err != nil {
			return nil, err
		}
		tmpTar := dstTar + ".tmp"
		f, err := os.OpenFile(tmpTar, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return nil, err
		}
		saveErr := imageSaver(ctx, cfg.DockerImageNames, f)
		closeErr := f.Close()
		if saveErr != nil {
			_ = os.Remove(tmpTar)
			return nil, fmt.Errorf("export docker images: %w", saveErr)
		}
		if closeErr != nil {
			_ = os.Remove(tmpTar)
			return nil, closeErr
		}
		fi, err := os.Stat(tmpTar)
		if err != nil {
			return nil, err
		}
		if isFAT32 && fi.Size() > fat32MaxFileBytes {
			_ = os.Remove(tmpTar)
			return nil, fmt.Errorf("docker image archive (%d bytes) exceeds FAT32 4 GB file limit", fi.Size())
		}
		if err := os.Rename(tmpTar, dstTar); err != nil {
			_ = os.Remove(tmpTar)
			return nil, err
		}
		tarHash, err := fileSHA256(dstTar)
		if err != nil {
			return nil, err
		}
		res.FilesCopied++
		res.BytesCopied += fi.Size()
		manifest.DockerImages = append([]string(nil), cfg.DockerImageNames...)
		manifest.Files = append(manifest.Files, ManifestFile{
			RelPath: relTar,
			Size:    fi.Size(),
			SHA256:  tarHash,
		})
		if onProgress != nil {
			onProgress(100, "Exported Docker images archive")
		}
	}

	sort.Slice(manifest.Files, func(i, j int) bool {
		return manifest.Files[i].RelPath < manifest.Files[j].RelPath
	})
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	tmpMan := res.ManifestPath + ".tmp"
	if err := os.WriteFile(tmpMan, rawManifest, 0644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpMan, res.ManifestPath); err != nil {
		return nil, err
	}
	return res, nil
}

// VerifyManifest validates every file listed in <targetPath>/manifest.json against size and SHA-256.
func VerifyManifest(targetPath string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(targetPath, "manifest.json"))
	if err != nil {
		return 0, fmt.Errorf("read manifest.json: %w", err)
	}
	var m ExportManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("parse manifest.json: %w", err)
	}
	for _, mf := range m.Files {
		p := filepath.Join(targetPath, filepath.FromSlash(mf.RelPath))
		fi, err := os.Stat(p)
		if err != nil {
			return 0, fmt.Errorf("missing bundle file %s: %w", mf.RelPath, err)
		}
		if fi.Size() != mf.Size {
			return 0, fmt.Errorf("size mismatch for %s: got %d, want %d", mf.RelPath, fi.Size(), mf.Size)
		}
		actual, err := fileSHA256(p)
		if err != nil {
			return 0, err
		}
		if !strings.EqualFold(actual, mf.SHA256) {
			return 0, fmt.Errorf("sha256 mismatch for %s: got %s, want %s", mf.RelPath, actual, mf.SHA256)
		}
	}
	return len(m.Files), nil
}

// MoveDataDir copies all files from srcDir to dstDir, verifies SHA-256 digests, and returns (filesMoved, bytesCopied, err).
// It rejects overlapping destinations and preserves srcDir intact if verification fails.
func MoveDataDir(srcDir, dstDir string) (int, int64, error) {
	srcAbs, err := filepath.Abs(srcDir)
	if err != nil {
		return 0, 0, err
	}
	dstAbs, err := filepath.Abs(dstDir)
	if err != nil {
		return 0, 0, err
	}
	if srcAbs == dstAbs {
		return 0, 0, errors.New("destination matches current ArkData directory")
	}
	if strings.HasPrefix(strings.ToLower(dstAbs)+string(os.PathSeparator), strings.ToLower(srcAbs)+string(os.PathSeparator)) {
		return 0, 0, errors.New("destination cannot be inside current ArkData directory")
	}
	if _, err := os.Stat(srcAbs); err != nil {
		return 0, 0, fmt.Errorf("source ArkData directory unreadable: %w", err)
	}
	if err := os.MkdirAll(dstAbs, 0755); err != nil {
		return 0, 0, fmt.Errorf("create destination directory: %w", err)
	}

	var copiedFiles int
	var copiedBytes int64
	err = filepath.WalkDir(srcAbs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dstAbs, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		srcHash, err := fileSHA256(path)
		if err != nil {
			return err
		}
		if err := copyFileAtomic(path, target, 0644); err != nil {
			return err
		}
		dstHash, err := fileSHA256(target)
		if err != nil || dstHash != srcHash {
			return fmt.Errorf("verification failed after copying %s", rel)
		}
		copiedFiles++
		copiedBytes += fi.Size()
		return nil
	})
	if err != nil {
		return copiedFiles, copiedBytes, err
	}
	return copiedFiles, copiedBytes, nil
}

func detectFAT32(ctx context.Context, target string) bool {
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	drives, err := ListDrives(ctx)
	if err != nil {
		return false
	}
	bestLen := -1
	bestFAT32 := false
	normTarget := strings.ToLower(absTarget)
	for _, d := range drives {
		mp := strings.ToLower(filepath.Clean(d.MountPoint))
		if strings.HasPrefix(normTarget, mp) && len(mp) > bestLen {
			bestLen = len(mp)
			bestFAT32 = d.FAT32Warning
		}
	}
	return bestFAT32
}

func ensureFreeSpace(ctx context.Context, target string, needed int64) error {
	usage, err := disk.UsageWithContext(ctx, target)
	if err != nil || usage.Total == 0 {
		return nil
	}
	if int64(usage.Free) < needed {
		return fmt.Errorf("insufficient free space on %s: need %d bytes, have %d bytes", target, needed, usage.Free)
	}
	return nil
}

func readExistingManifest(path string) map[string]ManifestFile {
	out := make(map[string]ManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var m ExportManifest
	if json.Unmarshal(raw, &m) != nil {
		return out
	}
	for _, f := range m.Files {
		out[f.RelPath] = f
	}
	return out
}

func exportSanitizedDB(srcDB, dstDB string, includeSensitive bool) error {
	d, err := sql.Open("sqlite", srcDB)
	if err != nil {
		return copyFileAtomic(srcDB, dstDB, 0600)
	}
	defer d.Close()

	escaped := strings.ReplaceAll(dstDB, "'", "''")
	if _, err := d.Exec(fmt.Sprintf("VACUUM INTO '%s'", escaped)); err != nil {
		return copyFileAtomic(srcDB, dstDB, 0600)
	}
	if includeSensitive {
		return nil
	}
	outDB, err := sql.Open("sqlite", dstDB)
	if err != nil {
		return err
	}
	defer outDB.Close()
	// Strip credentials unless explicit opt-in was requested.
	_, _ = outDB.Exec(`DELETE FROM settings WHERE key IN ('admin_password_hash', 'ai_api_key')`)
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFileAtomic(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
