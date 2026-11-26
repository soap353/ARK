// Package ai provides Ark's offline AI features: OpenAI/Ollama client calls,
// native document text extraction, SQLite FTS5 knowledge indexing, and chat session storage.
package ai

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"ark/internal/jobs"
)

// ScoredPoint represents a matched text chunk returned from SQLite knowledge search.
type ScoredPoint struct {
	ID         string  `json:"id"`
	Score      float32 `json:"score"`
	SourcePath string  `json:"source_path"`
	Title      string  `json:"title"`
	Text       string  `json:"text"`
	ChunkIndex int     `json:"chunk_index"`
}

// RAGDocument tracks an indexed source file or archive in SQLite.
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

// EnsureSchema ensures the SQLite RAG metadata, chunk, and FTS5 tables exist.
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
		CREATE VIRTUAL TABLE IF NOT EXISTS rag_fts USING fts5(chunk_id UNINDEXED, doc_id UNINDEXED, source_path UNINDEXED, title, content);
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
		b, err := readCapped(filePath)
		if err != nil {
			return nil, err
		}
		return []ExtractedDoc{{
			Path:  filePath,
			Title: filepath.Base(filePath),
			Text:  strings.TrimSpace(string(b)),
		}}, nil
	case ".html", ".htm", ".xhtml":
		b, err := readCapped(filePath)
		if err != nil {
			return nil, err
		}
		title, plain := htmlToPlainText(string(b), filepath.Base(filePath), 0)
		return []ExtractedDoc{{
			Path:  filePath,
			Title: title,
			Text:  plain,
		}}, nil
	case ".zim":
		return extractZIM(ctx, filePath)
	case ".epub":
		return extractEPUB(ctx, filePath)
	case ".pdf":
		return extractPDF(ctx, filePath)
	case ".png", ".jpg", ".jpeg", ".webp", ".tiff":
		return extractViaDockerIndexer(ctx, filePath)
	default:
		b, err := readCapped(filePath)
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

