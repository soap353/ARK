package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ark/catalog"
	"ark/internal/ai"
	"ark/internal/apps"
	"ark/internal/docker"
	"ark/internal/fieldguide"
	"ark/internal/jobs"
	"ark/internal/kiwix"
	"ark/internal/storage"
	"ark/internal/system"
	"ark/internal/update"
)

// MapPinView represents a saved custom map bookmark/marker on /maps.
type MapPinView struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Lat       float64 `json:"lat"`
	Lng       float64 `json:"lng"`
	Category  string  `json:"category"`
	Note      string  `json:"note"`
	CreatedAt string  `json:"created_at"`
}

// ---------------------------------------------------------------------------
// Dedicated Kiwix Library Catalog Page (/kiwix) & Library Disk Manager (/library)
// ---------------------------------------------------------------------------

func (s *Server) handleKiwixCatalogPage(w http.ResponseWriter, r *http.Request) {
	data := PageData{
		Title:        "Kiwix Library",
		ActiveNav:    "kiwix",
		CustomOPDS:   s.fetchSettings()["custom_opds_url"],
		InitialQuery: strings.TrimSpace(r.URL.Query().Get("q")),
	}
	s.render(w, "kiwix.html", data)
}

func (s *Server) handleAPILibraryDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimSpace(r.FormValue("rel_path"))
	if rel == "" || s.dataDir == "" {
		http.Redirect(w, r, "/library?err="+url.QueryEscape("Invalid file path"), http.StatusSeeOther)
		return
	}

	absData, _ := filepath.Abs(s.dataDir)
	targetPath := filepath.Clean(filepath.Join(absData, rel))
	if !strings.HasPrefix(targetPath, absData+string(filepath.Separator)) {
		http.Redirect(w, r, "/library?err="+url.QueryEscape("Path outside ArkData denied"), http.StatusSeeOther)
		return
	}

	_ = os.Remove(targetPath)
	if strings.HasSuffix(strings.ToLower(targetPath), ".zim") {
		lm := kiwix.NewLibraryManager(filepath.Join(s.dataDir, "zim", "library.xml"), "")
		_ = lm.RemoveZIM(r.Context(), targetPath)
		_ = lm.RemoveZIM(r.Context(), filepath.Base(targetPath))
	}
	if s.db != nil {
		_, _ = s.db.Exec(`DELETE FROM catalog_items WHERE file_path = ? OR file_path = ?`, targetPath, filepath.Base(targetPath))
	}
	http.Redirect(w, r, "/library?msg="+url.QueryEscape("Removed "+filepath.Base(targetPath)), http.StatusSeeOther)
}

func (s *Server) handleAPILibraryUpdates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	zimDir := filepath.Join(s.dataDir, "zim")
	entries, _ := os.ReadDir(zimDir)

	type zimUpdateStatus struct {
		FileName    string `json:"file_name"`
		Title       string `json:"title"`
		CurrentDate string `json:"current_date"`
		LatestDate  string `json:"latest_date"`
		LatestURL   string `json:"latest_url,omitempty"`
		HasUpdate   bool   `json:"has_update"`
	}

	opdsBase := kiwix.DefaultCatalogURL
	if override := strings.TrimSpace(os.Getenv("ARK_KIWIX_OPDS_URL")); override != "" {
		opdsBase = override
	}
	client := &http.Client{Timeout: 8 * time.Second}
	var results []zimUpdateStatus

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".zim") {
			continue
		}
		name := e.Name()
		baseNoExt := strings.TrimSuffix(name, ".zim")
		// Typical Kiwix filename ends with _YYYY-MM
		prefix := baseNoExt
		curDate := ""
		if idx := strings.LastIndex(baseNoExt, "_"); idx > 0 && len(baseNoExt)-idx-1 >= 7 {
			maybeDate := baseNoExt[idx+1:]
			if len(maybeDate) == 7 && maybeDate[4] == '-' {
				curDate = maybeDate
				prefix = baseNoExt[:idx]
			}
		}

		status := zimUpdateStatus{
			FileName:    name,
			Title:       prefix,
			CurrentDate: curDate,
		}
		feed, err := kiwix.FetchOPDSFeed(r.Context(), client, opdsBase, kiwix.FilterParams{
			Query: prefix,
			Limit: 3,
		})
		if err == nil && len(feed.Entries) > 0 {
			for _, ent := range feed.Entries {
				dl := ent.DirectZimURL()
				if dl == "" {
					continue
				}
				status.Title = ent.Title
				latestBase := filepath.Base(dl)
				if !strings.EqualFold(latestBase, name) {
					status.LatestURL = dl
					status.LatestDate = strings.Split(ent.Updated, "T")[0]
					status.HasUpdate = true
				}
				break
			}
		}
		results = append(results, status)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"items": results})
}

