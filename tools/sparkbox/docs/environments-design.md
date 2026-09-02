# Environments: the noun the tag was standing in for

How the four things a project needs to run — its credentials, its checkouts, its
egress policy and its disk — stop being four unrelated gestures against a tag
somebody invented, and become one object with a name, a description, and a
`ctl env show` that prints all of it.

## Status

**Phase A is built** on `feat/sparkbox-environments`: `internal/envs`, the `Var`
half of `internal/secrets`, the `ctlops` operations and interfaces over both,
the `ctl env` command family, `--env` on the `new@` door, and the `main.go`
wiring. `gofmt`, `go build ./...`, `go vet ./...` and `go test ./...` are all
clean. Phase D's REST routes and console tab are NOT built — the only thing this
change adds to `openapi.json` is the `environments` capability flag, because
`Capabilities` grew a field and `openapi_test.go` requires the spec to match.

**Phase B is built too, in script mode only.** `ctl env build <name>` and
`ctl env capture <name>` exist and run end to end against the mock driver:
`ghapp.App.ReadFile` (the seed read of `.sparkbox/setup.sh`, which returns bytes
and never a token), `metadata`'s `GET /self/setup` and
`POST /self/setup/result`, the guest's `/usr/local/sbin/sparkbox-env-setup`
oneshot and its deliberately-unenabled `sparkbox-env-setup.service`,
`envsync.StartSetup`, and `ctlops`'s `BuildEnvironment`, `SetupFor`,
`SetupDone`, `CaptureEnvironment` and `ReconcileEnvironmentBuilds`, wired in
`main.go` behind `--env-build-timeout` (default 45m) with a ten-minute
reconciler sweep. Part 4 below has been rewritten to describe what was built;
where it and the code disagreed, the code won and the paragraph says so.

