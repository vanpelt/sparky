#!/usr/bin/env bash
# Own the small set of Linux operations the CKS controller cannot perform as a
# capability-free UID: Pod-network TAPs, per-slot chroots, and VMM launch.
set -euo pipefail

readonly data_dir="${SPARKBOX_DATA_DIR:-/var/lib/sparkbox}"
readonly asset_dir="$data_dir/assets"
readonly hot_dir="$data_dir/hot"
readonly vm_state_dir="${SPARKBOX_VM_STATE_DIR:-$hot_dir/controller}"
readonly guest_subnet="${SPARKBOX_GUEST_SUBNET:-172.30.0.0/20}"
readonly guest_subnet6="${SPARKBOX_GUEST_SUBNET6:-}"
readonly helper_socket="${SPARKBOX_PRIVILEGED_HELPER_SOCKET:-/run/sparkbox-vmm/helper.sock}"
readonly controller_uid="${SPARKBOX_CONTROLLER_UID:-65532}"
readonly controller_gid="${SPARKBOX_CONTROLLER_GID:-65532}"

for device in /dev/kvm /dev/net/tun; do
	if [ ! -c "$device" ]; then
		echo "$device is not a character device; check the device-plugin allocation" >&2
		exit 1
	fi
done

# CKS exposes these sysctls read-only on some nodes. The existing values are
# sufficient in the POC; per-TAP configuration remains best-effort in the
# helper. Packet-filter setup, which does require NET_ADMIN, is authoritative.
sysctl -q -w net.ipv4.ip_forward=1 || true
sysctl -q -w net.ipv6.conf.all.forwarding=1 || true
sysctl -q -w net.ipv4.conf.all.rp_filter=1 || true
sysctl -q -w net.ipv4.conf.default.rp_filter=1 || true

export SPARKBOX_EDGE_REDIRECT=0
export SPARKBOX_GUEST_SUBNET="$guest_subnet"
/usr/local/sbin/sparkbox-net.sh

exec /usr/local/bin/sparkbox-vmm-helper serve \
	--socket "$helper_socket" \
	--firecracker "$asset_dir/firecracker" \
	--kernel "$asset_dir/vmlinux" \
	--vm-state-dir "$vm_state_dir" \
	--chroot-base "$hot_dir/jailer" \
	--subnet "$guest_subnet" \
	--subnet6 "$guest_subnet6" \
	--jailer-uid-base 100000 \
	--controller-uid "$controller_uid" \
	--controller-gid "$controller_gid"
