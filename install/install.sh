#!/usr/bin/env bash
set -euo pipefail

# Ark Command Center installer & offline USB bundle loader (Linux/macOS)
# Usage: ./install.sh [--from-usb /mnt/usb/ArkBundle] [--data-dir /var/lib/ark] [--port 8080] [--upgrade]

FROM_USB=""
DATA_DIR="${ARK_DATA_DIR:-/var/lib/ark}"
PORT="${ARK_PORT:-8080}"
BIN_TARGET="/usr/local/bin/ark"
UPGRADE="false"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --from-usb) FROM_USB="$2"; shift 2 ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --port)     PORT="$2"; shift 2 ;;
    --bin)      BIN_TARGET="$2"; shift 2 ;;
    --upgrade)  UPGRADE="true"; shift ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done

echo "[Ark] Installing Ark Command Center (data: $DATA_DIR, port: $PORT)..."

if ! command -v docker >/dev/null 2>&1; then
  echo "[Ark] Error: Docker is not installed. Install Docker Engine / Docker Desktop first." >&2
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "[Ark] Warning: Docker daemon is not running yet; start Docker before launching apps." >&2
fi

mkdir -p "$DATA_DIR" "$(dirname "$BIN_TARGET")"

if [[ -n "$FROM_USB" ]]; then
  if [[ ! -f "$FROM_USB/manifest.json" ]]; then
    echo "[Ark] Error: USB bundle manifest.json not found in $FROM_USB" >&2
    exit 1
  fi
  echo "[Ark] Found offline USB bundle at $FROM_USB"

  if [[ -f "$FROM_USB/images/ark-images.tar" ]]; then
    echo "[Ark] Loading offline Docker images..."
    docker load -i "$FROM_USB/images/ark-images.tar"
  fi

  if [[ -d "$FROM_USB/data" ]]; then
    echo "[Ark] Copying offline content to $DATA_DIR..."
    cp -a "$FROM_USB/data/." "$DATA_DIR/"
  fi

  if [[ -f "$FROM_USB/config/ark.db" && ! -f "$DATA_DIR/ark.db" ]]; then
    cp -p "$FROM_USB/config/ark.db" "$DATA_DIR/ark.db"
  fi

  USB_BIN="$FROM_USB/bin/ark"
  if [[ ! -f "$USB_BIN" ]]; then
    USB_BIN="$(find "$FROM_USB/bin" -maxdepth 1 -type f -name 'ark*' | head -n 1 || true)"
  fi
  if [[ -n "$USB_BIN" && -f "$USB_BIN" ]]; then
    if [[ -f "$BIN_TARGET" ]]; then
      cp -p "$BIN_TARGET" "${BIN_TARGET}.old"
    fi
    install -m 0755 "$USB_BIN" "$BIN_TARGET"
    echo "[Ark] Installed binary to $BIN_TARGET"
  fi
elif [[ -f "./ark" ]]; then
  if [[ -f "$BIN_TARGET" && "$UPGRADE" == "true" ]]; then
    cp -p "$BIN_TARGET" "${BIN_TARGET}.old"
  fi
  install -m 0755 "./ark" "$BIN_TARGET"
  echo "[Ark] Installed local ./ark binary to $BIN_TARGET"
fi

if [[ -f "$DATA_DIR/ark.db" && "$UPGRADE" == "true" ]]; then
  mkdir -p "$DATA_DIR/backups"
  cp -p "$DATA_DIR/ark.db" "$DATA_DIR/backups/ark.db.pre-upgrade"
fi

if command -v systemctl >/dev/null 2>&1 && [[ "$(id -u)" -eq 0 ]]; then
  cat >/etc/systemd/system/ark.service <<EOF
[Unit]
Description=Ark Offline Command Center
After=network-online.target docker.service
Wants=docker.service

[Service]
Type=simple
Environment=ARK_DATA_DIR=$DATA_DIR
Environment=ARK_PORT=$PORT
ExecStart=$BIN_TARGET run -data-dir $DATA_DIR -port $PORT
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now ark.service
  echo "[Ark] Systemd unit ark.service enabled and started on port $PORT."
else
  echo "[Ark] Start Ark manually with: $BIN_TARGET run -data-dir \"$DATA_DIR\" -port \"$PORT\""
fi
