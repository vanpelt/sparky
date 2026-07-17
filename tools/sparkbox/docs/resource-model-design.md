# Sparkbox pooled resource model: many cheap sandboxes on one host

Status: **design** (2026-07-17). Nothing here is built yet. The target is
exe.dev's felt experience — a user has "up to 50" always-on machines, each up to
2 vCPU / 8 GB / 25 GB disk, ~100 GB pooled — on a single 64 GB Elastic Metal box,
which is a 6× memory overcommit that only closes because idle sandboxes cost
almost nothing.

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
- **Fixed 2 vCPU / 8192 MB per sandbox** (`host/manager.go:28`), enforced as a
  hard Firecracker reservation with **no balloon device** (`fc.go:11`). So RAM is
  reserved, not "up to" — a warm sandbox costs its full 8 GB even if the guest
  uses 200 MB. Ballooning (Part 4) is what would let us keep more warm.
- **Admission is RAM-only and ignores paused VMs** (`manager.go:225`). It never
  considers disk, never counts snapshot cost, and on a full host it *refuses* a
  resume ("pause one to free a slot") rather than evicting (Part 5).
- **No disk quota of any kind.** The 300 GB XFS volume is mounted without
  `prjquota`; a sandbox can grow its rootfs to the 64 GB ceiling and nothing caps
  per-owner or pooled usage (`cloud-init.yaml:220`).
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
  implies a ceiling over a smaller baseline. Two moves: (a) give Firecracker a
  **memory balloon** so an idle-but-warm guest returns unused RAM to the host,
  letting us keep several more warm within the 64 GB budget; (b) run vCPUs under a
  **cgroup cpu.max** so 2 vCPU is a burst ceiling with fair sharing under
  contention, not a hard 2-core reservation. Both increase safe overcommit; both
  are additive to the reservation model we have today.

## Part 5 — Admission and eviction: disk- and RAM-aware, evict don't refuse

Admission today asks one question (does running RAM + this fit the budget?) and
refuses if not. The pooled model needs three changes:

1. **Two-dimensional admission.** Check the RAM budget *and* the owner's pooled
   disk quota (including the snapshot this pause would write). Starting a sandbox
   that would blow either is refused with the specific reason.
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

## What we already have (so this is mostly policy, not new mechanics)

- Snapshot pause/resume, resume-on-connect over SSH **and** HTTP, thin CoW rootfs,
  a per-minute reaper, RAM admission, `ctl@ pause`, fixed 2 vCPU / 8 GB matching
  exe.dev's ceiling. The Firecracker driver, tap/NDP lifecycle, and state
  persistence (`sandboxes.json`) are all in place.
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
4. **M4 — density + seamlessness.** Memory balloon, cgroup cpu.max, wake-on-any-
   port, LRU eviction under resume pressure, resume clock-step.
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
