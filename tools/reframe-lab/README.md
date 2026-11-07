# Reframe Lab

Dev tooling for measuring and improving how Reframe picks shots. It shows the eight world-locked
90° views Reframe's semantic index sees at each sampled moment, every signal's score for each
view, and where you actually aimed in your kept shots. Use it to label views, run local vision
models over them, try MobileCLIP prompts, train probes on the CLIP embeddings, and compare all
of it in one table.

```bash
cd tools/reframe-lab
uv run lab.py export                          # reframe-cli lab-export cloud -> data folder (~30 s)
uv run lab.py serve                           # http://127.0.0.1:8765
uv run lab.py run pplx-decider-flash          # score views with a local Ollama vision model (or use the run bar)
```

`export` needs a release build of `reframe-cli` (`cd ../reframe && swift build -c release`) and
clips with a semantic index (search on in the app, or `reframe-cli index cloud`). Data lives in
`~/Library/Application Support/Reframe/lab` (`--data` to change it):

| File | What |
| --- | --- |
| `manifest.json` | Every view: clip, time, yaw, image path, Reframe's own signals, and inside kept shots `aim_err` (degrees from where you aimed) and `shot_source` |
| `embeddings.f16` | Each view's MobileCLIP-S2 vector, copied from the app's index (so scores match the app exactly) |
| `views/` | 384² JPEGs with the Rec.709 LUT |
| `lab.sqlite` | Your labels and model predictions (`labels`, `preds`) |
| `probes/` | Trained probe weights (512 floats + bias), ready to ship to the app |

Export samples a moment every 8 s, and every 2 s inside your shots (`--every`, `--shot-every`).
Re-exporting after you add shots keeps every moment the previous export had, so labels and model
scores (keyed by view id) carry over; `--fresh` starts the sampling over.

Each shot's `shot_source` says where its aim came from: `manual` (you marked it), `reaimed` (a kept
suggestion you re-aimed) or `suggested` (a suggestion kept with its aim untouched). A `suggested`
shot agrees with the app by construction, so the honest aim metrics leave it out.

## The page

- **Moments**: one row per moment, eight views around the horizon. A cyan bar marks the view you
  aimed at (kept shots). A white dot marks the selected signal's pick, and the bottom bar shows
  the signal's percentile. Sort by "Score misses your aim" to see where a signal goes wrong.
- **Grid**: views ranked by any signal.
- **Labelling**: click a view, then <kbd>1</kbd>–<kbd>4</kbd> for Junk / Filler / Good / Great
  (Good and Great are positives). <kbd>0</kbd> clears, <kbd>n</kbd> marks notable, and arrows
  move. The side panel shows every signal plus model captions, tags and subject probabilities.
- **Candidates**: moments outside every kept shot, queued for rating. *Best* is the view the Score
  by signal likes most in a moment you skipped (did it find something you missed?). *Unsure* is the
  view nearest its decision line (0 for a probe, 0.5 for a model probability, the median otherwise),
  where one rating teaches a probe the most. Rating moves to the next candidate. **Train taste probe
  and re-rank** trains `probe:taste` on your own aims plus every rating, makes it the Score by
  signal and rebuilds the queue.
- **Metrics**: each signal against a ground truth: your aim (shots you aimed yourself, or all kept
  shots), your taste (those aims plus your ratings), your labels, or a model used as a teacher. The
  aim columns default to shots you aimed yourself; a dashed cyan bar marks a suggestion kept as-is. AUC and Spearman per view, and per moment whether the signal's pick is your view
  (≤22.5°) or a neighbour (≤67.5°). Random picks average about 90° and hit the right view 12.5%
  of the time. The page also has:
  - **Probe**: logistic regression on the CLIP vectors, cross-validated by clip, with its table
    rows scored out-of-fold.
  - **Prompt playground**: positive minus negative MobileCLIP prompts as a new signal.
  - **Run a vision model**: the same as `lab.py run`, from the page.

## Signals

| Name | Source |
| --- | --- |
| `interest` | The analysis's fused heading grid without the CLIP channels, best bin within ±20° of the view |
| `interest_clip` | The same with `.appeal`/`.dull`: what the app aims with today |
| `appeal`, `dull`, `clip_margin` | The MobileCLIP channels (SEMANTIC.md); `dull` is a penalty and ranks inverted |
| `faces` | Faces within 20° of the view centre |
| `<model>.aim / .quality / .notable / .junk` | Decision models via Ollama `/v1/systemone` (probabilities, not text) |
| `<model>.rating` | Chat vision models via `/api/chat` with a JSON schema (0–5, plus tags and a caption) |
| `probe:<truth>`, `prompt:…` | Made on the page (kept for that server session; probe weights saved to disk) |

The decision questions are in `lab.py` (`QUESTIONS`). This model wants exactly six levels for a
`score` question. If you change the questions, bump `QUESTIONS_VERSION` and re-run with `--force`.

## Model runs

- **The run bar** under the header shows the current run: progress, seconds a view and time left,
  with **Pause**, **Resume** and **Stop**. It follows runs started with `lab.py run` too: run
  state lives in `lab.sqlite`'s `jobs` table, and each run checks its own row between views.
- **Pause** finishes the view in flight and then waits. **Stop** ends the run. Starting the same
  model again (the bar's Start/Resume, or `lab.py run`) skips every view it has already scored.
- **If the process dies** (for example, the server stops), the bar shows the run as
  *interrupted* after 90 s; start it again to continue.
- **Live updates:** new scores reach the page as they land, and the Metrics table refreshes every
  15 s while a run is going.

Speed on this Mac (32 GB):

| Model | Kind | Per view | All 2,688 views |
| --- | --- | --- | --- |
| `pplx-decider-flash` (4.5B) | decision | 0.45 s | ~20 min |
| `autotrust-jev:35b` | decision | ~3.4 s (34 GB, swaps here) | ~2.5 h |
| `bonsai-2:27b` | chat | ~8 s | ~6 h |

Each model keeps its own sources (`pplx-decider-flash.aim`, `autotrust-jev.aim`, …), so they
can be compared side by side in Metrics.

## The loop

1. Rate a batch in **Candidates**, then train the taste probe and rate the re-ranked queue. Repeat.
2. Compare signals in Metrics. Run a decision model and check whether it agrees with you.
3. When `probe:taste` beats `interest_clip` on your own aims in Metrics (or a model agrees with you
   and can teach one), it's ready for the app. A probe is a 512-float dot product, so
   it runs on-device for free in the app, as another heading-grid channel next to `.appeal`.
4. Keep the winners in the app and check them with `reframe-cli evaluate-aim` / `evaluate`.
