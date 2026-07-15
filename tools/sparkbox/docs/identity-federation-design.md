# Sparkbox identity: user registration, SSH key linking, and OIDC workload identity

Status: **implemented** (2026-07-15) — M1 and M2 both landed in one pass; see
"What shipped" at the bottom for where each piece lives and where the built
thing deviates from this design.

## Why

Two asks that turn out to be one design:

1. **Workload identity federation for hivemind.** Sandboxes ship the `hivemind`
   CLI, and hivemind supports OIDC federation: a workload presents an id token
   from a trusted issuer to `POST /v1/auth/actions/exchange` and gets a scoped
   hivemind credential — no long-lived secret in the VM. GitHub Actions and
   Kubernetes work this way today; sparkbox should be a third such issuer, so
   `hivemind start` inside any sandbox just works.
2. **A user identity story.** Today a "user" is one line in `users.conf`
   (`<name> <authorized_keys line>`, exact-match pubkey → bare string). That is
   fine as an allowlist but too thin to put in a token: no registration, no
   multiple keys, no verified linkage to an external identity anyone else
   trusts. If sparkbox mints identity tokens, the claims must mean something.

## Constraints discovered up front (these shaped everything)

- **Hivemind verifies RS256/ES256 only — no EdDSA.** Verified in the exchange
  handler (PyJWT allowlist). Our gateway keys are ed25519, so the existing
  private key **cannot** sign these tokens directly. We add one new fleet
  secret: an **ES256 (P-256) OIDC signing key**, generated and distributed
  exactly like the two gateway keys (`~/.sparkbox/secrets/oidc_signing_key.pem`
  → cloud-init → state dir). Deriving the P-256 key from the ed25519 seed via
  HKDF was considered and rejected: nonstandard, saves one file, couples key
  lifetimes (an OIDC rotation would force an SSH key rotation and a rootfs
  re-bake).
- **Hivemind requires a live https issuer**: it fetches
  `{issuer}/.well-known/openid-configuration`, follows `jwks_uri` (https,
  public IP, SSRF-guarded), and caches JWKS. It also requires `exp, iss, aud,
  sub`, exact issuer+audience match, and a **CEL policy** on the service
  account (unrestricted federation is rejected server-side).
- **JTI replay protection**: each id token is exchangeable exactly once
  (`(iss, jti)` remembered for 24h). Tokens must be minted fresh per fetch,
  never served from cache.
- **The daemon's zero-config path**: hivemind's auth chain reads a JWT from
  `HIVEMIND_OIDC_TOKEN_FILE`, default **`/var/run/secrets/hivemind/token`**,
  and re-exchanges ~5 min before expiry by re-reading the file. If sparkbox
  keeps a fresh token at that exact path, `hivemind start` federates with no
  env vars and no login.

## Part 1 — sparkbox as an OIDC issuer

### Issuer endpoint

`https://oidc.<proxy-domain>` (e.g. `oidc.hivemind.tools`), served by the
existing proxy edge next to the console special-case. Wildcard DNS already
resolves it to the flexible IPs and autocert already issues per-SNI certs, so
the only new code is two GET handlers:

- `/.well-known/openid-configuration` — `issuer`, `jwks_uri`, and the static
  `response_types_supported`/`subject_types_supported`/
  `id_token_signing_alg_values_supported: ["ES256"]` boilerplate verifiers
  expect. No authorization/token endpoints: this is an id-token-only issuer,
  same as GitHub Actions'.
- `/jwks.json` — current + previous public key (two-key window makes rotation
  a non-event: publish new, keep signing with old for one TTL, swap).

Multi-box note: the issuer lives on the **gateway** (single issuer per fleet),
and the signing key is a fleet secret like the gateway keys — nothing here
fights the planned gateway/worker split.

### Token minting and delivery into guests

The host already has an unforgeable per-VM channel: each guest's default
gateway is its own tap's host address `172.30.<idx>.1`. A tiny metadata service
on port 8967 (chosen unused) identifies the caller by the **source** address of
the accepted connection. `GET /token?aud=...` then:

1. source addr `172.30.<idx>.2` → sandbox → `{name, owner}` from the manager
   (the same in-memory state the gateway trusts today); the destination must be
   that source's own gateway, so a guest can only ever ask its own;
2. joins owner → user record (Part 2) for the identity claims;
3. mints a fresh ES256 JWT, TTL **1h**, unique `jti`, and returns it.

