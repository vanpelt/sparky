#!/usr/bin/env python3
"""stateus — reconstruct a per-day location ledger from whatever evidence exists.

Built for day-count questions ("how many weekdays was I in NYC?") where the
answer has to survive scrutiny. Every day in the output cites the evidence
behind it; days with no evidence are reported as unknown rather than guessed.
"""

import argparse
import json
import re
import sqlite3
import sys
from collections import Counter, namedtuple
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

from zoneinfo import ZoneInfo

from shapely.geometry import Point, shape
from shapely.ops import unary_union
from shapely.prepared import prep

HERE = Path(__file__).parent
NYC_GEOJSON = HERE / "nyc_boroughs.geojson"
NY_STATE_GEOJSON = HERE / "ny_state.geojson"

# Core Data counts seconds from 2001-01-01; unix counts from 1970-01-01.
COREDATA_EPOCH = 978307200

DEFAULT_PHOTOS_DB = (
    Path.home() / "Pictures" / "Photos Library.photoslibrary" / "database" / "Photos.sqlite"
)

# An observation is one moment we can place on the map.
Obs = namedtuple("Obs", "day lat lng source detail")


def _shapes(path):
    return [shape(f["geometry"]) for f in json.loads(path.read_text())["features"]]


def load_nyc():
    """Five-borough boundary, as one prepared geometry."""
    return prep(unary_union(_shapes(NYC_GEOJSON)))


def load_ny_state():
    """New York State, unioned with the precise borough polygons.

    The state outline is coarse. Unioning it with the borough shapes means the
    dense, border-adjacent case (is this Manhattan or is it Hoboken?) is
    decided by the accurate geometry, while the coarse outline only has to
    handle upstate and Long Island, where no border is nearby.
    """
    return prep(unary_union(_shapes(NY_STATE_GEOJSON) + _shapes(NYC_GEOJSON)))


def local_day(utc_seconds: float, tz_offset_seconds):
    """The calendar day at the place the observation happened.

    Day-count rules care about the local day. A photo taken at 04:02 UTC in
    California is the *previous* evening locally, and counting it as the UTC
    day would silently shift it.
    """
    if tz_offset_seconds is None:
        tz_offset_seconds = 0
    return datetime.fromtimestamp(utc_seconds + tz_offset_seconds, tz=timezone.utc).date()


# ---------------------------------------------------------------- sources


def _photo_rows(db_path: Path, year: int):
    """Raw geotagged rows for the year, tagged with the device that took them."""
    # immutable=1 reads a live, WAL-mode library without taking locks on it.
    uri = f"file:{db_path}?immutable=1"
    con = sqlite3.connect(uri, uri=True)
    rows = con.execute(
        """
        SELECT a.ZDATECREATED, x.ZTIMEZONEOFFSET, a.ZLATITUDE, a.ZLONGITUDE,
               coalesce(e.ZCAMERAMODEL, '(unknown)')
        FROM ZASSET a
        LEFT JOIN ZADDITIONALASSETATTRIBUTES x ON x.ZASSET = a.Z_PK
        LEFT JOIN ZEXTENDEDATTRIBUTES e ON e.ZASSET = a.Z_PK
        WHERE a.ZLATITUDE IS NOT NULL AND a.ZLATITUDE > -180
          AND a.ZTRASHEDDATE IS NULL
        """
    ).fetchall()
    con.close()

    out = []
    for created, tzoff, lat, lng, model in rows:
        if created is None:
            continue
        day = local_day(created + COREDATA_EPOCH, tzoff)
        if day.year == year:
            out.append((day, lat, lng, model))
    return out


