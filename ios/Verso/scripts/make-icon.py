#!/usr/bin/env python3
"""Render the placeholder Verso app icon (1024px PNGs) with no dependencies.

Paper square, bottom-right corner folded over to show the oxblood verso underneath,
with a gilt "II" embossed on the fold. Usage: python3 scripts/make-icon.py
"""
import os, struct, zlib

S = 1024
OUT = os.path.join(os.path.dirname(__file__), "..", "App", "Assets.xcassets", "AppIcon.appiconset")

def hexrgb(h): return ((h >> 16) & 255, (h >> 8) & 255, h & 255)

# Seven-segment-free "II": two serif bars, coordinates in fold space.
def in_numeral(x, y, cx, cy, h):
    w, gap, serif = h * 0.09, h * 0.42, h * 0.07
    for bx in (cx - gap / 2 - w / 2, cx + gap / 2 + w / 2):
        if abs(x - bx) <= w / 2 and abs(y - cy) <= h / 2: return True
        if abs(x - bx) <= w / 2 + serif and (abs(y - (cy - h / 2)) <= h * 0.035 or abs(y - (cy + h / 2)) <= h * 0.035): return True
    return False

def render(paper, verso, fold, gilt, shadow):
    F = int(S * 0.40)          # fold leg length
    rows = []
    for y in range(S):
        row = bytearray([0])
        for x in range(S):
            c = paper
            # region past the fold line x + y > 2S - F is the corner
            d = (x + y) - (2 * S - F)
            if d > 0:
                c = verso                       # the verso showing underneath
            # The folded flap: mirror of the corner across the fold line, lying on the paper
            mx, my = (2 * S - F) - y, (2 * S - F) - x   # reflect across x + y = 2S - F
            if d <= 0 and mx >= S - F and my >= S - F and mx < S and my < S:
                c = fold
                # gilt numeral centred on the flap
                cx = cy = S - F * 2 / 3            # flap centroid
                if in_numeral(x, y, cx, cy, F * 0.30): c = gilt
                if -10 < d <= 0: c = shadow
            row += bytes(c)
        rows.append(bytes(row))
    raw = b"".join(rows)
    def chunk(t, data): return struct.pack(">I", len(data)) + t + data + struct.pack(">I", zlib.crc32(t + data) & 0xffffffff)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", S, S, 8, 2, 0, 0, 0)) + \
        chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b"")

light = render(hexrgb(0xF2ECDF), hexrgb(0x7B2121), hexrgb(0xE6DDCB), hexrgb(0xB08D3C), hexrgb(0xCDBFA8))
dark = render(hexrgb(0x141210), hexrgb(0xD4673F), hexrgb(0x1F1C18), hexrgb(0xC9A55A), hexrgb(0x3A322B))
tinted = render((40, 40, 40), (230, 230, 230), (70, 70, 70), (250, 250, 250), (20, 20, 20))
for name, data in (("icon-light.png", light), ("icon-dark.png", dark), ("icon-tinted.png", tinted)):
    with open(os.path.join(OUT, name), "wb") as f: f.write(data)
print("wrote icons to", os.path.abspath(OUT))
