#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""
measure-density.py — how much host RAM does an *idle* sparkbox actually cost?

The pooled resource-model design assumed a warm 8 GB VM costs 8 GB of host RAM,
so density had to come from suspend-to-disk pausing. exe.dev disproves that:
"Your VMs share CPU/RAM — you pay for underlying resources, not per VM." A
Firecracker guest's memory is lazily-allocated anonymous mmap, so an idle guest
that booted with an 8 GB *ceiling* only faults in its small working set. This
script measures the truth on a real KVM host.

It drives the local sparkbox control API to boot idle VMs one at a time and,
after each, samples:

  - /proc/meminfo   → MemAvailable (the honest "what's left" number, net of
                      reclaimable cache) and Swap usage
  - per-firecracker /proc/<pid>/smaps_rollup Pss → real host memory attributed
                      to each VM (Pss splits shared/KSM-merged pages fairly)
  - /sys/kernel/mm/ksm/* → dedup state (pages_sharing etc.)

The headline outputs are the *marginal* MemAvailable cost per idle VM and the
mean Pss per firecracker vs the 8 GB it was configured with — i.e. the real
overcommit ratio. With --ksm it then turns KSM on, waits for a scan, and
reports how much more it recovers (huge when every VM booted the same image).

SAFETY
  - Run as root (needed to read root-owned firecracker smaps and toggle KSM).
  - Probe VMs are created under a distinct owner (--owner, default
    "density-probe") and destroyed at the end unless --keep. Use --cleanup-only
    to remove leftovers from an interrupted run.
  - On a production box, probes consume real RAM and may hit admission
    (--mem-admission-pct). For a clean ceiling measurement, run against a
    scratch host, ideally a sparkbox started with --mem-admission-pct 0.
  - --ksm flips a HOST-GLOBAL knob (/sys/kernel/mm/ksm/run). It affects every
    VM on the box, not just the probes. It is restored to its prior value on
    exit.

USAGE
  sudo ./measure-density.py --count 20
  sudo ./measure-density.py --count 20 --ksm --json /tmp/density.json
  sudo ./measure-density.py --cleanup-only
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

KSM = "/sys/kernel/mm/ksm"


# --------------------------------------------------------------------------- #
# Host sampling (/proc, /sys)
# --------------------------------------------------------------------------- #
def read_meminfo() -> dict[str, int]:
    """Return /proc/meminfo values in MiB (they're reported in kB)."""
    out: dict[str, int] = {}
    with open("/proc/meminfo") as f:
        for line in f:
            key, _, rest = line.partition(":")
            kb = int(rest.strip().split()[0])
            out[key] = kb // 1024
    return out


def read_ksm() -> dict[str, int]:
    """Return the KSM counters we care about; empty if KSM isn't compiled in."""
    keys = ["run", "pages_sharing", "pages_shared", "pages_unshared",
            "pages_volatile", "full_scans", "pages_to_scan", "sleep_millisecs"]
    out: dict[str, int] = {}
    for k in keys:
        try:
            with open(os.path.join(KSM, k)) as f:
                out[k] = int(f.read().strip())
        except OSError:
            pass
    return out


def firecracker_pids() -> list[int]:
    """PIDs whose comm is 'firecracker' (one process per running VM)."""
    pids: list[int] = []
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            with open(f"/proc/{entry}/comm") as f:
                if f.read().strip() == "firecracker":
                    pids.append(int(entry))
        except OSError:
            continue
    return pids


def proc_pss_mb(pid: int) -> float:
    """Proportional set size of a process in MiB, from smaps_rollup (root-only)."""
    try:
        with open(f"/proc/{pid}/smaps_rollup") as f:
            for line in f:
                if line.startswith("Pss:"):
                    return int(line.split()[1]) / 1024.0
    except OSError:
        pass
    return 0.0


def total_fc_pss_mb() -> tuple[int, float]:
    """(count, summed Pss in MiB) across all firecracker processes."""
    pids = firecracker_pids()
    return len(pids), sum(proc_pss_mb(p) for p in pids)


def sample() -> dict:
    mem = read_meminfo()
    n, pss = total_fc_pss_mb()
    return {
        "t": round(time.time(), 1),
        "mem_available": mem.get("MemAvailable", 0),
        "mem_free": mem.get("MemFree", 0),
        "cached": mem.get("Cached", 0),
        "swap_used": mem.get("SwapTotal", 0) - mem.get("SwapFree", 0),
        "fc_count": n,
        "fc_pss_total": round(pss, 1),
        "ksm": read_ksm(),
    }


# --------------------------------------------------------------------------- #
# sparkbox control API
# --------------------------------------------------------------------------- #
class API:
    def __init__(self, base: str):
        self.base = base.rstrip("/")

    def _req(self, method: str, path: str, body: dict | None = None) -> tuple[int, dict | None]:
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=120) as r:
                raw = r.read()
                return r.status, (json.loads(raw) if raw else None)
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                return e.code, json.loads(raw)
            except Exception:
                return e.code, {"error": raw.decode(errors="replace")}

    def create(self, name, owner, image, vcpus, mem_mb):
        return self._req("POST", "/v1/sandboxes", {
            "name": name, "owner": owner, "image": image,
            "vcpus": vcpus, "mem_mb": mem_mb,
        })

    def list(self):
        _, body = self._req("GET", "/v1/sandboxes")
        return body or []

    def destroy(self, name):
        return self._req("DELETE", f"/v1/sandboxes/{name}")


# --------------------------------------------------------------------------- #
# KSM control (host-global; restored on exit)
# --------------------------------------------------------------------------- #
def ksm_write(key: str, value: int) -> bool:
    try:
        with open(os.path.join(KSM, key), "w") as f:
            f.write(str(value))
        return True
    except OSError as e:
        print(f"  ! couldn't set ksm/{key}={value}: {e}", file=sys.stderr)
        return False


# --------------------------------------------------------------------------- #
# Reporting
# --------------------------------------------------------------------------- #
def fmt_row(label: str, s: dict, base_avail: int) -> str:
    per_vm = ""
    if s["fc_count"]:
        avail_cost = (base_avail - s["mem_available"]) / s["fc_count"]
        per_vm = f"  Δavail/VM {avail_cost:6.0f}MB  pss/VM {s['fc_pss_total']/s['fc_count']:6.0f}MB"
    share = s["ksm"].get("pages_sharing", 0) * 4 // 1024  # 4KiB pages -> MiB
    return (f"{label:>14}  VMs {s['fc_count']:3d}  "
            f"MemAvail {s['mem_available']:7d}MB  swap {s['swap_used']:6d}MB  "
            f"ksm_shared {share:6d}MB{per_vm}")


def main() -> int:
    ap = argparse.ArgumentParser(description="Measure real idle-VM RAM density on a KVM host.")
    ap.add_argument("--api", default="http://127.0.0.1:8080", help="sparkbox control API base URL")
    ap.add_argument("--count", type=int, default=10, help="number of idle probe VMs to boot")
    ap.add_argument("--image", default="universal", help="rootfs template")
    ap.add_argument("--owner", default="density-probe", help="owner tag for probe VMs")
    ap.add_argument("--mem-mb", type=int, default=8192, help="per-VM memory ceiling")
    ap.add_argument("--vcpus", type=int, default=2, help="per-VM vCPU ceiling")
    ap.add_argument("--settle", type=float, default=25.0, help="seconds to let each VM idle before sampling")
    ap.add_argument("--ksm", action="store_true", help="after booting, enable KSM and measure the dedup savings")
    ap.add_argument("--ksm-scan-wait", type=float, default=60.0, help="seconds to let KSM scan before re-sampling")
    ap.add_argument("--ksm-pages-to-scan", type=int, default=1000, help="ksm pages_to_scan while measuring")
    ap.add_argument("--keep", action="store_true", help="don't destroy probe VMs at the end")
    ap.add_argument("--cleanup-only", action="store_true", help="destroy leftover probe VMs and exit")
    ap.add_argument("--json", default="", help="write the full sample log to this JSON path")
    args = ap.parse_args()

    api = API(args.api)

    # Cleanup path: remove every sandbox owned by the probe tag.
    if args.cleanup_only:
        return cleanup(api, args.owner)

    if os.geteuid() != 0:
        print("warning: not running as root — firecracker Pss and KSM control need root.\n",
              file=sys.stderr)

    mem = read_meminfo()
    print(f"host: MemTotal {mem.get('MemTotal',0)}MB  SwapTotal {mem.get('SwapTotal',0)}MB  "
          f"kvm={'yes' if os.path.exists('/dev/kvm') else 'NO'}  "
          f"ksm_run={read_ksm().get('run','?')}")
    print(f"plan: boot {args.count} × ({args.vcpus} vCPU / {args.mem_mb}MB {args.image}) "
          f"as owner={args.owner}, settle {args.settle}s each\n")

    samples: list[dict] = []
    base = sample()
    base_avail = base["mem_available"]
    samples.append({"label": "baseline", **base})
    print(fmt_row("baseline", base, base_avail))

    created: list[str] = []
    ksm_restore: int | None = None
    try:
        for i in range(1, args.count + 1):
            name = f"{args.owner}-{i:03d}"
            # Each probe gets its OWN owner so the per-owner running cap
            # (--max-running-per-owner, default 2) never fires — we want the
            # *memory* admission budget to be the thing that stops us, since
            # that's the number the redesign is about (it counts the full 8GB
            # ceiling per VM and will refuse while the box is still mostly free).
            owner = name
            status, body = api.create(name, owner, args.image, args.vcpus, args.mem_mb)
            if status not in (200, 201):
                print(f"  ! create {name} failed [{status}]: {(body or {}).get('error')}",
                      file=sys.stderr)
                print("    stopping early — likely hit admission or capacity.", file=sys.stderr)
                break
            created.append(name)
            time.sleep(args.settle)
            s = sample()
            samples.append({"label": name, **s})
            print(fmt_row(f"+{i}", s, base_avail))

        if args.ksm and created:
            print("\n--- enabling KSM (host-global) ---")
            ksm_restore = read_ksm().get("run", 0)
            ksm_write("pages_to_scan", args.ksm_pages_to_scan)
            ksm_write("run", 1)
            time.sleep(args.ksm_scan_wait)
            s = sample()
            samples.append({"label": "ksm-on", **s})
            print(fmt_row("ksm-on", s, base_avail))

        summarise(samples, base_avail, args)
    finally:
        if ksm_restore is not None:
            ksm_write("run", ksm_restore)
            print(f"restored ksm/run={ksm_restore}")
        if args.json:
            with open(args.json, "w") as f:
                json.dump({"args": vars(args), "samples": samples}, f, indent=2)
            print(f"wrote {args.json}")
        if created and not args.keep:
            print("cleaning up probe VMs...")
            for name in created:
                api.destroy(name)
            print(f"destroyed {len(created)} probe VMs")
        elif created:
            print(f"kept {len(created)} probe VMs (--keep); remove with --cleanup-only")

    return 0


def summarise(samples: list[dict], base_avail: int, args) -> None:
    booted = [s for s in samples if s["fc_count"] and s["label"] not in ("baseline", "ksm-on")]
    if not booted:
        print("\nno VMs booted — nothing to summarise.")
        return
    last = booted[-1]
    n = last["fc_count"]
    avail_cost = (base_avail - last["mem_available"]) / n
    pss_per = last["fc_pss_total"] / n
    print("\n" + "=" * 68)
    print(f"booted {n} idle VMs, each configured for {args.mem_mb}MB")
    print(f"  marginal MemAvailable cost : {avail_cost:6.0f} MB / VM   "
          f"({avail_cost/args.mem_mb*100:.1f}% of the 8GB ceiling)")
    print(f"  mean firecracker Pss       : {pss_per:6.0f} MB / VM")
    print(f"  overcommit ratio           : {args.mem_mb/max(pss_per,1):5.1f}× "
          f"(configured ÷ actual Pss)")
    mem = read_meminfo()
    if avail_cost > 0:
        fits = int(mem.get("MemTotal", 0) / avail_cost)
        print(f"  → at this rate a {mem.get('MemTotal',0)}MB box fits ~{fits} idle VMs "
              f"(admission today would cap it at {mem.get('MemTotal',0)//args.mem_mb})")
    ksm_on = next((s for s in samples if s["label"] == "ksm-on"), None)
    if ksm_on:
        recovered = ksm_on["mem_available"] - last["mem_available"]
        share = ksm_on["ksm"].get("pages_sharing", 0) * 4 // 1024
        print(f"  KSM recovered              : {recovered:6d} MB MemAvailable "
              f"({share}MB reported shared) across {n} same-image VMs")
    print("=" * 68)


def cleanup(api: API, owner: str) -> int:
    # Probes are created with owner == name == "<tag>-NNN", so match the tag as
    # a prefix (each probe is its own owner to dodge the per-owner running cap).
    boxes = [b for b in api.list()
             if b.get("owner", "").startswith(owner) or b.get("name", "").startswith(owner)]
    for b in boxes:
        api.destroy(b["name"])
    print(f"destroyed {len(boxes)} sandbox(es) tagged {owner!r}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