def trusted_devices(rows, verbose=True):
    """Work out which devices in the library are actually the user's.

    A photo library is not a location log. Pictures arrive by AirDrop, text
    and shared album carrying *someone else's* EXIF coordinates, and counting
    those invents days in places the user never was. So: take the device with
    the broadest coverage as the reference phone, then keep another device
    only if it never contradicts the phone on a day they both saw.
    """
    by_model = {}
    for day, lat, lng, model in rows:
        by_model.setdefault(model, []).append((day, lat, lng))
    if not by_model:
        return set()

    # The reference device is the one carried most: most days covered.
    primary = max(by_model, key=lambda m: len({d for d, _, _ in by_model[m]}))

    # Where the reference device was, per day.
    anchor = {}
    for day, lat, lng in by_model[primary]:
        anchor.setdefault(day, []).append((lat, lng))
    anchor = {d: (sum(a for a, _ in v) / len(v), sum(b for _, b in v) / len(v))
              for d, v in anchor.items()}

    trusted = {primary}
    notes = [(primary, len(by_model[primary]), 0, "reference device")]
    for model, pts in by_model.items():
        if model == primary:
            continue
        overlap = [(d, la, lo) for d, la, lo in pts if d in anchor]
        # ~1.5 degrees is a different metro area, not GPS jitter.
        conflicts = sum(1 for d, la, lo in overlap
                        if abs(la - anchor[d][0]) > 1.5 or abs(lo - anchor[d][1]) > 1.5)
        if overlap and conflicts == 0:
            trusted.add(model)
            notes.append((model, len(pts), conflicts, "corroborates reference"))
        else:
            why = "contradicts reference" if conflicts else "never co-observed"
            notes.append((model, len(pts), conflicts, why))

    if verbose:
        print("  device trust:")
        for model, n, conflicts, why in sorted(notes, key=lambda x: -x[1]):
            mark = "keep" if model in trusted else "drop"
            extra = f", {conflicts} conflict(s)" if conflicts else ""
            print(f"    [{mark}] {model:<22} {n:>5} pts  ({why}{extra})")
    return trusted


def source_photos(db_path: Path, year: int, devices=None) -> list[Obs]:
    """Geotagged photos from the local Apple Photos library."""
    if not db_path.exists():
        return []
    rows = _photo_rows(db_path, year)
    keep = set(devices) if devices else trusted_devices(rows)
    return [Obs(day, lat, lng, "photo", model)
            for day, lat, lng, model in rows if model in keep]


_LATLNG_RE = re.compile(r"(-?\d+\.\d+)\s*°?\s*,\s*(-?\d+\.\d+)\s*°?")


def _parse_latlng(value):
    """Google writes coordinates as 'geo:40.7,-74.0' or '40.7°, -74.0°'."""
    if not isinstance(value, str):
        return None
    m = _LATLNG_RE.search(value.replace("geo:", ""))
    if not m:
        return None
    lat, lng = float(m.group(1)), float(m.group(2))
    if abs(lat) > 90 or abs(lng) > 180:
        return None
    return lat, lng


def _parse_time(value):
    if not isinstance(value, str):
        return None
    try:
        dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt


