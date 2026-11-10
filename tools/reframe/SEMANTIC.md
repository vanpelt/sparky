# Semantic search: typed queries on the heading grid

Prototype. Type what you're looking for ("a waterfall", "someone riding a mountain bike") and
get the moments **and the directions** in the 360° footage that match it, plus suggested shots
whose camera path aims there.

## How it works

1. **Index** (`SemanticIndexer`, once per clip, cached): one decode of the LRV proxy. Every
   second it renders 8 world-locked 90° views around the horizon (45° apart, Lock stabilization
   at the clip's first frame, the same frame as the interest map), auto-levels them (I-Log is too
   flat for CLIP) and embeds all 8 in one batch with **MobileCLIP-S2**'s image encoder on the
   Neural Engine. Stored as Float16: 8 KB per second of footage, in
   `~/Library/Application Support/Reframe/semantic/<name>-<size>-<mtime>.plist`.
2. **Query**: the text encoder embeds the query (~50 ms; the CLIP BPE tokenizer is
   `CLIPTokenizer.swift`), and a dot product against every stored vector gives similarity per
   (second, view).
3. **Relevance** = `clamp((cos − 0.17) / 0.08)`. MobileCLIP cosines are tightly packed. On our
   footage, things in view score 0.22–0.30 and things that aren't there top out around 0.15. A
   bland caption ("a photo.") scores about 0.19 everywhere, so subtracting a baseline erases real
   matches; an absolute floor works better. Hits are ranked by raw cosine, because relevance
   saturates for broad queries.
4. **Heading grid**: relevance is interpolated to the 4 Hz × 72-bin grid as the `query` signal.
   `ClipAnalysis.searched(_:)` adds it to the cached interest map with a dominant weight (the
   other signals keep 25%, so they still shape cut points), re-runs the shot suggester, and keeps
   suggestions where the query is among the reasons. The AutoCam-style path then aims at the match.

Speed (M-series, release build): indexing runs at **0.03× real time** (29 min of footage in
27 s; decode 20%, render 20%, embed 60%). Searching an indexed clip takes ~0.3 s, model load
included.

## In the app

- **Settings › Search** turns it on. Turning it on downloads the model (with progress, resumable
  by turning it on again) and starts indexing in the background, the clip you're looking at
  first, then newest first. Settings › Storage lists the index and the models with their own
  Clear/Remove actions.
- **One search field** (the window toolbar's, ⌘F) serves every view:
  - *Clips*: the sidebar becomes the matching clips, best first, split into clear matches
    (cosine ≥ 0.21) and possible ones (0.17–0.21, often lookalikes). Each row's sparkline becomes
    the match over time, with a badge at the best moment. Opening one jumps there and looks that way.
  - *Editor*: the heading lane shows the query's heat instead of interest (labelled, still white:
    color stays reserved for the sightline), suggested shots become shots aimed at the query, and the
    inspector lists the matches; ⌘G / ⇧⌘G step through them.
  - *Shots*: a shot matches by name, by clip, or by what it looks at (similarity along its
    keyframed line of sight).
- Search off, the field still filters clips by file name and shots by name, with a prompt to turn it on.

## Storage, and why not a database

The index is derived, write-once, read-in-bulk data, so it's one memory-mapped file per clip
(`semantic/<name>-<size>-<mtime>.rfsi`: magic, JSON header, Float16 vectors), keyed like the
analysis cache. Scoring is a matrix-vector product with Accelerate (vImage Float16→Float, vDSP),
a few milliseconds per hour of footage, so there's nothing for an ANN index or SQLite (with a vector
extension) to speed up at this scale, and SwiftData/Core Data would add a schema and migrations for
data we can always regenerate. Settings are UserDefaults. If clips or shots should ever be findable
from Spotlight, Core Spotlight (`CSSearchableIndex`) is the Apple-native next step, fed with text
we derive (shot names, captions), not these vectors.

## Models

Downloaded by the app (Settings › Search) or `reframe-cli models install` into
`~/Library/Application Support/Reframe/models/` (excluded from Time Machine), from URLs pinned to
Hugging Face commits and checked by SHA-256 (`SemanticModels.swift`):

```
mobileclip_s2_image.mlpackage   huggingface.co/apple/coreml-mobileclip (71 MB)
mobileclip_s2_text.mlpackage    huggingface.co/apple/coreml-mobileclip (127 MB)
clip-vocab.json, clip-merges.txt  huggingface.co/openai/clip-vit-base-patch32 (1.4 MB)
```

Each package is compiled for this Mac (`compiled/*.mlmodelc`) and then deleted, so ~190 MB stays
on disk. The weights are under Apple's ASCL license.

## CLI

