# OCI rootfs templates and durable guest volumes

Status: design; three slices landed, see
[`oci-rootfs-remaining-work.md`](oci-rootfs-remaining-work.md)
Date: 2026-08-23 (revised the same day, after the CKS work merged to main)
Companion to [`smolvm-evaluation.md`](smolvm-evaluation.md), which is where
these two ideas came from. This document makes the design decisions; it does not
restate the evaluation's reasoning for *why* these two came first.

Two independent changes that happen to share a root cause: Sparkbox's rootfs is
a monolithic build artifact, and a sandbox's work only exists on the node it
booted on.

---

# Part 1 — OCI images as the rootfs format

## Where we are

```
CI      docker build images/Dockerfile
        → docker export → loop-mount a 25 GiB ext4 → bake gateway key + hooks
        → zstd → GitHub Release asset  (universal-<arch>.ext4.zst, under 2 GiB)
host    sparkbox setup downloads + decompresses → data/images/universal.ext4
        → refresh-agent-tools.sh loop-mounts and re-patches agent CLIs on a timer
create  reflink copy universal.ext4 → <vm>/rootfs.ext4
```

Four things are wrong with that, and they are all the same thing:

- **The unit of change is the whole 750 MB payload.** Bumping `claude` by a
  patch release means rebuilding and republishing everything, so we don't — we
  invented `refresh-agent-tools.sh` to avoid it.
- **`refresh-agent-tools.sh` is a second, divergent rootfs build.** It re-derives
  the login user, re-installs the identity payload, and carries its own staleness
  protocol (`/etc/sparkbox/tools-rev`, read with `debugfs`) because the host-side
  stamp it used to trust was demonstrably wrong on the DGX. Its header comment is
  a 30-line apology. Every guest-side change now has to be written twice, once in
  `build-rootfs.sh` and once here, and `install-guest-identity.sh` exists to stop
  those two from drifting.
- **Existing sandbox disks are never rewritten**, so a sandbox's tool versions
  are a function of when it was created. There is no way to reason about that.
- **It needs `CAP_SYS_ADMIN` and a loop device.** On CKS this is a whole
  `sparkbox.dev/loop` device-plugin resource and an init container with
  `SYS_ADMIN` + `AppArmor: Unconfined` + `seccomp: Unconfined`, just to build a
  filesystem. [`security-hardening.md`](security-hardening.md) lists removing
  that loop bundle as remaining work item 1, conditioned on "trusted-template
  tooling mov[ing] into the image build or a disposable helper microVM". This
  design is the first of those two.

## Where we're going

```
CI      docker build → push ghcr.io/vanpelt/sparkbox-rootfs:<tag>
host    sparkbox pulls by digest (no daemon) → flatten layers → apply the guest
        overlay → mke2fs -d → data/images/oci/<digest>-<overlay>.ext4  (cached)
create  reflink copy → <vm>/rootfs.ext4                        (unchanged)
```

### Decision: build the ext4 without a loop mount

`mke2fs -d <dir>` populates a filesystem from a directory tree at creation time.
No mount, no loop device, no `CAP_SYS_ADMIN`. Verified locally on e2fsprogs
1.47.0: ownership (uid/gid), modes including `0600`, and exec bits all survive
into the image.

This is the single highest-leverage line in the design. It deletes:

- the `sparkbox.dev/loop` device-plugin resource and its allocation logic;
- `SYS_ADMIN` from the CKS `prepare-vm-assets` init container;
- the loop-mount/umount failure modes in both `build-rootfs.sh` and
  `refresh-agent-tools.sh`.

It also lands on the right side of a rule main adopted while this was being
written. `refresh-agent-tools.sh` now refuses to touch user-derived
`snap-*.ext4` templates at all, because "mounting an untrusted guest filesystem
asks the privileged host kernel to parse attacker-controlled ext4 metadata and
turns the management plane into a second sandbox boundary." A build that never
mounts anything is not subject to that rule, which is worth more than the
capability arithmetic: it means the same tooling can eventually be pointed at a
guest-derived image, which the mounting path can never be.

`ext4.ReadFile` shells out to `debugfs` for the same reason. debugfs parses the
same attacker-controlled metadata, but in *userspace* — a bug there is a crashed
helper process, not a compromised host kernel. That is why reading a template's
stamp this way was already acceptable on main and mounting it was not.

