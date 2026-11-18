package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

	var dockerOnline bool
	var containers []ContainerView
	if s.docker != nil && s.docker.IsAvailable(r.Context()) {
		dockerOnline = true
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

	jobsList := s.fetchRecentSessionJobs(3)
	freeGB, totalGB, usedGB, pct, arkPct, otherPct, arkUsedText := s.getStorageBreakdown(r.Context())

	data := PageData{
		Title:               "System",
		ActiveNav:           "system",
		SystemMetrics:       sv,
		Containers:          containers,
		DockerOnline:        dockerOnline,
		LastBenchmark:       s.loadLastBenchmark(),
		LastDowntime:        system.GetDowntimeInfo(r.Context(), s.db),
		StorageFreeGB:       freeGB,
		StorageTotalGB:      totalGB,
		StorageUsedGB:       usedGB,
		StorageUsedPercent:  pct,
		StorageArkPercent:   arkPct,
		StorageOtherPercent: otherPct,
		ArkDataUsedText:     arkUsedText,
		Jobs:                jobsList,
		HasActiveJobs:       hasActive(jobsList),
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

func extractBestSnippet(chunkText, query string) string {
	cleaned := strings.NewReplacer("{", " ", "}", " ", "[", " ", "]", " ", "|", " ").Replace(chunkText)
	norm := strings.Join(strings.Fields(cleaned), " ")
	if norm == "" {
		return ""
	}
	var terms []string
	for _, w := range strings.Fields(strings.ToLower(query)) {
		w = strings.Trim(w, `.,?!:;"'()[]{}`)
		if len(w) >= 3 {
			terms = append(terms, w)
		}
	}
	// Pick the natural prose sentence with the most query keyword hits
	sentences := strings.FieldsFunc(norm, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == '\n'
	})
	best := ""
	bestScore := -1
	for _, s := range sentences {
		s = strings.TrimSpace(strings.TrimLeft(s, ",;:- "))
		if len([]rune(s)) < 28 {
			continue
		}
		low := strings.ToLower(s)
		score := 0
		for _, t := range terms {
			if strings.Contains(low, t) {
				score += 3
			}
		}
		for _, proseWord := range []string{" is ", " are ", " was ", " were ", " the ", " in ", " by ", " that ", " measures "} {
			if strings.Contains(low, proseWord) {
				score += 3
				break
			}
		}
		if score > bestScore {
			bestScore = score
			best = s
		}
	}
	if best == "" {
		best = norm
	}
	words := strings.Fields(best)
	// Strip leading repeated nav/heading words by taking up to 7 trailing/mid prose words
	if len(words) > 10 {
		words = words[len(words)-7:]
	} else if len(words) > 7 {
		words = words[:7]
	}
	return strings.Trim(strings.Join(words, " "), `.,;:-"' `)
}

type chatCitation struct {
	Num        int    `json:"num"`
	Title      string `json:"title"`
	CleanTitle string `json:"clean_title,omitempty"`
	Path       string `json:"path"`
	URL        string `json:"url,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
}

type aiStructuredRef struct {
	ID    int    `json:"id"`
	Quote string `json:"quote"`
}

func (r *aiStructuredRef) UnmarshalJSON(data []byte) error {
	var num int
	if err := json.Unmarshal(data, &num); err == nil {
		r.ID = num
		return nil
	}
	var obj struct {
		ID     int    `json:"id"`
		Source int    `json:"source"`
		Quote  string `json:"quote"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	if obj.ID > 0 {
		r.ID = obj.ID
	} else {
		r.ID = obj.Source
	}
	r.Quote = obj.Quote
	return nil
}

type aiStructuredResponse struct {
	Title      string            `json:"title"`
	Answer     string            `json:"answer"`
	References []aiStructuredRef `json:"references"`
}

var (
	mdLinkRe        = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\n]+)\)`)
	bareSourceTagRe = regexp.MustCompile(`\s*\[(?:Source\s+)?\d+\]`)
	trailingCiteSec = regexp.MustCompile(`(?is)\n+(?:#+\s*(?:sources?|references?|citations?)|\*\*(?:sources?|references?|citations?):?\*\*).*$`)
	latexCmdPrefix  = []string{
		"frac", "forall", "flat", "frown",
		"theta", "tau", "times", "text", "tan", "tanh", "to", "top", "triangle", "tilde", "therefore",
		"beta", "begin", "bar", "bold", "binom", "big", "bullet", "backslash", "box", "bot",
		"nabla", "nu", "neq", "not", "notin", "natural", "neg", "ni", "nmid",
		"rho", "right", "rightarrow", "Rightarrow", "mathrm", "rangle", "rceil", "rfloor", "real",
	}
)

