# OpenAI workload identity federation

Status: **implemented** (2026-09-02), **not yet deployed** — nothing is on until
a deploy passes a `--federation-config` naming an `openai` entry, and the ids
in it come from an OpenAI organization admin.

This is the OpenAI-specific half. The mechanism — the list of relying parties,
how the host serves it and how the guest walks it — is
[federation.md](federation.md); OpenAI is one entry in that list.

## Why

A sandbox already proves who it is to HiveMind with an OIDC assertion this
fleet signs — see [identity-federation-design.md](identity-federation-design.md).
OpenAI now accepts the same shape of proof: present a JWT from a trusted
issuer, get back a short-lived OpenAI access token (RFC 8693 token exchange at
`https://auth.openai.com/oauth/token`, one hour maximum, no refresh token).

So `codex` and the OpenAI SDKs can work inside a sandbox with **no API key
anywhere in the VM** — nothing in `/etc/environment`, nothing in a rootfs
template, nothing a fork or a snapshot can carry off. It also fixes a real
papercut: `codex` in a fresh sandbox otherwise asks a person to complete a
browser login, in a VM that has no browser.

We already run the issuer. The whole integration is one more audience out of
it, plus telling the guest which federation rule to present the assertion
against.

## What OpenAI needs from an issuer

Confirmed against the API guide, the token-exchange reference, and the
Kubernetes/custom-OIDC provider guide:

| Requirement | Sparkbox |
|---|---|
| Public HTTPS issuer serving `/.well-known/openid-configuration` | `https://oidc.<domain>` — live, verified |
| `jwks_uri` reachable from OpenAI | `https://oidc.<domain>/jwks.json` |
| JWT with `kid` and a supported `alg` | ES256, `kid` in every header |
| `iss`, `aud`, `sub`, `exp`, `iat` | all present; `nbf` and `jti` too |
| Unique `jti` when replay protection is on | every mint has a fresh one |
| Assertion lifetime bound | 1h (`oidc.TokenTTL`) |

Nothing had to change in `internal/oidc`. The claims a relying party can write
policy against are the ones already documented there: `owner`, `github`,
`key_fp`, `sandbox`, `sandbox_id`, `image`, `box`.

Note that `sub` is **per user, not per sandbox** (`sparkbox:user:<handle>`), by
the same deliberate choice HiveMind federation made: one rule then covers all of
a person's sandboxes, and per-sandbox scoping goes in a CEL condition on the
`sandbox` claim.

## What shipped

### The entry

```json
{
  "name": "openai",
  "audience": "https://api.openai.com/v1",
  "token_file": "/var/run/secrets/openai.com/identity-token",
  "token_file_env": "OPENAI_IDENTITY_TOKEN_FILE",
  "context_env": "OPENAI_WORKLOAD_IDENTITY_CONTEXT",
  "env": {
    "OPENAI_FEDERATION_RULE_ID": "idpm_...",
    "OPENAI_IDENTITY_PROVIDER_ID": "idp_...",
    "OPENAI_SERVICE_ACCOUNT_ID": "svc_..."
  }
}
```

`federation.OpenAI(provider, rule, serviceAccount)` builds exactly this, and a
test pins `deploy/kubernetes/federation.example.json` to it, so the example an
operator copies and the entry the tests exercise cannot drift.

- **Audience** `https://api.openai.com/v1` is what OpenAI's own Kubernetes guide
  tells operators to project, so an admin configuring the provider sees a value
  they have seen before. It is opaque to both ends and must match the
  provider's configured audience exactly. It joins `--oidc-audiences` by
  construction, like every audience in the list.
- **Token file** `/var/run/secrets/openai.com/identity-token`: the directory
  OpenAI's guidance names, mode 0600 in a 0700 directory owned by the login
  user, refreshed every 45 minutes against a 1-hour assertion. Codex reads the
  path from `OPENAI_IDENTITY_TOKEN_FILE`.
