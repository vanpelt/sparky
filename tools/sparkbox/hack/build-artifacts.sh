#!/usr/bin/env bash
# Publish a versioned sparkbox artifact release to Scaleway Object Storage, so
# new hosts provision by *fetching prebuilt binaries* (seconds) instead of
# building from scratch (minutes). Consumed by deploy/cloud-init.yaml.
#
# Run on a configured sparkbox build host (it needs the repo, a vmlinux, the
# firecracker binary, docker, go, rclone, and the fleet gateway public key).
# Uploads are public-read: none of these artifacts are secret — the rootfs only
# bakes in the gateway *public* key. The gateway *private* key is the fleet
# secret and is injected per-host by cloud-init, never uploaded here.
#
# Usage: build-artifacts.sh            # release tag defaults to UTC timestamp
#        RELEASE=v1 build-artifacts.sh
set -euo pipefail

SPARKBOX_DIR=${SPARKBOX_DIR:-/srv/sparkbox}
REPO_DIR=${REPO_DIR:-$SPARKBOX_DIR/repo}
BUCKET=${BUCKET:-sparkbox-artifacts}
RCLONE_REMOTE=${RCLONE_REMOTE:-sparkbox-artifacts}
REGION=${REGION:-fr-par}
RELEASE=${RELEASE:-$(date -u +%Y-%m-%d-%H%M)}
GATEWAY_PUBKEY_FILE=${GATEWAY_PUBKEY_FILE:-$SPARKBOX_DIR/gateway_upstream_key.pub}
IMAGE=${IMAGE:-ubuntu:24.04}
ROOTFS_MB=${ROOTFS_MB:-4096}

[ -f "$GATEWAY_PUBKEY_FILE" ] || { echo "missing gateway pubkey: $GATEWAY_PUBKEY_FILE"; exit 1; }
[ -f "$SPARKBOX_DIR/vmlinux" ] || { echo "missing kernel: $SPARKBOX_DIR/vmlinux"; exit 1; }

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
sha() { sha256sum "$1" | cut -d' ' -f1; }

echo "== build sparkbox binary =="
( cd "$REPO_DIR/tools/sparkbox" && go build -o "$STAGE/sparkbox" ./cmd/sparkbox )

echo "== collect kernel + firecracker =="
cp "$SPARKBOX_DIR/vmlinux" "$STAGE/vmlinux"
cp "$(command -v firecracker)" "$STAGE/firecracker"

echo "== build rootfs (bakes the fleet gateway public key) =="
"$REPO_DIR/tools/sparkbox/hack/build-rootfs.sh" \
  "$IMAGE" "$STAGE/ubuntu.ext4" "$GATEWAY_PUBKEY_FILE" "$ROOTFS_MB"
echo "== gzip rootfs (mostly-empty ext4 -> a few hundred MB) =="
gzip -f "$STAGE/ubuntu.ext4"   # -> ubuntu.ext4.gz

echo "== manifest =="
FC_VER=$(firecracker --version | head -1 | grep -oE 'v[0-9.]+' | head -1)
cat > "$STAGE/manifest.env" <<EOF
RELEASE=$RELEASE
FIRECRACKER_VERSION=$FC_VER
SHA256_VMLINUX=$(sha "$STAGE/vmlinux")
SHA256_FIRECRACKER=$(sha "$STAGE/firecracker")
SHA256_SPARKBOX=$(sha "$STAGE/sparkbox")
SHA256_ROOTFS_GZ=$(sha "$STAGE/ubuntu.ext4.gz")
GATEWAY_PUBKEY="$(cat "$GATEWAY_PUBKEY_FILE")"
EOF
echo "---"; cat "$STAGE/manifest.env"; echo "---"

echo "== upload release $RELEASE (public-read) =="
base="$RCLONE_REMOTE:$BUCKET/releases/$RELEASE"
for f in vmlinux firecracker sparkbox ubuntu.ext4.gz manifest.env; do
  echo ">> $f ($(du -h "$STAGE/$f" | cut -f1))"
  rclone copyto "$STAGE/$f" "$base/$f" --s3-acl public-read
done
# Point "latest" at this release for cloud-init's default resolution.
printf 'RELEASE=%s\n' "$RELEASE" > "$STAGE/latest.env"
rclone copyto "$STAGE/latest.env" "$RCLONE_REMOTE:$BUCKET/latest.env" --s3-acl public-read

echo
echo "== published =="
echo "  release:  $RELEASE"
echo "  base URL: https://$BUCKET.s3.$REGION.scw.cloud/releases/$RELEASE/"
echo "  manifest: https://$BUCKET.s3.$REGION.scw.cloud/releases/$RELEASE/manifest.env"
