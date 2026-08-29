# Templates carried by a tag

How a sandbox stops re-downloading and re-unpacking the same container images
every time it is created, by making "which rootfs do I start from" a property of
a tag rather than a global constant.

This is a proposal. Part 1 is shipped. Nothing from Part 3 onward is built.

---

# Part 1 — where this actually stands today

## The cost

A sandbox boots with docker running (`images/Dockerfile:154`) and an empty
`/var/lib/docker`. The first thing anybody does in a fresh box is pull the
images they were using in the last one. On a team, that is the same handful of
images, pulled independently by every sandbox, forever.

The bandwidth is the least of it. A pull has four costs and they want four
different fixes:

| cost | fixed by |
| --- | --- |
| bandwidth from the registry | a pull-through mirror |
| CPU decompressing layers | prebaking, or lazy-pull |
| **disk writes** unpacking into overlay2 | **copy-on-write sharing** |
| disk reads + host page cache | a genuinely shared inode |

The third row is the expensive one, and the platform already has the machinery
to make it free.

## What is already built

Every sandbox rootfs is a reflink copy. `hack/setup-host.sh:87` lays down the
work volume as `mkfs.xfs -m reflink=1`, and the driver's create path is
`cp --reflink=always <image-dir>/<image>.ext4 → <vm-dir>/rootfs.ext4`
(`internal/vmm/firecracker/fc.go:606-611`, via `reflinkClone` at
`fc.go:1222`, which uses `--reflink=always` precisely so the no-full-copy
policy cannot silently regress to `auto`). A brand new 25 GiB sandbox disk costs
approximately zero blocks. Whatever is in the template is free, in perpetuity,
for every sandbox made from it.

Snapshots turn that into a user-facing primitive. `internal/host/snapshot.go`
captures a customized sandbox as a fork-able template:

```
ssh ctl@<gateway> snapshot ls
ssh ctl@<gateway> snapshot create <box> <name>
ssh ctl@<gateway> snapshot rm <name>
ssh ctl@<gateway> fork <snapshot> <new-name> [--tag <t>]…
```

with the same four verbs on REST (`internal/restapi/snapshots.go`) and in the
user console (`internal/userconsole/console.go:261-270`).

`Manager.Snapshot` (`internal/host/snapshot.go:43`) takes the sandbox's disk
lock, strips the managed secret env block — every fork copies the rootfs
byte-for-byte, so the block cannot be allowed to ride along — pauses the guest
so the filesystem is flushed and unmounted, and hands off to
`Driver.Snapshot` (`fc.go:1177`), which runs `e2fsck -fy` + `zerofree`, reflinks
the result to `images/snap-<owner>-<name>.ext4`, and renames it into place. The
record is stamped with the node that holds the file.

`Manager.Fork` (`snapshot.go:96-105`) is five lines. It resolves the owner's
snapshot to its template name and calls `Create` with it, inheriting admission,
placement, routing, front-door setup and the create-time secret push unchanged.

So the answer to "my team keeps pulling the same 6 GB CUDA image" already
exists: pull it once, snapshot, fork. It costs one pull, ever.

## Why nobody does it

Because `create` cannot reach it. `ctlops.CreateArgs` has no image field at all;
`build` passes `o.defaultImage` — one global string — on both the local and the
remote path (`internal/ctlops/sandbox.go:100-106`). `fork` is a separate verb
with a separate name to remember, taking a snapshot name the user has to look up
first, and it silently drops out of every default workflow: `ssh new@<gateway>`
gets the stock template and always will.

The primitive is right. The door to it is in the wrong place.

---

# Part 2 — the object: a template attachment, carried by a tag

The right primitive is, once again, already in the schema. `sandbox_tags`
(`internal/secrets/store.go:173`) is a shared join table with three independent
readers — `internal/secrets` owns the mutations and computes `EnvForSandbox`,
`internal/netrules` computes `RulesForSandbox`, `internal/repos` computes the
repo manifest — and each owns its own side table (`secret_tags`,
`network_rule_tags`, `repo_tags`). A tag is this platform's settled answer to
"which of my things reach which of my boxes", and it has now been the right
answer three times.

So: a fourth table, a fourth reader, the same join.

```sql
CREATE TABLE IF NOT EXISTS template_tags (
  owner      TEXT NOT NULL,   -- handle that owns the snapshot
  tag        TEXT NOT NULL,   -- the shared sandbox_tags namespace
  snapshot   TEXT NOT NULL,   -- user-facing snapshot name, not the image basename
  created_at TIMESTAMP NOT NULL,
  PRIMARY KEY (owner, tag)
);
```

