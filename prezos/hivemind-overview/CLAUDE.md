# HiveMind Overview Deck

Slide deck on HiveMind, an internal tool for capturing how engineers use coding agents. Linked from the repo-root `index.html`.

## Files

- `HiveMind.html` — the deck. Single-file: embedded `<style>`, slides as `<section>` children of `<deck-stage>`, speaker notes as a JSON array. **Edit this.**
- `deck-stage.js` — `<deck-stage>` custom element (slide nav, keyboard, fullscreen). Shared framework. **Don't edit unless changing the framework itself**, and if you do, the same file lives in other decks under `prezos/` — keep them in sync.
- `assets/`, `uploads/` — images and embedded media. All paths in the HTML are relative.

## Slide editing

- Slides are `<section>` elements inside `<deck-stage>`. Each slide is self-contained — its layout lives in classes on its own elements, not in shared rules. Copy a nearby slide's structure when adding a new one.
- **Speaker notes:** `<script type="application/json" id="speaker-notes">` holds an array of strings, **one per slide, in slide order**. When you add/remove/reorder a slide, update this array in lockstep or the notes will desync.
- Design tokens are CSS variables on `:root` (OKLCH). Reuse them — don't hardcode hex.
- Fonts are Inter + JetBrains Mono from Google Fonts. The `.mono` class switches to JetBrains Mono.
- Slide chrome: `.chrome.dark` and `.chrome.paper` are the two base backgrounds.

## Previewing

Open `HiveMind.html` directly in a browser, or serve the repo root with any static server (`python -m http.server`, etc.) and visit `/prezos/hivemind-overview/HiveMind.html`. Don't auto-open browsers or take screenshots unless the user asks — the source is the source of truth.

## Claude Design bundles

If the user pulls a fresh export from claude.ai/design into `scratch/`, treat it as a reference, not a sync source. Only `HiveMind.html` is worth diffing — `deck-stage.js`, `assets/`, and `uploads/` are managed here. Ignore the bundle's `HiveMind - Standalone.html`, `HiveMind-print.html`, and `screenshots/` — those are export artifacts, not canonical.
