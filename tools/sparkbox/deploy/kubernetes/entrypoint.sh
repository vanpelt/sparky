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
readonly vm_state_dir="${SPARKBOX_VM_STATE_DIR:-$hot_dir}"
readonly key_dir="${SPARKBOX_KEY_DIR:-/run/sparkbox/keys}"
readonly durable_dir="${SPARKBOX_DURABLE_DIR:-/mnt/sparkbox-durable}"
readonly release="${SPARKBOX_RELEASE:-v0.5.3}"
readonly artifact_base="${SPARKBOX_ARTIFACT_BASE:-https://github.com/vanpelt/sparky/releases/download}"
readonly proxy_domain="${SPARKBOX_PROXY_DOMAIN:?SPARKBOX_PROXY_DOMAIN is required}"
readonly guest_subnet="${SPARKBOX_GUEST_SUBNET:-172.30.0.0/20}"
readonly host_mem_mb="${SPARKBOX_HOST_MEM_MB:-22000}"
readonly mem_admission_pct="${SPARKBOX_MEM_ADMISSION_PCT:-80}"
readonly max_running_per_owner="${SPARKBOX_MAX_RUNNING_PER_OWNER:-2}"
readonly max_sandboxes_per_owner="${SPARKBOX_MAX_SANDBOXES_PER_OWNER:-0}"
readonly mem_reserve_mb="${SPARKBOX_MEM_RESERVE_MB:-0}"
readonly owner_memory_pool_mb="${SPARKBOX_OWNER_MEMORY_POOL_MB:-0}"
readonly owner_memory_burst_mb="${SPARKBOX_OWNER_MEMORY_BURST_MB:-0}"
readonly disk_pool_mb_per_owner="${SPARKBOX_DISK_POOL_MB_PER_OWNER:-0}"
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
readonly privileged_helper_socket="${SPARKBOX_PRIVILEGED_HELPER_SOCKET:-}"
readonly sluice_socket="${SPARKBOX_SLUICE_SOCKET:-}"
readonly guest_dns="${SPARKBOX_GUEST_DNS:-}"
readonly controller_uid="${SPARKBOX_CONTROLLER_UID:-65532}"
readonly controller_gid="${SPARKBOX_CONTROLLER_GID:-65532}"
case "$mem_admission_pct" in
  ''|*[!0-9]*) echo "SPARKBOX_MEM_ADMISSION_PCT must be an integer from 0 to 100" >&2; exit 2 ;;
esac
if [ "$mem_admission_pct" -gt 100 ]; then
  echo "SPARKBOX_MEM_ADMISSION_PCT must be an integer from 0 to 100" >&2
  exit 2
fi
case "$max_running_per_owner" in
  ''|*[!0-9]*) echo "SPARKBOX_MAX_RUNNING_PER_OWNER must be a non-negative integer" >&2; exit 2 ;;
esac
validate_nonnegative() {
  local name=$1 value=$2
  case "$value" in
    ''|*[!0-9]*) echo "$name must be a non-negative integer" >&2; exit 2 ;;
  esac
}
validate_nonnegative SPARKBOX_MEM_RESERVE_MB "$mem_reserve_mb"
validate_nonnegative SPARKBOX_MAX_SANDBOXES_PER_OWNER "$max_sandboxes_per_owner"
validate_nonnegative SPARKBOX_OWNER_MEMORY_POOL_MB "$owner_memory_pool_mb"
validate_nonnegative SPARKBOX_OWNER_MEMORY_BURST_MB "$owner_memory_burst_mb"
validate_nonnegative SPARKBOX_DISK_POOL_MB_PER_OWNER "$disk_pool_mb_per_owner"
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
	"$vm_state_dir" "$durable_dir/checkpoints" "$node_key_dir"

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
	if [ -z "$privileged_helper_socket" ]; then
		if [ ! -c /dev/kvm ]; then
			echo "/dev/kvm is not a character device; check the sparkbox.dev/kvm allocation" >&2
			exit 1
		fi
		if [ ! -c /dev/net/tun ]; then
			echo "/dev/net/tun is not a character device; check the sparkbox.dev/tun allocation" >&2
			exit 1
		fi
	fi
fi

# Sparkbox clones a large sparse ext4 template for every VM. Reflinks make that
# operation instant and avoid consuming the template's full logical size.
reflink_source="$vm_state_dir/.reflink-source"
reflink_copy="$vm_state_dir/.reflink-copy"
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

  # The controller has to READ this template: a fresh sandbox is a
  # `cp --reflink=always` of it, and that cp runs as the unprivileged
  # controller, not as root.
  #
  # It does not inherit a usable mode on its own. zstd preserves the mode of
  # the file it decompressed, and the artifact fetched above is 0600 root:root,
  # so the template came out unreadable by uid $controller_uid and EVERY create
  # failed with a permission denied — while resumes kept working, because an
  # existing VM already has its own disk and never touches this file. Group
  # read rather than 0644: the controller is the only thing that needs it.
  #
  # Applied on every prepare rather than only after a decompress, because the
  # deployments that need it most are the ones whose template was written by an
  # older image and is sitting there at 0600 right now.
  chgrp "$controller_gid" "$rootfs"
  chmod 0640 "$rootfs"
