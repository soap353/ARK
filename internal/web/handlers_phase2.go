package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ark/internal/ai"
	"ark/internal/jobs"
	"ark/internal/system"
)

func (s *Server) fetchSettings() map[string]string {
	out := map[string]string{
		"device_name":         "Ark",
		"port":                "8080",
		"lan_enabled":         "true",
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

func (s *Server) handleSettingsGeneral(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	lan := "false"
	if r.FormValue("lan_enabled") == "on" || r.FormValue("lan_enabled") == "true" {
		lan = "true"
	}
	_ = s.saveSettings(map[string]string{
		"device_name": strings.TrimSpace(r.FormValue("device_name")),
		"port":        strings.TrimSpace(r.FormValue("port")),
		"lan_enabled": lan,
	})
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func (s *Server) handleSettingsAI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	pairs := map[string]string{
		"ai_server_url":  strings.TrimSpace(r.FormValue("ai_server_url")),
		"ai_chat_model":  strings.TrimSpace(r.FormValue("ai_chat_model")),
		"ai_embed_model": strings.TrimSpace(r.FormValue("ai_embed_model")),
	}
	if key := strings.TrimSpace(r.FormValue("ai_api_key")); key != "" {
		pairs["ai_api_key"] = key
	}
	_ = s.saveSettings(pairs)
	http.Redirect(w, r, "/settings?saved=1#ai", http.StatusSeeOther)
}

func (s *Server) handleSettingsDownloads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = s.saveSettings(map[string]string{
		"max_concurrent_jobs": strings.TrimSpace(r.FormValue("max_concurrent_jobs")),
		"video_quality":       strings.TrimSpace(r.FormValue("video_quality")),
	})
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	var sv *SystemView
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

	var dockerOnline bool
	var containers []ContainerView
	if s.docker != nil && s.docker.IsAvailable(r.Context()) {
		dockerOnline = true
		if list, err := s.docker.ListArkContainers(r.Context()); err == nil {
			for _, c := range list {
				cv := ContainerView{
					AppID:      c.AppID,
					Name:       c.Name,
					Image:      c.Image,
					Status:     c.Status,
					CPUText:    "-",
					MemoryText: "-",
				}
				if c.Status == "running" {
					if st, err := s.docker.Stats(r.Context(), c.AppID); err == nil && st != nil {
						cv.CPUText = fmt.Sprintf("%.1f%%", st.CPUPercent)
						cv.MemoryText = fmt.Sprintf("%.1f MB", float64(st.MemoryBytes)/(1024*1024))
					}
				}
				containers = append(containers, cv)
			}
		}
	}

	data := PageData{
		Title:         "System",
		ActiveNav:     "system",
		SystemMetrics: sv,
		Containers:    containers,
		DockerOnline:  dockerOnline,
	}
	s.render(w, "system.html", data)
}

func (s *Server) handleAPIContainerRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	appID := r.FormValue("id")
	if appID != "" && s.docker != nil && s.docker.IsAvailable(r.Context()) {
		_ = s.docker.Restart(r.Context(), appID, 10)
	}
	http.Redirect(w, r, "/system", http.StatusSeeOther)
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

func (s *Server) handleAPIAIDetect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	host := r.FormValue("host")
	servers := ai.DetectServers(r.Context(), host)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"servers": servers})
}

func (s *Server) handleAPIAITest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	urlStr := strings.TrimSpace(r.FormValue("ai_server_url"))
	apiKey := strings.TrimSpace(r.FormValue("ai_api_key"))
	if urlStr == "" || apiKey == "" {
		saved := ai.LoadConnFromDB(r.Context(), s.db)
		if urlStr == "" {
			urlStr = saved.BaseURL
		}
		if apiKey == "" {
			apiKey = saved.APIKey
		}
	}

	models, err := (ai.Conn{BaseURL: urlStr, APIKey: apiKey}).Models(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "models": models})
}

