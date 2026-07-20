# VM Network Allowlisting & Per-Domain Metering

*Design for `tools/sluice`. Companion to `agentic-sandbox-design.md`. Goal:
let an operator allowlist the domains a sandbox VM may reach, log every domain a
VM accesses, and account bandwidth per domain — with no agent inside the guest
and no TLS interception.*

## The chokepoint we already own

Every sparkbox sandbox is a Firecracker microVM on a point-to-point tap: the
host is `172.30.<idx>.1` on `sbtap<idx>`, the guest is `172.30.<idx>.2`, and all
egress is `MASQUERADE`d through the uplink (`sparkbox/deploy/sparkbox-net.sh`).
The host is therefore the default gateway, the DNS path, and the NAT for every
guest at once. That single position is where we instrument — no in-guest agent,
nothing to install per workload.

## The visibility ceiling

Guest traffic is overwhelmingly TLS, so *where* we tap decides what we can see:

| Layer | Allowlist by | See URLs? | Per-site bytes? |
|-------|--------------|-----------|-----------------|
| L3/L4 (eBPF at tap) | IP only | no | yes (per IP) |
| DNS (resolve on gateway) | **domain** | domains only | — (maps IP→domain) |
| TLS SNI (ClientHello) | domain | hostname only | — |
| Full URL (MITM) | URL | **yes** | yes |

Two facts fall out of this and shape the whole design:

1. **Domain allowlisting and per-site bandwidth are achievable host-side** — via
   DNS (the name) and eBPF (bytes per remote IP, joined back to the name).
2. **Capturing URLs is not**, without terminating TLS. A MITM proxy plus a CA in
   the guest is the only way, and we deliberately do **not** do that here — it
   breaks cert pinning and is a per-tenant trust decision, not a fleet default.

So sluice implements the two host-side layers — **DNS allowlisting (enforcement)**
and **eBPF metering (telemetry)** — and stops at the domain granularity.

## Architecture

```
                       ┌────────────────────── host ──────────────────────┐
 guest ──DNS:53──▶ sluice resolver ── allowlist? ──▶ upstream ──▶ record IPs
                          │                                          │
                          │ deny → NXDOMAIN + log            ┌───────▼────────┐
                          └──────────────────────────────▶ │ IP→domain table │
                                                            │  = allow-set    │
 guest ─pkt▶ sbtap ─ eBPF from_guest ─ meter tx; drop if dst ∉ allow-set ◀────┤
 out  ─pkt▶ sbtap ─ eBPF to_guest   ─ meter rx (never dropped)                │
                          ▲                                          ▲
                    Flows() join ──────────────────────────────────┘
                    → per-domain bandwidth report
```

**The IP→domain table is the hinge.** An address enters it *only* as the answer
to an allowlisted DNS query (plus pinned infrastructure). The table is read two
ways: the reporter joins eBPF byte counters back to a domain, and the enforcer
treats table membership as the egress allow-set.

### Enforcement model: DNS gate + resolved-IP pin

The DNS gate alone is soft — a guest could hard-code an IP or run its own DoH.
It is closed by pairing it with the eBPF enforcer:

- The resolver answers **only** allowlisted names, and records their A/AAAA
  answers into the allow-set.
- In `--enforce` mode the eBPF `from_guest` program **drops** any packet whose
  destination is not in the allow-set.

Because the sole way an address reaches the allow-set is an allowlisted lookup,
the allowlist becomes the only path to the internet:

- **Hard-coded IP** → never resolved → not in the set → dropped.
- **Public DoH/DoT (1.1.1.1, 8.8.8.8, …)** → those endpoints aren't allowlisted
  → dropped → the gateway resolver is the only working name path.
- **Allowed name** → resolved → IP pinned → reachable until TTL lapses.

The guest's own gateway address is pinned automatically (per tap), so
guest→gateway traffic — DNS, the metadata/identity endpoint on `:8967`, SSH —
is never caught by enforcement.

### TTL handling

DNS TTLs are often a few seconds — far too churny for a firewall allow-set, and
a race where a connection opens just as a record expires would sever it. Records
are held for `max(dnsTTL, min-ttl)` plus a grace window (`min-ttl` defaults to
5 min), and swept lazily. Pinned infrastructure never expires.

## Data plane

`internal/meter/bpf/sluice.c` is a TC/eBPF program attached to both clsact hooks
of each tap:

- `from_guest` (tap **ingress**, packets the guest sent): remote = destination;
  metered as tx; the enforcement point.
- `to_guest` (tap **egress**, packets bound for the guest): remote = source;
  metered as rx; never drops (a blocked request already failed on the way out).

It keys flows by the remote address stored as a 16-byte value (IPv4 held
v4-mapped), so one map covers v4 and v6 and lines up with Go's `netip.Addr`. It
parses fixed Ethernet/IP offsets with `bpf_skb_load_bytes` — no CO-RE, no
`vmlinux.h`, so it builds with a bare clang and no bpftool on the box. Maps:
`flows` (counters), `allowed` (the allow-set, mirrored from the table each
tick), and a one-entry `config` so observe⇄enforce flips live with no reload.

**Fail-open parsing.** A non-IP frame (ARP) or a short/malformed packet passes
un-metered rather than dropped: a parser corner case must never wedge a guest's
network. Enforcement only ever drops a well-formed IP packet to an
un-allowlisted destination.

Userspace (`internal/meter`, cilium/ebpf) loads the object, attaches via TCX
(kernel ≥ 6.6), auto-discovers `sbtap*` as sandboxes come and go, reconciles the
allow-set on a tick, and reads `flows` for the report.

## What sluice does not do

- **No URL capture.** Domain granularity only; paths stay encrypted. A per-tenant
  transparent MITM proxy could be added later behind an explicit opt-in for teams
  that need full URLs — kept out of the default path on purpose.
- **No SNI enforcement.** Enforcement keys on DNS answers, not the TLS SNI, so
  Encrypted Client Hello doesn't weaken it. The trade-off is IP granularity: a
  domain sharing an IP with an allowed domain (some CDNs) is reachable.
- **No in-guest hooks.** Everything runs host-side on the tap.

## Deployment

A `sluice.service` unit runs beside `sparkbox-net.service`. It needs `CAP_BPF` +
`CAP_NET_ADMIN` (load/attach) and `CAP_NET_BIND_SERVICE` (`:53`), and a kernel
≥ 6.6 for TCX. The guest rootfs points `resolv.conf` at its gateway — a one-line
addition to `sparkbox/hack/build-rootfs.sh`, which already writes that file.

Roll it out observe-only first (metering + domain logs, no drops) to see what
real workloads actually reach, turn those domains into the allowlist, then flip
`--enforce`.
