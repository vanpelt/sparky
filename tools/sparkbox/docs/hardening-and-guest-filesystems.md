# VM lifetime, VMM confinement, and guest filesystems

Status: findings and options; nothing here is scheduled
Date: 2026-08-23
Origin: [`smolvm-evaluation.md`](smolvm-evaluation.md), borrows 3 and 4, plus the
guest-filesystem question deferred out of
[`oci-rootfs-and-durable-volumes-design.md`](oci-rootfs-and-durable-volumes-design.md)

The two borrows that are being implemented have a design doc and a work plan.
This is the rest of the evaluation: the ideas that were worth writing down but
not worth scheduling ahead of the OCI work.

One of them turned out to be a live bug rather than an idea. That section is
first.

---

# 1. A control-plane restart destroys every running sandbox

## What actually happens today

Verified by reading the code, not inferred:

- `cmd/sparkbox/main.go:396` has `defer driver.Close()`.
- `firecracker.Driver.Close()` walks every VM and calls `stopVMM` →
  `machine.StopVMM()`. That is a hard stop of the VMM process. **No pause, no
  snapshot.**
- So an ordinary `systemctl restart sparkbox`, a redeploy, or any SIGTERM
  deliberately hard-stops every running VMM on the host.

Paused sandboxes are fine — their memory snapshot is already on disk, which is
what pause means. **Running** sandboxes lose guest RAM outright, and their rootfs
was never cleanly unmounted, so the next boot is a cold boot of a filesystem that
wants an `e2fsck`. For an agent halfway through a task, the task is gone.

There are two further layers underneath that, and each would independently
prevent a fix at the layer above it:

