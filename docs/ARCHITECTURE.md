# Ark Architecture & Per-File Reference Guide

This document provides a comprehensive, human-readable architectural reference for the Ark codebase. For each file and package, this guide describes its **Purpose**, **How It Works**, and its **Interactions** across the system.

---

## Architecture Overview

Ark is a self-hosted, offline-first "internet-in-a-box" and local knowledge archive designed for resilience and off-grid scenarios. It runs natively as a single Go (`1.27`, `CGO_ENABLED=0`) Command Center binary with pure-Go SQLite persistence (`modernc.org/sqlite` in WAL mode), in-process ZIM reading, in-process PMTiles vector map serving, local SQLite FTS5 RAG AI, a Field Guide & Calculator/Celestial suite, LAN Drop & OPDS sharing, peer discovery, and browser-bridged LoRa Mesh (`/mesh`) — with **zero Docker required** for core operation. Optional Docker or rootless Podman containers extend Ark with auxiliary self-hosted apps.

```
+-----------------------------------------------------------------------------------+
|                          Browser / LAN / OPDS / Peers                             |
|       (HTTP or Self-Signed HTTPS :8080, LAN PIN Auth, UDP :41234 Discovery)       |
+-----------------------------------------+-----------------------------------------+
                                          |
                                          v
+-----------------------------------------------------------------------------------+
|                        Ark Command Center (`cmd/ark`)                            |
|                                                                                   |
|  [Web & Security Layer: internal/web]                                             |
|   - 14 HTML Templates (`layout.html` + 13 views), HTMX, Static CSS Themes         |
|     (`almanac`, `topo`, `nightwatch`), Vendored Marked / KaTeX / MapLibre         |
|   - Pure-Go In-Process ZIM Reader (`zim_reader.go`) with PDF/HTML Export          |
|   - Native PMTiles v3 Range & Unified MVT Tile Server (`pmtiles.go`)              |
|   - Home Global Search + Autocorrect, LAN Drop, OPDS 1.2, Mesh UI (`/mesh`)       |
|   - LAN Security Middleware (`security.go`: CSRF, PIN allowlist, rate limits)     |
|   - Peer Discovery & Allowlisted Reverse Proxy (`peers.go`)                       |
|                                                                                   |
|  [Core Services]                                                                  |
|   - SQLite FTS5 RAG & Local LLM Client: `internal/ai` (no external vector DB)     |
|   - Background Job Queue & Runners: `internal/jobs` (`aria2c`, Range, PMTiles)    |
|   - Kiwix OPDS Catalog, Presets & Maps: `internal/kiwix`                          |
|   - Storage Accounting, Space Checks & Cascade Delete: `internal/storage`         |
|   - Field Remedy & Creator-Pack Guides: `internal/fieldguide`                     |
|   - Host Metrics SSE, Downtime & Hardware Info: `internal/system`                 |
|   - Signed Offline/GitHub Self-Update & Rollback: `internal/update`               |
|   - Air-Gapped USB Export & Data Relocation: `internal/usb`                       |
|   - Optional App Manifests & Reverse Proxy (`/a/<id>/`): `internal/apps`          |
|   - Optional Docker / Rootless Podman Engine: `internal/docker`                   |
|   - Pure-Go SQLite Database (WAL mode): `ArkData/ark.db`                          |
+--------------------+---------------------------------------+----------------------+
                     |                                       |
                     v                                       v
+--------------------+-------------------+   +---------------+----------------------+
|   Optional Container Runtime (Apps)    |   |           Local Storage              |
|  - ConvertX (`:3000`, `/a/convertx/`)  |   |  - `data/zim/` (ZIM archives)        |
|  - CyberChef (`/a/cyberchef/`)         |   |  - `data/maps/` (`.pmtiles` maps)    |
|  - Drug Labels (`/a/druglabels/`)      |   |  - `data/media/` (Video/Music/Books) |
|  - FlatNotes (`/a/flatnotes/`)         |   |  - `data/notes/` (Markdown notes)    |
|  - Kolibri (`/a/kolibri/`)             |   |  - `data/druglabels/` (openFDA FTS5) |
|  - LibreTranslate (`/a/libretranslate/`)   |  - `data/landrop/` (LAN uploads)     |
|  - Go-PMTiles CLI/Optional (`/maps`)   |   |  - `tls/`, `backups/`, `custom_apps/`|
+----------------------------------------+   +--------------------------------------+
```

---

## 1. CLI Entrypoint (`cmd/ark/`)

### `cmd/ark/main.go`
- **Purpose**: Main CLI entrypoint, configuration bootstrapper, SQLite initializer, and subcommand dispatcher for Ark.
- **How It Works**: Applies Go runtime memory tuning (`tuneRuntimeMemory`: `SetGCPercent(50)`, `SetMemoryLimit(256 MiB)` unless overridden by `GOMEMLIMIT`) and dispatches subcommands (`run` [default], `setup-runtime`, `update`, `rollback`, `usb`, `move-data`, `uninstall`, `help`). `runServer` resolves the data directory (`./ArkData` or `./data` via `resolveDefaultDataDir`/`resolveDataDir`), attaches bundled rootless Podman environment paths if present, runs update crash-recovery (`update.MarkBootingOrRecover`), opens a single-connection WAL SQLite database (`initDatabase` executing `initialMigrationSQL`), constructs `web.Server`, probes the optional container runtime (`docker.NewClient` and `RestoreRunningApps`), initializes AI/chat schemas (`ai.EnsureSchema`, `ai.EnsureChatSchema`, `ai.PruneExpiredSessions`), records downtime (`system.RecordStartupAndComputeDowntime`), starts `jobs.QueueManager` with `ai.IndexJobRunner`, `ai.OllamaPullRunner`, and `jobs.NewUSBRunner`, mounts routes wrapped in `WrapWithSecurity`, optionally loads or generates a self-signed TLS cert (`ensureSelfSignedTLSCert`), starts LAN peer discovery (`StartPeerDiscovery`), opens the browser (unless `-no-browser`), and handles signals, Windows console events, or tray shutdown cleanly. Flag `-container` runs `runContainerizedHost` (host metrics sidecar + `EnsureCoreContainer`).
- **Interactions**: Imports and coordinates `internal/ai`, `internal/docker`, `internal/jobs`, `internal/system`, `internal/update`, `internal/usb`, and `internal/web`.

### `cmd/ark/platform_windows.go` & `cmd/ark/platform_other.go`
- **Purpose**: OS-specific console close handling, background detachment, health probing, and Windows system tray integration.
- **How It Works**: On Windows (`platform_windows.go`, build-tagged `windows`), `registerConsoleCloseHandler` registers a Win32 `SetConsoleCtrlHandler` callback for clean database/container shutdown on `CTRL_CLOSE_EVENT`, `CTRL_LOGOFF_EVENT`, and `CTRL_SHUTDOWN_EVENT`; `isServiceHealthy` probes `/healthz` over HTTP/HTTPS; `maybeDetachBackground` relaunches with `-background-child` (`CREATE_NO_WINDOW | DETACHED_PROCESS`); and `startTrayIcon` creates a native Win32 `Shell_NotifyIconW` tray icon with **Open Home**, **System**, and **Shut Down** menu items. On non-Windows platforms (`platform_other.go`, `!windows`), these provide no-op stubs.
- **Interactions**: Invoked during `runServer` startup and lifecycle management in `cmd/ark/main.go`.

