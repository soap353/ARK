package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ark/internal/apps"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

const (
	// AppLabelKey is the Docker label applied to all Ark app containers.
	AppLabelKey = "ark.app"
	// NetworkName is the Docker network dedicated to Ark services.
	NetworkName = "ark-net"
	// JobLabelKey identifies ephemeral job containers.
	JobLabelKey = "ark.job"
)

// ContainerInfo represents the runtime metadata of an Ark app container.
type ContainerInfo struct {
	ID        string    `json:"id"`
	AppID     string    `json:"app_id"`
	Name      string    `json:"name"`
	Image     string    `json:"image"`
	Status    string    `json:"status"` // "running", "stopped", "exited", etc.
	HostPort  int       `json:"host_port"`
	ProxyMode string    `json:"proxy_mode"` // "path" or "port"
	IPAddress string    `json:"ip_address"`
	Created   time.Time `json:"created"`
}

// ContainerStats holds calculated resource utilization for a container.
type ContainerStats struct {
	CPUPercent     float64 `json:"cpu_percent"`
	MemoryBytes    uint64  `json:"memory_bytes"`
	MemoryLimit    uint64  `json:"memory_limit"`
	MemoryPercent  float64 `json:"memory_percent"`
	NetworkRxBytes uint64  `json:"network_rx_bytes"`
	NetworkTxBytes uint64  `json:"network_tx_bytes"`
}

// Client wraps the Docker SDK client with Ark-specific operations.
type Client struct {
	cli *client.Client
}

// NewClient initializes a new Docker client using environment configuration.
// It supports both Unix domain sockets (/var/run/docker.sock) and Windows named pipes.
func NewClient() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Ping checks if the Docker daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	if c.cli == nil {
		return errors.New("docker client is nil")
	}
	_, err := c.cli.Ping(ctx)
	return err
}

// IsAvailable returns true if the Docker daemon responds to ping.
func (c *Client) IsAvailable(ctx context.Context) bool {
	return c.Ping(ctx) == nil
}

// Close closes the underlying Docker client transport.
func (c *Client) Close() error {
	if c.cli != nil {
		return c.cli.Close()
	}
	return nil
}

// EnsureNetwork ensures that the dedicated Ark Docker network (ark-net) exists.
func (c *Client) EnsureNetwork(ctx context.Context) error {
	nets, err := c.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", NetworkName)),
	})
	if err != nil {
		return fmt.Errorf("failed to list networks: %w", err)
	}

	for _, n := range nets {
		if n.Name == NetworkName {
			return nil
		}
	}

	_, err = c.cli.NetworkCreate(ctx, NetworkName, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{
			"ark.managed": "true",
		},
	})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("failed to create network %s: %w", NetworkName, err)
	}
	return nil
}

// Pull downloads the specified image from its registry.
func (c *Client) Pull(ctx context.Context, imageName string) error {
	reader, err := c.cli.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", imageName, err)
	}
	defer reader.Close()

	// Drain output to finish pull
	_, _ = io.Copy(io.Discard, reader)
	return nil
}

