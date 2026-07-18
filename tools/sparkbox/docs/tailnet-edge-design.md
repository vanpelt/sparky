# Tailnet edge — wildcard DNS + split-DNS — design

## Goal

Give **every device on the operator's tailnet** native, private access to sandboxes
at `https://<name>.<domain>`, `ssh <name>@<domain>`, and `https://<name>.<domain>:<port>`
— real names, real TLS, every port — with **no public exposure, no VPS, no tunnel,
and $0**. Public reachability stays a deliberate, per-sandbox opt-in (see the seam
below). This is the "works great for me and anyone I add to the tailnet" tier; the
VPS-forwarder and exe-ssh-over-HTTPS designs cover the "works for anyone" tier.

Why this is possible: inside a tailnet, a node is reachable on **any port** with no
NAT and no ingress config. The only missing pieces are (1) wildcard **DNS** that
resolves `*.<domain>` to the edge's tailnet IP, and (2) the edge listening on a
tailnet-reachable address. Tailscale **split DNS** supplies (1); sparkbox supplies
(2) — which is exactly the native edge it already runs on a public box, just bound
to a tailnet IP.

## Architecture

```
laptop (on tailnet)                      DGX (on tailnet)
  resolve dazzling-canyon.catnip.sh
      │ split DNS: catnip.sh -> <edge-ip>       sparkbox
      ▼                                           ├─ wildcard DNS responder  (:53) → *.catnip.sh A <edge-ip>
  <edge-ip> (tailnet)  ───────────────────────▶  ├─ proxy edge  (:443, wildcard TLS) → route → guest:port
  ssh/https/any port over WireGuard              ├─ ssh gateway (:22)        → PTY into guest
                                                 └─ (metadata :8967, etc.)
```

Three things must be true on the DGX:
1. sparkbox serves **wildcard DNS** for `<domain>` → the edge IP.
2. Tailscale **split DNS** tells the tailnet to resolve `<domain>` via that responder.
3. The edge listens on a **tailnet-reachable IP** on :443 (+ :22, :53).

## 1. Built-in wildcard DNS responder

Rather than have every operator hand-configure dnsmasq, sparkbox serves its own
wildcard — it already knows its domain and edge address.

- New package `internal/dnsedge` using `github.com/miekg/dns` (already a
  transitive dep via certmagic; promote to direct).
- Answers `A`/`AAAA` for the apex and **any** `*.<proxy-domain>` with the edge IP;
  `NXDOMAIN`/empty for other qtypes it shouldn't own. Short TTL (e.g. 60s) so the
  edge IP can move.
- Wired like the other listeners in `main.go`
  (`go func(){ errCh <- dnsSrv.ListenAndServe() }()`) behind a new
  `--dns-addr` flag (e.g. `<edge-ip>:53`, empty = disabled).
- Serves UDP + TCP :53. Bind to the edge IP specifically so it never fights the
  host's `systemd-resolved` stub on `127.0.0.53`.

Off-the-shelf `dnsmasq` (`address=/<domain>/<ip>`) is the P0 stand-in to validate
the path before the Go responder lands.

## 2. The edge IP — three cases

`<name>.<domain>` implies port **443**, and DNS can't carry a port, so the edge
needs an IP where **:443 is free**.

- **Fresh box (host tailscale :443 free):** bind the edge to the host tailscale IP.
  Nothing extra. This is the default the setup script targets.
- **Host :443 taken (our DGX — openclaw holds `tailscale serve` on
  `100.65.150.80:443`):** give sparkbox a **dedicated tailnet-reachable IP**:
  - *(preferred)* a **subnet route**: add a private `/32` (e.g. `10.66.0.1`) to a
    dummy interface, bind the edge there, and `tailscale up
    --advertise-routes=10.66.0.1/32` (approve the route once in the admin console).
    One daemon, coexists cleanly with openclaw.
  - *(alt)* a **second `tailscaled` instance** (own state dir, socket, TUN, authkey)
    so sparkbox joins the tailnet as its own node `catnip` with its own IP. More
    isolation, more moving parts.

Recommend the subnet route for the DGX; document the second-node option.

## 3. TLS

