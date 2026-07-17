#!/usr/bin/env bash
# Build a Firecracker rootfs ext4 template from an OCI/Docker image.
#
# This is the "container image as block device" trick from the design doc:
# flatten an image, bake in the gateway's SSH public key, install the guest boot
# hooks, and every sandbox gets a CoW reflink copy of the result.
#
# Usage: build-rootfs.sh <docker-image> <output.ext4> <gateway-pubkey-file> [size-mb]
# Requires: docker (or podman), mkfs.ext4, root (for the loop mount).
#
# The image is expected to already ship sshd + systemd + a polished shell — our
# hack/images/Dockerfile bakes all of that (and declares its login user via the
# sparkbox.login-user label). This script no longer patches the image; it just
# flattens it. The ext4 is a thin ceiling: hosts clone it with XFS reflinks, so
# sandboxes only pay for blocks they write.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)

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

# Which user does the gateway log in as? The image declares it via the
# sparkbox.login-user label (our hack/images/Dockerfile sets "sparky"); default
# root for images that predate the label (bare ubuntu / codex-universal).
LOGIN_USER=$($DOCKER inspect -f '{{index .Config.Labels "sparkbox.login-user"}}' "$IMAGE" 2>/dev/null || true)
[ -n "$LOGIN_USER" ] || LOGIN_USER=root
echo ">> login user: $LOGIN_USER"

# Fail fast if the flattened image won't fit the requested ext4 — better now
# than 20 minutes in with a half-extracted tar and ENOSPC. 1.2x + 512MB covers
# ext4 metadata + a little breathing room (the real workspace headroom should
# come from passing a generous size-mb; the copy is thin either way).
IMG_BYTES=$($DOCKER image inspect -f '{{.Size}}' "$IMAGE")
MIN_MB=$(( IMG_BYTES / 1048576 * 12 / 10 + 512 ))
if [ "$SIZE_MB" -lt "$MIN_MB" ]; then
  echo "size-mb $SIZE_MB is too small: $IMAGE unpacks to ~$((IMG_BYTES/1048576))MB (need >= ${MIN_MB}MB)" >&2
  exit 1
fi

echo ">> creating ${SIZE_MB}MB ext4 at $OUT"
truncate -s "${SIZE_MB}M" "$OUT"
mkfs.ext4 -q -F "$OUT"
mount -o loop "$OUT" "$MNT"

echo ">> exporting $IMAGE"
CID=$($DOCKER create "$IMAGE" /bin/true)
$DOCKER export "$CID" | tar -x -C "$MNT"

if [ ! -x "$MNT/usr/sbin/sshd" ]; then
  echo "sshd missing in $IMAGE — bake it into the image (see hack/images/Dockerfile) or use a base that ships sshd" >&2
  exit 1
fi

echo ">> baking in gateway key for $LOGIN_USER + sshd config"
# Resolve the login user's home + numeric uid:gid from the flattened tree, so we
# own the authorized_keys correctly without a chroot.
if [ "$LOGIN_USER" = root ]; then
  HOME_DIR=/root
  ROOT_LOGIN=prohibit-password
else
  HOME_DIR=$(awk -F: -v u="$LOGIN_USER" '$1==u{print $6}' "$MNT/etc/passwd")
  [ -n "$HOME_DIR" ] || { echo "login user $LOGIN_USER not found in image /etc/passwd" >&2; exit 1; }
  ROOT_LOGIN=no
fi
UID_GID=$(awk -F: -v u="$LOGIN_USER" '$1==u{print $3":"$4}' "$MNT/etc/passwd")
[ -n "$UID_GID" ] || UID_GID=0:0
SSH_DIR="$MNT$HOME_DIR/.ssh"
mkdir -p "$SSH_DIR"
cp "$PUBKEY" "$SSH_DIR/authorized_keys"
chmod 700 "$SSH_DIR" && chmod 600 "$SSH_DIR/authorized_keys"
chown -R "$UID_GID" "$MNT$HOME_DIR/.ssh"

mkdir -p "$MNT/etc/ssh/sshd_config.d"
cat > "$MNT/etc/ssh/sshd_config.d/sparkbox.conf" <<EOF
PermitRootLogin $ROOT_LOGIN
PasswordAuthentication no
AcceptEnv LANG LC_* GIT_* TERM
EOF