def _walk_takeout_json(obj, year, label, out, _time_ctx=None):
    """Recursively pull (time, lat, lng) pairs out of any Takeout JSON shape.

    Google has shipped several incompatible location schemas (Records.json,
    semanticSegments, Photos metadata, My Activity). Rather than hardcode each
    one, walk the tree and take any coordinate that has a timestamp in scope.
    """
    if isinstance(obj, dict):
        # A timestamp at this level applies to coordinates nested beneath it.
        here = _time_ctx
        for key in ("timestamp", "time", "startTime", "photoTakenTime", "endTime"):
            if key in obj:
                v = obj[key]
                if isinstance(v, dict):  # Photos: {"timestamp": "1712...", ...}
                    v = v.get("timestamp") or v.get("formatted")
                    if isinstance(v, str) and v.isdigit():
                        here = datetime.fromtimestamp(int(v), tz=timezone.utc)
                        break
                t = _parse_time(v)
                if t:
                    here = t
                    break

        # Old Records.json style: integer degrees x 1e7.
        if "latitudeE7" in obj and "longitudeE7" in obj:
            lat, lng = obj["latitudeE7"] / 1e7, obj["longitudeE7"] / 1e7
            if here and here.year == year and not (lat == 0 and lng == 0):
                out.append(Obs(here.date(), lat, lng, "takeout", label))

        # Photos metadata style: {"geoData": {"latitude": .., "longitude": ..}}
        for geo_key in ("geoData", "geoDataExif"):
            g = obj.get(geo_key)
            if isinstance(g, dict):
                lat, lng = g.get("latitude"), g.get("longitude")
                if here and lat and lng and here.year == year:
                    out.append(Obs(here.date(), lat, lng, "takeout", f"{label}/photo-exif"))

        for k, v in obj.items():
            if isinstance(v, str):
                ll = _parse_latlng(v)
                # Only trust coordinate-ish keys, so we don't scrape URLs at random.
                if ll and any(t in k.lower() for t in ("latlng", "point", "center", "location")):
                    if here and here.year == year:
                        out.append(Obs(here.date(), ll[0], ll[1], "takeout", f"{label}/{k}"))
            else:
                _walk_takeout_json(v, year, label, out, here)

    elif isinstance(obj, list):
        for item in obj:
            _walk_takeout_json(item, year, label, out, _time_ctx)


def _geo(value):
    """'geo:40.72,-73.95' -> (40.72, -73.95)."""
    if not isinstance(value, str) or not value.startswith("geo:"):
        return None
    try:
        lat, lng = value[4:].split(",")
        return float(lat), float(lng)
    except ValueError:
        return None


def _segment_points(seg):
    """Coordinates in a segment, each tagged with the day(s) it belongs to.

    A visit is stationary, so its location holds for every day the segment
    covers. A journey is not: its endpoints belong to the day it left and the
    day it arrived, and crediting both ends to both days would place the
    traveller at the destination before they got there.
    """
    pts = []
    visit = seg.get("visit", {}).get("topCandidate", {})
    g = _geo(visit.get("placeLocation"))
    if g:
        pts.append((g, visit.get("semanticType", "visit"), "span"))

    act = seg.get("activity", {})
    kind = act.get("topCandidate", {}).get("type", "activity")
    for end, anchor in (("start", "start"), ("end", "end")):
        g = _geo(act.get(end))
        if g:
            pts.append((g, kind, anchor))

    for step in seg.get("timelinePath", []) or []:
        g = _geo(step.get("point"))
        if g:
            pts.append((g, "path", "span"))
    return pts


def source_timeline(path: Path, year: int, max_span_days: int = 7) -> list[Obs]:
    """Google Maps Timeline, as exported from the phone.

    Segments carry a start and an end, so an overnight stay is evidence of
    presence on *both* days — which is how day-count rules read it.
    """
    if not path.exists():
        return []
    data = json.loads(path.read_text())
    if isinstance(data, dict):  # Android nests it; iOS exports a bare list.
        data = data.get("semanticSegments", data.get("timelineObjects", []))

    # timelinePath segments are stamped in UTC with no offset, while visits and
    # activities carry the local one. Collect the known offsets so a UTC-only
    # segment can borrow the offset in force at that moment.
    anchors = []
    for seg in data:
        t = _parse_time(seg.get("startTime"))
        if t and seg.get("startTime", "").strip()[-1] != "Z":
            anchors.append((t, t.utcoffset().total_seconds()))
    anchors.sort()

    def offset_at(when):
        if not anchors:
            return 0
        best = min(anchors, key=lambda a: abs((a[0] - when).total_seconds()))
        return best[1]

    out = []
    for seg in data:
        start, end = _parse_time(seg.get("startTime")), _parse_time(seg.get("endTime"))
        if not start:
            continue
        end = end or start
        pts = _segment_points(seg)
        if not pts:
            continue

        raw = seg.get("startTime", "")
        off = start.utcoffset().total_seconds() if raw.strip()[-1] != "Z" else offset_at(start)
        d0 = local_day(start.timestamp(), off)
        d1 = local_day(end.timestamp(), off)
        if (d1 - d0).days > max_span_days:  # a gap in the data, not a long stay
            d1 = d0

        for (lat, lng), kind, anchor in pts:
            if anchor == "start":
                days = [d0]
            elif anchor == "end":
                days = [d1]
            else:
                days = [d0 + timedelta(days=i) for i in range((d1 - d0).days + 1)]
            for day in days:
                if day.year == year:
                    out.append(Obs(day, lat, lng, "timeline", kind))
    return out


