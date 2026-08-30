# Repositories that survive a template

How a checkout stops being a thing that happens once, at clone time, and becomes
a thing the platform keeps current — across a fork of somebody's snapshot, and
across a create that asks for a different branch.

This is a proposal. Nothing in it is built. It sits on top of
`docs/tag-templates-design.md` (Parts 2 and 5 shipped on `feat/tag-templates`)
and `docs/github-repos-design.md`, and it exists because those two features
compose into a problem neither of them has on its own.

---

# Part 1 — the problem the two features make together

## What each half does today

`internal/repos` attaches a GitHub repository to a **tag**, and every sandbox
carrying that tag arrives with the checkout in it. The guest fetches its own
manifest from the metadata service on its tap, mints a one-hour installation
token per repository through the credential helper, and clones
(`deploy/install-guest-identity.sh:1159`). Three callers run the same
reconciliation — `sparkbox-repos.service` at boot, `sparkbox repos sync` by
hand, and the gateway nudging a live box after a retag
(`internal/envsync/repos.go:76`).

Tag templates make a tag select the **rootfs** as well. `ssh new+cuda@gateway`
boots a snapshot somebody captured, reflinked, for free.

## What they do together

A snapshot of a box that had a repo attached contains **the checkout**. Every
fork of that snapshot therefore starts with `~/hivemind` already on disk — at
whatever commit, on whatever branch, in whatever state of dirt the source box
was in at the moment somebody typed `snapshot create`.

And the reconciler leaves it alone. Deliberately, in writing
(`install-guest-identity.sh:1137-1141`):

> Anything already at EITHER default location is left exactly as it is, git repo
> or not, and is reported where it actually sits.

That rule is correct for the case it was written for. The gateway restarts
`sparkbox-repos.service` against a filesystem somebody is working in, so
"reconcile" has to mean **add what is missing** and nothing else. But its
consequence under tag templates is the same shape as the tool-drift problem that
Part 5 of the tag-templates doc had to solve: the moment a team's tag resolves to
a snapshot, every sandbox that team creates arrives with a repository frozen at
the commit somebody captured, and it stays frozen forever. The reconciler looks
at it, says `present`, and moves on. `git log` in a two-week-old fork shows
two-week-old history and nothing anywhere says why.

Worse than stale, occasionally: a template captured mid-operation freezes
`.git/index.lock`, `.git/rebase-merge/` or a `MERGE_HEAD` into the image, and
every fork of it inherits a git that refuses to run. The pause is a hard freeze;
it does not care what git was doing.

## And the thing nobody can ask for at all

The attachment's `ref` is a **clone argument** — it becomes `git clone --branch
<ref>` and then it is over (`install-guest-identity.sh:1160`). There is no way
to say "give me this box with `feat/x` checked out". The nearest available
answer is: create the box, wait for the clone of `main`, then `git switch`
yourself. On a fork it is worse, because there was no clone to influence.

Three asks, then:

1. A fork must be able to **update** the checkout it inherited.
2. A create must be able to **choose the branch**, per instance.
3. A capture should leave a template whose checkouts are in a state the first
   two can actually act on.

---

# Part 2 — the invariant, stated before any mechanism

**The reconciler must never be able to lose work.**

This is not a guideline to weigh against convenience. `sparkbox-repos` runs as
root, unattended, on a filesystem a person is using, triggered by events they did
not cause — a retag by a teammate, a boot after a reaper pause. It has no
terminal to ask in and no way to be undone. Any design in which it can run `git
reset --hard`, `git clean -fd`, `git checkout -f`, `git rebase`, or a merge that
is not a fast-forward is wrong, and no amount of "but the user asked for
`--ref`" changes it.

So every mechanism below reduces to one of three acts:

| act | when |
| --- | --- |
| `git fetch` | always safe; the objects are additive |
| fast-forward | only a clean tree, only when the target strictly descends from HEAD |
| **say something** | every other case |

Third row is not the fallback. It is the feature. A fork that arrives with a
dirty inherited tree and a banner line saying so is a good outcome; a fork that
arrives clean because something threw the dirt away is an incident.

---

# Part 3 — the refresh: what "present" should become

The single-line change is that `present` splits into a small vocabulary, and the
worker gains a second half. The scan that finds an existing checkout
(`install-guest-identity.sh:1137`, the three-candidate loop that already exists
so that changing the attachment count does not orphan a week of work) stays
exactly as it is. What happens after it finds one is new.

For a checkout that exists, at path `$dest`, with a desired ref `$ref` (`""`
meaning "the repository's default branch"):

```
not a git worktree at all      -> report `present`, touch nothing, ever
a fetch that fails             -> report `offline`, touch nothing
dirty (see below)              -> fetch, report `dirty`, touch nothing
mid-operation (rebase/merge/
  bisect/cherry-pick in flight)-> report `busy`, do not even fetch
