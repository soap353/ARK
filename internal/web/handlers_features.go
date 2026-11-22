package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Server) ensureLANDropTable() {
	if s.db == nil {
		return
	}
	_, _ = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS lan_posts (
			id TEXT PRIMARY KEY,
			author TEXT NOT NULL DEFAULT 'Guest',
			category TEXT NOT NULL DEFAULT 'Note',
			message TEXT NOT NULL DEFAULT '',
			file_name TEXT NOT NULL DEFAULT '',
			file_rel TEXT NOT NULL DEFAULT '',
			file_size INTEGER NOT NULL DEFAULT 0,
			remote_addr TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_lan_posts_created ON lan_posts(created_at DESC);
	`)
}

func (s *Server) listLANShareBaseURLs() []string {
	settings := s.fetchSettings()
	port := strings.TrimSpace(settings["port"])
	if port == "" {
		port = "8080"
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return []string{"http://localhost:" + port}
	}
	seen := make(map[string]bool)
	var out []string
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP == nil || ipNet.IP.IsLoopback() {
			continue
		}
		v4 := ipNet.IP.To4()
		if v4 == nil {
			continue
		}
		// Skip link-local 169.254.x.x
		if v4[0] == 169 && v4[1] == 254 {
			continue
		}
		u := fmt.Sprintf("http://%s:%s", v4.String(), port)
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		out = append(out, "http://localhost:"+port)
	}
	return out
}

func newShortHexID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func sanitizeLANDropFilename(raw string) string {
	base := filepath.Base(strings.TrimSpace(raw))
	if base == "" || base == "." || base == ".." {
		return "upload.bin"
	}
	var sb strings.Builder
	for _, r := range base {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' || r == ' ':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	res := strings.TrimSpace(sb.String())
	if res == "" || res == "." || res == ".." {
		return "upload.bin"
	}
	return res
}

// ---------------------------------------------------------------------------
// Feature #35: Offline Field & Reference Calculators (/tools)
// ---------------------------------------------------------------------------

func (s *Server) handleToolsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	data := PageData{
		Title:        "Field Calculators",
		ActiveNav:    "tools",
		SessionToken: s.sessionStart.Format(time.RFC3339),
	}
	s.render(w, r, "tools.html", data)
}

// ---------------------------------------------------------------------------
// Feature #41: Local Bulletin Board / LAN Drop (/lan-drop)
// ---------------------------------------------------------------------------

func (s *Server) fetchLANDropPosts(limit int) []LANDropPostView {
	if s.db == nil {
		return nil
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.Query(`
		SELECT id, author, category, message, file_name, file_rel, file_size, remote_addr, created_at
		FROM lan_posts
		ORDER BY created_at DESC, rowid DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var posts []LANDropPostView
	for rows.Next() {
		var p LANDropPostView
		var fileRel string
		var fileSize int64
		if err := rows.Scan(&p.ID, &p.Author, &p.Category, &p.Message, &p.FileName, &fileRel, &fileSize, &p.RemoteAddr, &p.CreatedAt); err != nil {
			continue
		}
		if fileRel != "" {
			p.FileURL = "/api/lan-drop/file/" + url.PathEscape(p.ID)
			p.FileSize = formatHumanBytes(fileSize)
		}
		posts = append(posts, p)
	}
	return posts
}

type lanClientRecord struct {
	Addr     string
	Device   string
	LastSeen time.Time
}

func clientHostFromReq(r *http.Request) string {
	if r == nil {
		return ""
	}
	host := strings.TrimSpace(r.RemoteAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host
}

func detectDeviceKind(ua string) string {
	low := strings.ToLower(ua)
	switch {
	case strings.Contains(low, "iphone"):
		return "iPhone"
	case strings.Contains(low, "ipad"):
		return "iPad"
	case strings.Contains(low, "android"):
		return "Android"
	case strings.Contains(low, "windows"):
		return "Windows"
	case strings.Contains(low, "macintosh") || strings.Contains(low, "mac os"):
		return "Mac"
	case strings.Contains(low, "linux"):
		return "Linux"
	default:
		return "Device"
	}
}

func isLoopbackAddr(addr string) bool {
	return addr == "" || addr == "127.0.0.1" || addr == "::1" || addr == "localhost"
}

func (s *Server) trackLANClient(r *http.Request) {
	addr := clientHostFromReq(r)
	if addr == "" {
		return
	}
	s.lanClients.Store(addr, lanClientRecord{
		Addr:     addr,
		Device:   detectDeviceKind(r.UserAgent()),
		LastSeen: time.Now(),
	})
}

func (s *Server) buildConnectedDevices(posts []LANDropPostView) []LANDropDeviceView {
	byAddr := make(map[string]*LANDropDeviceView)
	ensure := func(addr string) *LANDropDeviceView {
		if d, ok := byAddr[addr]; ok {
			return d
		}
		remote := !isLoopbackAddr(addr)
		lbl := addr
		if !remote {
			lbl = "Host (" + addr + ")"
		}
		d := &LANDropDeviceView{
			Addr:     addr,
			Label:    lbl,
			IsRemote: remote,
		}
		byAddr[addr] = d
		return d
	}

	now := time.Now()
	s.lanClients.Range(func(_, val any) bool {
		rec, ok := val.(lanClientRecord)
		if !ok || now.Sub(rec.LastSeen) > 30*time.Minute {
			return true
		}
		d := ensure(rec.Addr)
		d.Online = now.Sub(rec.LastSeen) <= 5*time.Minute
		d.LastSeen = rec.LastSeen.Format("15:04:05")
		if d.IsRemote && rec.Device != "" {
			d.Label = rec.Device + " · " + rec.Addr
		} else if !d.IsRemote {
			d.Label = "Host (" + rec.Device + ")"
		}
		return true
	})

	for _, p := range posts {
		addr := strings.TrimSpace(p.RemoteAddr)
		if addr == "" {
			continue
		}
		d := ensure(addr)
		d.PostCount++
		if d.IsRemote && p.Author != "" && p.Author != "Guest" && !strings.Contains(d.Label, p.Author) {
			d.Label = p.Author + " (" + addr + ")"
		}
	}

	out := make([]LANDropDeviceView, 0, len(byAddr))
	for _, d := range byAddr {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsRemote != out[j].IsRemote {
			return out[i].IsRemote
		}
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		if out[i].PostCount != out[j].PostCount {
			return out[i].PostCount > out[j].PostCount
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

func (s *Server) handleLANDropPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	s.trackLANClient(r)
	enabled := s.isSettingEnabled("lan_drop_enabled", true)
	var posts []LANDropPostView
	var devices []LANDropDeviceView
	if enabled {
		posts = s.fetchLANDropPosts(200)
		devices = s.buildConnectedDevices(posts)
	}
	var shareURLs []string
	for _, base := range s.listLANShareBaseURLs() {
		shareURLs = append(shareURLs, base+"/lan-drop")
	}
	data := PageData{
		Title:            "LAN Drop & Bulletin Board",
		ActiveNav:        "lan-drop",
		LANDropEnabled:   enabled,
		LANDropPosts:     posts,
		ConnectedDevices: devices,
		LANShareURLs:     shareURLs,
		Settings:         s.fetchSettings(),
		FlashMsg:         strings.TrimSpace(r.URL.Query().Get("msg")),
		FlashErr:         strings.TrimSpace(r.URL.Query().Get("err")),
	}
	s.render(w, r, "lan_drop.html", data)
}

func (s *Server) handleAPILANDropPosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	s.trackLANClient(r)
	if !s.isSettingEnabled("lan_drop_enabled", true) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "enabled": false, "error": "LAN Drop is disabled in Settings."})
		return
	}
	allPosts := s.fetchLANDropPosts(200)
	devices := s.buildConnectedDevices(allPosts)
	filterDev := strings.TrimSpace(r.URL.Query().Get("device"))
	posts := allPosts
	if filterDev == "remote" {
		posts = make([]LANDropPostView, 0)
		for _, p := range allPosts {
			if !isLoopbackAddr(strings.TrimSpace(p.RemoteAddr)) {
				posts = append(posts, p)
			}
		}
	} else if filterDev != "" {
		posts = make([]LANDropPostView, 0)
		for _, p := range allPosts {
			if strings.TrimSpace(p.RemoteAddr) == filterDev {
				posts = append(posts, p)
			}
		}
	}
	if posts == nil {
		posts = []LANDropPostView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": true, "posts": posts, "devices": devices})
}

func (s *Server) handleAPILANDropPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	wantJSON := strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json") || r.URL.Query().Get("ajax") == "1"
	if !s.isSettingEnabled("lan_drop_enabled", true) {
		if wantJSON {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "LAN Drop is disabled in Settings."})
		} else {
			http.Error(w, "LAN Drop is disabled in Settings.", http.StatusForbidden)
		}
		return
	}
	if s.db == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	s.trackLANClient(r)
	s.ensureLANDropTable()

	const maxUploadBytes = 256 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)

	ct := strings.ToLower(r.Header.Get("Content-Type"))
	var author, category, message string
	var fileName, fileRel string
	var fileSize int64

	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			if wantJSON {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid upload: " + err.Error()})
			} else {
				http.Redirect(w, r, "/lan-drop?err="+url.QueryEscape("Invalid upload: "+err.Error()), http.StatusSeeOther)
			}
			return
		}
		author = r.FormValue("author")
		category = r.FormValue("category")
		message = r.FormValue("message")

		if fh, hdr, ferr := r.FormFile("file"); ferr == nil && hdr != nil {
			defer fh.Close()
			if s.dataDir == "" {
				http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
				return
			}
			dropDir := filepath.Join(s.dataDir, "landrop")
			if mkErr := os.MkdirAll(dropDir, 0o755); mkErr != nil {
				http.Error(w, "failed to prepare drop dir", http.StatusInternalServerError)
				return
			}
			fileName = sanitizeLANDropFilename(hdr.Filename)
			storedName := newShortHexID() + "_" + fileName
			destPath := filepath.Join(dropDir, storedName)
			dst, cerr := os.Create(destPath)
			if cerr != nil {
				http.Error(w, "failed to save uploaded file", http.StatusInternalServerError)
				return
			}
			nWritten, cpyErr := io.Copy(dst, fh)
			_ = dst.Close()
			if cpyErr != nil {
				_ = os.Remove(destPath)
				http.Error(w, "failed to write uploaded file", http.StatusInternalServerError)
				return
			}
			fileRel = storedName
			fileSize = nWritten
		}
	} else {
		_ = r.ParseForm()
		author = r.FormValue("author")
		category = r.FormValue("category")
		message = r.FormValue("message")
	}

	author = strings.TrimSpace(author)
	if author == "" {
		author = "Guest"
	}
	if len(author) > 64 {
		author = author[:64]
	}
	category = strings.TrimSpace(category)
	switch strings.ToLower(category) {
	case "alert":
		category = "Alert"
	case "question":
		category = "Question"
	case "file":
		category = "File"
	default:
		if fileRel != "" && strings.TrimSpace(category) == "" {
			category = "File"
		} else {
			category = "Note"
		}
	}
	message = strings.TrimSpace(message)
	if len(message) > 10000 {
		message = message[:10000]
	}
	if message == "" && fileRel == "" {
		if wantJSON {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Enter a message or attach a file."})
		} else {
			http.Redirect(w, r, "/lan-drop?err="+url.QueryEscape("Enter a message or attach a file."), http.StatusSeeOther)
		}
		return
	}

	remoteHost := r.RemoteAddr
	if h, _, splitErr := net.SplitHostPort(remoteHost); splitErr == nil {
		remoteHost = h
	}
	id := newShortHexID()
	nowStr := time.Now().Format("2006-01-02 15:04:05")
	if _, err := s.db.Exec(`
		INSERT INTO lan_posts (id, author, category, message, file_name, file_rel, file_size, remote_addr, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, author, category, message, fileName, fileRel, fileSize, remoteHost, nowStr); err != nil {
		http.Error(w, "failed to save post: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if wantJSON {
		post := LANDropPostView{
			ID:         id,
			Author:     author,
			Category:   category,
			Message:    message,
			FileName:   fileName,
			RemoteAddr: remoteHost,
			CreatedAt:  nowStr,
		}
		if fileRel != "" {
			post.FileURL = "/api/lan-drop/file/" + url.PathEscape(id)
			post.FileSize = formatHumanBytes(fileSize)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "post": post})
		return
	}
	http.Redirect(w, r, "/lan-drop?msg="+url.QueryEscape("Posted to LAN board."), http.StatusSeeOther)
}

func (s *Server) handleAPILANDropDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.isSettingEnabled("lan_drop_enabled", true) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "LAN Drop is disabled in Settings."})
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" || s.db == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "missing post id"})
		return
	}
	var fileRel string
	_ = s.db.QueryRow(`SELECT file_rel FROM lan_posts WHERE id = ?`, id).Scan(&fileRel)
	_, _ = s.db.Exec(`DELETE FROM lan_posts WHERE id = ?`, id)
	if fileRel != "" && s.dataDir != "" {
		safeBase := filepath.Base(fileRel)
		if safeBase != "" && safeBase != "." && safeBase != ".." {
			_ = os.Remove(filepath.Join(s.dataDir, "landrop", safeBase))
		}
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json") || r.URL.Query().Get("ajax") == "1" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	http.Redirect(w, r, "/lan-drop", http.StatusSeeOther)
}

func (s *Server) handleAPILANDropFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.isSettingEnabled("lan_drop_enabled", true) {
		http.Error(w, "LAN Drop is disabled in Settings.", http.StatusForbidden)
		return
	}
	id := filepath.Base(strings.TrimPrefix(r.URL.Path, "/api/lan-drop/file/"))
	if id == "" || id == "." || id == ".." || s.db == nil || s.dataDir == "" {
		http.NotFound(w, r)
		return
	}
	var fileName, fileRel string
	if err := s.db.QueryRow(`SELECT file_name, file_rel FROM lan_posts WHERE id = ?`, id).Scan(&fileName, &fileRel); err != nil || fileRel == "" {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join(s.dataDir, "landrop", filepath.Base(fileRel))
	if _, err := os.Stat(fullPath); err != nil {
		http.NotFound(w, r)
		return
	}
	dispName := sanitizeLANDropFilename(fileName)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(dispName, `"`, "")))
	http.ServeFile(w, r, fullPath)
}

