package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"html"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

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
	Key    string
	Label  string
	Query  string
	Active bool
}

var commonSituations = []Situation{
	{Key: "fever", Label: "Fever", Query: "fever"},
	{Key: "fever-child", Label: "Fever (Child)", Query: "fever child"},
	{Key: "pain", Label: "Pain / Ache", Query: "pain"},
	{Key: "headache", Label: "Headache / Migraine", Query: "headache"},
	{Key: "burn", Label: "Burn / Sunburn", Query: "burn"},
	{Key: "wound", Label: "Cut / Wound / Antiseptic", Query: "wound OR cut OR antiseptic"},
	{Key: "allergy", Label: "Allergy / Hives", Query: "allergy"},
	{Key: "rash", Label: "Rash / Itching", Query: "rash OR itch"},
	{Key: "cough", Label: "Cough", Query: "cough"},
	{Key: "cold", Label: "Cold / Congestion", Query: "congestion OR cold"},
	{Key: "sore-throat", Label: "Sore Throat", Query: "sore throat"},
	{Key: "nausea", Label: "Nausea / Vomiting", Query: "nausea OR vomiting"},
	{Key: "diarrhea", Label: "Diarrhea", Query: "diarrhea"},
	{Key: "constipation", Label: "Constipation", Query: "constipation"},
	{Key: "heartburn", Label: "Heartburn / Acid Reflux", Query: "heartburn OR acid"},
	{Key: "asthma", Label: "Asthma / Breathing", Query: "asthma OR breathing"},
	{Key: "infection", Label: "Bacterial Infection", Query: "infection"},
	{Key: "eye", Label: "Eye Irritation", Query: "eye"},
	{Key: "ear", Label: "Earache", Query: "ear"},
	{Key: "bite", Label: "Insect Bite / Sting", Query: "bite OR sting"},
	{Key: "swelling", Label: "Sprain / Swelling", Query: "swelling OR inflammation"},
	{Key: "sleep", Label: "Sleep / Insomnia", Query: "sleep OR insomnia"},
}

