#!/usr/bin/env bash
# Bootstrap the released Firecracker guest artifacts, configure networking in
# this Pod's network namespace, then run the private Sparkbox VM node as PID 1.
set -euo pipefail

mode=run
if [ "${1:-}" = prepare ]; then
  mode=prepare
  shift
fi
if [ "$#" -gt 0 ] && [ "$mode" = prepare ]; then
  echo "prepare mode accepts no additional arguments" >&2
  exit 2
fi

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
readonly gateway_addr="${SPARKBOX_GATEWAY_ADDR:-}"
readonly node_key_dir="${SPARKBOX_NODE_KEY_DIR:-$data_dir/node-identity}"
readonly skip_prepare="${SPARKBOX_SKIP_PREPARE:-false}"
readonly chroot_jailer="${SPARKBOX_CHROOT_JAILER:-false}"
readonly disable_host_rootfs_mounts="${SPARKBOX_DISABLE_HOST_ROOTFS_MOUNTS:-false}"
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
readonly firecracker_version="${SPARKBOX_FIRECRACKER_VERSION:-v1.16.1}"
readonly jailer_sha256="${SPARKBOX_JAILER_SHA256:-1f3a0c1fe86212d0001819bfe0819071c01208b3ccc9398c3b3bc1b84cf21edd}"
readonly kernel_sha256="${SPARKBOX_KERNEL_SHA256:-1b8c89b6c39303228a91da1862ebdb51f583a0aa6f6c78bbe8da22c79a615ae8}"
readonly rootfs_sha256="${SPARKBOX_ROOTFS_SHA256:-53ea8dfbe1dadff39c5df6ad62cb82aa6bef7fdff51525a0df2d64cbd4ed7c9a}"

mkdir -p \
  "$asset_dir" "$image_dir" "$tools_dir" "$control_dir" "$hot_dir" \
  "$durable_dir/checkpoints" "$node_key_dir"

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
  if ! curl --fail --location --retry 5 --retry-all-errors \
    --connect-timeout 15 --output "$temporary" "$url"; then
    rm -f "$temporary"
    trap - RETURN
    return 1
  fi
  if ! printf '%s  %s\n' "$expected" "$temporary" | sha256sum --check --status; then
    echo "checksum mismatch for $url" >&2
    rm -f "$temporary"
    trap - RETURN
    return 1
  fi
  mv "$temporary" "$destination"
  trap - RETURN
}

fetch_upstream_jailer() {
  local destination=$1
  local temporary_dir archive extracted

  temporary_dir=$(mktemp -d "$asset_dir/.jailer-download.XXXXXX")
  trap 'rm -rf "$temporary_dir"' RETURN
  archive="$temporary_dir/firecracker.tgz"
  extracted="$temporary_dir/jailer"
  echo "downloading matching jailer from Firecracker $firecracker_version"
  curl --fail --location --retry 5 --retry-all-errors \
    --connect-timeout 15 --output "$archive" \
    "https://github.com/firecracker-microvm/firecracker/releases/download/$firecracker_version/firecracker-$firecracker_version-x86_64.tgz"
  tar -xOf "$archive" \
    "release-$firecracker_version-x86_64/jailer-$firecracker_version-x86_64" \
    > "$extracted"
  printf '%s  %s\n' "$jailer_sha256" "$extracted" | sha256sum --check --status
  mv "$extracted" "$destination"
  trap - RETURN
  rm -rf "$temporary_dir"
}

if [ "$mode" = prepare ]; then
  for loop_device in /dev/loop-control /dev/loop{0..7}; do
    if [ ! -b "$loop_device" ] && [ ! -c "$loop_device" ]; then
      echo "$loop_device is not a device; check the sparkbox.dev/loop allocation" >&2
      exit 1
    fi
  done
else
  if [ ! -c /dev/kvm ]; then
    echo "/dev/kvm is not a character device; check the sparkbox.dev/kvm allocation" >&2
    exit 1
  fi
  if [ ! -c /dev/net/tun ]; then
    echo "/dev/net/tun is not a character device; check the sparkbox.dev/tun allocation" >&2
    exit 1
  fi
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
jailer="$asset_dir/jailer"
kernel="$asset_dir/vmlinux"
compressed_rootfs="$asset_dir/universal-${artifact_arch}.ext4.zst"
rootfs="$image_dir/universal.ext4"
rootfs_marker="$image_dir/.universal-${release}-${rootfs_sha256}.ready"

if [ "$mode" = prepare ] || [ "$skip_prepare" != true ]; then
  fetch_checked \
    "$artifact_base/$release/firecracker-$artifact_arch" \
    "$firecracker_sha256" \
    "$firecracker"
  # Releases cut after jailer support carry this beside Firecracker. Keep the
  # pinned upstream fallback for deployments that still select the external
  # jailer. The chroot launcher deliberately needs no jailer binary.
  if [ "$chroot_jailer" != true ]; then
    if ! fetch_checked \
      "$artifact_base/$release/jailer-$artifact_arch" \
      "$jailer_sha256" \
      "$jailer"; then
      fetch_upstream_jailer "$jailer"
    fi
    chmod 0755 "$jailer"
  fi
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
fi

if [ "$mode" = prepare ]; then
  echo "VM assets and trusted templates are prepared"
  exit 0
fi

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

role_args=(
  --key-dir "$key_dir"
  --require-keys
  --users /etc/sparkbox/users/users.conf
  --cluster-id "$cluster_id"
)
jail_args=(--jailer "$jailer")
if [ "$chroot_jailer" = true ]; then
  jail_args=(--chroot-jailer)
fi
if [ "$disable_host_rootfs_mounts" = true ]; then
  jail_args+=(--disable-host-rootfs-mounts)
fi
metrics_addr=
if [ -n "$gateway_addr" ]; then
  role_args=(
    --gateway "$gateway_addr"
    --gateway-host-key /run/sparkbox/trust/gateway_host_key.pub
    --key-dir "$node_key_dir"
    --node-control-transport ssh
  )
  metrics_addr=:9090
  echo "starting private Sparkbox VM node $node_name linked to $gateway_addr"
else
  echo "starting legacy combined Sparkbox gateway for *.$proxy_domain (TLS: $proxy_tls)"
fi

exec /usr/local/bin/sparkbox serve \
  --driver firecracker \
  --state-dir "$control_dir" \
  --vm-state-dir "$hot_dir" \
  --checkpoint-dir "$durable_dir" \
  --checkpoint-prefix checkpoints \
  --kernel "$kernel" \
  --image-dir "$image_dir" \
  "${jail_args[@]}" \
  --guest-subnet "$guest_subnet" \
  --ssh-addr :2222 \
  --ssh-advertise-host "$ssh_advertise_host" \
  --ssh-advertise-port 22 \
  --api-addr 127.0.0.1:8080 \
  --metrics-addr "$metrics_addr" \
  --proxy-addr :8081 \
  --proxy-advertise-port "$proxy_advertise_port" \
  --proxy-domain "$proxy_domain" \
  --node-name "$node_name" \
  --host-mem-mb "$host_mem_mb" \
  --mem-admission-pct 80 \
  --max-running-per-owner 2 \
  "${role_args[@]}" \
  "${tls_args[@]}" \
  "$@"
