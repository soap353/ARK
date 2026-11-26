// Ask AI HTTP/SSE chat endpoints, structured JSON/LaTeX repair, citation extraction,
// conversation session management, knowledge indexing, and Ollama model controls.
package web

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ark/internal/ai"
	"ark/internal/fieldguide"
	"ark/internal/jobs"
	"ark/internal/system"
)

// ---------------------------------------------------------------------------
// AI Server Status, Auto-Detection & Connectivity Testing
// ---------------------------------------------------------------------------

func (s *Server) handleAPIAIStatus(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	conn := ai.LoadConnFromDB(r.Context(), s.db)
	if isLocalRequest(r) {
		if u := strings.TrimSpace(r.URL.Query().Get("url")); u != "" {
			if strings.TrimRight(u, "/") != strings.TrimRight(conn.BaseURL, "/") {
				// Never forward a saved API key to an overridden URL.
				conn.APIKey = ""
			}
			conn.BaseURL = u
		}
		if m := strings.TrimSpace(r.URL.Query().Get("model")); m != "" {
			conn.ChatModel = m
		}
		if cl := strings.TrimSpace(r.URL.Query().Get("context_length")); cl != "" {
			var n int
			if _, err := fmt.Sscanf(cl, "%d", &n); err == nil && n >= 0 {
				conn.ContextLength = n
			}
		}
	}
	writeJSON(w, http.StatusOK, conn.Status(r.Context()))
}

func (s *Server) handleAPIAIDetect(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	servers := ai.DetectServers(r.Context(), r.FormValue("host"))
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

func (s *Server) handleAPIAITest(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	urlStr := strings.TrimSpace(r.FormValue("ai_server_url"))
	apiKey := strings.TrimSpace(r.FormValue("ai_api_key"))
	if urlStr == "" || apiKey == "" {
		saved := ai.LoadConnFromDB(r.Context(), s.db)
		if urlStr == "" {
			urlStr = saved.BaseURL
		}
		// Only reuse the stored API key when testing the same configured server URL.
		if apiKey == "" && strings.TrimRight(urlStr, "/") == strings.TrimRight(saved.BaseURL, "/") {
			apiKey = saved.APIKey
		}
	}

	models, err := (ai.Conn{BaseURL: urlStr, APIKey: apiKey}).Models(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": models})
}

// ---------------------------------------------------------------------------
// Ollama Model Manager (/ask)
// ---------------------------------------------------------------------------

func (s *Server) handleAPIAIModels(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	settings := s.fetchSettings()
	models, err := ai.ListOllamaModels(r.Context(), settings["ai_server_url"])
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "models": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"models":      models,
		"chat_model":  settings["ai_chat_model"],
		"embed_model": settings["ai_embed_model"],
	})
}

func (s *Server) handleAPIAIModelPull(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		redirectWithQuery(w, r, "/ask", "err", "Enter a model name (e.g. llama3.2:3b)")
		return
	}
	serverURL := s.fetchSettings()["ai_server_url"]

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
			redirectWithQuery(w, r, "/ask", "err", "Failed to queue pull: "+err.Error())
			return
		}
		http.Redirect(w, r, "/system", http.StatusSeeOther)
		return
	}
	if err := ai.PullOllamaModel(r.Context(), serverURL, model, nil); err != nil {
		redirectWithQuery(w, r, "/ask", "err", "Pull failed: "+err.Error())
		return
	}
	redirectWithQuery(w, r, "/ask", "msg", "Pulled model "+model)
}

func (s *Server) handleAPIAIModelDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if err := ai.DeleteOllamaModel(r.Context(), s.fetchSettings()["ai_server_url"], model); err != nil {
		redirectWithQuery(w, r, "/ask", "err", "Delete model failed: "+err.Error())
		return
	}
	redirectWithQuery(w, r, "/ask", "msg", "Deleted model "+model)
}

func (s *Server) handleAPIAIModelSelect(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	role := strings.TrimSpace(r.FormValue("role")) // "chat" or "embed"
	key := "ai_chat_model"
	if role == "embed" {
		key = "ai_embed_model"
	}
	if model != "" {
		_ = s.saveSettings(map[string]string{key: model})
	}
	redirectWithQuery(w, r, "/ask", "msg", "Selected "+model+" for "+role)
}

// ---------------------------------------------------------------------------
// Response Parsing, LaTeX Repair & Grounded Citation Linking
// ---------------------------------------------------------------------------

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
	// Pick the natural prose sentence with the most query keyword hits.
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
	// Trim leading navigation/heading words into a clean 7-word phrase for ?ark_hl=.
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
	MapActions []chatMapAction   `json:"map_actions"`
}

var (
	mdLinkRe        = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\n]+)\)`)
	bareSourceTagRe = regexp.MustCompile(`\s*\[(?:Source\s+)?\d+\]`)
	trailingCiteSec = regexp.MustCompile(`(?is)\n+(?:#+\s*(?:sources?|references?|citations?)|\*\*(?:sources?|references?|citations?):?\*\*).*$`)
	answerFieldKeyRe = regexp.MustCompile(`"answer"\s*:\s*"`)
	latexCmdPrefix   = []string{
		"frac", "forall", "flat", "frown",
		"theta", "tau", "times", "text", "tan", "tanh", "to", "top", "triangle", "tilde", "therefore",
		"beta", "begin", "bar", "bold", "binom", "big", "bullet", "backslash", "box", "bot",
		"nabla", "nu", "neq", "not", "notin", "natural", "neg", "ni", "nmid",
		"rho", "right", "rightarrow", "Rightarrow", "mathrm", "rangle", "rceil", "rfloor", "real",
	}
)

// repairJSONLaTeXEscapes fixes unescaped LaTeX backslashes (e.g. \frac, \theta, \nabla)
// and raw control characters inside JSON string literals so json.Unmarshal preserves math.
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
		out.WriteString(`\\`)
	}
	return out.String()
}

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
	ans, title, cites, _ := s.parseStructuredAIResponseFull(rawOutput, query, candidates)
	return ans, title, cites
}

func (s *Server) parseStructuredAIResponseFull(rawOutput, query string, candidates []chatCitation) (cleanAnswer, parsedTitle string, finalCites []chatCitation, mapActions []chatMapAction) {
	trimmed := strings.TrimSpace(rawOutput)
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
		return stripInlineLinksAndCiteBlocks(trimmed), "", nil, nil
	}

	cleanAnswer = stripInlineLinksAndCiteBlocks(parsed.Answer)
	parsedTitle = ai.ClampTitleWords(parsed.Title, 4)
	mapActions = validateMapActions(parsed.MapActions)

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

	return cleanAnswer, parsedTitle, finalCites, mapActions
}

// jsonAnswerStreamer incrementally extracts unescaped text inside the JSON "answer" field during SSE streaming.
type jsonAnswerStreamer struct {
	buf     strings.Builder
	started bool
	done    bool
	pos     int
	escape  bool
}

