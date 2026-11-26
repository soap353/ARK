// Administration, settings, Docker lifecycle/runtime management, USB export,
// signed binary/GitHub self-updates, and server restart/shutdown handlers.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"ark/catalog"
	"ark/internal/apps"
	"ark/internal/docker"
	"ark/internal/jobs"
	"ark/internal/kiwix"
	"ark/internal/storage"
	"ark/internal/system"
	"ark/internal/update"
	"ark/internal/usb"
)

var (
	onlineTimeProbeURL   = "https://www.google.com/generate_204"
	sharedTimeProbeClient = &http.Client{Timeout: 1500 * time.Millisecond}
)

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

// ---------------------------------------------------------------------------
// Settings Persistence & Form Handlers
// ---------------------------------------------------------------------------

func (s *Server) fetchSettings() map[string]string {
	out := map[string]string{
		"device_name":         "Ark",
		"port":                "8080",
		"ui_theme":            "almanac",
		"lan_enabled":         "true",
		"lan_drop_enabled":    "true",
		"opds_enabled":        "true",
		"max_concurrent_jobs": "3",
		"video_quality":       "balanced",
	}
	if s.db == nil {
		return out
	}
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			out[k] = v
		}
	}
	return out
}

func isSettingMapEnabled(cfg map[string]string, key string, defaultVal bool) bool {
	raw := strings.ToLower(strings.TrimSpace(cfg[key]))
	if raw == "" {
		return defaultVal
	}
	return raw != "false" && raw != "0" && raw != "off" && raw != "no"
}

func (s *Server) isSettingEnabled(key string, defaultVal bool) bool {
	return isSettingMapEnabled(s.fetchSettings(), key, defaultVal)
}