HEAD detached                  -> fetch, report `detached`, touch nothing
on $ref, ff possible           -> fast-forward, report `updated`
on $ref, diverged or ahead     -> report `ahead`, touch nothing
on some other branch           -> ADOPT MODE ONLY (Part 4); else report `moved`
```

"Dirty" is `git status --porcelain` non-empty, which includes untracked files on
purpose. An untracked `notes.md` is not a reason to refuse a fast-forward in
principle, but distinguishing "untracked file that would be clobbered by the
merge" from "untracked file that would not" is git's job and git only does it
inside the operation. Treating any dirt as a stop is one rule instead of two and
it errs in the safe direction.

The fast-forward is `git merge --ff-only <upstream>` after a `git fetch`, never
`git pull` (which will happily merge or rebase depending on config the user set),
and never `git reset --hard origin/<ref>` (which is a fast-forward exactly until
it silently is not).

Two mechanical notes:

- **The clone is blobless** (`--filter=blob:none`, and the comment there explains
  why: agents run `git log`, `git blame`, `git bisect`). A fast-forward
  therefore needs the network to materialize blobs for the files it touches. That
  is already true of every checkout in this system and is why `offline` is a
  state rather than an error.
- **The credential helper already covers fetch.** Git consults it per host per
  operation, so nothing new is minted, routed or stored to make this work. The
  egress overlay already unions `github.com`, `codeload.github.com` and
  `objects.githubusercontent.com` for a tagged sandbox (`internal/repos/store.go`,
  `cloneDomains`), and a fetch talks to exactly those.

A checkout the manifest no longer mentions — the repository was detached, or the
fork's tags do not select it — is reported `unmanaged` and is never touched, on
the same rule that `repo rm` promises the existing clone "is left alone".

---

# Part 4 — adoption, and the one moment nobody is working in the tree

`moved` is the state that matters for ask #2, and refusing it forever makes
`--ref` impossible: a fork's inherited checkout is on `main` and the user asked
for `feat/x`, so something has to switch branches under a tree it did not create.

There is exactly one moment when that is safe: **the first boot of a rootfs that
was just laid down from a template.** Nobody has logged in. Nothing is running.
The tree is whatever the capture froze and no person's work is in flight.

So the worker runs in one of two modes:

- **refresh** (every ordinary sync): the table in Part 3, with `moved` reported
  and never acted on. Switching a branch under a live box because a manifest says
  so is the reset-hard mistake wearing a different hat.
- **adopt** (first boot of a fresh disk): `moved` becomes an action —
  `git fetch origin <ref>` then `git switch <ref>` (creating a tracking branch
  when it exists only on the remote), and then the ordinary fast-forward. Still
  gated on a clean tree: an inherited dirty tree in adopt mode is reported
  `dirty` and left, because the dirt is somebody's captured work and a fork is not
  the place to discover it is gone.

Adopt mode also clears the inherited-lock class of damage, and only here:
`.git/index.lock` and `.git/shallow.lock` whose owning process demonstrably does
not exist in this kernel are removed. An in-flight rebase or merge is **not**
aborted — that is a decision with a wrong answer — it is reported `busy` with the
sentence naming `git rebase --abort` and the directory to run it in.

## How the guest knows it is a first boot

Not by guessing, and not from the stamp that already exists.
`sparkbox-identity-reset` (`install-guest-identity.sh:1217`) compares
`sparkbox_host=` on the kernel command line against `/var/lib/sparkbox/sandbox`
and sheds inherited identity on a mismatch. That marker is exactly right for what
it does and wrong here, because it fires on a **rename** too, and a rename must
not switch anybody's branch. The narrow but real regression: somebody
`git switch`es to their own branch, leaves the tree clean, gets renamed, and
comes back on `main`.

The host already knows the difference and should say so. `Driver.Create`
reflinks the rootfs only when one is not already there
(`internal/vmm/firecracker/fc.go`, the `os.Stat(rootfs)` guard before
`reflinkClone`); every other path into `boot` — resume, reboot via
`DropSnapshots`, checkpoint restore — runs against a disk that already exists. So
that boolean, threaded from `Create` into `boot`, becomes one more kernel arg:

```
sparkbox_fresh=1
```

written by the host, unforgeable by a guest, present on exactly the boots where
adoption is safe, and absent everywhere else. A host that predates it produces no
arg and every guest stays in refresh mode, which is the correct degradation:
forks keep the branch they inherited and say `moved`.

The one place to be careful in implementation is that `boot` is called on more
than one path and the argument must default to **off** at each of them. A
`sparkbox_fresh=1` that leaks onto a reboot is the branch-yank this whole section
exists to prevent.

---

# Part 5 — `--ref`, and where a per-instance override lives

## The door

```
ssh new+box@gateway --tag cuda --ref feat/x
ssh ctl@gateway fork cuda-12 box --ref feat/x
```

`--ref` parses in `splitNodeFlag`'s shape (`internal/sshgw/tags.go:92`) — the two
spellings, the consume-the-next-argument rule, last-wins — because a door's flags
should not each have their own dialect. For the multi-repository case it takes a
scoped form:

```
--ref wandb/hivemind=feat/x --ref wandb/other=main
```

A bare `--ref` applies to every attachment the sandbox's tags select; a scoped one
names its repository and wins over a bare one. A slug that matches no attachment
is a **refusal**, not a silent no-op: a create that ignores a flag somebody typed
is the bug this rule exists to prevent, and it is discoverable in the same place
and at the same moment as the multi-binding refusal in `ctlops.Create`.

## Where it is stored

A fifth table in `internal/repos`, and the only one keyed by a sandbox rather
than by a tag:

```sql
CREATE TABLE IF NOT EXISTS sandbox_repo_refs (
  owner      TEXT NOT NULL,
  sandbox    TEXT NOT NULL,
  host       TEXT NOT NULL,
  slug       TEXT NOT NULL COLLATE NOCASE,
  ref        TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  PRIMARY KEY (owner, sandbox, host, slug)
);
```

applied as an overlay inside `ReposForSandbox` (`internal/repos/store.go:381`),
owner on both sides of the join like every other query in that file. That
placement is the point of the whole design: **nothing else changes.**
`LocalRepos.Manifest` (`internal/metadata/repos.go:157`) already returns
`RepoEntry.Ref`, `fleet.RepoAttachment` already carries `Ref` across the node
link, and the guest already reads it. No new endpoint, no new fleet capability,
no new field on the wire — the per-instance branch reaches a sandbox on any
machine in the fleet the day the overlay lands.

Precedence, most specific first: the sandbox override, then the attachment's own
`ref`, then the repository's default branch.

## The cost, named

A per-sandbox table needs a per-sandbox lifecycle, and this is the part that is
easy to skip and expensive to skip. Sandbox names are **reusable**: destroy
`box`, create `box`, and a stale override row silently decides what the new
sandbox checks out. `internal/host` already has the shape for this — the manager
holds `DeleteBySandbox`/`RenameSandbox` interfaces and calls them on destroy and
rename for routes, schedules and tags (`internal/host/manager.go:2518-2528`,
`:2178`) — so this is a fourth registration in an existing list rather than a
new mechanism. It is still a wire that must be connected, and a test that proves
the row dies with the sandbox belongs next to the one proving tag rows do.

`internal/repos` writing this table does **not** violate the standing rule in its
package comment. That rule is specifically about `sandbox_tags`, whose mutations
`internal/secrets` owns; a table this package creates and owns outright is the
same arrangement `repo_tags` already has.

## What deliberately does not get built

A `ctl` verb to re-point a running box's ref. Inside the box, `git switch` is the
answer, it is one word, and it is the answer a person will reach for anyway. A
control-plane verb for it would exist only to be the thing that fights with what
the user did by hand.

---

# Part 6 — capture, and the honest version of "clean state on main"

The tempting version is that `snapshot create` gets the repositories to a clean
state on the default branch so that forks start from a known point. It is
tempting because it makes Parts 3 and 4 trivial. It is wrong, for a reason that
has nothing to do with git: **the capture does not own the source box.** The
sandbox being captured is one somebody is working in — that is precisely why they
have something worth capturing — and it is still theirs after the pause. A
capture path that commits, stashes, resets or checks out on their behalf is the
reconciler's forbidden act with a nicer trigger.

So the capture does three things, and all three are non-destructive.

**One: it fetches.** A sibling of `refreshToolsForPack`
(`internal/host/archive.go:111`), in the same position and under the same four
ordering constraints that comment already spells out — after `stripEnvForPack`
(the only step that may wake a paused guest safely), before the pause (the last
moment the guest is reachable), synchronous (a pause landing mid-write freezes a
torn object into a template every fork copies), and inside `lockDiskOperation`.
Best-effort, a `WARN` and carry on, for the same reason: a snapshot that failed
because GitHub was slow is worse than a snapshot whose objects are an hour old.

The payoff is real and it is not tidiness. A template whose object store is
current makes every fork's first fetch incremental instead of a month of history,
and adopt-mode's `git switch` to a branch that already exists locally needs
almost nothing from the network.

**Two: it reports, in the session, before the prompt.** `sparkbox snapshot <tag>`
already has the right shape for this: `PlanSelfSnapshot`
(`internal/ctlops/selfsnapshot.go`) is a pure read that answers every refusal a
person can act on **while the VM is fully alive and the terminal still exists**,
precisely because the box that issues the request is the box that stops. The
gateway cannot see inside the guest's filesystem, so the survey is local — the
guest's `sparkbox` wrapper runs it before printing the plan, and adds a section:

```
repos:
  ~/hivemind          feat/x   3 modified, 1 untracked   will be captured as-is
  ~/src/wandb/other   main     clean, 2 commits ahead    unpushed
