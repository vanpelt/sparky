"""Measure residual camera motion in rendered views to compare stabilization settings.

    uv run stabcheck.py <file.insv> <start> [--duration 3] [--yaw 0 --pitch -5 --hfov 90]

Renders small world-locked previews per variant and reports the mean frame-to-frame optical
flow (pixels at 640px width). Lower = steadier. Translation parallax is identical across
variants, so differences come from rotation handling.
"""

from __future__ import annotations

import argparse
from pathlib import Path

import cv2
import numpy as np

from render import render_clip

W, H = 640, 360


def jitter_score(frames: list[np.ndarray]) -> float:
    """High-frequency part of the global image motion: smooth pans and forward travel cancel out,
    shake and wobble remain. RMS px/frame at 640 wide."""
    vecs = []
    prev = None
    for img in frames:
        g = cv2.equalizeHist(cv2.cvtColor((np.clip(img, 0, 1) * 255).astype(np.uint8), cv2.COLOR_RGB2GRAY))
        if prev is not None:
            f = cv2.calcOpticalFlowFarneback(prev, g, None, 0.5, 3, 21, 3, 5, 1.1, 0)
            h = f.shape[0]
            vecs.append(np.median(f[: h // 2].reshape(-1, 2), axis=0))  # upper half: far scenery, horizon
        prev = g
    v = np.array(vecs)
    k = np.ones(9) / 9
    trend = np.stack([np.convolve(v[:, i], k, mode="same") for i in range(2)], 1)
    return float(np.sqrt(((v - trend)[4:-4] ** 2).sum(1).mean()))


def flow_score(frames: list[np.ndarray]) -> float:
    mags = []
    prev = None
    for img in frames:
        g = cv2.cvtColor((np.clip(img, 0, 1) * 255).astype(np.uint8), cv2.COLOR_RGB2GRAY)
        g = cv2.equalizeHist(g)  # log footage is flat; boost texture for the flow solver
        if prev is not None:
            f = cv2.calcOpticalFlowFarneback(prev, g, None, 0.5, 3, 21, 3, 5, 1.1, 0)
            mags.append(np.median(np.linalg.norm(f, axis=-1)))
        prev = g
    return float(np.mean(mags))


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("file", type=Path)
    ap.add_argument("start", type=float)
    ap.add_argument("--duration", type=float, default=3.0)
    ap.add_argument("--yaw", type=float, default=0.0)
    ap.add_argument("--pitch", type=float, default=-5.0)
    ap.add_argument("--hfov", type=float, default=90.0)
    ap.add_argument("--quick", action="store_true", help="only none / lock / lock+rs")
    ap.add_argument("--stab-study", action="store_true", help="tilt filter and rolling-shutter variants")
    a = ap.parse_args()

    variants = [
        ("none (camera-fixed)", dict(stab="none", rs=False)),
        ("lock, no RS", dict(stab="lock", rs=False)),
        ("lock, RS +", dict(stab="lock", rs=True, rs_sign=1.0)),
    ]
    if not a.quick:
        variants += [
            ("lock, RS -", dict(stab="lock", rs=True, rs_sign=-1.0)),
            ("lock, RS +, gyro -5ms", dict(stab="lock", rs=True, time_offset=-0.005)),
            ("lock, RS +, gyro +5ms", dict(stab="lock", rs=True, time_offset=0.005)),
        ]
    if a.stab_study:
        variants = [
            ("none (camera-fixed)", dict(stab="none", rs=False)),
            ("follow, mahony, RS linear", dict(stab="follow", rs=True)),
            ("follow, offline tilt, RS linear", dict(stab="follow", rs=True, tilt="offline")),
            ("follow, offline tilt, RS exact", dict(stab="follow", rs=True, tilt="offline", rs_exact=True)),
            ("follow, mahony, no RS", dict(stab="follow", rs=False)),
        ]
    for name, kw in variants:
        frames: list[np.ndarray] = []
        render_clip(a.file, a.start, a.duration, None, width=W, height=H, yaw=a.yaw, pitch=a.pitch,
                    hfov=a.hfov, decode_size=1280, audio=False,
                    on_frame=lambda k, img: frames.append(img), **kw)
        print(f"{name:34s} mean flow {flow_score(frames):6.2f}   jitter {jitter_score(frames):6.2f} px/frame", flush=True)


if __name__ == "__main__":
    main()