`PRIMARY KEY (owner, tag)` rather than `(owner, tag, snapshot)` is the whole
design in one line, and Part 3 is about why.

The verbs:

```
ssh ctl@<gateway> snapshot bind <snapshot> --tag <t>
ssh ctl@<gateway> snapshot unbind --tag <t>
```

and `snapshot ls` grows a column showing which tags each snapshot backs. Then:

```
ssh new@<gateway> --tag cuda
```

boots from the CUDA snapshot. No new verb, no name to look up, and every
existing surface — REST, console, the `new@` door — inherits it, because they
all already funnel through `ctlops.Create`.

The resolution belongs in `ctlops.build`, which today reads:

```go
if node == "" {
    return o.boxes.Create(ctx, name, owner, o.defaultImage, vcpus, memMB)
}
```

and becomes a lookup of the caller's `template_tags` rows for the tags already
computed at the top of `Create`, falling back to `o.defaultImage` when none
bind. Note that `Create` already stamps tags *before* it builds
(`sandbox.go:33-36`), explicitly because "Create fires the secret-env push
asynchronously and the tags decide its contents". Resolving the image from those
same tags is one more consumer of an ordering that is already correct for
exactly this reason.

`Fork` does not go away. It stays as the explicit one-off — "boot this snapshot
once, without making it my tag's default" — and it keeps its own tag argument.
Binding is the durable form of the same idea.

---

# Part 3 — composition, and the one refusal

Every other reader of `sandbox_tags` **adds**. Two tags on a sandbox mean the
union of two secret sets, the union of two repo lists. `internal/netrules` is
the exception that already proved the point: its rules *subtract*, and that
difference forced a specific refusal at write time.

A template does neither. It **replaces**. A sandbox has exactly one rootfs, and
`--tag cuda --tag node20` with both bound has no answer that is not a coin flip.

So: **a create whose tags resolve to more than one distinct snapshot is
refused**, with both snapshot names in the message and the suggestion to fork
explicitly or make a combined snapshot. Ambiguity here is not a corner case to
paper over with a precedence rule — a precedence rule means somebody gets a
sandbox with the wrong CUDA in it and finds out twenty minutes later.

The refusal must land **early**, next to the existing `nameIsFree` and
`placeable` checks and before `stampTags`. `Create`'s own comment explains why:
"a create that cannot possibly succeed must not leave tag rows behind for a
sandbox that never exists".

## `default` is refused at bind time

`ctlops.defaultTags` (`sandbox.go:178-186`) stamps `secrets.DefaultTag` on every
sandbox this package creates. `internal/netrules` already refuses that tag
outright at write time, and its reasoning transfers exactly
(`internal/netrules/store.go:204-210`):

> the same word that means "reaches everything" for the other two would silently
> cut the whole fleet down to three domains, minutes later, on a policy push
> nobody connected to the rule they had just saved.

A template bound to `default` would re-base every sandbox in the fleet on one
user's snapshot, and would do it to sandboxes created by people who have never
heard of that snapshot. Refuse it at `bind`, with the same shape of message that
`PutRule` uses. The legitimate version of "change the base image for everyone"
already exists and is an operator knob, not a user one: `o.defaultImage`.

That refusal is, as in netrules, what makes `Create` free to keep stamping
`default` on everything.

---

# Part 4 — placement, which is the hard part

A snapshot is a file in **one machine's** image directory. `Snapshot.Node`
records which. The ssh surface already refuses to pretend otherwise
(`internal/sshgw/control.go:644-648`):

> fork has no `--node`: a snapshot can only be forked on the machine that holds
> it

Binding a template to a tag inherits that constraint and makes it invisible.
`--tag cuda` stops being only a statement about secrets and egress and quietly
becomes a placement directive. On a single-machine host that is nothing. On the
DGX+laptop fleet, or on CKS with several nodes, it is the whole problem.

Two ways out.

**Placement follows the template.** The binding resolves to a snapshot, the
snapshot names a node, and `build` places there. This is a small change —
`placement` already takes a node name — and it is almost certainly the right
default, because the alternative is copying multi-gigabyte files around on
demand.

The cost is a new class of failure: `--tag cuda` can now fail for capacity
reasons that have nothing to do with the tag. That error message has to say so
in full — which node, why the sandbox could not go elsewhere, and that the
reason is a template binding — or this becomes the most confusing error in the
product. Combining it with an explicit `--node` that names a different machine
is a refusal, not an override.

**Templates replicate.** The general answer, and a real project: a
content-addressed template store, push-on-bind or pull-on-miss, GC for templates
no binding references, and a story for a node that is offline when the bind
happens. Multi-gigabyte artifacts, so every one of those is a transfer with a
progress bar and a failure mode.

