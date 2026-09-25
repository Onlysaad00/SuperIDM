# Changelog

All notable changes to SuperIDM are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[semantic versioning](https://semver.org/).

## [1.0.1] — 2026-09-25

### Fixed
- Jobs that fall back to a single connection (servers without byte-range
  support, or unknown-length streams) now report `1 connection` instead of `0`,
  so the UI never implies acceleration that is not happening.
- A pause arriving after the final byte of a transfer no longer leaves the job
  sitting at 100% in the paused state; it completes.
- The speed limiter now holds its configured cap exactly. Its sleep was
  previously clamped, which let long waits drift forward and raised effective
  throughput to roughly 2.6x the requested limit under many connections.
- Session files are no longer written while the job map is being read
  (a data race that the race detector flagged on shutdown).
- `settings.json` is created on first run, so the file always exists to inspect
  or edit.
- The `--inspect`/`-o` file name argument is honoured instead of being
  overwritten by the name derived from the URL.

### Added
- `arm64` builds are attached to GitHub releases, and CI asserts that every
  published binary really is a PE32+ executable of the intended architecture
  and subsystem.

## [1.0.0] — 2026-09-25

First release.

### Engine
- Adaptive work-unit scheduler (DAC): the file is a pool of byte ranges that
  idle connections claim, and the pool subdivides itself as it runs short, so
  the tail of a file is fetched in small pieces instead of leaving stragglers.
- 64 parallel connections per download by default, configurable up to 128
  (`-n`, the UI slider, or the settings file).
- HTTP/1.1-pinned worker sockets (one real TCP connection and congestion window
  per range) while metadata requests use HTTP/2.
- Stall watchdog: a connection that stops delivering bytes is cancelled within
  the idle timeout, and its remaining range is re-queued onto a fresh connection.
- Pre-allocated destination file with positional writes — no per-segment temp
  files and no final merge pass.
- Resume from a persisted range map: close the app mid-download and it continues
  from the exact bytes already on disk.
- Accurate virtual-scheduling rate limiter (global or per download) that holds
  its cap even with 64 concurrent connections.
- Checksum verification (MD5, SHA-1, SHA-256) with automatic failure on mismatch.
- Automatic single-connection fallback for servers without byte-range support,
  clearly reported in the log.
- HLS/M3U8 capture: master-playlist variant selection, init segments,
  AES-128-CBC decryption with explicit or sequence-derived IVs, re-ordered
  parallel segment download, single playable output file.
- Category routing (Video, Music, Documents, Compressed, Programs, Pictures),
  custom categories, and collision-safe file naming.

### App
- Native Windows window hosting the embedded UI in WebView2, with an automatic
  browser fallback when the runtime is missing.
- System tray: live speed tooltip, open, add download, pause all, resume all,
  speed limiter presets, open download folder, open extension folder, open log,
  about, exit.
- Single-instance handling, minimise-to-tray, "start with Windows" (writes the
  `HKCU\...\Run` value itself), per-monitor DPI awareness.
- Loopback REST API on 127.0.0.1 with Server-Sent Events, plus the complete web
  UI (downloads, media capture, activity log, extension setup, settings).
- Command line: `-d`, `-o`, `-n`, `-dir`, `-referer`, `-cookie`, `-checksum`,
  `-paused`, `--inspect`, `--tray`, `--headless`, `--version`.
- Session persistence: jobs, options, resume maps and settings survive restarts.

### Browser extension (Manifest V3)
- Context menus for links, media elements, selections, the page and bulk links.
- Optional takeover of the browser's own downloads.
- Media sniffing for HLS, DASH and progressive video/audio, with a per-tab
  toolbar badge and a captured-media list in the popup.
- Floating "Download with SuperIDM" button over playing video and audio.
- `Alt+S` to grab the current page's main media.
- Cookie and referer forwarding for authenticated downloads (can be disabled).
- Live job list with pause/resume/open actions inside the popup.

### Tooling
- `tools/make_icons.py` generates every icon (extension PNGs, multi-size `.ico`,
  README artwork, and the Go source of the embedded tray icon) from pure Python.
- `tools/benchmark.sh` + `tools/benchserver` reproduce the speed comparison
  against a locally shaped origin.
- `build.ps1`, `build.sh` and `Makefile` produce binaries, the extension zip, the
  release bundle and `SHA256SUMS.txt`; CI attaches them to GitHub Releases.
- Test suite covering byte-exact parallel transfers, single-connection fallback,
  pause/resume integrity, stall recovery, queue balancing, the rate limiter and
  playlist parsing.
