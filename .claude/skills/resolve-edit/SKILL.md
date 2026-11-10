---
name: resolve-edit
description: Work on a DaVinci Resolve Studio timeline from the command line with tools/resolve/resolve.py — inspect clips, music and levels, render contact sheets and stills from Resolve itself (grade included), set or clear LUTs on chosen clips, restore music fades, swap a song while keeping its placement, ducking and fades, and place audio. Use when the user is editing, grading or mixing in DaVinci Resolve (especially a cut handed off from Insta360 Studio with reframe-cli conform) and wants Claude to look at or change the timeline.
---

# Working in DaVinci Resolve

`tools/resolve/resolve.py` drives Resolve **Studio** through its scripting API. It acts on the
timeline Resolve is showing. Cuts usually arrive from the studio-edit skill's hand-off
(`reframe-cli studio conform`): clip files named `NNN VID_xxx.mov`, `music/`, an FCPXML, and a
`reframe-conform.json` manifest in the Studio project folder that this tool finds by itself.

## The user is working in Resolve while you are

This is the first rule. The user edits in Resolve at the same time as you script it, and the API
changes whatever timeline their screen shows.

- Never switch which timeline Resolve shows, and never leave it on a different one. `DuplicateTimeline`
  switches the view to the copy: switch back in the same breath (`music swap` does, and checks).
- Never delete a timeline or bin without asking, even one you made: the user may have worked in
  it. (It happened: a test copy took over the view, the user edited it for minutes, and the cleanup
  deleted their work.)
- Test risky changes with `--dry-run`, not on a scratch copy. If a copy is unavoidable, tell the
  user before making it and hand the view back immediately.
- Commands that change the timeline need `--timeline "<name>"` and refuse if Resolve shows another.
  Run `status` first to learn the name; don't copy it from earlier in the conversation.
- Before an edit that depends on positions, `dump`/`music` again: the user may have cut, moved or
  ripple-deleted since you last looked.
- Resolve keeps timeline backups in `~/Movies/Resolve Project Backups/<project>/<timeline>/`
  (`Timeline.<yyyymmddhhmmss>`, a 9-byte header and then zstd; the timeline's name is in the first
  strings). Edit › Undo in Resolve also brings back a deleted timeline.

## Setup

Resolve **Studio** (the free edition refuses external scripts, and since 21.1 has no Python in its
Scripts menu either), with Preferences › System › General › External scripting using = **Local**.
The script sets the scripting environment itself:

```bash
cd tools/resolve && uv run resolve.py status
```

Below, `rv` means `uv run resolve.py` in `tools/resolve`. `music swap --snap` and the dry-run's
section list use `reframe-cli studio beats`; build it with
`cd tools/reframe && swift build --product reframe-cli`.

The davinci-resolve MCP server (samuelgursky, 37 compound tools) is installed too and is
registered for Claude Code in this repo (local scope; it loads only in new sessions). Use it for
things this tool doesn't cover: render queue, Fusion, Fairlight details, project settings, markers.

## Commands

| Command | What it gives you |
|---|---|
| `rv status` | Edition, project, timeline, page, playhead, tracks and the music on each |
| `rv dump [--clips SPEC] [--json]` | Per clip: number, timeline time, length, name, camera audio dB, LUT on node 1, extra nodes, and (from the manifest) the Insta360 recording, the second it starts at, and speed |
| `rv sheet [--clips SPEC] [--at in\|mid\|out] [--out f.jpg] [--size 3000]` | A labelled contact sheet rendered by Resolve, grade included (`Timeline.ExportThumbnails`) |
| `rv frame N [--at SEC\|mid] [--out f.jpg]` | One still from clip N |
| `rv lut <name or path> --timeline T [--clips SPEC] [--except SPEC] [--node 1]` | Sets a LUT on node N of the chosen clips and labels the node; `--clear` instead of a name removes it |
| `rv music [list]` | Each song's pieces, grouped into runs that play continuously, with dB and fades |
| `rv music fades --timeline T [--dry-run]` | Puts back the music fades from the conform manifest (Resolve drops FCPXML fades on import) |
| `rv music swap <old song> <new file> --timeline T [--in SEC …] [--snap] [--gain DB] [--dry-run] [--no-backup]` | Replaces a song (see below) |
| `rv place <file> --timeline T --at SEC [--in SEC] (--length SEC \| --until SEC) [--track N] [--db DB]` | Puts a stretch of an audio file on the timeline (a new track by default) |
| `rv render --from SEC --to SEC [--out DIR] [--name N] [--frames 0,24,48] [--audio]` | Renders a stretch through Deliver (1080p ProRes Proxy) and tiles chosen frames into a sheet; puts the page, playhead and render format back. The only way to see Fusion work |

