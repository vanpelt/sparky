"""Read Insta360 .insv metadata: trailer records, protobuf metadata, lens calibration, gyro.

Format reference: insta360-rs docs/INSV_FORMAT.md (Apache-2.0). This is an independent,
read-only implementation covering ExtraInfo V3 (X4/X5/X6 era files).

    uv run insv.py info <file.insv>          # JSON summary
    uv run insv.py records <file.insv>       # list trailer records
"""

from __future__ import annotations

import json
import math
import struct
import sys
from dataclasses import asdict, dataclass, field
from pathlib import Path

import numpy as np

SIGNATURE = b"8db42d694ccc418790edff439fe026bf"

RECORD_NAMES = {
    0x01: "metadata", 0x02: "thumbnail", 0x03: "gyro", 0x04: "exposure",
    0x05: "thumbnail_ext", 0x06: "frame_pts", 0x07: "gps", 0x08: "gps_reception",
    0x09: "aaa", 0x0A: "anchors", 0x0B: "aaa_sim", 0x0C: "exposure_2",
    0x0D: "magnetic", 0x0E: "euler", 0x0F: "gyro_2", 0x10: "speed", 0x11: "tbox",
    0x12: "editor", 0x13: "heart_rate", 0x14: "forward_direction", 0x15: "up_view",
    0x16: "shell_recognition", 0x17: "position", 0x18: "timelapse_quat",
    0x19: "quaternion", 0x1A: "apei", 0x1B: "dynamic_isp", 0x1C: "static_isp",
    0x1D: "ae_flicker", 0x1E: "vehicle_info", 0x80: "time_map",
}

GROUP_TYPES = {
    0: "normal_video", 1: "bullet_time", 2: "timelapse", 6: "hdr_video",
    8: "static_timelapse", 9: "timeshift", 11: "super_normal_video", 12: "loop",
    16: "movie_video", 17: "slow_motion", 20: "pure_video", 21: "starlapse_video",
}

# ----------------------------------------------------------------------------- trailer


@dataclass
class Record:
    id: int
    format: int
    offset: int  # absolute file offset of payload
    length: int

    @property
    def name(self) -> str:
        return RECORD_NAMES.get(self.id, f"0x{self.id:02x}")


def read_records(path: Path) -> list[Record]:
    with open(path, "rb") as f:
        f.seek(0, 2)
        size = f.tell()
        f.seek(size - 72)
        term = f.read(72)
        if term[40:] != SIGNATURE:
            raise ValueError("no Insta360 trailer signature")
        extra_size, version = struct.unpack("<II", term[32:40])
        if version != 3:
            raise ValueError(f"unsupported ExtraInfo version {version}")
        payload_start = size - extra_size
        terminal_start = size - 72
        # The spec puts the inst header exactly 8 bytes before payload_start; X6 files
        # carry a few extra zero bytes between them, so search back a little.
        f.seek(payload_start - 32)
        head = f.read(32)
        at = head.rfind(b"inst")
        if at < 4:
            raise ValueError("trailer does not sit in a terminal inst box")
        box_start = payload_start - 32 + at - 4
        if struct.unpack(">I", head[at - 4 : at])[0] != size - box_start:
            raise ValueError("inst box does not end at EOF")

        f.seek(terminal_start - 6)
        fmt, rid, length = struct.unpack("<BBI", f.read(6))
        records: list[Record] = []
        if fmt == 0 and rid == 0:  # indexed directory
            f.seek(terminal_start - 6 - length)
            directory = f.read(length)
            for i in range(0, length, 10):
                eid, efmt, elen, eoff = struct.unpack("<BBII", directory[i : i + 10])
                if eid == 0 and efmt == 0 and elen == 0 and eoff == 0:
                    continue
                records.append(Record(eid, efmt, payload_start + eoff, elen))
        else:  # sequential: walk footers backward
            cursor = terminal_start
            while cursor > payload_start:
                f.seek(cursor - 6)
                fmt, rid, length = struct.unpack("<BBI", f.read(6))
                start = cursor - 6 - length
                records.append(Record(rid, fmt, start, length))
                cursor = start
            records.reverse()
    return records


def read_payload(path: Path, rec: Record) -> bytes:
    with open(path, "rb") as f:
        f.seek(rec.offset)
        return f.read(rec.length)