### `cmd/ark/tls.go`
- **Purpose**: Automatic self-signed TLS certificate provisioning for optional LAN HTTPS.
- **How It Works**: `ensureSelfSignedTLSCert` generates (or reuses existing) 10-year ECDSA P-256 certificate and private key files at `<dataDir>/tls/ark.crt` and `<dataDir>/tls/ark.key`. Populates Subject Alternative Names (SANs) with `localhost`, ASCII hostnames (`isASCIIDNSName`), loopback IPs (`127.0.0.1`, `::1`), and all active non-loopback IPv4 interface addresses.
- **Interactions**: Called by `cmd/ark/main.go` when `https_enabled` is true in SQLite `settings`; public cert served for client trust at `/tls/ark.crt` by `internal/web/security.go`.

### `cmd/ark/ark.rc`, `cmd/ark/ark.ico`, `cmd/ark/ark_windows_amd64.syso`
- **Purpose**: Windows application icon resource definition and compiled COFF resource object.
- **How It Works**: Embeds `ark.ico` into Windows `amd64` binaries during `go build`.
- **Interactions**: Linked automatically by the Go toolchain when targeting `windows/amd64`.

### `cmd/ark/main_test.go` & `cmd/ark/tls_test.go`
- **Purpose**: Unit tests for CLI subcommands, data directory resolution, and TLS certificate generation.
- **How It Works**: Validates `handleUSB`, `handleMoveData`, `handleUninstall`, `resolveDefaultDataDir`, and `ensureSelfSignedTLSCert` SAN coverage and cert reuse.
- **Interactions**: Tests `cmd/ark/main.go` and `cmd/ark/tls.go`.

---

## 2. Optional Container Runtime (`internal/docker/`)

### `internal/docker/docker.go`
- **Purpose**: Wrapper around the Docker Engine SDK supporting both Docker and rootless Podman for optional app modules and utility job containers.
- **How It Works**: Connects via `client.FromEnv` (`NewClient`), identifies Docker vs. Podman (`RuntimeName`), checks daemon availability (`Ping`, `IsAvailable`, `EnsureDaemonRunning`), manages the `ark-net` bridge network (`EnsureNetwork`), builds custom `ark/*` images from embedded `images.FS` or pulls upstream images (`Pull`), enforces manifest `MemoryMB` caps (`EnsureRunning`), manages lifecycle (`Stop`, `StopAll`, `Restart`, `Remove`, `RestoreRunningApps`, `UninstallAll`), runs ephemeral job containers (`RunJob`), collects container CPU/memory telemetry (`Stats`), streams container logs (`Logs`), and saves/loads offline image tarballs (`ImageSave`, `ImageLoad`).
- **Interactions**: Used by `cmd/ark/main.go`, `internal/web` (app lifecycle, telemetry, logs), `internal/jobs/runners.go`, `internal/ai/indexer.go` (optional OCR container), and `internal/usb/usb.go`.

### `internal/docker/runtime_podman.go`
- **Purpose**: Self-contained rootless Podman runtime installer for `linux/amd64` hosts without Docker.
- **How It Works**: `CanInstallPodmanRuntime` checks OS/arch compatibility; `AttachBundledRuntimeEnv` wires `<dataDir>/runtime/podman` binaries and rootless sockets into `PATH`/`DOCKER_HOST`; `InstallPodmanRuntime` downloads `podman-static`, safely extracts binaries into `RuntimeRootDir`, and writes default `policy.json` and `registries.conf`.
- **Interactions**: Invoked by `ark setup-runtime` in `cmd/ark/main.go` and `/api/system/install-runtime` in `internal/web/admin.go`.

### `internal/docker/docker_test.go`
- **Purpose**: Unit tests for container stats math, port allocation, container state inspection, `StopAll`, and Podman archive extraction.
- **How It Works**: Tests CPU percentage calculations, `GetFreePort`, mock container stops, and safe tarball extraction.
- **Interactions**: Validates `internal/docker/docker.go` and `internal/docker/runtime_podman.go`.

---

## 3. Host System Telemetry & Downtime (`internal/system/`)

### `internal/system/system.go`
- **Purpose**: Host hardware metrics collection, live SSE telemetry feed, and Command Center downtime tracking.
- **How It Works**: `CollectMetrics` queries CPU utilization, RAM total/used/free, disk usage, hostname, platform, and uptime via `gopsutil/v4` (or delegates to `ARK_HOST_METRICS_URL` when running in `-container` mode). `SSEFeed` (`NewSSEFeed`, `ServeHTTP`) streams JSON metric frames over HTTP SSE every 2 seconds until client disconnect. `RecordShutdownTime`, `RecordStartupAndComputeDowntime`, and `GetDowntimeInfo` persist UTC timestamps in SQLite `settings` to measure offline duration across restarts.
- **Interactions**: Startup/shutdown hooks called in `cmd/ark/main.go`; SSE endpoint mounted at `/api/system/sse` and downtime surfaced in `/system` via `internal/web/admin.go`.

### `internal/system/system_test.go`
- **Purpose**: Unit tests for host telemetry, SSE frame streaming, and downtime calculation/clock-rollback clamping.
- **How It Works**: Uses `httptest` recorders and temporary SQLite databases to verify metrics, SSE encoding, and timestamp arithmetic.
- **Interactions**: Validates `internal/system/system.go`.

---

## 4. Updates & Rollback (`internal/update/`)

### `internal/update/update.go`
- **Purpose**: Signed offline/USB binary updates, GitHub Releases updates, SQLite database snapshots, and automatic crash-recovery rollback.
- **How It Works**: Supports offline update bundles (`LoadUpdateFromDir`, `VerifyManifestAndBinary`, `VerifyFileSHA256`) verified with Ed25519 signatures (`EmbeddedPublicKeyHex`, `DefaultPublicKey`, `SigningPayload`) and SHA-256 digests, as well as online GitHub Releases checks and downloads (`CheckGitHubRelease`, `DownloadAndApplyGitHubRelease` targeting `soap353/ARK` and `ArkVersion`). `ApplyUpdate` creates a `VACUUM INTO` database backup (`SnapshotDB`), swaps binaries (`ark` -> `ark.old`), and writes a `PendingMarker`. On next boot, `MarkBootingOrRecover` / `CheckAndRecoverOnStartup` automatically rolls back (`Rollback`) if the new binary fails before `ConfirmHealthy`.
- **Interactions**: Invoked by `cmd/ark/main.go` (`update`/`rollback` subcommands and startup watchdog) and `internal/web/admin.go` (`/settings/update`, `/settings/update-github`, `/settings/rollback`, `/api/update/check`).

### `internal/update/update_test.go`
- **Purpose**: Unit tests for Ed25519 + SHA-256 verification, atomic binary/DB rollback, crash recovery, and GitHub Release metadata parsing.
- **How It Works**: Generates ephemeral Ed25519 keypairs, tests signature rejection on tampered artifacts, verifies rollback recovery, and mocks GitHub Release API payloads.
- **Interactions**: Validates `internal/update/update.go`.

---

## 5. USB Provisioning & Data Relocation (`internal/usb/`)

