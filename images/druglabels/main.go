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
	"time"

	_ "modernc.org/sqlite"
)

type DrugLabel struct {
	SetID          string `json:"set_id"`
	BrandName      string `json:"brand_name"`
	GenericName    string `json:"generic_name"`
	Manufacturer   string `json:"manufacturer"`
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

var db *sql.DB

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
  .container { max-width: 820px; margin: 0 auto; }
  h1 { font-size: 1.8rem; margin-bottom: 6px; }
  .quiet-disclaimer {
    color: var(--secondary);
    font-size: 0.9rem;
    margin-bottom: 20px;
  }
  input[type="text"] {
    width: 100%;
    box-sizing: border-box;
    padding: 14px 18px;
    background: var(--surface);
    border: 2px solid var(--border);
    border-radius: 8px;
    color: var(--primary);
    font-size: 1.05rem;
    outline: none;
    margin-bottom: 24px;
  }
  input[type="text"]:focus {
    border-color: var(--accent);
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
  <h1>FDA Drug Labels</h1>
  <div class="quiet-disclaimer">Label text from FDA. Not medical advice.</div>

  <form method="GET" action="">
    <input type="text" name="q" value="{{.Query}}" placeholder="Search by name (ibuprofen) or situation (fever child, burn)..." autofocus>
  </form>

  {{if .Query}}
    <div style="margin-bottom: 16px; color: var(--secondary);">Showing {{len .Results}} result(s) for "{{.Query}}"</div>
  {{end}}

  {{range .Results}}
  <div class="card">
    <h2>{{if .BrandName}}{{.BrandName}}{{else}}{{.GenericName}}{{end}}</h2>
    <div class="meta">
      {{if .GenericName}}<div><span class="badge">Generic</span>{{.GenericName}}</div>{{end}}
      {{if .Manufacturer}}<div><span class="badge">Manufacturer</span>{{.Manufacturer}}</div>{{end}}
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
      <p style="color: var(--secondary);">No matching drug labels found. Try searching for generic names or symptoms.</p>
    {{else}}
      <p style="color: var(--secondary);">Enter a medication name or symptom above to search FDA approved drug labels.</p>
    {{end}}
  {{end}}
</div>
</body>
</html>`

type PageData struct {
	Query   string
	Results []DrugLabel
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
		boxed_warning TEXT,
		indications TEXT,
		dosage TEXT,
		warnings TEXT,
		do_not_use TEXT,
		stop_use TEXT,
		active_ingredient TEXT,
		inactive_ingredient TEXT,
		storage TEXT
	);
	CREATE VIRTUAL TABLE IF NOT EXISTS labels_fts USING fts5(
		set_id UNINDEXED,
		brand_name,
		generic_name,
		indications,
		warnings,
		content='labels',
		content_rowid='rowid'
	);
	CREATE TRIGGER IF NOT EXISTS labels_ai AFTER INSERT ON labels BEGIN
		INSERT INTO labels_fts(rowid, brand_name, generic_name, indications, warnings)
		VALUES (new.rowid, new.brand_name, new.generic_name, new.indications, new.warnings);
	END;
	CREATE TRIGGER IF NOT EXISTS labels_ad AFTER DELETE ON labels BEGIN
		INSERT INTO labels_fts(labels_fts, rowid, brand_name, generic_name, indications, warnings)
		VALUES ('delete', old.rowid, old.brand_name, old.generic_name, old.indications, old.warnings);
	END;`
	_, err := targetDB.Exec(schema)
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
					} `json:"openfda"`
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
			stmt, _ := tx.Prepare(`INSERT OR REPLACE INTO labels (set_id, brand_name, generic_name, manufacturer, boxed_warning, indications, dosage, warnings, do_not_use, stop_use, active_ingredient, inactive_ingredient, storage) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
			for _, rec := range payload.Results {
				if rec.SetID == "" {
					continue
				}
				_, _ = stmt.Exec(
					rec.SetID,
					firstStr(rec.OpenFDA.BrandName),
					firstStr(rec.OpenFDA.GenericName),
					firstStr(rec.OpenFDA.ManufacturerName),
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

func searchLabels(query string) []DrugLabel {
	if db == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	cleanQ := strings.TrimSpace(query)

	var rows *sql.Rows
	var err error

	ftsQuery := `
		SELECT l.set_id, l.brand_name, l.generic_name, l.manufacturer,
		       l.boxed_warning, l.indications, l.dosage, l.warnings,
		       l.do_not_use, l.stop_use, l.active_ingredient, l.inactive_ingredient, l.storage
		FROM labels_fts f
		JOIN labels l ON f.rowid = l.rowid
		WHERE labels_fts MATCH ?
		ORDER BY bm25(labels_fts)
		LIMIT 25`
	rows, err = db.Query(ftsQuery, cleanQ+"*")
	if err != nil {
		likeQ := "%" + cleanQ + "%"
		likeQuery := `
			SELECT set_id, brand_name, generic_name, manufacturer,
			       boxed_warning, indications, dosage, warnings,
			       do_not_use, stop_use, active_ingredient, inactive_ingredient, storage
			FROM labels
			WHERE brand_name LIKE ? OR generic_name LIKE ? OR indications LIKE ?
			LIMIT 25`
		rows, err = db.Query(likeQuery, likeQ, likeQ, likeQ)
		if err != nil {
			log.Printf("Search query failed: %v", err)
			return nil
		}
	}
	defer rows.Close()

	var results []DrugLabel
	for rows.Next() {
		var item DrugLabel
		if err := rows.Scan(
			&item.SetID, &item.BrandName, &item.GenericName, &item.Manufacturer,
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
		res := searchLabels(q)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("q")
		res := searchLabels(q)
		data := PageData{
			Query:   q,
			Results: res,
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
