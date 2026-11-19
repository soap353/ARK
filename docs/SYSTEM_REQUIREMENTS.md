# Ark System Requirements & Memory Profile

## Measured idle memory (this machine, all apps running)

| Component | Idle RAM | Cap now enforced |
|---|---|---|
| `ark-core` (UI + API container) | ~30 MB | 384 MB (`GOMEMLIMIT=256MiB`) |
| `ark.exe` (host launcher / tray) | ~26 MB | Go soft limit 256 MiB, GC at 50% |
| LibreTranslate | ~225 MB (grows while translating) | 2048 MB |
| Kolibri | ~99 MB | 512 MB |
| Flatnotes | ~44 MB | 256 MB |
| ConvertX | ~19 MB | 512 MB |
| CyberChef | ~18 MB | 128 MB |
| Maps | ~10 MB | 256 MB |
| Kiwix | ~3 MB idle (page cache grows with use) | 512 MB |
| Drug Labels | ~2 MB | 256 MB |
| Ollama (optional) | model-dependent | none (model size decides) |

Ark's own code is ~56 MB total. Almost all memory belongs to optional apps, and each can be stopped independently.

## Estimated minimum requirements

These are estimates built from the measurements above plus typical Docker overhead. They were not benchmarked on small machines.

| Profile | RAM | CPU | Notes |
|---|---|---|---|
| **Minimum** (Ark + Kiwix/ZIM reading, 1-2 light apps) | 4 GB (Windows/macOS with Docker Desktop); 2 GB (native Linux Docker) | 2 cores | Docker Desktop's VM takes ~1-2 GB by itself. |
| **Recommended** (all apps except AI) | 8 GB | 4 cores | ~500 MB for apps at idle, more under load. |
| **With LibreTranslate in active use** | add ~1-2 GB | | Language models load into RAM. |
| **With local AI (Ollama)** | add 4-8+ GB | 4+ cores | 3B models need ~4 GB; 7-8B models ~8 GB. |

Disk: ~5-10 GB for Docker images, plus content (ZIM files range from MBs to 100+ GB). SSD recommended for ZIM search.

## Memory safeguards added

- **Go runtime**: GC triggers at 50% heap growth and a 256 MiB soft limit (override with `GOGC` / `GOMEMLIMIT`).
- **Container caps**: new `memory_mb` field in `catalog/*/app.yaml` sets a hard limit with swap disabled, so a runaway app is restarted by Docker and can't starve the host.
- **Drug-label import**: each FDA ZIP is streamed to a temp file instead of loaded entirely into RAM.
- **Indexer**: plain-text/HTML files are read at most 16 MB each when indexing.

## Reducing memory further

- Stop unused apps from the Apps page; stopped containers use no RAM.
- Avoid running Ollama and LibreTranslate at the same time on 8 GB machines.
