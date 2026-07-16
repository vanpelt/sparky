# Sparkbox key management: fleet secrets into Scaleway Secret Manager

Status: **design** (2026-07-15). Every Scaleway capability claim below was
verified against the live API from a laptop on 2026-07-15 — created, called,
and deleted — not read off a docs page. Several are things the documentation
gets wrong; those are called out inline.

## Why

We hold three private keys and two passwords, and every one of them takes the
same path to the host:

```
operator laptop ~/.sparkbox/secrets/*.pem + .env
      ↓  base64, plaintext
cloud-init user-data          ← retrievable from the Scaleway API for the
      ↓                          server's entire lifetime, by anyone holding
/srv/sparkbox/... 0600           any API key that can read Elastic Metal
```

**cloud-init user-data is functioning as a permanent, plaintext, fleet-wide
secret store.** Everything we have is in it right now, readable for as long as
the server exists, useful from anywhere, forever, with no way to revoke short
of regenerating the key and redeploying. That is the problem worth fixing, and
it is fixable without touching the crypto.

The secondary problem is that rotating any of these secrets means re-rendering
cloud-init and rebooting the host, which is why we have never rotated any of
them.

## Decision: Secret Manager, not Key Manager

Scaleway Key Manager does support server-side ES256 signing that would let the
OIDC key never exist on our host — verified working end to end, including a
real sparkbox-shaped JWT that stock PyJWT accepted. **We are not doing it, and
the reasoning is worth recording** so we don't relitigate it every quarter:

- **Scaleway has no workload identity.** No instance roles, no metadata
  credentials, no STS, no inbound OIDC federation; Elastic Metal has no
  metadata service at all. So the host must hold a static API key regardless.
- Given that key, **an attacker with root on the host can call `Sign` freely.**
  KMS therefore does not prevent forgery by the realistic attacker. It converts
  *permanent, portable, silent* compromise into *temporary, logged, revocable*
  compromise — real, but a much smaller prize than "the key can never be
  stolen" suggests.
- The costs are concrete: a DER→raw r‖s transcode on every mint, and a hard
  runtime dependency on `api.scaleway.com` in the token-minting path.

There's a structural reason the Secret Manager approach is the better trade,
beyond blast radius: **its dependency is boot-time only.** The host fetches
secrets once at startup. A Secret Manager outage delays a boot; a Key Manager
outage would have broken minting on a live fleet. We take an availability
dependency in the least dangerous place.

The KMS findings are preserved in the appendix — they cost real probing, they
are not in any documentation, and one use case (signing the artifact manifest)
survives this reasoning entirely.

## Constraints discovered up front (verified live)

- **Path-based access works, and it's what keeps cloud-init static.**
  `AccessSecretVersionByPath` takes `project_id` + `secret_path` +
  `secret_name` + `latest_enabled`, no UUID. Verified: a 387-byte ed25519 PEM
  round-tripped byte-identical. So cloud-init never needs to know a secret's
  ID, and rotating a secret never touches cloud-init.
- **Typed secrets are validated server-side.** `type=ssh_key` **rejects a raw
  PEM** with `invalid json` — the payload must be `{"ssh_private_key": "..."}`.
  Verified both ways. This is a real guard against storing the wrong thing, at
  the cost of a JSON wrap/unwrap on every read. Worth it for the PEMs.
- **Resource-level IAM conditions are live on our org.** They shipped
  2026-07-15 with no changelog entry, so this was the plan's biggest open
  question. Verified by creating a policy carrying both a resource-level and a
  request-level condition; it was accepted.
- **The documented CEL function name is wrong.** Scaleway's docs say
  `resource.name.startswith(...)`. That fails to compile:
  `undeclared reference to 'startswith'`. CEL's function is camelCase —
  **`startsWith`** — which the API accepts. Verified both.
- **`SecretManagerReadOnly` does not grant payload reads.** Reading a secret's
  value requires **`SecretManagerSecretAccess`**. The names are actively
  misleading and will cost someone an afternoon.
- **Secret Manager does not audit reads.** Create, update, delete, enable, and
  disable land in Audit Trail. `AccessSecretVersion` does **not**. We get a
  complete record of who *managed* secrets and no record of who *read* them.
  This is the sharpest edge of skipping KMS, which does audit every sign. See
  "What this does not buy".
