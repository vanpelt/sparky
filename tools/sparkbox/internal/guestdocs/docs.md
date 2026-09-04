# Sparkbox

A Sparkbox is a persistent Firecracker microVM for development and agentic work.

## Persistence

The VM disk persists across disconnects, pauses, and cold starts. Keep projects
in the home directory and commit important work to source control.

## Shared resources

CPU and memory come from the owner's shared pool rather than being reserved for
each VM independently. Idle guest memory can be ballooned down and returned
when the VM becomes active. Turbo temporarily increases a VM's working
allocation, subject to the owner's burst ceiling and node availability.

Resource sharing changes performance, not disk persistence. Memory pressure
does not delete or recreate the VM.

A new VM starts with 4 vCPUs and 12 GB of RAM. Turbo restarts it with 8 vCPUs
and 24 GB for that run; the next pause returns it to its normal size.

## Reading boot logs

When diagnosing a systemd unit, use the merged journal view:

    sudo journalctl --merge -u <unit>

`--merge` (`-m`) reads every machine-id directory on the disk, including the
boot immediately before the current one. Plain `journalctl` selects only the
current machine id, so it can hide the previous boot's entries after a rename
or when inspecting an older VM or template.

## Pinning this VM

- Run `sparkbox pin` before starting a server, daemon, or long-running job that
  must remain continuously available.
- Run `sparkbox unpin` when that work finishes so the owner pool can reclaim
  idle resources.
- Run `sparkbox status` to inspect the current pin state.

A pinned VM is not idle-paused and is protected from memory-pressure
reclamation. Pinning consumes shared capacity continuously, so do not use it as
the default for every VM.

## Pausing this VM

- Run `sparkbox pause` to stop this VM now instead of waiting for the idle
  reaper. Memory and processes are snapshotted, so reconnecting picks up exactly
  where you left off.
- The gateway confirms before it pauses, so the line you see is the host's own,
  not a guess.
- A pinned VM comes back up on its own at the next host restart. `sparkbox
  unpin` first if you want it to stay down.

## Saving this VM as your tag's template

A tag on a VM already selects three things: the secrets it is handed, the
repositories checked out into it, and the egress it is allowed. It can also
select the **disk it boots from** — the template every new VM on that tag starts
as a copy of.

`sparkbox snapshot <tag>` captures this VM's disk and points that tag at it. So
the next `--tag <tag>` VM your account creates starts with everything you have
installed here.

- **It is bound to a tag this VM already carries.** You cannot point a tag this
  VM was never given at it; ask for the tag first (`ssh ctl@<domain> tags <this
  vm> <tag>`), or do the whole thing from outside. The image and the secrets it
  was built with then stay together.
- **It pauses this VM and ends your session.** There is no way to capture
  without pausing: the capture reads the block device, and only a paused VM has
  finished writing to it. Nothing is lost — reconnecting resumes this VM with its
  processes intact — but the capture itself runs after you are gone.
- **It prints what it is about to do and asks first.** The tag it will re-point,
  what that tag boots from today, and every VM of yours carrying it. Pass
  `--yes` to skip the question; without a terminal to ask at, it refuses rather
  than proceeding.
- **It shows you the state of every checkout it is about to freeze.** Which
  branch each is on, what is not pushed, what is uncommitted. All of that is
  captured exactly as it stands — nothing is committed, stashed or reset on your
  behalf — and every VM forked from this template inherits it. It is printed so
  it is not a surprise later, not because anything is wrong.
- **A git operation in flight stops it.** A rebase, merge, cherry-pick or bisect
  half-done in a checkout is copied into the template byte-for-byte, lock file
  and all, so every fork would come up with a git that refuses to run. Finish or
  abort it, or pass `--allow-busy` if you meant to.
- **Re-pointing does not re-base a VM that already exists.** Running or paused,
  every VM keeps the disk it was created from, this one included. Only VMs
  created afterwards boot from the new template.
- **The previous template is kept.** Nothing is deleted, and the old binding can
  be restored from outside with one `snapshot bind`.

The outcome lands minutes later, with this VM paused, so there is nowhere in
here for it to be reported. Read it from outside:

    ssh ctl@<domain> snapshot ls

`default` cannot carry a template: every VM you create carries that tag, so the
binding would reach all of them.

## HTTPS proxy

Applications listening in the VM can be reached through Sparkbox's
authenticated wildcard HTTPS edge. See [the proxy guide](./proxy.md).
The explicitly exposed development ports are {{COMMON_HTTPS_PORTS}}.

From inside the VM:

- `sparkbox set-port PORT` changes the default route's forwarded port.
- `sparkbox make-public [PORT]` makes one port unauthenticated. With no PORT it
  opens only the default port, never whatever else is listening.
- `sparkbox make-private [PORT]` restores authenticated access. With no PORT it
  closes every port.

A dev server's own Host-header check and hot-reload client both need explicit
wiring for a domain that is never `localhost`. See
[running a dev server behind the proxy](./dev-environment.md) for the
per-framework fixes and the `.sparkbox/setup.sh` convention for replaying the
setup on a fresh VM.

## Attached repositories

