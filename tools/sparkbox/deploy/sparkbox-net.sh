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

# Sandbox forwarding. On a host that ALSO runs docker, dockerd sets the FORWARD
# policy to DROP and only whitelists its own bridges — so sandbox tap traffic is
# dropped and egress silently breaks. Explicitly accept sbtap+ in both
# directions, inserted at the top so it wins over docker's chain. Harmless on a
# non-docker host, where the default FORWARD policy is already ACCEPT.
iptables -C FORWARD -i sbtap+ -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -i sbtap+ -j ACCEPT
iptables -C FORWARD -o sbtap+ -j ACCEPT 2>/dev/null || iptables -I FORWARD 2 -o sbtap+ -j ACCEPT

# Any-port authenticated forwarding via an uplink REDIRECT is only for the
# direct-public-IP edge, where web traffic arrives ON the uplink. Behind a
# reverse tunnel (e.g. Cloudflare Tunnel) it arrives from localhost instead, so
# the REDIRECT is unnecessary — and on a shared/home box it would hijack ALL
# inbound uplink TCP in the range, clobbering unrelated LAN services. Set
# SPARKBOX_EDGE_REDIRECT=0 to skip it (tunnel mode).
if [ "${SPARKBOX_EDGE_REDIRECT:-1}" != 1 ]; then
  echo "SPARKBOX_EDGE_REDIRECT=0 — skipping uplink any-port REDIRECT (tunnel mode)" >&2
else

# Any-port authenticated forwarding: the edge listens on ONE port but a user can
# reach any guest port at https://<name>.<domain>:<PORT>. REDIRECT the whole
# private-port range on the public uplink to the edge listener; the edge recovers
# the original port via SO_ORIGINAL_DST and forwards to that guest port.
#
# Scoping matters: the redirect lives in a dedicated chain hooked only for
# traffic arriving on the uplink, so guest→gateway metadata (which arrives on
# sbtap+, not the uplink) is never caught. Admin sshd (:2222) and the edge port
# itself are excluded so we don't redirect them into the web edge.
PROXY_PORT="${PROXY_PORT:-443}"
PORT_LO="${PROXY_REDIRECT_LO:-1024}"
PORT_HI="${PROXY_REDIRECT_HI:-65535}"
EXCLUDE_PORTS="${PROXY_REDIRECT_EXCLUDE:-2222} $PROXY_PORT"
if [ -n "$UPLINK" ]; then
  # Rebuild the chain from scratch each run so it stays in sync with the config
  # (excludes, range, edge port) without accumulating stale rules.
  iptables -t nat -N SPARKBOX_EDGE 2>/dev/null || iptables -t nat -F SPARKBOX_EDGE
  for p in $EXCLUDE_PORTS; do
    iptables -t nat -A SPARKBOX_EDGE -p tcp --dport "$p" -j RETURN
  done
  iptables -t nat -A SPARKBOX_EDGE -p tcp --dport "$PORT_LO:$PORT_HI" -j REDIRECT --to-ports "$PROXY_PORT"
  iptables -t nat -C PREROUTING -i "$UPLINK" -p tcp -j SPARKBOX_EDGE 2>/dev/null || \
    iptables -t nat -I PREROUTING -i "$UPLINK" -p tcp -j SPARKBOX_EDGE

  # Same for IPv6 when the host has a routable prefix: web traffic to the
  # flexible v6 hits the same edge. Best-effort — skip if ip6tables nat is absent.
  if [ -n "${SUBNET6:-}" ] && ip6tables -t nat -L >/dev/null 2>&1; then
    ip6tables -t nat -N SPARKBOX_EDGE 2>/dev/null || ip6tables -t nat -F SPARKBOX_EDGE
    for p in $EXCLUDE_PORTS; do
      ip6tables -t nat -A SPARKBOX_EDGE -p tcp --dport "$p" -j RETURN
    done
    ip6tables -t nat -A SPARKBOX_EDGE -p tcp --dport "$PORT_LO:$PORT_HI" -j REDIRECT --to-ports "$PROXY_PORT"
    ip6tables -t nat -C PREROUTING -i "$UPLINK" -p tcp -j SPARKBOX_EDGE 2>/dev/null || \
      ip6tables -t nat -I PREROUTING -i "$UPLINK" -p tcp -j SPARKBOX_EDGE
  fi
else
  echo "WARN: no default route — skipping any-port REDIRECT" >&2
fi

fi  # SPARKBOX_EDGE_REDIRECT
