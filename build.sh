#!/usr/bin/env bash
#
# Build SuperIDM for Windows from Linux, macOS or WSL.
#
# Produces, in ./dist:
#   SuperIDM.exe             the desktop app (no console window)
#   SuperIDM-cli.exe         the same engine as a console tool for scripting
#   SuperIDM-chrome-extension-vX.Y.Z.zip    load-unpacked / store upload
#   SuperIDM-vX.Y.Z-windows-x64.zip         everything, ready to hand out
#   SHA256SUMS.txt
#
# Requirements: Go 1.22+ (no CGO, no resource compiler, no NSIS).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 1.0.0)}"
VERSION="${VERSION#v}"
OUT="${OUT:-dist}"

LDFLAGS_COMMON="-s -w -X main.Version=${VERSION}"
BUILD_FLAGS=(-trimpath)

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

command -v go >/dev/null 2>&1 || { echo "Go is required: https://go.dev/dl/" >&2; exit 1; }
say "Go $(go version | awk '{print $3}')"

mkdir -p "$OUT"
rm -f "$OUT/SuperIDM.exe" "$OUT/SuperIDM-cli.exe"

# ---------------------------------------------------------------------------
# 1. Windows binaries
# ---------------------------------------------------------------------------
say "Building SuperIDM.exe (Windows GUI, x64)"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build "${BUILD_FLAGS[@]}" \
  -ldflags "${LDFLAGS_COMMON} -H=windowsgui" \
  -o "$OUT/SuperIDM.exe" ./cmd/superidm

say "Building SuperIDM-cli.exe (Windows console, x64)"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build "${BUILD_FLAGS[@]}" \
  -ldflags "${LDFLAGS_COMMON}" \
  -o "$OUT/SuperIDM-cli.exe" ./cmd/superidm

# ARM64 Windows is increasingly common (Surface, Snapdragon X); the engine is
# pure Go so this is a free extra build.
if [[ "${SKIP_ARM64:-0}" != "1" ]]; then
  say "Building SuperIDM-arm64.exe (Windows GUI, arm64)"
  CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build "${BUILD_FLAGS[@]}" \
    -ldflags "${LDFLAGS_COMMON} -H=windowsgui" \
    -o "$OUT/SuperIDM-arm64.exe" ./cmd/superidm
fi

# ---------------------------------------------------------------------------
# 2. Logos / icons (only regenerate when missing, so the build is reproducible)
# ---------------------------------------------------------------------------
if [[ ! -f chrome-extension/icons/icon128.png || ! -f assets/superidm.ico ]]; then
  say "Generating icons"
  python3 tools/make_icons.py
fi

# ---------------------------------------------------------------------------
# 3. Browser extension package
# ---------------------------------------------------------------------------
EXT_ZIP="$OUT/SuperIDM-chrome-extension-${VERSION}.zip"
say "Packaging the browser extension -> $(basename "$EXT_ZIP")"
rm -f "$EXT_ZIP"
( cd chrome-extension && zip -qr "../$EXT_ZIP" . -x '*.DS_Store' -x '__MACOSX/*' )
# Chrome refuses extensions whose manifest.json is nested inside a folder.
python3 - "$EXT_ZIP" <<'PY'
import sys, zipfile
z = sys.argv[1]
with zipfile.ZipFile(z) as f:
    names = f.namelist()
assert 'manifest.json' in names, "manifest.json must be at the archive root"
print(f"    {len(names)} files, manifest.json at the root: OK")
PY

# ---------------------------------------------------------------------------
# 4. Release bundle
# ---------------------------------------------------------------------------
BUNDLE="$OUT/SuperIDM-v${VERSION}-windows-x64.zip"
say "Building release bundle -> $(basename "$BUNDLE")"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/SuperIDM"
cp "$OUT/SuperIDM.exe" "$OUT/SuperIDM-cli.exe" "$STAGE/SuperIDM/" 2>/dev/null || true
[[ -f "$OUT/SuperIDM-arm64.exe" ]] && cp "$OUT/SuperIDM-arm64.exe" "$STAGE/SuperIDM/"
cp -r chrome-extension "$STAGE/SuperIDM/"
cp LICENSE README.md "$STAGE/SuperIDM/" 2>/dev/null || true
cp -r docs "$STAGE/SuperIDM/" 2>/dev/null || true
cat > "$STAGE/SuperIDM/START-HERE.txt" <<'TXT'
SuperIDM
========

1. Run SuperIDM.exe                 (the desktop app opens)
2. Open the "Extension & setup" tab inside the app
3. In Chrome/Edge:  chrome://extensions  ->  enable "Developer mode"
   ->  "Load unpacked"  ->  select the chrome-extension folder next to this file
4. That's it. Downloads, videos and streams will now go through SuperIDM.

Command line:
   SuperIDM-cli.exe -d "https://example.com/big.iso" -n 64
   SuperIDM-cli.exe --inspect "https://example.com/big.iso"

Windows SmartScreen may warn about the unsigned build: "More info" -> "Run anyway".
TXT
( cd "$STAGE" && zip -qr "$OLDPWD/$BUNDLE" SuperIDM )

# ---------------------------------------------------------------------------
# 5. Checksums
# ---------------------------------------------------------------------------
say "Writing checksums"
( cd "$OUT" && sha256sum SuperIDM.exe SuperIDM-cli.exe "$(basename "$EXT_ZIP")" "$(basename "$BUNDLE")" \
    > SHA256SUMS.txt 2>/dev/null \
  || shasum -a 256 SuperIDM.exe SuperIDM-cli.exe "$(basename "$EXT_ZIP")" "$(basename "$BUNDLE")" > SHA256SUMS.txt )

say "Done. Artifacts in $OUT/"
ls -lh "$OUT" | sed 's/^/    /'
echo
say "Next: run a local smoke test with '$OUT/SuperIDM-cli.exe --help' (on Windows),"
say "or './dist/superidm-linux --headless' if you built the Linux binary."