var (
	importMu    sync.Mutex
	isImporting bool
	importErr   string
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
  * { box-sizing: border-box; }
  body {
    background: var(--bg);
    color: var(--primary);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    margin: 0;
    padding: 24px;
    font-size: 17px;
    line-height: 1.6;
  }
  .container { max-width: 860px; margin: 0 auto; }
  .topbar { display: flex; justify-content: space-between; align-items: baseline; flex-wrap: wrap; gap: 12px; }
  h1 { font-size: 1.8rem; margin: 0 0 4px 0; }
  .quiet-disclaimer {
    color: var(--secondary);
    font-size: 0.9rem;
    margin-bottom: 16px;
  }
  .search-row {
    display: flex;
    gap: 8px;
    margin-bottom: 10px;
  }
  .toolbar-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
    margin: 0 0 14px 0;
  }
  .search-hint {
    color: var(--secondary);
    font-size: 0.85rem;
    margin: 0;
  }
  .sort-wrap {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: 0.88rem;
    color: var(--secondary);
    font-weight: 600;
  }
  select.sort-select {
    padding: 6px 10px;
    border-radius: 6px;
    border: 1px solid var(--border);
    background: var(--surface);
    color: var(--primary);
    font-family: inherit;
    font-size: 0.88rem;
    font-weight: 500;
    cursor: pointer;
  }
  input[type="text"] {
    flex: 1;
    min-width: 0;
    padding: 13px 16px;
    background: var(--surface);
    border: 2px solid var(--border);
    border-radius: 8px;
    color: var(--primary);
    font-size: 1rem;
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
    white-space: nowrap;
  }
  .card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 18px 20px;
    margin-bottom: 16px;
    box-shadow: 0 2px 6px rgba(15, 23, 42, 0.05);
  }
  .situation-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .clear-filters {
    background: none;
    border: none;
    color: var(--accent);
    font-size: 0.85rem;
    font-weight: 600;
    cursor: pointer;
    padding: 2px 6px;
    text-decoration: underline;
  }
  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    margin-top: 10px;
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 6px 13px;
    border-radius: 999px;
    background: var(--bg);
    border: 1px solid var(--border);
    color: var(--primary);
    font-family: inherit;
    font-size: 0.85rem;
    font-weight: 500;
    cursor: pointer;
    transition: background 0.12s, border-color 0.12s, color 0.12s;
  }
  .chip:hover {
    border-color: var(--accent);
    color: var(--accent);
  }
  .chip.active {
    background: var(--accent);
    border-color: var(--accent);
    color: #FFFFFF;
    font-weight: 600;
  }
  .chip.active::before {
    content: "\2713";
    font-size: 0.8rem;
  }
  .result-card {
    padding: 16px 20px;
    margin-bottom: 12px;
  }
  .result-title {
    margin: 0 0 2px 0;
    font-size: 1.25rem;
    color: var(--accent);
    line-height: 1.3;
  }
  .result-mfr {
    color: var(--secondary);
    font-size: 0.92rem;
    margin-bottom: 8px;
  }
  .result-body {
    margin-top: 4px;
    border-top: 1px solid #E2E8F0;
    padding-top: 8px;
  }
  .result-body > summary {
    cursor: pointer;
    font-size: 0.9rem;
    font-weight: 600;
    color: var(--accent);
    list-style-position: inside;
    padding: 4px 0;
  }
  .result-sections {
    margin-top: 12px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }
  .meta-box {
    background: #F8FAFC;
    border: 1px solid #E2E8F0;
    border-radius: 6px;
    padding: 12px 14px;
    font-size: 0.92rem;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .meta-row {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    align-items: baseline;
  }
  .boxed-warning {
    background: #FEF2F2;
    border-left: 4px solid var(--danger);
    padding: 12px 16px;
    color: #7F1D1D;
    border-radius: 4px;
    font-size: 0.94rem;
  }
  .boxed-warning-title {
    color: var(--danger);
    font-weight: 700;
    display: block;
    margin-bottom: 6px;
    font-size: 0.92rem;
    letter-spacing: 0.02em;
  }
  .section-block {
    border: 1px solid #E2E8F0;
    border-radius: 6px;
    background: #FFFFFF;
    padding: 8px 14px;
  }
  .section-block > summary {
    cursor: pointer;
    font-weight: 600;
    color: var(--primary);
    font-size: 0.98rem;
    padding: 4px 0;
  }
  .section-block > summary:hover { color: var(--accent); }
  .section-content {
    padding: 8px 0 4px 0;
    color: #1E293B;
    font-size: 0.95rem;
    line-height: 1.6;
    border-top: 1px solid #F1F5F9;
    margin-top: 6px;
  }
  .section-content p {
    margin: 0 0 8px 0;
  }
  .section-content p:last-child {
    margin-bottom: 0;
  }
  .fmt-subhead {
    display: block;
    margin: 10px 0 4px 0;
    color: var(--primary);
    font-weight: 700;
    font-size: 0.94rem;
  }
  .fmt-list {
    margin: 4px 0 10px 0;
    padding-left: 20px;
  }
  .fmt-list li {
    margin-bottom: 4px;
  }
  .badge {
    display: inline-block;
    padding: 2px 8px;
    border-radius: 4px;
    font-size: 0.78rem;
    background: #E2E8F0;
    color: var(--secondary);
    font-weight: 600;
  }
  .tag-pill {
    display: inline-block;
    padding: 2px 8px;
    margin: 2px 4px 2px 0;
    border-radius: 4px;
    background: #EFF6FF;
    border: 1px solid #BFDBFE;
    color: #1E3A8A;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    font-size: 0.8rem;
  }
  @media (max-width: 600px) {
    body { padding: 14px; font-size: 16px; }
    .search-row { flex-direction: column; }
    .toolbar-row { flex-direction: column; align-items: stretch; }
    .sort-wrap { justify-content: space-between; }
    .btn { width: 100%; min-height: 48px; }
    .chip { padding: 8px 13px; }
    .card, .result-card { padding: 14px; }
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
    <h2 style="margin: 0 0 8px 0; color: var(--accent); font-size: 1.25rem;">Offline Drug Database Empty</h2>
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

  <form id="search-form" method="GET" action="" class="search-row" onsubmit="event.preventDefault(); runFilterSearch();">
    {{range .ActiveKeys}}
      <input type="hidden" name="s" value="{{.}}">
    {{end}}
    <input type="text" id="search-input" name="q" value="{{.Query}}" placeholder="Search drug name, pill ID / NDC, or combine with commas (e.g. ibuprofen, 0363-8139)...">
    <button type="submit" class="btn">Search</button>
  </form>
  <div class="toolbar-row">
    <p class="search-hint">Tip: Combine criteria with commas (e.g. <code>ibuprofen, L484</code> or <code>acetaminophen, Walgreen</code>) and toggle situation filters below.</p>
    <label class="sort-wrap">
      <span>Sort by</span>
      <select id="sort-select" name="sort" class="sort-select" form="search-form" onchange="runFilterSearch()">
        <option value="relevance" {{if or (eq .Sort "") (eq .Sort "relevance")}}selected{{end}}>Best Match</option>
        <option value="name" {{if eq .Sort "name"}}selected{{end}}>Name (A–Z)</option>
        <option value="mfr" {{if eq .Sort "mfr"}}selected{{end}}>Manufacturer (A–Z)</option>
        <option value="concise" {{if eq .Sort "concise"}}selected{{end}}>OTC / Concise First</option>
      </select>
    </label>
  </div>

  <div class="card">
    <div class="situation-header">
      <strong style="font-size: 0.95rem;">Common Situations &amp; Symptoms</strong>
      <button type="button" id="clear-filters-btn" class="clear-filters" onclick="clearSituations()" {{if not .ActiveKeys}}style="display:none;"{{end}}>Clear situation filters ({{len .ActiveKeys}})</button>
    </div>
    <div class="chips">
      {{range .Situations}}
        <button type="button" class="chip {{if .Active}}active{{end}}" data-sit="{{.Key}}" aria-pressed="{{if .Active}}true{{else}}false{{end}}" onclick="toggleSituation('{{.Key}}')">{{.Label}}</button>
      {{end}}
    </div>
  </div>

  <div id="results-summary" style="margin-bottom: 14px; color: var(--secondary); font-size: 0.95rem;">
    {{if .HasFilter}}Showing {{len .Results}} result(s){{if .Query}} for "{{.Query}}"{{end}}{{if .ActiveLabels}} filtered by {{.ActiveLabels}}{{end}}{{end}}
  </div>

  <div id="results-area">
  {{range .Results}}
  <div class="card result-card">
    <h2 class="result-title">{{if .BrandName}}{{.BrandName}}{{else}}{{.GenericName}}{{end}}</h2>
    <div class="result-mfr">{{if .Manufacturer}}{{.Manufacturer}}{{else}}Manufacturer not listed{{end}}</div>
    <details class="result-body" data-id="{{.SetID}}" ontoggle="loadLabelDetails(this)">
      <summary>View Label Details</summary>
      <div class="result-sections" data-loaded="0">
        <p style="color: var(--secondary); font-size: 0.9rem; margin: 0;">Loading formatted label...</p>
      </div>
    </details>
  </div>
  {{else}}
    {{if .HasFilter}}
      <p style="color: var(--secondary);">No matching drug labels found. Try adjusting your comma-separated keywords or situation filters.</p>
    {{end}}
  {{end}}
  </div>
</div>
<script>
var activeSearchController = null;
var labelCache = {};

function escHTML(str) {
  return String(str || '').replace(/[&<>"']/g, function(ch) {
    return ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'})[ch];
  });
}

function getActiveSituations() {
  var form = document.getElementById('search-form');
  return Array.prototype.slice.call(form.querySelectorAll('input[name="s"]')).map(function(el) { return el.value; });
}

function syncChipUI() {
  var active = getActiveSituations();
  var activeSet = {};
  active.forEach(function(k) { activeSet[k] = true; });
  document.querySelectorAll('.chip[data-sit]').forEach(function(btn) {
    var on = !!activeSet[btn.getAttribute('data-sit')];
    btn.classList.toggle('active', on);
    btn.setAttribute('aria-pressed', on ? 'true' : 'false');
  });
  var clearBtn = document.getElementById('clear-filters-btn');
  if (clearBtn) {
    if (active.length > 0) {
      clearBtn.style.display = 'inline-block';
      clearBtn.textContent = 'Clear situation filters (' + active.length + ')';
    } else {
      clearBtn.style.display = 'none';
    }
  }
}

function toggleSituation(key) {
  var form = document.getElementById('search-form');
  var inputs = Array.prototype.slice.call(form.querySelectorAll('input[name="s"]'));
  var found = false;
  inputs.forEach(function(el) {
    if (el.value === key) {
      el.parentNode.removeChild(el);
      found = true;
    }
  });
  if (!found) {
    var hidden = document.createElement('input');
    hidden.type = 'hidden';
    hidden.name = 's';
    hidden.value = key;
    form.appendChild(hidden);
  }
  syncChipUI();
  runFilterSearch();
}

function clearSituations() {
  var form = document.getElementById('search-form');
  Array.prototype.slice.call(form.querySelectorAll('input[name="s"]')).forEach(function(el) {
    el.parentNode.removeChild(el);
  });
  syncChipUI();
  runFilterSearch();
}

function buildSearchParams() {
  var params = new URLSearchParams();
  var q = document.getElementById('search-input').value;
  var sort = document.getElementById('sort-select').value;
  if (q.trim() !== '') params.set('q', q.trim());
  getActiveSituations().forEach(function(k) { params.append('s', k); });
  if (sort && sort !== 'relevance') params.set('sort', sort);
  return params;
}

function runFilterSearch() {
  var params = buildSearchParams();
  var qs = params.toString();
  var nextUrl = qs ? ('?' + qs) : window.location.pathname;
  window.history.replaceState(null, '', nextUrl);

  var summary = document.getElementById('results-summary');
  var area = document.getElementById('results-area');
  var hasFilter = params.has('q') || params.has('s');
  if (!hasFilter) {
    summary.textContent = '';
    area.innerHTML = '';
    return;
  }
  summary.textContent = 'Searching offline labels...';
  if (activeSearchController) {
    activeSearchController.abort();
  }
  activeSearchController = new AbortController();
  fetch('api/search?' + qs, { signal: activeSearchController.signal })
    .then(function(r) { return r.json(); })
    .then(function(data) {
      var list = data.results || [];
      var activeLabels = data.active_labels || '';
      var qVal = params.get('q') || '';
      var text = 'Showing ' + list.length + ' result(s)';
      if (qVal) text += ' for "' + qVal + '"';
      if (activeLabels) text += ' filtered by ' + activeLabels;
      summary.textContent = text;
      if (list.length === 0) {
        area.innerHTML = '<p style="color: var(--secondary);">No matching drug labels found. Try adjusting your comma-separated keywords or situation filters.</p>';
        return;
      }
      area.innerHTML = list.map(function(item) {
        var title = item.brand_name || item.generic_name || 'Unnamed Label';
        var mfr = item.manufacturer || 'Manufacturer not listed';
        return '<div class="card result-card">' +
          '<h2 class="result-title">' + escHTML(title) + '</h2>' +
          '<div class="result-mfr">' + escHTML(mfr) + '</div>' +
          '<details class="result-body" data-id="' + escHTML(item.set_id) + '" ontoggle="loadLabelDetails(this)">' +
            '<summary>View Label Details</summary>' +
            '<div class="result-sections" data-loaded="0"><p style="color: var(--secondary); font-size: 0.9rem; margin: 0;">Loading formatted label...</p></div>' +
          '</details></div>';
      }).join('');
    })
    .catch(function(err) {
      if (err && err.name === 'AbortError') return;
      summary.textContent = 'Search failed. Try again.';
    });
}

function loadLabelDetails(detailsEl) {
  if (!detailsEl.open) return;
  var box = detailsEl.querySelector('.result-sections');
  if (!box || box.getAttribute('data-loaded') === '1') return;
  var id = detailsEl.getAttribute('data-id');
  if (!id) return;
  if (labelCache[id]) {
    box.innerHTML = labelCache[id];
    box.setAttribute('data-loaded', '1');
    return;
  }
  fetch('api/label?id=' + encodeURIComponent(id))
    .then(function(r) { return r.text(); })
    .then(function(htmlText) {
      labelCache[id] = htmlText;
      box.innerHTML = htmlText;
      box.setAttribute('data-loaded', '1');
    })
    .catch(function() {
      box.innerHTML = '<p style="color: var(--danger); margin: 0;">Unable to load label details.</p>';
    });
}
</script>
</body>
</html>`

type PageData struct {
	Query        string
	Sort         string
	ActiveKeys   []string
	ActiveLabels string
	HasFilter    bool
	LabelCount   int
	Importing    bool
	ImportError  string
	Situations   []Situation
	Results      []DrugLabel
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

func openReadDB() (*sql.DB, error) {
	dbPath := dbFilePath()
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return nil, os.ErrNotExist
	}
	conn, err := sql.Open("sqlite", dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	return conn, nil
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
	_ = os.MkdirAll(filepath.Dir(destPath), 0755)
	tmpPath := destPath + ".tmp"
	_ = os.Remove(tmpPath)
	_ = os.Remove(tmpPath + "-journal")

	tmpDB, err := sql.Open("sqlite", tmpPath+"?_pragma=journal_mode(DELETE)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return err
	}
	defer func() {
		if tmpDB != nil {
			_ = tmpDB.Close()
		}
	}()
	if err := ensureSchema(tmpDB); err != nil {
		return err
	}

	for _, part := range manifest.Results.Drug.Label.Partitions {
		if ctx.Err() != nil {
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
	_ = tmpDB.Close()
	tmpDB = nil

	var renameErr error
	for attempt := 0; attempt < 6; attempt++ {
		renameErr = os.Rename(tmpPath, destPath)
		if renameErr == nil {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	return renameErr
}

func countLabels() int {
	conn, err := openReadDB()
	if err != nil {
		return 0
	}
	defer conn.Close()
	var n int
	_ = conn.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM labels`).Scan(&n)
	return n
}

