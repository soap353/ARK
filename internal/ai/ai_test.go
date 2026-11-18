package ai

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"127.0.0.1:11434", "http://127.0.0.1:11434/v1"},
		{"http://192.168.1.50:1234/", "http://192.168.1.50:1234/v1"},
		{"https://ai.local/v1/", "https://ai.local/v1"},
	}
	for _, tc := range cases {
		if got := NormalizeBaseURL(tc.in); got != tc.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOpenAICompatibleClient(t *testing.T) {
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"llama3.2:3b"},{"id":"nomic-embed-text"}]}`))
		case "/v1/embeddings":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Ark!\"}}]}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	conn := Conn{
		BaseURL:    strings.TrimPrefix(srv.URL, "http://"),
		APIKey:     "secret-key",
		ChatModel:  "llama3.2:3b",
		EmbedModel: "nomic-embed-text",
	}

	ctx := context.Background()
	models, err := conn.Models(ctx)
	if err != nil {
		t.Fatalf("Models failed: %v", err)
	}
	if authHeader != "Bearer secret-key" {
		t.Errorf("expected Bearer auth header, got %q", authHeader)
	}
	if len(models) != 2 || models[0] != "llama3.2:3b" {
		t.Errorf("unexpected models: %+v", models)
	}

	vecs, err := conn.Embed(ctx, []string{"test text"})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 3 {
		t.Errorf("unexpected embeddings: %+v", vecs)
	}

	var streamed strings.Builder
	full, err := conn.ChatStream(ctx, []Msg{{Role: "user", Content: "Hi"}}, func(tok string) {
		streamed.WriteString(tok)
	})
	if err != nil {
		t.Fatalf("ChatStream failed: %v", err)
	}
	if full != "Hello Ark!" || streamed.String() != "Hello Ark!" {
		t.Errorf("ChatStream = %q / %q, want %q", full, streamed.String(), "Hello Ark!")
	}
}

func TestQdrantAndIndexerPipeline(t *testing.T) {
	var upsertedCount int32
	var deletedCalled int32
	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections":
			_, _ = w.Write([]byte(`{"result":{"collections":[]}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/collections/ark_knowledge":
			http.NotFound(w, r)
		case r.Method == http.MethodPut && r.URL.Path == "/collections/ark_knowledge":
			_, _ = w.Write([]byte(`{"result":true}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/ark_knowledge/points":
			var body struct {
				Points []VectorPoint `json:"points"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			atomic.AddInt32(&upsertedCount, int32(len(body.Points)))
			_, _ = w.Write([]byte(`{"result":{"status":"completed"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/collections/ark_knowledge/points/delete":
			atomic.AddInt32(&deletedCalled, 1)
			_, _ = w.Write([]byte(`{"result":{"status":"completed"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/collections/ark_knowledge/points/search":
			_, _ = w.Write([]byte(`{"result":[{"id":"1","score":0.95,"payload":{"source_path":"guide.html","title":"Water Purification","text":"Boil water for 1 minute.","chunk_index":0}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer qdrantSrv.Close()

	aiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.25,0.5,0.75]}]}`))
	}))
	defer aiSrv.Close()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	tmpDir := t.TempDir()
	htmlPath := filepath.Join(tmpDir, "guide.html")
	htmlContent := `<html><head><title>Water Purification</title><script>var bad=1;</script></head><body><h1>Field Guide</h1><p>Boil water for at least 1 minute at sea level to disinfect it.</p></body></html>`
	if err := os.WriteFile(htmlPath, []byte(htmlContent), 0644); err != nil {
		t.Fatalf("write html: %v", err)
	}

	conn := Conn{BaseURL: aiSrv.URL, EmbedModel: "nomic-embed-text"}
	qdrant := NewQdrantClient(qdrantSrv.URL)
	ctx := context.Background()

	// 1. Index file
	n, err := IndexSource(ctx, db, conn, qdrant, DefaultCollection, "html", htmlPath, "", nil)
	if err != nil {
		t.Fatalf("IndexSource failed: %v", err)
	}
	if n != 1 || atomic.LoadInt32(&upsertedCount) != 1 {
		t.Fatalf("expected 1 indexed & upserted chunk, got n=%d upserted=%d", n, atomic.LoadInt32(&upsertedCount))
	}

	// 2. Second run with same file content skips cleanly (incremental hashing)
	n2, err := IndexSource(ctx, db, conn, qdrant, DefaultCollection, "html", htmlPath, "", nil)
	if err != nil {
		t.Fatalf("second IndexSource failed: %v", err)
	}
	if n2 != 0 {
		t.Errorf("expected 0 re-indexed chunks for unchanged file, got %d", n2)
	}

	// 3. Vector search hits Qdrant
	hits, err := SearchKnowledge(ctx, db, conn, qdrant, DefaultCollection, "how to boil water", 3)
	if err != nil || len(hits) == 0 {
		t.Fatalf("SearchKnowledge failed: err=%v hits=%v", err, hits)
	}
	if hits[0].Title != "Water Purification" {
		t.Errorf("unexpected hit title: %q", hits[0].Title)
	}

	// 4. Local SQLite keyword fallback works when Qdrant/AI are offline
	hitsLocal, err := SearchKnowledge(ctx, db, Conn{}, nil, DefaultCollection, "disinfect water", 3)
	if err != nil || len(hitsLocal) == 0 {
		t.Fatalf("local fallback SearchKnowledge failed: err=%v hits=%v", err, hitsLocal)
	}

	// 5. Remove document cleans DB and Qdrant
	docs, err := ListDocuments(ctx, db)
	if err != nil || len(docs) != 1 {
		t.Fatalf("expected 1 indexed doc, got %d (err=%v)", len(docs), err)
	}
	if err := RemoveDocument(ctx, db, qdrant, docs[0].ID); err != nil {
		t.Fatalf("RemoveDocument failed: %v", err)
	}
	if atomic.LoadInt32(&deletedCalled) == 0 {
		t.Errorf("expected Qdrant delete to be called")
	}
}

func buildTestZIMv6Zstd(t *testing.T, articles [][2]string) []byte {
	t.Helper()
	// Concatenate blobs and build cluster offsets
	var blobData bytes.Buffer
	numBlobs := len(articles)
	offs := make([]uint32, numBlobs+1)
	tableSize := uint32((numBlobs + 1) * 4)
	for i, a := range articles {
		offs[i] = tableSize + uint32(blobData.Len())
		blobData.WriteString("<html><head><title>" + a[0] + "</title></head><body><p>" + a[1] + "</p></body></html>")
	}
	offs[numBlobs] = tableSize + uint32(blobData.Len())

	var uncompressed bytes.Buffer
	for _, off := range offs {
		_ = binary.Write(&uncompressed, binary.LittleEndian, off)
	}
	uncompressed.Write(blobData.Bytes())

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer enc.Close()

	var clusterBuf bytes.Buffer
	clusterBuf.WriteByte(5) // comp=5 (Zstd), ext=0
	clusterBuf.Write(enc.EncodeAll(uncompressed.Bytes(), nil))

	mimeList := []byte("text/html;raw=true\x00\x00")
	mimePos := uint64(80)
	dirStart := mimePos + uint64(len(mimeList))

	var dirBuf bytes.Buffer
	entryOffs := make([]uint64, len(articles))
	for i, a := range articles {
		entryOffs[i] = dirStart + uint64(dirBuf.Len())
		_ = binary.Write(&dirBuf, binary.LittleEndian, uint16(0)) // mimeID 0
		dirBuf.WriteByte(0)                                       // paramLen
		dirBuf.WriteByte('C')                                     // namespace
		_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0)) // revision
		_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(0)) // cluster 0
		_ = binary.Write(&dirBuf, binary.LittleEndian, uint32(i)) // blob i
		dirBuf.WriteString("A_" + fmt.Sprintf("%d", i) + "\x00")  // url
		dirBuf.WriteString(a[0] + "\x00")                         // title
	}

	urlPtrPos := dirStart + uint64(dirBuf.Len())
	clusterPtrPos := urlPtrPos + uint64(len(articles)*8)
	clusterDataPos := clusterPtrPos + 8

	var out bytes.Buffer
	var hdr [80]byte
	binary.LittleEndian.PutUint32(hdr[0:4], ZIMMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], 6)
	binary.LittleEndian.PutUint16(hdr[6:8], 3)
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(len(articles)))
	binary.LittleEndian.PutUint32(hdr[28:32], 1)
	binary.LittleEndian.PutUint64(hdr[32:40], urlPtrPos)
	binary.LittleEndian.PutUint64(hdr[40:48], urlPtrPos)
	binary.LittleEndian.PutUint64(hdr[48:56], clusterPtrPos)
	binary.LittleEndian.PutUint64(hdr[56:64], mimePos)
	binary.LittleEndian.PutUint64(hdr[72:80], clusterDataPos+uint64(clusterBuf.Len()))

	out.Write(hdr[:])
	out.Write(mimeList)
	out.Write(dirBuf.Bytes())
	for _, off := range entryOffs {
		_ = binary.Write(&out, binary.LittleEndian, off)
	}
	_ = binary.Write(&out, binary.LittleEndian, clusterDataPos)
	out.Write(clusterBuf.Bytes())
	return out.Bytes()
}