- **The Go SDK's `Data []byte` is already base64-decoded** by `encoding/json`,
  despite the field comment saying "base64-encoded". Decoding again yields
  garbage. Raw HTTP callers *do* need to decode. A silent, easy mistake.
- **20 versions per secret**, and it does not rise with account verification.
  That's a rotation counter, not a storage limit: monthly Cloudflare token
  rotation hits it in under two years. Prune on rotate.
- Other ceilings: 250 secrets/org, 64 kB payload, `fr-par`/`nl-ams`/`pl-waw`
  only (not Milan, whatever the product page says). Pricing is noise: €0.04 per
  version/month.
- IAM object quotas bind at fleet scale: 100 applications, 100 API keys,
  **50 policies**. One application + one policy per host means the policy
  ceiling binds first, at ~50 hosts.

## The design

### Secret layout

All five secrets move. Not just the two passwords — the PEMs are the ones whose
exposure actually matters, and leaving them in user-data would mean doing this
work and still having a plaintext fleet-secret store.

```
/sparkbox/fleet/gateway-host-key        type: ssh_key   {"ssh_private_key": "..."}
/sparkbox/fleet/gateway-upstream-key    type: ssh_key
/sparkbox/fleet/oidc-signing-key        type: opaque    (PEM; not an SSH key)
/sparkbox/fleet/cloudflare-api-token    type: opaque
/sparkbox/fleet/console-password        type: opaque
```

The `/sparkbox/fleet/` prefix is load-bearing: it's what the IAM condition
scopes against, so a future `/sparkbox/host-<id>/` path gets per-host secrets
with the same policy shape.

### Boot flow

cloud-init carries **one** credential instead of five. A small `sparkbox-secrets`
oneshot unit, ordered before `sparkbox.service`, fetches each secret by path and
materializes it, then the server starts unchanged.

**Write the PEMs to `/run` (tmpfs), not the persistent state dir.** They are
re-hydrated from Secret Manager on every boot, so they don't need to survive
one — and tmpfs means a captured disk image or a pulled drive yields no keys.
This is a free win that falls out of the design: it removes the *at-rest disk*
exposure on top of the user-data exposure, and it's only available because
we're fetching at boot anyway.

The keys must stay byte-stable across reboots (clients pin the gateway host key
in `known_hosts`; every rootfs trusts the upstream key), which the fetch
guarantees — Secret Manager is now the authority, and `LoadOrCreateKey`'s
generate-if-missing branch must **not** run on the host. That branch becoming
reachable would silently mint a new fleet identity and lock everyone out; it
should hard-fail on the host instead.

### The bootstrap key, deliberately crippled

This is the load-bearing part. The API key in user-data is unavoidable, so make
it worth as close to nothing as possible off-host:

1. **One IAM application per host** (`sparkbox-host-<id>`), not a user — it
   survives staff departure and scopes blast radius to one box.
2. **`expires_at` set**, backstopped by the org-wide max credential duration
   setting so a forgotten key can't live forever. Rotation is create-new +
   delete-old; there is no native rotation primitive, so this is a cron we own.
3. **Pin to the host's IP** — the single highest-leverage control. The box's
   native public IPv4 is its outbound source address (verified on hardware:
   Elastic Metal does not NAT, so `curl api.ipify.org` from the box equals the
   IP `scw baremetal server get` reports), so the condition is simply:
   ```
   request.ip == '<host-ip>'
   ```
   A key exfiltrated off the box is inert anywhere else. **We do NOT scope by
   secret path**, despite the design's original intent and Scaleway's own docs
   (`resource.name.startsWith("/folder/")`): verified live, for
   `AccessSecretVersionByPath` **`resource.name` is the bare secret name**
   (`gateway-host-key`), not the `/sparkbox/fleet/...` path — a path prefix
   denies *every* read (403), which is exactly the bug that stalled the first
   boot. The isolation boundary that actually works for Secret Manager is the
   **Project** (the rule is project-scoped); for per-box secret isolation, give
   a box its own Project. (Aside: `startsWith` is the correct camelCase CEL
   name — `startswith` fails to compile — but the whole clause is moot here.)
