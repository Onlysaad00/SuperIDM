# SuperIDM Integration — browser extension

This extension is the browser half of [SuperIDM](../README.md). It does not
download anything itself: it finds links and streams, then hands them to the
SuperIDM desktop app over `http://127.0.0.1:8765`, where the multi-connection
engine takes over.

Works in any Chromium browser 102+: **Chrome, Edge, Brave, Opera, Vivaldi**.

## Install (developer mode)

1. Start **`SuperIDM.exe`** and open the **Extension & setup** tab.
2. In the browser, open `chrome://extensions` (Edge: `edge://extensions`).
3. Enable **Developer mode** (top-right toggle).
4. Click **Load unpacked** and select this folder.
5. Pin the extension to the toolbar — the icon doubles as a status badge.

No build step is required; the folder is the extension.

## What it does

| Feature | How to use it |
|---|---|
| Download a link | Right-click any link → **Download with SuperIDM** |
| Download a video/audio file | Right-click the player → **Download this media with SuperIDM** |
| Grab the media of the current page | Press **Alt+S**, or click the toolbar icon → ⬇ next to the item |
| Floating button | Hover a playing video/audio for half a second → **SuperIDM** pill appears |
| Download all links on a page | Right-click the page → **Download all links on this page** |
| Take over browser downloads | On by default. Any download the browser starts is cancelled and routed to SuperIDM with full acceleration. Toggle in the popup footer or options. |
| Send detected streams | Right-click the page → **Send detected streams to SuperIDM** |
| Pause everything | Toolbar icon → **Pause all** |

The toolbar badge shows how many media links were detected in the current tab.

## Media capture

The extension watches responses whose URL looks like media (`*.m3u8`, `*.mpd`,
`*.mp4`, `*.m4s`, `*.webm`, `*.ts`, `*.mp3`, `*.m4a`, …) plus the DOM
(`<video>`, `<audio>`, `<source>`, `<track>`, performance entries). Detected
items are listed in the popup with their resolution and size where known.

Choosing ⬇ for an **HLS stream** sends the playlist to SuperIDM, which resolves
the master playlist to the highest-bandwidth variant, downloads every segment
over its normal connection pool, decrypts AES-128 playlists, and writes one
playable `.ts`/`.mp4` file. This is the case browsers cannot save at all.

DASH manifests (`.mpd`) are detected and listed, but SuperIDM currently reports
them as unsupported rather than producing a broken file.

## Downloads with cookies

Member-only files and streams usually need the session cookie and the referring
page. The extension attaches the cookies for the target URL (via
`chrome.cookies`) and the page URL as `Referer` for every request it makes.

These values are sent **only to your own machine** on the loopback interface,
never anywhere else. You can turn cookie forwarding off in the extension options.
There is no telemetry in this project.

## Options

Open from the popup gear, or `chrome://extensions` → Details → Extension
options:

| Setting | Default | Meaning |
|---|---|---|
| Local API port | `8765` | must match the port shown in SuperIDM → Extension & setup |
| Take over browser downloads | on | cancel browser downloads and route them to SuperIDM |
| Capture media streams | on | detect HLS/DASH/progressive media |
| Show the floating download button | on | the hover pill over players |
| Send cookies | on | attach session cookies for authenticated downloads |
| Show notifications | on | confirm when a link has been handed over |

## Permissions, and why each one is needed

| Permission | Reason |
|---|---|
| `contextMenus` | the right-click entries |
| `downloads` | cancel a browser download so SuperIDM can take it over |
| `cookies` | authenticate protected downloads (optional, can be disabled) |
| `webRequest` | observe responses to find media streams (read-only) |
| `storage` | remember your settings |
| `notifications` | the "handed to SuperIDM" messages |
| `tabs`, `scripting` | read the current tab's title/URL and talk to the page |
| `host_permissions` | `http://127.0.0.1/*` for the local app, plus the sites you visit so links and streams can be detected |

## Troubleshooting

| Problem | Fix |
|---|---|
| "SuperIDM is not running" | Start `SuperIDM.exe`. Check the port in the options matches the app. |
| Nothing is captured on a page | Press play first — most players only request the stream when playback starts. Then click **Scan for media**. |
| A stream downloads but the file won't play | It was probably a DASH or DRM-protected (`SAMPLE-AES`) stream, which is out of scope. The log says so. |
| Downloads still go to the browser | Turn on *Take over browser downloads* in the popup, and reload the page. |
| The floating pill never appears | Turn it on in the options; note that it only shows over real `<video>`/`<audio>` elements. |

## Privacy

- Talk to `127.0.0.1` only. No analytics, no accounts, no remote endpoints.
- Nothing is uploaded anywhere by the extension.

## License

MIT — see the repository [LICENSE](../LICENSE).
