# Sparkbox authenticated port forwarding: any-port URLs behind an SSH-key login

Status: **M1 implemented** (2026-07-17). Turns the public-preview edge
(`internal/proxy`) into one that can also serve *private* ports at
`https://<sandbox>.<domain>:<port>`, gated by a browser session that a user
mints from their SSH key, forwarding their identity to the upstream app. M2/M3
(offline SSHSIG browser login, sharing beyond owner/operator) remain deferred —
see "What shipped" at the bottom for where each M1 piece lives.

## Why

The edge today is deliberately unauthenticated: `<name>.hivemind.tools` is a
public web preview of whatever the sandbox serves (see the package doc in
`internal/proxy/proxy.go`). That is right for a demo, wrong for anything real
running in a sandbox — a dashboard, a Jupyter server, an internal tool.

exe.dev's proxy solves this: any port is reachable at
`https://<vm>.exe.xyz:<port>`, **private by default**, and a first-time
visitor is redirected to log in before the request is proxied. We want the same
three things, reusing the primitives already in the tree:

1. **Any port in the URL** — `https://wacky-doo.hivemind.tools:4444` forwards to
   guest port `4444`, with no per-port route row to create first.
2. **A login gate** — an unauthenticated visitor to a private port is redirected
   to sign in; a signed session cookie then covers every subdomain and port.
3. **Identity to the upstream** — the app behind the port receives the visitor's
   handle and email as headers, so it can do its own authorization.

The trust root already exists: `internal/users` maps an **SSH public key → a
handle**, and `ctl@` (`internal/sshgw/control.go`) is a control channel already
authenticated by that key. "A signed token you create with your SSH PEM" is
therefore not a new credential system — it is one new `ctl@` verb plus a cookie
the edge can verify.

## Constraints discovered up front (these shaped the design)

- **The edge is one listener on one address.** `--proxy-addr` (`:443` in prod)
  terminates the wildcard TLS cert by SNI. To answer on `:4444` *and* every
  other port with that single cert + listener, the kernel has to funnel the
  connections in — the app can't `Listen` on 64k ports. This is the
  `iptables REDIRECT` + `SO_ORIGINAL_DST` plumbing in Part 1.
- **TLS survives the REDIRECT.** The ClientHello SNI is still
  `wacky-doo.hivemind.tools` regardless of which port the packet landed on, so
  the wildcard cert matches and autocert/DNS-01 issuance is unchanged. The port
  is an L4 fact, recovered below TLS.
- **The Host header already carries the dialed port.** A browser hitting
  `:4444` sends `Host: wacky-doo.hivemind.tools:4444` (the port is omitted only
  for the scheme default, 443). Convenient, but **not trustworthy** as the
  forward target — a client can lie. `SO_ORIGINAL_DST` is authoritative; the
  Host header is only used for subdomain routing, which `subdomainOf` already
  strips the port from.
- **There is no human web login anywhere in sparkbox.** Users are SSH keys; the
  console is a shared password. So the session cookie and its mint path are net
  new — but they lean entirely on the existing key→handle store, so no OAuth
  app, no password, no server-side session table.
- **The user record has no email.** `internal/users` stores handle + keys +
  optional GitHub login, nothing else. `X-Forwarded-Email` needs a new column
  (Part 6).
- **Cookie scope must be the parent domain.** One login has to cover
  `a.hivemind.tools:3000`, `b.hivemind.tools:8080`, … so the cookie is set for
  `Domain=.hivemind.tools`. That also means the cookie is offered to the
  console and OIDC subdomains — fine, they ignore it; it is signed and only the
  private-route gate reads it.

## Part 1 — Any port in the URL

**Getting the bytes to the edge.** A boot-time nat rule redirects the private
port range to the real edge port on the flexible IP(s):

```
iptables  -t nat -A PREROUTING -d <edge-v4> -p tcp --dport 1024:65535 -j REDIRECT --to-ports 443
ip6tables -t nat -A PREROUTING -d <edge-v6> -p tcp --dport 1024:65535 -j REDIRECT --to-ports 443
```