func (s *Server) saveSettings(pairs map[string]string) error {
	if s.db == nil {
		return nil
	}
	for k, v := range pairs {
		if _, err := s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return nil
}

func parseCheckboxField(r *http.Request, name string) string {
	v := strings.ToLower(strings.TrimSpace(r.FormValue(name)))
	if v == "on" || v == "true" || v == "1" {
		return "true"
	}
	return "false"
}

func (s *Server) handleSettingsGeneral(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	prev := s.fetchSettings()
	newPort := strings.TrimSpace(r.FormValue("port"))
	newHTTPS := parseCheckboxField(r, "https_enabled")
	prevHTTPS := "false"
	if isSettingMapEnabled(prev, "https_enabled", false) {
		prevHTTPS = "true"
	}
	restartNeeded := (newPort != "" && newPort != strings.TrimSpace(prev["port"])) || newHTTPS != prevHTTPS

	passcodeEnabled := parseCheckboxField(r, "lan_passcode_enabled")
	pin := strings.TrimSpace(r.FormValue("lan_passcode"))
	missingPasscode := false
	if passcodeEnabled == "true" && pin == "" && prev["lan_passcode_hash"] == "" {
		passcodeEnabled = "false"
		missingPasscode = true
	}

	pairs := map[string]string{
		"device_name":          strings.TrimSpace(r.FormValue("device_name")),
		"port":                 newPort,
		"lan_enabled":          parseCheckboxField(r, "lan_enabled"),
		"lan_passcode_enabled": passcodeEnabled,
		"https_enabled":        newHTTPS,
		"lan_drop_enabled":     parseCheckboxField(r, "lan_drop_enabled"),
		"opds_enabled":         parseCheckboxField(r, "opds_enabled"),
		"peer_discoverable":    parseCheckboxField(r, "peer_discoverable"),
		"peer_allow_downloads": parseCheckboxField(r, "peer_allow_downloads"),
		"peer_manual_hosts":    strings.TrimSpace(r.FormValue("peer_manual_hosts")),
	}
	if theme := strings.TrimSpace(r.FormValue("ui_theme")); theme != "" {
		pairs["ui_theme"] = validUITheme(theme)
	}
	_ = s.saveSettings(pairs)
	if pin != "" {
		_ = s.setLANPasscode(pin)
	}
	if missingPasscode {
		redirectWithQuery(w, r, "/settings", "err", "Enter a LAN passcode before enabling LAN passcode protection.")
		return
	}
	if restartNeeded {
		http.Redirect(w, r, "/settings?saved=1&restart_required=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func (s *Server) handleSettingsAI(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	ctxVal := strings.TrimSpace(r.FormValue("ai_context_length"))
	var ctxNum int
	if ctxVal == "custom" {
		_, _ = fmt.Sscanf(strings.TrimSpace(r.FormValue("ai_context_length_custom")), "%d", &ctxNum)
	} else if ctxVal != "" {
		_, _ = fmt.Sscanf(ctxVal, "%d", &ctxNum)
	}
	if ctxNum < 0 {
		ctxNum = 0
	}
	pairs := map[string]string{
		"ai_server_url":     strings.TrimSpace(r.FormValue("ai_server_url")),
		"ai_chat_model":     strings.TrimSpace(r.FormValue("ai_chat_model")),
		"ai_embed_model":    strings.TrimSpace(r.FormValue("ai_embed_model")),
		"ai_context_length": fmt.Sprintf("%d", ctxNum),
	}
	if key := strings.TrimSpace(r.FormValue("ai_api_key")); key != "" {
		pairs["ai_api_key"] = key
	}
	_ = s.saveSettings(pairs)
	http.Redirect(w, r, "/settings?saved=1#ai", http.StatusSeeOther)
}

func (s *Server) handleSettingsDownloads(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	_ = s.saveSettings(map[string]string{
		"max_concurrent_jobs": strings.TrimSpace(r.FormValue("max_concurrent_jobs")),
		"video_quality":       strings.TrimSpace(r.FormValue("video_quality")),
	})
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
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
	if target == "custom" || target == "" {
		target = strings.TrimSpace(r.FormValue("custom_path"))
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

// ---------------------------------------------------------------------------
// Signed Offline & GitHub Self-Update + Lifecycle Controls
// ---------------------------------------------------------------------------

func (s *Server) resolveGitHubRepo() string {
	settings := s.fetchSettings()
	if r := strings.TrimSpace(settings["github_repo"]); r != "" {
		return r
	}
	if envR := strings.TrimSpace(os.Getenv("ARK_GITHUB_REPO")); envR != "" {
		return envR
	}
	return update.DefaultGitHubRepo
}

func (s *Server) handleAPIUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo == "" {
		repo = s.resolveGitHubRepo()
	}
	apiBase := strings.TrimSpace(os.Getenv("ARK_GITHUB_API_URL"))
	rel, err := update.CheckGitHubRelease(r.Context(), nil, apiBase, repo, update.ArkVersion)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "release": rel})
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
	redirectWithQuery(w, r, "/settings", "msg", fmt.Sprintf("Update v%s verified and staged with DB snapshot. Restart Ark to complete upgrade.", m.Version))
}

func (s *Server) handleSettingsUpdateGitHub(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	repo := strings.TrimSpace(r.FormValue("github_repo"))
	if repo == "" {
		repo = s.resolveGitHubRepo()
	} else {
		_ = s.saveSettings(map[string]string{"github_repo": repo})
	}

	apiBase := strings.TrimSpace(os.Getenv("ARK_GITHUB_API_URL"))
	rel, err := update.CheckGitHubRelease(r.Context(), nil, apiBase, repo, update.ArkVersion)
	if err != nil {
		redirectWithQuery(w, r, "/settings", "err", "GitHub release check failed: "+err.Error())
		return
	}
	if !rel.HasUpdate {
		redirectWithQuery(w, r, "/settings", "msg", fmt.Sprintf("Already up to date (v%s; latest GitHub release is %s).", update.ArkVersion, rel.TagName))
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		redirectWithQuery(w, r, "/settings", "err", err.Error())
		return
	}

	s.stopAllContainersForLifecycle(r.Context())

	verifiedVia, err := update.DownloadAndApplyGitHubRelease(r.Context(), nil, rel, exePath, s.dataDir)
	if err != nil {
		redirectWithQuery(w, r, "/settings", "err", "GitHub self-update failed: "+err.Error())
		return
	}

	msg := fmt.Sprintf("Downloaded and staged Ark v%s from %s (%s) with DB snapshot. Restart Ark to launch the updated binary.", rel.Version, repo, verifiedVia)
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

func (s *Server) stopAllContainersForLifecycle(ctx context.Context) {
	if s.docker != nil && s.docker.IsAvailable(ctx) {
		stopCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		_, _ = s.docker.StopAll(stopCtx, 2)
	}
	if s.db != nil {
		_, _ = s.db.Exec(`UPDATE app_states SET status = 'stopped', updated_at = CURRENT_TIMESTAMP WHERE status = 'running'`)
		_, _ = s.db.Exec(`DELETE FROM jobs WHERE status = 'done'`)
		_ = system.RecordShutdownTime(ctx, s.db, time.Now())
	}
}

func (s *Server) triggerAsyncShutdown() {
	if s.onShutdown != nil {
		s.onShutdown()
	} else if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
}

func (s *Server) handleSettingsRestart(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	s.stopAllContainersForLifecycle(r.Context())
	targetScheme := "http:"
	if s.isSettingEnabled("https_enabled", false) {
		targetScheme = "https:"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, "<!DOCTYPE html><html><body style='font-family:system-ui;padding:2rem;background:#F5F2EB;color:#1C1F1D'><h2>Restarting Ark...</h2><p id='st'>Stopping services and relaunching. This page will reload automatically.</p><p id='alt' style='display:none'></p><script>(function(){var targetScheme=%q;if(location.protocol!==targetScheme){var targetUrl=targetScheme+'//'+location.host+'/settings';var alt=document.getElementById('alt');alt.style.display='block';alt.innerHTML='Switching protocol to <a href=\"'+targetUrl+'\">'+targetUrl+'</a>...';setTimeout(function(){location.href=targetUrl;},3500);return;}var tries=0;function poll(){tries++;fetch('/healthz',{cache:'no-store'}).then(function(r){if(r.ok&&tries>4){location.href='/settings';}else{setTimeout(poll,1000);}}).catch(function(){setTimeout(poll,1000);});}setTimeout(poll,2500);})();</script></body></html>", targetScheme)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	// Relaunch a fresh copy of this binary; the child waits for the port to free up (ARK_RESTART=1).
	if exe, err := os.Executable(); err == nil && os.Getenv("ARK_IN_CONTAINER") != "1" {
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Env = append(os.Environ(), "ARK_RESTART=1", "ARK_NO_BROWSER=1")
		prepareRestartCmd(cmd)
		if err := cmd.Start(); err != nil {
			log.Printf("[Ark] Restart relaunch failed: %v", err)
		} else {
			_ = cmd.Process.Release()
		}
	}
	go func() {
		time.Sleep(350 * time.Millisecond)
		s.triggerAsyncShutdown()
		time.Sleep(4500 * time.Millisecond)
		os.Exit(0)
	}()
}

func (s *Server) handleSettingsShutdown(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	s.stopAllContainersForLifecycle(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!DOCTYPE html><html><body style='font-family:system-ui;padding:2rem;background:#F5F2EB;color:#1C1F1D'><h2>Ark Service Stopped</h2><p>All Ark containers have been stopped cleanly and the background service has shut down. You may close this tab.</p></body></html>"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		s.triggerAsyncShutdown()
		time.Sleep(3500 * time.Millisecond)
		os.Exit(0)
	}()
}

// ---------------------------------------------------------------------------
// System Telemetry Page, Runtime Setup, Time & Storage Space Checks
// ---------------------------------------------------------------------------

func (s *Server) listDownloadedFiles() []DownloadedFileView {
	if s.dataDir == "" {
		return nil
	}
	var out []DownloadedFileView
	seen := make(map[string]bool)
	addItem := func(name, mediaType, rel string) {
		rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
		low := strings.ToLower(rel)
		if rel == "" || seen[low] || strings.Contains(low, "/.icons/") || filepath.Base(low) == "library.xml" {
			return
		}
		if strings.HasSuffix(low, ".part") || strings.HasSuffix(low, ".aria2") || strings.HasSuffix(low, ".miss") {
			return
		}
		seen[low] = true
		out = append(out, DownloadedFileView{Name: name, Type: mediaType, RelPath: rel})
	}

	for _, c := range []struct {
		cat   string
		label string
	}{
		{storage.CategoryZim, "ZIM"},
		{storage.CategoryMaps, "Maps"},
		{storage.CategoryVideo, "Video"},
		{storage.CategoryMusic, "Music"},
		{storage.CategoryBooks, "Books"},
		{storage.CategoryDocs, "Docs"},
	} {
		files, err := storage.ListCategoryFiles(s.dataDir, c.cat)
		if err != nil {
			continue
		}
		for _, f := range files {
			addItem(f.Name, c.label, f.Path)
		}
	}

	if fi, err := os.Stat(filepath.Join(s.dataDir, "druglabels", "druglabels.db")); err == nil && !fi.IsDir() {
		addItem("druglabels.db", "Drug Labels", "druglabels/druglabels.db")
	}

	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].RelPath < out[j].RelPath
	})
	return out
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	var wg sync.WaitGroup
	var sv *SystemView
	wg.Add(1)
	go func() {
		defer wg.Done()
		if m, err := system.CollectMetrics(r.Context(), s.dataDir); err == nil && m != nil {
			sv = &SystemView{
				CPUPercent:  m.CPUPercent,
				RAMPercent:  m.RAMPercent,
				RAMUsedGB:   fmt.Sprintf("%.1f", float64(m.RAMUsed)/(1024*1024*1024)),
				RAMTotalGB:  fmt.Sprintf("%.1f", float64(m.RAMTotal)/(1024*1024*1024)),
				DiskPercent: m.DiskPercent,
				DiskFreeGB:  m.DiskFree / (1024 * 1024 * 1024),
				DiskTotalGB: m.DiskTotal / (1024 * 1024 * 1024),
				Hostname:    m.Hostname,
				OS:          m.OS,
				UptimeText:  fmt.Sprintf("%dh %dm", m.Uptime/3600, (m.Uptime%3600)/60),
			}
		}
	}()

	var containers []ContainerView
	if s.docker != nil && s.docker.IsAvailable(r.Context()) {
		if list, err := s.docker.ListArkContainers(r.Context()); err == nil {
			containers = make([]ContainerView, len(list))
			statsCtx, statsCancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
			defer statsCancel()
			for i, c := range list {
				containers[i] = ContainerView{
					AppID:      c.AppID,
					Name:       c.Name,
					Image:      c.Image,
					Status:     c.Status,
					CPUText:    "-",
					MemoryText: "-",
				}
				if c.Status == "running" {
					wg.Add(1)
					go func(idx int, appID string) {
						defer wg.Done()
						if st, err := s.docker.Stats(statsCtx, appID); err == nil && st != nil {
							containers[idx].CPUText = fmt.Sprintf("%.1f%%", st.CPUPercent)
							containers[idx].MemoryText = fmt.Sprintf("%.1f MB", float64(st.MemoryBytes)/(1024*1024))
						}
					}(i, c.AppID)
				}
			}
		}
	}
	wg.Wait()

	jobsList := s.fetchRecentSessionJobs(5)
	freeGB, totalGB, usedGB, pct, arkPct, otherPct, arkUsedText := s.getStorageBreakdown(r.Context())
	rtOnline, rtLabel, canInstallRt := s.runtimeStatus(r.Context())

	data := PageData{
		Title:               "System",
		ActiveNav:           "system",
		FlashMsg:            strings.TrimSpace(r.URL.Query().Get("msg")),
		FlashErr:            strings.TrimSpace(r.URL.Query().Get("err")),
		SystemMetrics:       sv,
		Containers:          containers,
		DockerOnline:        rtOnline,
		RuntimeLabel:        rtLabel,
		CanInstallRuntime:   canInstallRt,
		LastDowntime:        system.GetDowntimeInfo(r.Context(), s.db),
		StorageFreeGB:       freeGB,
		StorageTotalGB:      totalGB,
		StorageUsedGB:       usedGB,
		StorageUsedPercent:  pct,
		StorageArkPercent:   arkPct,
		StorageOtherPercent: otherPct,
		ArkDataUsedText:     arkUsedText,
		Jobs:                jobsList,
		DownloadedFiles:     s.listDownloadedFiles(),
		HasActiveJobs:       hasActive(jobsList),
	}
	s.render(w, r, "system.html", data)
}