```bash
swift build -c release --product reframe-cli
.build/release/reframe-cli models install                    # or status / remove
.build/release/reframe-cli index cloud                       # or a folder / .insv files
.build/release/reframe-cli search "a waterfall" cloud --limit 8 --stills /tmp/hits
.build/release/reframe-cli search "a person riding a mountain bike" clip.insv --shots
.build/release/reframe-cli search "curtains" clip.insv --raw  # raw cosines and percentiles, for calibrating
```

## Better suggestions from the same embeddings

The index already says what is in every direction every second, so the interest map can use it
without new models. First measurement, `reframe-cli evaluate-aim cloud`: for the 10 shots kept so
far (208 samples, every 0.5 s), how far each signal's strongest heading is from where you aimed:

| Signal | Mean error | Within 30° | Within 60° |
| --- | --- | --- | --- |
| Camera forward (baseline) | 83° | 27% | 50% |
| Interest map without CLIP | 63° | 42% | 56% |
| Appeal alone (zero-shot "worth filming" minus "junk" prompts) | 76° | 27% | 49% |
| Taste (leave-one-out average of your other shots' views) | 92° | 17% | 32% |
| Interest + appeal, z-scored (the experiment) | 57° | 44% | 60% |
| **Interest map with `.appeal` and `.dull` (shipped)** | **56°** | **48%** | **63%** |

Small and biased (all ten started as suggestions the interest map aimed). Per shot, the shipped
channels fix two big misses (Tundra 70° → 11°, VID_004 56° → 32°), leave six about where they
were (VID_009 stays at 16°), and cost one (VID_016 21° → 36°). VID_077 shot 2 is still 147° off:
the z-scored blend got it to 42°, the weighted sum doesn't, because interest there is strong in
the wrong direction. Taste is worse than chance with ten picks: "people in the desert" is
everywhere.

### What's in (steps 1 and 2)

- **One decode.** `ProxyScan` renders the eight CLIP views alongside the face-detector views
  and embeds them on a queue of its own when the model is installed and search is on
  (`Analyzer.useSemantic`, synced from Settings › Search). The index it writes matches a
  separately built one (cosine ≥ 0.9997 per view). On a 2-minute clip analysis goes from 3.1 s
  to 3.7 s; indexing separately was another ~3.8 s and a second decode.
- **`.appeal`** (directional, `Semantic/SemanticSignals.swift`): per view, the cosine to the
  mean of ten "things people film" prompts minus the mean of ten junk prompts; then relative to
  that moment's median heading (0.04 above it is full scale: the 90th percentile here), so every
  moment has a best direction and nothing floods; then `persistentDamped` like faces, so your
  own face on the stick doesn't always win.
- **`.dull`**: views where the junk prompts win outright (margin below 0, full at −0.04). Not
  damped: the roll cage being in every frame is why the path should stay off it.
- **Weights**: appeal 0.3 / dull −0.3 in Blend (a sweep: 0.3/−0.3 had the most within 30°;
  0.5 helps the mean, 0.8+ starts overriding faces), 0.2 People, 0.25 Action, 0.6 / −0.45
  Scenery. Dull alone changes nothing (it's rarely where interest points); with appeal it helps a
  little.
- **Prompts** are generic outdoor categories ("a scenic view of mountains or canyons", "a vehicle
  driving off-road", "a dramatic sky"): the trip-specific wording ("red rock canyons", "the
  desert") scored the same, and fully generic wording ("a person doing a sport") a bit worse
  (60° / 45%).
- **No re-analysis needed**: analyses made before a clip had an index gain the channels when
  next loaded (two matrix-vector products over the mapped index, ~0.1 s), and a clip indexed in
  the background updates in place. `SemanticSignals.version` re-derives them when prompts change.
- **`evaluate cloud`**: the kept suggestions rank higher in their clips (mean rank 3.3 → 2.7; in
  the top 3: 6 → 7 of 10; Catan #6 → #2, Tundra #7 → #5; VID_077 shot 1 #2 → #3).

### Next

3. **Directional novelty**: embedding change per heading versus the previous 3–12 s, replacing
   the time-only colour-histogram novelty ("arriving somewhere new", and where).
4. **Titles**: classify each suggestion's peak view against a ~100-label vocabulary ("Waterfall",
   "Group photo", "Off-roading", "Board game") instead of "Faces, laughter". The same text could
   feed Core Spotlight later.
5. **Diversity**: rank library-wide suggestions with MMR on their peak-view embeddings, so five
   near-identical canyon shots don't fill the top.
6. **Taste, later**: once there are ~50 kept shots and some dismissals, a linear probe on kept vs
   dismissed views (a per-user weight vector, re-ranked like presets, not baked into the cache).

## Also next

- Calibrate the floor on more footage (it's from ~20 queries over 17 clips), and try
  MobileCLIP2 (better retrieval; would need a Core ML conversion).
- Use the same vectors for "more like this shot".
