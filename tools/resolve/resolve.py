"""Drive DaVinci Resolve Studio's current timeline from the command line.

Needs DaVinci Resolve Studio running with Preferences › System › General › External scripting
using set to Local. Works on the timeline Resolve is showing. Commands that change it (lut, music fades,
music swap, place) need --timeline "<its name>" and refuse if Resolve is showing a different one.

    uv run resolve.py status
    uv run resolve.py dump [--json] [--clips 1-20]
    uv run resolve.py sheet [--clips 1-40] [--at in|mid|out] [--out sheet.jpg] [--size 3000]
    uv run resolve.py frame <clip#> [--at SECONDS|mid] [--out frame.jpg]
    uv run resolve.py lut <lut path or name> --timeline T [--clips SPEC] [--except SPEC] [--node 1]
    uv run resolve.py lut --clear --timeline T [--clips SPEC] [--except SPEC] [--node 1]
    uv run resolve.py music [list]
    uv run resolve.py music fades --timeline T [--dry-run]
    uv run resolve.py music swap <old song> <new file> --timeline T [--in SEC ...] [--snap] [--gain DB] [--dry-run] [--no-backup]
    uv run resolve.py render --from SEC --to SEC [--out DIR] [--name N] [--frames 0,24,48]
    uv run resolve.py place <audio file> --timeline T --at SEC --in SEC (--length SEC | --until SEC) [--track N] [--db DB] [--fade-in SEC] [--fade-out SEC]

Clips are numbered by the "NNN " prefix conform puts on each file (so numbers survive edits), else by position on video track 1. SPEC is like "1-20,24,31".
Timeline positions are seconds from the start of the timeline.
"""
from __future__ import annotations

import argparse, glob, json, os, re, subprocess, sys

API = "/Library/Application Support/Blackmagic Design/DaVinci Resolve/Developer/Scripting"
LIB = "/Applications/DaVinci Resolve/DaVinci Resolve.app/Contents/Libraries/Fusion/fusionscript.so"
LUT_ROOT = "/Library/Application Support/Blackmagic Design/DaVinci Resolve/LUT"
STUDIO_PROJECTS = os.path.expanduser("~/Documents/Insta360/Studio/TimelineProject")
REFRAME_CLI = os.path.join(os.path.dirname(os.path.abspath(__file__)), "../reframe/.build/debug/reframe-cli")

os.environ.setdefault("RESOLVE_SCRIPT_API", API)
os.environ.setdefault("RESOLVE_SCRIPT_LIB", LIB)
sys.path.append(os.path.join(os.environ["RESOLVE_SCRIPT_API"], "Modules"))


def die(msg: str) -> None:
    print(msg, file=sys.stderr)
    sys.exit(1)


# ---------- connection and timeline helpers ----------

class Session:
    def __init__(self, expect: str | None = None, write: bool = False) -> None:
        import DaVinciResolveScript as dvr  # noqa: E402 (needs the env above)
        self.resolve = dvr.scriptapp("Resolve")
        if not self.resolve:
            die("Resolve isn't answering. Open DaVinci Resolve Studio (the free edition can't be scripted) and set "
                "Preferences › System › General › External scripting using to Local.")
        self.project = self.resolve.GetProjectManager().GetCurrentProject()
        if not self.project:
            die("No project is open in Resolve.")
        self.timeline = self.project.GetCurrentTimeline()
        if not self.timeline:
            die(f"Project {self.project.GetName()} has no current timeline.")
        name = self.timeline.GetName()
        if write:
            # Writes go to whatever timeline Resolve is showing, and the user may be working in it: make the
            # caller name it, so a timeline switched in the UI (or by a script) never gets changed by surprise.
            if expect is None:
                die(f"this changes the timeline: pass --timeline \"{name}\" to confirm that's the one to change")
            if expect != name:
                die(f"Resolve is showing timeline \"{name}\", not \"{expect}\"; nothing changed")
            print(f"timeline: {name}")
        self.fps = float(self.timeline.GetSetting("timelineFrameRate"))
        self.start = self.timeline.GetStartFrame()

    def video(self) -> list:
        return self.timeline.GetItemListInTrack("video", 1) or []

    def clips(self, spec: str | None = None) -> list[tuple[int, object]]:
        """(clip number, item) for video track 1, optionally filtered by SPEC. A clip's number is the
        "NNN " prefix conform gives its file, so it stays put when clips are moved, removed or added;
        clips without one are numbered by position."""
        out = []
        for pos, it in enumerate(self.video(), 1):
            m = re.match(r"(\d{3}) ", it.GetName())
            out.append((int(m.group(1)) if m else pos, it))
        want = parse_spec(spec)
        return [c for c in out if want is None or c[0] in want]

    def clip(self, n: int):
        hits = self.clips(str(n))
        if not hits:
            die(f"no clip {n} on video track 1")
        return hits[0][1]

    def rel(self, frame: float) -> float:
        """Seconds from the timeline's start."""
        return (frame - self.start) / self.fps

    def clock(self, frame: float) -> str:
        tenths = round(self.rel(frame) * 10)
        return f"{tenths // 600}:{tenths % 600 / 10:04.1f}"


