# Ark — Offline Internet Archive

> **Self-hosted "internet in a box" for preppers and offline independence.**

Ark archives essential human knowledge, reference manuals, education, medical documentation, vector maps, and multimedia so you can access and query them on your local network with **zero external dependencies** when the grid goes down.

---

## Features

- **Native Command Center**: Fast, lightweight Go dashboard running directly on Windows or Linux with real-time hardware telemetry and zero container overhead.
- **Docker Modularity**: Applications run in isolated, disposable Docker containers dynamically reverse-proxied under `/a/<id>/`.
  - **Kiwix**: Wikipedia (Full, No-pics, Mini, Top 100), Wiktionary, WikiMed, Project Gutenberg, Stack Exchange, and iFixit.
  - **Protomaps**: OpenStreetMap offline vector tiles with offline vector styles, fonts, and sprites.
  - **CyberChef**: GCHQ's 300+ offline analysis and decoding tools ("digital Swiss Army knife").
  - **FlatNotes**: Local markdown knowledge base and notebook.
  - **ConvertX**: Universal multi-format document, audio, video, and ebook converter.
  - **FDA Drug Labels**: Full offline database of ~200k+ FDA drug labels searchable by brand, generic, or symptom/situation with SQLite FTS5.
  - **Kolibri**: Offline educational platform with Khan Academy courses.
- **Kiwix Catalog Integration**: Search, filter, and bulk-download thousands of ZIM files directly from Kiwix OPDS feeds with `.meta4` multi-mirror resume.
- **Low-Density, Accessible UI**: Field-terminal dark aesthetic adhering strictly to **WCAG 2.2 AAA** contrast standards (`#0B0D10` background, `#F4F4F0` primary text at 17.7:1, `#FFB000` action accent at 10.6:1).
- **Disk & Storage Safety**: Category storage tracking, live free-capacity checks, and cascading delete preventing orphaned files or vector points.

---

## Quick Start

### Release Assets
Prebuilt standalone binaries are published on the [GitHub Releases](https://github.com/soap353/ARK/releases) page:

| Asset | Target Platform |
| :--- | :--- |
| `ark-linux-amd64` | Intel / AMD 64-bit Linux (`x86_64`) |
| `ark-linux-arm64` | ARM 64-bit Linux (`aarch64`, e.g. Raspberry Pi 4/5) |
| `ark-darwin-arm64` | macOS Apple Silicon (M1–M4) |
| `ark-windows-amd64.exe` | Windows 64-bit (Intel / AMD) |
| `checksums.txt` | SHA-256 checksums for verifying binary integrity |

### Prerequisites
- [Go 1.27+](https://go.dev/) (to build from source)
- [Docker Desktop](https://www.docker.com/products/docker-desktop/) (Windows) or [Docker Engine](https://docs.docker.com/engine/install/) (Linux)

### Build & Run
```bash
# Clone the repository
git clone https://github.com/soap353/ARK.git
cd ARK

# Build the native Command Center binary
go build -o ark.exe ./cmd/ark

# Start Ark Command Center
./ark.exe run -data ./data -port 8080
```

Open [http://localhost:8080](http://localhost:8080) in your browser.

---

## Architecture Overview

```
                      Browser (PC / Phone on LAN)
                                 │
                                 ▼
                     Ark Command Center (:8080)
               Native Go Service · SQLite WAL · SSE
                                 │
                 ┌───────────────┴───────────────┐
                 ▼                               ▼
       Dynamic Reverse Proxy               Job Queue
            (/a/<id>/)               aria2c · zimit · yt-dlp
                 │                               │
    ┌────────────┼────────────┐                  ▼
    ▼            ▼            ▼             ArkData Folder
  Kiwix      CyberChef    ConvertX      (ZIM, Maps, Media, DBs)
(Wikipedia)   (Tools)    (Conversion)
```

---

## Testing

Ark includes automated unit and integration tests across all packages:

```bash
go test -v ./...
```

---

## License

MIT License. See [LICENSE](LICENSE) for details.
