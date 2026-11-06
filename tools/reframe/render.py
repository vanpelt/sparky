"""Render frames or clips from an .insv using the camera's factory calibration and IMU.

    uv run render.py equirect <file.insv> <t> <out.png> [--width 3840]
    uv run render.py flat <file.insv> <t> <out.png> --yaw 0 --pitch 0 --hfov 90
    uv run render.py lenses <file.insv> <t> <out_prefix>          # raw fisheye frames
    uv run render.py clip <file.insv> <t> <out.mov> --duration 5 --stab follow --yaw 180 --hfov 100

Body frame: +X forward (lens-0 side), +Y left, +Z up. Yaw is positive to the left.
--stab: none (camera-fixed), follow (level horizon, smoothed heading), lock (world-locked).
Values stay in the camera's encoding (I-Log stays log). Stills are 16-bit PNG; clips are
ProRes 422 HQ 10-bit tagged Rec.709 (apply the I-Log LUT in the editor).
"""

from __future__ import annotations

import argparse
import math
import subprocess
from pathlib import Path

import cv2
import numpy as np

import insv
from motion import Motion, view_rotations

BLEND_BELT_DEG = 6.0
YUV_TO_RGB = "scale=in_color_matrix=bt709:in_range=tv"


class Rig:
    """Calibrated two-lens rig: maps body-frame rays to source pixels in each lens frame."""

    def __init__(self, path: Path):
        info, _, _ = insv.read_info(path)
        self.info = info
        self.lenses = insv.parse_calibration(info.calibration_current or info.calibration_original)
        self.rot = [insv.lens_rotation(lens, i) for i, lens in enumerate(self.lenses)]
        # Metadata field 80: 1 => video track 0 carries stream "10" (calibration lens 1).
        self.track_for_lens = [1, 0] if info.track_order == 1 else [0, 1]
        self.readout_s = (info.rolling_shutter_ms or 0.0) / 1000.0

    def _project(self, i: int, body: np.ndarray, frame_size: int):
        lens = self.lenses[i]
        local = body @ self.rot[i].T
        norm = np.linalg.norm(local, axis=-1)
        angle = np.degrees(np.arccos(np.clip(local[..., 2] / norm, -1, 1)))
        valid = angle < 100.0  # the omni model folds back beyond the front hemisphere
        cx, cy = insv.project(lens, np.where(valid[..., None], local, [0.0, 0.0, 1.0]))
        canvas_w = lens.width / 2.0
        scale = frame_size / canvas_w
        return ((cx - i * canvas_w) * scale).astype(np.float32), (cy * scale).astype(np.float32), angle, valid

    def maps(self, body: np.ndarray, frame_size: int, omega: np.ndarray | None = None, rs_sign: float = 1.0):
        """body rays (H,W,3) at mid-frame -> per-lens (map_x, map_y, weight).

        With omega (body rad/s), each source row is corrected for rolling shutter: a row read
        dt after mid-frame saw the scene through a camera rotated by omega*dt.
        """
        out, weights = [], []
        for i in range(2):
            mx, my, angle, valid = self._project(i, body, frame_size)
            if omega is not None and self.readout_s:
                for _ in range(2):  # fixed-point: row -> time -> rotated ray -> row
                    dt = (my / frame_size - 0.5) * self.readout_s * rs_sign
                    shifted = body - dt[..., None] * np.cross(omega, body)
                    mx, my, angle, valid = self._project(i, shifted, frame_size)
            inside = valid & (mx >= 0) & (my >= 0) & (mx <= frame_size - 1) & (my <= frame_size - 1)
            w = np.clip((90.0 - angle) / BLEND_BELT_DEG + 0.5, 0.0, 1.0) * inside
            out.append((mx, my))
            weights.append(w.astype(np.float32))
        total = weights[0] + weights[1]
        total[total == 0] = 1.0
        return [(mx, my, w / total) for (mx, my), w in zip(out, weights)]

    def render(self, frames: list[np.ndarray], body: np.ndarray, omega=None, rs_sign: float = 1.0) -> np.ndarray:
        """frames: per video track, uint16 RGB. Returns float32 RGB 0..1."""
        size = frames[0].shape[0]
        acc = np.zeros(body.shape[:2] + (3,), np.float32)
        for i, (mx, my, w) in enumerate(self.maps(body, size, omega, rs_sign)):
            src = frames[self.track_for_lens[i]]
            sampled = cv2.remap(src, mx, my, cv2.INTER_CUBIC, borderMode=cv2.BORDER_CONSTANT)
            acc += sampled.astype(np.float32) * (w[..., None] / 65535.0)
        return acc