// ---------------------------------------------------------------------------
// Feature #42: OPDS 1.2 Catalog Feed (/opds)
// ---------------------------------------------------------------------------

type opdsFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	XmlnsO  string      `xml:"xmlns:opds,attr"`
	XmlnsDC string      `xml:"xmlns:dc,attr"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Author  *opdsAuthor `xml:"author,omitempty"`
	Links   []opdsLink  `xml:"link"`
	Entries []opdsEntry `xml:"entry"`
}

type opdsAuthor struct {
	Name string `xml:"name"`
}

type opdsLink struct {
	Rel    string `xml:"rel,attr"`
	Href   string `xml:"href,attr"`
	Type   string `xml:"type,attr,omitempty"`
	Title  string `xml:"title,attr,omitempty"`
	Length int64  `xml:"length,attr,omitempty"`
}

type opdsEntry struct {
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Author  *opdsAuthor `xml:"author,omitempty"`
	Summary string      `xml:"summary,omitempty"`
	Links   []opdsLink  `xml:"link"`
}

func writeOPDSXML(w http.ResponseWriter, kind string, feed opdsFeed) {
	w.Header().Set("Content-Type", fmt.Sprintf("application/atom+xml;profile=opds-catalog;kind=%s;charset=utf-8", kind))
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(feed)
}

func (s *Server) requireOPDSEnabled(w http.ResponseWriter) bool {
	if !s.isSettingEnabled("opds_enabled", true) {
		http.Error(w, "OPDS Catalog Feed is disabled. Enable it in Settings -> General.", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) opdsDeviceName() string {
	settings := s.fetchSettings()
	name := strings.TrimSpace(settings["device_name"])
	if name == "" {
		return "Ark"
	}
	return name
}

func opdsMIMEForExt(name string) (mime string, supported bool) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".epub":
		return "application/epub+zip", true
	case ".pdf":
		return "application/pdf", true
	case ".mobi":
		return "application/x-mobipocket-ebook", true
	case ".azw3":
		return "application/vnd.amazon.ebook", true
	case ".cbz":
		return "application/vnd.comicbook+zip", true
	case ".cbr":
		return "application/vnd.comicbook-rar", true
	case ".txt":
		return "text/plain; charset=utf-8", true
	case ".md":
		return "text/markdown; charset=utf-8", true
	case ".html", ".htm":
		return "text/html", true
	default:
		return "", false
	}
}

type opdsMediaBook struct {
	Title   string
	SubDir  string
	RelName string
	Size    int64
	ModTime time.Time
	MIME    string
}

func (s *Server) scanOPDSMediaBooks(filter string) []opdsMediaBook {
	if s.dataDir == "" {
		return nil
	}
	lowF := strings.ToLower(strings.TrimSpace(filter))
	var out []opdsMediaBook
	for _, sub := range []string{"docs", "media"} {
		dir := filepath.Join(s.dataDir, sub)
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			base := d.Name()
			if strings.HasPrefix(base, ".") || strings.HasSuffix(strings.ToLower(base), ".part") {
				return nil
			}
			mime, ok := opdsMIMEForExt(base)
			if !ok {
				return nil
			}
			rel, rerr := filepath.Rel(dir, p)
			if rerr != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			title := strings.TrimSuffix(base, filepath.Ext(base))
			title = strings.ReplaceAll(title, "_", " ")
			if lowF != "" && !strings.Contains(strings.ToLower(title+" "+relSlash), lowF) {
				return nil
			}
			info, ierr := d.Info()
			if ierr != nil {
				return nil
			}
			out = append(out, opdsMediaBook{
				Title:   title,
				SubDir:  sub,
				RelName: relSlash,
				Size:    info.Size(),
				ModTime: info.ModTime(),
				MIME:    mime,
			})
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title)
	})
	return out
}

func (s *Server) handleOPDSRouter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireOPDSEnabled(w) {
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/opds")
	trimmed = strings.TrimPrefix(trimmed, "/")

	switch {
	case trimmed == "":
		s.serveOPDSRoot(w)
	case trimmed == "opensearch.xml":
		s.serveOPDSOpenSearch(w)
	case trimmed == "all":
		s.serveOPDSAllFeed(w)
	case trimmed == "media":
		s.serveOPDSMediaFeed(w, "")
	case trimmed == "zim":
		s.serveOPDSZIMCatalogFeed(w)
	case strings.HasPrefix(trimmed, "zim/"):
		stem := strings.TrimPrefix(trimmed, "zim/")
		stem = filepath.Base(stem)
		s.serveOPDSZIMPagesFeed(w, r, stem)
	case trimmed == "search":
		s.serveOPDSSearchFeed(w, r)
	case strings.HasPrefix(trimmed, "files/"):
		s.serveOPDSFile(w, r, strings.TrimPrefix(trimmed, "files/"))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) baseOPDSLinks(selfHref string) []opdsLink {
	return []opdsLink{
		{Rel: "self", Href: selfHref, Type: "application/atom+xml;profile=opds-catalog"},
		{Rel: "start", Href: "/opds", Type: "application/atom+xml;profile=opds-catalog;kind=navigation"},
		{Rel: "search", Href: "/opds/opensearch.xml", Type: "application/opensearchdescription+xml", Title: "Search Ark Library"},
	}
}

func (s *Server) serveOPDSRoot(w http.ResponseWriter) {
	now := time.Now().UTC().Format(time.RFC3339)
	dev := s.opdsDeviceName()
	mediaCount := len(s.scanOPDSMediaBooks(""))
	zimCount := len(s.listLocalZIMStems())

	feed := opdsFeed{
		XmlnsO:  "http://opds-spec.org/2010/catalog",
		XmlnsDC: "http://purl.org/dc/terms/",
		ID:      "urn:ark:opds:root",
		Title:   dev + " — OPDS Catalog",
		Updated: now,
		Author:  &opdsAuthor{Name: dev + " Almanac"},
		Links:   s.baseOPDSLinks("/opds"),
		Entries: []opdsEntry{
			{
				ID:      "urn:ark:opds:all",
				Title:   "All E-Books & Documents",
				Updated: now,
				Summary: fmt.Sprintf("Browse all local EPUB/PDF documents (%d) and ZIM archives (%d).", mediaCount, zimCount),
				Links:   []opdsLink{{Rel: "subsection", Href: "/opds/all", Type: "application/atom+xml;profile=opds-catalog;kind=acquisition"}},
			},
			{
				ID:      "urn:ark:opds:media",
				Title:   fmt.Sprintf("E-Books & PDFs (%d)", mediaCount),
				Updated: now,
				Summary: "Direct EPUB, PDF, CBZ, and document downloads stored in Ark.",
				Links:   []opdsLink{{Rel: "subsection", Href: "/opds/media", Type: "application/atom+xml;profile=opds-catalog;kind=acquisition"}},
			},
			{
				ID:      "urn:ark:opds:zim",
				Title:   fmt.Sprintf("ZIM Archives & Encyclopedias (%d)", zimCount),
				Updated: now,
				Summary: "Browse pages and articles from installed ZIM archives (Wikipedia, Gutenberg, Wikibooks, docs).",
				Links:   []opdsLink{{Rel: "subsection", Href: "/opds/zim", Type: "application/atom+xml;profile=opds-catalog;kind=navigation"}},
			},
		},
	}
	writeOPDSXML(w, "navigation", feed)
}

func (s *Server) serveOPDSOpenSearch(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/opensearchdescription+xml;charset=utf-8")
	dev := html.EscapeString(s.opdsDeviceName())
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
  <ShortName>%s Library</ShortName>
  <Description>Search books and ZIM articles hosted on %s</Description>
  <Url type="application/atom+xml;profile=opds-catalog;kind=acquisition" template="/opds/search?q={searchTerms}"/>
</OpenSearchDescription>`, dev, dev)
}

