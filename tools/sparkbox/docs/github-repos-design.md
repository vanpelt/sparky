# Repos in a sandbox

How a GitHub repository becomes something a sandbox *has* rather than something
its occupant remembers to clone, and how the credential that clones it stops
being a personal access token sitting in `/etc/environment`.

This is a proposal. Nothing in Part 3 onward is built.

---

# Part 1 — where this actually stands today

Two halves of a GitHub story already exist, and they have never met.

**Identity is done and it is good.** `docs/github-linking-design.md` describes a
link proved either by a key published on `github.com/<login>.keys` or by the
OAuth device flow, recorded with its provenance (`users.github_via`) and its
immutable account number (`users.github_id`), and surfaced as the `github` claim
in every id token the gateway mints. The client id it ships,
`Iv23liV6n9amGfGY20Js`, is a **GitHub App** — which matters more here than it
did there, because a GitHub App has installations, and installations are the
thing that can be scoped to individual repositories.

**Access is not done at all.** The shipped path is the one the `ctl@` help text
prints:

```
gh auth token | ssh ctl@<gateway> secret set GITHUB_TOKEN --tag ci
```

Read what that actually does. `gh auth token` prints the CLI's own OAuth token,
whose default scope set includes `repo` — every private repository the person
can reach, read and write, on every org they belong to. `secret set` seals it
under the fleet KEK and `internal/envsync` writes it into `/etc/environment` in
every sandbox tagged `ci`, where it is readable by every process in the guest,
survives pause/resume, and lives until the user revokes it by hand. Its scope is
"everything this human can do on GitHub, forever" and its blast radius is "any
code any agent ran in any box carrying that tag".

It is also, notably, not connected to the link in Part 1's first paragraph. The
platform knows the owner is `github.com/vanpelt` and it still asks them to paste
a bearer token in.

And cloning is entirely manual. A fresh sandbox has `git`, has an agent CLI, and
has an empty home directory. The first thing anybody does in a new box is type a
`git clone` they have typed a hundred times.

---

# Part 2 — the object: a repo attachment, carried by a tag

The right primitive is already in the schema. `sandbox_tags` is a shared table:
`internal/secrets` owns its mutations and computes `EnvForSandbox` from it,
`internal/netrules` reads the same rows and computes `RulesForSandbox` from it.
A tag is the platform's existing answer to "which of my things reach which of my
boxes", and it has now been the right answer twice.

So: a third table, a third store, the same join.

```sql
CREATE TABLE repos (
  id         INTEGER PRIMARY KEY,
  owner      TEXT NOT NULL,   -- sparkbox handle
  host       TEXT NOT NULL,   -- 'github.com' (reserved for GHES later)
  slug       TEXT NOT NULL,   -- 'wandb/hivemind'
  ref        TEXT,            -- branch/tag, NULL = the repo's default
  path       TEXT,            -- clone destination, NULL = the default layout
  access     TEXT NOT NULL,   -- 'read' | 'write'
  created_at TIMESTAMP NOT NULL,
  UNIQUE (owner, host, slug)
);
CREATE TABLE repo_tags (
  repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  tag     TEXT NOT NULL,
  PRIMARY KEY (repo_id, tag)
);
```

`internal/repos` mirrors `internal/netrules` almost line for line: same DB file,
own connection, WAL, pure-Go driver, `tagRe` copied so the tag namespaces align
exactly, owner scoping structural in the query rather than enforced by handlers.
`ReposForSandbox(sandbox, owner)` is the inner join on `sandbox_tags`.

Nothing here is encrypted, because nothing here is secret: a repo slug is
configuration. The credential is Part 3's problem and it is never stored at all.

## 2.1 Why tags and not a per-sandbox list

Because a per-sandbox list is a thing you set *after* the sandbox exists, and
the entire point is that the clone happens before you get there. `stampTags`
already runs before `Create` for exactly this reason — the comment on it says
so — and a repo attachment inherits that ordering for free.

It also inherits the fan-out. `ssh new@gw -- --tag hivemind` is one gesture that
now means: these secrets, this egress policy, and this checkout. That is the
sentence worth being able to say.

## 2.2 The two footguns tags bring with them

**`default`.** `secrets.DefaultTag` is stamped on an untagged secret *and* on an
untagged sandbox, so the two halves meet. A repo attached to `default` therefore
clones into every sandbox the owner creates from then on. That is a legitimate
thing to want — dotfiles, a monorepo you always work in — and a surprising thing
to get by accident. The `netrules` doc already carries this warning; the `ctl`
surface should print it at attach time rather than leave it in a design doc.

