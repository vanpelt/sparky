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
  --proxy-addr ${SPARKBOX_PROXY_ADDR} --proxy-domain ${SPARKBOX_DOMAIN}
Restart=always
RestartSec=2
LimitNOFILE=1048576
[Install]
WantedBy=multi-user.target
EOF
sudo systemctl enable --now sparkbox.service
```

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
sudo cloudflared tunnel route dns sparkbox '*.catnip.sh'   # wildcard CNAME -> tunnel
sudo cloudflared service install                            # enabled systemd service
```

`cloudflared` preserves the original `Host` header, so sparkbox resolves the
subdomain and routes to the right sandbox. Wildcard `*.catnip.sh` means every
sandbox is reachable with no per-name DNS.

Note: with the tunnel, sparkbox does **no DNS updates** — the one wildcard CNAME
covers every sandbox. Do NOT put a `CLOUDFLARE_API_TOKEN` in sparkbox's env: the
only thing that reads it is the front-door DNS publisher, which is gated behind
`--subnet6` (unset here) and would write per-name `AAAA` records that *shadow*
the wildcard CNAME and break routing.

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

## 6. Use it

```sh
ssh -p 2222 new@<host>                          # create a sandbox (host = tailnet name or IP)
ssh -p 2222 <name>@<host>                        # shell in (arm64, docker, /dev/net/tun)
ssh -p 2222 ctl@<host> share <name> public       # expose its URL (routes are private by default)
curl https://<name>.catnip.sh                    # served through the tunnel
```

## Gotchas

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
