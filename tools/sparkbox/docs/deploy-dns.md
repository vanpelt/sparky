# DNS + TLS + IPv6 for the sparkbox edge (Cloudflare)

How to make `<sandbox>.hivemind.tools` resolve, serve HTTPS with a single
wildcard certificate, and give each sandbox a routable IPv6 address. Examples
use `hivemind.tools` and the `/64` block `2001:bc8:702:1c7::/64` — substitute
your own.

## The shape of it

```
*.hivemind.tools ─┐
                  ├─ A    → <host IPv4>   (grey cloud / DNS only)
                  └─ AAAA → <host IPv6>   (grey cloud / DNS only)
                                │
                         one host, the sparkbox edge (:443)
                         terminates TLS with a *.hivemind.tools
                         wildcard cert, proxies by Host header to VMs
```

DNS always points at the **host**, never at a VM — the proxy fans out to
sandboxes by the `Host` header. One wildcard record covers every sandbox.

## Why Cloudflare (and why the nameservers must move)

sparkbox's default TLS mode gets one `*.hivemind.tools` wildcard certificate via
the ACME **DNS-01** challenge, which means it creates a temporary
`_acme-challenge.hivemind.tools` TXT record on the fly. That only works if
**Cloudflare is authoritative for the zone** — so you repoint the domain's
nameservers (at Squarespace, your registrar) to Cloudflare. Squarespace stays
your registrar; Cloudflare just runs DNS. Squarespace's own DNS can't do this
(no usable API, unreliable wildcard support).

One wildcard cert (vs. a cert per subdomain) matters here: sandboxes are
ephemeral and each gets a unique subdomain, and per-name issuance would blow
through Let's Encrypt's ~50-certs/week/domain rate limit.

> **Before you flip nameservers:** moving them moves *all* DNS. If you run a
> Squarespace site or email on the apex, recreate those records (the apex `A`s,
> `www` CNAME, any `MX`/`TXT`) in Cloudflare first so nothing goes dark. A
> parked domain with only the Squarespace defaults needs nothing carried over.

## Part 1 — Cloudflare

1. **Add the site.** dash.cloudflare.com → *Add a site* → `hivemind.tools` →
   Free plan. Cloudflare scans and imports existing records.
2. **Note the two nameservers** it assigns (e.g. `dana.ns.cloudflare.com`,
   `rob.ns.cloudflare.com`) — for Part 2.
3. **Records** (DNS → Records). The wildcard is what matters:

   | Type | Name | Value | Proxy status |
   |------|------|-------|--------------|
   | `A` | `*` | `<host IPv4>` | **DNS only (grey)** |
   | `AAAA` | `*` | `2001:bc8:702:1c7::1` | **DNS only (grey)** |
   | `A` | `*.xterm` | `<host IPv4>` | **DNS only (grey)** |

   - **Grey cloud is required.** sparkbox terminates TLS itself; Cloudflare's
     proxy (orange) would intercept it, and the free plan can't do a wildcard
     edge cert anyway. Grey = traffic hits the box directly.
   - **`*.xterm` is the browser terminal**, served at
     `<sandbox>.xterm.hivemind.tools`. It needs its own record because a
     wildcard matches exactly one label — `*.hivemind.tools` does not cover it,
     in DNS (RFC 4592) or in a certificate (RFC 6125). sparkbox publishes this
     one itself at startup only when `CLOUDFLARE_API_TOKEN` **and** `--edge-v4`
     are both set — it has no address to point the record at otherwise, and says
     so in one startup WARN naming the missing flag. Create the row here and it
     does not matter either way; it never removes it. Skip it (and pass
     `--xterm-subdomain ""`) if you don't want browser terminals.
   - The `AAAA` value is the **host's own** address from your `/64`.
     `::1` is the natural choice (sparkbox reserves it — VMs start at `::2`).
   - Optional: `A`/`AAAA` `console` → same host for a control-plane hostname;
     `A`/`AAAA` `ssh` → same host so you can `ssh new@ssh.hivemind.tools`.
4. **API token** (My Profile → API Tokens → Create Token → *Edit zone DNS*
   template). Zone Resources → *Include → Specific zone → hivemind.tools*.
   Confirm it has **Zone → DNS → Edit** *and* **Zone → Zone → Read** (the ACME
   client needs Zone:Read to resolve the zone id). Copy it — that's
   `CLOUDFLARE_API_TOKEN`.

