# Templates carried by a tag

How a sandbox stops re-downloading and re-unpacking the same container images
every time it is created, by making "which rootfs do I start from" a property of
a tag rather than a global constant.

## Status

Parts 1 through 6 are built, which is the whole of the build order in Part 8.
Part 7 (delta archives) and the template-replication half of Part 4 are still
written down and unbuilt, deliberately. Part 9's open questions have answers now
and carry them inline. Part 10 records the guest-side verbs, which were designed
after this document and shipped with the rest of it; Part 11 lists the surfaces
and the error codes that are now a compatibility surface; Part 12 records two
gaps this work found and did not introduce.

**None of it does anything on CKS.** `internal/vmm/firecracker/fc.go:1181-1183`
refuses `Driver.Snapshot` outright when `DisableHostRootfsMounts` is set, and
that is the live cluster's configuration — see `docs/deploy-cks.md`. A host that
cannot capture a snapshot cannot bind one either, so on CKS `snapshot create`
refuses, `bind` has nothing to point at, and every create resolves to the
operator's default image. The feature is inert there until fork identity
sanitization runs inside the guest (`docs/security-hardening.md`), and that is a
property of the deployment rather than of a flag anyone can turn on.

Everything below was exercised against the mock driver in `go test ./...`, which
covers the whole path with no KVM, and that is the only place any of it has been
run. The Firecracker half — the superblock reads, the reflink, the capture
itself — has not been executed on a real host at all: not on the live cluster,
where the paragraph above says it cannot be, and not on the DGX either. A
single-machine Linux/KVM host is where that half becomes checkable, and nobody
has checked it yet.

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
user console (`internal/userconsole/console.go`).

`Manager.Snapshot` (`internal/host/snapshot.go:46`) takes the sandbox's disk
lock, strips the managed secret env block — every fork copies the rootfs
byte-for-byte, so the block cannot be allowed to ride along — refreshes the agent
CLIs (Part 5), pauses the guest so the filesystem is flushed and unmounted, and
hands off to `Driver.Snapshot` (`fc.go:1177`), which runs `e2fsck -fy` +
`zerofree`, reflinks the result to `images/snap-<owner>-<name>.ext4`, and renames
it into place. The record is stamped with the node that holds the file.

`Manager.Fork` (`snapshot.go:108`) is eleven lines. It resolves the owner's
snapshot to its template name and calls `Create` with it, inheriting admission,
placement, routing, front-door setup and the create-time secret push unchanged.

So the answer to "my team keeps pulling the same 6 GB CUDA image" already
exists: pull it once, snapshot, fork. It costs one pull, ever.

## Why nobody does it

Because `create` could not reach it. `ctlops.CreateArgs` has no image field at
all; `build` passed `o.defaultImage` — one global string — on both the local and
the remote path (`internal/ctlops/sandbox.go`). `fork` is a separate verb
with a separate name to remember, taking a snapshot name the user has to look up
first, and it silently drops out of every default workflow: `ssh new@<gateway>`
gets the stock template and always will.

The primitive is right. The door to it is in the wrong place.

---

# Part 2 — the object: a template attachment, carried by a tag

The right primitive is, once again, already in the schema. `sandbox_tags`
(`internal/secrets/store.go:185`) is a shared join table with three independent
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

**Correction to this part as written.** The resolution does *not* belong in
`ctlops.build`, and it cannot be a `*ForSandbox` join, by this document's own
ordering argument. `build` has no access to the tags, and the refusal in Part 3
has to land beside `nameIsFree` and `placeable` — before the first write —
whereas `stampTags` runs after it. Joining `sandbox_tags` on the new name would
therefore find no rows and answer "no template" for every create there has ever
been.

So the lookup sits in `Ops.Create` (`internal/ctlops/sandbox.go`), between
`placeable` and `stampTags`, over the tag list `Create` computed at the top:
`resolveTemplate` reads `template_tags` for exactly those tags and falls back to
`o.defaultImage` when none bind, and `build` is handed the answer as a
`resolvedTemplate{Image, Node, Tag, Snapshot}`. `TemplatesForSandbox` — the join
this section originally implied — exists in `internal/templates` as a display and
debugging method and is deliberately not on the `ctlops` interface.

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

