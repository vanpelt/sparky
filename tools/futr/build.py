#!/usr/bin/env python3
"""Encrypt a static HTML page so it can be published somewhere public.

    uv run build.py                 # src/index.html -> index.html (encrypted)
    uv run build.py --decrypt       # index.html -> src/index.html (recover)

The page is gzipped, encrypted with AES-256-GCM under a key derived from the
password via PBKDF2-HMAC-SHA256, and base64'd into a small unlock shell. The
browser reverses that with WebCrypto and swaps the real document in.

Only the encrypted index.html is meant to be committed. The plaintext under
src/ is gitignored — if you lose it, --decrypt gets it back.
"""

import argparse
import base64
import getpass
import gzip
import json
import os
import re
import secrets
import sys
from pathlib import Path

from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.hashes import SHA256
from cryptography.hazmat.primitives.kdf.pbkdf2 import PBKDF2HMAC

HERE = Path(__file__).parent
PLAIN = HERE / "src" / "index.html"
ENCRYPTED = HERE / "index.html"

# PBKDF2 work factor. This is the only thing standing between a public copy of
# the file and an offline brute-force, so it is deliberately expensive: ~0.4s on
# a laptop, ~1.5s on an old phone. Raise it if you don't mind a slower unlock.
ITERATIONS = 600_000

B64 = lambda b: base64.b64encode(b).decode()


def derive(password: str, salt: bytes, iterations: int) -> bytes:
    return PBKDF2HMAC(
        algorithm=SHA256(), length=32, salt=salt, iterations=iterations
    ).derive(password.encode())


def encrypt(password: str, bake: bool = True) -> None:
    if not PLAIN.exists():
        sys.exit(f"no plaintext at {PLAIN} — run with --decrypt to recover it")

    if bake:
        bake_live_data()

    raw = PLAIN.read_bytes()
    packed = gzip.compress(raw, 9, mtime=0)  # mtime=0 keeps the build reproducible
    salt, iv = secrets.token_bytes(16), secrets.token_bytes(12)
    ct = AESGCM(derive(password, salt, ITERATIONS)).encrypt(iv, packed, None)

    payload = {
        "v": 1,
        "iter": ITERATIONS,
        "salt": B64(salt),
        "iv": B64(iv),
        "ct": B64(ct),
    }
    shell = SHELL.replace("__PAYLOAD__", json.dumps(payload, separators=(",", ":")))
    ENCRYPTED.write_text(shell)

    print(f"  plaintext   {len(raw):>9,} bytes")
    print(f"  gzipped     {len(packed):>9,} bytes")
    print(f"  published   {ENCRYPTED.stat().st_size:>9,} bytes  -> {ENCRYPTED}")


# ── build-time data capture ─────────────────────────────────────────────────
# The page can't reach any of this from a browser: Yahoo, Stooq and FRED serve
# no CORS headers, and a FRED API key doesn't change that — the block is at the
# origin, not the auth layer. Fetching here instead, where CORS doesn't exist,
# means the published page always opens with recent numbers.
# An honest identifying UA, and not a spoofed browser one: FRED hangs up on
# anything claiming to be Chrome, and StockAnalysis 403s a bare urllib.
UA = {"User-Agent": "futr-build/0.1 (+https://github.com/vanpelt/sparky)"}
FRED_CSV = "https://fred.stlouisfed.org/graph/fredgraph.csv?id={}"


def _get(url: str, timeout: int = 20) -> str:
    import urllib.request
    req = urllib.request.Request(url, headers=UA)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read().decode("utf-8", "replace")


def _fred(series_id: str):
    """[(date, value), …] newest last, or None. FRED marks gaps with '.'."""
    try:
        rows = _get(FRED_CSV.format(series_id)).strip().splitlines()[1:]
    except Exception as e:
        print(f"    {series_id:<9} unreachable ({type(e).__name__})")
        return None
    out = []
    for line in rows:
        parts = line.split(",")
        if len(parts) >= 2 and parts[1].strip() not in (".", ""):
            try:
                out.append((parts[0].strip(), float(parts[1])))
            except ValueError:
                pass
    return out or None


