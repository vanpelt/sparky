"""Per-clip log of a Studio project: what is said in and around each clip (from transcribe.py)
and a rough interest curve over its trim (Reframe's cached analysis signals, weighted as Reframe
weights them, with the strongest direction of the heading channels).

usage: python3 cliplog.py <dump.json> <transcripts.json> [clip numbers...] [--words]
  dump.json: `reframe-cli studio dump <project> --json`
  --words prints each line's words with their times, for cutting between words.

Lines marked "in" fall inside the trim; "~" overlap it or sit within 2 s of it.
"""
import base64, glob, json, os, re, sys

ANALYSIS = os.path.expanduser("~/Library/Application Support/Reframe/analysis")
args = [a for a in sys.argv[1:] if a != "--words"]
show_words = "--words" in sys.argv
if len(args) < 2:
    sys.exit(__doc__)
dump = json.load(open(args[0]))
tx = json.load(open(args[1]))
only = {int(x) for x in args[2:]}

analyses = {}
for f in glob.glob(os.path.join(ANALYSIS, "*.json")):
    num = re.search(r"_(\d{3})-", os.path.basename(f)).group(1)
    analyses[num] = f

def num_of(name):
    return re.search(r"(\d{3})", name).group(1)

def interest(a):
    """Weighted sum of the time channels plus the strongest direction of the heading channels."""
    w = a["weights"]["values"]
    ch = a["signals"]["channels"]
    n = a["signals"]["count"]
    out = [0.0] * n
    for k, v in ch.items():
        if k in w:
            for i in range(min(n, len(v))):
                out[i] += w[k] * v[i]
    h = a.get("heading")
    if h:
        bins = len(base64.b64decode(h["channels"]["people"])) // h["count"]
        for k, b64 in h["channels"].items():
            if k not in w:
                continue
            raw = base64.b64decode(b64)
            for i in range(min(n, h["count"])):
                out[i] += w[k] * max(raw[i * bins:(i + 1) * bins]) / 255
    return out, a["signals"]["rate"]

bars = " ▁▂▃▄▅▆▇█"
def spark(v):
    if not v:
        return ""
    lo, hi = min(v), max(v)
    return "".join(bars[int((x - lo) / (hi - lo + 1e-9) * 8)] for x in v)

for c in dump["video"]:
    if only and c["index"] not in only:
        continue
    n = num_of(c["name"])
    si, so, rate = c["sourceIn"], c["sourceOut"], c.get("rate", 1)
    line = f"#{c['index']:<3} VID_{n} src {si:6.1f}–{so:6.1f} ({c['timelineOut']-c['timelineIn']:.1f}s tl{'' if rate == 1 else f', {rate}x'})"
    if n in analyses:
        a = json.load(open(analyses[n]))
        cur, r = interest(a)
        lo, hi = int(si * r), int(so * r) + 1
        seg = cur[lo:hi]
        if seg:
            best = max(range(len(seg)), key=lambda i: seg[i])
            line += f"  interest {spark(seg)}  peak +{best / r:.1f}s"
        sp = a["signals"]["channels"].get("speech", [])[lo:hi]
        if sp and max(sp) > 0.5:
            line += f"  speech {sum(1 for x in sp if x > 0.5) / r:.1f}s"
    print(line)
    t = tx.get(f"VID_{n}")
    if t:
        for s in t["segments"]:
            if s["end"] > si - 2 and s["start"] < so + 2 and s["no_speech"] < 0.7:
                where = "in " if s["start"] >= si - 0.3 and s["end"] <= so + 0.3 else "~  "
                print(f"     {where}{s['start']:6.1f}–{s['end']:6.1f} (+{s['start'] - si:5.1f})  {s['text']}")
                if show_words:
                    print("          " + " ".join(f"{w['word'].strip()}[{w['start']:.1f}-{w['end']:.1f}]" for w in s["words"]))
