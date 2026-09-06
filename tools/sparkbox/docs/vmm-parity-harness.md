# The VMM parity harness

Status: built and green against the Firecracker driver on real hardware,
2026-09-04. Companion to [vmm-choice.md](vmm-choice.md) §9.2, which is why it
exists, and [cloud-hypervisor-feasibility.md](cloud-hypervisor-feasibility.md)
§12, whose closing paragraph asked for exactly this.

## The problem it solves

`internal/vmm` exposes a five-method `Driver` plus **ten** optional capability
interfaces — `Archivable`, `DiskReporter`, `TemplateReporter`,
`RootfsPresencer`, `Renamer`, `Rebooter`, `CPUStatser`, `NetStatser`,
`DiskResizer`, `Ballooner`. Until now it had one real implementation,
`firecracker/fc.go` at ~2,150 lines, and **its ~1,300 lines of tests never
booted a guest**: every one of them constructs the `Driver` struct directly,
with a comment saying it does so to avoid `New()`'s `/dev/kvm` requirement.
Every lifecycle end-to-end in the repository runs against the mock.

So the abstraction was unproven in the specific sense that matters:

- nobody had written down what "correct" means for a driver in a form a driver
  can be *run* against, so no second backend could be judged;
- the manager reaches every optional capability by type assertion and degrades
  silently when one is missing, and only four of the ten had a compile-time
  assertion holding them in place;
- the one bug we have shipped in this area — `ctl rm` leaking a rootfs whenever
  the driver had no in-memory record, so a re-used name booted the previous
  tenant's filesystem — is precisely the kind a boot-level suite catches and a
  mock-level one does not.

This is the missing suite. It is needed on any VMM, and building it did not
require the Firecracker-vs-Cloud-Hypervisor-vs-QEMU question to be settled,
which is its main virtue given that question is genuinely open.

## What it is

One suite — [`internal/vmm/vmmtest`](../internal/vmm/vmmtest) — parameterised by
a `Fixture` that names a driver, a template, a login key and a set of `Traits`.
Two drivers are wired into it:

| wiring | boots | gate |
| --- | --- | --- |
| `internal/vmm/mock/parity_test.go` | nothing (in-process SSH servers over a workdir) | none: ordinary `go test ./...` |
| `internal/vmm/firecracker/parity_linux_test.go` | real microVMs | `SPARKBOX_VMM_PARITY=1` + fixtures |

**The assertions come from the contracts documented on the interfaces**, not
from either implementation's behaviour. "Snapshot operates on a stopped VM", "a
disk under a name the ledger has never issued must be refused", "a missing
template is an error, not zero, so callers keep their last baseline", "rx is
what the *guest* received" — each is a sentence in `driver.go` that now has a
test.

### Traits, and why they are not just skips

Some contracts are only observable on a machine that really boots. A `Trait` is
a claim a driver makes about itself, and a false one removes only the part of a
case that would be meaningless — never the case:

| trait | what it unlocks |
| --- | --- |
| `RealGuest` | hostname from the unforgeable `sparkbox_host` kernel arg, `id -un` matching `Instance.SSHUser`, `Instance.HostIP` actually on the guest |
| `PreservesMemory` | `/proc/sys/kernel/random/boot_id` unchanged across a resume, and a `/dev/shm` marker still there — the difference between resuming and rebooting |
| `SanitizesForks` | a fork presents a *different* SSH host key from the sandbox it was taken from |
| `LiveDiskUsage` | writing 64 MiB inside a running guest moves `DiskUsageMB` |
| `DistinctHostIPs` | two sandboxes do not share an address (the mock puts everything on 127.0.0.1 and separates by port, which means it cannot forward the same guest port twice) |
| `BaseImageIsTemplate` | `TemplateUsageMB` can measure the base image |

The mock sets exactly one — `LiveDiskUsage`, because its "disk" is a host
directory and its usage figure is as live as the filesystem under it. That is
worth claiming rather than waiving: it means the *positive* form of that
assertion runs somewhere on every `go test ./...`, which is the only reason the
firecracker result below reads as a finding rather than as an untested case.
Everything else is false. That is the honest answer, and running the suite
against it is still worth it for two reasons: it keeps every case compiling and
exercised on a laptop between hardware runs, and it holds the mock to the same
refusals as the real driver — which is what makes it a defensible stand-in for
the manager tests.

