# Deploying sparkbox on an arm64 box at home (DGX Spark) behind a Cloudflare Tunnel

This is the recipe for running sparkbox on an **aarch64** KVM host that sits
behind home NAT with no public IP and no cloud secret store — proven on an
NVIDIA DGX Spark (GB10, 20-core arm64, 119 GB RAM). Two things differ from the
bare-metal x86 path (`deploy-hetzner.md` / `deploy-scaleway.md`):

1. **arm64 artifacts.** Everything the guest boots is architecture-specific.
   Build it natively on the box — the DGX is arm64, so no cross-compile.
2. **The public edge is a Cloudflare Tunnel**, not a direct `:443` with a
   wildcard TLS cert. The tunnel is outbound-only, so it sidesteps NAT/CGNAT and
   a dynamic residential IP entirely, and TLS terminates at Cloudflare's edge.
   No Scaleway Secret Manager: on a box you physically own, the fleet keys are
   just root-owned files.

Everything below is non-secret except the three fleet key PEMs and the tunnel
credentials, which stay root-owned under `/srv/sparkbox/state` and
`/root/.cloudflared`.

## 0. Prereqs

```sh
ls -l /dev/kvm                 # must exist (KVM enabled in the kernel)
uname -m                       # aarch64
docker version                 # arm64 engine, no sudo (you're in the docker group)
sudo -v                        # you need root: taps, iptables, /dev/kvm
```

## 1. Build the arm64 artifacts (native)

```sh
cd tools/sparkbox

# Guest kernel: build-kernel.sh is arch-aware (defaults ARCH to `uname -m`); on
# arm64 it pulls the aarch64 firecracker CI config and emits a flat Image.
KVER=6.1.155 ./hack/build-kernel.sh                 # -> ./vmlinux (arm64 Image)

# Firecracker: grab the aarch64 static binary.
REL=$(curl -s https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest | grep -o '"tag_name": "[^"]*' | cut -d'"' -f4)
curl -L "https://github.com/firecracker-microvm/firecracker/releases/download/${REL}/firecracker-${REL}-aarch64.tgz" | tar xz
sudo install release-*/firecracker-*-aarch64 /usr/local/bin/firecracker

# Base image: the Dockerfile is already arch-portable (Go/Node/uv/tailscale/
# docker all resolve per-arch; bases are multi-arch). Build it natively.
docker build --platform linux/arm64 -t sparkbox-base:arm64 ./images

# sparkbox binary: pure-Go (modernc sqlite, no cgo) — build on the box, or
# cross-compile from a laptop with CGO_ENABLED=0 GOOS=linux GOARCH=arm64.
go build -o /usr/local/bin/sparkbox ./cmd/sparkbox   # box has Go; else cross-compile
```

## 2. Lay out /srv/sparkbox + fleet keys + your login key

```sh
sudo mkdir -p /srv/sparkbox/{state,images}
sudo cp ./vmlinux /srv/sparkbox/vmlinux

# users.conf: one "<user> <ssh-authorized-keys-line>" per line. Reuse the
# laptop key(s) you already SSH into the box with so you can drive it directly.
awk 'NF && $1 !~ /^#/ {print "van", $0}' ~/.ssh/authorized_keys | sudo tee /srv/sparkbox/users.conf

# Generate the three fleet keys: a 3s mock run writes them, then derive the
# gateway public half the rootfs must trust.
timeout 3 sudo sparkbox serve --driver mock --state-dir /srv/sparkbox/state \
  --users /srv/sparkbox/users.conf --ssh-addr 127.0.0.1:0 --api-addr 127.0.0.1:0 || true
sudo ssh-keygen -y -f /srv/sparkbox/state/gateway_upstream_key.pem \
  | sudo tee /srv/sparkbox/gateway_upstream_key.pub

# Rootfs template (bakes in that gateway key). 8 GB is plenty for a slim host.
sudo ./hack/build-rootfs.sh sparkbox-base:arm64 \
  /srv/sparkbox/images/universal.ext4 /srv/sparkbox/gateway_upstream_key.pub 8192
```

