# klozar

Pulls the week's Serbian practice out of [Clozemaster](https://www.clozemaster.com)
and writes a lesson sheet to bring to a tutor.

```
klozar.py        the CLI
template.html    the interactive sheet, with a /*__DATA__*/ slot for the week
refresh.sh       rebuild the published sheet and push it
index.html       the shell + newest week baked in — committed, served by Pages
weeks/           one JSON of content per week    — committed, ~15 KB each
weeks.json       the week manifest the picker reads
.env             an alternative to the Keychain — gitignored
out/             ad-hoc sheets                  — gitignored
```

## Setup

```sh
uv sync
uv run klozar.py auth            # paste the cookie once, stored in the Keychain
uv run klozar.py collections     # confirms auth works
```

The cookie is HttpOnly, so it has to come out of DevTools by hand — Chrome →
**Application → Cookies → `https://www.clozemaster.com` → `_clozemaster_session`**.
`auth` reads it from stdin and stores it in the login Keychain under
`klozar-clozemaster`. (`security` takes the value as an argument, so it is briefly
visible in the process list on write; nothing persists in shell history.)

Off macOS, `CLOZEMASTER_SESSION` in the environment or in a `.env` beside this
script both still work — the lookup order is environment, then Keychain, then
`.env`. Only the Keychain and `.env` get the rotated cookie written back though;
a value from the environment goes stale on whatever the idle timeout is.

## Why the cookie doesn't go stale

The server **re-issues `_clozemaster_session` on every response**, with a fresh
`last_request_at` inside the encrypted payload. It's a Devise session on Rails'
encrypted CookieStore — self-contained, with an idle timeout that *using* it
resets. So klozar saves the rotated cookie back to wherever it came from after
every run, and a weekly job keeps it alive indefinitely. You only re-paste if you
go quiet long enough to cross the idle window.

This is also the reason the cookie does **not** belong in a GitHub Actions secret:
a static secret can't absorb the rotation, so it would expire on exactly the
schedule the rotation exists to prevent — and it's a full-account bearer
credential. Run it locally instead; see below.

## Use

```sh
uv run klozar.py lesson                  # last 7 days -> out/<date>-serbian-lesson.md
uv run klozar.py lesson --days 14
uv run klozar.py lesson --stdout         # straight to the terminal
uv run klozar.py snapshot                # raw JSON, one file per week, good for diffing
uv run klozar.py snapshot --scope favorited
uv run klozar.py artifact                # interactive HTML, ready to publish
```