_MAPS_ENTRY = re.compile(
    r'@(-?\d{1,3}\.\d{4,}),(-?\d{1,3}\.\d{4,})'          # map centre in the URL
    r'.{0,400}?<br>([A-Z][a-z]{2} \d{1,2}, 20\d\d), (\d{1,2}:\d{2}:\d{2}\s*[AP]M)',
    re.S)


def source_maps_activity(path: Path, year: int) -> list[Obs]:
    """Coordinates out of a Takeout 'My Activity' HTML file.

    Every Maps search records the map centre, which is where the user was
    standing when they searched. Checked against Timeline over a 55-weekday
    window this was right about NYC every time it claimed it (27/27).

    Takeout renders all timestamps in the account's *current* display zone,
    so the abbreviation on them says nothing about where the user was; only
    the coordinates are load-bearing here.
    """
    if not path.exists():
        return []
    text = path.read_text(encoding="utf-8", errors="ignore")
    out = []
    for lat, lng, day_s, time_s in _MAPS_ENTRY.findall(text):
        try:
            # Google separates time and AM/PM with a narrow no-break space.
            stamp = f"{day_s} {time_s.replace(chr(8239), ' ').replace(chr(160), ' ')}"
            dt = datetime.strptime(stamp, "%b %d, %Y %I:%M:%S %p")
        except ValueError:
            continue
        if dt.year == year:
            out.append(Obs(dt.date(), float(lat), float(lng), "maps", path.parent.name))
    return out


def source_takeout(root: Path, year: int) -> list[Obs]:
    """Any geo signal we can find in a Google Takeout tree."""
    if not root.exists():
        return []
    out = []
    for path in sorted(root.rglob("MyActivity.html")):
        out += source_maps_activity(path, year)
    for path in sorted(root.rglob("*.json")):
        try:
            data = json.loads(path.read_text())
        except (json.JSONDecodeError, UnicodeDecodeError, OSError):
            continue
        label = path.relative_to(root).as_posix()
        _walk_takeout_json(data, year, label, out)
    return out


NY = ZoneInfo("America/New_York")


def eastern_offset(day: date) -> int:
    """Seconds of UTC offset in New York on a given date.

    Not a constant: -0500 is Eastern in January but *Central* in June, which
    matters if any of the away-days were in the Central zone.
    """
    return int(datetime(day.year, day.month, day.day, 12, tzinfo=NY).utcoffset().total_seconds())


def source_photo_tz(db_path: Path, year: int) -> dict:
    """Timezone offsets from photos with no GPS at all.

    A screenshot carries no coordinates but still records the offset of the
    clock that took it, which pins the time zone the user was standing in.
    """
    if not db_path.exists():
        return {}
    con = sqlite3.connect(f"file:{db_path}?immutable=1", uri=True)
    rows = con.execute(
        """
        SELECT a.ZDATECREATED, x.ZTIMEZONEOFFSET
        FROM ZASSET a JOIN ZADDITIONALASSETATTRIBUTES x ON x.ZASSET = a.Z_PK
        WHERE (a.ZLATITUDE IS NULL OR a.ZLATITUDE <= -180)
          AND x.ZTIMEZONEOFFSET IS NOT NULL AND a.ZTRASHEDDATE IS NULL
        """
    ).fetchall()
    con.close()
    out = {}
    for created, off in rows:
        d = local_day(created + COREDATA_EPOCH, off)
        if d.year == year:
            out.setdefault(d, set()).add(int(off))
    return out


