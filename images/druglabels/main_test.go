package main

import (
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func seedTestDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "druglabels.db")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()
	if err := ensureSchema(conn); err != nil {
		t.Fatalf("schema: %v", err)
	}
	rows := []struct {
		id, brand, generic, mfr, ndc, imprint, bw, ind, dos, warn string
	}{
		{
			"id-1", "Advil", "IBUPROFEN", "Haleon", "0573-0169 | 0573-0169-10", "I-2 brown round tablet",
			"", "Uses temporarily: • reduces fever • relieves minor aches and pains",
			"Directions adults and children 12 years and over: take 1 tablet every 4 to 6 hours.",
			"Warnings Allergy alert: Ibuprofen may cause a severe allergic reaction. Do not use if you have ever had an allergic reaction.",
		},
		{
			"id-2", "Tylenol", "ACETAMINOPHEN", "Kenvue", "50580-449", "TYLENOL 500 white caplet L484",
			"", "INDICATIONS AND USAGE temporarily reduces fever and relieves minor aches.",
			"DOSAGE AND ADMINISTRATION Take 2 caplets every 6 hours.",
			"WARNINGS Liver warning: Severe liver damage may occur. Keep out of reach of children.",
		},
		{
			"id-3", "Benadryl", "DIPHENHYDRAMINE HCL", "Walgreen Co", "0363-2211", "W22 pink capsule",
			"", "Uses temporarily relieves symptoms of allergy and hay fever.",
			"Directions Adults: 1 capsule.",
			"Warnings May cause marked drowsiness.",
		},
	}
	for _, r := range rows {
		_, err := conn.Exec(`INSERT INTO labels (set_id, brand_name, generic_name, manufacturer, ndc, imprint, boxed_warning, indications, dosage, warnings) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			r.id, r.brand, r.generic, r.mfr, r.ndc, r.imprint, r.bw, r.ind, r.dos, r.warn)
		if err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	return dbPath
}

func TestMergedSearchCommaAndSituationFilter(t *testing.T) {
	t.Setenv("DRUGLABELS_DB", seedTestDB(t))
	h := buildHandler()

	// 1. Comma-separated name + pill ID ("ibuprofen, I-2") plus active situation ("fever")
	req := httptest.NewRequest("GET", "/?q=ibuprofen%2C+I-2&s=fever", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, `<h2 class="result-title">Advil</h2>`) {
		t.Fatalf("expected Advil in comma-separated + situation search, got:\n%s", body)
	}
	if strings.Contains(body, `<h2 class="result-title">Tylenol</h2>`) {
		t.Fatalf("did not expect Tylenol for ibuprofen, I-2 query")
	}
	// Highlighted chip is active, and query input stays verbatim ("ibuprofen, I-2") without appending situation text
	if !strings.Contains(body, `class="chip active" data-sit="fever"`) {
		t.Fatalf("expected fever chip highlighted with active class")
	}
	if !strings.Contains(body, `value="ibuprofen, I-2"`) {
		t.Fatalf("expected search input value to preserve user query without injected situation text")
	}
	// Mode tabs should be removed
	if strings.Contains(body, "mode-tab") {
		t.Fatalf("expected legacy mode tabs to be removed")
	}
	// Drug result content must be collapsed by default (no open attribute on result-body)
	if strings.Contains(body, `<details class="result-body" data-id="id-1" open`) {
		t.Fatalf("expected result-body details to be collapsed by default")
	}
	if !strings.Contains(body, `<details class="result-body" data-id="id-1"`) {
		t.Fatalf("expected collapsed lazy-loaded result-body details")
	}

	// 2. Lazy /api/label?id=id-1 renders procedurally formatted details
	reqLbl := httptest.NewRequest("GET", "/api/label?id=id-1", nil)
	recLbl := httptest.NewRecorder()
	h.ServeHTTP(recLbl, reqLbl)
	lblBody := recLbl.Body.String()
	if !strings.Contains(lblBody, `<span class="tag-pill">0573-0169</span>`) || !strings.Contains(lblBody, `<ul class="fmt-list"><li>reduces fever</li>`) {
		t.Fatalf("expected formatted label HTML from /api/label, got:\n%s", lblBody)
	}

	// 3. Situation-only filter + sort=name / sort=mfr via /api/search
	reqSort := httptest.NewRequest("GET", "/api/search?s=fever&sort=name", nil)
	recSort := httptest.NewRecorder()
	h.ServeHTTP(recSort, reqSort)
	sortBody := recSort.Body.String()
	if !strings.Contains(sortBody, `"brand_name":"Advil"`) || !strings.Contains(sortBody, `"active_labels":"Fever"`) {
		t.Fatalf("expected sorted JSON search results, got:\n%s", sortBody)
	}

	// 4. Situation-only filter (empty search input) highlights chip and shows matching results
	req2 := httptest.NewRequest("GET", "/?s=allergy", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	body2 := rec2.Body.String()
	if !strings.Contains(body2, `<h2 class="result-title">Benadryl</h2>`) {
		t.Fatalf("expected Benadryl for allergy situation filter")
	}
	if !strings.Contains(body2, `name="q" value=""`) {
		t.Fatalf("expected search bar to remain empty when only clicking situation chip")
	}

	// 5. Comma-separated manufacturer + NDC fallback search ("Walgreen, 0363-2211")
	req3 := httptest.NewRequest("GET", "/?q=Walgreen%2C+0363-2211", nil)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if !strings.Contains(rec3.Body.String(), `<h2 class="result-title">Benadryl</h2>`) {
		t.Fatalf("expected Benadryl via comma-separated manufacturer + NDC fallback")
	}
}

func TestFormatSectionProceduralFormatting(t *testing.T) {
	raw := "INDICATIONS AND USAGE temporarily: • reduces fever • relieves pain Allergy alert: Seek medical help. <script>alert(1)</script>"
	got := string(formatSection(raw))

	if strings.Contains(got, "INDICATIONS AND USAGE") {
		t.Fatalf("expected redundant prefix stripped, got %q", got)
	}
	if !strings.Contains(got, `<ul class="fmt-list"><li>reduces fever</li><li>relieves pain</li></ul>`) {
		t.Fatalf("expected bullet list formatting, got %q", got)
	}
	if !strings.Contains(got, `<strong class="fmt-subhead">Allergy alert:</strong>`) {
		t.Fatalf("expected clinical alert promoted to subheading, got %q", got)
	}
	if strings.Contains(got, "<script>") {
		t.Fatalf("expected HTML escaped safely, got %q", got)
	}

	ndc := string(formatNDC("0573-0169 | 0573-0169-10"))
	if !strings.Contains(ndc, `<span class="tag-pill">0573-0169</span><span class="tag-pill">0573-0169-10</span>`) {
		t.Fatalf("expected NDC pills, got %q", ndc)
	}
}
