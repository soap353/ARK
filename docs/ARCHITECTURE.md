# Ark Architecture & Per-File Reference Guide

This document provides a comprehensive, human-readable architectural reference for every file in the Ark codebase. For each file, this guide describes its **Purpose**, **How It Works**, and its **Interactions** across the system.

---

## Architecture Overview

Ark is a self-hosted "internet-in-a-box" and offline knowledge archive designed for disaster resilience and off-grid scenarios. It pairs a single native Go Command Center service with Docker-containerized application modules, pure-Go SQLite persistence, local search, and offline AI (RAG) capabilities.

```
+-----------------------------------------------------------------------------------+
|                                  Browser / LAN                                    |
+-----------------------------------------+-----------------------------------------+
                                          |
                                          v
+-----------------------------------------------------------------------------------+
|                        Ark Command Center (`cmd/ark`)                            |
|                                                                                   |
|  [Web Layer: internal/web]                                                        |
|   - Templates, HTMX, Static CSS, Routes & Handlers                                |
|   - In-Process ZIM Reader (`internal/web/zim_reader.go`)                          |
|                                                                                   |
|  [Core Services]                                                                  |
|   - Docker Orchestration: `internal/docker`                                       |
|   - App Proxy & Manifests: `internal/apps`                                        |
|   - Background Job Queue & Runners: `internal/jobs`                               |
|   - Offline AI & RAG Pipeline: `internal/ai`                                      |
|   - Kiwix & ZIM Management: `internal/kiwix`                                      |
|   - Storage, Integrity & Cascade Delete: `internal/storage`                       |
|   - Field Reference Guides: `internal/fieldguide`                                 |
|   - Host Metrics, Downtime & Benchmarks: `internal/system`                        |
|   - Offline Self-Update & Rollback: `internal/update`                             |
|   - Air-Gapped USB Provisioning: `internal/usb`                                   |
|   - Pure-Go SQLite Database (WAL mode): `ArkData/ark.db`                          |
+--------------------+---------------------------------------+----------------------+
                     |                                       |
                     v                                       v
+--------------------+-------------------+   +---------------+----------------------+
|             Docker Engine              |   |           Local Storage              |
|  - Kiwix Serve (`/a/kiwix/`)           |   |  - `data/zim/` (ZIM archives)        |
|  - Map Server / PMTiles (`/a/maps/`)   |   |  - `data/maps/` (Vector map tiles)   |
|  - CyberChef (`/a/cyberchef/`)         |   |  - `data/media/` (Video/Audio/Books) |
|  - Drug Labels (`/a/druglabels/`)      |   |  - `data/notes/` (Markdown wiki)     |
|  - FlatNotes, Kolibri, ConvertX        |   |  - `data/druglabels/` (FDA DB)       |
|  - Qdrant Vector DB & Ollama LLM       |   |  - `backups/` & `bin/`               |
+----------------------------------------+   +--------------------------------------+
```

---

## 1. CLI Entrypoint (`cmd/ark/`)

### `cmd/ark/main.go`
- **Purpose**: Main CLI entrypoint, configuration bootstrapper, and subcommand dispatcher for Ark.
- **How It Works**: Parses command-line flags and subcommands (`serve`/default, `update`, `rollback`, `usb`, `move-data`, `uninstall`). On startup, verifies data directory paths, initializes the SQLite database with WAL and foreign keys enabled, executes migrations, starts Docker client hooks, initializes the background job queue, mounts HTTP routes via `internal/web`, registers Windows console/tray handlers, and manages graceful shutdown on SIGINT/SIGTERM.
- **Interactions**: Imports and coordinates `internal/ai`, `internal/apps`, `internal/docker`, `internal/jobs`, `internal/kiwix`, `internal/storage`, `internal/system`, `internal/update`, `internal/usb`, and `internal/web`.

### `cmd/ark/main_test.go`
- **Purpose**: Unit tests for CLI flag parsing, path resolution, and command routing.
- **How It Works**: Executes test cases against `resolveDefaultDataDir` and CLI subcommand dispatch logic using temporary directories and mock flags.
- **Interactions**: Tests functions in `cmd/ark/main.go`.