Ship the first. Write down the second. Do not build the second until somebody
has actually been blocked by the first — on the current fleet the gateway holds
essentially every template that matters.

---

# Part 5 — tool drift, and `sparkbox update-tools`

## The drift

`deploy/refresh-agent-tools.sh` keeps the agent CLIs current by patching
templates in place — reflink copy, loop mount, install, atomic rename
(`refresh-agent-tools.sh:840-894`) — on a timer, so a fresh `ssh new@` always
gets the current Claude, Codex, Pi, HiveMind and agent-browser without a
65-minute image rebuild.

It deliberately skips user snapshots (`refresh-agent-tools.sh:189-193`):

> Only release/operator base templates are mounted here. User-derived
> `snap-*.ext4` images are deliberately excluded: mounting an untrusted guest
> filesystem asks the privileged host kernel to parse attacker-controlled ext4
> metadata and turns the management plane into a second sandbox boundary.

That is correct and it is not up for renegotiation. But its consequence is fatal
to Part 2 as written. Today, forks are occasional and a stale CLI in one of them
is a curiosity. The moment a team's tag *always* resolves to a snapshot, every
sandbox that team creates is frozen on whatever tool versions were on disk the
day somebody ran `snapshot create`, and it stays frozen until someone
re-snapshots. Tag templates would take the platform's best-maintained property —
that a new box has current tools — and silently switch it off for the users who
adopt the feature hardest.

## The fix: the guest pulls, over a channel it already has

Invert it. The host never mounts the guest; the guest fetches into its own disk.
The sandbox boundary stays exactly where it is, which is the entire reason the
patch loop skips these images.

The host already caches every artifact it needs, verified. `refresh-agent-tools.sh:65`
puts them in `/srv/sparkbox/tools` as content-named files
(`claude-$VER-$PLAT`, `codex-$TAG-$ARCH`, `pi-$TAG-linux-$ARCH.tar.gz`,
`hivemind-$VER-$PLAT`, `agent-browser-$VER.tgz`), with a `versions.env` stamp and
a prune pass (`:904`). The sha256 verification against each upstream's release
manifest has already happened, once, on the host.

The guest already has an authenticated, unforgeable channel to the host:
`internal/metadata`, reached at `http://$GW:<meta-port>` over the guest's own
tap. Its authenticator matches the request's source address to the guest's slot
*and* the connection's local address to `slot.Host`
(`internal/metadata/server.go:466-494`), which is why `/token`, `/identity` and
`/self/*` can be trusted without a bearer token.

So, two endpoints:

```
GET /tools/manifest        -> {"claude":{"version":…,"sha256":…,"size":…}, …}
GET /tools/<name>          -> the bytes, streamed from /srv/sparkbox/tools
```

and one guest command:

```
sparkbox update-tools [--check]
```

which reads the manifest, compares against the stamp in the guest, downloads
only what moved, verifies the digest, and installs to `/usr/local/bin` with the
same layouts the patch loop uses — the bundle-plus-symlink shape for Pi and
agent-browser, plain binaries for the rest, since a copied-out executable
resolves its skill data relative to its own path and breaks.

Serving from the host's cache rather than from upstream is what makes this work
for a tagged sandbox at all: egress is filtered to the tag's allow list, and
`downloads.claude.ai` is not on it. It also keeps sha256 verification in one
place, and it means a `update-tools` on a node hits that node's cache instead of
the WAN.

Two things fall out for free. A user can run it on a long-lived box that has
drifted, which is a request that exists today with no answer. And `snapshot
create` can run it first, so a snapshot is captured with current tools rather
than whatever the source box happened to have — turning the drift problem into a
one-line precondition on the capture path.

The command belongs in the guest either way, tag templates or not.

---

# Part 6 — accounting, which currently does not know about reflinks

`DiskUsageMB` reads the guest's ext4 superblock directly rather than asking the
host (`internal/vmm/firecracker/fc.go:1256-1276`), and the comment is explicit
about why: host-side `du` "counts shared reflink extents once for every clone",
and a decompressor that materializes the template's zeroes makes an almost-empty
25 GiB filesystem look full. Reading the superblock gives the number a user
expects next to their ceiling, and makes the answer "independent of
sparse/reflink representation".

Independent in both directions. A snapshot carrying 8 GB of docker images makes
**every** sandbox forked from it report 8 GB used the instant it boots — against
its own 25 GiB hard ceiling and against the owner's pooled soft budget
(`DiskPoolMBPerOwner`, `internal/host/manager.go:609`) — for blocks that
physically exist exactly once on the volume.

