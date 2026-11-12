package ai

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"ark/internal/jobs"
)

// RAGDocument tracks an indexed source in SQLite.
type RAGDocument struct {
	ID               string `json:"id"`
	SourceType       string `json:"source_type"`
	SourcePath       string `json:"source_path"`
	Title            string `json:"title"`
	IndexedPages     int    `json:"indexed_pages"`
	QdrantCollection string `json:"qdrant_collection"`
	ContentHash      string `json:"content_hash"`
	EmbedModel       string `json:"embed_model"`
	IndexedAt        string `json:"indexed_at"`
}

// EnsureSchema ensures RAG metadata and local keyword-fallback chunk tables exist.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rag_documents (
			id TEXT PRIMARY KEY,
			source_type TEXT NOT NULL,
			source_path TEXT NOT NULL,
			title TEXT NOT NULL,
			indexed_pages INTEGER NOT NULL DEFAULT 0,
			qdrant_collection TEXT NOT NULL DEFAULT '',
			indexed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS rag_chunks (
			id TEXT PRIMARY KEY,
			doc_id TEXT NOT NULL,
			source_path TEXT NOT NULL,
			title TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			content TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			embed_model TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_rag_chunks_doc ON rag_chunks(doc_id);
	`)
	return err
}

// ChunkText splits text into overlapping character chunks (~800 tokens = ~3200 chars, ~400 overlap).
func ChunkText(text string, maxChars, overlapChars int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if maxChars <= 0 {
		maxChars = 3200
	}
	if overlapChars < 0 || overlapChars >= maxChars {
		overlapChars = maxChars / 8
	}

	runes := []rune(text)
	if len(runes) <= maxChars {
		return []string{text}
	}

	var chunks []string
	step := maxChars - overlapChars
	for start := 0; start < len(runes); start += step {
		end := start + maxChars
		if end > len(runes) {
			end = len(runes)
		}
		chunk := strings.TrimSpace(string(runes[start:end]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		if end == len(runes) {
			break
		}
	}
	return chunks
}

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	titleTagRe    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	htmlTagRe     = regexp.MustCompile(`(?s)<[^>]+>`)
	multiSpaceRe  = regexp.MustCompile(`[ \t\r\f]+`)
	multiNewline  = regexp.MustCompile(`\n{3,}`)
)

// ExtractedDoc is a single document/article yielded by ExtractFile.
type ExtractedDoc struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// ExtractFile extracts plain text from native text/HTML files, or invokes the
// ark/indexer Docker container for PDF, EPUB, ZIM, and OCR image files.
func ExtractFile(ctx context.Context, filePath string) ([]ExtractedDoc, error) {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".txt", ".md", ".csv", ".log", ".json", ".yaml", ".yml", ".xml":
		b, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		return []ExtractedDoc{{
			Path:  filePath,
			Title: filepath.Base(filePath),
			Text:  strings.TrimSpace(string(b)),
		}}, nil
	case ".html", ".htm", ".xhtml":
		b, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		raw := string(b)
		title := filepath.Base(filePath)
		if m := titleTagRe.FindStringSubmatch(raw); len(m) > 1 {
			t := strings.TrimSpace(html.UnescapeString(htmlTagRe.ReplaceAllString(m[1], "")))
			if t != "" {
				title = t
			}
		}
		cleaned := scriptStyleRe.ReplaceAllString(raw, " ")
		cleaned = htmlTagRe.ReplaceAllString(cleaned, "\n")
		cleaned = html.UnescapeString(cleaned)
		cleaned = multiSpaceRe.ReplaceAllString(cleaned, " ")
		cleaned = multiNewline.ReplaceAllString(cleaned, "\n\n")
		return []ExtractedDoc{{
			Path:  filePath,
			Title: title,
			Text:  strings.TrimSpace(cleaned),
		}}, nil
	case ".pdf", ".epub", ".zim", ".png", ".jpg", ".jpeg", ".webp", ".tiff":
		return extractViaDockerIndexer(ctx, filePath)
	default:
		b, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		return []ExtractedDoc{{
			Path:  filePath,
			Title: filepath.Base(filePath),
			Text:  strings.TrimSpace(string(b)),
		}}, nil
	}
}

func extractViaDockerIndexer(ctx context.Context, filePath string) ([]ExtractedDoc, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(absPath)
	base := filepath.Base(absPath)

	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"-v", fmt.Sprintf("%s:/input:ro", dir),
		"ark/indexer:latest",
		"/input/"+base,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ark/indexer container extraction failed for %s (ensure Docker is running and ark/indexer image is built): %w", base, err)
	}

	var docs []ExtractedDoc
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var doc ExtractedDoc
		if err := json.Unmarshal([]byte(line), &doc); err == nil && strings.TrimSpace(doc.Text) != "" {
			if doc.Path == "" {
				doc.Path = filePath
			}
			docs = append(docs, doc)
		}
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("no extractable text found in %s", base)
	}
	return docs, nil
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func deterministicUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	// Format first 16 bytes as UUIDv4-like hex for Qdrant compatibility
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// IndexSource extracts, chunks, optionally embeds into Qdrant, and records the document in SQLite.
func IndexSource(ctx context.Context, db *sql.DB, conn Conn, qdrant *QdrantClient, collection, sourceType, sourcePath, titleOverride string, progress func(float64, string)) (int, error) {
	if err := EnsureSchema(ctx, db); err != nil {
		return 0, err
	}
	if collection == "" {
		collection = DefaultCollection
	}
	if sourceType == "" {
		sourceType = strings.TrimPrefix(strings.ToLower(filepath.Ext(sourcePath)), ".")
		if sourceType == "" {
			sourceType = "doc"
		}
	}

	if progress != nil {
		progress(10.0, "Extracting text...")
	}
	docs, err := ExtractFile(ctx, sourcePath)
	if err != nil {
		return 0, err
	}

	totalChunks := 0
	for i, doc := range docs {
		if ctx.Err() != nil {
			return totalChunks, ctx.Err()
		}
		docTitle := doc.Title
		if titleOverride != "" && len(docs) == 1 {
			docTitle = titleOverride
		}
		if docTitle == "" {
			docTitle = filepath.Base(doc.Path)
		}

		docID := "rag-" + hashText(doc.Path)[:16]
		docHash := hashText(doc.Text)

		// Incremental check: skip if identical content hash already indexed
		var existingHash string
		err := db.QueryRowContext(ctx, `SELECT content_hash FROM rag_chunks WHERE doc_id = ? LIMIT 1`, docID).Scan(&existingHash)
		if err == nil && existingHash == docHash {
			continue
		}

		chunks := ChunkText(doc.Text, 3200, 400)
		if len(chunks) == 0 {
			continue
		}

		// Clean previous chunks for this doc
		_, _ = db.ExecContext(ctx, `DELETE FROM rag_chunks WHERE doc_id = ?`, docID)
		if qdrant != nil {
			_ = qdrant.DeleteBySource(ctx, collection, doc.Path)
		}

		// Compute embeddings if embedding model and Qdrant are available
		var embeddings [][]float32
		if conn.BaseURL != "" && conn.EmbedModel != "" && qdrant != nil && qdrant.IsHealthy(ctx) {
			if progress != nil {
				progress(40.0, fmt.Sprintf("Embedding %d chunk(s)...", len(chunks)))
			}
			embs, embErr := conn.Embed(ctx, chunks)
			if embErr == nil && len(embs) == len(chunks) && len(embs[0]) > 0 {
				if err := qdrant.EnsureCollection(ctx, collection, len(embs[0])); err == nil {
					embeddings = embs
				}
			}
		}

		var points []VectorPoint
		for ci, chunkText := range chunks {
			chunkID := deterministicUUID(fmt.Sprintf("%s#%d", doc.Path, ci))
			_, _ = db.ExecContext(ctx, `
				INSERT INTO rag_chunks (id, doc_id, source_path, title, chunk_index, content, content_hash, embed_model)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`, chunkID, docID, doc.Path, docTitle, ci, chunkText, docHash, conn.EmbedModel)

			if len(embeddings) == len(chunks) {
				points = append(points, VectorPoint{
					ID:     chunkID,
					Vector: embeddings[ci],
					Payload: map[string]any{
						"doc_id":      docID,
						"source_path": doc.Path,
						"title":       docTitle,
						"chunk_index": ci,
						"text":        chunkText,
					},
				})
			}
			totalChunks++
		}

		if len(points) > 0 && qdrant != nil {
			if err := qdrant.Upsert(ctx, collection, points); err != nil {
				return totalChunks, err
			}
		}

		_, err = db.ExecContext(ctx, `
			INSERT INTO rag_documents (id, source_type, source_path, title, indexed_pages, qdrant_collection, indexed_at)
			VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(id) DO UPDATE SET
				title = excluded.title,
				indexed_pages = excluded.indexed_pages,
				qdrant_collection = excluded.qdrant_collection,
				indexed_at = CURRENT_TIMESTAMP
		`, docID, sourceType, doc.Path, docTitle, len(chunks), collection)
		if err != nil {
			return totalChunks, err
		}

		if progress != nil && len(docs) > 0 {
			pct := 20.0 + (float64(i+1)/float64(len(docs)))*75.0
			progress(pct, fmt.Sprintf("Indexed %d/%d document(s)", i+1, len(docs)))
		}
	}

	if progress != nil {
		progress(100.0, fmt.Sprintf("Indexed %d chunks", totalChunks))
	}
	return totalChunks, nil
}

// SearchKnowledge performs Qdrant vector search when available and falls back to
// SQLite keyword scoring over rag_chunks when Qdrant or embeddings are offline.
func SearchKnowledge(ctx context.Context, db *sql.DB, conn Conn, qdrant *QdrantClient, collection, query string, topK int) ([]ScoredPoint, error) {
	if topK <= 0 {
		topK = 5
	}
	if collection == "" {
		collection = DefaultCollection
	}

	// 1. Try vector similarity search via Qdrant
	if conn.BaseURL != "" && conn.EmbedModel != "" && qdrant != nil && qdrant.IsHealthy(ctx) {
		vecs, err := conn.Embed(ctx, []string{query})
		if err == nil && len(vecs) > 0 && len(vecs[0]) > 0 {
			pts, err := qdrant.Search(ctx, collection, vecs[0], topK)
			if err == nil && len(pts) > 0 {
				return pts, nil
			}
		}
	}

	// 2. Local SQLite keyword fallback over indexed chunks
	if db == nil {
		return nil, nil
	}
	_ = EnsureSchema(ctx, db)

	terms := strings.Fields(strings.ToLower(query))
	rows, err := db.QueryContext(ctx, `SELECT id, source_path, title, chunk_index, content FROM rag_chunks LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var scored []ScoredPoint
	for rows.Next() {
		var sp ScoredPoint
		if err := rows.Scan(&sp.ID, &sp.SourcePath, &sp.Title, &sp.ChunkIndex, &sp.Text); err != nil {
			continue
		}
		hay := strings.ToLower(sp.Title + " " + sp.Text)
		var hits float32
		for _, term := range terms {
			if len(term) > 1 && strings.Contains(hay, term) {
				hits++
			}
		}
		if hits > 0 {
			sp.Score = hits / float32(len(terms))
			scored = append(scored, sp)
		}
	}

	// Simple selection sort for topK (keeps stdlib minimal)
	for i := 0; i < len(scored); i++ {
		maxIdx := i
		for j := i + 1; j < len(scored); j++ {
			if scored[j].Score > scored[maxIdx].Score {
				maxIdx = j
			}
		}
		scored[i], scored[maxIdx] = scored[maxIdx], scored[i]
	}
	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored, nil
}