# The shell polish (MOTD, starship, /etc/environment, locale, ghostty terminfo)
# now lives in hack/images/Dockerfile so the image is the single source of truth.

# Don't let a stale /etc/hostname from the image leak into the guest; the
# sparkbox-netcfg hook sets the hostname to the sandbox name at boot.
: > "$MNT/etc/hostname"

echo ">> installing IPv6 guest-network hook"
# The kernel ip= arg only configures IPv4. The firecracker driver passes the
# guest's routable IPv6 on the cmdline (sparkbox_ip6=<addr>/127 sparkbox_gw6=..);
# this oneshot applies it to eth0 at boot. No-op when those args are absent.
mkdir -p "$MNT/usr/local/sbin"
cat > "$MNT/usr/local/sbin/sparkbox-netcfg" <<'EOF'
#!/bin/sh
# Apply sparkbox guest config (hostname + IPv6) from the kernel cmdline.
IFACE=eth0
IP6=""; GW6=""; HOST=""
for tok in $(cat /proc/cmdline); do
  case "$tok" in
    sparkbox_ip6=*)  IP6="${tok#sparkbox_ip6=}" ;;
    sparkbox_gw6=*)  GW6="${tok#sparkbox_gw6=}" ;;
    sparkbox_host=*) HOST="${tok#sparkbox_host=}" ;;
  esac
done

# Name the box after the sandbox so the prompt reads root@<name> (the kernel's
# ip= arg otherwise leaves the hostname as the guest IP -> "root@172"). This
# runs before sshd, so the login shell picks up the new hostname. Keep
# /etc/hosts resolvable too, or sudo/tools warn "unable to resolve host".
if [ -n "$HOST" ]; then
  hostname "$HOST" 2>/dev/null || true
  echo "$HOST" > /etc/hostname 2>/dev/null || true
  grep -q "[[:space:]]$HOST\$" /etc/hosts 2>/dev/null || \
    printf '127.0.1.1\t%s\n' "$HOST" >> /etc/hosts
fi

# The kernel ip= arg configures the interface but writes no resolver (Firecracker
# boots vmlinux with no initrd, so nothing populates /etc/resolv.conf). Egress is
# NAT'd, so point at public resolvers; skip if something already set one.
if [ ! -s /etc/resolv.conf ]; then
  printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/resolv.conf
fi

# Firecracker guests boot with no cloud-init to regenerate the SSH host keys the
# image intentionally omits (per-guest keys, never a pair shared across every
# sandbox). Generate any missing ones here — this oneshot is ordered
# Before=ssh.service, so sshd finds them and can actually bind :22. Without this,
# sshd exits on "no hostkeys available" and the gateway gets connection-refused.
ssh-keygen -A 2>/dev/null || true

[ -n "$IP6" ] || exit 0
ip link set "$IFACE" up
ip -6 addr replace "$IP6" dev "$IFACE"
[ -n "$GW6" ] && ip -6 route replace default via "$GW6" dev "$IFACE"
exit 0
EOF
chmod +x "$MNT/usr/local/sbin/sparkbox-netcfg"

echo ">> installing workload identity (OIDC token) hook"
# Shared with deploy/refresh-agent-tools.sh, which patches the same payload
# into already-published templates on a host — keep it in one place.
"$HERE/../deploy/install-guest-identity.sh" "$MNT"

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
# No systemd here, so no timer: fetch an identity token once at boot. It
# expires in an hour and won't renew — slim images are a fallback for bring-up,
# not the shipped path (which is the systemd timer).
/usr/local/sbin/sparkbox-token 2>/dev/null || true
mkdir -p /run/sshd
/usr/sbin/sshd
exec /bin/sh -c 'while true; do sleep 3600; done'
EOF
  chmod +x "$MNT/sbin/init"
fi

# Record the login user next to the template so build-artifacts.sh can publish it
# (ROOTFS_LOGIN_USER in the manifest) and the gateway logs in as the right user.
printf '%s\n' "$LOGIN_USER" > "$OUT.login-user"

echo ">> done: $OUT (login user: $LOGIN_USER)"
