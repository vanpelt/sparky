# sluice

A **sluice gate for VM egress**: a DNS allowlist resolver paired with a TC/eBPF
meter on each sandbox tap. Together they let an operator (1) restrict which
domains a sparkbox VM can reach, (2) log every domain a VM looked up, and (3)
account bandwidth **per domain** — without an agent inside the guest and without
terminating TLS.

It is the implementation of options **A** (host-side eBPF metering) and **B**
(DNS allowlist with resolved-IP egress pinning) from
[`docs/vm-network-allowlisting-design.md`](../../docs/vm-network-allowlisting-design.md).

```
guest ──DNS──▶ sluice resolver ──┬─ allowlisted?  no ─▶ NXDOMAIN + log
                                 └─ yes ─▶ upstream, record answer IPs, reply
guest ──pkt──▶ sbtap ──▶ eBPF(from_guest) ─ meter bytes; drop if dst ∉ allow-set
outside ─pkt─▶ sbtap ──▶ eBPF(to_guest)   ─ meter bytes (downloads never dropped)
```

The two halves share one IP→domain table. An address enters the table **only**
by being the answer to an allowlisted query, and — in `--enforce` mode — the
eBPF program drops egress to any address not in that table. So a guest cannot
escape the allowlist by hard-coding an IP or using a public DoH resolver: those
destinations were never resolved through the gate, so they are not reachable.

## Why this shape

The host is already the default gateway and NAT for every guest (`172.30.<idx>.1`
on `sbtap<idx>`), so all egress crosses one chokepoint we own. Guest traffic is
TLS, so the host cannot see URLs — but it **can** see, and gate on, the domain
(via DNS) and count bytes per remote IP (via eBPF), which is exactly what
"allowlist domains + per-site bandwidth" needs. Full URL capture would require a
MITM proxy and a CA in the guest; that is deliberately out of scope here.

## Usage

```sh
# Test a policy file without running anything.
sluice check --allowlist allowlist.example api.github.com googleapis.com evil.com
#   ALLOW  api.github.com   (matched github.com)
#   DENY   googleapis.com                          # *.googleapis.com ≠ apex
#   DENY   evil.com

# Run the gateway + meter. Observe-only (no drops) until you add --enforce.
sudo sluice run --allowlist allowlist.txt --dns-listen :53 --report-interval 30s

# Enforce: guests may reach only what the allowlist resolves.
sudo sluice run --allowlist allowlist.txt --enforce
```

`run` flags:

| flag | default | meaning |
|------|---------|---------|
| `--allowlist` | — | policy file, one rule per line (required) |
| `--dns-listen` | `:53` | resolver bind address (guests dial their gateway IP) |
| `--upstream` | `1.1.1.1:53,8.8.8.8:53` | real resolvers for allowed names (repeatable) |
| `--enforce` | `false` | drop egress to addresses the allowlist never resolved |
| `--tap-prefix` | `sbtap` | attach the meter to interfaces with this prefix |
| `--allow-ip` | — | always-reachable IP that bypasses DNS (repeatable) |
| `--deny` | `nxdomain` | reply for blocked names: `nxdomain` or `refused` |
| `--min-ttl` | `5m` | floor on how long a resolved IP stays reachable |
| `--report-interval` | `30s` | per-domain bandwidth report period (`0` disables) |
| `--sync-interval` | `5s` | tap-discovery + allow-set reconcile period |
| `--log` | `json` | `json` or `text` |

### Allowlist syntax

```
github.com        # the apex AND every subdomain (api.github.com, …)
*.googleapis.com  # subdomains only, NOT the bare googleapis.com
# comments and blank lines are ignored
```

### Per-domain bandwidth report

```
=== per-domain bandwidth @ 3:04PM ===
DOMAIN                 IPS  UP       DOWN     TOTAL
github.com             3    1.2MiB   84.5MiB  85.7MiB
files.pythonhosted.org 2    212KiB   43.1MiB  43.3MiB
93.184.216.34 (ip)     1    4.0KiB   9.2KiB   13.2KiB
TOTAL                       1.4MiB   127.7MiB 129.1MiB
```

Rows marked `(ip)` are raw-IP flows with no matching DNS record — in enforce
mode these only appear for pinned infrastructure (the gateway) or `--allow-ip`
entries, since everything else is dropped.

## Guest configuration

Guests must resolve through their gateway (`172.30.<idx>.1`), where sluice
listens. sparkbox wires this for you: start the gateway with

```sh
sparkbox serve --driver firecracker --guest-dns gateway ...
```

and the Firecracker driver passes `sparkbox_dns=<gateway-ip>` on the guest kernel
cmdline. The guest `sparkbox-netcfg` hook (baked by `hack/build-rootfs.sh`) reads
it and writes `/etc/resolv.conf` accordingly, overriding any resolver the image
shipped. Without the flag, guests fall back to public DNS (plain NAT'd egress, no
allowlisting) — so existing fleets are unaffected until you opt in. You can also
pass a literal address (`--guest-dns 10.0.0.53`) instead of `gateway`.

In `--enforce` mode a guest cannot bypass the resolver: public DNS/DoH/DoT
endpoints are not on the allowlist, so packets to them are dropped, leaving the
gateway resolver as the only working path to a name.

## Deploy

Runs alongside `sparkbox-net.service`; see
[`deploy/sluice.service`](deploy/sluice.service). It needs a BPF-capable kernel
(TCX attach ⇒ ≥ 6.6), `CAP_BPF`+`CAP_NET_ADMIN` to load and attach, and
`CAP_NET_BIND_SERVICE` for `:53`. The meter auto-discovers `sbtap*` taps as
sandboxes come and go, pins each tap's gateway address into the allow-set so
guest→gateway traffic (DNS, metadata, SSH) is never dropped, and expires
resolved IPs on their (clamped) DNS TTL.

## Layout

```
cmd/sluice            CLI: `run` and `check`
internal/allowlist    domain matching (apex+subdomain, wildcard)
internal/ipmap        TTL IP→domain table; doubles as the egress allow-set
internal/dnsproxy     miekg/dns handler: gate, forward, record, log
internal/report       join eBPF byte counters to domains → bandwidth table
internal/meter        cilium/ebpf loader, TCX attach, map I/O (Linux only)
internal/meter/bpf    sluice.c — the TC program (meter + enforce)
```

## Build & test

```sh
make bpf     # recompile internal/meter/sluice_bpfel.o from sluice.c (needs clang)
make build   # -> bin/sluice
make test    # go test ./...
```

The compiled eBPF object is committed, so `go build ./...` works without clang.
The data-plane tests (`internal/meter`) load the program through the kernel
verifier and drive it with `BPF_PROG_TEST_RUN`; they require root and a
BPF-capable kernel and skip cleanly otherwise.

## Limits

- **Domains, not URLs.** HTTPS paths are encrypted; capturing them needs a MITM
  proxy, which this tool intentionally avoids.
- **SNI/ECH.** Enforcement keys on DNS answers, not SNI, so Encrypted Client
  Hello does not weaken it. A domain sharing an IP with an allowed domain (some
  CDNs) is reachable if that IP is in the allow-set — IP-granular, not SNI.
- **DNS TTL vs. connections.** IPs are pinned for at least `--min-ttl` past a
  lookup so long-lived connections aren't cut when a short TTL lapses.
