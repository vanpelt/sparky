#!/usr/bin/env bash
# Stage one architecture's sparkbox release artifacts into a directory, ready to
# be attached to a GitHub Release. This is the build half of what used to be
# build-artifacts.sh; publishing now belongs to the caller (see
# .github/workflows/build-artifacts.yml, which uploads the staged files with
# `gh release upload`, or run it by hand on a build host and copy them wherever).
#
# Every asset name carries the target arch, because a release ships both
# linux/amd64 and linux/arm64 side by side in one flat GitHub asset namespace:
#
#   sparkbox-linux-<arch>          the control-plane binary (host side)
#   vmlinux-<arch>                 guest kernel (flat Image on arm64; the name
#                                  stays vmlinux — firecracker doesn't care)
#   firecracker-<arch>             the VMM
#   <rootfs-name>-<arch>.ext4.zst  the guest rootfs template
#   manifest-<arch>.env            sha256s + metadata `sparkbox setup` reads
#
# Runs natively: aarch64 artifacts want an aarch64 host (the rootfs build loop-
# mounts an ext4 and the kernel build is a native compile). The docker image is
# the one exception — pass a multi-arch IMAGE ref and docker pulls the variant
# matching this host.
#
# Usage: OUT_DIR=/tmp/rel RELEASE=v0.3.0 IMAGE=ghcr.io/…/universal:v0.3.0 \
#          GATEWAY_PUBKEY_FILE=… KERNEL=… ./stage-artifacts.sh
set -euo pipefail

SPARKBOX_DIR=${SPARKBOX_DIR:-/srv/sparkbox}
REPO_DIR=${REPO_DIR:-$SPARKBOX_DIR/repo}
RELEASE=${RELEASE:-$(date -u +%Y-%m-%d-%H%M)}
OUT_DIR=${OUT_DIR:?set OUT_DIR: where to stage the release assets}
GATEWAY_PUBKEY_FILE=${GATEWAY_PUBKEY_FILE:-$SPARKBOX_DIR/gateway_upstream_key.pub}

# Target arch in Go's vocabulary (amd64/arm64) — the same spelling `sparkbox
# setup` resolves from runtime.GOARCH when it picks which assets to download.
ARCH=${ARCH:-$(uname -m)}
case "$ARCH" in
  x86_64|amd64)  GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) echo "unsupported ARCH=$ARCH (want amd64 | arm64)" >&2; exit 1 ;;
esac

# Base image for the rootfs. CI passes the multi-arch image Depot built and
# pushed to GHCR; blank means build our own lean image from images/ locally
# (what a build host without a registry does).
IMAGE=${IMAGE:-}
IMAGES_DIR="$REPO_DIR/tools/sparkbox/images"
BUILD_OWN_IMAGE=0
if [ -z "$IMAGE" ]; then
  BUILD_OWN_IMAGE=1
  IMAGE="sparkbox-base:$RELEASE"
fi
ROOTFS_NAME=${ROOTFS_NAME:-universal}   # template basename; must match the server's --default-image
ROOTFS_MB=${ROOTFS_MB:-25600}   # per-sandbox root disk ceiling, 25 GiB (thin CoW copy; mostly unwritten)
KERNEL=${KERNEL:-$SPARKBOX_DIR/vmlinux}
FIRECRACKER_BIN=${FIRECRACKER_BIN:-$(command -v firecracker || true)}
# The rootfs build needs root (loop mount); self-elevate that one step when we
# aren't already root, so the rest (go build, staging) runs unprivileged in CI.
SUDO=""; [ "$(id -u)" -ne 0 ] && SUDO=sudo

[ -f "$GATEWAY_PUBKEY_FILE" ] || { echo "missing gateway pubkey: $GATEWAY_PUBKEY_FILE"; exit 1; }
[ -f "$KERNEL" ] || { echo "missing kernel: $KERNEL"; exit 1; }
[ -x "$FIRECRACKER_BIN" ] || { echo "missing firecracker binary (set FIRECRACKER_BIN)"; exit 1; }
command -v zstd >/dev/null || { echo "zstd required: apt-get install zstd"; exit 1; }

mkdir -p "$OUT_DIR"
OUT_DIR=$(cd "$OUT_DIR" && pwd)
sha() { sha256sum "$1" | cut -d' ' -f1; }

SPARKBOX_ASSET="sparkbox-linux-$GOARCH"
ROOTFS_ASSET="$ROOTFS_NAME-$GOARCH.ext4.zst"