// EnsureRunning guarantees that an app container described by manifest is running.
// If the container already exists and is running, its info is returned.
// If it is stopped, it is restarted. If it does not exist, it is created and started.
func (c *Client) EnsureRunning(ctx context.Context, app *apps.AppManifest, dataDir string) (*ContainerInfo, error) {
	if err := c.EnsureNetwork(ctx); err != nil {
		return nil, err
	}

	// 1. Check existing container with label ark.app=<id>
	existing, err := c.FindContainerByAppID(ctx, app.ID)
	if err != nil {
		return nil, err
	}

	if existing != nil {
		inspect, err := c.cli.ContainerInspect(ctx, existing.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to inspect container %s: %w", existing.ID, err)
		}

		if inspect.State.Running {
			return extractContainerInfo(&inspect, app.ID, app.Proxy)
		}

		// Container exists but stopped: start it
		if err := c.cli.ContainerStart(ctx, existing.ID, container.StartOptions{}); err != nil {
			// If start fails (e.g. port conflict or old image), remove and recreate
			_ = c.cli.ContainerRemove(ctx, existing.ID, container.RemoveOptions{Force: true})
		} else {
			inspect, err := c.cli.ContainerInspect(ctx, existing.ID)
			if err != nil {
				return nil, err
			}
			return extractContainerInfo(&inspect, app.ID, app.Proxy)
		}
	}

	// 2. Ensure image exists locally (build local ark/* images from images/ if not present)
	_, _, err = c.cli.ImageInspectWithRaw(ctx, app.Image)
	if err != nil {
		if strings.HasPrefix(app.Image, "ark/") {
			if buildErr := buildLocalArkImage(ctx, app.Image); buildErr != nil {
				return nil, fmt.Errorf("failed to build local image %s: %w", app.Image, buildErr)
			}
		} else if err := c.Pull(ctx, app.Image); err != nil {
			return nil, fmt.Errorf("failed to pull image %s: %w", app.Image, err)
		}
	}

	// 3. Configure container
	containerPort := nat.Port(fmt.Sprintf("%d/tcp", app.Port))
	portBindings := nat.PortMap{}

	if app.Proxy == "port" {
		// App binds directly to host port
		portBindings[containerPort] = []nat.PortBinding{
			{
				HostIP:   "0.0.0.0",
				HostPort: strconv.Itoa(app.Port),
			},
		}
	} else {
		// App reverse-proxied via CC; bind to loopback on random port (host port 0)
		portBindings[containerPort] = []nat.PortBinding{
			{
				HostIP:   "127.0.0.1",
				HostPort: "0",
			},
		}
	}

	binds := app.ResolveBinds(dataDir)

	var envList []string
	for k, v := range app.Env {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	containerConfig := &container.Config{
		Image:        app.Image,
		Cmd:          app.Command,
		Env:          envList,
		ExposedPorts: nat.PortSet{containerPort: struct{}{}},
		Labels: map[string]string{
			AppLabelKey: app.ID,
		},
	}

	hostConfig := &container.HostConfig{
		Binds:        binds,
		PortBindings: portBindings,
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
	}

	netConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			NetworkName: {},
		},
	}

	containerName := "ark-" + app.ID

	// If container with same name exists, remove it first
	_ = c.cli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true})

	created, err := c.cli.ContainerCreate(ctx, containerConfig, hostConfig, netConfig, nil, containerName)
	if err != nil {
		return nil, fmt.Errorf("failed to create container %s: %w", containerName, err)
	}

	if err := c.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("failed to start container %s: %w", created.ID, err)
	}

	inspect, err := c.cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container %s after start: %w", created.ID, err)
	}

	return extractContainerInfo(&inspect, app.ID, app.Proxy)
}

// Stop stops the container associated with appID.
func (c *Client) Stop(ctx context.Context, appID string, timeoutSec int) error {
	existing, err := c.FindContainerByAppID(ctx, appID)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil // Already not running
	}

	stopTimeout := timeoutSec
	if stopTimeout <= 0 {
		stopTimeout = 10
	}

	return c.cli.ContainerStop(ctx, existing.ID, container.StopOptions{
		Timeout: &stopTimeout,
	})
}

// Restart restarts the container associated with appID.
func (c *Client) Restart(ctx context.Context, appID string, timeoutSec int) error {
	existing, err := c.FindContainerByAppID(ctx, appID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("app container %q not found", appID)
	}

	stopTimeout := timeoutSec
	if stopTimeout <= 0 {
		stopTimeout = 10
	}

	return c.cli.ContainerRestart(ctx, existing.ID, container.StopOptions{
		Timeout: &stopTimeout,
	})
}

