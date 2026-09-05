# QEMU as the second VMM backend: what the spike measured

Status: spike complete, driver unimplemented. 2026-09-05, arm64 Mac dev box
(`hack/dev`), QEMU 8.2.2 from Ubuntu 24.04 `qemu-system-arm`, KVM enabled.

Read [vmm-choice.md](vmm-choice.md) first for why a second backend is the work.
This document is narrower: it is the list of things that were *run*, so the port
does not start by re-deriving them. Everything below was measured on the guest
kernel and rootfs template we actually ship. Nothing here is from documentation.

## Why QEMU and not Cloud Hypervisor

`vmm-choice.md` §1a already revised the target: CoreWeave runs **Kata + QEMU**,
so organisational alignment and the mature VFIO path both point at QEMU, and
Cloud Hypervisor's remaining case is narrow. The spike adds a third reason that
is decisive on its own, and a fourth that is about the abstraction rather than
the engine.

**QEMU keeps `Ballooner.BalloonStats`; Cloud Hypervisor cannot.**
[cloud-hypervisor-port-design.md](cloud-hypervisor-port-design.md) §1 spends a
section on this: `/vm.balloon-stats` exists only on unreleased `main`, so a CH
driver must either return an error — losing the live working-set signal for the
whole fleet — or return zeros, which makes `Manager.MemStats` report every
sandbox as using its full ceiling and balloon *innocent* sandboxes to relieve an
overage that does not exist. Measured under QEMU, one `qom-get`:

    stat-free-memory       1007513600
    stat-available-memory   978378752
    stat-total-memory      1034022912
    query-balloon → {"actual": 1073741824}

That is every field `vmm.BalloonStats` needs, from a released version.

**QEMU is also the better test of the abstraction.** Cloud Hypervisor is close
to a Firecracker clone in the ways that matter here — a REST/JSON API on a Unix
socket, a per-VM argv, a snapshot that is a small set of files. If it slots in
cleanly we learn very little. QEMU differs on every one of those axes (QMP, not
REST; one migration file, not a `mem`/`state` pair), so it exercises the seams
the harness exists to find.

## Measured: it boots our real guest

`-M virt -cpu host -enable-kvm -m 1024 -smp 2 -kernel /assets/vmlinux`, the
shipped `universal.ext4` template, tap networking, scratch on the reflink XFS:

| | |
| --- | --- |
| boot to SSH | **2.3 s, 2.3 s, 2.4 s, 3.3 s** over four runs |
| guest | `Linux 6.1.155 aarch64`, hostname from `sparkbox_host`, root `/dev/vda` 25G |
| the balloon device costs | nothing measurable (2.3 s with, 2.3 s without) |

The Firecracker column of that table is what `hack/parity/run-on-mac.sh` already
produces, which is the point: the comparison is the same 19 cases against two
drivers, not two ad-hoc benchmarks.

## Measured: four things on the kernel command line do not port

`kernelArgs` (`internal/vmm/firecracker/fc.go:711`) is Firecracker-shaped. A
QEMU driver needs its own, differing in exactly these ways:

1. **`pci=off` must go.** Firecracker is MMIO-only; `-M virt` puts virtio on
   PCIe. Left in, the guest comes up with no disk and no NIC.
2. **`root=/dev/vda rw` must be added.** Firecracker synthesises it from the
   drive's `is_root_device`; QEMU does not.
3. **`console=ttyS0` is wrong and the fix is not only in the command line.**
   `-M virt` on arm64 gives a PL011 at `ttyAMA0`, and **our guest kernel has no
   PL011 driver** — `-serial file:` captured 0 bytes across every boot while the
   guest was demonstrably healthy over SSH. The arm64 kernel fragment needs
   `CONFIG_SERIAL_AMBA_PL011`. `cloud-hypervisor-port-design.md` §3.1 predicted
   this for CH from source; it is now measured, and it applies to QEMU too. A
   QEMU sandbox is undebuggable by serial until that lands.