// ListDocuments returns all indexed documents ordered by most recent first.
func ListDocuments(ctx context.Context, db *sql.DB) ([]RAGDocument, error) {
	if db == nil {
		return nil, nil
	}
	if err := EnsureSchema(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT d.id, d.source_type, d.source_path, d.title, d.indexed_pages, d.qdrant_collection,
		       COALESCE((SELECT content_hash FROM rag_chunks c WHERE c.doc_id = d.id LIMIT 1), ''),
		       COALESCE((SELECT embed_model FROM rag_chunks c WHERE c.doc_id = d.id LIMIT 1), ''),
		       d.indexed_at
		FROM rag_documents d
		ORDER BY d.indexed_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []RAGDocument
	for rows.Next() {
		var d RAGDocument
		if err := rows.Scan(&d.ID, &d.SourceType, &d.SourcePath, &d.Title, &d.IndexedPages, &d.QdrantCollection, &d.ContentHash, &d.EmbedModel, &d.IndexedAt); err == nil {
			docs = append(docs, d)
		}
	}
	return docs, nil
}

// RemoveDocument deletes an indexed document from SQLite and Qdrant.
func RemoveDocument(ctx context.Context, db *sql.DB, qdrant *QdrantClient, docID string) error {
	if db == nil {
		return nil
	}
	var sourcePath, coll string
	_ = db.QueryRowContext(ctx, `SELECT source_path, qdrant_collection FROM rag_documents WHERE id = ?`, docID).Scan(&sourcePath, &coll)
	if qdrant != nil && sourcePath != "" {
		_ = qdrant.DeleteBySource(ctx, coll, sourcePath)
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM rag_chunks WHERE doc_id = ?`, docID)
	_, err := db.ExecContext(ctx, `DELETE FROM rag_documents WHERE id = ?`, docID)
	return err
}

// LoadConnFromDB reads AI connection settings from SQLite settings table.
func LoadConnFromDB(ctx context.Context, db *sql.DB) Conn {
	var c Conn
	if db == nil {
		return c
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN ('ai_server_url', 'ai_api_key', 'ai_chat_model', 'ai_embed_model')`)
	if err != nil {
		return c
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			switch k {
			case "ai_server_url":
				c.BaseURL = v
			case "ai_api_key":
				c.APIKey = v
			case "ai_chat_model":
				c.ChatModel = v
			case "ai_embed_model":
				c.EmbedModel = v
			}
		}
	}
	return c
}

// IndexJobRunner executes TypeIndex jobs queued through jobs.QueueManager.
type IndexJobRunner struct {
	DB        *sql.DB
	QdrantURL string
}

// Run implements jobs.Runner for TypeIndex background jobs.
func (r *IndexJobRunner) Run(ctx context.Context, job *jobs.Job, progress jobs.ProgressFunc) error {
	conn := LoadConnFromDB(ctx, r.DB)
	qdrant := NewQdrantClient(r.QdrantURL)
	meta := jobs.ParseMetadata(job.Metadata)
	title := meta.FileName
	sourceType := meta.Format
	if sourceType == "" {
		sourceType = "doc"
	}

	var cb func(float64, string)
	if progress != nil {
		cb = func(pct float64, msg string) {
			progress(pct, msg, 0, 0, 0)
		}
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	_, err := IndexSource(ctxTimeout, r.DB, conn, qdrant, DefaultCollection, sourceType, job.Target, title, cb)
	return err
}
