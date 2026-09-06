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
readonly restrict_internal_egress="${SPARKBOX_RESTRICT_INTERNAL_EGRESS:-1}"
readonly sluice_dns_ip="${SPARKBOX_SLUICE_DNS_IP:-}"
readonly sluice_socket="${SPARKBOX_SLUICE_SOCKET:-}"
readonly helper_socket="${SPARKBOX_PRIVILEGED_HELPER_SOCKET:-/run/sparkbox-vmm/helper.sock}"
readonly controller_uid="${SPARKBOX_CONTROLLER_UID:-65532}"
readonly controller_gid="${SPARKBOX_CONTROLLER_GID:-65532}"
# The VMM this helper launches, for its whole life. It reads the SAME variable
# the controller container reads, and that is the mechanism keeping them in
# step: a helper serving firecracker answers the controller's startup ping
# perfectly well, so a mismatch would first show up as the first launch being
# refused. There is deliberately nothing in the helper protocol to ask with --
# the backend is server-side only, because a field letting the unprivileged
# controller pick which binary root executes would not be a boundary.
readonly vmm_driver="${SPARKBOX_DRIVER:-firecracker}"
readonly qemu_machine_type="${SPARKBOX_QEMU_MACHINE_TYPE:-}"

case "$vmm_driver" in
	firecracker|qemu) ;;
	*) echo "SPARKBOX_DRIVER must be firecracker or qemu, got: $vmm_driver" >&2; exit 1 ;;
esac

case "$restrict_internal_egress" in
	0|1) ;;
	*)
		echo "SPARKBOX_RESTRICT_INTERNAL_EGRESS must be 0 or 1" >&2
		exit 1
		;;
esac

# QEMU is baked into the image (the Containerfile installs the per-arch
# qemu-system package) and named for the machine, not for Go's GOARCH:
# qemu-system-x86_64 and qemu-system-aarch64, which is what uname -m gives.
qemu_bin=
if [ "$vmm_driver" = qemu ]; then
	qemu_bin="$(command -v "qemu-system-$(uname -m)")" || {
		echo "SPARKBOX_DRIVER=qemu but qemu-system-$(uname -m) is not in PATH" >&2
		exit 1
	}
fi
readonly qemu_bin

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
export SPARKBOX_GUEST_SUBNET6="$guest_subnet6"
export SPARKBOX_RESTRICT_INTERNAL_EGRESS="$restrict_internal_egress"
export SLUICE_DNS_IP="$sluice_dns_ip"
/usr/local/sbin/sparkbox-net.sh

helper_args=(
	serve
	--socket "$helper_socket"
	--backend "$vmm_driver"
	--firecracker "$asset_dir/firecracker"
	--kernel "$asset_dir/vmlinux"
	--vm-state-dir "$vm_state_dir"
	--chroot-base "$hot_dir/jailer"
	--subnet "$guest_subnet"
	--subnet6 "$guest_subnet6"
	--jailer-uid-base 100000
	--controller-uid "$controller_uid"
	--controller-gid "$controller_gid"
)
if [ "$vmm_driver" = qemu ]; then
	helper_args+=(--qemu-bin "$qemu_bin")
	# Passed only when the operator set one. Left empty, both this process and
	# the controller take the same pinned per-arch default from the one place it
	# is written down (internal/vmm/qemuargs), which is what stops the two from
	# describing different machines to the same migration stream.
	if [ -n "$qemu_machine_type" ]; then
		helper_args+=(--machine-type "$qemu_machine_type")
	fi
fi
if [ "$restrict_internal_egress" = 1 ]; then
	helper_args+=(--restrict-internal-egress)
fi
if [ -n "$sluice_socket" ]; then
	helper_args+=(--sluice-socket "$sluice_socket")
fi
exec /usr/local/bin/sparkbox-vmm-helper "${helper_args[@]}"
