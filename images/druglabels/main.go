package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type DrugLabel struct {
	SetID          string `json:"set_id"`
	BrandName      string `json:"brand_name"`
	GenericName    string `json:"generic_name"`
	Manufacturer   string `json:"manufacturer"`
	NDC            string `json:"ndc"`
	Imprint        string `json:"imprint"`
	BoxedWarning   string `json:"boxed_warning"`
	Indications    string `json:"indications_and_usage"`
	Dosage         string `json:"dosage_and_administration"`
	Warnings       string `json:"warnings"`
	DoNotUse       string `json:"do_not_use"`
	StopUse        string `json:"stop_use"`
	ActiveIngred   string `json:"active_ingredient"`
	InactiveIngred string `json:"inactive_ingredient"`
	Storage        string `json:"storage_and_handling"`
}

type Situation struct {
	Label string
	Query string
}

var commonSituations = []Situation{
	{"Fever", "fever"},
	{"Fever (Child)", "fever child"},
	{"Pain / Ache", "pain"},
	{"Headache / Migraine", "headache"},
	{"Burn / Sunburn", "burn"},
	{"Cut / Wound / Antiseptic", "wound OR cut OR antiseptic"},
	{"Allergy / Hives", "allergy"},
	{"Rash / Itching", "rash OR itch"},
	{"Cough", "cough"},
	{"Cold / Congestion", "congestion OR cold"},
	{"Sore Throat", "sore throat"},
	{"Nausea / Vomiting", "nausea OR vomiting"},
	{"Diarrhea", "diarrhea"},
	{"Constipation", "constipation"},
	{"Heartburn / Acid Reflux", "heartburn OR acid"},
	{"Asthma / Breathing", "asthma OR breathing"},
	{"Bacterial Infection", "infection"},
	{"Eye Irritation", "eye"},
	{"Earache", "ear"},
	{"Insect Bite / Sting", "bite OR sting"},
	{"Sprain / Swelling", "swelling OR inflammation"},
	{"Sleep / Insomnia", "sleep OR insomnia"},
}

var (
	db         *sql.DB
	importMu   sync.Mutex
	isImporting bool
	importErr  string
)

const htmlIndex = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>FDA Drug Labels — Ark</title>
<style>
  :root {
    --bg: #F4F6F9;
    --surface: #FFFFFF;
    --primary: #0F172A;
    --secondary: #475569;
    --accent: #0369A1;
    --danger: #B91C1C;
    --border: #CBD5E1;
  }
  body {
    background: var(--bg);
    color: var(--primary);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    margin: 0;
    padding: 24px;
    font-size: 18px;
    line-height: 1.6;
  }
  .container { max-width: 860px; margin: 0 auto; }
  .topbar { display: flex; justify-content: space-between; align-items: baseline; flex-wrap: wrap; gap: 12px; }
  h1 { font-size: 1.8rem; margin: 0 0 4px 0; }
  .quiet-disclaimer {
    color: var(--secondary);
    font-size: 0.9rem;
    margin-bottom: 18px;
  }
  .mode-tabs {
    display: flex;
    gap: 8px;
    margin-bottom: 12px;
    flex-wrap: wrap;
  }
  .mode-tab {
    padding: 6px 14px;
    border-radius: 6px;
    border: 1px solid var(--border);
    background: var(--surface);
    color: var(--secondary);
    text-decoration: none;
    font-size: 0.9rem;
    font-weight: 600;
  }
  .mode-tab.active {
    background: var(--accent);
    border-color: var(--accent);
    color: #FFFFFF;
  }
  .search-row {
    display: flex;
    gap: 8px;
    margin-bottom: 18px;
  }
  input[type="text"] {
    flex: 1;
    box-sizing: border-box;
    padding: 14px 18px;
    background: var(--surface);
    border: 2px solid var(--border);
    border-radius: 8px;
    color: var(--primary);
    font-size: 1.05rem;
    outline: none;
  }
  input[type="text"]:focus {
    border-color: var(--accent);
  }
  .btn {
    padding: 12px 20px;
    border-radius: 8px;
    border: 1px solid var(--border);
    background: var(--accent);
    color: #FFFFFF;
    font-size: 1rem;
    font-weight: 600;
    cursor: pointer;
  }
  .btn-secondary {
    background: var(--surface);
    color: var(--primary);
  }
  .card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 20px;
    margin-bottom: 20px;
    box-shadow: 0 2px 6px rgba(15, 23, 42, 0.05);
  }
  .card h2 { margin-top: 0; font-size: 1.35rem; color: var(--accent); }
  .card .meta { color: var(--secondary); font-size: 0.92rem; margin-bottom: 12px; }
  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    margin-top: 10px;
  }
  .chip {
    display: inline-block;
    padding: 6px 12px;
    border-radius: 999px;
    background: var(--bg);
    border: 1px solid var(--border);
    color: var(--primary);
    text-decoration: none;
    font-size: 0.85rem;
    font-weight: 500;
  }
  .chip:hover {
    border-color: var(--accent);
    color: var(--accent);
  }
  .boxed-warning {
    background: #FEF2F2;
    border-left: 4px solid var(--danger);
    padding: 14px 16px;
    margin-bottom: 16px;
    color: #7F1D1D;
    border-radius: 4px;
  }
  .boxed-warning strong { color: var(--danger); display: block; margin-bottom: 6px; }
  details { margin-bottom: 10px; }
  summary {
    cursor: pointer;
    font-weight: 600;
    color: var(--primary);
    padding: 6px 0;
  }
  summary:hover { color: var(--accent); }
  .section-content {
    padding: 8px 0 8px 16px;
    color: #1E293B;
    white-space: pre-wrap;
  }
  .badge {
    display: inline-block;
    padding: 2px 8px;
    border-radius: 4px;
    font-size: 0.8rem;
    background: #E2E8F0;
    color: var(--secondary);
    margin-right: 6px;
    font-weight: 600;
  }
