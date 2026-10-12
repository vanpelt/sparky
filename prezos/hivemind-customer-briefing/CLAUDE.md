# HiveMind — Customer Briefing

Short (~5 minute) overview for prospective customers, shown before a live demo. Forked from `../hivemind-agent-engineering` (same framework, tokens and mark); the trust slide condenses `../hivemind-overview` slides 09–11, and "who it's for", security and review are simplified from its slides 05, 10 and 21. `assets/screencast.png` is the session-screencast viewer. Linked from the repo-root `index.html`.

The customer name is not in the source: open the deck with `?company=Acme` and the title slide shows "prepared for Acme"; it is remembered in localStorage (`hm-briefing-company`), and `?company=` with no value clears it. The date on the title slide is always today's. Both are filled by the small script at the end of `index.html` (`[data-company]`, `[data-company-line]`, `[data-today]`).

## Files

- `index.html` — the deck markup. Slides are `<section>` children of `<deck-stage>`; speaker notes live in a JSON array. **Edit this for content.**
- `styles.css` — all deck CSS, copied verbatim from the agent-engineering deck (it still carries rules for slides this deck doesn't use).
- `deck-stage.js` — shared framework. **Don't edit unless changing the framework itself**, and keep the copies under `prezos/` in sync.
- `assets/` — images. Dashboard screenshots are demo-org (acme) data.
- Numbers on the "said vs did" slide come from `../hivemind-agent-engineering/assets/charts/effort.json`; the W&B scale line on slide 05 ("what we capture") from that deck's slide 07. Update them there first.

## Slide editing

- **Speaker notes:** `<script type="application/json" id="speaker-notes">` holds one string per slide, in slide order. Update it in lockstep when adding/removing/reordering slides, along with the `NN / 12` page counters.
- Same design rules as `../hivemind-agent-engineering/CLAUDE.md`: reuse `:root` tokens, `var(--accent)` for highlights, `.chrome.dark` / `.chrome.paper` base, `.chrome.blue` for dividers and the survey hero.

## Previewing

Open `index.html` directly in a browser, or serve the repo root and visit `/prezos/hivemind-customer-briefing/`. Don't auto-open browsers or take screenshots unless the user asks.
