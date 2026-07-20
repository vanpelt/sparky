# Tailnet direct-to-VM with per-VM HTTPS — design

> **STATUS: PARKED (decision record), 2026-07-18.** We prototyped P0-lite (subnet
> route + per-name DNS) and it worked — a running sandbox was reachable directly at
> `http://<name>.catnip.sh:<port>` over the tailnet — but we chose **not** to pursue
> it. The only thing it wins over the edge is removing the shared-host port
> collision, and that is better solved by giving the **edge its own tailnet IP** (a
> dedicated node), which keeps publicly-trusted wildcard HTTPS, the auth gate, and
> resume-on-connect that this design would each have to reinvent. The HTTPS story
> here needs a **private CA** (its root is self-signed → every device must install
> it; not publicly trusted), which is the deal-breaker. Kept for the reasoning and
> the trilemma below. See `tailnet-edge-design.md` for the dedicated-node direction.

## Goal

Let a tailnet member open `https://<name>.<domain>:<any-port>` and have DNS resolve
**straight to the sandbox VM**, with the VM presenting a **valid TLS certificate**
for `<name>.<domain>` — no shared-host edge in the path, no iptables REDIRECT on
the host, no collision with other services on the sparkbox machine, and no
plaintext. Every port a VM listens on is reachable natively and encrypted.

This replaces, for tailnet users, the current model where one host edge on a
shared `:443` multiplexes every sandbox and an iptables REDIRECT range funnels
any-port URLs into it (see `tailnet-edge-design.md`). The edge does not go away —
it stays the front door for **public** (non-tailnet) users via the Cloudflare
tunnel, for the console/OIDC, and for authenticated private routes. This design
is the **tailnet-native, any-port, direct** tier layered beside it.

Why the current REDIRECT is worth replacing for tailnet use: it lives on the
host's *own* tailnet IP, so it must exclude every host-stack port (sshd `:22`,
gateway `:2222`, DNS `:53`, the edge `:443`, a co-tenant's `tailscale serve`
`:8443`, …) or hijack them — a standing collision hazard on a shared box. Pointing
DNS at the VMs sidesteps it entirely: VMs live in a different IP space (the guest
subnet), not on the host's ports.

## Architecture

```
laptop (tailnet, --accept-routes)                 DGX / sparkbox host
  resolve dazzling-canyon.catnip.sh
      │ split DNS: catnip.sh -> sparkbox dnsedge (:53)
      ▼                                            sparkbox
  dnsedge answers <name> -> 172.30.<vm>            ├─ dnsedge (:53)  per-name -> guest IP; wakes paused VMs
      │                                            ├─ host CA / cert minter  (holds signing key)
      │ subnet route 172.30.0.0/16 (host = router) ├─ metadata svc (:8967, guest-only)  serves per-name cert+key
      ▼                                            ├─ edge (:443)   still here for public/console/authed routes
  172.30.<vm>:<port>  ──WireGuard──▶ host ──tap──▶ guest VM
                                                     └─ tiny TLS terminator  (per-name cert) -> 127.0.0.1:<port> app
```

Three moving parts, none on the host's shared ports:
1. **Reachability** — the host advertises the guest subnet as a Tailscale subnet
   route, so tailnet peers can dial guest IPs directly on any port.
2. **Naming** — `dnsedge` answers `<name>.<domain>` with *that sandbox's* guest IP
   (and wakes it if paused).
3. **TLS** — each VM terminates HTTPS with a cert scoped to only its own name,
   minted by the host and delivered over the existing metadata channel.

## 1. Reachability — subnet route over the guest network

The host already NATs `172.30.0.0/16` for guests and is their gateway. Instead of
(only) masquerading egress, advertise that subnet into the tailnet:

```
tailscale set --advertise-routes=172.30.0.0/16     # approve once in the admin/API
```

Tailnet peers that run `--accept-routes` then route `172.30.<vm>` via the host
(WireGuard → host → `sbtap+`). The host is already an IP-forwarding router with
`FORWARD` ACCEPT rules for `sbtap+`; return traffic from the guest to the peer's
`100.x` goes back out `tailscale0` via the host's existing routes. No new NAT, no
REDIRECT, no host-port involvement.

Costs / requirements:
- One-time **route approval** (admin console or `POST /device/{id}/routes` via the
  API key we already use for split DNS).
- Peers must set **`--accept-routes`** (our tailnet currently has it off — the
  health check flags it). This is the client-side analogue of the split-DNS
  step; a `setup-tailnet.sh` line and a note in the docs cover it.
