package jobs

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DrugLabelsRunner downloads openFDA bulk label partitions and builds druglabels.db atomically.
type DrugLabelsRunner struct {
	ManifestURL string
	HTTPClient  *http.Client
}

// NewDrugLabelsRunner initializes a DrugLabelsRunner.
func NewDrugLabelsRunner() *DrugLabelsRunner {
	return &DrugLabelsRunner{
		ManifestURL: "https://api.fda.gov/download.json",
		HTTPClient:  &http.Client{Timeout: 10 * time.Minute},
	}
}

// Run downloads openFDA label ZIP shards into <OutputDir>/druglabels.db.
func (r *DrugLabelsRunner) Run(ctx context.Context, job *Job, progress ProgressFunc) error {
	meta := ParseMetadata(job.Metadata)
	outDir := meta.OutputDir
	if outDir == "" {
		outDir = filepath.Join(".", "druglabels")
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}

	if progress != nil {
		progress(1.0, "Fetching openFDA manifest...", 0, 0, 0)
	}

	manifestURL := r.ManifestURL
	if strings.HasPrefix(job.Target, "http://") || strings.HasPrefix(job.Target, "https://") {
		manifestURL = job.Target
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return err
	}
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch openFDA manifest: %w", err)
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
		return fmt.Errorf("decode openFDA manifest: %w", err)
	}

	parts := manifest.Results.Drug.Label.Partitions
	if len(parts) == 0 {
		return fmt.Errorf("no drug label partitions found in openFDA manifest")
	}

	destPath := filepath.Join(outDir, "druglabels.db")
	tmpPath := destPath + ".tmp"
	_ = os.Remove(tmpPath)

	tmpDB, err := sql.Open("sqlite", tmpPath)
	if err != nil {
		return err
	}
	defer func() {
		if tmpDB != nil {
			_ = tmpDB.Close()
		}
	}()

	if _, err := tmpDB.Exec(`
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
		);
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
	`); err != nil {
		return err
	}

	for idx, part := range parts {
		if ctx.Err() != nil {
			_ = tmpDB.Close()
			tmpDB = nil
			_ = os.Remove(tmpPath)
			return ctx.Err()
		}
		if progress != nil {
			pct := 2.0 + (float64(idx)/float64(len(parts)))*95.0
			progress(pct, fmt.Sprintf("Downloading FDA partition %d/%d...", idx+1, len(parts)), 0, 0, 0)
		}

		pReq, err := http.NewRequestWithContext(ctx, http.MethodGet, part.File, nil)
		if err != nil {
			continue
		}
		pResp, err := r.HTTPClient.Do(pReq)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
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

			tx, err := tmpDB.Begin()
			if err != nil {
				continue
			}
			stmt, err := tx.Prepare(`INSERT OR REPLACE INTO labels (set_id, brand_name, generic_name, manufacturer, ndc, imprint, boxed_warning, indications, dosage, warnings, do_not_use, stop_use, active_ingredient, inactive_ingredient, storage) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
			if err != nil {
				_ = tx.Rollback()
				continue
			}
			for _, rec := range payload.Results {
				if rec.SetID == "" {
					continue
				}
				_, _ = stmt.Exec(
					rec.SetID,
					strings.TrimSpace(strings.Join(rec.OpenFDA.BrandName, "\n")),
					strings.TrimSpace(strings.Join(rec.OpenFDA.GenericName, "\n")),
					strings.TrimSpace(strings.Join(rec.OpenFDA.ManufacturerName, "\n")),
					joinUniqueTrimmed(rec.OpenFDA.ProductNDC, rec.OpenFDA.PackageNDC),
					joinUniqueTrimmed(rec.SPLProductData, rec.Description, rec.HowSupplied),
					strings.TrimSpace(strings.Join(rec.BoxedWarning, "\n")),
					strings.TrimSpace(strings.Join(rec.Indications, "\n")),
					strings.TrimSpace(strings.Join(rec.Dosage, "\n")),
					strings.TrimSpace(strings.Join(rec.Warnings, "\n")),
					strings.TrimSpace(strings.Join(rec.DoNotUse, "\n")),
					strings.TrimSpace(strings.Join(rec.StopUse, "\n")),
					strings.TrimSpace(strings.Join(rec.ActiveIngred, "\n")),
					strings.TrimSpace(strings.Join(rec.InactiveIngred, "\n")),
					strings.TrimSpace(strings.Join(rec.Storage, "\n")),
				)
			}
			stmt.Close()
			_ = tx.Commit()
		}
	}

	_, _ = tmpDB.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('export_date', ?)`, manifest.Results.Drug.Label.ExportDate)
	_ = tmpDB.Close()
	tmpDB = nil

	if err := os.Rename(tmpPath, destPath); err != nil {
		return err
	}
	if progress != nil {
		progress(100.0, "FDA Drug Labels database ready", 0, 0, 0)
	}
	return nil
}

func joinUniqueTrimmed(lists ...[]string) string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range lists {
		for _, item := range list {
			item = strings.TrimSpace(item)
			if item != "" && !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		}
	}
	s := strings.Join(out, " | ")
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