Clip numbers are the `NNN` prefix of conform's file names, so they stay put while the user cuts,
moves and ripple-deletes; clips without a prefix are numbered by position on V1. SPEC is like
`1-20,24,31`. LUT names are matched under Resolve's LUT folder
(`/Library/Application Support/Blackmagic Design/DaVinci Resolve/LUT`); copy new LUTs there
(it's world-writable) and the command refreshes Resolve's list.

Look at every sheet and still you make (Read the image) before describing or changing anything.

## Swapping a song

`music swap Horizon ~/Music/new.mp3 --in 12.5 --in 71 --snap --dry-run` finds every piece of the old
song, groups them into runs, and plans each piece of the new song at the same place and length,
with the same level (so the dips under dialogue stay) and the same fades. Each `--in` is where a
run starts in the new song; later runs without one carry on from where the previous run stopped.
`--snap` moves each `--in` to the nearest downbeat. The dry run prints the new song's tempo and
section changes: put reflective runs on quiet sections and the peak of the edit on a loud one.

Without `--dry-run` it duplicates the timeline as a backup ("<name> (before <song>)"; tell the user
it exists, it's theirs to delete), puts the new song on a new audio track named after it, then
deletes the old pieces. If the new song is too short, the last run stops early (it warns); fade
that end or pick an earlier `--in`.

Picking the song is the user's call: ask for a file, then `rc studio beats <file>` (or the dry
run) to find in-points, and show the plan before running it.

## Grading notes

- X6 I-Log footage: `X6_I-Log_to_Rec709_BT1886_s65_v2.cube` on node 1 (installed under "Insta360
  X6"). High-frame-rate (slow-mo) and timelapse recordings aren't log: no LUT. A 30 fps proxy with
  no audio is a timelapse; 0.25× clips in the manifest came from 120 fps.
- Grade after the LUT node for most things. A node before it (in log) pulls back clipped
  highlights.
- `GetProperties()["AudioVolume"]` is the level in dB (−0.9 ≈ 90 %, −13.2 ≈ 22 %, +2.3 ≈ 130 %).
  `GetProperty("Volume")` returns nothing.

## Building things on the timeline (learned on the Moab title opener)

- **Seeing the result:** `ExportFrame`, `ExportThumbnails` and so `sheet`/`frame` show grades and
  edit sizing but **not Fusion** (titles, comps). Check Fusion work with `rv render`.
- **Fusion comps you add to media clips don't render** on 21.2 (`AddFusionComp` → MediaIn →
  Transform → MediaOut renders as the untouched clip; the MCP authors saw the same on 21.0.4). Text+
  **title** comps do: text, font, keyframes (`AddModifier(input, "BezierSpline")` then
  `SetInput(input, value, frame)`), extra tools (Blur, a second Text+, Merge) all render.
- **`AddModifier(input, "BezierSpline")` also keys the input's current value at the comp's current
  time** (wherever the playhead is, even a negative frame before the title). Read
  `spline.GetKeyFrames()` afterwards and remove strays with `spline.DeleteKeyFrames(start, end)`. A user
  changing a value in the Inspector while a title is animated adds keys the same way.
- **No keyframe API** for edit-page properties (zoom, position, opacity). For a camera move, render the
  plate yourself (keep it ungraded log or Rec.709 so it can still be graded) and keep the parts the
  user will change (text, timing, dissolves, grade) in Resolve.
- **Freeze frame:** append the clip, `SetSpeed({"Percentage": 0})` (holds its first frame), then
  `SetRightOffset(GetRightOffset() - n)` to make it longer than the source.
- **Opening a gap** (ripple insert): `MoveItems(every item on every track starting at or after the
  frame, {"startFrame": frame + gap}, False)` moves them all as a block; check each one afterwards.
- **`InsertFusionTitleIntoTimeline` is an insert edit at the playhead on V1** and ripples everything
  after it; it ignored a playhead just moved by script. Only call it with the playhead past the end of
  the edit, then `SetPosition({"trackIndex": 2, "startFrame": f})` and `SetRightOffset` for length.
- **Transitions:** `AddTransition({"type": "Cross Dissolve", "category": "simple", "position":
  "start", "alignment": "center", "duration": 19})` on the incoming clip; both sides need handles.
- **Grades:** `CopyGrades([items])` copies a clip's grade (LUT and nodes) to others.
- **Rendering** switches Resolve to Deliver and leaves the playhead at the mark-in; `rv render` waits
  and restores. Render settings persist in the project; restore them.
- **Test away from the edit:** a sandbox far past the end of the timeline (frame 30000+) on the same
  timeline avoids switching the user's view; delete it when done (it lengthens the timeline).
- **Text+ sizes:** `Size` is relative to frame width (0.13 ≈ 200 px caps in UHD); `Center` is
  normalized with y up; `CharacterSpacing` 1.0 is normal.

## Learned on Moab Cut v3 (look, credits, inserts)

- **A look for the whole cut without touching every node:** `proj.AddColorGroup(name)`, assign every V1 clip
  (`item.AssignToColorGroup(group)`), then `group.GetPostClipNodeGraph().SetLUT(1, cube)`. It sits after each
  clip's own grade, renders in `ExportFrame`, and is one knob for the user. Titles stay out of the group.
- **A LUT at reduced strength:** blend the cube toward identity yourself (`Rust_60.cube` = 60 %); Resolve's
  node key/LUT mix isn't scriptable.
- **Generators are capped at the 5 s default** through the API: `SetRightOffset`/`SetLeftOffset`/`SetSpeed`
  all refuse on a Fusion title or composition. For a longer title, build a **Fusion clip** from a plate of the
  right length (`timeline.CreateFusionClip([plate_item])`, then `AddFusionComp()` + `ImportFusionComp(path)`),
  wire `MediaOut1` to the text output only (`tool.ConnectInput("Input", other)`; attribute assignment
  `a.Input = b.Output` silently fails), clear its LUT and keep it out of the color group (the grade would tint
  the text), and put it on V2 over the graded plate on V1. Fusion clips render; comps on media clips don't.
- **A template comp's `MediaIn` references the template's own media by MEDIA_ID** and renders as Media
  Offline anywhere else: delete it, `comp.AddTool("MediaIn")` makes one bound to the Fusion clip's media.
  Motion Array's "Supreme" titles key the media's luma for a duotone (`LumaKeyer1` → yellow `Background`
  → `Merge3`); `Merge3.Blend` is the duotone strength.
- **`AppendToTimeline` maps `startFrame` through the clip's start timecode.** A 23.976 fps clip with a
  time-of-day TC (DJI, phones) lands ~1000 frames late by late morning (24 vs 23.976 drift over the TC).
  `mpi.SetClipProperty("Start TC", "00:00:00:00")` first, then frame numbers are 1:1. Check
  `item.GetLeftOffset()` equals what you asked and `ExportFrame(0)` against an ffmpeg frame.
- `AppendToTimeline` with video+audio returns only the video item; reach the audio via
  `item.GetLinkedItems()` to set its level and fades.
- **Ripple insert of a shot:** `MoveItems([every item with start >= F, except transitions], {"startFrame": F + n},
  False)` keeps each item's track; a music piece straddling F gets its tail extended by n; then re-lay the song
  so it doesn't repeat (`relay_song` in the scratch scripts: delete every piece and re-append each with
  source = its timeline offset from the first piece).