- **`deploy/sparkbox.service` sets no `KillMode`**, so it gets systemd's default
  `control-group`. Even if `Close()` left the VMMs alone, they would be
  reparented to PID 1 on exit and then SIGKILLed as members of the service's
  cgroup. This is the exact failure smolvm documents in
  [`lossless-serve-restart.md`](https://github.com/smol-machines/smolvm/blob/main/docs/lossless-serve-restart.md),
  including their observation that the service cgroup then fails to recreate.
- **There is no reattach path.** Nothing in the driver or the manager looks for
  an already-running VMM at startup. A surviving VMM would be an orphan its own
  control plane could not talk to.

`Restart=always` is in the unit, so a crash does all of this unattended.

On CKS the same chain applies with a shorter fuse: the guests are children of
the controller container, so an OOM kill, a failed liveness probe, or an ordinary
rollout takes every sandbox on the node with it.

## The fix is not smolvm's fix

smolvm moves each VM into its own `smolvm-vm-<id>.scope` owned by PID 1, so the
VMs survive and a restarted `serve` reattaches over `agent.sock`. That is the
right design *for them*, because libkrun has no productised suspend: keeping the
VM alive is the only way they can keep its state.

**Sparkbox has memory snapshots, and pause/resume is already the product.** So
the cheap fix is not to keep VMs running across a restart — it is to *pause* them
into it:

> On SIGTERM, pause every running sandbox before exiting. On startup they are
> paused, and resume-on-connect — which already exists, and which the idle reaper
> already exercises constantly — brings them back on the next SSH or HTTP
> connection.

That reuses machinery that is already load-bearing, needs no cgroup surgery, no
reattach protocol, and no new failure mode. It is strictly better than copying
the scope design, and it is the same lesson as the rclone/FUSE call below:
copying the implementation would have missed the idea.

Its limits are real and should be stated:

- **It only covers graceful shutdown.** A SIGKILL, an OOM kill, or a hard crash
  still loses running guests. Covering *those* does need the VMs to outlive the
  control plane, which means scopes (or, on CKS, a separate container or a
  node-level runtime) plus a reattach path.
- **It is not free.** Pausing N sandboxes writes N memory snapshots. A host
  running 40 warm sandboxes could take a while, and systemd's stop timeout and
  Kubernetes' `terminationGracePeriodSeconds` both have to be raised to match, or
  the pause gets SIGKILLed halfway and we are back where we started with extra
  steps.

Suggested order, if this gets picked up:

1. **Pause-on-SIGTERM**, with a bounded budget and a raised stop timeout. Covers
   redeploys, which is where the pain actually is.
2. **`KillMode=mixed`** in the unit, so systemd signals only the main process.
   Harmless on its own and a precondition for anything further.
3. **Scopes plus reattach**, only if crash survival turns out to matter. This is
   the expensive one and it is the one smolvm actually built.

Step 1 is small, self-contained, and worth doing regardless of whether 2 and 3
ever happen.

---

# 2. Confining the VMM: Landlock

## Where we already are

Better than the evaluation implied, and worth being precise about because the
borrow is narrower than "add seccomp and Landlock".

Firecracker **ships its own seccomp filter**. smolvm had to build one
(`seccompiler`, applied to the boot subprocess) specifically because libkrun
ships none — their own source comment says so. We get that for free, so that
half of the borrow does not apply to us.

`security-hardening.md` documents what else is in place: a per-VM chroot, a
per-VM UID of `100000 + slot`, an empty environment, no capabilities on the VMM,
and `RuntimeDefault` seccomp/AppArmor on the CKS containers.

## What is missing

**Landlock.** There is no Landlock anywhere in the Go tree — the only match in
the repository is a comment I wrote in `internal/ext4`. A Landlock ruleset
pinning the VMM to exactly its own VM directory is a second, independent lock on
the same door as the chroot: the chroot is a namespace boundary that root can
historically escape in creative ways, whereas Landlock is a kernel LSM applied to
the process and inherited by its children, and it does not care whether the
process is root.

It is additive, it needs no privilege to *apply* (an unprivileged process can
restrict itself further), and it composes with everything already there.

**The most valuable target is not Firecracker.** It is `internal/vmhelper` — the
root sidecar that owns TAP creation, device nodes, file ownership, and killing
per-VM UID children. It is the component with capabilities, it speaks a Unix
socket authenticated by `SO_PEERCRED`, and its protocol is already deliberately
path-free. A Landlock ruleset over the small set of directories it legitimately
touches would turn "path-free by convention" into "path-free by enforcement".

## Related, and cheaper

`security-hardening.md`'s remaining-work item 3 — replacing `RuntimeDefault` on
the helper with a measured syscall allowlist — is the same shape of work and
probably wants doing in the same pass.

Separately: R5 in
[`oci-rootfs-remaining-work.md`](oci-rootfs-remaining-work.md) removes the one
remaining `seccompProfile: Unconfined` in the CKS manifests, which is on the
template-preparation init container. That is a hardening win that falls out of
the OCI work rather than needing its own project.

---

# 3. A guest filesystem of our own

## Why this is open at all

[`oci-rootfs-and-durable-volumes-design.md`](oci-rootfs-and-durable-volumes-design.md)
Part 2 chose rclone for the guest-side S3 mount, and corrected the evaluation's
praise for smolvm's self-contained `smolvm-s3fs`: its "no libfuse, no
`fusermount3`, works in distroless" property is worth nothing to us, because our
guest is a full Ubuntu with systemd. That reasoning stands.

But one fact makes the choice less settled than it sounds: **the guest image
today has neither rclone nor FUSE in it.** Both are additions. rclone is the
smaller change by far, not the free one.

## When writing our own would be justified

Concrete triggers, so this gets revisited on evidence rather than on taste:

- **Write semantics.** `rclone mount`'s VFS cache is tuned for file-at-a-time
  workflows. An agent doing many small writes — a git checkout, a build tree, an
  editor's atomic-rename saves — is close to rclone's worst case, and the failure
  mode is silent slowness rather than an error. If a durable volume turns out to
  be somewhere people actually work rather than somewhere they park artifacts,
  measure this first.
- **A filesystem that is not object storage.** The stronger reason. A
  sparkbox-specific FUSE could expose things object storage cannot: the metadata
  service as a filesystem, a write-through workspace whose upper layer is
  node-local and lower layer durable, or a read-only view of another sandbox.
  None of that is reachable by pointing rclone at a bucket.
- **Removing a moving part from the guest.** rclone is a large third-party binary
  with network access running inside the sandbox. That is not a new trust
  boundary — the guest is already hostile by assumption — but it is one more
  thing to keep current, and `deploy/install-rclone.sh` exists because a stale
  rclone silently double-uploaded to R2.

## What it would cost

smolvm's version is a self-contained FUSE server plus a SigV4 S3 client with no
async runtime, no libfuse, and no external binary. The Go equivalent is roughly
the same shape and probably 1,000–1,500 lines against `go-fuse` or `bazil/fuse`,
plus the S3 signing we would otherwise get from an SDK.

Their implementation is Apache-2.0, so it is readable as a reference. It is not
portable — it is Rust and it is theirs — so what would carry over is the design,
not the code.

**Recommendation: do not start here.** Ship the volumes with rclone, find out
whether anyone hits the write-semantics wall, and treat "a filesystem that is not
object storage" as the thing that would actually justify the project.

---

# 4. Not scheduled, worth remembering

Four ideas from the evaluation that do not need their own section yet.

**Fork pools with CoW guest RAM.** smolvm freezes a golden VM and CoW-forks
workers from it, with leases and a reconciling pool controller. Sparkbox's
`Snapshot` produces a sanitized template that *cold-boots*, which is a different
and much slower thing. Firecracker supports snapshot-restore fan-out, so the idea
ports even though the code does not — several sandboxes restoring from one
snapshot, sharing page-cache-backed memory. This is what "turbo" wants to grow
into, and it is the most product-visible item on this page: it is the difference
between `ssh new@` taking a second and taking a moment.

The catch is the same one that makes template snapshots hard: a forked guest must
not inherit the golden's identity. smolvm handles it in their agent; we would
need the same sanitization the snapshot path needs, which ties this to
`security-hardening.md`'s remaining-work item 2.

**p2p blob distribution and a prewarm endpoint.** smolvm's `serve` exposes
`/p2p/blob/{digest}` and `/artifacts/warm` so nodes fetch layers from each other
rather than all pulling the registry. Irrelevant on one host; interesting once a
fleet of nodes all pull the same rootfs image after an R3 bump. Worth
remembering, not worth building before the fleet is big enough to notice.

**`RuntimeClass.overhead.podFixed` as a modelling idea.** Even without adopting
a RuntimeClass, smolvm declaring a per-pod VM overhead to the scheduler points at
something we cannot currently express at all: the host-side cost of a sandbox
beyond its guest RAM. The per-owner resource pools that landed recently account
for what the guest is given; they do not account for what the VMM, the tap, and
the snapshot cost the host.

**`critest` as a yardstick.** If the "should a sandbox be a Pod?" spike in the
evaluation ever runs, "82 passed / 7 failed / 24 skipped, and here are the 7 and
why they are structural to every VM-based runtime" is exactly the right way to
report the result — a number with an argument attached, rather than a verdict.

---

# 5. One note from the implementation so far

While landing the OCI work, CI caught a bug that this container could not
reproduce: `os.RemoveAll` cannot delete an unpacked rootfs, because unlinking an
entry needs write permission on its *parent* and `0555` directories are ordinary
under `/usr/share`. Running as root bypasses the check entirely, so it looked
fine locally and failed as soon as a normal user ran it.

That is worth carrying into R7 (rootless unpack) specifically, and into any
hardening work generally. **Every reduction in privilege will surface code that
silently depended on having it**, and the failure will not look like a permission
error at the point where the assumption was made. The cheap countermeasure is
already in place: this container now has a non-root user set up to run the suite
the way CI does. Use it before claiming a privilege reduction works.
