---
name: studio-edit
description: Cut Insta360 360° footage into a finished video by reading and rewriting Insta360 Studio timeline projects (.insproj) with reframe-cli — see the shots as contact sheets, find the music's beats and sections, reorder/trim/re-aim clips, and cut on the music. Use when the user wants to assemble, review, re-order, re-time or re-aim an edit in Insta360 Studio, build a 1–5 minute video from their Reframe selects, or cut a timeline to music.
---

# Editing Insta360 Studio timelines

Insta360 Studio (6.x) has no scripting API, but its timelines are JSON files that `reframe-cli`
reads, renders and rewrites. Studio stays the place for stitching, stabilization, colour and the
final export; this skill does the assembly: which shots, in what order, how long, aimed where,
cut to the music.

## Setup

```bash
cd tools/reframe && swift build --product reframe-cli   # binary: tools/reframe/.build/debug/reframe-cli
```

Below, `rc` means `tools/reframe/.build/debug/reframe-cli`. For I-Log footage add
`--lut /Users/vanpelt/Downloads/X6_I-Log_LUTs/X6_I-Log_to_Rec709_BT1886_s33_v2.cube` to `frame`
and `sheet` so previews aren't flat (skip it if the file is gone).

## Commands

| Command | What it gives you |
|---|---|
| `rc studio list` | Studio's projects, newest first |
| `rc studio dump <project>` | Clips in order (numbered `#1…`), timeline and source times, keyframes, music, gaps, flags |
| `rc studio dump <project> --spec` | The timeline as an edit spec: the starting point for `apply` |
| `rc studio sheet <project> <dir or .png> [--clip N] [--per-clip 3] [--width 400]` | Contact sheets: each clip's frames through Studio's keyframes |
| `rc studio frame <project> <time> <out.png>` | One frame at a timeline time |
| `rc studio beats <project or audio file>` | BPM, bar times, loudness per bar, section changes (in timeline time) |
| `rc studio apply <edit.json> --dry-run` | Shows the resulting timeline without writing |
| `rc studio apply <edit.json> --as "Name" --root <scratch dir>` | Writes a test copy anywhere, Studio untouched (then `sheet` it) |
| `rc studio apply <edit.json> --as "Name" [--quit-studio] [--open]` | Writes a new Studio project and adds it to Studio's list |
| `rc studio handles <cut> [--seconds 1] [--quit-studio] [--open]` | Writes "<cut> handles": every clip widened (default 1 s each side), no music, no effects, camera audio 100 %, plus `reframe-conform.json` |
| `rc studio conform "<cut> handles" <export.mov> <out dir>` | Splits Studio's single-file export of it into one file per clip (frame-exact) + `<cut>.fcpxml` and `<cut>.otio` |
| `cd tools/studio-speech && uv run transcribe.py <out.json>` | Whisper transcripts (word times) of every cloud recording Reframe heard speech in |
| `python3 tools/studio-speech/cliplog.py <dump.json> <transcripts.json> [N...] [--words]` | Per clip: lines spoken in or near its trim, an interest sparkline and its peak |

`<project>` is a project name, its folder or its `.insproj`. Times are seconds or `m:ss.s`.
Previews render Studio's keyframes with Reframe's renderer from the low-res proxies: framing and
timing match what Studio shows (verified), stitching and colour don't. Look at the sheets with the
Read tool; they're how you see the footage.

## Edit spec

```json
{
  "project": "Selects",
  "snap": "beat",
  "duck": 25,
  "clips": [
    {"from": 5, "volume": 35, "note": "b-roll: camera audio low"},
    {"from": 2, "in": "0:24", "duration": 5},
    {"from": 3, "adjust": {"pan": 20, "fov": -10}},
    {"from": 9, "in": 69.6, "out": 73.2, "volume": 130, "music": "duck", "snap": false,
     "note": "'That is a crazy drop.'"},
    {"from": 41, "in": 0.4, "out": 9.95, "volume": 130, "music": "off", "snap": false},
    {"from": 1, "in": 230, "out": 236,
     "keyframes": [{"at": 0, "pan": -40, "tilt": -5}, {"at": 6, "pan": 10, "tilt": -5, "fov": 70}]},
    {"from": 4, "project": "Other selects"}
  ],
  "music": [
    {"from": 1, "in": 0, "atClip": 1, "untilClip": 4, "volume": 90, "fadeOut": 1},
    {"from": 1, "in": 46.6, "atClip": 6, "fit": true, "fadeIn": 0.5}
  ]
}
```

- `clips` is the whole new timeline, in order, packed end to end. Clips left out are dropped;
  a clip may appear twice. `from` is the `#` from `dump` of `project` (or of the clip's own
  `project`, which must have the same fps). Untouched settings (stabilization, colour, Studio's
  caches) carry over.