`ctlops.defaultTags` (`sandbox.go:225`) stamps `secrets.DefaultTag` on every
sandbox this package creates. `internal/netrules` already refuses that tag
outright at write time, and its reasoning transfers exactly
(`internal/netrules/store.go:204-210`):

> the same word that means "reaches everything" for the other two would silently
> cut the whole fleet down to three domains, minutes later, on a policy push
> nobody connected to the rule they had just saved.

**Correction to this part as written.** The sentence here used to say a
`default` binding would re-base every sandbox *in the fleet*, including ones
created by people who have never heard of that snapshot. It would not. Every
`sandbox_tags` join in this tree carries the owner on both sides and
`template_tags` is keyed `(owner, tag)`, so alice binding `default` reaches
alice's sandboxes and nobody else's.

The refusal is still right for the reason that survives the correction, and that
is the reason the shipped message gives: every sandbox *you* create carries that
tag, so this snapshot would silently become the base image for all of them,
including ones you make months from now. Refused at `bind`, in the same shape
`PutRule` uses, and refused a second time at the guest door (Part 10) so it lands
before the pause rather than as a bind failure with nobody watching. The
legitimate version of "change the base image for everyone" already exists and is
an operator knob, not a user one: `o.defaultImage`.

That refusal is, as in netrules, what makes `Create` free to keep stamping
`default` on everything.

---

# Part 4 — placement, which is the hard part

A snapshot is a file in **one machine's** image directory. `Snapshot.Node`
records which. The ssh surface already refuses to pretend otherwise
(`internal/sshgw/control.go:752-756`):

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

**Shipped, with two things this section did not see.**