Repositories attached to one of your tags are cloned into this VM at boot, over
a short-lived GitHub App token that git fetches on demand — there is no personal
access token in the VM and nothing to rotate.

From inside the VM:

- `sparkbox repos` reports what is attached, where it was cloned, and the state
  of each checkout — up to date, behind, dirty, on another branch.
- `sparkbox repos sync` clones what is not there yet and brings what is there
  forward. Attaching a repository to a tag never reaches into a VM that already
  exists, so this is how an existing VM picks one up.
- `sparkbox repo authorize OWNER/NAME` authorizes one write attachment as the
  VM's owner. Afterward GitHub operations for that repository, including PR
  creation, are attributed to the user. Repositories you have not authorized
  continue using the Sparkbox bot token, so a VM with multiple repos can opt in
  one at a time.

Attaching and detaching happen outside the VM, with `ssh ctl@<domain> repo add`
or from the web console.

### What the repository credential can read

`gh` runs on the same per-repository credential `git` does, and it covers more
than code and pull requests. These all work inside a checkout, without any
setup:

    gh api repos/{owner}/{repo}/dependabot/alerts       # Dependabot alerts
    gh api repos/{owner}/{repo}/code-scanning/alerts    # code scanning
    gh run list                                         # workflow runs
    gh run view <id> --log                              # and their logs
    gh pr checks                                        # checks on a PR

There is no `gh dependabot` subcommand — security alerts are reachable through
`gh api` only, which is worth knowing before concluding the credential lacks
the permission.

Those five are read-only whatever the attachment says. A `--write` attachment
raises code, pull requests and issues to write; it does not let anything in the
VM dismiss an alert, cancel a run or create a deployment. A 403 on one of the
reads above usually means the App was never granted that permission on the
repository, which the **repos** panel in the web console reports per row.

A sync can do exactly three things to a checkout that already exists: fetch it,
fast-forward it when the tree is clean, or say why it did neither. It never
resets, rebases, merges anything but a fast-forward, stashes or deletes —
uncommitted edits, untracked files and unpushed commits are reported and left
alone. The one exception is narrow and deliberate: on the first boot of a VM
whose disk was just forked from a snapshot, a clean inherited checkout may be
switched to the branch the attachment names. Nobody has logged into that disk
yet, so there is no work in flight to lose; on every later run the branch you
are on is yours.

## Who this VM belongs to

    sparkbox whoami

reports the GitHub login and account number of this VM's owner, plus the owner's
Sparkbox handle and the VM's name, as `key: value` lines. `--json` prints the
host's identity document instead, for anything that would rather parse it.

Before per-repository authorization, `gh api user` cannot answer this because
the fallback credential is a GitHub App installation token with no user behind
it. After `sparkbox repo authorize OWNER/NAME`, commands scoped to that
repository use the linked user's token and GitHub attributes them to that user.

An owner with no linked GitHub account has no login to report, and the Sparkbox
handle is not a substitute — handles and GitHub logins are separate namespaces,
so a handle could be somebody else's login. `whoami` says so and exits non-zero
rather than answering with one.

## Commit authorship

git's author is configured at boot from the GitHub account linked to this VM's
owner, as `<id>+<login>@users.noreply.github.com` — the address github.com links
back to that account, without publishing a real one. Commits made in the VM are
therefore attributed on push with nothing to set up.

It is written in system scope, so `git config --global user.name` and
`git config --global user.email` still override it per VM and are never
overwritten. An owner with no GitHub account linked gets no author, and git
asks for one on the first commit.

## Agent harnesses

New VMs include Claude Code, Codex, Pi, and Hivemind. Sparkbox environment
guidance is installed at `~/.agents/AGENTS.md` and linked into each harness's
global instruction location. Repository-level instructions still apply.

This page's own URL is one an agent inside the VM cannot always reach:
`docs.<domain>` is a public DNS name that can resolve to this fleet's own
edge, which a VM's own network routing has no path back to. `sparkbox docs
[docs|proxy|dev-environment]` reads the identical content from inside the VM
over the always-open metadata channel instead.

## Updating the agent tools

The agent CLIs in a VM — `claude`, `codex`, `pi`, `hivemind` and
`agent-browser` — come from the template the VM was created from, and their own
auto-updaters are turned off so that one template means one set of versions and
no mid-session surprises. A VM that has been alive for a while therefore keeps
what its template shipped with, and this is the sanctioned way to move it.

From inside the VM:

- `sparkbox update-tools --check` lists each tool, the version installed here,
  the version the host has cached, and whether it is behind. It exits non-zero
  when anything is.
- `sparkbox update-tools` installs the difference. Each artifact is checked
  against the host's digest before it replaces anything, and one that fails is
  skipped rather than installed.

It pulls from this VM's own host rather than from the internet, so it works
unchanged on a VM whose egress is filtered by its tag. A newly created VM
normally reports everything current. Each update writes into this VM's own disk
and counts against the owner's pool, so it is a command to run when something is
actually behind, not on a timer.

## Supported surface

Only documented commands and URLs are public interfaces. Local metadata
endpoints, gateway ports, node services, and files outside the home directory
may change as Sparkbox evolves.
