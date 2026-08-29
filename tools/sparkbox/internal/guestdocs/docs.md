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

From inside the VM:

- `sparkbox set-port PORT` changes the default route's forwarded port.
- `sparkbox make-public` makes all routes for this VM unauthenticated.
- `sparkbox make-private` restores authenticated access to all routes.

## Attached repositories

Repositories attached to one of your tags are cloned into this VM at boot, over
a short-lived GitHub App token that git fetches on demand — there is no personal
access token in the VM and nothing to rotate.

From inside the VM:

- `sparkbox repos` reports what is attached, where it was cloned, and why
  anything is missing.
- `sparkbox repos sync` clones what is not there yet. Attaching a repository to
  a tag never reaches into a VM that already exists, so this is how an existing
  VM picks one up.

Attaching and detaching happen outside the VM, with `ssh ctl@<domain> repo add`
or from the web console. A clone that already exists is never touched: syncing
adds, and it does not update, reset or delete.

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