- **Guest IP stability — a required change, not just an open question.** Today the
  slot is `idx = d.next`, a **monotonic counter** assigned fresh on every start
  (`internal/vmm/firecracker/fc.go`), and `HostIP` is cleared to `""` on pause
  (`internal/host/manager.go`). So a paused→resumed sandbox returns on a
  **different** `172.30.<idx>.2`. For per-name DNS + wake-on-resolve to point at a
  stable target, the allocator must **reserve a deterministic slot per sandbox for
  its lifetime** (persist `idx` in the sandbox record; re-use it on resume; free it
  on delete). This also fixes a latent limit — a never-reused monotonic `idx`
  eventually exceeds 255 and breaks the `172.30.<idx>` carving.
  - *Cheaper interim:* skip the reservation and have wake-on-resolve boot the VM
    then return whatever fresh IP it got, leaning on the 60 s TTL. Works for a demo
    but the IP churns across resumes (stale caches / dropped long-lived conns), so
    it is not the shipping shape.

## 2. Naming — per-name `dnsedge` + wake-on-resolve

`internal/dnsedge` today answers the whole zone with a fixed edge IP. Extend it to
be **registry-aware**:

- `<name>.<domain>` → look up sandbox `<name>`; answer `A 172.30.<vm>` (and `AAAA`
  if guests get v6). Short TTL (already 60s) so a moved/rebuilt VM re-resolves.
- **apex, `console.`, `oidc.`, `login.`, and any unknown/non-sandbox label** →
  fall back to the **edge IP** (so the console, OIDC issuer, login, and the public
  path are unchanged).
- **Wake-on-resolve** — if `<name>` exists but is paused/archived, the responder
  triggers `mgr.EnsureRunning(name)` and returns the sandbox's (reserved) guest IP.
  The client's TCP SYN retries cover the ~1–4 s Firecracker boot. This moves the
  edge's resume-on-connect magic into the DNS layer so **scale-to-zero survives**
  the direct model. Guard it: only wake on a query from *inside the tailnet* (the
  responder is only reachable there via split DNS anyway), and rate-limit so a
  resolver retry storm can't thrash the VM.

`dnsedge` gains a small dependency on the sandbox store (read-only lookups) and on
the manager (for the wake hook) — both already in-process in `sparkbox serve`.

## 3. TLS — per-VM certificate, host-minted, guest-terminated

The hard constraint: the cert must be presented **at the VM**, but the VM is
untrusted (arbitrary agent code), so it may hold **only a cert scoped to its own
name** — never the `*.<domain>` wildcard key (whose leak would impersonate every
sandbox and the console).

Flow:
1. **Host mints** a leaf cert for exactly `<name>.<domain>` when the sandbox is
   created/started. The host holds the signing material; the guest never sees it.
2. **Delivery over metadata.** The guest fetches its cert+key from the existing
   guest-only metadata service (`:8967`, reachable solely from `sbtap+`, already
   the identity-token channel). The key rides the private host↔guest tap; nothing
   new is exposed. Re-fetch on renewal.
3. **Guest terminator.** A tiny TLS-terminating reverse proxy shipped in the guest
   image presents the per-name cert and forwards to the user's app on
   `127.0.0.1:<port>`. Any-port handling mirrors the host edge but **single-tenant**:
   an in-guest `PREROUTING` REDIRECT of the tap-facing port range → the terminator,
   which recovers the dialed port via `SO_ORIGINAL_DST` (reuse
   `internal/proxy/origdst_linux.go`) and dials `127.0.0.1:<that port>`. Because
   the VM has no other tenants, this REDIRECT can never collide — the exact concern
   that makes it objectionable on the shared host does not exist inside a VM.
   - *iptables-free variant:* the terminator fronts a **declared** set of ports
     (app binds loopback) instead of the whole range — no in-guest netfilter, at
     the cost of "every port just works" ergonomics. Offer as a config knob.

### 3a. Who signs the leaf — the real decision

| | **Private CA** (host is the CA) | **Public Let's Encrypt** (host DNS-01, per name) |
|---|---|---|
| Trust | Install operator root **once per tailnet device** | Publicly trusted, **zero client setup** |
| Scale | Unlimited, instant, offline | **~50 certs/week per registered domain** → breaks under churn |
| Fit | Ephemeral sandbox fleet | A few long-lived boxes |

**Recommendation: private CA.** The LE per-registered-domain cap is the very reason
the *edge* uses one wildcard rather than per-name certs; moving termination into the
VM doesn't lift it. This path is tailnet-only and operator-owned, so trusting the
operator's own root on the operator's own devices (the `mkcert`/internal-PKI
pattern) is a reasonable one-time step. The host keeps the root key; guests get
short-lived per-name leaves. Public visitors keep hitting the edge's real wildcard
over the tunnel and never encounter the private root.

Publicly-trusted per-name certs on `<name>.<domain>` **and** ephemeral scale **and**
`<domain>` naming is a genuine trilemma — pick two. The private CA gives up public
trust; the ts.net escape hatch (below) gives up the name.

