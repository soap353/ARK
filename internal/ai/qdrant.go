package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultCollection = "ark_knowledge"

// QdrantClient is a lightweight stdlib HTTP client for Qdrant REST APIs.
type QdrantClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// VectorPoint represents a single embedded chunk stored in Qdrant.
type VectorPoint struct {
	ID         string         `json:"id"`
	Vector     []float32      `json:"vector"`
	Payload    map[string]any `json:"payload"`
}

// ScoredPoint represents a matched chunk returned from vector or keyword search.
type ScoredPoint struct {
	ID         string  `json:"id"`
	Score      float32 `json:"score"`
	SourcePath string  `json:"source_path"`
	Title      string  `json:"title"`
	Text       string  `json:"text"`
	ChunkIndex int     `json:"chunk_index"`
}

// NewQdrantClient initializes a QdrantClient pointing to addr (default http://127.0.0.1:6333).
func NewQdrantClient(addr string) *QdrantClient {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = "http://127.0.0.1:6333"
	}
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	return &QdrantClient{
		BaseURL:    strings.TrimRight(addr, "/"),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (q *QdrantClient) client() *http.Client {
	if q.HTTPClient != nil {
		return q.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// IsHealthy returns true when Qdrant responds on /healthz or /collections.
func (q *QdrantClient) IsHealthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.BaseURL+"/collections", nil)
	if err != nil {
		return false
	}
	resp, err := q.client().Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// EnsureCollection creates the collection if it does not yet exist.
func (q *QdrantClient) EnsureCollection(ctx context.Context, name string, vectorSize int) error {
	if name == "" {
		name = DefaultCollection
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.BaseURL+"/collections/"+name, nil)
	if err != nil {
		return err
	}
	resp, err := q.client().Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
	}

	payload := map[string]any{
		"vectors": map[string]any{
			"size":     vectorSize,
			"distance": "Cosine",
		},
	}
	body, _ := json.Marshal(payload)
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, q.BaseURL+"/collections/"+name, bytes.NewReader(body))
	if err != nil {
		return err
	}
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := q.client().Do(putReq)
	if err != nil {
		return err
	}
	defer putResp.Body.Close()
	if putResp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(putResp.Body, 512))
		return fmt.Errorf("create qdrant collection %s status %d: %s", name, putResp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Upsert stores vector points into Qdrant.
func (q *QdrantClient) Upsert(ctx context.Context, collection string, points []VectorPoint) error {
	if len(points) == 0 {
		return nil
	}
	if collection == "" {
		collection = DefaultCollection
	}
	body, err := json.Marshal(map[string]any{"points": points})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, q.BaseURL+"/collections/"+collection+"/points?wait=true", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("qdrant upsert status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Search queries topK nearest vector neighbors in Qdrant.
func (q *QdrantClient) Search(ctx context.Context, collection string, vector []float32, topK int) ([]ScoredPoint, error) {
	if collection == "" {
		collection = DefaultCollection
	}
	if topK <= 0 {
		topK = 5
	}
	reqBody := map[string]any{
		"vector":       vector,
		"limit":        topK,
		"with_payload": true,
	}
	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.BaseURL+"/collections/"+collection+"/points/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("qdrant search status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var parsed struct {
		Result []struct {
			ID      any            `json:"id"`
			Score   float32        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	out := make([]ScoredPoint, 0, len(parsed.Result))
	for _, r := range parsed.Result {
		sp := ScoredPoint{
			ID:    fmt.Sprintf("%v", r.ID),
			Score: r.Score,
		}
		if r.Payload != nil {
			if v, ok := r.Payload["source_path"].(string); ok {
				sp.SourcePath = v
			}
			if v, ok := r.Payload["title"].(string); ok {
				sp.Title = v
			}
			if v, ok := r.Payload["text"].(string); ok {
				sp.Text = v
			}
			if v, ok := r.Payload["chunk_index"].(float64); ok {
				sp.ChunkIndex = int(v)
			}
		}
		out = append(out, sp)
	}
	return out, nil
}

// DeleteBySource deletes all points matching source_path.
func (q *QdrantClient) DeleteBySource(ctx context.Context, collection, sourcePath string) error {
	if collection == "" {
		collection = DefaultCollection
	}
	payload := map[string]any{
		"filter": map[string]any{
			"must": []map[string]any{
				{"key": "source_path", "match": map[string]any{"value": sourcePath}},
			},
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.BaseURL+"/collections/"+collection+"/points/delete?wait=true", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.client().Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