func (s *Server) handleAPIAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var reqBody struct {
		Question string `json:"question"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&reqBody)
	q := strings.TrimSpace(reqBody.Question)
	if q == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "question is required"})
		return
	}

	conn := ai.LoadConnFromDB(r.Context(), s.db)
	qdrant := ai.NewQdrantClient("")
	hits, _ := ai.SearchKnowledge(r.Context(), s.db, conn, qdrant, ai.DefaultCollection, q, 4)

	var ctxBuilder strings.Builder
	type citation struct {
		Title string `json:"title"`
		Path  string `json:"path"`
	}
	var cites []citation
	for i, h := range hits {
		fmt.Fprintf(&ctxBuilder, "[Source %d: %s]\n%s\n\n", i+1, h.Title, h.Text)
		cites = append(cites, citation{Title: h.Title, Path: h.SourcePath})
	}

	systemPrompt := "You are Ark AI, a concise offline reference assistant. Answer using the provided context when relevant and cite source titles."
	userPrompt := q
	if ctxBuilder.Len() > 0 {
		userPrompt = fmt.Sprintf("Context from local library:\n%s\nQuestion: %s", ctxBuilder.String(), q)
	}

	answer, err := conn.ChatStream(r.Context(), []ai.Msg{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, nil)

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "citations": cites})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"answer":    answer,
		"citations": cites,
	})
}

func (s *Server) handleAPIAIIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	rawPath := strings.TrimSpace(r.FormValue("path"))
	if rawPath == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	targetPath := rawPath
	if !filepath.IsAbs(targetPath) {
		if _, err := os.Stat(targetPath); err != nil && s.dataDir != "" {
			candidate := filepath.Join(s.dataDir, targetPath)
			if _, err2 := os.Stat(candidate); err2 == nil {
				targetPath = candidate
			}
		}
	}

	jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
	if s.queue != nil {
		_ = s.queue.Submit(r.Context(), &jobs.Job{
			ID:          jobID,
			Type:        jobs.TypeIndex,
			Target:      targetPath,
			Status:      jobs.StatusPending,
			Progress:    0.0,
			ProgressMsg: "Queued for indexing",
		})
		http.Redirect(w, r, "/jobs", http.StatusSeeOther)
		return
	}

	// Synchronous fallback when QueueManager is unavailable (e.g. unit tests)
	conn := ai.LoadConnFromDB(r.Context(), s.db)
	_, err := ai.IndexSource(r.Context(), s.db, conn, ai.NewQdrantClient(""), ai.DefaultCollection, "doc", targetPath, "", nil)
	if err != nil {
		http.Error(w, "Index failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ask#knowledge", http.StatusSeeOther)
}

func (s *Server) handleAPIAIIndexArticle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 512*1024)).Decode(&payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(payload.Text) == "" {
		http.Error(w, "empty article text", http.StatusBadRequest)
		return
	}

	notesDir := filepath.Join(s.dataDir, "notes", "ai-clips")
	_ = os.MkdirAll(notesDir, 0755)
	safeName := fmt.Sprintf("clip-%d.txt", time.Now().UnixNano())
	clipPath := filepath.Join(notesDir, safeName)
	_ = os.WriteFile(clipPath, []byte(payload.Title+"\n"+payload.URL+"\n\n"+payload.Text), 0644)

	conn := ai.LoadConnFromDB(r.Context(), s.db)
	n, err := ai.IndexSource(r.Context(), s.db, conn, ai.NewQdrantClient(""), ai.DefaultCollection, "zim", clipPath, payload.Title, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"indexed_chunks": n})
}

func (s *Server) handleAPIAIKnowledgeRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	docID := r.FormValue("id")
	if docID != "" {
		_ = ai.RemoveDocument(r.Context(), s.db, ai.NewQdrantClient(""), docID)
	}
	http.Redirect(w, r, "/ask#knowledge", http.StatusSeeOther)
}
