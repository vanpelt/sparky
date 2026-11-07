# reframe

A pre-editor for Insta360 X6 360° footage: browse clips, look around with gyro stabilization,
and (next) export flat reframed shots for Final Cut / Resolve. Rendering goes straight from
the two fisheye streams using the camera's factory calibration (one resample, no equirect
intermediate). Pixels stay in the camera's encoding, so I-Log footage stays log; LUTs are a
preview look only.

Two implementations share the same math:

- **Swift app + ReframeKit** (`Package.swift`, `Sources/`): the product. Metal renderer,
  hardware decode, SwiftUI library and viewer.
- **Python** (`insv.py`, `motion.py`, `render.py`, `stabcheck.py`): the reference and test bench.
  The Metal renderer matches `render.py` at 51 dB PSNR.

## App

```bash
./bundle.sh                # build .build/Reframe.app (release) and open it
swift run reframe-cli info clip.insv
swift run reframe-cli still clip.insv 20 out.png --yaw 180 --hfov 100 [--equirect] [--proxy] [--lut x.cube]
```

Or open the folder in Xcode (it reads `Package.swift`) and run the `Reframe` scheme. Builds
need full Xcode; if `xcode-select` points at the Command Line Tools, set
`DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer` (`bundle.sh` does this). The Metal
shader compiles at runtime, so Xcode's optional Metal Toolchain download isn't needed.

- **Library:** your Insta360 Cloud recordings, plus a footage folder only if you open one (File →
  Open Footage Folder, ⌘O; Close Footage Folder to go back to none).
  Each clip shows a levelled 360° thumbnail rendered from its LRV proxy, plus badges for the
  color profile, timelapse, frame rate and how shaky it is (from the gyro).
- **Viewer:** drag to look, scroll or pinch to zoom, space to play, ←/→ to step a frame.
  Flat (16:9) or 360°; in 360°, click to aim the flat camera. Stabilization: Off / Follow / Lock.
  Scrubbing decodes from the proxy; playback streams the 8K master in real time
  (both 3840² 10-bit lens tracks at 48 fps on an M6).
- **Timeline lanes:** a **filmstrip** of frames seen through the current framing (or the active
  shot's keyframed path), so you can judge what an export will look like; and a **heading lane**
  where x is time and y is direction relative to where the camera faced at the start (Left /
  Ahead / Right / Behind). The line is where the camera looks. Click the lane to jump there *and*
  look there; drag either lane to scrub. The ribbon underneath is gyro speed, so calm stretches
  are easy to spot. The heading lane also carries the interest heat (below).
- **Audio:** the clip's audio plays in sync and acts as the playback clock (video frames are paced
  to it). M mutes.
- **Shots:** N starts a 5 s shot at the playhead, I and O set its ends, or Shift-drag across the
  heading lane to mark one; drag a shot's edge in the lane to trim it. Inside a shot, changing
  the view (drag, zoom, lens, sliders, or clicking the lane) edits the keyframe under the
  playhead, or adds one there. Click a keyframe (its diamond in the lane or its row in the
  inspector) to select it, drag a diamond to move it in time, and press Delete to remove it. K pins
  the current view as a keyframe. Between keyframes the camera eases along a smooth path. ⌘Z / ⇧⌘Z
  undo and redo shot edits. Shots are saved per footage folder in
  `~/Library/Application Support/Reframe/projects/` (the footage folder is never written).
- **Search (⌘F; SEMANTIC.md):** describe a moment ("a waterfall", "riding bikes") in the toolbar
  field. Clips become the matching clips, best first; opening one jumps to its best match and looks
  that way; the heading lane shows where the query is, suggested shots aim at it, the inspector
  lists the matches (⌘G / ⇧⌘G step through them), and Shots matches what each shot looks at. Turn
  it on in **Settings › Search**, which downloads Apple's MobileCLIP-S2 (~200 MB, on-device) and
  indexes clips in the background (about 2 s per minute of footage).
