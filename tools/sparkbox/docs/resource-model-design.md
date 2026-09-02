# Sparkbox pooled resource model: many cheap sandboxes on one host

Status: **being built** (updated 2026-07-17). The target is exe.dev's felt
experience — a user has "up to 50" always-on machines, each up to 2 vCPU / 8 GB /
25 GB disk, ~100 GB pooled — on a single 64 GB Elastic Metal box, which is a 6×
memory overcommit that only closes because idle sandboxes cost almost nothing.

> **Course correction (2026-07-17): live overcommit, not pause-for-density.**
> This doc originally assumed a warm 8 GB VM costs 8 GB of host RAM, so density
> had to come from suspend-to-disk *pausing*. That premise is wrong, and
> exe.dev's own docs say so: "Your VMs share CPU/RAM — you pay for underlying
> resources, not per VM." Their VMs report **running**, never paused. The real
> mechanism is **live overcommit**: a Firecracker/CLH guest's RAM is lazily
> allocated anonymous memory (confirmed in our `fc.go` — no prealloc), so an idle
> guest with an 8 GB *ceiling* only faults in its small working set. KSM dedups
> the shared base image, a memory balloon + swap reclaim the rest under pressure,
> and idle vCPU threads cost ~0. So the primary density lever is **keep VMs
> running and account honestly**, not pause them. Pause/Stopped survive as
> *deeper* idle tiers (host-reboot survival, disk reclaim), not the thing that
> makes 50 fit. **Landed on this pivot:** a memory balloon on every Firecracker
> VM, a two-stage reaper (balloon-down → pause), and working-set-aware admission
> gated behind `--mem-reserve-mb` (measure with `hack/measure-density.py`, then
> set it). The sections below are being revised toward this; where they still say
> "pause is the density lever," read "balloon-down is."

## Why

We already have the hard mechanics (Firecracker microVMs, snapshot pause/resume,
resume-on-connect over both SSH and HTTP, thin CoW rootfs, RAM admission). What we
don't have is the **policy and lifecycle layer** that turns those mechanics into a
pooled quota model — and, more subtly, an honest answer to the question the whole
illusion trips over:

> If idle sandboxes are paused, what happens to the server I'm running in one?
> What about a cron job that should fire every 30 minutes?

That question is the spine of this doc. The pooled model is fundamentally
**overcommit-via-pause**, and pause has a sharp edge: a paused VM is *frozen*, so
anything that must run without an external trigger — a timer, a queue worker, a
polling loop — silently stops. Getting the density *and* not lying to users about
what "always-on" means is the actual design problem.

## The core model, stated plainly