// ---------------------------------------------------------------------------
// Field Medical Conditions & Remedies Guide (/field-guide)
// ---------------------------------------------------------------------------

func (s *Server) handleFieldGuidePage(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	condSlug := strings.TrimSpace(r.URL.Query().Get("condition"))
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))

	guide, err := fieldguide.LoadGuide()
	var flashErr string
	if err != nil {
		flashErr = "Failed to load embedded Field Guide data: " + err.Error()
		guide = &fieldguide.Guide{}
	}
	remedies := guide.Search(q, condSlug, kind)

	data := PageData{
		Title:             "Remedy",
		ActiveNav:         "field-guide",
		FlashErr:          flashErr,
		GuideConditions:   guide.Conditions,
		GuideCategories:   guide.Categories,
		GuideRemedies:     remedies,
		InitialQuery:      q,
		SelectedCondition: condSlug,
		SelectedGuideKind: kind,
	}
	s.render(w, "field_guide.html", data)
}

// ---------------------------------------------------------------------------
// GitHub Releases Update Check & Self-Update
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
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo == "" {
		repo = s.resolveGitHubRepo()
	}
	apiBase := strings.TrimSpace(os.Getenv("ARK_GITHUB_API_URL"))
	rel, err := update.CheckGitHubRelease(r.Context(), nil, apiBase, repo, update.ArkVersion)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"release": rel,
	})
}

