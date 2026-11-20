package docker

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var podmanStaticTarballURL = "https://github.com/mgoltzsche/podman-static/releases/latest/download/podman-linux-amd64.tar.gz"

// CanInstallPodmanRuntime reports whether Ark can download its self-contained rootless Podman bundle on this host.
func CanInstallPodmanRuntime() bool {
	return runtime.GOOS == "linux" && runtime.GOARCH == "amd64"
}

// RuntimeRootDir returns the directory where Ark stores its optional bundled Podman runtime.
func RuntimeRootDir(dataDir string) string {
	if strings.TrimSpace(dataDir) != "" {
		return filepath.Join(dataDir, "runtime", "podman")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "ark", "runtime", "podman")
	}
	return filepath.Join(".", "ArkData", "runtime", "podman")
}

// AttachBundledRuntimeEnv prepends any Ark-downloaded Podman binary directories to PATH
// and ensures rootless container config defaults are visible.
func AttachBundledRuntimeEnv(dataDir string) {
	if runtime.GOOS == "windows" {
		return
	}
	root := RuntimeRootDir(dataDir)
	binDirs := []string{
		filepath.Join(root, "usr", "local", "bin"),
		filepath.Join(root, "usr", "bin"),
		filepath.Join(root, "bin"),
	}
	curPath := os.Getenv("PATH")
	var add []string
	for _, d := range binDirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			if !strings.Contains(curPath, d) {
				add = append(add, d)
			}
		}
	}
	if len(add) > 0 {
		_ = os.Setenv("PATH", strings.Join(add, string(os.PathListSeparator))+string(os.PathListSeparator)+curPath)
	}
	if os.Getenv("CONTAINERS_POLICY") == "" {
		pol := filepath.Join(root, "etc", "containers", "policy.json")
		if _, err := os.Stat(pol); err == nil {
			_ = os.Setenv("CONTAINERS_POLICY", pol)
		}
	}
	if os.Getenv("CONTAINERS_REGISTRIES_CONF") == "" {
		reg := filepath.Join(root, "etc", "containers", "registries.conf")
		if _, err := os.Stat(reg); err == nil {
			_ = os.Setenv("CONTAINERS_REGISTRIES_CONF", reg)
		}
	}
}

func ensurePodmanUserConfigs(root string) {
	dirs := []string{filepath.Join(root, "etc", "containers")}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, ".config", "containers"))
	}
	const defaultPolicy = `{"default":[{"type":"insecureAcceptAnything"}]}` + "\n"
	const defaultRegistries = "unqualified-search-registries = [\"docker.io\", \"ghcr.io\", \"quay.io\"]\n"

	for _, dir := range dirs {
		_ = os.MkdirAll(dir, 0o755)
		polPath := filepath.Join(dir, "policy.json")
		if _, err := os.Stat(polPath); os.IsNotExist(err) {
			_ = os.WriteFile(polPath, []byte(defaultPolicy), 0o644)
		}
		regPath := filepath.Join(dir, "registries.conf")
		if _, err := os.Stat(regPath); os.IsNotExist(err) {
			_ = os.WriteFile(regPath, []byte(defaultRegistries), 0o644)
		}
	}
}

// InstallPodmanRuntime downloads and configures the portable rootless Podman runtime on Intel Linux (linux/amd64)
// so containerized Ark apps can run without installing Docker.
func InstallPodmanRuntime(ctx context.Context, dataDir string) (string, error) {
	if !CanInstallPodmanRuntime() {
		return "", fmt.Errorf("automatic Podman bundle download supports Intel/AMD 64-bit Linux (linux/amd64); current platform is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	AttachBundledRuntimeEnv(dataDir)
	root := RuntimeRootDir(dataDir)
	if binPath, err := exec.LookPath("podman"); err == nil && binPath != "" {
		ensurePodmanUserConfigs(root)
		return "Podman is already available at " + binPath, nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, podmanStaticTarballURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("download podman-static bundle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download podman-static bundle returned HTTP %d", resp.StatusCode)
	}

	_ = os.MkdirAll(root, 0o755)
	if err := extractTarGzSafe(resp.Body, root); err != nil {
		return "", fmt.Errorf("extract podman bundle: %w", err)
	}
	ensurePodmanUserConfigs(root)
	AttachBundledRuntimeEnv(dataDir)

	if _, err := exec.LookPath("podman"); err != nil {
		return "", errors.New("podman binary not found in PATH after extraction")
	}
	return "Installed rootless Podman runtime cleanly into " + root, nil
}

func extractTarGzSafe(r io.Reader, destDir string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gzr.Close()

	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		target := filepath.Join(absDest, clean)
		if !strings.HasPrefix(target, absDest+string(filepath.Separator)) && target != absDest {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			_ = f.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err == nil {
				_ = os.Remove(target)
				_ = os.Symlink(hdr.Linkname, target)
			}
		}
	}
	return nil
}
