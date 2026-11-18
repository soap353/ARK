package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// BenchmarkResult captures a hardware + optional AI inference benchmark snapshot.
type BenchmarkResult struct {
	CPUMBs         float64 `json:"cpu_mbs"`           // Multi-core SHA-256 hash throughput (MB/s)
	MemoryGBs      float64 `json:"memory_gbs"`        // In-memory slice copy bandwidth (GB/s)
	DiskWriteMBs   float64 `json:"disk_write_mbs"`    // Sequential disk write speed (MB/s)
	DiskReadMBs    float64 `json:"disk_read_mbs"`     // Sequential disk read speed (MB/s)
	AITokensPerSec float64 `json:"ai_tokens_per_sec"` // 0 if AI server not tested/available
	AIModel        string  `json:"ai_model,omitempty"`
	CPUCores       int     `json:"cpu_cores"`
	OverallScore   int     `json:"overall_score"`
	Tier           string  `json:"tier"`
	DurationMs     int64   `json:"duration_ms"`
}

// RunBenchmark measures CPU compute, RAM copy speed, disk I/O in dataDir, and optional AI tokens/s.
func RunBenchmark(ctx context.Context, dataDir, aiURL, aiModel string) BenchmarkResult {
	start := time.Now()
	cores := runtime.NumCPU()
	res := BenchmarkResult{
		CPUCores: cores,
	}

	// 1. Multi-core CPU SHA-256 throughput (~450ms)
	cpuStart := time.Now()
	chunk := make([]byte, 1024*1024)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	var totalHashed int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	workers := cores
	if workers < 1 {
		workers = 1
	}
	if workers > 8 {
		workers = 8
	}
	deadline := cpuStart.Add(450 * time.Millisecond)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := sha256.New()
			var local int64
			for time.Now().Before(deadline) {
				if ctx.Err() != nil {
					break
				}
				h.Reset()
				_, _ = h.Write(chunk)
				_ = h.Sum(nil)
				local += int64(len(chunk))
			}
			mu.Lock()
			totalHashed += local
			mu.Unlock()
		}()
	}
	wg.Wait()
	cpuSec := time.Since(cpuStart).Seconds()
	if cpuSec > 0 {
		res.CPUMBs = (float64(totalHashed) / (1024 * 1024)) / cpuSec
	}

	// 2. RAM sequential copy bandwidth (64 MB buffer × 6 passes)
	memSrc := make([]byte, 32*1024*1024)
	memDst := make([]byte, 32*1024*1024)
	memStart := time.Now()
	var copied int64
	for i := 0; i < 8; i++ {
		copy(memDst, memSrc)
		copied += int64(len(memSrc))
	}
	memSec := time.Since(memStart).Seconds()
	if memSec > 0 {
		res.MemoryGBs = (float64(copied) / (1024 * 1024 * 1024)) / memSec
	}

	// 3. Disk write + read test in dataDir (16 MB sequential file)
	if dataDir == "" {
		dataDir = os.TempDir()
	}
	_ = os.MkdirAll(dataDir, 0755)
	benchFile := filepath.Join(dataDir, ".ark-bench-tmp.bin")
	defer os.Remove(benchFile)

	diskBuf := make([]byte, 16*1024*1024)
	wStart := time.Now()
	if err := os.WriteFile(benchFile, diskBuf, 0600); err == nil {
		wSec := time.Since(wStart).Seconds()
		if wSec > 0 {
			res.DiskWriteMBs = 16.0 / wSec
		}
		rStart := time.Now()
		if readData, rErr := os.ReadFile(benchFile); rErr == nil {
			rSec := time.Since(rStart).Seconds()
			if rSec > 0 {
				res.DiskReadMBs = (float64(len(readData)) / (1024 * 1024)) / rSec
			}
		}
	}

	// 4. Optional local AI tokens/sec benchmark
	if strings.TrimSpace(aiURL) != "" {
		res.AITokensPerSec, res.AIModel = measureAITokens(ctx, aiURL, aiModel)
	}

	// Compute composite Ark Readiness Score
	score := int(res.CPUMBs*0.18 + res.MemoryGBs*45.0 + (res.DiskWriteMBs+res.DiskReadMBs)*0.15 + res.AITokensPerSec*12.0)
	if score < 10 {
		score = 10
	}
	res.OverallScore = score
	switch {
	case score >= 1200:
		res.Tier = "Workstation Class"
	case score >= 500:
		res.Tier = "Standard Field Node"
	default:
		res.Tier = "Compact Survival Node"
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res
}

func measureAITokens(ctx context.Context, rawURL, model string) (float64, string) {
	base := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	base = strings.TrimSuffix(base, "/v1")

	if strings.TrimSpace(model) == "" {
		model = "llama3.2:3b"
	}
	reqBody, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "Count from 1 to 20 separated by spaces."},
		},
		"max_tokens": 48,
	})

	tCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(tCtx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return 0, model
	}
	req.Header.Set("Content-Type", "application/json")

	t0 := time.Now()
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0, model
	}
	defer resp.Body.Close()

	var parsed struct {
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0, model
	}
	elapsed := time.Since(t0).Seconds()
	tokens := parsed.Usage.CompletionTokens
	if tokens == 0 && len(parsed.Choices) > 0 {
		tokens = len(strings.Fields(parsed.Choices[0].Message.Content))
	}
	if elapsed > 0 && tokens > 0 {
		return float64(tokens) / elapsed, model
	}
	return 0, model
}
