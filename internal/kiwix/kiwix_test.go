package kiwix

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const sampleOPDS = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"
      xmlns:dc="http://purl.org/dc/terms/"
      xmlns:opds="https://specs.opds.io/opds-1.2"
      xmlns:opensearch="http://a9.com/-/spec/opensearch/1.1/">
  <id>test-feed-id</id>
  <title>Filtered Entries</title>
  <updated>2026-11-12T00:00:00Z</updated>
  <totalResults>2</totalResults>
  <entry>
    <id>urn:uuid:001eb72b-e815-af2c-7225-f9ad60b5c891</id>
    <title>WikiMed Medicine</title>
    <updated>2024-11-12T00:00:00Z</updated>
    <summary>Medical encyclopedia for offline doctors</summary>
    <language>eng</language>
    <name>wikimed_en_all</name>
    <flavour>maxi</flavour>
    <category>medical</category>
    <tags>Medicine;_videos:no</tags>
    <link rel="http://opds-spec.org/acquisition/open-access" type="application/x-zim" href="https://lb.download.kiwix.org/zim/wikimed_en.zim.meta4" length="5000000000" />
  </entry>
  <entry>
    <id>urn:uuid:11223344-5566-7788-99aa-bbccddeeff00</id>
    <title>Wikipedia Mini</title>
    <updated>2024-10-01T00:00:00Z</updated>
    <summary>Lead paragraphs of English Wikipedia</summary>
    <language>eng</language>
    <name>wikipedia_en_mini</name>
    <flavour>mini</flavour>
    <category>knowledge</category>
    <tags>Wikipedia</tags>
    <link rel="http://opds-spec.org/acquisition/open-access" type="application/x-zim" href="https://lb.download.kiwix.org/zim/wikipedia_en_mini.zim" length="12000000000" />
  </entry>