def _git_identities() -> tuple:
    """Whose commits count as the user's, from local git config."""
    import subprocess

    out = []
    for key in ("user.email", "user.name"):
        try:
            r = subprocess.run(["git", "config", "--get", key],
                               capture_output=True, text=True, timeout=5)
        except (subprocess.SubprocessError, OSError):
            continue
        v = r.stdout.strip()
        if v:
            out.append(v.split("@")[0].lower() if key == "user.email" else v.lower())
    return tuple(dict.fromkeys(out))


def source_git_tz(roots, year: int, emails=()) -> dict:
    """Timezone offsets from the user's own git commits.

    A developer commits from wherever they are, and git records the committer's
    UTC offset. It is dense, dated and already on disk. Commits made through
    the GitHub web UI are stamped +0000 regardless of where the user was, so
    those identities are dropped.
    """
    import subprocess

    emails = tuple(e.lower() for e in emails) or _git_identities()
    if not emails:
        return {}  # nothing identifies the user's own commits; don't guess

    out = {}
    for root in roots:
        root = Path(root).expanduser()
        if not root.exists():
            continue
        for gitdir in root.glob("*/.git"):
            try:
                res = subprocess.run(
                    ["git", "-C", str(gitdir.parent), "log", "--all",
                     f"--since={year}-01-01", f"--until={year + 1}-01-01",
                     "--pretty=%ae|%ad", "--date=format:%Y-%m-%d|%z"],
                    capture_output=True, text=True, timeout=30)
            except (subprocess.SubprocessError, OSError):
                continue
            for line in res.stdout.splitlines():
                try:
                    email, day_s, off = line.split("|")
                except ValueError:
                    continue
                if "noreply.github.com" in email:
                    continue  # web merges are stamped UTC, not the user's clock
                if not any(e in email.lower() for e in emails):
                    continue
                secs = int(off[:3]) * 3600 + int(off[0] + off[3:]) * 60
                out.setdefault(date.fromisoformat(day_s), set()).add(secs)
    return out


def apply_tz_evidence(ledger: dict, tz_by_day: dict) -> dict:
    """Settle days using the clock the user's devices were set to.

    Weaker than a coordinate: an Eastern offset is consistent with New York but
    also with Boston or Miami, so it gets its own basis and is reported apart
    from the observed floor. It does two jobs here — it settles days that have
    no evidence at all, and it overrules a lone map viewport, which is a guess
    about location that a clock reading can contradict outright.

    A day showing two different inhabited zones is travel or ambiguity, not
    corroboration, so it settles nothing. UTC is ignored throughout: it means a
    build server, not a user standing on the Greenwich meridian.
    """
    for day, entry in ledger.items():
        if entry["basis"] not in ("no evidence", "viewport"):
            continue
        offsets = {o for o in tz_by_day.get(day, ()) if o != 0}
        if not offsets:
            continue
        east = eastern_offset(day)
        if east in offsets and len(offsets) == 1:
            ledger[day] = {"status": "NYC", "basis": "tz-corroborated",
                           "evidence": "New York clock, nothing contradicting it",
                           "sources": "timezone", "points": entry["points"]}
        elif east not in offsets:
            note = ("map centre overruled by a non-Eastern clock"
                    if entry["basis"] == "viewport" else
                    f"non-Eastern clock {sorted(o // 3600 for o in offsets)}")
            ledger[day] = {"status": "AWAY", "basis": "tz-corroborated",
                           "evidence": note, "sources": "timezone",
                           "points": entry["points"]}
        elif entry["basis"] == "viewport":
            # Eastern plus another zone: too ambiguous to keep a viewport guess.
            ledger[day] = {"status": "AWAY", "basis": "tz-corroborated",
                           "evidence": "map centre, clock ambiguous across zones",
                           "sources": "timezone", "points": entry["points"]}
    return ledger


