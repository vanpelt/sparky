"""Reframe lab: label and score the views Reframe aims shots with.

    uv run lab.py export                 # reframe-cli lab-export cloud -> the data folder
    uv run lab.py serve [--port 8765]    # the grid, labelling, prompts, probes, metrics
    uv run lab.py run pplx-decider-flash [--limit N]   # score views with a local Ollama model

Data lives in ~/Library/Application Support/Reframe/lab (override with --data): the export's
manifest.json, embeddings.f16 and views/, plus lab.sqlite with your labels and model predictions.
See README.md.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import random
import sqlite3
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

import numpy as np

HERE = Path(__file__).resolve().parent
DEFAULT_DATA = Path.home() / "Library/Application Support/Reframe/lab"
DEFAULT_CLI = HERE.parent / "reframe/.build/release/reframe-cli"
OLLAMA = "http://localhost:11434"

# Human ratings, 0..3. "Good" and "Great" count as positives in metrics.
RATINGS = ["Junk", "Filler", "Good", "Great"]

# The decision-model questions (Ollama /v1/systemone). Bump QUESTIONS_VERSION when editing them;
# `run --force` re-scores.
QUESTIONS_VERSION = 1
STATE = (
    "A 90-degree view cut from a 360-degree action camera recording (off-road trips, hiking, "
    "biking, hanging out with friends). A video editor is choosing where to point the virtual "
    "camera for a highlight reel."
)
QUESTIONS = {
    "aim": {"type": "noul", "instructions": "Would a good video editor point the camera in this direction?"},
    "quality": {
        "type": "score",
        "instructions": "How good is this view as a shot?",
        "criteria": [
            "Unusable: blur, lens covered, black",
            "Junk: ground, sky, vehicle interior, the camera operator",
            "Filler: ordinary, nothing happening",
            "Decent: usable B-roll",
            "Good: a nice shot worth keeping",
            "Great: a highlight-reel moment",
        ],
    },
    "notable": {"type": "noul", "instructions": "Is something notable or unusual happening in this view?"},
    "subject": {
        "type": "choice",
        "instructions": "What is the main subject?",
        "criteria": {
            "people": "People or faces",
            "vehicle": "Vehicles in action",
            "landscape": "Scenery or landscape",
            "interior": "Inside a vehicle, or the camera operator",
            "ground": "Only the ground or the sky",
            "other": None,
        },
    },
}

CHAT_PROMPT = (
    "This is a 90-degree view cut from a 360-degree action camera video. A video editor is choosing "
    "where to point the camera for a highlight reel. Rate this view from 0 (junk: ground, sky, vehicle "
    "interior, the operator) to 5 (a highlight-reel moment), list a few short content tags, and write "
    "a one-sentence caption. Reply with JSON only."
)
CHAT_SCHEMA = {
    "type": "object",
    "properties": {
        "rating": {"type": "integer", "minimum": 0, "maximum": 5},
        "tags": {"type": "array", "items": {"type": "string"}},
        "caption": {"type": "string"},
    },
    "required": ["rating", "tags", "caption"],
}


# MARK: Data


class Lab:
    def __init__(self, data: Path, cli: Path):
        self.data = data
        self.cli = cli
        manifest = json.loads((data / "manifest.json").read_text())
        self.manifest = manifest
        self.views: list[dict] = manifest["views"]
        self.index = {v["id"]: i for i, v in enumerate(self.views)}
        dim = manifest["dimension"]
        self.emb = np.fromfile(data / "embeddings.f16", dtype=np.float16).astype(np.float32).reshape(-1, dim)
        assert len(self.emb) == len(self.views), "embeddings and manifest disagree: re-export"
        self.lock = threading.Lock()
        self.db = sqlite3.connect(data / "lab.sqlite", check_same_thread=False)
        self.db.execute("pragma journal_mode=wal")
        self.db.executescript(
            """
            create table if not exists labels (view_id text primary key, rating integer, notable integer default 0,
                                               updated real);
            create table if not exists preds (view_id text, source text, value real, detail text, updated real,
                                              primary key (view_id, source));
            create table if not exists jobs (id integer primary key, model text, kind text, total integer,
                                             done integer default 0, errors integer default 0, last_error text,
                                             state text, started real, updated real, finished real, pid integer,
                                             seconds real default 0);
            """
        )
        self.text_cache: dict[str, np.ndarray] = {}
        self.probes: dict[str, dict] = {}

    # Labels and predictions

    def labels(self) -> dict[str, dict]:
        with self.lock:
            rows = self.db.execute("select view_id, rating, notable from labels").fetchall()
        return {r[0]: {"rating": r[1], "notable": bool(r[2])} for r in rows}

    def set_label(self, view_id: str, rating: int | None, notable: bool):
        with self.lock:
            if rating is None and not notable:
                self.db.execute("delete from labels where view_id = ?", (view_id,))
            else:
                self.db.execute(
                    "insert into labels values (?, ?, ?, ?) on conflict(view_id) do update set "
                    "rating = excluded.rating, notable = excluded.notable, updated = excluded.updated",
                    (view_id, rating, int(notable), time.time()),
                )
            self.db.commit()

    def preds(self) -> dict[str, dict[str, float]]:
        """source -> {view_id: value}"""
        with self.lock:
            rows = self.db.execute("select source, view_id, value from preds").fetchall()
        out: dict[str, dict[str, float]] = {}
        for s, v, x in rows:
            out.setdefault(s, {})[v] = x
        return out

    def detail(self, view_id: str) -> dict:
        with self.lock:
            rows = self.db.execute("select source, value, detail from preds where view_id = ?", (view_id,)).fetchall()
        return {s: {"value": x, "detail": json.loads(d) if d else None} for s, x, d in rows}

    def put_preds(self, view_id: str, values: dict[str, tuple[float, object]]):
        with self.lock:
            for source, (value, detail) in values.items():
                self.db.execute(
                    "insert or replace into preds values (?, ?, ?, ?, ?)",
                    (view_id, source, float(value), json.dumps(detail) if detail is not None else None, time.time()),
                )
            self.db.commit()

    def preds_since(self, t: float) -> dict[str, dict[str, float]]:
        with self.lock:
            rows = self.db.execute("select source, view_id, value from preds where updated > ?", (t,)).fetchall()
        out: dict[str, dict[str, float]] = {}
        for s, v, x in rows:
            out.setdefault(s, {})[v] = x
        return out

    # Model runs: state lives in the jobs table so the page can watch and steer a run started from
    # the CLI too. A run polls its own row between views for pause / stop.

    def job(self) -> dict | None:
        with self.lock:
            cur = self.db.execute("select * from jobs order by id desc limit 1")
            row = cur.fetchone()
            cols = [c[0] for c in cur.description]
        if not row:
            return None
        j = dict(zip(cols, row))
        # A live run touches its row at least every few seconds (paused ones too); if it hasn't
        # for a while, its process is gone.
        if j["state"] in ("running", "paused", "stopping") and time.time() - (j["updated"] or 0) > 90:
            j["state"] = "interrupted"
        return j

    def job_update(self, job_id: int, **fields):
        cols = "".join(f"{k} = ?, " for k in fields)
        with self.lock:
            self.db.execute(f"update jobs set {cols}updated = ? where id = ?", (*fields.values(), time.time(), job_id))
            self.db.commit()

    def job_state(self, job_id: int) -> str:
        with self.lock:
            return self.db.execute("select state from jobs where id = ?", (job_id,)).fetchone()[0]

    def job_control(self, state: str) -> dict | None:
        j = self.job()
        if j and j["state"] in ("running", "paused"):
            self.job_update(j["id"], state=state)
        return self.job()

    def is_live(self) -> bool:
        j = self.job()
        return bool(j and j["state"] in ("running", "paused", "stopping"))

    # CLIP prompts and probes

    def text_vectors(self, texts: list[str]) -> np.ndarray:
        missing = [t for t in texts if t not in self.text_cache]
        if missing:
            out = subprocess.run([str(self.cli), "clip-text"], input=json.dumps(missing).encode(),
                                 capture_output=True, check=True)
            for t, v in zip(missing, json.loads(out.stdout)):
                self.text_cache[t] = np.asarray(v, dtype=np.float32)
        return np.stack([self.text_cache[t] for t in texts])

    def prompt_scores(self, positive: list[str], negative: list[str]) -> list[float]:
        pos = self.emb @ self.text_vectors(positive).T
        score = pos.mean(axis=1)
        if negative:
            score = score - (self.emb @ self.text_vectors(negative).T).mean(axis=1)
        return [round(float(x), 5) for x in score]

    def truth(self, name: str) -> tuple[np.ndarray, np.ndarray]:
        """(rows, 0/1 labels) for a ground truth:
        - "human": your ratings (Good/Great positive)
        - "aim": every kept shot (the view nearest your aim positive, the rest of the moment negative)
        - "aim_yours": the same, only shots you aimed yourself (marked by hand or re-aimed), since a
          suggestion kept as-is grades the app against itself
        - "taste": aim_yours plus your ratings (a rating wins where both say something)
        - any pred source (>= 0.5, as a teacher)"""
        def aim_rows(yours: bool) -> dict[int, int]:
            return {i: int(v["aim_err"] <= 22.5) for i, v in enumerate(self.views)
                    if "aim_err" in v and (not yours or v.get("shot_source") in ("manual", "reaimed"))}

        def rated() -> dict[int, int]:
            lab = self.labels()
            return {self.index[k]: int(v["rating"] >= 2) for k, v in lab.items()
                    if v["rating"] is not None and k in self.index}

        if name in ("human", "aim", "aim_yours", "taste"):
            d = {"human": rated, "aim": lambda: aim_rows(False), "aim_yours": lambda: aim_rows(True),
                 "taste": lambda: {**aim_rows(True), **rated()}}[name]()
            rows, y = list(d), list(d.values())
        else:
            p = self.preds().get(name, {})
            rows = [self.index[k] for k in p if k in self.index]
            y = [1 if p[self.views[r]["id"]] >= 0.5 else 0 for r in rows]
        return np.asarray(rows, dtype=int), np.asarray(y, dtype=np.float32)

    def train_probe(self, truth: str, l2: float = 1e-3) -> dict:
        """Logistic regression on the 512-d view embeddings, with cross-validation by clip (no
        leaking a clip's own moments into its test fold). Returns CV AUC and scores for every view."""
        rows, y = self.truth(truth)
        if len(rows) < 20 or y.min() == y.max():
            raise ValueError(f"need at least 20 examples of both kinds for '{truth}' (have {len(rows)})")
        X = self.emb[rows]
        clips = np.asarray([self.views[r]["clip"] for r in rows])
        names = sorted(set(clips))
        rng = random.Random(0)
        rng.shuffle(names)
        folds = {c: i % 5 for i, c in enumerate(names)}
        cv = np.full(len(rows), np.nan)
        for k in range(min(5, len(names))):
            test = np.asarray([folds[c] == k for c in clips])
            if test.all() or (~test).all() or y[~test].min() == y[~test].max():
                continue
            w, b = fit_logistic(X[~test], y[~test], l2)
            cv[test] = X[test] @ w + b
        ok = ~np.isnan(cv)
        w, b = fit_logistic(X, y, l2)
        scores = self.emb @ w + b
        # Honest values for evaluation: every view of a training clip gets the score from the fold
        # model that never saw that clip; other views get the full model's score.
        held = {}
        for k in range(min(5, len(names))):
            test_clips = {c for c in names if folds[c] == k}
            train = np.asarray([folds[c] != k for c in clips])
            if train.all() or (~train).all() or y[train].min() == y[train].max():
                continue
            wk, bk = fit_logistic(X[train], y[train], l2)
            for i, v in enumerate(self.views):
                if v["clip"] in test_clips:
                    held[i] = float(self.emb[i] @ wk + bk)
        for i, x in held.items():
            scores[i] = x
        name = f"probe:{truth}"
        self.probes[name] = {"weights": w.tolist(), "bias": float(b), "truth": truth}
        (self.data / "probes").mkdir(exist_ok=True)
        (self.data / "probes" / f"{name.replace(':', '_')}.json").write_text(json.dumps(
            {"truth": truth, "dimension": len(w), "bias": float(b), "weights": [float(x) for x in w],
             "examples": int(len(rows)), "positives": int(y.sum()), "cv_auc": auc(cv[ok], y[ok])}))
        self.probes[name]["held_out"] = held
        self.probes[name]["cv_auc"] = auc(cv[ok], y[ok])
        return {"name": name, "n": int(len(rows)), "positives": int(y.sum()), "cv_auc": auc(cv[ok], y[ok]),
                "values": [round(float(x), 4) for x in scores]}


def fit_logistic(X: np.ndarray, y: np.ndarray, l2: float, steps: int = 400, lr: float = 0.5):
    """Plain full-batch gradient descent, class-balanced (positives are rare)."""
    n, d = X.shape
    mu = X.mean(axis=0)
    Xc = X - mu
    pw = (len(y) - y.sum()) / max(y.sum(), 1)
    sw = np.where(y > 0, pw, 1.0)
    sw = sw / sw.mean()
    w = np.zeros(d, dtype=np.float32)
    b = 0.0
    scale = 1 / max(np.sqrt((Xc**2).sum(axis=1).mean()), 1e-6)
    Xs = Xc * scale
    for _ in range(steps):
        p = 1 / (1 + np.exp(-(Xs @ w + b)))
        g = sw * (p - y)
        w -= lr * (Xs.T @ g / n + l2 * w)
        b -= lr * g.mean()
    w_out = w * scale
    return w_out, float(b - mu @ w_out)


def auc(scores: np.ndarray, y: np.ndarray) -> float | None:
    if len(y) == 0 or y.min() == y.max():
        return None
    order = np.argsort(scores, kind="mergesort")
    ranks = np.empty(len(scores))
    ranks[order] = np.arange(1, len(scores) + 1)
    pos = y > 0
    return float((ranks[pos].sum() - pos.sum() * (pos.sum() + 1) / 2) / (pos.sum() * (~pos).sum()))


# MARK: Model runs


def model_tag(model: str) -> str:
    return model.split(":")[0]


def ollama(path: str, body: dict, timeout: float = 600) -> dict:
    req = urllib.request.Request(OLLAMA + path, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"{e.code}: {e.read().decode()[:300]}") from None


def capabilities(model: str) -> list[str]:
    return ollama("/api/show", {"model": model}).get("capabilities", [])


def score_view(lab: Lab, model: str, kind: str, view: dict) -> dict[str, tuple[float, object]]:
    img = base64.b64encode((lab.data / view["file"]).read_bytes()).decode()
    tag = model_tag(model)
    if kind == "decision":
        r = ollama("/v1/systemone", {"model": model, "state": STATE, "images": [img], "questions": QUESTIONS})
        a = r["answers"]
        subj = a["subject"]
        probs = subj["probabilities"]
        return {
            f"{tag}.aim": (a["aim"]["noul"], {"v": QUESTIONS_VERSION}),
            f"{tag}.quality": (a["quality"]["score"] / 5, {"probabilities": a["quality"]["probabilities"]}),
            f"{tag}.notable": (a["notable"]["noul"], None),
            f"{tag}.junk": (probs.get("interior", 0) + probs.get("ground", 0),
                            {"subject": subj["choice"], "probabilities": probs}),
        }
    r = ollama("/api/chat", {"model": model, "stream": False, "think": False, "format": CHAT_SCHEMA,
                             "options": {"temperature": 0},
                             "messages": [{"role": "user", "content": CHAT_PROMPT, "images": [img]}]})
    out = json.loads(r["message"]["content"])
    return {f"{tag}.rating": (max(0, min(5, int(out.get("rating", 0)))) / 5,
                              {"tags": out.get("tags", []), "caption": out.get("caption", "")})}


def run_order(lab: Lab) -> list[int]:
    """Whole moments at a time (aim metrics need all eight views): your shots' moments first,
    then the rest in a fixed shuffled order."""
    moments: dict[tuple, list[int]] = {}
    for i, v in enumerate(lab.views):
        moments.setdefault((v["clip"], v["time"]), []).append(i)
    keys = list(moments)
    random.Random(7).shuffle(keys)
    keys.sort(key=lambda k: "aim_err" not in lab.views[moments[k][0]])
    return [i for k in keys for i in moments[k]]


def run_model(lab: Lab, model: str, limit: int | None = None, force: bool = False, log=print):
    if lab.is_live():
        raise RuntimeError("a run is already going: pause or stop it first")
    caps = capabilities(model)
    kind = "decision" if "decision" in caps else "chat"
    if "vision" not in caps:
        raise RuntimeError(f"{model} has no vision capability ({caps})")
    tag = model_tag(model)
    probe_source = f"{tag}.aim" if kind == "decision" else f"{tag}.rating"
    done = set() if force else set(lab.preds().get(probe_source, {}))
    todo = [i for i in run_order(lab) if lab.views[i]["id"] not in done]
    if limit:
        todo = todo[:limit]
    now = time.time()
    with lab.lock:
        job_id = lab.db.execute(
            "insert into jobs (model, kind, total, state, started, updated, pid) values (?, ?, ?, 'running', ?, ?, ?)",
            (model, kind, len(todo), now, now, os.getpid())).lastrowid
        lab.db.commit()
    count = errors = 0
    busy = 0.0  # seconds spent scoring (not paused), for the rate
    for i in todo:
        state = lab.job_state(job_id)
        while state == "paused":
            time.sleep(1)
            lab.job_update(job_id)  # heartbeat
            state = lab.job_state(job_id)
        if state == "stopping":
            break
        v = lab.views[i]
        t0 = time.time()
        try:
            lab.put_preds(v["id"], score_view(lab, model, kind, v))
        except Exception as e:  # keep going; report the last error
            errors += 1
            lab.job_update(job_id, last_error=str(e)[:300])
            log(f"  {v['id']}: {e}")
        busy += time.time() - t0
        count += 1
        lab.job_update(job_id, done=count, errors=errors, seconds=busy)
        if count % 50 == 0:
            log(f"  {count}/{len(todo)}  {busy / count:.2f}s/view")
    final = "stopped" if lab.job_state(job_id) == "stopping" else "done"
    lab.job_update(job_id, state=final, finished=time.time())
    return lab.job()


# MARK: Server


def make_handler(lab: Lab):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, fmt, *args):
            pass

        def send(self, code: int, body: bytes, ctype: str = "application/json", cache: bool = False):
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "max-age=86400" if cache else "no-store")
            self.end_headers()
            self.wfile.write(body)

        def json(self, obj, code: int = 200):
            self.send(code, json.dumps(obj).encode())

        def body(self) -> dict:
            n = int(self.headers.get("Content-Length") or 0)
            return json.loads(self.rfile.read(n) or b"{}")

        def do_GET(self):
            u = urlparse(self.path)
            if u.path == "/":
                return self.send(200, (HERE / "index.html").read_bytes(), "text/html; charset=utf-8")
            if u.path.startswith("/views/"):
                p = (lab.data / u.path.lstrip("/")).resolve()
                if lab.data.resolve() not in p.parents or not p.is_file():
                    return self.send(404, b"not found", "text/plain")
                return self.send(200, p.read_bytes(), "image/jpeg", cache=True)
            if u.path == "/api/data":
                preds = lab.preds()
                for name, p in lab.probes.items():
                    preds[name] = {v["id"]: p["held_out"].get(i, float(np.dot(lab.emb[i], p["weights"]) + p["bias"]))
                                   for i, v in enumerate(lab.views)}
                return self.json({
                    "clips": lab.manifest["clips"], "lut": lab.manifest.get("lut"), "ratings": RATINGS,
                    "views": lab.views, "labels": lab.labels(), "preds": preds,
                })
            if u.path == "/api/detail":
                vid = parse_qs(u.query).get("id", [""])[0]
                return self.json(lab.detail(vid))
            if u.path == "/api/status":
                q = parse_qs(u.query)
                since = float(q.get("since", ["0"])[0])
                out = {"job": lab.job(), "now": time.time()}
                if since:
                    out["preds"] = lab.preds_since(since)
                return self.json(out)
            if u.path == "/api/models":
                try:
                    tags = ollama_get("/api/tags")["models"]
                    out = []
                    for m in tags:
                        caps = capabilities(m["name"])
                        if "vision" in caps:
                            out.append({"name": m["name"], "kind": "decision" if "decision" in caps else "chat"})
                    return self.json(out)
                except Exception as e:
                    return self.json({"error": str(e)}, 502)
            self.send(404, b"not found", "text/plain")

        def do_POST(self):
            u = urlparse(self.path)
            try:
                b = self.body()
                if u.path == "/api/label":
                    lab.set_label(b["id"], b.get("rating"), bool(b.get("notable")))
                    return self.json({"ok": True})
                if u.path == "/api/prompt":
                    pos = [t.strip() for t in b.get("positive", []) if t.strip()]
                    neg = [t.strip() for t in b.get("negative", []) if t.strip()]
                    if not pos:
                        return self.json({"error": "add at least one prompt"}, 400)
                    return self.json({"values": lab.prompt_scores(pos, neg)})
                if u.path == "/api/probe":
                    return self.json(lab.train_probe(b.get("truth", "human")))
                if u.path == "/api/run":
                    if lab.is_live():
                        return self.json({"error": "a run is already going: pause or stop it first"}, 409)
                    model, limit = b["model"], b.get("limit") or None

                    def go():
                        try:
                            run_model(lab, model, limit)
                        except Exception as e:
                            print(f"run failed: {e}", file=sys.stderr)
                    threading.Thread(target=go, daemon=True).start()
                    time.sleep(0.5)  # let it create its row
                    return self.json({"job": lab.job()})
                if u.path in ("/api/pause", "/api/resume", "/api/stop"):
                    state = {"/api/pause": "paused", "/api/resume": "running", "/api/stop": "stopping"}[u.path]
                    return self.json({"job": lab.job_control(state)})
            except Exception as e:
                return self.json({"error": str(e)}, 400)
            self.send(404, b"not found", "text/plain")

    return Handler