- **`OPENAI_FEDERATION_RULE_ID`** is the one identifier Codex needs. The
  provider and service-account ids are what the OpenAI SDKs take when they
  perform the exchange themselves.
- **`OPENAI_WORKLOAD_IDENTITY_CONTEXT`** is the optional audit attribution,
  `{"instance_id":"<sandbox>","labels":{"owner":"...","box":"..."}}`, so an
  admin reading their logs sees which sandbox made a request and not only which
  principal. It lives in `/etc/profile.d/sparkbox-openai.sh` rather than
  `/etc/environment` for the reason federation.md spells out: a JSON value
  cannot ride that file safely.

None of these is a secret. They name a provider and a rule that grant nothing
without an assertion this fleet's OIDC key signed, which is why they ride a
ConfigMap rather than Secret Manager.

**Half-configured is worse than off.** A guest that exports
`OPENAI_IDENTITY_TOKEN_FILE` with no rule or provider to present the assertion
against leaves Codex *worse* than untouched: it federates, it fails, and it
never falls back to the login the user could have completed. So an `openai`
entry needs at least `OPENAI_FEDERATION_RULE_ID` or
`OPENAI_IDENTITY_PROVIDER_ID` in its `env` — and dropping the entry from the
list is what turns it off cleanly, because the guest removes the whole block on
its next refresh.

### Egress