# Airports whose terminals sit inside a state, so a boarding record is proof of
# presence there. Newark is deliberately absent: it is in New Jersey, and
# flying out of it says nothing about having been in New York that day.
AIRPORTS = {
    "JFK": (40.6413, -73.7781), "LGA": (40.7769, -73.8740),
    "EWR": (40.6895, -74.1745), "SFO": (37.6213, -122.3790),
    "BOS": (42.3656, -71.0096), "MIA": (25.7959, -80.2870),
    "MSP": (44.8848, -93.2223), "BNA": (36.1263, -86.6774),
    "HND": (35.5494, 139.7798), "SLC": (40.7899, -111.9791),
    "LAS": (36.0840, -115.1537), "ICT": (37.6499, -97.4331),
    "DFW": (32.8998, -97.0403),
}


def source_flights(path: Path, year: int) -> list[Obs]:
    """Flown segments from airline records.

    The strongest evidence in the stack: a third party recorded it at the time,
    which is the kind of document a day-count audit actually wants. A leg
    contributes a fix at each end, and the geometry decides what that means —
    departing LaGuardia lands inside New York, departing Newark does not.
    """
    if not path.exists():
        return []
    import csv as _csv

    out = []
    with open(path) as f:
        for row in _csv.DictReader(f):
            try:
                day = date.fromisoformat(row["date"])
            except (ValueError, KeyError):
                continue
            if day.year != year:
                continue
            if (row.get("status") or "flown").strip().lower() != "flown":
                continue  # a booking that was cancelled is not evidence of anything
            for code in (row.get("from"), row.get("to")):
                if code in AIRPORTS:
                    lat, lng = AIRPORTS[code]
                    out.append(Obs(day, lat, lng, "flight",
                                   f"{row.get('from')}>{row.get('to')} {row.get('conf','')}".strip()))
    return out


# ---------------------------------------------------------------- ledger


# A coordinate from a phone's GPS says where the device physically was. A
# coordinate lifted from a Maps search URL only says where the *map* was
# centred, which defaults to the last place viewed — usually home. Treat the
# two as different grades of evidence.
DEVICE_LOCATED = ("flight", "photo", "timeline")


def classify(obs: list[Obs], region) -> dict:
    """Group observations into one verdict per day.

    Presence outranks absence: one coordinate inside the region settles the day,
    because day-count rules ask whether any part of it was spent there. The one
    exception is a map viewport, which is discarded when a device-located source
    puts the user somewhere else the same day — searching Maps from a laptop in
    California happily reports a map still centred on Brooklyn.
    """
    by_day: dict[date, list[Obs]] = {}
    for o in obs:
        by_day.setdefault(o.day, []).append(o)

    days = {}
    for day, items in by_day.items():
        inside, outside = [], []
        for o in items:
            (inside if region.contains(Point(o.lng, o.lat)) else outside).append(o)

        dev_in = [o for o in inside if o.source in DEVICE_LOCATED]
        dev_out = [o for o in outside if o.source in DEVICE_LOCATED]
        sources = ",".join(sorted({o.source for o in items}))

        if dev_in:
            days[day] = {"status": "NYC", "basis": "observed",
                         "evidence": f"{len(dev_in)} device fix(es) inside",
                         "sources": sources, "points": items}
        elif inside and not dev_out:
            days[day] = {"status": "NYC", "basis": "viewport",
                         "evidence": f"{len(inside)} map centre(s), uncontradicted",
                         "sources": sources, "points": items}
        else:
            why = (f"{len(dev_out)} device fix(es) elsewhere"
                   if dev_out else f"{len(outside)} point(s), none inside")
            if inside and dev_out:
                why += f"; {len(inside)} map centre(s) discarded as contradicted"
            days[day] = {"status": "AWAY", "basis": "observed",
                         "evidence": why, "sources": sources, "points": items}
    return days


