package kiwix

import (
	"context"
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

// DirectZimURL converts a .meta4 mirror URL to direct .zim URL if applicable.
func (e *OPDSEntry) DirectZimURL() string {
	dl := e.DownloadURL()
	return strings.TrimSuffix(dl, ".meta4")
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
	decoder := xml.NewDecoder(r)
	if err := decoder.Decode(&feed); err != nil {
		return nil, fmt.Errorf("decode OPDS feed: %w", err)
	}
	return &feed, nil
}

// FetchOPDSFeed fetches and parses OPDS feed entries from a given baseURL using filter parameters.
func FetchOPDSFeed(ctx context.Context, client *http.Client, baseURL string, params FilterParams) (*Feed, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
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