- `in`/`out` are real seconds into the SOURCE file (what `dump` shows as "source"); `duration`
  (timeline seconds) can replace `out`. Omitted, the clip keeps its current trim. On a clip with a
  speed change (`dump` shows "0.25× speed") the two differ: `"in": 1, "duration": 8` on a 0.25×
  clip uses 2 s of source. Speed itself is set in Studio; `apply` keeps it. Moving `in`/`out` beyond the original
  shot is fine; keyframes outside the trim are kept and Studio holds the nearest one.
- `keyframes` replaces the clip's camera path. `t` is source seconds, `at` is seconds from the clip's
  start. `pan` is degrees, left-positive, from where the camera faced on the file's FIRST frame
  (a world direction, not relative to the camera). `tilt` is up-positive. `fov` is horizontal
  degrees and `distance` is lens pull-back; both default to the clip's current view. Lens presets
  (fov/distance): Linear 110/0, Dewarp 88/0.40, Mega 80/0.95, Ultra 69.5/0.95. Studio interpolates
  linearly between keyframes in world space, so a long pan with few keyframes is a steady sweep.
- `adjust` offsets every keyframe of the clip (pan/tilt/fov degrees): use it to re-aim a shot
  without losing its move.
- `volume` is the clip's own camera audio (percent, up to 200). Keep b-roll low (≈35) so wind
  doesn't fight the music, dialogue high (≈130), slow motion at 0.
- `music` is how loud the music is under the clip: `"duck"` (the edit's `duck` percent, default
  25), `"off"`, `"full"` or a percent. Studio has no volume keyframes on music, so `apply` splits
  each music clip into pieces at those clip boundaries; the level steps on the picture cut.