def place_name(points: list[Obs]) -> str:
    """Human label for where an away-day was, using the modal coordinate."""
    if not points:
        return ""
    import reverse_geocoder as rg

    lat, lng = Counter((round(p.lat, 1), round(p.lng, 1)) for p in points).most_common(1)[0][0]
    try:
        g = rg.search([(lat, lng)], mode=1)[0]
    except Exception:
        return f"{lat},{lng}"
    return f"{g.get('name','?')}, {g.get('admin1','?')} ({g.get('cc','?')})"


def fill_gaps(ledger: dict, start: date, end: date, max_gap: int) -> dict:
    """Infer unobserved days that sit between two matching observed days.

    Only fills a run when both ends agree and the run is short. Anything
    else stays UNKNOWN so it lands in the review queue instead of the total.
    """
    all_days = [start + timedelta(days=i) for i in range((end - start).days + 1)]
    for d in all_days:
        ledger.setdefault(d, {"status": "UNKNOWN", "basis": "no evidence",
                              "evidence": "", "sources": "", "points": []})

    observed = [d for d in all_days if ledger[d]["basis"] in ("observed", "viewport", "tz-corroborated")]
    for prev, nxt in zip(observed, observed[1:]):
        gap = (nxt - prev).days - 1
        if gap <= 0 or gap > max_gap:
            continue
        a, b = ledger[prev]["status"], ledger[nxt]["status"]
        if a != b:
            continue  # travel happened somewhere in here; don't guess.
        for i in range(1, gap + 1):
            d = prev + timedelta(days=i)
            ledger[d] = {
                "status": a,
                "basis": "inferred",
                "evidence": f"between {prev} and {nxt}, both {a}",
                "sources": "",
                "points": [],
            }
    return ledger


# ---------------------------------------------------------------- report