> **Correction (found during implementation).** This design originally said to
> trust the connection's *local* address, on the reasoning that a guest could
> spoof its source but not which tap delivered the packet. That is backwards,
> and would have handed any guest a token for any other sandbox:
>
> - The **destination is attacker-chosen.** The host has IP forwarding on and
>   Linux uses the weak host model, so it accepts a packet addressed to any
>   local address arriving on any interface. A guest in slot 5 just connects to
>   `172.30.9.1`; the kernel delivers it, and the accepted socket's local
>   address reads `172.30.9.1` — slot 9's token, handed to slot 5. Binding a
>   separate listener per tap does not help: that socket accepts the packet too.
> - The **source is not spoofable here.** This is TCP: a SYN with a forged
>   source is answered by a SYN-ACK routed to the real owner of that address (a
>   different tap), so the spoofer never completes the handshake and gets
>   nothing. `rp_filter=1` on each tap drops those packets outright as well.
>
> Regression test: `TestGuestCannotMintATokenForAnotherSandbox`.

No secret material lives in the guest; possession of the network position *is*
the authentication, exactly like cloud IMDS. Rate-limit per VM and never cache
(replay protection makes cached tokens worthless anyway).

In the guest, a systemd service + timer (`sparkbox-token.service/.timer`,
baked next to `sparkbox-netcfg`): fetch on boot and every **45 min**
(kubelet-style ~80% of TTL) into `/var/run/secrets/hivemind/token` (0600,
root). Discovering the metadata IP is trivial: it is the guest's IPv4 default
gateway. Delivery into existing templates uses the same host-side template
patching as `refresh-agent-tools.sh` — no image rebuild. Also write
`/run/sparkbox/identity.json` (the decoded claims) so shells and tools can
cheaply answer "who am I" without parsing a JWT.

### Claims

```json
{
  "iss": "https://oidc.hivemind.tools",
  "sub": "sparkbox:user:vanpelt",
  "aud": "https://hivemind.wandb.tools",
  "exp": 1752570000, "iat": 1752566400, "nbf": 1752566400,
  "jti": "8f7c…",

  "owner": "vanpelt",
  "github": "vanpelt",
  "key_fp": "SHA256:…",
  "sandbox": "tidy-meteor",
  "image": "universal",
  "box": "sparkbox-07151236"
}
```

- `sub` is **stable per user**, not per sandbox, so one hivemind service
  account (sub_selector `sparkbox:user:vanpelt`) covers all of a user's
  sandboxes; per-sandbox scoping goes in the CEL policy instead
  (`claims.sandbox == "ci-runner"`, `claims.image == "universal"`, …).
- `aud` comes from the `?aud=` query (allowlisted per host config, default
  the hivemind SaaS URL) so the same endpoint can later serve other relying
  parties (W&B platform federation, vaults) without a second issuer.
- `github` appears **only when verified** (Part 2); CEL policies get a strong
  external anchor: `claims.github == "vanpelt"`.
- `key_fp` is the fingerprint of the SSH key that authenticated the session
  which created/last-resumed the sandbox — useful for auditing, not intended
  for policy.

### Hivemind onboarding (per user, one time)

Dashboard → Service Accounts → New: issuer `https://oidc.hivemind.tools`,
audience default, sub_selector `sparkbox:user:<handle>`, policy e.g.
`claims.owner == "<handle>"`. Then any of that user's sandboxes:
`hivemind start`. Done — no tokens pasted anywhere.

## Part 2 — user registration and key linking

Principles: SSH pubkeys stay the sole credential (no passwords, no cookies for
users); registration must be possible entirely over SSH (the only door users
already have); external identity (GitHub) is an optional, *verified* claim,
not a login dependency.

### User store

**SQLite** (`<state-dir>/sparkbox.db`, the same file the proxy routes already
live in — a second connection, which WAL makes safe). This is the one shape
change from the original plan: the store went straight to SQLite rather than a
`users.json` staging post, since invite redemption wants an atomic
compare-and-set and key lookup wants a real index. The record it holds:

```json
{ "handle": "vanpelt",
  "created_at": "…", "status": "active",
  "invited_by": "operator",
  "github": {"login": "vanpelt", "verified_at": "…"},
  "keys": [ {"authorized_key": "ssh-ed25519 AAAA… laptop",
             "fp": "SHA256:…", "label": "laptop",
             "added_at": "…", "via": "signup|ctl|github-import"} ] }
```

`users.conf` remains as the **bootstrap seed**: on startup, entries not in the
store are imported as active users (operator-blessed). The gateway's auth path
switches from the flat map to the store; everything downstream keeps using the
handle string, unchanged.

### Registration: `ssh signup@<domain>`

A third reserved door beside `new`/`ctl` (front-door address included). The
gateway already has the connecting pubkey before any session opens, which
makes signup a short interactive TTY dialog:

```
$ ssh signup@hivemind.tools
sparkbox: this key isn't registered yet (SHA256:Zk3…).
invite code: ····-····
handle: cvp
link a GitHub account? enter your GitHub username to verify
this key against github.com/<user>.keys (or blank to skip): vanpelt
✓ key SHA256:Zk3… is listed on github.com/vanpelt — verified.
registered. try:  ssh new@hivemind.tools
```

- **Invite codes** gate who may join (they are authorization; the key is
  authentication). `ctl invite` mints them — operator always; optionally any
  active user with a small quota (`--invites-per-user`, default 0 = operator
  only). Codes are single-use, expiring, stored hashed.
- **GitHub verification without OAuth**: fetch `https://github.com/<login>.keys`
  and check the *connecting* key is in the set. Possession of a key GitHub
  serves for that account proves control of the account, to the same strength
  GitHub itself accepts for git push. No OAuth app, no browser, no client
  secret. (Device-flow OAuth can be added later for org/email claims; it is
  deliberately not the MVP.)
- Handles are claimed first-come, `[a-z0-9-]{2,32}`, immutable (they appear in
  `sub`; renames would silently break CEL policies).

### Key management: `ctl keys`

An authenticated session (any linked key) manages the keyring:

```
ssh ctl@hivemind.tools keys list
ssh ctl@hivemind.tools keys add "ssh-ed25519 AAAA… work-laptop"
ssh ctl@hivemind.tools keys import-github        # sync all keys from linked login
ssh ctl@hivemind.tools keys rm SHA256:…          # refuses to remove the last key
```

`import-github` re-fetches `.keys` for the linked login; `keys add` marks keys
`via: ctl` (they authenticate, but only github-verified presence keeps the
`github` claim — if the linked account ever stops listing *any* of the user's
keys, the claim persists; verification is at link time, recorded with
`verified_at`, and `ctl keys verify-github` re-runs it on demand).

### What this fixes beyond federation

- Multiple devices per user (today: one line per key with duplicated names and
  no grouping).
- Losing a laptop = `keys rm` from another device, not editing a file on the
  host over admin SSH.
- The per-owner running cap and `ctl` ownership checks get a real subject.
- The unauthenticated internal API's free-text `owner` field stays
  localhost-only, but the console (operator-scoped today) can later grow
  per-user views keyed by real records.

## Rollout

1. **M1 — issuer + tokens with today's flat users.** ES256 key in secrets;
   `/.well-known` + JWKS on the proxy; per-tap metadata service; guest
   token-fetch unit patched into the template. Claims carry `owner` but no
   `github`. This alone makes `hivemind start` work federated.
2. **M2 — user store + signup door + ctl keys.** Seed from users.conf; invite
   codes; GitHub `.keys` verification; `github` claim appears in tokens.
3. **M3 — later**: device-flow OAuth (email/org claims), per-sandbox service
   subjects (`sub: sparkbox:sandbox:<owner>/<name>`) if a relying party wants
   sandbox-granular subjects, console self-service, moving the *sandbox
   registry* (still `sandboxes.json`) into SQLite alongside the user store when
   multi-box lands.

## Resolved questions

Each shipped as proposed:

- **Audience allowlist**: `--oidc-audiences`, default-closed to the hivemind
  SaaS URL. Empty allows any `aud`.
- **Invite policy**: operator-only. `--invites-per-user` (default 0) opens a
  quota to non-operators. "Operator" needed a definition and got one with no
  new config: the accounts seeded from `users.conf` — the file whoever
  provisions the host writes — are the operators (`invited_by = "operator"`).
- **Token TTL**: 1h, refreshed by the guest timer every 45 min (~75% of life).
- **Open signup**: `--open-signup`, off by default.

## What shipped

| Piece | Where |
|---|---|
| Identity store (users, keys, invites) | `internal/users` |
| Issuer: ES256 key, minting, discovery + JWKS | `internal/oidc` |
| Per-sandbox token endpoint | `internal/metadata` |
| `signup@` door, `ctl keys`, `ctl invite`, `ctl whoami` | `internal/sshgw` |
| Guest token unit + timer | `deploy/install-guest-identity.sh` |

Notes on the built thing:

- The guest payload is installed by one script called from **both**
  `hack/build-rootfs.sh` (bake time) and `deploy/refresh-agent-tools.sh` (host
  side, no rebuild), so templates published before workload identity existed
  pick it up on the next provision. It is versioned by `IDENTITY_REV`, which
  the refresher stamps and compares.
- The issuer rides the existing proxy edge at `oidc.<domain>`; both TLS
  providers already cover it (wildcard / autocert HostPolicy). Running without
  `--proxy-tls` logs a warning: the `iss` claim is https and a verifier will
  not be able to fetch it.
- `key_fp` is recorded via `Manager.RecordKey` on each session rather than
  threaded through `Create`, keeping the signature churn out of the API.
