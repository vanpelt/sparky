# Getting started: stand up a sparkbox host

This is the operator quickstart for running your own sparkbox on **any Linux
host with KVM** — a bare-metal box, a nested-virt cloud instance, a DGX, a
homelab server — or on an **Apple Silicon Mac**. The `sparkbox` binary is its
own installer: point it at a host and it fetches a prebuilt release, lays down
the storage and networking, wires systemd, and starts the gateway.

```
sparkbox doctor        # is this host ready?
sparkbox setup         # provision it into a running gateway
```

Those two commands are the whole story on both platforms. On a Mac they do one
extra thing — provision a nested Linux machine to run the gateway in, because
macOS has no KVM — and that is described in [macOS](#macos-apple-silicon) below.
Sections 0–5 are the Linux walkthrough; the macOS section tells you which parts
of it differ.

For a provider-managed, zero-touch fleet on Scaleway Elastic Metal, see
[deploy-scaleway.md](deploy-scaleway.md) instead. To just kick the tires with no
VM at all, jump to [Local dev](#local-dev-no-kvm) below.

## 0. What you need

- A **Linux host with hardware virtualization** — `/dev/kvm` must exist and the
  CPU must expose `vmx` (Intel) or `svm` (AMD). Bare metal, or a cloud instance
  with nested virtualization enabled. `sparkbox doctor` checks this for you.
- **Root** on that host (firecracker needs `/dev/kvm` and tap devices).
- Ideally a **wildcard DNS record** (`*.yourdomain`) pointed at the host, if you
  want web routes and HTTPS. You can add this later.
- An **SSH public key** — the operator identity. `setup` auto-detects
  `~/.ssh/id_ed25519.pub`, or pass one explicitly.

Outbound HTTPS to the artifact bucket is required during `setup` (it downloads
the guest kernel, the firecracker binary, and the rootfs template).

## 1. Get the binary onto the host

Build it from this repo:

```sh
cd tools/sparkbox
go build -o sparkbox ./cmd/sparkbox
scp sparkbox root@<host>:/usr/local/bin/sparkbox
```

(Or download the `sparkbox` binary from your artifact release. `setup` itself
fetches the *kernel*, *firecracker*, and *rootfs* — it does not need to build
anything on the host.)

It does not matter where the binary lands: `setup` installs *itself* to
`/usr/local/bin/sparkbox` — the path the systemd unit runs — so
`curl`-ing it into `$PWD` and running `sudo ./sparkbox setup` is enough. Pass
`--bin-path` to install it elsewhere (the unit follows), or `--bin-path ""` to
manage the binary yourself. If the destination already holds a *newer* sparkbox,
the install is refused unless you pass `--force`, and when the binary changes
under a running gateway `setup` restarts the service so the new build actually
takes effect.

## 2. Preflight

```sh
ssh root@<host>
sparkbox doctor
```

`doctor` reports **PASS / WARN / FAIL** for every prerequisite — KVM, CPU
virtualization, IP forwarding, the guest kernel, the rootfs template, the fleet
keys, `users.conf`, disk space, the NAT rules, whether the service is genuinely
*alive* (it samples the unit twice ~10s apart, so a gateway restarting every two
seconds reads as a FAIL with its journal inlined, not as "active"), and whether
the running service, the installed binary and `--release` are the same version —
each with a one-line fix. Before provisioning, the two that must pass
are **kvm device** and **hardware virtualization**; everything else `setup`
puts in place. It exits non-zero if anything FAILs, so it drops into CI too.

## 3. Provision

```sh
sudo sparkbox setup --proxy-domain yourdomain.tools
```

That runs the full pipeline (preview any time with `--dry-run`):

1. **swapfile** — a 16 GiB overcommit safety valve.
2. **resolve-release** + **fetch-artifacts** — download and **sha256-verify**
   the guest kernel, firecracker, and the rootfs template from the release
   bucket, decompressing the rootfs in place.
3. **data-volume** — a 300 GiB **XFS reflink** volume at `/srv/sparkbox/data`,
   so each sandbox's rootfs is an instant copy-on-write clone.
4. **users.conf** — seed your operator SSH key.
5. **host-config** — write `/srv/sparkbox/sparkbox.env`.
6. **net-rules** — install the packet-filter script + sysctls (IP forwarding,
   strict rp_filter, sandbox NAT, any-port edge REDIRECT).
7. **systemd-units** — install `sparkbox.service` + `sparkbox-net.service`.
8. **enable-services** — start everything, then re-run the health checks.

Every step is idempotent: re-running `setup` fixes only what drifted, and an
already-verified artifact is not re-downloaded.

Useful flags:

| Flag | Default | Purpose |
|---|---|---|
| `--proxy-domain` | `hivemind.tools` | base domain for `<name>.<domain>` web routes |
| `--operator-key` | auto-detect `~/.ssh/*.pub` | path to, or literal text of, your SSH public key |
| `--move-admin-ssh` | off | relocate the host's own sshd to **:2222** so the gateway owns **:22** (see below) |
| `--release` | `latest` | pin a specific artifact release tag |
| `--artifact-base` | the public bucket | point at your own release mirror |
| `--data-volume-gb` / `--swap-gb` | 300 / 16 | size the data volume / swapfile |
| `--dry-run` | off | print the plan and change nothing |

### Port 22 and the SSH gateway

By default the gateway binds **:2222** and leaves the host's own sshd on :22, so
`setup` can never lock you out. You connect with `ssh -p 2222 new@<host>`.

For the clean `ssh new@<host>` experience, re-run with **`--move-admin-ssh`**:
the host's admin sshd moves to :2222 and the gateway takes :22. This evicts the
listener on :22, so **keep a second shell open** the first time in case you need
to reconnect on :2222.

## 4. Connect

```sh
ssh -p 2222 new@<host>          # create a sandbox; it prints the name
ssh -p 2222 <name>@<host>       # resume + shell in (auto-resumes if paused)
ssh -p 2222 ctl@<host> help     # an index of every control; `help <topic>` for detail
```

Words after `new@` (or `new+<name>@`) are **tags**, not a command — a new
sandbox always gets a shell. Passing any word also stops ssh from allocating a
terminal, which leaves you in a shell with no prompt, so add `-t`:

```sh
ssh -t -p 2222 new+foo@<host> claude   # sandbox `foo` tagged `claude`
```
The same commands are also a REST API at `api.<domain>` and each sandbox also
has a shell in a browser tab at `<name>-xterm.<domain>` — both need the HTTPS
edge, so see *Day-2* below.

Web routes are live at `http://<name>.<proxy-domain>:8081` immediately. For a
public **HTTPS** edge, add a wildcard DNS record and turn on TLS (next section).

## 5. Day-2

- **HTTPS edge.** Point `*.yourdomain` at the host, then set `TLS_FLAGS` in
  `/srv/sparkbox/sparkbox.env` and restart:
  ```sh
  # on-demand Let's Encrypt (needs :443 + :80 reachable):
  TLS_FLAGS=--proxy-addr :443 --proxy-tls --tls-provider autocert --tls-email you@example.com
  ```
  For a wildcard cert via Cloudflare DNS-01 (no inbound :80/:443 needed), see
  [deploy-dns.md](deploy-dns.md). Restart with `systemctl restart sparkbox`.
- **Operator console.** Uncomment `SPARKBOX_CONSOLE_PASSWORD` in `sparkbox.env`
  (run it under TLS) to get `console.<domain>`.
- **REST API + browser terminals.** Both are on by default once the HTTPS edge
  is up: `https://api.<domain>/docs` documents every `ctl@` command as an HTTP
  call, and `https://<name>-xterm.<domain>` is a shell in a browser tab. The
  credential for both is a session token minted from your SSH key.
  ```sh
  # `session-token` speaks the ctl channel's CRLF, so strip the \r or the
  # Authorization header is malformed and curl gets a 400.
  TOKEN=$(ssh -p 2222 ctl@<host> session-token | tr -d '\r\n')
  AUTH="Authorization: Bearer $TOKEN"

  curl -sH "$AUTH" https://api.<domain>/v1/whoami
  curl -sH "$AUTH" https://api.<domain>/v1/sandboxes           # {"sandboxes":[…]}
  curl -sH "$AUTH" -H 'Content-Type: application/json' \
       -d '{"name":"demo","tags":["prod"]}' https://api.<domain>/v1/sandboxes
  curl -sH "$AUTH" -X POST https://api.<domain>/v1/sandboxes/demo/pause
  ```
  The create response carries `terminal_url` — open it in a browser and you have
  a shell. `/v1/capabilities` tells you what this host has configured (archiving,
  snapshots, schedules, terminals) so you can check before an endpoint answers
  `501`.

  The terminal host is **one label** — `<name>-xterm.<domain>`, hyphen, not
  `<name>.xterm.<domain>` — so the `*.<domain>` wildcard you already have in DNS
  and in the certificate covers it, and there is nothing further to publish.
  (A wildcard matches exactly one label, so the dotted form needed a second
  wildcard of each; Cloudflare's universal certificate will not issue one, which
  made browser terminals fail with `ERR_SSL_VERSION_OR_CIPHER_MISMATCH` for
  anyone off the tailnet.) The price is a reserved suffix: sandbox names and
  route subdomains may not end in `-xterm`, and the store refuses them. Move or
  disable either surface with `--api-subdomain` / `--xterm-subdomain`.
- **IPv6 per sandbox.** With a routed `/64`, set `SUBNET6` + `SUBNET6_FLAG` in
  `sparkbox.env`; see the README's IPv6 section.
- **Add users.** Operators mint invite codes (`ssh -p 2222 ctl@<host> invite`);
  newcomers register with `ssh signup@<host>`. For people who are on GitHub
  there is a shorter path that needs no code and nothing typed at their end —
  `ssh ctl@<host> user add <login>` adopts the keys github.com publishes for
  them, and `gh auth token | ssh ctl@<host> user sync-github-org <org>` does a
  whole organization. See [onboarding-users.md](onboarding-users.md).
- **Sign the agent CLIs in.** A fresh sandbox has `claude`, `codex`, `pi` and
  `gh` but no credentials. Store one per owner and it is pushed into their
  sandboxes' environment:
  ```sh
  gh auth token | ssh -p 2222 ctl@<host> secret set GITHUB_TOKEN
  claude setup-token | ssh -p 2222 ctl@<host> secret set CLAUDE_CODE_OAUTH_TOKEN
  ```
  The value is read from stdin, never from argv, so it stays out of shell
  history. A banner around the value is fine — `claude setup-token` prints one,
  and the credential is picked out of it by its own shape, with anything
  ambiguous refused rather than guessed at. `ssh -t … secret set <NAME>` prompts
  for a paste instead. Untagged secrets carry the `default`
  tag and so does every new sandbox — including one created with tags of its
  own — so they find each other without anyone learning what a tag is.
- **Tag templates.** A tag already picks a sandbox's secrets, repos and egress;
  bind a snapshot to it and the tag also picks the disk every sandbox carrying it
  boots from.
  ```sh
  ssh -p 2222 ctl@<host> snapshot create dev-box cuda-base
  ssh -p 2222 ctl@<host> snapshot bind cuda-base --tag cuda
  ssh -t new@<host> cuda        # boots from cuda-base, no name to remember
  ```
  `snapshot ls` shows which snapshots are bound; `snapshot unbind --tag cuda`
  takes it away without deleting anything. `default` cannot be bound — every
  sandbox you create carries it. A capture also carries the port the sandbox was
  serving on, so if `dev-box` was on 5173 then every box on `cuda` answers its
  own URL there with nothing to set up; `snapshot ls` prints that port on the
  rows that have one, and `sparkbox port <n>` from inside a sandbox moves it. Someone inside a sandbox can capture it into a
  tag it already carries with `sparkbox snapshot <tag>`, which prints what it
  will re-point and asks first; `--guest-self-snapshot=false` turns that door
  off. Capturing needs a host that can loop-mount the image, so a deployment run
  with `--disable-host-rootfs-mounts` refuses it. See
  [tag-templates-design.md](tag-templates-design.md).
- **Environments.** A tag names four things at once; an environment is the
  object that name deserves — a description, plain (non-secret) variables, and
  the setup script the project needs, with the tag as its name.
  ```sh
  ssh -p 2222 ctl@<host> env create web --repo wandb/hivemind --secret GITHUB_TOKEN
  ssh -p 2222 ctl@<host> env build web     # returns as soon as the build starts
  ssh -t new@<host> -- --env web           # boots from the disk that build made
  ```
  `env build` boots one sandbox called `web-build`, runs the setup script inside
  the checkout, and — when it succeeds — the gateway captures that box and binds
  the capture to the tag. The script is the one you piped in with `env script web
  --set`, or `.sparkbox/setup.sh` read out of an attached repository and then
  recorded, so the next build is the same build. It is asynchronous: read the
  outcome with `env show web` rather than by waiting. A build that fails leaves
  its builder **paused** with the half-built disk in it, so you can `ssh
  web-build@<host>`, fix what was missing, and keep exactly that disk with `env
  capture web`. `--env-build-timeout` (default 45m) bounds how long a build may
  sit unfinished before a sweep gives up on it — the builder is still left
  paused. See [environments-design.md](environments-design.md).
- **Agent CLI drift.** A template is frozen at the tool versions of the day it
  was captured, so `snapshot create` refreshes them first and a long-lived
  sandbox catches up on demand with `sparkbox update-tools` (`--check` to look
  without installing). It pulls from its own host's verified cache — `setup`
  points `--tools-dir` at `<root>/tools`, the same directory the refresher unit
  fills — so it works inside a sandbox whose egress is filtered by its tag, and
  nothing crosses a fleet link. It writes ~150 MB into that sandbox's own disk
  and against its owner's pool, so it is a command, not a timer.
- **Health + logs.** `sparkbox doctor` any time; `journalctl -u sparkbox -f`.
- **Re-provision / upgrade.** Drop in a newer `sparkbox` binary and re-run
  `sparkbox setup` — idempotent, and `--release <tag>` pins the artifacts. If
  you are rebuilding the *host* rather than upgrading it in place, read the next
  section first: the certificate cache is the one thing you cannot re-create on
  demand.

### Rebuilding a host: keep the certificate cache

Everything on a sparkbox host is reproducible from the release except its
certificates. Under `--tls-provider cloudflare` the edge holds one Let's Encrypt
wildcard covering `<domain>` and `*.<domain>`, and Let's Encrypt caps
**duplicate certificates at five per week** — the same names, over and over, is
exactly what a rebuild asks for. The wildcard is what keeps sandbox churn off
the rate limits; it is also what makes *host* churn hit them, because every
rebuild requests that identical pair.

Running out is not a graceful degradation. `sparkbox serve` obtains the
certificate **synchronously at startup** and exits if it cannot, so the sixth
rebuild in a rolling week takes down sandbox routes, the console, the REST API
and browser terminals together, until the window rolls forward — up to seven
days. There is no staging-CA flag to rehearse against today, so the cache is the
whole mitigation.

The cache is a plain directory: `<state-dir>/certmagic` (or
`<state-dir>/autocert` under `--tls-provider autocert`). It also holds the ACME
account key, so preserving it keeps the account too. Save it before you wipe
anything, and restore it before the gateway starts:

```sh
# BEFORE the rebuild — and keep a copy off the host
sudo systemctl stop sparkbox
sudo tar czf ~/sparkbox-certs-$(date +%Y%m%d).tgz \
  -C /srv/sparkbox/data/state certmagic

# ... reimage / reinstall / re-run `sparkbox setup` ...

# AFTER: restore into the state dir the unit actually uses, then start
sudo systemctl stop sparkbox
sudo tar xzf ~/sparkbox-certs-<date>.tgz -C /srv/sparkbox/data/state
sudo chown -R root:root /srv/sparkbox/data/state/certmagic
sudo chmod 700 /srv/sparkbox/data/state/certmagic
sudo systemctl start sparkbox
journalctl -u sparkbox -n 30 | grep 'tls certificates managed'
```

A restored cache is used as-is: the certificate is read off disk, found still
valid, and nothing is requested. The success line is
`tls certificates managed names="[<domain> *.<domain>]"` with no ACME order
logged above it — if you see an order, the cache did not land where the gateway
looks.

Two things to get right:

- **The state dir.** Restore into the path the *installed unit* names, not the
  one you remember: `systemctl cat sparkbox | grep -- --state-dir`. A host
  provisioned before the `data/` layout keeps its cache at
  `/srv/sparkbox/state/certmagic`. Restoring one directory over from where the
  gateway looks fails silently — it just issues a new certificate.
- **Permissions.** The gateway runs as root, so it can read the cache whatever
  you restore it as; the `chown`/`chmod` above are there because the directory
  holds the certificate's private key and the ACME account key, and an
  unprivileged `tar x` (or a copy through a shared directory) can leave both
  readable by everyone on the host.

`sparkbox setup` refuses outright to point the gateway at a different state
directory while a live one (cert cache included) sits elsewhere on the host. The
only way past it is `--adopt-legacy`, which means "use the directory that is
already there" — there is no flag that provisions onto the new path anyway; to
do that you migrate by hand, and the refusal prints the exact commands. That
guard only covers the case where `setup` itself moves the layout out from under
a live cache — reimaging the box, deleting the VM, or building a fresh instance
is entirely outside its reach, which is why the backup above is the procedure
and the guard is only a backstop.

## macOS (Apple Silicon)

A Mac cannot run Firecracker: there is no KVM on macOS. So `sparkbox setup`
provisions a **nested Linux machine** using Apple's
[`container`](https://github.com/apple/container) CLI and runs the ordinary
Linux `sparkbox setup` *inside* it. Everything from section 3 onwards — the
steps, the release, `sparkbox.env`, the systemd units, `ctl@` — is unchanged;
it just lives one layer down.

```sh
curl -fLO https://github.com/vanpelt/sparky/releases/latest/download/sparkbox-darwin-arm64
chmod +x sparkbox-darwin-arm64 && sudo mv sparkbox-darwin-arm64 /usr/local/bin/sparkbox

sparkbox doctor
sparkbox setup --dry-run --proxy-domain yourdomain.tools   # read the plan first
sparkbox setup --proxy-domain yourdomain.tools
```

### What you need

`sparkbox setup` checks all of these before it changes anything and refuses with
the reason, so `sparkbox setup --dry-run` is the safe way to ask "can this Mac
do it?" — a host that passes prints no preflight section at all, so silence
there means yes:

- **Apple Silicon M3 or newer.** Nested virtualization arrived with M3, and it
  is the entire reason this works — Firecracker needs `/dev/kvm` inside the
  machine. An M1 or M2 cannot do it, and the preflight says exactly that rather
  than failing later in a way that looks like a bug.
- **macOS 15 or newer.**
- **Apple's `container` CLI, 1.1.0 or newer** — `brew install container`, then
  `container system start`. Both the version and the service are checked.
- Free disk for the machine's data volume (`--data-volume-gb`).

Unlike Linux, you do not need `sudo` for `setup` itself: the machine's
provisioning runs as root *inside* the VM, not on your Mac. You only need it to
put the binary in `/usr/local/bin`.

### What `setup` does differently

Five extra steps run before the familiar Linux ones, and every one is
idempotent and visible in `--dry-run`:

1. **outer-kernel** — download and sha256-verify `vmlinux-macos-arm64`, the
   KVM-capable kernel Apple's `container machine` boots. (Not the same as
   `vmlinux-<arch>`, the *guest* kernel a sandbox boots one layer further in.
   `setup` fetches both.)
2. **machine-image** — build the gateway image. Its tag is content-addressed
   from the embedded build context, so editing the context produces a new tag
   and a rebuild instead of silently reusing a stale image.
3. **machine** — create the machine, or adopt an existing one after checking it
   is ours: the name, the image *repository* and `home-mount=none` must all
   match. (The repository rather than the full reference, because the image tag
   is content-addressed and legitimately changes when the build context does.) A
   machine you created for something else is never adopted.
4. **machine-sparkbox** — fetch the released `sparkbox-linux-arm64` *inside* the
   machine, verify it against the manifest's checksum, and refuse if the binary
   reports a version different from the tag it was published under.
5. **provision-inner** — run `sparkbox setup` in the machine with your flags
   forwarded.

### Flags

Every flag you would use on Linux means the same thing and is **forwarded** to
the gateway inside the machine: `--proxy-domain`, `--operator-key`,
`--operator-handle`, `--sluice`, `--gateway`/`--node-name`, the listen
addresses, the TLS flags, `--data-volume-gb`. Only flags the operator actually
typed are forwarded, so re-running `setup` after an upgrade cannot quietly
rewrite a live machine's config with compiled-in defaults.

These describe the Mac itself and are not forwarded:

| Flag | Default | Purpose |
|---|---|---|
| `--machine-name` | `sparkbox` | the nested VM to create or adopt |
| `--machine-cpus` | 8 | the machine's CPU budget (shared by every sandbox in it) |
| `--machine-memory-gb` | 24 | the machine's memory budget |
| `--machine-image` | content-addressed | override the gateway image reference |
| `--outer-kernel` | `~/Library/Application Support/sparkbox/vmlinux-macos-arm64` | where the outer kernel is stored |
| `--container-bin` | `container` | path to Apple's CLI |

Two flags are **refused** on macOS rather than silently ignored:

- `--move-admin-ssh` — the machine runs one process, the gateway, and nothing
  competes with it for `:22` inside the VM.
- `--bin-path` — the gateway's binary is installed by the inner `setup` from
  what the bootstrap staged. The Mac's own binary is not what runs the gateway.

### Verifying and day-2

`sparkbox doctor` on a Mac reports on **two hosts at once**, and every line says
which one it means:

```
sparkbox doctor — this Mac and machine "sparkbox" (guest paths are inside it, under /srv/sparkbox)
  [PASS] mac: macos version              26.5.2
  [PASS] mac: apple container            container 1.1.0
  [FAIL] mac: outer kernel               no outer kernel at ~/Library/…/vmlinux-macos-arm64
  [PASS] machine: state                  sparkbox running at 192.168.64.18 (8 cpu, 24 GiB)
  [PASS] machine: nested virtualization  enabled
  [PASS] machine: sparkbox service       active (running), stable across a 10s window
  [FAIL] machine: gateway doctor         the gateway's own doctor FAILED (exit 1); its report is below
                                         │ ── sparkbox doctor, inside machine sparkbox ──
                                         │   [FAIL] users.conf  no users.conf at /srv/sparkbox/users.conf
```

- `mac:` lines are about this laptop — macOS version, Apple Silicon generation,
  the `container` CLI and its service, free disk, and the outer kernel the
  hypervisor boots. The kernel's checksum is only *asserted* when you pass a
  concrete `sparkbox doctor --release <tag>`; without one there is no release to
  be right or wrong about, so a difference from the newest published kernel is
  reported as a WARN naming the local hash — a Mac deliberately a release behind
  is not a broken Mac, and the kernel is rebuilt on every tag.
- `machine:` lines are about the nested Linux machine: does it exist, is it
  ours, is it running, is nested virtualization really on, is the image current,
  `/dev/kvm`, `/dev/net/tun`, no `/Users` mount, the gateway service's liveness —
  and finally the gateway's **own `doctor`, relayed out of the machine** in its
  own words.

It exits non-zero when either layer is genuinely wrong. When the machine is
missing, stopped or unreachable, the machine section is one honest line saying
which of those it is — never an empty section that reads as health. `setup` ends
by running the same machine-layer battery and exits non-zero if the gateway is
not alive.

`doctor` takes the same macOS-only flags `setup` does — `--machine-name`,
`--machine-image`, `--outer-kernel`, `--container-bin` — so a gateway you
provisioned under a non-default machine name is diagnosed with
`sparkbox doctor --machine-name <name>`.

Everything in [Day-2](#5-day-2) applies unchanged — TLS, the console, the REST
API, adding users — because it is all happening in a normal Linux gateway.
Re-running `sparkbox setup` upgrades it, exactly as on Linux.

### Honest limits

- **No CI creates a real machine.** GitHub's hosted macOS runners have no
  `container` CLI and cannot nest VMs. CI builds and unit-tests the darwin code
  on real Apple Silicon, but the machine-lifecycle tests drive an in-memory fake
  of the CLI. Your laptop is where this is proven for the first time.
- **`macos/poc.sh` is still in the tree**, as the fallback and as the reference
  the Go port was written from. It uses a different machine (`sparkbox-poc`), so
  it coexists with `setup`'s `sparkbox`. Its `smoke` and `destroy` subcommands
  have no equivalent in the binary yet: nothing in `sparkbox` boots a test
  sandbox end-to-end, and nothing deletes a machine, an image or the outer
  kernel.
- **Sizing is a single budget.** `--machine-cpus` / `--machine-memory-gb` size
  the whole machine, and every sandbox in it draws from that, not from your
  Mac's totals.

## Local dev (no KVM)

To develop against the full control plane with no VM isolation, use the **mock
driver** — it emulates each sandbox as an in-process SSH server, so it runs on a
laptop:

```sh
go build ./cmd/sparkbox
ssh-keygen -t ed25519 -f mykey -N ''
echo "me $(cat mykey.pub)" > users.conf
./sparkbox serve --driver mock --state-dir ./state --users users.conf
# elsewhere:
ssh -i mykey -p 2222 new@localhost
```

`go test ./...` runs the end-to-end suite over exactly this stack.