Wiring the mock in found three genuine divergences on the first run, all now
recorded as traits rather than papered over: every mock sandbox shares
`127.0.0.1`, the mock has no memory snapshot so it cannot refuse a rename over
one, and the mock's base image is not a template until `Snapshot` mints one.

### What it covers

Nineteen cases, in
[lifecycle.go](../internal/vmm/vmmtest/lifecycle.go) and
[capabilities.go](../internal/vmm/vmmtest/capabilities.go):

- **Boot and SSH in** — the case the whole harness exists for. A driver that
  boots nothing has always passed every test in this repository.
- **Pause / resume** — the disk survives (all drivers) and the *memory* survives
  (`PreservesMemory`). Pause is asserted idempotent, because the reaper pauses
  unattended and can race a user.
- **Pause / resume of an unknown name** — must error. A `Pause` returning nil
  for a sandbox the driver has never seen would have the reaper believe it freed
  memory it never touched.
- **Destroy** — idempotent, and `RootfsPresent` false afterwards.
- **Destroy then re-use the name** — the shipped bug, in the shape that broke:
  drop the driver's record first (via `Rebooter`, the only interface that does
  so without touching the disk), *then* destroy, then create the name again and
  assert the new sandbox cannot read the old one's file.
- **Create refuses residue** — the same contract pointing the other way. With a
  disk present and no record, `Create{NewSandbox: true}` must be refused and
  `Create{NewSandbox: false}` must cold-boot it.
- **Two at once** — distinct addresses, distinct disks, distinct hostnames.
  Per-slot collisions (tap names, jail uids) are invisible to a suite that only
  ever creates one VM.
- **Checkpoint / restore** (`Archivable`) — pack refuses a running VM; the
  artifact survives `Destroy` of the VM it came from; unpack into a name with no
  disk at all, then cold-boot it and read the marker back.
- **Fork from a snapshot template** (`Archivable`) — the fork carries the
  source's files, the *source still works afterwards*, the fork is not the
  source (`SanitizesForks`), and `RemoveTemplate` is idempotent.
- **Rootfs presence** (`RootfsPresencer`) — false before, true after, still true
  while paused.
- **Reboot** (`Rebooter`) — refused while running; then the boot id *changes*,
  `/dev/shm` is empty, and the disk marker survives. On a real guest that is a
  measurement, not an assumption.
- **Rename** (`Renamer`) — refused while running, refused with a memory snapshot
  present, and afterwards the guest calls itself by the new name and the old
  disk is gone rather than copied.
- **Disk report** (`DiskReporter`) — usage ≤ capacity, and the driver's reading
  compared against the guest's own `df`, **always printed** whether or not the
  driver claims `LiveDiskUsage`. A meter that is merely stale looks identical to
  a correct one until the two are put side by side; this is the case that put
  them there, and §"What the first run found" is what came back.
- **Template usage** (`TemplateReporter`) — a positive baseline, and a missing
  template is an **error**.
- **Disk resize** (`DiskResizer`) — refused while running, refused shrinking,
  grows the ceiling, and the guest sees the new size.
- **Balloon** (`Ballooner`) — inflate, poll to target, *the guest is still
  answering SSH while ballooned* (the whole difference between this and a
  pause), deflate to zero, and both calls refused on a paused sandbox.
- **CPU stats** (`CPUStatser`) — cumulative and advancing across real guest work.
- **Net stats** (`NetStatser`) — 8 MiB each way, so a driver reporting one
  counter for both directions or swapping them is visible.

Plus a **Capabilities** case that logs the full inventory, so `-v` answers
"which capabilities did this run exercise" without reading skip lines.

## What the first runs found

Nineteen cases, arm64, against the dev box's own `universal` template and guest
kernel. Eighteen passed first time. The nineteenth found a product bug, and
running the suite twice more found an environment limit that would otherwise
have been read as one.

### 1. `DiskUsageMB` does not track a running sandbox. At all.

The guest wrote 256 MiB (the case now writes 64; the characterisation run used
256), ran `sync`, and reported the change in its own `df`
immediately — 3502 MiB used, up from 3229. The driver's reading stayed at
**3229 MiB**: across the write, across `sync`, across four minutes of repeated
syncs 30 seconds apart, and across a pause. 3229 MiB is not a rounding error or
a lag — it is *the template's* usage, the figure the disk had before this
sandbox ever booted.