func (st *jsonAnswerStreamer) feed(chunk string) string {
	st.buf.WriteString(chunk)
	if st.done {
		return ""
	}
	raw := st.buf.String()
	if !st.started {
		loc := answerFieldKeyRe.FindStringIndex(raw)
		if loc == nil {
			return ""
		}
		st.started = true
		st.pos = loc[1]
	}
	var out strings.Builder
	for st.pos < len(raw) {
		b := raw[st.pos]
		st.pos++
		if st.escape {
			st.escape = false
			switch b {
			case 'n':
				out.WriteByte('\n')
			case 't':
				out.WriteByte('\t')
			case 'r':
			case '"', '\\', '/':
				out.WriteByte(b)
			default:
				out.WriteByte('\\')
				out.WriteByte(b)
			}
			continue
		}
		if b == '\\' {
			st.escape = true
			continue
		}
		if b == '"' {
			st.done = true
			break
		}
		out.WriteByte(b)
	}
	return out.String()
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

// ---------------------------------------------------------------------------
// Ask AI Chat Endpoint (/api/ai/chat) and Helpers
// ---------------------------------------------------------------------------

type aiHistoryTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type aiChatRequest struct {
	Question      string
	SessionID     string
	Scope         string
	TriageType    string
	PreZIM        string
	PreArticle    string
	SourcesOnly   bool
	ProtocolMode  bool
	WantStream    bool
	AttachedNames []string
	AttachedText  string
	ImageParts    []ai.ContentPart
	ClientHistory []aiHistoryTurn
}

func parseAIChatRequest(r *http.Request) (*aiChatRequest, error) {
	req := &aiChatRequest{Scope: "all"}
	ct := strings.ToLower(r.Header.Get("Content-Type"))

	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			return nil, fmt.Errorf("invalid upload payload: %w", err)
		}
		req.Question = strings.TrimSpace(r.FormValue("question"))
		req.SessionID = strings.TrimSpace(r.FormValue("session_id"))
		if s := strings.ToLower(strings.TrimSpace(r.FormValue("scope"))); s != "" {
			req.Scope = s
		}
		req.TriageType = strings.ToLower(strings.TrimSpace(r.FormValue("triage_type")))
		req.PreZIM = strings.TrimSpace(r.FormValue("zim"))
		req.PreArticle = strings.TrimSpace(r.FormValue("article"))
		req.SourcesOnly = r.FormValue("sources_only") == "1" || r.FormValue("sources_only") == "true"
		req.ProtocolMode = r.FormValue("protocol_mode") == "1" || r.FormValue("protocol_mode") == "true"
		req.WantStream = r.FormValue("stream") == "1" || r.FormValue("stream") == "true"
		if rawHist := strings.TrimSpace(r.FormValue("history")); rawHist != "" {
			_ = json.Unmarshal([]byte(rawHist), &req.ClientHistory)
		}

		var attachedText strings.Builder
		if r.MultipartForm != nil {
			for _, fh := range r.MultipartForm.File["files"] {
				req.AttachedNames = append(req.AttachedNames, fh.Filename)
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
					req.ImageParts = append(req.ImageParts, ai.ContentPart{
						Type:     "image_url",
						ImageURL: &ai.ImageURL{URL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(rawImg)},
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
		req.AttachedText = attachedText.String()
		return req, nil
	}

	var body struct {
		Question     string          `json:"question"`
		SessionID    string          `json:"session_id"`
		Scope        string          `json:"scope"`
		SourcesOnly  bool            `json:"sources_only"`
		ProtocolMode bool            `json:"protocol_mode"`
		TriageType   string          `json:"triage_type"`
		ZIM          string          `json:"zim"`
		Article      string          `json:"article"`
		Stream       bool            `json:"stream"`
		History      []aiHistoryTurn `json:"history"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 128*1024)).Decode(&body)
	req.Question = strings.TrimSpace(body.Question)
	req.SessionID = strings.TrimSpace(body.SessionID)
	if s := strings.ToLower(strings.TrimSpace(body.Scope)); s != "" {
		req.Scope = s
	}
	req.SourcesOnly = body.SourcesOnly
	req.ProtocolMode = body.ProtocolMode
	req.TriageType = strings.ToLower(strings.TrimSpace(body.TriageType))
	req.PreZIM = strings.TrimSpace(body.ZIM)
	req.PreArticle = strings.TrimSpace(body.Article)
	req.WantStream = body.Stream
	req.ClientHistory = body.History
	return req, nil
}

func (s *Server) prepareAIChatSession(ctx context.Context, w http.ResponseWriter, req *aiChatRequest) (history []ai.Msg, isFirstTurn, pinned bool, title string) {
	isFirstTurn = true
	sessionID := req.SessionID

	if sessionID != "" && s.db != nil {
		if sess, prior, err := ai.GetChatSession(ctx, s.db, sessionID); err == nil {
			pinned = sess.Pinned
			title = sess.Title
			if len(prior) > 0 {
				isFirstTurn = false
			}
			if len(prior) > 16 {
				prior = prior[len(prior)-16:]
			}
			for _, m := range prior {
				history = append(history, ai.Msg{Role: m.Role, Content: m.Content})
			}
		} else {
			sessionID = ""
		}
	}
	if sessionID == "" && s.db != nil {
		if newID, err := ai.CreateChatSession(ctx, s.db, "New Chat"); err == nil {
			sessionID = newID
			title = "New Chat"
		}
	}
	if len(history) == 0 && len(req.ClientHistory) > 0 {
		isFirstTurn = false
		turns := req.ClientHistory
		if len(turns) > 16 {
			turns = turns[len(turns)-16:]
		}
		for _, ht := range turns {
			role := strings.ToLower(strings.TrimSpace(ht.Role))
			content := strings.TrimSpace(ht.Content)
			if (role == "user" || role == "assistant") && content != "" {
				history = append(history, ai.Msg{Role: role, Content: content})
				if sessionID != "" && s.db != nil {
					_ = ai.AppendChatMessage(ctx, s.db, sessionID, role, content, "")
				}
			}
		}
	}

	req.SessionID = sessionID
	if sessionID != "" {
		w.Header().Set("X-Ark-Session-Id", sessionID)
	}
	return history, isFirstTurn, pinned, title
}

func (s *Server) assembleAIChatContext(ctx context.Context, req *aiChatRequest, history []ai.Msg, maxChunkRunes int) (contextText, extraToolText string, candidates []chatCitation) {
	var ctxBuilder strings.Builder
	seenPath := make(map[string]bool)

	addCandidate := func(title, cleanTitle, sourcePath, urlStr, text, snippet string) {
		if len(candidates) >= 8 || strings.TrimSpace(text) == "" {
			return
		}
		if sourcePath != "" && seenPath[sourcePath] {
			return
		}
		if sourcePath != "" {
			seenPath[sourcePath] = true
		}
		if cleanTitle == "" {
			cleanTitle = title
		}
		chunkPreview := strings.TrimSpace(text)
		if len([]rune(chunkPreview)) > maxChunkRunes {
			chunkPreview = string([]rune(chunkPreview)[:maxChunkRunes]) + "..."
		}
		num := len(candidates) + 1
		fmt.Fprintf(&ctxBuilder, "[%d] Title: %s\n%s\n\n", num, cleanTitle, chunkPreview)
		candidates = append(candidates, chatCitation{
			Num:        num,
			Title:      title,
			CleanTitle: cleanTitle,
			Path:       sourcePath,
			URL:        urlStr,
			Snippet:    snippet,
		})
	}

	// Preloaded ZIM article opened directly from ZIM reader (?zim=...&article=...)
	if req.PreZIM != "" && req.PreArticle != "" {
		if artTitle, artTxt, artURL, ok := s.readZIMArticlePlainText(req.PreZIM, req.PreArticle, 3200); ok {
			zimPath := filepath.Join(s.dataDir, "zim", req.PreZIM+".zim") + "#" + strings.TrimPrefix(req.PreArticle, "C/")
			addCandidate(fmt.Sprintf("%s (%s)", artTitle, req.PreZIM), artTitle, zimPath, artURL, artTxt, excerptForTerms(artTxt, ai.SignificantTerms(req.Question)))
		}
	}

	ragQuery := req.Question
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" && strings.TrimSpace(history[i].Content) != "" {
			ragQuery = history[i].Content + " " + req.Question
			break
		}
	}

	calcRes := evalAskMathTool(req.Question)
	if queryNeedsLocalKnowledge(req.Question, req.Scope, req.TriageType, req.PreZIM, req.SourcesOnly, calcRes != "") && req.Scope != "attached" {
		if req.Scope == "all" || req.Scope == "notes" || req.Scope == "zim" {
			hits, _ := ai.SearchKnowledge(ctx, s.db, ragQuery, 3)
			for _, h := range hits {
				isZIM := strings.Contains(strings.ToLower(h.SourcePath), ".zim")
				if (req.Scope == "notes" && isZIM) || (req.Scope == "zim" && !isZIM) {
					continue
				}
				citeURL, snippet := s.buildCitationLink(h.SourcePath, h.Text, ragQuery)
				cleanTitle := strings.TrimSpace(strings.Split(h.Title, " (")[0])
				addCandidate(h.Title, cleanTitle, h.SourcePath, citeURL, h.Text, snippet)
			}
		}
		if req.Scope == "all" || req.Scope == "zim" {
			for _, h := range s.searchLiveZIMs(ragQuery, 2) {
				addCandidate(h.Title, h.CleanTitle, h.SourcePath, h.URL, h.Text, h.Snippet)
			}
		}
		if req.Scope == "all" || req.Scope == "remedy" || req.TriageType == "symptom" {
			for _, h := range searchRemedyGuide(ragQuery, 2) {
				addCandidate(h.Title, h.CleanTitle, h.SourcePath, h.URL, h.Text, h.Snippet)
			}
		}
		if req.Scope == "all" || req.Scope == "druglabels" || req.TriageType == "pill" || req.TriageType == "symptom" {
			for _, h := range s.searchDrugLabels(ctx, ragQuery, 2) {
				addCandidate(h.Title, h.CleanTitle, h.SourcePath, h.URL, h.Text, h.Snippet)
			}
		}
	}

	var extraToolCtx strings.Builder
	lowQ := strings.ToLower(req.Question)
	if req.Scope == "all" && (coordPairRe.MatchString(req.Question) || strings.Contains(lowQ, "pin") || strings.Contains(lowQ, "waypoint") || strings.Contains(lowQ, "map") || strings.Contains(lowQ, "closest") || strings.Contains(lowQ, "nearest") || strings.Contains(lowQ, "shelter") || strings.Contains(lowQ, "water source")) {
		if mp := s.queryMapWaypointsContext(req.Question); mp != "" {
			extraToolCtx.WriteString(mp)
			extraToolCtx.WriteByte('\n')
		}
	}
	if calcRes != "" {
		extraToolCtx.WriteString(calcRes)
		extraToolCtx.WriteByte('\n')
	}

	return ctxBuilder.String(), extraToolCtx.String(), candidates
}

func buildAIChatPrompt(req *aiChatRequest, history []ai.Msg, contextText, extraToolText, existingTitle string, dt system.DowntimeInfo) (systemPrompt, userPrompt string, chatMsgs []ai.Msg) {
	systemPrompt = `You are Ark AI, a helpful offline assistant. Stay focused on the user's question, ignore any unrelated context snippets, and give concise direct answers.
Maintain multi-turn conversation continuity: use the prior conversation history to answer follow-up questions, resolve pronouns (it, that, they, this), and remember facts established in earlier turns of this chat.
Tools and retrieved local sources are optional: if the query is conversational (greetings, thanks, casual chat), creative, coding/general reasoning, or does not require looking up local archives, maps, or tools, answer directly without using or mentioning any tools and return "references": [] and "map_actions": [].
Respond ONLY with a valid JSON object matching this exact schema:
{
  "title": "1 to 4 word title based strictly on the user's query",
  "answer": "Concise Markdown and LaTeX ($...$ inline, $$...$$ display) response. Do NOT include any links, URLs, [1] tags, or a references section inside answer.",
  "references": [
    {"id": 1, "quote": "exact phrase or line referenced from context source 1"}
  ],
  "map_actions": [
    {"title": "Location name", "lat": 40.0, "lng": -74.5, "category": "waypoint", "note": "optional note"}
  ]
}
Only list sources in "references" when retrieved Context sources were provided and actually used to answer the question; otherwise return "references": []. Only include "map_actions" when discussing specific coordinates or saved waypoints.`

	if req.SourcesOnly {
		systemPrompt += "\nSTRICT MODE ENABLED: Answer ONLY from the provided Context sources, attachments, or calculated values. Do NOT use outside memory or speculate. If the context does not contain the answer, say so clearly."
	}
	if req.ProtocolMode {
		systemPrompt += "\nPROTOCOL MODE ENABLED: Format \"answer\" starting with a brief **Warnings / Contraindications** line (from sources if applicable), followed by numbered Markdown task list items (`- [ ] **Step 1:** ...`)."
	}
	if req.TriageType == "pill" {
		systemPrompt += "\nTRIAGE MODE (PILL): Prioritize exact matches on pill imprint, active/generic ingredients, dosage, boxed warnings, and contraindications from Drug Labels."
	} else if req.TriageType == "symptom" {
		systemPrompt += "\nTRIAGE MODE (SYMPTOM): Highlight red-flag emergency warning signs first, then summarize matching guidance from Drug Labels and Remedy."
	}
	if dt.HasPrevious && dt.Downtime >= time.Minute {
		systemPrompt += fmt.Sprintf("\n(Current local time: %s; Ark resumed after being offline for %s.)", time.Now().Format("2006-01-02 15:04"), dt.DowntimeHuman)
	}

	var promptBuilder strings.Builder
	if len(history) > 0 {
		promptBuilder.WriteString("Prior conversation history in this chat:\n")
		for _, hm := range history {
			roleLbl := "User"
			if hm.Role == "assistant" {
				roleLbl = "Ark AI"
			}
			snippet := strings.TrimSpace(hm.Content)
			if len([]rune(snippet)) > 700 {
				snippet = string([]rune(snippet)[:700]) + "..."
			}
			fmt.Fprintf(&promptBuilder, "%s: %s\n", roleLbl, snippet)
		}
		promptBuilder.WriteByte('\n')
	}
	if extraToolText != "" {
		promptBuilder.WriteString(extraToolText)
		promptBuilder.WriteByte('\n')
	}
	if contextText != "" {
		fmt.Fprintf(&promptBuilder, "Context sources:\n%s\n", contextText)
	}
	if req.AttachedText != "" {
		fmt.Fprintf(&promptBuilder, "Temporary user attachments:\n%s\n", req.AttachedText)
	}
	fmt.Fprintf(&promptBuilder, "User query: %s", req.Question)
	userPrompt = promptBuilder.String()

	userMsg := ai.Msg{Role: "user", Content: userPrompt}
	if len(req.ImageParts) > 0 {
		userMsg.Parts = append([]ai.ContentPart{{Type: "text", Text: userPrompt}}, req.ImageParts...)
	}

	chatMsgs = make([]ai.Msg, 0, len(history)+2)
	chatMsgs = append(chatMsgs, ai.Msg{Role: "system", Content: systemPrompt})
	for _, hm := range history {
		if hm.Role == "assistant" && !strings.HasPrefix(strings.TrimSpace(hm.Content), "{") {
			b, _ := json.Marshal(map[string]any{
				"title":       existingTitle,
				"answer":      hm.Content,
				"references":  []any{},
				"map_actions": []any{},
			})
			chatMsgs = append(chatMsgs, ai.Msg{Role: "assistant", Content: string(b)})
		} else {
			chatMsgs = append(chatMsgs, hm)
		}
	}
	chatMsgs = append(chatMsgs, userMsg)
	return systemPrompt, userPrompt, chatMsgs
}

func initAIChatSSE(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	return flusher
}

func emitSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, payload any) {
	b, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) handleAPIAIChat(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	req, err := parseAIChatRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Question == "" && req.AttachedText == "" && len(req.ImageParts) == 0 {
		writeJSONError(w, http.StatusBadRequest, "question or attachment is required")
		return
	}
	if req.Question == "" {
		req.Question = "Analyze the attached content."
	}

	historyMsgs, isFirstTurn, existingPinned, existingTitle := s.prepareAIChatSession(r.Context(), w, req)

	conn := ai.LoadConnFromDB(r.Context(), s.db)
	status := conn.Status(r.Context())
	if !status.ServerOnline || (!status.ModelLoaded && strings.TrimSpace(conn.ChatModel) == "") {
		writeJSONError(w, http.StatusServiceUnavailable, "No AI model available. Check your AI server URL and selected model in Settings.")
		return
	}
	if status.ModelLoaded && status.LoadedModel != "" {
		conn.ChatModel = status.LoadedModel
	}

	ctxMax := status.ContextLength
	maxChunkRunes := 1200
	if ctxMax > 0 && ctxMax <= 4096 {
		maxChunkRunes = 650
		if len(historyMsgs) > 4 {
			historyMsgs = historyMsgs[len(historyMsgs)-4:]
		}
	}
	if ctxMax <= 0 {
		ctxMax = 8192
	}

	if s.db != nil && s.dataDir != "" && (req.Scope == "all" || req.Scope == "notes") {
		s.syncAutoIndexedDirs(r.Context(), conn)
	}

	contextText, extraToolText, candidates := s.assembleAIChatContext(r.Context(), req, historyMsgs, maxChunkRunes)

	// Strict Verified Sources Only short-circuit when zero local evidence matched.
	if req.SourcesOnly && contextText == "" && req.AttachedText == "" && extraToolText == "" {
		noSrcAns := "No verified local source found for this question in the selected scope."
		if req.SessionID != "" && s.db != nil {
			_ = ai.AppendChatMessage(r.Context(), s.db, req.SessionID, "user", req.Question, strings.Join(req.AttachedNames, ", "))
			_ = ai.AppendChatMessage(r.Context(), s.db, req.SessionID, "assistant", noSrcAns, "[]")
		}
		payload := map[string]any{
			"title":          existingTitle,
			"answer":         noSrcAns,
			"citations":      []chatCitation{},
			"map_actions":    []chatMapAction{},
			"session_id":     req.SessionID,
			"session_title":  existingTitle,
			"session_pinned": existingPinned,
			"context_used":   estimatePromptTokens("", historyMsgs, req.Question, noSrcAns),
			"context_max":    ctxMax,
		}
		if req.WantStream {
			flusher := initAIChatSSE(w)
			emitSSEEvent(w, flusher, "token", map[string]any{"token": noSrcAns})
			emitSSEEvent(w, flusher, "done", payload)
			return
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}

	systemPrompt, userPrompt, chatMsgs := buildAIChatPrompt(req, historyMsgs, contextText, extraToolText, existingTitle, system.GetDowntimeInfo(r.Context(), s.db))

	var flusher http.Flusher
	var streamer jsonAnswerStreamer
	var tokenCB func(string)
	if req.WantStream {
		flusher = initAIChatSSE(w)
		tokenCB = func(tok string) {
			if delta := streamer.feed(tok); delta != "" {
				emitSSEEvent(w, flusher, "token", map[string]any{"token": delta})
			}
		}
	}

	rawAnswer, err := conn.ChatStream(r.Context(), chatMsgs, tokenCB)
	if err != nil {
		errMsg := err.Error()
		if len(req.ImageParts) > 0 && strings.Contains(errMsg, "400") {
			errMsg += " (Active chat model may not support vision/images; try pulling a multimodal model such as llama3.2-vision or qwen2.5vl.)"
		}
		if req.WantStream {
			emitSSEEvent(w, flusher, "error", map[string]any{"error": errMsg})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": errMsg, "citations": candidates})
		return
	}

	answer, parsedTitle, cites, mapActions := s.parseStructuredAIResponseFull(rawAnswer, req.Question, candidates)
	if req.SessionID != "" && s.db != nil {
		if isFirstTurn {
			genTitle := ai.TitleFromQuery(req.Question, 4)
			if parsedTitle != "" && len(strings.Fields(parsedTitle)) <= 4 {
				genTitle = parsedTitle
			}
			if genTitle != "" {
				existingTitle = genTitle
				_ = ai.UpdateSessionTitle(r.Context(), s.db, req.SessionID, genTitle)
			}
		}
		citesBytes, _ := json.Marshal(cites)
		_ = ai.AppendChatMessage(r.Context(), s.db, req.SessionID, "user", req.Question, strings.Join(req.AttachedNames, ", "))
		_ = ai.AppendChatMessage(r.Context(), s.db, req.SessionID, "assistant", answer, string(citesBytes))
	}

	payload := map[string]any{
		"title":          existingTitle,
		"answer":         answer,
		"citations":      cites,
		"map_actions":    mapActions,
		"session_id":     req.SessionID,
		"session_title":  existingTitle,
		"session_pinned": existingPinned,
		"context_used":   estimatePromptTokens(systemPrompt, historyMsgs, userPrompt, answer),
		"context_max":    ctxMax,
	}
	if req.WantStream {
		emitSSEEvent(w, flusher, "done", payload)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// ---------------------------------------------------------------------------
// Chat Session CRUD & Knowledge Base Indexing
// ---------------------------------------------------------------------------

func (s *Server) handleAPIAISessions(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	sessions, err := ai.ListChatSessions(r.Context(), s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sessions": sessions})
}

func (s *Server) handleAPIAISessionGet(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	sess, msgs, err := ai.GetChatSession(r.Context(), s.db, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "session not found"})
		return
	}
	used := 0
	for _, m := range msgs {
		used += (len(m.Content) + 19) / 4
	}
	ctxMax := ai.LoadConnFromDB(r.Context(), s.db).ContextWindow(r.Context())
	if ctxMax <= 0 {
		ctxMax = 8192
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"session":      sess,
		"messages":     msgs,
		"context_used": used,
		"context_max":  ctxMax,
	})
}

func (s *Server) handleAPIAISessionPin(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	pinned := r.FormValue("pinned") == "1" || r.FormValue("pinned") == "true"
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "session id is required"})
		return
	}
	if err := ai.SetSessionPinned(r.Context(), s.db, id, pinned); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "pinned": pinned})
}

func (s *Server) handleAPIAISessionDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "session id is required"})
		return
	}
	if err := ai.DeleteChatSession(r.Context(), s.db, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleAPIAIIndex(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
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
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": errMsg})
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
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			http.Error(w, "Queue error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if wantsJSON {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": jobID, "msg": "Indexing " + filepath.Base(targetPath) + " into AI knowledge base..."})
			return
		}
		http.Redirect(w, r, sanitizeLocalRedirect(r.FormValue("redirect")), http.StatusSeeOther)
		return
	}

	conn := ai.LoadConnFromDB(r.Context(), s.db)
	if _, err := ai.IndexSource(r.Context(), s.db, conn, ext, targetPath, "", nil); err != nil {
		if wantsJSON {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		http.Error(w, "Index failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if wantsJSON {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "Indexed " + filepath.Base(targetPath) + " into AI knowledge base"})
		return
	}
	http.Redirect(w, r, "/ask#knowledge", http.StatusSeeOther)
}

func (s *Server) handleAPIAIIndexArticle(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
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
	clipPath := filepath.Join(notesDir, fmt.Sprintf("clip-%d.txt", time.Now().UnixNano()))
	_ = os.WriteFile(clipPath, []byte(payload.Title+"\n"+payload.URL+"\n\n"+payload.Text), 0644)

	n, err := ai.IndexSource(r.Context(), s.db, ai.LoadConnFromDB(r.Context(), s.db), "zim", clipPath, payload.Title, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"indexed_chunks": n})
}

func (s *Server) handleAPIAIKnowledgeRemove(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if docID := r.FormValue("id"); docID != "" {
		_ = ai.RemoveDocument(r.Context(), s.db, docID)
	}
	http.Redirect(w, r, "/ask#knowledge", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Federated Retrieval, Map Waypoints & Offline Math Evaluator
// ---------------------------------------------------------------------------
type federatedHit struct {
	SourceType string
	Title      string
	CleanTitle string
	SourcePath string
	URL        string
	Text       string
	Snippet    string
}

type chatMapAction struct {
	Title    string  `json:"title"`
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
	Category string  `json:"category"`
	Note     string  `json:"note"`
	MapURL   string  `json:"map_url"`
	Saved    bool    `json:"saved,omitempty"`
}

var (
	coordPairRe  = regexp.MustCompile(`(-?\d{1,2}\.\d{2,})\s*,\s*(-?\d{1,3}\.\d{2,})`)
	mathExprRe   = regexp.MustCompile(`(?:^|[^a-zA-Z0-9.])(\(?\s*-?\d+(?:\.\d+)?(?:\s*[+\-*/]\s*\(?\s*-?\d+(?:\.\d+)?\s*\)?)+\s*\)?)(?:$|[^a-zA-Z0-9.])`)
	unitConvRe   = regexp.MustCompile(`(?i)\b(-?\d+(?:\.\d+)?)\s*(Â°?[cf]|celsius|fahrenheit|kg|lbs?|pounds?|km|miles?|mi|ft|feet|meters?|liters?|l|gal|gallons?|oz|ml|mhz)\b`)
	dosageRe     = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*mg/kg\b.*?\b(\d+(?:\.\d+)?)\s*(kg|lbs?|pounds?)\b`)
	batteryWhRe  = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*ah\b.*?\b(\d+(?:\.\d+)?)\s*v\b`)
	wattLoadRe   = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*w(?:atts?)?\b`)
	autoIdxMu    sync.Mutex
	autoIdxLast  time.Time
)

func titleWord(w string) string {
	if w == "" {
		return ""
	}
	return strings.ToUpper(w[:1]) + w[1:]
}

// #1: On-the-Fly ZIM Title & Article Lookup across data/zim/*.zim without pre-indexing.
func (s *Server) searchLiveZIMs(query string, maxHits int) []federatedHit {
	if s.dataDir == "" || maxHits <= 0 {
		return nil
	}
	stems := s.listLocalZIMStems()
	if len(stems) == 0 {
		return nil
	}
	terms := ai.SignificantTerms(query)
	if len(terms) == 0 {
		return nil
	}

	var hits []federatedHit
	seenKey := make(map[string]bool)

	for _, stem := range stems {
		if len(hits) >= maxHits {
			break
		}
		zimPath := filepath.Join(s.dataDir, "zim", stem+".zim")
		arc, err := getZIMArchive(zimPath, stem)
		if err != nil {
			continue
		}

		// Candidate article slugs for O(log N) binary lookup (memory-safe even on huge ZIMs)
		var candidateRecs []zimTitleRecord
		tryNames := []string{strings.Join(terms, "_")}
		if len(terms) >= 2 {
			tryNames = append(tryNames, titleWord(terms[0])+"_"+strings.Join(terms[1:], "_"))
			titled := make([]string, len(terms))
			for i, t := range terms {
				titled[i] = titleWord(t)
			}
			tryNames = append(tryNames, strings.Join(titled, "_"))
		}
		for _, t := range terms {
			if len(t) >= 3 {
				tryNames = append(tryNames, titleWord(t), t)
			}
		}

		if f, err := os.Open(arc.path); err == nil {
			arc.mu.Lock()
			for _, cand := range tryNames {
				if e, ok := arc.findEntryFlex(f, cand); ok {
					if e.mimeID == 0xffff {
						e, _ = arc.followRedirects(f, e)
					}
					if e.mimeID < 0xfffd && int(e.mimeID) < len(arc.mimes) && strings.HasPrefix(strings.ToLower(arc.mimes[e.mimeID]), "text/html") {
						candidateRecs = append(candidateRecs, zimTitleRecord{ns: e.ns, url: e.url, title: e.title})
					}
				}
			}
			arc.mu.Unlock()
			_ = f.Close()
		}

		// For small/medium ZIMs (<=250k articles) or already-cached title lists, scan titles cleanly too
		arc.mu.Lock()
		canScanTitles := arc.titlesBuilt || arc.hdr.ArticleCount <= 250000
		arc.mu.Unlock()
		if len(candidateRecs) == 0 && canScanTitles {
			for _, qTry := range append([]string{strings.Join(terms, " ")}, terms...) {
				if len(qTry) < 3 {
					continue
				}
				recs := arc.searchTitles(qTry, 3)
				candidateRecs = append(candidateRecs, recs...)
				if len(candidateRecs) >= 3 {
					break
				}
			}
		}

		for _, rec := range candidateRecs {
			if len(hits) >= maxHits {
				break
			}
			subPath := fmt.Sprintf("%c/%s", rec.ns, rec.url)
			key := stem + "|" + subPath
			if seenKey[key] {
				continue
			}
			seenKey[key] = true

			title, plain, resolved, ok := s.readZIMArticlePlainText(stem, subPath, 1400)
			if !ok {
				continue
			}
			if title == "" {
				title = rec.title
			}
			hits = append(hits, federatedHit{
				SourceType: "zim",
				Title:      fmt.Sprintf("%s (%s)", title, stem),
				CleanTitle: title,
				SourcePath: zimPath + "#" + rec.url,
				URL:        resolved,
				Text:       plain,
				Snippet:    excerptForTerms(plain, terms),
			})
		}
	}
	return hits
}

// #2: Automatic Remedy Guide Lookup across internal/fieldguide.
func searchRemedyGuide(query string, maxHits int) []federatedHit {
	if maxHits <= 0 {
		return nil
	}
	terms := ai.SignificantTerms(query)
	if len(terms) == 0 {
		return nil
	}
	guide, err := fieldguide.LoadGuide()
	if err != nil || len(guide.Remedies) == 0 {
		return nil
	}

	type scored struct {
		rem   fieldguide.Remedy
		score int
	}
	var list []scored
	for _, rem := range guide.Remedies {
		hay := strings.ToLower(rem.Name + " " + strings.Join(rem.CommonNames, " ") + " " + rem.Uses + " " + rem.How + " " + strings.Join(rem.Conditions, " "))
		sc := 0
		nameLow := strings.ToLower(rem.Name + " " + strings.Join(rem.CommonNames, " "))
		for _, t := range terms {
			if strings.Contains(nameLow, t) {
				sc += 4
			} else if strings.Contains(hay, t) {
				sc++
			}
		}
		if sc > 0 {
			list = append(list, scored{rem: rem, score: sc})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].score > list[j].score })
	if len(list) > maxHits {
		list = list[:maxHits]
	}

	var hits []federatedHit
	for _, it := range list {
		r := it.rem
		u := "/field-guide?q=" + url.QueryEscape(r.Name)
		if len(r.Conditions) > 0 {
			u = "/field-guide?condition=" + url.QueryEscape(r.Conditions[0]) + "&q=" + url.QueryEscape(r.Name)
		}
		txt := fmt.Sprintf("Remedy: %s (%s)\nUses: %s\nPreparation: %s", r.Name, r.Kind, r.Uses, r.How)
		if r.Cautions != "" {
			txt += "\nCautions: " + r.Cautions
		}
		hits = append(hits, federatedHit{
			SourceType: "remedy",
			Title:      r.Name + " (Remedy)",
			CleanTitle: r.Name,
			SourcePath: "remedy:" + r.Name,
			URL:        u,
			Text:       txt,
			Snippet:    r.Uses,
		})
	}
	return hits
}

// #3: Automatic FDA Drug Label & Pill Imprint Lookup from data/druglabels/druglabels.db.
func (s *Server) searchDrugLabels(ctx context.Context, query string, maxHits int) []federatedHit {
	if s.dataDir == "" || maxHits <= 0 {
		return nil
	}
	dbPath := filepath.Join(s.dataDir, "druglabels", "druglabels.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	andQ, orQ := ai.BuildFTSQueries(query)
	if orQ == "" {
		return nil
	}

	// Open read-only briefly with tight busy_timeout to keep baseline memory minimal.
	dlDB, err := sql.Open("sqlite", dbPath+"?mode=ro&_busy_timeout=1000")
	if err != nil {
		return nil
	}
	defer dlDB.Close()

	tryMatch := andQ
	if tryMatch == "" {
		tryMatch = orQ
	}
	rows, err := dlDB.QueryContext(ctx, `
		SELECT l.set_id, COALESCE(l.brand_name,''), COALESCE(l.generic_name,''),
		       COALESCE(l.imprint,''), COALESCE(l.boxed_warning,''),
		       COALESCE(l.indications,''), COALESCE(l.dosage,''), COALESCE(l.warnings,'')
		FROM labels_fts f
		JOIN labels l ON l.rowid = f.rowid
		WHERE labels_fts MATCH ?
		LIMIT ?`, tryMatch, maxHits)
	if err != nil && andQ != "" && orQ != andQ {
		rows, err = dlDB.QueryContext(ctx, `
			SELECT l.set_id, COALESCE(l.brand_name,''), COALESCE(l.generic_name,''),
			       COALESCE(l.imprint,''), COALESCE(l.boxed_warning,''),
			       COALESCE(l.indications,''), COALESCE(l.dosage,''), COALESCE(l.warnings,'')
			FROM labels_fts f
			JOIN labels l ON l.rowid = f.rowid
			WHERE labels_fts MATCH ?
			LIMIT ?`, orQ, maxHits)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()

	trunc := func(str string, limit int) string {
		str = strings.TrimSpace(str)
		r := []rune(str)
		if len(r) > limit {
			return string(r[:limit]) + "..."
		}
		return str
	}

	var hits []federatedHit
	for rows.Next() {
		var setID, brand, generic, imprint, boxed, ind, dosage, warn string
		if err := rows.Scan(&setID, &brand, &generic, &imprint, &boxed, &ind, &dosage, &warn); err != nil {
			continue
		}
		name := strings.TrimSpace(brand)
		if name == "" {
			name = strings.TrimSpace(generic)
		}
		if name == "" {
			name = "Drug Label " + setID
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Medication: %s", name)
		if generic != "" && !strings.EqualFold(name, generic) {
			fmt.Fprintf(&b, " (Generic: %s)", trunc(generic, 100))
		}
		if imprint != "" {
			fmt.Fprintf(&b, "\nImprint/Details: %s", trunc(imprint, 160))
		}
		if boxed != "" {
			fmt.Fprintf(&b, "\nBoxed Warning: %s", trunc(boxed, 260))
		}
		if ind != "" {
			fmt.Fprintf(&b, "\nUses: %s", trunc(ind, 350))
		}
		if dosage != "" {
			fmt.Fprintf(&b, "\nDosage: %s", trunc(dosage, 350))
		}
		if warn != "" {
			fmt.Fprintf(&b, "\nWarnings: %s", trunc(warn, 300))
		}
		qLink := strings.TrimSpace(brand)
		if qLink == "" {
			qLink = strings.TrimSpace(generic)
		}
		hits = append(hits, federatedHit{
			SourceType: "druglabels",
			Title:      name + " (FDA Drug Label)",
			CleanTitle: name,
			SourcePath: "druglabel:" + setID,
			URL:        "/a/druglabels/?q=" + url.QueryEscape(qLink),
			Text:       b.String(),
			Snippet:    trunc(ind, 90),
		})
	}
	return hits
}

// #4: Lazily syncs modified files in data/notes/ and data/media/docs/ into SQLite RAG index.
// shortcut: runs at most once every 20s and caps to 5 changed files per pass to keep idle/RAM overhead zero.
func (s *Server) syncAutoIndexedDirs(ctx context.Context, conn ai.Conn) {
	if s.db == nil || s.dataDir == "" {
		return
	}
	autoIdxMu.Lock()
	if time.Since(autoIdxLast) < 20*time.Second {
		autoIdxMu.Unlock()
		return
	}
	autoIdxLast = time.Now()
	autoIdxMu.Unlock()

	_ = ai.EnsureSchema(ctx, s.db)
	type candidate struct {
		path       string
		sourceType string
		stamp      string
	}
	var dirs = []struct {
		rel   string
		sType string
	}{
		{"notes", "note"},
		{filepath.Join("media", "docs"), "pdf"},
	}

	var queue []candidate
	for _, d := range dirs {
		root := filepath.Join(s.dataDir, d.rel)
		if _, err := os.Stat(root); err != nil {
			continue
		}
		_ = filepath.WalkDir(root, func(p string, ent os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			name := ent.Name()
			if ent.IsDir() {
				if strings.HasPrefix(name, ".") || name == "attachments" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(name, ".") || strings.HasSuffix(strings.ToLower(name), ".part") {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(name))
			switch ext {
			case ".md", ".txt", ".csv", ".html", ".htm", ".json", ".yaml", ".yml":
			case ".pdf":
				if !strings.HasPrefix(d.sType, "note") {
					break
				}
			default:
				return nil
			}
			info, ierr := ent.Info()
			if ierr != nil || info.Size() == 0 || info.Size() > (16<<20) {
				return nil
			}
			stamp := fmt.Sprintf("auto:%d:%d", info.Size(), info.ModTime().Unix())
			var existingHash string
			err = s.db.QueryRowContext(ctx, `
				SELECT COALESCE((SELECT content_hash FROM rag_chunks WHERE doc_id = (SELECT id FROM rag_documents WHERE source_path = ? LIMIT 1) LIMIT 1), '')
			`, p).Scan(&existingHash)
			if err == nil && existingHash != "" {
				var savedStamp string
				_ = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, "autoidx:"+p).Scan(&savedStamp)
				if savedStamp == stamp {
					return nil
				}
			}
			queue = append(queue, candidate{path: p, sourceType: d.sType, stamp: stamp})
			if len(queue) >= 5 {
				return filepath.SkipAll
			}
			return nil
		})
	}

	for _, c := range queue {
		stType := c.sourceType
		if strings.ToLower(filepath.Ext(c.path)) == ".pdf" {
			stType = "pdf"
		}
		if _, err := ai.IndexSource(ctx, s.db, conn, stType, c.path, filepath.Base(c.path), nil); err == nil {
			_, _ = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, "autoidx:"+c.path, c.stamp)
		}
	}
}

func haversineKm(lat1, lng1, lat2, lng2 float64) float64 {
	const r = 6371.0
	rad := math.Pi / 180.0
	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * r * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// #7: Summarizes saved map_markers (with Haversine distances if query contains coordinates).
func (s *Server) queryMapWaypointsContext(query string) string {
	pins := s.listMapPins()
	if len(pins) == 0 {
		return ""
	}
	var hasRef bool
	var refLat, refLng float64
	if m := coordPairRe.FindStringSubmatch(query); len(m) == 3 {
		la, err1 := strconv.ParseFloat(m[1], 64)
		ln, err2 := strconv.ParseFloat(m[2], 64)
		if err1 == nil && err2 == nil && la >= -90 && la <= 90 && ln >= -180 && ln <= 180 {
			hasRef = true
			refLat, refLng = la, ln
		}
	}

	type pinDist struct {
		pin  MapPinView
		dist float64
	}
	list := make([]pinDist, 0, len(pins))
	for _, p := range pins {
		d := 0.0
		if hasRef {
			d = haversineKm(refLat, refLng, p.Lat, p.Lng)
		}
		list = append(list, pinDist{pin: p, dist: d})
	}
	if hasRef {
		sort.Slice(list, func(i, j int) bool { return list[i].dist < list[j].dist })
	}
	if len(list) > 10 {
		list = list[:10]
	}

	var b strings.Builder
	b.WriteString("Saved Map Waypoints (user pins):\n")
	for _, it := range list {
		p := it.pin
		fmt.Fprintf(&b, "- %s [%s] at (%.5f, %.5f)", p.Title, p.Category, p.Lat, p.Lng)
		if hasRef {
			fmt.Fprintf(&b, " â€” %.2f km (%.2f mi) away", it.dist, it.dist*0.621371)
		}
		if p.Note != "" {
			fmt.Fprintf(&b, " | Note: %s", p.Note)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func validateMapActions(raw []chatMapAction) []chatMapAction {
	var out []chatMapAction
	for _, a := range raw {
		if a.Lat < -90 || a.Lat > 90 || a.Lng < -180 || a.Lng > 180 {
			continue
		}
		if a.Lat == 0 && a.Lng == 0 && strings.TrimSpace(a.Title) == "" {
			continue
		}
		if strings.TrimSpace(a.Title) == "" {
			a.Title = fmt.Sprintf("%.4f, %.4f", a.Lat, a.Lng)
		}
		if strings.TrimSpace(a.Category) == "" {
			a.Category = "waypoint"
		}
		a.MapURL = fmt.Sprintf("/maps?lat=%.6f&lng=%.6f&zoom=12", a.Lat, a.Lng)
		out = append(out, a)
		if len(out) >= 4 {
			break
		}
	}
	return out
}

func evalArithmeticAST(expr string) (float64, bool) {
	node, err := parser.ParseExpr(expr)
	if err != nil {
		return 0, false
	}
	var walk func(ast.Expr) (float64, bool)
	walk = func(n ast.Expr) (float64, bool) {
		switch e := n.(type) {
		case *ast.ParenExpr:
			return walk(e.X)
		case *ast.BasicLit:
			if e.Kind != token.INT && e.Kind != token.FLOAT {
				return 0, false
			}
			v, err := strconv.ParseFloat(e.Value, 64)
			return v, err == nil
		case *ast.UnaryExpr:
			v, ok := walk(e.X)
			if !ok {
				return 0, false
			}
			if e.Op == token.SUB {
				return -v, true
			}
			if e.Op == token.ADD {
				return v, true
			}
			return 0, false
		case *ast.BinaryExpr:
			l, ok1 := walk(e.X)
			r, ok2 := walk(e.Y)
			if !ok1 || !ok2 {
				return 0, false
			}
			switch e.Op {
			case token.ADD:
				return l + r, true
			case token.SUB:
				return l - r, true
			case token.MUL:
				return l * r, true
			case token.QUO:
				if r == 0 {
					return 0, false
				}
				return l / r, true
			}
		}
		return 0, false
	}
	return walk(node)
}

// #10: Deterministic offline calculator for dosage, battery/solar Wh, radio wavelength, unit conversions, and arithmetic.
func evalAskMathTool(query string) string {
	var notes []string
	low := strings.ToLower(query)

	if m := dosageRe.FindStringSubmatch(query); len(m) == 4 {
		mgPerKg, _ := strconv.ParseFloat(m[1], 64)
		wt, _ := strconv.ParseFloat(m[2], 64)
		wtKg := wt
		if strings.HasPrefix(strings.ToLower(m[3]), "lb") || strings.HasPrefix(strings.ToLower(m[3]), "pound") {
			wtKg = wt * 0.45359237
		}
		notes = append(notes, fmt.Sprintf("Weight-based dosage: %.2f mg/kg Ã— %.2f kg = %.2f mg", mgPerKg, wtKg, mgPerKg*wtKg))
	}

	if m := batteryWhRe.FindStringSubmatch(query); len(m) == 3 {
		ah, _ := strconv.ParseFloat(m[1], 64)
		volts, _ := strconv.ParseFloat(m[2], 64)
		wh := ah * volts
		line := fmt.Sprintf("Battery energy: %.2f Ah Ã— %.2f V = %.2f Wh (usable ~%.2f Wh at 80%% DoD)", ah, volts, wh, wh*0.8)
		if wm := wattLoadRe.FindStringSubmatch(query); len(wm) == 2 {
			w, _ := strconv.ParseFloat(wm[1], 64)
			if w > 0 {
				line += fmt.Sprintf("; runtime at %.1f W = %.2f hours (80%% usable = %.2f hours)", w, wh/w, (wh*0.8)/w)
			}
		}
		notes = append(notes, line)
	}

	for _, m := range unitConvRe.FindAllStringSubmatch(query, 4) {
		val, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		u := strings.ToLower(strings.TrimPrefix(m[2], "Â°"))
		switch u {
		case "f", "fahrenheit":
			notes = append(notes, fmt.Sprintf("%.2f Â°F = %.2f Â°C", val, (val-32)*5.0/9.0))
		case "c", "celsius":
			notes = append(notes, fmt.Sprintf("%.2f Â°C = %.2f Â°F", val, val*9.0/5.0+32))
		case "kg":
			notes = append(notes, fmt.Sprintf("%.3f kg = %.3f lb", val, val*2.20462262))
		case "lb", "lbs", "pound", "pounds":
			notes = append(notes, fmt.Sprintf("%.3f lb = %.3f kg", val, val*0.45359237))
		case "km":
			notes = append(notes, fmt.Sprintf("%.3f km = %.3f mi", val, val*0.62137119))
		case "mi", "mile", "miles":
			notes = append(notes, fmt.Sprintf("%.3f mi = %.3f km", val, val*1.609344))
		case "ft", "feet":
			notes = append(notes, fmt.Sprintf("%.3f ft = %.3f m", val, val*0.3048))
		case "l", "liter", "liters":
			notes = append(notes, fmt.Sprintf("%.3f L = %.3f gal (US) = %.1f mL", val, val*0.264172, val*1000))
		case "gal", "gallon", "gallons":
			notes = append(notes, fmt.Sprintf("%.3f gal = %.3f L", val, val*3.78541178))
		case "mhz":
			if val > 0 {
				notes = append(notes, fmt.Sprintf("%.3f MHz full wavelength = %.3f m; half-wave dipole â‰ˆ %.3f m (%.2f ft)", val, 299.792458/val, 142.65/val, 468.0/val))
			}
		}
	}

	if !strings.Contains(low, ":") {
		if m := mathExprRe.FindStringSubmatch(query); len(m) == 2 {
			if v, ok := evalArithmeticAST(m[1]); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
				notes = append(notes, fmt.Sprintf("%s = %g", strings.TrimSpace(m[1]), v))
			}
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return "Exact Calculated Values:\n- " + strings.Join(notes, "\n- ") + "\n"
}

func excerptForTerms(text string, terms []string) string {
	lines := strings.Split(text, "\n")
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if len(ln) < 20 {
			continue
		}
		low := strings.ToLower(ln)
		for _, t := range terms {
			if strings.Contains(low, t) {
				if len([]rune(ln)) > 110 {
					return string([]rune(ln)[:110])
				}
				return ln
			}
		}
	}
	r := []rune(strings.TrimSpace(text))
	if len(r) > 100 {
		return string(r[:100])
	}
	return string(r)
}

// estimatePromptTokens uses standard ~4 chars/token approximation for context ratio UI.
// shortcut: character/4 approximation avoids shipping a BPE tokenizer in Go (~2-5% variance).
func estimatePromptTokens(systemPrompt string, history []ai.Msg, userPrompt string, assistantReply string) int {
	totalChars := len(systemPrompt) + len(userPrompt) + len(assistantReply) + 32
	for _, m := range history {
		totalChars += len(m.Content) + 16
	}
	return (totalChars + 3) / 4
}

func stripChatPunct(s string) string {
	return strings.TrimSpace(strings.Trim(s, " \t\r\n.,!?\"'`:;"))
}

// queryNeedsLocalKnowledge returns false when a query can be answered directly without querying
// local archives/databases (greetings, conversational follow-ups, creative writing, pure math).
func queryNeedsLocalKnowledge(q, scope, triageType, preZIM string, sourcesOnly, hasCalc bool) bool {
	if sourcesOnly || triageType != "" || preZIM != "" || (scope != "" && scope != "all" && scope != "attached") {
		return true
	}
	if scope == "attached" {
		return false
	}
	low := stripChatPunct(strings.ToLower(q))
	if low == "" {
		return false
	}
	for _, kw := range []string{"source", "cite", "citation", "reference", "wiki", "wikipedia", "zim", "library", "book", "guide", "remedy", "medical", "drug", "pill", "medication", "dosage", "symptom", "map", "waypoint", "pin", "coordinate", "notes", "document", "search", "lookup", "look up", "according to"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	if hasCalc {
		return false
	}
	switch low {
	case "hi", "hello", "hey", "yo", "sup", "greetings", "good morning", "good afternoon", "good evening",
		"thanks", "thank you", "thx", "ty", "ok", "okay", "cool", "nice", "great", "awesome", "got it",
		"bye", "goodbye", "see you", "how are you", "how are u", "who are you", "what are you",
		"what can you do", "help", "test", "testing", "ping":
		return false
	}
	for _, pref := range []string{"my name is ", "remember that ", "what is my name", "what's my name", "what did i ", "earlier you ", "tell me a joke", "write a ", "write an ", "draft a ", "compose a ", "rewrite ", "proofread ", "translate "} {
		if strings.HasPrefix(low, pref) || strings.Contains(low, pref) {
			return false
		}
	}
	return len(ai.SignificantTerms(q)) > 0
}