func mediaBookToOPDSEntry(b opdsMediaBook) opdsEntry {
	parts := strings.Split(b.RelName, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	href := "/opds/files/" + url.PathEscape(b.SubDir) + "/" + strings.Join(parts, "/")
	return opdsEntry{
		ID:      "urn:ark:file:" + b.SubDir + ":" + b.RelName,
		Title:   b.Title,
		Updated: b.ModTime.UTC().Format(time.RFC3339),
		Summary: fmt.Sprintf("%s (%s)", filepath.Ext(b.RelName), formatHumanBytes(b.Size)),
		Links: []opdsLink{
			{
				Rel:    "http://opds-spec.org/acquisition",
				Href:   href,
				Type:   b.MIME,
				Length: b.Size,
			},
		},
	}
}

func (s *Server) serveOPDSMediaFeed(w http.ResponseWriter, filter string) {
	books := s.scanOPDSMediaBooks(filter)
	entries := make([]opdsEntry, 0, len(books))
	for _, b := range books {
		entries = append(entries, mediaBookToOPDSEntry(b))
	}
	feed := opdsFeed{
		XmlnsO:  "http://opds-spec.org/2010/catalog",
		XmlnsDC: "http://purl.org/dc/terms/",
		ID:      "urn:ark:opds:media",
		Title:   s.opdsDeviceName() + " — E-Books & PDFs",
		Updated: time.Now().UTC().Format(time.RFC3339),
		Links:   s.baseOPDSLinks("/opds/media"),
		Entries: entries,
	}
	writeOPDSXML(w, "acquisition", feed)
}

func (s *Server) serveOPDSAllFeed(w http.ResponseWriter) {
	books := s.scanOPDSMediaBooks("")
	entries := make([]opdsEntry, 0, len(books)+32)
	for _, b := range books {
		entries = append(entries, mediaBookToOPDSEntry(b))
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, stem := range s.listLocalZIMStems() {
		zimPath := filepath.Join(s.dataDir, "zim", stem+".zim")
		arc, err := getZIMArchive(zimPath, stem)
		if err != nil {
			continue
		}
		recs, _, _ := arc.listPages(0, 15, "")
		for _, rec := range recs {
			if strings.EqualFold(rec.title, "Main Page") {
				continue
			}
			artURL := buildZIMArticleHref(stem, rec.ns, rec.url)
			entries = append(entries, opdsEntry{
				ID:      "urn:ark:zim:" + stem + ":" + rec.url,
				Title:   fmt.Sprintf("%s — %s", rec.title, stem),
				Updated: now,
				Summary: "HTML article / book from " + stem,
				Links: []opdsLink{
					{Rel: "http://opds-spec.org/acquisition", Href: artURL + "?download=1", Type: "text/html"},
					{Rel: "alternate", Href: artURL, Type: "text/html"},
				},
			})
		}
	}
	feed := opdsFeed{
		XmlnsO:  "http://opds-spec.org/2010/catalog",
		XmlnsDC: "http://purl.org/dc/terms/",
		ID:      "urn:ark:opds:all",
		Title:   s.opdsDeviceName() + " — All Books & Documents",
		Updated: now,
		Links:   s.baseOPDSLinks("/opds/all"),
		Entries: entries,
	}
	writeOPDSXML(w, "acquisition", feed)
}

func (s *Server) serveOPDSZIMCatalogFeed(w http.ResponseWriter) {
	now := time.Now().UTC().Format(time.RFC3339)
	stems := s.listLocalZIMStems()
	entries := make([]opdsEntry, 0, len(stems))
	for _, stem := range stems {
		zimPath := filepath.Join(s.dataDir, "zim", stem+".zim")
		title := strings.ReplaceAll(stem, "_", " ")
		summary := "Local ZIM archive"
		if arc, err := getZIMArchive(zimPath, stem); err == nil {
			if mTitle, mDesc := arc.metadata(); mTitle != "" {
				title = mTitle
				if mDesc != "" {
					summary = mDesc
				}
			}
		}
		escStem := url.PathEscape(stem)
		entries = append(entries, opdsEntry{
			ID:      "urn:ark:zim:" + stem,
			Title:   title,
			Updated: now,
			Summary: summary,
			Links: []opdsLink{
				{Rel: "subsection", Href: "/opds/zim/" + escStem, Type: "application/atom+xml;profile=opds-catalog;kind=acquisition"},
				{Rel: "http://opds-spec.org/image/thumbnail", Href: "/api/library/icon?file=" + url.QueryEscape(stem+".zim") + "&cat=ZIM", Type: "image/png"},
			},
		})
	}
	feed := opdsFeed{
		XmlnsO:  "http://opds-spec.org/2010/catalog",
		XmlnsDC: "http://purl.org/dc/terms/",
		ID:      "urn:ark:opds:zim",
		Title:   s.opdsDeviceName() + " — ZIM Libraries",
		Updated: now,
		Links:   s.baseOPDSLinks("/opds/zim"),
		Entries: entries,
	}
	writeOPDSXML(w, "navigation", feed)
}

func (s *Server) serveOPDSZIMPagesFeed(w http.ResponseWriter, r *http.Request, stem string) {
	if s.dataDir == "" || stem == "" || stem == "." || stem == ".." {
		http.NotFound(w, r)
		return
	}
	zimPath := filepath.Join(s.dataDir, "zim", stem+".zim")
	arc, err := getZIMArchive(zimPath, stem)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, convErr := strconv.Atoi(raw); convErr == nil && n > 0 {
			offset = n
		}
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	const pageSize = 50
	recs, _, hasMore := arc.listPages(offset, pageSize, q)
	now := time.Now().UTC().Format(time.RFC3339)

	entries := make([]opdsEntry, 0, len(recs))
	for _, rec := range recs {
		artURL := buildZIMArticleHref(stem, rec.ns, rec.url)
		entries = append(entries, opdsEntry{
			ID:      "urn:ark:zim:" + stem + ":" + rec.url,
			Title:   rec.title,
			Updated: now,
			Links: []opdsLink{
				{Rel: "http://opds-spec.org/acquisition", Href: artURL + "?download=1", Type: "text/html"},
				{Rel: "alternate", Href: artURL, Type: "text/html"},
			},
		})
	}
	escStem := url.PathEscape(stem)
	selfURL := fmt.Sprintf("/opds/zim/%s?offset=%d", escStem, offset)
	if q != "" {
		selfURL += "&q=" + url.QueryEscape(q)
	}
	links := s.baseOPDSLinks(selfURL)
	if hasMore {
		nextURL := fmt.Sprintf("/opds/zim/%s?offset=%d", escStem, offset+len(recs))
		if q != "" {
			nextURL += "&q=" + url.QueryEscape(q)
		}
		links = append(links, opdsLink{Rel: "next", Href: nextURL, Type: "application/atom+xml;profile=opds-catalog;kind=acquisition"})
	}
	if offset > 0 {
		prevOff := offset - pageSize
		if prevOff < 0 {
			prevOff = 0
		}
		prevURL := fmt.Sprintf("/opds/zim/%s?offset=%d", escStem, prevOff)
		if q != "" {
			prevURL += "&q=" + url.QueryEscape(q)
		}
		links = append(links, opdsLink{Rel: "previous", Href: prevURL, Type: "application/atom+xml;profile=opds-catalog;kind=acquisition"})
	}

	displayTitle, _ := arc.metadata()
	if displayTitle == "" {
		displayTitle = strings.ReplaceAll(stem, "_", " ")
	}
	feed := opdsFeed{
		XmlnsO:  "http://opds-spec.org/2010/catalog",
		XmlnsDC: "http://purl.org/dc/terms/",
		ID:      "urn:ark:opds:zim:" + stem,
		Title:   displayTitle,
		Updated: now,
		Links:   links,
		Entries: entries,
	}
	writeOPDSXML(w, "acquisition", feed)
}

func (s *Server) serveOPDSSearchFeed(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	now := time.Now().UTC().Format(time.RFC3339)
	var entries []opdsEntry
	for _, b := range s.scanOPDSMediaBooks(q) {
		entries = append(entries, mediaBookToOPDSEntry(b))
	}
	if q != "" {
		for _, stem := range s.listLocalZIMStems() {
			zimPath := filepath.Join(s.dataDir, "zim", stem+".zim")
			arc, err := getZIMArchive(zimPath, stem)
			if err != nil {
				continue
			}
			for _, rec := range arc.searchTitles(q, 15) {
				artURL := buildZIMArticleHref(stem, rec.ns, rec.url)
				entries = append(entries, opdsEntry{
					ID:      "urn:ark:zim:" + stem + ":" + rec.url,
					Title:   fmt.Sprintf("%s — %s", rec.title, stem),
					Updated: now,
					Links: []opdsLink{
						{Rel: "http://opds-spec.org/acquisition", Href: artURL + "?download=1", Type: "text/html"},
						{Rel: "alternate", Href: artURL, Type: "text/html"},
					},
				})
			}
		}
	}
	feed := opdsFeed{
		XmlnsO:  "http://opds-spec.org/2010/catalog",
		XmlnsDC: "http://purl.org/dc/terms/",
		ID:      "urn:ark:opds:search:" + url.QueryEscape(q),
		Title:   fmt.Sprintf("Search: %s — %s", q, s.opdsDeviceName()),
		Updated: now,
		Links:   s.baseOPDSLinks("/opds/search?q=" + url.QueryEscape(q)),
		Entries: entries,
	}
	writeOPDSXML(w, "acquisition", feed)
}

func (s *Server) serveOPDSFile(w http.ResponseWriter, r *http.Request, relPath string) {
	if s.dataDir == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(relPath, "/"), "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	sub := parts[0]
	if sub != "media" && sub != "docs" {
		http.NotFound(w, r)
		return
	}
	cleanRel := filepath.Clean(filepath.FromSlash(parts[1]))
	if cleanRel == "." || strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
		http.NotFound(w, r)
		return
	}
	baseDir := filepath.Join(s.dataDir, sub)
	fullPath := filepath.Join(baseDir, cleanRel)
	if !strings.HasPrefix(fullPath, baseDir+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	if _, ok := opdsMIMEForExt(fullPath); !ok {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, fullPath)
}
