#!/usr/bin/env bash
set -euo pipefail

# Build Ark Command Center for Linux (works on Intel/AMD x86_64 with zero Docker required)
echo "[Ark] Building Ark binary..."
CGO_ENABLED=0 go build -o ark ./cmd/ark

if [[ "${1:-}" == "--with-podman" ]]; then
  echo "[Ark] Installing self-contained rootless Podman runtime..."
  ./ark setup-runtime
else
  echo "[Ark] Built ./ark in Native Mode (no Docker required)."
  echo "[Ark] Optional: run './ark setup-runtime' later to download rootless Podman for containerized apps."
fi

echo "[Ark] Run './ark' to start Command Center on http://localhost:8080"