</style>
</head>
<body>
<div class="container">
  <div class="topbar">
    <h1>FDA Drug Labels</h1>
    <span class="badge">{{.LabelCount}} labels offline</span>
  </div>
  <div class="quiet-disclaimer">Label text from FDA. Not medical advice.</div>

  {{if eq .LabelCount 0}}
  <div class="card" style="border-color: var(--accent);">
    <h2 style="margin-bottom: 8px;">Offline Drug Database Empty</h2>
    <p style="margin-top: 0; color: var(--secondary); font-size: 0.95rem;">
      Download the public-domain openFDA drug label dataset into your local storage so name, situation, and pill ID/NDC searches work offline.
    </p>
    {{if .Importing}}
      <div class="badge" style="background: #DBEAFE; color: #1E40AF;">Importing openFDA labels in background... Refresh in a minute.</div>
    {{else}}
      <form method="POST" action="api/import" style="margin: 0;">
        <button type="submit" class="btn">Download FDA Drug Labels</button>
      </form>
    {{end}}
    {{if .ImportError}}
      <p style="color: var(--danger); font-size: 0.9rem; margin-bottom: 0;">Last import error: {{.ImportError}}</p>
    {{end}}
  </div>
  {{end}}

  <div class="mode-tabs">
    <a href="?{{if .Query}}q={{.Query}}{{end}}" class="mode-tab {{if eq .Mode ""}}active{{end}}">All (Name / Situation / Pill ID)</a>
    <a href="?mode=situation{{if .Query}}&q={{.Query}}{{end}}" class="mode-tab {{if eq .Mode "situation"}}active{{end}}">By Situation</a>
    <a href="?mode=pill{{if .Query}}&q={{.Query}}{{end}}" class="mode-tab {{if eq .Mode "pill"}}active{{end}}">By Pill ID / Imprint / NDC</a>
  </div>

  <form method="GET" action="" class="search-row">
    {{if .Mode}}<input type="hidden" name="mode" value="{{.Mode}}">{{end}}
    <input type="text" name="q" value="{{.Query}}" placeholder="{{if eq .Mode "pill"}}Enter pill imprint (e.g. L484, I-2, M365) or NDC (0573-0169)...{{else if eq .Mode "situation"}}Enter symptom or situation (e.g. fever child, burn, allergy)...{{else}}Search by drug name (ibuprofen), situation (fever child), or pill imprint / NDC...{{end}}" autofocus>
    <button type="submit" class="btn">Search</button>
  </form>

  {{if or (eq .Query "") (eq .Mode "situation")}}
  <div class="card">
    <strong style="font-size: 0.95rem;">Common Situations &amp; Symptoms</strong>
    <div class="chips">
      {{range .Situations}}
        <a class="chip" href="?mode=situation&q={{.Query}}">{{.Label}}</a>
      {{end}}
    </div>
  </div>
  {{end}}

  {{if .Query}}
    <div style="margin-bottom: 16px; color: var(--secondary);">Showing {{len .Results}} result(s) for "{{.Query}}"</div>
  {{end}}

  {{range .Results}}
  <div class="card">
    <h2>{{if .BrandName}}{{.BrandName}}{{else}}{{.GenericName}}{{end}}</h2>
    <div class="meta">
      {{if .GenericName}}<div><span class="badge">Generic</span>{{.GenericName}}</div>{{end}}
      {{if .Manufacturer}}<div><span class="badge">Manufacturer</span>{{.Manufacturer}}</div>{{end}}
      {{if .NDC}}<div><span class="badge">NDC</span>{{.NDC}}</div>{{end}}
      {{if .Imprint}}<div><span class="badge">Pill ID / Description</span>{{.Imprint}}</div>{{end}}
    </div>

    {{if .BoxedWarning}}
    <div class="boxed-warning">
      <strong>BOXED WARNING</strong>
      {{.BoxedWarning}}
    </div>
    {{end}}

    {{if .Indications}}
    <details open>
      <summary>Uses / Indications</summary>
      <div class="section-content">{{.Indications}}</div>
    </details>
    {{end}}

    {{if .Dosage}}
    <details open>
      <summary>Dosage &amp; Administration</summary>
      <div class="section-content">{{.Dosage}}</div>
    </details>
    {{end}}

    {{if .DoNotUse}}
    <details open>
      <summary>Do Not Use &amp; Warnings</summary>
      <div class="section-content">{{.DoNotUse}}</div>
    </details>
    {{end}}

    {{if .Warnings}}
    <details open>
      <summary>Warnings &amp; Precautions</summary>
      <div class="section-content">{{.Warnings}}</div>
    </details>
    {{end}}

    {{if .StopUse}}
    <details>
      <summary>Stop Use</summary>
      <div class="section-content">{{.StopUse}}</div>
    </details>
    {{end}}

    {{if .ActiveIngred}}
    <details>
      <summary>Active Ingredients</summary>
      <div class="section-content">{{.ActiveIngred}}</div>
    </details>
    {{end}}

    {{if .Storage}}
    <details>
      <summary>Storage &amp; Handling</summary>
      <div class="section-content">{{.Storage}}</div>
    </details>
    {{end}}
  </div>
  {{else}}
    {{if .Query}}
      <p style="color: var(--secondary);">No matching drug labels found. Try another medication name, situation chip, or pill imprint / NDC.</p>
    {{end}}
  {{end}}
