# futr

A password-encrypted build of the portfolio model page, safe to publish on
GitHub Pages out of a public repo.

```
src/index.html   the real page   — gitignored, never committed
index.html       what ships      — encrypted, committed
build.py         encrypt/decrypt + build-time data capture
```

## Build

```sh
uv sync
uv run build.py                 # src/index.html -> index.html
uv run build.py --no-bake       # skip the live-data refresh
uv run build.py --decrypt       # index.html -> src/index.html
```

The password is read from `$FUTR_PASSWORD`, or prompted for if that's unset.
Edit `src/index.html`, re-run `build.py`, commit `index.html`.

**`src/` is gitignored on purpose.** Committing it to a public repo would
publish everything the encryption exists to hide. If you lose it, `--decrypt`
rebuilds it from the committed artifact — that's the only backup, so don't
forget the password.

## How the encryption works

The page is gzipped, then encrypted with **AES-256-GCM** under a key derived
from the password by **PBKDF2-HMAC-SHA256, 600,000 iterations**, with a fresh
random salt and IV per build. Ciphertext, salt and IV are base64'd into a small
unlock shell. The browser reverses it with WebCrypto and swaps the real document
in via `document.write`.

Nothing about the real page survives in the published file — not the title, not
the numbers, not the structure. GCM authenticates the ciphertext, so a wrong
password fails the tag check instead of returning garbage.

`crypto.subtle` requires a **secure context**, which means this works on
`https://` and on `http://localhost`, and *not* on a `file://` page opened
straight off disk. The shell says so rather than failing silently.

The derived key (not the password) is cached in `sessionStorage`, so a reload
doesn't re-prompt. It dies with the tab, and every build's new salt invalidates it.

### What this does and doesn't protect against

It genuinely protects the contents from anyone browsing the repo or the URL —
they get an opaque blob. What it can't do is protect against someone who has the
password, or against an **offline brute-force**: the file is public, so an
attacker can guess at it locally, forever, with no rate limit. 600k PBKDF2
iterations is the only thing slowing that down.

## Live data

Three of the page's original sources were dead, and all of them for the same
reason: **no CORS headers**. Yahoo, Stooq and FRED all refuse cross-origin reads
from a static page, so every request failed at the preflight rather than the
parse — which is why the status lines sat on "Checking…" forever. The public
CORS proxies they fell back to (allorigins, codetabs, corsproxy.io) are down or
rate-limited.

An API key does **not** fix this. FRED's keyed API (`api.stlouisfed.org`) sends
no CORS headers either — the block is at the origin, not the auth layer.

So:

- **Price** comes from `stockanalysis.com`, which does send CORS and needs no
  key, with Yahoo-via-`r.jina.ai` as a fallback.
- **Inflation** is fetched at *build time* by `build.py`, server-side, where CORS
  doesn't apply, and baked into the page between the `BAKED:START/END` markers.
  The page still tries live FRED first and only falls back to the baked numbers,
  labelling them "captured when this page was built" when it does.

A build with no network leaves the previous `BAKED` block alone rather than
blanking it.

## Dates

The departure slider's floor is computed from the **viewing** date, not a baked
constant: tranches whose month has already passed are treated as banked, and the
slider won't let you choose to leave in the past. The banner text states which
tranches have landed. This keeps working as time passes without a rebuild.

"Reset to sheet defaults" still restores the source sheet's own values —
including its $400k living expenses and $109.30 price — so the "reproduces the
sheet row-for-row" claim stays checkable. The page's *load* default for living
expenses is $500k.
