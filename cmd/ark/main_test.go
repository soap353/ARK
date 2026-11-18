package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCLIHandlersUSBAndMoveData(t *testing.T) {
	srcData := t.TempDir()
	usbDest := filepath.Join(t.TempDir(), "usb")
	moveDest := filepath.Join(t.TempDir(), "moved")

	_ = os.MkdirAll(filepath.Join(srcData, "zim"), 0755)
	_ = os.WriteFile(filepath.Join(srcData, "zim", "test.zim"), []byte("offline-wiki"), 0644)

	dbPath := filepath.Join(srcData, "ark.db")
	db, err := initDatabase(dbPath)
	if err != nil {
		t.Fatalf("initDatabase failed: %v", err)
	}
	_ = db.Close()

	// Test ark usb --data-dir <srcData> --to <usbDest> --categories zim
	handleUSB([]string{"--data-dir", srcData, "--to", usbDest, "--categories", "zim"})
	if _, err := os.Stat(filepath.Join(usbDest, "manifest.json")); err != nil {
		t.Fatalf("expected manifest.json in usbDest, got err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(usbDest, "data", "zim", "test.zim")); err != nil {
		t.Fatalf("expected copied zim file in usbDest, got err: %v", err)
	}

	// Test ark move-data --data-dir <srcData> --to <moveDest>
	handleMoveData([]string{"--data-dir", srcData, "--to", moveDest})
	if _, err := os.Stat(filepath.Join(moveDest, "zim", "test.zim")); err != nil {
		t.Fatalf("expected moved zim file in moveDest, got err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moveDest, "ark.db")); err != nil {
		t.Fatalf("expected moved ark.db in moveDest, got err: %v", err)
	}

	// Test ark uninstall --all --keep-data preserves data dir
	handleUninstall([]string{"--all", "--keep-data", "--data-dir", moveDest})
	if _, err := os.Stat(filepath.Join(moveDest, "ark.db")); err != nil {
		t.Fatalf("expected ark.db preserved when --keep-data is set")
	}
}
