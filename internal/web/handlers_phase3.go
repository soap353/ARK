package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
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
	Name       string     `json:"name"`
	Title      string     `json:"title"`
	FileName   string     `json:"file_name"`
	FileURL    string     `json:"file_url"`
	SizeText   string     `json:"size_text"`
	TileType   string     `json:"tile_type"`
	MinZoom    int        `json:"min_zoom"`
	MaxZoom    int        `json:"max_zoom"`
	Bounds     [4]float64 `json:"bounds"`
	Center     [3]float64 `json:"center"`
	HasGeoMeta bool       `json:"has_geo_meta"`
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
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectWithQuery(w, r, "/settings", "err", "Invalid form submission")
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
			redirectWithQuery(w, r, "/settings", "err", err.Error())
			return
		}
	}
	redirectWithQuery(w, r, "/settings", "msg", "USB export profile saved.")
}

func (s *Server) handleSettingsUSBExport(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectWithQuery(w, r, "/settings", "err", "Invalid form submission")
		return
	}

	target := strings.TrimSpace(r.FormValue("target_path"))
	custom := strings.TrimSpace(r.FormValue("custom_path"))
	if target == "custom" || target == "" {
		target = custom
	}
	if target == "" {
		redirectWithQuery(w, r, "/settings", "err", "Select a target drive or enter a custom destination path.")
		return
	}

	categories := r.Form["categories"]
	includeDocker := r.FormValue("include_docker") != ""
	includeConfig := r.FormValue("include_config") != ""
	if len(categories) == 0 && !includeDocker && !includeConfig {
		redirectWithQuery(w, r, "/settings", "err", "Select at least one content category, Docker images, or settings to export.")
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
			redirectWithQuery(w, r, "/settings", "err", "Failed to enqueue USB export: "+err.Error())
			return
		}
		http.Redirect(w, r, "/system", http.StatusSeeOther)
		return
	}

	if _, err := usb.ExportToUSB(r.Context(), s.dataDir, cfg, nil, nil); err != nil {
		redirectWithQuery(w, r, "/settings", "err", err.Error())
		return
	}
	redirectWithQuery(w, r, "/settings", "msg", "USB export completed.")
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
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	src := strings.TrimSpace(r.FormValue("update_source"))
	if src == "" {
		redirectWithQuery(w, r, "/settings", "err", "Enter the update bundle directory or mounted USB path.")
		return
	}

	pubKey, err := update.DefaultPublicKey()
	if err != nil {
		redirectWithQuery(w, r, "/settings", "err", err.Error())
		return
	}
	m, newBin, err := update.LoadUpdateFromDir(src, pubKey)
	if err != nil {
		redirectWithQuery(w, r, "/settings", "err", "Update rejected: "+err.Error())
		return
	}
	exePath, err := os.Executable()
	if err != nil {
		redirectWithQuery(w, r, "/settings", "err", err.Error())
		return
	}
	s.stopAllContainersForLifecycle(r.Context())
	if err := update.ApplyUpdate(exePath, newBin, s.dataDir, update.ArkVersion, m.Version); err != nil {
		redirectWithQuery(w, r, "/settings", "err", "Apply update failed: "+err.Error())
		return
	}
	msg := fmt.Sprintf("Update v%s verified and staged with DB snapshot. Restart Ark to complete upgrade.", m.Version)
	redirectWithQuery(w, r, "/settings", "msg", msg)
}

func (s *Server) handleSettingsRollback(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	exePath, err := os.Executable()
	if err != nil {
		redirectWithQuery(w, r, "/settings", "err", err.Error())
		return
	}
	if err := update.Rollback(exePath, s.dataDir); err != nil {
		redirectWithQuery(w, r, "/settings", "err", "Rollback failed: "+err.Error())
		return
	}
	redirectWithQuery(w, r, "/settings", "msg", "Rolled back binary and database snapshot. Restart Ark to run the previous version.")
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
		item := MapArchiveView{
			Name:     name,
			Title:    formatMapArchiveTitle(name),
			FileName: e.Name(),
			FileURL:  "/maps/files/" + e.Name(),
			SizeText: sizeText,
			TileType: "mvt",
			MinZoom:  0,
			MaxZoom:  15,
		}
		if meta, err := ParsePMTilesMeta(filepath.Join(mapsDir, e.Name())); err == nil {
			item.MinZoom = meta.MinZoom
			item.MaxZoom = meta.MaxZoom
			item.TileType = meta.TileType
			item.Bounds = meta.Bounds
			item.Center = meta.Center
			item.HasGeoMeta = meta.Bounds != [4]float64{}
		}
		archives = append(archives, item)
	}

	mapsRunning := false
	if s.db != nil {
		var status string
		if err := s.db.QueryRow(`SELECT status FROM app_states WHERE id = 'maps'`).Scan(&status); err == nil && status == "running" {
			mapsRunning = true
		}
	}

	selectedMap := strings.TrimSpace(r.URL.Query().Get("place"))
	if selectedMap == "" {
		selectedMap = strings.TrimSpace(r.URL.Query().Get("map"))
	}
	selectedMap = strings.TrimSuffix(selectedMap, ".pmtiles")
	if selectedMap == "" && len(archives) > 0 {
		selectedMap = archives[0].Name
	}
	pins := s.listMapPins()
	if archives == nil {
		archives = []MapArchiveView{}
	}
	if pins == nil {
		pins = []MapPinView{}
	}
	archivesJSON, _ := json.Marshal(archives)
	pinsJSON, _ := json.Marshal(pins)

	data := PageData{
		Title:           "Maps",
		ActiveNav:       "maps",
		MapArchives:     archives,
		MapArchivesJSON: template.JS(archivesJSON),
		SelectedMap:     selectedMap,
		MapsRunning:     mapsRunning,
		CountryMaps:     kiwix.GetCountryMapOptions(),
		MapPins:         pins,
		MapPinsJSON:     template.JS(pinsJSON),
		FlashMsg:        strings.TrimSpace(r.URL.Query().Get("msg")),
		FlashErr:        strings.TrimSpace(r.URL.Query().Get("err")),
	}
	s.render(w, "maps.html", data)
}