func buildLocalArkImage(ctx context.Context, imageName string) error {
	short := strings.TrimPrefix(imageName, "ark/")
	if idx := strings.Index(short, ":"); idx != -1 {
		short = short[:idx]
	}
	candidates := []string{
		filepath.Join("images", short),
		filepath.Join("..", "..", "images", short),
	}
	for _, dir := range candidates {
		if fi, err := os.Stat(filepath.Join(dir, "Dockerfile")); err == nil && !fi.IsDir() {
			cmd := exec.CommandContext(ctx, "docker", "build", "-t", imageName, dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		}
	}
	return fmt.Errorf("Dockerfile not found for local image %s", imageName)
}

// Remove stops and removes the container associated with appID.
func (c *Client) Remove(ctx context.Context, appID string, force bool) error {
	existing, err := c.FindContainerByAppID(ctx, appID)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil
	}

	return c.cli.ContainerRemove(ctx, existing.ID, container.RemoveOptions{
		Force:         force,
		RemoveVolumes: false,
	})
}

// RunJob runs a short-lived container (e.g. archiver, indexer) to completion and returns its output.
func (c *Client) RunJob(ctx context.Context, imageName string, args []string, env []string, binds []string) (string, error) {
	if err := c.EnsureNetwork(ctx); err != nil {
		return "", err
	}

	containerConfig := &container.Config{
		Image: imageName,
		Cmd:   args,
		Env:   env,
		Labels: map[string]string{
			JobLabelKey: "true",
		},
	}

	hostConfig := &container.HostConfig{
		Binds: binds,
	}

	netConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			NetworkName: {},
		},
	}

	created, err := c.cli.ContainerCreate(ctx, containerConfig, hostConfig, netConfig, nil, "")
	if err != nil {
		return "", fmt.Errorf("failed to create job container: %w", err)
	}
	containerID := created.ID
	defer func() {
		_ = c.cli.ContainerRemove(context.Background(), containerID, container.RemoveOptions{Force: true})
	}()

	if err := c.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("failed to start job container: %w", err)
	}

	// Wait for container completion
	statusCh, errCh := c.cli.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return "", fmt.Errorf("error waiting for job container: %w", err)
		}
	case status := <-statusCh:
		// Read logs
		out, logErr := c.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
		})
		var logBuf bytes.Buffer
		if logErr == nil && out != nil {
			defer out.Close()
			_, _ = io.Copy(&logBuf, out)
		}

		if status.StatusCode != 0 {
			return logBuf.String(), fmt.Errorf("job exited with non-zero status code %d: %s", status.StatusCode, logBuf.String())
		}
		return logBuf.String(), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "", nil
}

// Stats returns current CPU and memory utilization for an app container.
func (c *Client) Stats(ctx context.Context, appID string) (*ContainerStats, error) {
	existing, err := c.FindContainerByAppID(ctx, appID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("app container %q not found", appID)
	}

	resp, err := c.cli.ContainerStats(ctx, existing.ID, false)
	if err != nil {
		return nil, fmt.Errorf("failed to get stats for container %s: %w", existing.ID, err)
	}
	defer resp.Body.Close()

	var stats container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("failed to parse stats json: %w", err)
	}

	cpuPercent := calculateCPUPercent(&stats)
	memBytes := stats.MemoryStats.Usage
	memLimit := stats.MemoryStats.Limit
	var memPercent float64
	if memLimit > 0 {
		memPercent = (float64(memBytes) / float64(memLimit)) * 100.0
	}

	var rxBytes, txBytes uint64
	for _, n := range stats.Networks {
		rxBytes += n.RxBytes
		txBytes += n.TxBytes
	}

	return &ContainerStats{
		CPUPercent:     cpuPercent,
		MemoryBytes:    memBytes,
		MemoryLimit:    memLimit,
		MemoryPercent:  memPercent,
		NetworkRxBytes: rxBytes,
		NetworkTxBytes: txBytes,
	}, nil
}