def parse_spec(spec: str | None) -> set[int] | None:
    """'1-5,24' -> {1,2,3,4,5,24}; None (every clip) when spec is None or 'all'."""
    if not spec or spec == "all":
        return None
    out: set[int] = set()
    for part in spec.split(","):
        a, _, b = part.strip().partition("-")
        out.update(range(int(a), int(b or a) + 1))
    return out


def is_music(item) -> bool:
    mpi = item.GetMediaPoolItem()
    return bool(mpi) and mpi.GetClipProperty("Type") == "Audio"


def music_items(s: Session) -> list:
    out = []
    for n in range(1, s.timeline.GetTrackCount("audio") + 1):
        for it in s.timeline.GetItemListInTrack("audio", n) or []:
            if is_music(it):
                out.append((n, it))
    return sorted(out, key=lambda x: x[1].GetStart())


def song_name(item) -> str:
    return item.GetMediaPoolItem().GetClipProperty("File Name") or item.GetName()


def volume_db(item) -> float | None:
    p = item.GetProperties() or {}
    return p.get("AudioVolume") if p.get("AudioVolumeEnabled", True) else 0.0


def runs(pieces: list) -> list[list]:
    """Groups a song's pieces into runs that play continuously (each piece starts where the last ended,
    in both the timeline and the song)."""
    out: list[list] = []
    for tr, it in pieces:
        if out:
            ptr, prev = out[-1][-1]
            if (abs(it.GetStart() - prev.GetEnd()) <= 1
                    and abs(it.GetSourceStartFrame() - (prev.GetSourceStartFrame() + prev.GetDuration())) <= 1):
                out[-1].append((tr, it))
                continue
        out.append([(tr, it)])
    return out


_manifest_cache: dict[int, dict | None] = {}


def find_manifest(s: "Session") -> dict | None:
    """The reframe-cli conform manifest for this timeline: the one whose cut has the timeline's name, or
    else (a duplicated or renamed timeline) the one whose clips match video track 1 clip for clip."""
    if id(s) in _manifest_cache:
        return _manifest_cache[id(s)]
    name = s.timeline.GetName()
    numbered = [(n, os.path.splitext(it.GetName())[0].split(" ", 1)[-1]) for n, it in s.clips()
                if re.match(r"\d{3} ", it.GetName())]
    found = None
    for f in glob.glob(os.path.join(STUDIO_PROJECTS, "*", "reframe-conform.json")):
        try:
            m = json.load(open(f))
        except (OSError, ValueError):
            continue
        if m.get("cut") == name:
            found = m
            break
        mc = m.get("clips", [])
        if numbered and all(n <= len(mc) and os.path.splitext(mc[n - 1]["name"])[0] == base for n, base in numbered):
            found = m
    _manifest_cache[id(s)] = found
    return found


def manifest_music(s: "Session", item) -> dict:
    """The conform manifest's entry for a music piece: same song, same in-point in the song. (Matching on
    the song rather than the timeline keeps working after clips are moved or ripple-deleted.)"""
    m = find_manifest(s) or {}
    name, src = song_name(item), item.GetSourceStartTime()
    for x in m.get("music", []):
        if x["name"] == name and abs(x["sourceIn"] - src) < 1.5 / s.fps:
            return x
    return {}


def downbeats(path: str) -> dict | None:
    """Tempo, downbeats and section changes from reframe-cli, if it's built."""
    cli = os.path.normpath(REFRAME_CLI)
    if not os.path.exists(cli):
        return None
    r = subprocess.run([cli, "studio", "beats", path, "--json"], capture_output=True, text=True)
    if r.returncode:
        return None
    return json.loads(r.stdout[r.stdout.index("{"):])