### `internal/usb/usb.go`
- **Purpose**: Removable drive discovery, incremental offline USB bundle export, export profile persistence, and `ArkData` directory relocation.
- **How It Works**: `ListDrives` enumerates removable drives and flags FAT32 volumes (4 GiB file-size limit). `ScanCategories` reports exportable category sizes. `SaveProfile`, `ListProfiles`, and `GetProfile` store export selections in SQLite `usb_profiles`. `ExportToUSB` performs incremental SHA-256-verified copies of selected data categories, writes a sanitized copy of `ark.db` (stripping password hashes, API keys, and LAN passcodes), optionally bundles the Ark executable and Docker `ark-images.tar` via `ImageSaver`, and writes `manifest.json` (`VerifyManifest`). `MoveDataDir` copies and SHA-256 verifies all files to a new `ArkData` path.
- **Interactions**: Used by `cmd/ark/main.go` (`usb`, `move-data`), `internal/jobs/runners.go` (`USBRunner`), and `internal/web/admin.go` (`/settings/usb-export`, `/settings/usb-profile`).

### `internal/usb/usb_test.go`
- **Purpose**: Unit tests for USB profile CRUD, incremental export, DB credential sanitization, manifest verification, and `MoveDataDir`.
- **How It Works**: Runs exports and relocations against temporary directories and mock `ImageSaver` implementations.
- **Interactions**: Validates `internal/usb/usb.go`.

---

## 6. Offline AI & SQLite RAG Engine (`internal/ai/`)

### `internal/ai/client.go`
- **Purpose**: Unified client for OpenAI-compatible inference servers (LM Studio, llama.cpp, vLLM, LocalAI) and Ollama model management.
- **How It Works**: `NormalizeBaseURL` adapts `localhost` URLs to `host.docker.internal` when running containerized. `Conn` provides `Models`, `ContextWindow`, `Status` (probing LM Studio `/api/v0/models`, Ollama `/api/ps` + `/api/show`, and `/v1/models`), `Embed`, and `ChatStream` (SSE token streaming and buffered JSON fallback, including multimodal `ContentPart` / `ImageURL` payloads). `DetectServers` scans common local AI endpoints. `ListOllamaModels`, `PullOllamaModel`, `DeleteOllamaModel`, and `OllamaPullRunner` (`jobs.Runner`) manage local Ollama models.
- **Interactions**: Consumed by `internal/ai/indexer.go`, `internal/web/ai.go`, and `internal/jobs/queue.go`.

### `internal/ai/extract_native.go`
- **Purpose**: Pure-Go in-process text extraction from ZIM archives, EPUBs, PDFs, HTML, and plain text.
- **How It Works**: Parses ZIM v5/v6 headers (`ReadZIMHeader`) and uncompressed/XZ/Zstd clusters (`ParseZIMCluster`) in `extractZIM` (sampling up to `maxZimArticles` = 120,000), parses EPUB ZIP XHTML chapters (`extractEPUB`), extracts PDF text streams via `github.com/ledongthuc/pdf` (`extractPDF`), and strips markup via `htmlToPlainText`.
- **Interactions**: Invoked by `internal/ai/indexer.go` during RAG indexing jobs without requiring external containers.

### `internal/ai/indexer.go`
- **Purpose**: SQLite FTS5 knowledge indexer and hybrid keyword/BM25 retriever (all RAG metadata and chunks stored natively in `ark.db`).
- **How It Works**: `EnsureSchema` initializes `rag_documents`, `rag_chunks`, and `rag_fts` (FTS5 virtual table) in SQLite. `ExtractFile` runs native extraction from `extract_native.go` and optionally falls back to the `ark/indexer` container (`extractViaDockerIndexer`) for OCR on image files (`.png`, `.jpg`, `.jpeg`, `.webp`, `.tiff`). `ChunkText` splits documents into 3,200-character overlapping chunks (400-character overlap) and `IndexSource` / `indexArchiveDocs` persist deduplicated chunks. `SearchKnowledge` executes FTS5 BM25 AND/OR queries (`SignificantTerms`, `BuildFTSQueries`) with a keyword scan fallback. Also provides `ListDocuments`, `RemoveDocument`, `LoadConnFromDB`, and `IndexJobRunner` (`jobs.Runner`).
- **Interactions**: Coordinates `internal/ai/extract_native.go`, `internal/docker`, `internal/jobs/queue.go`, `internal/storage/storage.go` (cleanup on delete), and `internal/web/ai.go`.

### `internal/ai/sessions.go`
- **Purpose**: Persistent Ask AI chat session and message history manager in SQLite.
- **How It Works**: `EnsureChatSchema` creates `ai_chat_sessions` and `ai_chat_messages`. `PruneExpiredSessions` automatically expires unpinned sessions after 48 hours. Provides `CreateChatSession`, `AppendChatMessage`, `UpdateSessionTitle`, `SetSessionPinned`, `DeleteChatSession`, `ListChatSessions`, `GetChatSession`, and automatic title generation (`GenerateSessionTitle`, `TitleFromQuery`, `ClampTitleWords` up to 4 words).
- **Interactions**: Consumed by `cmd/ark/main.go` on startup and `internal/web/ai.go` for Ask AI conversation history.

### `internal/ai/ai_test.go`
- **Purpose**: Unit tests for URL normalization, mock LLM/Ollama HTTP APIs, SQLite FTS5 indexing/deduplication/search/removal, Zstd ZIM & EPUB extraction, and 48-hour session pruning/pinning.
- **How It Works**: Uses in-memory/temp SQLite databases, mock `httptest` servers, and synthetic ZIM/EPUB archives.
- **Interactions**: Validates `internal/ai`.

---

## 7. Optional App Manifests & Reverse Proxy (`internal/apps/`)

### `internal/apps/manifest.go`
- **Purpose**: Declarative `app.yaml` parser, validator, volume bind resolver, and custom user app store.
- **How It Works**: Loads embedded built-in manifests (`LoadAllManifestsFS` from `catalog/embed.go`) and filesystem manifests (`LoadManifest`, `LoadAllManifests`), validates fields (`Validate`, `ValidCategories`), resolves relative bind mounts against `dataDir` / `ARK_HOST_DATA_DIR` while preserving `ark-*` named volumes (`ResolveBinds`), and manages user-defined manifests in `<dataDir>/custom_apps/` (`SanitizeCustomAppID`, `SaveCustomManifest`, `LoadCustomManifests`, `LoadCustomManifest`, `DeleteCustomManifest`).
- **Interactions**: Consumed by `internal/apps/proxy.go`, `internal/web/web.go`, and `internal/web/admin.go`.

### `internal/apps/proxy.go`
- **Purpose**: Reverse proxy handler serving optional containerized apps under `/a/<id>/`.
- **How It Works**: `ProxyManager` (`Register`, `Unregister`, `GetRoute`, `ServeHTTP`) wraps `net/http/httputil.ReverseProxy`, rewrites paths when `stripPrefix` is enabled, sets `X-Forwarded-*` headers, strips `Accept-Encoding` to inject the `#ark-proxy-nav` topbar into HTML responses, and renders friendly status pages when a container is stopped or unavailable.
- **Interactions**: Mounted at `/a/` in `internal/web/web.go` (with `/a/maps`, `/a/mesh`, `/a/meshtastic`, and `/a/kiwix*` redirecting straight to native routes `/maps`, `/mesh`, and `/library`).