The cause is a one-line consequence of how the number is read.
`firecracker.DiskUsageMB` parses `s_free_blocks_count` out of the rootfs image's
ext4 superblock (`ext4DiskMB`, fc.go). Linux keeps that counter in memory for a
mounted filesystem and does not write it back on `sync` — `statfs` answers from
the in-memory percpu counters, which is why the guest's `df` is right and the
host's read is not. It is committed on unmount, on freeze, and on
remount-read-only, none of which happen to a sandbox's root filesystem while it
exists. What *does* fix it is `e2fsck`, which `PackRootfs` and `Snapshot` run —
so the figure is accurate after an archive or a snapshot and stale every other
moment.

Consequences worth stating plainly, because this feeds two user-visible things:

- the console's disk meter shows the template's usage for the life of a
  sandbox, so a user filling their disk sees no movement;
- pooled per-owner disk accounting is computed from the same reading, so quota
  is enforced against a number that does not grow.

Neither is fixed here — this branch is the harness, and the fix is a driver
change with its own trade (an `e2fsck` per poll is not viable; asking the guest
agent for `statfs` or reading the block-group descriptors are the plausible
directions). What the harness does is make it *stated*: `firecracker` declares
`LiveDiskUsage: false` with the measurement in the comment, `vmm.DiskReporter`'s
doc now says freshness is not part of its contract, and the case prints the
guest-vs-host gap on every run so the day a driver fixes it is visible.

This is the harness paying for itself on its first run, and it is exactly the
class of thing 1,300 lines of tests that never boot a guest cannot see.

### 2. 2 GiB parity guests do not fit on the Mac dev box beside the dev pod

The second full run degraded case by case — `ForkFromTemplate` 51s → 320s,
`Reboot` 7s → 142s — and then every case from the thirteenth on failed
identically: `dial …:22: connect: no route to host`, three minutes, guest never
up. Six failures that look exactly like a driver that cannot boot a VM.

It is not. The container machine has 12 GiB and was sharing it with the dev
pod's own sandboxes and ~8 GiB of page cache from repeatedly reading a 25 GiB
template, and this box has a **measured first-touch memory cliff above roughly
1 GiB per guest** — the same one that produced the "wedged guest" hunt recorded
in the dev-box notes. Dropping the parity guests from 2048 MiB to **1024 MiB**
turned the three worst cases into `ForkFromTemplate` 32s, `Reboot` 6.7s,
`Rename` 8.0s. Nothing else changed.

So `run-on-mac.sh` defaults to 1 GiB guests and a 240s boot timeout, both
overridable (`--mem`, `--boot-timeout`), and it prints free memory and the count
of firecracker processes already on the machine before it starts. A slow run
here is a capacity fact about this laptop, and the harness should say so rather
than let it read as a defect in the thing under test.

The general lesson for anyone adding cases: **on this host, a timeout is
ambiguous.** Re-run the failing case alone before believing it.

### The green run

`hack/parity/run-on-mac.sh`, arm64, 1 GiB guests, 2 vCPU, against
`universal.ext4` and the dev box's own `vmlinux`. **19 of 19, 152s.**

| case | s | case | s |
| --- | --: | --- | --: |
| Capabilities | 0.0 | RootfsPresence | 1.5 |
| BootAndSSH | 3.0 | Reboot | 6.5 |
| PauseResume | 8.2 | Rename | 7.5 |
| PauseResumeUnknown | 0.0 | DiskReport | 4.4 |
| Destroy | 2.9 | TemplateUsage | 0.0 |
| DestroyThenReuseName | 8.8 | DiskResize | 9.0 |
| CreateRefusesResidue | 6.8 | Balloon | 7.3 |
| TwoAtOnce | 4.3 | CPUStats | 6.6 |
| Archive | 50.5 | NetStats | 3.9 |
| ForkFromTemplate | 21.6 | | |

`Archive` is the expensive one and legitimately so: `PackRootfs` runs `e2fsck` and
`zerofree` across the 25 GiB image and then zstd-compresses it to ~1 GB. Nothing
else is over ten seconds.

## Compile-time capability assertions: four → ten