def _ticker() -> str | None:
    """Read the symbol out of the page rather than hardcoding it here.

    src/ is gitignored but this file is published, so naming the ticker in it
    would tell anyone browsing the repo what the encrypted page is about —
    which is most of what the encryption is for.
    """
    m = re.search(r"stockanalysis\.com/api/quotes/s/([A-Za-z.\-]+)/", PLAIN.read_text())
    return m.group(1).upper() if m else None


def bake_live_data() -> None:
    """Fetch the price + inflation series and rewrite the BAKED block in place."""
    print("baking live data:")
    baked = {"px": None, "infl": None, "at": None}

    sym = _ticker()
    if not sym:
        print("    price     no symbol found in the page — skipping")
    else:
        try:
            q = json.loads(_get(f"https://stockanalysis.com/api/quotes/s/{sym}/"))["data"]
            baked["px"] = {
                "price": float(q["p"]),
                "ts": int(float(q["ts"])),
                "name": f"Built-in ({q.get('u', 'at build time')})",
                "rank": 99,
            }
            print(f"    {sym:<9} ${q['p']}  ({q.get('u', '')})")
        except Exception as e:
            print(f"    {sym:<9} unreachable ({type(e).__name__})")

    infl = {}
    b30, b10, cpi = (_fred(i) for i in ("T30YIEM", "T10YIE", "CPIAUCSL"))
    if b30:
        infl["b30"] = {"v": b30[-1][1], "date": b30[-1][0], "baked": True,
                       "label": "30-yr breakeven",
                       "note": "market-implied, matches this model's horizon"}
    if b10:
        infl["b10"] = {"v": b10[-1][1], "date": b10[-1][0], "baked": True,
                       "label": "10-yr breakeven",
                       "note": "market-implied, updated daily"}
    if cpi and len(cpi) > 12:
        infl["cpi"] = {"v": (cpi[-1][1] / cpi[-13][1] - 1) * 100, "date": cpi[-1][0],
                       "baked": True, "label": "Trailing CPI",
                       "note": "realised over the last 12 months"}
    for k, d in infl.items():
        print(f"    {d['label']:<16} {d['v']:.2f}%  as of {d['date']}")
    baked["infl"] = infl or None

    # A build with no network shouldn't silently strip numbers a previous one won.
    page = PLAIN.read_text()
    if baked["px"] is None and baked["infl"] is None:
        print("    nothing fetched — leaving the existing BAKED block alone")
        return

    baked["at"] = _get_utc_date()
    block = (f"/* BAKED:START — rewritten by build.py on every build; don't hand-edit */\n"
             f"const BAKED = {json.dumps(baked, separators=(',', ':'))};\n"
             f"/* BAKED:END */")
    new, n = re.subn(r"/\* BAKED:START.*?/\* BAKED:END \*/", block, page, flags=re.S)
    if n != 1:
        sys.exit("couldn't find exactly one BAKED block in src/index.html")
    PLAIN.write_text(new)


def _get_utc_date() -> str:
    from datetime import datetime, timezone
    return datetime.now(timezone.utc).strftime("%Y-%m-%d")


def decrypt(password: str) -> None:
    if not ENCRYPTED.exists():
        sys.exit(f"nothing to decrypt at {ENCRYPTED}")

    m = re.search(r"const PAYLOAD\s*=\s*(\{.*?\});", ENCRYPTED.read_text(), re.S)
    if not m:
        sys.exit(f"{ENCRYPTED} doesn't look like a build.py artifact")
    p = json.loads(m.group(1))

    d = lambda k: base64.b64decode(p[k])
    key = derive(password, d("salt"), p["iter"])
    try:
        packed = AESGCM(key).decrypt(d("iv"), d("ct"), None)
    except Exception:
        sys.exit("wrong password")

    PLAIN.parent.mkdir(parents=True, exist_ok=True)
    PLAIN.write_bytes(gzip.decompress(packed))
    print(f"recovered {PLAIN}")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--decrypt", action="store_true", help="recover src/index.html")
    ap.add_argument("--no-bake", action="store_true",
                    help="skip the build-time price/inflation refresh")
    args = ap.parse_args()

    # Env var keeps the password out of shell history; otherwise prompt.
    password = os.environ.get("FUTR_PASSWORD") or getpass.getpass("Password: ")
    if not password:
        sys.exit("no password given")

    decrypt(password) if args.decrypt else encrypt(password, bake=not args.no_bake)


