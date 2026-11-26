// Package kiwix handles Kiwix OPDS feed fetching/parsing, SQLite catalog caching,
// curated Ark preset/map bundles, and local library.xml management.
package kiwix

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultCatalogURL points to the public Kiwix OPDS feed.
const DefaultCatalogURL = "https://library.kiwix.org/catalog/v2/entries"

var sharedOPDSClient = &http.Client{Timeout: 30 * time.Second}

// Feed represents an Atom/OPDS catalog feed.
type Feed struct {
	XMLName      xml.Name    `xml:"feed"`
	ID           string      `xml:"id"`
	Title        string      `xml:"title"`
	Updated      string      `xml:"updated"`
	TotalResults int         `xml:"totalResults"`
	StartIndex   int         `xml:"startIndex"`
	ItemsPerPage int         `xml:"itemsPerPage"`
	Entries      []OPDSEntry `xml:"entry"`
}

// OPDSEntry represents an item in the Kiwix OPDS catalog.
type OPDSEntry struct {
	ID           string     `xml:"id"`
	Title        string     `xml:"title"`
	Updated      string     `xml:"updated"`
	Summary      string     `xml:"summary"`
	Language     string     `xml:"language"`
	Name         string     `xml:"name"`
	Flavour      string     `xml:"flavour"`
	Category     string     `xml:"category"`
	Tags         string     `xml:"tags"`
	ArticleCount int        `xml:"articleCount"`
	MediaCount   int        `xml:"mediaCount"`
	Publisher    string     `xml:"publisher>name"`
	Author       string     `xml:"author>name"`
	Links        []OPDSLink `xml:"link"`
}

// OPDSLink represents link relationships inside an OPDS entry.
type OPDSLink struct {
	Rel    string `xml:"rel,attr"`
	Type   string `xml:"type,attr"`
	Href   string `xml:"href,attr"`
	Length int64  `xml:"length,attr"`
}

// DownloadURL returns the primary ZIM download link (.meta4 or .zim).
func (e *OPDSEntry) DownloadURL() string {
	for _, link := range e.Links {
		if link.Type == "application/x-zim" || strings.HasSuffix(link.Href, ".zim") || strings.HasSuffix(link.Href, ".meta4") {
			return link.Href
		}
	}
	return ""
}

// DirectZimURL converts a .meta4 mirror URL to a direct .zim URL when applicable.
func (e *OPDSEntry) DirectZimURL() string {
	return strings.TrimSuffix(e.DownloadURL(), ".meta4")
}

// SizeBytes returns the content size in bytes from the acquisition link.
func (e *OPDSEntry) SizeBytes() int64 {
	for _, link := range e.Links {
		if (link.Type == "application/x-zim" || strings.HasSuffix(link.Href, ".zim") || strings.HasSuffix(link.Href, ".meta4")) && link.Length > 0 {
			return link.Length
		}
	}
	return 0
}

// ThumbnailURL returns the thumbnail image link if present.
func (e *OPDSEntry) ThumbnailURL() string {
	for _, link := range e.Links {
		if strings.Contains(link.Rel, "thumbnail") || strings.Contains(link.Rel, "image") {
			return link.Href
		}
	}
	return ""
}

// ParseOPDSFeed parses an OPDS XML feed from an io.Reader.
func ParseOPDSFeed(r io.Reader) (*Feed, error) {
	var feed Feed
	if err := xml.NewDecoder(r).Decode(&feed); err != nil {
		return nil, fmt.Errorf("decode OPDS feed: %w", err)
	}
	return &feed, nil
}

// FetchOPDSFeed fetches and parses OPDS feed entries from baseURL using filter parameters.
func FetchOPDSFeed(ctx context.Context, client *http.Client, baseURL string, params FilterParams) (*Feed, error) {
	if client == nil {
		client = sharedOPDSClient
	}
	if baseURL == "" {
		baseURL = DefaultCatalogURL
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base url: %w", err)
	}

	q := u.Query()
	if params.Query != "" {
		q.Set("q", params.Query)
	}
	if params.Language != "" {
		q.Set("lang", params.Language)
	}
	if params.Category != "" {
		q.Set("category", params.Category)
	}
	if params.Flavour != "" {
		q.Set("flavour", params.Flavour)
	}
	if params.Limit > 0 {
		q.Set("count", strconv.Itoa(params.Limit))
	}
	if params.Offset > 0 {
		q.Set("start", strconv.Itoa(params.Offset))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create opds request: %w", err)
	}
	req.Header.Set("User-Agent", "Ark-CommandCenter/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch opds feed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opds feed error: status code %d", resp.StatusCode)
	}

	return ParseOPDSFeed(resp.Body)
}