// repairJSONLaTeXEscapes fixes unescaped LaTeX backslashes (e.g. \frac, \theta, \nabla, \mathbf, \[)
// and raw control characters inside JSON string literals so json.Unmarshal preserves LaTeX math.
func repairJSONLaTeXEscapes(raw string) string {
	var out strings.Builder
	out.Grow(len(raw) + 32)
	inString := false
	n := len(raw)

	isHex := func(b byte) bool {
		return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
	}

	for i := 0; i < n; i++ {
		ch := raw[i]
		if !inString {
			if ch == '"' {
				inString = true
			}
			out.WriteByte(ch)
			continue
		}
		if ch == '"' {
			inString = false
			out.WriteByte(ch)
			continue
		}
		if ch == '\n' {
			out.WriteString(`\n`)
			continue
		}
		if ch == '\r' {
			out.WriteString(`\r`)
			continue
		}
		if ch == '\t' {
			out.WriteString(`\t`)
			continue
		}
		if ch != '\\' {
			out.WriteByte(ch)
			continue
		}
		if i+1 >= n {
			out.WriteString(`\\`)
			continue
		}
		next := raw[i+1]
		if next == '\\' || next == '"' || next == '/' {
			out.WriteByte('\\')
			out.WriteByte(next)
			i++
			continue
		}
		if next == 'u' && i+5 < n && isHex(raw[i+2]) && isHex(raw[i+3]) && isHex(raw[i+4]) && isHex(raw[i+5]) {
			out.WriteString(raw[i : i+6])
			i += 5
			continue
		}
		if next == 'b' || next == 'f' || next == 'n' || next == 'r' || next == 't' {
			rest := raw[i+1:]
			isLatex := false
			for _, cmd := range latexCmdPrefix {
				if strings.HasPrefix(rest, cmd) {
					isLatex = true
					break
				}
			}
			if isLatex {
				out.WriteString(`\\`)
			} else {
				out.WriteByte('\\')
				out.WriteByte(next)
				i++
			}
			continue
		}
		// Any other \x (such as \alpha, \varepsilon, \mathbf, \[, \() is invalid in JSON unless doubled
		out.WriteString(`\\`)
	}
	return out.String()
}

// stripInlineLinksAndCiteBlocks removes inline Markdown links and trailing reference lists from answer text.
func stripInlineLinksAndCiteBlocks(answer string) string {
	if strings.TrimSpace(answer) == "" {
		return answer
	}
	answer = strings.TrimRight(trailingCiteSec.ReplaceAllString(answer, ""), "\r\n ")
	answer = mdLinkRe.ReplaceAllString(answer, "$1")
	answer = bareSourceTagRe.ReplaceAllString(answer, "")
	return strings.TrimSpace(answer)
}

func cleanQuoteSnippet(rawQuote, fallbackSnippet string) string {
	cleaned := strings.NewReplacer("{", " ", "}", " ", "[", " ", "]", " ", "|", " ", `"`, " ", "\n", " ", "\r", " ").Replace(rawQuote)
	words := strings.Fields(cleaned)
	if len(words) < 2 {
		return fallbackSnippet
	}
	if len(words) > 8 {
		words = words[:8]
	}
	out := strings.Trim(strings.Join(words, " "), `.,;:-"' `)
	if len([]rune(out)) < 6 {
		return fallbackSnippet
	}
	return out
}