def ollama_get(path: str) -> dict:
    with urllib.request.urlopen(OLLAMA + path, timeout=10) as r:
        return json.load(r)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--data", type=Path, default=DEFAULT_DATA)
    ap.add_argument("--cli", type=Path, default=DEFAULT_CLI, help="reframe-cli (for export and CLIP text)")
    sub = ap.add_subparsers(dest="cmd", required=True)
    e = sub.add_parser("export", help="export views from Reframe (reframe-cli lab-export)")
    e.add_argument("source", nargs="?", default="cloud", help="footage folder or 'cloud'")
    e.add_argument("--every", default="8")
    e.add_argument("--shot-every", default="2")
    s = sub.add_parser("serve")
    s.add_argument("--port", type=int, default=8765)
    r = sub.add_parser("run", help="score views with an Ollama vision model")
    r.add_argument("model")
    r.add_argument("--limit", type=int)
    r.add_argument("--force", action="store_true", help="re-score views already scored")
    args = ap.parse_args()

    if args.cmd == "export":
        sys.exit(subprocess.call([str(args.cli), "lab-export", args.source, "--out", str(args.data),
                                  "--every", args.every, "--shot-every", args.shot_every]))
    if not (args.data / "manifest.json").exists():
        sys.exit(f"no export in {args.data}: run `uv run lab.py export` first")
    lab = Lab(args.data, args.cli)
    if args.cmd == "run":
        print(f"{args.model}: {len(lab.views)} views")
        j = run_model(lab, args.model, args.limit, args.force)
        print(f"{j['state']}: {j['done']} views, {j['errors']} errors, {j['seconds'] / max(j['done'], 1):.2f}s/view")
    elif args.cmd == "serve":
        server = ThreadingHTTPServer(("127.0.0.1", args.port), make_handler(lab))
        print(f"Reframe lab: http://127.0.0.1:{args.port}  ({len(lab.views)} views, data {args.data})")
        server.serve_forever()


if __name__ == "__main__":
    main()
