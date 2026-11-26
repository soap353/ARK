# Ark System Requirements & Memory Profile

## Native-First Architecture

Ark runs as a **single native Go 1.27 host binary** (`cmd/ark`) by default. **Docker or Podman is not required** for the Command Center web UI, SQLite WAL database, native ZIM archive reader (`/read/zim/`), native PMTiles v3 vector map server (`/maps`), Ask AI SQLite FTS5 RAG (`/ask`), Field Guide (`/field-guide`), Calculators & Celestial Tracker (`/tools`), LAN Drop (`/lan-drop`), OPDS (`/opds`), or LoRa Mesh radio bridge (`/mesh`).

External vector databases (such as Qdrant) are no longer used or required; all AI document chunks, full-text FTS5 indexes, and chat sessions live inside `ark.db`.

## Catalog App Memory Caps (`catalog/*/app.yaml`)

Containerized apps are strictly optional. Each manifest in `catalog/*/app.yaml` defines a `memory_mb` cap enforced by `internal/docker` (hard memory limit, with swap disabled on Docker):

| App ID | Name | Runtime | `memory_mb` Cap | Notes |
|---|---|---|---|---|
| `libretranslate` | LibreTranslate | Container (`libretranslate/libretranslate:latest`) | `2048 MB` | Self-hosted offline translation; loads language models into RAM |
| `convertx` | ConvertX | Container (`c4illin/convertx:latest`) | `512 MB` | Offline document, image, audio, and video converter |
| `kolibri` | Kolibri | Container (`learningequality/kolibri:latest`) | `512 MB` | Offline K-12 learning platform |
| `druglabels` | Drug Labels | Container (`ark/druglabels:latest`) | `256 MB` | Optional FDA drug label web UI (Home global search also reads `druglabels.db` natively) |
| `flatnotes` | Flatnotes | Container (`dullage/flatnotes:latest`) | `256 MB` | Local Markdown notebook |
| `maps` | Maps | Container (`ghcr.io/protomaps/go-pmtiles:latest`) | `256 MB` | Optional external tile container; `/maps` serves `.pmtiles` natively by default |
| `cyberchef` | Data Chef | Container (`ghcr.io/gchq/cyberchef:latest`) | `128 MB` | Offline text/file encoding, hashing, and transformation |
| `meshtastic` | Meshtastic / LoRa Bridge | Native (`native/ark-mesh`) | `32 MB` | Built-in at `/mesh` (USB Serial & Web Bluetooth LE); no container started |

When launching Ark in optional containerized host mode (`ark run -container`), the `ark-core` container is capped at **384 MB** with `GOMEMLIMIT=256MiB`.

## Go Runtime & Memory Safeguards

- **Go GC & soft memory limit** (`tuneRuntimeMemory()` in `cmd/ark/main.go`): sets `debug.SetGCPercent(50)` and `debug.SetMemoryLimit(256 << 20)` (256 MiB) by default unless `GOGC` or `GOMEMLIMIT` is already set in the environment.
- **Container caps** (`internal/apps/manifest.go`, `internal/docker/docker.go`): `memory_mb` in `catalog/*/app.yaml` enforces a hard container memory ceiling (`MemorySwap = Memory` on Docker) so a single optional app cannot exhaust host RAM.
- **Streaming & size guards**: Plain-text/HTML RAG indexing (`internal/ai/indexer.go`) caps reads at 16 MiB (`maxPlainTextBytes = 16 << 20`), ZIM cluster reads are bounded at 64 MiB (`maxRead` in `internal/web/zim_reader.go`), and PMTiles metadata parsing caps at 2 MiB (`maxMetaBytes` in `internal/web/pmtiles.go`).

## Estimated Hardware Requirements

Because Core, ZIM reading, and Vector Maps run natively in-process without a container VM, baseline hardware needs are modest (estimates below were not formally benchmarked across low-spec hardware):

| Profile | RAM | CPU | Notes |
|---|---|---|---|
| **Minimum (Native Core + ZIM + Maps + Mesh)** | 512 MB – 1 GB (Linux); 2 GB (Windows/macOS) | 1–2 cores | No Docker or Podman needed. |
| **With Optional Light Containers** (`cyberchef`, `flatnotes`, `druglabels`, `convertx`, `kolibri`) | 2 GB (native Linux Docker/Podman); 4 GB (Windows/macOS VM) | 2+ cores | Docker Desktop's VM adds ~1–2 GB overhead on Windows/macOS. |
| **With LibreTranslate Active** | Add ~2 GB | 2–4 cores | Manifest caps container at `2048 MB`. |
| **With Local AI Server** (Ollama, LM Studio, llama.cpp) | Add 4–8+ GB | 4+ cores | Model parameters and context size determine RAM/VRAM usage. |

**Storage**: ~50 MB for the native `ark` binary plus archive storage (`data/zim/`, `data/maps/`, `data/media/`, `data/druglabels/`). Individual ZIM archives and z0–z15 US state / country PMTiles packs range from tens of MBs to 100+ GB; an SSD is recommended for fast ZIM and SQLite FTS5 search.

## Reducing Memory Further

- Leave Docker/Podman stopped if you only use native Ark features (Library/ZIM, Vector Maps, Ask AI via external host, Field Guide, Calculators, LAN Drop, OPDS, and LoRa Mesh).
- Stop unused optional containers from `/apps`; stopped containers consume zero RAM.
- Override Go memory tuning with `GOGC` and `GOMEMLIMIT` (for example, `GOMEMLIMIT=128MiB`) on memory-constrained SBCs.
- Avoid running LibreTranslate and large local LLM models simultaneously on 8 GB systems.