def resolve_lut(name: str) -> str:
    if os.path.isabs(name) and os.path.exists(name):
        return name
    hits = [f for f in glob.glob(os.path.join(LUT_ROOT, "**", "*"), recursive=True)
            if os.path.isfile(f) and (os.path.basename(f) == name or os.path.splitext(os.path.basename(f))[0] == name)]
    if not hits:
        hits = [f for f in glob.glob(os.path.join(LUT_ROOT, "**", f"*{name}*"), recursive=True) if os.path.isfile(f)]
    if len(hits) != 1:
        die(f"LUT '{name}': {'no match' if not hits else 'several matches: ' + ', '.join(hits)} under {LUT_ROOT}")
    return hits[0]


# ---------- commands ----------

def cmd_status(a) -> None:
    s = Session()
    t = s.timeline
    print(f"{s.resolve.GetProductName()} {s.resolve.GetVersionString()} · page {s.resolve.GetCurrentPage()}")
    print(f"project {s.project.GetName()} · timeline {t.GetName()} ({s.project.GetTimelineCount()} timelines)")
    print(f"{s.fps:g} fps · {s.clock(t.GetEndFrame())} long · playhead {t.GetCurrentTimecode()}")
    for kind in ("video", "audio"):
        for n in range(1, t.GetTrackCount(kind) + 1):
            items = t.GetItemListInTrack(kind, n) or []
            songs = sorted({song_name(i) for i in items if kind == "audio" and is_music(i)})
            extra = f" · music: {', '.join(songs)}" if songs else ""
            print(f"  {kind[0].upper()}{n} {t.GetTrackName(kind, n)}: {len(items)} items{extra}")
    m = find_manifest(s)
    print(f"conform manifest: {'found' if m else 'none'}")


def cmd_dump(a) -> None:
    s = Session()
    m = find_manifest(s)
    rows = []
    for i, it in s.clips(a.clips):
        linked = [x for x in (it.GetLinkedItems() or []) if x.GetTrackTypeAndIndex()[0] == "audio"]
        g = it.GetNodeGraph()
        row = {
            "clip": i, "name": it.GetName(), "start": round(s.rel(it.GetStart()), 3),
            "length": round(it.GetDuration() / s.fps, 3), "sourceIn": round(it.GetSourceStartTime() or 0, 3),
            "lut": g.GetLUT(1) if g and g.GetNumNodes() else "", "nodes": g.GetNumNodes() if g else 0,
            "audioDb": volume_db(linked[0]) if linked else None,
        }
        if m and i <= len(m["clips"]):
            mc = m["clips"][i - 1]
            row["recording"] = mc["name"]
            row["recordingIn"] = round(mc["sourceIn"], 3)
            row["rate"] = mc.get("rate", 1)
        rows.append(row)
    if a.json:
        print(json.dumps(rows, indent=1))
        return
    for r in rows:
        db = "" if r["audioDb"] is None else f"{r['audioDb']:+.1f} dB"
        rate = f" {r['rate']:g}x" if r.get("rate", 1) != 1 else ""
        rec = f" ← {r['recording']} @{r['recordingIn']:.1f}s{rate}" if "recording" in r else ""
        lut = os.path.basename(r["lut"]) if r["lut"] else "no LUT"
        more = f" +{r['nodes'] - 1} nodes" if r["nodes"] > 1 else ""
        print(f"{r['clip']:3d} {s.clock(s.start + r['start'] * s.fps):>7} {r['length']:5.1f}s  {r['name']:<18} "
              f"{db:>9}  {lut}{more}{rec}")