### `internal/apps/manifest_test.go` & `internal/apps/proxy_test.go`
- **Purpose**: Unit tests for embedded `catalog/*/app.yaml` validation, volume bind resolution, path prefix stripping, redirects, and navbar injection.
- **How It Works**: Tests valid/invalid YAML specs and exercises `ProxyManager` against mock HTTP backends.
- **Interactions**: Validates `internal/apps`.

---

## 8. Background Jobs & Download Runners (`internal/jobs/`)

### `internal/jobs/queue.go`
- **Purpose**: Job types, statuses, runner registry, and concurrent semaphore-bounded worker pool.
- **How It Works**: Defines job types (`TypeDownload`, `TypeEncode`, `TypeIndex`, `TypeZim`, `TypePreset`, `TypeDrugLabels`, `TypeUSB`, `TypeMapExtract`, `TypeOllamaPull`) and statuses (`StatusPending`, `StatusRunning`, `StatusDone`, `StatusFailed`, `StatusPaused`, `StatusCancelled`). `QueueManager` runs worker goroutines constrained by three semaphores (`downloadSem` = 3, `encodeSem` = 1, `indexSem` = 1), supporting `Submit`, `Notify`, `Cancel`, `Retry`, and `Remove`.
- **Interactions**: Coordinates `internal/jobs/store.go` and runners across `internal/jobs/runners.go`, `internal/jobs/druglabels.go`, `internal/ai/indexer.go`, and `internal/ai/client.go`.

### `internal/jobs/store.go`
- **Purpose**: SQLite-backed persistent job repository.
- **How It Works**: `JobStore` provides `EnsureTable`, `Create`, `Get`, `UpdateProgress`, `SetStatus`, `List`, `Delete`, `ResetStaleRunning`, `ClearCompleted`, `CountByStatus`, and atomic dequeuing via `AcquireNextPending` (`UPDATE ... RETURNING` with `SQLITE_BUSY` retry).
- **Interactions**: Consumed by `internal/jobs/queue.go` and `internal/web`.

### `internal/jobs/runners.go`
- **Purpose**: Execution runners for file/ZIM/preset downloads, web archiving, media capture, USB export, and map extraction.
- **How It Works**: `Aria2cRunner` resolves preset/pack targets and Kiwix mirror date rotations, executes `aria2c` (or falls back to a native 12-worker segmented HTTP Range & `.meta4` multi-mirror downloader), and registers completed `.zim` files in `kiwix.LibraryManager`. `ZimitRunner` and `YtDlpRunner` run containerized website/media archiving. `USBRunner` delegates to `internal/usb`. `MapExtractRunner` resolves Protomaps `builds.json` and extracts regions using a native `go-pmtiles` binary (auto-downloaded if missing) or optional `ghcr.io/protomaps/go-pmtiles:latest` container.
- **Interactions**: Interacts with `internal/docker`, `internal/kiwix`, `internal/usb`, and `internal/jobs/store.go`.

### `internal/jobs/druglabels.go`
- **Purpose**: Native Go runner (`DrugLabelsRunner`) for downloading and indexing openFDA drug label datasets into local SQLite FTS5.
- **How It Works**: Queries `api.fda.gov/download.json`, downloads partitioned ZIP archives to disk, streams JSON label records into an atomic `data/druglabels/druglabels.db` with an FTS5 index, and cleans up staging files.
- **Interactions**: Registered with `jobs.QueueManager`; queried directly by Ask AI federated search (`internal/web/ai.go`), Global Home Search (`internal/web/content.go`), and the optional `druglabels` container app.

### `internal/jobs/jobs_test.go`
- **Purpose**: Unit tests for `JobStore`, atomic dequeuing, semaphore concurrency, native HTTP fallback, parallel Range downloads, and `.meta4` mirror handling.
- **How It Works**: Uses temporary SQLite databases and mock HTTP Range/mirror servers.
- **Interactions**: Validates `internal/jobs`.

---

## 9. Kiwix OPDS, Presets & Map Catalogs (`internal/kiwix/`)

### `internal/kiwix/catalog.go`
- **Purpose**: OPDS 1.2 Atom XML feed parser and SQLite catalog cache for Kiwix archives.
- **How It Works**: `ParseOPDSFeed` and `FetchOPDSFeed` parse `library.kiwix.org` OPDS feeds into `OPDSEntry` records (URLs, sizes, dates, flavours, thumbnails). `CatalogCache` stores and queries `catalog_items` in SQLite (`UpsertEntries`, `Search`, `GetByID`, `MarkDownloaded`, `MarkDeleted`, `Delete`, `Count`).
- **Interactions**: Used by `internal/web/content.go` (`/kiwix`, `/api/kiwix/catalog`) and `internal/storage/storage.go`.

### `internal/kiwix/library.go`
- **Purpose**: Kiwix `library.xml` manifest manager for optional `kiwix-serve` compatibility and library metadata.
- **How It Works**: `LibraryManager` (`EnsureLibraryFile`, `AddZIM`, `RemoveZIM`, `ListBooks`) updates `data/zim/library.xml` via `kiwix-manage` if installed, falling back to an in-process pure-Go XML serializer.
- **Interactions**: Called by `internal/jobs/runners.go` on ZIM download completion and `internal/storage/storage.go` during cascade deletion.

### `internal/kiwix/presets.go`
- **Purpose**: Curated offline knowledge bundles, Wikipedia tiers, Ark collections, US state/regional PMTiles map catalogs, country map extracts, and creator video packs.
- **How It Works**: Defines `GetAllPresetTiers`, `GetStarterPack`, `FindPresetByID`, `GetWikipediaOptions` (5 tiers), `GetArkCollections` (6 collections across 3 cumulative tiers), `GetArkMapRegions` (9 US regions covering 50 US states at `z0-z15` + `GetAllUSStateMaps`), `GetVideoCreatorPacks` (4 packs), `GetCountryMapOptions` (18 global/country extracts + `FindCountryMapBySlug`), and `ResolvePresetResources`.
- **Interactions**: Powers `/get-content` (`internal/web/web.go`), `/maps` (`internal/web/content.go`), and preset resolution in `internal/jobs/runners.go`.

### `internal/kiwix/kiwix_test.go`
- **Purpose**: Unit tests for OPDS feed parsing, SQLite catalog caching, preset/region/tier definitions, and `LibraryManager` XML round-trips.
- **How It Works**: Uses sample OPDS fixtures, temp SQLite DBs, and temp XML files.
- **Interactions**: Validates `internal/kiwix`.

---

## 10. Storage Accounting, Safety & Cascade Deletion (`internal/storage/`)

### `internal/storage/storage.go`
- **Purpose**: Disk usage accounting, pre-download free-space safety checks, disk-vs-database integrity verification, and multi-layer cascade deletion.
- **How It Works**: Maps 11 storage categories (`CategoryFolderMap`: `zim`, `maps`, `video`, `music`, `books`, `docs`, `druglabels`, `notes`, `kolibri`, `ai_models`, `other`). `GetDiskUsage` and `ListCategoryFiles` scan `dataDir` and report filesystem capacity. `EvaluateSpace`, `CheckDiskSpaceForPath`, `ParseSizeText`, and `ProbeRemoteSize` enforce free-space buffers (`SpaceSafetyBufferBytes` = 1 GiB, `SpacePortionThreshold` = 25%, `LowDiskUnknownSizeBytes` = 2 GiB). `CheckIntegrity` and `PruneOrphans` reconcile physical files with SQLite records. `CascadeDelete` removes the file (and `.part`/`.aria2` artifacts), deregisters from `kiwix.LibraryManager`, removes `usb_profiles` and `catalog_items` references, and deletes `rag_chunks` / `rag_documents` entries in SQLite.
- **Interactions**: Consumed by `internal/web/web.go`, `internal/web/admin.go` (`/api/storage/check-space`), and `internal/web/content.go` (`/api/library/delete`).

