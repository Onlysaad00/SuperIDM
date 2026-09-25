# Building SuperIDM

SuperIDM builds to a **single self-contained `.exe`** with nothing but the Go
toolchain. No CGO, no MSVC, no Windows SDK, no resource compiler, no installer
framework, no `node_modules`.

That is a deliberate design decision: the more of the build that lives in Go,
the easier it is for anyone to rebuild the binary and verify it.

## Requirements

| | |
|---|---|
| **Go** | 1.22 or newer — <https://go.dev/dl/> (`winget install GoLang.Go`) |
| **Python** | only to regenerate icons (3.8+). Icons are committed, so this is optional. |
| **zip** | only for the release bundle (`make release`) |

Everything else — HTTP/2, WebView2 hosting, the tray icon, the clipboard, the
embedded UI — is implemented in-tree with the standard library and `syscall`.

## Quick builds

### Windows (native)

```powershell
git clone https://github.com/Onlysaad00/SuperIDM
cd SuperIDM

go build -o dist\SuperIDM.exe -ldflags "-s -w -H=windowsgui" .\cmd\superidm

# or the full pipeline: binaries + extension zip + checksums
.\build.ps1 -Zip -Test
```

### Linux / macOS / WSL (cross-compiling for Windows)

```bash
make release          # everything into ./dist
# or
bash build.sh
```

Cross-compilation needs no extra toolchain because the whole program is CGO-free
(`CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build`).

## What the flags do

```
-ldflags "-s -w -H=windowsgui -X main.Version=1.2.0"
             │   │         │                └── version shown in the UI and --version
             │   │         └── build as a GUI app: no console window is created
             │   └── drop the DWARF symbol table
             └── drop the symbol table (≈35% smaller)
```

`SuperIDM-cli.exe` is the same code without `-H=windowsgui`, so it keeps a
console for scripting.

## Build targets

```bash
make               # windows exe + cli exe + arm64 + extension zip
make windows       # just the Windows binaries
make linux         # a native binary for testing the API/UI on this machine
make test          # unit + integration tests (local origins, no internet needed)
make race          # the engine under the race detector
make bench         # the throughput comparison (see README)
make icons         # regenerate every icon from tools/make_icons.py
make release       # build.sh: binaries, extension zip, release bundle, SHA256SUMS
make clean
```

## How the pieces of a "normal" Windows build are replaced

| Usual requirement | How SuperIDM avoids it |
|---|---|
| `.rc` resource file + resource compiler for the icon | The icon is generated as raw `.ico` bytes by `tools/make_icons.py`, embedded in the binary as a Go byte slice, and turned into an `HICON` at runtime with `CreateIconFromResourceEx`. |
| A GUI framework (Qt, WinForms, Electron) | The UI is HTML/CSS/JS served from the embedded filesystem and rendered by WebView2 — which ships with Windows 11 and with Windows 10 via the Evergreen Runtime. If it is missing, SuperIDM opens the same UI in the default browser instead. |
| Node.js + npm to build the UI | The UI is `internal/api/web/{index.html,style.css,app.js}` — no bundler, no framework, no build step. `go:embed` packs it into the executable. |
| An installer (NSIS/MSI) | A `.zip`. The app is portable by design; autostart is a single `HKCU\...\Run` value the app writes itself. |
| `golang.org/x/sys/windows` or COM bindings | Everything uses `syscall.NewLazyDLL` / `syscall.SyscallN` directly, including the WebView2 COM interfaces (see `internal/app/webview2_windows.go`). |

## Verifying a build

```bash
# the exe is a real 64-bit Windows GUI binary
python3 - <<'EOF'
import struct
f = open('dist/SuperIDM.exe','rb').read()
e = struct.unpack_from('<I', f, 0x3c)[0]
print('machine  :', hex(struct.unpack_from('<H', f, e+4)[0]), '(0x8664 = x64)')
print('subsystem:', struct.unpack_from('<H', f, e+24+68)[0], '(2 = GUI, 3 = console)')
EOF

# checksums published with each release
sha256sum -c dist/SHA256SUMS.txt
```

## Reproducible builds

```bash
go build -trimpath -ldflags "-s -w -X main.Version=1.0.0" ./cmd/superidm
```

`-trimpath` strips local filesystem paths, so two people building the same
commit with the same Go version get identical output. Pin `Version` explicitly
when comparing.

## Regenerating icons

All artwork is code — the gradient tile with the download arrow, the six-size
`.ico` for Explorer and the tray, and the four PNG sizes for the extension:

```bash
python3 tools/make_icons.py
```

Outputs:

```
chrome-extension/icons/icon{16,32,48,128}.png   extension toolbar icons
assets/superidm.ico                            Explorer icon (16→256 px)
assets/superidm-{256,512}.png                   README / store artwork
internal/app/trayicon_windows.go                ICO bytes embedded in the exe
```

## Packaging the extension

Chrome requires `manifest.json` at the **root** of the archive when you upload a
zip to the Web Store, so `build.sh` verifies exactly that:

```bash
cd chrome-extension && zip -r ../SuperIDM-chrome-extension.zip . -x '*.DS_Store'
```

For local development you don't need a zip at all: `chrome://extensions` →
Developer mode → **Load unpacked** → pick the `chrome-extension` folder.

## Continuous integration

`.github/workflows/build.yml` builds Windows binaries and the extension zip on
every push, runs the test suite, and attaches everything to a GitHub Release
when you push a tag:

```bash
git tag v1.1.0 && git push --tags
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `go: cannot find main module` | run from the repository root (where `go.mod` lives) |
| Window never appears | The WebView2 runtime is missing; the app then opens the UI in your default browser. Install the Evergreen Runtime, or run with `--headless` and open the printed URL. |
| Antivirus flags the exe | Normal for an unsigned, freshly built binary — it is a self-extracting-looking single file. Add an exclusion or sign the binary. |
| SmartScreen blocks it | *More info → Run anyway*, or sign it. There is no paid certificate in this project. |
| Extension says "SuperIDM is not running" | Start `SuperIDM.exe`. The extension talks to it over `http://127.0.0.1:8765`. |
| Port 8765 already in use | The app automatically tries 8766…8774; it also prints the port it settled on. Set the same port in the extension's options. |