func (s *Server) handleAPIContainerRestart(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	appID := r.FormValue("id")
	if appID != "" && s.docker != nil && s.docker.IsAvailable(r.Context()) {
		_ = s.docker.Restart(r.Context(), appID, 2)
	}
	http.Redirect(w, r, sanitizeLocalRedirect(r.FormValue("redirect")), http.StatusSeeOther)
}

func (s *Server) handleAPIContainerLogs(w http.ResponseWriter, r *http.Request) {
	appID := r.URL.Query().Get("id")
	if appID == "" || s.docker == nil || !s.docker.IsAvailable(r.Context()) {
		http.Error(w, "Docker or app unavailable", http.StatusServiceUnavailable)
		return
	}
	rc, err := s.docker.Logs(r.Context(), appID, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, io.LimitReader(rc, 256*1024))
}

func (s *Server) handleAPISystemTime(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	now := time.Now()
	source := "device"
	if onlineTimeProbeURL != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
		defer cancel()
		if req, err := http.NewRequestWithContext(ctx, http.MethodHead, onlineTimeProbeURL, nil); err == nil {
			if resp, err := sharedTimeProbeClient.Do(req); err == nil {
				_ = resp.Body.Close()
				if dateHdr := resp.Header.Get("Date"); dateHdr != "" {
					if parsed, err := http.ParseTime(dateHdr); err == nil {
						now = parsed.In(time.Local)
						source = "online"
					}
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"unix_ms": now.UnixMilli(),
		"iso":     now.Format(time.RFC3339),
		"source":  source,
	})
}

func (s *Server) handleAPISystemInstallRuntime(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	msg, err := docker.InstallPodmanRuntime(r.Context(), s.dataDir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if cli, cerr := docker.NewClient(); cerr == nil {
		_ = cli.EnsureDaemonRunning(r.Context())
		s.SetDockerClient(cli)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg, "runtime": cli.RuntimeName()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg, "runtime": "Podman"})
}

func (s *Server) handleAPISystemRuntime(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	online, label, canInstall := s.runtimeStatus(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"online":      online,
		"label":       label,
		"can_install": canInstall,
	})
}