### `cmd/ark/console_windows.go` & `cmd/ark/console_other.go`
- **Purpose**: OS-specific console event handling and graceful process termination.
- **How It Works**: On Windows, `console_windows.go` registers a Win32 `SetConsoleCtrlHandler` callback to capture `CTRL_CLOSE_EVENT`, `CTRL_LOGOFF_EVENT`, and `CTRL_SHUTDOWN_EVENT`, triggering clean database flushing and Docker teardown. On non-Windows platforms, `console_other.go` provides a no-op stub.
- **Interactions**: Invoked during initialization in `cmd/ark/main.go`.

### `cmd/ark/tray_windows.go` & `cmd/ark/tray_other.go`
- **Purpose**: Windows system tray integration and detached background execution.
- **How It Works**: On Windows, `tray_windows.go` checks server health at `/api/health`, initializes a native Win32 system tray notification icon with "Open Command Center" and "Quit Ark" context menu items, and handles browser launching. `tray_other.go` supplies no-op stubs for Linux/macOS.
- **Interactions**: Invoked from `cmd/ark/main.go` when run as a background service or tray app.

---

## 2. Docker & Container Management (`internal/docker/`)

### `internal/docker/docker.go`
- **Purpose**: Abstraction wrapper around Docker Engine SDK for managing Ark's containerized application ecosystem.
- **How It Works**: Connects to the local Docker daemon via environment socket or Windows named pipe. Manages bridge networks (`ark-net`), inspects daemon readiness, creates/starts/stops/restarts containers with the `ark.app=<id>` label, executes short-lived job containers (`RunJob`), captures real-time CPU/memory stats, streams logs, and exports/imports images (`ImageSave`/`ImageLoad`).
- **Interactions**: Utilized by `cmd/ark/main.go`, `internal/web` (app actions, telemetry, log streaming), and `internal/usb` (exporting image tarballs).

### `internal/docker/docker_test.go`
- **Purpose**: Unit tests for container configuration, CPU percentage calculations, port allocation, and daemon availability checks.
- **How It Works**: Tests calculation logic against known Docker stats structs and validates graceful degradation when the Docker daemon is unreachable.
- **Interactions**: Tests `internal/docker/docker.go`.

---

## 3. Host System Telemetry & Downtime (`internal/system/`)

### `internal/system/system.go`
- **Purpose**: Host hardware and system metrics collector.
- **How It Works**: Queries host metrics using `gopsutil/v4` (or delegates to `ARK_HOST_METRICS_URL` if containerized) to capture CPU utilization percentages, RAM total/used/free, disk capacity, host uptime, platform, and hostname.
- **Interactions**: Used by `internal/system/sse.go`, `internal/system/benchmark.go`, and `internal/web` handlers.

### `internal/system/sse.go`
- **Purpose**: Server-Sent Events (SSE) live telemetry feed for dashboard monitoring.
- **How It Works**: Implements an `http.Handler` that emits an immediate JSON metrics frame via `sendMetricsFrame` and streams updates every 2 seconds over HTTP chunked transfer encoding until client context cancellation.
- **Interactions**: Calls `CollectMetrics` in `system.go`; mounted at `/api/system/metrics` in `internal/web`.

### `internal/system/downtime.go`
- **Purpose**: Tracks offline downtime between Command Center service restarts.
- **How It Works**: Records normalized UTC timestamps in SQLite `settings` (`last_shutdown_at`) during shutdown and computes elapsed offline duration on startup using `RecordStartupAndComputeDowntime`.
- **Interactions**: Called during startup/shutdown in `cmd/ark/main.go`; surfaced in `internal/web` system dashboards.

### `internal/system/benchmark.go`
- **Purpose**: On-demand hardware and AI inference performance benchmarking.
- **How It Works**: Executes multi-core SHA-256 cryptographic throughput tests, memory buffer copy bandwidth tests, sequential disk write/read throughput tests, and optional local AI token/s generation measurements against `/v1/chat/completions`.
- **Interactions**: Invoked from the system settings page via `internal/web/handlers_nomad.go`.

