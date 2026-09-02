#!/usr/bin/env python3
"""Pull a week of Clozemaster study data out into a lesson sheet.

    uv run klozar.py lesson                 # last 7 days -> out/<date>-serbian-lesson.md
    uv run klozar.py lesson --days 14
    uv run klozar.py snapshot               # raw per-sentence JSON, for history/diffing
    uv run klozar.py collections            # what's in the account

Clozemaster has no public API. The web app talks to an undocumented JSON API
under /api/v1/lp/<pairing>/, which is what this uses. Two things to know:

  * Every /api/v1 call needs a `Time-Zone-Offset-Hours` header. Omit it and the
    server answers 400 Bad Request, which reads like an auth failure and isn't.
  * Auth is the `_clozemaster_session` cookie. It's HttpOnly, so copy it out of
    DevTools once into .env (see .env.example). Without it the API still answers
    200 — with the public catalog and none of your progress — so we check that
    a username came back rather than trusting the status code.
"""

import argparse
import json
import os
import re
import sys
from collections import Counter
from datetime import date, datetime, timedelta
from pathlib import Path

import httpx

HERE = Path(__file__).parent
BASE = "https://www.clozemaster.com"
CLOZE = re.compile(r"\{\{(.+?)\}\}")


# --- config -----------------------------------------------------------------


def load_env() -> dict[str, str]:
    """Read .env next to this script, letting the real environment win."""
    env = {}
    dotenv = HERE / ".env"
    if dotenv.exists():
        for line in dotenv.read_text().splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip().strip("'\"")
    env.update({k: v for k, v in os.environ.items() if k.startswith("CLOZEMASTER_")})
    return env


# --- api --------------------------------------------------------------------


class Clozemaster:
    def __init__(self, session: str, pairing: str):
        offset = round(datetime.now().astimezone().utcoffset().total_seconds() / 3600)
        self.pairing = pairing
        self.http = httpx.Client(
            base_url=BASE,
            timeout=30,
            cookies={"_clozemaster_session": session},
            headers={
                "Time-Zone-Offset-Hours": str(offset),
                "X-Requested-With": "XMLHttpRequest",
                "Accept": "*/*",
            },
        )

    def get(self, path: str, **params):
        r = self.http.get(path, params=params)
        r.raise_for_status()
        return r.json()

    def dashboard(self) -> dict:
        """Collections, per-collection stats, and the URLs for everything else."""
        data = self.get(
            f"/api/v1/lp/{self.pairing}",
            include_cefr="true",
            include_fast_track_v2_collections="true",
        )
        if not (data.get("user") or {}).get("username"):
            sys.exit(
                "Not signed in — Clozemaster returned the public catalog with no "
                "progress. Your CLOZEMASTER_SESSION cookie is missing or expired; "
                "recopy it from DevTools (see .env.example)."
            )
        return data

    def sentences(self, collection: dict, scope: str = "playing") -> list[dict]:
        """Every sentence in a collection at the given scope, with your progress.

        per_page is generous on purpose: a 1,000-sentence collection comes back
        in one request, so there's no pagination to get wrong.
        """
        out, page = [], 1
        while True:
            data = self.get(
                collection["collectionClozeSentencesUrl"],
                scope=scope,
                page=page,
                per_page=500,
            )
            batch = data.get("collectionClozeSentences") or []
            out.extend(batch)
            if len(out) >= data.get("total", 0) or not batch:
                return out
            page += 1


# --- shaping ----------------------------------------------------------------


def answer_of(sentence: dict) -> str:
    """The cloze word — the bit the app blanks out."""
    m = CLOZE.search(sentence.get("text") or "")
    return m.group(1) if m else ""


def plain(sentence: dict) -> str:
    return CLOZE.sub(r"\1", sentence.get("text") or "")


def collect_week(cm: Clozemaster, days: int) -> dict:
    """Everything played in the last `days`, grouped by how it went."""
    dash = cm.dashboard()
    cutoff = (date.today() - timedelta(days=days)).isoformat()

    played = []
    for collection in dash["collections"]:
        if not collection.get("playing"):
            continue
        for s in cm.sentences(collection):
            if (s.get("lastPlayedDate") or "") >= cutoff:
                s["_collection"] = collection["name"]
                played.append(s)

    def miss_rate(s):
        return s.get("numIncorrect", 0) / max(s.get("numPlayed", 0), 1)

    # Miss rate ties are common (half the week sits at "1 of 2"), so break them
    # on recency — a sentence missed yesterday is worth more of a tutor's time
    # than the same sentence missed six days ago.
    struggles = sorted(played, key=lambda s: s.get("lastPlayedDate") or "", reverse=True)
    struggles = [s for s in struggles if s.get("numIncorrect", 0) > 0]
    struggles.sort(key=lambda s: (-miss_rate(s), -s.get("numIncorrect", 0)))
    # No creation timestamp is exposed, so "new" is inferred: seen at most twice
    # and last seen inside the window means it almost certainly started here.
    # Newest first, or a --limit cut only ever shows the oldest day of the week.
    fresh = sorted(
        (s for s in played if s.get("numPlayed", 0) <= 2),
        key=lambda s: s.get("lastPlayedDate") or "",
        reverse=True,
    )
    starred = [s for s in played if s.get("favorited")]

    words = Counter(answer_of(s).lower() for s in played if answer_of(s))

    return {
        "user": dash["user"],
        "cutoff": cutoff,
        "days": days,
        "played": played,
        "struggles": struggles,
        "fresh": fresh,
        "starred": starred,
        "words": words,
    }


