package main

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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
    --bg: #0B0D10;
    --surface: #15181D;
    --primary: #F4F4F0;
    --secondary: #80868B;
    --accent: #FFB000;
    --danger: #E5484D;
    --border: #2A2F37;
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
  .container { max-width: 800px; margin: 0 auto; }
  h1 { font-size: 1.8rem; margin-bottom: 8px; }
  .quiet-disclaimer {
    color: var(--secondary);
    font-size: 0.9rem;
    margin-bottom: 24px;
  }
  input[type="text"] {
    width: 100%;
    box-sizing: border-box;
    padding: 14px 18px;
    background: var(--surface);
    border: 2px solid var(--border);
    border-radius: 8px;
    color: var(--primary);
    font-size: 1.1rem;
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
  }
  .card h2 { margin-top: 0; font-size: 1.4rem; color: var(--accent); }
  .card .meta { color: var(--secondary); font-size: 0.95rem; margin-bottom: 12px; }
  .boxed-warning {
    background: rgba(229, 72, 77, 0.15);
    border-left: 4px solid var(--danger);
    padding: 14px 16px;
    margin-bottom: 16px;
    color: #FFA4A8;
    border-radius: 4px;
  }
  .boxed-warning strong { color: var(--danger); display: block; margin-bottom: 6px; }
  details { margin-bottom: 12px; }
  summary {
    cursor: pointer;
    font-weight: 600;
    color: var(--primary);
    padding: 6px 0;
  }
  summary:hover { color: var(--accent); }
  .section-content {
    padding: 8px 0 8px 16px;
    color: #D6D8DC;
    white-space: pre-wrap;
  }
  .badge {
    display: inline-block;
    padding: 2px 8px;
    border-radius: 4px;
    font-size: 0.8rem;
    background: var(--border);
    color: var(--secondary);
    margin-right: 6px;
  }
</style>
</head>
<body>
<div class="container">
  <h1>FDA Drug Labels</h1>
  <div class="quiet-disclaimer">Label text from FDA. Not medical advice.</div>

  <form method="GET" action="/">
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
      <summary>Dosage & Administration</summary>
      <div class="section-content">{{.Dosage}}</div>
    </details>
    {{end}}

    {{if .DoNotUse}}
    <details open>
      <summary>Do Not Use & Warnings</summary>
      <div class="section-content">{{.DoNotUse}}</div>
    </details>
    {{end}}

    {{if .Warnings}}
    <details open>
      <summary>Warnings & Precautions</summary>
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
      <summary>Storage & Handling</summary>
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

func initDB() {
	dbPath := os.Getenv("DRUGLABELS_DB")
	if dbPath == "" {
		dbPath = "/data/druglabels.db"
	}
	_ = os.MkdirAll(filepath.Dir(dbPath), 0755)

	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		log.Printf("Warning: cannot open %s: %v", dbPath, err)
		return
	}

	// Create tables if they do not exist
	schema := `
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
	);`
	_, _ = db.Exec(schema)
}

func searchLabels(query string) []DrugLabel {
	if db == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	cleanQ := strings.TrimSpace(query)

	var rows *sql.Rows
	var err error

	// Try FTS first
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
		// Fallback to LIKE query
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
	initDB()

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
