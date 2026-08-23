# OCI rootfs and durable volumes — what is left

Status: plan
Date: 2026-08-23
Design: [`oci-rootfs-and-durable-volumes-design.md`](oci-rootfs-and-durable-volumes-design.md)
Origin: [`smolvm-evaluation.md`](smolvm-evaluation.md), borrows 1 and 2

Work-item numbering follows [`forward-plan.md`](forward-plan.md)'s convention.
**R** items are the OCI rootfs (design Part 1); **V** items are durable volumes
(design Part 2). Sizes are S/M/L against the same yardstick that plan uses.

## Where we actually are

Landed on this branch, all tested, no callers yet:

- **`internal/ext4`** — a directory tree becomes an ext4 with `mke2fs -d`. No
  mount, no loop device, no `CAP_SYS_ADMIN`. Modes, extended attributes
  (`security.capability`, i.e. `setcap`), hardlinks, and sparseness are each
  asserted by a test rather than assumed.
- **`internal/ociimage` unpack** — a flattened image tar becomes that tree.
  Refuses both path traversal and the subtler escape: a symlink placed earlier
  in the archive that a later, lexically innocent entry writes through.
- **`internal/ociimage` pull + materialize** — a registry reference becomes a
  cached template, named by everything baked into it (digest, overlay revision,
  gateway key, size ceiling). Tested end to end against an in-process registry.

Not started: everything below. Nothing in the running system calls any of it
yet, so the branch is currently additive and inert.

Two things the merge from main changed about this plan, both in our favour:

- `security-hardening.md`'s remaining-work item 1 — "remove the preparation init
  container's loop bundle after trusted-template tooling moves into the image
  build" — is what **R5** closes, and `internal/ext4` is the tooling it was
  waiting for.
- `refresh-agent-tools.sh` now refuses to mount user-derived `snap-*.ext4` at
  all, because mounting an attacker-controlled filesystem makes the host kernel
  parse hostile ext4 metadata. A mountless build is not subject to that rule,
  which is what makes **R7** thinkable at all.

---

# Part 1 — OCI rootfs

## R1 — the guest overlay, in Go — **size M, priority 0**

The blocking item: everything else in Part 1 is wiring, and **V3 is blocked on
this too**, because installing a new guest payload today means writing it into
both `build-rootfs.sh` and `refresh-agent-tools.sh`.

Port the guest payload to a Go `ociimage.Overlay`. It currently lives in three
places that already have an anti-drift mechanism holding them together:

- `hack/build-rootfs.sh` — login user from the `sparkbox.login-user` label,
  `authorized_keys`, the sshd drop-in, blanking `/etc/hostname`, the
  `sparkbox-netcfg` hook and its systemd unit;
- `deploy/install-guest-identity.sh` — the OIDC token unit and timer (it exists
  *only* because two callers needed the same payload);
- `deploy/refresh-agent-tools.sh` — the agent CLIs, the `/etc/environment`
  knobs, the `~/.claude.json` onboarding seed, the hivemind daemon unit, and
  since main, `install_agent_guidance`.

Two of those three move into the **image build** rather than the overlay. The
overlay's job is only what is host-specific and cannot be baked: the gateway
key, and anything derived from the sandbox's own identity. Agent CLIs belong in
the image's final layer (see R3) — that is the entire argument for OCI here.

`ociimage.Image.LoginUser()` already reads the label, so the overlay does not
re-derive the login user by scanning `/etc/passwd` for a home with an
`authorized_keys` in it, the way `install-guest-identity.sh` has to.

**Acceptance:** a template materialized from the current base image is
byte-equivalent in behaviour to one built by `build-rootfs.sh` +
`refresh-agent-tools.sh` — same login user, same key, sshd starts, the identity
timer fetches a token, `sudo` works (setuid intact), `ping` works without sudo
(xattr intact). A guest boots from it and the gateway logs in.

## R2 — wire it into `serve` — **size M, priority 1**

Add `--rootfs-image <oci-ref>` beside the existing `--default-image`. When set:

- resolve and materialize at startup, before the gateway opens, and use the
  resulting template name as the effective default image;
