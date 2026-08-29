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

## Supported surface

Only documented commands and URLs are public interfaces. Local metadata
endpoints, gateway ports, node services, and files outside the home directory
may change as Sparkbox evolves.