</feed>`

func TestParseOPDS(t *testing.T) {
	feed, err := ParseOPDSFeed(strings.NewReader(sampleOPDS))
	if err != nil {
		t.Fatalf("ParseOPDSFeed failed: %v", err)
	}

	if feed.TotalResults != 2 {
		t.Errorf("Expected TotalResults 2, got %d", feed.TotalResults)
	}
	if len(feed.Entries) != 2 {
		t.Fatalf("Expected 2 entries, got %d", len(feed.Entries))
	}

	entry1 := feed.Entries[0]
	if entry1.Title != "WikiMed Medicine" {
		t.Errorf("Unexpected title: %s", entry1.Title)
	}
	if entry1.DownloadURL() != "https://lb.download.kiwix.org/zim/wikimed_en.zim.meta4" {
		t.Errorf("Unexpected dl url: %s", entry1.DownloadURL())
	}
	if entry1.DirectZimURL() != "https://lb.download.kiwix.org/zim/wikimed_en.zim" {
		t.Errorf("Unexpected direct zim url: %s", entry1.DirectZimURL())
	}
	if entry1.SizeBytes() != 5000000000 {
		t.Errorf("Unexpected size: %d", entry1.SizeBytes())
	}
}

func TestCatalogCache(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Open in-memory db: %v", err)
	}
	defer db.Close()

	cache := NewCatalogCache(db)
	if err := cache.EnsureTable(ctx); err != nil {
		t.Fatalf("EnsureTable failed: %v", err)
	}

	feed, err := ParseOPDSFeed(strings.NewReader(sampleOPDS))
	if err != nil {
		t.Fatalf("Parse sample OPDS: %v", err)
	}

	if err := cache.UpsertEntries(ctx, feed.Entries); err != nil {
		t.Fatalf("UpsertEntries failed: %v", err)
	}

	count, err := cache.Count(ctx)
	if err != nil {
		t.Fatalf("Count failed: %v", err)
	}
	if count != 2 {
		t.Fatalf("Expected count 2, got %d", count)
	}

	// Search by query
	items, total, err := cache.Search(ctx, FilterParams{Query: "Medicine"})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("Expected 1 result for 'Medicine', got %d", total)
	}
	if items[0].Title != "WikiMed Medicine" {
		t.Errorf("Unexpected item title: %s", items[0].Title)
	}

	// Filter by language & flavour
	items, total, err = cache.Search(ctx, FilterParams{Language: "eng", Flavour: "mini"})
	if err != nil {
		t.Fatalf("Search with filter failed: %v", err)
	}
	if total != 1 || items[0].Title != "Wikipedia Mini" {
		t.Errorf("Expected Wikipedia Mini, got %v", items)
	}

	// Mark downloaded
	id := items[0].ID
	if err := cache.MarkDownloaded(ctx, id, "/data/zim/wikipedia_mini.zim"); err != nil {
		t.Fatalf("MarkDownloaded failed: %v", err)
	}

	updated, err := cache.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if !updated.IsDownloaded || updated.FilePath != "/data/zim/wikipedia_mini.zim" {
		t.Errorf("MarkDownloaded not persisted correctly: %+v", updated)
	}

	// DownloadedOnly filter
	dlItems, dlTotal, err := cache.Search(ctx, FilterParams{DownloadedOnly: true})
	if err != nil {
		t.Fatalf("Search downloaded only failed: %v", err)
	}
	if dlTotal != 1 || dlItems[0].ID != id {
		t.Errorf("Expected 1 downloaded item, got %d", dlTotal)
	}

	// Mark deleted
	if err := cache.MarkDeleted(ctx, id); err != nil {
		t.Fatalf("MarkDeleted failed: %v", err)
	}
	updated, _ = cache.GetByID(ctx, id)
	if updated.IsDownloaded || updated.FilePath != "" {
		t.Errorf("Expected item marked deleted, got %+v", updated)
	}
}

func TestPresets(t *testing.T) {
	tiers := GetAllPresetTiers()
	if len(tiers) == 0 {
		t.Fatal("Expected preset tiers to be non-empty")
	}

	starterPack := GetStarterPack()
	if starterPack.ID != "prepper-starter-pack" {
		t.Errorf("Unexpected bundle ID: %s", starterPack.ID)
	}
	if len(starterPack.Items) != 4 {
		t.Errorf("Expected 4 items in starter pack, got %d", len(starterPack.Items))
	}
	if starterPack.EstimatedTotalSize <= 0 {
		t.Errorf("Expected positive total size, got %d", starterPack.EstimatedTotalSize)
	}

	found := FindPresetByID("wikimed-en")
	if found == nil || found.Name != "WikiMed (Medical Encyclopedia)" {
		t.Errorf("Expected to find wikimed-en preset, got %v", found)
	}

	if opts := GetWikipediaOptions(); len(opts) != 5 {
		t.Errorf("Expected 5 Wikipedia options, got %d", len(opts))
	}
	if cols := GetNomadCollections(); len(cols) != 6 {
		t.Errorf("Expected 6 NOMAD collections, got %d", len(cols))
	} else {
		for _, c := range cols {
			if len(c.Tiers) != 3 {
				t.Errorf("Expected 3 tiers for collection %s, got %d", c.Slug, len(c.Tiers))
			}
		}
	}
	if regs := GetNomadMapRegions(); len(regs) != 9 {
		t.Errorf("Expected 9 NOMAD map regions, got %d", len(regs))
	}

	medStd := ResolvePresetResources("medicine-standard")
	if len(medStd) < 5 {
		t.Errorf("Expected medicine-standard to include inherited essential items (>=5), got %d", len(medStd))
	}
	pacific := ResolvePresetResources("map-pacific")
	if len(pacific) != 5 || pacific[0].Kind != "pmtiles" {
		t.Errorf("Expected 5 pmtiles items for map-pacific, got %+v", pacific)
	}
}

func TestLibraryManager(t *testing.T) {
	tempDir := t.TempDir()
	xmlPath := filepath.Join(tempDir, "library.xml")

	lm := NewLibraryManager(xmlPath, "")

	ctx := context.Background()
	meta := &LibraryBook{
		ID:          "test-book-1",
		Path:        filepath.Join(tempDir, "test1.zim"),
		Title:       "Test ZIM 1",
		Description: "A test book",
		Language:    "eng",
		Size:        1024,
	}

	// Add
	if err := lm.AddZIM(ctx, meta.Path, meta); err != nil {
		t.Fatalf("AddZIM failed: %v", err)
	}

	books, err := lm.ListBooks()
	if err != nil {
		t.Fatalf("ListBooks failed: %v", err)
	}
	if len(books) != 1 || books[0].ID != "test-book-1" {
		t.Fatalf("Expected 1 book with ID test-book-1, got %v", books)
	}

	// Add second book
	zim2Path := filepath.Join(tempDir, "test2.zim")
	_ = os.WriteFile(zim2Path, []byte("fake zim data"), 0644)
	if err := lm.AddZIM(ctx, zim2Path, nil); err != nil {
		t.Fatalf("AddZIM for second book failed: %v", err)
	}

	books, err = lm.ListBooks()
	if err != nil {
		t.Fatalf("ListBooks failed: %v", err)
	}
	if len(books) != 2 {
		t.Fatalf("Expected 2 books, got %d", len(books))
	}

	// Remove first book
	if err := lm.RemoveZIM(ctx, "test-book-1"); err != nil {
		t.Fatalf("RemoveZIM failed: %v", err)
	}

	books, err = lm.ListBooks()
	if err != nil {
		t.Fatalf("ListBooks failed: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("Expected 1 book remaining, got %d", len(books))
	}
}