- re-resolve on a timer, so a moved tag is picked up without a restart. This is
  the job `refresh-agent-tools.sh`'s systemd timer does today.

`host.Manager` already records a per-sandbox `Image`, so existing sandboxes keep
the template they were created from. That is the correct behaviour and it comes
for free: a template is immutable, the reflink already happened, and a running
sandbox has its own copy regardless.

Keep `--default-image` working unchanged. A host with a plain `universal.ext4`
and no registry configured must behave exactly as it does now — this is the
property that lets R1–R4 land before R5 without a flag day.

**Acceptance:** `sparkbox serve --rootfs-image ghcr.io/…:edge` on a host with an
empty image directory pulls, builds, and serves `ssh new@` with no `universal.ext4`
present. `doctor` reports the resolved digest and the template it maps to.

## R3 — publish the image from CI, layered — **size S, priority 2**

`build-artifacts.yml` already builds `images/Dockerfile`; add a push to
`ghcr.io/vanpelt/sparkbox-rootfs` with a full-SHA tag and a moving `edge`, the
same scheme `sparkbox-cks-image.yml` uses for the runtime image.

The layer split is the point: agent CLIs (`claude`, `codex`, `pi`, `hivemind`)
and the guidance payload go in a **final thin layer** over the expensive base.
Bumping them then rebuilds one layer in seconds and a host pulls tens of MB, not
750. Without the split this is a distribution change with no benefit.

**Acceptance:** bumping a `claude` version produces a new digest whose pull, on
a host that already has the previous digest, transfers only the tools layer.

## R4 — CKS pulls the image instead of a release asset — **size S, priority 3**

`deploy/kubernetes/entrypoint.sh` currently downloads a SHA-pinned
`universal-<arch>.ext4.zst` from GitHub Releases, decompresses a sparse ext4,
and then runs the template refresher. Replace all three with `--rootfs-image`.

This also retires the <2 GiB GitHub per-asset cap that `stage-artifacts.sh`
checks for, which is a ceiling the base image will hit eventually.

**Acceptance:** a cold CKS Pod reaches ready without `SPARKBOX_ROOTFS_SHA256`,
and a warm one reuses the cached template.

## R5 — drop the loop bundle and `SYS_ADMIN` — **size S, priority 4**

The payoff, and the first item that is externally verifiable rather than
internal cleanup. With R1–R4 landed the `prepare-vm-assets` init container has
no reason to mount anything:

- remove `sparkbox.dev/loop` from the device plugin and both resource blocks;
- drop `SYS_ADMIN` from its capability set, and `Unconfined` AppArmor/seccomp
  with it if nothing else needs them;
- keep `CHOWN`, `FOWNER`, `DAC_OVERRIDE`, and add `MKNOD` — the unpack needs
  these to reproduce ownership and device nodes, and `ociimage.Result` counts
  every one it could not apply rather than shipping a wrong rootfs quietly.

Update `security-hardening.md`'s remaining-work list in the same change.

**Acceptance:** the CKS deployment has no loop device anywhere, `SYS_ADMIN`
appears nowhere in the manifests, and sandboxes still create.

## R6 — retire the old pipeline — **size S, priority 5**

Delete `deploy/refresh-agent-tools.sh`, its systemd unit and timer, the
`/etc/sparkbox/tools-rev` stamp protocol, `deploy/install-guest-identity.sh`,
and the rootfs release asset in `stage-artifacts.sh`. Reduce `build-rootfs.sh`
to a local development convenience or delete it.

Wire `Cache.Prune`: the keep set is every template name referenced by a live
sandbox record plus the current default. Templates are immutable and named by
their inputs, so without this an image bump leaks one template per bump.

Do this **only after R5 has run on a real host for a while.** These scripts are
the fallback if materialization has a failure mode we have not met yet.

**Acceptance:** `grep -r refresh-agent-tools` is empty; a host that has bumped
its image three times holds one template plus those still referenced.

## R7 — rootless unpack, via a user namespace — **size M, priority 6**

R1–R6 get to "root without `SYS_ADMIN`". Unpacking inside a user namespace gets
to `runAsNonRoot: true` on CKS, which is a categorically different claim.