That is precisely backwards from the incentive this whole design is trying to
create. The user who does the efficient thing watches their quota fall over.

The fix is a baseline. Record the template's own used-blocks figure at bind time
(or read it from the template's superblock on demand — the same passive read,
against a file nothing is writing), and report a forked sandbox's usage as
`used - template_baseline`, floored at zero. That is the number that answers
"how much disk have *I* caused", which is the question a quota is asking.

The hard ceiling is a different question and should keep the raw number: the
guest really will hit ENOSPC at 25 GiB regardless of who wrote the blocks. Two
numbers, two purposes — the same distinction the existing comment draws between
a numerator and a denominator on the same basis. The console's meter shows raw
against the ceiling; the pooled budget sums baselines-subtracted.

---

# Part 7 — archiving a fork, deferred

Archive today is self-contained. `Manager.Archive` (`internal/host/manager.go:2137`)
pauses, calls `PackRootfs` — `e2fsck` + `zerofree` + `zstd` — uploads to
`<prefix>/<owner>/<name>.ext4.zst` (`manager.go:2128`) and reclaims the local VM
directory. Restore downloads, unpacks, and marks the sandbox `StatePaused` so
the next start cold-boots it.

A forked box breaks the economics. Its rootfs is mostly its template, so
archiving ten forks of one snapshot ships that snapshot to object storage ten
times, and pays the compression CPU ten times.

The obvious shape is a delta: archive the template once, and for each fork store
only what it changed.

Two plausible mechanisms:

- **`zstd --patch-from=<template>`.** One command, no filesystem introspection,
  and it needs the template present at both compress and decompress time. Reads
  the whole template each way, but it is CPU that is already being spent.
- **FIEMAP.** A fork starts as an exact reflink copy, so XFS flags every
  still-shared extent `FIEMAP_EXTENT_SHARED`; the unshared ones are exactly the
  blocks the fork wrote. Passive, read-only, and much less I/O. But `SHARED`
  means shared with *something*, not with *this* template, and `zerofree` writes
  into free space and would break sharing before the walk ever ran — so the
  compaction step and the delta walk would have to be reordered against each
  other.

Neither of those is the reason to defer. **The reason is that a delta archive
has a dependency, and the archive store currently has none.** `<owner>/<name>.ext4.zst`
restores from itself, anywhere, forever. A delta restores only if
`snap-<owner>-<snap>.ext4` is still in the object store — which means archiving
the snapshot itself, refcounting deltas against it, refusing or cascading
`snapshot rm` while archives depend on it, garbage-collecting templates nothing
references, and making restore a two-fetch operation that can half-fail. That is
a lifecycle graph in object storage, and it wants to exist for a reason better
than saving R2 bytes on a workload nobody has measured yet.

Noted, deliberately not scheduled. Revisit when fork archives are actually a
meaningful share of the bucket.

---

# Part 8 — build order

1. **`sparkbox update-tools` and the `/tools` metadata endpoints** (Part 5).
   Independent of everything else, useful on its own, and the precondition that
   makes tag templates safe to adopt. Ship first.
2. **`template_tags` + `bind`/`unbind` + image resolution in `ctlops.build`**
   (Parts 2 and 3), including the multi-binding refusal and the `default`
   refusal, both at write time.
3. **Placement follows the template** (Part 4), with the full-sentence error for
   the capacity case. Needed before this is safe on a fleet; the single-machine
   host does not notice it either way.
4. **Quota baseline subtraction** (Part 6). Needed before anyone is told to bake
   images into a snapshot on purpose.
5. Call `update-tools` from `snapshot create` so captures start current.

Template replication across nodes (Part 4) and delta archives (Part 7) stay
written down and unbuilt.

---

# Part 9 — open questions

- **Does `bind` re-point, or is a binding immutable?** Re-pointing gives a team
  a way to move a tag's image forward — someone snapshots a refreshed box,
  rebinds, and everyone's next sandbox is current. It also means a create's
  rootfs can change under people with no notification. Re-pointing is probably
  right, with the change in the audit log and the previous snapshot left intact.
- **What happens to `snapshot rm` on a bound snapshot?** Refuse while bound is
  the obvious answer, and matches the shape of the other stores' refusals.
- **Cross-owner bindings.** Snapshots are owner-scoped and tags are owner-scoped,
  so a team sharing one base image means every member snapshotting their own copy
  — N copies of the same blocks, reflinked from nothing. A shared/org-scoped
  snapshot is the real answer and is out of scope here.
- **Does a bound tag deserve to show up in `list`?** A sandbox's tags are already
  printed. Whether the resolved template is worth a column, or only appears when
  it is not the default, is a display question worth answering before people
  start debugging "why is this box different".
