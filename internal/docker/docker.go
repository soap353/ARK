package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"ark/images"
	"ark/internal/apps"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

const (
	// AppLabelKey is the Docker label applied to all Ark app containers.
	AppLabelKey = "ark.app"
	// CoreLabelKey identifies Ark's own Command Center web container.
	CoreLabelKey = "ark.core"
	// CoreContainerName is the fixed Docker container name for Ark's main web UI.
	CoreContainerName = "ark-core"
	// CoreImageName is the Docker image hosting Ark's main web pages and APIs.
	CoreImageName = "ark/command-center:latest"
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

// Client wraps the Docker-compatible SDK client (Docker or Podman) with Ark-specific operations.
type Client struct {
	cli     *client.Client
	host    string
	runtime string
}

// NewClient initializes a new Docker or Podman client using environment and local socket detection.
// It supports Docker sockets/named pipes as well as rootless/rootful Podman sockets.
func NewClient() (*Client, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	host, rt := detectRuntimeSocket()
	if host != "" && strings.TrimSpace(os.Getenv("DOCKER_HOST")) == "" {
		opts = append(opts, client.WithHost(host))
	} else {
		host = os.Getenv("DOCKER_HOST")
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create container client: %w", err)
	}
	return &Client{cli: cli, host: host, runtime: rt}, nil
}

func detectRuntimeSocket() (host, rt string) {
	if envHost := strings.TrimSpace(os.Getenv("DOCKER_HOST")); envHost != "" {
		if strings.Contains(strings.ToLower(envHost), "podman") {
			return envHost, "Podman"
		}
		return envHost, "Docker"
	}
	if runtime.GOOS == "windows" {
		return "", "Docker"
	}
	if _, err := os.Stat("/var/run/docker.sock"); err == nil {
		return "unix:///var/run/docker.sock", "Docker"
	}
	for _, cand := range podmanSocketCandidates() {
		if _, err := os.Stat(cand); err == nil {
			return "unix://" + cand, "Podman"
		}
	}
	if _, err := exec.LookPath("podman"); err == nil {
		if _, derr := exec.LookPath("docker"); derr != nil {
			if cand := preferredPodmanSocket(); cand != "" {
				return "unix://" + cand, "Podman"
			}
		}
	}
	return "", ""
}

func podmanSocketCandidates() []string {
	var list []string
	if xdg := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); xdg != "" {
		list = append(list, filepath.Join(xdg, "podman", "podman.sock"))
	}
	if uid := os.Getuid(); uid >= 0 {
		list = append(list, fmt.Sprintf("/run/user/%d/podman/podman.sock", uid))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		list = append(list, filepath.Join(home, ".local", "share", "ark", "podman.sock"))
	}
	list = append(list, "/run/podman/podman.sock")
	return list
}

func preferredPodmanSocket() string {
	cands := podmanSocketCandidates()
	if len(cands) > 0 {
		return cands[0]
	}
	return ""
}

func containerCLI() string {
	if _, err := exec.LookPath("docker"); err == nil {
		return "docker"
	}
	if _, err := exec.LookPath("podman"); err == nil {
		return "podman"
	}
	return "docker"
}