### `internal/system/system_test.go`
- **Purpose**: Unit tests for host metric collection, SSE streaming, downtime calculation, and benchmarking.
- **How It Works**: Uses mock HTTP response recorders and temporary SQLite databases to verify metric structures, downtime arithmetic, and SSE frame formatting.
- **Interactions**: Validates `internal/system`.

---

## 4. Updates & Rollback (`internal/update/`)

### `internal/update/update.go`
- **Purpose**: Air-gapped and offline binary self-update, SQLite database backup, and automatic health rollback.
- **How It Works**: Validates Ed25519 digital signatures and SHA-256 hashes on incoming update manifests, snapshots `ark.db`, swaps executables atomically (`ark.exe` -> `ark.old`), and marks a pending update state. If the new binary fails to start or pass `/healthz` checks, automatically restores the backup snapshot and binary.
- **Interactions**: Invoked by `cmd/ark/main.go` (`update`/`rollback` commands) and `internal/web/handlers_phase3.go`.

### `internal/update/github.go`
- **Purpose**: Online GitHub Releases integration for checking and downloading updates.
- **How It Works**: Queries the GitHub Releases API for the latest semver tag, compares against `ArkVersion`, downloads architecture-specific binaries and `checksums.txt`, and invokes `ApplyUpdate`.
- **Interactions**: Used by `internal/web/handlers_nomad.go` and `internal/update/update.go`.

### `internal/update/update_test.go`
- **Purpose**: Unit tests for cryptographic signature verification, rollback triggers, and GitHub release JSON parsing.
- **How It Works**: Generates test Ed25519 keypairs, signs mock binaries, verifies signature rejection on tampered payloads, and tests file swapping.
- **Interactions**: Validates `internal/update`.

---

## 5. USB Drive & Air-Gapped Provisioning (`internal/usb/`)

### `internal/usb/usb.go`
- **Purpose**: Removable drive discovery, offline content/image export, and data directory relocation.
- **How It Works**: Enumerates removable drives via OS mount points, scans ArkData categories, exports selected data bundles and Docker images, writes SHA-256 checksum manifests, and supports atomic data migration via `MoveDataDir`.
- **Interactions**: Utilized by `cmd/ark/main.go` (`usb`, `move-data` subcommands), `internal/jobs/usb.go`, and `internal/web/handlers_phase3.go`.

### `internal/usb/usb_test.go`
- **Purpose**: Unit tests for removable drive detection, export profile validation, and data relocation.
- **How It Works**: Tests manifest generation, directory copying, and validation logic against temporary mock drive directories.
- **Interactions**: Validates `internal/usb/usb.go`.

---

## 6. Offline AI & RAG Engine (`internal/ai/`)

### `internal/ai/client.go`
- **Purpose**: Unified client for local LLM text generation, chat streaming, and vector embeddings.
- **How It Works**: Wraps OpenAI-compatible endpoints (`/v1/chat/completions`, `/v1/embeddings`) and Ollama APIs, supporting both streaming SSE tokens and buffered requests with configurable timeouts.
- **Interactions**: Consumed by `internal/ai/indexer.go`, `internal/web/handlers_phase2.go` (Ask AI), and `internal/system/benchmark.go`.

### `internal/ai/extract_native.go`
- **Purpose**: Pure-Go in-process text extraction from documents.
- **How It Works**: Parses PDFs, EPUBs, HTML, Markdown, and plain text without external dependencies, extracting text streams, headings, and metadata for RAG indexing.
- **Interactions**: Invoked by `internal/ai/indexer.go` during document indexing jobs.

### `internal/ai/indexer.go`
- **Purpose**: Document chunking, vector embedding, SQLite FTS5 ingestion, Qdrant upserting, and hybrid search.
- **How It Works**: Splits extracted text into token-sized chunks with overlap, generates embeddings via `client.go`, indexes text in SQLite FTS5 (`rag_chunks`) and Qdrant collections, and provides reciprocal-rank hybrid retrieval for `/ask`.
- **Interactions**: Coordinates `client.go`, `extract_native.go`, `qdrant.go`, `internal/jobs`, and `internal/web`.

