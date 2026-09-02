# klozar

Pulls the week's Serbian practice out of [Clozemaster](https://www.clozemaster.com)
and writes a lesson sheet to bring to a tutor.

```
klozar.py        the CLI
template.html    the interactive sheet, with a /*__DATA__*/ slot for the week
.env             your session cookie   — gitignored
out/             generated sheets      — gitignored
```

## Setup

Copy `.env.example` to `.env` and paste in your session cookie. It's HttpOnly, so
it has to come out of DevTools by hand — Chrome → **Application → Cookies →
`https://www.clozemaster.com` → `_clozemaster_session`**.

```sh
uv sync
uv run klozar.py collections     # confirms auth works
```

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
