#!/usr/bin/env bash
#
# Proves the point with numbers: downloads the same file three ways against a
# local origin that emulates a real (shaped, high-latency) link, and prints a
# comparison table.
#
#   1. a single connection   - what a browser or wget does
#   2. 8 connections         - Internet Download Manager's default
#   3. 64 connections        - SuperIDM's default
#
# Usage:  bash tools/benchmark.sh [SIZE_MIB] [PER_CONN_MIBPS]

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SIZE="${1:-64}"
PER_CONN="${2:-2}"
ORIGIN="127.0.0.1:8799"
API="127.0.0.1:8791"
WORK="${TMPDIR:-/tmp}/superidm-bench"
mkdir -p "$WORK"

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
command -v go >/dev/null || { echo "Go is required" >&2; exit 1; }

say "Building the test origin and the app"
go build -o "$WORK/origin" ./tools/benchserver
go build -o "$WORK/superidm" ./cmd/superidm

say "Starting the origin (${SIZE} MiB payload, ${PER_CONN} MiB/s per connection)"
"$WORK/origin" -addr "$ORIGIN" -size-mib "$SIZE" -single-mibps "$PER_CONN" -per-conn-mibps "$PER_CONN" >"$WORK/origin.log" 2>&1 &
ORIGIN_PID=$!
say "Starting SuperIDM (headless API on $API)"
HOME="$WORK/home" "$WORK/superidm" --headless --port 8791 >"$WORK/superidm.log" 2>&1 &
APP_PID=$!
cleanup() { kill "$ORIGIN_PID" "$APP_PID" 2>/dev/null || true; }
trap cleanup EXIT

for _ in $(seq 1 50); do
  curl -sf "http://$ORIGIN/health" >/dev/null && break || sleep 0.2
done
for _ in $(seq 1 50); do
  curl -sf "http://$API/api/health" >/dev/null && break || sleep 0.2
done
sleep 0.5

mkdir -p "$WORK/dl"
rm -f "$WORK/single.bin"

python3 - "$SIZE" "$PER_CONN" "$ORIGIN" "$API" "$WORK" <<'PY'
import hashlib, json, os, subprocess, sys, time, urllib.request

size_mib = float(sys.argv[1])
single = float(sys.argv[2])
origin, api, work = sys.argv[3], sys.argv[4], sys.argv[5]
SRC = f"http://{origin}/file.bin"
API = f"http://{api}"

def req(method, path, body=None):
    r = urllib.request.Request(API + path,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Content-Type": "application/json"}, method=method)
    return json.loads(urllib.request.urlopen(r).read())

def run_job(conns, name):
    req("POST", "/api/settings", {**req("GET", "/api/settings")["settings"],
                                  "connections": conns, "maxConcurrent": 3})
    job = req("POST", "/api/jobs", {"url": SRC, "dir": work + "/dl", "fileName": name,
                                    "connections": conns, "overwrite": True})
    while True:
        j = req("GET", "/api/jobs/" + job["id"])
        if j["state"] in ("completed", "error", "canceled"):
            break
        time.sleep(0.05)
    if j["state"] != "completed":
        raise SystemExit(f"{name} failed: {j.get('error')}")
    seconds = (j["finishedAt"] - j["startedAt"]) / 1000.0
    return seconds, j, j["savePath"]

print()
print("=" * 74)
print(f"  SuperIDM benchmark - {size_mib:.0f} MiB over an emulated shaped link")
print(f"  (12 ms latency per request, every connection shaped to {single:.0f} MiB/s)")
print("=" * 74)

# 1. single connection via curl
out = work + "/single.bin"
t0 = time.time()
subprocess.run(["curl", "-s", "-o", out, SRC], check=True)
t1 = time.time()
rows = [("single connection (curl / browser)", t1 - t0, "")]

# 2 & 3. SuperIDM
for conns, label in ((8, "SuperIDM, 8 connections (IDM default)"),
                     (64, "SuperIDM, 64 connections (default)")):
    secs, j, path = run_job(conns, f"conn{conns}.bin")
    rows.append((label, secs, f"{j['segments']} segments, {j['retries']} retries"))

base = rows[0][1]
print()
print(f"  {'method':<42}{'time':>9}{'speed':>13}{'vs single':>11}")
print("  " + "-" * 72)
for label, secs, note in rows:
    speed = size_mib / secs
    print(f"  {label:<42}{secs:>8.2f}s{speed:>10.1f} MiB/s{base/secs:>10.1f}x")
    if note:
        print(f"  {'':<42}{note}")
print()

idm = rows[1][1]
new = rows[2][1]
print(f"  SuperIDM default vs IDM default (8 conns): {idm/new:.2f}x faster")
print(f"  SuperIDM default vs a single connection  : {base/new:.1f}x faster")

h = lambda p: hashlib.sha256(open(p, "rb").read()).hexdigest()
single_h = h(out)
checks = [h(work + f"/dl/conn{c}.bin") for c in (8, 64)]
print()
print(f"  integrity: all three files identical -> {all(x == single_h for x in checks)}")
print("=" * 74)
PY