// CatalogItem corresponds to the catalog_items table.
type CatalogItem struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Category     string    `json:"category"`
	Language     string    `json:"language"`
	SizeBytes    int64     `json:"size_bytes"`
	Date         string    `json:"date"`
	Flavour      string    `json:"flavour"`
	URL          string    `json:"url"`
	IsDownloaded bool      `json:"is_downloaded"`
	FilePath     string    `json:"file_path"`
	CreatedAt    time.Time `json:"created_at"`
}

// FilterParams holds search and filtering options for catalog browsing.
type FilterParams struct {
	Query          string `json:"query"`
	Language       string `json:"language"`
	Category       string `json:"category"`
	Flavour        string `json:"flavour"`
	MinSize        int64  `json:"min_size"`
	MaxSize        int64  `json:"max_size"`
	DownloadedOnly bool   `json:"downloaded_only"`
	Offset         int    `json:"offset"`
	Limit          int    `json:"limit"`
}

// CatalogCache provides SQLite caching and querying for Kiwix OPDS catalog entries.
type CatalogCache struct {
	db *sql.DB
}

// NewCatalogCache creates a new catalog cache instance.
func NewCatalogCache(db *sql.DB) *CatalogCache {
	return &CatalogCache{db: db}
}

// EnsureTable ensures the catalog_items table and indexes exist.
func (c *CatalogCache) EnsureTable(ctx context.Context) error {
	schema := `
	CREATE TABLE IF NOT EXISTS catalog_items (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		category TEXT NOT NULL,
		language TEXT NOT NULL,
		size_bytes INTEGER NOT NULL DEFAULT 0,
		date TEXT NOT NULL DEFAULT '',
		flavour TEXT NOT NULL DEFAULT '',
		url TEXT NOT NULL DEFAULT '',
		is_downloaded BOOLEAN NOT NULL DEFAULT 0,
		file_path TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_catalog_category ON catalog_items(category);
	CREATE INDEX IF NOT EXISTS idx_catalog_language ON catalog_items(language);
	`
	_, err := c.db.ExecContext(ctx, schema)
	return err
}