### `internal/ai/ollama.go`
- **Purpose**: Management client for local Ollama container instances.
- **How It Works**: Interacts with Ollama's HTTP API (`/api/tags`, `/api/pull`, `/api/delete`, `/api/show`) to monitor local models, pull curated presets, and track download progress.
- **Interactions**: Used by `internal/web/handlers_nomad.go` and `internal/web/handlers_phase2.go`.

### `internal/ai/qdrant.go`
- **Purpose**: Lightweight REST client for the Qdrant vector database.
- **How It Works**: Creates and configures collections with distance metrics (Cosine/Dot), upserts vector points with document payload metadata, and executes vector similarity searches.
- **Interactions**: Used by `internal/ai/indexer.go` and `internal/storage/delete.go`.

### `internal/ai/sessions.go`
- **Purpose**: Persistent chat session and conversational history manager.
- **How It Works**: Manages CRUD operations in SQLite for chat sessions and message logs, allowing persistent multi-turn conversations in the Ask AI interface.
- **Interactions**: Consumed by `internal/web/handlers_phase2.go`.

### `internal/ai/ai_test.go`
- **Purpose**: Unit tests for text chunking, embedding generation, FTS query construction, and session persistence.
- **How It Works**: Uses mock HTTP servers and in-memory SQLite instances to verify tokenizer logic and database transactions.
- **Interactions**: Validates `internal/ai`.

---

## 7. App Modules & Reverse Proxy (`internal/apps/`)

### `internal/apps/manifest.go`
- **Purpose**: `app.yaml` schema parser, validator, and volume mount resolver.
- **How It Works**: Decodes YAML application manifests, validates IDs, ports, categories, proxy modes (`path` vs `port`), and LAN access rules (`read` vs `admin`), and translates volume definitions into host-absolute bind paths.
- **Interactions**: Consumed by `internal/apps/proxy.go`, `internal/apps/custom.go`, and `cmd/ark/main.go`.

### `internal/apps/proxy.go`
- **Purpose**: Reverse proxy routing manager for apps served under `/a/<id>/`.
- **How It Works**: Wraps `net/http/httputil.ReverseProxy`, dynamic rewrites URL prefixes, injects the Ark navigation header into proxied HTML responses, and returns user-friendly offline status pages via `writeAppStatusPage` when containers are starting or stopped.
- **Interactions**: Mounted on the root HTTP mux in `internal/web/web.go`; queries `internal/docker`.

### `internal/apps/custom.go`
- **Purpose**: Storage and management of user-defined custom application manifests.
- **How It Works**: Reads, validates, and writes custom `app.yaml` manifests in `<ArkData>/custom_apps/`.
- **Interactions**: Consumed by `internal/web/handlers_nomad.go` and `internal/apps/manifest.go`.

### `internal/apps/manifest_test.go` & `internal/apps/proxy_test.go`
- **Purpose**: Unit tests for manifest validation rules, volume binding, URL prefix rewriting, and proxy routing.
- **How It Works**: Tests valid and malformed YAML inputs, mock backend reverse proxy requests, and navbar injection filters.
- **Interactions**: Validates `internal/apps`.

---

## 8. Background Jobs & Download Queue (`internal/jobs/`)

### `internal/jobs/model.go`
- **Purpose**: Data types, job status enums, and runner interfaces for background tasks.
- **How It Works**: Defines `Job`, `JobStatus` (`pending`, `running`, `done`, `failed`, `paused`), and the `Runner` interface.
- **Interactions**: Shared across all job runners and queue managers.

### `internal/jobs/store.go`
- **Purpose**: SQLite-backed persistent job queue repository.
- **How It Works**: Manages transactional job queries, status updates, retry tracking, progress updates, and atomic job dequeuing (`AcquireNextPending`).
- **Interactions**: Consumed by `internal/jobs/queue.go` and `internal/web/web.go`.

### `internal/jobs/queue.go`
- **Purpose**: Concurrent job dispatcher and worker pool.
- **How It Works**: Manages worker goroutines constrained by type-specific semaphores (download=2, encode=1, index=1), dispatches tasks to registered `Runner` instances, and handles cancellation.
- **Interactions**: Coordinates runners in `runners.go`, `druglabels.go`, and `usb.go`.