lives in `deploy/sparkbox-net.sh` next to the existing MASQUERADE rule (the one
the reboot-persistence unit already reapplies — see the deployment memo). The
range **excludes the host's own service ports** (22 gateway, 2222 admin sshd,
443 edge, the API and metadata addrs) by binding those below 1024 or carving
them out of the range; a REDIRECT of `--dport 22` would eat the SSH gateway.

**Recovering the target port.** The edge already knows how to stash a
connection's addresses on the request context — the metadata server does exactly
this with `ConnContext` (`internal/metadata/server.go`) to read the accepted
socket's *local* address. We do the same, but read the **original**
pre-DNAT destination via `getsockopt(SO_ORIGINAL_DST)` on the raw
`*net.TCPConn`, and put the port on the context:

```go
srv := &http.Server{
    ConnContext: func(ctx context.Context, c net.Conn) context.Context {
        if p, ok := originalDstPort(c); ok {            // SO_ORIGINAL_DST, Linux
            return context.WithValue(ctx, origPortKey{}, p)
        }
        return ctx                                       // :443 direct → default port
    },
}
```

`originalDstPort` is ~15 lines of `syscall.GetsockoptIPv6Mreq` /
`unix.GetsockoptIPv6MTUInfo`-style raw getsockopt behind a `//go:build linux`
file, with a no-op stub elsewhere so the mock/dev build on macOS still compiles
(it just always serves the default port). A connection that arrived straight on
`:443` (no REDIRECT) has no original dst → default port `8000`, preserving
today's behavior for `https://name.hivemind.tools` with no port.

**Routing.** `ServeHTTP` stays almost the same: `subdomainOf(r.Host)` → sandbox
name (unchanged), then the **port comes from the context**, not a route row:

```go
port := defaultPort
if p, ok := r.Context().Value(origPortKey{}).(int); ok {
    port = p
}
```

No route row is required to reach a port — the port *is* the URL, matching
exe.dev. The `routes` table keeps its role for two things it's still needed for:
a friendly *named* subdomain that isn't the sandbox name, and per-(sandbox,port)
**visibility** (Part 2). A bare `<sandbox>.<domain>:<port>` with no matching row
resolves the sandbox by name and defaults to **private** (owner-only).

## Part 2 — Visibility and who may view

Add a `visibility` column to `routes` (`private` | `public`), default
**`private`**. For ports with no row, the effective default is also `private` —
opt *out* to publish, matching exe.dev, and safer than today's open edge.

```sql
ALTER TABLE routes ADD COLUMN visibility TEXT NOT NULL DEFAULT 'private';
```

`ctl@` gains a `share` verb (owner-only, mirrors the existing owned-box
commands in `control.go`):

```
ssh ctl@hivemind.tools share wacky-doo 4444 public     # publish one port
ssh ctl@hivemind.tools share wacky-doo 4444 private    # re-gate it
ssh ctl@hivemind.tools share wacky-doo                 # list this box's ports + visibility
```

Legacy public previews stay reachable: the default route a sandbox gets at
create time is written `public` if we want to preserve today's zero-config
preview, or `private` if we want secure-by-default. **Recommend private**, with
`share <name> <port> public` the one command to get a shareable link — a
one-line behavior change worth calling out at deploy time.

**Access check** (the picked scope — owner + operators):

```go
func mayView(u users.User, r routes.Route) bool {
    return u.Handle == r.Owner || u.IsOperator()
}
```

`IsOperator()` already exists. A public route skips the whole gate. A private
route with no authenticated session → redirect (Part 3); with a session whose
handle fails `mayView` → **403**, not a redirect loop (they *are* logged in,
just not allowed).

## Part 3 — The login: a token you mint from your SSH key

Two mint paths, both rooting in the SSH key the user already has. Neither adds a
password or an OAuth app.