func (s *Server) parseStructuredAIResponse(rawOutput, query string, candidates []chatCitation) (cleanAnswer, parsedTitle string, finalCites []chatCitation) {
	trimmed := strings.TrimSpace(rawOutput)
	// Strip optional ```json ... ``` wrapper
	if strings.HasPrefix(trimmed, "```") {
		if nl := strings.IndexByte(trimmed, '\n'); nl != -1 {
			trimmed = strings.TrimSpace(trimmed[nl+1:])
		}
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "```"))
	}

	var parsed aiStructuredResponse
	parsedOK := false
	startBrace := strings.IndexByte(trimmed, '{')
	endBrace := strings.LastIndexByte(trimmed, '}')
	if startBrace != -1 && endBrace > startBrace {
		candidateJSON := repairJSONLaTeXEscapes(trimmed[startBrace : endBrace+1])
		if err := json.Unmarshal([]byte(candidateJSON), &parsed); err == nil && strings.TrimSpace(parsed.Answer) != "" {
			parsedOK = true
		}
	}

	if !parsedOK {
		cleanAnswer = stripInlineLinksAndCiteBlocks(trimmed)
		for i, c := range candidates {
			c.Num = i + 1
			finalCites = append(finalCites, c)
		}
		return cleanAnswer, "", finalCites
	}

	cleanAnswer = stripInlineLinksAndCiteBlocks(parsed.Answer)
	parsedTitle = ai.ClampTitleWords(parsed.Title, 4)

	seenSource := make(map[int]bool)
	for _, ref := range parsed.References {
		if ref.ID < 1 || ref.ID > len(candidates) || seenSource[ref.ID] {
			continue
		}
		seenSource[ref.ID] = true
		base := candidates[ref.ID-1]
		quote := cleanQuoteSnippet(ref.Quote, base.Snippet)
		citeURL, _ := s.buildCitationLinkWithSnippet(base.Path, quote)
		if citeURL == "" {
			citeURL = base.URL
		}
		base.Num = len(finalCites) + 1
		base.URL = citeURL
		base.Snippet = quote
		finalCites = append(finalCites, base)
	}

	// If the model omitted reference IDs despite retrieved context being available, attach retrieved sources
	if len(finalCites) == 0 && len(candidates) > 0 {
		for i, c := range candidates {
			c.Num = i + 1
			finalCites = append(finalCites, c)
		}
	}
	return cleanAnswer, parsedTitle, finalCites
}

func (s *Server) buildCitationLinkWithSnippet(sourcePath, snippet string) (openURL, cleanSnippet string) {
	cleanSnippet = snippet
	if idx := strings.Index(strings.ToLower(sourcePath), ".zim#"); idx != -1 {
		zimFile := sourcePath[:idx+4]
		articleURL := strings.TrimPrefix(sourcePath[idx+5:], "/")
		stem := strings.TrimSuffix(filepath.Base(zimFile), filepath.Ext(zimFile))
		if resolved, ok := s.resolveZIMArticleURL(stem, articleURL); ok {
			if cleanSnippet != "" {
				rPath, rFrag, rHasFrag := strings.Cut(resolved, "#")
				resolved = rPath + "?ark_hl=" + url.QueryEscape(cleanSnippet)
				if rHasFrag && rFrag != "" {
					resolved += "#" + rFrag
				}
			}
			return resolved, cleanSnippet
		}
		u := "/read/zim/" + url.PathEscape(stem) + "/C/" + articleURL
		if cleanSnippet != "" {
			u += "?ark_hl=" + url.QueryEscape(cleanSnippet)
		}
		return u, cleanSnippet
	}
	if s.dataDir != "" {
		mediaRoot := filepath.Clean(filepath.Join(s.dataDir, "media")) + string(os.PathSeparator)
		cleanSrc := filepath.Clean(sourcePath)
		if strings.HasPrefix(strings.ToLower(cleanSrc), strings.ToLower(mediaRoot)) {
			rel := filepath.ToSlash(cleanSrc[len(mediaRoot):])
			return "/media/" + rel, cleanSnippet
		}
	}
	return "", cleanSnippet
}

