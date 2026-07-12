# sparkbox

A single-host MVP of an [exe.dev](https://exe.dev)-style agentic sandbox
service in Go: on-demand sandbox VMs behind a **smart SSH gateway** with
resume-on-connect. Companion implementation to
[`docs/agentic-sandbox-design.md`](../../docs/agentic-sandbox-design.md).

```
ssh new@gateway            → creates a sandbox, tells you its name
ssh <name>@gateway         → resumes it if suspended, drops you in
https://<name>.hivemind.sh → reverse-proxy to a port inside the sandbox
POST /v1/sandboxes         → control API (create/list/pause/resume/destroy/routes)
```

SSH has no Host header, so the sandbox name travels in the SSH *username*;
the *user* is identified purely by their public key (the exe.dev model). HTTP
*does* have a Host header, so web traffic routes by subdomain instead:
`<subdomain>.hivemind.sh` maps to a sandbox and a guest port (default `:8000`,
subdomain defaults to the sandbox name). Idle sandboxes are automatically
paused by a reaper and transparently resumed on the next connection — over SSH
*or* HTTP.

## Architecture

```
cmd/sparkbox            single binary: `sparkbox serve`
internal/sshgw          smart SSH proxy (gliderlabs/ssh): pubkey auth,
                        username routing, resume-on-connect, session piping
internal/proxy          HTTP edge: <sub>.hivemind.sh -> guest IP:port,
                        resume-on-connect, reverse proxy (websockets ok)
internal/routes         sqlite route store (subdomain -> sandbox:port),
                        pure-Go modernc.org/sqlite, no cgo
internal/host           manager: sandbox records, JSON state, idle reaper
internal/api            control-plane HTTP API (sandbox + route CRUD)
internal/vmm            driver interface
internal/vmm/mock       fake VMs (in-process ssh servers) — runs anywhere
internal/vmm/firecracker real microVMs via firecracker-go-sdk — needs /dev/kvm
```

## Web routing

Every sandbox is reachable over HTTP at `<name>.hivemind.sh`, forwarded to
port `8000` inside the VM by default. Change the port or add extra subdomains
through the control API:

```sh
# forward myvm.hivemind.sh to :3000 instead of :8000
curl -XPOST localhost:8080/v1/sandboxes/myvm/routes -d '{"port":3000}'
# expose a second subdomain -> :9000
curl -XPOST localhost:8080/v1/sandboxes/myvm/routes -d '{"subdomain":"api-myvm","port":9000}'
curl       localhost:8080/v1/sandboxes/myvm/routes    # list
curl -XDELETE localhost:8080/v1/routes/api-myvm       # remove
```

Route state lives in `<state-dir>/sparkbox.db` (sqlite). The proxy listens on
`--proxy-addr` (default `:8081`) under `--proxy-domain` (default
`hivemind.sh`). For a real public edge, point a wildcard DNS record
`*.hivemind.sh` at the host and pass `--proxy-tls` to have sparkbox obtain
per-subdomain certificates on demand via ACME (needs `:443` and port `80`
reachable). Previews are **public** — anyone with the URL reaches the app,
the same model as exe.dev.

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
- [x] Control API (sandbox + route CRUD)
- [x] Firecracker driver (compiles; untested pending a KVM host)
- [x] HTTPS edge proxy for in-sandbox web servers (`<sub>.hivemind.sh`,
      sqlite route store, resume-on-connect, ACME on-demand certs)
- [ ] Validate on a Hetzner auction box; measure cold-create and resume times
      (incl. the proxy path — the host reaches the guest directly over the
      tap's /30, so no extra NAT is needed, but it's untested on hardware)
- [ ] Warm-snapshot pool (restore instead of cold boot on create)
- [ ] Jailer, balloon, I/O limits; inter-sandbox network isolation
- [ ] Multi-host: capacity reporting + best-fit placement (flyd pattern)
