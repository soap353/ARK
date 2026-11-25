<p align="center">
  <img src="internal/web/static/ark-logo.png" alt="Ark Logo" width="140">
</p>

<h1 align="center">Ark — Offline Internet Archive</h1>

<p align="center">
  <strong>Self-hosted, offline-first knowledge hub for local networks and grid-down independence.</strong>
</p>

Ark runs as a single native Go binary (Windows & Linux) with zero external runtime dependencies. It packages offline encyclopedias, vector maps, medical references, local AI search, LAN file sharing, LoRa mesh messaging, and optional Docker apps behind a clean web interface.

---

## Core Features

- **Offline Library & Native ZIM Reader**: Browse, search, and read Wikipedia, WikiMed, Project Gutenberg, iFixit, and other `.zim` archives natively with saved reader themes and Kiwix OPDS catalog downloads.
- **Offline Vector Maps (`/maps`)**: Unified MapLibre + PMTiles v3 viewer with multi-region switching, country/state downloads, and persistent custom waypoints.
- **Ask AI (`/ask`)**: Local OpenAI/Ollama-compatible RAG chat with live citations, direct ZIM/document/map links, and zero external APIs.
- **Field Guide & Tools (`/field-guide`, `/tools`)**: Offline medical/remedy reference, unit/dosage/RF calculators, graphing calculator, and real-time celestial sky chart.
- **LAN Drop, OPDS & Peer Sync (`/lan-drop`, `/opds`, `/library`)**: Local bulletin board and file drop, OPDS 1.2 feed for e-readers (KOReader, Calibre), and automatic UDP LAN discovery between Ark instances.
- **LoRa Mesh Radio (`/mesh`)**: USB Serial and Web Bluetooth support for Meshtastic and MeshCore radios with live message feed and node telemetry.
- **Optional Docker Apps (`/apps`)**: Isolated containers proxied under `/a/<id>/` (CyberChef, FlatNotes, ConvertX, FDA Drug Labels, Kolibri, LibreTranslate).
- **LAN Security & Offline Updates**: Host-only admin controls, optional LAN passcode + IP allowlist, auto-generated self-signed HTTPS (`/tls/ark.crt`), sanitized USB export, and Ed25519-signed offline updates with automatic rollback.

---

## Quick Start

### Release Binaries
Prebuilt binaries are published on [GitHub Releases](https://github.com/soap353/ARK/releases) (`ark-windows-amd64.exe`, `ark-linux-amd64`, `ark-linux-arm64`, `ark-darwin-arm64`, plus `checksums.txt`).

### Build & Run from Source
Requires [Go 1.27+](https://go.dev/) (and optionally Docker/Podman for containerized apps):

```bash
git clone https://github.com/soap353/ARK.git
cd ARK

go build -o ark.exe ./cmd/ark
./ark.exe run -data-dir ./data -port 8080
```

Open [http://localhost:8080](http://localhost:8080) in your browser.

---

## Architecture

```
Browser / Phone / E-Reader (LAN)
               │
               ▼
   Ark Command Center (:8080)
 Native Go · SQLite WAL · TLS
               │
   ┌───────────┼───────────┐
   ▼           ▼           ▼
Native      /a/<id>/    Job Queue
Readers     App Proxy   (Downloads,
(ZIM, Maps, (Docker)    USB, Index)
 AI, Mesh)
```

---

## Testing

```bash
go test -count=1 ./...
```

---

## License

MIT License — see [LICENSE](LICENSE).
