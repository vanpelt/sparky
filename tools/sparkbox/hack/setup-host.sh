#!/usr/bin/env bash
# One-shot setup for a fresh Ubuntu 24.04 bare-metal host (e.g. Scaleway
# Elastic Metal, Hetzner) to run sparkbox with the firecracker driver.
#
# Run as root on the host:  sudo ./setup-host.sh <your-laptop-pubkey-line...>
#
# Does: sanity checks, firecracker install, guest kernel download, XFS
# work volume (reflink CoW), Go + sparkbox build, gateway keys, ubuntu
# rootfs template, NAT, and prints how to start the server.
set -euo pipefail

SPARKBOX_DIR=${SPARKBOX_DIR:-/srv/sparkbox}
SPARKBOX_USER_KEY=${1:?usage: setup-host.sh '<user> <ssh-ed25519 AAAA... comment>' — the users.conf line for your laptop key}
REPO_URL=${REPO_URL:-https://github.com/vanpelt/sparky}

echo "== sanity checks =="
[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }
[ -e /dev/kvm ] || { echo "/dev/kvm missing — not a KVM-capable host?"; exit 1; }
grep -qE 'vmx|svm' /proc/cpuinfo || { echo "no VT-x/AMD-V in cpuinfo"; exit 1; }

echo "== packages =="
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  golang-go docker.io curl iptables xfsprogs git

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

echo "== guest kernel (Firecracker CI build) =="
mkdir -p "$SPARKBOX_DIR"
if [ ! -f "$SPARKBOX_DIR/vmlinux" ]; then
  ARCH=$(uname -m)
  if [ -n "${KERNEL_URL:-}" ]; then
    # Pin an exact kernel (the artifact pipeline does this for reproducibility).
    echo "fetching pinned $KERNEL_URL"
    curl -fsSL "$KERNEL_URL" -o "$SPARKBOX_DIR/vmlinux"
  else
    # The CI kernel bucket lags Firecracker *releases* and its folders don't
    # map 1:1 to release tags (e.g. release v1.16.x but newest kernel folder
    # v1.15), so don't derive the folder from the release. Instead discover the
    # newest *stable* firecracker-ci/vX.Y/ folder that actually ships a vmlinux,
    # skipping experimental suffixes (-pcie-poc, -backup, -al, -vmclock, ...).
    BUCKET="http://spec.ccfc.min.s3.amazonaws.com"
    CI_VERSION=$(curl -fsSL "${BUCKET}/?list-type=2&prefix=firecracker-ci/&delimiter=/" \
      | grep -oP '(?<=<Prefix>)firecracker-ci/v[0-9]+\.[0-9]+/(?=</Prefix>)' \
      | grep -oP 'v[0-9]+\.[0-9]+' | sort -V | tail -1)
    [ -n "$CI_VERSION" ] || { echo "could not enumerate CI kernel versions"; exit 1; }
    LIST=$(curl -fsSL "${BUCKET}/?list-type=2&prefix=firecracker-ci/${CI_VERSION}/${ARCH}/vmlinux-" \
      | grep -oP '(?<=<Key>)firecracker-ci/[^<]+(?=</Key>)' \
      | grep -vE '\.config$|-no-acpi')
    # Prefer a 6.1.x LTS guest kernel; fall back to the newest available.
    KEY=$(printf '%s\n' "$LIST" | grep -E 'vmlinux-6\.1\.' | sort -V | tail -1)
    [ -n "$KEY" ] || KEY=$(printf '%s\n' "$LIST" | sort -V | tail -1)
    [ -n "$KEY" ] || { echo "could not resolve CI kernel for ${CI_VERSION}"; exit 1; }
    echo "resolved guest kernel: $KEY"
    curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/${KEY}" -o "$SPARKBOX_DIR/vmlinux"
  fi
fi

echo "== XFS work volume (reflink CoW for instant rootfs copies) =="
if ! mountpoint -q "$SPARKBOX_DIR/data"; then
  mkdir -p "$SPARKBOX_DIR/data"
  if [ ! -f "$SPARKBOX_DIR/data.img" ]; then
    truncate -s 100G "$SPARKBOX_DIR/data.img"   # sparse; grows on use
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

echo "== ubuntu rootfs template =="
if [ ! -f "$SPARKBOX_DIR/data/images/ubuntu.ext4" ]; then
  ./hack/build-rootfs.sh ubuntu:24.04 "$SPARKBOX_DIR/data/images/ubuntu.ext4" \
    "$SPARKBOX_DIR/gateway_upstream_key.pub" 4096
fi

echo "== NAT for sandbox egress (172.30.0.0/16 via default uplink) =="
sysctl -qw net.ipv4.ip_forward=1
echo 'net.ipv4.ip_forward=1' > /etc/sysctl.d/99-sparkbox.conf
UPLINK=$(ip route | awk '/default/{print $5; exit}')
iptables -t nat -C POSTROUTING -s 172.30.0.0/16 -o "$UPLINK" -j MASQUERADE 2>/dev/null || \
  iptables -t nat -A POSTROUTING -s 172.30.0.0/16 -o "$UPLINK" -j MASQUERADE

cat <<EOF

== done ==
Start the server with:

  sparkbox serve --driver firecracker \\
    --state-dir $SPARKBOX_DIR/data/state \\
    --kernel $SPARKBOX_DIR/vmlinux \\
    --image-dir $SPARKBOX_DIR/data/images \\
    --users $SPARKBOX_DIR/users.conf \\
    --ssh-addr :2222 --api-addr 127.0.0.1:8080 \\
    --proxy-addr :8081 --proxy-domain hivemind.tools

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
