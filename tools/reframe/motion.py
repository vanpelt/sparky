"""Camera orientation from the X6 IMU, and smoothed virtual-camera paths.

Orientation is body -> world (body: +X forward/lens-0 side, +Y left, +Z up; world: Z up).
It comes from integrating the gyro, with a gentle accelerometer pull toward gravity
(Mahony-style) so roll and pitch can't drift. Heading drifts slowly, which follow mode absorbs.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import numpy as np
from scipy.ndimage import gaussian_filter1d
from scipy.spatial.transform import Rotation, Slerp

import insv


def imu_to_body(v: np.ndarray) -> np.ndarray:
    """X5 raw IMU axes -> body frame, per insta360-rs; verified on X6 by gravity leveling."""
    return np.stack([v[..., 1], v[..., 2], v[..., 0]], -1)


def video_time(cam_t: np.ndarray, info: insv.InsvInfo, apply_adjust: bool = False) -> np.ndarray:
    """Camera-clock seconds -> seconds on the video timeline (field 24 first-frame timestamp).

    Field 28 (gyro adjustment, ~1.1 ms on X6) is skipped by default: a time-offset sweep with
    stabcheck.py on X6 footage was steadiest without it (0.58 vs 1.05 px/frame residual).
    """
    base = (info.first_frame_ts or 0) / (1e6 if info.raw_imu else 1e3)
    adjust = (info.gyro_adjust_ms or 0.0) / 1000.0 if apply_adjust else 0.0
    return cam_t - base - adjust


def _level(up_body: np.ndarray) -> Rotation:
    up = up_body / np.linalg.norm(up_body)
    rot, _ = Rotation.align_vectors([[0, 0, 1]], [up])
    return rot


@dataclass
class Motion:
    t: np.ndarray  # video-timeline seconds
    rot: Rotation  # body -> world, one per sample
    omega: np.ndarray  # body angular velocity, rad/s
    _slerp: Slerp

    @classmethod
    def from_insv(cls, path: Path, kp: float = 0.6, time_offset: float = 0.0) -> Motion:
        info, recs, _ = insv.read_info(path)
        g = insv.load_gyro(path, info, recs)
        t = video_time(g[:, 0], info) + time_offset
        acc = imu_to_body(g[:, 1:4])
        gyr = imu_to_body(g[:, 4:7])

        # Start level: average the first half-second of accelerometer.
        q = _level(acc[t < t[0] + 0.5].mean(0)).as_quat()  # x, y, z, w
        quats = np.empty((len(t), 4))
        quats[0] = q
        dts = np.diff(t, prepend=t[0])
        for k in range(1, len(t)):
            w = gyr[k]
            a = acc[k]
            n = np.linalg.norm(a)
            if kp and 0.85 < n < 1.15:  # trust gravity only when not accelerating hard
                x, y, z, s = q
                # world up expressed in body coords: R^T @ [0,0,1]
                up_est = np.array([2 * (x * z - s * y), 2 * (y * z + s * x), 1 - 2 * (x * x + y * y)])
                w = w + kp * np.cross(a / n, up_est)
            q = _quat_mul(q, _quat_exp(w * dts[k]))
            q /= np.linalg.norm(q)
            quats[k] = q
        rot = Rotation.from_quat(quats)
        return cls(t=t, rot=rot, omega=gyr, _slerp=Slerp(t, rot))

    def at(self, times: np.ndarray) -> Rotation:
        return self._slerp(np.clip(times, self.t[0], self.t[-1]))

    def omega_at(self, times: np.ndarray, window: float = 0.004) -> np.ndarray:
        """Mean body angular velocity around each time (rad/s)."""
        out = np.empty((len(times), 3))
        for i, tt in enumerate(times):
            sel = np.abs(self.t - tt) <= window
            out[i] = self.omega[sel].mean(0) if sel.any() else 0.0
        return out


def _quat_mul(a, b):
    ax, ay, az, aw = a
    bx, by, bz, bw = b
    return np.array([
        aw * bx + ax * bw + ay * bz - az * by,
        aw * by - ax * bz + ay * bw + az * bx,
        aw * bz + ax * by - ay * bx + az * bw,
        aw * bw - ax * bx - ay * by - az * bz,
    ])


def _quat_exp(v):
    angle = np.linalg.norm(v)
    if angle < 1e-12:
        return np.array([v[0] / 2, v[1] / 2, v[2] / 2, 1.0])
    s = np.sin(angle / 2) / angle
    return np.array([v[0] * s, v[1] * s, v[2] * s, np.cos(angle / 2)])


def view_rotations(motion: Motion, times: np.ndarray, mode: str, smooth_s: float = 0.8) -> list[np.ndarray]:
    """World-space orientation of the virtual camera base for each frame (before user yaw/pitch).

    none   - camera-fixed: the view moves with the camera body (no stabilization)
    lock   - world-locked: the view never rotates
    follow - horizon level, heading follows the camera's smoothed heading
    """
    body = motion.at(times)
    if mode == "none":
        # The calibration body frame has +Z pointing out the bottom of an upright X6 (gravity
        # leveling flips it), so a camera-fixed view needs a half-turn about X to read upright.
        return list(body.as_matrix() @ np.diag([1.0, -1.0, -1.0]))
    fwd = body.apply([1.0, 0.0, 0.0])
    yaw = np.unwrap(np.arctan2(fwd[:, 1], fwd[:, 0]))
    if mode == "lock":  # hold the heading the camera had at the first frame
        return [Rotation.from_euler("z", yaw[0]).as_matrix()] * len(times)
    if mode == "follow":
        fps = 1 / np.median(np.diff(times)) if len(times) > 1 else 1
        yaw = gaussian_filter1d(yaw, sigma=smooth_s * fps, mode="nearest")
        return list(Rotation.from_euler("z", yaw[:, None]).as_matrix())
    raise ValueError(mode)