# ── the unlock shell ────────────────────────────────────────────────────────
# Everything the served page reveals: no title, no description, no structure.
SHELL = r"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>Protected</title>
<style>
:root{
  color-scheme: light;
  --surface-1:#fcfcfb; --page:#f9f9f7;
  --text-primary:#0b0b0b; --text-secondary:#52514e; --muted:#898781;
  --border:rgba(11,11,11,0.10); --field:#ffffff;
  --accent:#2a78d6; --crit:#d03b3b;
}
@media (prefers-color-scheme: dark){
  :root{
    color-scheme: dark;
    --surface-1:#1a1a19; --page:#0d0d0d;
    --text-primary:#ffffff; --text-secondary:#c3c2b7; --muted:#898781;
    --border:rgba(255,255,255,0.10); --field:#0d0d0d;
    --accent:#3987e5; --crit:#d03b3b;
  }
}
*{box-sizing:border-box}
body{
  margin:0; min-height:100vh; background:var(--page); color:var(--text-primary);
  font-family:system-ui,-apple-system,"Segoe UI",sans-serif; font-size:15px; line-height:1.5;
  display:flex; align-items:center; justify-content:center; padding:24px;
  -webkit-text-size-adjust:100%;
}
.card{
  background:var(--surface-1); border:1px solid var(--border); border-radius:12px;
  padding:26px 24px; width:100%; max-width:360px;
}
.mark{width:22px; height:22px; display:block; margin-bottom:14px; color:var(--muted)}
h1{font-size:16px; margin:0 0 3px; letter-spacing:-0.005em}
p.note{color:var(--text-secondary); font-size:13px; margin:0 0 18px}
label{display:block; font-size:12.5px; color:var(--text-secondary); margin-bottom:6px}
input{
  width:100%; font:inherit; font-size:15px; color:var(--text-primary); background:var(--field);
  border:1px solid var(--border); border-radius:8px; padding:9px 11px;
}
input:focus{outline:2px solid var(--accent); outline-offset:-1px; border-color:transparent}
button{
  width:100%; margin-top:12px; font:inherit; font-size:14px; font-weight:500;
  background:var(--accent); color:#fff; border:0; border-radius:8px;
  padding:10px 14px; cursor:pointer;
}
button:disabled{opacity:0.55; cursor:default}
.msg{font-size:12.5px; margin:12px 0 0; min-height:1.3em}
.msg.err{color:var(--crit)}
.msg.busy{color:var(--text-secondary)}
</style>
</head>
<body>
<main class="card">
  <svg class="mark" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6"
       stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <rect x="4" y="10.5" width="16" height="10.5" rx="2"></rect>
    <path d="M8 10.5V7a4 4 0 0 1 8 0v3.5"></path>
  </svg>
  <h1>This page is encrypted</h1>
  <p class="note">Enter the password to decrypt it in your browser.</p>

  <form id="f" autocomplete="on">
    <label for="pw">Password</label>
    <input id="pw" type="password" autocomplete="current-password" autofocus
           spellcheck="false" required>
    <button id="go" type="submit">Unlock</button>
  </form>
  <p class="msg" id="msg" role="status" aria-live="polite"></p>
</main>

<script>
/* Everything here lives inside an IIFE on purpose. document.open()/write()
   replaces the document but NOT the JavaScript realm, so any top-level
   `const` declared out here survives into the decrypted page — and the page
   declares its own `$`, which made the whole model script die at parse time
   with "Identifier '$' has already been declared". Leak no globals. */