4. **`SecretManagerSecretAccess` only** — not FullAccess, not Write, not
   Delete. The host reads its secrets and can do nothing else, to nothing else.
   (Note: `SecretManagerReadOnly` grants *metadata* only and cannot read a
   payload — the names mislead.)
5. **Audit Trail exported to a bucket** (free; 90-day retention otherwise), with
   an **alert on any Secret Manager management operation whose source IP is not
   the flex IP**. Reads won't be in there — see below — but a key being used to
   *modify* secrets from elsewhere is precisely the signal we want.

## What this does and does not buy

Stated plainly, because the bootstrap key means the secret does not actually
disappear:

| | today | after |
|---|---|---|
| user-data leak yields | **every fleet key, forever, from anywhere** | one API key, expiring, useless off the flex IP |
| Revoke a leaked credential | regenerate + re-render + reboot | one API call, host untouched |
| Rotate a secret | re-render cloud-init + reboot | new version; next boot picks it up |
| Keys at rest on disk | yes, 0600, survive a drive pull | **no — tmpfs, re-fetched each boot** |
| Root on the host yields | every fleet key | every fleet key |

The last row is the honest one: **this does not defend against root on the
host.** An attacker there reads the same secrets, because the host legitimately
needs them. What changes is everything *else* — the permanent plaintext copy in
user-data, the portability of a leaked credential, the ability to revoke, and
the drive-pull exposure.

The regression to accept: **we will not know when secrets are read**, because
Secret Manager doesn't audit `AccessSecretVersion`. Today we have no read
audit either, so this is not a loss against the status quo — but it *is* the
one thing KMS would have given us, and if read-attribution ever becomes a
requirement, that's the trigger to revisit the appendix.

## Rollout

1. **M1 — the bootstrap identity.** IAM application, IP-pinned
   `SecretManagerSecretAccess` policy, key with `expires_at`. Nothing consumes
   it yet. Ships alone because it's the part that makes everything after it
   safe, and it's independently testable.
2. **M2 — the two passwords.** `cloudflare-api-token` and `console-password`
   into Secret Manager; `sparkbox-secrets` unit fetches them into
   `sparkbox.env`. Lowest risk (a bad fetch fails a boot, it doesn't lock
   anyone out), so it proves the mechanism.
3. **M3 — the three PEMs**, to tmpfs, with `LoadOrCreateKey`'s generate branch
   hard-failing on the host. Highest risk: a mistake here locks every client
   out of every sandbox. Needs a break-glass rehearsal on a throwaway host
   before it touches the live one.
4. **M4 — drop the secrets from cloud-init entirely** and rotate all five, on
   the assumption that everything ever committed to user-data should be treated
   as disclosed.

M4 is the point of the exercise. Rotating at the end is what actually retires
the exposure — leaving the old values valid means the old user-data is still a
live secret store.

## Built and verified on hardware (2026-07-16)

M1–M3 shipped together and booted a real Elastic Metal host (`sparkbox-07161634`,
`62.210.158.126`) end to end: `sparkbox fetch-secrets` pulled all five secrets
from Secret Manager into `/run/sparkbox` (tmpfs), sparkbox came up with
`--require-keys` on those keys, the gateway on :22 served the fetched host key
(fingerprint matched byte-for-byte), OIDC served the fetched signing key, and a
sandbox was created through the fetched upstream key. Confirmed on the box: no
PEMs on persistent disk, zero private keys in cloud-init user-data, only the
IP-pinned bootstrap key in `/etc/sparkbox/fetch.env`.

Resolved along the way, now folded into the scripts/units:

- **`nb_rules: 0` was a red herring** — the create response just doesn't reflect
  inline rules; `scw iam rule list` shows them attached and working. No
  `SetRules` needed.
- **`resource.name` is the bare secret name, not the path** (see Part 3). The
  path-prefix condition denied every read and stalled the first boot. Fixed to
  IP-pin + project scope.