### `internal/storage/storage_test.go`
- **Purpose**: Unit tests for disk usage reporting, free-space threshold checks, orphan pruning, and zero-orphan cascade deletion.
- **How It Works**: Verifies cleanup across temporary file trees, `library.xml`, and SQLite tables.
- **Interactions**: Validates `internal/storage/storage.go`.

---

## 11. Field Reference Guide (`internal/fieldguide/`)

### `internal/fieldguide/fieldguide.go` & `internal/fieldguide/data/*.json`
- **Purpose**: Embedded offline medical/remedy reference and curated creator-pack catalog.
- **How It Works**: Embeds `data/conditions.json`, `data/creator-packs.json`, `data/home_remedies.json`, and `data/natural_remedies.json` via `go:embed data/*.json`. `LoadGuide` parses conditions, home remedies, natural remedies, and creator packs; `Guide.Search` filters by query, remedy type, and condition; `GetCreatorPacks` returns curated media packs.
- **Interactions**: Mounted at `/field-guide` (`internal/web/content.go`), queried by Ask AI federated search (`internal/web/ai.go`), and included in Global Home Search (`internal/web/content.go`).

### `internal/fieldguide/fieldguide_test.go`
- **Purpose**: Unit tests for embedded JSON loading, schema integrity, and remedy search filtering.
- **How It Works**: Exercises `LoadGuide`, `Guide.Search`, and `GetCreatorPacks`.
- **Interactions**: Validates `internal/fieldguide`.

---

## 12. Web UI, Handlers, Templates & Static Assets (`internal/web/`)

### `internal/web/web.go`
- **Purpose**: Core HTTP server struct, view models, template embedding, route registration (`RegisterRoutes`), primary page handlers, and shared handler utilities.
- **How It Works**: Embeds `templates/*` and `static/*`, parses 13 page templates alongside `templates/layout.html`, cleans legacy unused rows from `app_states`, seeds `catalog/` manifests, initializes LAN Drop/Mesh/Security tables and rate limiters, and registers all HTTP routes (see Route Map below). Implements `handleHealthz`, `handleHome`, `handleLibrary`, `handleGetContent`, `handleApps`, `handleAsk`, `handleJobsRedirect` (`/jobs` -> 303 `/system`), `handleSettings`, `handleAPIJobs`, `handleMediaFiles` (with `Content-Security-Policy: sandbox` on HTML/SVG/XML), `handleAPIJob{Cancel,Retry,Delete,Status}`, `handleAPIKiwixCatalog`, `handleAPIApp{Install,Start,Stop,Uninstall}`, `RestoreRunningApps`, `fetchApps` (exposing native `/mesh`), `getStorageBreakdown`, `renderAppIconSVG`, `localOnly`, `validUITheme` (`almanac`, `topo`, `nightwatch`), and shared helpers (`requireMethod`, `writeJSON`, `writeJSONError`, `redirectWithQuery`, `sanitizeLocalRedirect`, `isLocalRequest`).
- **Interactions**: Coordinates all core `internal/*` packages and serves as the base `Server` struct for all `internal/web/*.go` handler files.

### `internal/web/admin.go`, `restart_windows.go` & `restart_other.go`
- **Purpose**: Local-only settings persistence, system telemetry/container views, runtime setup, USB export triggers, signed/GitHub updates, custom app registration, and process lifecycle.
- **How It Works**: Implements `fetchSettings`/`saveSettings`, `handleSettingsGeneral` (device name, port, `ui_theme`, `lan_enabled`, `lan_passcode_enabled` + PIN, `https_enabled` + `restart_required=1`, `lan_drop_enabled`, `opds_enabled`, peer discovery toggles), `handleSettings{AI,Downloads,USBProfile,USBExport,Update,UpdateGitHub,Rollback,Restart,Shutdown}`, `handleAPIUpdateCheck`, `handleSystem`, `handleAPIContainer{Restart,Logs}`, `handleAPISystem{Time,InstallRuntime,Runtime}`, `handleAPIStorageCheckSpace`, `handleAPICustomAppSave`, and `handleAPIAppUpdateImage`. Platform-specific `prepareRestartCmd` in `restart_windows.go` / `restart_other.go` detaches restarted child processes (`ARK_RESTART=1`, `ARK_NO_BROWSER=1`).
- **Interactions**: Extends `Server` in `web.go`; coordinates `internal/system`, `internal/update`, `internal/usb`, `internal/storage`, `internal/apps`, and `internal/docker`.

### `internal/web/ai.go`
- **Purpose**: Ask AI backend, Ollama model management, federated offline RAG retrieval, structured citation guardrails, and deterministic offline math/dosage/RF tools.
- **How It Works**: Implements `handleAPIAI{Status,Detect,Test}`, Ollama handlers (`handleAPIAI{Models,ModelPull,ModelDelete,ModelSelect}`), session CRUD (`handleAPIAI{Sessions,SessionGet,SessionPin,SessionDelete}`), and knowledge indexing (`handleAPIAI{Index,IndexArticle,KnowledgeRemove}`). `handleAPIAIChat` accepts JSON or `multipart/form-data` (extracting PDF/EPUB/text attachments and base64 images for vision models), bypasses unnecessary RAG on casual/math queries (`queryNeedsLocalKnowledge`), runs federated retrieval across SQLite `rag_fts` (`ai.SearchKnowledge`), live `.zim` archives (`searchLiveZIMs`), `internal/fieldguide` (`searchRemedyGuide`), `data/druglabels/druglabels.db` FTS5 (`searchDrugLabels`), auto-synced `data/notes/` and `data/media/docs/` (`syncAutoIndexedDirs`), nearby `map_markers` via Haversine (`queryMapWaypointsContext`), and deterministic AST/calculator checks (`evalAskMathTool`), then streams SSE `token`/`done`/`error` events (`jsonAnswerStreamer`), repairs JSON/LaTeX escapes (`repairJSONLaTeXEscapes`), enforces `SourcesOnly` / `ProtocolMode` / `TriageType`, and validates or generates local `?ark_hl=` citation links (`buildCitationLinkWithSnippet`, `stripInlineLinksAndCiteBlocks`).
- **Interactions**: Extends `Server`; coordinates `internal/ai`, `internal/fieldguide`, `internal/web/zim_reader.go`, and `templates/ask.html`.