- **Ducking without keyframes:** replace a music piece with three (before / under dialogue / after) via
  `DeleteClips` + `AppendToTimeline(mediaType 2)` with the same source offsets, 8-frame fades, dialogue piece
  at −14 dB. −9 dB wasn't enough under a running UTV.
- **Never rewrite a media file while Resolve may be rendering it:** the render crawled (13 % in 10 min),
  `StopRendering` left `IsRenderingInProgress` true and the page unreadable, and only a Resolve restart cleared
  it. Re-render plates first, `mpi.ReplaceClip(path)` to relink, then render.
- `ExportThumbnails` takes at most 48 frames (`sheet` now splits into numbered files).
- `Project.SetName` renames the open project; `pm.SaveProject()` before `pm.LoadProject(other)`.
- `SetCDL({"NodeIndex": "1", "Slope": "r g b", "Offset": …, "Power": …, "Saturation": "0.88"})` is the only
  scriptable primary correction; strings, 1-based node.

## Vertical (9:16) cut from a 16:9 edit (learned on the Moab sizzle)

- `mp.CreateEmptyTimeline(name)` then `t.SetSetting("useCustomSettings", "1")`, `timelineResolutionWidth/Height`
  (strings). It becomes the shown timeline: only with the user's OK.
- Append the same media pool items with the source ranges of the shots you want (`item.GetSourceStartFrame()`
  + offset); grades don't follow media, so `src_item.CopyGrades([new])` and `AssignToColorGroup`.
