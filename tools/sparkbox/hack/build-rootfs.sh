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
cleanup() {
  [ -n "$CID" ] && $DOCKER rm -f "$CID" >/dev/null 2>&1 || true
  mountpoint -q "$MNT" && umount "$MNT" || true
  rmdir "$MNT" 2>/dev/null || true
}
trap cleanup EXIT

echo ">> creating ${SIZE_MB}MB ext4 at $OUT"
truncate -s "${SIZE_MB}M" "$OUT"
mkfs.ext4 -q -F "$OUT"
mount -o loop "$OUT" "$MNT"

echo ">> exporting $IMAGE"
CID=$($DOCKER create "$IMAGE" /bin/true)
$DOCKER export "$CID" | tar -x -C "$MNT"

echo ">> baking in gateway key + sshd + serial getty"
mkdir -p "$MNT/root/.ssh"
cp "$PUBKEY" "$MNT/root/.ssh/authorized_keys"
chmod 700 "$MNT/root/.ssh" && chmod 600 "$MNT/root/.ssh/authorized_keys"

# Ensure sshd exists and starts. For the ubuntu image, install openssh-server
# at build time if missing (chroot needs qemu-user-static only for cross-arch).
if [ ! -x "$MNT/usr/sbin/sshd" ]; then
  echo ">> openssh-server missing in image — installing via chroot"
  cp /etc/resolv.conf "$MNT/etc/resolv.conf"
  chroot "$MNT" /bin/sh -c "apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-server" \
    || { echo "install openssh-server failed — use an image that ships sshd"; exit 1; }
fi
mkdir -p "$MNT/etc/ssh/sshd_config.d"
cat > "$MNT/etc/ssh/sshd_config.d/sparkbox.conf" <<'EOF'
PermitRootLogin prohibit-password
PasswordAuthentication no
AcceptEnv LANG LC_* GIT_* TERM
EOF

# Minimal boot: systemd images work as-is with init=/sbin/init; for slim
# images, drop in a tiny rc that brings up sshd on the kernel-arg-configured
# eth0 (ip=... is handled by the kernel itself, no DHCP client needed).
if [ ! -e "$MNT/sbin/init" ] && [ ! -e "$MNT/lib/systemd/systemd" ]; then
  cat > "$MNT/sbin/init" <<'EOF'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev 2>/dev/null
mkdir -p /run/sshd
/usr/sbin/sshd
exec /bin/sh -c 'while true; do sleep 3600; done'
EOF
  chmod +x "$MNT/sbin/init"
fi

echo ">> done: $OUT"