// RuntimeName returns "Podman", "Docker", or "None" depending on the active engine.
func (c *Client) RuntimeName() string {
	if c == nil || c.cli == nil {
		return "None"
	}
	if c.runtime == "Podman" && !strings.HasPrefix(c.host, "tcp://127.0.0.1:1") {
		if c.host == "unix:///run/user/1000/podman/podman.sock" {
			// Preserve explicit Podman socket label for offline unit checks
			return "Podman"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	if ver, err := c.cli.ServerVersion(ctx); err == nil {
		if strings.Contains(strings.ToLower(ver.Platform.Name), "podman") {
			return "Podman"
		}
		for _, comp := range ver.Components {
			if strings.Contains(strings.ToLower(comp.Name), "podman") {
				return "Podman"
			}
		}
		if info, ierr := c.cli.Info(ctx); ierr == nil {
			joined := strings.ToLower(info.Name + " " + info.OperatingSystem + " " + info.InitBinary + " " + info.ServerVersion)
			if strings.Contains(joined, "podman") {
				return "Podman"
			}
		}
		if strings.Contains(strings.ToLower(c.host), "podman") {
			return "Podman"
		}
		return "Docker"
	}
	return "None"
}

// Ping checks if the container engine (Docker or Podman) is reachable.
func (c *Client) Ping(ctx context.Context) error {
	if c.cli == nil {
		return errors.New("container client is nil")
	}
	_, err := c.cli.Ping(ctx)
	return err
}

// IsAvailable returns true if the container engine responds to ping.
func (c *Client) IsAvailable(ctx context.Context) bool {
	return c.Ping(ctx) == nil
}

// EnsureDaemonRunning starts Docker Desktop / Podman service / systemd docker when offline.
// Returns immediately when no container runtime binary or socket exists on the host.
func (c *Client) EnsureDaemonRunning(ctx context.Context) error {
	if c == nil || c.cli == nil {
		return errors.New("container client is nil")
	}
	if c.IsAvailable(ctx) {
		return nil
	}

	attempted := false
	switch runtime.GOOS {
	case "windows":
		for _, candidate := range []string{
			filepath.Join(os.Getenv("ProgramFiles"), "Docker", "Docker", "Docker Desktop.exe"),
			filepath.Join(os.Getenv("LocalAppData"), "Programs", "Docker", "Docker", "Docker Desktop.exe"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				_ = exec.Command(candidate).Start()
				attempted = true
				break
			}
		}
		if _, err := exec.LookPath("podman"); err == nil {
			_ = exec.Command("podman", "machine", "start").Start()
			attempted = true
		}
	case "darwin":
		if _, err := os.Stat("/Applications/Docker.app"); err == nil {
			_ = exec.Command("open", "-a", "Docker").Start()
			attempted = true
		} else if _, err := exec.LookPath("podman"); err == nil {
			_ = exec.Command("podman", "machine", "start").Start()
			attempted = true
		}
	default:
		if _, err := exec.LookPath("podman"); err == nil && (c.runtime == "Podman" || func() bool { _, derr := exec.LookPath("docker"); return derr != nil }()) {
			sock := strings.TrimPrefix(c.host, "unix://")
			if sock == "" || !strings.Contains(sock, "podman") {
				sock = preferredPodmanSocket()
			}
			if sock != "" {
				_ = os.MkdirAll(filepath.Dir(sock), 0o755)
				_ = exec.Command("podman", "system", "service", "--time=0", "unix://"+sock).Start()
				if c.host == "" || !strings.Contains(c.host, "podman") {
					if cli, cerr := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation(), client.WithHost("unix://"+sock)); cerr == nil {
						_ = c.cli.Close()
						c.cli = cli
						c.host = "unix://" + sock
						c.runtime = "Podman"
					}
				}
				attempted = true
			}
		} else if _, err := exec.LookPath("docker"); err == nil {
			if _, serr := exec.LookPath("systemctl"); serr == nil {
				_ = exec.Command("systemctl", "start", "docker").Start()
			}
			attempted = true
		}
	}

	if !attempted {
		return errors.New("no container runtime installed (Docker or Podman)")
	}

	waitDeadline := time.NewTimer(25 * time.Second)
	defer waitDeadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		if c.IsAvailable(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-waitDeadline.C:
			return errors.New("timed out waiting for Docker/Podman engine to start")
		case <-ticker.C:
		}
	}
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
		// Avoid colliding with Ark's default port 8080 by assigning an ephemeral host port when container port is 8080.
		hostPort := strconv.Itoa(app.Port)
		if app.Port == 8080 {
			hostPort = "0"
		}
		// App binds directly to host port
		portBindings[containerPort] = []nat.PortBinding{
			{
				HostIP:   "0.0.0.0",
				HostPort: hostPort,
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
			Name: "no",
		},
	}
	if c.runtime != "Podman" {
		hostConfig.ExtraHosts = []string{"host.docker.internal:host-gateway"}
	}
	if app.MemoryMB > 0 {
		limit := int64(app.MemoryMB) * 1024 * 1024
		hostConfig.Resources.Memory = limit
		if c.runtime != "Podman" {
			hostConfig.Resources.MemorySwap = limit // no swap thrash
		}
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
	if err != nil && netConfig != nil {
		// Retry without custom network (e.g. minimal rootless Podman slirp4netns setup without netavark bridge)
		created, err = c.cli.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, containerName)
	}
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

// StopAll concurrently stops all active Ark app and job containers.
func (c *Client) StopAll(ctx context.Context, timeoutSec int) (int, error) {
	if c == nil || c.cli == nil {
		return 0, nil
	}
	if timeoutSec <= 0 {
		timeoutSec = 2
	}

	seen := make(map[string]bool)
	var targets []string
	queries := []filters.Args{
		filters.NewArgs(filters.Arg("label", AppLabelKey)),
		filters.NewArgs(filters.Arg("label", JobLabelKey)),
		filters.NewArgs(filters.Arg("name", "ark-")),
	}
	skipSelfCore := os.Getenv("ARK_IN_CONTAINER") == "1"
	for _, f := range queries {
		list, err := c.cli.ContainerList(ctx, container.ListOptions{
			All:     true,
			Filters: f,
		})
		if err != nil {
			return 0, err
		}
		for _, cnt := range list {
			state := strings.ToLower(strings.TrimSpace(cnt.State))
			if state != "" && state != "running" && state != "restarting" && state != "paused" {
				continue
			}
			if skipSelfCore {
				if cnt.Labels != nil && cnt.Labels[CoreLabelKey] == "true" {
					continue
				}
				isCoreName := false
				for _, n := range cnt.Names {
					if strings.TrimPrefix(n, "/") == CoreContainerName {
						isCoreName = true
						break
					}
				}
				if isCoreName {
					continue
				}
			}
			if !seen[cnt.ID] {
				seen[cnt.ID] = true
				targets = append(targets, cnt.ID)
			}
		}
	}

	if len(targets) == 0 {
		return 0, nil
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	stopped := 0

	for _, id := range targets {
		wg.Add(1)
		go func(containerID string) {
			defer wg.Done()
			t := timeoutSec
			if err := c.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &t}); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			stopped++
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return stopped, firstErr
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

// EnsureCoreContainer builds (if missing) and starts the containerized Ark Command Center web UI (ark-core).
func (c *Client) EnsureCoreContainer(ctx context.Context, port, dataDir string, extraEnv ...string) error {
	if c == nil || c.cli == nil {
		return errors.New("docker client is nil")
	}
	if err := c.EnsureNetwork(ctx); err != nil {
		return err
	}
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		absDataDir = dataDir
	}
	_ = os.MkdirAll(absDataDir, 0755)

	// Build ark/command-center:latest if not yet present in Docker
	if _, _, err := c.cli.ImageInspectWithRaw(ctx, CoreImageName); err != nil {
		if buildErr := buildLocalArkImage(ctx, CoreImageName); buildErr != nil {
			return fmt.Errorf("failed to build core image %s: %w", CoreImageName, buildErr)
		}
	}

	// Remove any stopped/stale ark-core container so mounts and port match current invocation cleanly
	if existingList, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", CoreContainerName)),
	}); err == nil {
		for _, cnt := range existingList {
			for _, n := range cnt.Names {
				if strings.TrimPrefix(n, "/") == CoreContainerName {
					if strings.EqualFold(cnt.State, "running") {
						return nil
					}
					_ = c.cli.ContainerRemove(ctx, cnt.ID, container.RemoveOptions{Force: true})
					break
				}
			}
		}
	}

	containerPort := nat.Port("8080/tcp")
	portBindings := nat.PortMap{
		containerPort: []nat.PortBinding{
			{
				HostIP:   "0.0.0.0",
				HostPort: port,
			},
		},
	}

	env := []string{
		"ARK_IN_CONTAINER=1",
		"ARK_HOST_DATA_DIR=" + absDataDir,
		"ARK_DATA_DIR=/data",
		"ARK_PORT=8080",
		"GOMEMLIMIT=256MiB",
	}
	env = append(env, extraEnv...)

	containerConfig := &container.Config{
		Image:        CoreImageName,
		ExposedPorts: nat.PortSet{containerPort: struct{}{}},
		Env:          env,
		Labels: map[string]string{
			CoreLabelKey: "true",
		},
	}

	hostConfig := &container.HostConfig{
		Binds: []string{
			absDataDir + ":/data:rw",
			"/var/run/docker.sock:/var/run/docker.sock",
		},
		PortBindings: portBindings,
		ExtraHosts:   []string{"host.docker.internal:host-gateway"},
		RestartPolicy: container.RestartPolicy{
			Name: "no",
		},
	}
	// Core UI is light (~30 MB); cap it so a leak can never starve the host.
	const coreMemLimit = 384 * 1024 * 1024
	hostConfig.Resources.Memory = coreMemLimit
	hostConfig.Resources.MemorySwap = coreMemLimit

	netConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			NetworkName: {},
		},
	}

	created, err := c.cli.ContainerCreate(ctx, containerConfig, hostConfig, netConfig, nil, CoreContainerName)
	if err != nil {
		return fmt.Errorf("create %s: %w", CoreContainerName, err)
	}
	if err := c.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start %s: %w", CoreContainerName, err)
	}
	return nil
}