func (s *Server) handleSettingsUpdateGitHub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	repo := strings.TrimSpace(r.FormValue("github_repo"))
	if repo == "" {
		repo = s.resolveGitHubRepo()
	} else if s.db != nil {
		_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('github_repo', ?)`, repo)
	}

	apiBase := strings.TrimSpace(os.Getenv("ARK_GITHUB_API_URL"))
	rel, err := update.CheckGitHubRelease(r.Context(), nil, apiBase, repo, update.ArkVersion)
	if err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("GitHub release check failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	if !rel.HasUpdate {
		http.Redirect(w, r, "/settings?msg="+url.QueryEscape(fmt.Sprintf("Already up to date (v%s; latest GitHub release is %s).", update.ArkVersion, rel.TagName)), http.StatusSeeOther)
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	s.stopAllContainersForLifecycle(r.Context())

	verifiedVia, err := update.DownloadAndApplyGitHubRelease(r.Context(), nil, rel, exePath, s.dataDir)
	if err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("GitHub self-update failed: "+err.Error()), http.StatusSeeOther)
		return
	}

	msg := fmt.Sprintf("Downloaded and staged Ark v%s from %s (%s) with DB snapshot. Restart Ark to launch the updated binary.", rel.Version, repo, verifiedVia)
	http.Redirect(w, r, "/settings?msg="+url.QueryEscape(msg), http.StatusSeeOther)
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

func (s *Server) handleSettingsRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	s.stopAllContainersForLifecycle(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!DOCTYPE html><html><body style='font-family:system-ui;padding:2rem;background:#F5F2EB;color:#1C1F1D'><h2>Restarting Ark...</h2><p id='st'>Stopping services and relaunching. This page will reload automatically.</p><script>(function(){var tries=0;function poll(){tries++;fetch('/healthz',{cache:'no-store'}).then(function(r){if(r.ok&&tries>4){location.href='/settings';}else{setTimeout(poll,1000);}}).catch(function(){setTimeout(poll,1000);});}setTimeout(poll,2500);})();</script></body></html>"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	// Relaunch a fresh copy of this binary; the child waits for the port to free up (ARK_RESTART=1).
	if exe, err := os.Executable(); err == nil && os.Getenv("ARK_IN_CONTAINER") != "1" {
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Env = append(os.Environ(), "ARK_RESTART=1", "ARK_NO_BROWSER=1")
		if err := cmd.Start(); err != nil {
			log.Printf("[Ark] Restart relaunch failed: %v", err)
		} else {
			_ = cmd.Process.Release()
		}
	}
	go func() {
		time.Sleep(350 * time.Millisecond)
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
		time.Sleep(4500 * time.Millisecond)
		os.Exit(0)
	}()
}

func (s *Server) handleSettingsShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
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
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
		time.Sleep(3500 * time.Millisecond)
		os.Exit(0)
	}()
}

// ---------------------------------------------------------------------------
// Custom Docker Apps + Container Update Checker (/apps)
// ---------------------------------------------------------------------------

func (s *Server) handleAPICustomAppSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	image := strings.TrimSpace(r.FormValue("image"))
	portStr := strings.TrimSpace(r.FormValue("port"))
	proxyMode := strings.TrimSpace(r.FormValue("proxy_mode"))
	envRaw := r.FormValue("env")
	volSubdir := strings.TrimSpace(r.FormValue("volume_target"))

	if name == "" || image == "" {
		http.Redirect(w, r, "/apps?err="+url.QueryEscape("App name and Docker image are required."), http.StatusSeeOther)
		return
	}
	id := apps.SanitizeCustomAppID(name)
	if id == "" {
		http.Redirect(w, r, "/apps?err="+url.QueryEscape("Name must contain at least 2 alphanumeric characters."), http.StatusSeeOther)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		http.Redirect(w, r, "/apps?err="+url.QueryEscape("Valid container port (1-65535) required."), http.StatusSeeOther)
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
		http.Redirect(w, r, "/apps?err="+url.QueryEscape("Save custom app failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	if s.db != nil {
		_, _ = s.db.Exec(`
			INSERT INTO app_states (id, name, category, image, status, proxy_mode, auto_start, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'stopped', ?, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			ON CONFLICT(id) DO UPDATE SET name = excluded.name, image = excluded.image, proxy_mode = excluded.proxy_mode, updated_at = CURRENT_TIMESTAMP
		`, m.ID, m.Name, m.Category, m.Image, m.Proxy)
	}
	http.Redirect(w, r, "/apps?msg="+url.QueryEscape("Added custom container '"+m.Name+"'. Click Start to launch."), http.StatusSeeOther)
}

func (s *Server) handleAPIAppUpdateImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	appID := strings.TrimSpace(r.FormValue("id"))
	m, err := s.loadAnyManifest(appID)
	if err != nil {
		http.Redirect(w, r, "/apps?err="+url.QueryEscape("App not found: "+appID), http.StatusSeeOther)
		return
	}
	if s.docker == nil || !s.docker.IsAvailable(r.Context()) {
		http.Redirect(w, r, "/apps?err="+url.QueryEscape("Docker daemon unavailable"), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if !strings.HasPrefix(m.Image, "ark/") {
		if err := s.docker.Pull(ctx, m.Image); err != nil {
			http.Redirect(w, r, "/apps?err="+url.QueryEscape("Pull failed for "+m.Image+": "+err.Error()), http.StatusSeeOther)
			return
		}
	}
	// Recreate container cleanly from newly pulled image
	_ = s.docker.Remove(ctx, appID, true)
	if err := s.startAppContainer(ctx, appID); err != nil {
		http.Redirect(w, r, "/apps?err="+url.QueryEscape("Restart after pull failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/apps?msg="+url.QueryEscape("Updated "+m.Name+" to latest "+m.Image), http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Ollama Model Manager (/ask)
// ---------------------------------------------------------------------------

func (s *Server) handleAPIAIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	settings := s.fetchSettings()
	models, err := ai.ListOllamaModels(r.Context(), settings["ai_server_url"])
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     false,
			"error":  err.Error(),
			"models": []any{},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":          true,
		"models":      models,
		"chat_model":  settings["ai_chat_model"],
		"embed_model": settings["ai_embed_model"],
	})
}

func (s *Server) handleAPIAIModelPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		http.Redirect(w, r, "/ask?err="+url.QueryEscape("Enter a model name (e.g. llama3.2:3b)"), http.StatusSeeOther)
		return
	}
	settings := s.fetchSettings()
	serverURL := settings["ai_server_url"]

	if s.queue != nil {
		metaBytes, _ := json.Marshal(jobs.JobMetadata{FileName: serverURL})
		err := s.queue.Submit(r.Context(), &jobs.Job{
			ID:          fmt.Sprintf("job-ollama-%d", time.Now().UnixNano()),
			Type:        jobs.TypeOllamaPull,
			Target:      model,
			Status:      jobs.StatusPending,
			Progress:    0,
			ProgressMsg: "Queued Ollama pull for " + model,
			Metadata:    string(metaBytes),
		})
		if err != nil {
			http.Redirect(w, r, "/ask?err="+url.QueryEscape("Failed to queue pull: "+err.Error()), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/system", http.StatusSeeOther)
		return
	}
	if err := ai.PullOllamaModel(r.Context(), serverURL, model, nil); err != nil {
		http.Redirect(w, r, "/ask?err="+url.QueryEscape("Pull failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ask?msg="+url.QueryEscape("Pulled model "+model), http.StatusSeeOther)
}

func (s *Server) handleAPIAIModelDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	settings := s.fetchSettings()
	if err := ai.DeleteOllamaModel(r.Context(), settings["ai_server_url"], model); err != nil {
		http.Redirect(w, r, "/ask?err="+url.QueryEscape("Delete model failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ask?msg="+url.QueryEscape("Deleted model "+model), http.StatusSeeOther)
}

func (s *Server) handleAPIAIModelSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	role := strings.TrimSpace(r.FormValue("role")) // "chat" or "embed"
	key := "ai_chat_model"
	if role == "embed" {
		key = "ai_embed_model"
	}
	if s.db != nil && model != "" {
		_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, key, model)
	}
	http.Redirect(w, r, "/ask?msg="+url.QueryEscape("Selected "+model+" for "+role), http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// System Time & Container Runtime Setup (/system)
// ---------------------------------------------------------------------------

var onlineTimeProbeURL = "https://www.google.com/generate_204"

func (s *Server) handleAPISystemTime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now()
	source := "device"
	if onlineTimeProbeURL != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
		defer cancel()
		if req, err := http.NewRequestWithContext(ctx, http.MethodHead, onlineTimeProbeURL, nil); err == nil {
			client := &http.Client{Timeout: 1500 * time.Millisecond}
			if resp, err := client.Do(req); err == nil {
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"unix_ms": now.UnixMilli(),
		"iso":     now.Format(time.RFC3339),
		"source":  source,
	})
}

func (s *Server) handleAPISystemInstallRuntime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	msg, err := docker.InstallPodmanRuntime(r.Context(), s.dataDir)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if cli, cerr := docker.NewClient(); cerr == nil {
		_ = cli.EnsureDaemonRunning(r.Context())
		s.SetDockerClient(cli)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": msg, "runtime": cli.RuntimeName()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": msg, "runtime": "Podman"})
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
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (s *Server) handleAPISystemRuntime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	online, label, canInstall := s.runtimeStatus(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"online":      online,
		"label":       label,
		"can_install": canInstall,
	})
}

func (s *Server) handleAPIZIMSuggest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	stem := filepath.Base(strings.TrimSpace(r.URL.Query().Get("stem")))
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if s.dataDir == "" || stem == "" || q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"hits": []any{}})
		return
	}
	zimPath := filepath.Join(s.dataDir, "zim", stem+".zim")
	arc, err := getZIMArchive(zimPath, stem)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"hits": []any{}})
		return
	}
	recs := arc.searchTitles(q, 10)
	type suggestHit struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	hits := make([]suggestHit, 0, len(recs))
	escStem := url.PathEscape(stem)
	for _, rec := range recs {
		hits = append(hits, suggestHit{
			Title: rec.title,
			URL:   fmt.Sprintf("/read/zim/%s/%c/%s", escStem, rec.ns, rec.url),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
}

func (s *Server) handleAPIZIMPages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	stem := filepath.Base(strings.TrimSpace(r.URL.Query().Get("stem")))
	if s.dataDir == "" || stem == "" {
		writeJSON(w, http.StatusOK, map[string]any{"pages": []any{}, "total": 0, "next_offset": 0, "has_more": false})
		return
	}
	offset := 0
	if rawOff := strings.TrimSpace(r.URL.Query().Get("offset")); rawOff != "" {
		if n, err := strconv.Atoi(rawOff); err == nil && n > 0 {
			offset = n
		}
	}
	limit := 200
	if rawLim := strings.TrimSpace(r.URL.Query().Get("limit")); rawLim != "" {
		if n, err := strconv.Atoi(rawLim); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	filter := strings.TrimSpace(r.URL.Query().Get("q"))
	zimPath := filepath.Join(s.dataDir, "zim", stem+".zim")
	arc, err := getZIMArchive(zimPath, stem)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"pages": []any{}, "total": 0, "next_offset": 0, "has_more": false})
		return
	}
	recs, total, hasMore := arc.listPages(offset, limit, filter)
	type pageHit struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	pages := make([]pageHit, 0, len(recs))
	escStem := url.PathEscape(stem)
	for _, rec := range recs {
		pages = append(pages, pageHit{
			Title: rec.title,
			URL:   fmt.Sprintf("/read/zim/%s/%c/%s", escStem, rec.ns, strings.TrimPrefix(rec.url, "/")),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pages":       pages,
		"total":       total,
		"next_offset": offset + len(pages),
		"has_more":    hasMore,
	})
}

// ---------------------------------------------------------------------------
// Maps Country Extract & Custom Offline Map Pins (/maps)
// ---------------------------------------------------------------------------

func (s *Server) ensureMapMarkersTable() {
	if s.db == nil {
		return
	}
	_, _ = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS map_markers (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			lat REAL NOT NULL,
			lng REAL NOT NULL,
			category TEXT NOT NULL DEFAULT 'waypoint',
			note TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`)
}

