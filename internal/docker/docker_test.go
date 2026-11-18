package docker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

func TestCalculateCPUPercent(t *testing.T) {
	// Case 1: zero values / divide by zero guard
	statsZero := &container.StatsResponse{}
	pctZero := calculateCPUPercent(statsZero)
	if pctZero != 0.0 {
		t.Errorf("expected 0.0, got %f", pctZero)
	}

	// Case 2: standard non-zero delta
	statsNormal := &container.StatsResponse{
		CPUStats: container.CPUStats{
			CPUUsage: container.CPUUsage{
				TotalUsage: 2000000000, // 2s
			},
			SystemUsage: 10000000000, // 10s
			OnlineCPUs:  4,
		},
		PreCPUStats: container.CPUStats{
			CPUUsage: container.CPUUsage{
				TotalUsage: 1000000000, // 1s
			},
			SystemUsage: 8000000000, // 8s
			OnlineCPUs:  4,
		},
	}
	// cpuDelta = 1s, sysDelta = 2s, 4 CPUs -> (1 / 2) * 4 * 100 = 200%
	pct := calculateCPUPercent(statsNormal)
	if pct != 200.0 {
		t.Errorf("expected 200.0%% CPU usage, got %f", pct)
	}
}

func TestGetFreePort(t *testing.T) {
	port, err := GetFreePort()
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Errorf("invalid port returned: %d", port)
	}

	// Verify we can listen on the returned port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind tcp: %v", err)
	}
	_ = ln.Close()
}

func TestExtractContainerInfo(t *testing.T) {
	portKey, err := nat.NewPort("tcp", "8080")
	if err != nil {
		t.Fatalf("failed to create nat port: %v", err)
	}

	inspect := &container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:   "abcdef1234567890",
			Name: "/ark-kiwix",
			State: &container.State{
				Status: "running",
			},
		},
		Config: &container.Config{
			Image: "ghcr.io/kiwix/kiwix-serve:3.7.0",
		},
		NetworkSettings: &container.NetworkSettings{
			NetworkSettingsBase: container.NetworkSettingsBase{
				Ports: nat.PortMap{
					portKey: []nat.PortBinding{
						{HostIP: "127.0.0.1", HostPort: "49152"},
					},
				},
			},
		},
	}

	info, err := extractContainerInfo(inspect, "kiwix", "path")
	if err != nil {
		t.Fatalf("extractContainerInfo error: %v", err)
	}
	if info.AppID != "kiwix" {
		t.Errorf("expected AppID 'kiwix', got %q", info.AppID)
	}
	if info.Name != "ark-kiwix" {
		t.Errorf("expected Name 'ark-kiwix', got %q", info.Name)
	}
	if info.HostPort != 49152 {
		t.Errorf("expected HostPort 49152, got %d", info.HostPort)
	}
	if info.Status != "running" {
		t.Errorf("expected Status 'running', got %q", info.Status)
	}
}

func TestDockerClientGraceful(t *testing.T) {
	client, err := NewClient()
	if err != nil {
		t.Skipf("Docker client creation returned error in test env: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	available := client.IsAvailable(ctx)
	t.Logf("Docker daemon available in this environment: %v", available)
}

func TestStopAllStopsLabeledContainersConcurrently(t *testing.T) {
	var mu sync.Mutex
	stoppedIDs := make(map[string]bool)

	mockDocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			w.Header().Set("Content-Type", "application/json")
			filtersQ := r.URL.Query().Get("filters")
			if strings.Contains(filtersQ, AppLabelKey) {
				_, _ = w.Write([]byte(`[{"Id":"cnt-1","Names":["/ark-kiwix"],"Labels":{"ark.app":"kiwix"},"State":"running"},{"Id":"cnt-2","Names":["/ark-ollama"],"Labels":{"ark.app":"ollama"},"State":"running"}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/stop"):
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			for i, p := range parts {
				if p == "containers" && i+1 < len(parts) {
					mu.Lock()
					stoppedIDs[parts[i+1]] = true
					mu.Unlock()
					break
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer mockDocker.Close()

	rawCli, err := client.NewClientWithOpts(client.WithHost(mockDocker.URL), client.WithVersion("1.41"))
	if err != nil {
		t.Fatalf("create mock docker client: %v", err)
	}
	defer rawCli.Close()

	c := &Client{cli: rawCli}
	n, err := c.StopAll(context.Background(), 2)
	if err != nil {
		t.Fatalf("StopAll failed: %v", err)
	}
	if n != 2 || !stoppedIDs["cnt-1"] || !stoppedIDs["cnt-2"] {
		t.Errorf("expected 2 containers stopped (cnt-1, cnt-2), got count=%d stopped=%v", n, stoppedIDs)
	}
}
