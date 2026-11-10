"""Transcribe the proxy (LRV) audio of Reframe's cloud recordings with Whisper, word by word.

Only recordings where Reframe's sound classifier heard speech are transcribed (run
`reframe-cli analyze` first), plus any named on the command line. Results are cached in the
output file; already-transcribed recordings are skipped.

usage: uv run transcribe.py <out.json> [VID_NNN ...] [--model mlx-community/whisper-large-v3-turbo]

Writes {"VID_NNN": {"file": lrv, "segments": [{start, end, text, no_speech, logprob,
words: [{start, end, word, p}]}]}}, times in seconds into the recording.
"""
import glob, json, os, re, sys

import mlx_whisper

CLOUD = os.path.expanduser("~/Library/Application Support/Reframe/Cloud")
ANALYSIS = os.path.expanduser("~/Library/Application Support/Reframe/analysis")

args = sys.argv[1:]
model = "mlx-community/whisper-large-v3-turbo"
if "--model" in args:
    i = args.index("--model")
    model = args[i + 1]
    del args[i:i + 2]
if not args:
    sys.exit(__doc__)
out_path, forced = args[0], set(args[1:])
done = json.load(open(out_path)) if os.path.exists(out_path) else {}

# Seconds of speech per recording number, from Reframe's analysis cache.
speechy = {}
for f in glob.glob(os.path.join(ANALYSIS, "*.json")):
    a = json.load(open(f))
    sp = a["signals"]["channels"].get("speech", [])
    secs = sum(1 for x in sp if x > 0.3) / a["signals"]["rate"]
    m = re.search(r"_(\d{3})-", os.path.basename(f))
    if m and secs >= 0.5:
        speechy[m.group(1)] = secs

for d in sorted(glob.glob(os.path.join(CLOUD, "*", "cloud.json"))):
    rec = json.load(open(d))
    m = re.search(r"_(\d{3})\.insv$", rec["name"])
    if not m:
        continue
    num, key = m.group(1), f"VID_{m.group(1)}"
    if key in done or (num not in speechy and key not in forced):
        continue
    lrv = os.path.join(os.path.dirname(d), rec.get("lrvName") or "")
    if not os.path.isfile(lrv):
        print(f"{key}: no proxy downloaded", file=sys.stderr)
        continue
    print(f"{key}  ({speechy.get(num, 0):.0f}s of speech)", flush=True)
    try:
        r = mlx_whisper.transcribe(
            lrv, path_or_hf_repo=model, word_timestamps=True, language="en",
            condition_on_previous_text=False, no_speech_threshold=0.5,
            hallucination_silence_threshold=2.0,
        )
    except Exception as e:  # e.g. a proxy without an audio track
        print(f"{key}: {e}", file=sys.stderr)
        continue
    segs = []
    for s in r["segments"]:
        words = [{"start": round(w["start"], 2), "end": round(w["end"], 2), "word": w["word"],
                  "p": round(w.get("probability", 0), 2)} for w in s.get("words", [])]
        segs.append({"start": round(s["start"], 2), "end": round(s["end"], 2), "text": s["text"].strip(),
                     "no_speech": round(s.get("no_speech_prob", 0), 2),
                     "logprob": round(s.get("avg_logprob", 0), 2), "words": words})
    done[key] = {"file": lrv, "segments": segs}
    json.dump(done, open(out_path, "w"), indent=1)

print("transcribed", len(done), "recordings")