func (s *Server) estimateDownloadBytes(ctx context.Context, rawType, target string, sizeHint int64) int64 {
	if sizeHint > 0 {
		return sizeHint
	}
	switch strings.ToLower(strings.TrimSpace(rawType)) {
	case "preset":
		var total int64
		for _, res := range kiwix.ResolvePresetResources(target) {
			if res.SizeMB > 0 {
				total += int64(res.SizeMB) * 1024 * 1024
			}
		}
		if total > 0 {
			return total
		}
		norm := strings.ToLower(strings.TrimSpace(target))
		for _, opt := range kiwix.GetWikipediaOptions() {
			if norm == strings.ToLower(opt.ID) || norm == strings.ToLower(opt.PresetTarget) {
				if parsed := storage.ParseSizeText(opt.SizeLabel); parsed > 0 {
					return parsed
				}
			}
		}
	case "map_extract":
		if opt := kiwix.FindCountryMapBySlug(target); opt != nil {
			return storage.ParseSizeText(opt.SizeEstimate)
		}
	}
	if s.db != nil && (strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://")) {
		var sz int64
		if err := s.db.QueryRow(`SELECT COALESCE(size_bytes, 0) FROM catalog_items WHERE url = ? LIMIT 1`, target).Scan(&sz); err == nil && sz > 0 {
			return sz
		}
	}
	return storage.ProbeRemoteSize(ctx, target)
}