- **Don't use a hard `Requires=` on the fetch unit.** It permanently fails
  sparkbox if the fetch is briefly down, and systemd won't start it when the
  fetch later succeeds. `Wants=`+`After=` plus sparkbox's own `Restart=always`
  (held down by `--require-keys`) self-heals once keys land. Both units set
  `StartLimitIntervalSec=0` so the retry loop survives the ~60s IAM propagation
  window at first boot.
- **Reuse the existing gateway keys** (upload them to Secret Manager), never
  regenerate — the rootfs bakes in the upstream pubkey, and clients pin the host
  key. The OIDC key can be minted fresh only because no relying party is bound to
  a brand-new box's issuer yet.

## Open questions

- Does the flexible IP survive a host reinstall? For the verified box we pinned
  to the native public IPv4 (no flexible IP). When a flexible IP is attached,
  confirm whether egress uses it or the native IP before pinning to it — the
  pin must match the *outbound source* address.
- 50-policy org quota vs. one policy per host caps us near 50 hosts. Fine now;
  worth knowing the ceiling exists before the multi-box work in the backlog.
- Per-host teardown must delete the IAM application (done in `teardown-host.sh`),
  or the app/policy/key quota leaks one per launch.
- Secret Manager is `v1beta1` and there is no GA `v1`.

---

## Appendix: Key Manager findings (deferred, not discarded)

Preserved because none of it is documented and all of it was verified live.
**The artifact-manifest use case survives the reasoning above** and is the
thing to pick up first if we revisit: the boot chain is
`latest.env → manifest.env → sha256s`, all fetched from a **public-read bucket
with no signature**, so whoever can write that bucket controls what every host
boots. Object Storage is also the one product with neither resource-level IAM
conditions nor Audit Trail coverage. Signing the manifest with KMS in CI has no
host-identity problem at all — signing happens in CI, the host only needs the
public half, and the private half would then live nowhere, notably *not* in
GitHub Actions secrets.

Verified facts, none of which are in the docs:

- **Sign returns ASN.1 DER, not the raw r‖s that JWT ES256 requires.** A
  transcode is mandatory.
- **DER length varies** — observed 71 and 72 bytes from the same key on
  consecutive calls. Code that assumes a fixed length or slices at a fixed
  offset fails intermittently, which is the worst way to fail. Parse the ASN.1.
- **`GetPublicKey` emits a non-standard PEM label**: `EC PUBLIC KEY` where SPKI
  says `PUBLIC KEY`. **Stock OpenSSL cannot parse it**; the DER body is
  correct. Go's `pem.Decode` ignores the label, so `x509.ParsePKIXPublicKey`
  is fine — but any shell tooling or non-Go verifier needs a relabel.
- **HSM-backed keys exist and are undocumented.** The CLI takes
  `protection-level=software|hsm`; `hsm` returns
  `quota exceeded for resources 'KmsHsmKeys' (0/0)` — a real, quota-gated
  feature. The docs make no HSM or FIPS claim anywhere. One support ticket
  would answer the terms.
- **No ed25519** — EC P-256/P-384, RSA, ML-DSA only. Moving the gateway SSH
  keys to KMS would be a key *type* change (ed25519 → ecdsa-sha2-nistp256),
  forcing a rootfs re-bake of `authorized_keys` and invalidating every client's
  `known_hosts`. Notably, `x/crypto/ssh`'s `NewSignerFromSigner` parses ASN.1
  DER from `crypto.Signer` for ECDSA keys, so KMS drops into the SSH stack with
  *no* transcode — the opposite of the JWT path.
- **Rotation does not apply to signing keys** ("only available for symmetric
  keys"), and there is **no version parameter on Sign/Verify/GetPublicKey**.
  Roll over with a new key + overlapping JWKS, which is what
  `oidc_signing_key_prev` already does. Don't design around `rotation_policy`.
- **Sign latency 446–782 ms** (median 508) transatlantic from a laptop; expect
  low tens of ms in-region.
- **The `scw` CLI has no `sign`/`verify`/`public-key` subcommands** as of
  2.58.3 — probing required raw REST. The Go SDK also trails the API (ML-DSA is
  in the CLI and Python SDK, absent from Go master). Key Manager is GA but the
  API path is still `v1alpha1`. Don't assume CLI parity for ops tooling.
