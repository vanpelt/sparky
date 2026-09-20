# stateus

Reconstruct a per-day location ledger from whatever evidence you actually have,
for day-count questions like "how many weekdays was I in NYC this year?"

## Why it works this way

The obvious approach — export Google Maps Timeline from Takeout — **no longer
exists**. Google moved Timeline to on-device storage in late 2024 and finished
the cutover in 2025. Takeout now returns an empty archive for Location History.
If you enabled Timeline recently, Google holds nothing from before that date.

So this tool treats location as an *evidence problem* rather than a log to
parse. It merges whatever sources exist, states the basis for every day, and
reports days it cannot resolve instead of guessing at them.

## Sources

- **Apple Photos** (default, no export needed) — reads the local library's
  sqlite directly, read-only.
- **Google Maps Timeline** (`--timeline FILE.json`) — exported from the phone,
  not Takeout. The highest-fidelity source, but only covers the period since
  Timeline was switched on.
- **Google Takeout** (`--takeout DIR`) — the Timeline folder in a Takeout is
  empty by design, but `My Activity/Maps` records the map centre of every
  Maps search, which is a genuine position fix. Also reads `Records.json`,
  `semanticSegments` and Photos metadata where present.
- **Timezone corroboration** — non-geotagged photos (screenshots included)
  and the user's own git commits both record a UTC offset, which pins the
  zone even when nothing records coordinates. Disable with `--no-tz`.

### Measured accuracy

Each source was scored against Timeline over a 55-weekday window where both
existed, rather than assumed to be good:

| source | precision on NYC | recall |
|---|---|---|
| Apple Photos alone | 100% | 49% |
| Maps activity coordinates | 100% | 66% |
| Timezone (git + screenshots) | 93% | 95% |
| **all of the above combined** | **100%** | **93%** |

Two findings worth keeping: Takeout renders every timestamp in the account's
*current* display timezone, so the `EDT`/`PDT` label on an activity entry says
nothing about where the user was — only coordinates are usable from it. And
evidence of presence is much stronger than evidence of absence: on 5 of the
window's days a source placed the user elsewhere while they were in fact in
NYC part of that day, so `AWAY` is only ever concluded after every source has
been merged.

## Two things it gets right that a naive version doesn't

**Local days, not UTC days.** A photo taken at 04:02 UTC in California is the
previous evening locally. Counting the UTC day silently shifts it across a
date boundary — which is the whole ballgame for a day count.

**Other people's photos.** A photo library is not a location log. Pictures
arrive by AirDrop, text and shared album carrying someone else's EXIF
coordinates. Counting those invents days in places you never went. `stateus`
picks the device with the broadest coverage as a reference, then keeps another
device only if it never contradicts that reference on a day they both saw.
The keep/drop decision is printed every run so it can be audited or overridden
with `--device`.

NYC is tested against real five-borough polygons, not a bounding box — a box
around NYC also contains Jersey City, Newark and Hoboken.

## Output

Every day gets a status (`NYC` / `AWAY` / `UNKNOWN`) and a **basis**:

- `observed` — there is a datapoint placing you there that day.
- `inferred` — unobserved, but sits in a short gap between two days that agree.
  Only fills when both ends match and the run is no longer than `--max-gap`.
- `no evidence` — reported as unresolved. Never counted.

The headline splits the same way: an observed floor you can defend, a best
estimate including inferences, and a count of days needing manual review.

## Quickstart

```bash
cd tools/stateus
uv sync

# Photos only
uv run stateus.py --year 2026

# Add a Takeout dump, and write the day-by-day ledger
uv run stateus.py --year 2026 --takeout ~/Downloads/Takeout -o ledger.csv
```

## Two different questions

These are not the same count, and mixing them up is the easy mistake:

- **Income allocation** — *weekdays worked in NYC*. Uses the five-borough
  boundary and only counts weekdays.
- **Statutory residency (the 183-day test)** — *all days present in New York
  State*. Weekends count, any part of a day counts, and the whole state
  counts, not just the city. `load_ny_state()` unions the coarse state outline
  with the precise borough polygons, so border-adjacent points are still
  decided by the accurate geometry.

Statutory residency is also a two-prong test: the day count only bites if a
permanent place of abode in New York is maintained for substantially all of
the year. It is separate from domicile, which is about intent rather than
arithmetic.

## Caveat

This reconstructs a defensible estimate; it is not tax advice. New York
day-count audits expect contemporaneous records, and third-party evidence
(flights, hotel folios, tolls, card transactions) carries more weight than
inferred days. Use the `UNKNOWN` list as a worklist against those records.