# ----------------------------------------------------------------------------- rays


def equirect_rays(width: int) -> np.ndarray:
    height = width // 2
    lon = (np.arange(width) + 0.5) / width * 2 * np.pi - np.pi  # centre column = forward
    lat = np.pi / 2 - (np.arange(height) + 0.5) / height * np.pi
    lon, lat = np.meshgrid(lon, lat)
    return np.stack([np.cos(lat) * np.cos(lon), -np.cos(lat) * np.sin(lon), np.sin(lat)], -1)


def flat_rays(width: int, height: int, yaw: float, pitch: float, roll: float, hfov: float) -> np.ndarray:
    f = (width / 2) / math.tan(math.radians(hfov) / 2)
    u = (np.arange(width) + 0.5 - width / 2) / f
    v = (np.arange(height) + 0.5 - height / 2) / f
    u, v = np.meshgrid(u, v)
    rays = np.stack([np.ones_like(u), -u, -v], -1)  # camera: forward +X, right -Y, down -Z
    r = insv._rot("z", yaw) @ insv._rot("y", -pitch) @ insv._rot("x", roll)
    return rays @ r.T


# ----------------------------------------------------------------------------- video io


def lens_stream(path: Path, track: int, start: float, n: int, size: int | None):
    """Yield decoded frames (uint16 RGB, size x size) from one lens track."""
    vf = YUV_TO_RGB + (f",scale={size}:{size}:flags=lanczos" if size else "")
    cmd = ["ffmpeg", "-v", "error", "-hwaccel", "videotoolbox", "-ss", f"{start:.4f}", "-i", str(path),
           "-map", f"0:v:{track}", "-frames:v", str(n), "-vf", vf,
           "-f", "rawvideo", "-pix_fmt", "rgb48le", "-"]
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE)
    side = size
    try:
        if side is None:
            side = int(subprocess.run(
                ["ffprobe", "-v", "error", "-select_streams", f"v:{track}", "-show_entries", "stream=width",
                 "-of", "csv=p=0", str(path)], capture_output=True, text=True, check=True).stdout.strip())
        nbytes = side * side * 6
        while True:
            buf = proc.stdout.read(nbytes)
            if len(buf) < nbytes:
                break
            yield np.frombuffer(buf, dtype="<u2").reshape(side, side, 3)
    finally:
        proc.stdout.close()
        proc.wait()


def frame_rate(path: Path) -> float:
    r = subprocess.run(["ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
                        "stream=avg_frame_rate", "-of", "csv=p=0", str(path)],
                       capture_output=True, text=True, check=True).stdout.strip()
    num, den = r.split("/")
    return int(num) / int(den)


def prores_writer(out: Path, width: int, height: int, fps: float, audio_src: Path | None, start: float, dur: float):
    out.parent.mkdir(parents=True, exist_ok=True)
    cmd = ["ffmpeg", "-v", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgb48le", "-s", f"{width}x{height}",
           "-r", f"{fps}", "-i", "-"]
    if audio_src:
        cmd += ["-ss", f"{start:.4f}", "-t", f"{dur:.4f}", "-i", str(audio_src), "-map", "0:v", "-map", "1:a?",
                "-c:a", "pcm_s24le"]
    cmd += ["-vf", "scale=out_color_matrix=bt709:out_range=tv,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv", "-c:v", "prores_ks", "-profile:v", "3",
            "-pix_fmt", "yuv422p10le", "-vendor", "apl0", "-color_primaries", "bt709", "-color_trc", "bt709",
            "-colorspace", "bt709", "-color_range", "tv", str(out)]
    return subprocess.Popen(cmd, stdin=subprocess.PIPE)


