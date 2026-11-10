# Interest map: plan

Suggest the moments worth keeping and where to point the camera in them, on-device, mostly with
classical computer vision and the camera's own sensors.

## What it produces

1. **Interest map** `I(t, heading)` for every clip: a time × direction grid (4 Hz × 72 bins of 5°)
   drawn as heat in the heading lane, so you can see *when* and *where* things happen.
2. **Suggested shots**: in point, out point, a keyframed camera path and a lens preset, plus the
   reasons ("faces, laughter", "jump", "fast motion ahead"). They appear as ghost shots on the
   timeline. Accept with A, dismiss with X; accepted ones are ordinary editable shots.
3. **Library ranking**: a per-clip score and a "best moment" frame, so a day of footage can be
   sorted by what's worth opening first.

## Constraints

- On-device and fast: analysis runs on the 2048×1024 LRV proxy, targeting under 0.2× real time
  (an 88 s clip in under 18 s), in the background, cached to disk.
- Explainable: every suggestion keeps its per-signal contributions, so the UI can say why.
- Classical where it's good enough. Apple's Vision models (faces, people, saliency) run on the
  Neural Engine in milliseconds and are treated as cheap detectors, not "AI".
- Deterministic and tunable: weights in one place, a CLI to dump everything as JSON.

## Prior work worth borrowing

- **Pano2Vid / AutoCam** (Su, Jayaraman, Grauman, ACCV 2016): score space-time "glimpses",
  then pick a smooth camera path with dynamic programming. We use the same path formulation;
  our glimpse scores come from classical signals rather than a model learned from web video.
- **Spherical spectral residual saliency** (Du & Varshney, CG&A 2021): classical saliency
  natively on the sphere, reported at over 60 fps for 4K. A good candidate for the "stands out"
  signal without cube-face seams.
- **Google AutoFlip** (MediaPipe): the reframing strategy of choosing still, pan or track
  depending on how subjects move, plus its detector-weighted crop path.
- **VideoHighlighter** and the audio-energy highlight literature: loudness and motion peaks carry
  most of the value. Crowd reactions often lag the event, so pull segment starts earlier.

## Signals

All directional signals are computed in the *world-stabilized* frame (we have the gyro), which is
what makes 360° analysis tractable: the scene stands still, so anything that changes is real.

| Signal | Source | Gives | Cost |
| --- | --- | --- | --- |
| Gyro events | IMU (already parsed) | Jumps and drops (vertical acceleration and airtime), calm stretches, the operator turning the camera toward something | Free |
| Shake | IMU | Negative weight for unusable stretches | Free |
| World motion | Optical flow (Vision or OpenCV DIS) between consecutive stabilized 512×256 equirect frames, minus the smooth forward-travel component | Things moving relative to the scene: people, animals, vehicles, splashes | ~3 ms per sample |
| People and faces | Vision face and human detection on 6 cube faces (or 8 horizon views at 90°) | Heading and pitch of each person; face size (nearness); looking at the camera | ~10 ms per sample (Neural Engine) |
| Visual saliency | Spherical spectral residual, or Vision attention saliency per cube face | What stands out in the scenery | 2–10 ms per sample |
| Audio | Loudness envelope and onsets; Apple's sound classifier for laughter, cheering, speech and screams, if it covers those (not confirmed yet) | When something happened (no direction: the file's audio is stereo) | Faster than real time |
| Novelty | Color-histogram change of the horizon band over time | Arriving somewhere new | ~0.5 ms per sample |
| Junk | Exposure records, frame darkness, a lens covered by a hand or pocket | Negative weight | Free |

Sampling: flow and novelty at 8 Hz, detectors and saliency at 4 Hz. Proxy decode, cube-face
rendering and Vision batching all stay on the GPU or Neural Engine.

## Fusion

1. Each signal writes a normalized grid `S_k(t, h)`. Time-only signals (audio, gyro events) spread
   evenly across headings, or toward the operator-facing heading on selfie-stick clips.
2. `I = Σ w_k · S_k`, then a light smoothing in time (0.5 s) and heading (one bin), wrapping at ±180°.
3. Weight presets the user picks from: **People** (faces, speech, laughter), **Action** (world
   motion, gyro events, loudness) and **Scenery** (saliency, novelty, calm). Default is a blend.
4. Contributions are kept per cell so the UI can explain any spot.

## From map to suggestions

1. **Find moments:** take `max_h I(t, h)` over time, find peaks with hysteresis, and grow each into
   a 3–12 s segment. Snap the ends to calm gyro moments so cuts don't land mid-swing, start a beat
   early for audio-led peaks, then merge neighbours and suppress overlaps.
2. **Aim the camera:** dynamic programming over heading bins (AutoCam-style). Maximize
   `Σ I(t, h_t) − λ·|Δh| − μ·|Δ²h|` for a smooth path that still follows the action. Pitch comes
   from where the detected subjects sit vertically.
3. **Make it editable:** simplify the path into a few keyframes (Douglas–Peucker with a 3–5°
   tolerance), so a suggestion is just a normal shot in our keyframe UI.
