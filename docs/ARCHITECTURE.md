# SuperIDM architecture

Four layers, each with one job. Everything below is in the repository; there are
no generated files to keep in sync beyond the icons.

```
cmd/superidm          entry point: parses flags, picks GUI / tray / headless
   │
   ├── internal/api        REST + SSE over 127.0.0.1, embedded web UI
   ├── internal/app        Win32 window, WebView2 host, tray, autostart
   │
   ├── internal/service    job manager, settings, persistence, event bus
   │        │
   │        └── internal/engine   the transfer core
   └── chrome-extension    browser integration (separate process, talks to the API)
```

## 1. The engine (`internal/engine`)

### Probing

`probe()` sends **`GET` with `Range: bytes=0-0`** rather than a `HEAD`.
Many CDNs answer `HEAD` with a different status code, omit `Content-Length`, or
reject it outright, while a one-byte ranged GET always describes the real
transfer. From the reply the engine learns:

- the total size (`Content-Range: bytes 0-0/12345`),
- whether byte ranges are honoured (206 vs 200),
- the real file name (`Content-Disposition`, with RFC 5987 support),
- the content type and ETag.

If the server ignores `Range`, the job transparently drops to a single
connection and says so in the log — no silent 1× download.

### Adaptive work units (`segments.go`)

The heart of the accelerator. Instead of "N segments, one per connection":

```
file:  [=========================================================]
seed:  [======][======][======][======][======][======][======]   workers × 4
                                  ↓ as the pool runs short, the head range is
                                    split in half and only a half is handed out
claim: [===][===][==][==][=][=][=][=][=][=][=][=][=][=][=][=][=][=]   down to minChunk
```

Properties that make this work:

- **Claims are disjoint**, so the live byte counter is exact and no byte is
  fetched twice.
- **Refinement is automatic**: whenever `len(pending) <= workers`, the head
  range is halved and half is handed out; the pool length stays constant, so it
  keeps subdividing until units reach `minChunk` (1 MiB by default). The last
  megabytes of a file are therefore divided finely and every connection finishes
  within one work unit of the others.
- **No deadlock by construction**: workers only ever block when the pool is
  empty *and* some other worker still owns a unit, because an empty pool with
  zero outstanding units means the transfer is finished.

When a connection fails or stalls, `downloadUnit` re-queues exactly the
unwritten tail (`requeue`) and, once a range has failed twice, hands half of it
to the pool so a healthy socket can race ahead (`partial`). Because claims stay
disjoint and writes are idempotent (same bytes, same offsets), this is safe.

For context: Internet Download Manager splits a file into 8 segments by default,
halves a segment it judges slow, and reassigns freed connections. Those ideas
work, but they are applied *after* a decision to intervene. Here the same
mechanism is the default path for every byte and starts from 8x the connections.

### Connections (`session.go`)

One `httpSession` per worker. Each session owns a client whose transport is
pinned to **HTTP/1.1 with `TLSNextProto` emptied**, so the 64 ranges live on 64
real TCP connections, each with its own congestion window. Metadata requests
(probe, playlists, keys, redirect resolution) use a separate HTTP/2 transport
where multiplexing is a win.

Idle connections are pooled (`MaxIdleConnsPerHost`) so retries and new ranges
reuse a warm TLS session instead of paying another handshake.

### Watchdog

Every read loop stamps an atomic timestamp. A 500 ms ticker cancels the request
when no bytes have arrived for `IdleTimeout` (30 s). `downloadUnit` then treats
it as a normal failure: it keeps the bytes already written, re-queues the tail,
and retries on a fresh connection. This is what turns "download stuck at 43%"
into a two-second hiccup.

### Writing and resume

The destination file is **pre-allocated with `Truncate(size)`** and every worker
writes with `WriteAt` at its own offset. There are no `.part` files and no merge
pass. Completed ranges are appended to a resume map (`[]Range`, merged lazily),
which the service persists every 10 seconds; on restart the engine subtracts
those ranges from `[0,size)` and only re-fetches the gaps.

### Rate limiting

Virtual-scheduling token bucket. Each `wait(n)` call takes a *departure time*
under a mutex and sleeps in 500 ms slices until that fixed time — the schedule
is never shortened, so the cap holds exactly even with 64 concurrent callers
(measured within 1%).

### HLS capture (`hls.go`)

Parses M3U8 playlists (master → best variant by bandwidth, media playlists,
`EXT-X-MAP` init segments, `AES-128` keys with explicit or sequence-derived
IVs), downloads segments through the same worker pool, decrypts AES-128-CBC in
flight, and streams them to the output file **in index order** through a writer
goroutine fed by a small reorder buffer, so memory stays bounded regardless of
segment count.

## 2. The service layer (`internal/service`)

- **Job map + ordering**, snapshotting each job 3.3×/s for the UI.
- **Scheduler**: `MaxConcurrent` (default 3) jobs may run at once; the rest sit
  in `queued` and are promoted automatically as slots free up.