(() => {
"use strict";
const PAYLOAD = __PAYLOAD__;

const $ = id => document.getElementById(id);
const b64 = s => Uint8Array.from(atob(s), c => c.charCodeAt(0));
const KEY_CACHE = "futr.k.v1";   // survives a reload, dies with the tab

function say(text, cls){ const m = $("msg"); m.textContent = text; m.className = "msg " + (cls||""); }

/* WebCrypto only exists in a secure context: https:// or http://localhost.
   Opening this file straight off disk (file://) is neither. */
if (!(window.crypto && crypto.subtle)) {
  $("f").hidden = true;
  say("This page needs to be served over https:// or from localhost — " +
      "opening the file directly can't decrypt it.", "err");
}

/* sessionStorage is absent in some embedded/sandboxed viewers, so never assume. */
const cache = {
  get(){ try { return sessionStorage.getItem(KEY_CACHE); } catch(e){ return null; } },
  set(v){ try { sessionStorage.setItem(KEY_CACHE, v); } catch(e){} },
  clear(){ try { sessionStorage.removeItem(KEY_CACHE); } catch(e){} }
};

async function keyFromPassword(pw){
  const base = await crypto.subtle.importKey(
    "raw", new TextEncoder().encode(pw), "PBKDF2", false, ["deriveKey"]);
  return crypto.subtle.deriveKey(
    { name:"PBKDF2", salt:b64(PAYLOAD.salt), iterations:PAYLOAD.iter, hash:"SHA-256" },
    base, { name:"AES-GCM", length:256 }, true, ["decrypt"]);
}

/* Throws if the password is wrong — GCM authenticates the ciphertext, so a bad
   key fails the tag check rather than handing back garbage. */
async function decryptPage(key){
  if (typeof DecompressionStream !== "function") throw new Error("unsupported-browser");
  const packed = await crypto.subtle.decrypt(
    { name:"AES-GCM", iv:b64(PAYLOAD.iv) }, key, b64(PAYLOAD.ct));
  const stream = new Blob([packed]).stream()
    .pipeThrough(new DecompressionStream("gzip"));
  return new Response(stream).text();
}

/* Last thing anyone does with this document: open() wipes it, so every side
   effect worth keeping (the key cache) has to be committed before we get here.
   The payload is a whole document whose script is a plain inline block at the
   end of <body>, so re-parsing it runs it exactly as if it had been served. */
function swap(html){
  document.open();
  document.write(html);
  document.close();
}

/* A cached key means a reload shouldn't re-prompt or re-run PBKDF2. */
(async () => {
  const saved = cache.get();
  if (!saved || !crypto.subtle) return;
  try {
    const key = await crypto.subtle.importKey(
      "raw", b64(saved), { name:"AES-GCM", length:256 }, true, ["decrypt"]);
    say("Decrypting…", "busy");
    swap(await decryptPage(key));
  } catch(e) {
    cache.clear();
    say("");
  }
})();

$("f").addEventListener("submit", async (e) => {
  e.preventDefault();
  const pw = $("pw").value;
  if (!pw) return;

  $("go").disabled = true;
  say("Decrypting…", "busy");
  /* Yield once so "Decrypting…" paints before the key derivation starts.
     Deliberately not requestAnimationFrame: a hidden or background tab never
     fires one, which would wedge the unlock here forever. */
  await new Promise(r => setTimeout(r, 0));

  try {
    const key = await keyFromPassword(pw);
    const html = await decryptPage(key);          // fails here if the password is wrong
    const raw = await crypto.subtle.exportKey("raw", key);
    cache.set(btoa(String.fromCharCode(...new Uint8Array(raw))));
    swap(html);
  } catch(err) {
    $("go").disabled = false;
    $("pw").select();
    say(err && err.message === "unsupported-browser"
      ? "This browser is too old to decompress the page."
      : "Wrong password.", "err");
  }
});
})();
</script>
</body>
</html>
"""

if __name__ == "__main__":
    main()
