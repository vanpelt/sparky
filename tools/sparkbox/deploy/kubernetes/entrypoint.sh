#!/usr/bin/env bash
# Bootstrap the released Firecracker guest artifacts, configure networking in
# this Pod's network namespace, then run Sparkbox directly as PID 1.
set -euo pipefail

readonly data_dir="${SPARKBOX_DATA_DIR:-/var/lib/sparkbox}"
readonly asset_dir="$data_dir/assets"
readonly image_dir="$data_dir/images"
readonly tools_dir="$data_dir/tools"
readonly control_dir="$data_dir/control"
readonly hot_dir="$data_dir/hot"
readonly key_dir="${SPARKBOX_KEY_DIR:-/run/sparkbox/keys}"
readonly durable_dir="${SPARKBOX_DURABLE_DIR:-/mnt/sparkbox-durable}"
readonly release="${SPARKBOX_RELEASE:-v0.5.3}"
readonly artifact_base="${SPARKBOX_ARTIFACT_BASE:-https://github.com/vanpelt/sparky/releases/download}"
readonly proxy_domain="${SPARKBOX_PROXY_DOMAIN:?SPARKBOX_PROXY_DOMAIN is required}"
readonly guest_subnet="${SPARKBOX_GUEST_SUBNET:-172.30.0.0/20}"
readonly host_mem_mb="${SPARKBOX_HOST_MEM_MB:-22000}"
readonly proxy_tls="${SPARKBOX_PROXY_TLS:-true}"
readonly tls_provider="${SPARKBOX_TLS_PROVIDER:-autocert}"
readonly tls_email="${SPARKBOX_TLS_EMAIL:-}"
readonly ssh_advertise_host="${SPARKBOX_SSH_ADVERTISE_HOST:-ssh.$proxy_domain}"
readonly node_name="${SPARKBOX_NODE_NAME:-cks-poc}"
readonly cluster_id="${SPARKBOX_CLUSTER_ID:-$node_name}"
proxy_advertise_port="${SPARKBOX_PROXY_ADVERTISE_PORT:-}"
if [ -z "$proxy_advertise_port" ]; then
  if [ "$proxy_tls" = true ]; then
    proxy_advertise_port=443
  else
    proxy_advertise_port=80
  fi
fi
readonly proxy_advertise_port

case "$(uname -m)" in
  x86_64)
    readonly artifact_arch=amd64
    ;;
  *)
    echo "unsupported Kubernetes POC architecture: $(uname -m) (expected x86_64)" >&2
    exit 1
    ;;
esac

readonly firecracker_sha256="${SPARKBOX_FIRECRACKER_SHA256:-2fd0171309af7e24cf8dafc8a6f921c1434c49b5f9349bb996b7ed0a4deb8aa7}"
readonly kernel_sha256="${SPARKBOX_KERNEL_SHA256:-1b8c89b6c39303228a91da1862ebdb51f583a0aa6f6c78bbe8da22c79a615ae8}"
readonly rootfs_sha256="${SPARKBOX_ROOTFS_SHA256:-53ea8dfbe1dadff39c5df6ad62cb82aa6bef7fdff51525a0df2d64cbd4ed7c9a}"

mkdir -p \
  "$asset_dir" "$image_dir" "$tools_dir" "$control_dir" "$hot_dir" \
  "$durable_dir/checkpoints"

fetch_checked() {
  local url=$1
  local expected=$2
  local destination=$3
  local temporary

  if [ -f "$destination" ] \
    && printf '%s  %s\n' "$expected" "$destination" | sha256sum --check --status; then
    echo "using cached $(basename "$destination")"
    return
  fi

  temporary=$(mktemp "${destination}.download.XXXXXX")
  trap 'rm -f "$temporary"' RETURN
  echo "downloading $(basename "$destination") from $url"
  curl --fail --location --retry 5 --retry-all-errors \
    --connect-timeout 15 --output "$temporary" "$url"
  printf '%s  %s\n' "$expected" "$temporary" | sha256sum --check --status
  mv "$temporary" "$destination"
  trap - RETURN
}

if [ ! -c /dev/kvm ]; then
  echo "/dev/kvm is not a character device; Sparkbox needs a privileged KVM-capable Pod" >&2
  exit 1
fi
if [ ! -c /dev/net/tun ]; then
  echo "/dev/net/tun is not a character device; Sparkbox cannot create guest TAP devices" >&2
  exit 1
fi

