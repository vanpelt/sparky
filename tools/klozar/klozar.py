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
import subprocess
import sys
from collections import Counter
from datetime import date, datetime, timedelta
from pathlib import Path

import httpx

HERE = Path(__file__).parent
BASE = "https://www.clozemaster.com"
CLOZE = re.compile(r"\{\{(.+?)\}\}")


# --- config -----------------------------------------------------------------


KEYCHAIN_SERVICE = "klozar-clozemaster"


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


# --- cookie storage ---------------------------------------------------------
#
# The session cookie rotates on every response — the server re-issues it with a
# fresh last_request_at, so the idle clock resets each time it's used. Storing
# the rotated value back is what makes a scheduled run keep working forever
# instead of dying on whatever Devise timeout is configured. It's also why a
# static CI secret is the wrong home for it: a secret can't absorb the rotation.


def keychain_read() -> str | None:
    try:
        r = subprocess.run(
            ["security", "find-generic-password", "-s", KEYCHAIN_SERVICE, "-w"],
            capture_output=True, text=True,
        )
    except FileNotFoundError:
        return None  # not macOS
    return r.stdout.strip() or None if r.returncode == 0 else None


def keychain_write(value: str) -> bool:
    try:
        r = subprocess.run(
            ["security", "add-generic-password", "-U", "-s", KEYCHAIN_SERVICE,
             "-a", os.environ.get("USER", "klozar"), "-w", value],
            capture_output=True, text=True,
        )
    except FileNotFoundError:
        return False
    return r.returncode == 0


def env_file_write(value: str) -> bool:
    """Update CLOZEMASTER_SESSION in .env, leaving the rest of the file alone."""
    dotenv = HERE / ".env"
    if not dotenv.exists():
        return False
    lines = dotenv.read_text(encoding="utf-8").splitlines(keepends=True)
    for i, line in enumerate(lines):
        if line.startswith("CLOZEMASTER_SESSION="):
            lines[i] = f"CLOZEMASTER_SESSION={value}\n"
            dotenv.write_text("".join(lines), encoding="utf-8")
            return True
    return False


def read_session(env: dict[str, str]) -> tuple[str | None, str]:
    """The cookie and where it came from, so the rotated one goes back there."""
    if os.environ.get("CLOZEMASTER_SESSION"):
        return os.environ["CLOZEMASTER_SESSION"], "env"
    kc = keychain_read()
    if kc:
        return kc, "keychain"
    v = env.get("CLOZEMASTER_SESSION")
    if v and v != "paste_the_value_here":
        return v, "dotenv"
    return None, "none"


def write_session(value: str, origin: str) -> None:
    if origin == "keychain":
        keychain_write(value)
    elif origin == "dotenv":
        env_file_write(value)
    # "env" came from the process environment — nothing durable to write back to.


# --- api --------------------------------------------------------------------


class Clozemaster:
    def __init__(self, session: str, pairing: str, origin: str = "env"):
        offset = round(datetime.now().astimezone().utcoffset().total_seconds() / 3600)
        self.pairing = pairing
        self.session = session
        self.origin_value = session
        self.origin = origin
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
        fresh = r.cookies.get("_clozemaster_session")
        if fresh and fresh != self.session:
            self.session = fresh
        return r.json()

    def persist(self) -> None:
        """Save the rolled-forward cookie so the next run starts from a fresh clock."""
        if self.session != self.origin_value:
            write_session(self.session, self.origin)

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


def as_item(s: dict) -> dict:
    """The subset of a sentence the page actually renders."""
    return {
        "id": s["id"],
        "text": s.get("text") or "",
        "plain": plain(s),
        "translation": s.get("translation") or "",
        "level": s.get("level", 0),
        "played": s.get("numPlayed", 0),
        "missed": s.get("numIncorrect", 0),
        "favorited": bool(s.get("favorited")),
        "last": s.get("lastPlayedDate") or "",
    }


STANDALONE_HEAD = """<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<meta name="description" content="A week of Serbian practice from Clozemaster, ranked by where I slipped.">
<meta name="color-scheme" content="light dark">
<meta property="og:type" content="website">
<meta property="og:title" content="Serbian Lesson Sheet">
<meta property="og:description" content="A week of Serbian practice from Clozemaster, ranked by where I slipped.">
<style>body{margin:0}img{max-width:100%}[hidden]{display:none!important}</style>
"""