### `internal/jobs/runners.go`
- **Purpose**: Core runners for downloads, site archiving, and media capture (`aria2c`, `zimit`, `yt-dlp`).
- **How It Works**: Invokes external containerized tools or native HTTP fallback downloaders with resume support, updates progress in SQLite, and registers downloaded ZIMs via `internal/kiwix`.
- **Interactions**: Interacts with `internal/docker`, `internal/kiwix`, and `internal/jobs/store.go`.

### `internal/jobs/druglabels.go` & `internal/jobs/usb.go`
- **Purpose**: Specialized runners for OpenFDA bulk ingestion and USB drive export jobs.
- **How It Works**: Handles unzipping and streaming drug labels into SQLite and copying data archives to USB drives with checksum verification.
- **Interactions**: Implements `Runner` for `internal/jobs/queue.go`.

### `internal/jobs/extra_runners.go`
- **Purpose**: Extensible runner registry for custom auxiliary background tasks.
- **How It Works**: Allows modular registration of custom job types without modifying core queue logic.
- **Interactions**: Registered with `internal/jobs/queue.go`.

### `internal/jobs/jobs_test.go`
- **Purpose**: Unit tests for job queue state transitions, semaphores, cancellation, and download fallback.
- **How It Works**: Verifies FIFO queue behavior, concurrent execution limits, and retry handlers.
- **Interactions**: Validates `internal/jobs`.

---

## 9. Kiwix, OPDS & ZIM Management (`internal/kiwix/`)

### `internal/kiwix/opds.go`
- **Purpose**: Atom/OPDS 1.2 catalog feed parser for Kiwix repositories (`library.kiwix.org`).
- **How It Works**: Parses XML feeds, extracting ZIM book entries, categories, download links (HTTP/meta4), file sizes, languages, and flavour metadata.
- **Interactions**: Populates `internal/kiwix/catalog.go`.

### `internal/kiwix/catalog.go`
- **Purpose**: Cached catalog repository and search filter.
- **How It Works**: Caches OPDS entries in the SQLite `catalog_items` table, providing multi-criteria offline filtering by language, category, size, and flavour.
- **Interactions**: Consumed by `internal/web/handlers_nomad.go` and `internal/web/web.go`.

### `internal/kiwix/presets.go`
- **Purpose**: Curated offline preset definitions (Wikipedia Mini/Full, WikiMed, iFixit, Prepper Starter Pack).
- **How It Works**: Maps preset tiers to catalog items and estimated storage footprints.
- **Interactions**: Used by the "Get Content" page in `internal/web`.

### `internal/kiwix/library.go`
- **Purpose**: Kiwix `library.xml` configuration manager.
- **How It Works**: Updates `library.xml` for `kiwix-serve` using `kiwix-manage` if available, or falls back to an embedded pure-Go XML serializer.
- **Interactions**: Called by `internal/jobs/runners.go` upon download completion and `internal/storage/delete.go` during deletion.

### `internal/kiwix/nomad_catalog.go`
- **Purpose**: Curated NOMAD offline knowledge packs and category mappings.
- **How It Works**: Defines static starter bundles for emergency preparedness, medical guides, and reference libraries.
- **Interactions**: Used by `catalog.go` and `presets.go`.

### `internal/kiwix/kiwix_test.go`
- **Purpose**: Unit tests for OPDS feed parsing, catalog caching, preset expansion, and `library.xml` manipulation.
- **How It Works**: Uses sample OPDS XML fixtures and temporary XML files to verify parsing and serialization.
- **Interactions**: Validates `internal/kiwix`.

---

## 10. Storage Accounting & Cascade Deletion (`internal/storage/`)

### `internal/storage/usage.go`
- **Purpose**: Disk usage accounting and category breakdown.
- **How It Works**: Scans `ArkData` subdirectories (`zim`, `maps`, `media`, `druglabels`, `notes`, `kolibri`, `ai_models`), calculates cumulative sizes, and queries filesystem free space via `gopsutil`.
- **Interactions**: Consumed by `internal/web/web.go` and dashboard telemetry widgets.