```

`--yes` skips the prompt and not the survey; the lines still print. Nobody is
blocked and nobody is surprised later.

**Three: it refuses one thing.** A checkout with a rebase, merge, cherry-pick or
bisect in flight fails the plan, with the directory and `git rebase --abort` in
the message. This is the one state where "capture it as-is" produces a template
that is actively broken for every fork rather than merely stale, and the plan is
the only place to say so where somebody is still there to read it. `--allow-busy`
exists for the person who genuinely means it.

`ctl snapshot create <box> <name>` — the operator door, run from outside — gets
the fetch and not the survey. There is no session inside the guest to print into,
and the gateway is not going to grow a way to read a guest's working trees.

---

# Part 7 — what the reconciler says out loud

The banner is one line (`status_line`), the detail is `sparkbox repos`, and the
vocabulary above has to survive the trip to both. The counts collapse to three
buckets, because a banner read in the second before somebody starts typing cannot
carry seven:

- **ready** — `present`, `updated`, `unmanaged`
- **stale** — `dirty`, `ahead`, `moved`, `detached`, `offline`
- **failed** — clone failed, `busy`

with `stale` and `failed` both appending the existing `— run \`sparkbox repos\``
pointer, and the per-repository table carrying the real word and the sentence
that fixes it. `busy` counting as failed rather than stale is deliberate: an
inherited half-finished rebase is a thing to go and deal with, not a thing to be
told about twice a day.