</div>
</body>
</html>`

type PageData struct {
	Query       string
	Mode        string
	LabelCount  int
	Importing   bool
	ImportError string
	Situations  []Situation
	Results     []DrugLabel
}

func dbFilePath() string {
	dbPath := os.Getenv("DRUGLABELS_DB")
	if dbPath == "" {
		dbPath = "/data/druglabels.db"
	}
	return dbPath
}

func ensureSchema(targetDB *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
	CREATE TABLE IF NOT EXISTS labels (
		set_id TEXT PRIMARY KEY,
		brand_name TEXT,
		generic_name TEXT,
		manufacturer TEXT,
		ndc TEXT DEFAULT '',
		imprint TEXT DEFAULT '',
		boxed_warning TEXT,
		indications TEXT,
		dosage TEXT,
		warnings TEXT,
		do_not_use TEXT,
		stop_use TEXT,
		active_ingredient TEXT,
		inactive_ingredient TEXT,
		storage TEXT
	);`
	if _, err := targetDB.Exec(schema); err != nil {
		return err
	}
	// Migrate older DBs safely if ndc/imprint columns were absent
	_, _ = targetDB.Exec(`ALTER TABLE labels ADD COLUMN ndc TEXT DEFAULT ''`)
	_, _ = targetDB.Exec(`ALTER TABLE labels ADD COLUMN imprint TEXT DEFAULT ''`)

	ftsSchema := `
	CREATE VIRTUAL TABLE IF NOT EXISTS labels_fts USING fts5(
		set_id UNINDEXED,
		brand_name,
		generic_name,
		ndc,
		imprint,
		indications,
		warnings,
		content='labels',
		content_rowid='rowid'
	);
	CREATE TRIGGER IF NOT EXISTS labels_ai AFTER INSERT ON labels BEGIN
		INSERT INTO labels_fts(rowid, brand_name, generic_name, ndc, imprint, indications, warnings)
		VALUES (new.rowid, new.brand_name, new.generic_name, new.ndc, new.imprint, new.indications, new.warnings);
	END;
	CREATE TRIGGER IF NOT EXISTS labels_ad AFTER DELETE ON labels BEGIN
		INSERT INTO labels_fts(labels_fts, rowid, brand_name, generic_name, ndc, imprint, indications, warnings)
		VALUES ('delete', old.rowid, old.brand_name, old.generic_name, old.ndc, old.imprint, old.indications, old.warnings);
	END;`
	_, err := targetDB.Exec(ftsSchema)
	return err
}

