#!/usr/bin/env bash
# Host packet-filter rules for sparkbox. Idempotent, and run on EVERY boot by
# sparkbox-net.service (ordered before sparkbox.service).
#
# Why a boot unit and not cloud-init: cloud-init's runcmd is per-INSTANCE, not
# per-boot, so rules applied there silently disappear on the first reboot —
# taking sandbox egress with them. Why not iptables-persistent: a saved rules
# file freezes the uplink interface name at provision time, whereas this
# re-derives it, so the rules survive a NIC rename or a netplan change.
#
# Sysctls are NOT set here: /etc/sysctl.d/99-sparkbox.conf already persists them
# and is applied earlier in boot than any unit could be.
set -euo pipefail

# NAT for IPv4 sandbox egress. IPv6 needs none: with --subnet6 each sandbox
# holds a globally routable /128 and egresses unmasqueraded.
UPLINK=$(ip route | awk '/default/{print $5; exit}')
if [ -n "$UPLINK" ]; then
  iptables -t nat -C POSTROUTING -s 172.30.0.0/16 -o "$UPLINK" -j MASQUERADE 2>/dev/null || \
    iptables -t nat -A POSTROUTING -s 172.30.0.0/16 -o "$UPLINK" -j MASQUERADE
else
  echo "WARN: no default route — skipping sandbox NAT" >&2
fi

# The guest metadata service (identity tokens) binds every interface, because
# taps come and go with sandboxes and it must answer on each of them. It refuses
# any caller that isn't a sandbox talking to its own gateway, but don't rely on
# that alone: only sandbox taps may reach the port at all.
iptables -C INPUT -p tcp --dport 8967 ! -i sbtap+ -j DROP 2>/dev/null || \
  iptables -I INPUT -p tcp --dport 8967 ! -i sbtap+ -j DROP