**Phase C — agent mode — is built.** A build with no script anywhere no longer
refuses; it runs `claude -p` in the builder against this platform's own
dev-environment guidance and keeps the `.sparkbox/setup.sh` the agent writes.
`ctl env rebuild` exists as a second name for `env build`, and Part 5(b)'s three
mitigations are real rather than asserted. Two claims that Part 5 made about
this phase turned out to be FALSE when the code was read, and both are corrected
where they appear: the "governed by construction" claim (an environment with no
rule-set was unrestricted, not governed) and the timeout ladder (the gateway
gave up *before* the guest's own budget). Phase C also found that
`--permission-mode auto` is silently downgraded to `default` under `-p`, which
is why the invocation carries `bypassPermissions` and why success is judged by
the artifact rather than the exit status.

**Phase D — REST routes and the console tab — is still NOT built.** The only
thing environments add to `openapi.json` is the `environments` capability flag.

Part 5's two posture changes are therefore half-taken: (a) we now run a script
from a repository, unattended, in a VM holding the owner's secrets. (b) we do
not yet run an agent.

The schema in Part 1 carried Phase B's columns from the first `CREATE TABLE`,
because this tree has no migration framework: schema is
`CREATE TABLE IF NOT EXISTS` at `Open` plus a per-package copy of
`addColumnIfMissing` kept alive with `var _ = addColumnIfMissing`
(`internal/secrets/store.go:273-278`). Adding a column later is possible and
tedious; adding it now was free, and this is the phase that collected on it.

**Nothing in this document has been run on a real host, and for Phase B that
matters far more than it did for Phase A.** Every test in the tree runs against
the mock driver with no KVM: no builder VM has booted, no setup script has run,
no `runuser` privilege drop has happened, and no rootfs has been captured from a
box a script had just modified. The interesting failure of this feature — a VM
that boots, runs somebody's shell script, and does not come back — is precisely
the one `go test ./...` cannot reach. The reconciler exists because of it.

One correction to a sibling document while we are here.
`docs/tag-templates-design.md`'s Status section says "**None of it does anything
on CKS**" because `Driver.Snapshot` refuses outright under
`DisableHostRootfsMounts`. That has not been true since
`docs/cks-snapshot-design.md` shipped: `internal/vmm/firecracker/fc.go:1291-1296`
now skips only `sanitizeTemplate` under that flag and runs the rest of the
capture, and the identity regeneration moved into the guest's first boot. Tag
templates work on the cluster. Part 7 has the caveat that replaced it, which is
a different and smaller one.

---

# Why

The low-level primitives are right. Four independent readers already join
through one `sandbox_tags` table, each owning its own side table, each with
owner scoping structural in the SQL rather than checked by a handler
(`internal/repos/store.go:400-418`). A tag has been the correct answer to "which
of my things reach which of my boxes" four times running, and this document does
not propose replacing it.

What is missing is the noun. Here is what a person does today to stand up one
project. Count the steps and then read what each of them is named after.

1. `claude setup-token | ssh ctl@<gateway> secret set CLAUDE_CODE_OAUTH_TOKEN --tag proj`
   — the tip the gateway itself prints (`internal/sshgw/secret.go:61`).
2. The same verb again for every other credential the project needs, one at a
   time, each one sealed under the fleet KEK because `secret set` is the only
   door and it encrypts unconditionally. `DATABASE_HOST`,
   `NEXT_PUBLIC_API_URL` and `PORT` are not secrets and there is nowhere else to
   put them.
3. `ssh ctl@<gateway> repo add wandb/hivemind --tag proj` — and if there are
   three repositories, three times.
4. Egress rules, which have **no `ctl` and no REST surface at all** and are
   reachable only from the user console at `my.<domain>`
   (`docs/github-repos-design.md` Part 5 says so out loud; `internal/sshgw` and
   `internal/restapi` import `internal/netrules` nowhere). So step 4 happens in
   a different browser tab, under a different mental model, at a different time.
5. `ssh new+box@<gateway> --tag proj` — a sandbox, finally.
6. Inside it: install the dependencies, write the `systemd --user` unit so the
   dev server outlives the SSH session, and `sparkbox set-port 5173` so the URL
   a person opens is the thing they should look at
   (`internal/guestdocs/dev-environment.md:62-102`).
7. Write all of step 6 down as `.sparkbox/setup.sh` and commit it to the
   project's repo, because otherwise the next VM re-derives it
   (`dev-environment.md:88-102`). Sparkbox does not read this file, write it, or
   run it — the example's own header says so
   (`internal/guestdocs/examples/.sparkbox/setup.sh:4-6`).
8. `sparkbox snapshot proj` from inside the box, which captures the disk and
   binds it to the tag in one act (`internal/ctlops/template.go:195-238`), so
   that the *next* box skips steps 5 through 7.

Eight steps, four stores, two transports and one shell script the platform
pretends not to know about. And the name of the thing being built appears in
exactly one place: as the argument `proj` to five different verbs that have
nothing else in common.

That word is doing all the work and it is not an object. It has no description,
so nobody can find out what `proj` is except by reading five listings and
intersecting them. It has no lifecycle, so deleting it means remembering all
five. It has no state, so "is this ready?" is a question with no one to ask. And
it has no answer at all for step 2's non-secrets or step 7's script.

**The primitive is right. The noun is missing.** This document adds the noun and
changes nothing underneath it.

---

# Part 1 — the object

An **environment** is a row. It owns exactly one tag, and **its name IS the
tag**.

```sql
CREATE TABLE IF NOT EXISTS environments (
  owner        TEXT NOT NULL,
  name         TEXT NOT NULL,      -- IS the tag, in the shared sandbox_tags namespace
  description  TEXT NOT NULL DEFAULT '',
  setup_script TEXT NOT NULL DEFAULT '',   -- the captured .sparkbox/setup.sh
  setup_from   TEXT NOT NULL DEFAULT '',   -- repo | agent | manual | ''
  state        TEXT NOT NULL DEFAULT 'draft',
  build_box    TEXT NOT NULL DEFAULT '',
  build_error  TEXT NOT NULL DEFAULT '',
  built_at     TIMESTAMP,
  created_at   TIMESTAMP NOT NULL,
  updated_at   TIMESTAMP NOT NULL,
  PRIMARY KEY (owner, name)
);
```

`PRIMARY KEY (owner, name)` where `name` is the tag is the whole design in one
line, exactly as `PRIMARY KEY (owner, tag)` was for `template_tags`
(`docs/tag-templates-design.md` Part 2). Everything else in this document is a
consequence of it.

`internal/envs` is the **fifth reader** of `sandbox_tags` and it reads only. It
copies the DDL byte-identically, opens the same database file with the same DSN
(`file:`+path+`?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)`,
plus the three explicit `PRAGMA` execs whose rationale is at
`internal/secrets/store.go:186-196`), and never writes a tag row.
`internal/secrets` owns the mutations, and the reason is stated identically in
three package comments already (`internal/repos/store.go:11-16`,
`internal/templates/store.go:10-15`): a second writer bypasses secrets'
in-transaction cross-owner refusal and the invariant that a tag belongs to
exactly one handle is gone.

`EnvironmentsForSandbox(sandbox, owner)` is the join, with owner on both sides
like every other one in the tree:

```sql
SELECT e.* FROM environments e
JOIN sandbox_tags bt ON bt.tag = e.name AND bt.owner = e.owner
WHERE bt.sandbox = ? AND e.owner = ?
```

The first term of that `AND` looks redundant next to the `WHERE` and is not.
`internal/repos/store.go:400-405` explains why at length and the explanation
transfers: without it, any tag name two people happen to share — `ci`, `prod`,
`dev` — joins their rows together. What leaks here is smaller than a private
repository slug, but it is not nothing: an environment's description and its
setup script, which is somebody's infrastructure written in prose and shell.

## The alternative, and why it is refused

The obvious richer design is an environment that **composes several tags**: an
environment `web` made of tags `base`, `node20` and `web-secrets`, so tags stay
the composable primitive and environments are the convenient bundle over them.
It is more expressive. It is wrong, and the argument is already written down in
this tree.

`docs/tag-templates-design.md` Part 3, and the package comment at
`internal/templates/store.go:17-25`, make it. Every other reader of
`sandbox_tags` **adds** — two tags mean the union of two secret sets, the union
of two repo lists. `internal/netrules` subtracts. But a template does neither:
it **replaces**, because a sandbox has exactly one rootfs, and `--tag cuda --tag
node20` with both bound has no answer that is not a coin flip. So a create whose
tags resolve to more than one distinct snapshot is refused outright —
`template_ambiguous`, at `internal/ctlops/binding.go:170-177`, before any write —
rather than papered over with a precedence rule, because "a precedence rule
means somebody gets a sandbox with the wrong CUDA in it and finds out twenty
minutes later."

**An environment binds a template.** That is not an optional feature of it; it
is Part 4's entire reason to exist and it is what makes step 8 of the eight
disappear. So a multi-tag environment inherits the coin flip immediately, and
there are only two ways to hold it:

- **Each member tag may carry its own binding, and the environment refuses when
  more than one does.** Then the composition the design was sold on is exactly
  the thing that breaks it. `env add-tag node20` becomes an operation that can
  brick an environment for a reason involving a fourth store the user was not
  looking at.
- **The environment owns the binding, and its member tags do not.** Then two
  environments that share a member tag can both apply to one sandbox, and the
  ambiguity is back — one level deeper, where the user cannot see it, because
  the thing in conflict is no longer named by anything they typed.

Either way you have re-derived `template_ambiguous` and made it harder to
explain. One tag, and the refusal stays where it already is, in the store that
already owns it.

Two smaller arguments point the same way.

`ctl tags <box> a b c` **replaces the whole set**, and `--clear` empties it
(`internal/sshgw/control.go:861-862`). If an environment were a set of tags,
then "which environments is this box in" is subset matching over a set the user
edits one word at a time, and `env show` has to invent a vocabulary for partial
membership. With name == tag it is one join and the answer is a list.

And the launch door. A repo attachment carries `Tags`, and
`internal/launch/create.go:159-169` hands them to `ctlops.Create` **verbatim**,
from the store, under the session's own handle. If an environment were several
tags, an attachment would have to carry all of them or a clicker lands in half
an environment — half its secrets, none of its rootfs. Part 6 is about how much
falls out for free from name == tag, and this is why it does.

## What the decision costs, stated

You cannot say "environments `web` and `api` share these three secrets" by
naming a shared environment. You say it on the attachment, which is where the
tag set already lives: `secret set STRIPE_KEY --tag web --tag api` puts one
sealed value in both, and `repo add wandb/hivemind --tag web --tag api` puts one
checkout in both. `secret_tags`, `repo_tags` and `network_rule_tags` are all
many-to-many, so three of the four joins compose exactly as they did before.

Only the template does not, and that is the one that could never have been
shared anyway — for the reason above. Composition did not go away; it moved onto
the attachment, which is the object that was already carrying a tag *set*.

---

# Part 2 — what composes and what replaces

An environment is a name over five readers. Four exist; the fifth is new.

| reader | side table | semantics | what two tags on one sandbox mean |
| --- | --- | --- | --- |
| `internal/secrets` | `secret_tags` | **union** | both secret sets, decrypted into `/etc/environment` |
| `internal/repos` | `repo_tags` | **union** | both checkout manifests |
| `internal/netrules` | `network_rule_tags` | **subtractive** | a *governed* box filtered to the union of the allow lists, where an ungoverned one has open egress (`internal/netrules/store.go:431-449`) |
| `internal/templates` | `template_tags` | **replaces** | refused if they name different snapshots (`ctlops/binding.go:170-177`) |
| `internal/secrets` (new) | `env_vars` | **union**, losing to secrets on a name collision | both var sets, then secrets written over the top |

The middle row is the one that reads as an oddity and is not. `netrules`
subtracts because an untagged sandbox has unrestricted egress and a tagged one
is filtered to its allow list; adding a rule-set *removes* reachability. That
asymmetry is why `PutRule` refuses `default` outright
(`internal/netrules/store.go:204-210`), and Part 3 is about why an environment
inherits that refusal for a different reason.

## Where plain environment variables sit

They go in `internal/secrets`, in a new `env_vars` table, and not in a sixth
package. The reason is one line long: **`EnvForSandbox` is where the merge
happens**, and it is the only place in the tree that decrypts
(`internal/secrets/store.go:569-608`). A vars store in its own package means the
merge happens in a caller — and then in a second caller, and then the two
disagree about precedence for a release before anybody notices.

```sql
CREATE TABLE IF NOT EXISTS env_vars (
  owner      TEXT NOT NULL,
  tag        TEXT NOT NULL,
  name       TEXT NOT NULL,
  value      TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  PRIMARY KEY (owner, tag, name)
);
```

Note the shape, because it deliberately differs from `secrets` +`secret_tags`. A
secret is **a value with a tag set**: one row in `secrets`, N rows in
`secret_tags`, and `secret rm` versus untagging are genuinely different
operations the CLI has to explain. A var is **a row per tag**. `PORT=8080` on
two environments is two rows, and changing it is two edits.

That is a real cost and it buys two things. `DeleteVarsForTag` is one statement,
so `env rm` cleans up completely without a second concept; and "delete this
environment" has no ambiguity about whether a shared value dies with it, because
there is no shared value. A var's whole premise is that it is cheap — it is
declared non-sensitive, a console can render it, an edit does not have to retype
it (`internal/secrets/store.go:157-161`). Duplicating something cheap is the
correct trade; duplicating a credential would not be.

The value cap should be `maxValueLen` — 4 KiB, the same as a secret
(`internal/secrets/store.go:135-136`). These are environment variables either
way, and the guest-side limit is the same file.

## Secrets win on a collision, and why that direction

`EnvForSandbox` writes the vars first and the secrets over the top. The two
possible orderings fail in very different directions and only one of them is
survivable.

If a var shadowed a secret, an owner who sets `DATABASE_URL` as a var for a
staging box and *also* holds a sealed `DATABASE_URL` gets the plaintext one — a
credential silently replaced by the value that was, by declaration, safe to
print. The failure is invisible and it is in the direction of using the value
somebody took less care with.

The other way round, the sealed value wins. The failure is that a var is
ignored, which is annoying, visible the moment anybody echoes it, and fixable by
deleting one of the two.

So: secrets overwrite. And the complement to that rule is a **warning at write
time, not a refusal** — `PutVar` on a name that already has a secret on the same
tag should say so in the response, and so should `PutSecret` the other way. A
cross-table uniqueness constraint would be a second write path through two
tables that today have no transaction in common, to prevent a state that is
already deterministic and already explainable. `ctl env show` renders the
shadowed var struck through, which is the honest display.

## Both halves ride the channel that already exists

`internal/envsync` pushes an owner's environment into `/etc/environment` over
SSH, rewriting only the block between `BlockBegin` and `BlockEnd`
(`internal/envsync/sync.go:43-52`), so the toolchain `PATH` the image bakes in
survives. Vars go in the same block, which gets three properties free:

- the push-on-create and push-on-retag hooks already cover them;
- the pre-capture strip removes them from a template along with the secrets
  (`internal/host/snapshot.go:72`), so a snapshot never freezes an environment's
  variables into a rootfs every fork then copies byte-for-byte;
- and because they are stripped, they come back from the **store** on the next
  create, which is what makes editing a var take effect on a forked box at all.

That last one matters more than it looks. If vars were written outside the
managed block to "survive", an environment rebuilt in March would keep handing
out February's `API_URL` forever, from a disk nobody thinks of as configuration.

---

# Part 3 — why `default` is refused

`ctlops.defaultTags` (`internal/ctlops/sandbox.go:242`) stamps
`secrets.DefaultTag` on **every** sandbox this package creates, unconditionally,
and `internal/secrets/store.go:103-133` explains at length why narrowing it to
untagged creates was tried and moved the same silent failure one step along.

Two stores already refuse that word at write time, for the same shape of reason
in two different registers.

`internal/netrules/store.go:204-210`:

> an egress rule-set cannot be tagged `default` — every sandbox carries that
> tag, so this rule would filter your whole fleet. Tag it with a name you also
> put on the sandboxes you mean to govern.

`internal/templates/store.go:228-235`:

> a template cannot be bound to the `default` tag — every sandbox you create
> carries that tag, so this snapshot would silently become the base image for
> all of them, including ones you make months from now.

An environment binds a template, so it replaces, so it takes the same refusal in
the same sentence shape at `env create` time:

> an environment cannot be named `default` — every sandbox you create carries
> that tag, so this environment's base image, egress rules and setup script
> would silently apply to all of them, including ones you make months from now.
> Name it something you also put on the sandboxes you mean to use it.

Note what got added to the template's sentence. An environment named `default`
would not merely re-base every one of the owner's future sandboxes; once Part 4
lands, it would run **its setup script** in all of them. Part 5 is the argument
for why running a script unattended needs a confirmation at all; a `default`
environment is that posture change applied to the whole of one person's fleet by
a single word, permanently, with no per-create way to opt out short of
retagging every box.

Two implementation notes for whoever writes it.

**Fold case and check before the grammar check.** `templates.Bind` does
(`internal/templates/store.go:211-218`): `DEFAULT` would be refused either way,
because `tagRe` has no uppercase, but by a message about the character set,
which says nothing about why this particular word is the one that cannot be
used.

**`internal/reserved` is the wrong list here, and it will be tempting.** That
package is the single claim over three things that all become hostnames — a
sandbox's default subdomain, a route subdomain, a user handle
(`internal/reserved/reserved.go:1-21`). An environment's name is a tag. A tag is
never a hostname, never dispatched on at the edge, and never resolvable. Calling
`reserved.Name` here would refuse `api`, `docs`, `www` and `git` as environment
names for reasons no user could act on and no failure would ever justify. The
contract's `ErrReservedName` covers `default` and, today, nothing else; the
grammar (`secrets.ValidTag`, `^[a-z0-9][a-z0-9-]{0,39}$`,
`internal/secrets/store.go:73`) is `ErrInvalidName`'s job and they should stay
two errors because they have two repairs.

That refusal is, as in netrules and templates, exactly what keeps
`ctlops.Create` free to keep stamping `default` on everything.

---

# Part 4 — the build

**Built, both modes** (Phases B and C). This part describes the code that
exists; where the original plan and the implementation disagreed, the paragraph
says which won.

An environment whose composition is complete still leaves steps 6, 7 and 8 of
the eight. `env build <name>` is the verb that removes them:

```
ssh ctl@<gateway> env build web
```

## The shape

0. Every refusal comes **before the first write**, which for this verb means
   before `SetState(building)`: environments off, a driver that cannot snapshot,
   no template-binding store, no tag store, no such environment *or* not yours
   (one masked answer from one line), already building, `web-build` already
   taken, and — last — no setup script anywhere. There is no transaction
   spanning the environment store, the tag store and the VM manager, so ordering
   is the only thing that makes this safe.
1. The **script is resolved**. The stored one wins. Otherwise the environment's
   attached repositories are read, in slug order, first hit wins, for
   `.sparkbox/setup.sh` — and finding one **writes it to the row** with
   `setup_from = "repo"`, which is the first write this verb makes. Seeding is
   what "seed" says: a later build re-uses the stored copy rather than re-reading
   the repository, so a hand-fixed script is never silently overwritten.
2. The gateway creates a **builder sandbox** named `<name>-build`, tagged with
   the environment — an ordinary create through `ctlops.Create`, which means it
   gets the environment's secrets, its vars, its checkouts and its egress policy
   by the four joins that already exist, with no special case anywhere — and
   `FromBase: true`, so a rebuild starts from the operator's stock image rather
   than accumulating the side effects of every previous build. Its name is in
   `environments.build_box` and the row is in `building`.
3. The gateway **nudges** that guest (`envsync.StartSetup`), which restarts a
   guest-side **systemd oneshot** that is installed but deliberately **not
   enabled** — see below. The verb returns here. The create is waited on, and
   only the create: a name that is taken or a host at capacity is a refusal the
   person typing this can act on, and learning it from a `failed` row a minute
   later would be strictly worse.
4. The guest fetches its own job (`GET /self/setup`), runs it, and **reports
   back over the metadata service** (`POST /self/setup/result`): whether it
   worked, the exit status, the `.sparkbox/setup.sh` the run ended with, and a
   bounded tail of the log.
5. On success the **gateway** — not the guest — takes the snapshot, through
   `ctlops.SnapshotToTag`, which captures and binds in one act; then it destroys
   the builder and the row moves to `ready` with `built_at` set.
6. On failure the row moves to `failed` with `build_error` carrying one
   sentence, **and the builder is kept, paused**. That is the correction to the
   original plan, which said the builder was destroyed either way. It is what
   `env capture` exists to finish from: the box holds the half-built disk, the
   log and the checkout, and throwing that away at the exact moment somebody
   needs it is the wrong trade. `env show` prints the box name and both
   commands.

Two half-failures are worth stating because they are decided rather than
incidental. If the **create** fails, the row goes straight to `failed` with an
empty `build_box` — a row stuck in `building` with no builder is the one state
only the forty-five-minute timeout recovers from. If the capture and the bind
succeed but the **destroy** fails, the environment is `ready` anyway: the
expensive half is done, the disk exists, the tag points at it, and reporting
that as a failure would send somebody to pay the whole cost again to reach the
state they are already in. The leftover box is logged with the `rm` line.

## The oneshot, and why it is installed but not enabled

`/usr/local/sbin/sparkbox-env-setup` and `sparkbox-env-setup.service`, installed
by `deploy/install-guest-identity.sh` (IDENTITY_REV 26). Shaped like
`sparkbox-repos.service`: `Type=oneshot`, `RemainAfterExit=yes` so `systemctl
status` shows that the pass ran, a bounded `TimeoutStartSec` (5400s, set
deliberately *above* the worker's own 2400s budget so the worker is always what
gives up first and therefore always reports), `After=` the network, token and
repo units, and — the load-bearing one — **no ordering before `ssh.service`**.
That unit's own comment says why, and it cites the incident: "copying it into a
unit that clones a repository would put a multi-minute network operation in
front of the first attach, which is exactly the class of bug main@e196d5f
already cost this platform once." A setup script is slower than a clone.

**It has no `[Install]` section and is never enabled**, which is a change from
the plan and the more interesting half. Three reasons, and a test in
`deploy/assets_test.go` that fails if anyone adds one. Only a builder ever has a
job, so enabling it fleet-wide would have every VM in the fleet make a metadata
request on every boot to be told 204. The owner's secrets arrive by an env
*push*, not by a unit, so boot ordering cannot express "after the secrets land"
and a boot-time run would race it — a setup script that runs before the managed
block is written sees none of the owner's credentials. And the gateway has to
know when the run finished, which means it has to be the thing that started it.
So `envsync.StartSetup` restarts the unit explicitly, after `PushEnv` has
returned. `restart` and not `start`, because `RemainAfterExit=yes` makes a
second `start` a silent no-op on a re-used builder.

The worker itself drops privilege (`runuser`, else `sudo -n`) to the login user,
`cd`s to the primary checkout `sparkbox-repos` published, sources
`/etc/environment` *inside* the unprivileged child — a systemd unit gets no
`pam_env`, and sourcing it after the drop means a `$(...)` in a secret's value
cannot run as root — and runs `bash` under `timeout -k`. Its log is 0600 and
owned by the login user, not 0644 like the repo worker's, because a `set -x`
over an API key is a realistic thing for a setup script to produce.

**In Phase B it runs exactly one thing: `bash .sparkbox/setup.sh`.** That is the
path the platform has been telling agents to prepare for since
`deploy/refresh-agent-tools.sh:777-783` started installing the instruction into
every template's `~/.agents/AGENTS.md`, and since
`internal/guestdocs/dev-environment.md:88-102` started documenting the
convention. `setup_from = "repo"` when the script was seeded from a repository,
`"manual"` when somebody piped it in.

Phase C added the second branch: **`claude -p`** with the dev-environment
guidance, when there is no script anywhere. The guidance is not new either —
`sparkbox docs dev-environment` (`internal/guestdocs`) is the per-framework
Host-header and hot-reload material, and the platform-owned `~/.agents/AGENTS.md`
already tells an agent to bind `0.0.0.0`, use a `systemd --user` unit, call
`sparkbox set-port`, and **write what it did down as `.sparkbox/setup.sh`**.
`setup_from = "agent"`.

**The seam was less cut than this paragraph claimed.** It said "the only ctlops
change is what happens where `env build` refuses with `env_no_setup`". The wire
FORMAT was ready — line 2 has always been the mode and the guest has always
refused an unknown one by name — but nothing in Go carried a mode. `line 2` was
the literal `b.WriteString("\nscript\n")` in `metadata`'s renderer, and
`SetupFor` returned `(script, env string, ok bool, err error)`. Adding a mode
touched five signatures and one relay struct, and `SetupFor`'s own guard
("no script on the row means no job") refused agent mode by construction.

What that turned into: `SetupJob{Env, Mode, Payload}` in `metadata`, mirrored in
`ctlops` for the same import-direction reason `SetupResult`/`SetupReport` are
mirrored, and a `Mode` field on `nodelink.SelfSetupResp`. The payload field kept
its shipped JSON tag `script` even though it now carries a prompt: gateway and
node are separate processes that can run separate builds, and a renamed tag
makes a new gateway's payload arrive at an old node as an empty string — a
builder that runs nothing and reports success — where an unknown *mode* fails
loudly at the guest by name. A struct rather than four positional returns,
because three same-typed strings in a row is a shape where transposing two of
them compiles and serves the mode as the environment name.

**Which mode a build is, is read off the ROW and never off the request** — the
same rule as the environment name, and for a sharper reason. `startBuild` stamps
`SetScript(owner, name, "", envs.SetupFromAgent)` before the state moves, and
that stamp is the only durable record: two readers need it long afterwards with
nothing else to go on — `SetupFor`, answering a guest that boots minutes later,
and `ReconcileEnvironmentBuilds`, deciding after a gateway restart whether an
expired builder is a paused disk to finish by hand or an unattended agent to
destroy. Nothing in the guest's report carries the mode. `setup_from` is honest
here rather than overloaded: an empty script with `from = agent` means "an agent
is writing one", and the store already documents that an empty script with a
`from` is distinguishable from never having looked.

The second path's real output is therefore the first path's input. An agent that
does its job leaves a script in the repo, a person reviews it in a pull request
like any other file, and the next `env build` takes the deterministic branch.
That loop is the argument for the agent path existing at all — not "an agent can
configure a box", which is unremarkable, but "an agent can produce the artifact
that means no agent has to next time."

Either way the script text that actually ran lands on the environment row via
`SetScript(owner, name, script, from)`, which is why the contract has that
method and why `SetupScript` is `""` until a build runs. It is the record of
what happened, not a plan for what will. An empty script field in the report
means "unchanged" and the stored one is kept — the guest sends empty rather than
truncating an oversized file, because half a script is what every future fork of
the environment would run.

## The fetch and the report

**Two routes, not one.** The plan said `POST /self/setup`; the implementation is
`GET /self/setup` for the job and `POST /self/setup/result` for the outcome.
Sharing one path would have made "no job" (204) and "here is what happened" the
same URL, which is a distinction worth a path segment.

Both are in the shape of `POST /repos/status`
(`internal/metadata/server.go:386`), the existing precedent for a guest
publishing a fact about itself that the gateway stores on the record. The
authentication is the one the whole metadata service already rests on: the
handler matches the request's source address to the guest's `/30` slot **and**
the connection's local address to `slot.Host`
(`internal/metadata/server.go:802`), and the firecracker driver sets `rp_filter`
so a spoofed source is dropped rather than merely unanswered. Nothing in either
request names a sandbox, an environment or an owner. The guest arrives on a tap;
the host decides everything else.

The guest has no JSON encoder — it is POSIX `sh` with `curl` and `base64` — so
both bodies are line-oriented, which is `/repos/status`' rule applied again:

```
GET  /self/setup          -> 204, empty          (no job: every ordinary VM)
                          -> 200 text/plain
                             line 1  environment name
                             line 2  mode ("script"; the Phase C seam)
                             line 3  base64 of the script (folding tolerated)

POST /self/setup/result   -> 202, then the work runs on its own goroutine
                             line 1   ok | failed
                             line 2   exit status 0-255
                             line 3   base64 of .sparkbox/setup.sh, or empty
                             line 4+  the log tail, VERBATIM, not base64
```

Guest-authored bytes are bounded on both sides. The body is capped at 128 KiB
before parsing (the largest honest report is ~74 KiB); the decoded script is
**refused** over 64 KiB rather than truncated; the log is **truncated** to its
last 8 KiB rather than refused, because a rejected report leaves the row in
`building` with nobody able to say why. `build_error` is then reduced further —
last non-empty line, control bytes and partial runes replaced, cut at 200 runes
— because an ANSI escape in a refusal is a terminal a stranger can drive, and
that column is printed by `env ls`, `env show` and every `create --env` refusal.

The 202 is written, flushed and the guest's FIN awaited **before** `SetupDone`
runs, for the reason the self-service pause and capture verbs do it: the first
thing that work does is pause the VM the request came from.

**The owner term on the way back in is a security boundary, not belt and
braces.** `envs.Building()` is the one store query with no owner scope — the
reconciler acts for no person — and sandbox names are a single global namespace,
so `web-build` is a name anybody may take. Both `SetupFor` and `SetupDone`
compare the row's owner against the sandbox record's, and log a mismatch. Without
that comparison a stranger who created that name would be handed another owner's
setup script: the shape of their private toolchain, and often the names of their
internal repositories and services.

## Both routes are relayed from a node

A node holds no environments table, no tag-to-template bindings and no placement
ledger, so it can answer neither route from anything of its own. Both therefore
travel up the node link as `sandbox.self_setup` and `sandbox.self_setup_result`
(`internal/nodelink/frame.go`), exactly as the lifecycle trio and the repo pair
do, and land on `Fleet.SelfSetup` / `Fleet.SelfSetupResult`, whose first act is
`selfServiceBox` — the gateway's own ledger must place that sandbox on the node
that asked. The node's side is `relayEnvSetup` in `cmd/sparkbox/node.go`, wired
into its metadata service beside `relaySelfLifecycle`.

This is not an optimisation for a fleet that happens to have nodes. **On CKS the
gateway is control-plane-only, so every sandbox — every builder included — is
placed on a node.** Left unrelayed both routes answer 501 there, a builder reads
that as "no job", exits 0 without running anything, and the row sits in
`building` until the 45-minute timeout reports a cause that is not the real one:
no environment build can complete at all. It is also the leak the metadata
service exists to prevent, which is the same argument in its general form — a
guest must not be able to tell which machine it landed on from the status it
got.

The relayed report is bounded again on arrival (`MaxSelfSetupScriptBytes`,
`MaxSelfSetupLogBytes`) rather than trusted to have passed the metadata door's
caps, because the body originated in a guest and the door it passed was on
another machine.

## Why the gateway takes the snapshot

The guest could ask for its own capture. `sparkbox snapshot <tag>` already
exists and already does exactly this — plan, confirm, commit, capture, bind
(`docs/tag-templates-design.md` Part 10). Reusing it would be the smaller diff
and it is the wrong call, for three reasons.

**The gateway-side path is already proven and already synchronous.**
`host.Manager.Snapshot` (`internal/host/snapshot.go:46`) takes the disk lock,
strips the managed secret block (`:72`) — every fork copies the rootfs
byte-for-byte, so that block cannot ride along — refreshes the agent CLIs
(`:81`), pauses the guest so the filesystem is flushed, and hands off to the
driver. `ctlops.SnapshotToTag` (`internal/ctlops/template.go:195-238`) wraps
that with the bind, in the right order (capture first: binding first would leave
the tag pointing at an image that does not exist, so every create on that tag in
that window resolves to a missing rootfs), and with the half-failure already
thought through — if the bind fails the snapshot is **kept**, and a typed
`snapshot_not_bound` carries the one-command repair. Every one of those
decisions is one an environment build would otherwise have to make again, worse.

**The guest then needs no second credential, and no relaxation of the rules that
protect the first one.** The guest door is deliberately hard to use unattended,
and every one of its restrictions is in the way here. It refuses without a TTY,
on purpose — "there is nobody here to read the warning, so I will not proceed" —
and a builder VM has no TTY by construction. It is rate-limited to three commits
per hour per sandbox. It may only re-point **a tag it already carries**, checked
server-side, which is the restriction that caps a compromised guest's
persistence at the trust its box already had. Driving a build through that door
means either handing an unattended process a `--yes` and a plan token, or
loosening a rule whose entire purpose is that unattended processes cannot use
it. The gateway already holds the authority to capture and bind. Let it. The
guest reports a fact; the gateway acts on it.

**And the fleet case is already handled.** `Fleet.Snapshot` runs the pre-capture
tool refresh gateway-side before it hangs up the node sessions, because a node
caches only the gateway's upstream *public* key and has no signer with which to
open a session into its own guests (`docs/tag-templates-design.md` Part 5). A
gateway-driven build inherits that. A guest-driven one would run into it.

## The reconciler, and why `Building()` is on the contract

A gateway restart in the middle of a build leaves a row saying `building`, a
`build_box` naming a sandbox that may or may not still exist, and an owner's
decrypted secrets inside it. `Store.Building()` returns every such row across
**all** owners — the only method on the contract that is not owner-scoped, and
deliberately so, because the caller is the process itself and not a person.

Resuming is tempting and wrong — the guest's oneshot may have completed,
half-completed, or be running still, and nothing on the gateway can tell the
three apart after a restart. So the reconciler settles rather than resumes,
three cases:

- **the builder is gone** -> `failed`. Nothing can finish a build whose box does
  not exist, and leaving the row in `building` would refuse every `create --env`
  on it forever.
- **the build is older than `--env-build-timeout`** (default 45m) -> `failed`,
  **builder left paused**. This is the second correction to the plan, which said
  to destroy it. The whole reason a failed build keeps its builder is that the
  box holds the half-built filesystem, the log and the checkout — which is what
  `env capture` exists to finish from — and a reconciler that destroyed it would
  throw that away for the one failure mode where nobody was watching.
- **anything else** -> **left alone**. This is the case a naive sweep gets
  wrong: a build two minutes into a twenty-minute `cargo build` looks exactly
  like a stranded one from out here, and failing it would destroy work that was
  going to succeed. The result POST may still land.

It runs once at startup — the load-bearing pass, because a restart mid-build is
the state nothing else recovers from — and then every ten minutes.
`cmd/sparkbox` owns that goroutine, in `pushLoop`'s shape and with its shutdown
discipline; nothing in `ctlops` starts a timer the wiring cannot see or stop.

## `env capture`: finishing by hand

A verb the original plan did not have, and the reason the failure path keeps its
builder. Somebody `ssh`es into `web-build`, finds the missing apt package,
installs it, and runs `env capture web` — and the environment is built, from the
disk they just fixed, with no second run of a script that was never going to
work. It shares the build's singleflight key, so a manual capture and a guest's
late result cannot both try to snapshot the same box, and it is synchronous like
`snapshot create` because the person who typed it is watching.

Both verbs' refusals are owner-scoped through the same masked read, so a
stranger's `env build web` and one on an environment nobody has are the same
sentence from the same line.

---

# Part 5 — the two posture changes

These are not consequences of the design. They are the design, and burying them
under Part 4's mechanism would be the dishonest way to write this document.

## (a) We will run a script from a repository, unattended

Before Phase B, `internal/guestdocs/dev-environment.md` ended with:

> Read a `.sparkbox/setup.sh` you did not write before running it — like any
> script, it runs as you.

and the example file's own header
(`internal/guestdocs/examples/.sparkbox/setup.sh`) said:

> Written by the agent that first configured this project to run inside a
> Sparkbox VM … **Sparkbox itself never writes or runs this file.**

Both sentences were true when they were written and both have now been corrected
in place, because Phase B made the second one false.

Part 4 is what broke them. `env build` runs `bash .sparkbox/setup.sh` as the
guest's login user, in a box holding the owner's decrypted secrets, from a file
whose contents are whatever is on the branch the attachment points at — which
anyone with write access to that repository can change, including a merged pull
request nobody read closely.

The exposure is bounded by what a sandbox is: the script runs *inside* the
guest, under the environment's own egress policy, with no host mount and no
control-plane authority. It cannot reach the gateway except through the metadata
service, which answers only for the sandbox that asked. This is not a new
boundary and this document does not move it.

But the **trigger** is new. Today a person types `bash .sparkbox/setup.sh` after
reading it, or does not. Tomorrow a CLI verb does it on their behalf.

So the mitigation is the one the docs already prescribe, moved into the product:
**`env build` prints the script and asks**, before the builder VM is created,
unless `--yes`. The full text, not a hash and not a summary — a summary of a
shell script is a category error.

And the confirmation is remembered against the text, not against the
environment. `environments.setup_script` holds what was confirmed and run; a
build whose repo script differs from it prompts again, showing the diff. That is
the property worth having: the second `env build` of an unchanged environment is
frictionless, and the one after somebody edited the setup is not. `--yes` skips
the prompt, exactly as it does for `sparkbox snapshot`, and skipping is the
scriptable path for a person who has already decided.

The one rule to carry over from that verb verbatim: without a terminal, and
without `--yes`, it refuses. A warning nobody can read is not a warning.

**As built (Phase B), this mitigation is NOT in place, and that is the one gap
in this phase worth reading twice.** `env build` does not print the script and
does not ask; there is no `--yes` and no diff-on-change. The posture change in
this section has therefore been taken without the compensating gesture that was
supposed to come with it. What does exist is weaker and worth naming honestly:
the seed read records the script on the row **before** the builder boots, so
`env script <name>` prints exactly what is about to run and `env show` says how
many bytes it is and where it came from; and a later build resolves from the row
rather than going back to github.com, so nothing changes under an environment
between builds.

One interaction here is surprising enough to state outright. The guest's report
carries the `.sparkbox/setup.sh` the run **ended with**, and that is what lands
on the row — deliberately, because in agent mode the whole point is that the
agent wrote it. In script mode it means a build re-records whatever the fresh
checkout had, so the row tracks the repository as of the last build. `setup_from`
is *not* re-litigated: a script somebody piped in stays `manual`, and a later
build still resolves from the row rather than seeding again. The prompt, the
`--yes`, and the re-confirm-on-diff are owed.

## (b) We will run an agent, unattended, in a box holding the owner's secrets

`claude -p` in a builder VM is an agent with a shell, network access under the
environment's rules, and `/etc/environment` full of the owner's decrypted
credentials.

Say plainly what is and is not new. **The exposure is not new** — that is what a
sandbox *is*, and the platform's own tip is `claude setup-token | ssh ctl@…
secret set CLAUDE_CODE_OAUTH_TOKEN` precisely so that agents in sandboxes are
signed in and useful (`internal/sshgw/secret.go:61`). Every box anybody has ever
created this way has had exactly this shape. **The trigger is new**: today a
person opens a terminal and starts the agent; tomorrow a CLI verb starts one
with no person in the room, at a moment nobody chose, in a box the person never
sees.

Three mitigations, and none of them is "trust the model".

**The environment's own netrules — and the paragraph that used to be here was
WRONG, which is the most useful thing in this section.**

It said a builder is "governed by construction" because it is tagged. It is not.
`AllowForSandbox` returns `governed = len(rules) > 0`, where `rules` is the join
of `network_rules` → `network_rule_tags` → `sandbox_tags` — so governance keys
off the sandbox's tags carrying a **rule-set**, not off the sandbox being
tagged. Under the deployment sparkbox actually runs (`sluice --enforce
--open-untagged`) a sandbox absent from the policy snapshot is UNFILTERED. A
builder tagged `{web, default}` where nobody has written a rule-set for `web`
was therefore exactly as open as an untagged box. The mitigation this section
claimed cost nothing and was already true cost something and was false, and had
it been written down as-is the platform would have recorded a safety property it
did not have: the DEFAULT agent builder, with open egress, an unattended agent,
and the owner's decrypted credentials in it.

**What is built instead.** `ctlops.defaultEnvEgress` gives every NEW environment
an egress rule-set named after it, with an empty allow list, carried by its tag.
That makes every sandbox on the environment governed, which under sluice means
filtered to the operator's base allowlist plus the domains its repo attachments
imply.

An empty rule-set being *usable as a default* rests on one fact that the name
actively obscures, and `AllowForSandbox`'s own comment used to get it wrong too:
**an empty allow list is not deny-all.** `policy.AllowedFor` checks the BASE
allowlist first and grants unconditionally, so a governed sandbox with no
patterns of its own still reaches `pypi.org`, `registry.npmjs.org`, `crates.io`,
`proxy.golang.org`, `github.com`, `api.anthropic.com` and the rest of
`deploy/sluice-allowlist.txt`. The base list is a floor under every governed
sandbox, not a ceiling over it — which is also why `api.anthropic.com` being on
it is what lets an agent build work at all under its own default policy.

Three properties of the default worth stating, because each is a decision:

- **On create only, never on `set`.** `resolveEnvRules`' comment warns that
  quietly creating an empty rule-set would cut every sandbox on a tag down to
  the base allowlist — a policy nobody wrote, discovered as a build that cannot
  reach the internet. That warning is right *about an environment that already
  exists*. At the moment one is born there is nothing to narrow, so this is a
  default rather than a change. `create` and `set` are one verb, so the code
  takes a create/update discriminator BEFORE its first write — the store's Put
  is an upsert, so after it there is no way to tell — otherwise deleting the
  rule-set to open an environment's egress would be silently undone by the next
  unrelated `env set --var`.
- **Overridable, in both directions.** Widen it in the console's Network panel
  like any other rule-set; or pass `--open-egress` on create to have no rules at
  all and be unfiltered, which is what every environment was before this.

**And the policy is pushed before the guest is nudged.** Egress policy is
otherwise pushed by a thirty-second sweep, so a sandbox created between sweeps
is absent from sluice's snapshot — and absent means unrestricted. Every other
sandbox can wait; a builder cannot, because it starts an unattended agent within
seconds of booting, and waiting would mean the agent spends its first half
minute with exactly the open egress this rule-set exists to deny it.
`ctlops.NetPusher` exists for that one caller. A failed push WARNS in script
mode and REFUSES in agent mode: the argument for `bypassPermissions` is that it
happens in a governed box, so a host that cannot confirm the box is governed
must not start the agent.
- **Best effort, never fatal.** The environment and everything the caller asked
  for are already written when this runs. Failing the whole verb over a default
  would report failure for a command that mostly succeeded.

**A hard build timeout, and the builder DESTROYED on expiry.** Enforced by the
gateway, not by the guest, because the guest is the thing that might be stuck.
`ReconcileEnvironmentBuilds` reads the mode off the row and `expireBuild` is the
one place the two modes diverge after they start: a script build's builder is
paused and kept, because it holds a half-built disk and a log that somebody can
finish by hand with `env capture`; an agent build's builder is destroyed,
because an overrun agent build is by definition one whose guest never reported,
so the likeliest thing in that VM is an agent still running with a shell, egress
and the owner's decrypted credentials. There is also nothing to keep: an agent
build that overran has not written the script that was its deliverable. The row
goes to `failed`, and its `build_box` is cleared **only if the destroy
succeeded** — a row naming no builder means "nothing left to look at", and
saying that about a VM still sitting there is the one lie this path must not
tell.

This section originally specified a *shorter* 15-minute budget for agent mode,
citing `ctlops.ArchiveTimeout`. That was not built, and the reason is the
timeout ladder below: the number that actually bounds an unattended agent's life
is the budget plus the reconciler's ten-minute sweep interval, so a second
budget would have been a second thing to keep in sync for a bound it does not
really deliver. One budget, `--env-build-timeout`, applies to both modes; what
differs is what happens when it expires.

**The timeout ladder, which was wrong and is now ordered.** Three budgets bound
one build and they only work in one order — the guest's must be smallest, so it
always reports before anything else gives up:

| bound | value | where |
|---|---|---|
| guest worker | 40 min | `TIMEOUT` in `sparkbox-env-setup` |
| gateway reconciler | 45 min (`--env-build-timeout`) | `ReconcileEnvironmentBuilds` |
| systemd unit | 90 min | `TimeoutStartSec` |

The worker's budget used to be **60 minutes** — longer than the gateway's 45 —
so a build between the two was marked `failed` by the reconciler while the guest
was still working, and the guest's eventual report landed on a row that was no
longer `building` and was discarded with a warning. The build had actually
finished and nobody could tell. Setting `--env-build-timeout` below 40 minutes
re-creates that inversion, which is why the flag's help now says so.

**`--permission-mode bypassPermissions`, and why that is not the decision
`refresh-agent-tools.sh` declined to make.** This one is not in the original
list because it was not known to be necessary. Measured against a real `claude`:
under `-p`, the `auto` permission mode this platform seeds into every template's
`~/.claude/settings.json` is silently downgraded to `default`, every `Write` and
every `Bash` is DENIED, and **the run still exits 0**. An agent build without
the flag does nothing, reports success, and gets an untouched base image
captured as the environment's disk — a failure invisible to everything
downstream, since the row says `ready` and the snapshot exists.

`refresh-agent-tools.sh` deliberately seeds `auto` and not `bypassPermissions`,
with the comment "`--dangerously-skip-permissions` stays theirs to type." That
decision stands and this does not overturn it: it declined to make the call
*once, globally, on behalf of every user of every sandbox*. This is one
invocation, in one ephemeral builder the owner asked for by name, whose egress
is governed by the default rule-set above and which is destroyed if it overruns.

Two consequences follow and both are built. Success is judged by the **artifact**
— `rc == 0` *and* a non-empty `.sparkbox/setup.sh` — because the exit status is
not an answer in this mode. And the invocation carries
`--no-session-persistence`, because the builder's disk becomes the environment's
template and is copied byte-for-byte into every fork of it: nothing in the
capture path strips `~/.claude/projects`, so *not writing* the transcript — the
prompt, every command, and every command's output — is the only place that can
be prevented.

**Refuse the build up front when the owner has no agent credential.** The
gateway can answer this without decrypting anything: `ListSecrets(owner)`
returns `SecretMeta` carrying `Name` and `Tags` and no value at all
(`internal/secrets/store.go:149-155`), so "does this owner have
`CLAUDE_CODE_OAUTH_TOKEN` on this tag" is a listing question. Refusing at the
door matters because the alternative is genuinely bad: a builder boots, runs
`claude -p`, hits an interactive login prompt it cannot answer, and burns the
whole timeout producing a `failed` row whose real cause is three layers down.
The refusal's sentence should be the `secret set` tip, which is the repair.

The `.sparkbox/setup.sh` path takes none of these except the timeout and the
default egress rule-set, which is the other half of the argument for Phase B
shipping before Phase C.

One thing this section still OWES, and it is worth naming rather than leaving to
be discovered: the prompt-and-confirm for a script adopted out of a repository.
`env build` reads `.sparkbox/setup.sh` from an attachment and runs it with no
human having read it, which inverts the instruction the platform's own example
script carries ("Read a `.sparkbox/setup.sh` you did not write before running
it"). Agent mode does not make that worse — an agent-written script is reviewed
in a pull request like any other file, which is the whole point of the script
being the deliverable — but it does make the gap load-bearing, because agent
mode is how most environments will acquire their first script. The `--yes`, the
printed diff, and the re-confirm when the repository's copy has changed are
still owed.

---

# Part 6 — the launch door needs no change at all

`go.<domain>` serves a button somebody pastes into a pull request comment, and
the promise is narrow on purpose: whoever clicks it signs in as themselves and
lands in their own sandbox with that repository checked out
(`internal/launch/launch.go:1-24`).

An environment **is** a tag. A launch link's tags come from
`repos.Repo.Tags` on the matched attachment, read from the store under the
verified session's own handle, and handed to `ctlops.Create` verbatim
(`internal/launch/create.go:159-169`). The confirm page already renders them as
pills, plus the `default` that `defaultTags` will stamp
(`internal/launch/page.go:642-654`).

So: attach a repository to an environment's tag, and every launch link for that
repository lands the clicker in that environment — its secrets, its vars, its
checkouts, its egress rules and, once Part 4 lands, its snapshot. Zero lines
changed in `internal/launch`. The environment's name simply appears in the pills
that are already there.

That is not luck. It is the second dividend of name == tag, and it is worth
saying out loud because the alternative shape would have needed real work here:
a multi-tag environment would have forced launch to decide whether an attachment
carrying two of an environment's three tags means the environment or half of it.

## And `?env=` must never exist

The temptation, once environments have names, is `go.<domain>/wandb/hivemind?env=web`.

`internal/launch/launch.go:55-76` forbids it already, in the general case, and
the argument is the hard rule of that package:

> Put that selector in a URL and you have handed the author of a public,
> immutable comment the ability to choose which of the CLICKER's secrets are
> decrypted into a VM whose working tree sits at a branch the same author chose.

An `env=` parameter is that primitive and **more**. A tag selects the owner's
secrets. An environment selects the secrets, the plain vars, the repositories,
the egress policy, the rootfs — and a setup script that *runs*. The comment
author would be choosing which of a stranger's credentials get decrypted into a
VM **and** which of that stranger's shell scripts executes with them present.

The package's own note that narrowing does not help applies here verbatim: "a
repository carried on both `dev` and `prod` still lets the author pick", and the
narrowed rule would live in a second place where it goes stale the moment the
attachment's tags change. Tags come from exactly one place, which is the matched
attachment's stored `Tags`. Environments change nothing about that and must not.

---

# Part 7 — what this does not do, on purpose

**No sharing across owners.** `environments` is keyed `(owner, name)` and
`EnvironmentsForSandbox` carries the owner on both sides of the join, exactly
like the four readers before it. Alice's `web` and Bob's `web` are different
environments that never meet. The real answer to a team wanting one environment
is a shared or org-scoped object, and it is out of scope here for the same
reason it is out of scope in `docs/tag-templates-design.md` Part 9 and
`docs/repos-in-templates-design.md` Part 8: cross-owner sharing is a permissions
model, not a column. Worth noting that owner scoping is also what bounds Part
3's blast radius from the fleet to one handle.

**No environment composing several tags.** Part 1.

**No garbage collection of the snapshots an environment creates.** Each `env
build` captures a new snapshot named `<tag>-<YYMMDD-HHMM>`, and the previous
generation survives, unbound — which is deliberate, because it is the only thing
that makes a bad build recoverable with one `snapshot bind`
(`docs/tag-templates-design.md` Part 10). It is also a slow leak, and worse than
it looks: snapshots are **unmetered**. Nothing anywhere counts them, by number
or by bytes, and they land in the image directory every VM on the machine
reflinks from. An environment rebuilt weekly is 52 templates a year that nobody
is told about.

The fix is not hard — keep the last N per environment, or expire on age — and it
is deliberately not in Phase A, because a retention policy on a thing that also
serves as somebody's rollback needs a decision about N that nobody has an
opinion on yet. What Phase A **must** do is make `env rm` list the snapshots it
is leaving behind rather than silently orphaning them.

**On CKS, an environment's template is node-local and can evaporate.** The
cluster Deployment is pinned to one exact Node and uses a named hostPath
(`docs/cks-reflink-persistence-plan.md`), so the image directory — templates
included — lives on that node's local XFS. Templates are not replicated across
nodes (`docs/tag-templates-design.md` Part 4 says the replication half is
written down and unbuilt), and object storage on that cluster holds checkpoint
objects, not templates.

So an environment whose node is reclaimed has a binding pointing at a file that
is gone, and every create on it hits `resolveTemplate`'s refusal
(`internal/ctlops/binding.go:200-211`):

> tag `web` boots from snapshot `web-260902-1130`, which no longer exists. Bind
> the tag to a snapshot you still have, or unbind it to go back to the default
> image.

That refusal is exactly right and must never degrade into a silent fallback to
the stock image — the comment above it says why, and it is the same argument as
everywhere else in this tree: a box that quietly boots the wrong rootfs is
invisible for twenty minutes and then reads as a broken toolchain.

But its **message** is wrong for an environment. `snapshot unbind --tag web` is
the repair for a hand-made binding; for an environment it throws away the object
and reads as an instruction to dismantle the thing you were using. When the tag
names an environment, the sentence must say `ctl env rebuild web` — which is the
same act, done by the thing that knows how to do it, and which is why `Get` is
on the `ctlops.Environments` interface at all: `resolveTemplate` needs to know
whether the tag it is refusing over is an environment before it picks a hint.

---

# Part 8 — build order

Four steps. Each is independently shippable and independently useful, and the
ordering is chosen so that the two posture changes in Part 5 land as late as
possible and separately from each other.

**A. The object and its composition. (Phase A — DONE.)**
`internal/envs` with the schema in Part 1; `secrets.Var`, `PutVar`,
`DeleteVar`, `ListVars`, `VarsForTag`, `VarsForSandbox`, `DeleteVarsForTag` and
the merge in `EnvForSandbox`; the `Environments` and `EnvVars` interfaces in
`ctlops`; `ctl env create|ls|show|rm|var set|var rm`, and the REST mirror. No
build, no builder VM, no script. An environment is a named bundle you compose by
hand, and `env show` prints all five joins in one place.

This is useful on its own, which is the test every step here has to pass. It
turns the eight steps of the Why section into three (`env create`, attach
things to it, `ssh new@<gateway> --tag web`), it gives non-secret configuration
its first home, and it makes "what is `proj`?" a question with an answer. The
build columns exist from this step's first `CREATE TABLE` and are never written,
so B is purely additive.

**B. The build, `.sparkbox/setup.sh` only. (DONE, mock-driver only.)** The
builder VM, the guest oneshot, `GET /self/setup` + `POST /self/setup/result`
(two routes, not the one this document originally planned), the gateway-side
`SnapshotToTag`, the `Building()` reconciler, and `--env-build-timeout`.
**No agent.** This ships the entire orchestration — which is where the
interesting failures are: the builder that never reports, the gateway that
restarts mid-build, the guest whose oneshot outlives its timeout — with the
smallest blast radius available, because the script came from a repository and a
person can read it on screen before it runs.

It also added a verb this plan did not have: **`env capture`**, which adopts a
failed build's paused builder exactly as it stands. That is what made "keep the
builder on failure" the right call rather than a leak.

What B did NOT ship is Part 5(a)'s print-and-confirm — `env build` does not show
the script and ask. `env script <name>` prints it and the seed is recorded on
the row before the builder boots, so the script is inspectable before and after,
but the confirmation gesture itself is still owed.

**C. The agent path — BUILT.** `claude -p`, `setup_from = "agent"` stamped
before the state moves, `SetupJob` carrying the mode from the gateway through
the node relay to the guest, and Part 5(b)'s mitigations made real rather than
asserted: the no-credential refusal at the door, `bypassPermissions` with
success judged by the artifact, `--no-session-persistence` so the transcript
does not ride into every fork, the builder destroyed rather than paused when an
agent build overruns, and a default egress rule-set that makes "the builder is
governed" true instead of merely claimed. `ctl env rebuild` is a second name for
`env build` — `build` already boots the stock image and runs the current script,
so there is one code path — plus the two refusals that now name it: the
`template_missing` sentence when an environment's snapshot is gone, and the
`env_build_failed` sentence, which used to tell somebody whose REBUILD failed
that their image was gone when the tag was still bound to the previous good one.

Landing this after B means the day an unattended agent first runs, everything
underneath it has already been exercised by builds that did not have one — and
in fact B's deploy to real hardware is what found the two bugs C had to build
on top of.

**D. The consoles.** An Environments panel on `my.<domain>` beside Secrets,
Network and Repos — those panels are one object viewed four ways and this is the
step that says so — plus whatever the operator console needs. It trails because
everything before it is reachable from `ctl` and REST, and because a panel for a
feature whose shape is still moving is a rewrite waiting to happen.

Cross-owner sharing (Part 7) and snapshot retention (Part 7) stay written down
and unbuilt. Neither has a measured workload behind it, and retention in
particular needs somebody to have an opinion about N.