func resolveSituations(keys []string) ([]Situation, []Situation, []string, string) {
	activeSet := make(map[string]bool)
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k != "" {
			activeSet[k] = true
		}
	}
	all := make([]Situation, len(commonSituations))
	var selected []Situation
	var activeKeys []string
	var activeLabels []string
	for i, s := range commonSituations {
		s.Active = activeSet[s.Key]
		all[i] = s
		if s.Active {
			selected = append(selected, s)
			activeKeys = append(activeKeys, s.Key)
			activeLabels = append(activeLabels, s.Label)
		}
	}
	return all, selected, activeKeys, strings.Join(activeLabels, ", ")
}

func parseCommaTerms(rawQuery string) []string {
	parts := strings.Split(rawQuery, ",")
	var terms []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			terms = append(terms, p)
		}
	}
	return terms
}

func ftsQuoteWord(word string) string {
	clean := strings.ReplaceAll(word, `"`, "")
	clean = strings.TrimSpace(clean)
	if clean == "" {
		return ""
	}
	isAlnum := true
	for _, r := range clean {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			isAlnum = false
			break
		}
	}
	if isAlnum {
		return `"` + clean + `"*`
	}
	// Hyphenated pill codes/NDCs (e.g. 0363-8139, I-2) match adjacent tokenized sequence safely when quoted.
	return `"` + clean + `"`
}