4. **Pick a lens:** Mega or Ultra when the interest is spread wide or subjects are close (a group
   selfie), Dewarp by default, Linear for distant scenery or architecture.
5. **Rank:** the segment score is its integrated interest along the chosen path, less penalties for
   shake and junk.

## UI

- **Heading lane heat:** drawn as luminance (white at varying opacity) so the cyan sightline stays
  the only accent. Hovering explains a spot ("2 faces, laughter, motion").
- **Ghost shots:** dashed spans on the lanes plus pale tiles in the shot strip with their reasons.
  A accepts, X dismisses, and accepting turns one into a normal shot.
- **Library:** "Best moments first" sorting and an interest sparkline under each thumbnail.
- **Weight presets** (People / Action / Scenery) in the inspector, re-ranking instantly from the
  cached grids without re-analysis.

## Architecture

- `ReframeKit/Analysis/`
  - `AnalysisPipeline` (actor): proxy decode, stabilized sampling, fan-out to signals, caching.
  - `InterestSignal` protocol: `process(sample)` then `finish() -> SignalGrid`. One type per signal.
  - `InterestMap`: the fused grid plus per-signal contributions, with weights applied at read time.
  - `ShotSuggester`: segmentation, the dynamic-programming path, keyframe fitting and lens choice.
- Cache: `~/Library/Application Support/Reframe/analysis/<clip-id>.bin`, versioned so analysis
  re-runs when the algorithms change.
- `reframe-cli analyze clip.insv --json`: dumps every grid and suggestion for tuning.
- Python stays the lab: prototype and tune signals there (OpenCV flow, spectral residual,
  thresholds), check them against your picks, then port the winners to Swift, as we did for
  stabilization.

## Evaluation

We need ground truth from your footage, not a benchmark. You mark the moments you'd actually
keep on 5–10 clips: just the shots, which the app already records. We then measure:

- **Moment recall @ k:** how many of your shots overlap one of the top-k suggestions.
- **Aim error:** the heading difference between our path and your keyframes inside overlapping
  shots (Pano2Vid's metric, in spirit).
- **Speed:** analysis time per clip relative to real time.

Every change to signals or weights is scored against this set before it ships.

**Built (2026-11-06):** `reframe-cli evaluate <footage folder> [--preset P] [--json]` reads the
folder's shots and dismissals from the app and scores all four presets side by side:

- **Recall @1, @3, any:** of the shots you marked yourself, how many overlap a suggestion at that
  rank (overlap ≥ half the shorter of the two).
- **Aim off / ≤ 30°:** mean world-heading difference between your shot and the matching
  suggestion's path over their overlap, and the share within 30° (in frame for any lens).
- **Keep rate:** kept ÷ (kept + dismissed) suggestions.
- **Re-aimed:** kept suggestions you turned by more than 10° from the path as originally suggested.
  Shots now record `suggestionID` and `suggestedKeyframes` when kept, so this is measured against
  what you were offered, not today's analysis.
- **Dismissed in top 3:** dismissed moments the current analysis still ranks highly.
- **Speed:** analysis time ÷ footage time.

Shots kept before provenance was recorded are recognised by their path (Lock at the first frame,
keyframes on analysis cell centres). The `REFRAME_DEMO_SHOT` debug shot is flagged so it can be
deleted.

## Phases

1. **Time-only signals (done, 2026-11-06).** Gyro events, audio loudness and onsets, the sound
   classifier if it covers laughter and cheering, and junk detection. These give moment
   suggestions that use the follow heading, plus the library ranking. Useful straight away for
   whittling down footage.
   What shipped (`Sources/ReframeKit/Analysis/`, `reframe-cli analyze`):
   - Signals on a 4 Hz grid: loudness and spectral-flux onsets; Apple's sound classifier, which
     *does* cover laughter (six labels), cheering/applause/crowd, screaming/shout/yell, speech,
     music and wind noise, run on 1.5 s windows every 0.75 s; jumps (sustained free fall under
     0.35 g), impacts (60 ms-averaged force standing out from the local 90th percentile, because
     a vibrating mount spikes past 10 g), the operator turning ≥ 50° then holding, calm and shake
     from gyro speed, handling at the ends, and dark or covered-lens frames from the proxy's luma.
   - Suggestions follow the steps above (hysteresis peaks, 3–12 s, audio lead-in, calm cuts,
     merge) and aim along the camera's heading (follow, yaw 0, Dewarp).
   - App: interest heat in the heading lane (white, spread over all headings since nothing is
     directional yet), dashed ghost spans and tiles, A keeps and X dismisses (persisted per
     folder), a "why" breakdown in the inspector, an interest sparkline per clip, "best moments
     first" sorting, and clip thumbnails taken at the best moment.
   - Cache: `~/Library/Application Support/Reframe/analysis/<name>-<size>-<mtime>.json`
     (`ClipAnalysis.currentVersion`). Signals are stored unweighted, so weights re-rank instantly.
   - Speed: about 0.01× real time (an 88 s clip in 0.7 s; the proxy decode dominates).
   - Known gaps: on selfie-stick clips the follow heading often points away from the people
     (the stick side is behind the camera), which Phase 2's faces fix; dark and covered-lens
     detection are untested because none of the sample clips have such stretches; timelapses
     get no suggestions (motion events are meaningless at compressed time).
2. **Directional map (done, 2026-11-06).** Faces and people, world motion, the heat lane, the
   dynamic-programming camera path, keyframe fitting and lens choice.
   What shipped (`ProxyScan`, `DirectionalAnalysis`, `HeadingGrid`, `CameraPath`):
   - One sequential proxy decode feeds everything. The detectors see six world-locked horizon
     views (100° rectilinear, 448², every 60°, 2 Hz) through Vision's face and full-body human
     detectors. Motion uses a world-locked equirect (rendered 576×288, box-filtered to 144×72) at
     8 Hz, differenced in the ±35° band. Views use Lock at the clip's first frame, which is
     the heading lane's frame. I-Log is fed without a LUT, and detection works fine on it.
   - Key finding: things fixed to the camera (the operator on a selfie stick, a rider's body, a
     UTV's cab, travel parallax) sweep across *world* headings whenever the camera turns, so
     the persistence damping (persistent faces and people count half) and the motion baseline
     (±4 s running median) are both measured in the camera's own frame (heading relative to
     the follow heading), not per world heading.
   - Moments come from time-only interest plus the best direction's interest. Hysteresis is
     measured above the clip's 30th percentile, so people who are always around don't stretch
     every shot to 12 s. Paths use λ = 0.04 per 5° bin, ≤ 40°/s, a small pull toward where the
     camera faced, 0.8 s smoothing, Douglas–Peucker at 4°, and a single still keyframe when
     the path wanders less than 12°. Pitch is 0.7 × the mean pitch of nearby people, clamped
     to −20…15°. The lens is Mega when interest spans ≥ 100° or a face is ≥ 14° tall (arm's
     length), otherwise Dewarp. Suggested shots use Lock at the clip's first frame.
   - App: 2D heat in the heading lane (pre-rendered per clip), previews that follow the
     suggested path with outlined keyframes, K on a preview keeps it and keyframes there, and
     hover reads "2 faces, laughter" for that exact spot.
   - Speed: 0.03–0.07× real time; Vision dominates. Cache v2 stores heading grids as 8-bit
     base64 (a 6-minute clip is ~300 KB).
   - Checked with `reframe-cli analyze --stills` contact sheets: UTV clips frame the people in
     the cab (and the other UTV), arch clips frame the group, selfie clips frame the faces.
   - Known gaps: on the handlebar bike clip every suggestion frames the rider, because the rider
     is the only person there and persistent people still count half (kept deliberately; see
     question 3). Linear is never chosen until saliency lands (Phase 3). A subject right behind the
     start heading wraps across the lane's top and bottom edges.
3. **Refinement (done, 2026-11-06).** Saliency, novelty, weight presets, the hover explanations
   and the disk cache.
   What shipped:
   - **Salience** ("stands out", directional): spectral residual (Hou & Zhang) on the same
     world-locked 128×64 equirect the motion signal uses, at 2 Hz, over −30…+50° pitch. Chosen
     over Vision's attention saliency because Vision scales each view on its own, so it can't say
     which *direction* stands out; one 360° FFT can, and it wraps at ±180° with no seam. It costs
     about 0.02–0.2 s per clip. Each frame counts only what sits above its median heading, and the
     camera-fixed part is damped by 80% (a roll cage or the rider isn't scenery).
   - **Novelty** ("new scenery", time-only): total variation between 4×4×4 RGB histograms of
     the horizon band now (±2 s) and 3–12 s earlier. On these short clips, each shot in
     one place, it barely fires (a bump near the end of 016); footage that travels between places
     should exercise it more, and its threshold is untuned.
   - **Linear lens** when a suggestion is scenery-led (stands out, new scenery, steady or a camera
     turn leads) and nobody is along its path.
   - **Timelapses** get suggestions: motion now runs on them (the camera-relative baseline absorbs
     the constant churn), so the dinner-table timelapse's moments come from movement and faces.
   - **Presets in the app:** Blend, People, Action and Scenery in the inspector's Suggestions
     section re-rank every clip instantly from cached analysis (persisted; the library ranking
     and best moments follow).
   - Cache version 3.
   - Known gap: on the handlebar bike clip the camera's "ahead" points at the rider, so with
     nothing standing out the path settles on the rider even under Scenery. Fixing it needs the
     direction of travel (for example, the focus of expansion of the optical flow).
4. **Optional semantics.** CLIP-style embeddings for text search ("the dog jumping"), and Claude
   writing descriptions and notes for exported selects.

## Questions for you

1. In your footage, what makes a moment worth keeping: people and reactions, action, or scenery?
   This sets the default weights.
2. How long should suggestions be? Phase 1 assumes 3–12 s.
3. On selfie-stick clips, should suggestions favour you and the group (the stick side) or what
   you were pointing at? **Answered 2026-11-06:** keep counting the operator (or a rider fixed to
   the camera) at half weight, so they're framed when nothing else is going on but anything new
   wins.
4. Will you mark your real picks on 5–10 clips as ground truth? It's the fastest way to tune this
   honestly.