sparkbox mints a real `*.<domain>` cert via **Cloudflare DNS-01**
(`--proxy-tls --tls-provider cloudflare`, the token already present). DNS-01
validates over public DNS and does **not** require the name to resolve publicly to
the box, so tailnet clients get valid HTTPS with no warnings even though the A
record points at a private tailnet IP. **Keep `--subnet6` unset** so the same token
does not wake the front-door AAAA publisher (which would fight the wildcard).

## 4. Registering split DNS

**Manual** (admin console → DNS): Nameservers → Add → Custom → the edge IP →
"Restrict to domain" → `<domain>` → Save.

**Automated:** the Tailscale API exposes split DNS (`tailscale_dns_split_nameservers`
in the Terraform provider maps to it). Given a tailnet API key, the setup script
`PUT`s `{ "<domain>": ["<edge-ip>"] }`; without a key it prints the one-click step.

## 5. Setup-script UX

A `--tailnet` bring-up mode (in `setup-host.sh` / a `deploy/setup-tailnet.sh`):

```
DOMAIN=catnip.sh TS_AUTHKEY=tskey-… [TS_API_KEY=tskey-api-…] ./deploy/setup-tailnet.sh
```

It: (a) ensures the edge IP (host IP, or subnet-route a `/32`), (b) starts sparkbox
with `--proxy-addr <ip>:443 --proxy-tls --tls-provider cloudflare --ssh-addr <ip>:22
--dns-addr <ip>:53 --proxy-domain $DOMAIN`, (c) registers split DNS via the API or
prints the manual step. A user points their own domain's `*.` wherever they like
(it only matters inside the tailnet) and gets `*.theirdomain` on their tailnet.

## 6. Private-by-default, public-on-request (the "catnip.sh for public ports" seam)

Split DNS is **tailnet-only** by construction. Public reach stays opt-in and reuses
sparkbox's existing route **visibility** model:

- **Default:** `*.<domain>` resolves (via split DNS) to the tailnet IP — native, all
  ports, tailnet-only. Nothing is public.
- **Opt-in:** `ctl@ share <name> public` (or opening a specific port) additionally
  fronts *that* route through the public path (VPS forwarder / tunnel) and publishes
  a **public** DNS record. `<domain>` does double duty: split DNS → tailnet IP for
  members, public record → forwarder for the opened route.

So the common case needs zero public infra, and going public is one explicit action
per sandbox — which is exactly the private-by-default posture the proxy already has.

## 7. Security

- Split DNS scopes the override to `<domain>` only; the rest of the tailnet's DNS is
  untouched. Access is still gated by **tailnet ACLs** (who can reach the edge node)
  *and* sparkbox's own auth (SSH keys, route visibility) — belt and suspenders.
- The wildcard responder answers a private IP; that's a "DNS rebinding"-shaped answer
  and is fine here because it's served *inside* the tailnet by a trusted resolver
  (unlike a public A → private IP, which some resolvers filter).
- Certs are real (DNS-01), so no TLS-warning training.

## 8. Phasing

- **P0 — validate (no new code):** subnet-route a `/32` on the DGX, dnsmasq
  wildcard, split-DNS entry, run sparkbox's existing edge on `<ip>:443` with a
  DNS-01 cert. Prove `https://<name>.catnip.sh` and `ssh <name>@catnip.sh` from the
  laptop over the tailnet.
- **P1 — productize DNS:** `internal/dnsedge` + `--dns-addr`, retire dnsmasq.
- **P2 — setup automation:** `deploy/setup-tailnet.sh` + Tailscale-API split-DNS.
- **P3 — public seam:** wire `share public` to the forwarder + public record.

## Open questions

- Subnet-route `/32` vs dedicated `tailscaled` node as the productized default — the
  route is simpler but needs one-time admin approval of the advertised route.
- Should the DNS responder also answer AAAA with the edge's tailnet v6 (dual-stack
  tailnet), and should split DNS hand out both?
- Idle policy: a tailnet hit should `EnsureRunning` the target like a web hit does;
  confirm the DNS layer doesn't need to (it only resolves the shared edge IP, not
  per-sandbox).