- Fill the tall frame: `SetProperty("ZoomX"/"ZoomY", (H/H_src)/(W/W_src))` (3.16 for UHD → 1080×1920), then
  `SetProperty("Pan", px)` in timeline pixels, positive shows more of the left side; a UHD source pixel is
  0.889 timeline px at that zoom. `ExportFrame` shows zoom and pan, so check a frame per shot and adjust.
- Zoom 1.0 letterboxes a 16:9 shot into a band — good under a title card (text above and below it).
- `rv render --width 1080 --height 1920` for the preview; the default is 1920×1080.
- Music to the bar: `reframe-cli studio beats <song>` lists downbeats and loudness per bar; cut on them and end
  on the song's own ending (the last loud bar before loudness drops to 0 %).
- Re-cutting to a song the user laid themselves: timeline 0 = song `piece.GetSourceStartFrame() / fps`; subtract
  that from `studio beats --json` downbeats to get the grid. Rebuild V1 only (delete, re-append), keep the user's
  `Pan`/`Tilt`/`ZoomX` and `GetLeftOffset()` for shots that stay, and leave their music piece alone except for a
  3-way split where a voice snippet needs the music ducked (≈ −9 dB with the camera audio at +6 was intelligible).
- Finding a spoken line: Whisper (mlx-whisper, tools/studio-speech venv) over the conform clips' audio with word
  timestamps; offsets are into the clip file, and a v3 item's `GetLeftOffset()` says where the file the item starts.
  Verify the mix by transcribing the rendered preview's audio around the line.
- Slipping a song under a cut (same positions, levels and fades; different part of the song): delete the pieces
  and re-append each with `startFrame` shifted by the same amount. Find the hit with a 40–110 Hz band-pass
  envelope (a beat tracker's "downbeat" can be a beat off; the kick says where the one is). Resolve played this
  MP3 ≈ 51 ms (1.2 frames) later than its source frame implied: measure by cross-correlating the render's audio
  with the song and add the measured offset before trusting a frame number.

## API notes (Resolve 21.2)

Docs: `DaVinciResolveScript.pyi`, `README.md` and `CHANGELOG.md` in
`/Library/Application Support/Blackmagic Design/DaVinci Resolve/Developer/Scripting/`.

- `RefreshLUTList` is on Project, not Resolve.
- `Timeline.ExportThumbnails(frames, opts)` takes timeline frame offsets with labels;
  `TimelineItem.ExportFrame(offset, opts)` takes offsets into the item.
- `MediaPool.AppendToTimeline([{mediaPoolItem, startFrame, endFrame, recordFrame, trackIndex,
  mediaType: 2}])` places audio at an exact frame; source frames count at the timeline rate.
- `TimelineItem.SetProperties({"AudioVolume": dB})`, `SetFades({"FadeIn": f, "FadeOut": f})`.
- An MP3's media pool item has no "Frames"; use ffprobe (or its "Duration" timecode).
- `GetStart()` is in absolute frames; subtract `Timeline.GetStartFrame()`.

## Known gaps (worth building)

- No video edits yet: trimming, moving or replacing clips (`SetLeftOffset`/`SetRightOffset`,
  `MoveItems`, `SetPosition` exist) and a round trip from an updated Studio cut.
- `music swap` matches the old song by name only; it can't swap part of a song's runs.
- No volume keyframes, so ducking is still per piece (steps at cuts).
- Swap doesn't fade the end of a song that runs out early.