### 3b. Escape hatch: `.ts.net` names with zero cert machinery

If `.ts.net` names are acceptable for the direct path, run **tailscaled inside each
guest** (Option B; the guest kernel already has `CONFIG_TUN`). Then
`tailscale cert <vm>.<tailnet>.ts.net` yields a **real, publicly-trusted** cert with
no CA, no rate limit, and no key handling on our side, and `tailscale serve` can
terminate it. Cost: `https://<name>.<tailnet>.ts.net:<port>` instead of
`catnip.sh`, plus tailscaled + ephemeral/tagged auth keys per guest (which also buy
**per-VM tag ACLs** — the strongest privacy story). This is the "perfect HTTPS,
imperfect name" corner of the trilemma; keep it documented as the alternative.

## 4. Auth / visibility posture

Direct-to-VM bypasses the edge's `authorize()` gate (private-by-default, SSH-key
login, per-route public/private). On the direct path, access is gated by **tailnet
ACLs** — who is on the tailnet and allowed the route — not per sandbox. For a solo
operator that is exactly right. Consequences to state plainly:

- Any tailnet member allowed the subnet route can reach any *running* VM on any
  port. There is no per-sandbox private/public distinction on this path.
- If per-sandbox privacy among tailnet members is ever needed, that pushes toward
  Option B (per-VM tailnet nodes + tag ACLs), where Tailscale enforces it per node.
- The authenticated, per-route model **still exists on the edge** for public users
  and for anyone who wants the login gate; the two coexist.

## 5. Relationship to the edge (hybrid, not replacement)

- **Tailnet user, any port:** `dnsedge` → guest IP → VM terminator. Direct, HTTPS,
  no host multiplexing.
- **Public user / console / OIDC / authed private route:** `dnsedge` apex+reserved
  labels and public DNS → the **edge** (real wildcard, tunnel, `authorize()`).
- The host-side any-port REDIRECT (`SPARKBOX_TAILNET_IF` in `sparkbox-net.sh`)
  becomes unnecessary for the tailnet once this lands, and can be turned off — its
  collision surface is the thing we set out to remove.

## 6. Setup UX

Fold into `setup-tailnet.sh`:
- advertise + approve `172.30.0.0/16`;
- print/automate `tailscale set --accept-routes` guidance for clients;
- generate the operator root CA (or choose LE mode) and emit the **root cert for
  clients to install**, with per-OS install hints;
- everything else (split DNS, cert minting, metadata delivery) is host-internal.

## 7. Security

- **Wildcard key never leaves the host.** Guests hold only a per-name leaf; leak
  radius is one sandbox name.
- **Metadata channel** is already guest-only (`INPUT ... ! -i sbtap+ -j DROP`);
  the leaf key travels the private tap.
- **Subnet route** exposes guest IPs to route-accepting tailnet peers only; scope
  with tailnet ACLs. This is a trusted-network "private A record", not a public
  DNS-rebind.
- **Wake-on-resolve** must be tailnet-gated and rate-limited so DNS retries can't
  be used to spin sandboxes.
- **Private root** is a real trust decision for the operator's devices; keep the
  root key offline/host-only and issue short-lived leaves.

## 8. Phasing

- **P0 — reachability:** advertise `172.30.0.0/16`, approve, `--accept-routes` on
  the laptop, per-name `dnsedge`. Prove `http://<name>.catnip.sh:<port>` hits the
  VM directly (no edge, no REDIRECT). Wake-on-resolve for scale-to-zero.
- **P1 — TLS:** host per-name minter (private CA) + metadata cert delivery + guest
  terminator. Prove `https://<name>.catnip.sh:<port>` green with the operator root
  installed.
- **P2 — productize:** `setup-tailnet.sh` (route + accept-routes + CA + root
  export), renewal, turn off the host tailnet REDIRECT, docs.
- **P3 — optional isolation:** Option B (per-VM tailscaled + tag ACLs +
  `tailscale cert`) for operators who want per-sandbox privacy and/or ts.net certs.

## Open questions

- Is a sandbox's `172.30.x` already reserved across pause/resume? If not, make it
  deterministic per sandbox so DNS + wake-on-resolve have a stable target.
- Terminator: ship a ~100-line Go binary (reuse `origdst`) vs. bake Caddy into the
  guest image. Lean Go for size/control; Caddy for on-demand cert reload ergonomics.
- Leaf lifetime + renewal cadence; does the terminator hot-reload or the metadata
  service push?
- Do guests get tailnet/guest **v6** on this path (dual-stack `dnsedge` answers)?
- Should wake-on-resolve block briefly for the boot, or always return fast and rely
  on TCP retry? Measure real Firecracker cold-resume from archived vs paused.