- **Settings › Storage (⌘,; also Manage Storage… in the cloud menu):** what Reframe keeps on disk
  as a two-ring chart (inner: proxies, originals, gyro and metadata, analysis, search index and
  models, shots and settings;
  outer: the clips in each; hover to read a segment) with sizes, how much is cloned from Insta360
  Studio's or your own copies (removing that frees less), and confirmed ways to clear the
  analysis cache or search index, remove the search models, turn downloaded originals back into trailer stubs, or remove cached cloud clips
  (only those without shots, or all; shots are always kept). `reframe-cli storage
  [--remove-originals]` prints the same report.
- **Shots view (⌘2; ⌘1 back to clips):** every shot you've made, local and cloud, as a contact
  sheet grouped by recording day (or newest / longest first, filterable by name or clip). Hover a
  card to scrub through the shot; the ticks along its bottom are keyframes. Each card shows the
  length, lens, stabilization, clip and time range, a cloud badge, and a sparkle if it was kept
  from a suggestion. Click to select (⌘-click, Shift-click, ⌘A for more); double-click the picture
  (or Return) to open it in the editor at its in point; Delete to delete (undoable). Double-click a
  shot's name (or choose Rename) to rename it, or use the inspector's Name field. With shots
  selected, Export exports just those.
- **Export (⌘E):** ProRes 422 HQ / 422 / LT, 10-bit 4:2:2, at HD, 4K UHD, vertical or square,
  with 24-bit source audio and a time-of-day timecode track. Each frame is rendered straight into
  the encoder's 10-bit 4:2:2 Y'CbCr buffer, with no intermediate RGB file. Files are named
  `<recording date and time> <shot name>.mov`. Optional timelines:
  - **DaVinci Resolve:** OpenTimelineIO (`.otio`). Import it with File › Import › Timeline.
  - **Final Cut Pro:** FCPXML 1.10, with an event of selects and a project in shot order.
  Exports stay in I-Log unless you choose to bake the preview look in.
- **Send to Insta360 Studio** (in the export sheet): writes the shots as a new Studio timeline
  project over the original `.insv` files, with the framing as keyframes, and renders nothing.
  Studio must be closed while its project list is written, so Reframe asks to quit it and reopens
  it afterwards. You choose the project name (a taken name gets " 2") and the timeline's frame
  rate (24 fps by default). Headless: `reframe-cli studio <footage-folder> <name> [--register]
  [--fps 24]`. See "Insta360 Studio projects" below for the format.
- **Suggested shots (interest map):** each clip is analyzed in the background from its audio,
  gyro and proxy frames: faces and people (Vision, on six world-locked views), movement in the
  scene, laughter, cheering and speech (Apple's sound classifier), jumps and impacts, and junk
  (dark, covered lens), plus scenery that stands out and arriving somewhere new. It runs at
  3–7% of real time and is cached (`~/Library/Application Support/Reframe/analysis/`, keyed by
  file name, size and modification time; entries for older versions of a loaded clip's file are
  pruned at launch). The inspector's Suggestions section picks what to favour (Blend,
  People, Action or Scenery) and re-ranks every clip instantly. The heading lane shows interest as heat
  over time *and* direction. Suggested shots appear as dashed spans and tiles with their reasons;
  each comes with a camera path (keyframes that follow the people or action, plus pitch and
  lens). Click one to preview it along its path, then press A to keep it as a normal shot, X to
  dismiss it, or K to keep it and add a keyframe. Hover the lane to see what's at a spot ("2 faces,
  laughter"). The sidebar shows an interest sparkline per clip and can sort each day by best
  moments first. `reframe-cli analyze <clip or folder> [--json] [--heat] [--stills dir --lut x.cube]
  [--preset people|action|scenery]` prints every signal and suggestion and renders each
  suggestion's peak frame. `reframe-cli evaluate <footage folder>` scores every preset against the
  shots you kept and the suggestions you dismissed (recall, aim, keep rate). See `INTEREST_MAP.md`.
- **LUTs:** reads `.cube` files from `~/Movies/X6_I-Log_LUTs` (File → Choose LUT Folder). Keep
  them out of Downloads, Documents and Desktop: macOS asks to allow those after every rebuild.
  They apply to the preview of I-Log clips only and are never baked into exports unless you ask.
  The LUT files are Insta360's and are not in the repo.