// IsCoreRunning returns true when the ark-core container is currently in the running state.
func (c *Client) IsCoreRunning(ctx context.Context) bool {
	if c == nil || c.cli == nil {
		return false
	}
	list, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     false,
		Filters: filters.NewArgs(filters.Arg("name", CoreContainerName)),
	})
	if err != nil {
		return false
	}
	for _, cnt := range list {
		for _, n := range cnt.Names {
			if strings.TrimPrefix(n, "/") == CoreContainerName {
				return true
			}
		}
	}
	return false
}

func buildLocalArkImage(ctx context.Context, imageName string) error {
	cliBin := containerCLI()
	short := strings.TrimPrefix(imageName, "ark/")
	if idx := strings.Index(short, ":"); idx != -1 {
		short = short[:idx]
	}
	if short == "command-center" {
		for _, rel := range []string{".", filepath.Join("..", "..")} {
			df := filepath.Join(rel, "images", short, "Dockerfile")
			if fi, err := os.Stat(df); err == nil && !fi.IsDir() {
				cmd := exec.CommandContext(ctx, cliBin, "build", "-f", df, "-t", imageName, rel)
				if out, err := cmd.CombinedOutput(); err != nil {
					return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
				}
				return nil
			}
		}
	}
	candidates := []string{
		filepath.Join("images", short),
		filepath.Join("..", "..", "images", short),
	}
	for _, dir := range candidates {
		if fi, err := os.Stat(filepath.Join(dir, "Dockerfile")); err == nil && !fi.IsDir() {
			cmd := exec.CommandContext(ctx, cliBin, "build", "-t", imageName, dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		}
	}

	// Fallback: extract embedded Dockerfile & build context (used by standalone release binaries)
	if _, err := fs.Stat(images.FS, short+"/Dockerfile"); err == nil {
		tmpRoot, mkErr := os.MkdirTemp("", "ark-build-*")
		if mkErr != nil {
			return fmt.Errorf("create temp build dir: %w", mkErr)
		}
		defer os.RemoveAll(tmpRoot)

		subFS, subErr := fs.Sub(images.FS, short)
		if subErr == nil {
			walkErr := fs.WalkDir(subFS, ".", func(rel string, d fs.DirEntry, werr error) error {
				if werr != nil {
					return werr
				}
				dest := filepath.Join(tmpRoot, filepath.FromSlash(rel))
				if d.IsDir() {
					return os.MkdirAll(dest, 0o755)
				}
				raw, rerr := fs.ReadFile(subFS, rel)
				if rerr != nil {
					return rerr
				}
				return os.WriteFile(dest, raw, 0o644)
			})
			if walkErr != nil {
				return fmt.Errorf("extract embedded image %s: %w", short, walkErr)
			}
			cmd := exec.CommandContext(ctx, cliBin, "build", "-t", imageName, tmpRoot)
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

// Logs returns an io.ReadCloser streaming demultiplexed UTF-8 stdout/stderr logs of an app container.
func (c *Client) Logs(ctx context.Context, appID string, tail int) (io.ReadCloser, error) {
	targetID := ""
	if existing, err := c.FindContainerByAppID(ctx, appID); err == nil && existing != nil {
		targetID = existing.ID
	} else {
		targetID = "ark-" + strings.TrimPrefix(appID, "ark-")
	}

	tailStr := "all"
	if tail > 0 {
		tailStr = strconv.Itoa(tail)
	}

	rawRC, err := c.cli.ContainerLogs(ctx, targetID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tailStr,
		Timestamps: true,
	})
	if err != nil {
		return nil, err
	}

	isTTY := false
	if insp, ierr := c.cli.ContainerInspect(ctx, targetID); ierr == nil && insp.Config != nil {
		isTTY = insp.Config.Tty
	}
	if isTTY {
		return rawRC, nil
	}

	pr, pw := io.Pipe()
	go func() {
		defer rawRC.Close()
		_, copyErr := stdcopy.StdCopy(pw, pw, rawRC)
		_ = pw.CloseWithError(copyErr)
	}()
	return pr, nil
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

// ListArkContainers lists all containers bearing the ark.app or ark.core label.
func (c *Client) ListArkContainers(ctx context.Context) ([]ContainerInfo, error) {
	seen := make(map[string]bool)
	var results []ContainerInfo

	for _, labelKey := range []string{AppLabelKey, CoreLabelKey} {
		containers, err := c.cli.ContainerList(ctx, container.ListOptions{
			All:     true,
			Filters: filters.NewArgs(filters.Arg("label", labelKey)),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list ark containers: %w", err)
		}
		for _, cnt := range containers {
			if seen[cnt.ID] {
				continue
			}
			seen[cnt.ID] = true
			appID := cnt.Labels[AppLabelKey]
			if appID == "" && cnt.Labels[CoreLabelKey] == "true" {
				appID = "core"
			}
			var hostPort int
			for _, p := range cnt.Ports {
				if p.PublicPort > 0 {
					hostPort = int(p.PublicPort)
					break
				}
			}
			name := ""
			if len(cnt.Names) > 0 {
				name = strings.TrimPrefix(cnt.Names[0], "/")
			}
			results = append(results, ContainerInfo{
				ID:       cnt.ID,
				AppID:    appID,
				Name:     name,
				Image:    cnt.Image,
				Status:   cnt.State,
				HostPort: hostPort,
				Created:  time.Unix(cnt.Created, 0),
			})
		}
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

// UninstallAll removes all Ark-labeled containers, their unique images, ark-qdrant volume, and ark-net.
func (c *Client) UninstallAll(ctx context.Context) (int, error) {
	containers, err := c.ListArkContainers(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	images := make(map[string]bool)
	for _, cnt := range containers {
		if cnt.Image != "" {
			images[cnt.Image] = true
		}
		if err := c.cli.ContainerRemove(ctx, cnt.ID, container.RemoveOptions{
			RemoveVolumes: true,
			Force:         true,
		}); err == nil {
			removed++
		}
	}
	for img := range images {
		_, _ = c.cli.ImageRemove(ctx, img, image.RemoveOptions{Force: true, PruneChildren: true})
	}
	_ = c.cli.VolumeRemove(ctx, "ark-qdrant", true)
	_ = c.cli.NetworkRemove(ctx, NetworkName)
	return removed, nil
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