**Path A — `ctl@` bearer token (the easy path, ships first).** The SSH transport
has *already* proven key possession by the time `ctl@` runs, so minting is
unconditional:

```
$ ssh ctl@hivemind.tools session-token            # optional: --ttl 12h
spk_v1.eyJoIjoidmFucGVsdCIsImUiOiJ2YW5wZWx0QHdhbmRiLmNvbSIsImV4cCI6...
```

The value is a signed session token (format in Part 4). Use it two ways:
- **Programmatic** — `curl -H "Authorization: Bearer spk_v1...."
  https://wacky-doo.hivemind.tools:4444` (CLI/API access to a private port).
- **Browser** — paste it into the login page, which sets it as the
  `spark_session` cookie for `.hivemind.tools`.

**Path B — offline SSHSIG (no SSH access needed, ships second).** The login page
issues a nonce; the user signs it with their private key and uploads the
signature:

```
$ echo -n "<nonce>" | ssh-keygen -Y sign -n sparkbox-login -f ~/.ssh/id_ed25519
```

The edge parses the `SSHSIG` blob, reconstructs the signed-data envelope
(`MAGIC "SSHSIG" || namespace || reserved || hashAlg || H(message)`), and
verifies it with `xssh.PublicKey.Verify` against the public keys in the `users`
store — the *same* store `users.Lookup` uses for SSH auth. A match yields the
handle; the edge then sets the `spark_session` cookie itself. This is the pure
"signed token from your PEM" flow for someone who only has a browser and their
key file. ~120 lines of SSHSIG framing (RFC-draft, stable) plus reuse of the
existing verify path.

**The redirect.** A private route with no valid session returns `302` to
`https://login.hivemind.tools/?return=<url-encoded original URL>`. `login` joins
`console` and `oidc` as an edge-reserved subdomain (`SetLogin`, alongside
`SetConsole`/`SetIssuer` in `proxy.go`; add `login` to the `reserved` set in
`internal/users`). The login page: a nonce, the one-liner to run, a paste box
(Path A) or a signature upload (Path B); on success it `Set-Cookie`s and
`302`s back to `return`. Non-browser clients (no `text/html` in `Accept`) get a
`401` with a `WWW-Authenticate`-style hint instead of a redirect, same
branch the error page already uses.

## Part 4 — The session token

A session token asserts *this human, until this time* to the **edge only** — a
narrower audience than the OIDC workload tokens, verified by nobody else. So it
does not need the ES256 issuer; a keyed MAC is faster (no per-request public-key
verify) and simpler:

```
payload = base64url(json{ h:<handle>, e:<email>, exp:<unix>, v:1 })
token   = "spk_v1." + payload + "." + base64url(HMAC-SHA256(K, payload))
```

`K` is **derived from the OIDC signing key**, not a new fleet secret:
`K = HKDF(oidc_key_bytes, "sparkbox-edge-session/v1")`. This reuses a secret
already distributed to every host (`/run/sparkbox/keys`, see the boot-secrets
design) and keeps the session domain cryptographically separate from token
signing — an OIDC rotation invalidates outstanding sessions, which is
acceptable (they re-mint with one `ssh` command) and arguably desirable.
Rejected: a 5th fleet secret (more key distribution for a low-value,
short-lived, re-mintable credential); signing sessions with ES256 (a
public-key verify on every proxied request buys nothing when the edge is the
only verifier).

Verification on each request: split, recompute the MAC (constant-time compare,
like `console.go` already does), check `exp`. Cheap, stateless, no session
store — same property the console cookie has. Default TTL 12h; `--ttl` caps at,
say, 7d.

## Part 5 — Identity to the upstream

Once a request is authorized, the reverse-proxy `Rewrite` (already present in
`proxy.go`) injects the visitor's identity and **strips any client-supplied
copies first** so an inbound request can't spoof them:

```go
Rewrite: func(pr *httputil.ProxyRequest) {
    // ...existing SetURL / SetXForwarded / Host preservation...
    for _, h := range []string{"X-Forwarded-User", "X-Forwarded-Email",
        "X-Forwarded-Preferred-Username", "X-Forwarded-Access-Token"} {
        pr.Out.Header.Del(h)
    }
    if id, ok := pr.In.Context().Value(identityKey).(sessionIdentity); ok {
        pr.Out.Header.Set("X-Forwarded-User", id.Handle)
        pr.Out.Header.Set("X-Forwarded-Email", id.Email)
        pr.Out.Header.Set("X-Forwarded-Preferred-Username", id.Handle)
    }
},
```

The `X-Forwarded-User` / `X-Forwarded-Email` / `X-Forwarded-Preferred-Username`
names are the **oauth2-proxy convention**, so any upstream already written to sit
behind oauth2-proxy (a large ecosystem: Grafana, many internal tools) works
unmodified. A public route sets none of these — absence means "anonymous," never
an empty string a naive upstream might treat as a user.

## Part 6 — Email on the user record

```sql
ALTER TABLE users ADD COLUMN email TEXT;
```

Populated by:
- `ssh ctl@hivemind.tools email set vanpelt@wandb.com` (self-service verb in
  `controlKeys`-style handler),
- auto-fill from the GitHub-verified account's primary email when
  `verify-github` runs with the `user:email` scope (opportunistic; not required),
- operator set at invite time.

`claimsFor`/`identityDoc` in `internal/metadata` gain the email too, so a
sandbox's own identity doc and OIDC token can carry it — a small consistency win,
not required for this feature. A user with no email set → `X-Forwarded-Email`
omitted (not blank); upstreams that require an email will 403 such a visitor,
which is the correct fail-closed behavior.

## Data model & files touched

| Area | Change |
| --- | --- |
| `internal/routes` | `+ visibility` column; `Route.Visibility`; `SetVisibility(sub/sandbox, port, v)`; default `private` |
| `internal/users`  | `+ email` column; `User.Email`; `SetEmail`; add `login` to `reserved` |
| `internal/proxy`  | `SetLogin`; original-dst port on context; visibility gate → redirect/403; identity headers in `Rewrite`; session cookie verify |
| `internal/edgeauth` (new) | session-token mint/verify (HMAC + HKDF), SSHSIG parse+verify, login HTTP handler |
| `internal/sshgw/control.go` | `session-token`, `share`, `email` verbs |
| `internal/metadata` | carry `email` in claims/identity (optional) |
| `deploy/sparkbox-net.sh` | REDIRECT rule for the private port range (v4+v6) |
| `cmd/sparkbox/main.go` | wire `SetLogin`, `ConnContext`, `--session-ttl`, HKDF the session key from the loaded OIDC key |

No new fleet secret, no new datastore, no new listener.

## Security notes

- **Port target is authoritative from `SO_ORIGINAL_DST`, never the Host header.**
  A lying Host can only misroute the *subdomain* — and that resolves to a
  sandbox the visitor is authorized for or gets 403'd anyway.
- **Identity headers are stripped before injection**, so a client cannot preset
  `X-Forwarded-User`. This is the single most common auth-proxy footgun.
- **Cookie is `HttpOnly`, `Secure` (behind `--proxy-tls`), `SameSite=Lax`,
  `Domain=.hivemind.tools`.** Lax is enough — these are first-party navigations;
  no cross-site POST needs the cookie. The token is a MAC over server-held key
  material, so a stolen cookie is only usable until `exp`.
- **The gate runs before resume-on-connect.** An unauthenticated hit on a paused
  private sandbox redirects to login *without* waking the VM — the auth check is
  cheap and must not be a free way to spin up someone else's box. (Reorder:
  visibility + session check first, `EnsureRunning` second.)
- **Rate-limit login POSTs** (nonce verify) the way `internal/metadata` rate-
  limits minting, so the SSHSIG path isn't an oracle.

## Phasing