# --- output -----------------------------------------------------------------


def entry(s: dict) -> str:
    bits = [f"level {s.get('level', 0)}"]
    if s.get("numIncorrect"):
        bits.append(f"missed {s['numIncorrect']} of {s.get('numPlayed', 0)}")
    else:
        bits.append(f"seen {s.get('numPlayed', 0)}×")
    if s.get("favorited"):
        bits.append("starred")
    line = f"- **{plain(s)}** — {s.get('translation') or ''}\n"
    line += f"  `{answer_of(s)}` · {' · '.join(bits)} · last {s.get('lastPlayedDate')}"
    if s.get("notes"):
        line += f"\n  > {s['notes']}"
    return line


def render(week: dict, limit: int) -> str:
    today = date.today()
    start = date.fromisoformat(week["cutoff"])
    md = [
        f"# Serbian — week of {start:%-d %b} to {today:%-d %b %Y}",
        "",
        f"{len(week['played'])} sentences practiced over {week['days']} days · "
        f"{len(week['struggles'])} tripped me up · {len(week['starred'])} starred.",
        "",
        "## Gave me trouble",
        "",
        "Ranked by how often I got them wrong. This is the list worth talking through.",
        "",
    ]
    md += [entry(s) for s in week["struggles"][:limit]] or ["_Clean week — nothing missed._"]

    if week["starred"]:
        md += ["", "## Starred this week", "",
               "Sentences I flagged in the app while playing.", ""]
        md += [entry(s) for s in week["starred"]]

    if week["fresh"]:
        md += ["", "## New this week", "",
               "First encounters — still fragile.", ""]
        md += [entry(s) for s in week["fresh"][:limit]]

    top = [w for w, n in week["words"].most_common(30) if n > 1]
    if top:
        md += ["", "## Words that kept coming up", "", ", ".join(f"`{w}`" for w in top)]

    md += ["", "---", "",
           f"Pulled from Clozemaster on {today:%-d %b %Y} for {week['user'].get('username')}."]
    return "\n".join(md) + "\n"


# --- commands ---------------------------------------------------------------


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = p.add_subparsers(dest="cmd", required=True)

    lesson = sub.add_parser("lesson", help="weekly markdown lesson sheet")
    lesson.add_argument("--days", type=int, default=7)
    lesson.add_argument("--limit", type=int, default=25, help="entries per section")
    lesson.add_argument("--out", type=Path, help="default: out/<date>-serbian-lesson.md")
    lesson.add_argument("--stdout", action="store_true", help="print instead of writing")

    snap = sub.add_parser("snapshot", help="raw per-sentence JSON")
    snap.add_argument("--scope", default="playing",
                      help="all | playing | favorited | mastered | ready_for_review | ignored")
    snap.add_argument("--out", type=Path, help="default: out/<date>-snapshot.json")

    sub.add_parser("collections", help="list collections and their counts")

    args = p.parse_args()
    env = load_env()
    session = env.get("CLOZEMASTER_SESSION")
    if not session or session == "paste_the_value_here":
        sys.exit("Set CLOZEMASTER_SESSION in tools/klozar/.env — see .env.example.")
    cm = Clozemaster(session, env.get("CLOZEMASTER_PAIRING", "srp-eng"))

    out_dir = HERE / "out"

    if args.cmd == "collections":
        for c in cm.dashboard()["collections"]:
            if not c.get("playing"):
                continue
            print(f"{c['name']:<28} playing {c['numPlaying']:>4} · "
                  f"starred {c['numFavorited']:>3} · mastered {c['numMastered']:>3} · "
                  f"due {c['numReadyForReview']:>3}")
        return

    if args.cmd == "snapshot":
        dash = cm.dashboard()
        data = {
            c["name"]: cm.sentences(c, scope=args.scope)
            for c in dash["collections"] if c.get("playing")
        }
        out = args.out or out_dir / f"{date.today()}-snapshot.json"
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(json.dumps(data, ensure_ascii=False, indent=2))
        print(f"{sum(len(v) for v in data.values())} sentences -> {out}")
        return

    week = collect_week(cm, args.days)
    md = render(week, args.limit)
    if args.stdout:
        print(md)
        return
    out = args.out or out_dir / f"{date.today()}-serbian-lesson.md"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(md)
    print(f"{len(week['played'])} sentences this week -> {out}")


if __name__ == "__main__":
    main()
