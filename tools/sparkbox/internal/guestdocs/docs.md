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

## Agent harnesses

New VMs include Claude Code, Codex, Pi, and Hivemind. Sparkbox environment
guidance is installed at `~/.agents/AGENTS.md` and linked into each harness's
global instruction location. Repository-level instructions still apply.

## Supported surface

Only documented commands and URLs are public interfaces. Local metadata
endpoints, gateway ports, node services, and files outside the home directory
may change as Sparkbox evolves.