def cmd_sheet(a) -> None:
    s = Session()
    frames = []
    for i, it in s.clips(a.clips):
        st, d = it.GetStart() - s.start, it.GetDuration()
        off = {"in": st, "out": st + d - 1, "mid": st + d // 2}[a.at]
        frames.append({"frameOffset": int(off), "label": f"{i} · {s.clock(it.GetStart())} · {it.GetName()[4:]}"})
    out = os.path.abspath(a.out or f"{s.timeline.GetName()} sheet.jpg")
    # Resolve's thumbnail grid holds at most 48 frames: longer timelines become numbered sheets.
    chunks = [frames[i:i + 48] for i in range(0, len(frames), 48)]
    for k, chunk in enumerate(chunks, 1):
        path = out if len(chunks) == 1 else re.sub(r"(\.\w+)$", f" {k}\\1", out)
        r = s.timeline.ExportThumbnails(chunk, {"outputType": "file", "filePath": path, "maxDimension": a.size, "quality": 85})
        if not r or not r.get("success"):
            die(f"ExportThumbnails failed: {r}")
        print(f"{path} ({len(chunk)} clips, {r['columns']}×{r['rows']} grid, {r['width']}×{r['height']})")


def cmd_frame(a) -> None:
    s = Session()
    it = s.clip(a.clip)
    off = it.GetDuration() // 2 if a.at == "mid" else int(float(a.at) * s.fps)
    out = os.path.abspath(a.out or f"clip {a.clip}.jpg")
    r = it.ExportFrame(int(off), {"outputType": "file", "filePath": out, "maxDimension": a.size, "quality": 90})
    if not r or not r.get("success"):
        die(f"ExportFrame failed: {r}")
    print(out)


def cmd_lut(a) -> None:
    s = Session(a.timeline, write=not getattr(a, "dry_run", False))
    skip = parse_spec(a.except_) or set()
    picked = [(i, it) for i, it in s.clips(a.clips) if i not in skip]
    if a.clear:
        path = ""
    elif a.lut:
        path = resolve_lut(a.lut)
        s.project.RefreshLUTList()
    else:
        die("name a LUT, or pass --clear")
    done, failed = 0, []
    for i, it in picked:
        g = it.GetNodeGraph()
        if g and g.SetLUT(a.node, path):
            if path:
                g.SetNodeLabel(a.node, os.path.splitext(os.path.basename(path))[0][:40])
            done += 1
        else:
            failed.append(i)
    what = "cleared the LUT on" if a.clear else f"set {os.path.basename(path)} on"
    print(f"{what} node {a.node} of {done} clips" + (f"; failed on {failed}" if failed else ""))


def cmd_music_list(a) -> None:
    s = Session()
    pieces = music_items(s)
    if not pieces:
        print("no music on the timeline")
        return
    by_song: dict[str, list] = {}
    for tr, it in pieces:
        by_song.setdefault(song_name(it), []).append((tr, it))
    for song, ps in by_song.items():
        print(f"{song}: {len(ps)} pieces")
        for run in runs(ps):
            a0, z = run[0][1], run[-1][1]
            print(f"  {s.clock(a0.GetStart())}–{s.clock(z.GetEnd())} on A{run[0][0]}, "
                  f"song {a0.GetSourceStartTime():.1f}–{a0.GetSourceStartTime() + (z.GetEnd() - a0.GetStart()) / s.fps:.1f}s")
            for tr, it in run:
                f = it.GetFades() or {}
                fades = "".join([f" fade in {f['FadeIn']:g}f" if f.get("FadeIn") else "",
                                 f" fade out {f['FadeOut']:g}f" if f.get("FadeOut") else ""])
                print(f"    {s.clock(it.GetStart()):>7} {it.GetDuration() / s.fps:5.1f}s {volume_db(it):+6.1f} dB{fades}")


def cmd_music_fades(a) -> None:
    """Sets music fades from the conform manifest (Resolve drops FCPXML fades on import)."""
    s = Session(a.timeline, write=not getattr(a, "dry_run", False))
    m = find_manifest(s)
    if not m:
        die("no conform manifest for this timeline")
    n = 0
    for tr, it in music_items(s):
        x = manifest_music(s, it)
        if not (x.get("fadeIn") or x.get("fadeOut")):
            continue
        fades = {"FadeIn": int(x.get("fadeIn", 0)), "FadeOut": int(x.get("fadeOut", 0))}
        print(f"{s.clock(it.GetStart()):>7} {song_name(it)}: fade in {fades['FadeIn']}f, out {fades['FadeOut']}f")
        if not a.dry_run and not it.SetFades(fades):
            print("  (SetFades failed)")
        n += 1
    print(f"{'would set' if a.dry_run else 'set'} fades on {n} pieces")


def import_audio(s: Session, path: str):
    path = os.path.abspath(os.path.expanduser(path))
    if not os.path.exists(path):
        die(f"{path} doesn't exist")
    pool = s.project.GetMediaPool()

    def find(folder):
        for c in folder.GetClipList() or []:
            if c.GetClipProperty("File Path") == path:
                return c
        for sub in folder.GetSubFolderList() or []:
            hit = find(sub)
            if hit:
                return hit
        return None

    hit = find(pool.GetRootFolder())
    if hit:
        return hit
    got = pool.ImportMedia([path]) or []
    if not got:
        die(f"Resolve couldn't import {path}")
    return got[0]


def audio_seconds(path: str) -> float:
    """Length of an audio file (Resolve reports no frame count for MP3s)."""
    r = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path],
                       capture_output=True, text=True)
    try:
        return float(r.stdout.strip())
    except ValueError:
        die(f"couldn't read the length of {path} (is ffprobe installed?)")


