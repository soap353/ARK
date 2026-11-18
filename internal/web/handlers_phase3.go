package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ark/internal/jobs"
	"ark/internal/kiwix"
	"ark/internal/update"
	"ark/internal/usb"
)

// MapArchiveView represents a .pmtiles archive on disk in ArkData/maps.
type MapArchiveView struct {
	Name     string
	FileName string
	SizeText string
}

// USBDriveView formats a mounted drive for the Settings USB wizard.
type USBDriveView struct {
	MountPoint   string
	FSType       string
	FreeGB       string
	TotalGB      string
	FAT32Warning bool
}

// USBCategoryView formats an ArkData category for USB export selection.
type USBCategoryView struct {
	Key       string
	Label     string
	FileCount int
	SizeText  string
	SizeBytes int64
}

func (s *Server) populatePhase3Settings(ctx context.Context, data *PageData) {
	if drives, err := usb.ListDrives(ctx); err == nil {
		for _, d := range drives {
			data.USBDrives = append(data.USBDrives, USBDriveView{
				MountPoint:   d.MountPoint,
				FSType:       d.FSType,
				FreeGB:       fmt.Sprintf("%.1f GB", float64(d.FreeBytes)/(1024*1024*1024)),
				TotalGB:      fmt.Sprintf("%.1f GB", float64(d.TotalBytes)/(1024*1024*1024)),
				FAT32Warning: d.FAT32Warning,
			})
		}
	}

	if cats, err := usb.ScanCategories(ctx, s.dataDir); err == nil {
		for _, c := range cats {
			data.USBCategories = append(data.USBCategories, USBCategoryView{
				Key:       c.Key,
				Label:     c.Label,
				FileCount: c.FileCount,
				SizeText:  formatHumanBytes(c.SizeBytes),
				SizeBytes: c.SizeBytes,
			})
		}
	}

	if s.db != nil {
		if profiles, err := usb.ListProfiles(s.db); err == nil {
			data.USBProfiles = profiles
		}
	}

	data.RollbackAvailable = update.HasRollbackAvailable("")
	data.RollbackNotice = update.ConsumeRollbackNotice(s.dataDir)
}

func formatHumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func (s *Server) handleSettingsUSBProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("Invalid form submission"), http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("profile_name"))
	desc := strings.TrimSpace(r.FormValue("profile_desc"))
	cfg := usb.ExportConfig{
		Categories:          r.Form["categories"],
		IncludeDockerImages: r.FormValue("include_docker") != "",
		IncludeConfig:       r.FormValue("include_config") != "",
	}
	if s.db != nil {
		if err := usb.SaveProfile(s.db, name, desc, cfg); err != nil {
			http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/settings?msg="+url.QueryEscape("USB export profile saved."), http.StatusSeeOther)
}

func (s *Server) handleSettingsUSBExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("Invalid form submission"), http.StatusSeeOther)
		return
	}

	target := strings.TrimSpace(r.FormValue("target_path"))
	custom := strings.TrimSpace(r.FormValue("custom_path"))
	if target == "custom" || target == "" {
		target = custom
	}
	if target == "" {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("Select a target drive or enter a custom destination path."), http.StatusSeeOther)
		return
	}

	categories := r.Form["categories"]
	includeDocker := r.FormValue("include_docker") != ""
	includeConfig := r.FormValue("include_config") != ""
	if len(categories) == 0 && !includeDocker && !includeConfig {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("Select at least one content category, Docker images, or settings to export."), http.StatusSeeOther)
		return
	}

	exePath, _ := os.Executable()
	cfg := usb.ExportConfig{
		TargetPath:          target,
		Categories:          categories,
		IncludeDockerImages: includeDocker,
		IncludeConfig:       includeConfig,
		BinaryPath:          exePath,
	}
	if includeDocker {
		cfg.DockerImageNames = s.installedImageNames()
	}

	if s.queue != nil {
		metaBytes, _ := json.Marshal(cfg)
		job := &jobs.Job{
			ID:          fmt.Sprintf("job-usb-%d", time.Now().UnixNano()),
			Type:        jobs.TypeUSB,
			Target:      target,
			Status:      jobs.StatusPending,
			ProgressMsg: "Queued USB offline bundle export...",
			Metadata:    string(metaBytes),
		}
		if err := s.queue.Submit(r.Context(), job); err != nil {
			http.Redirect(w, r, "/settings?err="+url.QueryEscape("Failed to enqueue USB export: "+err.Error()), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/system", http.StatusSeeOther)
		return
	}

	if _, err := usb.ExportToUSB(r.Context(), s.dataDir, cfg, nil, nil); err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?msg="+url.QueryEscape("USB export completed."), http.StatusSeeOther)
}

func (s *Server) installedImageNames() []string {
	var images []string
	seen := make(map[string]bool)
	if s.db == nil {
		return images
	}
	rows, err := s.db.Query(`SELECT image FROM app_states WHERE COALESCE(image, '') != ''`)
	if err != nil {
		return images
	}
	defer rows.Close()
	for rows.Next() {
		var img string
		if rows.Scan(&img) == nil && !seen[img] {
			seen[img] = true
			images = append(images, img)
		}
	}
	return images
}

func (s *Server) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	src := strings.TrimSpace(r.FormValue("update_source"))
	if src == "" {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("Enter the update bundle directory or mounted USB path."), http.StatusSeeOther)
		return
	}

	pubKey, err := update.DefaultPublicKey()
	if err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	m, newBin, err := update.LoadUpdateFromDir(src, pubKey)
	if err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("Update rejected: "+err.Error()), http.StatusSeeOther)
		return
	}
	exePath, err := os.Executable()
	if err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	s.stopAllContainersForLifecycle(r.Context())
	if err := update.ApplyUpdate(exePath, newBin, s.dataDir, update.ArkVersion, m.Version); err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("Apply update failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	msg := fmt.Sprintf("Update v%s verified and staged with DB snapshot. Restart Ark to complete upgrade.", m.Version)
	http.Redirect(w, r, "/settings?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) handleSettingsRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	exePath, err := os.Executable()
	if err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if err := update.Rollback(exePath, s.dataDir); err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("Rollback failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?msg="+url.QueryEscape("Rolled back binary and database snapshot. Restart Ark to run the previous version."), http.StatusSeeOther)
}

func (s *Server) handleMapsViewer(w http.ResponseWriter, r *http.Request) {
	mapsDir := filepath.Join(s.dataDir, "maps")
	entries, _ := os.ReadDir(mapsDir)
	var archives []MapArchiveView
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".pmtiles") {
			continue
		}
		info, err := e.Info()
		sizeText := ""
		if err == nil {
			sizeText = formatHumanBytes(info.Size())
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		archives = append(archives, MapArchiveView{
			Name:     name,
			FileName: e.Name(),
			SizeText: sizeText,
		})
	}

	mapsRunning := false
	if s.db != nil {
		var status string
		if err := s.db.QueryRow(`SELECT status FROM app_states WHERE id = 'maps'`).Scan(&status); err == nil && status == "running" {
			mapsRunning = true
		}
	}

	data := PageData{
		Title:       "Offline Maps",
		ActiveNav:   "maps",
		MapArchives: archives,
		MapsRunning: mapsRunning,
		CountryMaps: kiwix.GetCountryMapOptions(),
		MapPins:     s.listMapPins(),
		FlashMsg:    strings.TrimSpace(r.URL.Query().Get("msg")),
		FlashErr:    strings.TrimSpace(r.URL.Query().Get("err")),
	}
	s.render(w, "maps.html", data)
}