echo "== build sparkbox binary ($GOARCH) =="
# Static (CGO off) so one binary runs on any glibc/musl host of this arch.
( cd "$REPO_DIR/tools/sparkbox" && \
  CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" \
  go build -trimpath -ldflags "-s -w -X main.version=$RELEASE" \
    -o "$OUT_DIR/$SPARKBOX_ASSET" ./cmd/sparkbox )

echo "== collect kernel + firecracker =="
cp "$KERNEL" "$OUT_DIR/vmlinux-$GOARCH"
cp "$FIRECRACKER_BIN" "$OUT_DIR/firecracker-$GOARCH"

if [ "$BUILD_OWN_IMAGE" = 1 ]; then
  echo "== build base image from images/Dockerfile (tag $IMAGE) =="
  docker build -t "$IMAGE" "$IMAGES_DIR"
else
  echo "== pull $IMAGE (this host's arch variant) =="
  docker pull -q "$IMAGE"
fi

echo "== build rootfs (bakes the fleet gateway public key) =="
$SUDO "$REPO_DIR/tools/sparkbox/hack/build-rootfs.sh" \
  "$IMAGE" "$OUT_DIR/$ROOTFS_NAME.ext4" "$GATEWAY_PUBKEY_FILE" "$ROOTFS_MB"
# build-rootfs.sh runs as root under $SUDO; reclaim the artifact + the login-user
# sidecar it wrote so we can compress, read, and upload them unprivileged.
[ -n "$SUDO" ] && $SUDO chown "$(id -u):$(id -g)" \
  "$OUT_DIR/$ROOTFS_NAME.ext4" "$OUT_DIR/$ROOTFS_NAME.ext4.login-user"
ROOTFS_LOGIN_USER=$(cat "$OUT_DIR/$ROOTFS_NAME.ext4.login-user" 2>/dev/null || echo root)
rm -f "$OUT_DIR/$ROOTFS_NAME.ext4.login-user"

# Disk-starved CI runners: once the template is flattened, the base image is
# dead weight in /var/lib/docker — drop it before compression adds another copy
# alongside the ext4. Opt-in so build hosts keep their layer cache.
if [ "${PRUNE_IMAGE:-0}" = 1 ] && command -v docker >/dev/null; then
  $SUDO docker rmi -f "$IMAGE" >/dev/null 2>&1 || true
fi

# zstd over gzip: multicore compress here, and — the real win — ~5x faster
# decompress on the host, where gunzip of the template was a long pole of
# provisioning. -10 lands near gzip-9's ratio at a fraction of the time; --rm
# drops the raw ext4 as soon as the .zst lands (disk headroom).
echo "== compress rootfs (zstd) =="
zstd -T0 -10 -f --rm "$OUT_DIR/$ROOTFS_NAME.ext4" -o "$OUT_DIR/$ROOTFS_ASSET"

# GitHub caps a single release asset at 2 GiB. The template is the only asset
# anywhere near that, and it grows with the Dockerfile's toolset — fail here,
# where the message can say what to do, rather than in an opaque upload 500.
ROOTFS_BYTES=$(stat -c %s "$OUT_DIR/$ROOTFS_ASSET")
if [ "$ROOTFS_BYTES" -gt $((2 * 1024 * 1024 * 1024)) ]; then
  echo "$ROOTFS_ASSET is $((ROOTFS_BYTES / 1048576))MB — over GitHub's 2GiB per-asset cap." >&2
  echo "Trim images/Dockerfile, or split the template across assets and teach hostsetup to rejoin it." >&2
  exit 1
fi

echo "== manifest =="
FC_VER=$("$FIRECRACKER_BIN" --version | head -1 | grep -oE 'v[0-9.]+' | head -1)
cat > "$OUT_DIR/manifest-$GOARCH.env" <<EOF
RELEASE=$RELEASE
ARCH=$GOARCH
FIRECRACKER_VERSION=$FC_VER
SHA256_VMLINUX=$(sha "$OUT_DIR/vmlinux-$GOARCH")
SHA256_FIRECRACKER=$(sha "$OUT_DIR/firecracker-$GOARCH")
SHA256_SPARKBOX=$(sha "$OUT_DIR/$SPARKBOX_ASSET")
ROOTFS_NAME=$ROOTFS_NAME
ROOTFS_ASSET=$ROOTFS_ASSET
SHA256_ROOTFS=$(sha "$OUT_DIR/$ROOTFS_ASSET")
ROOTFS_LOGIN_USER=$ROOTFS_LOGIN_USER
GATEWAY_PUBKEY="$(cat "$GATEWAY_PUBKEY_FILE")"
EOF
echo "---"; cat "$OUT_DIR/manifest-$GOARCH.env"; echo "---"

echo
echo "== staged $RELEASE ($GOARCH) in $OUT_DIR =="
( cd "$OUT_DIR" && du -h -- * | sed 's/^/  /' )