func ftsClauseForTerm(term string) string {
	words := strings.Fields(term)
	var parts []string
	for _, w := range words {
		q := ftsQuoteWord(w)
		if q != "" {
			parts = append(parts, q)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

func ftsClauseForSituation(sitQuery string) string {
	branches := strings.Split(sitQuery, " OR ")
	var orParts []string
	for _, b := range branches {
		if c := ftsClauseForTerm(b); c != "" {
			orParts = append(orParts, c)
		}
	}
	if len(orParts) == 0 {
		return ""
	}
	expr := orParts[0]
	if len(orParts) > 1 {
		expr = "(" + strings.Join(orParts, " OR ") + ")"
	}
	// Scope situation filters to medication name/uses so warnings mentioning unrelated symptoms don't pollute hits.
	return "{brand_name generic_name indications} : " + expr
}

func buildFTSExpr(commaTerms []string, situations []Situation) string {
	var groups []string
	for _, t := range commaTerms {
		if c := ftsClauseForTerm(t); c != "" {
			groups = append(groups, c)
		}
	}
	for _, s := range situations {
		if c := ftsClauseForSituation(s.Query); c != "" {
			groups = append(groups, c)
		}
	}
	return strings.Join(groups, " AND ")
}

func orderByClause(sortMode string, hasRank bool) string {
	const completenessRank = "CASE WHEN TRIM(COALESCE(l.brand_name,'')) != '' AND TRIM(COALESCE(l.manufacturer,'')) != '' THEN 0 WHEN TRIM(COALESCE(l.brand_name,'')) != '' OR TRIM(COALESCE(l.generic_name,'')) != '' THEN 1 ELSE 2 END"
	const displayTitle = "COALESCE(NULLIF(TRIM(l.brand_name),''), NULLIF(TRIM(l.generic_name),''), 'ZZZ') COLLATE NOCASE"
	const displayMfr = "COALESCE(NULLIF(TRIM(l.manufacturer),''), 'ZZZ') COLLATE NOCASE"
	switch sortMode {
	case "name":
		return completenessRank + " ASC, " + displayTitle + " ASC"
	case "mfr":
		return completenessRank + " ASC, " + displayMfr + " ASC, " + displayTitle + " ASC"
	case "concise":
		return completenessRank + " ASC, (LENGTH(COALESCE(l.indications,'')) + LENGTH(COALESCE(l.warnings,''))) ASC, " + displayTitle + " ASC"
	default:
		if hasRank {
			return completenessRank + " ASC, m.rank ASC"
		}
		return completenessRank + " ASC, " + displayTitle + " ASC"
	}
}

func searchLabels(rawQuery string, situations []Situation, sortMode string) []DrugLabel {
	commaTerms := parseCommaTerms(rawQuery)
	if len(commaTerms) == 0 && len(situations) == 0 {
		return nil
	}
	conn, err := openReadDB()
	if err != nil {
		return nil
	}
	defer conn.Close()

	ftsExpr := buildFTSExpr(commaTerms, situations)
	var results []DrugLabel
	var ftsErr error

	if ftsExpr != "" {
		// Bounded inner FTS candidate selection cuts large broad-term queries from ~300ms to ~10ms.
		q := `
			SELECT l.set_id, COALESCE(l.brand_name,''), COALESCE(l.generic_name,''), COALESCE(l.manufacturer,'')
			FROM (
				SELECT rowid, bm25(labels_fts, 0.0, 12.0, 10.0, 10.0, 8.0, 4.0, 0.5) AS rank
				FROM labels_fts
				WHERE labels_fts MATCH ?
				LIMIT 400
			) m
			JOIN labels l ON m.rowid = l.rowid
			ORDER BY ` + orderByClause(sortMode, true) + `
			LIMIT 25`
		rows, err := conn.Query(q, ftsExpr)
		ftsErr = err
		if err == nil {
			results = scanLabelSummaries(rows)
			rows.Close()
		}
	}

	if ftsErr != nil || len(results) == 0 {
		results = searchLabelsLike(conn, commaTerms, situations, sortMode)
	}
	return results
}

func searchLabelsLike(conn *sql.DB, commaTerms []string, situations []Situation, sortMode string) []DrugLabel {
	var whereClauses []string
	var args []any

	const allColsExpr = "(COALESCE(brand_name,'') LIKE ? OR COALESCE(generic_name,'') LIKE ? OR COALESCE(manufacturer,'') LIKE ? OR COALESCE(ndc,'') LIKE ? OR COALESCE(imprint,'') LIKE ? OR COALESCE(indications,'') LIKE ? OR COALESCE(warnings,'') LIKE ?)"
	for _, term := range commaTerms {
		like := "%" + term + "%"
		whereClauses = append(whereClauses, allColsExpr)
		args = append(args, like, like, like, like, like, like, like)
	}

	for _, s := range situations {
		branches := strings.Split(s.Query, " OR ")
		var orParts []string
		for _, b := range branches {
			words := strings.Fields(b)
			if len(words) == 0 {
				continue
			}
			var wordAnds []string
			for _, w := range words {
				like := "%" + w + "%"
				wordAnds = append(wordAnds, allColsExpr)
				args = append(args, like, like, like, like, like, like, like)
			}
			orParts = append(orParts, "("+strings.Join(wordAnds, " AND ")+")")
		}
		if len(orParts) > 0 {
			whereClauses = append(whereClauses, "("+strings.Join(orParts, " OR ")+")")
		}
	}

	if len(whereClauses) == 0 {
		return nil
	}

	q := `
		SELECT l.set_id, COALESCE(l.brand_name,''), COALESCE(l.generic_name,''), COALESCE(l.manufacturer,'')
		FROM labels l
		WHERE ` + strings.Join(whereClauses, " AND ") + `
		ORDER BY ` + orderByClause(sortMode, false) + `
		LIMIT 25`
	rows, err := conn.Query(q, args...)
	if err != nil {
		log.Printf("Fallback search failed: %v", err)
		return nil
	}
	defer rows.Close()
	return scanLabelSummaries(rows)
}

func scanLabelSummaries(rows *sql.Rows) []DrugLabel {
	var results []DrugLabel
	for rows.Next() {
		var item DrugLabel
		if err := rows.Scan(&item.SetID, &item.BrandName, &item.GenericName, &item.Manufacturer); err == nil {
			results = append(results, item)
		}
	}
	return results
}

func getLabelByID(setID string) (DrugLabel, bool) {
	var item DrugLabel
	if strings.TrimSpace(setID) == "" {
		return item, false
	}
	conn, err := openReadDB()
	if err != nil {
		return item, false
	}
	defer conn.Close()
	err = conn.QueryRow(`
		SELECT set_id, COALESCE(brand_name,''), COALESCE(generic_name,''), COALESCE(manufacturer,''), COALESCE(ndc,''), COALESCE(imprint,''),
		       COALESCE(boxed_warning,''), COALESCE(indications,''), COALESCE(dosage,''), COALESCE(warnings,''),
		       COALESCE(do_not_use,''), COALESCE(stop_use,''), COALESCE(active_ingredient,''), COALESCE(inactive_ingredient,''), COALESCE(storage,'')
		FROM labels WHERE set_id = ? LIMIT 1`, setID).Scan(
		&item.SetID, &item.BrandName, &item.GenericName, &item.Manufacturer, &item.NDC, &item.Imprint,
		&item.BoxedWarning, &item.Indications, &item.Dosage, &item.Warnings,
		&item.DoNotUse, &item.StopUse, &item.ActiveIngred, &item.InactiveIngred, &item.Storage,
	)
	return item, err == nil
}

var (
	reLeadingHeader = regexp.MustCompile(`^(?i)(INDICATIONS\s+AND\s+USAGE|BOXED\s+WARNING|DOSAGE\s+AND\s+ADMINISTRATION|WARNINGS\s+AND\s+PRECAUTIONS|STOP\s+USE\s+AND\s+ASK\s+A\s+DOCTOR\s+IF|INACTIVE\s+INGREDIENTS?|ACTIVE\s+INGREDIENTS?|STORAGE\s+AND\s+HANDLING|DO\s+NOT\s+USE|DIRECTIONS|WARNINGS?|USES?)\s*:?\s*`)
	reClinicalCues  = regexp.MustCompile(`(?i)\b(Allergy alert:|Stomach bleeding warning:|Heart attack and stroke warning:|Liver warning:|Reye's syndrome:|Sore throat warning:|Adults and children [^:]{1,35}:|Children under [^:]{1,30}:|Adults:|Ask a doctor before use if|Ask a doctor or pharmacist before use if|When using this product\b|Stop use and ask a doctor if|If pregnant or breast-feeding,|Keep out of reach of children\.?)`)
	reAlertLabel    = regexp.MustCompile(`^([A-Z][^:\n]{2,55}:)\s*(.*)$`)
	reSentenceEnd   = regexp.MustCompile(`([.!?])\s+([A-Z])`)
)

func formatNDC(raw string) template.HTML {
	parts := strings.Split(raw, "|")
	var out strings.Builder
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out.WriteString(`<span class="tag-pill">`)
			out.WriteString(html.EscapeString(p))
			out.WriteString(`</span>`)
		}
	}
	if out.Len() == 0 {
		return template.HTML(html.EscapeString(raw))
	}
	return template.HTML(out.String())
}

func formatImprint(raw string) template.HTML {
	parts := strings.Split(raw, "|")
	var cleaned []string
	seen := make(map[string]bool)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			cleaned = append(cleaned, p)
		}
	}
	if len(cleaned) <= 1 {
		return formatSection(strings.Join(cleaned, " "))
	}
	var out strings.Builder
	out.WriteString(`<ul class="fmt-list">`)
	for _, item := range cleaned {
		out.WriteString(`<li>`)
		out.WriteString(html.EscapeString(item))
		out.WriteString(`</li>`)
	}
	out.WriteString(`</ul>`)
	return template.HTML(out.String())
}