## Part 2 — Squarespace (repoint nameservers)

Squarespace → *Domains → hivemind.tools → DNS / Nameservers* (look for a
**Nameservers** section, sometimes under *Advanced settings*). Switch from
Squarespace's nameservers to **Custom nameservers** and enter the two Cloudflare
nameservers from Part 1. Save. Cloudflare emails you when the zone goes
**Active** — usually minutes, up to 24h. Once active, the Squarespace DNS panel
no longer applies; Cloudflare is authoritative.

## Part 3 — IPv6 on the host

You need a **routed `/64`** (Scaleway Elastic Metal and Hetzner delegate one).
Confirm it's routed *to the box* (not merely on-link), then:

- Assign the host's edge address, e.g. `2001:bc8:702:1c7::1`, to the box (that's
  the `AAAA` target). VMs use `::2` and up.
- Enable forwarding: `sysctl -w net.ipv6.conf.all.forwarding=1` (the setup
  scripts do this). No NAT — the guest `/128`s are globally routable and
  sparkbox adds a per-VM `/127` route so inbound reaches each tap.

`hack/setup-host.sh` wires this when you pass the block:

```sh
SPARKBOX_SUBNET6=2001:bc8:702:1c7::/64 ./setup-host.sh "you $(cat ~/.ssh/authorized_keys | head -1)"
```

## Part 4 — Run sparkbox with TLS + IPv6

```sh
export CLOUDFLARE_API_TOKEN=<scoped Zone.DNS:Edit token from Part 1>

sparkbox serve --driver firecracker \
  --state-dir /srv/sparkbox/data/state \
  --kernel /srv/sparkbox/vmlinux --image-dir /srv/sparkbox/data/images \
  --users /srv/sparkbox/users.conf \
  --ssh-addr :2222 \
  --proxy-addr :443 --proxy-tls \
  --proxy-domain hivemind.tools --tls-email you@example.com \
  --subnet6 2001:bc8:702:1c7::/64
```

On first start it runs the DNS-01 dance (creates a temp TXT via Cloudflare, gets
the `*.hivemind.tools` cert, caches it under `state/certmagic/`) and auto-renews.
Then `https://<sandbox>.hivemind.tools` works for every sandbox with a real cert.

It then asks for `*.xterm.hivemind.tools` in a **second** ACME order, for the
browser terminals. That one is non-fatal: if it fails, sparkbox logs
`browser terminals will not be reachable over https` and carries on serving
everything else. Check the `tls certificates managed` line at startup for what
it actually holds — a missing terminal wildcard otherwise surfaces only as a
full-page certificate warning, with no server-side log at all.

Open firewall ports **443** (edge) and **2222** (SSH gateway). Port 80 is *not*
needed — DNS-01 doesn't use it.

## Verify

```sh
dig NS hivemind.tools +short           # → *.ns.cloudflare.com (zone moved)
dig +short anything.hivemind.tools     # → host IPv4 (wildcard A)
dig +short AAAA anything.hivemind.tools # → host IPv6 (wildcard AAAA)
curl -I https://demo.hivemind.tools    # after creating sandbox "demo"
```

## Gotchas

- **The wildcard covers one label.** `*.hivemind.tools` (record *and* cert)
  matches `myvm.hivemind.tools` but not `a.b.hivemind.tools`. Default sandbox
  subdomains are single-label, so avoid dotted custom subdomains unless you add
  records/certs for them. The browser terminal is the one built-in exception,
  which is exactly why it ships with its own `*.xterm` record and certificate.
- **Grey cloud, always**, for anything sparkbox serves — otherwise Cloudflare
  terminates TLS and the DNS-01 wildcard cert never gets used.
- **IPv6-only box?** Then IPv4-only clients can't reach a grey-cloud record. The
  only fix that keeps universal reach is orange-cloud proxying (Cloudflare
  bridges v4→v6), which moves TLS termination to Cloudflare's edge — a different
  setup. Prefer dual-stack (Part 1's `A` + `AAAA`).
- **No token yet?** Validate the plumbing with `--tls-provider autocert`
  (per-host certs, needs port 80) or plain HTTP on `--proxy-addr :8081` first.
