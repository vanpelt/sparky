#!/usr/bin/env bash
# One-shot setup for a fresh Ubuntu 24.04 bare-metal host (e.g. Scaleway
# Elastic Metal, Hetzner) to run sparkbox with the firecracker driver.
#
# ─────────────────────────────────────────────────────────────────────────────
# NOTE: For most hosts, prefer the built-in provisioner — it needs no repo
# checkout, downloads a PREBUILT release (no ~65-min rootfs/kernel build), is
# idempotent, installs + starts the systemd service, and self-verifies:
#
#     sparkbox doctor                       # preflight
#     sudo sparkbox setup --proxy-domain …  # provision + start
#
# See docs/getting-started.md. Keep using THIS script when you specifically need
# to BUILD the kernel + rootfs from source on the host (custom image, air-gapped
# bucket, or no published release) — that is what it still does below.
# ─────────────────────────────────────────────────────────────────────────────
#
# Does: sanity checks, firecracker install, guest kernel download, XFS
# work volume (reflink CoW), Go + sparkbox build, gateway keys, rootfs
# template (codex-universal by default), NAT, and prints how to start the
# server.
set -euo pipefail

SPARKBOX_DIR=${SPARKBOX_DIR:-/srv/sparkbox}
SPARKBOX_USER_KEY=${1:?usage: setup-host.sh '<user> <ssh-ed25519 AAAA... comment>' — the users.conf line for your laptop key}
REPO_URL=${REPO_URL:-https://github.com/vanpelt/sparky}
# Rootfs template: codex-universal = ubuntu:24.04 + toolchains for ~10
# languages (~11GB pull / ~30GB unpacked — the thing coding agents expect).
# Override IMAGE=ubuntu:24.04 ROOTFS_NAME=ubuntu ROOTFS_MB=4096 for a quick
# slim host; ROOTFS_NAME must then match the server's --default-image flag.
IMAGE=${IMAGE:-ghcr.io/openai/codex-universal:latest}
ROOTFS_NAME=${ROOTFS_NAME:-universal}
# 25 GiB per-sandbox ceiling (exe.dev parity). The ext4 IS this size, so the
# guest can't exceed it; thin CoW reflink copies mean sandboxes only pay for
# blocks they write. The 300 GB host XFS volume below is the shared pool.
ROOTFS_MB=${ROOTFS_MB:-25600}

echo "== sanity checks =="
[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }
[ -e /dev/kvm ] || { echo "/dev/kvm missing — not a KVM-capable host?"; exit 1; }
grep -qE 'vmx|svm' /proc/cpuinfo || { echo "no VT-x/AMD-V in cpuinfo"; exit 1; }

echo "== packages =="
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  golang-go docker.io curl iptables xfsprogs git unzip \
  e2fsprogs zerofree zstd   # archive/snapshot: fsck + zero free space + compress (rclone below)

echo "== rclone (current — apt's is too old for Cloudflare R2) =="
# The distro rclone (~1.60) 501s on R2 uploads; install a current static binary.
"$(dirname "$0")/../deploy/install-rclone.sh"

echo "== firecracker =="
if ! command -v firecracker >/dev/null; then
  ARCH=$(uname -m)
  REL=$(curl -fsSL https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest \
    | grep -o '"tag_name": "[^"]*' | cut -d'"' -f4)
  cd "$(mktemp -d)"
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${REL}/firecracker-${REL}-${ARCH}.tgz" | tar xz
  install "release-${REL}-${ARCH}/firecracker-${REL}-${ARCH}" /usr/local/bin/firecracker
  echo "installed $(firecracker --version | head -1)"
fi

echo "== guest kernel (sparkbox build: FC CI 6.1 config + docker/TUN deltas) =="
mkdir -p "$SPARKBOX_DIR"
if [ ! -f "$SPARKBOX_DIR/vmlinux" ]; then
  if [ -n "${KERNEL_URL:-}" ]; then
    # Pin an exact prebuilt kernel (e.g. reuse one this script built earlier).
    echo "fetching pinned $KERNEL_URL"
    curl -fsSL "$KERNEL_URL" -o "$SPARKBOX_DIR/vmlinux"
  else
    # Build our own vmlinux. The stock firecracker-ci kernel omits IP_NF_RAW /
    # NF_TABLES (docker container networking) and TUN (tailscale), so a sandbox
    # can't run either on it. build-kernel.sh = FC CI 6.1 config + our fragment;
    # keep KVER in lockstep with .github/workflows/build-artifacts.yml.
    OUT="$SPARKBOX_DIR/vmlinux" KVER="${KVER:-6.1.155}" "$(dirname "$0")/build-kernel.sh"
  fi
fi

echo "== XFS work volume (reflink CoW for instant rootfs copies) =="
if ! mountpoint -q "$SPARKBOX_DIR/data"; then
  mkdir -p "$SPARKBOX_DIR/data"
  if [ ! -f "$SPARKBOX_DIR/data.img" ]; then
    # Sparse; grows on use. The universal template alone is ~30GB of real
    # blocks, plus per-sandbox write deltas + memory snapshot files.
    truncate -s 300G "$SPARKBOX_DIR/data.img"
    mkfs.xfs -q -m reflink=1 "$SPARKBOX_DIR/data.img"
  fi
  mount -o loop "$SPARKBOX_DIR/data.img" "$SPARKBOX_DIR/data"
  grep -q "$SPARKBOX_DIR/data.img" /etc/fstab || \
    echo "$SPARKBOX_DIR/data.img $SPARKBOX_DIR/data xfs loop 0 0" >> /etc/fstab
fi
mkdir -p "$SPARKBOX_DIR/data/state" "$SPARKBOX_DIR/data/images"

echo "== build sparkbox =="
if [ ! -d "$SPARKBOX_DIR/repo" ]; then
  git clone --depth 1 "$REPO_URL" "$SPARKBOX_DIR/repo"
fi
cd "$SPARKBOX_DIR/repo/tools/sparkbox"
go build -o /usr/local/bin/sparkbox ./cmd/sparkbox

echo "== users + gateway keys =="
echo "$SPARKBOX_USER_KEY" > "$SPARKBOX_DIR/users.conf"
# First run generates the gateway host + upstream keys, then we derive the
# public half the rootfs needs. Timeout kills it once keys exist.
timeout 3 sparkbox serve --driver mock --state-dir "$SPARKBOX_DIR/data/state" \
  --users "$SPARKBOX_DIR/users.conf" --ssh-addr 127.0.0.1:0 --api-addr 127.0.0.1:0 || true
[ -f "$SPARKBOX_DIR/data/state/gateway_upstream_key.pem" ] || { echo "key generation failed"; exit 1; }
ssh-keygen -y -f "$SPARKBOX_DIR/data/state/gateway_upstream_key.pem" > "$SPARKBOX_DIR/gateway_upstream_key.pub"

echo "== $ROOTFS_NAME rootfs template ($IMAGE) =="
if [ ! -f "$SPARKBOX_DIR/data/images/$ROOTFS_NAME.ext4" ]; then
  ./hack/build-rootfs.sh "$IMAGE" "$SPARKBOX_DIR/data/images/$ROOTFS_NAME.ext4" \
    "$SPARKBOX_DIR/gateway_upstream_key.pub" "$ROOTFS_MB"
fi

echo "== kernel networking (forwarding + strict reverse-path filtering) =="
# ip_forward routes sandbox traffic between taps and the uplink. rp_filter is
# strict because the metadata service identifies its caller by source address
# and the host forwards between taps, so a guest must not be able to
# source-spoof a neighbour; the driver sets it per-tap as each is created, this
# covers the host default. /etc/sysctl.d so it survives reboots.
cat > /etc/sysctl.d/99-sparkbox.conf <<'SYSCTL'
net.ipv4.ip_forward=1
net.ipv4.conf.all.rp_filter=1
net.ipv4.conf.default.rp_filter=1
SYSCTL
sysctl -q --system

echo "== packet-filter rules (sandbox NAT + metadata port), applied at boot =="
# Same script and unit the cloud-init path installs — iptables rules are not
# kernel state that persists, so a boot unit owns them. Without this, one reboot
# silently drops sandbox egress.
install -m 0755 "$(dirname "$0")/../deploy/sparkbox-net.sh" /usr/local/sbin/sparkbox-net.sh
cat > /etc/systemd/system/sparkbox-net.service <<'UNIT'
[Unit]
Description=sparkbox host packet-filter rules (sandbox NAT + metadata port)
After=network-online.target
Wants=network-online.target
Before=sparkbox.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/sparkbox-net.sh

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now sparkbox-net.service

echo "== agent-CLI refresher (claude/codex/pi/hivemind) + daily timer =="
# The one host-side unit this script used to omit — the cloud path (cloud-init)
# always installs it, so a from-source box would silently ship sandboxes with no
# agent CLIs. install-host-tooling.sh bakes them into the template now and every
# day. IMAGES_DIR/TOOLS_DIR match this script's XFS data volume layout.
IMAGES_DIR="$SPARKBOX_DIR/data/images" TOOLS_DIR="$SPARKBOX_DIR/data/tools" \
  "$(dirname "$0")/../deploy/install-host-tooling.sh"

# IPv6: if you have a routed /64, each sandbox gets a globally-routable /128
# from it (dual-stack, no NAT). Set SPARKBOX_SUBNET6 to enable, e.g.
#   SPARKBOX_SUBNET6=2001:bc8:702:1c7::/64 ./setup-host.sh '<user> <key>'
SUBNET6_ARG=""
if [ -n "${SPARKBOX_SUBNET6:-}" ]; then
  echo "== IPv6 forwarding for sandbox egress ($SPARKBOX_SUBNET6, no NAT) =="
  sysctl -qw net.ipv6.conf.all.forwarding=1
  echo 'net.ipv6.conf.all.forwarding=1' >> /etc/sysctl.d/99-sparkbox.conf
  # The /64 must be *routed to this host* by your provider (Scaleway Elastic
  # Metal delegates one). No NAT needed: guest /128s are globally routable and
  # the per-VM /127 tap routes the driver adds deliver inbound traffic. Reserve
  # ::1 for this host's own edge address (the AAAA target).
  SUBNET6_ARG=" \\
    --subnet6 $SPARKBOX_SUBNET6"
fi

cat <<EOF

== done ==
Start the server with:

  sparkbox serve --driver firecracker \\
    --state-dir $SPARKBOX_DIR/data/state \\
    --kernel $SPARKBOX_DIR/vmlinux \\
    --image-dir $SPARKBOX_DIR/data/images \\
    --default-image $ROOTFS_NAME \\
    --users $SPARKBOX_DIR/users.conf \\
    --ssh-addr :2222 --api-addr 127.0.0.1:8080 \\
    --proxy-addr :8081 --proxy-domain hivemind.tools${SUBNET6_ARG}

Then from your laptop:

  ssh -p 2222 new@<this-host-ip>       # create a sandbox
  ssh -p 2222 <name>@<this-host-ip>    # shell into it

Web routing: a sandbox "myvm" serving on :8000 is reachable at
myvm.hivemind.tools. For a public HTTPS edge, point a wildcard DNS record
*.hivemind.tools at this host, then serve TLS on :443 with a Cloudflare DNS-01
wildcard cert:

  export CLOUDFLARE_API_TOKEN=<scoped Zone.DNS:Edit token>
  sparkbox serve ... --proxy-addr :443 --proxy-tls \\
    --proxy-domain hivemind.tools --tls-email you@example.com

Route state lives in $SPARKBOX_DIR/data/state/sparkbox.db.
EOF
