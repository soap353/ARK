// Content & viewer handlers: Kiwix catalog, disk library scan & icons, Field Guide,
// Offline Maps viewer/extractor/pins, Tools, LAN Drop, and local OPDS feeds.
package web

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"html/template"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"ark/internal/fieldguide"
	"ark/internal/jobs"
	"ark/internal/kiwix"
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
	s.render(w, r, "maps.html", data)
}
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
	s.render(w, r, "kiwix.html", data)
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
	s.render(w, r, "field_guide.html", data)
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
	for _, rec := range recs {
		hits = append(hits, suggestHit{
			Title: rec.title,
			URL:   buildZIMArticleHref(stem, rec.ns, rec.url),
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
	for _, rec := range recs {
		pages = append(pages, pageHit{
			Title: rec.title,
			URL:   buildZIMArticleHref(stem, rec.ns, rec.url),
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
			if !isLocalRequest(r) {
				if wantsJSON {
					writeJSONError(w, http.StatusForbidden, "forbidden: deleting pins is only allowed from the local host")
					return
				}
				http.Error(w, "Forbidden: deleting pins is only allowed from the local host.", http.StatusForbidden)
				return
			}
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
		if title == "" || err1 != nil || err2 != nil || math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lng) || math.IsInf(lng, 0) || lat < -90 || lat > 90 || lng < -180 || lng > 180 {
			if wantsJSON {
				writeJSONError(w, http.StatusBadRequest, "Valid title, latitude (-90..90), and longitude (-180..180) are required.")
				return
			}
			http.Redirect(w, r, "/maps?err="+url.QueryEscape("Valid title, latitude (-90..90), and longitude (-180..180) are required."), http.StatusSeeOther)
			return
		}
		if len(title) > 120 {
			title = title[:120]
		}
		cat := strings.TrimSpace(r.FormValue("category"))
		if cat == "" {
			cat = "waypoint"
		}
		if len(cat) > 32 {
			cat = cat[:32]
		}
		note := strings.TrimSpace(r.FormValue("note"))
		if len(note) > 1000 {
			note = note[:1000]
		}
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
	scheme := "http"
	if s.isSettingEnabled("https_enabled", false) {
		scheme = "https"
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return []string{scheme + "://localhost:" + port}
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
		u := fmt.Sprintf("%s://%s:%s", scheme, v4.String(), port)
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		out = append(out, scheme+"://localhost:"+port)
	}
	return out
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
	return normalizeClientIP(host)
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
			d.Label = rec.Device + " Â· " + rec.Addr
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
			storedName := randomHex(8) + "_" + fileName
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

	remoteHost := clientHostFromReq(r)
	id := randomHex(8)
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
		Title:   dev + " â€” OPDS Catalog",
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
		Title:   s.opdsDeviceName() + " â€” E-Books & PDFs",
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
				Title:   fmt.Sprintf("%s â€” %s", rec.title, stem),
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
		Title:   s.opdsDeviceName() + " â€” All Books & Documents",
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
		Title:   s.opdsDeviceName() + " â€” ZIM Libraries",
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
					Title:   fmt.Sprintf("%s â€” %s", rec.title, stem),
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
		Title:   fmt.Sprintf("Search: %s â€” %s", q, s.opdsDeviceName()),
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
