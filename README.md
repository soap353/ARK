<p align="center">
  <img src="internal/web/static/ark-logo.png" alt="Ark Logo" width="140">
</p>

<h1 align="center">Ark — Offline Internet Archive</h1>

<p align="center">
  <strong>Self-hosted, offline-first knowledge hub for local networks and grid-down independence.</strong>
</p>

Ark runs natively as a single Go binary (Windows, Linux, and macOS) with SQLite WAL persistence and zero required external runtime dependencies. It brings together offline encyclopedias, vector maps, medical references, local AI search, field calculators, LAN file sharing, LoRa mesh messaging, and optional Docker/Podman apps behind a clean, responsive web interface.

---

## Key Features

- **Global Home Search & Autocorrect (`/`)**: Live search (`/api/search`) across local Library titles, Calculators, installed & downloadable Vector Maps and waypoints, Field Guide remedies, LoRa Mesh nodes, LAN Drop bulletin posts, AI chat sessions, and FDA Drug Labels—complete with offline typo autocorrect and an exact-query toggle.
- **Offline Library & Native ZIM Reader (`/library`, `/kiwix`, `/get-content`, `/read/zim/`)**: Pure-Go in-process ZIM reader (no Kiwix container needed) with live title suggestions, saved reader themes, Kiwix OPDS catalog & curated bundle downloads, and a topbar **Download** menu to export articles as **PDF** or standalone offline **HTML**.
- **Offline Vector Maps (`/maps`)**: Native MapLibre GL + PMTiles v3 tile server supporting the world basemap (`z0-6`), regional/country extracts (`z0-12` to `z0-15`), all 50 US states at street & building detail (`z0-15`), a collapsible Download Maps picker, and persistent custom waypoints.
- **Ask AI with SQLite RAG (`/ask`)**: Connects to any local OpenAI/Ollama-compatible endpoint (Ollama, LM Studio, llama.cpp, vLLM) with pure-Go SQLite FTS5 indexing and retrieval (no Qdrant required), inline citations, direct ZIM/document/map links (`?ark_hl=`), vision model support, and pinnable chat sessions.
- **Field Guide & Calculators / Celestial Tracker (`/field-guide`, `/tools`)**: Searchable offline home & natural remedy reference by symptom, condition, or ingredient, plus a unified calculator dropdown containing a Graphing Calculator, Dosage, Solar & Battery, Radio & Antenna, Coordinates & Bearing, Unit Converter, and real-time J2000 Celestial & Star Tracker.
- **LAN Drop, OPDS & Peer Sync (`/lan-drop`, `/opds`, `/library`)**: Local bulletin board and file drop, an OPDS 1.2 XML feed for e-readers (KOReader, ReadEra, Calibre), and automatic UDP LAN peer discovery (plus manual peer hosts) to browse and pull archives between Ark instances.
- **LoRa Mesh Radio (`/mesh`)**: Built-in USB Serial and Web Bluetooth LE support for Meshtastic (Protobuf) and MeshCore radios with a persistent SQLite message feed and node telemetry.
- **Optional Containerized Apps (`/apps`)**: Isolated optional Docker/Podman apps proxied under `/a/<id>/` (ConvertX, Data Chef / CyberChef, Drug Labels, Flatnotes, Kolibri, LibreTranslate, plus optional `maps` tile container) with per-app RAM caps and an optional rootless Podman installer (`ark setup-runtime`) on Intel Linux.
- **LAN Security, TLS & Offline Updates (`/settings`)**: Host-only admin routes, optional LAN passcode gating with an auto-managed trusted IP allowlist, CSRF origin checks, per-IP rate limiting, auto-generated self-signed HTTPS (`/tls/ark.crt`), sanitized USB archive export (`ark usb`), and Ed25519 + SHA-256 verified offline updates with automatic startup rollback (`ark update`, `ark rollback`).
- **Selectable UI Color Palettes**: Switch themes anytime in Settings between **Field Almanac** (warm parchment & forest, default), **Expedition Topo Atlas** (crisp stone & signal orange), and **Nightwatch Console** (dark charcoal & amber).

---

## Quick Start

### Prebuilt Release Binaries

Download prebuilt cross-platform binaries from [GitHub Releases](https://github.com/soap353/ARK/releases):

- `ark-windows-amd64.exe`
- `ark-linux-amd64`
- `ark-linux-arm64`
- `ark-darwin-arm64`
- `checksums.txt` (SHA-256 verification)

### Build & Run from Source

Requires [Go 1.27+](https://go.dev/) (and optionally Docker or Podman only if you want to run containerized apps):

```bash
git clone https://github.com/soap353/ARK.git
cd ARK

# Linux / macOS
go build -o ark ./cmd/ark
./ark run -data-dir ./data -port 8080

# Windows (PowerShell)
go build -o ark.exe ./cmd/ark
.\ark.exe run -data-dir .\data -port 8080
```

Open [http://localhost:8080](http://localhost:8080) in your browser.

### CLI Commands & Flags

```text
ark [command] [options]
```

| Command | Purpose |
|---|---|
| `run` | Start the Ark Command Center natively on the host (default; no Docker required) |
| `setup-runtime` | Download & configure rootless Podman on Intel Linux (optional, for containerized apps) |
| `update` | Verify an Ed25519-signed offline bundle (`--from <path>`) and stage a binary/DB update |
| `rollback` | Roll back to the previous Ark binary and SQLite database snapshot |
| `usb` | Export selected content categories, sanitized config, and optional images (`--to <path>`) |
| `move-data` | Relocate `ArkData` to a new path (`--to <path>`) with SHA-256 verification |
| `uninstall` | Remove Ark containers, images, volumes, and optionally data (`--all`, `--keep-data`) |

Common `run` flags: `-port` (default `8080`), `-data-dir` (auto-detects `./data` or `./ArkData`), `-no-browser`, `-foreground`, `-container`, `-no-container` (default).

---

## Architecture

```text
Browser / Phone / E-Reader / LoRa Radio (LAN)
                     │
                     ▼
      Ark Command Center (:8080 / TLS)
     Native Go 1.27 · SQLite WAL · FTS5
                     │
     ┌───────────────┼───────────────┐
     ▼               ▼               ▼
Native Engines   /a/<id>/        Job Queue
(ZIM Reader,     App Proxy       (Downloads,
 PMTiles Maps,   (Optional       AI Indexing,
 Ask AI, Mesh)   Docker/Podman)  USB Export)
```

For detailed documentation, see:

- [Architecture & Per-File Reference](docs/ARCHITECTURE.md)
- [System Requirements & Memory Profile](docs/SYSTEM_REQUIREMENTS.md)

---

## Testing

```bash
go test -count=1 ./...
```

---

## License

MIT License — see [LICENSE](LICENSE).