`internal/vmm/mock/mock_test.go` and `internal/vmm/firecracker/fc_linux_test.go`
each held four `var _ vmm.X = (*Driver)(nil)` lines. Both now hold eleven — the
`Driver` interface and all ten capabilities.

This is not tidiness. `host.Manager` reaches each capability by type assertion
and falls back silently when the assertion fails, so a capability lost to a
refactor — a receiver changed from pointer to value, a method renamed, a
signature drifting — degrades the fleet with **no error anywhere**. The block is
the only thing that turns that into a build failure, and the suite is the only
thing that would notice the degradation at runtime.

## The gate: an environment variable, not a build tag

`SPARKBOX_VMM_PARITY=1`, checked at run time. `go test ./...` on a laptop
compiles every line of the real-guest wiring and skips.

A build tag was the obvious alternative and is the wrong one. A tag keeps the
file out of `go test ./...` **by never compiling it**, so it stops catching
exactly the signature drift the capability assertions exist for — which is how a
harness rots into a file nobody can build. The env gate costs a `t.Skip` and
keeps the compiler in the loop.

Once the gate is set, a *missing fixture is fatal, not a skip*. The operator
meant to run this; a harness that silently declines to run is the thing being
replaced.

## Where it runs

### Tier 1 — every PR, no special hardware

`go test ./...` already runs the mock wiring. Nothing to configure, ~2s, and it
is what keeps the suite alive.

### Tier 2 — local iteration on the Mac dev box (arm64)

```sh
hack/parity/run-on-mac.sh
hack/parity/run-on-mac.sh --run 'TestFirecrackerParity/Balloon' --keep
```

`/dev/kvm`, the guest kernel, the rootfs template and a reflink-capable
filesystem all live inside the Apple container machine `hack/dev` uses. The
script builds a `linux/arm64` `go test -c` binary, ships it in, and runs it in a
throwaway container **beside** the dev pod — its own network namespace so the
`sbtapN` devices cannot collide with the node's, its own `VMStateDir` so nothing
is visible to a sandbox somebody is working in, and its own driver `Options` so
it runs the direct launcher and lets the driver do its own rootfs key injection,
which the node (privileged helper, host rootfs mounts disabled) never exercises.
It mounts the dev pod's assets and image template **read-only**: a parity run
should boot the artifact the fleet boots.

Two transport details worth keeping, because both cost time to discover:

- **The binary rides a registry layer, not HTTP.** Serving it from a listener on
  the Mac and `curl`-ing from inside the machine fails with `curl: (52) Empty
  reply from server` — the TCP handshake completes and then nothing, which is
  the macOS firewall and not a routing problem, and it looks like neither.
  `docker build` + push to the local registry + pull inside the machine is the
  direction `hack/dev/image.sh` already proves works.
- **The container is detached and polled, not streamed.** A `docker run` held
  open down the `container machine run -i` stdin transport for forty minutes is
  one dropped connection away from losing the run; a container that outlives the
  transport is the recovery path.

### Tier 3 — the other architecture, on CKS (x86_64)

```sh
hack/parity/run-on-cks.sh
```

Same discipline as `hack/m0b/run-on-cks.sh`: its own namespace, `hostPath`
`/dev/kvm` with `privileged: true` (the device plugin cannot help —
`sparkbox.dev/kvm` is allocatable 1 and held by `vmm-helper`), CPU, memory and
ephemeral-storage capped, deleted on exit, and `kubectl -n sparkbox-poc get
pods` printed at the end so the "did I disturb anything" check is not optional.

It mounts `/var/lib/sparkbox` **read-only** for the kernel and template, and
writes nothing there. Its scratch is a **loopback XFS inside the Pod's
emptyDir**, because the driver reflink-clones the template and refuses to fall
back to a full 25 GiB copy — so `VMStateDir` must be reflink-capable and must
not be the node's filesystem. The template is copied in once (`--sparse=always`,
so it costs its used size), and every VM after that is a clone within the image.

x86_64 matters and is not a formality: the CKS fleet is amd64 and the DGX and
Mac are arm64, and the driver's CPUID handling, `vmlinux` entry path and MSR
behaviour are all architecture-specific.