- **Persistence**: `%APPDATA%\SuperIDM\state.json` keeps options, snapshots and
  the resume map; `settings.json` keeps user settings; `superidm.log` keeps a
  rolling activity log. Writes are atomic (temp file + rename).
- **Event bus**: fan-out channels, non-blocking send (a slow UI can never stall
  a download).
- **Clipboard**: `copyToClipboard` / `clipboardText` go straight to
  `user32.dll` on Windows so no console window flashes.

## 3. The API and UI (`internal/api`)

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/health` | liveness + version (used by the extension) |
| `GET` | `/api/jobs` | all jobs |
| `POST` | `/api/jobs` | add a download |
| `GET` | `/api/jobs/{id}` | one job |
| `POST` | `/api/jobs/{id}/{pause,resume,cancel,restart,open,reveal}` | control |
| `DELETE` | `/api/jobs/{id}?delete=1` | remove (and optionally delete the file) |
| `POST` | `/api/jobs/actions/{pause-all,resume-all,clear-completed}` | bulk |
| `GET`/`POST` | `/api/settings` | read/write settings |
| `POST` | `/api/speedlimit` | global or per-job cap |
| `GET`/`POST` | `/api/inspect` | probe a URL without downloading |
| `POST` | `/api/clipboard` | put text on the clipboard |
| `GET`/`POST`/`DELETE` | `/api/sniff` | captured media list / add / clear |
| `POST` | `/api/sniff/download` | turn a captured stream into a job |
| `GET` | `/api/events` | Server-Sent Events: jobs, log, settings |
| `GET` | `/api/log` | tail of the log file |

Security posture:

- socket bound to `127.0.0.1`, never `0.0.0.0`;
- CORS reflected only for `chrome-extension://`, `moz-extension://`, and
  `localhost` origins;
- simple-request content types are not enough to change state — handlers only
  read `application/json` bodies, which forces a CORS preflight that a random
  website cannot pass.

The UI is served from the same server via `go:embed`, so `SuperIDM.exe` contains
its own front-end. It is plain HTML/CSS/JS (no framework, no build step) and
subscribes to `/api/events` for live updates.

## 4. The Windows shell (`internal/app`)

- `win32.go` — every Win32 declaration, struct and constant used.
- `app_windows.go` — window class, message loop, tray commands, DPI awareness,
  single-instance mutex, autostart registry value, `ShellExecute` helpers.
- `webview2_windows.go` — a **hand-written COM host** for WebView2: IIDs, vtable
  slot numbers named per method, `syscall.SyscallN` calls, Go implementations of
  `ICoreWebView2CreateEnvironmentCompletedHandler`,
  `ICoreWebView2CreateControllerCompletedHandler` and
  `ICoreWebView2EnvironmentOptions`.
- `tray_windows.go` — `Shell_NotifyIconW` with a context menu, balloon
  notifications, and live tooltips; re-adds the icon if Explorer restarts.

If `WebView2Loader.dll` or the Evergreen runtime is missing, the app logs it and
opens the UI in the default browser — the product still works.

## 5. The extension (`chrome-extension`)

Manifest V3, `service_worker` background, no bundler.

- **Context menus** for links, media elements, selections, the page itself, and
  bulk link collection.
- **Download takeover**: `chrome.downloads.onCreated` → cancel → hand the URL,
  filename, referer and cookies to the local API.
- **Media sniffing**: `chrome.webRequest.onResponseStarted` on media-shaped URL
  patterns. Only interesting responses are examined (`classify()` rejects
  ordinary files), results are grouped per tab, debounced, and published to
  `/api/sniff`. The toolbar badge shows the per-tab count.
- **Content script**: DOM + `performance.getEntriesByType('resource')` scanning,
  the floating "Download with SuperIDM" pill over players, and page toasts.
- **Popup**: live job progress, captured stream list, quick-add box, one-click
  bulk actions; it polls the worker, which polls the API.

## Testing

`internal/engine/engine_test.go` spins up local origins and asserts behaviour
rather than implementation:

| Test | Proves |
|---|---|
| `TestParallelDownloadExactContent` | 6 MiB + 12345 bytes arrives byte-exact over 32 connections, with multiple segments |
| `TestSingleConnectionFallback` | a server without range support still produces the right file |
| `TestChecksumVerification` | good digests pass, wrong digests fail the job |
| `TestPauseAndResumeKeepsData` | pause mid-transfer, resume with the persisted map, byte-identical result |
| `TestStalledConnectionIsRecovered` | a socket that sends headers then hangs is detected by the watchdog and re-queued (the middle of a 4 MiB file is served by healthy connections) |
| `TestWorkQueueBalances` | the pool subdivides: 100 MiB across 10 workers, no unit bigger than a quarter of the file, every byte claimed exactly once |
| `TestRateLimiter*` | the cap holds within 25% for one caller and 30% for 64 concurrent callers |
| `TestParseM3U8`, `TestMasterPlaylistPicksHighestBandwidth` | playlist parsing, sequence-derived IVs, variant selection |

Run everything with `make test`; `make race` re-runs the concurrency-heavy ones
under the race detector.