fi

if [ "$mode" = prepare ]; then
	# The helper split gives the controller a writable subdirectory rather than
	# the whole hot tier. Migrate the pre-split layout once while no VMM exists.
	if [ "$vm_state_dir" != "$hot_dir" ] && [ -d "$hot_dir/fc-vms" ]; then
		if [ -e "$vm_state_dir/fc-vms" ]; then
			echo "both legacy and helper VM-state directories exist; refusing an ambiguous migration" >&2
			exit 1
		fi
		mv "$hot_dir/fc-vms" "$vm_state_dir/fc-vms"
	fi
	# The runtime controller is deliberately non-root. Preparation is the only
	# point at which stale high-UID VM files from an earlier deployment are safe
	# to return to the controller: no Firecracker process exists yet.
	chown -R "$controller_uid:$controller_gid" \
		"$control_dir" "$vm_state_dir" "$node_key_dir" "$durable_dir"
	mkdir -p "$hot_dir/jailer"
	chown 0:0 "$hot_dir" "$hot_dir/jailer"
	chmod 0755 "$hot_dir"
	# The unprivileged controller needs search-only access to the fixed path of
	# each helper-published Firecracker API socket. It cannot list this directory;
	# per-VM jail roots further restrict traversal to the controller group.
	chmod 0711 "$hot_dir/jailer"
	echo "VM assets and trusted templates are prepared"
	exit 0
fi

export PATH="$asset_dir:$PATH"
if [ -n "$privileged_helper_socket" ]; then
	# Containers start concurrently after init. Wait for the capability-bearing
	# sidecar to authenticate this UID and finish Pod-network setup.
	for _ in $(seq 1 60); do
		if /usr/local/bin/sparkbox-vmm-helper ping --socket "$privileged_helper_socket" >/dev/null 2>&1; then
			break
		fi
		sleep 1
	done
	/usr/local/bin/sparkbox-vmm-helper ping --socket "$privileged_helper_socket"
else
	# Legacy combined process: these affect only the Pod network namespace.
	sysctl -q -w net.ipv4.ip_forward=1
	sysctl -q -w net.ipv6.conf.all.forwarding=1
	sysctl -q -w net.ipv4.conf.all.rp_filter=1
	sysctl -q -w net.ipv4.conf.default.rp_filter=1
	export SPARKBOX_EDGE_REDIRECT=0
	export SPARKBOX_GUEST_SUBNET="$guest_subnet"
	/usr/local/sbin/sparkbox-net.sh
fi

sluice_args=()
if [ -n "$sluice_socket" ]; then
	# A node configured for filtering must never start in an accidentally open
	# state. Sluice creates its socket only after the eBPF collection loaded, so
	# a successful API probe proves both the control plane and enforcement plane
	# were initialized before Sparkbox accepts a VM operation.
	for _ in $(seq 1 60); do
		if curl --fail --silent --show-error --unix-socket "$sluice_socket" \
			http://sluice/report.json >/dev/null 2>&1; then
			break
		fi
		sleep 1
	done
	curl --fail --silent --show-error --unix-socket "$sluice_socket" \
		http://sluice/report.json >/dev/null
	sluice_args+=(--sluice-socket "$sluice_socket")
fi
if [ -n "$guest_dns" ]; then
	sluice_args+=(--guest-dns "$guest_dns")
fi

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
	jail_args=(--chroot-jailer --jailer-chroot-base "$hot_dir/jailer")
fi
if [ "$disable_host_rootfs_mounts" = true ]; then
	jail_args+=(--disable-host-rootfs-mounts)
fi
if [ -n "$privileged_helper_socket" ]; then
	jail_args+=(
		--privileged-helper-socket "$privileged_helper_socket"
		--privileged-helper-bin /usr/local/bin/sparkbox-vmm-helper
		--helper-controller-gid "$controller_gid"
	)
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
	--vm-state-dir "$vm_state_dir" \
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
  --mem-admission-pct "$mem_admission_pct" \
  --max-running-per-owner "$max_running_per_owner" \
  --max-sandboxes-per-owner "$max_sandboxes_per_owner" \
  --mem-reserve-mb "$mem_reserve_mb" \
  --owner-memory-pool-mb "$owner_memory_pool_mb" \
  --owner-memory-burst-mb "$owner_memory_burst_mb" \
  --disk-pool-mb-per-owner "$disk_pool_mb_per_owner" \
  "${sluice_args[@]}" \
  "${role_args[@]}" \
  "${tls_args[@]}" \
  "$@"