## 3. Host networking (tunnel + docker aware)

The host runs docker, so the `FORWARD` policy is `DROP` — sandbox egress needs
explicit `sbtap+` ACCEPT rules. And because the public edge is a tunnel (web
traffic arrives from localhost, not the uplink), the any-port uplink REDIRECT is
**off** — on a shared/home box it would otherwise hijack all inbound uplink TCP
and clobber unrelated LAN services. `sparkbox-net.sh` handles both:
`SPARKBOX_EDGE_REDIRECT=0` skips the REDIRECT, and the `sbtap+` accepts are
unconditional (harmless on a non-docker host).

```sh
sudo tee /etc/sysctl.d/99-sparkbox.conf >/dev/null <<'EOF'
net.ipv4.ip_forward=1
net.ipv4.conf.all.rp_filter=1
net.ipv4.conf.default.rp_filter=1
EOF
sudo sysctl --system

sudo install -m0755 ./deploy/sparkbox-net.sh /usr/local/sbin/sparkbox-net.sh
sudo mkdir -p /etc/sparkbox
echo 'SPARKBOX_EDGE_REDIRECT=0' | sudo tee /etc/sparkbox/net.env
# unit ordered After=docker.service so our sbtap+ accepts land above docker's chains
sudo tee /etc/systemd/system/sparkbox-net.service >/dev/null <<'EOF'
[Unit]
Description=sparkbox host packet-filter rules (tunnel mode)
After=network-online.target docker.service
Wants=network-online.target
Before=sparkbox.service
[Service]
Type=oneshot
RemainAfterExit=yes
EnvironmentFile=/etc/sparkbox/net.env
ExecStart=/usr/local/sbin/sparkbox-net.sh
[Install]
WantedBy=multi-user.target
EOF
sudo systemctl enable --now sparkbox-net.service
```

## 4. sparkbox.service

Note the ports: the fleet defaults (`:8080` API, `:8081` proxy) may collide with
other things on a shared box — this box uses `127.0.0.1:8079` / `127.0.0.1:8091`.
The proxy binds to localhost because only `cloudflared` needs to reach it. The
SSH gateway is `:2222` (host sshd owns `:22`).

Note the **overcommit flags** (`--mem-reserve-mb 1024 --max-running-per-owner
50`): these are the fleet density defaults that `deploy/launch-host.sh` bakes
into the cloud path, and they belong here too. Without them the per-owner
running cap falls back to its conservative default of **2** — and RAM admission
charges each VM its full ceiling (~8 GB) rather than the 1 GB reserve floor, so
you'd cap out at ~12 concurrent long before 50. A sandbox's RAM is lazily
allocated (an idle 8 GB VM costs ~0.5 GB measured), which is what makes the
reserve floor safe; retune it from `hack/measure-density.py` or the console's
live-usage readout.

```sh
sudo tee /etc/sparkbox/sparkbox.env >/dev/null <<'EOF'
SPARKBOX_STATE_DIR=/srv/sparkbox/state
SPARKBOX_KERNEL=/srv/sparkbox/vmlinux
SPARKBOX_IMAGE_DIR=/srv/sparkbox/images
SPARKBOX_USERS=/srv/sparkbox/users.conf
SPARKBOX_DOMAIN=catnip.sh
SPARKBOX_SSH_ADDR=:2222
SPARKBOX_API_ADDR=127.0.0.1:8079
SPARKBOX_PROXY_ADDR=127.0.0.1:8091
EOF
sudo tee /etc/systemd/system/sparkbox.service >/dev/null <<'EOF'
[Unit]
Description=sparkbox agentic microVM control plane
After=network-online.target sparkbox-net.service docker.service
Wants=network-online.target
Requires=sparkbox-net.service
[Service]
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
EnvironmentFile=/etc/sparkbox/sparkbox.env
ExecStart=/usr/local/bin/sparkbox serve --driver firecracker \
  --state-dir ${SPARKBOX_STATE_DIR} --kernel ${SPARKBOX_KERNEL} \
  --image-dir ${SPARKBOX_IMAGE_DIR} --default-image universal --default-login-user sparky \
  --users ${SPARKBOX_USERS} \
  --ssh-addr ${SPARKBOX_SSH_ADDR} --api-addr ${SPARKBOX_API_ADDR} \
  --proxy-addr ${SPARKBOX_PROXY_ADDR} --proxy-domain ${SPARKBOX_DOMAIN} \
  --mem-reserve-mb 1024 --max-running-per-owner 50
Restart=always
RestartSec=2
LimitNOFILE=1048576
[Install]
WantedBy=multi-user.target
EOF
sudo systemctl enable --now sparkbox.service
```