The sheet has four sections: **Gave me trouble** (ranked by miss rate — the part
worth a tutor's time), **Starred this week**, **New this week**, and the cloze
words that recurred most.

## The interactive sheet

`artifact` bakes the week into `template.html` and writes a self-contained page:
translations hide for drilling, each sentence takes a note, and ticking one off
tracks what the hour actually got through. Ask Claude to publish the file with the
Artifact tool (`capabilities: {db: {}}`) and you get a link.

Persistence is two-tiered on purpose. Every change writes to `localStorage`
immediately, which works for any viewer in any browser and never fails. When the
page can reach the artifact's shared store it also writes there, so notes sync
live between whoever has it open — the badge in the corner says which is in
effect. Declaring `db` makes the artifact organization-internal, so a tutor
outside your org can read a shared link but their notes stay on their own device.

Each week is a fresh publish and a fresh link. State is keyed by week, so an old
sheet keeps its own notes.

## The published sheet

`site` writes the same page as `tools/klozar/index.html` — a complete standalone
document, since GitHub Pages serves the file as-is and without a `<meta charset>`
every `č ć š ž đ` turns to mojibake. Commit it and the repo's `pages.yml` workflow
puts it at **<https://vanpelt.github.io/sparky/tools/klozar/>**, publicly, no
sign-in.

The page is public, so it publishes the week's sentences and your error counts.
Lesson notes are never baked into the file.

### Shared notes

Notes and ticks sync through a **Turso** database that the page talks to directly
over its HTTP protocol — plain JSON `POST`s to `/v2/pipeline`, so there's no
client library and nothing loaded from a CDN. Turso answers with
`access-control-allow-origin: *`, which is what makes a static page able to reach
it at all.

The point of this over anything GitHub-native is that **your tutor needs no
account**. They open the link and type.

The trade, chosen deliberately: **the read-write token is in the published HTML,
so anyone who opens the sheet can read, edit, or wipe the notes.** It is a
dedicated database holding nothing else, and the token is public the moment it
enters git history — rotating it means issuing a new one, not scrubbing this one.
See `notes-backend.json`. Run `site --no-notes` to publish without it and keep
notes per-device.

The database has Turso's Delete Protection on, which prevents the *database* being
deleted through the platform API. It is not a guard on the data: an `rw` token
still permits `drop table notes` or `delete from notes`. Worth keeping a copy of
anything that matters — `klozar.py snapshot` already archives the sentences, and
notes also persist in each reader's `localStorage`.

Inside a claude.ai artifact that `fetch` is blocked by the CSP, so that copy of
the page uses the artifact's own store instead; on any other host with neither,
`localStorage` carries it alone and the badge reads "Saving on this device".

### Earlier weeks

There is **one** HTML shell. `index.html` carries the CSS, the JS, and the newest
week's data baked in, so the default URL paints immediately and works offline.
Every week also gets a `weeks/<date>.json` of pure content — about 15 KB, against
36 KB if the whole document were copied per week. Older weeks load as
`index.html?week=<date>`; a dropdown in the controls row switches between them.

Two things fall out of that split. Because the shell is the only copy of the CSS
and JS, **fixing it fixes every past sheet** instead of leaving old weeks frozen
with old bugs. And because notes are keyed by week, opening an old sheet brings
back the notes taken on it.

The picker reads `weeks.json` **at load rather than baking it in**. A sheet
published in March would otherwise be frozen and could never list a week from May.
A `?week=` that doesn't exist falls back to the latest sheet rather than a blank
page.

The sheets stay in git rather than the database on purpose. They're the record —
append-only, versioned, restorable — and the page's Turso token is public and
read-write, so putting them there would mean anyone with the link could erase the
archive rather than just this week's notes.

Two details worth keeping if you touch the sync code:

- **One write in flight per sentence.** Ticking a box and then typing a note fires
  two saves for the same row; without serialization the slower first request can
  land last and overwrite the newer state. This ate a note the first time it was
  tested. A change made while a save is out sets a flag, and the re-run reads
  current state, so only the latest wins.
- **A poll must not take a row that has a write pending**, or it reverts what was
  just typed and the queued save writes the reverted value back out.

Polling is every 15s and only while the tab is visible.

### On a schedule

```sh
./refresh.sh          # rebuild, commit, push — no-ops if nothing changed
```

It only ever stages `tools/klozar/index.html`, and it refuses to push from any
branch but `main`, so it's safe to leave on a timer. Weekly, an hour before the
lesson:

```sh
cat > ~/Library/LaunchAgents/sh.catnip.klozar.plist <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>Label</key><string>sh.catnip.klozar</string>
  <key>ProgramArguments</key>
  <array><string>SPARKY/tools/klozar/refresh.sh</string></array>
  <key>StartCalendarInterval</key>
  <dict><key>Weekday</key><integer>1</integer><key>Hour</key><integer>8</integer></dict>
  <key>StandardErrorPath</key><string>/tmp/klozar.log</string>
</dict></plist>
PLIST
# replace SPARKY with the repo path, then:
launchctl load ~/Library/LaunchAgents/sh.catnip.klozar.plist
```

Each run rolls the cookie forward, so the schedule is what keeps auth alive.

Starring alone turns out to be a weak signal: it's sticky, so the same handful of
old sentences come back every week. `lastPlayedDate` combined with
`numIncorrect / numPlayed` is what actually surfaces the current week's friction.

## The API

Clozemaster publishes no API. There is one official export — **Download
Favorites** on the dashboard, a Pro feature that emits
`/l/<pairing>/collections/<slug>/favorites.tsv` in Anki cloze format — but it
carries no dates, levels, or error counts, so it can't answer "what did I work on
this week."

So this reads the same undocumented JSON API the web app uses:

| Endpoint | Returns |
| --- | --- |
| `GET /api/v1/lp/<pairing>` | collections, your stats, and the URLs for everything else |
| `GET /api/v1/lp/<pairing>/c/<slug>/ccs` | per-sentence records with progress |
| `GET /api/v1/lp/<id>/daily-stats` | per-day counts, streak |
| `GET /api/v1/lp/<id>/more-stats` | aggregate stats |

`ccs` takes `scope`, `page`, `per_page`, and `query`. Scopes: `all`, `playing`,
`favorited`, `ignored`, `known`, `mastered`, `ready_for_review`, and
`{0,25,50,75}pct_mastered`. `per_page=500` returns a whole 1,000-sentence
collection at once.

Each sentence record carries `text` (with the answer wrapped in `{{…}}`),
`translation`, `pronunciation`, `hint`, `notes`, `level`, `numPlayed`,
`numIncorrect`, `lastPlayedDate`, `nextReview`, `favorited`, `ignored`,
`difficulty`, `tatoebaId`, and `ttsAudioUrl`.

### Two things that will waste your afternoon

**`Time-Zone-Offset-Hours` is mandatory.** Every `/api/v1` call without it returns
`400 {"status":400,"error":"Bad Request"}`, which looks exactly like an auth
problem. It isn't. `X-CSRF-Token` and `X-Requested-With` are *not* needed for GETs.

**Missing auth doesn't 401.** Without the cookie the API happily returns 200 with
the public catalog and no progress attached, so a broken cookie yields an empty
sheet rather than an error. `klozar` checks that a `username` came back and says
so.

Auth is the `_clozemaster_session` cookie only — there's no token or OAuth
endpoint (`/oauth/token` and `/api/v1/sessions` are both 404), and
`api.clozemaster.com` is just a second host serving the same Rails app.