# Sparkbox clones a large sparse ext4 template for every VM. Reflinks make that
# operation instant and avoid consuming the template's full logical size.
reflink_source="$hot_dir/.reflink-source"
reflink_copy="$hot_dir/.reflink-copy"
printf 'sparkbox-reflink-probe\n' > "$reflink_source"
if ! cp --reflink=always "$reflink_source" "$reflink_copy"; then
  rm -f "$reflink_source" "$reflink_copy"
  echo "$hot_dir does not support reflinks; use CKS /mnt/local storage" >&2
  exit 1
fi
rm -f "$reflink_source" "$reflink_copy"

firecracker="$asset_dir/firecracker"
kernel="$asset_dir/vmlinux"
compressed_rootfs="$asset_dir/universal-${artifact_arch}.ext4.zst"
rootfs="$image_dir/universal.ext4"
rootfs_marker="$image_dir/.universal-${release}-${rootfs_sha256}.ready"

fetch_checked \
  "$artifact_base/$release/firecracker-$artifact_arch" \
  "$firecracker_sha256" \
  "$firecracker"
fetch_checked \
  "$artifact_base/$release/vmlinux-$artifact_arch" \
  "$kernel_sha256" \
  "$kernel"
fetch_checked \
  "$artifact_base/$release/universal-$artifact_arch.ext4.zst" \
  "$rootfs_sha256" \
  "$compressed_rootfs"
chmod 0755 "$firecracker"

if [ ! -f "$rootfs_marker" ] || [ ! -f "$rootfs" ]; then
  temporary_rootfs="$image_dir/.universal.ext4.decompressing"
  rm -f "$temporary_rootfs"
  echo "decompressing universal rootfs (the sparse image has a large logical size)"
  zstd --decompress --force --sparse -T0 \
    --output-dir-flat "$image_dir" "$compressed_rootfs"
  # zstd preserves the source basename, minus .zst.
  mv "$image_dir/universal-${artifact_arch}.ext4" "$temporary_rootfs"
  mv "$temporary_rootfs" "$rootfs"
  rm -f "$image_dir"/.universal-*.ready
  : > "$rootfs_marker"
fi

# The released rootfs intentionally carries no fast-moving agent CLIs. Patch
# the template before opening the gateway so every newly created sandbox gets
# the same claude/codex/pi/hivemind toolchain and workload-identity payload as
# other Sparkbox hosts.
IMAGES_DIR="$image_dir" \
TOOLS_DIR="$tools_dir" \
GUEST_IDENTITY=/usr/local/sbin/sparkbox-install-guest-identity.sh \
  /usr/local/sbin/sparkbox-refresh-tools.sh

# These sysctls and packet-filter rules affect only the Pod network namespace:
# the CKS node and its Calico data plane remain untouched.
sysctl -q -w net.ipv4.ip_forward=1
sysctl -q -w net.ipv6.conf.all.forwarding=1
sysctl -q -w net.ipv4.conf.all.rp_filter=1
sysctl -q -w net.ipv4.conf.default.rp_filter=1

export PATH="$asset_dir:$PATH"
export SPARKBOX_EDGE_REDIRECT=0
export SPARKBOX_GUEST_SUBNET="$guest_subnet"
/usr/local/sbin/sparkbox-net.sh

tls_args=()
if [ "$proxy_tls" = true ]; then
  tls_args+=(--proxy-tls --tls-provider "$tls_provider")
  if [ -n "$tls_email" ]; then
    tls_args+=(--tls-email "$tls_email")
  fi
fi

echo "starting Sparkbox for *.$proxy_domain (TLS: $proxy_tls)"
exec /usr/local/bin/sparkbox serve \
  --driver firecracker \
  --state-dir "$control_dir" \
  --vm-state-dir "$hot_dir" \
  --key-dir "$key_dir" \
  --require-keys \
  --checkpoint-dir "$durable_dir" \
  --checkpoint-prefix checkpoints \
  --users /etc/sparkbox/users/users.conf \
  --kernel "$kernel" \
  --image-dir "$image_dir" \
  --guest-subnet "$guest_subnet" \
  --ssh-addr :2222 \
  --ssh-advertise-host "$ssh_advertise_host" \
  --ssh-advertise-port 22 \
  --api-addr 127.0.0.1:8080 \
  --metrics-addr "" \
  --proxy-addr :8081 \
  --proxy-advertise-port "$proxy_advertise_port" \
  --proxy-domain "$proxy_domain" \
  --node-name "$node_name" \
  --cluster-id "$cluster_id" \
  --host-mem-mb "$host_mem_mb" \
  --mem-admission-pct 80 \
  --max-running-per-owner 2 \
  "${tls_args[@]}" \
  "$@"