- **Lens presets:** Insta360 Studio's reframing lenses, as field of view and distortion:
  Mega (80°, 0.95), Ultra (69.5°, 0.95), Dewarp (88°, 0.40, the default) and Linear (110°, 0).
  Distortion is the unified camera model's ξ: 0 is rectilinear, and values toward 1 approach
  stereographic. So Mega actually covers about 155° across, Ultra 135°, Dewarp 120° and Linear 110°.
  Keyframes store the lens, so a shot can ease from Dewarp to Mega. Scroll or pinch fine-tunes
  the angle (shown as Custom).

Keys: Space play/pause, ←/→ step a frame (⇧ for 10), N new shot, I/O in/out, K keyframe, Delete remove
the keyframe at the playhead, ⌘Z/⇧⌘Z undo/redo,
A keep a suggested shot, X dismiss it,
1–4 Mega/Ultra/Dewarp/Linear, F toggle frame/360°, Esc leave the shot, ⌘E export, ⌘O open folder.

`reframe-cli export clip.insv 14 20 out/ --yaw 180 --hfov 100` exports one shot headlessly. On an M6,
1080p exports run at about 190 fps (4× real time). Exported frames match the Python reference at
45–47 dB PSNR, including ProRes compression.

Debug environment variables, used to check the app without screen recording. Launch through
LaunchServices (`open -n -F --env KEY=value --stderr log.txt .build/Reframe.app`); executing the
binary inside the bundle directly can leave the app without a window.
`REFRAME_SELECT=<name substring>`, `REFRAME_PROJECTION=equirect`, `REFRAME_YAW=<deg>`,
`REFRAME_SEEK=<s>`, `REFRAME_AUTOPLAY=1`, `REFRAME_DEMO_SHOT=1` (a keyframed shot at 0:12–0:30),
`REFRAME_SUGGESTION=1` (preview the best suggestion; `=last` the latest; `=accept` keeps the best),
`REFRAME_PRESET=people|action|scenery` (this launch only), `REFRAME_SELFTEST=1`
(keyframe editing, trims and undo, PASS/FAIL on stderr, undone afterwards),
`REFRAME_EXPORT_SHEET=1`, `REFRAME_EXPORT_AUTOSTART=1`, `REFRAME_BROWSE=shots` (with
`REFRAME_SHOTS_SELECT=<n>` to select the first n), `REFRAME_OPEN_SHOT=<i>` (Open in Editor on the
i-th shot, reports where the viewer lands), `REFRAME_RENAME_TEST=1` (renames the first shot and
back), `REFRAME_FOCUS_TEST=1` (a click on the viewer and Return must take focus off the inspector's
Name field), `REFRAME_SETTINGS=1` (opens Settings; the snapshot then captures it, `REFRAME_SETTINGS_TAB=search|storage`
picks the tab, `REFRAME_STORAGE_HOVER=<kind>` highlights a segment), `REFRAME_SEARCH=<query>` (types a
search; `REFRAME_SEARCH_ON=1` also turns search on), `REFRAME_GESTURE_TEST=x,y;x,y` (with `REFRAME_BROWSE=shots`: double-clicks the first
card's name and picture at those points), `REFRAME_DEBUG=1` (playback fps and window
state to stderr), `REFRAME_SNAPSHOT=/path.png` (window chrome, plus `-viewer.png` with the Metal
view and `-sheet.png` for an open sheet). See `DESIGN.md` for the design plan.

## Insta360 Cloud

Recordings in Insta360 Cloud sit in the sidebar beside the open folder's, on the same days, with a
cloud badge. When the open folder has the same recording, the cloud entry wins (shots always
reference the cloud); the local copy is used instead of the network to fetch it and as its
original for export. Likewise, when Insta360 Studio is installed and has already downloaded a
recording (it records them in its `download.db`), Reframe clones Studio's copy instead of
fetching (`reframe-cli cloud studio` lists them). Studio saves to `~/Downloads/Insta360 Download`
by default, which makes macOS ask Reframe for Downloads access; pointing Studio's "Cloud file
download path" at e.g. `~/Movies/Insta360 Download` avoids that. Connect with the cloud button at the bottom of the
sidebar: Insta360's own sign-in page opens and you sign in there; Reframe keeps only the session
token (30 days, renewed automatically).

