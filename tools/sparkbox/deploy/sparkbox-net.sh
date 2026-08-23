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
SPARKBOX_GUEST_SUBNET="${SPARKBOX_GUEST_SUBNET:-172.30.0.0/16}"
SPARKBOX_GUEST_SUBNET6="${SPARKBOX_GUEST_SUBNET6:-}"
SPARKBOX_RESTRICT_INTERNAL_EGRESS="${SPARKBOX_RESTRICT_INTERNAL_EGRESS:-0}"
UPLINK=$(ip route | awk '/default/{print $5; exit}')
if [ -n "$UPLINK" ]; then
  iptables -t nat -C POSTROUTING -s "$SPARKBOX_GUEST_SUBNET" -o "$UPLINK" -j MASQUERADE 2>/dev/null || \
    iptables -t nat -A POSTROUTING -s "$SPARKBOX_GUEST_SUBNET" -o "$UPLINK" -j MASQUERADE
else
  echo "WARN: no default route — skipping sandbox NAT" >&2
fi

# The guest metadata service (identity tokens) binds every interface, because
# taps come and go with sandboxes and it must answer on each of them. It refuses
# any caller that isn't a sandbox talking to its own gateway, but don't rely on
# that alone: only sandbox taps may reach the port at all.
iptables -C INPUT -p tcp --dport 8967 ! -i sbtap+ -j DROP 2>/dev/null || \
  iptables -I INPUT -p tcp --dport 8967 ! -i sbtap+ -j DROP

# Sandbox forwarding. CKS enables the restricted mode, which provides a
# fail-closed inner boundary in the Pod network namespace. It composes with
# sluice rather than replacing it: sluice's TC ingress program first narrows a
# tagged TAP to DNS-derived addresses, then these rules reject host, private,
# link-local, documentation, multicast, and reserved destinations regardless
# of what DNS returned. Untagged/open sluice TAPs still inherit this ceiling.
#
# Rebuild named chains so a sidecar restart cannot accumulate stale policy.
# Remove both the legacy blanket accepts and known hooks before inserting the
# selected policy. `sbtap+` is iptables' interface-prefix syntax.
while iptables -D FORWARD -i sbtap+ -j ACCEPT 2>/dev/null; do :; done
while iptables -D FORWARD -o sbtap+ -j ACCEPT 2>/dev/null; do :; done
while iptables -D FORWARD -i sbtap+ -j SPARKBOX_GUEST_OUT 2>/dev/null; do :; done
while iptables -D FORWARD -o sbtap+ -j SPARKBOX_GUEST_IN 2>/dev/null; do :; done
while iptables -D INPUT -i sbtap+ -j SPARKBOX_GUEST_HOST 2>/dev/null; do :; done

if [ "$SPARKBOX_RESTRICT_INTERNAL_EGRESS" = 1 ]; then
  iptables -N SPARKBOX_GUEST_OUT 2>/dev/null || iptables -F SPARKBOX_GUEST_OUT
  iptables -N SPARKBOX_GUEST_IN 2>/dev/null || iptables -F SPARKBOX_GUEST_IN
  iptables -N SPARKBOX_GUEST_HOST 2>/dev/null || iptables -F SPARKBOX_GUEST_HOST

  # A broad subnet check is present from helper startup. The helper inserts a
  # stricter per-TAP /32 source rule when it creates each device, preventing a
  # guest from spoofing a sibling's address.
  iptables -A SPARKBOX_GUEST_OUT ! -s "$SPARKBOX_GUEST_SUBNET" -j DROP
  for cidr in \
    0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 \
    169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.0.2.0/24 \
    192.88.99.0/24 192.168.0.0/16 198.18.0.0/15 198.51.100.0/24 \
    203.0.113.0/24 224.0.0.0/4 240.0.0.0/4; do
    iptables -A SPARKBOX_GUEST_OUT -d "$cidr" -j DROP
  done
  iptables -A SPARKBOX_GUEST_OUT -j ACCEPT

  # No unsolicited network traffic may enter a guest, but replies to a
  # guest-originated connection are allowed back through the TAP.
  iptables -A SPARKBOX_GUEST_IN -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  iptables -A SPARKBOX_GUEST_IN -j DROP

  # Controller-originated guest SSH uses host OUTPUT and is unaffected. Its
  # replies arrive through INPUT, so established traffic must precede the host
  # service allow-list. DNS supports a future node-local sluice resolver; 8967
  # is Sparkbox's authenticated guest metadata service.
  iptables -A SPARKBOX_GUEST_HOST -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  iptables -A SPARKBOX_GUEST_HOST -p udp --dport 53 -j ACCEPT
  iptables -A SPARKBOX_GUEST_HOST -p tcp --dport 53 -j ACCEPT
  iptables -A SPARKBOX_GUEST_HOST -p tcp --dport 8967 -j ACCEPT
  iptables -A SPARKBOX_GUEST_HOST -j DROP

  iptables -I FORWARD 1 -i sbtap+ -j SPARKBOX_GUEST_OUT
  iptables -I FORWARD 2 -o sbtap+ -j SPARKBOX_GUEST_IN
  iptables -I INPUT 1 -i sbtap+ -j SPARKBOX_GUEST_HOST

  if [ -n "$SPARKBOX_GUEST_SUBNET6" ]; then
    while ip6tables -D FORWARD -i sbtap+ -j SPARKBOX_GUEST_OUT 2>/dev/null; do :; done
    while ip6tables -D FORWARD -o sbtap+ -j SPARKBOX_GUEST_IN 2>/dev/null; do :; done
    while ip6tables -D INPUT -i sbtap+ -j SPARKBOX_GUEST_HOST 2>/dev/null; do :; done
    ip6tables -N SPARKBOX_GUEST_OUT 2>/dev/null || ip6tables -F SPARKBOX_GUEST_OUT
    ip6tables -N SPARKBOX_GUEST_IN 2>/dev/null || ip6tables -F SPARKBOX_GUEST_IN
    ip6tables -N SPARKBOX_GUEST_HOST 2>/dev/null || ip6tables -F SPARKBOX_GUEST_HOST
    ip6tables -A SPARKBOX_GUEST_OUT ! -s "$SPARKBOX_GUEST_SUBNET6" -j DROP
    for cidr in \
      ::/128 ::1/128 ::ffff:0:0/96 64:ff9b:1::/48 100::/64 \
      2001:db8::/32 fc00::/7 fe80::/10 ff00::/8; do
      ip6tables -A SPARKBOX_GUEST_OUT -d "$cidr" -j DROP
    done
    ip6tables -A SPARKBOX_GUEST_OUT -j ACCEPT
    ip6tables -A SPARKBOX_GUEST_IN -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    ip6tables -A SPARKBOX_GUEST_IN -j DROP
    ip6tables -A SPARKBOX_GUEST_HOST -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    ip6tables -A SPARKBOX_GUEST_HOST -p udp --dport 53 -j ACCEPT
    ip6tables -A SPARKBOX_GUEST_HOST -p tcp --dport 53 -j ACCEPT
    ip6tables -A SPARKBOX_GUEST_HOST -p tcp --dport 8967 -j ACCEPT
    ip6tables -A SPARKBOX_GUEST_HOST -j DROP
    ip6tables -I FORWARD 1 -i sbtap+ -j SPARKBOX_GUEST_OUT
    ip6tables -I FORWARD 2 -o sbtap+ -j SPARKBOX_GUEST_IN
    ip6tables -I INPUT 1 -i sbtap+ -j SPARKBOX_GUEST_HOST
  fi