def render_clip(path: Path, start: float, duration: float, out: Path, *, width=1920, height=1080,
                yaw=0.0, pitch=0.0, roll=0.0, hfov=90.0, stab="follow", smooth=0.8, rs=True,
                rs_sign=1.0, decode_size=None, time_offset=0.0, audio=True, on_frame=None):
    rig = Rig(path)
    fps = frame_rate(path)
    n = int(round(duration * fps))
    motion = Motion.from_insv(path, time_offset=time_offset)
    times = start + np.arange(n) / fps
    views = view_rotations(motion, times, stab, smooth)
    body_rot = motion.at(times).as_matrix()  # body -> world
    omegas = motion.omega_at(times) if rs else [None] * n
    cam = flat_rays(width, height, yaw, pitch, roll, hfov)

    streams = [lens_stream(path, k, start, n, decode_size) for k in (0, 1)]
    writer = prores_writer(out, width, height, fps, path if audio else None, start, duration) if out else None
    try:
        for k, frames in enumerate(zip(*streams)):
            world = cam @ views[k].T  # camera -> world
            body = world @ body_rot[k]  # world -> body (R^T, row-vector form)
            img = rig.render(list(frames), body, omegas[k], rs_sign)
            if on_frame:
                on_frame(k, img)
            if writer:
                writer.stdin.write((np.clip(img, 0, 1) * 65535).astype("<u2").tobytes())
    finally:
        if writer:
            writer.stdin.close()
            writer.wait()
    if out:
        print(f"wrote {out} {width}x{height} {n} frames @ {fps:g}fps stab={stab} rs={rs}")


# ----------------------------------------------------------------------------- stills


def decode_lens_frames(path: Path, t: float) -> list[np.ndarray]:
    return [next(lens_stream(path, k, t, 1, None)) for k in (0, 1)]


def save_png(img: np.ndarray, out: Path) -> None:
    out.parent.mkdir(parents=True, exist_ok=True)
    if img.dtype != np.uint16:
        img = (np.clip(img, 0, 1) * 65535).astype(np.uint16)
    cv2.imwrite(str(out), cv2.cvtColor(img, cv2.COLOR_RGB2BGR))
    print(f"wrote {out} {img.shape[1]}x{img.shape[0]}")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("mode", choices=["equirect", "flat", "lenses", "clip"])
    ap.add_argument("file", type=Path)
    ap.add_argument("t", type=float)
    ap.add_argument("out", type=Path)
    ap.add_argument("--width", type=int)
    ap.add_argument("--height", type=int, default=1080)
    ap.add_argument("--yaw", type=float, default=0.0)
    ap.add_argument("--pitch", type=float, default=0.0)
    ap.add_argument("--roll", type=float, default=0.0)
    ap.add_argument("--hfov", type=float, default=90.0)
    ap.add_argument("--duration", type=float, default=5.0)
    ap.add_argument("--stab", choices=["none", "follow", "lock"], default="follow")
    ap.add_argument("--smooth", type=float, default=0.8, help="follow-mode heading smoothing, seconds")
    ap.add_argument("--no-rs", action="store_true", help="skip rolling-shutter correction")
    ap.add_argument("--decode-size", type=int, help="downscale lens frames before remap (faster previews)")
    a = ap.parse_args()

    if a.mode == "clip":
        render_clip(a.file, a.t, a.duration, a.out, width=a.width or 1920, height=a.height, yaw=a.yaw,
                    pitch=a.pitch, roll=a.roll, hfov=a.hfov, stab=a.stab, smooth=a.smooth, rs=not a.no_rs,
                    decode_size=a.decode_size)
        return

    frames = decode_lens_frames(a.file, a.t)
    if a.mode == "lenses":
        for i, fr in enumerate(frames):
            save_png(fr, a.out.with_name(f"{a.out.stem}_track{i}.png"))
        return
    rig = Rig(a.file)
    rays = equirect_rays(a.width or 3840) if a.mode == "equirect" else \
        flat_rays(a.width or 1920, a.height, a.yaw, a.pitch, a.roll, a.hfov)
    motion = Motion.from_insv(a.file)
    body_rot = motion.at(np.array([a.t])).as_matrix()[0]
    save_png(rig.render(frames, rays @ body_rot), a.out)  # level via IMU orientation


if __name__ == "__main__":
    main()