The unpack still needs `CHOWN`, `FOWNER`, `DAC_OVERRIDE`, and `MKNOD` to
reproduce a container image's ownership and device nodes faithfully — so this is
"root without `SYS_ADMIN`", not "unprivileged". Going fully rootless means
unpacking inside a user namespace (the standard rootless-container trick); that
is a follow-up, not this slice, and it is worth doing separately because it is
the difference between `runAsUser: 0` and `runAsNonRoot: true` on CKS.

### Decision: layer the fast-moving tools last, and let OCI do the diffing

The reason agent CLIs were pulled *out* of the image is that a rebuild is a
~65-minute CI affair. That doesn't change. What changes is that a rebuild is no
longer the unit of *distribution*.

Put `claude`, `codex`, `pi`, `hivemind`, and `agent-browser` in a final thin
layer over the expensive base. Bumping them rebuilds one layer in seconds, and a
host that already has the base pulls only that layer — tens of MB, not 750. That
is the whole argument for OCI here, and it is what retires `refresh-agent-tools.sh`
rather than merely relocating it.

The corollary matters too: a template is now identified by a **digest**, so
"which tools does this sandbox have" has an answer that is a fact about an
immutable artifact rather than an inference from a stamp file.

### Decision: digest-addressed cache, keyed by more than the digest

The overlay bakes host-specific bytes into the template — the gateway's public
key, and the guest hook payload. Two hosts with the same image do not have the
same template. So the cache key is:

```
<image-digest> + <overlay-revision> + <sha256(gateway pubkey)> + <size-mb>
```

Materialization is atomic in the same style as `firecracker.Snapshot`: build to
`.<key>.ext4.tmp`, `rename` into place. A concurrent create sees the old
template or the new one, never a torn one.

Main reached the same conclusion independently while this was being written:
`refresh-agent-tools.sh` now folds `gateway_key=<sha256>` into the stamp it
compares against, for exactly this reason. The difference is where the fact
lives — there it is a stamp written *into* the artifact and trusted on the next
read; here it is an input to the artifact's name, so a mismatch cannot be
mistaken for a match.

### Decision: keep the `vmm.Config.Image` seam exactly as it is

The driver resolves `Config.Image` to `<imageDir>/<image>.ext4` today. It keeps
doing that. `internal/ociimage` sits *above* the driver: the control plane
resolves an OCI reference to a materialized template name and hands the driver a
name, as it always has. Nothing in `internal/vmm` learns what a registry is.

This keeps the change additive — a host with a plain `universal.ext4` and no
registry configured behaves exactly as it does now — and it means bring-your-own
image, when we expose it, is a control-plane feature rather than a driver one.

### What this retires

- The rootfs release asset and its <2 GiB GitHub cap (`stage-artifacts.sh`).
- `deploy/refresh-agent-tools.sh` and its `/etc/sparkbox/tools-rev` protocol.
- The `build-rootfs.sh` / `refresh-agent-tools.sh` duplication, and with it the
  reason `install-guest-identity.sh` has to be callable from both.
- `sparkbox.dev/loop` and `SYS_ADMIN` on CKS.
- The "templates published before workload identity existed" back-patching path.

### What it does not solve

Template snapshots are still refused on CKS. `Snapshot` sanitizes a *live
guest's* rootfs — an attacker-controlled filesystem — which today means mounting
it, and that is the thing main just ruled out.
[`security-hardening.md`](security-hardening.md) names the fix as remaining work
item 2: "guest-side first-boot sanitization or a mountless/disposable sanitizer".

This design does not do that, but it supplies the missing half of the second
option: `internal/ext4` writes a filesystem without mounting one, and
`ext4.ReadFile` reads one without mounting it either. What is still needed is a
mountless *editor* — the sanitizer has to delete host keys and machine-id from an
existing image, which is neither of those. `debugfs -w` can do it, and whether
that is acceptable against a hostile image is a real question, not a formality.

---

# Part 2 — Durable guest volumes on object storage

## The constraint that decides the design

**Firecracker has no filesystem passthrough.** No virtiofs, no 9p — only
virtio-block. A host-side FUSE mount therefore cannot be handed to a guest.