func initDB() {
	dbPath := dbFilePath()
	_ = os.MkdirAll(filepath.Dir(dbPath), 0755)

	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		log.Printf("Warning: cannot open %s: %v", dbPath, err)
		return
	}
	_ = ensureSchema(db)
}

func firstStr(arr []string) string {
	return strings.TrimSpace(strings.Join(arr, "\n"))
}

func joinUnique(parts ...[]string) string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range parts {
		for _, item := range list {
			item = strings.TrimSpace(item)
			if item != "" && !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		}
	}
	res := strings.Join(out, " | ")
	if len(res) > 500 {
		res = res[:500]
	}
	return res
}

// importOpenFDA streams FDA drug label partitions from api.fda.gov/download.json into a temp SQLite DB and swaps atomically.
func importOpenFDA(ctx context.Context, manifestURL string) error {
	if manifestURL == "" {
		manifestURL = "https://api.fda.gov/download.json"
	}
	hc := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var manifest struct {
		Results struct {
			Drug struct {
				Label struct {
					ExportDate string `json:"export_date"`
					Partitions []struct {
						File string `json:"file"`
					} `json:"partitions"`
				} `json:"label"`
			} `json:"drug"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return err
	}

	destPath := dbFilePath()
	tmpPath := destPath + ".tmp"
	_ = os.Remove(tmpPath)
	tmpDB, err := sql.Open("sqlite", tmpPath)
	if err != nil {
		return err
	}
	if err := ensureSchema(tmpDB); err != nil {
		tmpDB.Close()
		return err
	}

	for _, part := range manifest.Results.Drug.Label.Partitions {
		if ctx.Err() != nil {
			tmpDB.Close()
			return ctx.Err()
		}
		pReq, err := http.NewRequestWithContext(ctx, http.MethodGet, part.File, nil)
		if err != nil {
			continue
		}
		pResp, err := hc.Do(pReq)
		if err != nil {
			continue
		}
		zipBytes, err := io.ReadAll(pResp.Body)
		pResp.Body.Close()
		if err != nil {
			continue
		}
		zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
		if err != nil {
			continue
		}
		for _, zf := range zr.File {
			rc, err := zf.Open()
			if err != nil {
				continue
			}
			var payload struct {
				Results []struct {
					SetID   string `json:"set_id"`
					OpenFDA struct {
						BrandName        []string `json:"brand_name"`
						GenericName      []string `json:"generic_name"`
						ManufacturerName []string `json:"manufacturer_name"`
						ProductNDC       []string `json:"product_ndc"`
						PackageNDC       []string `json:"package_ndc"`
					} `json:"openfda"`
					SPLProductData []string `json:"spl_product_data_elements"`
					Description    []string `json:"description"`
					HowSupplied    []string `json:"how_supplied"`
					BoxedWarning   []string `json:"boxed_warning"`
					Indications    []string `json:"indications_and_usage"`
					Dosage         []string `json:"dosage_and_administration"`
					Warnings       []string `json:"warnings"`
					DoNotUse       []string `json:"do_not_use"`
					StopUse        []string `json:"stop_use"`
					ActiveIngred   []string `json:"active_ingredient"`
					InactiveIngred []string `json:"inactive_ingredient"`
					Storage        []string `json:"storage_and_handling"`
				} `json:"results"`
			}
			_ = json.NewDecoder(rc).Decode(&payload)
			rc.Close()

			tx, _ := tmpDB.Begin()
			stmt, _ := tx.Prepare(`INSERT OR REPLACE INTO labels (set_id, brand_name, generic_name, manufacturer, ndc, imprint, boxed_warning, indications, dosage, warnings, do_not_use, stop_use, active_ingredient, inactive_ingredient, storage) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
			for _, rec := range payload.Results {
				if rec.SetID == "" {
					continue
				}
				ndc := joinUnique(rec.OpenFDA.ProductNDC, rec.OpenFDA.PackageNDC)
				imprint := joinUnique(rec.SPLProductData, rec.Description, rec.HowSupplied)
				_, _ = stmt.Exec(
					rec.SetID,
					firstStr(rec.OpenFDA.BrandName),
					firstStr(rec.OpenFDA.GenericName),
					firstStr(rec.OpenFDA.ManufacturerName),
					ndc,
					imprint,
					firstStr(rec.BoxedWarning),
					firstStr(rec.Indications),
					firstStr(rec.Dosage),
					firstStr(rec.Warnings),
					firstStr(rec.DoNotUse),
					firstStr(rec.StopUse),
					firstStr(rec.ActiveIngred),
					firstStr(rec.InactiveIngred),
					firstStr(rec.Storage),
				)
			}
			stmt.Close()
			_ = tx.Commit()
		}
	}

	_, _ = tmpDB.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('export_date', ?)`, manifest.Results.Drug.Label.ExportDate)
	tmpDB.Close()
	if db != nil {
		db.Close()
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return err
	}
	initDB()
	return nil
}

func countLabels() int {
	if db == nil {
		return 0
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM labels`).Scan(&n)
	return n
}

