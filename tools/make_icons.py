#!/usr/bin/env python3
"""Generate every SuperIDM icon from code.

The project intentionally has no binary art dependencies: the app icon, the
Windows .ico for the tray and the browser-extension PNGs are all rasterised
here with nothing but the Python standard library (zlib + struct), so the
repository stays fully reproducible and auditable.

Usage:  python3 tools/make_icons.py
"""

import os
import struct
import zlib

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Brand palette (matches the UI stylesheet).
GRAD_A = (0x4F, 0x8C, 0xFF)
GRAD_B = (0x7C, 0x5C, 0xFF)
SS = 4  # supersampling factor for anti-aliasing


def lerp(a, b, t):
    return tuple(int(round(a[i] + (b[i] - a[i]) * t)) for i in range(3))


def rounded_square_coverage(x, y, size, radius):
    """Distance-based coverage test for a rounded square."""
    r = radius
    cx = min(max(x, r), size - r)
    cy = min(max(y, r), size - r)
    if x >= r and x <= size - r:
        return True
    if y >= r and y <= size - r:
        return True
    dx, dy = x - cx, y - cy
    return dx * dx + dy * dy <= r * r


def arrow_coverage(x, y, size):
    """The white download glyph: stem, chevron head and a baseline bar."""
    u = x / size
    v = y / size

    # Baseline bar near the bottom (with a clear gap under the arrow head).
    if 0.695 <= v <= 0.805 and 0.255 <= u <= 0.745:
        return True

    # Vertical stem.
    if 0.442 <= u <= 0.558 and 0.135 <= v <= 0.44:
        return True

    # Arrow head: a triangle whose tip sits just above the baseline bar.
    tip_v, apex_v = 0.665, 0.345
    if apex_v <= v <= tip_v:
        half = 0.245 * (tip_v - v) / (tip_v - apex_v)
        if 0.500 - half <= u <= 0.500 + half:
            return True

    return False


def render(size):
    """Return an RGBA byte buffer for one icon size (premultiplied not needed)."""
    big = size * SS
    # Accumulate into a float canvas at supersampled resolution.
    px = [[(0, 0, 0, 0)] * size for _ in range(size)]
    radius = big * 0.22

    for oy in range(size):
        for ox in range(size):
            r_sum = g_sum = b_sum = a_sum = 0
            for sy in range(SS):
                for sx in range(SS):
                    x = ox * SS + sx + 0.5
                    y = oy * SS + sy + 0.5
                    if not rounded_square_coverage(x, y, big, radius):
                        continue
                    # Diagonal gradient across the tile.
                    t = (x / big + y / big) / 2.0
                    base = lerp(GRAD_A, GRAD_B, t)
                    # Slight top-left highlight.
                    hl = max(0.0, 1.0 - (x / big * 0.8 + y / big * 1.2))
                    base = lerp(base, (255, 255, 255), hl * 0.18)
                    if arrow_coverage(x, y, big):
                        col = (255, 255, 255)
                    else:
                        col = base
                    r_sum += col[0]
                    g_sum += col[1]
                    b_sum += col[2]
                    a_sum += 255
            n = SS * SS
            if a_sum == 0:
                px[oy][ox] = (0, 0, 0, 0)
            else:
                # Un-premultiply so edges keep their colour against any background.
                cover = a_sum / n
                px[oy][ox] = (
                    int(round(r_sum / (a_sum / 255))),
                    int(round(g_sum / (a_sum / 255))),
                    int(round(b_sum / (a_sum / 255))),
                    int(round(cover)),
                )
    for row in px:
        for c in row:
            assert len(c) == 4
    return px


def png_bytes(px, size):
    raw = bytearray()
    for y in range(size):
        raw.append(0)  # filter: none
        for x in range(size):
            r, g, b, a = px[y][x]
            raw += bytes((r & 255, g & 255, b & 255, a & 255))

    def chunk(tag, data):
        c = struct.pack(">I", len(data)) + tag + data
        return c + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)

    ihdr = struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr)
            + chunk(b"IDAT", zlib.compress(bytes(raw), 9)) + chunk(b"IEND", b""))


def ico_entries(size, px):
    """A BMP-format ICO entry (32bpp BGRA + AND mask), the most compatible."""
    header = struct.pack("<IiiHHIIiiII", 40, size, size * 2, 1, 32, 0, 0, 0, 0, 0, 0)
    # BITMAPINFOHEADER uses a bottom-up DIB.
    body = bytearray()
    for y in range(size - 1, -1, -1):
        for x in range(size):
            r, g, b, a = px[y][x]
            body += bytes((b & 255, g & 255, r & 255, a & 255))
    # AND mask: all zero (fully opaque) since we carry a real alpha channel.
    stride = ((size + 31) // 32) * 4
    body += bytes(stride * size)
    return header + bytes(body)


def write(path, data):
    if isinstance(data, str):
        data = data.encode("utf-8")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as fh:
        fh.write(data)
    print("wrote %-58s %7d bytes" % (os.path.relpath(path, ROOT), len(data)))


def main():
    cache = {}

    def px(size):
        if size not in cache:
            cache[size] = render(size)
        return cache[size]

    # --- browser extension PNGs -------------------------------------------
    for s in (16, 32, 48, 128):
        write(os.path.join(ROOT, "chrome-extension", "icons", f"icon{s}.png"),
              png_bytes(px(s), s))

    # --- standalone PNGs used by the web UI / README ----------------------
    write(os.path.join(ROOT, "assets", "superidm-256.png"), png_bytes(px(256), 256))
    write(os.path.join(ROOT, "assets", "superidm-512.png"), png_bytes(px(512), 512))

    # --- Windows .ico (multi size, BMP entries) ---------------------------
    sizes = [16, 32, 48, 64, 128, 256]
    images = [ico_entries(s, px(s)) for s in sizes]
    header = struct.pack("<HHH", 0, 1, len(images))
    offset = 6 + 16 * len(images)
    directory = b""
    for s, img in zip(sizes, images):
        w = 0 if s >= 256 else s
        directory += struct.pack("<BBBBHHII", w, w, 0, 0, 1, 32, len(img), offset)
        offset += len(img)
    ico = header + directory + b"".join(images)
    write(os.path.join(ROOT, "assets", "superidm.ico"), ico)

    # --- embed a small ICO in the Go tray code ----------------------------
    tray_sizes = [16, 32]
    tray_images = [ico_entries(s, px(s)) for s in tray_sizes]
    h = struct.pack("<HHH", 0, 1, len(tray_images))
    off = 6 + 16 * len(tray_images)
    d = b""
    for s, img in zip(tray_sizes, tray_images):
        d += struct.pack("<BBBBHHII", s, s, 0, 0, 1, 32, len(img), off)
        off += len(img)
    tray_ico = h + d + b"".join(tray_images)

    write(os.path.join(ROOT, "internal", "app", "trayicon_windows.go"), compose_go(tray_ico))


def compose_go(data):
    lines = [
        "// Code generated by tools/make_icons.py. DO NOT EDIT.",
        "// The tray icon is carried in the binary and turned into an HICON at",
        "// runtime with CreateIconFromResourceEx, so no Windows resource",
        "// compiler is needed to build SuperIDM.",
        "",
        "//go:build windows",
        "",
        "package app",
        "",
        "var trayIconICO = []byte{",
    ]
    for i in range(0, len(data), 16):
        chunk = data[i:i + 16]
        lines.append("\t" + " ".join("0x%02x," % b for b in chunk))
    lines.append("}")
    lines.append("")
    return "\n".join(lines)


if __name__ == "__main__":
    main()