# ----------------------------------------------------------------------------- protobuf


def _varint(buf: bytes, i: int) -> tuple[int, int]:
    shift = result = 0
    while True:
        b = buf[i]
        i += 1
        result |= (b & 0x7F) << shift
        if not b & 0x80:
            return result, i
        shift += 7


def decode_pb(buf: bytes) -> dict[int, list]:
    """Schema-less protobuf decode: field number -> list of raw values."""
    out: dict[int, list] = {}
    i = 0
    while i < len(buf):
        key, i = _varint(buf, i)
        num, wire = key >> 3, key & 7
        if wire == 0:
            val, i = _varint(buf, i)
        elif wire == 1:
            val = struct.unpack_from("<d", buf, i)[0]
            i += 8
        elif wire == 2:
            n, i = _varint(buf, i)
            val = buf[i : i + n]
            i += n
        elif wire == 5:
            val = struct.unpack_from("<f", buf, i)[0]
            i += 4
        else:
            raise ValueError(f"unsupported wire type {wire} at {i}")
        out.setdefault(num, []).append(val)
    return out


def _s(v: bytes) -> str:
    return v.decode("utf-8", "replace").strip("\x00 ")


def _int32(v: int) -> int:
    return v - (1 << 64) if v >= 1 << 63 else v


# ----------------------------------------------------------------------------- calibration

V6_FIELDS = ["xi", "fx", "fy", "cx", "cy", "ex", "ey", "ez", "tx", "ty", "tz",
             "k1", "k2", "k3", "k4", "k5", "p1", "p2", "p3", "p4", "s1", "s2", "s3", "s4",
             "width", "height", "lens_type"]
V3_FIELDS = ["xi", "fx", "fy", "cx", "cy", "ex", "ey", "ez", "tx", "ty", "tz",
             "k1", "k2", "k3", "p1", "p2", "width", "height", "lens_type"]


@dataclass
class Lens:
    xi: float
    fx: float
    fy: float
    cx: float
    cy: float
    ex: float
    ey: float
    ez: float
    distortion: list[float]  # V6: k1..k5, p1..p4, s1..s4 ; V3: k1..k3, p1, p2
    width: int  # calibration canvas (both lenses side by side)
    height: int
    lens_type: int
    generation: int


def parse_calibration(s: str) -> list[Lens]:
    toks = [t for t in s.strip("\x00 ").split("_")]
    if toks[0] != "2":
        raise ValueError(f"expected 2 lenses, got {toks[0]!r}")
    n = len(toks)
    fields, gen = {56: (V6_FIELDS, 6), 40: (V3_FIELDS, 3)}.get(n, (None, None))
    if fields is None:
        raise ValueError(f"unsupported calibration token count {n}")
    packed = int(float(toks[-1]))
    if packed >> 16 != gen:
        raise ValueError(f"packed generation {packed >> 16} != {gen}")
    per = len(fields)
    lenses = []
    for li in range(2):
        vals = dict(zip(fields, map(float, toks[1 + li * per : 1 + (li + 1) * per])))
        dist_keys = [k for k in fields if k[0] in "kps" and k[1:].isdigit()]
        lenses.append(Lens(
            xi=vals["xi"], fx=vals["fx"], fy=vals["fy"], cx=vals["cx"], cy=vals["cy"],
            ex=vals["ex"], ey=vals["ey"], ez=vals["ez"],
            distortion=[vals[k] for k in dist_keys],
            width=int(vals["width"]), height=int(vals["height"]),
            lens_type=int(vals["lens_type"]), generation=gen,
        ))
    return lenses


def _rot(axis: str, deg: float) -> np.ndarray:
    c, s = math.cos(math.radians(deg)), math.sin(math.radians(deg))
    return {
        "x": np.array([[1, 0, 0], [0, c, -s], [0, s, c]]),
        "y": np.array([[c, 0, s], [0, 1, 0], [-s, 0, c]]),
        "z": np.array([[c, -s, 0], [s, c, 0], [0, 0, 1]]),
    }[axis]


def lens_rotation(lens: Lens, index: int) -> np.ndarray:
    """Body -> lens rotation (lens optical axis = +Z). Body: +X forward, +Y left, +Z up."""
    r = _rot("z", lens.ez) @ _rot("y", lens.ey + 90.0) @ _rot("x", lens.ex)
    return _rot("y", 180.0) @ r if index == 1 else r