4. **Every PCI device needs `romfile=`.** The Ubuntu package ships no option
   ROMs, so `-device virtio-net-pci,netdev=net0` fails outright with
   `failed to find romfile "efi-virtio.rom"`. We boot with `-kernel`, so the
   PXE ROM is dead weight regardless.

## Measured: snapshot and restore work, with a different shape

    stop
    migrate uri=file:/…/state.migrate     → query-migrate status=completed
    (restore) same argv + -incoming file:/…/state.migrate, then cont

- **57 MB** for a 1 GiB guest — the migration stream skips zero pages.
- Resume reached SSH in ~9 s, and a balloon inflated *before* the snapshot was
  still inflated after it, which is the memory state surviving.
- **One file, not two.** Firecracker's `Pause` writes `mem.snap` + `state.snap`
  and several places stat that literal pair — `Renamer.RenameVM`'s refusal
  predicate (`fc.go:1612`) and `Rebooter.DropSnapshots` among them. Lifted
  unchanged against a single `state.migrate`, the stat matches nothing and
  `RenameVM` silently stops refusing the renames it exists to refuse. This is
  the same trap `cloud-hypervisor-port-design.md` flags for CH's three-file
  snapshot, and the parity suite's `Rename` case is what catches it.

## Measured: scratch placement dominates everything else

A first attempt put VM state on the container's overlayfs. `cp --reflink=always`
fails there outright, and `--reflink=auto` silently falls back to copying 25 GiB
— which turned a 2.3 s boot into a >45 s one and made every timing meaningless.
Scratch must be on the reflink XFS, which is what `hack/parity/run-on-mac.sh`
already arranges. Worth stating because the failure mode is a slow test, not an
error.

## Since superseded: the driver exists and the suite is green

Everything above is the hand-driven spike that decided the backend. The driver
it justified is `internal/vmm/qemu`, and it passes all nineteen parity cases
against real guests — see
[vmm-parity-harness.md](vmm-parity-harness.md#what-the-second-driver-found) for
what the port found, the Firecracker-vs-QEMU timings, and the harness gap the
port exposed. `hack/parity/run-on-mac.sh --pkg ./internal/vmm/qemu --run
TestQEMUParity --base 127.0.0.1:5001/sparkbox-qemu:dev` reproduces it; the base
image is `sparkbox-cks:dev` plus `qemu-system-arm`, because the stock node image
has no QEMU and every case would fail at exec of the VMM.

Three of the entries below are now answered, and are struck through.

## Not measured — do not assume these

- **x86_64.** Everything above is arm64. `hack/parity/run-on-cks.sh` exists and
  has still never been run.
- **Nested virtualisation through a QEMU snapshot.** Carrying it is Cloud
  Hypervisor's one measured win (`cloud-hypervisor-feasibility.md` §11). QEMU
  almost certainly manages it and that is worth nothing until someone runs it.
- **Whether `-incoming` tolerates a changed disk.** It must not, and the driver
  contract already assumes it does not, but it was not tested. Still open — the
  parity suite's `Rename` case exercises the *refusal*, not the tolerance.
- **CPU and net stats.** Expected to lift unchanged — both read the host
  (`/proc/<pid>/stat`, `/sys/class/net/sbtapN/statistics/*`) and neither asks
  the VMM anything. Unverified.
- ~~**Any of the 19 parity cases.**~~ **All nineteen pass.** 236.90s total on
  the arm64 dev box against Firecracker's 225.12s, all ten capabilities present,
  no skips. Read the timings in
  [vmm-parity-harness.md](vmm-parity-harness.md#how-not-to-measure-this-learned-the-hard-way)
  with its caveats: a first pass at this comparison was wrong in both directions
  because the machine was resized between runs and a cold page cache inflated
  three cases by 4-6x.

## Reproducing

`hack/qemu-spike/` holds the scripts, and they are throwaway: they build a
`sparkbox-qemu:smoke` image on the dev box from `sparkbox-cks:dev` plus
`qemu-system-arm`, and drive QEMU with hand-written argv and a 30-line QMP
client. They are kept because the measurements above should be checkable, not
because they are a harness. The harness is `internal/vmm/vmmtest`.
