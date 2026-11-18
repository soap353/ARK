package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ArkVersion is the current Command Center build version (overridable via -ldflags -X).
var ArkVersion = "0.3.0"

// EmbeddedPublicKeyHex is the Ed25519 release signing public key (32-byte hex).
const EmbeddedPublicKeyHex = "6e3d21a8e41b9c6f0d8a5e7c3b2f1098d4a6e2c8b0f4e6a2c8d0b2f4a6e8c0d2"

// Manifest describes a signed Ark binary update bundle (e.g., on USB or downloaded release).
type Manifest struct {
	Version      string `json:"version"`
	Filename     string `json:"filename"`
	SHA256       string `json:"sha256"`
	SignatureHex string `json:"signature_hex"`
	Changelog    string `json:"changelog,omitempty"`
}

// PendingMarker records an in-progress binary swap until the new process confirms healthy startup.
type PendingMarker struct {
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	DBBackup    string    `json:"db_backup"`
	CreatedAt   time.Time `json:"created_at"`
}

// DefaultPublicKey decodes EmbeddedPublicKeyHex.
func DefaultPublicKey() (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(EmbeddedPublicKeyHex)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid embedded ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

// SigningPayload produces the canonical byte string covered by an update manifest signature.
func SigningPayload(m Manifest) []byte {
	return []byte(fmt.Sprintf("ark-update:v1:%s:%s:%s", strings.TrimSpace(m.Version), strings.TrimSpace(m.Filename), strings.ToLower(strings.TrimSpace(m.SHA256))))
}

// VerifyFileSHA256 computes the SHA-256 hex digest of path and compares it with expectedHex.
func VerifyFileSHA256(path, expectedHex string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, strings.TrimSpace(expectedHex)) {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", filepath.Base(path), actual, expectedHex)
	}
	return nil
}

// VerifyManifestAndBinary validates the Ed25519 signature and binary SHA-256 checksum inside dir.
func VerifyManifestAndBinary(dir string, m Manifest, pubKey ed25519.PublicKey) (string, error) {
	if len(pubKey) != ed25519.PublicKeySize {
		return "", errors.New("invalid ed25519 public key length")
	}
	sig, err := hex.DecodeString(strings.TrimSpace(m.SignatureHex))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", errors.New("invalid update signature encoding")
	}
	if !ed25519.Verify(pubKey, SigningPayload(m), sig) {
		return "", errors.New("update signature verification failed: tampered or untrusted release")
	}

	cleanName := filepath.Base(strings.TrimSpace(m.Filename))
	if cleanName == "" || cleanName == "." || cleanName == ".." {
		return "", errors.New("invalid binary filename in update manifest")
	}
	binPath := filepath.Join(dir, cleanName)
	if err := VerifyFileSHA256(binPath, m.SHA256); err != nil {
		return "", err
	}
	return binPath, nil
}

// LoadUpdateFromDir reads update-manifest.json in dir and verifies its signature and binary checksum.
func LoadUpdateFromDir(dir string, pubKey ed25519.PublicKey) (Manifest, string, error) {
	var m Manifest
	raw, err := os.ReadFile(filepath.Join(dir, "update-manifest.json"))
	if err != nil {
		return m, "", fmt.Errorf("read update-manifest.json: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, "", fmt.Errorf("parse update-manifest.json: %w", err)
	}
	binPath, err := VerifyManifestAndBinary(dir, m, pubKey)
	if err != nil {
		return m, "", err
	}
	return m, binPath, nil
}

// SnapshotDB creates a consistent SQLite snapshot of dbPath under <dataDir>/backups/ark.db.<tag>.
func SnapshotDB(dbPath, dataDir, tag string) (string, error) {
	backupDir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return "", err
	}
	safeTag := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(strings.TrimSpace(tag))
	if safeTag == "" {
		safeTag = time.Now().Format("20060102-150405")
	}
	dst := filepath.Join(backupDir, "ark.db."+safeTag)
	_ = os.Remove(dst)

	if _, err := os.Stat(dbPath); err != nil {
		return "", err
	}
	d, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return copyFile(dbPath, dst, 0600)
	}
	defer d.Close()

	// VACUUM INTO creates an atomic, WAL-safe snapshot without blocking readers.
	escaped := strings.ReplaceAll(dst, "'", "''")
	if _, err := d.Exec(fmt.Sprintf("VACUUM INTO '%s'", escaped)); err != nil {
		return copyFile(dbPath, dst, 0600)
	}
	return dst, nil
}

