package kiwix

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// LibraryXML is the root XML structure for Kiwix library.xml files.
type LibraryXML struct {
	XMLName xml.Name      `xml:"library"`
	Version string        `xml:"version,attr,omitempty"`
	Books   []LibraryBook `xml:"book"`
}

// LibraryBook represents a single book / ZIM in library.xml.
type LibraryBook struct {
	ID           string `xml:"id,attr"`
	Path         string `xml:"path,attr"`
	Title        string `xml:"title,attr,omitempty"`
	Description  string `xml:"description,attr,omitempty"`
	Language     string `xml:"language,attr,omitempty"`
	Creator      string `xml:"creator,attr,omitempty"`
	Publisher    string `xml:"publisher,attr,omitempty"`
	Date         string `xml:"date,attr,omitempty"`
	Size         int64  `xml:"size,attr,omitempty"`
	ArticleCount int    `xml:"articleCount,attr,omitempty"`
	MediaCount   int    `xml:"mediaCount,attr,omitempty"`
	Tags         string `xml:"tags,attr,omitempty"`
	URL          string `xml:"url,attr,omitempty"`
}

// LibraryManager manages Kiwix library.xml file additions and deletions.
type LibraryManager struct {
	mu             sync.Mutex
	LibraryXMLPath string
	KiwixManageBin string
}

// NewLibraryManager creates a new library manager instance.
func NewLibraryManager(libraryXMLPath, kiwixManageBin string) *LibraryManager {
	if kiwixManageBin == "" {
		if p, err := exec.LookPath("kiwix-manage"); err == nil {
			kiwixManageBin = p
		}
	}
	return &LibraryManager{
		LibraryXMLPath: libraryXMLPath,
		KiwixManageBin: kiwixManageBin,
	}
}

// EnsureLibraryFile ensures the library.xml file and its directory exist.
func (lm *LibraryManager) EnsureLibraryFile() error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	dir := filepath.Dir(lm.LibraryXMLPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create library dir: %w", err)
	}

	if _, err := os.Stat(lm.LibraryXMLPath); errors.Is(err, os.ErrNotExist) {
		initial := LibraryXML{
			Version: "2.0",
			Books:   []LibraryBook{},
		}
		data, err := xml.MarshalIndent(initial, "", "  ")
		if err != nil {
			return err
		}
		data = append([]byte(xml.Header), data...)
		return os.WriteFile(lm.LibraryXMLPath, data, 0644)
	}
	return nil
}

// AddZIM adds a ZIM file to library.xml using kiwix-manage, falling back to direct XML update.
func (lm *LibraryManager) AddZIM(ctx context.Context, zimPath string, bookMeta *LibraryBook) error {
	if err := lm.EnsureLibraryFile(); err != nil {
		return err
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	// Try kiwix-manage CLI if available
	if lm.KiwixManageBin != "" {
		cmd := exec.CommandContext(ctx, lm.KiwixManageBin, lm.LibraryXMLPath, "add", zimPath)
		if output, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else {
			// If kiwix-manage failed (e.g. file format issue or bad binary), fall through to pure-Go XML edit
			_ = output
		}
	}

	// Pure Go fallback
	lib, err := lm.readLibraryUnsafe()
	if err != nil {
		lib = &LibraryXML{Version: "2.0"}
	}

	absZimPath, _ := filepath.Abs(zimPath)
	absLibDir, _ := filepath.Abs(filepath.Dir(lm.LibraryXMLPath))
	cleanPath := filepath.Clean(zimPath)
	if absZimPath != "" && absLibDir != "" && filepath.Dir(absZimPath) == absLibDir {
		cleanPath = filepath.Base(absZimPath)
	}

	// Check if already present
	for i, b := range lib.Books {
		if b.Path == zimPath || b.Path == absZimPath || b.Path == cleanPath || filepath.Base(b.Path) == filepath.Base(cleanPath) || (bookMeta != nil && b.ID == bookMeta.ID) {
			if bookMeta != nil {
				lib.Books[i] = *bookMeta
				lib.Books[i].Path = cleanPath
			}
			return lm.writeLibraryUnsafe(lib)
		}
	}

	// Add new entry
	newBook := LibraryBook{
		ID:   filepath.Base(zimPath),
		Path: cleanPath,
	}
	if fi, err := os.Stat(zimPath); err == nil {
		newBook.Size = fi.Size()
	}
	if bookMeta != nil {
		newBook = *bookMeta
		newBook.Path = cleanPath
	}

	lib.Books = append(lib.Books, newBook)
	return lm.writeLibraryUnsafe(lib)
}

// RemoveZIM removes a ZIM from library.xml using kiwix-manage or direct XML purge.
func (lm *LibraryManager) RemoveZIM(ctx context.Context, idOrPath string) error {
	if err := lm.EnsureLibraryFile(); err != nil {
		return err
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	// Try kiwix-manage CLI if available
	if lm.KiwixManageBin != "" {
		cmd := exec.CommandContext(ctx, lm.KiwixManageBin, lm.LibraryXMLPath, "remove", idOrPath)
		if output, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else {
			_ = output
		}
	}

	// Pure Go fallback
	lib, err := lm.readLibraryUnsafe()
	if err != nil {
		return err
	}

	cleanTarget := filepath.Clean(idOrPath)
	baseTarget := filepath.Base(idOrPath)

	var filtered []LibraryBook
	for _, b := range lib.Books {
		if b.ID == idOrPath || b.Path == idOrPath || filepath.Clean(b.Path) == cleanTarget || filepath.Base(b.Path) == baseTarget {
			continue // skip (remove)
		}
		filtered = append(filtered, b)
	}

	lib.Books = filtered
	return lm.writeLibraryUnsafe(lib)
}

// ListBooks returns all books currently registered in library.xml.
func (lm *LibraryManager) ListBooks() ([]LibraryBook, error) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	lib, err := lm.readLibraryUnsafe()
	if err != nil {
		return nil, err
	}
	return lib.Books, nil
}

func (lm *LibraryManager) readLibraryUnsafe() (*LibraryXML, error) {
	data, err := os.ReadFile(lm.LibraryXMLPath)
	if err != nil {
		return nil, err
	}

	var lib LibraryXML
	if err := xml.Unmarshal(data, &lib); err != nil {
		return nil, fmt.Errorf("unmarshal library.xml: %w", err)
	}
	return &lib, nil
}

func (lm *LibraryManager) writeLibraryUnsafe(lib *LibraryXML) error {
	data, err := xml.MarshalIndent(lib, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal library.xml: %w", err)
	}

	content := append([]byte(xml.Header), data...)
	content = append(content, '\n')
	return os.WriteFile(lm.LibraryXMLPath, content, 0644)
}

// CleanPathIDHelper returns a normalized string identifier for matching ZIM records.
func CleanPathIDHelper(val string) string {
	s := strings.TrimSpace(val)
	s = strings.TrimPrefix(s, "urn:uuid:")
	return s
}
