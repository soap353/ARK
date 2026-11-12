package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Conn holds connection settings for any OpenAI-compatible AI server
// (Ollama, LM Studio, vLLM, llama.cpp).
type Conn struct {
	BaseURL    string
	APIKey     string
	ChatModel  string
	EmbedModel string
	HTTPClient *http.Client
}

// Msg represents a chat message in OpenAI format.
type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// DetectedServer describes a local AI server discovered via port probing.
type DetectedServer struct {
	Name   string   `json:"name"`
	URL    string   `json:"url"`
	Models []string `json:"models,omitempty"`
}

// NormalizeBaseURL ensures the URL has http(s):// and ends with /v1.
func NormalizeBaseURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	u = strings.TrimRight(u, "/")
	if !strings.HasSuffix(u, "/v1") {
		u += "/v1"
	}
	return u
}

func (c Conn) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c Conn) endpoint(path string) (string, error) {
	base := NormalizeBaseURL(c.BaseURL)
	if base == "" {
		return "", errors.New("ai server URL is not configured")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path, nil
}

func (c Conn) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	urlStr, err := c.endpoint(path)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(c.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.APIKey))
	}
	return req, nil
}

// Models fetches available model IDs from GET /v1/models.
func (c Conn) Models(ctx context.Context) ([]string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("models endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode /v1/models: %w", err)
	}
	models := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	sort.Strings(models)
	return models, nil
}

// Embed computes vector embeddings for texts via POST /v1/embeddings.
func (c Conn) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	model := strings.TrimSpace(c.EmbedModel)
	if model == "" {
		return nil, errors.New("embedding model is not configured")
	}

	reqBody := map[string]any{
		"model": model,
		"input": texts,
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/embeddings", reqBody)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embeddings returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode /v1/embeddings: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, errors.New("empty embeddings response")
	}

	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		idx := d.Index
		if idx < 0 || idx >= len(out) {
			idx = i
		}
		out[idx] = d.Embedding
	}
	return out, nil
}

// ChatStream sends messages to POST /v1/chat/completions with streaming enabled,
// calling onToken for each incremental text chunk and returning the full response.
func (c Conn) ChatStream(ctx context.Context, msgs []Msg, onToken func(string)) (string, error) {
	model := strings.TrimSpace(c.ChatModel)
	if model == "" {
		return "", errors.New("chat model is not configured")
	}

	reqBody := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   true,
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", reqBody)
	if err != nil {
		return "", err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("chat endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var fullResp struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&fullResp); err != nil {
			return "", fmt.Errorf("decode chat json: %w", err)
		}
		if len(fullResp.Choices) == 0 {
			return "", errors.New("no choices returned from chat model")
		}
		text := fullResp.Choices[0].Message.Content
		if onToken != nil && text != "" {
			onToken(text)
		}
		return text, nil
	}

	var full strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			tok := chunk.Choices[0].Delta.Content
			if tok == "" {
				tok = chunk.Choices[0].Message.Content
			}
			if tok != "" {
				full.WriteString(tok)
				if onToken != nil {
					onToken(tok)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return full.String(), err
	}
	return full.String(), nil
}

// DetectServers probes common local OpenAI-compatible ports (Ollama 11434,
// LM Studio 1234, vLLM 8000, llama.cpp 8081/8080) on host (defaults to 127.0.0.1).
func DetectServers(ctx context.Context, host string) []DetectedServer {
	host = strings.TrimSpace(host)
	if host == "" {
		host = "127.0.0.1"
	}
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimPrefix(host, "https://")
	if idx := strings.Index(host, "/"); idx != -1 {
		host = host[:idx]
	}
	if idx := strings.Index(host, ":"); idx != -1 {
		host = host[:idx]
	}

	candidates := []struct {
		name string
		port int
	}{
		{"Ollama", 11434},
		{"LM Studio", 1234},
		{"vLLM", 8000},
		{"llama.cpp", 8081},
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	var found []DetectedServer
	hc := &http.Client{Timeout: 700 * time.Millisecond}

	for _, c := range candidates {
		wg.Add(1)
		go func(name string, port int) {
			defer wg.Done()
			base := fmt.Sprintf("http://%s:%d", host, port)
			models, err := (Conn{BaseURL: base, HTTPClient: hc}).Models(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			found = append(found, DetectedServer{
				Name:   name,
				URL:    base,
				Models: models,
			})
			mu.Unlock()
		}(c.name, c.port)
	}
	wg.Wait()
	sort.Slice(found, func(i, j int) bool { return found[i].URL < found[j].URL })
	return found
}
