# sparkbox

A single-host MVP of an [exe.dev](https://exe.dev)-style agentic sandbox
service in Go: on-demand sandbox VMs behind a **smart SSH gateway** with
resume-on-connect. Companion implementation to
[`docs/agentic-sandbox-design.md`](../../docs/agentic-sandbox-design.md).

```
ssh new@gateway            → creates a sandbox, tells you its name
ssh <name>@gateway         → resumes it if suspended, drops you in
https://<name>.hivemind.tools → reverse-proxy to a port inside the sandbox
POST /v1/sandboxes         → control API (create/list/pause/resume/destroy/routes)
```

SSH has no Host header, so the sandbox name travels in the SSH *username*;
the *user* is identified purely by their public key (the exe.dev model). HTTP
*does* have a Host header, so web traffic routes by subdomain instead:
`<subdomain>.hivemind.tools` maps to a sandbox and a guest port (default `:8000`,
subdomain defaults to the sandbox name). Idle sandboxes are automatically
paused by a reaper and transparently resumed on the next connection — over SSH
*or* HTTP.

## Architecture

```
cmd/sparkbox            single binary: `sparkbox serve`
internal/sshgw          smart SSH proxy (gliderlabs/ssh): pubkey auth,
                        username routing, resume-on-connect, session piping
internal/proxy          HTTP edge: <sub>.hivemind.tools -> guest IP:port,
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

Every sandbox is reachable over HTTP at `<name>.hivemind.tools`, forwarded to
port `8000` inside the VM by default. Change the port or add extra subdomains
through the control API:

```sh
# forward myvm.hivemind.tools to :3000 instead of :8000
curl -XPOST localhost:8080/v1/sandboxes/myvm/routes -d '{"port":3000}'
# expose a second subdomain -> :9000
curl -XPOST localhost:8080/v1/sandboxes/myvm/routes -d '{"subdomain":"api-myvm","port":9000}'
curl       localhost:8080/v1/sandboxes/myvm/routes    # list
curl -XDELETE localhost:8080/v1/routes/api-myvm       # remove
```

Route state lives in `<state-dir>/sparkbox.db` (sqlite). The proxy listens on
`--proxy-addr` (default `:8081`) under `--proxy-domain` (default
`hivemind.tools`). Previews are **public** — anyone with the URL reaches the
app, the same model as exe.dev.

### TLS

For a real public edge, point a wildcard DNS record `*.hivemind.tools` at the
host and add `--proxy-tls`. Two providers:

- **`--tls-provider cloudflare`** (default) — obtains a single
  `*.hivemind.tools` wildcard certificate via the ACME **DNS-01** challenge and
  auto-renews it (via [CertMagic](https://github.com/caddyserver/certmagic) +
  the Cloudflare libdns provider). One cert covers every sandbox subdomain, so
  no per-name issuance and no brush with Let's Encrypt rate limits regardless of
  how many ephemeral sandboxes churn. Needs a scoped `Zone.DNS:Edit` token in
  `CLOUDFLARE_API_TOKEN`; no inbound port 80/443 needed for issuance (DNS-01).
- **`--tls-provider autocert`** — per-host certificates issued on demand via
  TLS-ALPN-01/HTTP-01 (no DNS API needed, but needs `:443` + port `80`
  reachable, and each new subdomain is a separate cert subject to rate limits).

Run TLS on `:443`: `--proxy-addr :443 --proxy-tls`. Full walkthrough (Cloudflare
setup, nameserver move, records, IPv6): [`docs/deploy-dns.md`](docs/deploy-dns.md).

### IPv6

If your host has a **routed IPv6 `/64`** (Scaleway Elastic Metal, Hetzner, etc.
delegate one), pass it with `--subnet6` and every sandbox becomes dual-stack:
it keeps its internal IPv4 (for the SSH gateway and proxy hops) and additionally
gets a **globally-routable `/128`** carved from the block — no NAT, real v6
egress and a real v6 identity (surfaced as `guest_v6` in the sandbox API).

```sh
sparkbox serve --driver firecracker --subnet6 2001:bc8:702:1c7::/64 ...
```

Per slot the driver assigns a point-to-point `/127` (host `::2`, guest `::3`,
next VM `::4`/`::5`, …), leaving `::1` free for the host's own edge address. The
host must have IPv6 forwarding on (`net.ipv6.conf.all.forwarding=1`); the guest
side is configured at boot by the `sparkbox-netcfg` hook baked into the rootfs
(the kernel `ip=` arg is IPv4-only). No NAT — the `/64` is routed to the host,
which forwards inbound to each guest via the per-VM `/127` route.

**DNS:** the edge is dual-stack, so point records at the *host*, not at VMs —
`AAAA *.hivemind.tools → <host's own v6, e.g. 2001:bc8:702:1c7::1>` alongside
`A *.hivemind.tools → <host v4>`. Keep both **grey-cloud (DNS-only)** in
Cloudflare so sparkbox terminates TLS directly.

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
- [x] HTTPS edge proxy for in-sandbox web servers (`<sub>.hivemind.tools`,
      sqlite route store, resume-on-connect; TLS via Cloudflare DNS-01 wildcard
      cert or autocert on-demand)
- [ ] Validate on a Hetzner auction box; measure cold-create and resume times
      (incl. the proxy path — the host reaches the guest directly over the
      tap's /30, so no extra NAT is needed, but it's untested on hardware)
- [x] IPv6-native: dual-stack guests with a routable /128 per sandbox from a
      delegated /64 (`--subnet6`), no NAT
- [ ] Warm-snapshot pool (restore instead of cold boot on create)
- [ ] Jailer, balloon, I/O limits; inter-sandbox network isolation
- [ ] Multi-host: capacity reporting + best-fit placement (flyd pattern)