**This script has not been run.** The Mac tier answered the question this
session set out to answer — the suite is green against the Firecracker driver on
a real KVM host — and the only reason left to reach for the production node was
the second architecture, which is worth doing but was not worth doing
unsupervised. Two things in it are therefore unverified by anything but reading:
the layout of `/var/lib/sparkbox` on the node (it guesses `assets/vmlinux` and
`images/*.ext4`, and dies with a clear message if it guesses wrong), and whether
`ubuntu:24.04` plus the `apt-get` line in step 5 is a complete toolchain for the
`Archivable` and `DiskResizer` cases. Run it with `--run
'TestFirecrackerParity/BootAndSSH'` first.

### GitHub Actions — measured, not assumed

The `smoke` job's comments say hosted runners have no nested virtualization and
`/dev/kvm` is a placeholder file. That is certainly true of `ubuntu-24.04-arm`,
which is the runner that job uses. Applied to `ubuntu-24.04` on x86_64 it is a
claim about a *different runner image*, and reading one artifact's behaviour as
another's is the exact mistake feasibility §10 had to correct on hardware.

So the workflow now carries a `kvm-probe` job that records, per run and failing
on nothing:

1. whether `/dev/kvm` is present, is a character device rather than a
   placeholder file, and is readable and writable by the runner user;
2. whether any writable filesystem supports reflink, and — since the answer for
   ext4 is no — whether a **loopback XFS** can be created and mounted, which is
   the fallback both parity runners use;
3. how much room the disks have, since the fixture is a 25 GiB template.

**The decision keyed to that probe**, to be taken on its first green run rather
than now:

- If `VERDICT_KVM=character-device` and `VERDICT_KVM_ACCESS` is not
  `unreadable`, and `VERDICT_LOOPBACK_XFS=yes`, then a nightly x86_64 parity job
  is buildable on hosted runners. It would download the pinned release's
  `vmlinux-linux-amd64` and `universal-amd64.ext4.zst` — the release workflow
  already builds both on hosted runners — decompress onto the loopback XFS with
  `zstd --sparse`, and run the same test binary the other two tiers run. That
  gives per-night x86_64 coverage without touching the production node, and
  leaves `run-on-cks.sh` for pre-release checks against the deployed artifact.
- If the probe says otherwise, Tier 3 is the only x86_64 answer and it stays
  manual and pre-release. Write that down and stop guessing.

Either way the job is nightly or manual, never per-PR: the fixture download and
a full suite run are minutes, and per-PR value is already covered by Tier 1.

## What a green run does not tell you

Named explicitly, because a harness that oversells itself is worse than none:

- **The privileged-helper and jailer launch paths are not covered.** The runners
  use the direct launcher (`JailerBin` empty, `ChrootJailer` false,
  `PrivilegedHelperSocket` empty). Production runs `ChrootJailer` +
  `PrivilegedHelperSocket` with `DisableHostRootfsMounts=true`. That is the next
  thing to parameterise, and it is a `Fixture` variation rather than new
  assertions.
- **IPv6 is not covered.** `Subnet6` is left empty, so `GuestV6`, proxy NDP and
  the per-slot /127 carving are exercised only by the existing unit tests.
- **Nothing is asserted about concurrency beyond two.** `TwoAtOnce` catches slot
  collisions; it does not model the fleet.
- **No nested guests.** feasibility §12's open list — a fork restoring the same
  snapshot twice, an inner VM doing real work across a pause, repeated
  pause/resume cycles, restore onto a different host — is still open. The
  harness is where those cases belong; they are not in it yet.
- **A pass on one architecture is not a pass on the other.** Run both tiers.

## Adding a second driver

Write `internal/vmm/<name>/parity_linux_test.go`, build a `Fixture`, declare the
`Traits` truthfully, and call `vmmtest.Run`. That is the whole integration. What
the suite then reports is a measurement of the abstraction rather than an
argument about it — which was the point of building it before choosing the
backend.

That claim has now been tested. `internal/vmm/qemu` is the second backend
([qemu-spike.md](qemu-spike.md) for why QEMU and not Cloud Hypervisor), and the
integration really was a `Fixture` plus a `Traits` declaration: **the abstraction
in `driver.go` needed no change at all.** All eleven compile-time assertions
compile against it, all ten capabilities are present, and the suite went green
19/19 on its first real-guest run.

### What the second driver found

Three things, in increasing order of how much they matter.

