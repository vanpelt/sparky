# Getting started: stand up a sparkbox host

This is the operator quickstart for running your own sparkbox on **any Linux
host with KVM** — a bare-metal box, a nested-virt cloud instance, a DGX, a
homelab server. The `sparkbox` binary is its own installer: point it at a host
and it fetches a prebuilt release, lays down the storage and networking, wires
systemd, and starts the gateway.

```
sparkbox doctor        # is this host ready?
sparkbox setup         # provision it into a running gateway
```

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
ssh -p 2222 ctl@<host> help     # account/keys/routes/schedule controls
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
  newcomers register with `ssh signup@<host>`.
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