def notes_backend() -> dict | None:
    """The shared-notes store baked into the published page, if configured.

    The token in notes-backend.json is deliberately public — see the note in that
    file. Only url and token reach the page; the schema and comment stay here.
    """
    cfg = HERE / "notes-backend.json"
    if not cfg.exists():
        return None
    data = json.loads(cfg.read_text(encoding="utf-8"))
    if not data.get("url") or not data.get("token"):
        return None
    return {"url": data["url"].rstrip("/"), "token": data["token"]}


def update_manifest(root: Path, week: dict, entry_label: str) -> list[dict]:
    """Upsert this week into weeks.json, newest first.

    The manifest is read at page load rather than baked in, so a sheet published
    in March still lists weeks that only exist in May.
    """
    path = root / "weeks.json"
    weeks = []
    if path.exists():
        try:
            weeks = json.loads(path.read_text(encoding="utf-8"))
        except json.JSONDecodeError:
            weeks = []
    stamp = date.today().isoformat()
    weeks = [w for w in weeks if w.get("week") != stamp]
    weeks.append({
        "week": stamp,
        "label": entry_label,
        "played": len(week["played"]),
        "missed": len(week["struggles"]),
        "starred": len(week["starred"]),
    })
    weeks.sort(key=lambda w: w["week"], reverse=True)
    path.write_text(json.dumps(weeks, indent=2) + "\n", encoding="utf-8")
    return weeks