func TestNativeZIMAndEPUBExtractionAndFTSSearch(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(tmpDir, "rag_native.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	// 1. ZIM archive (v6, Zstd-compressed)
	zimBytes := buildTestZIMv6Zstd(t, [][2]string{
		{"Penicillin", strings.Repeat("Penicillin is a beta-lactam antibiotic discovered by Alexander Fleming that treats bacterial infections. ", 2)},
		{"Maxwell Equations", strings.Repeat("Maxwell equations describe classical electromagnetism using divergence and curl of electric and magnetic fields. ", 2)},
	})
	zimPath := filepath.Join(tmpDir, "mini_wiki.zim")
	if err := os.WriteFile(zimPath, zimBytes, 0o644); err != nil {
		t.Fatalf("write zim: %v", err)
	}

	ctx := context.Background()
	n, err := IndexSource(ctx, db, Conn{}, nil, DefaultCollection, "zim", zimPath, "", nil)
	if err != nil || n < 2 {
		t.Fatalf("IndexSource zim failed: n=%d err=%v", n, err)
	}

	docs, err := ListDocuments(ctx, db)
	if err != nil || len(docs) != 1 || docs[0].SourceType != "zim" {
		t.Fatalf("expected 1 archive rag_documents row, got %+v (err=%v)", docs, err)
	}

	hits, err := SearchKnowledge(ctx, db, Conn{}, nil, DefaultCollection, "Alexander Fleming antibiotic", 3)
	if err != nil || len(hits) == 0 || !strings.Contains(hits[0].Title, "Penicillin") {
		t.Fatalf("expected FTS5 hit on Penicillin, got err=%v hits=%+v", err, hits)
	}

	// 2. EPUB archive (ZIP with XHTML chapter)
	epubPath := filepath.Join(tmpDir, "field_manual.epub")
	ef, err := os.Create(epubPath)
	if err != nil {
		t.Fatalf("create epub: %v", err)
	}
	zw := zip.NewWriter(ef)
	ch, _ := zw.Create("OEBPS/ch1.xhtml")
	_, _ = ch.Write([]byte("<html><head><title>Solar Geometry</title></head><body><p>" + strings.Repeat("Photovoltaic panels generate DC electricity when exposed to photons above the semiconductor bandgap energy. ", 2) + "</p></body></html>"))
	_ = zw.Close()
	_ = ef.Close()

	n2, err := IndexSource(ctx, db, Conn{}, nil, DefaultCollection, "epub", epubPath, "", nil)
	if err != nil || n2 < 1 {
		t.Fatalf("IndexSource epub failed: n=%d err=%v", n2, err)
	}

	hits2, err := SearchKnowledge(ctx, db, Conn{}, nil, DefaultCollection, "semiconductor bandgap photovoltaic", 3)
	if err != nil || len(hits2) == 0 || !strings.Contains(hits2[0].Title, "Solar Geometry") {
		t.Fatalf("expected FTS5 hit on Solar Geometry, got err=%v hits=%+v", err, hits2)
	}

	if err := RemoveDocument(ctx, db, nil, docs[0].ID); err != nil {
		t.Fatalf("RemoveDocument failed: %v", err)
	}
	hitsAfter, _ := SearchKnowledge(ctx, db, Conn{}, nil, DefaultCollection, "Fleming penicillin", 3)
	if len(hitsAfter) != 0 {
		t.Errorf("expected removed ZIM FTS entries to disappear, got %+v", hitsAfter)
	}
}

func TestChatSessionsAutoTitlePinAnd2DayExpiry(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	ctx := t.Context()
	if err := EnsureChatSchema(ctx, db); err != nil {
		t.Fatalf("EnsureChatSchema: %v", err)
	}

	// ClampTitleWords must enforce max 4 words
	if got := ClampTitleWords(`"General Relativity and Field Equations Explained"`, 4); got != "General Relativity and Field" {
		t.Errorf("expected 4-word clamped title, got %q", got)
	}

	// Create temp session (> 48h old) and pinned session (> 48h old)
	tempID, _ := CreateChatSession(ctx, db, "Old Temp Topic")
	pinID, _ := CreateChatSession(ctx, db, "Saved Pinned Topic")
	_ = AppendChatMessage(ctx, db, tempID, "user", "hi", "")
	_ = AppendChatMessage(ctx, db, pinID, "user", "keep me", "")
	_ = SetSessionPinned(ctx, db, pinID, true)

	oldTime := time.Now().UTC().Add(-49 * time.Hour).Format("2006-01-02 15:04:05")
	_, _ = db.Exec(`UPDATE ai_chat_sessions SET updated_at = ?`, oldTime)

	pruned, err := PruneExpiredSessions(ctx, db, time.Now().UTC())
	if err != nil || pruned != 1 {
		t.Fatalf("expected 1 pruned expired temp session, got %d (err=%v)", pruned, err)
	}

	list, err := ListChatSessions(ctx, db)
	if err != nil || len(list) != 1 || list[0].ID != pinID {
		t.Fatalf("expected only pinned session to remain, got %+v (err=%v)", list, err)
	}

	if err := DeleteChatSession(ctx, db, pinID); err != nil {
		t.Fatalf("DeleteChatSession: %v", err)
	}
	listAfter, _ := ListChatSessions(ctx, db)
	if len(listAfter) != 0 {
		t.Errorf("expected 0 sessions after delete, got %+v", listAfter)
	}
}