func (s *Server) checkDownloadSpace(ctx context.Context, rawType, target string, sizeHint int64) (storage.SpaceCheckResult, bool) {
	est := s.estimateDownloadBytes(ctx, rawType, target, sizeHint)
	res, err := storage.CheckDiskSpaceForPath(ctx, s.dataDir, est)
	if err != nil {
		return storage.SpaceCheckResult{}, false
	}
	return res, true
}

func (s *Server) handleAPIStorageCheckSpace(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	rawType := strings.TrimSpace(r.URL.Query().Get("type"))
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	var sizeHint int64
	if sb := strings.TrimSpace(r.URL.Query().Get("size_bytes")); sb != "" {
		if parsed, err := strconv.ParseInt(sb, 10, 64); err == nil && parsed > 0 {
			sizeHint = parsed
		}
	}
	res, ok := s.checkDownloadSpace(r.Context(), rawType, target, sizeHint)
	if !ok {
		res = storage.EvaluateSpace(0, 0, sizeHint)
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------------------
// Custom Docker Apps & Manifest Lookup (/apps)
// ---------------------------------------------------------------------------

func (s *Server) loadAnyManifest(appID string) (*apps.AppManifest, error) {
	catDir := findCatalogDir()
	if m, err := apps.LoadManifest(filepath.Join(catDir, appID, "app.yaml")); err == nil {
		return m, nil
	}
	if m, err := apps.LoadManifestFS(catalog.FS, appID+"/app.yaml"); err == nil {
		return m, nil
	}
	return apps.LoadCustomManifest(s.dataDir, appID)
}

func (s *Server) handleAPICustomAppSave(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	image := strings.TrimSpace(r.FormValue("image"))
	portStr := strings.TrimSpace(r.FormValue("port"))
	proxyMode := strings.TrimSpace(r.FormValue("proxy_mode"))
	envRaw := r.FormValue("env")
	volSubdir := strings.TrimSpace(r.FormValue("volume_target"))

	if name == "" || image == "" {
		redirectWithQuery(w, r, "/apps", "err", "App name and Docker image are required.")
		return
	}
	id := apps.SanitizeCustomAppID(name)
	if id == "" {
		redirectWithQuery(w, r, "/apps", "err", "Name must contain at least 2 alphanumeric characters.")
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		redirectWithQuery(w, r, "/apps", "err", "Valid container port (1-65535) required.")
		return
	}
	if proxyMode != "port" {
		proxyMode = "path"
	}

	envMap := make(map[string]string)
	for _, line := range strings.Split(envRaw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			envMap[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}

	var vols []string
	if volSubdir != "" && strings.HasPrefix(volSubdir, "/") {
		vols = append(vols, fmt.Sprintf("custom/%s:%s:rw", id, volSubdir))
	}

	m := &apps.AppManifest{
		Version:  1,
		ID:       id,
		Name:     name,
		Category: "custom",
		Image:    image,
		Port:     port,
		Proxy:    proxyMode,
		Env:      envMap,
		Volumes:  vols,
	}

	if err := apps.SaveCustomManifest(s.dataDir, m); err != nil {
		redirectWithQuery(w, r, "/apps", "err", "Save custom app failed: "+err.Error())
		return
	}
	if s.db != nil {
		_, _ = s.db.Exec(`
			INSERT INTO app_states (id, name, category, image, status, proxy_mode, auto_start, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'stopped', ?, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			ON CONFLICT(id) DO UPDATE SET name = excluded.name, image = excluded.image, proxy_mode = excluded.proxy_mode, updated_at = CURRENT_TIMESTAMP
		`, m.ID, m.Name, m.Category, m.Image, m.Proxy)
	}
	redirectWithQuery(w, r, "/apps", "msg", "Added custom container '"+m.Name+"'. Click Start to launch.")
}

func (s *Server) handleAPIAppUpdateImage(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	appID := strings.TrimSpace(r.FormValue("id"))
	m, err := s.loadAnyManifest(appID)
	if err != nil {
		redirectWithQuery(w, r, "/apps", "err", "App not found: "+appID)
		return
	}
	if s.docker == nil || !s.docker.IsAvailable(r.Context()) {
		redirectWithQuery(w, r, "/apps", "err", "Docker daemon unavailable")
		return
	}
	ctx := r.Context()
	if !strings.HasPrefix(m.Image, "ark/") {
		if err := s.docker.Pull(ctx, m.Image); err != nil {
			redirectWithQuery(w, r, "/apps", "err", "Pull failed for "+m.Image+": "+err.Error())
			return
		}
	}
	_ = s.docker.Remove(ctx, appID, true)
	if err := s.startAppContainer(ctx, appID); err != nil {
		redirectWithQuery(w, r, "/apps", "err", "Restart after pull failed: "+err.Error())
		return
	}
	redirectWithQuery(w, r, "/apps", "msg", "Updated "+m.Name+" to latest "+m.Image)
}