else
  # Standalone installations retain their existing behavior. On a host that
  # also runs Docker, its FORWARD policy is commonly DROP, so explicit accepts
  # remain necessary for sandbox egress.
  iptables -I FORWARD 1 -i sbtap+ -j ACCEPT
  iptables -I FORWARD 2 -o sbtap+ -j ACCEPT
fi

# sluice's allowlist resolver needs an address of its OWN on any host that also
# runs sparkbox's wildcard DNS responder: they are two different DNS servers and
# only one can hold :53. `sparkbox setup --sluice-dns-addr 172.30.0.53:53` is the
# documented answer and it writes SLUICE_DNS_IP here — because RECOMMENDING an
# address is not the same as creating one, and an address no interface holds is
# not bindable. Without this block sluice failed with "bind: cannot assign
# requested address", its Restart=always unit looped forever, every guest handed
# that literal as --guest-dns had no resolver at all, and `sparkbox setup` still
# printed a clean report (the port preflight tolerates EADDRNOTAVAIL by design,
# and `enable --now` on a Type=simple unit returns 0 at the fork).
#
# A dummy interface, exactly like the edge's /32 below: host-local, reached by
# guests across their tap through the host's own routing, and free. Recreated
# every boot, because dummy links do not survive one. Skipped when the address is
# already on some interface — the operator may have put it somewhere real, and a
# second copy on a dummy would shadow it.
SLUICE_DNS_IP="${SLUICE_DNS_IP:-}"
if [ -n "$SLUICE_DNS_IP" ]; then
  # Captured rather than `| grep -q`: this script runs under `set -o pipefail`,
  # and a `grep -q` that exits on its first match can SIGPIPE the producers, so
  # the pipeline would report 141 exactly when the address WAS found — i.e. the
  # check would invert itself on a host with enough interfaces to fill the pipe
  # buffer, and then `ip addr add` would fail on the duplicate and `set -e`
  # would abort the whole packet filter at boot.
  HAVE_DNS_IP=$(ip -4 -o addr show 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | grep -Fx "$SLUICE_DNS_IP" || true)
  if [ -n "$HAVE_DNS_IP" ]; then
    echo "SLUICE_DNS_IP=$SLUICE_DNS_IP is already on this host — leaving it where it is" >&2
  else
    ip link show sparkdns >/dev/null 2>&1 || ip link add sparkdns type dummy
    ip addr add "$SLUICE_DNS_IP/32" dev sparkdns
  fi
  # Unconditionally up: an interface that exists but is DOWN has no local route,
  # so the bind fails with the same EADDRNOTAVAIL the block above exists to stop.
  if ip link show sparkdns >/dev/null 2>&1; then
    ip link set sparkdns up
  fi
fi

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
    # Bare ssh on the edge IP: the edge owns this /32, so :22 can be the SSH
    # gateway without touching host sshd (which answers on the host's IPs, not
    # here). DNAT rather than a gateway bind: sshd's 0.0.0.0:22 wildcard makes a
    # specific-IP :22 bind EADDRINUSE. :22 is below TNET_LO, so without this rule
    # it would fall through to the host stack.
    GW_PORT="${SPARKBOX_GATEWAY_PORT:-2222}"
    iptables -t nat -A SPARKBOX_TNET -p tcp --dport 22 -j DNAT --to-destination "$EDGE_IP:$GW_PORT"
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