func deterministicChunkID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// IndexSource extracts text from sourcePath, splits it into chunks, and indexes them in SQLite + FTS5.
func IndexSource(ctx context.Context, db *sql.DB, conn Conn, sourceType, sourcePath, titleOverride string, progress func(float64, string)) (int, error) {
	if err := EnsureSchema(ctx, db); err != nil {
		return 0, err
	}
	if sourceType == "" {
		sourceType = strings.TrimPrefix(strings.ToLower(filepath.Ext(sourcePath)), ".")
		if sourceType == "" {
			sourceType = "doc"
		}
	}

	if progress != nil {
		progress(10.0, "Extracting text from "+filepath.Base(sourcePath)+"...")
	}
	docs, err := ExtractFile(ctx, sourcePath)
	if err != nil {
		return 0, err
	}

	// Multi-page ZIM/EPUB archives record one archive-level row in rag_documents.
	if len(docs) > 1 || strings.EqualFold(filepath.Ext(sourcePath), ".zim") || strings.EqualFold(filepath.Ext(sourcePath), ".epub") {
		return indexArchiveDocs(ctx, db, sourceType, sourcePath, titleOverride, docs, progress)
	}

	totalChunks := 0
	for i, doc := range docs {
		if ctx.Err() != nil {
			return totalChunks, ctx.Err()
		}
		docTitle := doc.Title
		if titleOverride != "" {
			docTitle = titleOverride
		}
		if docTitle == "" {
			docTitle = filepath.Base(doc.Path)
		}

		docID := "rag-" + hashText(doc.Path)[:16]
		docHash := hashText(doc.Text)

		// Skip re-indexing when the extracted content hash matches what is already in SQLite.
		var existingHash string
		err := db.QueryRowContext(ctx, `SELECT content_hash FROM rag_chunks WHERE doc_id = ? LIMIT 1`, docID).Scan(&existingHash)
		if err == nil && existingHash == docHash {
			continue
		}

		chunks := ChunkText(doc.Text, 3200, 400)
		if len(chunks) == 0 {
			continue
		}

		tx, txErr := db.BeginTx(ctx, nil)
		if txErr != nil {
			return totalChunks, txErr
		}
		_, _ = tx.ExecContext(ctx, `DELETE FROM rag_chunks WHERE doc_id = ?`, docID)
		_, _ = tx.ExecContext(ctx, `DELETE FROM rag_fts WHERE doc_id = ?`, docID)

		for ci, chunkText := range chunks {
			chunkID := deterministicChunkID(fmt.Sprintf("%s#%d", doc.Path, ci))
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO rag_chunks (id, doc_id, source_path, title, chunk_index, content, content_hash, embed_model)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`, chunkID, docID, doc.Path, docTitle, ci, chunkText, docHash, conn.EmbedModel); err != nil {
				_ = tx.Rollback()
				return totalChunks, err
			}
			_, _ = tx.ExecContext(ctx, `
				INSERT INTO rag_fts (chunk_id, doc_id, source_path, title, content)
				VALUES (?, ?, ?, ?, ?)
			`, chunkID, docID, doc.Path, docTitle, chunkText)
			totalChunks++
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO rag_documents (id, source_type, source_path, title, indexed_pages, qdrant_collection, indexed_at)
			VALUES (?, ?, ?, ?, ?, '', CURRENT_TIMESTAMP)
			ON CONFLICT(id) DO UPDATE SET
				title = excluded.title,
				indexed_pages = excluded.indexed_pages,
				indexed_at = CURRENT_TIMESTAMP
		`, docID, sourceType, doc.Path, docTitle, len(chunks))
		if err != nil {
			_ = tx.Rollback()
			return totalChunks, err
		}
		if err := tx.Commit(); err != nil {
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

func indexArchiveDocs(ctx context.Context, db *sql.DB, sourceType, sourcePath, titleOverride string, docs []ExtractedDoc, progress func(float64, string)) (int, error) {
	docID := "rag-" + hashText(sourcePath)[:16]
	archiveTitle := strings.TrimSpace(titleOverride)
	if archiveTitle == "" {
		archiveTitle = strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
	}
	archiveHash := hashText(fmt.Sprintf("%s:%d:%s", sourcePath, len(docs), docs[0].Title))
	if st, err := os.Stat(sourcePath); err == nil {
		archiveHash = hashText(fmt.Sprintf("%s:%d:%d", sourcePath, st.Size(), st.ModTime().Unix()))
	}

	var existingHash string
	if err := db.QueryRowContext(ctx, `SELECT content_hash FROM rag_chunks WHERE doc_id = ? LIMIT 1`, docID).Scan(&existingHash); err == nil && existingHash == archiveHash {
		if progress != nil {
			progress(100.0, fmt.Sprintf("Already indexed (%d articles cached)", len(docs)))
		}
		return 0, nil
	}

	if progress != nil {
		progress(35.0, fmt.Sprintf("Indexing %d extracted articles into FTS search...", len(docs)))
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	_, _ = tx.ExecContext(ctx, `DELETE FROM rag_chunks WHERE doc_id = ?`, docID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM rag_fts WHERE doc_id = ?`, docID)

	stmtChunk, err := tx.PrepareContext(ctx, `
		INSERT INTO rag_chunks (id, doc_id, source_path, title, chunk_index, content, content_hash, embed_model)
		VALUES (?, ?, ?, ?, ?, ?, ?, '')
	`)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	defer stmtChunk.Close()

	stmtFTS, err := tx.PrepareContext(ctx, `
		INSERT INTO rag_fts (chunk_id, doc_id, source_path, title, content)
		VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	defer stmtFTS.Close()

	totalChunks := 0
	for i, doc := range docs {
		if i%500 == 0 {
			if ctx.Err() != nil {
				_ = tx.Rollback()
				return totalChunks, ctx.Err()
			}
			if progress != nil {
				pct := 35.0 + (float64(i)/float64(len(docs)))*60.0
				progress(pct, fmt.Sprintf("Indexed %d / %d articles (%d chunks)...", i, len(docs), totalChunks))
			}
		}
		itemTitle := doc.Title
		if itemTitle == "" {
			itemTitle = archiveTitle
		}
		displayTitle := itemTitle + " (" + archiveTitle + ")"
		chunks := ChunkText(doc.Text, 3200, 300)
		for ci, chunkText := range chunks {
			chunkID := deterministicChunkID(fmt.Sprintf("%s#%s#%d", sourcePath, doc.Path, ci))
			if _, err := stmtChunk.ExecContext(ctx, chunkID, docID, doc.Path, displayTitle, totalChunks, chunkText, archiveHash); err != nil {
				_ = tx.Rollback()
				return totalChunks, err
			}
			if _, err := stmtFTS.ExecContext(ctx, chunkID, docID, doc.Path, displayTitle, chunkText); err != nil {
				_ = tx.Rollback()
				return totalChunks, err
			}
			totalChunks++
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO rag_documents (id, source_type, source_path, title, indexed_pages, qdrant_collection, indexed_at)
		VALUES (?, ?, ?, ?, ?, '', CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			indexed_pages = excluded.indexed_pages,
			indexed_at = CURRENT_TIMESTAMP
	`, docID, sourceType, sourcePath, archiveTitle, totalChunks)
	if err != nil {
		_ = tx.Rollback()
		return totalChunks, err
	}
	if err := tx.Commit(); err != nil {
		return totalChunks, err
	}
	if progress != nil {
		progress(100.0, fmt.Sprintf("Indexed %d articles (%d chunks)", len(docs), totalChunks))
	}
	return totalChunks, nil
}

// SearchKnowledge runs fast SQLite FTS5 BM25 search (with a keyword scan fallback) over indexed chunks.
func SearchKnowledge(ctx context.Context, db *sql.DB, query string, topK int) ([]ScoredPoint, error) {
	if topK <= 0 {
		topK = 5
	}
	if db == nil {
		return nil, nil
	}
	_ = EnsureSchema(ctx, db)

	runFTS := func(ftsExpr string, limit int) []ScoredPoint {
		if ftsExpr == "" || limit <= 0 {
			return nil
		}
		frows, ferr := db.QueryContext(ctx, `
			SELECT chunk_id, source_path, title, content, bm25(rag_fts, 3.0, 1.0) AS rank
			FROM rag_fts
			WHERE rag_fts MATCH ?
			ORDER BY rank ASC
			LIMIT ?
		`, ftsExpr, limit)
		if ferr != nil {
			return nil
		}
		defer frows.Close()
		var out []ScoredPoint
		for frows.Next() {
			var sp ScoredPoint
			var rank float64
			if err := frows.Scan(&sp.ID, &sp.SourcePath, &sp.Title, &sp.Text, &rank); err == nil {
				sp.Score = float32(1.0 / (1.0 - rank))
				out = append(out, sp)
			}
		}
		return out
	}

	if andExpr, orExpr := buildFTSQueries(query); orExpr != "" {
		var ftsHits []ScoredPoint
		seenID := make(map[string]bool)
		if andExpr != "" && andExpr != orExpr {
			for _, h := range runFTS(andExpr, topK) {
				if !seenID[h.ID] {
					seenID[h.ID] = true
					ftsHits = append(ftsHits, h)
				}
			}
		}
		if len(ftsHits) < topK {
			for _, h := range runFTS(orExpr, topK) {
				if !seenID[h.ID] {
					seenID[h.ID] = true
					ftsHits = append(ftsHits, h)
					if len(ftsHits) >= topK {
						break
					}
				}
			}
		}
		if len(ftsHits) > 0 {
			return ftsHits, nil
		}
	}

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

// RemoveDocument deletes an indexed document and its chunks/FTS entries from SQLite.
func RemoveDocument(ctx context.Context, db *sql.DB, docID string) error {
	if db == nil {
		return nil
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM rag_fts WHERE doc_id = ?`, docID)
	_, _ = db.ExecContext(ctx, `DELETE FROM rag_chunks WHERE doc_id = ?`, docID)
	_, err := db.ExecContext(ctx, `DELETE FROM rag_documents WHERE id = ?`, docID)
	return err
}

var ftsTokenRe = regexp.MustCompile(`[a-zA-Z0-9]{2,}`)

var ftsStopWords = map[string]bool{
	"about": true, "after": true, "all": true, "also": true, "and": true, "are": true,
	"brief": true, "briefly": true, "can": true, "concise": true, "could": true, "did": true, "does": true,
	"explain": true, "for": true, "from": true, "give": true, "have": true, "how": true, "into": true, "its": true,
	"know": true, "like": true, "make": true, "more": true, "please": true, "quick": true, "quickly": true,
	"short": true, "some": true, "summarize": true, "summary": true, "tell": true, "than": true, "that": true,
	"the": true, "their": true, "them": true, "then": true, "there": true, "these": true, "they": true, "this": true,
	"using": true, "want": true, "was": true, "were": true, "what": true, "when": true, "where": true,
	"which": true, "who": true, "why": true, "will": true, "with": true, "would": true, "you": true, "your": true,
}

func buildFTSQueries(raw string) (andQuery, orQuery string) {
	return BuildFTSQueries(raw)
}

// SignificantTerms extracts lowercase non-stopword alphanumeric terms from raw text.
func SignificantTerms(raw string) []string {
	tokens := ftsTokenRe.FindAllString(strings.ToLower(raw), 16)
	seen := make(map[string]bool, len(tokens))
	var out, fallback []string
	for _, t := range tokens {
		if seen[t] {
			continue
		}
		seen[t] = true
		fallback = append(fallback, t)
		if !ftsStopWords[t] {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// BuildFTSQueries constructs FTS5 AND and OR match queries from a raw user query.
func BuildFTSQueries(raw string) (andQuery, orQuery string) {
	terms := SignificantTerms(raw)
	if len(terms) == 0 {
		return "", ""
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + t + `"`
	}
	if len(quoted) >= 2 && len(quoted) <= 5 {
		andQuery = strings.Join(quoted, " ")
	}
	return andQuery, strings.Join(quoted, " OR ")
}

// HTMLToPlainText strips script/style tags and HTML markup to produce clean text.
func HTMLToPlainText(rawHTML, fallbackTitle string, maxRunes int) (title, plain string) {
	return htmlToPlainText(rawHTML, fallbackTitle, maxRunes)
}

// LoadConnFromDB reads AI connection settings from SQLite settings table.
func LoadConnFromDB(ctx context.Context, db *sql.DB) Conn {
	var c Conn
	if db == nil {
		return c
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN ('ai_server_url', 'ai_api_key', 'ai_chat_model', 'ai_embed_model', 'ai_context_length')`)
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
			case "ai_context_length":
				var n int
				if _, _ = fmt.Sscanf(strings.TrimSpace(v), "%d", &n); n > 0 {
					c.ContextLength = n
				}
			}
		}
	}
	return c
}

// IndexJobRunner executes TypeIndex jobs queued through jobs.QueueManager.
type IndexJobRunner struct {
	DB *sql.DB
}

// Run implements jobs.Runner for TypeIndex background jobs.
func (r *IndexJobRunner) Run(ctx context.Context, job *jobs.Job, progress jobs.ProgressFunc) error {
	conn := LoadConnFromDB(ctx, r.DB)
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

	_, err := IndexSource(ctx, r.DB, conn, sourceType, job.Target, title, cb)
	return err
}

// maxPlainTextBytes bounds how much of one plain-text/HTML file is loaded for indexing.
const maxPlainTextBytes = 16 << 20

// readCapped reads at most maxPlainTextBytes of a file so huge logs/exports can't exhaust RAM.
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxPlainTextBytes))
}
