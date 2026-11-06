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

- **Library:** opens `~/Downloads/Insta360 Download` by default (File → Open Footage Folder, ⌘O).
  Each clip shows a levelled 360° thumbnail rendered from its LRV proxy, plus badges for the
  color profile, timelapse, frame rate and how shaky it is (from the gyro).
- **Viewer:** drag to look, scroll or pinch to zoom, space to play, ←/→ to step a frame.
  Flat (16:9) or 360°; in 360°, click to aim the flat camera. Stabilization: Off / Follow / Lock.
  Scrubbing decodes from the proxy; playback streams the 8K master in real time
  (both 3840² 10-bit lens tracks at 48 fps on an M6).
- **LUTs:** reads `.cube` files from `~/Downloads/X6_I-Log_LUTs` (File → Choose LUT Folder).
  They apply to the preview of I-Log clips only and are never baked into exports. The LUT files
  are Insta360's and are not in the repo.

Debug environment variables, used to check the app without screen recording:
`REFRAME_SELECT=<name substring>`, `REFRAME_PROJECTION=equirect`, `REFRAME_YAW=<deg>`,
`REFRAME_AUTOPLAY=1`, `REFRAME_DEBUG=1` (playback fps to stderr),
`REFRAME_SNAPSHOT=/path.png` (window chrome, plus `-viewer.png` with the Metal view).

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

Orientation is gyro integration with a weak accelerometer pull toward gravity (Mahony, kp 0.6,
only when |a| is within 15% of 1 g), so roll and pitch cannot drift.

## Known limits
- Seam blending is a plain angular feather: near objects ghost and the two lenses' exposures
  do not match across the seam. Views that stay inside one lens look clean.
- Only rotation is stabilized. Hand/vehicle translation and its parallax remain, as with FlowState.
- The gravity pull assumes accelerations average out. Long sustained turns in a vehicle can tilt
  the horizon slightly; a forward-backward smoother over the whole clip would fix that.
- Python/numpy renderer runs ~0.4 s per 1080p frame and is for validation only; the real
  pipeline is planned as Metal + VideoToolbox.

Format reference: [insta360-rs `docs/INSV_FORMAT.md`](https://github.com/s00500/insta360-rs) (Apache-2.0).