`ctlops.templatePlacementFailed` is the full-sentence error: it copies the
classified error rather than rewriting it in place (`AsError` hands back
`internal/fleet`'s own `*Error` pointer for anything already classified), keeps
its `Kind`/`Code`/`Exit`/`Status`, and appends the binding's explanation to `Msg`
— never to `Hint`, which `sshgw`'s `failCtl` does not print. The `--node`
contradiction is `templateNodeAgrees`, code `template_node_conflict`, refused
before any write.

The first thing this section missed is the one that would have bricked every
single-machine deployment. `host.NewManager` coerces an unset node name to
`"local"` and `load()` re-stamps every snapshot record with it, so on a one-box
host *every* snapshot carries `Node="local"`. `build` therefore asserts
`ctlops.placer` **before** it reads `tpl.Node`: the absence of `CreateOn` is the
statement that there is one machine and the template is on it. Read the other
way round, every tag-templated create on every laptop and every standalone host
would have answered "this host runs a single machine, so a sandbox can't be
placed on a named one." There is a named regression test for it.

The second is that handing a snapshot image to `Sandboxes.Create` does not, on
its own, place on the template's machine — `fleet.pick` short-circuits to the
local machine when no placer is installed and no node was preferred. Setting
`node = tpl.Node` is what makes placement follow the binding. And `Candidate.Fits`
would then have refused the very machine holding the template: it checks the
requested image against `Facts.Images`, a directory listing taken once per link
connection, while the gateway's knowledge that the node holds the template comes
from the live `n.Templates()` inventory. `Fleet.candidate` now unions the live
template basenames into the candidate's facts (`internal/fleet/place.go`), which
is strictly additive — `Fits` only ever refuses on an image it cannot find.

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
templates in place — reflink copy, loop mount, install, atomic rename — on a
timer, so a fresh `ssh new@` always gets the current Claude, Codex, Pi, HiveMind
and agent-browser without a 65-minute image rebuild.

It deliberately skips user snapshots (`refresh-agent-tools.sh:210`, and the
header at `:8-16`):

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

**Correction to this part as written: the host did not already cache every
artifact it needs.** The refresher's early exit is `exit 0` and it precedes every
download — `if [ ${#STALE[@]} = 0 ]; then echo …; exit 0; fi` sat above the whole
download block — so a host whose templates all carried a current stamp reached
the end of a run with an *empty* `$TOOLS_DIR`. The premise was false on precisely
the freshest hosts, which are the ones that have been refreshing on their timer
all along.

So the download, the `versions.env` stamp and the prune were hoisted above that
exit, and the run now always resolves and verifies the artifacts before it asks
the separate question of whether any template needs patching. On a warm cache
this costs nothing — every fetch is `[ ! -x ]`-guarded and the version resolution
had already run — but it is a real behaviour change and not a tidy-up: a broken
upstream now fails a run that used to exit 0 without looking. That is the honest
outcome. A host that cannot verify an artifact has no business publishing a
manifest that says it did.

With the hoist, `TOOLS_DIR` (`refresh-agent-tools.sh:75`, default
`/srv/sparkbox/tools`) holds the content-named files (`claude-$VER-$PLAT`,
`codex-$TAG-$ARCH`, `pi-$TAG-linux-$ARCH.tar.gz`, `hivemind-$VER-$PLAT`,
`agent-browser-$VER.tgz`) on every run. The sha256 verification against each
upstream's release manifest has happened, once, on the host.

The guest already has an authenticated, unforgeable channel to the host:
`internal/metadata`, reached at `http://$GW:<meta-port>` over the guest's own
tap. Its authenticator matches the request's source address to the guest's slot
*and* the connection's local address to `slot.Host`
(`internal/metadata/server.go:518`), which is why `/token`, `/identity` and
`/self/*` can be trusted without a bearer token.

So, two endpoints:

```
GET /tools/manifest        -> the artifacts this host has verified
GET /tools/<name>          -> the bytes, streamed from $TOOLS_DIR
```

and one guest command:

```
sparkbox update-tools [--check]
```

which reads the manifest, compares against the stamp in the guest, downloads
only what moved, verifies the digest, and installs with the same layouts the
patch loop uses.

Four things about the shipped shape that this section got wrong or left out, all
of them forced by the two ends it has to satisfy:

- **The manifest is an array of objects, each carrying its own `name`, and every
  value is a quoted JSON string — `size` included.** The map-of-objects form
  above does not survive the guest's parser: the guest payload holds to curl and
  awk on purpose (`install-guest-identity.sh` says why — the slim systemd-less
  fallback template has no JSON library), and its pair-walk splits on `{`, which
  puts the tool's name on a line where it cannot be recovered, and only matches
  `"key": "value"`, so an unquoted number reads back as empty.
- **"Installs to `/usr/local/bin`" is wrong for two of the five.** Pi lands in
  `/usr/local/lib/pi` and agent-browser in `/usr/local/lib/agent-browser`, each
  reached by a *relative* symlink on `PATH`. The layout therefore travels as
  manifest data — kind, bin, dir, exec, link, the `bin/` prune, the directories
  to drop — rather than being derived in the guest: a copied-out executable
  resolves its skill data relative to its own path and breaks.
- **The digests are recomputed from the files on disk on every run.** A warm
  cache skips the download block entirely, so a digest carried over from the day
  a file arrived would be a claim about bytes nobody looked at this run.
- **The handlers set their own write deadline.** The metadata server's
  `WriteTimeout` is 10 seconds (`server.go:493`) and agent-browser's tarball is
  ~92 MB, so each `/tools` handler raises its own deadline through
  `http.NewResponseController`. Raising it server-wide would weaken `/token` and
  `/github/credential`, whose 10 seconds is deliberate. If that per-handler
  deadline is ever dropped, the symptom is not a timeout — it is an unexplained
  sha256 mismatch inside the guest.

The guest's stamp is `/var/lib/sparkbox/tools-rev`, seeded on first run from the
template's `/etc/sparkbox/tools-rev`, and the guest never writes the latter: that
file is the host patch loop's decision variable, and its `identity=` and
`agentenv=` words name payloads only a host with the template mounted can
install. A guest that wrote it would make the refresher believe a template was
current that it never patched.

`/tools` is served by every host to its own guests and is **never relayed** over
the fleet link. Each machine runs its own refresher and therefore holds its own
cache; a relay would drag ~92 MB across the link and defeat the argument for
having the cache at all.

Serving from the host's cache rather than from upstream is what makes this work
for a tagged sandbox at all: egress is filtered to the tag's allow list, and
`downloads.claude.ai` is not on it. It also keeps sha256 verification in one
place, and it means a `update-tools` on a node hits that node's cache instead of
the WAN.

Two things fall out for free. A user can run it on a long-lived box that has
drifted, which is a request that existed with no answer. And `snapshot create`
runs it first, so a snapshot is captured with current tools rather than whatever
the source box happened to have — turning the drift problem into a precondition
on the capture path.

That precondition has three constraints the sentence above hides, and all three
decided where the call lives. It is in `host.Manager.Snapshot`, between the strip
and the pause, and not in `ctlops.CreateSnapshot`: only there is the guest safely
awake (`stripEnvForPack` woke it via `resumeOrRecreate` precisely so
`EnsureRunning`'s async env push could not race the strip), only there is the
call inside `lockDiskOperation`, which is what keeps the reaper from pausing the
box mid-download, and both consoles call `boxes.Snapshot` directly and would have
missed a `ctlops`-only step entirely. It is **synchronous**, unlike the repo sync
that returns the moment the guest accepts the job, because a pause landing halfway
through writing `/usr/local/bin/claude` freezes a truncated binary into a template
every fork then copies byte-for-byte. And it is best-effort: a failure is a
gateway `WARN` and the capture proceeds, because a slightly stale template is not
a failed one.

The remote half is not an extra. A node caches only the gateway's upstream
*public* key, so it has no signer with which to open a session into its own
guests; `Fleet.Snapshot` therefore runs the refresh gateway-side before it hangs
up the sessions, skipping the local machine (the manager does its own) and
skipping a paused remote guest, which nothing here may wake. That last asymmetry
is invisible to the user — a paused box captured on a one-machine host *is*
refreshed, and on a fleet it is not — so it is stated in `help snapshots` and in
the OpenAPI description rather than left to be discovered.

Budget: the refresh is bounded well inside `ctlops.ArchiveTimeout`, which is 15
minutes for the whole capture. If captures start timing out, the fix is to raise
that ceiling, and the symptom will read as "snapshots got slower after the tool
refresh landed".

Two limits worth writing down before somebody automates this. The `/tools`
budget is a *rate* bound and not a concurrency one, so a fleet-wide
`update-tools` is N guests each streaming ~150 MB off one host at once —
acceptable for a hand-run command, not on a timer. And each update writes ~150 MB
into that VM's own 25 GiB ceiling and against its owner's pool: these are blocks
the guest genuinely wrote, so Part 6's baseline subtraction does not help, which
is why the agent-browser `bin/` prune (92 MB → 13 MB) is not optional in the
guest.

The command belongs in the guest either way, tag templates or not.

---

# Part 6 — accounting, which currently does not know about reflinks

`DiskUsageMB` reads the guest's ext4 superblock directly rather than asking the
host (`internal/vmm/firecracker/fc.go:1268`, rationale at `:1256-1267`), and it is explicit
about why: host-side `du` "counts shared reflink extents once for every clone",
and a decompressor that materializes the template's zeroes makes an almost-empty
25 GiB filesystem look full. Reading the superblock gives the number a user
expects next to their ceiling, and makes the answer "independent of
sparse/reflink representation".

Independent in both directions. A snapshot carrying 8 GB of docker images makes
**every** sandbox forked from it report 8 GB used the instant it boots — against
its own 25 GiB hard ceiling and against the owner's pooled soft budget
(`DiskPoolMBPerOwner`, `internal/host/manager.go:652`) — for blocks that
physically exist exactly once on the volume.

That is precisely backwards from the incentive this whole design is trying to
create. The user who does the efficient thing watches their quota fall over.

The fix is a baseline: report a forked sandbox's pooled usage as
`used - template_baseline`, floored at zero. That is the number that answers
"how much disk have *I* caused", which is the question a quota is asking.

The hard ceiling is a different question and keeps the raw number: the guest
really will hit ENOSPC at 25 GiB regardless of who wrote the blocks. Two numbers,
two purposes — the same distinction the existing comment draws between a
numerator and a denominator on the same basis. The console's meter shows raw
against the ceiling; the pooled budget sums baselines-subtracted. Full detail is
in `docs/resource-model-design.md`; three things this section got wrong belong
here:

- **Bind time is the wrong hook, whatever the build order.** A binding is an
  `(owner, tag)` row with no per-sandbox instance, and the number a sandbox needs
  is the baseline of the image it was *actually* reflinked from — for most
  sandboxes the operator default, for an explicit fork a snapshot that never
  passed through `bind`. The baseline keys on the existing `host.Sandbox.Image`
  instead, which makes this part independent of Parts 2 and 3 and fixes the
  identical over-charge that already applied to the default base template.
- **On-demand is necessary and not sufficient.** `DeleteSnapshot` removes the
  template file, after which a purely on-demand read has nothing to read and
  every fork's pooled charge would jump by a whole template at the next tick. So
  it is an on-demand read *plus* a persisted `Sandbox.BaseDiskMB`, and a
  measurement error keeps the stored value rather than treating it as zero.
- **The archive case was missing.** `Archive` replaces `DiskMB` with the
  compressed artifact's size and object storage dedups nothing, so an archived
  box pays full freight and both restore paths clear the baseline.

The measurement is `vmm.TemplateReporter.TemplateUsageMB`, a new additive driver
capability kept deliberately *off* `DiskReporter` so a driver that can do one and
not the other loses the discount rather than the accounting. Firecracker reads
the template through the same `ext4DiskMB` that `DiskUsageMB` uses, and that
identity is exactly what makes the two subtractable.

**This is a live policy change on a binary swap with no flag.** It enlarges every
existing `--disk-pool-mb-per-owner` setting the moment the first reaper tick
backfills baselines. It only ever loosens — it can never refuse a create that
used to succeed — and it is the incentive this whole document is arguing for, but
an operator should re-check the number against what they meant. See the deploy
note in `docs/deploy-cks.md`.

---

# Part 7 — archiving a fork, deferred

Archive today is self-contained. `Manager.Archive` (`internal/host/manager.go:2245`)
pauses, calls `PackRootfs` — `e2fsck` + `zerofree` + `zstd` — uploads to
`<prefix>/<owner>/<name>.ext4.zst` (`manager.go:2227`) and reclaims the local VM
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

All five shipped, in this order:

1. **`sparkbox update-tools` and the `/tools` metadata endpoints** (Part 5).
   Independent of everything else, useful on its own, and the precondition that
   makes tag templates safe to adopt. Shipped first, and the refresher hoist
   shipped with it because without it the cache is empty on the freshest hosts.
2. **`template_tags` + `bind`/`unbind` + image resolution in `ctlops.Create`**
   (Parts 2 and 3), including the multi-binding refusal and the `default`
   refusal, both at write time.
3. **Placement follows the template** (Part 4), with the full-sentence error for
   the capacity case, the `--node` contradiction refusal, and the `placer`
   assertion that keeps a single-machine host building here.
4. **Quota baseline subtraction** (Part 6).
5. `snapshot create` refreshes the agent CLIs first, locally through
   `host.Manager.Snapshot` and remotely through `Fleet.Snapshot`.

Template replication across nodes (Part 4) and delta archives (Part 7) stay
written down and unbuilt. Neither has a measured workload behind it.

---

# Part 9 — the open questions, answered

- **Does `bind` re-point, or is a binding immutable?** It re-points. The primary
  key is `(owner, tag)`, so there is one binding per tag by construction; `Bind`
  returns the snapshot it replaced, refreshes `created_at`, and every surface
  prints "that tag used to boot from *x* — sandboxes already created from it are
  unaffected." The previous snapshot is left intact, which is the only thing that
  makes a bad re-point recoverable.
- **What happens to `snapshot rm` on a bound snapshot?** Refused, code
  `snapshot_bound`, naming the tags. With one hole: the user console calls
  `boxes.DeleteSnapshot` directly and never routes through `ctlops`, so it can
  still delete a bound snapshot. That is backstopped by `resolveTemplate`'s
  `template_missing` refusal, which is exactly why that refusal must never
  degrade into a silent fallback to the stock image. Routing the console through
  `ctlops` is a separate change with its own ordering bug and is not folded in
  here.
- **Cross-owner bindings.** Still out of scope, and the answer is unchanged: a
  shared or org-scoped snapshot is the real fix. Note that owner scoping is what
  reduced Part 3's blast radius from the fleet to one handle.
- **Does a bound tag deserve to show up in `list`?** It shows up on the
  *snapshot*, not on the sandbox. `snapshot ls` grew two conditional suffixes —
  `tags: …` when a snapshot is bound, `on <node>` when the host has a fleet — and
  a host with neither prints exactly the row it printed before. `SnapshotInfo.Node`
  is set only when the sandbox store can place on a named machine, so a
  single-machine host never prints "on local".

---

# Part 10 — capturing from inside the box

Everything above is an outside gesture: an owner at `ssh ctl@<gateway>` captures
somebody's box and binds the result. But the person who knows a box is worth
keeping is the person sitting in it, and the box they are sitting in is the box
that has to stop. That is the whole of this part's design constraint: **nothing
refusable may happen after the response, and nothing destructive may happen
before it.**

Two guest verbs and three metadata routes:

```
sparkbox pause                          -> POST /self/pause
sparkbox snapshot [--yes] [TAG [NAME]]  -> GET  /self/snapshot   (the plan; a pure read)
                                        -> POST /self/snapshot   (the commit)
```

## Plan, then commit

`GET /self/snapshot` mutates nothing. Every refusal a user can act on lands
there, while the sandbox is still running and the session is still open: an
unsupported driver, the feature switched off, no binding store, a bad tag,
`default`, a tag this sandbox does not carry, zero or several candidate tags, a
name already taken, the rate limit. The plan also carries the warnings, because a
warning informs a decision and the thing being warned about is the destruction of
the terminal displaying it — the tag's current snapshot and the exact `bind` that
puts it back, every sandbox of yours carrying the tag *with its state*, the
non-rebasing rule stated in full, the disk size so "a minute or two" is
quantified, and a disk operation already in flight.

That last one is a warning and never a gate. `lockDiskOperation` is a plain
blocking mutex with no busy error, so a capture issued during an archive would
otherwise return its acceptance, announce that the session is about to end, and
then leave the box running for up to fifteen minutes — the worst possible
transcript. A racy warning is fine; a racy gate is a lie.

Then the guest prompts on a TTY, `--yes` skips the question, and **without a
terminal it refuses**. A sandbox may be driven by an agent, and "there is nobody
here to read the warning, so I will not proceed" is the correct reading of a
warning's purpose. It costs a legitimate script one flag. It prevents accidents,
not attacks.

The commit re-runs the plan synchronously — so a refusal is still delivered on a
healthy connection — checks the plan token, writes the acceptance with an
explicit `Content-Length`, flushes, waits *inside the handler* for the guest's own
FIN with a 2 s fail-soft cap, and only then starts the capture on a context
detached from the request. Three mechanics there are load-bearing: without
`Content-Length` the flush goes chunked and curl waits for the terminating chunk
`net/http` writes at handler return, deadlocking into the cap on every call; a
goroutine selecting on the request context fires immediately, because `ServeHTTP`
cancels it on return; and calling the capture after the wait is what makes the
design identical on a gateway and on a node, where the chain runs guest tap →
node metadata → nodelink → gateway → back down to the node before anything
pauses.

The one residual — response written, lost in transit — is answered by a message
that claims nothing and exit 75. The guest genuinely cannot distinguish "the
connection died before the handler ran" from "the answer was written and lost".

The plan token is checked by the metadata service that answered the guest, and
on a node that service runs *on the node*. So the tag and name a node relays up
are, by themselves, a node's assertion. `Fleet.SelfSnapshot` therefore re-derives
them from the gateway's own plan and refuses a pair that plan does not produce
(`plan_stale`). That is what makes the carried-tag rule below a real cap rather
than one enforced on the machine it is meant to bound: an enrolled node already
authors the bytes of every sandbox it runs, but without the re-plan it could also
point *any* tag of those sandboxes' owners at a rootfs of its choosing — and a
tag hands its secrets to every sandbox created on it afterwards, on any machine.

## Naming

`<stem>-<YYMMDD-HHMM>` UTC, e.g. `web-260829-1412`, where the stem is the tag
truncated to 29 characters. Every legal tag is already a legal snapshot name
(`secrets.tagRe` is a strict subset of `host.snapNameRe`), so the grammar was
never the constraint — `Manager.Snapshot` refusing a name already taken is, and
the *second* `sparkbox snapshot web` is the gesture this feature exists for.
Overwriting is worse: not atomic, and it destroys the only thing that makes a bad
re-point recoverable. Two captures inside one minute collide and are refused at
plan time, which is free even when the plan races it, because the name check runs
before the strip and before the pause. The previous generation survives, unbound;
`snapshot ls` becomes that tag's history and rollback is one `bind`.

## What a compromised guest gains

The authenticator proves *which sandbox*; the owner comes from the manager
record, never the request. That is an elevation from "a machine" to "the person
who owns it", and it is narrowed structurally: `internal/metadata` is handed a
three-method `SelfLifecycle`, never an `*ctlops.Ops`, and every method takes a
`*host.Sandbox` rather than a name from the request.

Enumerated, a compromised guest gains: pausing itself (nothing new — it can
already `halt`, and this is a *cleaner* transition); snapshots as the owner,
which are **unmetered** — nothing anywhere counts snapshots, by number or by
bytes — against the image volume every VM on the machine reflinks from; a
host-side loop-mount of attacker-controlled ext4 on demand and repeatedly, which
is not a new capability but is new timing and frequency; and re-pointing one of
its owner's tags to a rootfs it controls, which is persistence made sharper by
secrets being tag-scoped and pushed at create time.

Three restrictions answer those, and the second is the one that matters:

- **`default` is refused here as well as at bind**, because every sandbox carries
  it so "a tag you carry" does not exclude it, and because the refusal has to
  land before the pause rather than as a bind failure two minutes later with
  nobody watching.
- **A guest may only re-point a tag it already carries**, checked server-side
  against the tag store and never against anything the request asserts. This caps
  the new capability at the trust the box already had: the attacker can poison
  only the tag whose secrets that box had already been given, so the foothold
  yields persistence but no new material. Without it, one compromised box owns
  every tag its owner has, including tags it was deliberately never trusted with.
  The case it blocks — founding a *new* binding from an untagged box — is minting
  rather than moving, and minting stays at the operator door the refusal prints.
- **Three commits per hour per sandbox**, in its own sliding window, deliberately
  not sharing `/token`'s or the repo pair's: a shared window lets one workload
  starve the guest's identity. The plan is limited separately and generously,
  because it is a read and it is where people discover what they may do.

Plus an operator switch, `--guest-self-snapshot`, default **on**: the carried-tag
rule already bounds this, and a self-service feature nobody is told about does
not exist. The flag is for handing boxes to third parties. If the carried-tag
rule is ever relaxed, the default must flip with it.

The guest surface is capture and re-point only. There is no `snapshot rm` and no
`unbind` from inside a box.

## One implementation, and the half-failure

`Ops.SnapshotToTag` is the single composite all three doors go through — the
guest verb, `ssh ctl@<gateway> snapshot create <box> <name> --tag <t>`, and the
plan endpoint's own re-run. It calls the existing `CreateSnapshot` and then
`BindTemplate` and reimplements neither. Composing in a transport adapter would
have put the ordering *and* the compensation policy in `cmd/sparkbox` and
produced two audit lines for one user gesture.

Capture first, bind second, and it cannot be the other way: binding first would
leave the tag pointing at an image that does not exist, so every create on that
tag in that window resolves to a missing rootfs — a failure that spreads forward.

If the bind fails after the capture succeeded, **the snapshot is kept**. The
expensive, unrepeatable half succeeded — the box is now paused, and a re-run
captures a different filesystem — so the composite returns the populated snapshot
*and* a typed `snapshot_not_bound` error whose sentence carries the one-command
repair. `snapshot ls` then shows the truth on its own: the new snapshot with an
empty tag column above the one that still holds the tag. `ls` was deliberately
not taught a "pending bind" state, which would be a fourth durable thing to keep
consistent.

## The uncomfortable interaction

`/usr/local/bin/sparkbox` lives in the rootfs, and the refresher deliberately
never patches `snap-*.ext4`. So a box forked from a snapshot taken before this
shipped will never have these verbs, and one forked after is frozen at that day's
CLI. Tag templates turn "my CLI is old" from a curiosity into the norm, which is
the strongest argument in the tree for Part 5 — and it is why `IDENTITY_REV` is
now 11: a bump is what makes the refresher re-patch every base template, and
`deploy/assets_test.go` pins the literal so forgetting it fails the build rather
than shipping to nobody.

---

# Part 11 — the surfaces, and the codes that are now frozen

Every verb, on every door:

| | |
| --- | --- |
| ssh | `snapshot bind <name> --tag <t>` · `snapshot unbind --tag <t>` · `snapshot create <box> <name> [--tag <t>]` · `snapshot ls` (bound tags + node) |
| REST | `PUT /v1/templates/{tag}` · `DELETE /v1/templates/{tag}` · `bound_tags` and `node` on `Snapshot` · `template_tags` on `Capabilities` |
| user console | a read-only bound-tags column on the snapshot list; no bind or unbind this milestone |
| guest | `sparkbox snapshot [--yes] [TAG [NAME]]` · `sparkbox pause` · `sparkbox update-tools [--check]` |
| metadata | `GET /tools/manifest` · `GET /tools/{name}` · `GET`/`POST /self/snapshot` · `POST /self/pause` |
| flags | `--tools-dir` (empty serves no cache and answers 501) · `--guest-self-snapshot` (default on) |

The bindings live at `/v1/templates/{tag}` rather than under `/v1/snapshots`, and
that is not a taste question: a sibling like `/v1/snapshots/bindings/{tag}` gives
Go 1.22's mux two four-segment patterns neither of which is more specific than
the other — that one and `/v1/snapshots/{name}/fork` — which it rejects at
registration. The server would not boot. Do not tidy them back.

`AGENTS.md` makes an error `code` a compatibility surface once shipped: clients
match on codes rather than parsing messages. These are the new ones.

| code | kind | what it means |
| --- | --- | --- |
| `template_ambiguous` | conflict | the create's tags bind two different snapshots; both are named, and nothing was written |
| `template_missing` | conflict | the tag's binding points at a snapshot that is gone. Never a silent fallback to the stock image |
| `template_node_conflict` | conflict | an explicit `--node` contradicts the machine the bound template lives on |
| `snapshot_bound` | conflict | `snapshot rm` on a snapshot a tag boots from; unbind first |
| `template_binding_not_found` | not found | no such binding — the same answer another owner's tag gets |
| `bad_binding` | invalid | the store refused the binding; its own sentence passes through verbatim, `default` above all |
| `missing_tag` | invalid | `bind`/`unbind` needs exactly one tag and got none |
| `too_many_tags` | invalid | …and got more than one. A tag has one template, so naming two says nothing |
| `snapshot_not_bound` | conflict | the capture succeeded and the bind did not; the snapshot is kept and the sentence carries the repair |
| `snapshot_name_taken` | conflict | the derived `<tag>-<YYMMDD-HHMM>` already exists — wait a minute, or name it |
| `tag_is_default` | denied | `default` cannot carry a template, refused at the guest door as well as at bind |
| `tag_not_on_sandbox` | denied | a guest may only re-point a tag it already carries; the tags it does carry are listed |
| `plan_stale` | conflict | the world moved between the plan and the commit; re-read the plan |
| `self_snapshot_rate_limited` | limit | three captures per hour per sandbox |

---

# Part 12 — two gaps this work found and did not create

Neither of these was introduced by tag templates and neither is fixed by them.
Both were found while reading the code this feature sits next to, and both are
recorded here so the next person does not have to find them again.

**A snapshot or archive captured on a fleet node never strips the managed secret
block.** `host.Manager.stripEnvForPack` type-asserts `m.envSync`, and a node
never calls `SetEnvSync` — deliberately, because an owner's decrypted secrets
must not sit on every machine that happens to run one of their sandboxes. The
consequence is that on a node the strip is a no-op, so a template captured there
carries plaintext secret values in `/etc/environment` and **every fork copies
them**. Combined with tag templates, that means a bound template can hand one
owner's credentials to every sandbox created from it. This is a cross-tenant
credential leak and it is the highest-priority follow-up in this area. The
gateway-side pre-capture hook Part 5 added is the *shape* of the fix — a
gateway-side step over the same channel — but it is a separate change with its
own tests and was deliberately not smuggled in. See
`docs/security-hardening.md`.

**A snapshot captured on a remote node is invisible to the gateway until that
node's link reconnects.** The gateway's picture of a node's templates is the
node's last-sent inventory, and a node sends a full inventory on connect and when
its event queue overflows — there is no snapshot event and no periodic resync. So
after a remote capture, `snapshot ls` does not list the new snapshot, and `bind`
of it is masked as not-found, because `ownedSnapshot` answers a masked not-found
for anything not in the owner's list. That masking is correct — it is what stops
a refusal confirming another owner's snapshot exists — but here it makes a
timing problem read like a permissions bug. The fix is an inventory refresh (or a
snapshot event) after a remote capture; it is not made here.