**Retagging a live box.** `SetTags` on a running sandbox changes which secrets
it should see, and `envsync` pushes the difference immediately. The analogous
push for repos is a clone into a running guest, which is a much bigger action:
it writes into a filesystem somebody is using, it can take minutes, and it can
fail halfway. Proposal: **retagging never clones**. It updates the manifest the
guest can read, `sparkbox repos` shows the new entry as `not cloned`, and
`sparkbox repos sync` (in-guest, one command, the user's choice) does the work.
Clone-on-create is the automatic path; clone-on-retag is offered, not imposed.

---

# Part 3 — the credential

This is the half worth getting right, and the design is constrained by something
already true: a sandbox has an **unforgeable channel to its own gateway** and no
credential of its own. `internal/metadata` authenticates a guest by the source
address of its own `/30` tap, refuses a request whose destination is another
slot's host address, and the firecracker driver sets `rp_filter` so a spoofed
source is dropped rather than merely unanswered. That is how the OIDC token
already reaches a guest with nothing baked into the image.

A GitHub credential should arrive the same way, for the same reason.

## 3.1 The three candidate credentials

| | what it is | scope | lifetime | at rest where |
|---|---|---|---|---|
| **A. PAT as a secret** (today) | the user's `gh` token | every repo the human can reach | until revoked | the sandbox's `/etc/environment`, the sqlite store |
| **B. App user-to-server token** | device flow with scopes | intersection of the user's access and the app's installations | 8h + refresh token | the store, and refreshing needs a client **secret** on the gateway |
| **C. App installation token** | minted from the App private key | **exactly the repository ids asked for**, with permissions down-scoped per request | 1 hour, not refreshable | **nowhere** |

C is the answer. It is the only one of the three whose scope can be narrowed to
"this sandbox's repos", the only one that expires fast enough that a leak is an
incident rather than a breach, and the only one with nothing to steal from the
guest afterwards. B is worth naming only to say why it loses: a refreshable
user token still has to be stored, and storing it buys a *coarser* scope than C
gives for free.

A stays supported. Some orgs will not install a third-party App, GitHub
Enterprise Server is its own project, and a user with one weird repo should not
be blocked on an org admin. It becomes the documented fallback rather than the
documented path.

## 3.2 How C works, concretely

The gateway holds a ninth fleet secret.

| Secret | File | Blast radius |
|---|---|---|
| `github-app-key` | `github_app_key.pem` | Mints installation tokens for **every installation of the app** — i.e. every repo any user has granted it. Large. Unlike the OIDC key it is **revocable in one click**: regenerate the key in the App's settings and every copy is dead. |

It joins the manifest in `internal/bootsecrets/fetch.go` as `opaquePEM`,
`required: false` — a fleet that has not set up an App keeps working, with repo
attachment answering `Disabled`, which is the same shape `tagging is not enabled
on this host` already uses.

Minting, on the gateway only:

1. Sign a short-lived RS256 JWT with the App key (`iss` = the app's client id,
   `exp` ≤ 10 minutes).
2. `GET /repos/{owner}/{repo}/installation` with that JWT resolves a slug to the
   installation that covers it. Cached; installations change rarely.
3. `POST /app/installations/{id}/access_tokens` with
   `{"repository_ids": [...], "permissions": {"contents": "read"}}` returns a
   token good for one hour, scoped to nothing else.

The scope in step 3 comes from **the gateway's own ledger** — sandbox → owner →
tags → repos — never from anything the guest asked for. A guest that requests a
token for a repo it has no attachment to is answered 404, and a guest that
requests write on a `read` attachment is answered with a read token. This is the
same discipline as `Local.claims`: the guest names itself and the host decides
everything else.

## 3.3 The binding problem, and why it is the sharp edge

An installation belongs to a GitHub account or org. Sparkbox has to decide that
installation #12345 may be used on behalf of sparkbox handle `vanpelt`, and get
that decision right, because getting it wrong hands one user's private repos to
another user's agent.

This is the *same* hazard §2.3 of `github-linking-design.md` already reasoned
about for key adoption, and it takes the same answer: **bind on `github_id`, and
require a strong provenance.**

- A personal installation is usable by the handle whose `users.github_id`
  equals the installation's `account.id`. The id and not the login, because a
  login is renameable and, once released, re-registerable by somebody else.
- An org installation is usable by a handle that the org's membership includes —
  which `internal/users/githuborg.go` can already answer, and which is a pull,
  so it is a snapshot with all the deprovisioning caveats that file states.
- `users.StrongGitHubLink` gates attachment. A link recorded `assertion` may not
  attach a repo, for precisely the reason it may not adopt a key: a channel that
  could be wrong about which human is on the other end must not reach a verb
  that grants access.

That last bullet is the constraint that survives whatever else is decided here,
and it should be written next to the predicate rather than rediscovered.

## 3.4 What a compromised guest gets

One hour of access to the repos its own tags declare, at the permission its
attachments declare, and nothing else. Compare today: a `repo`-scoped PAT for
every repository the owner can reach, until they notice.

That is the whole argument for the extra machinery, and it should be the first
line of the changelog entry.

## 3.5 Fleet nodes

`internal/metadata` splits caller-identification (every host can do it) from
signing (only the gateway holds the key), via the `Identity` interface with a
`Local` implementation and a relay. The App key is in exactly the same position
as the OIDC key: it lives on the gateway and must stay there.

So repo credentials get the same treatment — a `GitHubCreds` interface with a
`Local` on the gateway and a relay over the fleet link on a node, where the node
sends **only the sandbox name** and the gateway resolves owner, tags and repos
from its own placement ledger. Same one check that makes the identity relay safe
makes this one safe. This is a small amount of work that must not be deferred:
a node implementation that trusts a node-supplied repo list is a cross-tenant
hole, and it is much easier to not write it than to find it later.

---

# Part 4 — the guest side

Two new endpoints on the metadata mux, next to `/token` and `/identity`:

```
GET /repos                     -> the manifest this sandbox should have
GET /github/credential?repo=…  -> {"username":"x-access-token","password":…,"expires_at":…}
```

Both are subject to the existing per-sandbox rate limiter. Tokens are cached
host-side per `(installation, repo-set, permission)` until five minutes before
expiry, so a guest running a build loop does not mint one per `git fetch`.

## 4.1 The credential helper

`/usr/local/bin/sparkbox-git-credential` speaks git's credential protocol: read
`protocol`/`host`/`path` from stdin on `get`, print `username` and `password`.
It is installed and configured by `deploy/install-guest-identity.sh` — the same
file, bumping `IDENTITY_REV`, so `refresh-agent-tools.sh` re-patches published
templates and no image rebuild is needed. That mechanism exists and works; this
is one more payload through it.

```
git config --system credential.https://github.com.helper /usr/local/bin/sparkbox-git-credential
git config --system credential.https://github.com.useHttpPath true
```

`useHttpPath` is not incidental. Without it git hands the helper only
`host=github.com`, and the helper has to ask for a token covering every attached
repo. With it git also hands over `path=wandb/hivemind`, so the helper asks for
a token scoped to the one repository this operation touches. That turns "scoped
to the sandbox" into "scoped to the fetch", for the price of one config line.

Nothing is written to disk. No token in `~/.git-credentials`, none in a remote
URL, none in `/etc/environment` — which also means none of it rides into a
snapshot, a fork, or an archived rootfs. The checkout does ride along, warm,
which is the part you want.

## 4.2 Cloning at boot

A `sparkbox-repos.service` oneshot, `After=sparkbox-token.service`, wanted by
`multi-user.target`. It reads `GET /repos`, clones what is missing, and does
nothing to what already exists.

The host does **not** pre-seed the clone into the rootfs before boot. It could —
`refresh-agent-tools.sh` mounts templates — but that script's own header
explains why it refuses to mount user-derived images: asking the privileged host
kernel to parse a guest's ext4 metadata turns the management plane into a second
sandbox boundary. Cloning into a template would also mean the host holding a
GitHub credential on behalf of a user, which is the thing this design is trying
to delete.

**It must not block sshd.** `docs`-worthy precedent: the first-attach race
already cost this platform a fix (`main@e196d5f`), and a clone of a large repo
is far slower than anything in that story. So the unit is `Type=oneshot` with no
ordering before `ssh.service`, and the arrival experience is handled in the
shell instead:

- the motd reports `repos: cloning wandb/hivemind…` or `2 repos ready`;
- `sparkbox repos` in-guest reports per-repo status and reasons;
- a failed clone is a warning in the motd, never a failed boot.

## 4.3 Layout

Default `~/<repo>` for a single attachment and `~/src/<owner>/<repo>` when there
is more than one, with `--path` overriding. The single-repo case is the common
one and `cd hivemind` beats `cd src/wandb/hivemind`; the multi-repo case needs
the disambiguation. Agents start in `~`, so both are one `cd` away and both are
visible in an `ls`.

Shallow by default is tempting and probably wrong for this workload: agents run
`git log`, `git blame` and `git bisect`. `--filter=blob:none` is the better
default — full history, blobs on demand — and `--depth` stays available for
someone who knows their repo is enormous.

---

# Part 5 — the seams this touches

**Egress.** A tagged sandbox is *filtered* (`sluice`, v0.2.0): untagged is
unlimited, tagged gets the base allowlist plus its tag's rule-set. So the moment
a repo is attached to a tag, that tag's sandboxes need `github.com`,
`codeload.github.com` and `objects.githubusercontent.com` reachable, or the
clone fails with a DNS NXDOMAIN and an error that names none of this.

Do not make the user discover that. A repo attachment should **imply** those
allow entries — added as an overlay inside `netrules.AllowForSandbox`, which is
already the one place the enforced list is computed, and *not* written into the
user's rule-set. The console's Network panel then still shows what the user
wrote, and the effective list shows what is enforced. Writing them in would mean
a detached repo silently leaves its holes behind.

Worth noting while here: `netrules` has **no `ctl` and no REST surface at all**
— it is reachable only from the user console. Repos should not repeat that. The
`ctl` path is where a new sandbox gets made, so it is exactly where somebody
finds out their tag has no repo on it.

**Snapshots and forks.** Clones live in the rootfs and ride into a fork, which
is a feature. The credential does not, because there is no credential. One thing
to check: `git config` in the cloned repo must not have a URL-embedded token,
which the helper approach guarantees, and the fork's new sandbox gets its own
`GET /github/credential` from its own tap.

**Archive/restore.** No new state. The manifest is host-side, the clone is in
the rootfs, both survive.

**Reserved names.** `internal/reserved` is one list shared by sandbox names,
routes and handles. Repo *attachments* are not named, so nothing new — but if
attachments ever get nicknames, that is the list they go in.

---

# Part 6 — surfaces

```
ctl@ repo add wandb/hivemind --tag hm [--write] [--ref main] [--path src/hm]
ctl@ repo ls [--tag hm]
ctl@ repo rm wandb/hivemind
ctl@ repo check                 # which attachments the app can actually reach
ctl@ github install             # prints the install URL for the App
```

`repo check` earns its place: the failure mode of this whole design is "the App
is not installed on that repo", and it is invisible until a clone fails inside a
VM at boot. `add` should run the check inline and say
`attached — but the app is not installed on wandb/hivemind yet: <url>` rather
than accept silently.

REST mirrors it under the existing conventions —
`GET|POST /v1/repos`, `DELETE /v1/repos/{host}/{owner}/{name}`, and the tag
endpoints already exist. The user console (`my.<domain>`) grows a Repos panel
next to Secrets and Network, which is where it belongs: those three panels are
the same object viewed three ways.

---

# Part 7 — build order

Each of these is shippable alone and useful alone.

1. **`internal/repos` + `ctl repo` + the console panel.** Attachment as pure
   configuration, no credential anywhere. Sandboxes clone public repos at boot
   with no auth at all. This is genuinely useful on its own and it proves the
   manifest, the guest unit, the layout and the motd without touching a key.
2. **The App key and installation tokens.** The fleet secret, the mint path,
   the `github_id` binding, the `StrongGitHubLink` gate, `repo check`.
3. **The credential helper and `useHttpPath` scoping.** One `IDENTITY_REV` bump.
4. **The node relay.** Must land with or before any node ever runs (2).
5. **The egress overlay.** Can trail, because until it lands the workaround is
   one allow entry added in the console's Network panel — but the error it
   produces first (an NXDOMAIN from `sluice`, mid-clone, in a boot log) names
   none of this, so it should not trail far.
6. **Deprecate the PAT path in the help text.** Keep the code; stop printing it
   as the first suggestion.

---

# Part 8 — open questions

1. **Its own App, or Hivemind's?** The linking flow already ships Hivemind's
   client id, and reusing it means one consent screen for the user. But an App's
   *permissions* are set per App, and adding `contents: write` to the app people
   currently authorize for identity alone silently widens what a past consent
   meant. A separate `sparkbox` App is the honest answer and costs one more
   install; worth confirming before (2) starts.
2. **Org installations and who may use them.** Membership is a pull and a
   snapshot (see `githuborg.go`). Does a user who left the org lose repo access
   at the next sync, at the next token mint, or when an operator notices? "At
   the next mint, by re-checking membership" is the strict answer and the
   expensive one.
3. **Write access defaults.** `--write` per attachment is proposed. Does a PR
   from a sandbox need `pull_requests: write` too, and should that be its own
   flag or implied by `--write`?
4. **Rate limits.** 5,000/hr per installation is plenty for humans and not
   obviously plenty for a fleet of agents in a loop. The host-side cache makes
   this a non-issue for `git fetch`; it may not for a guest that shells out to
   `gh`.
5. **`gh` and other tools.** The helper fixes `git`. `gh` reads `GH_TOKEN`, and
   an agent that runs `gh pr create` will not find one. Exporting a short-lived
   token into the environment re-introduces the at-rest problem in miniature;
   a `gh auth` credential shim, or a `sparkbox gh` wrapper that fetches and
   `env`-passes per invocation, is the better shape and needs a decision.
6. **GHES.** The `host` column reserves it. Nothing else here assumes
   github.com, but nothing else here has been checked against it either.