## 4b. Agent CLIs in sandboxes (claude, codex, hivemind)

The rootfs template does **not** ship the agent CLIs — they move too fast to bake
into an image rebuild. The cloud path (`deploy/cloud-init.yaml`) patches them
into the template in seconds and re-patches daily; on a hand-built box, run the
same tooling installer once. It drops `sparkbox-refresh-tools.sh` +
`sparkbox-install-guest-identity.sh` into `/usr/local/sbin`, installs a daily
timer, and does one immediate `--force` patch of the current template. Point
`IMAGES_DIR` at wherever `--image-dir` reads from (this box uses
`/srv/sparkbox/images`, not the cloud path's `data/images`).

```sh
cd tools/sparkbox
sudo IMAGES_DIR=/srv/sparkbox/images TOOLS_DIR=/srv/sparkbox/tools \
  ./deploy/install-host-tooling.sh
```

The patch is atomic (reflink/copy → loop-mount → install → rename), so it's safe
to run on a live box: in-flight `ssh new@` sees either the old or new template,
never a torn one; running sandboxes keep their own disk. Verify:
`ssh -p2222 new@<gateway>` then `ssh -p2222 <name>@<gateway> 'claude --version;
codex --version; hivemind --version'`.

## 5. Cloudflare Tunnel

```sh
sudo cloudflared tunnel login                 # authorize the zone (writes cert.pem)
sudo cloudflared tunnel create sparkbox       # writes /root/.cloudflared/<uuid>.json
TUNNEL=<uuid-from-create>

sudo tee /etc/cloudflared/config.yml >/dev/null <<EOF
tunnel: $TUNNEL
credentials-file: /root/.cloudflared/$TUNNEL.json
ingress:
  - hostname: "*.catnip.sh"
    service: http://127.0.0.1:8091
  - service: http_status:404
EOF

sudo cloudflared tunnel ingress validate
sudo cloudflared tunnel route dns sparkbox '*.catnip.sh'    # wildcard CNAME -> tunnel
sudo cloudflared service install                            # enabled systemd service
```

One wildcard covers everything, browser terminals included: they are served at
`<name>-xterm.catnip.sh`, a single label. This is why the separator is a hyphen.
A wildcard matches exactly one label, so `*.catnip.sh` covers neither
`demo.xterm.catnip.sh` the DNS record nor — and this is the one that cannot be
fixed by adding a record — Cloudflare's edge certificate, which is issued as
`catnip.sh, *.catnip.sh` and stops there. A two-label terminal host therefore
died inside the TLS handshake at Cloudflare with
`ERR_SSL_VERSION_OR_CIPHER_MISMATCH`, before the tunnel, before the origin, and
with no sparkbox log line anywhere — so it read like a DNS bug for hours and was
reachable only over the tailnet. Multi-label edge certificates need Cloudflare's
paid Advanced Certificate Manager; one hyphen needs nothing.

`cloudflared` preserves the original `Host` header, so sparkbox resolves the
subdomain and routes to the right sandbox. Wildcard `*.catnip.sh` means every
sandbox is reachable with no per-name DNS.

Note: with the tunnel, sparkbox does **no DNS updates** — the two wildcard CNAMEs
cover every sandbox. Do NOT put a `CLOUDFLARE_API_TOKEN` in sparkbox's env: the
things that read it are the front-door DNS publisher, which is gated behind
`--subnet6` (unset here) and would write per-name `AAAA` records that *shadow*
the wildcard CNAME and break routing, and the browser-terminal wildcard
publisher, which wants an `A`/`AAAA` at an edge address the tunnel does not
have. With no token both stand down and say so; the terminal one logs the exact
`cloudflared tunnel route dns` command above. (Defensively, it also refuses to
overwrite an existing CNAME, so a token added later cannot take the tunnel
offline — but withholding the token is still the rule here.)

## 5b. Sandbox archiving to Cloudflare R2 (optional)

Archiving parks an idle sandbox's rootfs in object storage (`e2fsck` + `zerofree`
+ `zstd`, then upload) and frees host disk; a restore (or just reconnecting)
brings it back. It talks to any S3 endpoint via rclone — here, Cloudflare R2.