def project(lens: Lens, rays: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    """Lens-space rays (...,3) -> calibration-canvas pixel coords (unified omni + radtan)."""
    x, y, z = rays[..., 0], rays[..., 1], rays[..., 2]
    norm = np.sqrt(x * x + y * y + z * z)
    den = z + lens.xi * norm
    mx, my = x / den, y / den
    x2, y2, xy = mx * mx, my * my, mx * my
    r2 = x2 + y2
    d = lens.distortion
    if lens.generation == 6:
        k1, k2, k3, k4, k5, p1, p2, p3, p4, s1, s2, s3, s4 = d
        radial = 1 + r2 * (k1 + r2 * (k2 + r2 * (k3 + r2 * (k4 + r2 * k5))))
        r4 = r2 * r2
        dx = mx * radial + (p1 + r2 * p3) * (r2 + 2 * x2) + 2 * (p2 + r2 * p4) * xy + s1 * r2 + s3 * r4
        dy = my * radial + (p2 + r2 * p4) * (r2 + 2 * y2) + 2 * (p1 + r2 * p3) * xy + s2 * r2 + s4 * r4
    else:
        k1, k2, k3, p1, p2 = d
        rad = k1 * r2 + k2 * r2 * r2 + k3 * r2 ** 3
        dx = mx + mx * rad + 2 * p1 * xy + p2 * (r2 + 2 * x2)
        dy = my + my * rad + 2 * p2 * xy + p1 * (r2 + 2 * y2)
    return lens.cx + dx * lens.fx, lens.cy + dy * lens.fy


# ----------------------------------------------------------------------------- telemetry


def decode_gyro(buf: bytes, raw: bool, ranges: tuple[float, float] | None) -> np.ndarray:
    """-> (N, 7) array: t_seconds, ax, ay, az (g), gx, gy, gz (rad/s). Camera clock."""
    if raw:
        a = np.frombuffer(buf, dtype=np.dtype([("t", "<i8"), ("v", "<u2", 6)]))
        acc_range, gyro_range = ranges or (8.0, 2000.0)
        v = a["v"].astype(np.float64) - 32768.0
        acc = v[:, :3] * acc_range / 32768.0
        gyr = np.radians(v[:, 3:] * gyro_range / 32768.0)
        t = a["t"] / 1e6
    else:
        a = np.frombuffer(buf, dtype=np.dtype([("t", "<i8"), ("v", "<f8", 6)]))
        acc, gyr = a["v"][:, :3], a["v"][:, 3:]
        t = a["t"] / 1e3
    return np.column_stack([t, acc, gyr])


def decode_exposure(buf: bytes, raw: bool) -> np.ndarray:
    a = np.frombuffer(buf, dtype=np.dtype([("t", "<i8"), ("s", "<f8")]))
    return np.column_stack([a["t"] / (1e6 if raw else 1e3), a["s"]])


# ----------------------------------------------------------------------------- high level


@dataclass
class InsvInfo:
    path: str
    camera: str = ""
    color_profile: str = ""
    capture_time: str = ""
    serial: str = ""
    firmware: str = ""
    group_type: str = ""
    nominal_fps: float | None = None
    rational_fps: tuple[int, int] | None = None
    dims: tuple[int, int] | None = None
    export_dims: tuple[int, int] | None = None
    rolling_shutter_ms: float | None = None
    gyro_adjust_ms: float | None = None
    raw_imu: bool = False
    imu_ranges: tuple[float, float] | None = None
    optical_state: int | None = None
    guard_detected: int | None = None
    stream_layout: int | None = None
    track_order: int | None = None
    blend_angle: int | None = None
    first_frame_ts: int | None = None
    timelapse_interval_ms: int | None = None
    calibration_generation: int | None = None
    calibration_current: str = ""
    calibration_original: str = ""
    records: dict[str, int] = field(default_factory=dict)
    unknown_fields: dict[int, str] = field(default_factory=dict)


KNOWN = {1, 2, 3, 5, 7, 22, 17, 19, 20, 24, 25, 26, 27, 28, 29, 31, 44, 53, 54, 55, 56, 62, 64,
         65, 68, 78, 79, 80, 104, 111, 112, 128, 129, 130, 131, 132, 133, 135, 136, 145}


def _vec(v: bytes) -> tuple[int, int]:
    m = decode_pb(v)
    return (m.get(1, [0])[0], m.get(2, [0])[0])


def read_info(path: Path) -> tuple[InsvInfo, list[Record], dict[int, list]]:
    recs = read_records(path)
    info = InsvInfo(path=str(path))
    for r in recs:
        info.records[r.name] = info.records.get(r.name, 0) + r.length
    meta_rec = next((r for r in recs if r.id == 1), None)
    m: dict[int, list] = {}
    if meta_rec and meta_rec.format == 1:
        m = decode_pb(read_payload(path, meta_rec))
        g = lambda k, d=None: m.get(k, [d])[0]  # noqa: E731
        info.serial = _s(g(1, b""))
        info.camera = _s(g(2, b""))
        info.firmware = _s(g(3, b""))
        info.color_profile = _s(g(22, b""))  # e.g. "I_Log" (empty = standard)
        info.capture_time = str(g(7, ""))  # YYYYMMDDhhmmss as an integer
        info.nominal_fps = g(20)
        info.first_frame_ts = g(24)
        info.rolling_shutter_ms = g(25)
        info.gyro_adjust_ms = g(28)
        info.raw_imu = bool(g(62, 0))
        info.optical_state = g(68)
        info.stream_layout = g(131)
        info.track_order = g(80)
        info.blend_angle = g(128)
        info.guard_detected = g(104)
        info.timelapse_interval_ms = g(133)
        info.calibration_generation = g(136)
        if 26 in m:
            gt = decode_pb(m[26][0]).get(1, [0])[0]
            info.group_type = GROUP_TYPES.get(gt, f"unknown({gt})")
        if 78 in m:
            fr = decode_pb(m[78][0])
            info.rational_fps = (fr.get(1, [0])[0], fr.get(2, [1])[0])
        if 19 in m:
            info.dims = _vec(m[19][0])
        if 44 in m:
            info.export_dims = _vec(m[44][0])
        if 65 in m:
            rr = decode_pb(m[65][0])
            info.imu_ranges = (float(rr.get(1, [8])[0]), float(rr.get(2, [2000])[0]))
        for cur, orig in ((111, 112), (54, 56), (53, 55)):
            if cur in m or orig in m:
                info.calibration_current = _s(m.get(cur, [b""])[0])
                info.calibration_original = _s(m.get(orig, [b""])[0])
                break
        for k, vals in m.items():
            if k not in KNOWN:
                v = vals[0]
                info.unknown_fields[k] = (
                    v.hex()[:48] if isinstance(v, bytes) else str(_int32(v) if isinstance(v, int) else v)
                )
    return info, recs, m


def load_gyro(path: Path, info: InsvInfo, recs: list[Record]) -> np.ndarray | None:
    rec = next((r for r in recs if r.id == 3), None)
    if rec is None:
        return None
    data = decode_gyro(read_payload(path, rec), info.raw_imu, info.imu_ranges)
    return data


def main() -> None:
    if len(sys.argv) < 3:
        print(__doc__)
        sys.exit(1)
    cmd, path = sys.argv[1], Path(sys.argv[2])
    info, recs, _ = read_info(path)
    if cmd == "records":
        for r in recs:
            print(f"{r.name:18s} id=0x{r.id:02x} fmt={r.format} len={r.length:>10,d}")
    elif cmd == "info":
        d = asdict(info)
        cal = info.calibration_current or info.calibration_original
        if cal:
            d["lenses"] = [asdict(lens) for lens in parse_calibration(cal)]
        gyro = load_gyro(path, info, recs)
        if gyro is not None and len(gyro):
            dt = np.diff(gyro[:, 0])
            d["gyro_summary"] = {
                "samples": int(len(gyro)),
                "rate_hz": round(float(1 / np.median(dt)), 1),
                "duration_s": round(float(gyro[-1, 0] - gyro[0, 0]), 2),
                "mean_accel_g": np.round(gyro[:, 1:4].mean(0), 3).tolist(),
                "rms_gyro_dps": np.round(np.degrees(np.sqrt((gyro[:, 4:7] ** 2).mean(0))), 1).tolist(),
            }
        print(json.dumps(d, indent=2, default=str))
    else:
        print(__doc__)
        sys.exit(1)


if __name__ == "__main__":
    main()