func (s *Server) buildCitationLink(sourcePath, chunkText, query string) (openURL, snippet string) {
	snippet = extractBestSnippet(chunkText, query)
	return s.buildCitationLinkWithSnippet(sourcePath, snippet)
}

func (s *Server) handleAPIAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var q, sessionID string
	var attachedNames []string
	var attachedText strings.Builder
	var imageParts []ai.ContentPart

	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid upload payload: " + err.Error()})
			return
		}
		q = strings.TrimSpace(r.FormValue("question"))
		sessionID = strings.TrimSpace(r.FormValue("session_id"))
		if r.MultipartForm != nil {
			for _, fh := range r.MultipartForm.File["files"] {
				attachedNames = append(attachedNames, fh.Filename)
				f, err := fh.Open()
				if err != nil {
					continue
				}
				ext := strings.ToLower(filepath.Ext(fh.Filename))
				switch ext {
				case ".png", ".jpg", ".jpeg", ".webp", ".gif":
					rawImg, rerr := io.ReadAll(io.LimitReader(f, 8<<20))
					_ = f.Close()
					if rerr != nil || len(rawImg) == 0 {
						continue
					}
					mimeType := mime.TypeByExtension(ext)
					if mimeType == "" {
						mimeType = http.DetectContentType(rawImg)
					}
					dataURI := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(rawImg)
					imageParts = append(imageParts, ai.ContentPart{
						Type:     "image_url",
						ImageURL: &ai.ImageURL{URL: dataURI},
					})
				default:
					tmpF, terr := os.CreateTemp("", "ark-att-*"+ext)
					if terr != nil {
						_ = f.Close()
						continue
					}
					tmpPath := tmpF.Name()
					_, _ = io.Copy(tmpF, io.LimitReader(f, 12<<20))
					_ = tmpF.Close()
					_ = f.Close()
					docs, derr := ai.ExtractFile(r.Context(), tmpPath)
					_ = os.Remove(tmpPath)
					if derr == nil && len(docs) > 0 {
						var docText strings.Builder
						for _, d := range docs {
							docText.WriteString(d.Text)
							docText.WriteString("\n\n")
							if docText.Len() > 14000 {
								break
							}
						}
						txt := strings.TrimSpace(docText.String())
						if len(txt) > 14000 {
							txt = txt[:14000]
						}
						if txt != "" {
							fmt.Fprintf(&attachedText, "[Attached File: %s]\n%s\n\n", fh.Filename, txt)
						}
					}
				}
			}
		}
	} else {
		var reqBody struct {
			Question  string `json:"question"`
			SessionID string `json:"session_id"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&reqBody)
		q = strings.TrimSpace(reqBody.Question)
		sessionID = strings.TrimSpace(reqBody.SessionID)
	}

	if q == "" && attachedText.Len() == 0 && len(imageParts) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "question or attachment is required"})
		return
	}
	if q == "" {
		q = "Analyze the attached content."
	}

	// Load prior session messages for multi-turn memory or start a fresh session
	var historyMsgs []ai.Msg
	var existingPinned bool
	var existingTitle string
	isFirstTurn := true
	if sessionID != "" && s.db != nil {
		if sess, prior, err := ai.GetChatSession(r.Context(), s.db, sessionID); err == nil {
			existingPinned = sess.Pinned
			existingTitle = sess.Title
			if len(prior) > 0 {
				isFirstTurn = false
			}
			startIdx := 0
			if len(prior) > 10 {
				startIdx = len(prior) - 10
			}
			for _, m := range prior[startIdx:] {
				historyMsgs = append(historyMsgs, ai.Msg{Role: m.Role, Content: m.Content})
			}
		} else {
			sessionID = ""
		}
	}
	if sessionID == "" && s.db != nil {
		if newID, err := ai.CreateChatSession(r.Context(), s.db, "New Chat"); err == nil {
			sessionID = newID
			existingTitle = "New Chat"
		}
	}

	conn := ai.LoadConnFromDB(r.Context(), s.db)
	qdrant := ai.NewQdrantClient("")
	hits, _ := ai.SearchKnowledge(r.Context(), s.db, conn, qdrant, ai.DefaultCollection, q, 3)

	var ctxBuilder strings.Builder
	var candidates []chatCitation
	for i, h := range hits {
		citeURL, snippet := s.buildCitationLink(h.SourcePath, h.Text, q)
		cleanTitle := strings.TrimSpace(strings.Split(h.Title, " (")[0])
		if cleanTitle == "" {
			cleanTitle = h.Title
		}
		chunkPreview := strings.TrimSpace(h.Text)
		if len([]rune(chunkPreview)) > 1400 {
			chunkPreview = string([]rune(chunkPreview)[:1400]) + "..."
		}
		fmt.Fprintf(&ctxBuilder, "[%d] Title: %s\n%s\n\n", i+1, cleanTitle, chunkPreview)
		candidates = append(candidates, chatCitation{
			Num:        i + 1,
			Title:      h.Title,
			CleanTitle: cleanTitle,
			Path:       h.SourcePath,
			URL:        citeURL,
			Snippet:    snippet,
		})
	}

	systemPrompt := `You are Ark AI, a focused offline reference assistant. Stay strictly on the user's question, ignore unrelated context snippets, and give concise direct answers (2-4 short sentences or brief bullets).
Respond ONLY with a valid JSON object matching this exact schema:
{
  "title": "1 to 4 word title based strictly on the user's query",
  "answer": "Concise Markdown and LaTeX ($...$ inline, $$...$$ display) response. Do NOT include any links, URLs, [1] tags, or a references section inside answer.",
  "references": [
    {"id": 1, "quote": "exact phrase or line referenced from context source 1"}
  ]
}
Only list sources in "references" that you actually used.`
	if dt := system.GetDowntimeInfo(r.Context(), s.db); dt.HasPrevious && dt.Downtime >= time.Minute {
		systemPrompt += fmt.Sprintf("\n(Current local time: %s; Ark resumed after being offline for %s.)", time.Now().Format("2006-01-02 15:04"), dt.DowntimeHuman)
	}

	var promptBuilder strings.Builder
	if ctxBuilder.Len() > 0 {
		fmt.Fprintf(&promptBuilder, "Context sources:\n%s\n", ctxBuilder.String())
	}
	if attachedText.Len() > 0 {
		fmt.Fprintf(&promptBuilder, "Temporary user attachments:\n%s\n", attachedText.String())
	}
	fmt.Fprintf(&promptBuilder, "User query: %s", q)

	userMsg := ai.Msg{Role: "user", Content: promptBuilder.String()}
	if len(imageParts) > 0 {
		parts := append([]ai.ContentPart{{Type: "text", Text: promptBuilder.String()}}, imageParts...)
		userMsg.Parts = parts
	}

	chatMsgs := make([]ai.Msg, 0, len(historyMsgs)+2)
	chatMsgs = append(chatMsgs, ai.Msg{Role: "system", Content: systemPrompt})
	chatMsgs = append(chatMsgs, historyMsgs...)
	chatMsgs = append(chatMsgs, userMsg)

	rawAnswer, err := conn.ChatStream(r.Context(), chatMsgs, nil)

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		errMsg := err.Error()
		if len(imageParts) > 0 && strings.Contains(errMsg, "400") {
			errMsg += " (Active chat model may not support vision/images; try pulling a multimodal model such as llama3.2-vision or qwen2.5vl.)"
		}
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": errMsg, "citations": candidates})
		return
	}
	answer, parsedTitle, cites := s.parseStructuredAIResponse(rawAnswer, q, candidates)

	if sessionID != "" && s.db != nil {
		if isFirstTurn {
			// Title is based strictly on the user's query (using parsedTitle or query keywords)
			genTitle := ai.TitleFromQuery(q, 4)
			if parsedTitle != "" && len(strings.Fields(parsedTitle)) <= 4 {
				genTitle = parsedTitle
			}
			if genTitle != "" {
				existingTitle = genTitle
				_ = ai.UpdateSessionTitle(r.Context(), s.db, sessionID, genTitle)
			}
		}
		citesBytes, _ := json.Marshal(cites)
		_ = ai.AppendChatMessage(r.Context(), s.db, sessionID, "user", q, strings.Join(attachedNames, ", "))
		_ = ai.AppendChatMessage(r.Context(), s.db, sessionID, "assistant", answer, string(citesBytes))
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"title":          existingTitle,
		"answer":         answer,
		"citations":      cites,
		"session_id":     sessionID,
		"session_title":  existingTitle,
		"session_pinned": existingPinned,
	})
}

func (s *Server) handleAPIAISessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	sessions, err := ai.ListChatSessions(r.Context(), s.db)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "sessions": sessions})
}

func (s *Server) handleAPIAISessionGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	sess, msgs, err := ai.GetChatSession(r.Context(), s.db, id)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "session not found"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": sess, "messages": msgs})
}

func (s *Server) handleAPIAISessionPin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	pinned := r.FormValue("pinned") == "1" || r.FormValue("pinned") == "true"
	w.Header().Set("Content-Type", "application/json")
	if id == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "session id is required"})
		return
	}
	if err := ai.SetSessionPinned(r.Context(), s.db, id, pinned); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id, "pinned": pinned})
}

func (s *Server) handleAPIAISessionDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	w.Header().Set("Content-Type", "application/json")
	if id == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "session id is required"})
		return
	}
	if err := ai.DeleteChatSession(r.Context(), s.db, id); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
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
	wantsJSON := r.FormValue("ajax") == "1" || strings.Contains(r.Header.Get("Accept"), "application/json")
	targetPath := rawPath
	if !filepath.IsAbs(targetPath) && s.dataDir != "" {
		if _, err := os.Stat(targetPath); err != nil {
			candidate := filepath.Join(s.dataDir, targetPath)
			if _, err2 := os.Stat(candidate); err2 == nil {
				targetPath = candidate
			}
		}
	}
	if st, err := os.Stat(targetPath); err != nil || st.IsDir() {
		errMsg := "File not found: " + rawPath
		if err == nil && st.IsDir() {
			errMsg = "Target must be a file, not a directory"
		}
		if wantsJSON {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": errMsg})
			return
		}
		http.Redirect(w, r, "/ask?err="+url.QueryEscape(errMsg)+"#knowledge", http.StatusSeeOther)
		return
	}

	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(targetPath)), ".")
	metaBytes, _ := json.Marshal(jobs.JobMetadata{
		FileName: strings.TrimSuffix(filepath.Base(targetPath), filepath.Ext(targetPath)),
		Format:   ext,
	})

	jobID := fmt.Sprintf("job-%d-%d", time.Now().UnixNano(), jobSeq.Add(1))
	if s.queue != nil {
		if err := s.queue.Submit(r.Context(), &jobs.Job{
			ID:          jobID,
			Type:        jobs.TypeIndex,
			Target:      targetPath,
			Status:      jobs.StatusPending,
			Progress:    0.0,
			ProgressMsg: "Queued for indexing",
			Metadata:    string(metaBytes),
		}); err != nil {
			if wantsJSON {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
				return
			}
			http.Error(w, "Queue error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if wantsJSON {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "job_id": jobID, "msg": "Indexing " + filepath.Base(targetPath) + " into AI knowledge base..."})
			return
		}
		redir := sanitizeLocalRedirect(r.FormValue("redirect"))
		http.Redirect(w, r, redir, http.StatusSeeOther)
		return
	}

	// Synchronous fallback when QueueManager is unavailable (e.g. unit tests)
	conn := ai.LoadConnFromDB(r.Context(), s.db)
	_, err := ai.IndexSource(r.Context(), s.db, conn, ai.NewQdrantClient(""), ai.DefaultCollection, ext, targetPath, "", nil)
	if err != nil {
		if wantsJSON {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		http.Error(w, "Index failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if wantsJSON {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "msg": "Indexed " + filepath.Base(targetPath) + " into AI knowledge base"})
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
	if err := json.NewDecoder(io.LimitReader(r.Body, 2*1024*1024)).Decode(&payload); err != nil {
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
