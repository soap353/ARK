# Ark Codebase Refactoring Log

This document records the safe and clean refactoring changes applied across Ark packages. All changes preserve 100% of existing functionality, HTTP routes, APIs, and test suites while eliminating dead code, removing unused imports, reducing nesting, and consolidating duplicate helper logic.

---

## 1. `cmd/` & System / Host Packages (`cmd-pkg`)

### `internal/system/sse.go`
- **Change**: Extracted helper function `sendMetricsFrame(ctx, w, flusher, diskPath) error`.
- **Reason**: Deduplicated metrics-gathering (`CollectMetrics`), JSON marshaling, and SSE frame formatting (`data: ...\n\n`) shared between the initial frame transmission and the recurring ticker loop in `SSEFeed.ServeHTTP`.

### `internal/system/downtime.go`
- **Change**: Extracted helper functions `normalizeUTC(now time.Time) time.Time` and `upsertSetting(ctx context.Context, db *sql.DB, key, value string) error`.
- **Reason**: Deduplicated UTC timestamp normalization (stripping sub-second drift) and SQLite `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value` queries across `RecordShutdownTime` and `RecordStartupAndComputeDowntime`.

---

## 2. Core Service Packages (`core-pkg`)

### `internal/storage/integrity.go` & `internal/storage/delete.go`
- **Change**: Extracted shared helper function `resolveDataPath(dataDir, p string) string`.
- **Reason**: Consolidated 4 duplicate relative-vs-absolute path resolution blocks across `CheckIntegrity`, `PruneOrphans`, and `CascadeDelete` into a single canonical helper.

### `internal/apps/proxy.go`
- **Change**: Extracted helper function `writeAppStatusPage(w http.ResponseWriter, status int, title, heading, bodyHTML string)`.
- **Reason**: Deduplicated the identical HTML status/error page template used in both `ProxyManager.Register`'s reverse-proxy `errorHandler` and `ProxyManager.ServeHTTP`'s inactive/unregistered app responses.

### `internal/jobs/runners.go`
- **Change**: Removed unused unexported method `Aria2cRunner.probeRangeSupport`.
- **Reason**: Dead code elimination; HTTP range header validation is handled directly by `fallbackHTTPDownload` and upstream `aria2c`.

---

## 3. Web & UI Packages (`web-pkg`)

### `internal/web/helpers.go` (New)
- **Change**: Created centralized HTTP response and routing utilities:
  - `requireMethod(w http.ResponseWriter, r *http.Request, method string) bool`: Validates HTTP request method, sending `405 Method Not Allowed` on mismatch.
  - `writeJSON(w http.ResponseWriter, status int, v any)`: Sets `Content-Type: application/json`, status header, and encodes JSON payload.
  - `writeJSONError(w http.ResponseWriter, status int, message string)`: Emits consistent JSON error responses `{"error": "..."}`.
  - `redirectWithQuery(w http.ResponseWriter, r *http.Request, base, param, value string)`: Constructs and executes a 303 redirect with URL-query escaped parameters.
- **Reason**: Eliminated redundant method checks, header declarations, JSON encoding boilerplate, and error handling across HTTP handlers.

### `internal/web/handlers_phase3.go`
- **Change**: Replaced repetitive `r.Method != http.MethodPost` guard clauses and manual `http.Redirect(..., "/settings?err="+url.QueryEscape(...))` patterns with `requireMethod` and `redirectWithQuery` in `handleSettingsUSBProfile`, `handleSettingsUSBExport`, `handleSettingsUpdate`, and `handleSettingsRollback`. Removed unused import `"net/url"`.
- **Reason**: Code consolidation, improved readability, and elimination of compiler errors due to unused imports.

### `internal/web/handlers_phase2.go`
- **Change**: Replaced boilerplate method checks in `handleSettingsGeneral`, `handleSettingsAI`, `handleSettingsDownloads`, `handleAPIContainerRestart`, `handleAPIAIDetect`, and `handleAPIAITest` with `requireMethod`. Standardized JSON responses in `handleAPIAIDetect` using `writeJSON`.
- **Reason**: Consistency and deduplication across settings and telemetry endpoints.

---

## 4. Images, Manifests, & Installers (`images-catalog`)

### `images/druglabels/main.go`, `images/indexer/extract.py`, `catalog/`, `install/`
- **Audit Result**: Clean bill of health. All 9 app manifests (`catalog/*/app.yaml`), standalone service files (`images/druglabels/main.go`, `images/druglabels/go.mod`, `images/indexer/extract.py`), and installers (`install/install.ps1`, `install/install.sh`) are minimal, modular, and free of dead code or extraneous dependencies.

---

## 5. Memory Efficiency Pass

- `cmd/ark/main.go`: `tuneRuntimeMemory()` sets GC percent 50 and a 256 MiB soft memory limit (respects `GOGC`/`GOMEMLIMIT`).
- `internal/apps/manifest.go` + `internal/docker/docker.go`: new `memory_mb` manifest field applied as a hard container limit (swap disabled). Core container capped at 384 MB with `GOMEMLIMIT=256MiB`.
- `catalog/*/app.yaml`: added `memory_mb` caps to 8 apps (Ollama intentionally uncapped).
- `internal/jobs/druglabels.go`: FDA ZIP partitions are streamed to a temp file (`spoolZip`) instead of `io.ReadAll`.
- `internal/ai/indexer.go`: `readCapped` limits plain-text/HTML reads to 16 MB per file.
- See `docs/SYSTEM_REQUIREMENTS.md` for measurements and minimum-spec estimates.
