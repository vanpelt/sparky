#!/usr/bin/env bash
# Publish a versioned sparkbox artifact release to Scaleway Object Storage, so
# new hosts provision by *fetching prebuilt binaries* (seconds) instead of
# building from scratch (minutes). Consumed by deploy/cloud-init.yaml.
#
# Run on a configured sparkbox build host (it needs the repo, a vmlinux, the
# firecracker binary, docker, go, rclone, and the fleet gateway public key).
# The rootfs is content-addressed in the bucket (rootfs/<key>/) and reused
# across releases when nothing that shapes it changed — a binary-only release
# takes ~2 minutes and never touches docker.
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
# Default base: OpenAI's codex-universal — ubuntu:24.04 preloaded with
# toolchains for ~10 languages (python/node/go/rust/java/ruby/php/swift/
# elixir/bazel), i.e. what a coding agent expects to find. ~11GB to pull,
# ~30GB unpacked, so the build host wants ~70GB of scratch disk. Override
# IMAGE=ubuntu:24.04 ROOTFS_NAME=ubuntu ROOTFS_MB=10240 for a slim build.
IMAGE=${IMAGE:-ghcr.io/openai/codex-universal:latest}
ROOTFS_NAME=${ROOTFS_NAME:-universal}   # template + artifact basename; must match the server's --default-image
ROOTFS_MB=${ROOTFS_MB:-65536}   # per-sandbox root disk ceiling (thin CoW copy; ~30GB is toolchains)
# Kernel + firecracker default to a build host's staged copies, but CI (see
# .github/workflows/build-artifacts.yml) downloads them and points here.
KERNEL=${KERNEL:-$SPARKBOX_DIR/vmlinux}
FIRECRACKER_BIN=${FIRECRACKER_BIN:-$(command -v firecracker || true)}
# The rootfs build needs root (loop mount); self-elevate that one step when we
# aren't already root, so the rest (go build, upload) runs unprivileged in CI.
SUDO=""; [ "$(id -u)" -ne 0 ] && SUDO=sudo

[ -f "$GATEWAY_PUBKEY_FILE" ] || { echo "missing gateway pubkey: $GATEWAY_PUBKEY_FILE"; exit 1; }
[ -f "$KERNEL" ] || { echo "missing kernel: $KERNEL"; exit 1; }
[ -x "$FIRECRACKER_BIN" ] || { echo "missing firecracker binary (set FIRECRACKER_BIN)"; exit 1; }

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
sha() { sha256sum "$1" | cut -d' ' -f1; }

echo "== build sparkbox binary =="
( cd "$REPO_DIR/tools/sparkbox" && go build -o "$STAGE/sparkbox" ./cmd/sparkbox )

echo "== collect kernel + firecracker =="
cp "$KERNEL" "$STAGE/vmlinux"
cp "$FIRECRACKER_BIN" "$STAGE/firecracker"

# ---- rootfs: content-addressed, reused across releases ----
# The ~11GB rootfs dwarfs everything else here, yet most releases only change
# the sparkbox binary. Key the rootfs on everything that determines its bytes:
# the base image's registry manifest (digest changes when upstream pushes),
# build-rootfs.sh and the guest-identity payload it installs (between them they
# embed the whole guest config), the baked gateway pubkey, and the name/size
# args. Same key -> reuse the bucket copy and skip
# docker/compress/upload entirely. REBUILD_ROOTFS=1 forces a rebuild (and
# refreshes the cached copy). The .sha256 sidecar is uploaded *after* the
# image, so its presence marks a complete upload — never a torn one.
ROOTFS_REUSED=0
ROOTFS_PATH="releases/$RELEASE/$ROOTFS_NAME.ext4.zst"   # fallback: uncached, built per-release
IMG_MANIFEST=$(docker manifest inspect "$IMAGE" 2>/dev/null || true)
if [ -n "$IMG_MANIFEST" ]; then
  ROOTFS_KEY=$({ printf '%s' "$IMG_MANIFEST"; \
                 cat "$REPO_DIR/tools/sparkbox/hack/build-rootfs.sh" \
                     "$REPO_DIR/tools/sparkbox/deploy/install-guest-identity.sh" \
                     "$GATEWAY_PUBKEY_FILE"; \
                 printf '%s %s' "$ROOTFS_NAME" "$ROOTFS_MB"; } | sha256sum | cut -c1-16)
  ROOTFS_PATH="rootfs/$ROOTFS_KEY/$ROOTFS_NAME.ext4.zst"
  if [ "${REBUILD_ROOTFS:-0}" != 1 ] \
     && SHA256_ROOTFS=$(rclone cat "$RCLONE_REMOTE:$BUCKET/$ROOTFS_PATH.sha256" 2>/dev/null) \
     && [ -n "$SHA256_ROOTFS" ]; then
    ROOTFS_REUSED=1
    echo "== rootfs cache hit: $ROOTFS_PATH (skipping build + upload) =="
  fi
else
  echo "!! docker manifest inspect $IMAGE failed — no cache key; building into the release dir"
fi

