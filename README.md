<div align="center">

<img src="assets/superidm-256.png" width="120" alt="SuperIDM">

# SuperIDM

**A multi-connection download manager for Windows with a matching Chrome/Edge extension — built to be faster than Internet Download Manager.**

`64 connections` · `adaptive work units` · `HLS stream capture` · `pause & resume` · `single .exe` · `no dependencies`

[![Build](https://github.com/Onlysaad00/SuperIDM/actions/workflows/build.yml/badge.svg)](https://github.com/Onlysaad00/SuperIDM/actions/workflows/build.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Windows%2010%2F11-blue)](https://github.com/Onlysaad00/SuperIDM/releases)
[![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8)](https://go.dev)

</div>

---

## Why it is faster than IDM

Internet Download Manager splits a file into **8 fixed segments**. SuperIDM uses three ideas that a fixed-segment design cannot match:

| Technique | What original IDM does | What SuperIDM does |
|---|---|---|
| **Connections per file** | 8 by default, up to 32 in the registered version | **64 by default, up to 128** |
| **Work distribution** | starts with 8 fixed segments; a segment judged slow is split in half and freed connections are reassigned | **one global claim queue**: any idle connection takes the next range, and the queue *subdivides itself* as it runs short, down to 1 MiB units |
| **Ownership of a range** | a range stays with the connection that owns it until it finishes or is deemed slow | ownership is transient — the unwritten tail of any range can move to a healthy connection immediately |
| **Dead sockets** | stalls until a timeout expires | watchdog cancels within seconds and re-queues that byte range |
| **Assembling the file** | writes N temp segment files, then merges them | writes straight into the final file with `pwrite` — **no merge pass** |

The third row is the important one. Any design where a range belongs to one connection until that connection is *finished or judged slow* still lets a straggler hold back the whole transfer between those decisions, and it starts from only 8 pieces — so the entire file is divided coarsely for most of the download. SuperIDM makes ownership transient from the first byte and keeps shrinking the granularity, so aggregate throughput tracks the **average** connection rather than the slowest one, and the last megabytes are split finely enough that every connection finishes within one work unit of the others.

(To be fair to IDM: its segment halving and connection reassignment are real and effective. SuperIDM simply makes those two ideas the default path for every byte, and starts with 8× the connections.)

### Measured, not claimed

`tools/benchmark.sh` emulates a link where every connection is individually shaped to 2 MiB/s with 12 ms of latency — i.e. exactly the situation a per-flow traffic shaper creates — and downloads the same 64 MiB file three ways:

```
  method                                         time        speed  vs single
  ------------------------------------------------------------------------
  single connection (curl / browser)           33.12s       1.9 MiB/s       1.0x
  SuperIDM, 8 connections (IDM default)         4.08s      15.7 MiB/s       8.1x
  SuperIDM, 64 connections (default)            0.59s     108.7 MiB/s      56.2x

  SuperIDM default vs IDM default (8 conns): 6.93x faster
  integrity: all three files identical -> True
```

Reproduce it yourself: `bash tools/benchmark.sh`

> Numbers depend on your link and the server. Servers that don't support byte ranges (some CDNs, dynamically generated archives) fall back to a single connection — SuperIDM detects that and tells you, instead of silently downloading at 1× speed.

---

## What you get

### 1. `SuperIDM.exe` — the desktop app
- Native Windows window (WebView2) with a dark, fast UI; **falls back to your browser automatically** if the WebView2 runtime is missing.
- System-tray icon with live speed in the tooltip, pause/resume/exit, and balloon notifications.
- **Add by link, paste detection, per-download folders**, category sorting (Video/Music/Documents/Compressed/Programs/Pictures), checksum verification (MD5/SHA-1/SHA-256).
- **Pause, resume and cancel** at any time; a crashed or closed app resumes from the exact bytes already on disk.
- Global and per-download **speed limiter** (accurate to within 1%, even with 64 connections).
- Seamless multi-part download manager features: live per-job speed, ETA, segment counts, retry counts and a colour-coded activity log.
- **HLS / M3U8 capture** — saves segmented streams (including AES-128 encrypted playlists) as one playable file.
- Command line for scripting: `SuperIDM-cli.exe -d URL -n 64 -o file.iso`.

### 2. `chrome-extension/` — the browser integration
Same idea as the official IDM extension, but it also understands streams:

- **Right-click any link → "Download with SuperIDM"**, and right-click a video/audio element → download that media.
- **Takes over the browser's own downloads** (optional) and routes them into SuperIDM with full acceleration.
- **Media sniffing**: catches HLS (`.m3u8`), DASH (`.mpd`) and progressive video/audio the page plays, lists them in the popup, and downloads streams as a single file.
- **Floating button over any playing video** — hover for half a second and a SuperIDM pill appears, no menus needed.
- **Alt+S** grabs the main media of the current page.
- **Download all links on a page**, with the page's own cookies and referer attached so protected files work.
- Popup shows live job progress and lets you pause/resume right from the toolbar.
- Works in **Chrome, Edge, Brave, Opera and Vivaldi** (any Chromium 102+).

---

## Install

### Windows (prebuilt)

1. Download `SuperIDM-vX.Y.Z-windows-x64.zip` from [Releases](../../releases) and **extract the whole archive**.
2. Run **`SuperIDM.exe`**. (Windows SmartScreen may warn about the unsigned build: *More info → Run anyway*.)
3. In the app open **Extension & setup → Open extension folder**.
4. In your browser go to `chrome://extensions`, enable **Developer mode**, click **Load unpacked**, and select the `chrome-extension` folder from step 3.
5. Done — downloads and streams now route through SuperIDM.

### Build it yourself

```bash
git clone https://github.com/Onlysaad00/SuperIDM
cd SuperIDM
go build ./...            # no CGO, no MSVC, no resource compiler needed
make release              # or: bash build.sh   -> dist/
```

On Windows: `.\build.ps1 -Zip -Test`

Full instructions, including how the single-file build works without any toolchain beyond Go, are in **[docs/BUILDING.md](docs/BUILDING.md)**.

---

## Command line

```text
SuperIDM-cli.exe -d "https://example.com/ubuntu.iso" -n 64 -o ubuntu.iso
SuperIDM-cli.exe -d "https://example.com/file.zip" -referer "https://example.com/page" \
                 -cookie "session=abc" -checksum sha256:9f86d0…
SuperIDM-cli.exe --inspect "https://example.com/file.zip"
SuperIDM-cli.exe --tray            # start hidden in the tray
SuperIDM-cli.exe --headless        # API only, for scripts
```

| Option | Meaning |
|---|---|
| `-d URL` | download this URL |
| `-o NAME\|PATH` | output file name, or an absolute path |
| `-n N` | parallel connections, 1–128 (default 64) |
| `-dir FOLDER` | destination folder |
| `-referer`, `-cookie` | headers for protected links |
| `-checksum SPEC` | `md5:…`, `sha1:…` or `sha256:…` to verify |
| `-paused` | queue without starting |
| `--inspect URL` | show size, name, and whether ranges are supported |

Links passed as plain arguments are also queued: `SuperIDM-cli.exe "https://…"`.

---

## How it works

```
                 ┌──────────────────────────── SuperIDM.exe ───────────────────────────┐
  ┌──────────┐   │                                                                     │
  │  Chrome  │   │   ┌──────────────┐   ┌────────────────┐   ┌────────────────────┐    │
  │  + Super │──▶│   │  Local API   │──▶│  Job manager   │──▶│  Transfer engine   │    │
  │  IDM ext │   │   │ 127.0.0.1:   │   │ scheduler,     │   │  N sockets,        │    │
  └──────────┘   │   │ 8765 (loop-  │   │ queue, resume, │   │  adaptive units,   │    │
                 │   │ back only)   │   │ persistence    │   │  pwrite, watchdog  │    │
  ┌──────────┐   │   └──────────────┘   └────────────────┘   └────────────────────┘    │
  │  Desktop │──▶│            ▲                                        │              │
  │   UI     │   │            └──────── live progress (SSE) ───────────┘              │
  └──────────┘   │                                                                     │
                 └─────────────────────────────────────────────────────────────────────┘
```

- **`internal/engine`** — the transfer core: probe, adaptive work queue, per-connection sessions, watchdog, HLS, checksums, resume map. Pure Go, no dependencies.
- **`internal/service`** — job lifecycle, concurrency limits, settings, session persistence, log, event bus.
- **`internal/api`** — loopback REST API + Server-Sent Events + the embedded web UI.
- **`internal/app`** — Win32 shell: window, WebView2 host, tray icon, autostart (all through `syscall`, no CGO).
- **`chrome-extension/`** — MV3 extension: context menus, download takeover, media sniffing, floating button.

Design detail worth knowing: the engine deliberately pins **HTTP/1.1 for the range workers** so each connection is a real, independent TCP socket with its own congestion window. HTTP/2 would multiplex all 64 ranges over one connection — which is the opposite of what an accelerator wants — while metadata requests (probing, playlists) still use HTTP/2.

See **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** for the full walkthrough.

---

## Privacy & safety

- The API binds to **`127.0.0.1` only**. Nothing is exposed to your network or the internet.
- The extension sends captured URLs, and (if you leave that option on) cookies for those URLs, **to your own machine, over loopback**. There is no telemetry, no analytics and no remote server.
- CORS is restricted to extension origins and localhost, and state-changing requests must be `application/json` — a random website cannot drive your local API.

---

## Project layout

```
cmd/superidm/         application entry point (GUI + CLI in one binary)
internal/engine/      download engine (multi-connection core, HLS, checksums)
internal/service/     job manager, settings, persistence, event bus
internal/api/         REST API, SSE, embedded web UI
internal/api/web/     the UI (HTML/CSS/JS, no build step, no frameworks)
internal/app/         Win32 window, WebView2 host, tray, autostart
chrome-extension/     the browser extension (load unpacked)
tools/                icon generator, benchmark origin, benchmark runner
docs/                 architecture, building, protocol reference
```

## Contributing

Issues and pull requests are welcome. Before sending a patch:

```bash
go test ./... -timeout 300s     # includes integration tests against local origins
gofmt -l cmd internal           # must print nothing
go vet ./...
```

## License

MIT — see [LICENSE](LICENSE).

SuperIDM is an independent project. It is not affiliated with, endorsed by, or derived from Internet Download Manager (Tonec Inc.); "IDM" is referenced only to compare behaviour.