### `internal/storage/integrity.go`
- **Purpose**: Disk-vs-database consistency verification and orphan pruning.
- **How It Works**: Uses `resolveDataPath` to cross-reference physical files against SQLite records (`catalog_items`, `rag_documents`), identifying and pruning orphaned records or untracked files.
- **Interactions**: Triggered by user maintenance actions via `internal/web/web.go`.

### `internal/storage/delete.go`
- **Purpose**: Multi-layer cascading deletion of archived items.
- **How It Works**: Atomically deletes disk files via `resolveDataPath`, deregisters entries from Kiwix `library.xml`, cleans up vector points from Qdrant, updates USB export profiles, and deletes SQLite records.
- **Interactions**: Coordinates `internal/kiwix`, `internal/ai/qdrant.go`, and SQLite repositories.

### `internal/storage/storage_test.go`
- **Purpose**: Unit tests for disk usage reporting, orphan detection, and cascading delete verification.
- **How It Works**: Creates temporary file trees and SQLite records to verify zero orphans remain after deletion.
- **Interactions**: Validates `internal/storage`.

---

## 11. Field Reference Guide (`internal/fieldguide/`)

### `internal/fieldguide/fieldguide.go`
- **Purpose**: Offline medical reference, survival guide, and creator-pack browser.
- **How It Works**: Embeds structured reference data (`data/*.json`) via Go `embed.FS`, parsing emergency protocols, first-aid procedures, and offline checklists with keyword search and category filtering.
- **Interactions**: Mounted at `/field-guide` by `internal/web/handlers_nomad.go`.

### `internal/fieldguide/fieldguide_test.go`
- **Purpose**: Unit tests for embedded JSON loading, schema validation, and search indexing.
- **How It Works**: Verifies JSON deserialization and keyword search results.
- **Interactions**: Validates `internal/fieldguide`.

---

## 12. Web UI, Templates & Static Assets (`internal/web/`)

### `internal/web/web.go`
- **Purpose**: Core web server definition, route registration, view models (`PageData`), and primary page handlers.
- **How It Works**: Embeds HTML templates and static assets, registers HTTP routes (`/`, `/library`, `/apps`, `/get`, `/ask`, `/settings`, `/media/*`), renders templates with security headers, and enforces LAN access permissions.
- **Interactions**: Coordinates with all core packages; base struct for all handler extensions.

### `internal/web/helpers.go`
- **Purpose**: Centralized HTTP request/response helper functions.
- **How It Works**: Provides `requireMethod`, `writeJSON`, `writeJSONError`, and `redirectWithQuery` to eliminate boilerplate and standardize responses across handlers.
- **Interactions**: Used by `handlers_phase2.go`, `handlers_phase3.go`, and available to all `internal/web` handlers.

### `internal/web/handlers_nomad.go`
- **Purpose**: Extended handlers for Kiwix catalog, Field Guide, map pins/extraction, system benchmarks, and app lifecycle.
- **How It Works**: Implements API endpoints for starting/stopping Docker apps, triggering hardware benchmarks, searching the Field Guide, and updating Kiwix feeds.
- **Interactions**: Extends `Server` in `web.go`; coordinates `internal/kiwix`, `internal/fieldguide`, `internal/system`.

### `internal/web/handlers_phase2.go`
- **Purpose**: Handlers for general settings, telemetry views, container logs, and the Ask AI interface.
- **How It Works**: Streams structured LLM chat responses via live SSE tokens with Markdown/KaTeX citation repair, enforces Strict Sources Only / Protocol / Triage modes and scope filtering, tests AI server connections, handles RAG indexing requests, and displays container logs.
- **Interactions**: Extends `Server` in `web.go`; coordinates `ask_federated.go`, `internal/ai`, `internal/docker`, `internal/system`.

### `internal/web/ask_federated.go`
- **Purpose**: Federated offline retrieval, map waypoint context, auto-indexing, deterministic unit/dosage/radio calculator, and context token ratio estimation for Ask AI.
- **How It Works**: Queries local `.zim` archives on-the-fly, `internal/fieldguide` Remedy data, `data/druglabels/druglabels.db` FTS5, `map_markers` with Haversine distance, and lazily syncs changed files in `data/notes/` and `data/media/docs/`.
- **Interactions**: Called by `handlers_phase2.go` (`/api/ai/chat`) and `web.go` (`/ask`).