if [ "$ROOTFS_REUSED" = 0 ]; then
  echo "== build rootfs (bakes the fleet gateway public key) =="
  $SUDO "$REPO_DIR/tools/sparkbox/hack/build-rootfs.sh" \
    "$IMAGE" "$STAGE/$ROOTFS_NAME.ext4" "$GATEWAY_PUBKEY_FILE" "$ROOTFS_MB"
  # build-rootfs.sh runs as root under $SUDO; reclaim the artifact so we can
  # compress and upload it unprivileged.
  [ -n "$SUDO" ] && $SUDO chown "$(id -u):$(id -g)" "$STAGE/$ROOTFS_NAME.ext4"

  # Disk-starved CI runners: once the template is flattened, the (huge) base
  # image is dead weight in /var/lib/docker — drop it before compression adds
  # another ~11GB alongside the ext4. Opt-in so build hosts keep their pull cache.
  if [ "${PRUNE_IMAGE:-0}" = 1 ] && command -v docker >/dev/null; then
    $SUDO docker rmi -f "$IMAGE" >/dev/null 2>&1 || true
  fi

  # zstd over gzip: multicore compress here, and — the real win — ~5x faster
  # decompress on the host, where gunzip of the ~30GB template was the long
  # pole of provisioning. -10 lands near gzip-9's ratio at a fraction of the
  # time; --rm drops the raw ext4 as soon as the .zst lands (disk headroom).
  command -v zstd >/dev/null || { echo "zstd required: apt-get install zstd"; exit 1; }
  echo "== compress rootfs (zstd) =="
  zstd -T0 -10 -f --rm "$STAGE/$ROOTFS_NAME.ext4" -o "$STAGE/$ROOTFS_NAME.ext4.zst"
  SHA256_ROOTFS=$(sha "$STAGE/$ROOTFS_NAME.ext4.zst")
fi

echo "== manifest =="
FC_VER=$("$FIRECRACKER_BIN" --version | head -1 | grep -oE 'v[0-9.]+' | head -1)
cat > "$STAGE/manifest.env" <<EOF
RELEASE=$RELEASE
FIRECRACKER_VERSION=$FC_VER
SHA256_VMLINUX=$(sha "$STAGE/vmlinux")
SHA256_FIRECRACKER=$(sha "$STAGE/firecracker")
SHA256_SPARKBOX=$(sha "$STAGE/sparkbox")
ROOTFS_NAME=$ROOTFS_NAME
ROOTFS_PATH=$ROOTFS_PATH
SHA256_ROOTFS=$SHA256_ROOTFS
GATEWAY_PUBKEY="$(cat "$GATEWAY_PUBKEY_FILE")"
EOF
echo "---"; cat "$STAGE/manifest.env"; echo "---"

echo "== upload release $RELEASE (public-read) =="
if [ "$ROOTFS_REUSED" = 0 ]; then
  echo ">> $ROOTFS_PATH ($(du -h "$STAGE/$ROOTFS_NAME.ext4.zst" | cut -f1))"
  # Fat multipart settings: rclone's defaults (5MB chunks x4 in flight) crawl
  # at ~2MB/s from a US GitHub runner to fr-par (~100ms RTT) — the 11GB rootfs
  # took 1h37m of a 2h build. 64MB x16 keeps the pipe full regardless of RTT.
  rclone copyto "$STAGE/$ROOTFS_NAME.ext4.zst" "$RCLONE_REMOTE:$BUCKET/$ROOTFS_PATH" \
    --s3-acl public-read --s3-chunk-size 64M --s3-upload-concurrency 16
  # Sidecar last: it doubles as the cache's "upload completed" marker.
  printf '%s\n' "$SHA256_ROOTFS" > "$STAGE/rootfs.sha256"
  rclone copyto "$STAGE/rootfs.sha256" "$RCLONE_REMOTE:$BUCKET/$ROOTFS_PATH.sha256" --s3-acl public-read
fi
base="$RCLONE_REMOTE:$BUCKET/releases/$RELEASE"
for f in vmlinux firecracker sparkbox manifest.env; do
  echo ">> $f ($(du -h "$STAGE/$f" | cut -f1))"
  rclone copyto "$STAGE/$f" "$base/$f" --s3-acl public-read
done
# Point "latest" at this release for cloud-init's default resolution. Set
# PROMOTE_LATEST=0 to publish a release without making it the fleet default.
if [ "${PROMOTE_LATEST:-1}" != 0 ]; then
  printf 'RELEASE=%s\n' "$RELEASE" > "$STAGE/latest.env"
  rclone copyto "$STAGE/latest.env" "$RCLONE_REMOTE:$BUCKET/latest.env" --s3-acl public-read
  echo "  promoted latest -> $RELEASE"
else
  echo "  (skipped latest promotion; PROMOTE_LATEST=0)"
fi

echo
echo "== published =="
echo "  release:  $RELEASE"
echo "  rootfs:   $ROOTFS_PATH ($([ "$ROOTFS_REUSED" = 1 ] && echo reused || echo built))"
echo "  base URL: https://$BUCKET.s3.$REGION.scw.cloud/releases/$RELEASE/"
echo "  manifest: https://$BUCKET.s3.$REGION.scw.cloud/releases/$RELEASE/manifest.env"