func searchLabels(query, mode string) []DrugLabel {
	if db == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	cleanQ := strings.TrimSpace(query)

	var rows *sql.Rows
	var err error

	if mode == "pill" {
		likeQ := "%" + cleanQ + "%"
		rows, err = db.Query(`
			SELECT set_id, brand_name, generic_name, manufacturer, COALESCE(ndc,''), COALESCE(imprint,''),
			       boxed_warning, indications, dosage, warnings,
			       do_not_use, stop_use, active_ingredient, inactive_ingredient, storage
			FROM labels
			WHERE ndc LIKE ? OR imprint LIKE ? OR brand_name LIKE ?
			LIMIT 25`, likeQ, likeQ, likeQ)
	} else {
		ftsExpr := cleanQ
		if !strings.Contains(cleanQ, " OR ") && !strings.HasSuffix(cleanQ, "*") {
			ftsExpr = cleanQ + "*"
		}
		ftsQuery := `
			SELECT l.set_id, l.brand_name, l.generic_name, l.manufacturer, COALESCE(l.ndc,''), COALESCE(l.imprint,''),
			       l.boxed_warning, l.indications, l.dosage, l.warnings,
			       l.do_not_use, l.stop_use, l.active_ingredient, l.inactive_ingredient, l.storage
			FROM labels_fts f
			JOIN labels l ON f.rowid = l.rowid
			WHERE labels_fts MATCH ?
			ORDER BY bm25(labels_fts)
			LIMIT 25`
		rows, err = db.Query(ftsQuery, ftsExpr)
		if err != nil {
			likeQ := "%" + cleanQ + "%"
			likeQuery := `
				SELECT set_id, brand_name, generic_name, manufacturer, COALESCE(ndc,''), COALESCE(imprint,''),
				       boxed_warning, indications, dosage, warnings,
				       do_not_use, stop_use, active_ingredient, inactive_ingredient, storage
				FROM labels
				WHERE brand_name LIKE ? OR generic_name LIKE ? OR indications LIKE ? OR ndc LIKE ? OR imprint LIKE ?
				LIMIT 25`
			rows, err = db.Query(likeQuery, likeQ, likeQ, likeQ, likeQ, likeQ)
		}
	}
	if err != nil {
		log.Printf("Search query failed: %v", err)
		return nil
	}
	defer rows.Close()

	var results []DrugLabel
	for rows.Next() {
		var item DrugLabel
		if err := rows.Scan(
			&item.SetID, &item.BrandName, &item.GenericName, &item.Manufacturer, &item.NDC, &item.Imprint,
			&item.BoxedWarning, &item.Indications, &item.Dosage, &item.Warnings,
			&item.DoNotUse, &item.StopUse, &item.ActiveIngred, &item.InactiveIngred, &item.Storage,
		); err == nil {
			results = append(results, item)
		}
	}
	return results
}

func main() {
	doImport := flag.Bool("import", false, "Download and rebuild FDA drug labels database from openFDA")
	flag.Parse()

	initDB()

	if *doImport {
		if err := importOpenFDA(context.Background(), ""); err != nil {
			log.Fatalf("openFDA import failed: %v", err)
		}
		log.Println("openFDA drug labels import complete.")
		return
	}

	tmpl := template.Must(template.New("index").Parse(htmlIndex))

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	http.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		mode := r.URL.Query().Get("mode")
		res := searchLabels(q, mode)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})

	http.HandleFunc("/api/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		importMu.Lock()
		if !isImporting {
			isImporting = true
			importErr = ""
			go func() {
				err := importOpenFDA(context.Background(), "")
				importMu.Lock()
				isImporting = false
				if err != nil {
					importErr = err.Error()
				}
				importMu.Unlock()
			}()
		}
		importMu.Unlock()
		http.Redirect(w, r, "../", http.StatusSeeOther)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("q")
		mode := r.URL.Query().Get("mode")
		res := searchLabels(q, mode)

		importMu.Lock()
		importing := isImporting
		lastErr := importErr
		importMu.Unlock()

		data := PageData{
			Query:       q,
			Mode:        mode,
			LabelCount:  countLabels(),
			Importing:   importing,
			ImportError: lastErr,
			Situations:  commonSituations,
			Results:     res,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.Execute(w, data)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("FDA Drug Labels serving on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
