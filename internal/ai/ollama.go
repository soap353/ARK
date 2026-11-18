package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ark/internal/jobs"
)

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
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
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

// PullOllamaModel downloads a model from Ollama registry with progress callbacks.
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

	resp, err := (&http.Client{Timeout: 0}).Do(req)
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
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
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
