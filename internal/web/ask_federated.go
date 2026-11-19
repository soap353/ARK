package web

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
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
)

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
	unitConvRe   = regexp.MustCompile(`(?i)\b(-?\d+(?:\.\d+)?)\s*(°?[cf]|celsius|fahrenheit|kg|lbs?|pounds?|km|miles?|mi|ft|feet|meters?|liters?|l|gal|gallons?|oz|ml|mhz)\b`)
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
	qdrant := ai.NewQdrantClient("")
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
		if _, err := ai.IndexSource(ctx, s.db, conn, qdrant, ai.DefaultCollection, stType, c.path, filepath.Base(c.path), nil); err == nil {
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
			fmt.Fprintf(&b, " — %.2f km (%.2f mi) away", it.dist, it.dist*0.621371)
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
		notes = append(notes, fmt.Sprintf("Weight-based dosage: %.2f mg/kg × %.2f kg = %.2f mg", mgPerKg, wtKg, mgPerKg*wtKg))
	}

	if m := batteryWhRe.FindStringSubmatch(query); len(m) == 3 {
		ah, _ := strconv.ParseFloat(m[1], 64)
		volts, _ := strconv.ParseFloat(m[2], 64)
		wh := ah * volts
		line := fmt.Sprintf("Battery energy: %.2f Ah × %.2f V = %.2f Wh (usable ~%.2f Wh at 80%% DoD)", ah, volts, wh, wh*0.8)
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
		u := strings.ToLower(strings.TrimPrefix(m[2], "°"))
		switch u {
		case "f", "fahrenheit":
			notes = append(notes, fmt.Sprintf("%.2f °F = %.2f °C", val, (val-32)*5.0/9.0))
		case "c", "celsius":
			notes = append(notes, fmt.Sprintf("%.2f °C = %.2f °F", val, val*9.0/5.0+32))
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
				notes = append(notes, fmt.Sprintf("%.3f MHz full wavelength = %.3f m; half-wave dipole ≈ %.3f m (%.2f ft)", val, 299.792458/val, 142.65/val, 468.0/val))
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