### `internal/web/content.go`
- **Purpose**: Maps viewer & pins, Library icons/updates/deletion, Field Guide, Tools page, LAN Drop bulletin board, OPDS 1.2 Atom server, and Home global search + autocorrect.
- **How It Works**: Implements `handleMapsViewer`, `handleAPIMapsExtract`, `handleAPIMapPins` (with coordinate bounds validation), `handleKiwixCatalogPage`, `handleAPILibrary{Delete,Updates,Icon}` (embedded ZIM cover + OPDS thumbnail cache + offline origin SVG fallbacks), `handleFieldGuidePage`, `handleAPIZIM{Suggest,Pages}`, and `handleToolsPage`. Implements LAN Drop (`handleLANDropPage`, `handleAPILANDrop{Posts,Post,Delete,File}` storing posts in SQLite `lan_posts` and files up to 256 MB in `data/landrop/`). Implements OPDS 1.2 Atom XML (`handleOPDSRouter`: `/opds`, `opensearch.xml`, `all`, `media`, `zim`, `zim/<stem>`, `search`, `files/{media,docs}/...`). Implements `handleAPISearch` (`/api/search`): searches titles-only local Library items, Calculators, Maps/Pins/Downloads, Remedies, Mesh Nodes, Bulletin posts, AI Chat sessions, and Drug Labels (`collectGlobalSearchCandidates`), applying Levenshtein edit-distance 1–2 autocorrect (`autocorrectSearchQuery`) with `&exact=1` override.
- **Interactions**: Extends `Server`; coordinates `internal/kiwix`, `internal/storage`, `internal/fieldguide`, `internal/jobs`, `internal/web/zim_reader.go`, and `internal/web/pmtiles.go`.

### `internal/web/zim_reader.go`
- **Purpose**: Pure-Go, in-process ZIM archive reader, search engine, and PDF/HTML article exporter (`/read/zim/...`).
- **How It Works**: Reads ZIM v5/v6 directory entries and zstd/XZ/uncompressed clusters directly from `data/zim/*.zim` with cluster caching (requiring no external Kiwix container). Supports binary-search and streaming title suggestions (`searchTitles` -> `/api/zim/suggest`), paginated TOC listing (`listPages` -> `/api/zim/pages`), start pages (`renderZIMStartPage`), URL rewriting, `?ark_hl=` quote highlighting, and `injectZIMReaderNav` injecting `#ark-proxy-nav` (collapsible `#ark-toc-panel`, live `<datalist>` search, 5 reader themes `forest`/`light`/`sepia`/`dark`/`default` persisted via `/api/reader/theme`, text-selection Explain/Summarize/Translate popover, `#ark-page-pager` prev/next, and `#ark-download-btn` dropdown offering **PDF** print export via `window.print()` + `@media print` and standalone **HTML** export via `?download=1` + `inlineZIMPageAssets` embedding CSS/JS/images inline as base64 data URIs).
- **Interactions**: Mounted at `/read/zim` and `/read/zim/`; called by `internal/web/content.go` (`/api/zim/*`), `internal/web/ai.go` (live ZIM RAG + citation links), and `internal/web/peers.go` (peer ZIM proxying).

### `internal/web/pmtiles.go`
- **Purpose**: Pure-Go PMTiles v3 metadata parser, Range file server, TileJSON generator, and unified multi-archive MVT tile server.
- **How It Works**: `ParsePMTilesMeta` reads 127-byte PMTiles v3 headers and gzipped JSON metadata. `handleMapsFiles` serves `.pmtiles` archives with HTTP Range (`http.ServeContent`). `handleAPIMapsTileJSON` emits combined bounds/zoom/vector-layer metadata. `handleUnifiedMapTiles` (`/maps/tiles/{z}/{x}/{y}.mvt`) caches bounded root headers, filters archives by WGS84 tile bounding-box intersection, performs Hilbert `tileID` lookups via `os.File.ReadAt`, streams single-archive tiles verbatim (`Content-Encoding: gzip`), and merges overlapping MVT protobuf payloads across border/overview archives.
- **Interactions**: Registered in `web.go` and consumed by MapLibre GL + Protomaps `basemaps.js` in `templates/maps.html`.

### `internal/web/mesh.go`
- **Purpose**: Off-grid LoRa Mesh bridge UI and persistent SQLite node/message log (`/mesh`, `/api/mesh/*`).
- **How It Works**: Serves `templates/mesh.html` at `/mesh` and persists messages (`mesh_messages`) and discovered nodes (`mesh_nodes` with lat/lng/SNR/RSSI) via `handleAPIMeshState` (GET), `handleAPIMeshMessage`, `handleAPIMeshNode`, and `handleAPIMeshClear` (local-only POSTs). The browser frontend bridges directly to connected hardware over Web Serial (`navigator.serial`) and Web Bluetooth (`navigator.bluetooth`), supporting Meshtastic (Protobuf), MeshCore (Companion `0x3C`/`0x3E` & BLE NUS), and Generic LoRa/AT radios.
- **Interactions**: Mounted in `web.go`; node coordinates link into `/maps?lat=...` and appear in Global Home Search (`/api/search`).

### `internal/web/peers.go`
- **Purpose**: Automatic LAN Ark peer discovery and allowlisted peer library browsing/proxying/pulling.
- **How It Works**: `StartPeerDiscovery` broadcasts and listens for `ark-peer-v1` UDP beacons on port `:41234` every 5 seconds (20s TTL) and polls configured `peer_manual_hosts`. `handleAPIPeers{Catalog,Download,List,Pull}` expose local shareable libraries and pull peer items locally with SSRF guards against registered peer base URLs and relative path prefixes. `handlePeerProxy` (`/peer/<id>/...`) reverse-proxies only allowlisted read paths (`/read/zim/`, `/media/`, `/api/library/icon`, `/api/zim/`, `/api/reader/`, `/static/`) and rewrites HTML links for seamless peer browsing.
- **Interactions**: Started by `cmd/ark/main.go`; surfaced in `templates/library.html` and configured in `templates/settings.html`.

### `internal/web/security.go`
- **Purpose**: HTTP security headers, CSRF origin validation, LAN access control, salted PIN authentication, IP allowlisting, and rate limiting.
- **How It Works**: `WrapWithSecurity` applies `X-Content-Type-Options: nosniff`, `X-Frame-Options: SAMEORIGIN`, and `Referrer-Policy: same-origin`; blocks cross-site/untrusted state-changing requests (`Sec-Fetch-Site: cross-site`, opaque `Origin: null`, non-loopback origins on local actions); enforces `lan_enabled`; gates remote LAN access behind `/lan-auth` when `lan_passcode_enabled` is enabled (salted SHA-256 PIN verification, 5-failure / 5-minute `429` lockout, persisting trusted IPs in SQLite `lan_allowlist`, exempting `/static/`, `/healthz`, `/lan-auth`, `/tls/ark.crt`, `/api/peers/catalog`, `/api/peers/download`); rate-limits remote POSTs (20/min/IP) on `/api/lan-drop/post`, `/api/ai/chat`, and `/api/maps/pins`; and serves `handleSettingsAllowlist{Add,Remove,Clear}` and `handleTLSCertDownload` (`/tls/ark.crt`).
- **Interactions**: Wraps the root `http.ServeMux` in `cmd/ark/main.go`; configured in `internal/web/admin.go`.

### `internal/web/web_test.go`
- **Purpose**: Comprehensive HTTP integration and unit test suite for the web layer.
- **How It Works**: Contains 25 `Test*` suites covering route registration, app lifecycle, CSS theme palettes, job queue & space pre-checks, settings/AI/model status/streaming, USB/updates, ZIM reader/search/TOC/HTML download, maps/PMTiles, Field Guide, LAN Drop/OPDS/calculators, Mesh & Celestial Tracker, remote access/CSRF/PIN allowlist/peer hardening/UI themes, and Home global search + autocorrect.
- **Interactions**: Validates all files in `internal/web`.