A 64 GB host cannot hold 50 warm 8 GB VMs (that's 400 GB). It holds ~6–7. The
other ~43 are paused or stopped at any instant, and the bet — correct for coding
agents — is that a user actively touches very few at once. So every sandbox lives
somewhere on an **activity gradient**, and the platform's job is to move it up on
demand (fast) and down on idle (reclaiming RAM, then disk), while making "up on
demand" feel instant.

The wake trigger is the crux. **Servers wake on connection; timers cannot.** A
request to a sandbox's exposed port is an external event the platform can
intercept and resume on. A cron tick inside a paused VM is not — there is no
packet, no syscall, nothing for the platform to see. This asymmetry is not an
implementation gap we can close; it's physics of suspend-to-disk. So the design
handles servers and background work as **two different problems** (Parts 2 and 3).

## Constraints discovered up front (verified against our code + Firecracker)

- **Resume-on-connect already spans SSH and HTTP.** `internal/proxy/proxy.go:152`
  resumes a paused sandbox on an inbound HTTP request and marks it active so the
  reaper leaves it alone while serving; the SSH gateway does the same on connect
  (`gateway.go:199`). So "a web server in a sandbox feels always-on" is *already
  true* for HTTP(S) routes. What's missing is (a) waking on **arbitrary TCP
  ports**, not just proxied HTTP and gateway SSH, and (b) an answer for work with
  **no inbound trigger at all**.
- **A paused VM keeps its entire RAM as a snapshot on disk.** `PauseVM` →
  `CreateSnapshot(mem.snap, state.snap)` (`fc.go:281`); `mem.snap` ≈ MemMB. So
  pausing frees RAM but *spends disk* — a paused 8 GB VM is 8 GB on disk. 43 paused
  VMs = 344 GB of snapshots. **Pausing everything doesn't fit.** This forces a
  fifth state (Part 1).
- **We have exactly two states today**, `StateRunning` / `StatePaused`
  (`vmm/driver.go:11`). No "stopped" (snapshot dropped, disk only), no GC.
- **A resumed guest's wall clock is stale.** Firecracker restores the snapshot's
  clock; the guest wakes believing it's still the moment it paused. This is a
  correctness problem for us specifically: our OIDC identity tokens are 1 h TTL
  and time-sensitive — a sandbox resumed after an hour would mint/hold tokens with
  a wrong `iat/exp`. TLS validation and logs skew too. Resume must step the guest
  clock (Part 6).
- **Fixed 4 vCPU / 12288 MB per sandbox** (`host/manager.go`). ~~So RAM is
  reserved, not "up to."~~ **Corrected:** Firecracker configures guest RAM as
  lazily-allocated anonymous mmap (no prealloc/hugepages in `fc.go`), so a warm
  sandbox that touched 300 MB costs ~300 MB of host RAM, *not* 12 GB. The "12 GB"
  is a **ceiling**, and the old full-ceiling charge was purely an *admission
  accounting* choice, not a kernel fact — which is exactly why we thought only ~7
  fit. **Landed:** every VM now gets a `deflate_on_oom` memory balloon
  (`vmm.Ballooner`, `fc.go`), so we can reclaim an idle guest's RAM to the host
  without pausing it (Part 4).
- **Admission is RAM-only and ignores paused VMs** (`manager.go:225`). It never
  considers disk, never counts snapshot cost, and on a full host it *refuses* a
  resume ("pause one to free a slot") rather than evicting (Part 5).
- **No disk quota of any kind.** The 300 GB XFS volume is mounted without
  `prjquota`; a sandbox can grow its rootfs to the 64 GB ceiling and nothing caps
  per-owner or pooled usage (`cloud-init.yaml:220`). **Addressed in Part 8:** the
  per-VM ceiling is now 25 GB (the ext4 *is* that size, so it's hard), and pooled
  per-owner usage has soft admission accounting (`--disk-pool-mb-per-owner`); hard
  `prjquota` is still the future exact replacement.
- **`--max-running-per-owner` (default 2) caps running, not total.** There is no
  total-sandbox-per-owner cap.

## Part 1 — The lifecycle: five states, not two

Every sandbox moves along this gradient. Each downward step reclaims a scarcer
resource; each upward step costs latency.

| State | RAM | Disk | Wake latency | Trigger down → |
|---|---|---|---|---|
| **Active** | full (resident) | rootfs | — | no I/O for `idle-warm` (~2 min) |
| **Warm** | full (resident, ballooned) | rootfs | 0 | no I/O for `idle-pause` (~15 min) |
| **Paused** | 0 (snapshot on disk) | rootfs + `mem.snap` | ~1.7 s | idle for `idle-stop` (~2 h) |
| **Stopped** | 0 | rootfs only (snapshot dropped) | cold boot ~3–8 s | idle for `idle-delete` (~14 d) |
| **Deleted** | 0 | 0 | recreate from template | — |

The two new states are the ones that make the math close:

- **Warm** vs Active is a soft distinction (both resident) but lets us *balloon
  down* an idle-but-resident guest's RAM (Part 4) and mark it a preferred eviction
  target before we pay snapshot I/O.
- **Stopped is the load-bearing addition.** It drops `mem.snap` (reclaims MemMB of
  disk) and keeps only the thin rootfs. This is what lets a user have 50 sandboxes
  without 400 GB of snapshots — only a small, recently-used set stay Paused (fast
  resume); the long tail is Stopped (disk-cheap, cold boot). Stopped → running is
  a fresh boot from the persisted rootfs, so **in-guest process state is lost**
  (a running server must be restarted by the guest's own init/agent), whereas
  Paused → running restores the live process. That difference is the whole reason
  to keep two idle tiers instead of one.

Timers are **advisory floors, not deadlines** — the reaper walks them opportunistically and, under pressure, skips ahead (Part 5). All five thresholds are per-tier config with per-owner/plan overrides.

## Part 2 — Wake on anything, and don't pause what's busy

Two changes make "servers feel always-on" robust rather than HTTP-only:

1. **Wake on arbitrary exposed ports, not just proxied HTTP + gateway SSH.** Today
   a raw TCP connection to a sandbox's non-HTTP port doesn't resume it. Put a tiny
   host-side **wake-proxy** in front of each sandbox's published ports: on SYN to a
   paused/stopped sandbox, hold the connection, `EnsureRunning`, then splice to the
   guest. This generalizes the existing proxy.go behavior to any port and is the
   mechanism behind "my server just answers."
2. **Activity-aware idle.** Base the down-transitions on *observed activity* —
   established connections, recent bytes in/out on the tap, CPU above a floor — not
   just "no SSH session." A sandbox mid-build, or serving steady traffic, or
   holding an open connection, is never idle. This is what keeps us from pausing a
   VM out from under a long-running task. `LastActive` becomes a richer signal fed
   by the tap counters the driver already sees.

This fully covers **request-driven** workloads. It does nothing for work with no
inbound trigger — which is Part 3.

## Part 3 — Background work (the crux): schedule outside the VM

A paused/stopped sandbox runs no code, so **in-VM `cron` and systemd timers do not
fire.** `Persistent=true` timers and anacron will try to catch up on the next
resume, but batched and hours late — useless for "every 30 minutes." We do not try
to hide this. Instead we give it a real home, with three tiers of answer:

1. **Platform scheduler (the default answer).** A sandbox carries schedule
   metadata — `every 30m: run <cmd>` — owned by the control plane, not the guest.
   A host-side timer resumes the sandbox on schedule, runs the command through the
   gateway exec path, and lets it idle again. This is exactly fly.io scheduled
   machines / Cloud Run Jobs: the VM only wakes when the job is due, so it stays
   cheap *and* the job is reliable. `ctl@` gains `schedule add "*/30 * * * *" <cmd>`
   and the box's own `cron` becomes a thin shim that registers with the platform
   (or we detect crontab edits and mirror them — see open questions).
2. **Pinned / always-on tier (opt-in, metered).** The sandbox is exempt from the
   Paused/Stopped transitions (Warm floor only), so in-VM cron, daemons, and queue
   workers run normally. It costs a permanent RAM slot, so it's a bounded, paid
   capability — a user gets N pinned sandboxes, the rest are scale-to-zero. This is
   the escape hatch for "I genuinely need a persistent process."
3. **Honest contract.** The UX states plainly: a scale-to-zero sandbox freezes when
   idle; use a platform schedule for periodic work, or pin the sandbox for
   always-running daemons. No user should have to *discover* that their cron
   silently stopped — the console shows each sandbox's tier, its next scheduled
   wake, and whether it's pinned.

This is the part the naive "just pause idle VMs" model gets wrong, and it's the
part worth getting right first, because it's a correctness/trust issue, not a
density one.

## Part 4 — Quotas: total sandboxes, per-VM disk, pooled disk, burstable compute

- **Total-sandbox-per-owner cap** (the "50"). A new counter over *all* non-deleted
  sandboxes for an owner, enforced at create — distinct from
  `--max-running-per-owner`, which stays as the *concurrently-resident* cap (the
  "how many can be warm at once", governed by RAM, Part 5).
- **Per-VM disk ceiling (the "25 GB")** via **XFS project quotas**. Mount the data
  volume `-o prjquota`, assign each sandbox's rootfs+snapshot dir a project ID, set
  the hard limit. This bounds a single sandbox and is the enforcement primitive we
  currently lack entirely. The 64 GB *template* ceiling stays as the filesystem
  size; the project quota is the real, smaller cap.
- **Pooled per-owner disk (the "100 GB")** by summing project-quota usage across an
  owner's sandboxes (rootfs deltas **and** live snapshots — a paused VM's `mem.snap`
  counts against the pool, which is the honest accounting and also the pressure
  that drives Paused→Stopped). Enforced at create/resume and surfaced live.
- **Burstable compute, not fixed reservation.** exe.dev's "*up to* 2 vCPU / 8 GB"
  implies a ceiling over a smaller baseline. Two moves: (a) **✅ landed** — every
  Firecracker VM gets a `deflate_on_oom` **memory balloon** with stats polling
  (`vmm.Ballooner` + `fc.go`); the idle reaper balloons a warm guest down to the
  `--mem-reserve-mb` working-set floor (RAM returned to the host, guest still
  running), and activity deflates it. (b) **still to do** — run vCPUs under a
  **cgroup cpu.max** so 2 vCPU is a burst ceiling with fair sharing under
  contention, not a hard 2-core reservation. Both increase safe overcommit.

## Part 5 — Admission and eviction: disk- and RAM-aware, evict don't refuse

Admission today asks one question (does running RAM + this fit the budget?) and
refuses if not. The pooled model needs three changes:

1. **Two-dimensional admission.** Check the RAM budget *and* the owner's pooled
   disk quota (including the snapshot this pause would write). Starting a sandbox
   that would blow either is refused with the specific reason. **RAM dimension
   landed:** with `--mem-reserve-mb` set, admission charges the working-set floor
   per running VM, not the full ceiling (`Manager.effectiveMemMB`), so the box
   fits `ceiling ÷ reserve`× more warm — the actual density unlock. The disk
   dimension is still to do.
2. **Count the true cost of paused VMs.** Paused VMs cost 0 RAM but real disk;
   admission and the reaper must see snapshot disk as a first-class resource, or
   the "keep a Paused cache" policy silently fills the volume.
3. **Evict, don't refuse.** When a user connects to a Stopped/Paused sandbox and
   the host is at its RAM ceiling, the platform should **pause the owner's
   least-recently-active warm sandbox to make room**, then resume the requested
   one — so resume "just works" and the illusion holds. This is LRU eviction under
   resume pressure, the thing that makes 50 sandboxes feel live on a 7-warm host.
   It replaces today's "pause one yourself to free a slot" dead end. Guard it so a
   sandbox actively serving traffic (Part 2) is never the eviction victim, and so
   two resumes can't thrash the same slot (small hysteresis).

## Part 6 — Correctness on resume

- **Step the guest clock.** On every resume, signal the guest to jump its wall
  clock to now (kvm-clock adjust + a resume hook that kicks `chronyd`/
  `systemd-timesyncd`, or the paravirt clock notification). Without this, a
  sandbox resumed after an hour holds a stale clock — and our own **OIDC tokens are
  1 h TTL and time-sensitive** (`internal/oidc`), so token minting and any TLS the
  guest does would be wrong until the next NTP tick. This is a real, us-specific
  bug the current two-state pause hasn't surfaced only because resume is usually
  quick.
- **Re-establish network identity on resume.** The driver already re-adds the tap
  and NDP proxy on resume; the wake-proxy (Part 2) must not forward until the guest
  is actually reachable, or the first request after a cold resume races the guest's
  sshd/HTTP server coming back (Stopped→running especially, since that's a full
  boot and the in-guest server must restart itself).

## Part 7 — When one host isn't enough

Even perfect overcommit caps out: a 64 GB box holds ~7 warm 8 GB sandboxes, so the
active *working set* across all users, not the total sandbox count, is the ceiling.
The pooled model per single host works while each user's simultaneously-active set
is small; a busy fleet needs the **multi-box path** (already in the backlog):

- **Per-owner host pinning** first — an owner's sandboxes live on one host, so
  quota accounting, the pooled disk volume, and eviction stay node-local and
  simple. A control plane assigns owners to hosts and rebalances by capacity.
- **Sandbox migration** later — Stopped sandboxes are just a rootfs, so moving one
  is a disk copy; that's the cheap lever for rebalancing without live migration.
  Live (Paused) migration (ship `mem.snap` + rootfs) is a phase-3 luxury.

Cross-host pooled quota and a real scheduler are out of scope here; this doc is the
single-host model plus the pinning seam that keeps multi-box from requiring a
rewrite.

## Part 8 — Disk lifecycle: 25 GB VMs, pooled quota, archive & snapshot (✅ landed)

The activity gradient (Parts 1, 4-5) reclaims RAM then, at the Stopped tier, disk.
This part makes the disk story concrete and adds two levers beyond it: parking a
cold VM's disk **off-host** entirely, and reusing a customized VM as a **template**.
Every fork here took the simpler option deliberately (see the git history / the
plan that shipped it).

- **25 GB per VM, hard.** The rootfs ext4 template is now built at 25 GiB
  (`ROOTFS_MB=25600` in `hack/stage-artifacts.sh` + `hack/setup-host.sh`), matching
  exe.dev. Because the guest's filesystem *is* that size, it physically cannot
  exceed it — no quota needed for the per-VM cap. Thin XFS reflink copies mean a
  sandbox still only pays for blocks it writes. Changing the size busts the
  content-addressed rootfs cache key, so the next release rebuilds the template.

- **Pooled per-owner disk, soft.** `--disk-pool-mb-per-owner` (0 = off) caps the
  sum of an owner's on-disk footprints. Accounting is *soft*, mirroring RAM
  admission: the driver reports each VM's `du` (`vmm.DiskReporter.DiskUsageMB`),
  the reaper refreshes `Sandbox.DiskMB` each tick, and `admit` refuses a create/
  restore that would push the owner over (`DiskQuotaError`). Archived boxes count
  their compressed object-storage size. Hard XFS `prjquota` (Part 4) remains the
  exact future replacement.

- **The pooled sum is baseline-subtracted.** The paragraph above used to end
  "conservative (reflink-shared base blocks are counted per VM), never under" —
  it no longer does, and the difference is the point. `DiskUsageMB` reads the
  guest's own ext4 counters, which know nothing of reflink, so ten sandboxes
  forked from one 8 GB image each reported ~8 GB while the host held a single
  copy: a pool was worth roughly one image however the operator sized it, and
  every incentive ran against the sharing the design is built on. The manager now
  also measures the template named by `Sandbox.Image` through
  `vmm.TemplateReporter.TemplateUsageMB` — the *same* ext4 superblock read, which
  is what makes the two subtractable — stores it as `Sandbox.BaseDiskMB`, and
  charges the pool `max(0, DiskMB - BaseDiskMB)`. An owner pays for blocks their
  sandboxes wrote. Notes:
  - The per-VM 25 GB ceiling, both consoles' meters, the REST projection and
    every per-sandbox figure a user *sees* stay **raw**: that is the number they
    can reconcile against `df` inside the guest. Only the pooled sums move.
    `BaseDiskMB` itself does now ride the node link (see Part 10) so a gateway
    can compute those sums for sandboxes it does not run; nothing renders it.
  - **This enlarges every existing `--disk-pool-mb-per-owner` setting** the moment
    the first reaper tick backfills baselines. It only ever loosens — it can never
    refuse a create that used to succeed — but it is a live policy change on a
    binary swap with no flag, so re-check the number against what you meant.
  - An **archived** box pays full freight: `Archive` replaces `DiskMB` with the
    compressed artifact's size, and object storage dedups nothing against a local
    template. Delta archives are still the deferred Part 7.
  - The baseline is re-measured each tick rather than stamped at create time,
    because the agent-tools refresher replaces base templates by atomic rename —
    a create-time figure would describe an image that no longer exists. A
    measurement error (a deleted snapshot, an unreadable image) **keeps** the
    stored value; treating it as zero would spike every fork's charge by a whole
    template with nobody having written a byte.
  - A restore or checkpoint-restore clears the baseline, because `UnpackRootfs`
    writes a full image that shares no extents. The next tick may hand the
    discount back anyway — a bounded over-credit in the user's favour on a soft
    knob, deliberately preferred over a persisted "was cloned" bit that create,
    both restore paths and `resumeOrRecreate` would each have to maintain.

- **Archive → object storage (a 6th state below Stopped).** `ctl@ archive <name>`
  / the console Archive button / `POST …/archive` pause the VM, drop its memory
  snapshot, **`e2fsck` + `zerofree` + `zstd`** the rootfs (the "fschk/compaction"
  step — zeroed free space compresses away, so the artifact is ~the used size),
  upload it to `<prefix>/<owner>/<name>.ext4.zst` (private ACL), and destroy the
  local VM. The record survives in the new **`StateArchived`**, costing **zero host
  RAM and zero host disk**. Resume-on-connect restores transparently: `EnsureRunning`
  downloads + unpacks the rootfs, flips the box to Paused, and the existing
  cold-boot-from-present-rootfs path (`fc.go` Create skips its reflink when a
  rootfs is already there) brings it up — no new boot path. Object storage lives
  above the driver (`host.ObjectStore` → `internal/objstore`, an rclone wrapper
  reusing the release-artifact conventions); the driver only ever deals in local
  files (`vmm.Archivable.PackRootfs`/`UnpackRootfs`). Archive is a *move*: restore
  deletes the object, a later re-archive rewrites it, and Destroy drops it.
  **Deploy prereq:** the serve host needs `e2fsprogs`/`zerofree`/`zstd` (added to
  setup-host + cloud-init) and, unlike the public-read release bucket that CI owns,
  **S3 write creds in its rclone.conf** — archives are private user data.

- **Snapshot → fork (a reusable template).** `ctl@ snapshot create <box> <name>`
  captures a customized VM as a fork-able template: the driver compacts and
  **sanitizes** the rootfs (strips SSH host keys, `machine-id`, hostname, cached
  identity token — so every fork regenerates its own identity, exactly what
  `build-rootfs.sh` does at image-build time) into a new `<image-dir>/snap-<owner>-
  <name>.ext4`. `ctl@ fork <snap> <newname>` (or the console) then creates a
  sandbox with that template as its image, reusing the reflink `--image` path with
  zero new mechanism. Snapshots are owner-scoped in a small `snapshots.json`
  registry; forks already made keep their own reflink copy, so deleting a snapshot
  never affects them. v1 keeps snapshots **local** to the host (no object-storage
  backup, no OCI export) — both are follow-ups.

Archive stays **manual** in v1 (no auto-archive reaper tier yet); the natural next
step is driving Paused→Stopped→Archived on idle + disk pressure, which slots onto
this same machinery.

## Part 9 — Turbo: double the machine, for one run (✅ landed)

The other half of "burstable compute" (Part 4), taken from the front of an 80s
desktop: a latching **turbo** switch in the user console's machine card and in
the browser terminal's header that restarts a sandbox with **2× its vCPUs and
2× its RAM**, and hands them back at the next pause.

- **It is a cold boot, and it cannot be anything else.** Firecracker has no CPU
  hotplug, and the balloon can only *return* memory to the host — it can never
  borrow more than the VM was configured with. So `Manager.SetTurbo` is the
  Part 6 pause → **drop the memory snapshot** → cold-boot dance that `Reboot`
  and `Resize` already use, with the new size written between the drop and the
  boot. Both UIs say so before they ask.
- **The doubling lives in `VCPUs`/`MemMB`,** with the sandbox's own figures held
  aside in `BaseVCPUs`/`BaseMemMB`. Nothing downstream had to learn about turbo:
  admission (Part 5), the balloon target, the `vmm.Config` the cold boot is
  built from, and every meter's denominator all keep reading the one pair of
  fields that has always meant "what this VM has".
- **One run, and `pause` is the single place that ends it.** Every path that
  stops a guest goes through `Manager.pause`, so an idle reap, an explicit
  pause, a reboot and a rename all release the allocation — one place that has
  to remember instead of four that have to agree. The snapshot goes with it: a
  guest's shape is baked into its memory image, so a 2×-sized snapshot resumed
  under a 1× record would be a silent overcommit.
- **Admission runs before anything is torn down.** A host that cannot afford the
  doubled boot refuses with the sandbox still running at its own size, rather
  than parking it with an apology. If the cold boot itself fails, the record is
  reverted so the next (possibly automatic) resume asks for an allocation this
  host has already served.
- **Fleet-wide**, over both control transports (`NodeControl.BeginSetTurbo` and
  the legacy `sandbox.turbo` frame). The multiplier deliberately does not cross
  the wire — the machine that allocates decides what turbo means, so a gateway
  and a node on different releases cannot disagree.

**Owner-pool integration landed.** A normal VM is charged one effective working
set (`--mem-reserve-mb`) to the owner's baseline pool. Turbo is charged twice
that amount and may cross the baseline only up to
`--owner-memory-burst-mb`. The burst ceiling is borrowed capacity, not a
reservation: every owner's baseline and burst pools overlap on the node, while
the node-wide effective-memory budget remains the final admission authority.
This fixes the subtle overcommit bug where an 8 GiB VM and its 16 GiB turbo
form both collapsed to the same reserve charge and turbo was effectively free.

A ten-second pressure controller closes the gap between statistical admission
and reality. It samples Firecracker balloon working-set statistics, compares
actual resident use with owner baseline/burst ceilings and the node budget, and
balloons cold unpinned VMs without pausing them. Borrowed turbo allocations are
reclaimed before ordinary LRU candidates. Node capacity reports effective,
resident, and entitled memory separately so an operator can see both the real
load and the intentional owner-pool overcommit ratio.

Still open: a time-based turbo lease, a per-owner simultaneous-turbo policy,
and cgroup CPU enforcement. Today turbo lasts for one run and naturally returns
on pause, while the host kernel time-slices its doubled vCPU threads.

## Part 10 — The footprint card: showing an owner what they actually cost (✅ landed)

Part 8 made the *host* charge honestly. This makes the honesty visible to the
person being charged, on the Machines tab of the user console (`my.<domain>`), in
one small three-tile card above the machine list.

The premise is that every number a sandbox owner had until now was the
provisioned one. Six machines at 25 GB read as 150 GB of disk and five at 16 GB
read as 80 GB of RAM, when the reflinked disks hold a few gigabytes between them
and the balloon has handed most of that RAM back. Those figures make the product
look expensive in exactly the dimension it is cheap.

- **Disk** — the pooled, baseline-subtracted charge over the owner's disk pool,
  with the raw sum and the sharing dividend underneath: *"24 GB across 3 disks ·
  4.0× smaller than copies · 18 GB of template shared"*.
- **Memory** — the live balloon reading over the provisioned ceiling, with the
  overcommit ratio and the pool charge underneath. The numerator is the same
  `Vitals` read the per-machine rows already make, routed to the machine holding
  each VM; the ceiling and the pool come from the rollup.
- **CPU** — cores busy over vCPUs held, derived in the browser from the same
  cumulative `cpu_seconds` delta the per-machine meter uses. It is weighted back
  up by each machine's own vCPUs before summing, because a percentage of a
  2-vCPU guest and a percentage of an 8-vCPU guest are not addable.

Three pieces of mechanism, and only the first is new policy:

1. **`BaseDiskMB` now crosses the node link** (`base_disk_mb`, `node.proto`
   field 23, plus the `nodelink.SandboxRow` JSON form). It used to be
   deliberately node-local, which was correct while only the owning node charged
   the pool; the moment a *gateway* computes an owner's disk it has to know which
   blocks are shared, or it charges every remote fork for a copy that was never
   made. `refreshDiskUsage` therefore also wakes the observer when only the
   baseline moves, which it did not before. A node too old to send it, or one
   whose driver cannot measure templates, sends 0 — which reads as "charge this
   one raw", exactly the pre-Part-8 behaviour, so the card degrades to showing no
   dividend rather than a wrong one.
2. **`host.RollUpOwner` + `OwnerCapacity.Merge`** are the arithmetic, extracted
   from `Manager.CapacityForOwner` so that a gateway folding records for VMs it
   does not run uses the same code rather than a second copy of the policy. Each
   machine's sandboxes are charged against *that machine's* `OwnerPolicy`, read
   from the capacity report it already publishes — which matters because on the
   Kubernetes deployment the owner pools are declared on the node Deployment and
   the gateway sets none at all. Merge **adds usage and does not add pools**: an
   owner who straddles two machines has one entitlement, not two.
3. **`GET /api/usage`** on the user console returns that rollup for the session's
   own handle. It is a second request rather than a field on `/api/machines`
   because the two answer different questions — the list is per-sandbox and
   carries the live readings, this is the pooled arithmetic no browser can do —
   and because a failure to fetch it hides the card without making the list stale.

No remote call is added: every input is already cached (the inventory nodes
stream, and the capacity report the link refreshes in the background), which is
what lets a console poll it every few seconds.

## What we already have (so this is mostly policy, not new mechanics)

- Snapshot pause/resume, resume-on-connect over SSH **and** HTTP, thin CoW rootfs,
  a per-minute reaper, RAM admission, `ctl@ pause`, and a 4 vCPU / 12 GB default
  allocation (8 vCPU / 24 GB for a Turbo run). The Firecracker driver, tap/NDP
  lifecycle, and state persistence (`sandboxes.json`) are all in place.
- The genuinely new engineering is: the Stopped state + snapshot GC, XFS project
  quotas + pooled accounting, the platform scheduler, wake-on-arbitrary-port,
  balloon + cpu.max, LRU eviction, and the resume clock-step.

## Rollout

1. **M1 — honesty + the crux. ✅ landed.** Platform scheduler (`ctl@ schedule`) and
   the pinned tier (`ctl@ pin`/`unpin`, reaper-exempt, resumed on boot), plus the
   console surfacing each sandbox's tier (pinned badge) and next scheduled wake.
   This fixes the trust problem (cron) before we chase density, and needs no new
   VM states. Schedules live in their own sqlite store; a host-side loop resumes
   the sandbox when a job is due, runs it through the gateway exec path, and lets
   it idle again.
2. **M2 — the Stopped tier + snapshot GC.** Add Stopped, drive Paused→Stopped→
   Deleted from the reaper on idle + disk pressure. This is what makes "50
   sandboxes" physically fit.
3. **M3 — quotas.** XFS project quotas (per-VM), pooled per-owner accounting,
   total-sandbox cap, two-dimensional admission.
4. **M4 — density + seamlessness.** **Memory balloon + working-set admission
   landed early** (`--mem-reserve-mb`, two-stage reaper) — this is the real
   density lever now that we know pause isn't. Still to do: KSM host tuning
   (measure with `hack/measure-density.py --ksm` first), cgroup cpu.max,
   wake-on-any-port, LRU eviction under resume pressure, resume clock-step.
5. **M5 — multi-box.** Per-owner host pinning + a capacity-aware assignment control
   plane; Stopped-sandbox migration for rebalancing.

## Open questions

- **Do we mirror in-VM crontabs automatically, or require `ctl@ schedule`?**
  Auto-detecting crontab/systemd-timer edits inside the guest and mirroring them to
  the platform scheduler is magical and meets users where they are, but it's
  fragile (parsing, drift, surprise wakes). Explicit `schedule` is honest and
  simple. Likely: explicit first, detection as a later convenience that *warns*
  "this timer won't fire while your sandbox is asleep — register it?"
- **Balloon vs. just more swap.** A balloon reclaims guest RAM cleanly; host swap
  is simpler but thrashes under real pressure. Measure before committing.
- **Snapshot cache sizing.** How many Paused (fast-resume) VMs per owner before we
  force Stopped? A function of the disk pool and observed resume patterns — tune
  empirically.
- **Stopped→running restart contract.** A Stopped sandbox cold-boots, so in-guest
  servers must be brought back by the guest's init/agent. Do we standardize a
  "sandbox boot manifest" (declare the processes/ports to start) so a resumed-from-
  cold server comes back without the user re-running it? This is arguably the
  cleaner long-term model than snapshot-restoring live processes at all.
- **Does clock-step break anything mid-flight** (TLS handshakes, DB connections in
  the guest) when time jumps forward on resume? Probably fine (time monotonically
  advances) but worth testing with the agent CLIs.