func formatSection(raw string) template.HTML {
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = reLeadingHeader.ReplaceAllString(text, "")
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	// Put inline bullet characters onto individual lines for semantic list conversion.
	for _, marker := range []string{"•", "●", "▪", "◦"} {
		text = strings.ReplaceAll(text, marker, "\n• ")
	}
	// Separate FDA clinical subsections onto clean block boundaries.
	text = reClinicalCues.ReplaceAllString(text, "\n$1")

	rawLines := strings.Split(text, "\n")
	var out strings.Builder
	inList := false
	closeList := func() {
		if inList {
			out.WriteString("</ul>")
			inList = false
		}
	}

	for _, line := range rawLines {
		line = strings.Join(strings.Fields(strings.TrimSpace(line)), " ")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "•") || strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			item := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(line, "•"), "- "), "* "))
			if item == "" {
				continue
			}
			if !inList {
				out.WriteString(`<ul class="fmt-list">`)
				inList = true
			}
			out.WriteString("<li>")
			out.WriteString(html.EscapeString(item))
			out.WriteString("</li>")
			continue
		}

		closeList()
		if m := reAlertLabel.FindStringSubmatch(line); len(m) == 3 {
			out.WriteString(`<strong class="fmt-subhead">`)
			out.WriteString(html.EscapeString(m[1]))
			out.WriteString(`</strong>`)
			if rest := strings.TrimSpace(m[2]); rest != "" {
				writeChunkedParagraphs(&out, rest)
			}
			continue
		}
		writeChunkedParagraphs(&out, line)
	}
	closeList()

	return template.HTML(out.String())
}