### `internal/web/static/`
- **`ark.css`**: Primary design system implementing three selectable UI palettes (`almanac` Warm Editorial Almanac default `#F5F2EB` / `#1B2E26` / `#C25E28`, `topo` Survey Topo `#EEF2F1` / `#E04F25`, and `nightwatch` Night Watch `#0B100F` / `#F59E0B`) controlled via `<html data-theme="...">`.
- **`ark-logo.png`** & **`htmx.min.js`**: Brand asset and vendored HTMX runtime.
- **`vendor/`**: Zero-CDN vendored frontend dependencies: `marked.min.js`, `katex.min.css`, `katex.min.js`, `auto-render.min.js`, 20 `fonts/KaTeX_*.woff2` files, and `vendor/maps/` (`maplibre-gl.js`, `maplibre-gl.css`, `basemaps.js`, `pmtiles.js`, and `fonts/Noto Sans {Regular,Medium,Italic}/*.pbf` with empty-protobuf fallback in `web.go` for unbundled glyph ranges).

### `internal/web/templates/*.html`
- **`layout.html`**: Shared sidebar navigation shell (`<html data-theme="{{ .UITheme }}">`), hiding Command (`System`/`Settings`) on remote `!IsLocal` clients, global pre-download space checker (`/api/storage/check-space`), live job button progress sync (`/api/jobs`), and toast notifications.
- **`home.html`**: Live debounced Global Search (`/api/search` with autocorrect banner + `&exact=1` override across titles-only Library items, Calculators, Maps/Pins/Downloads, Remedies, Mesh Nodes, Bulletin posts, AI Chat sessions, and Drug Labels; up to 4 per category, 25 total) + 3-column Installed Applications grid with right-click Start/Stop/Open context menu.
- **`library.html`**: Local content filter, ZIM update checker (`/api/library/updates`), per-ZIM inline search, right-click context menu (`Open`, `Add to AI`, `Delete`), and LAN peer library sections (`/api/peers/list`, `/peer/<id>/...`, `/api/peers/pull`).
- **`kiwix.html`** & **`get_content.html`**: Live OPDS browser (`/api/kiwix/catalog`) with custom mirror override; curated Wikipedia tiers, Prepper Starter Pack, 6 reactive 3-tier Ark collections, 9 US Regional Maps, Video/Creator packs, and Download Any URL form.
- **`field_guide.html`**: Offline Remedy reference (`/field-guide`) with symptom/ingredient search, Remedy Type (`Home Remedy` / `Natural Remedy`) & Condition filters, and collapsible instruction cards.
- **`apps.html`**: Native Mode status banner, optional rootless Podman installer (`/api/system/install-runtime`), module table + `<dialog id="app-modal">` (Install/Start/Stop/Restart/Pull Update/Logs/Uninstall), and Add Custom Docker App form.
- **`ask.html`**: Offline Marked + KaTeX AI chat UI, circular context-window SVG gauge, live model/vision polling, session selector/pin/delete, scope pills (`All`, `ZIMs`, `Remedy`, `Drug Labels`, `My Notes`, `Attachment Only`), `Sources Only` & `Protocol` toggles, collapsible `Triage` bar, multimodal file/image attachments, SSE streaming, citation badges + map fly-to/save-pin actions, and Knowledge Index table.
- **`maps.html`**: MapLibre GL + Protomaps `basemaps.js` unified vector map (`/maps/tiles/{z}/{x}/{y}.mvt`), place switcher (`?place=`), 4 map styles (`contrast`, `light`, `dark`, `grayscale`), `+ Drop Pin` & Shift+click coordinate capture, Saved Pins sidebar (`/api/maps/pins`), and collapsed-by-default `<details id="extract-maps-details">` ("Download Maps") with region tabs (`All`, `US States (z15)`, `World`, `North America`, `Europe`, `Asia & Pacific`, `Other Regions`) serving country extracts (`/api/maps/extract`) and US States `z0-z15` (`/api/jobs` `map_pmtiles`).
- **`mesh.html`**: Web Serial / Web Bluetooth bridge for Meshtastic, MeshCore, and Generic LoRa/AT radios, live message feed, discovered nodes table with `/maps?lat=...` links, and raw packet console backed by `/api/mesh/*`.
- **`lan_drop.html`**: Local bulletin board and drag-and-drop file sharing (256 MB max upload) with LAN Share URL copy controls, Connected Devices filter bar, and 8-second auto-refresh.
- **`tools.html`**: Single `<select id="calc-select">` dropdown switching 7 offline panels (`?calc=`): Graphing Calculator (Canvas + RPN parser + calculus/table/keypad), Dosage, Solar & Battery, Radio & Antenna, Coordinates & Bearing (Map Pins loader + UTM/DMS/DDM), Unit Converter, and **Celestial & Stars** (`#tab-celestial`: Meeus/J2000 ephemeris, ~185-star catalog, 25+ constellations, 14 DSOs, 5 planets, Sun/Moon phase & disc, Milky Way, ecliptic, draggable 360° compass dome, map-pin observer loader).
- **`system.html`** & **`settings.html`**: Live `/api/system/sse` telemetry, `/api/system/time` clock, storage breakdown bar, HTMX 2s polling job queue table, container logs/restart, Shutdown trigger; General settings (`ui_theme`, LAN/passcode/HTTPS/`ark.crt`, LAN Drop, OPDS, Peer Discovery, Trusted LAN IP Allowlist), AI config/model manager, Download limits, USB Export & Profiles, and Signed/GitHub Updates & Rollback.

### Route Map (`RegisterRoutes` in `internal/web/web.go`)
- **Public / Auth / Static / Media**: `/static/` (with empty-protobuf `.pbf` fallback under `/static/vendor/maps/fonts/`), `/media/` (when `dataDir != ""`), `/healthz`, `/lan-auth`, `/tls/ark.crt`
- **Pages**: `/`, `/library`, `/kiwix`, `/get-content`, `/field-guide`, `/apps`, `/ask`, `/jobs` (`localOnly` -> 303 `/system`), `/system` (`localOnly`), `/settings` (`localOnly`), `/settings/{general,lan-allowlist/remove,lan-allowlist/add,lan-allowlist/clear,ai,downloads,usb-export,usb-profile,update,update-github,rollback,restart,shutdown}` (`localOnly`), `/maps`, `/mesh`, `/maps/files/`, `/maps/tiles/`, `/read/zim`, `/read/zim/`, `/tools`, `/lan-drop`, `/opds`, `/opds/`
- **APIs & Proxies**: `/api/jobs`, `/api/jobs/{cancel,retry,delete}` (`localOnly`), `/api/jobs/status`, `/api/kiwix/catalog`, `/api/search`, `/api/library/{delete,updates}` (`localOnly`), `/api/library/icon`, `/api/storage/check-space` (`localOnly`), `/api/update/check` (`localOnly`), `/api/apps/{install,start,stop,uninstall,custom,update}` (`localOnly`), `/api/system/{sse,runtime,time,install-runtime}` (`localOnly`), `/api/zim/{suggest,pages}`, `/api/lan-drop/{posts,post,file/}`, `/api/lan-drop/delete` (`localOnly`), `/api/containers/{restart,logs}` (`localOnly`), `/api/ai/{detect,test,index,index-article,knowledge/remove,sessions/pin,sessions/delete,models/pull,models/delete,models/select}` (`localOnly`), `/api/ai/{status,chat,sessions,sessions/get,models}`, `/api/maps/extract` (`localOnly`), `/api/maps/{pins,tilejson}`, `/api/reader/theme`, `/api/peers/{catalog,download,list}`, `/api/peers/pull` (`localOnly`), `/api/mesh/state`, `/api/mesh/{message,node,clear}` (`localOnly`), `/peer/`, and `/a/` (with redirects `/a/maps` -> `/maps`, `/a/mesh` & `/a/meshtastic` -> `/mesh`, `/a/kiwix*` -> `/library`).