`deploy/sluice-allowlist.txt` gained `api.openai.com`, `auth.openai.com` and
`chatgpt.com`; `netrules.TrustedDomains` (the console's prefill and a new
environment's default rule-set) gained the same three.
**`auth.openai.com` is the one that matters** — it is where the assertion is
exchanged, so without it a tagged sandbox fails closed and `codex` falls back to
asking for a login. Note the seed file is only written when absent, so a fleet
that already has one needs the three lines added by hand plus
`systemctl reload-or-restart sluice`.

## What to ask an OpenAI organization admin for

They need Admin Portal access; none of this can be done from our side. It is
two objects and then four identifiers handed back.

**1. A Workload Identity Provider**

| Field | Value |
|---|---|
| Type / preset | **Custom OIDC** |
| Name | `sparkbox-catnip` (anything stable) |
| Issuer URL | `https://oidc.catnip.sh` — exactly, no trailing slash |
| Audience | `https://api.openai.com/v1` |
| JWKS | leave on **standard OIDC discovery** — the issuer is public and serves both documents |
| Max assertion lifetime | `3600` seconds |
| **Prevent assertion replay** | **OFF** |

Replay protection off is a deliberate ask, not an oversight. It consumes the
`jti` at each exchange, and the docs are explicit that even `codex login status`
burns one — so with a token file refreshed every 45 minutes, the second
concurrent process in a sandbox fails. What it would buy is small here: the
assertion never leaves the VM, lives an hour, is scoped to one audience, and is
readable only by the login user in a 0700 directory. If they insist on it, say
so and the guest can mint per-exchange instead, which is a real change rather
than a flag.

**2. A federation rule (mapping) on that provider**

| Field | Value |
|---|---|
| Subject match | `sparkbox:user:*` (prefix) |
| Audiences | `https://api.openai.com/v1` |
| Workspace | the managed workspace Codex should bill to |
| Principal | one ChatGPT user or service account |
| Access token lifetime | `600` seconds is plenty |

One rule accepting many subjects and mapping to one principal is supported, and
it is what makes a single fleet-wide `OPENAI_FEDERATION_RULE_ID` work. They can
narrow further with exact claims or a CEL condition over `assertion`, using any
claim in the table above — e.g. `assertion.github == "vanpelt"`, or
`assertion.image == "universal"`.

They also need to confirm the org's **login policy permits ChatGPT
authentication and includes that workspace**, or every exchange fails at the
last step.

**3. Hand back**

The provider id (`idp_...`), the rule id (`idpm_...`), the service account id
(`svc_...`), and — easiest — the `workload-identity-idpm_*.env` file the portal
offers at the end of the flow.

### Per-seat billing was considered and rejected

One shared service account is the decision, not a first cut. Per-seat would mean
one rule per person, each mapping to that person's ChatGPT user, and OpenAI's
quotas make that a dead end: **a provider may hold at most 50 non-archived
rules, and an organization at most 50 non-archived providers.**

The ceiling is therefore 2,500 rather than 50 — you can shard people across
providers — but sharding makes it *worse*, not better. A sandbox would need a
per-owner `OPENAI_IDENTITY_PROVIDER_ID` as well as a per-owner
`OPENAI_FEDERATION_RULE_ID`, so the fleet would be maintaining a handle → (
provider, rule) map that has to be edited on every hire and every departure, and
a person whose rule was never created gets no useful error — just a failed
exchange. That is a lot of moving parts to buy an attribution we already have.

Because attribution does not depend on the principal. Every exchange carries the
`sub` claim (`sparkbox:user:<handle>`) into OpenAI's own audit trail, and we
attach `owner`, `box` and the sandbox name as
`OPENAI_WORKLOAD_IDENTITY_CONTEXT`. What one principal costs is per-seat
*billing* and per-seat rate limits, not knowing who did what.

`external_subject` accepts an exact `sub` or **one trailing-`*` prefix** (up to
4,096 bytes), which is exactly the shape `sparkbox:user:*` needs — so the
single-rule design is on the documented happy path rather than leaning on
anything. A rule may additionally match up to 32 exact top-level scalar claims
(`sub` excluded) or a CEL condition.

If per-seat is ever genuinely wanted, the hook is small: the metadata service
already knows the caller's owner, so it is a per-owner `env` on the `openai`
entry and a handle → rule map. The quota, not the plumbing, is what makes it a
bad idea.

## Turning it on

Copy `deploy/kubernetes/federation.example.json`, fill in the three ids, and:

```
sparkbox federation check federation.json      # optional; deploy.sh runs it too
deploy/kubernetes/deploy.sh --image ... --federation-config federation.json
```

The list is stored in the `sparkbox-federation` ConfigMap and **carried
forward** on a later re-run by simply not being touched, like `--hivemind-api`
and unlike `--hivemind-manifest`: it is a permanent fact about the deployment.
Every run echoes the live list, and says so when `openai` is missing from it,
because the symptom of losing it is remote — nothing fails at deploy time, and
days later somebody's `codex` asks them to log in.

Running sandboxes pick it up on their next 45-minute refresh; new ones get it at
boot. Nothing needs a rebuild: the audience, the identifiers and the token path
are all served, never baked. Templates do need one `refresh-agent-tools.sh` pass
for `IDENTITY_REV=28`, which the daily timer performs on its own.

## Verifying

From inside a sandbox:

```
cat /run/sparkbox/federation             # the list, as served; openai's lines among it
ls -l /var/run/secrets/openai.com/       # 0700 dir, 0600 token, owned by you
codex login status                       # "Logged in using workload identity"
codex exec "Reply with only: workload identity is working"
```

`sparkbox whoami` prints the claims OpenAI is matching a rule against.

A failure at the exchange reports `invalid_grant` when an attribute condition
rejected the assertion and `invalid_subject_token` when verification itself
failed — the first is their rule, the second is our issuer.

## Tests

The mechanism's tests are listed in [federation.md](federation.md). The
OpenAI-specific ones:

- `internal/federation` — `federation.OpenAI` exports exactly what Codex and
  the SDKs read, and an id the admin did not hand back is not exported empty.
- `deploy/federation_test.go` — the real `sparkbox-federation-env` against a
  tree with the OpenAI entry, in both identity encodings: the four variables,
  the attribution context surviving being sourced by a shell and staying under
  OpenAI's 1,024-byte cap, and the block and snippet going away when the entry
  leaves the list. Every branch was also exercised under `dash`, which is the
  `/bin/sh` a real sandbox runs these with.
- `deploy/federation_test.go` — the example file loads and its `openai` entry
  is `federation.OpenAI`'s.