// Logs returns an io.ReadCloser streaming the stdout/stderr logs of an app container.
func (c *Client) Logs(ctx context.Context, appID string, tail int) (io.ReadCloser, error) {
	existing, err := c.FindContainerByAppID(ctx, appID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("app container %q not found", appID)
	}

	tailStr := "all"
	if tail > 0 {
		tailStr = strconv.Itoa(tail)
	}

	return c.cli.ContainerLogs(ctx, existing.ID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tailStr,
		Timestamps: true,
	})
}

// ImageSave saves image tarballs to out for USB export or backup.
func (c *Client) ImageSave(ctx context.Context, imageNames []string, out io.Writer) error {
	reader, err := c.cli.ImageSave(ctx, imageNames)
	if err != nil {
		return fmt.Errorf("failed to save images: %w", err)
	}
	defer reader.Close()

	_, err = io.Copy(out, reader)
	return err
}

// ImageLoad loads images from a tarball input (e.g. from USB drive during offline install).
func (c *Client) ImageLoad(ctx context.Context, input io.Reader) error {
	resp, err := c.cli.ImageLoad(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to load image: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ListArkContainers lists all containers bearing the ark.app label.
func (c *Client) ListArkContainers(ctx context.Context) ([]ContainerInfo, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", AppLabelKey)),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list ark containers: %w", err)
	}

	var results []ContainerInfo
	for _, cnt := range containers {
		appID := cnt.Labels[AppLabelKey]
		var hostPort int
		for _, p := range cnt.Ports {
			if p.PublicPort > 0 {
				hostPort = int(p.PublicPort)
				break
			}
		}

		results = append(results, ContainerInfo{
			ID:       cnt.ID,
			AppID:    appID,
			Name:     strings.TrimPrefix(cnt.Names[0], "/"),
			Image:    cnt.Image,
			Status:   cnt.State,
			HostPort: hostPort,
			Created:  time.Unix(cnt.Created, 0),
		})
	}
	return results, nil
}

// FindContainerByAppID locates an existing container with label ark.app=<appID>.
func (c *Client) FindContainerByAppID(ctx context.Context, appID string) (*container.Summary, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", fmt.Sprintf("%s=%s", AppLabelKey, appID))),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query container by label %s=%s: %w", AppLabelKey, appID, err)
	}
	if len(containers) == 0 {
		return nil, nil
	}
	return &containers[0], nil
}

func extractContainerInfo(inspect *container.InspectResponse, appID, proxyMode string) (*ContainerInfo, error) {
	var hostPort int
	for _, bindings := range inspect.NetworkSettings.Ports {
		if len(bindings) > 0 {
			if p, err := strconv.Atoi(bindings[0].HostPort); err == nil {
				hostPort = p
				break
			}
		}
	}

	ip := inspect.NetworkSettings.IPAddress
	if netSettings, ok := inspect.NetworkSettings.Networks[NetworkName]; ok && netSettings != nil {
		ip = netSettings.IPAddress
	}

	createdTime, _ := time.Parse(time.RFC3339Nano, inspect.Created)

	return &ContainerInfo{
		ID:        inspect.ID,
		AppID:     appID,
		Name:      strings.TrimPrefix(inspect.Name, "/"),
		Image:     inspect.Config.Image,
		Status:    inspect.State.Status,
		HostPort:  hostPort,
		ProxyMode: proxyMode,
		IPAddress: ip,
		Created:   createdTime,
	}, nil
}

func calculateCPUPercent(stats *container.StatsResponse) float64 {
	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage) - float64(stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemUsage) - float64(stats.PreCPUStats.SystemUsage)

	if systemDelta > 0.0 && cpuDelta > 0.0 {
		numCPUs := float64(stats.CPUStats.OnlineCPUs)
		if numCPUs == 0 {
			numCPUs = float64(len(stats.CPUStats.CPUUsage.PercpuUsage))
		}
		if numCPUs == 0 {
			numCPUs = 1.0
		}
		return (cpuDelta / systemDelta) * numCPUs * 100.0
	}
	return 0.0
}

// GetFreePort returns an available localhost port for test/fallback binding.
func GetFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