// ApplyUpdate snapshots ark.db, backs up binPath to binPath.old, installs newBinPath, and writes update.pending.
func ApplyUpdate(binPath, newBinPath, dataDir, fromVersion, toVersion string) error {
	if _, err := os.Stat(newBinPath); err != nil {
		return fmt.Errorf("new binary missing: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}

	dbPath := filepath.Join(dataDir, "ark.db")
	var dbBackup string
	if _, err := os.Stat(dbPath); err == nil {
		snapPath, err := SnapshotDB(dbPath, dataDir, fromVersion)
		if err != nil {
			return fmt.Errorf("snapshot database before update: %w", err)
		}
		dbBackup = snapPath
	}

	oldPath := binPath + ".old"
	_ = os.Remove(oldPath)
	if _, err := os.Stat(binPath); err == nil {
		// Rename works on both Linux and Windows even when binPath is currently executing.
		if err := os.Rename(binPath, oldPath); err != nil {
			return fmt.Errorf("backup existing binary: %w", err)
		}
	}

	if _, err := copyFile(newBinPath, binPath, 0755); err != nil {
		// Attempt immediate restore if placing the new binary fails.
		_ = os.Rename(oldPath, binPath)
		return fmt.Errorf("install new binary: %w", err)
	}

	marker := PendingMarker{
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		DBBackup:    dbBackup,
		CreatedAt:   time.Now().UTC(),
	}
	raw, _ := json.MarshalIndent(marker, "", "  ")
	return os.WriteFile(filepath.Join(dataDir, "update.pending"), raw, 0644)
}

// ConfirmHealthy clears the pending update and boot-attempt markers once startup succeeds.
func ConfirmHealthy(dataDir string) error {
	_ = os.Remove(filepath.Join(dataDir, "update.booting"))
	err := os.Remove(filepath.Join(dataDir, "update.pending"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// MarkBootingOrRecover protects startup after an update: on the first boot with update.pending
// it writes update.booting; if update.booting already exists (previous boot crashed), it rolls back.
func MarkBootingOrRecover(binPath, dataDir string) (bool, error) {
	pendingPath := filepath.Join(dataDir, "update.pending")
	if _, err := os.Stat(pendingPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	bootingPath := filepath.Join(dataDir, "update.booting")
	if _, err := os.Stat(bootingPath); err == nil {
		_ = os.Remove(bootingPath)
		if err := Rollback(binPath, dataDir); err != nil {
			return false, err
		}
		return true, nil
	}
	_ = os.WriteFile(bootingPath, []byte(time.Now().UTC().Format(time.RFC3339)), 0644)
	return false, nil
}

// Rollback restores binPath.old -> binPath and restores the saved DB snapshot when present.
func Rollback(binPath, dataDir string) error {
	oldPath := binPath + ".old"
	if _, err := os.Stat(oldPath); err != nil {
		return fmt.Errorf("no previous binary backup found at %s", oldPath)
	}

	// Read pending marker first to locate the exact pre-update DB snapshot if present.
	var dbSnap string
	pendingPath := filepath.Join(dataDir, "update.pending")
	if raw, err := os.ReadFile(pendingPath); err == nil {
		var pm PendingMarker
		if json.Unmarshal(raw, &pm) == nil {
			dbSnap = pm.DBBackup
		}
	}
	if dbSnap == "" {
		matches, _ := filepath.Glob(filepath.Join(dataDir, "backups", "ark.db.*"))
		if len(matches) > 0 {
			sort.Strings(matches)
			dbSnap = matches[len(matches)-1]
		}
	}

	failedBin := binPath + ".failed"
	_ = os.Remove(failedBin)
	_ = os.Rename(binPath, failedBin)
	if err := os.Rename(oldPath, binPath); err != nil {
		return fmt.Errorf("restore previous binary: %w", err)
	}

	if dbSnap != "" {
		if _, err := os.Stat(dbSnap); err == nil {
			dbPath := filepath.Join(dataDir, "ark.db")
			_ = os.Remove(dbPath + "-wal")
			_ = os.Remove(dbPath + "-shm")
			if _, err := copyFile(dbSnap, dbPath, 0600); err != nil {
				return fmt.Errorf("restore database snapshot: %w", err)
			}
		}
	}

	_ = os.Remove(pendingPath)
	_ = os.WriteFile(filepath.Join(dataDir, "update.rolledback"), []byte("Rolled back Ark binary and database snapshot."), 0644)
	return nil
}

// CheckAndRecoverOnStartup rolls back automatically if a previous update crashed before ConfirmHealthy.
func CheckAndRecoverOnStartup(binPath, dataDir string) (bool, error) {
	pendingPath := filepath.Join(dataDir, "update.pending")
	if _, err := os.Stat(pendingPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err := Rollback(binPath, dataDir); err != nil {
		return false, err
	}
	return true, nil
}

// HasRollbackAvailable reports whether a previous binary backup exists for rollback.
func HasRollbackAvailable(binPath string) bool {
	if binPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return false
		}
		binPath = exe
	}
	_, err := os.Stat(binPath + ".old")
	return err == nil
}

// ConsumeRollbackNotice reads and removes the rollback banner notice if present.
func ConsumeRollbackNotice(dataDir string) string {
	p := filepath.Join(dataDir, "update.rolledback")
	raw, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	_ = os.Remove(p)
	return strings.TrimSpace(string(raw))
}

func copyFile(src, dst string, mode os.FileMode) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	_ = os.Remove(dst)
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return dst, nil
}
