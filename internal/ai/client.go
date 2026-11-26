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
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"ark/internal/jobs"
)

var (
	sharedAIClient      = &http.Client{Timeout: 180 * time.Second}
	sharedProbeClient   = &http.Client{Timeout: 1500 * time.Millisecond}
	sharedStreamClient  = &http.Client{Timeout: 0}
	sharedOllamaClient  = &http.Client{Timeout: 8 * time.Second}
	sharedDetectClient  = &http.Client{Timeout: 700 * time.Millisecond}
)

// Conn holds connection settings for any OpenAI-compatible AI server
// (Ollama, LM Studio, vLLM, llama.cpp).
type Conn struct {
	BaseURL       string
	APIKey        string
	ChatModel     string
	EmbedModel    string
	ContextLength int
	HTTPClient    *http.Client
}

// ModelInfo describes a model installed on the local AI server and its properties.
type ModelInfo struct {
	ID                  string   `json:"id"`
	Type                string   `json:"type,omitempty"`
	Arch                string   `json:"arch,omitempty"`
	Quantization        string   `json:"quantization,omitempty"`
	State               string   `json:"state,omitempty"`
	MaxContextLength    int      `json:"max_context_length,omitempty"`
	LoadedContextLength int      `json:"loaded_context_length,omitempty"`
	Capabilities        []string `json:"capabilities,omitempty"`
	SupportsVision      bool     `json:"supports_vision,omitempty"`
	SupportsTools       bool     `json:"supports_tools,omitempty"`
}

// ModelRuntimeStatus reports live reachability, load state, and capabilities of the AI server.
type ModelRuntimeStatus struct {
	ServerOnline     bool        `json:"server_online"`
	ModelLoaded      bool        `json:"model_loaded"`
	LoadedModel      string      `json:"loaded_model,omitempty"`
	ConfiguredModel  string      `json:"configured_model,omitempty"`
	ContextLength    int         `json:"context_length"`
	MaxContextLength int         `json:"max_context_length,omitempty"`
	SupportsVision   bool        `json:"supports_vision"`
	SupportsTools    bool        `json:"supports_tools"`
	Quantization     string      `json:"quantization,omitempty"`
	Arch             string      `json:"arch,omitempty"`
	Models           []ModelInfo `json:"models,omitempty"`
}

// ImageURL carries a base64 data URI or URL for vision-capable chat models.
type ImageURL struct {
	URL string `json:"url"`
}

// ContentPart represents a text or image_url segment in multimodal OpenAI chat messages.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// Msg represents a chat message in OpenAI format (string content or multimodal parts).
type Msg struct {
	Role    string        `json:"role"`
	Content string        `json:"-"`
	Parts   []ContentPart `json:"-"`
}