---

## 13. Application Catalog Manifests (`catalog/`)

- `catalog/embed.go`: Embeds `*/app.yaml` into the `ark` binary via `go:embed`.
- `catalog/*/app.yaml`: Declarative manifests parsed by `internal/apps/manifest.go`:
  - `convertx/app.yaml`: `c4illin/convertx:latest`, port `3000`, `proxy: port`, `lan: read`, `memory_mb: 512`.
  - `cyberchef/app.yaml`: `ghcr.io/gchq/cyberchef:latest`, port `8080`, `proxy: path`, `lan: read`, `memory_mb: 128`.
  - `druglabels/app.yaml`: `ark/druglabels:latest`, port `8080`, `proxy: path`, volume `druglabels:/data:rw`, `lan: read`, `memory_mb: 256`.
  - `flatnotes/app.yaml`: `dullage/flatnotes:latest`, port `8080`, `proxy: port`, volume `notes:/data:rw`, `lan: admin`, `memory_mb: 256`.
  - `kolibri/app.yaml`: `learningequality/kolibri:latest`, port `8080`, `proxy: port`, volume `kolibri:/kolibri:rw`, `lan: read`, `memory_mb: 512`.
  - `libretranslate/app.yaml`: `libretranslate/libretranslate:latest`, port `5000`, `proxy: port`, volume `ark-libretranslate-data:/home/libretranslate/.local:rw`, `lan: read`, `memory_mb: 2048`.
  - `maps/app.yaml`: `ghcr.io/protomaps/go-pmtiles:latest`, port `8080`, `proxy: path`, `open_url: /maps`, `memory_mb: 256` (in-process web UI serves PMTiles natively; container optional).
  - `meshtastic/app.yaml`: `native/ark-mesh`, port `8080`, `proxy: path`, `open_url: /mesh`, `healthcheck: /mesh`, `memory_mb: 32` (native browser Web Serial / Web Bluetooth bridge).

---

## 14. Custom Container Images (`images/`)

- `images/embed.go`: Embeds `druglabels`, `archiver`, `indexer`, and `kolibri` build contexts into the binary for on-demand local builds via `internal/docker`.
- `images/command-center/Dockerfile`: Alpine container wrapping a prebuilt `ark-linux` binary, `docker-cli`, `catalog/`, and `images/` for optional `-container` host mode.
- `images/druglabels/` (`Dockerfile`, `main.go`, `main_test.go`, `mod.txt`, `sum.txt`): Multi-stage Go (`1.24` builder + Alpine) standalone openFDA FTS5 web microservice with comma-separated search, situation chips, lazy `/api/label` rendering, `/api/search`, `/api/import`, and `/healthz`.
- `images/indexer/` (`Dockerfile`, `extract.py`): Python 3.11 + Tesseract OCR + `libzim` / `pymupdf` / `ebooklib` JSONL extractor invoked optionally by `internal/ai/indexer.go` when OCR is needed on image files.
- `images/archiver/Dockerfile`: Alpine + Python 3 + `ffmpeg` + `aria2` + `yt-dlp` image for `ZimitRunner` / `YtDlpRunner`.
- `images/kolibri/Dockerfile`: Python 3.11 slim image configuring `kolibri==0.16.1` with local data volumes.

---

## 15. Database Migrations (`migrations/`)

### `migrations/0001_init.sql`
- **Purpose**: Reference initial SQLite schema (`user_version = 1`).
- **How It Works**: Defines base tables (`settings`, `jobs`, `app_states`, `catalog_items`, `usb_profiles`, `rag_documents`) and indexes. `cmd/ark/main.go` embeds an updated `initialMigrationSQL` adding `lan_drop_enabled`, `opds_enabled`, and `lan_posts`, while package initializers create `rag_chunks`, `rag_fts`, `ai_chat_sessions`, `ai_chat_messages`, `map_markers`, `mesh_messages`, `mesh_nodes`, and `lan_allowlist` on startup.
- **Interactions**: Executed/superseded during `initDatabase` and `NewServer` startup in `cmd/ark/main.go` and `internal/web/web.go`.

---

## 16. Installers & Build Scripts (`install/`, `scripts/`)

### `install/install.ps1` & `install/install.sh`
- **Purpose**: Windows PowerShell and Linux/macOS installers and offline USB bundle loaders.
- **How It Works**: Support `-FromUSB` / `--from-usb`, `-DataDir` / `--data-dir`, `-Port` / `--port`, and `-Upgrade` / `--upgrade`. Verify Docker availability for containerized setups, copy offline USB data/images/binaries/DB, back up `ark.db` on upgrade, configure Windows Private firewall rules + optional `ArkCommandCenter` scheduled task (`-RegisterStartup`), or install an `ark.service` systemd unit on Linux when run as root.
- **Interactions**: Deploy and register `ark` on Windows, Linux, and macOS hosts.

### `scripts/install-linux.sh`
- **Purpose**: Lightweight native Linux build script requiring zero Docker.
- **How It Works**: Compiles `CGO_ENABLED=0 go build -o ark ./cmd/ark` and optionally runs `./ark setup-runtime` (`--with-podman`) to install self-contained rootless Podman for optional apps.
- **Interactions**: Builds and optionally configures Linux hosts for Native Mode.

---

## 17. Root Configuration & CI (`./`)

- `go.mod` & `go.sum`: Root Go module (`ark`, Go `1.27`) and locked checksums (`modernc.org/sqlite`, `github.com/docker/docker`, `github.com/docker/go-connections`, `github.com/shirou/gopsutil/v4`, `gopkg.in/yaml.v3`, plus native Zstd/XZ/PDF indirect libraries).
- `README.md`: Project overview, feature tour, quickstart, and documentation links.
- `AGENTS.md`: Repository agent rules enforcing minimal scoped edits, offline-first zero-CDN operation, and local link verification.
- `LICENSE`: MIT license.
- `.github/workflows/release.yml`: GitHub Actions workflow triggered on `v*.*.*` tags or `workflow_dispatch`; runs `go test -count=1 ./...`, builds `CGO_ENABLED=0` binaries (`ark-windows-amd64.exe`, `ark-linux-amd64`, `ark-linux-arm64`, `ark-darwin-arm64`) injecting `ark/internal/update.ArkVersion`, generates `checksums.txt`, and publishes a GitHub Release.
- `.dockerignore` & `.gitignore`: Exclude build outputs, runtime databases, and local data directories from Docker contexts and Git history.
