# Dedicated edge IP cutover (/32) — plan + rollback

## Why

The edge shares the host's tailnet IP (`100.65.150.80`), so the any-port REDIRECT
range must exclude every host-stack service (sshd `:22`, gateway `:2222`, DNS `:53`,
edge `:443`, openclaw `:8443`) or hijack it — a standing collision hazard that grows
with every new host service. Give the edge its **own IP** and scope the REDIRECT to
that IP: host services on the main IP become untouchable, and the excludes go away.

Chosen: a subnet-routed **`/32`** on a dummy interface (one `tailscaled`, no
route/netfilter conflict, cannot affect the host's main tailnet IP or sshd — so the
operator's SSH lifeline is safe during cutover). Rejected: a second `tailscaled`
node (route conflict over `100.64.0.0/10` in the root netns; needs a network
namespace to isolate — a future path if bare `:22` `ssh <name>@catnip.sh` is wanted).

## Target

`EDGE_IP = 10.66.0.1` (private, dummy iface `sparkedge`). Everything the edge serves
binds it: proxy `:443` (TLS), DNS `:53`, gateway `:2222`. cloudflared and split DNS
point at it. Peers reach it via the `/32` subnet route (host is the router).

## Steps

1. **Dummy IP** (host): `ip link add sparkedge type dummy; ip addr add 10.66.0.1/32
   dev sparkedge; ip link set sparkedge up`. Persist via a tiny unit or
   `sparkbox-net.sh`.
2. **Advertise + approve**: `tailscale set --advertise-routes=10.66.0.1/32`; approve
   via the API (`POST /device/{id}/routes {"routes":["10.66.0.1/32"]}`).
3. **Clients**: `tailscale set --accept-routes=true` (document for other members).
4. **Rebind edge** (`/etc/sparkbox/sparkbox.env`): `SPARKBOX_PROXY_ADDR=10.66.0.1:443`,
   `SPARKBOX_SSH_ADDR=10.66.0.1:2222`, `SPARKBOX_DNS_ADDR=10.66.0.1:53`,
   `SPARKBOX_DNS_ANSWER=10.66.0.1`. Keep `--proxy-tls` (same cached wildcard cert —
   DNS-01, valid regardless of where the name points). Restart sparkbox.
5. **Any-port by dest IP** (`sparkbox-net.sh`): replace the interface-scoped
   `SPARKBOX_TNET` with `-d $EDGE_IP -p tcp --dport <lo:hi> -j DNAT --to-destination
   $EDGE_IP:443`. Drop `SPARKBOX_TAILNET_IF`; add `SPARKBOX_EDGE_IP=10.66.0.1`.
   - **Gotcha (why DNAT, not REDIRECT):** `REDIRECT` rewrites the destination to the
     *incoming interface's primary IP* — here `tailscale0` = the host's tailnet IP,
     **not** the edge `/32` — so redirected any-port traffic lands on a dead
     `100.65.150.80:443`. `DNAT --to-destination $EDGE_IP:443` targets the edge
     explicitly. `SO_ORIGINAL_DST` still recovers the dialed port (conntrack).
   - Only the edge IP's own in-range service (`:2222` gateway) needs sparing; host
     services live on other IPs and never match `-d $EDGE_IP`. `:443`/`:53` are
     below the range and pass through.
6. **cloudflared**: `service: https://10.66.0.1:443`, `originServerName: catnip.sh`.
   Restart. (Public path keeps working.)
7. **split DNS**: `PATCH /tailnet/-/dns/split-dns {"catnip.sh":["10.66.0.1"]}`.
8. **Optional**: openclaw's `tailscale serve` can move back to `:443` on the host
   node (now free); or leave it on `:8443`.

## Verify

- Host: edge/DNS/gateway listening on `10.66.0.1`; wildcard cert loads.
- Laptop (accept-routes on): `10.66.0.1/32` in the route table; `https://<name>.catnip.sh`
  and `:<port>` → 200 via `10.66.0.1`; `ssh -p2222 <name>@catnip.sh`.
- Public: `console.catnip.sh` still 200 through the tunnel.
- My `ssh sparky` (host IP `:22`) unaffected throughout.

## Rollback

Everything reverts to the working shared-IP edge:
1. `sparkbox.env` → `100.65.150.80` for proxy/ssh/dns (restore backup), restart sparkbox.
2. `sparkbox-net.sh`/`net.env` → interface-scoped `SPARKBOX_TNET` with excludes
   (`SPARKBOX_TAILNET_IF=tailscale0`), restart sparkbox-net.
3. cloudflared → `https://100.65.150.80:443` (restore backup), restart.
4. split DNS → `{"catnip.sh":["100.65.150.80"]}`.
5. `tailscale set --advertise-routes=` (withdraw); `ip link del sparkedge`.
Backups keyed by `/etc/sparkbox/.last-backup-ts` (unit/env/cloudflared).
