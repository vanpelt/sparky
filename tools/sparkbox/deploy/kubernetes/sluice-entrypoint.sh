#!/usr/bin/env bash
# Run the node-local DNS allow-list and TC/eBPF enforcer without granting it
# access to KVM, TUN, guest disks, or the VMM helper socket.
set -euo pipefail

readonly dns_ip="${SPARKBOX_SLUICE_DNS_IP:-172.30.0.53}"
readonly socket="${SPARKBOX_SLUICE_SOCKET:-/run/sluice/sluice.sock}"
readonly allowlist="${SPARKBOX_SLUICE_ALLOWLIST:-/etc/sparkbox/sluice-allowlist.txt}"

case "$dns_ip" in
	*[!0-9.]*|'')
		echo "SPARKBOX_SLUICE_DNS_IP must be an IPv4 address" >&2
		exit 2
		;;
esac
[ -r "$allowlist" ] || { echo "sluice allowlist is not readable: $allowlist" >&2; exit 1; }

# The VMM helper owns Pod-network mutation and creates this address before it
# opens its own socket. Containers start concurrently, so wait for that fixed
# resolver route rather than giving Sluice NET_ADMIN for ordinary link setup.
for _ in $(seq 1 60); do
	if ip -4 addr show | grep -F " $dns_ip/32 " >/dev/null; then
		break
	fi
	sleep 1
done
ip -4 addr show | grep -F " $dns_ip/32 " >/dev/null || {
	echo "sluice resolver address $dns_ip was not created by the VMM helper" >&2
	exit 1
}

rm -f "$socket"
exec /usr/local/bin/sluice run \
	--allowlist "$allowlist" \
	--dns-listen "$dns_ip:53" \
	--tap-prefix sbtap \
	--allow-ip "$dns_ip" \
	--enforce \
	--report-interval 60s \
	--api-listen "$socket"
