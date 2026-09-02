# OpenAI workload identity federation

Status: **implemented** (2026-09-02), **not yet deployed** — nothing is on until
a deploy passes `--openai-provider-id` / `--openai-rule-id`, and those come from
an OpenAI organization admin.

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

### Host

`internal/metadata/openai.go` — `metadata.OpenAI`, the fleet's federation
configuration, served to guests at `GET /openai` and answering **501** on a
fleet that has not configured it. Four flags feed it, on both the gateway and
the node (`cmd/sparkbox/main.go`, `cmd/sparkbox/node.go`):

```
--openai-provider-id          idp_...    OpenAI Workload Identity Provider
--openai-service-account-id   svc_...    service account the rule maps to
--openai-rule-id              idpm_...   federation rule; the one codex needs
--openai-audience                        default https://api.openai.com/v1
```

None of these is a secret. They name a provider and a rule that grant nothing
without an assertion this fleet's OIDC key signed, which is why they ride the
deployment environment rather than Secret Manager.

Two decisions worth keeping:

- **A provider or rule id is what turns the feature on — an audience is not.**
  A guest that exported `OPENAI_IDENTITY_TOKEN_FILE` with no rule to present the
  assertion against would leave Codex *worse* than untouched: it federates, it
  fails, and it never falls back to the login the user could have completed.
- **The audience joins `--oidc-audiences` by construction** (`withOpenAIAudience`),
  rather than by the operator remembering to repeat it. Those are two statements
  of one fact, and the failure when they disagree is remote from its cause:
  everything configures cleanly and guests take a 400 from a mint nobody is
  watching. It never widens an *empty* allowlist, because empty means "any" and
  appending would silently narrow it.

A node signs nothing — `Issue` relays to the gateway, whose allowlist is what
can refuse an audience — so the node takes these flags purely as configuration
to hand its own guests, exactly as it already takes `--hivemind-audience`.

### Guest (`deploy/install-guest-identity.sh`, `IDENTITY_REV=25`)

`sparkbox-token`, which already refreshes the HiveMind assertion on boot and
every 45 minutes, gained a second half:

1. `GET /openai`; a 501 erases `/run/sparkbox/openai.json` and stops here.
2. `GET /token?aud=<audience>` → `/var/run/secrets/openai.com/identity-token`,
   mode 0600, in a 0700 directory owned by the login user. That is the path and
   the permissions OpenAI's own guidance asks for.
3. `sparkbox-openai-env` renders the managed block in `/etc/environment`.

It runs **last**, and that ordering is load-bearing: the HiveMind fetch above
exits 1 on failure and is what a sandbox cannot work without, so an integration
a fleet may not even have configured must never be able to reach that exit.

`sparkbox-openai-env` is called **unconditionally**, including when the fetch
found nothing — that call is what removes a stale block from a box whose fleet
turned federation off, and from a fork template that crossed fleets.

Refresh cadence is unchanged at 45 minutes against a 1-hour assertion, the
kubelet's shape for projected tokens.

### The environment split, and why there are two files

`/etc/environment` carries the credentials:

```
OPENAI_IDENTITY_TOKEN_FILE=/var/run/secrets/openai.com/identity-token
OPENAI_FEDERATION_RULE_ID=idpm_...
OPENAI_IDENTITY_PROVIDER_ID=idp_...
OPENAI_SERVICE_ACCOUNT_ID=svc_...
```

It is the only file that reaches both an interactive shell (pam_env) and the
non-interactive `ssh box '<cmd>'` execs agents actually run, which read no
profile at all.

`/etc/profile.d/sparkbox-openai.sh` carries the optional audit attribution:

```sh
export OPENAI_WORKLOAD_IDENTITY_CONTEXT='{"instance_id":"dazzling-canyon","labels":{"owner":"vanpelt","box":"..."}}'
```

The split is forced, not chosen. That value is a JSON object, so it contains
double quotes, and `/etc/environment` cannot carry one safely: written bare it
survives pam_env but a shell sourcing the file performs quote removal on it —
measured, not feared, `{"instance_id":"box"}` comes back as `{instance_id:box}`,
which is no longer JSON — and written quoted it depends on pam_env stripping the
pair back off, which is not a promise that file format makes. A profile.d
snippet is unambiguously shell.

The cost is that attribution reaches login shells and not `ssh box '<cmd>'`.
That is the right way round for a field OpenAI documents as optional: the
credentials that *must* reach both readers are plain identifiers with no
quoting problem at all.

### Egress

`deploy/sluice-allowlist.txt` gained `api.openai.com`, `auth.openai.com` and
`chatgpt.com`; the user console's trusted-domains prefill gained the same three.
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
already knows the caller's owner, so it is a field on `metadata.OpenAI` and a
handle → rule map. The quota, not the plumbing, is what makes it a bad idea.

## Turning it on

```
deploy/kubernetes/deploy.sh --image ... \
  --openai-provider-id idp_... \
  --openai-service-account-id svc_... \
  --openai-rule-id idpm_...
```

All four are **carried forward** on a later re-run, like `--hivemind-api` and
unlike `--hivemind-manifest`: they are a permanent fact about the deployment, so
dropping one is always a mistake and the symptom is remote — nothing fails at
deploy time, and days later somebody's `codex` asks them to log in.

Running sandboxes pick it up on their next 45-minute refresh; new ones get it at
boot. Nothing needs a rebuild: the audience, the identifiers and the token path
are all served, never baked. Templates do need one `refresh-agent-tools.sh` pass
for `IDENTITY_REV=25`, which the daily timer performs on its own.

## Verifying

From inside a sandbox:

```
cat /run/sparkbox/openai.json            # the fleet's configuration
ls -l /var/run/secrets/openai.com/       # 0700 dir, 0600 token, owned by you
codex login status                       # "Logged in using workload identity"
codex exec "Reply with only: workload identity is working"
```

`sparkbox whoami` prints the claims OpenAI is matching a rule against.

A failure at the exchange reports `invalid_grant` when an attribute condition
rejected the assertion and `invalid_subject_token` when verification itself
failed — the first is their rule, the second is our issuer.

## Tests

- `internal/metadata/openai_test.go` — the endpoint, its defaults, its 501, and
  that an audience alone does not enable anything.
- `cmd/sparkbox/openai_test.go` — the audience reaches the issuer allowlist, is
  not added twice, and never narrows an empty one.
- `deploy/openai_test.go` — runs the real `sparkbox-openai-env` against a tree,
  in both JSON encodings, and asserts the attribution context survives being
  sourced by a shell. Every branch was also exercised under `dash`, which is the
  `/bin/sh` a real sandbox runs these with.
