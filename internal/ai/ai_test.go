package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

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