`ociimage.Result` already reports exactly what a rootless unpack cannot do —
`SkippedOwnership`, `SkippedDevices`, `SkippedXattrs` — so the shape of the
problem is already measurable before anyone writes the namespace code.

Independent of everything above; do it when the CKS security posture is worth
more than the effort.

---

# Part 2 — durable volumes

**V1–V4 are sequenced after R1.** Not for tidiness: V3 installs a guest-side
unit, and until R1 there are two places to install it and a revision protocol
holding them in sync.

## V1 — volume records and control surface — **size M**

Owner-scoped volume records (`volumes/<owner>/<volume>/…`, parallel to the
existing `archives/<owner>/<name>`), through `internal/ctlops` so one ownership
check serves SSH `ctl@`, REST, and the web console.

```
ssh ctl@<domain> volumes list | create <name> | attach <name> <sandbox> [--at /path] [--ro] | detach …
```

## V2 — `GET /volumes` on the metadata service — **size M**

A third endpoint beside `/token` and `/identity`, authenticated the same way:
by network position, over the sandbox's own tap. Serves volume specs plus
short-lived credentials, which are served and never written to the guest disk —
the property the OIDC token already has.

**This item carries the honest limitation.** The first implementation serves an
owner-scoped credential, so a sandbox can reach its owner's whole volume prefix,
not only the volumes attached to it. That is a real gap. It goes in
`security-hardening.md` where an operator will read it, not in a commit message.

## V3 — the guest mounter — **size S, blocked on R1**

A `sparkbox-volumes` oneshot plus timer in the overlay, beside `sparkbox-netcfg`
and `sparkbox-token`, ordered after the network hook and before the login shell.
Mounts with rclone, which ships in the tools layer at no marginal cost and
already encodes the "must be ≥1.65 or R2 uploads silently run twice" lesson from
`deploy/install-rclone.sh`.

Firecracker has no virtiofs or 9p, so the mount must happen inside the guest.
That is a constraint, not a preference — a host-side FUSE mount cannot be handed
to a guest through virtio-block.

## V4 — a default `home` volume — **size S**

Every account gets one, mounted in every sandbox at a fixed path, opt-out. This
is the item that actually delivers the borrow: node loss costs the sandbox, not
the work.

---

# Decisions needed

These change what gets built, and guessing wrong is expensive.

1. **Registry and visibility for the rootfs image.** GHCR public (no credential
   on any host, and the base image's contents become public) or private with
   `docker login ghcr.io` per host? `authn.DefaultKeychain` already reads
   `~/.docker/config.json`, so private costs no sparkbox-specific plumbing — but
   it does mean a host cannot cold-start without a credential, which is a new
   failure mode on the CKS path. **Affects R3, R4.**

2. **Does the base image keep its ~65-minute CI build?** R3 assumes yes and
   makes it not matter by splitting layers. If the base is instead trimmed, the
   split matters less and R3 shrinks. **Affects R3.**

3. **The size ceiling becomes a Spec field.** Today it is 25 GiB baked at build
   time by `stage-artifacts.sh`. Materialization can produce several ceilings
   from one image, which interacts with the per-owner disk pools that landed on
   main — is the ceiling a property of the sandbox tier now? **Affects R2.**

4. **Credential scoping for volumes.** S3 supports session-policy scoping via
   STS; R2 supports scoped tokens but not per-request session policies. Ship the
   owner-scoped limitation and document it, or block V2 on provider work?
   **Affects V2.**

5. **The snapshot sanitizer** — `security-hardening.md` item 2, adjacent to this
   work rather than part of it. `internal/ext4` supplies mountless *write* and
   mountless *read*; a sanitizer needs a mountless *edit* of a hostile image.
   `debugfs -w` can do it, and whether that is acceptable against an
   attacker-controlled filesystem is a real question. The alternative is
   guest-side first-boot sanitization, which moves the problem inside the
   boundary. **Not scheduled here; flagged so it is not assumed solved.**

# Suggested order

R1 → R2 → R3 → R4 → R5, then V1/V2 in parallel with R6, then V3 → V4. R7 whenever.

R5 is the first item worth announcing: it is the one an operator can verify by
reading a manifest.