- `snap: false` keeps a clip's out-point exactly as given (dialogue); `note` is ignored.
- `music`: omit it to keep the project's music as it is, `[]` removes it. `from` is the music
  clip's number in its project; one song can appear several times (`in` picks up where you want).
  Place a piece by `atClip` (starts with that clip of `clips`, 1-based) rather than `start`
  seconds, and end it with `untilClip` (ends with that clip), `fit: true` (the video's end) or
  `out`; clip-anchored music survives re-timing. Leave clips between two pieces with no music for
  a narrative stretch. `fadeIn`/`fadeOut` are seconds (Studio's `currentFadeIn/Out`; we write
  frames, which is unconfirmed). Music can only come from a project that already has it (add a
  track in Studio first). Pieces must not overlap; `apply` warns.
- `snap`: `"bar"` or `"beat"` moves each clip's out-point to the nearest bar line or beat of
  whatever music is playing there, so cuts land on the music. Use `"beat"` for a light touch
  (moves ≤ half a beat). Warnings report big moves. Without `snap`, cut where you choose.

Unknown to `apply` (do these in Studio, after): transitions (`apply` drops any it finds and
warns), speed ramps, titles, other aspect ratios.

## Workflow

1. **Find the material.** Run `rc studio list`. The user's selects are usually a project made
   by Reframe's "Send to Studio" (or by `rc studio <footage folder | cloud> "<name>" --register`).
   `dump` it, then `sheet` it (`--per-clip 3`) and look at every page. Write yourself a shot log:
   for each `#`, what it shows, how strong it is, its camera move, and which moments are best.
   For a closer look at a shot's motion, use `sheet --clip N --per-clip 8`.
2. **Hear what's said.** `cd tools/studio-speech && uv run transcribe.py <scratch>/transcripts.json`
   (needs `reframe-cli analyze` to have run on the recordings; it's cached), then
   `python3 tools/studio-speech/cliplog.py <dump.json> <transcripts.json> --words`. Note the lines
   worth keeping (jokes, reactions, a reflective line for a cold open) with word times: cut 0.2–0.3 s
   outside the first and last word, and extend `in`/`out` past the user's trim if the line runs
   over (check the framing there on a sheet: keyframes outside the trim are held). Whisper
   occasionally mishears or hallucinates on wind; prefer lines with clear words.
3. **Read the music.** `rc studio beats <project>` gives the bar grid and a loudness curve. Map the
   sections (intro, build, drop or peak, breakdown, finale, outro) to timeline times. If there's no
   music yet, ask the user to add a track in Studio and save, then re-dump.
4. **Plan the story** before writing JSON, and share it with the user as a short list of sections
   with shots. For a 1–5 minute piece, a good default:
   - **Hook (first 3–8 s):** the single most striking moment, not the chronological first shot.
   - **Set the scene:** wide shots that establish place and people.
   - **Build:** rising energy; shots get shorter as the music lifts.
   - **Peak:** the best action on the loudest section, cut quickly (about 1–2 bars per shot).
   - **Breather:** on quiet bars, hold longer and calmer shots (3–6 bars): faces, landscape.
   - **Finale and close:** a strong group or landscape shot, held to the end of the music.
   Longer pieces: chapter by day or place; drop the music entirely for a narrative stretch (a
   guide's story, a joke) and bring it back after; duck it under every real line of dialogue;
   alternate fast jump cuts (1.5–3 s, several snippets of one shot) with slower, reflective runs
   (4–6 s). A spoken line over a wide shot makes a good cold open. Speech clips go in with
   `"snap": false`; shouts can sit on full music with the camera audio up.
   Cut on beats or bars. Alternate wide and close, and people and landscape. Don't put two
   cuts from the same recording back to back unless the time jump is the point. Keep the
   direction of motion consistent across a cut. Begin a shot just before its action, not at the
   start of the clip. Total length follows the music section you're using; trim the music with
   `in`/`out` and `fit`.
5. **Write the spec** to a file (start from `dump --spec`), `apply --dry-run`, fix any warnings,
   then write a test copy with `--root <scratchpad dir> --as "<name>"` and `sheet` it. Check the
   cut points and framing with your own eyes. Iterate.
6. **Hand it to Studio.** Writing into Studio needs Studio quit, since it rewrites its project list
   while running. Ask the user before quitting it (unsaved work gets a save prompt); then
   `rc studio apply edit.json --as "<name>" --quit-studio --open`. Never write over the user's
   selects: always use `--as` with a new name (`--replace` only for a project this skill created,
   and only after asking).
7. **Hand off to Resolve / Final Cut** (for grading the log footage, finishing the mix):
   `rc studio handles "<cut>"` (after the user saves; ask before quitting Studio), then the user
   exports "<cut> handles" from Studio as ONE file — Export Mode single, ProRes 422, Rec.709
   10-bit, project frame rate, no LUT or Color Plus — and you run
   `rc studio conform "<cut> handles" <export.mov> <out dir>`. It refuses an export whose
   length doesn't match. Out: `clips/NNN VID_xxx.mov` (ProRes copy + 24-bit PCM, with handles),
   `music/`, and timelines trimmed back to the cut: FCPXML (camera levels as `adjust-volume`,
   music as connected clips with its levels and fades) and OTIO (levels only as metadata).
   Resolve: File › Import › Timeline › the .fcpxml. Levels come through; fades don't. From there
   the resolve-edit skill takes over (LUTs on the log clips only, `music fades`, song swaps).
   Don't use Studio's "Multiple Clips" export: it has no handles and stopped after 10 of 96 clips.
   Prefer Studio's Local Export (ProRes). With cloud footage Studio may route to Cloud Export
   (H.264/HEVC around 50 Mbps): `conform` then re-encodes each clip to ProRes 422, which is
   frame-exact but can't restore the grading room the compressed export lost.
8. **Round trip.** When the user tweaks the edit in Studio (re-aims, trims), they need to save; then
   `dump` again and build further edits from what Studio saved, not from your earlier spec.
   Without `--as`, `apply` updates a project in place and keeps the previous version in
   `<project folder>/.reframe-history/`.

## Gotchas

- `dump` reads the file as last saved. When Studio is running, say so and ask the user to save if
  they've been editing.
- Numbers (`from`) refer to the project as it is now. Re-dump after every write before referencing
  clips of the new project.
- The beat tracker can land on half or double the real tempo (e.g. 74 versus 147 BPM). If bars
  feel too long or short for the music, snap to `"beat"` instead, or ask the user.
- Clips whose originals aren't on this Mac (Insta360 Cloud) show "no local file" in `dump`; they
  only preview if Reframe has fetched their proxy.
- Studio's `first_frame.jpg` thumbnail can be stale; don't use it as a reference.
- Studio projects can carry effect tracks (type 6, e.g. "Fade out"); `dump` lists them as
  `effect`, `apply` keeps them at their old times (warns), `handles` drops them.
- Some proxies have no audio track (the night-time card game); those can't be transcribed
  without the full original, so treat them as picture only.
- A shot whose line matters but whose framing wanders: give it `keyframes` aimed at the speaker
  (check with `sheet --clip N`) — 360 clips often look away from whoever is talking.
- Footage is usually I-Log; previews use the LUT, the project doesn't. Grading happens later.

## Known gaps (worth building)

- `transcribe.py`/`cliplog.py` should become `rc studio speech` / `rc studio log` so the whole
  loop is one tool; the interest curve is a rough re-implementation of Reframe's fused score.
- No J/L cuts: a clip's audio can't run under the next picture (Studio's `audioDetached` is
  unexplored), so a line plays only over its own shot.
- Music ducking steps at the cut; a short fade on each piece would be smoother.
- Fade units and transition JSON are unconfirmed in Studio.
- `conform` doesn't carry Studio's effect clips (a "Fade out") or transitions; redo them in
  Resolve. Resolve reads the FCPXML's audio levels but drops its fades (`resolve.py music fades`
  puts them back).
