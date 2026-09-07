# klozar

Pulls the Serbian practice since the last lesson out of
[Clozemaster](https://www.clozemaster.com) and writes a lesson sheet to bring to
a tutor.

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
uv run klozar.py lesson                  # since the last sheet -> out/<date>-serbian-lesson.md
uv run klozar.py lesson --days 14        # override the window
uv run klozar.py lesson --max-days 21    # or just raise the ceiling
uv run klozar.py lesson --stdout         # straight to the terminal
uv run klozar.py snapshot                # raw JSON, one file per week, good for diffing
uv run klozar.py snapshot --scope favorited
uv run klozar.py artifact                # interactive HTML, ready to publish
```

### How far back it looks

Lessons are not weekly — sometimes two land in one week, sometimes a fortnight
goes by. A fixed 7 days gets both cases wrong: it re-shows what the last sheet
already covered, or it drops the middle of a long gap.

So the window runs from **the previous sheet to today**, with a ceiling of 14
days (`--max-days`). `weeks.json` records when every sheet was made and a sheet
is made per lesson, so the previous entry is the best available answer to "when
did we last do this". Today's own entry is skipped, or rebuilding would measure
zero against itself.

It is a proxy, not a log: rebuild twice in one day for some unrelated reason and
the next window shrinks to match. `--days N` overrides it, and every run prints
which window it chose and why.

The window also feeds the page — recency in the default ordering is normalised
over the actual span, so a four-day sheet doesn't treat a three-day-old sentence
as ancient.

One consequence worth expecting: narrowing the window drops sentences that fall
outside it, annotated or not. Nothing is lost — a note belongs to the sentence,
so it comes back with the sentence.

The sheet has four sections: **Gave me trouble** — the part worth a tutor's time
— **Starred this week**, **New this week**, and the cloze words that recurred
most.

### How it's ordered

Miss rate alone ranks badly. Half the week sits at "1 of 2", and a sentence
fumbled last Monday has usually been re-drilled since. The default order is a
blend instead:

    0.55 × (missed / played)  +  0.30 × recency  +  0.15 × starred

where recency decays linearly across the window, measured from the newest day
*in the sheet* rather than from today — so a sheet reopened in December still
ranks the week it covers the way it did when it was pulled.

The page carries the same formula in JS and re-sorts client-side, so a dropdown
in the controls row switches between:

| | |
| --- | --- |
| **Worst & freshest** | the blend above — the default |
| **Most recent** | last played, newest first |
| **Worst miss rate** | what the sheet used to do, kept because it still answers a real question |

Beside it, **★ Starred only** filters every section down to what was flagged by
hand. Both settings live in `localStorage` under `klozar:prefs` — they're a
reading habit, not a property of a week, so they survive switching weeks.

Because the page re-orders, the data can't be pre-cut by one ordering: "most
recent" over the 25 worst is not the most recent. So every matching sentence
ships and a section *opens* on the first `--limit` (25) of them, with the rest
behind a "show all". That is why a week's JSON went from ~15 KB to ~65 KB —
about 9 KB over the wire, since Pages gzips it.

## The interactive sheet

`artifact` bakes the week into `template.html` and writes a self-contained page:
translations hide for drilling, each sentence takes a note, a **Lesson notes** pad
above the sheet takes everything that belongs to no single sentence, and ticking
one off tracks what the hour actually got through. Ask Claude to publish the file with the
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

Notes, ticks and the week's Lesson notes pad sync through a **Turso** database that the page talks to directly
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
Every week also gets a `weeks/<date>.json` of pure content — around 65 KB now
that whole sections ship rather than their top 25 (9 KB gzipped), against a whole
duplicated document per week. Older weeks load as
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

Three tables, and the page never creates any of them. The DDL lives in
`notes-backend.json`; apply it by hand before publishing a page that depends on
it. All three statements are `if not exists`, so re-running them is free.

| Table | Keyed by | Holds |
| --- | --- | --- |
| `sentence_notes` | sentence | the note — no week column at all |
| `notes` | week + sentence | the weekly tick |
| `week_notes` | week | the Lesson notes pad |

### What carries between weeks

A sentence often comes back. What I worked out about *posebno* is still true in
November, so **a note belongs to the sentence, not to the week** — it has no week
column, and the same note shows up in every sheet the sentence appears in.

A tick means something different: *did I go through this in this week's lesson*.
That has to reset. But a sentence I have already been through is not the same as
one I have never touched, so a box covered in an earlier week and not yet in this
one renders in the checkbox's third state — a dash rather than empty — with a
legend above the sheet and a tooltip on the box. Ticking it fills it in; unticking
returns it to the dash rather than to empty, because this week is undone but the
earlier weeks still happened. The "covered today" tally counts only this week.

The three are separate sync keys (`n:<id>`, `t:<id>`, and `__week__`) with
separate revisions, because they do not share a lifetime and so cannot share a
row. The page above the sync layer still reads one object per sentence; `readCell`
and `writeCell` are the whole of the translation.

On disk that means two `localStorage` scopes: `klozar:<week>` for the ticks and
the pad, `klozar:notes` for the notes, plus `klozar:prior` for which sentences
carry a tick from an earlier week. A blob written by an older page kept its notes
under the week, so boot reads those too and an upgrade keeps them on screen.

The `note` column on `notes` is vestigial after the migration to `sentence_notes`.
It is left populated on purpose, so a browser still running an older copy of the
page keeps showing something; it is safe to blank once every client has reloaded.

### Two people editing at once

The tutor and I are both in the sheet during the hour, so the question isn't
whether edits collide but what happens when they do. Two things stop one person
erasing the other:

**Text is published on a pause in typing, not on leaving the field.** Blur-only
saving meant your work sat in your browser for as long as you kept the cursor
there, and whoever left their field last overwrote the other outright. A write
now goes out ~800 ms after you stop typing, so the window in which two people can
diverge is about a second instead of a whole train of thought.

**Every write is a compare-and-swap on `rev`.** The upsert carries a
`where rev = <the revision this browser last read>`, so a write that would land
on top of a change you never saw simply doesn't apply — `affected_row_count`
comes back 0 and the same round trip returns what is actually stored. The page
then runs a line-level three-way merge between the common ancestor, your text
and theirs, and retries from their revision. Both edits survive.

Non-overlapping edits merge with nobody noticing, including the ordinary case of
two people appending different lines. A hunk that merely *starts* where another
ends counts as adjacent, not conflicting — rewording a line while someone adds
one after it is not a disagreement. Only a genuine overlap, both rewriting the
same lines differently, keeps both copies between `[both edited — yours]` /
`[both edited — theirs]` / `[end]`, and the badge says "Both edited — kept both"
so the markers don't read as corruption.

Incoming text is applied into a field you are *typing in*, with the caret mapped
across the change rather than thrown to the end. Dropping remote edits to protect
the typist was the old behaviour, and it guaranteed the loss: the drop was
silent, and the next save wrote the dropped text back out.

`rev` is not a clock, which is the point — see the rejected `updated_at` guard
below. It answers only "did this row change since I read it".

The third copy of every note, `shadow`, is the ancestor those merges need. It
lives in `localStorage` under `klozar:base:<week>`, beside but separate from the
notes themselves, so a page saved before it existed still loads and simply adopts
the store's version on its first poll.

The document store behind the artifact copy has no conditional write, so there
the guard is checked against the newest snapshot instead of inside the write —
narrower, since two writes inside one snapshot interval can still race, but it
still refuses a revision the page hasn't seen.

The pad is not a second sync system. It rides the per-sentence machinery under
the reserved key `__week__`, which no sentence id can collide with, so it inherits
the coalescing, the localStorage fallback and the don't-clobber-what's-being-typed
rule rather than growing a subtly different copy of each.

Two details worth keeping if you touch the sync code:

- **One write in flight per key.** Ticking a box and then typing a note fires
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
`numIncorrect / numPlayed` is what actually surfaces the current week's friction —
which is why the blend gives starring only a 0.15 nudge, enough to break a tie
and not enough to float a stale favourite to the top. It gets its own filter
instead, for when that *is* what you want to look at.

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