Selecting a cloud recording fetches just enough to work on it: the original's trailer (gyro,
calibration, metadata; a few to ~25 MB) and the low-res LRV proxy (~6% of the original). Playback,
analysis, suggestions and shots then work as for local clips, from the proxy. Exporting files needs
the originals: the export sheet lists the clips still in the cloud and downloads them first (also in
a cloud clip's context menu). Send to Studio doesn't: it references them as cloud media, which
Studio streams. "Fetch All Cloud Clips" fetches every proxy in the background.

Cache: `~/Library/Application Support/Reframe/Cloud/<mediaId>/`; cloud clips' shots are kept in
their own store, so they don't depend on the open folder. `reframe-cli cloud migrate [--dry-run]`
moves shots saved against local files over to the same recordings in the cloud (seeding the cache
from the local copies, no download), backs up each store it rewrites (`*.json.bak-<time>`), and
forgets the saved footage folder once nothing in it has shots (quit Reframe first). This uses the private API Insta360 Studio uses; see [CLOUD.md](CLOUD.md).
`reframe-cli cloud login|status|list|probe|fetch [--original]|logout` drive it from the terminal.

## Python reference

```bash
uv sync
uv run insv.py records clip.insv          # trailer record table
uv run insv.py info clip.insv             # metadata, calibration, gyro summary (JSON)
uv run render.py equirect clip.insv 20 out.png --width 3840
uv run render.py flat clip.insv 20 out.png --yaw 180 --pitch -10 --hfov 100   # 1920x1080
uv run render.py lenses clip.insv 20 raw  # raw fisheye frames per track
uv run render.py clip clip.insv 6 out/shot.mov --duration 6 --stab follow --yaw 180 --hfov 100
uv run stabcheck.py clip.insv 8 --yaw 20 --pitch 10   # residual-motion score per stabilization variant
```

Requires `ffmpeg` on PATH. Stills are 16-bit RGB PNGs. Clips are ProRes 422 HQ 10-bit with
24-bit PCM audio, tagged Rec.709; the pixels are still I-Log, so apply Insta360's X6 I-Log LUT
in the editor. Render output goes in `out/` (gitignored).

Stabilization modes (`--stab`):

- `none`: the view is fixed to the camera body.
- `follow`: horizon level, heading follows the camera's heading smoothed over `--smooth` seconds (default 0.8).
- `lock`: horizon level, heading held where it was on the first frame.

Rolling-shutter correction is on by default (`--no-rs` disables it).

## Insta360 Studio projects (Studio 6.0.8)

Studio has no scripting interface, but its timelines are plain JSON:
`~/Documents/Insta360/Studio/TimelineProject/<name>/<uuid>.insproj`, listed in
`~/Library/Application Support/Insta360/Insta360 Studio/nle/pro.insproj` (Studio rewrites that list
while running; Reframe keeps the original as `pro.insproj.reframe-backup`). Measured against
projects Studio saved and frames matched against Reframe renders:

- Every frame number counts at the project's rate, whatever each clip was shot at: a clip has
  `floor(duration × project fps)` frames (Studio showed a 48 fps clip's 342 frames as 11.4 s on a
  30 fps timeline). With `autoFps` on, Studio takes the first clip's rate (48 fps → 24, 29.97 → 30);
  Reframe writes the rate chosen in the export sheet (24 by default, `--fps` in the CLI) with
  `autoFps` off. Trims are frames cut from each end; keyframe times are source frames.
- Keyframe `fov` is the vertical field of view of the 16:9 frame (radians) and `distance` is
  Reframe's lens distortion, so Mega/Ultra/Dewarp/Linear carry over exactly.
- Keyframe pan is a **world direction**: left-positive, zero where the camera faced on the file's
  first frame, interpolated in a straight line between keyframes. Reframe converts with
  `pan = view-base heading + yaw − heading at t=0` (within 2° on six test frames) and adds
  keyframes wherever the camera turns enough that Studio's interpolation would drift from the
  shot by more than 2°. Lock and Off shots export as plain Studio stabilization.
- Not handled yet: clip transitions (no saved example), aspect ratios other than 16:9, cameras
  other than the X6.

## What we know about X6 files (firmware v1.2.36)

- One `.insv` per clip: two 3840×3840 HEVC Main10 tracks (one per lens), AAC stereo, and an
  Insta360 `inst` trailer (ExtraInfo v3, indexed). The `inst` box carries 10 zero bytes before
  the record region, which the reference spec does not mention. AVFoundation opens the file
  directly (via a `.mp4` name).
- Containers are tagged Rec.709 for I-Log/standard clips and BT.2020 HLG for Dolby Vision clips.
  The actual profile is metadata field 22: `I_Log`, `standard`, or `Dolby_Vision`.
- `.lrv` proxy: 2048×1024 8-bit HEVC, both fisheyes side by side; 24fps (or source fps for timelapse).
- Calibration: V6 string in fields 111/112 (unified omni + 5 radial, 4 tangential, 4 thin-prism
  terms), canvas 15488×7744 for both lenses, lens type 193 bare.
- IMU: packed raw samples at ~1 kHz, ±32 g / ±2000 °/s. Gyro time maps to video time via
  field 24 (first-frame timestamp, µs). Field 28 (gyro adjustment, 1.1 ms) is *not* applied:
  a time-offset sweep was steadiest without it.
- Rolling shutter readout 16 ms at 48fps (field 25); rows later in the source image are read later.
- The X5 IMU→body axis mapping from insta360-rs (`body = (y, z, x)`) holds on X6 for both the
  accelerometer and gyro: the best of all 48 signed axis permutations on two clips.

## Stabilization results

Residual motion from `stabcheck.py` (median optical flow, px/frame at 640px wide; hike selfie clip
`125702`, 3 s from 0:08, world-locked view of the rock wall):

| Variant | px/frame |
| --- | ---: |
| none (camera-fixed) | 13.7 |
| gyro lock | 2.0 |
| gyro lock + rolling-shutter correction | 1.05 |
| ... with rolling-shutter sign flipped | 2.9 |
| ... without the field-28 adjustment (default now) | 0.58 |

Orientation is pure gyro integration, levelled offline: the world-frame accelerometer is
smoothed over ±3 s (zero-phase), so braking, cornering and vibration average out instead of
tipping the horizon. Rolling shutter is corrected per row from a 17-step pose table across the
readout. The original causal accelerometer filter (Mahony) left a median ~8.5° of horizon error
on a handlebar-mounted clip.

"Jitter" (`stabcheck.py --stab-study`) is the high-frequency part of the global image motion in
the upper half of a follow-mode view, with smooth pans and forward travel removed. RMS px/frame
at 640 wide, 4 s:

| Variant | Bike (handlebar) | UTV (selfie stick) |
| --- | ---: | ---: |
| none (camera-fixed) | 2.52 | 2.44 |
| Mahony tilt, linear rolling shutter (previous) | 0.65 | 0.62 |
| offline tilt, linear rolling shutter | 0.19 | 0.41 |
| **offline tilt, per-row rolling shutter (current, Swift and Python)** | **0.10** | **0.41** |

The viewer presents frames at the clip's own rate while playing (48 Hz on a ProMotion display),
so a 48 fps clip doesn't judder on a 60 Hz cadence.

## Known limits
- Seam blending is a plain angular feather: near objects ghost and the two lenses' exposures
  do not match across the seam. Views that stay inside one lens look clean.
- Not yet compared head to head with FlowState on the same clip (needs a Studio export).
- Only rotation is stabilized. Hand/vehicle translation and its parallax remain, as with FlowState.
- Levelling assumes accelerations average out over ±3 s. A sustained turn longer than that
  (a long banked curve) can still tilt the horizon slightly.
- Python/numpy renderer runs ~0.4 s per 1080p frame and is for validation only; the real
  pipeline is planned as Metal + VideoToolbox.

Format reference: [insta360-rs `docs/INSV_FORMAT.md`](https://github.com/s00500/insta360-rs) (Apache-2.0).