def append_audio(s: Session, mpi, track: int, record: int, src_in: int, length: int):
    """Places [src_in, src_in + length) of an audio clip at timeline frame `record` on audio track `track`."""
    info = {"mediaPoolItem": mpi, "startFrame": src_in, "endFrame": src_in + length - 1,
            "recordFrame": record, "trackIndex": track, "mediaType": 2}
    got = s.project.GetMediaPool().AppendToTimeline([info]) or []
    if not got:
        return None
    it = got[0]
    if abs(it.GetDuration() - length) > 0:  # endFrame inclusivity differs between builds: trim to the exact length
        it.SetRightOffset(it.GetRightOffset() + (it.GetDuration() - length))
    return it


def new_audio_track(s: Session, name: str) -> int:
    t = s.timeline
    if not t.AddTrack("audio", "stereo"):
        die("couldn't add an audio track")
    n = t.GetTrackCount("audio")
    t.SetTrackName("audio", n, name[:30])
    return n


def cmd_music_swap(a) -> None:
    s = Session(a.timeline, write=not getattr(a, "dry_run", False))
    old = [(tr, it) for tr, it in music_items(s) if a.old.lower() in song_name(it).lower()]
    if not old:
        die(f"no music matching '{a.old}' on the timeline")
    old_name = song_name(old[0][1])
    new_path = os.path.abspath(os.path.expanduser(a.new))
    if not os.path.exists(new_path):
        die(f"{new_path} doesn't exist")
    song_len = audio_seconds(new_path)
    beats = downbeats(new_path) if a.snap or a.dry_run else None

    def snap(t: float) -> float:
        if not (a.snap and beats and beats.get("downbeats")):
            return t
        return min(beats["downbeats"], key=lambda d: abs(d - t))

    plan, cursor = [], 0.0
    ins = list(a.in_ or [])
    for k, run in enumerate(runs(old)):
        first, last = run[0][1], run[-1][1]
        start_in = snap(ins[k]) if k < len(ins) else cursor
        length = (last.GetEnd() - first.GetStart()) / s.fps
        if start_in + length > song_len + 0.05:
            print(f"warning: run {k + 1} needs {length:.1f}s from {start_in:.1f}s but the song is {song_len:.1f}s; "
                  f"it'll stop early", file=sys.stderr)
        for tr, it in run:
            off = (it.GetStart() - first.GetStart()) / s.fps
            f = it.GetFades() or {}
            mf = manifest_music(s, it)
            plan.append({
                "old": it, "track": tr, "record": int(it.GetStart()), "frames": int(it.GetDuration()),
                "songIn": start_in + off, "db": (volume_db(it) or 0.0) + a.gain,
                "fadeIn": int(f.get("FadeIn") or mf.get("fadeIn") or 0),
                "fadeOut": int(f.get("FadeOut") or mf.get("fadeOut") or 0),
            })
        cursor = start_in + length
        print(f"run {k + 1}: timeline {s.clock(first.GetStart())}–{s.clock(last.GetEnd())} ({length:.1f}s) "
              f"← new song {start_in:.1f}–{start_in + length:.1f}s, {len(run)} pieces")
    if beats:
        secs = ", ".join(f"{x:.1f}" for x in beats.get("sections", []))
        print(f"new song: {beats['bpm']:.1f} BPM, {beats['duration']:.1f}s; section changes at {secs}")
    if a.dry_run:
        for p in plan:
            print(f"  {s.clock(p['record']):>7} {p['frames'] / s.fps:5.1f}s ← {p['songIn']:6.1f}s {p['db']:+6.1f} dB")
        return

    mpi = import_audio(s, new_path)
    if not a.no_backup:
        name = s.timeline.GetName()
        backup = s.timeline.DuplicateTimeline(f"{name} (before {os.path.splitext(os.path.basename(new_path))[0]})")
        # Duplicating switches Resolve's view to the copy; put the user back on their timeline at once.
        if not s.project.SetCurrentTimeline(s.timeline) or s.project.GetCurrentTimeline().GetName() != name:
            die(f"made a backup but couldn't switch Resolve back to \"{name}\"; nothing else changed")
        if backup:
            print(f"backup timeline: {backup.GetName()} (Resolve is still showing {name})")
    track = new_audio_track(s, os.path.splitext(os.path.basename(new_path))[0])
    placed = []
    for p in plan:
        src = int(round(p["songIn"] * s.fps))
        frames = min(p["frames"], int(song_len * s.fps) - src)
        if frames <= 0:
            continue
        it = append_audio(s, mpi, track, p["record"], src, frames)
        if not it:
            die(f"couldn't place the new song at {s.clock(p['record'])}. The old song is untouched; "
                f"delete audio track A{track} to undo the pieces already placed.")
        it.SetProperties({"AudioVolumeEnabled": True, "AudioVolume": max(-100.0, min(30.0, p["db"]))})
        if p["fadeIn"] or p["fadeOut"]:
            it.SetFades({"FadeIn": p["fadeIn"], "FadeOut": p["fadeOut"]})
        placed.append(it)
    if len(placed) != len(plan):
        print(f"placed {len(placed)} of {len(plan)} pieces (the song ran out)", file=sys.stderr)
    s.timeline.DeleteClips([p["old"] for p in plan], False)
    print(f"replaced {len(plan)} pieces of {old_name} with {os.path.basename(new_path)} on A{track}")