// MarshalJSON encodes string content for text-only messages and array content when Parts is populated.
func (m Msg) MarshalJSON() ([]byte, error) {
	if len(m.Parts) > 0 {
		return json.Marshal(struct {
			Role    string        `json:"role"`
			Content []ContentPart `json:"content"`
		}{Role: m.Role, Content: m.Parts})
	}
	return json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: m.Role, Content: m.Content})
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
	if os.Getenv("ARK_IN_CONTAINER") == "1" {
		u = strings.ReplaceAll(u, "://127.0.0.1", "://host.docker.internal")
		u = strings.ReplaceAll(u, "://localhost", "://host.docker.internal")
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
	return sharedAIClient
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
	hc := c.HTTPClient
	if hc == nil {
		hc = sharedAIClient
	}
	resp, err := hc.Do(req)
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

// ContextWindow returns the effective context length in tokens for the active model.
func (c Conn) ContextWindow(ctx context.Context) int {
	st := c.Status(ctx)
	if st.ContextLength > 0 {
		return st.ContextLength
	}
	return 8192
}

func resolveEffectiveCtx(configured, loaded, maxCtx int) int {
	base := loaded
	if base <= 0 {
		base = maxCtx
	}
	if base <= 0 {
		base = 8192
	}
	if configured > 0 {
		if maxCtx > 0 && configured > maxCtx {
			return maxCtx
		}
		if loaded > 0 && configured > loaded {
			return loaded
		}
		return configured
	}
	return base
}

func hasCapability(caps []string, target string) bool {
	for _, capStr := range caps {
		if strings.EqualFold(strings.TrimSpace(capStr), target) {
			return true
		}
	}
	return false
}

// Status probes the AI server (LM Studio /api/v0/models, Ollama /api/ps + /api/show,
// or generic OpenAI /v1/models) to determine true server reachability, whether a chat
// model is actually loaded in memory, its active context length, and vision/tool support.
// shortcut: falls back to 8192 tokens on generic servers that omit context metadata.
func (c Conn) Status(ctx context.Context) ModelRuntimeStatus {
	out := ModelRuntimeStatus{
		ConfiguredModel: strings.TrimSpace(c.ChatModel),
		ContextLength:   8192,
	}
	base := NormalizeBaseURL(c.BaseURL)
	if base == "" {
		return out
	}
	root := strings.TrimSuffix(base, "/v1")
	hc := sharedProbeClient
	if c.HTTPClient != nil {
		hc = c.HTTPClient
	}

	// 1. Probe LM Studio native /api/v0/models (explicit state: loaded|not-loaded, type, max_context_length, loaded_context_length)
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/api/v0/models", nil); err == nil {
		if resp, err := hc.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var lms struct {
					Data []ModelInfo `json:"data"`
				}
				if json.NewDecoder(io.LimitReader(resp.Body, 512*1024)).Decode(&lms) == nil {
					out.ServerOnline = true
					var cfgMatch, loadedMatch *ModelInfo
					for i := range lms.Data {
						m := &lms.Data[i]
						m.SupportsVision = strings.EqualFold(m.Type, "vlm") || hasCapability(m.Capabilities, "vision")
						m.SupportsTools = hasCapability(m.Capabilities, "tool_use")
						if m.ID == out.ConfiguredModel {
							cfgMatch = m
						}
						if strings.EqualFold(m.State, "loaded") && !strings.EqualFold(m.Type, "embeddings") {
							if m.ID == out.ConfiguredModel || loadedMatch == nil {
								loadedMatch = m
							}
						}
					}
					out.Models = lms.Data
					active := loadedMatch
					if loadedMatch != nil {
						out.ModelLoaded = true
						out.LoadedModel = loadedMatch.ID
					} else if cfgMatch != nil {
						active = cfgMatch
					}
					if active != nil {
						out.MaxContextLength = active.MaxContextLength
						out.SupportsVision = active.SupportsVision
						out.SupportsTools = active.SupportsTools
						out.Quantization = active.Quantization
						out.Arch = active.Arch
						out.ContextLength = resolveEffectiveCtx(c.ContextLength, active.LoadedContextLength, active.MaxContextLength)
					} else {
						out.ContextLength = resolveEffectiveCtx(c.ContextLength, 0, 0)
					}
					return out
				}
			}
		}
	}

	// 2. Probe Ollama /api/ps (currently loaded in RAM/VRAM) + /api/tags & /api/show
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/api/ps", nil); err == nil {
		if resp, err := hc.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var ps struct {
					Models []struct {
						Name          string `json:"name"`
						Model         string `json:"model"`
						ContextLength int    `json:"context_length"`
					} `json:"models"`
				}
				if json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&ps) == nil {
					out.ServerOnline = true
					var loadedCtx int
					for _, m := range ps.Models {
						name := m.Name
						if name == "" {
							name = m.Model
						}
						if name == "" {
							continue
						}
						if !out.ModelLoaded || (out.ConfiguredModel != "" && (name == out.ConfiguredModel || m.Model == out.ConfiguredModel)) {
							out.ModelLoaded = true
							out.LoadedModel = name
							if m.ContextLength > 0 {
								loadedCtx = m.ContextLength
							}
						}
					}
					queryModel := out.LoadedModel
					if queryModel == "" {
						queryModel = out.ConfiguredModel
					}
					var maxCtx int
					if queryModel != "" {
						maxCtx, out.SupportsVision = probeOllamaShow(ctx, hc, root, queryModel)
					}
					if installed, err := c.Models(ctx); err == nil {
						for _, id := range installed {
							out.Models = append(out.Models, ModelInfo{ID: id})
						}
					}
					out.MaxContextLength = maxCtx
					out.ContextLength = resolveEffectiveCtx(c.ContextLength, loadedCtx, maxCtx)
					return out
				}
			}
		}
	}

	// 3. Fallback: generic OpenAI server (/v1/models)
	if req, err := c.newRequest(ctx, http.MethodGet, "/models", nil); err == nil {
		if resp, err := hc.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var mdl struct {
					Data []struct {
						ID            string `json:"id"`
						ContextLength int    `json:"context_length"`
						MaxModelLen   int    `json:"max_model_len"`
					} `json:"data"`
				}
				if json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&mdl) == nil {
					out.ServerOnline = true
					var maxCtx int
					for _, m := range mdl.Data {
						if m.ID == "" {
							continue
						}
						mc := m.ContextLength
						if mc <= 0 {
							mc = m.MaxModelLen
						}
						out.Models = append(out.Models, ModelInfo{ID: m.ID, MaxContextLength: mc, State: "loaded"})
						if m.ID == out.ConfiguredModel || (!out.ModelLoaded && m.ID != "") {
							out.ModelLoaded = true
							out.LoadedModel = m.ID
							maxCtx = mc
						}
					}
					out.MaxContextLength = maxCtx
					out.ContextLength = resolveEffectiveCtx(c.ContextLength, 0, maxCtx)
					return out
				}
			}
		}
	}

	return out
}

