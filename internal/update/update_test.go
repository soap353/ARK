package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func createSignedBundle(t *testing.T, dir string, priv ed25519.PrivateKey, version, filename string, payload []byte) {
	t.Helper()
	binPath := filepath.Join(dir, filename)
	if err := os.WriteFile(binPath, payload, 0755); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	sum := sha256.Sum256(payload)
	m := Manifest{
		Version:   version,
		Filename:  filename,
		SHA256:    hex.EncodeToString(sum[:]),
		Changelog: "Bug fixes",
	}
	sig := ed25519.Sign(priv, SigningPayload(m))
	m.SignatureHex = hex.EncodeToString(sig)

	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "update-manifest.json"), raw, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestVerifyManifestAndBinary(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	dir := t.TempDir()
	createSignedBundle(t, dir, priv, "0.4.0", "ark-new", []byte("valid-binary-bytes-v0.4.0"))

	m, binPath, err := LoadUpdateFromDir(dir, pub)
	if err != nil {
		t.Fatalf("expected valid signed bundle, got err: %v", err)
	}
	if m.Version != "0.4.0" || filepath.Base(binPath) != "ark-new" {
		t.Fatalf("unexpected manifest/binPath: %+v, %s", m, binPath)
	}

	// Tamper binary bytes -> SHA-256 check must reject.
	if err := os.WriteFile(binPath, []byte("tampered-binary"), 0755); err != nil {
		t.Fatalf("overwrite tampered binary: %v", err)
	}
	if _, _, err := LoadUpdateFromDir(dir, pub); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got %v", err)
	}

	// Restore binary bytes, tamper signature -> Ed25519 verification must reject.
	if err := os.WriteFile(binPath, []byte("valid-binary-bytes-v0.4.0"), 0755); err != nil {
		t.Fatalf("restore binary: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "update-manifest.json"))
	var tampered Manifest
	_ = json.Unmarshal(raw, &tampered)
	tampered.SignatureHex = strings.Repeat("00", ed25519.SignatureSize)
	tamperedRaw, _ := json.Marshal(tampered)
	_ = os.WriteFile(filepath.Join(dir, "update-manifest.json"), tamperedRaw, 0644)

	if _, _, err := LoadUpdateFromDir(dir, pub); err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("expected signature verification failure, got %v", err)
	}
}

func TestApplyUpdateConfirmHealthyAndRollback(t *testing.T) {
	dataDir := t.TempDir()
	binDir := t.TempDir()

	curBin := filepath.Join(binDir, "ark")
	newBin := filepath.Join(binDir, "ark-0.4.0")
	if err := os.WriteFile(curBin, []byte("old-binary-v0.3.0"), 0755); err != nil {
		t.Fatalf("write curBin: %v", err)
	}
	if err := os.WriteFile(newBin, []byte("new-binary-v0.4.0"), 0755); err != nil {
		t.Fatalf("write newBin: %v", err)
	}

	dbPath := filepath.Join(dataDir, "ark.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE notes (val TEXT); INSERT INTO notes (val) VALUES ('before-update');`); err != nil {
		t.Fatalf("seed db: %v", err)
	}
	_ = db.Close()

	if err := ApplyUpdate(curBin, newBin, dataDir, "0.3.0", "0.4.0"); err != nil {
		t.Fatalf("ApplyUpdate failed: %v", err)
	}

	gotBin, _ := os.ReadFile(curBin)
	if string(gotBin) != "new-binary-v0.4.0" {
		t.Fatalf("expected new binary installed, got %q", string(gotBin))
	}
	if !HasRollbackAvailable(curBin) {
		t.Fatalf("expected HasRollbackAvailable to be true")
	}

	// Mutate DB after update so we verify Rollback restores the pre-update snapshot.
	db2, _ := sql.Open("sqlite", dbPath)
	_, _ = db2.Exec(`DELETE FROM notes; INSERT INTO notes (val) VALUES ('corrupted-after-update');`)
	_ = db2.Close()

	// Simulated crash recovery on startup when update.pending still exists.
	rolledBack, err := CheckAndRecoverOnStartup(curBin, dataDir)
	if err != nil {
		t.Fatalf("CheckAndRecoverOnStartup error: %v", err)
	}
	if !rolledBack {
		t.Fatalf("expected automatic rollback when update.pending is present")
	}

	restoredBin, _ := os.ReadFile(curBin)
	if string(restoredBin) != "old-binary-v0.3.0" {
		t.Fatalf("expected restored old binary, got %q", string(restoredBin))
	}

	db3, _ := sql.Open("sqlite", dbPath)
	var val string
	if err := db3.QueryRow(`SELECT val FROM notes LIMIT 1`).Scan(&val); err != nil {
		t.Fatalf("query restored db: %v", err)
	}
	_ = db3.Close()
	if val != "before-update" {
		t.Fatalf("expected restored DB value 'before-update', got %q", val)
	}

	notice := ConsumeRollbackNotice(dataDir)
	if notice == "" {
		t.Fatalf("expected non-empty rollback notice")
	}
	if ConsumeRollbackNotice(dataDir) != "" {
		t.Fatalf("expected rollback notice to be consumed once")
	}

	// Now test healthy confirmation removes update.pending and avoids rollback.
	if err := ApplyUpdate(curBin, newBin, dataDir, "0.3.0", "0.4.0"); err != nil {
		t.Fatalf("re-apply failed: %v", err)
	}
	if err := ConfirmHealthy(dataDir); err != nil {
		t.Fatalf("ConfirmHealthy failed: %v", err)
	}
	rolledBack, err = CheckAndRecoverOnStartup(curBin, dataDir)
	if err != nil || rolledBack {
		t.Fatalf("expected no rollback after ConfirmHealthy, got rolledBack=%v err=%v", rolledBack, err)
	}
}