def cmd_place(a) -> None:
    s = Session(a.timeline, write=not getattr(a, "dry_run", False))
    mpi = import_audio(s, a.file)
    record = s.start + int(round(a.at * s.fps))
    length = a.length if a.length is not None else (a.until - a.at if a.until is not None else None)
    if not length or length <= 0:
        die("give --length or --until")
    track = a.track or new_audio_track(s, os.path.splitext(os.path.basename(a.file))[0])
    it = append_audio(s, mpi, track, record, int(round(a.in_ * s.fps)), int(round(length * s.fps)))
    if not it:
        die("Resolve refused the placement (is that spot on the track free?)")
    it.SetProperties({"AudioVolumeEnabled": True, "AudioVolume": a.db})
    fades = {"FadeIn": int(round(a.fade_in * s.fps)), "FadeOut": int(round(a.fade_out * s.fps))}
    if fades["FadeIn"] or fades["FadeOut"]:
        it.SetFades(fades)
    print(f"placed {os.path.basename(a.file)} {a.in_:.1f}s+{length:.1f}s at {s.clock(record)} on A{track}, {a.db:+.1f} dB"
          + (f", fades {a.fade_in:g}s/{a.fade_out:g}s" if fades["FadeIn"] or fades["FadeOut"] else ""))


def cmd_render(a) -> None:
    """Renders part of the timeline through the Deliver page: the only view of Fusion work (titles, comps),
    which ExportFrame and sheets don't include. Puts back the page, playhead and render format afterwards."""
    import time
    s = Session()
    r, p, t = s.resolve, s.project, s.timeline
    page, here = r.GetCurrentPage(), t.GetCurrentTimecode()
    fmt, mode = p.GetCurrentRenderFormatAndCodec() or {}, p.GetCurrentRenderMode()
    first, last = s.start + int(round(a.from_ * s.fps)), s.start + int(round(a.to * s.fps))
    out = os.path.abspath(os.path.expanduser(a.out or "."))
    os.makedirs(out, exist_ok=True)
    name = a.name or f"{t.GetName()} {a.from_:g}-{a.to:g}s"
    try:
        p.SetCurrentRenderMode(1)
        p.SetCurrentRenderFormatAndCodec("mov", "ProRes422Proxy")
        p.SetRenderSettings({"SelectAllFrames": False, "MarkIn": first, "MarkOut": last, "TargetDir": out,
                             "CustomName": name, "ExportVideo": True, "ExportAudio": a.audio,
                             "FormatWidth": a.width, "FormatHeight": a.height})
        job = p.AddRenderJob()
        if not job:
            die("Resolve wouldn't add the render job")
        p.StartRendering([job], False)
        while p.IsRenderingInProgress():
            time.sleep(0.3)
        status = p.GetRenderJobStatus(job)
        p.DeleteRenderJob(job)
    finally:
        if fmt:
            p.SetCurrentRenderFormatAndCodec(fmt.get("format", "mov"), fmt.get("codec", "H264"))
        p.SetCurrentRenderMode(mode)
        p.SetRenderSettings({"SelectAllFrames": True})
        # Resolve keeps flipping to Deliver for a moment after a job ends: restore, then check, a few times.
        for _ in range(10):
            time.sleep(0.5)
            r.OpenPage(page)
            t.SetCurrentTimecode(here)
            if r.GetCurrentPage() == page and t.GetCurrentTimecode() == here:
                break
        else:
            print(f"couldn't put Resolve back on the {page} page at {here}", file=sys.stderr)
    movie = os.path.join(out, name + ".mov")
    print(f"{status.get('JobStatus')}: {movie}")
    if a.frames:
        picks = "+".join(f"eq(n\\,{int(x)})" for x in a.frames.split(","))
        n = len(a.frames.split(","))
        sheet = os.path.join(out, name + " frames.jpg")
        subprocess.run(["ffmpeg", "-v", "error", "-y", "-i", movie, "-vf", f"select='{picks}',scale=960:-1,tile={min(n, 3)}x{(n + 2) // 3}",
                        "-frames:v", "1", sheet], check=True)
        print(sheet)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("status")
    p = sub.add_parser("dump"); p.add_argument("--json", action="store_true"); p.add_argument("--clips")
    p = sub.add_parser("sheet"); p.add_argument("--clips"); p.add_argument("--at", choices=["in", "mid", "out"], default="mid")
    p.add_argument("--out"); p.add_argument("--size", type=int, default=3000)
    p = sub.add_parser("frame"); p.add_argument("clip", type=int); p.add_argument("--at", default="mid")
    p.add_argument("--out"); p.add_argument("--size", type=int, default=1600)
    p = sub.add_parser("lut"); p.add_argument("lut", nargs="?"); p.add_argument("--clear", action="store_true")
    p.add_argument("--clips"); p.add_argument("--except", dest="except_"); p.add_argument("--node", type=int, default=1)
    p.add_argument("--timeline", help="name of the timeline to change (must be the one Resolve is showing)")
    p = sub.add_parser("music"); ms = p.add_subparsers(dest="mcmd")
    ms.add_parser("list")
    q = ms.add_parser("fades"); q.add_argument("--dry-run", action="store_true"); q.add_argument("--timeline")
    q = ms.add_parser("swap"); q.add_argument("old"); q.add_argument("new")
    q.add_argument("--in", dest="in_", type=float, action="append", help="where each run starts in the new song (seconds); repeat per run")
    q.add_argument("--snap", action="store_true", help="snap --in points to the new song's downbeats")
    q.add_argument("--gain", type=float, default=0.0, help="dB added to every piece's level")
    q.add_argument("--dry-run", action="store_true"); q.add_argument("--no-backup", action="store_true")
    q.add_argument("--timeline")
    p = sub.add_parser("render"); p.add_argument("--from", dest="from_", type=float, required=True)
    p.add_argument("--to", type=float, required=True); p.add_argument("--out"); p.add_argument("--name")
    p.add_argument("--frames", help="frame numbers within the render to tile into a contact sheet")
    p.add_argument("--audio", action="store_true")
    p.add_argument("--width", type=int, default=1920, help="render size (default 1920x1080; use 1080 1920 for a vertical timeline)")
    p.add_argument("--height", type=int, default=1080)
    p = sub.add_parser("place"); p.add_argument("file"); p.add_argument("--at", type=float, required=True)
    p.add_argument("--in", dest="in_", type=float, default=0.0); p.add_argument("--length", type=float)
    p.add_argument("--until", type=float); p.add_argument("--track", type=int); p.add_argument("--db", type=float, default=0.0)
    p.add_argument("--fade-in", type=float, default=0.0); p.add_argument("--fade-out", type=float, default=0.0)
    p.add_argument("--timeline")
    a = ap.parse_args()
    if a.cmd == "music":
        {"list": cmd_music_list, None: cmd_music_list, "fades": cmd_music_fades, "swap": cmd_music_swap}[a.mcmd](a)
    else:
        {"status": cmd_status, "dump": cmd_dump, "sheet": cmd_sheet, "frame": cmd_frame,
         "lut": cmd_lut, "place": cmd_place, "render": cmd_render}[a.cmd](a)


if __name__ == "__main__":
    main()