// UpsertEntries inserts or updates parsed OPDS entries into the SQLite cache.
func (c *CatalogCache) UpsertEntries(ctx context.Context, entries []OPDSEntry) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO catalog_items (id, title, category, language, size_bytes, date, flavour, url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			category = excluded.category,
			language = excluded.language,
			size_bytes = excluded.size_bytes,
			date = excluded.date,
			flavour = excluded.flavour,
			url = excluded.url
	`)
	if err != nil {
		return fmt.Errorf("prepare upsert stmt: %w", err)
	}
	defer stmt.Close()

	for _, entry := range entries {
		downloadURL := entry.DownloadURL()
		size := entry.SizeBytes()
		category := entry.Category
		if category == "" && entry.Tags != "" {
			parts := strings.Split(entry.Tags, ";")
			if len(parts) > 0 && !strings.HasPrefix(parts[0], "_") {
				category = parts[0]
			}
		}

		cleanID := entry.ID
		if strings.HasPrefix(cleanID, "urn:uuid:") {
			cleanID = strings.TrimPrefix(cleanID, "urn:uuid:")
		}

		date := entry.Updated
		if len(date) > 10 {
			date = date[:10]
		}

		_, err := stmt.ExecContext(ctx,
			cleanID,
			entry.Title,
			category,
			entry.Language,
			size,
			date,
			entry.Flavour,
			downloadURL,
		)
		if err != nil {
			return fmt.Errorf("exec upsert: %w", err)
		}
	}

	return tx.Commit()
}

// Search queries the cached catalog entries with search and filter parameters.
func (c *CatalogCache) Search(ctx context.Context, params FilterParams) ([]CatalogItem, int, error) {
	whereClauses := []string{"1=1"}
	args := []any{}

	if params.Query != "" {
		whereClauses = append(whereClauses, "(title LIKE ? OR id LIKE ?)")
		pattern := "%" + params.Query + "%"
		args = append(args, pattern, pattern)
	}
	if params.Language != "" {
		whereClauses = append(whereClauses, "language = ?")
		args = append(args, params.Language)
	}
	if params.Category != "" {
		whereClauses = append(whereClauses, "category LIKE ?")
		args = append(args, "%"+params.Category+"%")
	}
	if params.Flavour != "" {
		whereClauses = append(whereClauses, "flavour = ?")
		args = append(args, params.Flavour)
	}
	if params.MinSize > 0 {
		whereClauses = append(whereClauses, "size_bytes >= ?")
		args = append(args, params.MinSize)
	}
	if params.MaxSize > 0 {
		whereClauses = append(whereClauses, "size_bytes <= ?")
		args = append(args, params.MaxSize)
	}
	if params.DownloadedOnly {
		whereClauses = append(whereClauses, "is_downloaded = 1")
	}

	whereSQL := strings.Join(whereClauses, " AND ")

	// Count total matching
	countSQL := "SELECT COUNT(*) FROM catalog_items WHERE " + whereSQL
	var total int
	if err := c.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count catalog items: %w", err)
	}

	// Fetch page
	querySQL := "SELECT id, title, category, language, size_bytes, date, flavour, url, is_downloaded, file_path, created_at FROM catalog_items WHERE " + whereSQL + " ORDER BY size_bytes DESC"
	if params.Limit > 0 {
		querySQL += fmt.Sprintf(" LIMIT %d", params.Limit)
		if params.Offset > 0 {
			querySQL += fmt.Sprintf(" OFFSET %d", params.Offset)
		}
	}

	rows, err := c.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query catalog items: %w", err)
	}
	defer rows.Close()

	var items []CatalogItem
	for rows.Next() {
		var it CatalogItem
		var createdAtStr string
		if err := rows.Scan(
			&it.ID,
			&it.Title,
			&it.Category,
			&it.Language,
			&it.SizeBytes,
			&it.Date,
			&it.Flavour,
			&it.URL,
			&it.IsDownloaded,
			&it.FilePath,
			&createdAtStr,
		); err != nil {
			return nil, 0, fmt.Errorf("scan catalog item: %w", err)
		}
		if t, err := time.Parse("2006-01-02 15:04:05", createdAtStr); err == nil {
			it.CreatedAt = t
		} else if t, err := time.Parse(time.RFC3339, createdAtStr); err == nil {
			it.CreatedAt = t
		}
		items = append(items, it)
	}

	return items, total, nil
}

// GetByID retrieves a single catalog item by ID.
func (c *CatalogCache) GetByID(ctx context.Context, id string) (*CatalogItem, error) {
	cleanID := strings.TrimPrefix(id, "urn:uuid:")
	row := c.db.QueryRowContext(ctx, `
		SELECT id, title, category, language, size_bytes, date, flavour, url, is_downloaded, file_path, created_at
		FROM catalog_items
		WHERE id = ?
	`, cleanID)

	var it CatalogItem
	var createdAtStr string
	if err := row.Scan(
		&it.ID,
		&it.Title,
		&it.Category,
		&it.Language,
		&it.SizeBytes,
		&it.Date,
		&it.Flavour,
		&it.URL,
		&it.IsDownloaded,
		&it.FilePath,
		&createdAtStr,
	); err != nil {
		return nil, err
	}
	return &it, nil
}

// MarkDownloaded updates an item's download state and file path.
func (c *CatalogCache) MarkDownloaded(ctx context.Context, id string, filePath string) error {
	cleanID := strings.TrimPrefix(id, "urn:uuid:")
	_, err := c.db.ExecContext(ctx, `
		UPDATE catalog_items
		SET is_downloaded = 1, file_path = ?
		WHERE id = ?
	`, filePath, cleanID)
	return err
}

// MarkDeleted marks an item as not downloaded and clears its file path.
func (c *CatalogCache) MarkDeleted(ctx context.Context, id string) error {
	cleanID := strings.TrimPrefix(id, "urn:uuid:")
	_, err := c.db.ExecContext(ctx, `
		UPDATE catalog_items
		SET is_downloaded = 0, file_path = ''
		WHERE id = ?
	`, cleanID)
	return err
}

// Delete removes an item completely from the catalog cache.
func (c *CatalogCache) Delete(ctx context.Context, id string) error {
	cleanID := strings.TrimPrefix(id, "urn:uuid:")
	_, err := c.db.ExecContext(ctx, "DELETE FROM catalog_items WHERE id = ?", cleanID)
	return err
}

// Count returns the total number of cached entries.
func (c *CatalogCache) Count(ctx context.Context) (int, error) {
	var count int
	err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM catalog_items").Scan(&count)
	return count, err
}