def render_html(week: dict, limit: int, standalone: bool = False,
                notes: dict | None = None, archive: dict | None = None) -> str:
    """The same sheet as an interactive page, data baked in.

    The artifact host supplies <!doctype>, charset and a small reset, so the
    template is body content only. `standalone` wraps it for GitHub Pages, which
    serves the file as-is — without the charset every č ć š ž đ turns to mojibake.
    """
    today = date.today()
    start = date.fromisoformat(week["cutoff"])
    data = {
        "week": today.isoformat(),
        "eyebrow": f"Week of {start:%-d %B} – {today:%-d %B %Y}",
        "title": "Serbian lesson sheet",
        "counts": {
            "played": len(week["played"]),
            "missed": len(week["struggles"]),
            "starred": len(week["starred"]),
        },
        "sections": [
            {"title": "Gave me trouble",
             "blurb": "Ranked by how often I got them wrong. The heavier the red edge, "
                      "the worse the ratio. This is the list worth the hour.",
             "items": [as_item(s) for s in week["struggles"][:limit]]},
            {"title": "Starred this week",
             "blurb": "Flagged in the app while playing.",
             "items": [as_item(s) for s in week["starred"]]},
            {"title": "New this week",
             "blurb": "Inferred first encounters — seen twice or fewer. Still fragile.",
             "items": [as_item(s) for s in week["fresh"][:limit]]},
        ],
        "words": [w for w, n in week["words"].most_common(30) if n > 1],
        "stamp": f"Pulled {today:%-d %B %Y} for {week['user'].get('username')}",
        "notes": notes,
        "archive": archive,
    }
    tpl = (HERE / "template.html").read_text(encoding="utf-8")
    # json.dumps can emit "</script>" inside a string and close the tag early.
    blob = json.dumps(data, ensure_ascii=False).replace("</", "<\\/")
    page = tpl.replace("/*__DATA__*/", blob)
    if not standalone:
        return page
    # The <title> and <style> the template opens with belong in <head>; the rest
    # is body. Splitting on the first tag after them keeps that honest.
    head_end = page.index("<div class=\"wrap\">")
    return STANDALONE_HEAD + page[:head_end] + "</head>\n<body>\n" + \
        page[head_end:] + "</body>\n</html>\n"


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

    art = sub.add_parser("artifact", help="interactive HTML sheet, ready to publish")
    art.add_argument("--days", type=int, default=7)
    art.add_argument("--limit", type=int, default=25, help="entries per section")
    art.add_argument("--out", type=Path, help="default: out/<date>-serbian-lesson.html")

    snap = sub.add_parser("snapshot", help="raw per-sentence JSON")
    snap.add_argument("--scope", default="playing",
                      help="all | playing | favorited | mastered | ready_for_review | ignored")
    snap.add_argument("--out", type=Path, help="default: out/<date>-snapshot.json")

    site = sub.add_parser("site", help="write the sheet into the GitHub Pages tree")
    site.add_argument("--days", type=int, default=7)
    site.add_argument("--limit", type=int, default=25, help="entries per section")
    site.add_argument("--out", type=Path, help="default: index.html beside this script")
    site.add_argument("--no-notes", action="store_true",
                      help="omit the shared-notes store; notes stay per-device")

    sub.add_parser("collections", help="list collections and their counts")
    sub.add_parser("auth", help="store the session cookie in the macOS Keychain")

    args = p.parse_args()
    env = load_env()

    if args.cmd == "auth":
        print("Paste the _clozemaster_session cookie value, then press Enter.")
        print("Chrome > DevTools > Application > Cookies > www.clozemaster.com")
        value = sys.stdin.readline().strip()
        if not value:
            sys.exit("Nothing pasted.")
        if not keychain_write(value):
            sys.exit("Could not write to the Keychain — is this macOS?")
        print(f"Stored in the login Keychain as “{KEYCHAIN_SERVICE}”.")
        print("klozar re-saves the rotated cookie after every run, so regular use "
              "keeps it alive.")
        return

    session, origin = read_session(env)
    if not session:
        sys.exit("No session cookie. Run `uv run klozar.py auth` to store one in "
                 "the Keychain. (CLOZEMASTER_SESSION in the environment also works, "
                 "off macOS — but nothing writes the rotated cookie back to it.)")
    cm = Clozemaster(session, env.get("CLOZEMASTER_PAIRING", "srp-eng"), origin)

    out_dir = HERE / "out"

    if args.cmd == "collections":
        for c in cm.dashboard()["collections"]:
            if not c.get("playing"):
                continue
            print(f"{c['name']:<28} playing {c['numPlaying']:>4} · "
                  f"starred {c['numFavorited']:>3} · mastered {c['numMastered']:>3} · "
                  f"due {c['numReadyForReview']:>3}")
        cm.persist()
        return

    if args.cmd == "snapshot":
        dash = cm.dashboard()
        data = {
            c["name"]: cm.sentences(c, scope=args.scope)
            for c in dash["collections"] if c.get("playing")
        }
        out = args.out or out_dir / f"{date.today()}-snapshot.json"
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
        cm.persist()
        print(f"{sum(len(v) for v in data.values())} sentences -> {out}")
        return

    if args.cmd == "artifact":
        week = collect_week(cm, args.days)
        out = args.out or out_dir / f"{date.today()}-serbian-lesson.html"
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(render_html(week, args.limit), encoding="utf-8")
        cm.persist()
        print(f"{len(week['played'])} sentences this week -> {out}")
        print("Publish it with the Artifact tool (capabilities: db) to get a link.")
        return

    if args.cmd == "site":
        week = collect_week(cm, args.days)
        # Only the published page gets the shared store; inside an artifact the
        # CSP blocks the fetch anyway, and that copy uses claude.use("db").
        notes = None if args.no_notes else notes_backend()
        root = args.out.parent if args.out else HERE
        label = f"{date.fromisoformat(week['cutoff']):%-d %b} – {date.today():%-d %b %Y}"
        update_manifest(root, week, label)

        # index.html is the canonical latest sheet; weeks/<date>.html is its
        # permanent home. Same content, different relative paths to the manifest
        # and to sibling weeks, so each knows how to reach the others.
        index = args.out or root / "index.html"
        archived = root / "weeks" / f"{date.today()}.html"
        archived.parent.mkdir(parents=True, exist_ok=True)
        index.parent.mkdir(parents=True, exist_ok=True)

        index.write_text(
            render_html(week, args.limit, True, notes,
                        {"manifest": "weeks.json", "base": "weeks/"}),
            encoding="utf-8")
        archived.write_text(
            render_html(week, args.limit, True, notes,
                        {"manifest": "../weeks.json", "base": ""}),
            encoding="utf-8")
        cm.persist()
        print(f"{len(week['played'])} sentences this week -> {index}")
        print(f"archived at {archived}")
        print("Commit and push; the pages.yml workflow publishes it at "
              "https://vanpelt.github.io/sparky/tools/klozar/")
        return

    week = collect_week(cm, args.days)
    md = render(week, args.limit)
    cm.persist()
    if args.stdout:
        print(md)
        return
    out = args.out or out_dir / f"{date.today()}-serbian-lesson.md"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(md, encoding="utf-8")
    print(f"{len(week['played'])} sentences this week -> {out}")


if __name__ == "__main__":
    main()