func probeOllamaShow(ctx context.Context, hc *http.Client, root, model string) (maxCtx int, vision bool) {
	bodyBytes, err := json.Marshal(map[string]string{"name": model})
	if err != nil {
		return 0, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, root+"/api/show", bytes.NewReader(bodyBytes))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0, false
	}
	defer resp.Body.Close()
	var show struct {
		Capabilities []string       `json:"capabilities"`
		Parameters   string         `json:"parameters"`
		ModelInfo    map[string]any `json:"model_info"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 512*1024)).Decode(&show)
	vision = hasCapability(show.Capabilities, "vision")
	for _, line := range strings.Split(show.Parameters, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "num_ctx" {
			var n int
			if _, _ = fmt.Sscanf(fields[1], "%d", &n); n > 0 {
				maxCtx = n
			}
		}
	}
	for k, v := range show.ModelInfo {
		if strings.Contains(strings.ToLower(k), "vision") {
			vision = true
		}
		if strings.HasSuffix(k, ".context_length") {
			if f, ok := v.(float64); ok && int(f) > 0 && maxCtx == 0 {
				maxCtx = int(f)
			}
		}
	}
	return maxCtx, vision
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
	hc := c.HTTPClient
	if hc == nil {
		hc = sharedAIClient
	}
	resp, err := hc.Do(req)
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
	hc := c.HTTPClient
	if hc == nil {
		hc = sharedAIClient
	}
	resp, err := hc.Do(req)
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
		if os.Getenv("ARK_IN_CONTAINER") == "1" {
			host = "host.docker.internal"
		} else {
			host = "127.0.0.1"
		}
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
	hc := sharedDetectClient

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

// ---------------------------------------------------------------------------
// Ollama Native Model Management (tags, pull, delete)
// ---------------------------------------------------------------------------

// OllamaModel describes an installed model returned by Ollama's /api/tags endpoint.
type OllamaModel struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	ModifiedAt   string `json:"modified_at"`
	Family       string `json:"family,omitempty"`
	ParamSize    string `json:"parameter_size,omitempty"`
	Quantization string `json:"quantization_level,omitempty"`
}

func normalizeOllamaBase(raw string) string {
	base := strings.TrimSpace(raw)
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/v1")
	return base
}

// ListOllamaModels lists locally installed models from Ollama's native /api/tags API.
func ListOllamaModels(ctx context.Context, serverURL string) ([]OllamaModel, error) {
	base := normalizeOllamaBase(serverURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := sharedOllamaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama /api/tags status %d", resp.StatusCode)
	}

	var parsed struct {
		Models []struct {
			Name       string `json:"name"`
			Size       int64  `json:"size"`
			ModifiedAt string `json:"modified_at"`
			Details    struct {
				Family            string `json:"family"`
				ParameterSize     string `json:"parameter_size"`
				QuantizationLevel string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make([]OllamaModel, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		out = append(out, OllamaModel{
			Name:         m.Name,
			Size:         m.Size,
			ModifiedAt:   strings.Split(m.ModifiedAt, "T")[0],
			Family:       m.Details.Family,
			ParamSize:    m.Details.ParameterSize,
			Quantization: m.Details.QuantizationLevel,
		})
	}
	return out, nil
}

// PullOllamaModel downloads a model from the Ollama registry with streaming progress callbacks.
func PullOllamaModel(ctx context.Context, serverURL, model string, onProgress func(pct float64, status string, completed, total int64)) error {
	base := normalizeOllamaBase(serverURL)
	body, _ := json.Marshal(map[string]any{
		"name":   strings.TrimSpace(model),
		"stream": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := sharedStreamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ollama /api/pull status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var chunk struct {
			Status    string `json:"status"`
			Error     string `json:"error"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
			continue
		}
		if chunk.Error != "" {
			return fmt.Errorf("ollama pull error: %s", chunk.Error)
		}
		pct := 0.0
		if chunk.Total > 0 {
			pct = (float64(chunk.Completed) / float64(chunk.Total)) * 100.0
		}
		if onProgress != nil {
			onProgress(pct, chunk.Status, chunk.Completed, chunk.Total)
		}
	}
	return scanner.Err()
}

// DeleteOllamaModel deletes an installed model via Ollama's /api/delete API.
func DeleteOllamaModel(ctx context.Context, serverURL, model string) error {
	base := normalizeOllamaBase(serverURL)
	body, _ := json.Marshal(map[string]string{"name": strings.TrimSpace(model)})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/api/delete", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sharedOllamaClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ollama /api/delete status %d", resp.StatusCode)
	}
	return nil
}

// OllamaPullRunner implements jobs.Runner for pulling Ollama models with live progress.
type OllamaPullRunner struct{}

// Run executes `POST /api/pull` against the AI server URL stored in metadata FileName.
func (r *OllamaPullRunner) Run(ctx context.Context, job *jobs.Job, progress jobs.ProgressFunc) error {
	meta := jobs.ParseMetadata(job.Metadata)
	if progress != nil {
		progress(2, "Starting Ollama pull for "+job.Target+"...", 0, 0, 0)
	}
	err := PullOllamaModel(ctx, meta.FileName, job.Target, func(pct float64, status string, completed, total int64) {
		if progress != nil {
			progress(pct, status, completed, total, 0)
		}
	})
	if err != nil {
		return err
	}
	if progress != nil {
		progress(100, "Model ready: "+job.Target, 0, 0, 0)
	}
	return nil
}