func writeChunkedParagraphs(out *strings.Builder, block string) {
	if len(block) <= 280 {
		out.WriteString("<p>")
		out.WriteString(html.EscapeString(block))
		out.WriteString("</p>")
		return
	}
	// Break dense multi-sentence FDA blocks into 2-sentence paragraphs for readability.
	splitReady := reSentenceEnd.ReplaceAllString(block, "$1\n$2")
	sentences := strings.Split(splitReady, "\n")
	var buf []string
	curLen := 0
	flush := func() {
		if len(buf) == 0 {
			return
		}
		out.WriteString("<p>")
		out.WriteString(html.EscapeString(strings.Join(buf, " ")))
		out.WriteString("</p>")
		buf = nil
		curLen = 0
	}
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		buf = append(buf, s)
		curLen += len(s)
		if len(buf) >= 2 && curLen >= 200 {
			flush()
		}
	}
	flush()
}

const htmlLabelSections = `
{{if or .GenericName .NDC .Imprint}}
<div class="meta-box">
  {{if .GenericName}}<div class="meta-row"><span class="badge">Generic</span><span>{{.GenericName}}</span></div>{{end}}
  {{if .NDC}}<div class="meta-row"><span class="badge">NDC</span><div>{{formatNDC .NDC}}</div></div>{{end}}
  {{if .Imprint}}<div class="meta-row"><span class="badge">Pill ID / Form</span><div>{{formatImprint .Imprint}}</div></div>{{end}}
</div>
{{end}}
{{if .BoxedWarning}}
<div class="boxed-warning">
  <span class="boxed-warning-title">BOXED WARNING</span>
  <div class="section-content" style="border-top: none; margin-top: 0; padding-top: 0; color: #7F1D1D;">{{formatSection .BoxedWarning}}</div>
</div>
{{end}}
{{if .Indications}}<details class="section-block" open><summary>Uses / Indications</summary><div class="section-content">{{formatSection .Indications}}</div></details>{{end}}
{{if .Dosage}}<details class="section-block" open><summary>Dosage &amp; Administration</summary><div class="section-content">{{formatSection .Dosage}}</div></details>{{end}}
{{if .DoNotUse}}<details class="section-block"><summary>Do Not Use</summary><div class="section-content">{{formatSection .DoNotUse}}</div></details>{{end}}
{{if .Warnings}}<details class="section-block" open><summary>Warnings &amp; Precautions</summary><div class="section-content">{{formatSection .Warnings}}</div></details>{{end}}
{{if .StopUse}}<details class="section-block"><summary>Stop Use</summary><div class="section-content">{{formatSection .StopUse}}</div></details>{{end}}
{{if .ActiveIngred}}<details class="section-block"><summary>Active Ingredients</summary><div class="section-content">{{formatSection .ActiveIngred}}</div></details>{{end}}
{{if .InactiveIngred}}<details class="section-block"><summary>Inactive Ingredients</summary><div class="section-content">{{formatSection .InactiveIngred}}</div></details>{{end}}
{{if .Storage}}<details class="section-block"><summary>Storage &amp; Handling</summary><div class="section-content">{{formatSection .Storage}}</div></details>{{end}}
{{if not (or .GenericName .NDC .Imprint .BoxedWarning .Indications .Dosage .DoNotUse .Warnings .StopUse .ActiveIngred .InactiveIngred .Storage)}}
<p style="color: var(--secondary); margin: 0;">No additional label text stored for this entry.</p>
{{end}}`