**The abstraction holds.** No method of `vmm.Driver` or of the ten capability
interfaces had to be widened, split or relaxed to admit a VMM with a different
control protocol (QMP, not REST) and a different snapshot mechanism (one
migration file, not a `mem`/`state` pair). The one interface that was *predicted*
to be a problem — `Ballooner.BalloonStats`, which
[cloud-hypervisor-port-design.md](cloud-hypervisor-port-design.md) §1 shows Cloud
Hypervisor cannot satisfy from any released version — QEMU satisfies in full, and
the parity run proves it on hardware: `free 847, available 880 MiB`.

**Several capability implementations lift, but two of them lift *wrongly*, and
silently.** `Renamer.RenameVM` refuses a rename over a snapshot by stat-ing the
literal `mem.snap`/`state.snap` pair; against a one-file snapshot that stat
matches nothing and the refusal quietly stops refusing. `Rebooter.DropSnapshots`
has the same defect in the other direction — it removes two files QEMU never
writes and leaves the real one in place, for exactly the `Resume` it exists to
prevent. Both are the shape this suite was built to catch, and its `Rename` and
`Reboot` cases do catch them.

**The suite has a gap of its own, and porting against it is what exposed it.**
`NetStats` cannot catch an rx/tx swap. It pushes 8 MiB into the guest and 8 MiB
out, then asserts both counters grew past the *same* 4 MiB threshold
(`capabilities.go:534-582`), so a driver that swaps the directions — or reports
one shared counter for both — passes green. `NetStatser`'s contract is that the
numbers are guest-oriented while the host's `sbtapN` counters are host-oriented,
so the swap is real code that this case does not verify. Fixing it needs
asymmetric payloads and per-direction thresholds. Until then the QEMU wiring's
`RealGuest` comment records the gap rather than claiming the coverage.

### Firecracker vs QEMU, same box, same nineteen cases

Two runs each, back to back on the arm64 dev box with 32 GiB and 1 GiB guests.
Totals 225.12s (Firecracker) against 236.90s (QEMU).

| case | Firecracker | QEMU |
| --- | --- | --- |
| PauseResume | 9.57s | **5.20s** |
| ForkFromTemplate | 48.18s | **36.52s** |
| DestroyThenReuseName | 9.06s | **7.40s** |
| CreateRefusesResidue | 7.09s | **5.33s** |
| RootfsPresence | 2.16s | **1.11s** |
| DiskResize | 10.03s | **8.50s** |
| NetStats | 5.50s | **4.96s** |
| BootAndSSH | **2.97s** | 3.49s |
| Destroy | **2.92s** | 3.67s |
| TwoAtOnce | **3.35s** | 3.77s |
| DiskReport | **4.33s** | 6.80s |
| Reboot | **6.16s** | 9.34s |
| Balloon | **6.45s** | 7.89s |
| Rename | **7.12s** | 8.42s |
| CPUStats | **7.51s** | 9.04s |
| Archive | **92.70s** | 115.46s |

**The two engines are within 5% of each other**, and that is the result that
matters: QEMU costs essentially nothing in performance, so the backend choice
rests on capability and alignment rather than on speed — which is what
[vmm-choice.md](vmm-choice.md) argued before any of this was measured.

Two differences survive repetition. **QEMU pauses about twice as fast** (5.20s
and 6.43s against 9.57s and 22.59s), which is plausible on mechanism: `migrate`
skips zero pages and wrote 57 MB for a 1 GiB guest in the spike, where
Firecracker writes a full memory image. **Firecracker archives about 20% faster**
(92.00s/92.70s against 124.45s/115.46s). Neither has been investigated, and both
are driver questions before they are VMM questions.

### x86_64, on the CKS node

The first run of either driver on the architecture we actually serve. Same
throwaway Pod, 8 CPUs and 12 GiB cap, 2 GiB guests, node otherwise busy serving
real sandboxes.

