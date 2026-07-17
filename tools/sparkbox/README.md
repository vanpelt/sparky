# sparkbox

A single-host MVP of an [exe.dev](https://exe.dev)-style agentic sandbox
service in Go: on-demand sandbox VMs behind a **smart SSH gateway** with
resume-on-connect. Companion implementation to
[`docs/agentic-sandbox-design.md`](../../docs/agentic-sandbox-design.md).

```
ssh signup@gateway         → registers your key (invite code + handle)
ssh new@gateway            → creates a sandbox, tells you its name
ssh <name>@gateway         → resumes it if suspended, drops you in
ssh ctl@gateway keys list  → manage your account's SSH keys
https://<name>.hivemind.tools       → reverse-proxy to a port inside the sandbox
https://<name>.hivemind.tools:4444  → …any port; private by default, login-gated
POST /v1/sandboxes         → control API (create/list/pause/resume/destroy/routes)
```

SSH has no Host header, so the sandbox name travels in the SSH *username*;
the *user* is identified purely by their public key (the exe.dev model). HTTP
*does* have a Host header, so web traffic routes by subdomain instead:
`<subdomain>.hivemind.tools` maps to a sandbox and a guest port (default `:8000`,
subdomain defaults to the sandbox name). Idle sandboxes are automatically
paused by a reaper and transparently resumed on the next connection — over SSH
*or* HTTP.

## Identity

Users are SSH public keys and nothing else — no passwords. (A browser session
for a private web route is a cookie, but its value is a token you mint from that
same SSH key; see *Authenticated forwarding* below.) An account (handle +
keyring + optional *verified* GitHub link, plus an optional email) lives in the
sqlite store next to the routes; `users.conf` remains the bootstrap seed, and
the accounts it names are the operators. Registration happens over the only
door a stranger has, SSH: `ssh signup@<domain>` runs a short dialog gated by a
single-use invite code (`ssh ctl@<domain> invite` mints one). GitHub linking
needs no OAuth app — it checks the key you're connected with against
`github.com/<login>.keys`, which is the same evidence GitHub accepts for a
push.

Each sandbox can also prove who it belongs to. Sparkbox is an OIDC issuer at
`oidc.<domain>`, and every guest gets a fresh 1h ES256 id token dropped at
`/var/run/secrets/hivemind/token` — the path hivemind's auth chain already
reads — so `hivemind start` federates with no secret in the VM and nothing
pasted. The guest is authenticated by its network position (it can only reach
the metadata endpoint over its own tap), exactly like a cloud IMDS. See
[`docs/identity-federation-design.md`](docs/identity-federation-design.md).

## Authenticated forwarding

Web routes are **private by default**: a visitor to `<name>.hivemind.tools`
(any port) is redirected to sign in and must be the sandbox's owner or an
operator. The login credential is a session token you mint from your SSH key —
`ssh ctl@<domain> session-token` — and paste into the sign-in page (or send as
`Authorization: Bearer <token>` for API access). It is a keyed MAC whose key is
HKDF-derived from the OIDC signing key, so this adds no new fleet secret and no
server-side session store. Once authorised, the edge forwards the visitor's
identity upstream as `X-Forwarded-User` / `X-Forwarded-Email` /
`X-Forwarded-Preferred-Username` (oauth2-proxy names; client-supplied copies are
stripped first), so the app behind the port can do its own authorization.

Publish a port to the world with `ssh ctl@<domain> share <name> public`
(`private` re-gates it); set the forwarded address with `ssh ctl@<domain> email
set you@example.com`. Any port works without pre-registering a route: a boot-time
`iptables REDIRECT` (`deploy/sparkbox-net.sh`) funnels the private port range to
the single TLS edge, which recovers the dialed port via `SO_ORIGINAL_DST`. See
[`docs/authenticated-proxy-design.md`](docs/authenticated-proxy-design.md).

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

### Operator console

A password-gated web console lists every sandbox on the host and lets you
pause a running one (or resume a paused one). It rides the proxy edge at
`console.<domain>`, so it's covered by the same wildcard cert/DNS as sandbox
routes — no extra record needed.

```sh
# enable it; empty password (the default) disables the console entirely
sparkbox serve ... --console-password s3cret        # or SPARKBOX_CONSOLE_PASSWORD=…
# then browse to https://console.hivemind.tools
```

The password is a single shared secret; prefer the `SPARKBOX_CONSOLE_PASSWORD`
env var over the flag so it stays out of `ps`/`systemd status`. A correct login
sets an HMAC-derived cookie (no server-side session store), `Secure` whenever
the edge serves TLS. The console UI is a single `go:embed`-ed page — no frontend
build. **Run it behind `--proxy-tls`**: the password crosses the wire on login.

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
- [x] Password-gated operator console at `console.<domain>` (list + pause/resume + pin)
- [x] Pinned "always-on" tier: reaper-exempt sandboxes so in-VM cron/daemons keep
      running (`ctl pin <name>`), resumed automatically on host boot
- [x] Platform scheduler: cron jobs the host fires by waking a scale-to-zero
      sandbox, so periodic work is reliable without keeping it warm
      (`ctl schedule add <box> "*/30 * * * *" <cmd>`); next wake shown in console
- [x] User accounts in sqlite: `signup@` over SSH, invite codes, multi-key
      keyrings (`ctl keys`), GitHub linking verified against `<login>.keys`
- [x] OIDC workload identity: issuer at `oidc.<domain>`, per-sandbox id tokens
      over an IMDS-style metadata endpoint, so `hivemind start` federates with
      no secret in the guest
- [x] Live memory overcommit: `deflate_on_oom` balloon per VM + two-stage idle
      reaper (balloon-down → pause) + working-set admission (`--mem-reserve-mb`),
      so idle VMs return RAM to the host while staying live. Measure the real
      per-VM cost + KSM savings with `hack/measure-density.py`
- [ ] Warm-snapshot pool (restore instead of cold boot on create)
- [ ] KSM host tuning + cgroup cpu.max; jailer, I/O limits; net isolation
- [ ] Multi-host: capacity reporting + best-fit placement (flyd pattern)
