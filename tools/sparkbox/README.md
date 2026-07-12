# sparkbox

A single-host MVP of an [exe.dev](https://exe.dev)-style agentic sandbox
service in Go: on-demand sandbox VMs behind a **smart SSH gateway** with
resume-on-connect. Companion implementation to
[`docs/agentic-sandbox-design.md`](../../docs/agentic-sandbox-design.md).

```
ssh new@gateway            → creates a sandbox, tells you its name
ssh <name>@gateway         → resumes it if suspended, drops you in
POST /v1/sandboxes         → control API (create/list/pause/resume/destroy)
```

SSH has no Host header, so the sandbox name travels in the SSH *username*;
the *user* is identified purely by their public key (the exe.dev model).
Idle sandboxes are automatically paused by a reaper and transparently resumed
on the next connection.

## Architecture

```
cmd/sparkbox            single binary: `sparkbox serve`
internal/sshgw          smart SSH proxy (gliderlabs/ssh): pubkey auth,
                        username routing, resume-on-connect, session piping
internal/host           manager: sandbox records, JSON state, idle reaper
internal/api            control-plane HTTP API
internal/vmm            driver interface
internal/vmm/mock       fake VMs (in-process ssh servers) — runs anywhere
internal/vmm/firecracker real microVMs via firecracker-go-sdk — needs /dev/kvm
```

## Quickstart (no KVM needed — mock driver)

```sh
go build ./cmd/sparkbox

ssh-keygen -t ed25519 -f mykey -N ''
echo "myuser $(cat mykey.pub)" > users.conf

./sparkbox serve --driver mock --state-dir ./state --users users.conf
# in another terminal:
ssh -i mykey -p 2222 new@localhost          # create a sandbox
ssh -i mykey -p 2222 <name>@localhost       # interactive shell / commands
```

The mock driver emulates each VM as an in-process SSH server executing in a
per-sandbox workdir — the full control plane, gateway, pause/resume, and
reaper paths are real; only the isolation is fake. `go test ./...` runs an
end-to-end suite over exactly this stack.

## Real microVMs

`--driver firecracker` boots actual Firecracker microVMs (rootfs templates
built from OCI images by `hack/build-rootfs.sh`, CoW reflink per-VM copies,
static tap networking, snapshot-to-disk on pause). Requires `/dev/kvm`, root,
a vmlinux, and has **not yet been exercised on real hardware** — see
[`docs/deploy-hetzner.md`](docs/deploy-hetzner.md) for bring-up and the
production gap list (jailer, warm snapshots, rate limits).

## Status / roadmap

- [x] Driver interface + mock driver, manager, JSON persistence, idle reaper
- [x] SSH gateway: pubkey identity, username routing, resume-on-connect,
      PTY + winch + env + exit-code piping, `new@` provisioning
- [x] Control API
- [x] Firecracker driver (compiles; untested pending a KVM host)
- [ ] Validate on a Hetzner auction box; measure cold-create and resume times
- [ ] Warm-snapshot pool (restore instead of cold boot on create)
- [ ] Jailer, balloon, I/O limits; inter-sandbox network isolation
- [ ] HTTPS edge proxy for in-sandbox web servers
- [ ] Multi-host: capacity reporting + best-fit placement (flyd pattern)