### `internal/web/handlers_phase3.go`
- **Purpose**: Handlers for USB export profiles, offline update deployment, and PMTiles map viewing.
- **How It Works**: Enqueues USB export jobs, applies offline update bundles, and renders offline vector map tiles.
- **Interactions**: Extends `Server` in `web.go`; coordinates `internal/usb`, `internal/update`, `internal/jobs`.

### `internal/web/zim_reader.go`
- **Purpose**: In-process offline ZIM archive viewer and asset server (`/zim/*`).
- **How It Works**: Directly parses local `.zim` archives, extracting articles, styles, images, and search indexes without requiring external containers, and rewrites internal URLs for local viewing.
- **Interactions**: Serves articles linked from the Library and Ask AI citations.

### `internal/web/pmtiles_meta.go`
- **Purpose**: Native PMTiles v3 binary header/metadata parser and HTTP Range file server (`/maps/files/*`, `/api/maps/tilejson`).
- **How It Works**: Reads 127-byte PMTiles v3 headers and gzipped JSON metadata (bounds, center, min/max zoom, vector layers) directly from disk and serves `.pmtiles` archives via `http.ServeContent` (`206 Partial Content`) so interactive vector maps render offline without requiring Docker.
- **Interactions**: Used by `handlers_phase3.go` (`/maps`), `handlers_nomad.go` (`/library`), and `templates/maps.html`.

### `internal/web/pmtiles_tile.go`
- **Purpose**: Unified, memory-efficient multi-archive PMTiles v3 tile server (`/maps/tiles/{z}/{x}/{y}.mvt`).
- **How It Works**: Caches only bounded root-directory headers (up to 64 archives), filters archives by WGS84 tile bounding-box intersection, looks up Hilbert `tileID`s with `os.File.ReadAt`, streams single-archive tiles verbatim (`Content-Encoding: gzip`), and merges overlapping border/overview MVT protobuf payloads cleanly.
- **Interactions**: Registered in `web.go` and consumed by MapLibre GL in `templates/maps.html`.

### `internal/web/web_test.go`
- **Purpose**: Integration tests for HTTP routes, template rendering, and WCAG palette compliance.
- **How It Works**: Uses `httptest` to execute GET/POST requests against all registered routes and verifies WCAG AAA color contrast tokens in `ark.css`.
- **Interactions**: Validates `internal/web`.

### `internal/web/static/ark.css`
- **Purpose**: Primary stylesheet for Ark, implementing a WCAG AAA compliant, low-density dark interface.
- **How It Works**: Defines CSS variables for background (`#0B0D10`), primary text (`#F4F4F0`), accent (`#FFB000`), secondary text (`#80868B`), surface cards (`#15181D`), and 48px touch targets, with `@media (prefers-reduced-motion)` support.
- **Interactions**: Embedded in `web.go` and served at `/static/ark.css`.

### `internal/web/static/htmx.min.js` & `internal/web/static/vendor/*`
- **Purpose**: Vendored frontend libraries (HTMX, KaTeX, Marked, MapLibre GL JS, PMTiles, Protomaps themes, Noto Sans PBF glyphs) enabling zero-CDN, fully offline interactive UI components and vector maps.
- **How It Works**: Embedded via Go `embed.FS` and served locally (with empty-protobuf fallback for unbundled Unicode glyph ranges).
- **Interactions**: Loaded by `templates/layout.html`, `templates/ask.html`, and `templates/maps.html`.

### `internal/web/templates/*.html`
- **Purpose**: Go HTML templates for all Ark views (`layout.html`, `home.html`, `library.html`, `apps.html`, `get_content.html`, `ask.html`, `kiwix.html`, `field_guide.html`, `maps.html`, `system.html`, `settings.html`).
- **How It Works**: Defines UI components, tables, search bars, and dashboards populated by `PageData` view models.
- **Interactions**: Parsed and rendered by `internal/web/web.go`.

---

## 13. Dockerfiles & Custom Images (`images/`)

