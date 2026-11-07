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

## Next

- Fold indexing into `ProxyScan` (it already renders world-locked views for the face detector)
  so analysis decodes the proxy once.
- Calibrate the floor on more footage (it's from ~20 queries over 17 clips), and try
  MobileCLIP2 (better retrieval; would need a Core ML conversion).
- Use the same vectors for library-wide de-duplication and "more like this shot".