So the mount happens *inside* the guest. That is not a preference; it is the only
option, and it is the same conclusion smolvm reached (their agent enters the
workload's mount namespace and mounts natively).

## Honest correction to the evaluation

The evaluation praised smolvm's `smolvm-s3fs` for needing "no libfuse, no
`fusermount3`, no external binary" so it works in a distroless image. **That
property is worth nothing to us.** Our guest is a full Ubuntu 24.04 with
systemd. Distroless is smolvm's constraint, not ours.

The part of the borrow that is valuable to us is the *architecture*: durable
object storage mounted in the guest, so a sandbox's work is not solely on
node-local disk. Writing our own FUSE+SigV4 S3 client to achieve that would be
copying the implementation and missing the idea.

**So: use rclone in the guest for the first implementation.** We already depend
on it host-side in `internal/objstore`, `deploy/install-rclone.sh` already exists
and already encodes the "must be ≥1.65 or R2 uploads silently run twice" lesson,
and it is a single static binary that drops into the tools layer from Part 1 at
no marginal distribution cost. The mounter is an implementation detail behind the
interface below; swapping it later is an internal change.

## Design

### Note: there are now two object-store backends

Main added `objstore.Filesystem`, an immutable store over a mounted filesystem,
for the VAST PVC. It does not change this design — a guest cannot reach a PVC
either, for the same virtio-block reason — but it does mean "durable storage" is
no longer synonymous with "S3 via rclone" on the host side. The guest-facing
volume path stays S3, because that is the only protocol that crosses the VM
boundary without a filesystem passthrough.

### The volume belongs to the owner, not the sandbox

This is the whole point. A volume outlives the sandbox that mounted it, and it
outlives the node. Key layout parallels the archive layout `internal/objstore`
already documents:

```
archives/<owner>/<name>...     (today)
volumes/<owner>/<volume>/...   (new)
```

Default behaviour: each account gets a `home` volume, mounted in every sandbox
at a fixed path. Node loss then costs the sandbox, not the work.

### The guest learns its volumes from the metadata service

Sparkbox already has exactly the right mechanism and should not grow a second
one. `internal/metadata` serves `GET /token` and `GET /identity` to the guest,
authenticated by network position — the guest can only reach it over its own tap,
like a cloud IMDS. Volumes are a third endpoint on that same service:

```
GET /volumes → { "volumes": [ { "name", "mountpoint", "read_only",
                                "endpoint", "bucket", "prefix",
                                "credentials": {...}, "expires_at" } ] }
```

Credentials are served, never stored on the guest disk — the same property the
OIDC token already has, for the same reason.

A `sparkbox-volumes` oneshot + timer in the guest overlay fetches this and
mounts, ordered after the network hook and before the login shell. It sits
beside `sparkbox-netcfg` and `sparkbox-token`, is installed by the same overlay,
and — because of Part 1 — is installed in exactly one place.

### Credential scoping: state the limitation, don't paper over it

Ideally each sandbox gets a credential scoped to its own owner's prefix, minted
per-boot with a short TTL. Whether that is available depends on the provider:
S3 supports it via STS `AssumeRole` with a session policy; R2 supports scoped
tokens but not per-request session policies.

The first implementation serves an owner-scoped credential with a TTL and
**documents plainly that a sandbox can reach its owner's whole volume prefix,
not just the volumes attached to it.** That is a real limitation. It is
acceptable because the blast radius is one account's own data and the
alternative is blocking the feature on provider work — but it must be written
down where an operator will read it, not discovered later.

### Control surface

Volumes go through `internal/ctlops` like everything else, so one ownership
check serves all three transports (SSH `ctl@`, REST, web console):

```
ssh ctl@<domain> volumes list
ssh ctl@<domain> volumes create <name>
ssh ctl@<domain> volumes attach <name> <sandbox> [--at /path] [--ro]
ssh ctl@<domain> volumes detach <name> <sandbox>
```

## Sequencing

Part 1 first, and not only because it is bigger. `sparkbox-volumes` is a new
guest-side payload, and installing it today means writing it twice — into
`build-rootfs.sh` and into `refresh-agent-tools.sh` — which is precisely the
duplication Part 1 exists to delete. Landing Part 1 first means Part 2's guest
payload has exactly one home.