The `sync` path keeps the guarantee it has now — a sync that cannot run is a
warning and never a failed boot, because this unit is ordered beside sshd and
exiting nonzero would mark the machine degraded over a checkout that is still
fixable the next time anybody looks.

---

# Part 8 — out of scope, on purpose

- **Submodules and LFS.** A blobless clone plus `--recurse-submodules` plus a
  per-repository installation token is three interacting things and each one has
  its own credential story. Today neither works; this proposal does not make
  either worse and does not fix them.
- **Pushing.** `access: write` mints a `contents: write` token and that is all it
  has ever meant. Nothing here pushes on anybody's behalf, at capture or
  otherwise, and the `unpushed` line in the survey is a statement, not an offer.
- **Cross-owner templates.** A shared base image is the real answer to a team
  wanting one checkout, and it is out of scope in the tag-templates doc for the
  same reason it is out of scope here.
- **Worktrees.** `git worktree` in a captured checkout has absolute paths in
  `.git/worktrees/*/gitdir`. They survive a fork intact, because the fork's home
  directory is at the same path. Noted so the next person does not go looking.

---

# Part 9 — build order

1. **The refresh half** (Part 3) — the state vocabulary, fetch, fast-forward-only,
   and the reporting in Part 7. Useful the day it lands on every long-lived box
   that has drifted, entirely independent of templates, and it is the piece that
   proves the never-lose-work rule under real trees before anything is allowed to
   switch a branch.
2. **`sparkbox_fresh=1` and adopt mode** (Part 4). Small on the host, and the
   thing to test hardest is that no non-Create path emits the arg.
3. **The capture survey and the pre-capture fetch** (Part 6). Independent of 4,
   and it is what makes a captured template worth adopting from.
4. **`--ref` and `sandbox_repo_refs`** (Part 5), including the destroy/rename
   registration and the no-such-attachment refusal. Last, because until 1 and 2
   exist it can only affect a clone that was going to happen anyway — which is a
   feature that looks finished and silently does nothing on the fork path, the
   exact failure mode tag templates shipped with.

---

# Part 10 — open questions

- **Should adopt mode run on a create from a stock template too?** It is a no-op
  there — the home directory is empty and the clone takes `--branch <ref>` — so
  the answer only matters if the stock template ever ships with a checkout in it.
  Making the mode unconditional on a fresh disk is one fewer branch and is
  probably right.
- **Does `--ref` on a tag with several attachments and no scoped spelling mean
  "all of them" or is it a refusal?** "All" is convenient and is wrong more often
  as the attachment count grows. Refusing above one attachment, with the scoped
  form in the message, is the conservative read and costs a person one retype.
- **Should a `moved` checkout in refresh mode ever converge?** A box that has sat
  on `feat/x` for a month while the manifest says `main` is arguably drift the
  platform should mention every time and arguably a decision the user made once.
  It is currently a banner line forever, which is the annoying-but-safe answer.
- **Does the survey belong in `ctl snapshot create` as an opt-in?** It would mean
  the gateway asking a guest to describe its own working trees over the exec
  channel, which is a new kind of thing for that channel to carry. Probably no,
  but it is the obvious request the first time somebody captures a broken template
  from outside.
