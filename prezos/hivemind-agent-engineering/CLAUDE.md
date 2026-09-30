# HiveMind — Agent Engineering Talk

20-minute expo-hall talk, "Agent engineering with a shared understanding of how teams build with AI". Forked from `../hivemind-overview` (same framework and tokens, new HiveMind mark). Linked from the repo-root `index.html`.

Placeholders still to fill are wrapped in `<span class="tk">` (amber, dashed underline): `grep -n 'class="tk"' index.html`.

## Files

- `index.html` — the deck markup. Slides are `<section>` children of `<deck-stage>`; speaker notes live in a JSON array. **Edit this for content.**
- `styles.css` — all deck CSS. Slide-specific styles are grouped by topic with `/* ── Section ─── */` comment banners. **Edit this for visual changes.**
- `deck-stage.js` — `<deck-stage>` custom element (slide nav, keyboard, fullscreen). Shared framework. **Don't edit unless changing the framework itself**, and if you do, the same file lives in other decks under `prezos/` — keep them in sync.
- `assets/` — images. All paths in the HTML are relative. Dashboard screenshots are demo-org (acme) data copied from agentstream-py.
- The HiveMind mark is defined once as `<mask id="hm-mask">` at the top of `<body>`; reuse it with `<svg class="hm-logo">` (color = `currentColor`). `assets/logo.svg` / `favicon.svg` are standalone copies.
- `analysis/hivemind_me_analysis.py` — the speaker's own `hivemind export --format parquet` (default `~/.hivemind/exports/me`) → `assets/charts/me.js` (charts for the "zoom in" slides 12–15) and `me.json` (every number quoted on them). Run with `uv run analysis/hivemind_me_analysis.py`; slide copy is hand-updated from `me.json`.
- `analysis/effort_reports.sql` — the "said vs did" slide (20): prod `effort_reports` (the in-product "how long without AI?" survey) joined to `sessions`, aggregates only. Run it from an agentstream-py checkout with the `clickhouse-query` skill; the results it was built from are in `assets/charts/effort.json`.
- `title.png` — the title slide rendered on its own, for the event organisers. Re-render it after changing slide 1.

## Slide editing

- Slides are `<section>` elements inside `<deck-stage>`. Each slide is self-contained — its layout lives in classes on its own elements, not in shared rules. Copy a nearby slide's structure when adding a new one.
- **Speaker notes:** `<script type="application/json" id="speaker-notes">` holds an array of strings, **one per slide, in slide order**. When you add/remove/reorder a slide, update this array in lockstep or the notes will desync.
- Design tokens are CSS variables on `:root` (OKLCH). Reuse them — don't hardcode hex.
- Fonts are Inter + JetBrains Mono from Google Fonts. The `.mono` class switches to JetBrains Mono.
- Slide chrome: `.chrome.dark` and `.chrome.paper` are the two base backgrounds.

## Previewing

Open `index.html` directly in a browser, or serve the repo root with any static server (`python -m http.server`, etc.) and visit `/prezos/hivemind-agent-engineering/`. Don't auto-open browsers or take screenshots unless the user asks — the source is the source of truth.