- **M1 — the gate + easy token.** REDIRECT plumbing, original-dst port,
  `visibility` column + default-private, owner/operator check, redirect→login,
  `ctl@ session-token` (Path A), paste-token login page, identity headers,
  `email` column. This is the whole feature for anyone who can `ssh`.
- **M2 — browser-native login.** SSHSIG offline signing (Path B) so a
  pure-browser visitor with only their key file can log in, and the
  GitHub-email auto-fill.
- **M3 — sharing beyond owner/operator.** If needed later, a `shared_with`
  table and `share <name> <port> --with <handle>` (deferred; the picked scope is
  owner + operators).

## What this reuses vs. builds

Reuses: the wildcard TLS edge, `subdomainOf` routing, the `ConnContext`
address-stashing trick from metadata, the key→handle store, the `ctl@`
authenticated channel, the OIDC key as HKDF input, the console's HMAC-cookie
pattern, and the reverse-proxy `Rewrite` hook. Builds: `SO_ORIGINAL_DST`
recovery, the `visibility` gate, the session token + SSHSIG verify, and the
login page. The net-new surface is one package (`internal/edgeauth`) and two
columns.

## What shipped (M1)

Landed on branch `claude/agentic-sandbox-design-1mwipf`, 2026-07-17. Deviations
from the design above are noted.

- **`internal/edgeauth`** (new) — `Signer` (HKDF-SHA256 from the OIDC key →
  HMAC-SHA256 session tokens `spk_v1.<payload>.<mac>`), `IdentityFrom` (cookie
  then Bearer), and the `login.<domain>` handler (`token.go`, `login.go`,
  `login.html`). Unit-tested: mint/verify round-trip, tamper/expiry/wrong-key
  rejection, cookie+redirect flow, open-redirect guard.
- **`internal/routes`** — `visibility` column (migration via
  `pragma_table_info`), default `private`; `SetVisibility`; `Upsert` updates
  only the port on conflict so a manager re-upsert never re-privatises a shared
  route.
- **`internal/users`** — `email` column + `SetEmail`/`ValidEmail`; `login` added
  to the reserved handles.
- **`internal/proxy`** — `SetAuth` wires the gate (off ⇒ everything serves open,
  for mock/dev); `authorize` runs **before** resume-on-connect; `targetPort`
  resolves the any-port URL; identity headers injected (and client copies
  stripped) in `Rewrite`. `SO_ORIGINAL_DST` recovery is build-tagged
  (`origdst_linux.go` / `origdst_other.go`).
- **`internal/sshgw`** — `ctl@` verbs `session-token [--ttl]`, `share <name>
  [<port>] [public|private|forget]`, `email [set|clear]` (`control_auth.go`).
- **`deploy/sparkbox-net.sh`** — `SPARKBOX_EDGE` nat chain REDIRECTing the
  private port range (v4, and v6 when `SUBNET6` is set) to the edge, hooked only
  for uplink traffic so guest→gateway metadata is never caught; excludes admin
  sshd (:2222) and the edge port.

**Visibility is per-(subdomain, port)**, as designed. The routes table is keyed
by subdomain and carries the visibility of the port its portless URL forwards
to; `route_ports` (`internal/routes/ports.go`) carries every other port the same
hostname serves, and a port with no row is private. `ctl@ share <name> <port>`,
the console's port strip, and the guest's `sparkbox make-public PORT` all write
one port. The no-port spellings are deliberately asymmetric: `private` closes
every port (a panic button has to mean it) while `public` opens only the default
one, so no single command can publish whatever happened to be listening when it
ran.

**Deviations from the design.** The target port is taken from the Host header when `SO_ORIGINAL_DST`
is unavailable (non-Linux dev, or a direct :443 hit), with the getsockopt value
authoritative when present — so the feature is exercisable on the mock stack
without iptables. M2 (offline SSHSIG login) and M3 (share lists) are not built;
the browser login is the paste-a-`ctl@`-token page.
