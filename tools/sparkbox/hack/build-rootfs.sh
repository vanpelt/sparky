#!/usr/bin/env bash
# Build a Firecracker rootfs ext4 template from an OCI/Docker image.
#
# This is the "container image as block device" trick from the design doc:
# flatten an image, bake in the gateway's SSH public key + a tiny init, and
# every sandbox gets a CoW reflink copy of the result.
#
# Usage: build-rootfs.sh <docker-image> <output.ext4> <gateway-pubkey-file> [size-mb]
# Requires: docker (or podman), mkfs.ext4, root (for the loop mount).
set -euo pipefail

IMAGE=${1:?docker image, e.g. ubuntu:24.04}
OUT=${2:?output path, e.g. images/ubuntu.ext4}
PUBKEY=${3:?gateway public key file (state/gateway_upstream_key.pem -> derive with sparkbox, or ssh-keygen -y)}
SIZE_MB=${4:-2048}

DOCKER=$(command -v docker || command -v podman)
MNT=$(mktemp -d)
CID=""
BUILD_IMAGE=""
cleanup() {
  [ -n "$CID" ] && $DOCKER rm -f "$CID" >/dev/null 2>&1 || true
  [ -n "$BUILD_IMAGE" ] && $DOCKER rmi "$BUILD_IMAGE" >/dev/null 2>&1 || true
  mountpoint -q "$MNT" && umount "$MNT" || true
  rmdir "$MNT" 2>/dev/null || true
}
trap cleanup EXIT

# Ensure the image ships sshd + a real init before we flatten it. We do this
# with `docker build` (a real builder with working apt) rather than chrooting
# into the exported tree — the chroot has no /proc,/sys,/dev bind mounts, so
# apt/gpg fail with "apt-key error code 29". Debian/Ubuntu only; bring your own
# sshd for other bases.
echo ">> ensuring sshd + init in $IMAGE"
BUILD_IMAGE="sparkbox-rootfs-build:$$"
$DOCKER build -t "$BUILD_IMAGE" - >/dev/null <<EOF
FROM $IMAGE
ENV DEBIAN_FRONTEND=noninteractive
RUN set -eu; \
    if [ ! -x /usr/sbin/sshd ]; then \
      apt-get update -qq; \
      apt-get install -y -qq openssh-server systemd-sysv iproute2 iputils-ping ca-certificates; \
      rm -rf /var/lib/apt/lists/*; \
    fi; \
    mkdir -p /run/sshd
EOF

echo ">> creating ${SIZE_MB}MB ext4 at $OUT"
truncate -s "${SIZE_MB}M" "$OUT"
mkfs.ext4 -q -F "$OUT"
mount -o loop "$OUT" "$MNT"

echo ">> exporting $IMAGE"
CID=$($DOCKER create "$BUILD_IMAGE" /bin/true)
$DOCKER export "$CID" | tar -x -C "$MNT"

echo ">> baking in gateway key + sshd config"
mkdir -p "$MNT/root/.ssh"
cp "$PUBKEY" "$MNT/root/.ssh/authorized_keys"
chmod 700 "$MNT/root/.ssh" && chmod 600 "$MNT/root/.ssh/authorized_keys"

if [ ! -x "$MNT/usr/sbin/sshd" ]; then
  echo "sshd still missing after build — use a Debian/Ubuntu base or an image that ships sshd" >&2
  exit 1
fi
mkdir -p "$MNT/etc/ssh/sshd_config.d"
cat > "$MNT/etc/ssh/sshd_config.d/sparkbox.conf" <<'EOF'
PermitRootLogin prohibit-password
PasswordAuthentication no
AcceptEnv LANG LC_* GIT_* TERM
EOF

echo ">> installing IPv6 guest-network hook"
# The kernel ip= arg only configures IPv4. The firecracker driver passes the
# guest's routable IPv6 on the cmdline (sparkbox_ip6=<addr>/127 sparkbox_gw6=..);
# this oneshot applies it to eth0 at boot. No-op when those args are absent.
mkdir -p "$MNT/usr/local/sbin"
cat > "$MNT/usr/local/sbin/sparkbox-netcfg" <<'EOF'
#!/bin/sh
# Apply sparkbox IPv6 config from the kernel cmdline to eth0.
IFACE=eth0
IP6=""; GW6=""
for tok in $(cat /proc/cmdline); do
  case "$tok" in
    sparkbox_ip6=*) IP6="${tok#sparkbox_ip6=}" ;;
    sparkbox_gw6=*) GW6="${tok#sparkbox_gw6=}" ;;
  esac
done
[ -n "$IP6" ] || exit 0
ip link set "$IFACE" up
ip -6 addr replace "$IP6" dev "$IFACE"
[ -n "$GW6" ] && ip -6 route replace default via "$GW6" dev "$IFACE"
exit 0
EOF
chmod +x "$MNT/usr/local/sbin/sparkbox-netcfg"

# systemd path (ubuntu:24.04 and friends): run early, before sshd.
if [ -e "$MNT/lib/systemd/systemd" ]; then
  cat > "$MNT/etc/systemd/system/sparkbox-net.service" <<'EOF'
[Unit]
Description=sparkbox guest IPv6 configuration
DefaultDependencies=no
After=network-pre.target local-fs.target
Before=ssh.service sshd.service network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/sparkbox-netcfg

[Install]
WantedBy=multi-user.target
EOF
  # Enable without a chroot: symlink into multi-user.target.wants.
  mkdir -p "$MNT/etc/systemd/system/multi-user.target.wants"
  ln -sf ../sparkbox-net.service \
    "$MNT/etc/systemd/system/multi-user.target.wants/sparkbox-net.service"
fi

# Minimal boot: systemd images work as-is with init=/sbin/init; for slim
# images, drop in a tiny rc that brings up sshd on the kernel-arg-configured
# eth0 (ip=... is handled by the kernel itself, no DHCP client needed).
if [ ! -e "$MNT/sbin/init" ] && [ ! -e "$MNT/lib/systemd/systemd" ]; then
  cat > "$MNT/sbin/init" <<'EOF'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev 2>/dev/null
/usr/local/sbin/sparkbox-netcfg 2>/dev/null || true
mkdir -p /run/sshd
/usr/sbin/sshd
exec /bin/sh -c 'while true; do sleep 3600; done'
EOF
  chmod +x "$MNT/sbin/init"
fi

echo ">> done: $OUT"
