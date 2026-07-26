# sparkbox

A single-host MVP of an [exe.dev](https://exe.dev)-style agentic sandbox
service in Go: on-demand sandbox VMs behind a **smart SSH gateway** with
resume-on-connect. Companion implementation to
[`docs/agentic-sandbox-design.md`](../../docs/agentic-sandbox-design.md).

```
ssh signup@gateway         → registers your key (invite code + handle)
ssh new@gateway            → creates a sandbox, tells you its name
ssh -t new+foo@gateway bar → names it `foo`, tags it `bar` (-t: see below)
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

One wrinkle on the `new@` / `new+<name>@` door: the words after it are read as
**tags**, never as a command, because a freshly created sandbox always gets a
shell. But `ssh host word` makes your ssh client skip terminal allocation, and
a shell with no terminal prints no prompt and gives you no line editing — you
end up typing blind into a pipe. Pass `-t` whenever you pass tags:

```
ssh -t new+foo@gateway claude   # sandbox `foo`, tagged `claude`, working prompt
```

Connecting to an existing sandbox (`ssh foo@gateway`) needs no `-t`, and
`ssh foo@gateway <cmd>` does run `<cmd>` in the guest as you'd expect.

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

## The same sandboxes, without ssh

Everything `ssh ctl@<domain>` does is also a REST call, and every sandbox also
has a shell in a browser tab. Both authenticate with the session token above,
and both run through the same control-plane core (`internal/ctlops`) as the SSH
channel — one ownership check, one set of timeout budgets, three transports.

```sh
# `session-token` writes the token to stdout and its notes to stderr, with the
# ctl channel's CRLF line endings — strip the \r or the header is malformed.
TOKEN=$(ssh ctl@hivemind.tools session-token | tr -d '\r\n')
API=https://api.hivemind.tools
AUTH="Authorization: Bearer $TOKEN"

# who am I, and what does this host actually have configured?
curl -sH "$AUTH" $API/v1/whoami
curl -sH "$AUTH" $API/v1/capabilities
# {"archiving":true,"snapshots":true,"scheduling":true,"tags":true,
#  "routes":true,"session_tokens":true,"terminal":true}

# create one (unnamed → an adjective-noun name), then list them
curl -sH "$AUTH" -H 'Content-Type: application/json' \
     -d '{"tags":["prod"]}' $API/v1/sandboxes
# {"name":"plucky-panda","state":"running","tags":["prod"],…,
#  "url":"https://plucky-panda.hivemind.tools",
#  "terminal_url":"https://plucky-panda-xterm.hivemind.tools"}
curl -sH "$AUTH" $API/v1/sandboxes          # {"sandboxes":[…]}

# act on one; retype the tag set; free the slot
curl -sH "$AUTH" -X PUT -H 'Content-Type: application/json' \
     -d '{"tags":["prod","gpu"]}' $API/v1/sandboxes/plucky-panda/tags
curl -sH "$AUTH" -X POST $API/v1/sandboxes/plucky-panda/pause
```

Browsable docs and the machine-readable contract live on the same host:
`https://api.<domain>/docs`, `/openapi.json`, `/openapi.yaml`. Collections come
back wrapped (`{"sandboxes":[…]}`) so a field can be added later without
breaking every parser, and failures past the auth gate are all one envelope —
`{"error":{"kind","op","code","message"}}` — with a stable `code` to match on
instead of a message to grep.

Operations that can take minutes (archive, resize, restore) answer
synchronously when they finish fast and escalate to `202` + a job resource when
they don't, because no proxy on the path tolerates a fifteen-minute request.
`Prefer: wait=0` forces the `202` immediately; `Prefer: respond-async` does too;
either way you poll `/v1/jobs/{id}`. Asking twice for the same work on the same
sandbox returns the job already running rather than starting a second one, and
`Idempotency-Key` replays any mutation for 24 hours.

The browser terminal is `https://<name>-xterm.<domain>` — an xterm.js page over
an authenticated WebSocket into a real PTY in that sandbox, with the same
resume-on-connect as `ssh <name>@<domain>`. Open it and you get a shell; nothing
to install, and the sign-in is the same passkey/token flow as any private route.
One host per sandbox, so the browser's own origin isolation keeps one sandbox's
page from scripting another's socket. The identical bridge is mounted at
`wss://api.<domain>/v1/sandboxes/<name>/terminal` for clients that are not
browsers — bearer token, no `Origin`, subprotocol `sparkbox.terminal.v1`, raw
binary frames each way.

Pause the sandbox under a live terminal and the session is hung up cleanly: the
page restores your terminal modes, prints why, and offers Reconnect rather than
reconnecting on its own and resurrecting the VM the reaper just parked. Typing
counts as activity (throttled to once a minute); merely having the tab open does
not, so a forgotten tab cannot pin a sandbox warm forever.

`--api-subdomain` and `--xterm-subdomain` move or disable either surface. See
[`docs/rest-api-and-xterm-design.md`](docs/rest-api-and-xterm-design.md).

> The terminal host is one label on purpose. A wildcard matches exactly one
> label — RFC 4592 in DNS, RFC 6125 in certificates — so the dotted
> `<name>.xterm.<domain>` would need a second wildcard in *both*, and hosted TLS
> front ends generally will not issue it: Cloudflare's universal certificate is
> `<domain>, *.<domain>` and stops there, so the deeper name dies inside the TLS
> handshake with `ERR_SSL_VERSION_OR_CIPHER_MISMATCH` and no sparkbox log line
> to explain it. `<name>-xterm.<domain>` needs nothing beyond the `*.<domain>`
> record and certificate every sandbox front door already uses. The cost is a
> reserved name suffix: sandboxes and routes may not end in `-xterm`.

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
internal/ctlops         control-plane core: one method per ctl@ command,
                        shared by the SSH channel, the REST API and the terminal
internal/restapi        authenticated REST surface at api.<domain> + OpenAPI
internal/xterm          browser terminal at <name>-xterm.<domain> (WebSocket→PTY)
internal/api            legacy loopback-only CRUD API — never mount on the edge
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
  how many ephemeral sandboxes churn — but *rebuilding the host* does brush one:
  the wildcard is the same certificate every time, and duplicates are capped at
  five per week, so back up `<state-dir>/certmagic` before you wipe anything
  ([getting-started](docs/getting-started.md#rebuilding-a-host-keep-the-certificate-cache)).
  Needs a scoped `Zone.DNS:Edit` token in
  `CLOUDFLARE_API_TOKEN`; no inbound port 80/443 needed for issuance (DNS-01).
  That one pair is everything — browser terminals live at
  `<name>-xterm.hivemind.tools`, a single label, so they are covered by the same
  wildcard rather than needing one of their own. The startup log line
  `tls certificates managed` reports what was actually obtained.
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

## Deploy on your own host

The `sparkbox` binary is its own installer. On any KVM-capable Linux host:

```sh
sparkbox doctor        # is this host ready? (PASS/WARN/FAIL per prerequisite)
sudo sparkbox setup --proxy-domain yourdomain.tools   # provision + start (idempotent; --dry-run to preview)
```

`setup` installs the binary you ran it from to `/usr/local/bin/sparkbox`
(`--bin-path`) so the unit runs exactly the build that provisioned the host,
fetches a prebuilt release (guest kernel, firecracker, rootfs — all
sha256-verified), lays down an XFS reflink volume, seeds your operator SSH key,
installs the systemd units, and starts the gateway. It then verifies the result
and **exits non-zero** if the gateway is not alive: a crash-looping service is a
FAIL carrying the tail of its journal, not a PASS. `doctor` runs the same
battery at any time. Full walkthrough — prerequisites, TLS, port 22, day-2 ops — in
[`docs/getting-started.md`](docs/getting-started.md). For a Scaleway zero-touch
fleet instead, see [`docs/deploy-scaleway.md`](docs/deploy-scaleway.md).

### On a Mac (Apple Silicon)

Same binary-is-the-installer story, same two commands:

```sh
curl -fLO https://github.com/vanpelt/sparky/releases/latest/download/sparkbox-darwin-arm64
chmod +x sparkbox-darwin-arm64 && sudo mv sparkbox-darwin-arm64 /usr/local/bin/sparkbox

sparkbox doctor
sparkbox setup --proxy-domain yourdomain.tools
```

macOS has no KVM, so a Mac cannot run Firecracker directly. `setup` therefore
provisions a **nested Linux machine** with Apple's
[`container`](https://github.com/apple/container) CLI and runs the real Linux
`sparkbox setup` *inside* it — the same steps, the same release, the same
`sparkbox.env`. What you get is an ordinary sparkbox gateway that happens to
live one layer down.

Prerequisites. `sparkbox setup` checks every one of these *before* it touches
anything and refuses with the reason. `sparkbox setup --dry-run` runs the same
preflight and changes nothing, so it is the safe way to ask whether this Mac
qualifies — a host that passes prints no preflight section at all, and anything
that does not is listed with its fix:

- **Apple Silicon M3 or newer** — nested virtualization is an M3+ feature, and
  it is what lets Firecracker run inside the machine. An M1/M2 fails here.
- **macOS 15 or newer**, and Apple's `container` CLI **1.1.0 or newer**, with
  its service running (`container system start`).
- Disk for the machine's data volume (`--data-volume-gb`).

`sparkbox doctor` answers the *other* question — how are both hosts doing now.
It reports the Mac (`mac:` lines — the same prerequisites, plus the outer kernel
verified against the release's checksum) and the nested machine (`machine:`
lines — state, nested virtualization, `/dev/kvm`, the gateway service), ending
with the gateway's **own `doctor` relayed out of the machine**. It exits
non-zero if either layer is wrong, and when the machine is missing or stopped it
says so in one line rather than printing an empty section. Use `setup --dry-run`
before provisioning, `doctor` after.

Everything you would pass on Linux still means the same thing and is forwarded
to the gateway inside the machine — `--proxy-domain`, `--operator-key`,
`--sluice`, `--gateway`/`--node-name`, the listen addresses, TLS flags. Only
these describe the Mac itself:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--machine-name` | `sparkbox` | the nested VM to create/adopt |
| `--machine-cpus` / `--machine-memory-gb` | 8 / 24 | the machine's whole budget |
| `--machine-image` | content-addressed | override the gateway image reference |
| `--outer-kernel` | `~/Library/Application Support/sparkbox/vmlinux-macos-arm64` | where the outer kernel is stored |
| `--container-bin` | `container` | path to Apple's CLI |

Two flags are **refused** on darwin rather than ignored: `--move-admin-ssh`
(the machine runs one process and nothing competes for `:22` inside it) and
`--bin-path` (the gateway's binary is installed by the inner `setup`, not by
the Mac). `--dry-run` prints the whole plan — including the exact inner `setup`
command line — and touches nothing.

> **Two kernels, do not confuse them.** `vmlinux-macos-arm64` is the *outer*
> KVM-capable kernel Apple's `container machine` boots; `vmlinux-<arch>` is the
> *guest* kernel each Firecracker sandbox boots, one layer further in. `setup`
> fetches and sha256-verifies both.

An existing machine is adopted only if its name, image repository and
`home-mount=none` all match, so `setup` never takes over a machine you created
for something else.

> **What is proven, and what is not.** CI builds and unit-tests the darwin paths
> on a real Apple Silicon runner, but GitHub's hosted Macs have no `container`
> CLI and cannot nest VMs, so **no CI anywhere creates a real machine** — the
> lifecycle tests drive a fake. `macos/poc.sh` remains in the tree as the
> fallback and as the reference the Go port was written from; it uses a separate
> machine (`sparkbox-poc`), so the two coexist on one Mac.

### Adding a second machine

A second host joins an existing gateway as a **node**. Provision the gateway
first, exactly as above, then run `setup` on the new machine with `--gateway`:

```sh
sudo sparkbox setup --gateway <gateway-host>:2222 --node-name laptop
```

`--gateway` is the whole difference: instead of standing up a gateway of its
own, the machine mints a node key, dials *out* to that address (so a node behind
NAT needs no inbound anything), enrolls, and parks as **pending**. Nothing is
trusted yet. It logs its own identity at startup and then the exact command that
unblocks it:

```
node identity  node=laptop fingerprint=SHA256:IZWmZrHR+PrPOFr5DI5b93scC2XC+0uEZ2pD76MJpnM
this node is enrolled and waiting for approval  node=laptop gateway=10.66.0.1:2222
  ssh ctl@catnip.sh node approve SHA256:IZWmZrHR+PrPOFr5DI5b93scC2XC+0uEZ2pD76MJpnM
  — after checking that fingerprint against the one this machine printed at startup.
```

On the gateway, as an operator, read the roster and approve — by **fingerprint**,
never by name, because a node chooses its own name and only the key is evidence:

```sh
ssh ctl@<gateway> node ls        # name, status, presence, arch, sandbox count, fingerprint
ssh ctl@<gateway> node approve SHA256:IZWmZrHR+PrPOFr5DI5b93scC2XC+0uEZ2pD76MJpnM
ssh ctl@<gateway> node rm laptop # drop one; refused while it still holds sandboxes
```

Compare the fingerprint against the one the node printed before you say yes —
that out-of-band comparison is the entire trust decision. The node retries on
its own backoff (~30s), so approval needs no restart: it reconnects, logs
`linked to the gateway`, and starts heartbeating. The same roster is
`GET /v1/nodes` over the REST API.

> **What a fleet is at v0.4.0.** What shipped is the **link layer** — enrollment,
> trust, roster, heartbeat, capacity and architecture reporting, all surviving a
> restart on either side. **No sandbox lands on a remote node yet.** `ssh
> new@<gateway>` always creates on the gateway itself: `Fleet.Create` has no
> placement step and there is no `--node` override. Remote lifecycle is the next
> milestone, and the data plane that makes a remote sandbox indistinguishable
> from a local one is the one after. So a fleet today is a trusted roster of
> machines, not a scheduler — worth knowing before you add a node expecting your
> laptop to start running sandboxes. The blueprint for both milestones is
> [`docs/multi-node-implementation.md`](docs/multi-node-implementation.md).

## Real microVMs

`--driver firecracker` boots actual Firecracker microVMs (rootfs templates
built from OCI images by `hack/build-rootfs.sh`, CoW reflink per-VM copies,
static tap networking, snapshot-to-disk on pause). Requires `/dev/kvm`, root,
a vmlinux, and has **not yet been exercised on real hardware** — see
[`docs/deploy-hetzner.md`](docs/deploy-hetzner.md) for bring-up and the
production gap list (jailer, warm snapshots, rate limits).

The default base image is **self-built** from [`images/Dockerfile`](images/Dockerfile)
(a lean Ubuntu 24.04 + Go/Python·uv/Node + headless Chrome, ~4GB — replacing the
~30GB `codex-universal`). It logs in as a non-root **`sparky`** user, declared by
the image's `sparkbox.login-user` label and honored end-to-end (build-rootfs bakes
the gateway key into `/home/sparky`, the release manifest carries
`ROOTFS_LOGIN_USER`, and `sparkbox serve --default-login-user` follows it).

## Releases

Artifacts ship as **GitHub Releases, built for linux/amd64, linux/arm64 and
darwin/arm64** — the same tag provisions an x86 cloud VM, an aarch64 DGX Spark
or an Apple Silicon Mac. Push a `v*` tag and
[`.github/workflows/build-artifacts.yml`](../../.github/workflows/build-artifacts.yml)
does the rest: Depot builds the `images/Dockerfile` base for both linux platforms
and pushes one multi-arch tag to GHCR, then a matrix of native runners
(`ubuntu-24.04` / `ubuntu-24.04-arm`) each compile the guest kernel, build the
`sparkbox` and `sluice` binaries, and flatten their arch's image into an ext4
template; alongside them a native arm64 runner compiles the macOS *outer* kernel
once, and a cheap cross-compile leg emits the Mac's own binary and manifest. The
release only goes public once **every** platform lands, so `setup` can never
resolve a half-populated `latest`.

```sh
git tag v0.4.0 && git push origin v0.4.0    # cut a release
gh workflow run "sparkbox release"          # ad-hoc dev-<ts> prerelease (doesn't move `latest`)
```

A release's asset namespace is flat, so every name carries the platform it is
for. `sparkbox setup` picks its own set, pinned to the tag the manifest names:

| Asset | What | Who fetches it |
| --- | --- | --- |
| `sparkbox-linux-<arch>` | control-plane binary | curl'd by the operator; `setup` then installs the binary it is *running* |
| `sluice-linux-<arch>` | egress gateway (DNS allowlist + eBPF meter) | `setup --sluice`, on either platform — it is a linux daemon even when a Mac puts it inside the nested machine |
| `vmlinux-<arch>` | **guest** kernel a microVM boots | `setup` |
| `firecracker-<arch>` | the VMM | `setup` |
| `universal-<arch>.ext4.zst` | guest rootfs template | `setup` |
| `manifest-<arch>.env` | sha256s + metadata; unqualified name means **linux** | `setup`, `macos/sparkbox-bootstrap.sh` |
| `sparkbox-darwin-arm64` | the binary a Mac runs | the operator on macOS |
| `manifest-darwin-arm64.env` | the Mac's manifest — repeats the linux arm64 checksums it provisions, plus `MACHINE_SPARKBOX_ASSET` and `OUTER_KERNEL_ASSET` | `setup` on darwin, `macos/kernel/fetch.sh` |
| `vmlinux-macos-arm64` (+ `.config`) | the **outer** KVM kernel Apple's `container machine` boots — a different kernel from `vmlinux-<arch>` | `macos/kernel/fetch.sh` |

To build a release by hand on a build host, `hack/stage-artifacts.sh` stages one
linux arch into `OUT_DIR` (blank `IMAGE` = build the base image locally; set
`IMAGE=` to flatten a prebuilt one instead), and `hack/stage-darwin-artifacts.sh`
derives the darwin pair from the arm64 manifest that produced.

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
- [x] Vitals-based idleness: the reaper samples each VM's host CPU time and tap
      byte counters, so an unattended agent, build, or training run counts as
      active with no inbound traffic (`--activity-cpu-pct`, `--activity-net-kb`).
      An idle box measures ~0.4% CPU / ~3 KB/min against 3.6–14% / 400 KB+ for a
      working agent. Lifetime bytes in/out are metered per sandbox and shown in
      both consoles — the basis for future egress limits
- [x] Clean hang-up on pause: attached terminals get their modes restored
      (mouse reporting, alternate screen, bracketed paste) and a reason, instead
      of being left wedged against a VM that stopped answering
- [ ] Warm-snapshot pool (restore instead of cold boot on create)
- [ ] KSM host tuning + cgroup cpu.max; jailer, I/O limits; net isolation
- [x] Fleet link layer: a second machine joins with `setup --gateway host:port`,
      enrolls by its own key, is approved by fingerprint (`ctl node approve`),
      and reports arch + capacity over a heartbeat — see *Adding a second
      machine*. The roster is real; the scheduler is not (next item)
- [ ] Multi-host placement: remote create/pause/resume and best-fit scheduling
      (flyd pattern). Until this lands every sandbox runs on the gateway,
      whatever the roster says