### `images/archiver/Dockerfile`
- **Purpose**: Builds the `ark/archiver` container image for capturing websites, videos, and music.
- **How It Works**: Packages Alpine Linux with Python 3, `yt-dlp`, FFmpeg (with SVT-AV1), and `aria2`.
- **Interactions**: Executed by `internal/jobs/runners.go`.

### `images/command-center/Dockerfile`
- **Purpose**: Builds the containerized Linux Command Center image (`ark-core:latest`).
- **How It Works**: Copies the prebuilt Linux binary (`ark-linux`), `catalog/`, and `images/` into an Alpine container with the Docker CLI client.
- **Interactions**: Deployed in containerized Ark environments.

### `images/druglabels/Dockerfile`, `main.go`, `go.mod`, `go.sum`, `main_test.go`
- **Purpose**: Standalone microservice for searching and viewing FDA drug labels offline.
- **How It Works**: Multi-stage Go build embedding SQLite FTS5 search over OpenFDA bulk drug label exports.
- **Interactions**: Packaged as `ark/druglabels:latest` and proxied by Ark at `/a/druglabels/`.

### `images/indexer/Dockerfile` & `images/indexer/extract.py`
- **Purpose**: Python-based text extraction and OCR container (`ark/indexer`).
- **How It Works**: Uses `pymupdf`, `ebooklib`, `libzim`, and Tesseract OCR to extract text from documents into JSONL streams.
- **Interactions**: Invoked by `internal/ai/indexer.go`.

### `images/kolibri/Dockerfile`
- **Purpose**: Offline educational platform container (`ark/kolibri`).
- **How It Works**: Configures Python 3.11 slim with Kolibri 0.16.1 and local data volumes.
- **Interactions**: Proxied by Ark under `/a/kolibri/`.

---

## 14. Application Catalog Manifests (`catalog/`)

- `catalog/*/app.yaml` (`convertx`, `cyberchef`, `druglabels`, `flatnotes`, `kiwix`, `kolibri`, `libretranslate`, `maps`, `ollama`):
  - **Purpose**: Declarative application manifests for built-in modules.
  - **How It Works**: Defines container images, port mappings, volume mounts, reverse proxy modes, and LAN permissions.
  - **Interactions**: Parsed by `internal/apps/manifest.go` and managed by `internal/docker/docker.go`.

---

## 15. Database Migrations (`migrations/`)

### `migrations/0001_init.sql`
- **Purpose**: Initial database schema for Ark.
- **How It Works**: Creates SQLite tables (`settings`, `jobs`, `app_states`, `catalog_items`, `usb_profiles`, `rag_documents`, `rag_chunks`, `chat_sessions`, `chat_messages`, `map_markers`) and indexes with WAL mode enabled.
- **Interactions**: Executed on startup by `initDatabase` in `cmd/ark/main.go`.

---

## 16. Installers (`install/`)

### `install/install.ps1`
- **Purpose**: Windows PowerShell installer and offline USB loader.
- **How It Works**: Checks Docker Desktop, installs `ark.exe`, imports offline Docker images, copies data directories, configures firewall rules, and creates scheduled tasks.
- **Interactions**: Sets up Ark on Windows hosts.

### `install/install.sh`
- **Purpose**: Linux/macOS shell installer and offline USB loader.
- **How It Works**: Verifies Docker, installs the `ark` binary, loads image tarballs, configures systemd service units, and initializes directories.
- **Interactions**: Sets up Ark on Linux/macOS hosts.

---

## 17. Root Configuration & Project Files

- `go.mod` & `go.sum`: Root Go module definition (`ark`, Go 1.27) and cryptographically locked dependency checksums.
- `README.md`: Project documentation, feature summary, and installation instructions.
- `AGENTS.md`: Repository development rules enforcing offline-first operation, no CDN links, and link verification.
- `.github/workflows/release.yml`: GitHub Actions workflow for cross-compiling static binaries and generating release bundles.
- `.dockerignore`: Excludes build artifacts, databases, and temporary files from Docker build contexts.
- `.gitignore`: Prevents compiled binaries, runtime databases, and local caches from being committed to Git.