func buildHandler() http.Handler {
	funcMap := template.FuncMap{
		"formatSection": formatSection,
		"formatNDC":     formatNDC,
		"formatImprint": formatImprint,
	}
	tmpl := template.Must(template.New("index").Funcs(funcMap).Parse(htmlIndex))
	detailTmpl := template.Must(template.New("detail").Funcs(funcMap).Parse(htmlLabelSections))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		sortMode := r.URL.Query().Get("sort")
		_, selected, _, activeLabels := resolveSituations(r.URL.Query()["s"])
		res := searchLabels(q, selected, sortMode)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Results      []DrugLabel `json:"results"`
			ActiveLabels string      `json:"active_labels"`
		}{Results: res, ActiveLabels: activeLabels})
	})

	mux.HandleFunc("/api/label", func(w http.ResponseWriter, r *http.Request) {
		item, ok := getLabelByID(r.URL.Query().Get("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = detailTmpl.Execute(w, item)
	})

	mux.HandleFunc("/api/import", func(w http.ResponseWriter, r *http.Request) {
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

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("q")
		sortMode := r.URL.Query().Get("sort")
		allSituations, selected, activeKeys, activeLabels := resolveSituations(r.URL.Query()["s"])
		res := searchLabels(q, selected, sortMode)

		importMu.Lock()
		importing := isImporting
		lastErr := importErr
		importMu.Unlock()

		data := PageData{
			Query:        q,
			Sort:         sortMode,
			ActiveKeys:   activeKeys,
			ActiveLabels: activeLabels,
			HasFilter:    strings.TrimSpace(q) != "" || len(selected) > 0,
			LabelCount:   countLabels(),
			Importing:    importing,
			ImportError:  lastErr,
			Situations:   allSituations,
			Results:      res,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.Execute(w, data)
	})
	return mux
}

func main() {
	doImport := flag.Bool("import", false, "Download and rebuild FDA drug labels database from openFDA")
	flag.Parse()

	if *doImport {
		if err := importOpenFDA(context.Background(), ""); err != nil {
			log.Fatalf("openFDA import failed: %v", err)
		}
		log.Println("openFDA drug labels import complete.")
		return
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("FDA Drug Labels serving on :%s", port)
	if err := http.ListenAndServe(":"+port, buildHandler()); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