func TestGitHubReleaseCheckAndSelfUpdate(t *testing.T) {
	if !IsNewerVersion("v0.4.0", "0.3.0") || !IsNewerVersion("1.0.0", "0.9.9") {
		t.Fatalf("expected newer semver comparisons to be true")
	}
	if IsNewerVersion("0.3.0", "0.3.0") || IsNewerVersion("0.2.9", "0.3.0") {
		t.Fatalf("expected equal/older semver comparisons to be false")
	}

	payload := []byte("ark-new-release-binary-payload-v0.4.0")
	sum := sha256.Sum256(payload)
	hexSum := hex.EncodeToString(sum[:])
	assetName := ExpectedAssetName()

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/soap353/ARK/releases/latest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"tag_name": "v0.4.0",
				"name": "Ark v0.4.0",
				"body": "Sidebar redesign + self-update",
				"published_at": "2026-11-18T12:00:00Z",
				"assets": [
					{"name": "` + assetName + `", "size": 37, "browser_download_url": "` + srvURL + `/download/` + assetName + `"},
					{"name": "checksums.txt", "size": 90, "browser_download_url": "` + srvURL + `/download/checksums.txt"}
				]
			}`))
		case "/download/" + assetName:
			_, _ = w.Write(payload)
		case "/download/checksums.txt":
			_, _ = w.Write([]byte(hexSum + "  " + assetName + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	rel, err := CheckGitHubRelease(context.Background(), srv.Client(), srv.URL, "soap353/ARK", "0.3.0")
	if err != nil {
		t.Fatalf("CheckGitHubRelease failed: %v", err)
	}
	if !rel.HasUpdate || rel.Version != "0.4.0" || rel.AssetName != assetName {
		t.Fatalf("unexpected release info: %+v", rel)
	}

	tmp := t.TempDir()
	exePath := filepath.Join(tmp, "ark")
	_ = os.WriteFile(exePath, []byte("old-binary"), 0755)
	dataDir := filepath.Join(tmp, "data")

	verifiedVia, err := DownloadAndApplyGitHubRelease(context.Background(), srv.Client(), rel, exePath, dataDir)
	if err != nil {
		t.Fatalf("DownloadAndApplyGitHubRelease failed: %v", err)
	}
	if !strings.Contains(verifiedVia, "SHA-256") {
		t.Errorf("expected SHA-256 verification, got %q", verifiedVia)
	}
	got, _ := os.ReadFile(exePath)
	if string(got) != string(payload) {
		t.Fatalf("expected updated binary payload, got %q", string(got))
	}
}