func (s *Server) listMapPins() []MapPinView {
	if s.db == nil {
		return nil
	}
	s.ensureMapMarkersTable()
	rows, err := s.db.Query(`SELECT id, title, lat, lng, category, note, COALESCE(created_at, '') FROM map_markers ORDER BY rowid DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var pins []MapPinView
	for rows.Next() {
		var p MapPinView
		if err := rows.Scan(&p.ID, &p.Title, &p.Lat, &p.Lng, &p.Category, &p.Note, &p.CreatedAt); err == nil {
			pins = append(pins, p)
		}
	}
	return pins
}

func (s *Server) handleAPIMapsExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	wantsJSON := strings.Contains(r.Header.Get("Accept"), "application/json") || r.FormValue("ajax") == "1"
	slug := strings.TrimSpace(r.FormValue("slug"))
	opt := kiwix.FindCountryMapBySlug(slug)
	if opt == nil {
		if wantsJSON {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Unknown country/map slug"})
			return
		}
		http.Redirect(w, r, "/maps?err="+url.QueryEscape("Unknown country/map slug"), http.StatusSeeOther)
		return
	}
	if spaceRes, ok := s.checkDownloadSpace(r.Context(), "map_extract", opt.Slug, 0); ok && spaceRes.Status == "insufficient" {
		if wantsJSON {
			writeJSON(w, http.StatusInsufficientStorage, spaceRes)
			return
		}
		http.Redirect(w, r, "/maps?err="+url.QueryEscape(spaceRes.Message), http.StatusSeeOther)
		return
	}
	jobID := fmt.Sprintf("job-map-%d", time.Now().UnixNano())
	metaBytes, _ := json.Marshal(jobs.JobMetadata{
		OutputDir: filepath.Join(s.dataDir, "maps"),
	})
	if s.queue != nil {
		_ = s.queue.Submit(r.Context(), &jobs.Job{
			ID:          jobID,
			Type:        jobs.TypeMapExtract,
			Target:      opt.Slug,
			Status:      jobs.StatusPending,
			ProgressMsg: "Queued extract for " + opt.Name,
			Metadata:    string(metaBytes),
		})
	} else if s.db != nil {
		_, _ = s.db.Exec(`INSERT INTO jobs (id, type, target, status, progress_msg, metadata) VALUES (?, ?, ?, 'pending', ?, ?)`,
			jobID, jobs.TypeMapExtract, opt.Slug, "Queued extract for "+opt.Name, string(metaBytes))
	}
	if wantsJSON {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"job_id": jobID,
			"target": opt.Slug,
			"status": "pending",
		})
		return
	}
	http.Redirect(w, r, "/system", http.StatusSeeOther)
}

func (s *Server) handleAPIMapPins(w http.ResponseWriter, r *http.Request) {
	s.ensureMapMarkersTable()
	wantsJSON := strings.Contains(r.Header.Get("Accept"), "application/json") || r.FormValue("format") == "json"
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"pins": s.listMapPins()})
	case http.MethodPost:
		action := strings.TrimSpace(r.FormValue("action"))
		if action == "delete" {
			id := strings.TrimSpace(r.FormValue("id"))
			if s.db != nil && id != "" {
				_, _ = s.db.Exec(`DELETE FROM map_markers WHERE id = ?`, id)
			}
			if wantsJSON {
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pins": s.listMapPins()})
				return
			}
			http.Redirect(w, r, "/maps?msg="+url.QueryEscape("Removed map pin"), http.StatusSeeOther)
			return
		}
		title := strings.TrimSpace(r.FormValue("title"))
		lat, err1 := strconv.ParseFloat(strings.TrimSpace(r.FormValue("lat")), 64)
		lng, err2 := strconv.ParseFloat(strings.TrimSpace(r.FormValue("lng")), 64)
		if title == "" || err1 != nil || err2 != nil {
			if wantsJSON {
				writeJSONError(w, http.StatusBadRequest, "Valid title, latitude, and longitude are required.")
				return
			}
			http.Redirect(w, r, "/maps?err="+url.QueryEscape("Valid title, latitude, and longitude are required."), http.StatusSeeOther)
			return
		}
		cat := strings.TrimSpace(r.FormValue("category"))
		if cat == "" {
			cat = "waypoint"
		}
		note := strings.TrimSpace(r.FormValue("note"))
		if s.db != nil {
			_, _ = s.db.Exec(`INSERT INTO map_markers (id, title, lat, lng, category, note) VALUES (?, ?, ?, ?, ?, ?)`,
				fmt.Sprintf("pin-%d", time.Now().UnixNano()), title, lat, lng, cat, note)
		}
		if wantsJSON {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pins": s.listMapPins()})
			return
		}
		http.Redirect(w, r, "/maps?msg="+url.QueryEscape("Saved offline map pin: "+title), http.StatusSeeOther)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// Manifest & Disk Library Helpers shared with web.go
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

func (s *Server) scanDiskLibraryItems(queryFilter, catFilter string) []LibraryItemView {
	q := strings.ToLower(strings.TrimSpace(queryFilter))
	cat := strings.ToLower(strings.TrimSpace(catFilter))

	// Load ZIM metadata index from library.xml when present
	zimMetaByBase := make(map[string]kiwix.LibraryBook)
	lm := kiwix.NewLibraryManager(filepath.Join(s.dataDir, "zim", "library.xml"), "")
	if books, err := lm.ListBooks(); err == nil {
		for _, b := range books {
			zimMetaByBase[strings.ToLower(filepath.Base(b.Path))] = b
		}
	}

	var items []LibraryItemView
	seenRel := make(map[string]bool)

	scanDir := func(subDir, category, openPrefix string) {
		fullDir := filepath.Join(s.dataDir, filepath.FromSlash(subDir))
		entries, err := os.ReadDir(fullDir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			low := strings.ToLower(name)
			if strings.HasSuffix(low, ".part") || low == "library.xml" {
				continue
			}
			if subDir == "zim" && !strings.HasSuffix(low, ".zim") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}

			title := name
			desc := ""
			stem := ""
			if subDir == "zim" {
				stem = strings.TrimSuffix(name, filepath.Ext(name))
				if meta, ok := zimMetaByBase[low]; ok && meta.Title != "" {
					title = meta.Title
					desc = meta.Description
				} else if arc, aerr := getZIMArchive(filepath.Join(fullDir, name), stem); aerr == nil {
					if mTitle, mDesc := arc.metadata(); mTitle != "" {
						title = mTitle
						desc = mDesc
					} else {
						title = strings.ReplaceAll(stem, "_", " ")
					}
				} else {
					title = strings.ReplaceAll(stem, "_", " ")
				}
			}

			relPath := filepath.ToSlash(filepath.Join(subDir, name))
			seenRel[strings.ToLower(relPath)] = true

			if cat != "" && cat != "all" && !strings.EqualFold(category, catFilter) {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(title+" "+desc+" "+name+" "+category), q) {
				continue
			}

			openURL := openPrefix + url.PathEscape(name)
			if subDir == "zim" {
				openURL = "/read/zim/" + url.PathEscape(stem) + "/"
			}

			items = append(items, LibraryItemView{
				Title:       title,
				Description: desc,
				Stem:        stem,
				FileName:    name,
				RelPath:     relPath,
				Category:    category,
				Size:        formatHumanBytes(info.Size()),
				Date:        info.ModTime().Format("2006-01-02"),
				OpenURL:     openURL,
				IconURL:     "/api/library/icon?file=" + url.QueryEscape(name) + "&cat=" + url.QueryEscape(category),
			})
		}
	}

	scanDir("zim", "ZIM", "/a/kiwix/")
	scanDir("media/docs", "Documents", "/media/docs/")
	scanDir("media/video", "Video", "/media/video/")
	scanDir("media/music", "Music", "/media/music/")

	// Also surface catalog_items marked downloaded in SQLite that aren't already matched on disk
	if s.db != nil {
		rows, err := s.db.Query(`SELECT title, category, size_bytes, date, file_path FROM catalog_items WHERE is_downloaded = 1`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var title, category, date, fp string
				var sizeBytes int64
				if err := rows.Scan(&title, &category, &sizeBytes, &date, &fp); err == nil {
					rel := "zim/" + filepath.Base(fp)
					if seenRel[strings.ToLower(rel)] {
						continue
					}
					if cat != "" && cat != "all" && !strings.EqualFold(category, catFilter) {
						continue
					}
					if q != "" && !strings.Contains(strings.ToLower(title+" "+category), q) {
						continue
					}
					baseName := filepath.Base(fp)
					if baseName == "" || baseName == "." {
						baseName = title
					}
					stem := strings.TrimSuffix(baseName, filepath.Ext(baseName))
					items = append(items, LibraryItemView{
						Title:    title,
						FileName: baseName,
						Category: category,
						Size:     formatHumanBytes(sizeBytes),
						Date:     date,
						OpenURL:  "/read/zim/" + url.PathEscape(stem) + "/",
						IconURL:  "/api/library/icon?file=" + url.QueryEscape(baseName) + "&cat=" + url.QueryEscape(category),
					})
				}
			}
		}
	}

	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Title) < strings.ToLower(items[j].Title)
	})
	return items
}

func (s *Server) handleAPILibraryIcon(w http.ResponseWriter, r *http.Request) {
	fileName := strings.TrimSpace(r.URL.Query().Get("file"))
	category := strings.TrimSpace(r.URL.Query().Get("cat"))
	low := strings.ToLower(fileName)

	// 1. Check local disk icon cache (<dataDir>/zim/.icons/<stem>.png) or extract embedded ZIM cover directly
	if s.dataDir != "" && fileName != "" {
		iconDir := filepath.Join(s.dataDir, "zim", ".icons")
		stem := strings.TrimSuffix(filepath.Base(low), filepath.Ext(low))
		cachedPath := filepath.Join(iconDir, stem+".png")
		if raw, err := os.ReadFile(cachedPath); err == nil && len(raw) > 0 {
			w.Header().Set("Content-Type", http.DetectContentType(raw))
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(raw)
			return
		}
		if strings.HasSuffix(low, ".zim") {
			zimFile := filepath.Join(s.dataDir, "zim", filepath.Base(fileName))
			if arc, aerr := getZIMArchive(zimFile, stem); aerr == nil {
				if raw, mt := arc.embeddedIcon(); len(raw) > 0 {
					_ = os.MkdirAll(iconDir, 0755)
					_ = os.WriteFile(cachedPath, raw, 0644)
					w.Header().Set("Content-Type", mt)
					w.Header().Set("Cache-Control", "public, max-age=86400")
					_, _ = w.Write(raw)
					return
				}
			}
		}
		// Opportunistically fetch ZIM illustration thumbnail from OPDS once if online (unless disabled in tests)
		missMarker := filepath.Join(iconDir, stem+".miss")
		if strings.HasSuffix(low, ".zim") && os.Getenv("ARK_OFFLINE_ICONS") != "1" {
			if _, missErr := os.Stat(missMarker); os.IsNotExist(missErr) {
				queryStem := stem
				if idx := strings.LastIndex(stem, "_"); idx > 0 && len(stem)-idx-1 == 7 && stem[idx+5] == '-' {
					queryStem = stem[:idx]
				}
				opdsURL := kiwix.DefaultCatalogURL
				if override := strings.TrimSpace(os.Getenv("ARK_KIWIX_OPDS_URL")); override != "" {
					opdsURL = override
				}
				ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
				feed, err := kiwix.FetchOPDSFeed(ctx, &http.Client{Timeout: 1500 * time.Millisecond}, opdsURL, kiwix.FilterParams{
					Query: queryStem,
					Limit: 1,
				})
				cancel()
				if err == nil && len(feed.Entries) > 0 {
					if thumb := feed.Entries[0].ThumbnailURL(); thumb != "" {
						tCtx, tCancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
						tReq, _ := http.NewRequestWithContext(tCtx, http.MethodGet, thumb, nil)
						if tReq != nil {
							tResp, tErr := (&http.Client{Timeout: 1500 * time.Millisecond}).Do(tReq)
							if tErr == nil {
								imgBytes, _ := io.ReadAll(io.LimitReader(tResp.Body, 512*1024))
								tResp.Body.Close()
								if tResp.StatusCode == http.StatusOK && len(imgBytes) > 0 {
									_ = os.MkdirAll(iconDir, 0755)
									_ = os.WriteFile(cachedPath, imgBytes, 0644)
									tCancel()
									w.Header().Set("Content-Type", http.DetectContentType(imgBytes))
									w.Header().Set("Cache-Control", "public, max-age=86400")
									_, _ = w.Write(imgBytes)
									return
								}
							}
						}
						tCancel()
					}
				}
				_ = os.MkdirAll(iconDir, 0755)
				_ = os.WriteFile(missMarker, []byte("1"), 0644)
			}
		}
	}

	// 2. Crisp offline origin brand SVG fallback
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(originBrandSVG(low, category)))
}

func originBrandSVG(lowName, category string) string {
	switch {
	case strings.Contains(lowName, "wikimed") || strings.Contains(lowName, "medicine") || strings.Contains(lowName, "nhs") || strings.Contains(lowName, "cdc") || strings.Contains(lowName, "medline") || strings.Contains(lowName, "pathology"):
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="10" fill="#FCEAEA" stroke="#B91C1C" stroke-width="1.5"/><path d="M20 12h8v8h8v8h-8v8h-8v-8h-8v-8h8z" fill="#B91C1C"/></svg>`
	case strings.Contains(lowName, "wikipedia") || strings.Contains(lowName, "wiktionary") || strings.Contains(lowName, "wikibooks") || strings.Contains(lowName, "wikiversity") || strings.Contains(lowName, "wikivoyage"):
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="10" fill="#F5F2EB" stroke="#1C1F1D" stroke-width="1.5"/><text x="24" y="32" text-anchor="middle" font-family="Georgia,serif" font-weight="700" font-size="25" fill="#1C1F1D">W</text></svg>`
	case strings.Contains(lowName, "ifixit"):
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="10" fill="#E8F1FA" stroke="#0369A1" stroke-width="1.5"/><path d="M30 13a7 7 0 0 0-8.5 8.5L13 30l4.5 4.5 8.5-8.5A7 7 0 0 0 34.5 17l-4 4-3.5-3.5 3-4.5z" fill="#0369A1"/></svg>`
	case strings.Contains(lowName, "gutenberg"):
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="10" fill="#FBECE3" stroke="#C25E28" stroke-width="1.5"/><text x="24" y="30" text-anchor="middle" font-family="Georgia,serif" font-weight="700" font-size="18" fill="#C25E28">PG</text></svg>`
	case strings.Contains(lowName, "stackexchange") || strings.Contains(lowName, "stackoverflow"):
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="10" fill="#EAF2FB" stroke="#1E40AF" stroke-width="1.5"/><rect x="14" y="13" width="20" height="4" rx="1" fill="#1E40AF"/><rect x="14" y="20" width="20" height="4" rx="1" fill="#2563EB"/><rect x="14" y="27" width="20" height="4" rx="1" fill="#3B82F6"/><path d="M18 33h12l-6 6z" fill="#3B82F6"/></svg>`
	case strings.Contains(lowName, "ted_") || strings.Contains(lowName, "ted-"):
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="10" fill="#E62B1E"/><text x="24" y="30" text-anchor="middle" font-family="system-ui,sans-serif" font-weight="800" font-size="16" fill="#FFFFFF">TED</text></svg>`
	case strings.Contains(lowName, "devdocs") || strings.Contains(lowName, "freecodecamp") || strings.Contains(lowName, "python"):
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="10" fill="#1B2E26"/><text x="24" y="30" text-anchor="middle" font-family="monospace" font-weight="700" font-size="17" fill="#F5F2EB">&lt;/&gt;</text></svg>`
	case strings.EqualFold(category, "Video"):
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="10" fill="#ECE7DC" stroke="#525B54" stroke-width="1.5"/><polygon points="19,15 34,24 19,33" fill="#C25E28"/></svg>`
	case strings.EqualFold(category, "Music"):
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="10" fill="#E8F3EC" stroke="#2E6F40" stroke-width="1.5"/><path d="M20 33V16l14-3v17" fill="none" stroke="#2E6F40" stroke-width="2.5"/><circle cx="16.5" cy="33" r="3.5" fill="#2E6F40"/><circle cx="30.5" cy="30" r="3.5" fill="#2E6F40"/></svg>`
	case strings.EqualFold(category, "Documents"):
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="10" fill="#FDFBF7" stroke="#525B54" stroke-width="1.5"/><path d="M16 11h11l7 7v19H16z" fill="none" stroke="#1C1F1D" stroke-width="2"/><path d="M20 24h8M20 29h8" stroke="#C25E28" stroke-width="2"/></svg>`
	default:
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="10" fill="#F5F2EB" stroke="#1B2E26" stroke-width="1.5"/><path d="M13 15.5A3.5 3.5 0 0 1 16.5 12H35v24H16.5A3.5 3.5 0 0 1 13 32.5z" fill="none" stroke="#1B2E26" stroke-width="2"/><path d="M13 31h22" stroke="#C25E28" stroke-width="2"/></svg>`
	}
}