```sh
# 1. Current rclone (apt's ~1.60 501s on R2 uploads — see gotchas).
sudo ./deploy/install-rclone.sh

# 2. R2 bucket + an R2 API token with **Object Read & Write** on that bucket.
#    The token page shows an S3 Access Key ID + Secret Access Key — THOSE are what
#    rclone uses (not the `cfat…` Cloudflare API *token value*, which is REST-only).
sudo install -d -m700 /root/.config/rclone
sudo tee /root/.config/rclone/rclone.conf >/dev/null <<EOF
[r2]
type = s3
provider = Cloudflare
access_key_id = <R2 Access Key ID>
secret_access_key = <R2 Secret Access Key>
endpoint = https://<account-id>.r2.cloudflarestorage.com
region = auto
no_check_bucket = true
EOF
# NOTE: do NOT set `acl = private`. R2 rejects PutObject carrying an ACL header
# unless the token has *admin* (not just Read & Write) — you'll get 403 on writes
# while reads succeed. Omitting acl is the fix.

# 3. Point sparkbox at it (add to /etc/sparkbox/sparkbox.env + the ExecStart flags):
#    --archive-remote ${SPARKBOX_ARCHIVE_REMOTE} --archive-bucket ${SPARKBOX_ARCHIVE_BUCKET}
#    SPARKBOX_ARCHIVE_REMOTE=r2
#    SPARKBOX_ARCHIVE_BUCKET=<bucket>
sudo systemctl restart sparkbox        # log should read: "sandbox archiving enabled"
```

Then: `ssh -p 2222 ctl@<host> archive <name>` parks it; `restore <name>` (or just
reconnecting / hitting its URL) brings it back.

## 5c. REST API + browser terminals

Both surfaces come up **on by default** with the proxy edge — there is nothing
to enable in `sparkbox.env`. What they need is a name that resolves and a
certificate that covers it.

| Flag | Default | What it does | Turn it off with |
|---|---|---|---|
| `--api-subdomain` | `api` | `https://api.catnip.sh` — every `ctl@` command as an HTTP call, plus `/docs`, `/openapi.json`, `/openapi.yaml` | `--api-subdomain ""` |
| `--xterm-subdomain` | `xterm` | `https://<name>-xterm.catnip.sh` — an xterm.js shell in a browser tab | `--xterm-subdomain ""` |

`api.catnip.sh` is a single label, so the wildcard you already have covers it in
both DNS and TLS: nothing to do. `<name>-xterm.catnip.sh` is also a single
label, for exactly that reason — see §5. Neither surface costs a record or a
certificate name of its own.

The one thing it does cost is a **reserved name suffix**. The edge dispatches
every subdomain ending in `-xterm` to the terminal handler before it consults a
route, so a sandbox or route actually named `web-xterm` would be answered by the
terminal instead of by itself. New ones are refused; anything already on disk
gets a startup `WARN` naming it.

### What must exist, and what breaks without it

| Missing | Symptom | Fix |
|---|---|---|
| `*.catnip.sh` DNS / cert | everything is broken, not just terminals | §5 |
| tunnel ingress rule | Cloudflare `404`s before the origin is dialled | the `hostname:` block in §5 |
| nothing (feature off) | `<name>-xterm.catnip.sh` 404s cleanly, `terminal_url` is absent from API responses, `/v1/capabilities` reports `"terminal":false` | — |

Startup tells you which of these you are in. Grep the journal for:

```
msg="browser terminals enabled" url=https://<name>-xterm.catnip.sh
msg="rest api enabled" url=https://api.catnip.sh docs=https://api.catnip.sh/docs
msg="tls certificates managed" names="[catnip.sh *.catnip.sh]"
```

### This box specifically (tunnel + tailnet edge)

The live box has moved on from §5's plain-tunnel baseline: it serves the tailnet
path from sparkbox's own `:443` on a dedicated tailnet IP, with cloudflared
repointed at that as an **https origin** ([`tailnet-edge-design.md`](tailnet-edge-design.md),
[`dedicated-edge-ip-cutover.md`](dedicated-edge-ip-cutover.md)). It therefore
**does** carry a `CLOUDFLARE_API_TOKEN`, for the DNS-01 wildcard certificate —
safe because `--subnet6` stays unset. That shape is why the terminal hostname
matters twice over: cloudflared validates the origin certificate with the
original hostname as SNI, so a name outside the wildcard fails at the origin as
well as at Cloudflare's edge. With `<name>-xterm.catnip.sh` both are the same
one certificate. So:

- **Nothing to do for terminals, on either path.** The wildcard `*.catnip.sh` —
  the DNS record, Cloudflare's edge certificate, and sparkbox's own DNS-01
  certificate — already covers them. Confirm from a peer:
  `openssl s_client -connect 10.66.0.1:443 -servername demo-xterm.catnip.sh </dev/null 2>/dev/null | openssl x509 -noout -text | grep DNS:`
- **This is what the two-label form cost.** `demo.xterm.catnip.sh` worked over
  the tailnet (split-DNS sends the whole zone to the edge, and `dnsedge` answers
  the entire subtree) and died on the public path at Cloudflare's universal
  certificate. Anyone off the tailnet — or on it but using an exit node, which
  routes DNS through the exit node and quietly bypasses the split-DNS route —
  got `ERR_SSL_VERSION_OR_CIPHER_MISMATCH` and nothing in any log.
- **Sanity check when a terminal misbehaves:** compare
  `dig +short <name>-xterm.catnip.sh` against `dig +short @10.66.0.1
  <name>-xterm.catnip.sh`. Cloudflare anycast IPs from the first mean you are on
  the public path (which now works); `10.66.0.1` means the tailnet edge.

Verify the whole thing end to end from a laptop:

```sh
TOKEN=$(ssh -p 2222 ctl@<host> session-token | tr -d '\r\n')
curl -sH "Authorization: Bearer $TOKEN" https://api.catnip.sh/v1/capabilities
# → …"terminal":true}

curl -sH "Authorization: Bearer $TOKEN" https://api.catnip.sh/v1/sandboxes \
  | python3 -c 'import json,sys; [print(s["terminal_url"]) for s in json.load(sys.stdin)["sandboxes"]]'
# open one of those URLs; you should get a login redirect, then a shell
```

A `terminal_url` in that output means sparkbox *believes* terminals are on; it
is derived from the flag, not from the certificate, so it is not evidence that
DNS and TLS are in place. The two `curl`s above plus actually opening the page
are.

## 6. Use it

```sh
ssh -p 2222 new@<host>                          # create a sandbox (host = tailnet name or IP)
ssh -p 2222 <name>@<host>                        # shell in (arm64, docker, /dev/net/tun)
ssh -p 2222 ctl@<host> share <name> public       # expose its URL (routes are private by default)
curl https://<name>.catnip.sh                    # served through the tunnel

open https://<name>-xterm.catnip.sh              # a shell in a browser tab
TOKEN=$(ssh -p 2222 ctl@<host> session-token | tr -d '\r\n')
curl -H "Authorization: Bearer $TOKEN" https://api.catnip.sh/v1/sandboxes
```

## 7. Rebuilding this box (keep `/srv/sparkbox/state`)

Everything above is reproducible from pinned inputs. `/srv/sparkbox/state` is
not, and on this box it is where three unrecoverable things live. Save it before
any rebuild, reimage or disk swap:

```sh
sudo systemctl stop sparkbox
sudo tar czf ~/sparkbox-state-$(date +%Y%m%d).tgz -C /srv/sparkbox state
# keep a copy OFF the box
```

Note the path: this box predates `sparkbox setup`'s `data/` layout, so its state
dir is the flat `/srv/sparkbox/state` (`SPARKBOX_STATE_DIR` in §4), not
`/srv/sparkbox/data/state`. Restore into whichever one the running unit names.

What is in there, in order of how badly you want it back:

1. **`state/certmagic/`** — the Let's Encrypt wildcard for `catnip.sh` +
   `*.catnip.sh` and the ACME account key. This box issues it over DNS-01
   (§5c), and Let's Encrypt allows **five duplicate certificates per week**. The
   wildcard is what keeps sandbox churn off the rate limits; it is also what
   makes *box* churn hit them, because every rebuild asks for the same two
   names. And `serve` obtains the certificate synchronously at startup and exits
   if it cannot, so the sixth rebuild in a rolling week does not degrade — it
   takes the whole edge down (sandbox routes, `api.`, `console.`, terminals,
   and the tunnel origin behind them) for up to seven days.
2. **`state/gateway_upstream_key.pem`** — the key baked into every rootfs
   template in §2. Lose it and the freshly generated one is not the one the
   templates trust, so `ssh <name>@` into every existing and every newly cloned
   sandbox fails until you re-derive the `.pub` and rebuild the templates.
3. **`state/gateway_host_key.pem`**, **`state/oidc_signing_key.pem`** and
   **`state/sparkbox.db`** — the identity fleet nodes pinned on first contact,
   the key that signs guest id tokens *and* derives both the session-token MAC
   and the KEK that user secrets are sealed under, and the database holding
   users, keyrings, routes, tags, secrets, schedules and the node roster. A new
   host key makes every enrolled node refuse the gateway it already trusted; a
   new OIDC key invalidates every outstanding session token and makes the sealed
   secrets in the DB undecryptable even though the rows survive.

Restore the archive, put ownership back (root reads it regardless — the point is
that the directory holds five private keys and an unprivileged `tar x` can leave
them world-readable), and start:

```sh
sudo systemctl stop sparkbox
sudo tar xzf ~/sparkbox-state-<date>.tgz -C /srv/sparkbox
sudo chown -R root:root /srv/sparkbox/state
sudo chmod 700 /srv/sparkbox/state
sudo systemctl start sparkbox
journalctl -u sparkbox -n 30 | grep 'tls certificates managed'
```

`tls certificates managed names="[catnip.sh *.catnip.sh]"` with no ACME order
logged above it means the cache was found and nothing was requested. An order
means the restore did not land where the gateway looks.

## Gotchas

- **Rebuilding the box costs a certificate if you do not carry
  `state/certmagic/` across.** Five duplicates per week, and the edge does not
  come up without one — see §7.
- **Guest apps must be *enabled*, not just started, to survive a reboot.** A
  host reboot cold-boots each sandbox from its persisted disk (the in-memory
  snapshot is gone), so a manually-`start`ed server won't come back. `systemctl
  enable --now` it (disk change persists) — or run it under the login user's
  lingering systemd. The image ships nginx/docker's-own-services disabled by
  design; docker is enabled, nginx is not.
- **Routes are private by default** (authenticated forwarding). Until `ctl@ share
  <name> public`, the edge returns `401`.
- **Ports**: default `:8080`/`:8081` collide with common services; this box uses
  `:8079`/`:8091`. Keep the proxy on `127.0.0.1` so only the tunnel reaches it.
- **No per-sandbox IPv6 through the tunnel.** cloudflared is an L7 proxy; clients
  always hit Cloudflare's anycast IPs and the origin's addresses are never
  surfaced. For per-sandbox v6, give each sandbox a ULA `/128` and run the box as
  a Tailscale subnet router advertising that range — every tailnet device then
  reaches each sandbox by its own dedicated v6. Public per-sandbox v6 would need
  the ISP to delegate a routed prefix (DHCPv6-PD) + a router pinhole + proxy-NDP,
  which a typical home connection doesn't provide.
