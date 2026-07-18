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

# Any-port forwarding over the TAILNET. The uplink REDIRECT above is off in
# tunnel mode, but tailnet members dial the edge IP directly over WireGuard, so
# https://<name>.<domain>:<PORT> works once the edge IP's <PORT> is REDIRECTed to
# the edge listener (the edge recovers the original port via SO_ORIGINAL_DST).
# Scoped to the tailscale interface. Caveat: the edge IP is the HOST's OWN tailnet
# IP, so host-stack services reachable over the tailnet — sshd :22, the gateway
# :2222, the DNS responder :53, the edge :443 itself, and any co-tenant such as an
# openclaw `tailscale serve` — MUST be excluded or a dial to them is hijacked into
# the web edge. Getting :22/:2222 wrong locks the operator out, so the defaults
# below are deliberately protective; extend via SPARKBOX_TAILNET_EXCLUDE. Enable
# by setting SPARKBOX_TAILNET_IF (e.g. tailscale0); empty disables it.
TNET_PORT="${PROXY_PORT:-443}"
TNET_LO="${PROXY_REDIRECT_LO:-1024}"
TNET_HI="${PROXY_REDIRECT_HI:-65535}"
EDGE_IP="${SPARKBOX_EDGE_IP:-}"       # edge has its OWN /32 -> match by dest, no excludes (preferred)
TNET_IF="${SPARKBOX_TAILNET_IF:-}"    # edge shares the host tailnet IP -> match by iface, exclude host ports
if [ -n "$EDGE_IP" ]; then
  # Give the edge its own address on a dummy iface (a subnet-routed /32 advertised
  # to the tailnet). Boot-persistent: recreated here every boot.
  ip link show sparkedge >/dev/null 2>&1 || ip link add sparkedge type dummy
  ip addr show dev sparkedge 2>/dev/null | grep -q "$EDGE_IP/32" || ip addr add "$EDGE_IP/32" dev sparkedge
  ip link set sparkedge up
fi
if [ -n "$EDGE_IP" ] || [ -n "$TNET_IF" ]; then
  # Rebuild from scratch each run so range/excludes/edge-port stay in sync.
  iptables -t nat -N SPARKBOX_TNET 2>/dev/null || iptables -t nat -F SPARKBOX_TNET
  if [ -n "$EDGE_IP" ]; then
    # Dest-scoped: host services live on other IPs and never match -d EDGE_IP, so the
    # only ports to spare are the edge IP's OWN in-range services (the SSH gateway).
    # :443 and :53 are below the range and pass through untouched.
    EXC="${SPARKBOX_TAILNET_EXCLUDE:-2222}"
  else
    # Interface-scoped (shared host IP): protect every host-stack port on the tailnet IP.
    EXC="${SPARKBOX_TAILNET_EXCLUDE:-22 2222 53 8443} $TNET_PORT"
  fi
  for p in $EXC; do
    iptables -t nat -A SPARKBOX_TNET -p tcp --dport "$p" -j RETURN
  done
  if [ -n "$EDGE_IP" ]; then
    # DNAT to the edge's own IP explicitly. REDIRECT can't be used here: it rewrites
    # the destination to the INCOMING interface's primary IP (the host's tailnet IP),
    # not the edge /32, so redirected traffic would miss the edge entirely.
    iptables -t nat -A SPARKBOX_TNET -p tcp --dport "$TNET_LO:$TNET_HI" -j DNAT --to-destination "$EDGE_IP:$TNET_PORT"
  else
    iptables -t nat -A SPARKBOX_TNET -p tcp --dport "$TNET_LO:$TNET_HI" -j REDIRECT --to-ports "$TNET_PORT"
  fi
  # Re-hook cleanly: drop ANY prior hook of either mode (loop — a live mode-switch can
  # leave a stale one), then install the current one at the top of PREROUTING.
  while iptables -t nat -D PREROUTING -i tailscale0 -p tcp -j SPARKBOX_TNET 2>/dev/null; do :; done
  if [ -n "$EDGE_IP" ]; then
    while iptables -t nat -D PREROUTING -d "$EDGE_IP" -p tcp -j SPARKBOX_TNET 2>/dev/null; do :; done
    iptables -t nat -I PREROUTING -d "$EDGE_IP" -p tcp -j SPARKBOX_TNET
    echo "tailnet any-port DNAT: dest $EDGE_IP -> :$TNET_PORT (spare: $EXC)" >&2
  else
    iptables -t nat -I PREROUTING -i "$TNET_IF" -p tcp -j SPARKBOX_TNET
    echo "tailnet any-port REDIRECT: iface $TNET_IF -> :$TNET_PORT (excludes: $EXC)" >&2
  fi
fi