def main():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--year", "-y", type=int, default=datetime.now().year)
    p.add_argument("--photos", type=Path, default=DEFAULT_PHOTOS_DB,
                   help="Apple Photos library sqlite (default: your local library)")
    p.add_argument("--takeout", type=Path, action="append", default=[],
                   help="Google Takeout directory; repeatable")
    p.add_argument("--flights", type=Path, action="append", default=[],
                   help="CSV of flown segments: date,from,to,conf,source")
    p.add_argument("--timeline", type=Path, action="append", default=[],
                   help="Google Maps Timeline JSON exported from your phone; repeatable")
    p.add_argument("--no-photos", action="store_true")
    p.add_argument("--no-tz", action="store_true",
                   help="Skip timezone corroboration from screenshots and git commits")
    p.add_argument("--author", action="append", default=[],
                   help="Substring identifying your commits (default: git config user.email)")
    p.add_argument("--git", type=Path, action="append",
                   default=[Path.home() / "Development"],
                   help="Directory of git repos to read commit timezones from")
    p.add_argument("--device", action="append", default=[],
                   help="Only trust these camera models (default: auto-detect)")
    p.add_argument("--max-gap", type=int, default=3,
                   help="Longest run of unobserved days to infer across (default 3)")
    p.add_argument("--output", "-o", type=Path, help="Write the day-by-day ledger as CSV")
    args = p.parse_args()

    obs: list[Obs] = []
    if not args.no_photos:
        got = source_photos(args.photos, args.year, args.device or None)
        print(f"  photos    {len(got):>6} observations")
        obs += got
    for fl in args.flights:
        got = source_flights(fl, args.year)
        print(f"  flights   {len(got):>6} observations  ({fl.name})")
        obs += got
    for tl in args.timeline:
        got = source_timeline(tl, args.year)
        print(f"  timeline  {len(got):>6} observations  ({tl.name})")
        obs += got
    for t in args.takeout:
        got = source_takeout(t, args.year)
        print(f"  takeout   {len(got):>6} observations  ({t})")
        obs += got

    if not obs:
        print("\nNo observations found. Nothing to report.", file=sys.stderr)
        sys.exit(1)

    in_nyc = load_nyc()
    ledger = classify(obs, in_nyc)

    tz_by_day = {}
    if not args.no_tz:
        for d, v in source_photo_tz(args.photos, args.year).items():
            tz_by_day.setdefault(d, set()).update(v)
        for d, v in source_git_tz(args.git, args.year, tuple(args.author)).items():
            tz_by_day.setdefault(d, set()).update(v)
        print(f"  timezone  {len(tz_by_day):>6} days with a clock reading")

    start = date(args.year, 1, 1)
    end = min(date(args.year, 12, 31), date.today())
    for d in [start + timedelta(days=i) for i in range((end - start).days + 1)]:
        ledger.setdefault(d, {"status": "UNKNOWN", "basis": "no evidence",
                              "evidence": "", "sources": "", "points": []})
    ledger = apply_tz_evidence(ledger, tz_by_day)
    ledger = fill_gaps(ledger, start, end, args.max_gap)

    days = sorted(ledger)
    weekdays = [d for d in days if d.weekday() < 5]

    def count(dd, status, basis=None):
        return sum(1 for d in dd if ledger[d]["status"] == status
                   and (basis is None or ledger[d]["basis"] == basis))

    nyc_obs_wd = count(weekdays, "NYC", "observed")
    nyc_vp_wd = count(weekdays, "NYC", "viewport")
    nyc_tz_wd = count(weekdays, "NYC", "tz-corroborated")
    nyc_inf_wd = count(weekdays, "NYC", "inferred")
    unknown_wd = count(weekdays, "UNKNOWN")

    print(f"\n{'='*62}")
    print(f"  NYC weekday count — {args.year} through {end}")
    print(f"{'='*62}")
    print(f"  Observed in NYC          {nyc_obs_wd:>4}   <- defensible floor")
    print(f"  Map-centre only          {nyc_vp_wd:>4}   (uncontradicted viewport)")
    print(f"  Timezone-corroborated    {nyc_tz_wd:>4}   (NY clock, ~93% precise)")
    print(f"  Inferred in NYC          {nyc_inf_wd:>4}   (between two NYC days)")
    print(f"  {'-'*46}")
    print(f"  Best estimate            {nyc_obs_wd + nyc_vp_wd + nyc_tz_wd + nyc_inf_wd:>4}")
    print(f"  Unresolved weekdays      {unknown_wd:>4}   <- review these")
    print(f"{'='*62}")
    print(f"  Weekdays elapsed         {len(weekdays):>4}")
    print(f"  Observed away            {count(weekdays,'AWAY','observed'):>4}")

    # Month-by-month, so a suspicious stretch is easy to spot.
    print(f"\n  {'month':<9}{'nyc':>5}{'tz':>5}{'inf':>5}{'away':>6}{'unk':>5}")
    for m in range(1, end.month + 1):
        wd = [d for d in weekdays if d.month == m]
        if not wd:
            continue
        print(f"  {date(args.year,m,1):%b %Y:<4}"[:11].ljust(11)
              + f"{count(wd,'NYC','observed'):>3}{count(wd,'NYC','tz-corroborated'):>5}"
              + f"{count(wd,'NYC','inferred'):>5}"
              + f"{count(wd,'AWAY','observed'):>6}{count(wd,'UNKNOWN'):>5}")

    if args.output:
        import csv
        with open(args.output, "w", newline="") as f:
            w = csv.writer(f)
            w.writerow(["date", "weekday", "is_weekday", "status", "basis",
                        "evidence", "sources", "place"])
            for d in days:
                e = ledger[d]
                place = place_name(e["points"]) if e["status"] == "AWAY" and e["points"] else ""
                w.writerow([d, d.strftime("%a"), d.weekday() < 5, e["status"],
                            e["basis"], e["evidence"], e["sources"], place])
        print(f"\n  Ledger written to {args.output}")


if __name__ == "__main__":
    main()