| case | Firecracker | QEMU |
| --- | --- | --- |
| BootAndSSH | 1.68s | 1.61s |
| TwoAtOnce | 1.79s | 1.87s |
| Destroy | 2.16s | **0.98s** |
| RootfsPresence | 9.61s | **0.34s** |
| PauseResume | 13.69s | **8.16s** |
| DestroyThenReuseName | 14.12s | **7.79s** |
| Rename | 12.82s | **9.47s** |
| DiskReport | 8.36s | **6.69s** |
| DiskResize | 13.94s | **10.02s** |
| ForkFromTemplate | 29.04s | 28.17s |
| Archive | 127.21s | **97.56s** |
| CreateRefusesResidue | **11.52s** | 13.78s |
| Balloon | **7.29s** | 12.21s |
| CPUStats | **10.88s** | 12.91s |
| NetStats | 13.92s | **8.04s** |
| **Reboot** | **FAIL** | **5.10s** |
| total | 289.89s (18/19) | **224.71s (19/19)** |

**QEMU passes 19/19 here and Firecracker does not**, which is not the result
anyone ordered. Note also that `Archive` reverses from arm64: Firecracker won it
by 20% there and loses it by 23% here.

A guest boots to SSH in **1.6s** on this node, both drivers.

### The one x86_64 failure: a socket-unlink race, since fixed

`Reboot` failed on x86_64 while QEMU passed the same case on the same node. It
was a real bug in the Firecracker driver, and the way it hid is the interesting
part.

The SDK reports only what it can see:

    Firecracker did not create API socket .../fc.sock: context deadline exceeded

That is false. Firecracker created the socket and said so — but the driver threw
the VMM's stderr away, so nobody could read it. The first fix was to stop doing
that: the direct launcher now writes `firecracker.log` beside the VM, and a
failed start folds its tail into the returned error, the way the QEMU driver has
always done with `qemuLogTail`. The log then said:

    Running Firecracker v1.16.1
    Listening on API socket ("/work/.../fc.sock").
    API server started.

With a probe added at the point of failure, the socket **file did not exist** —
though Firecracker had bound it and does not unlink on exit.

The unlinker is the SDK. When a VMM exits it runs a cleanup func that does
`os.Remove(m.Cfg.SocketPath)` (`machine.go:568`, v1.0.0), on the `cmd.Wait()`
goroutine, unbounded in time. `stopMachine` waited for that reap **only on the
privileged-helper path**. A socket path is derived from the VM's name, so:

1. `Pause` SIGTERMs VMM #1 and returns; its reaper is still pending.
2. `DropSnapshots` drops the record, and the next `Create` starts VMM #2 on the
   same path. It binds, and logs `Listening`.
3. VMM #1's reaper runs and unlinks the path — VMM #2's live socket.
4. VMM #2's `waitForSocket` stats ENOENT for three seconds and blames
   Firecracker for never creating it.

The fix is one guard removed: `stopMachine` now waits for the reap on every
path, not just the helper's. **7 failures in 10 before, 10 passes in 10 after**,
and the full suite is now 19/19 on x86_64.

Three things worth keeping from it. A race gets likelier the faster the host, so
the fast production node found what the dev laptop never could — arm64 never
failed once. `Pause` + `DropSnapshots` + start is exactly a user's "reboot my
sandbox". And the shipping CKS deployment was never exposed, because it runs the
privileged helper, which happened to be the one path that already waited: the
guard protected production by accident, not by design.

### How not to measure this, learned the hard way

An earlier draft of the table above reported QEMU booting 1.8x *faster* than
Firecracker, and Firecracker winning DiskReport by 4.7x. Both were wrong, and how
they were wrong is worth more than the numbers.

**The container machine was resized from 12 GiB to 32 GiB between the two runs.**
Nothing in either runner's output recorded machine memory, so nothing in the
committed artifacts would have caught it; it surfaced only because the devpod
containers' uptime had reset. Firecracker's own total moved **315.87s → 225.12s**
for that change alone, with `BootAndSSH` going 7.55s → 2.97s. That is a larger
effect than any difference between the two VMMs, from a variable nobody was
recording.

**Three of the four largest apparent gaps evaporated on a repeat run** on the
same machine: DiskReport 43.36s → 6.80s, Rename 32.73s → 8.42s, Reboot 26.28s →
9.34s. That first QEMU run happened minutes after a machine boot, with a cold
page cache. A single run on this box is close to worthless.

So: `run-on-mac.sh` now logs total machine memory beside the free-memory and
firecracker-process-count checks it already printed, and a comparison is only
credible if both drivers ran back to back on the same machine and the gap
survived a repeat. The same caution applies to §2's memory-cliff finding, which
was measured at 12 GiB and is milder at 32.
