# Signing in to sparkbox from HiveMind

A spike: what it would take for someone already signed in to the HiveMind app
at `wandb.hivemind.tools` to click a link and arrive at `my.catnip.sh` — or at
a sandbox — as themselves, with an account created for them if they had none.

This extends `docs/github-linking-design.md` Part 3, which designed a HiveMind
assertion that *links a GitHub login to an existing account*. This is the
larger ask: the assertion also has to be able to **create** the account and
**establish the browser session**.

---

# Part 1 — what already exists, on both sides

The two systems already federate. In one direction only, and the machinery of
that direction is most of what this needs.

## 1.1 sparkbox → HiveMind (shipped)

`internal/oidc` mints an ES256 id token per sandbox with `sub =
sparkbox:user:<handle>` and a `github` claim. HiveMind's
`oidc_providers.SPARKBOX_KIND` declares that shape, `TRUSTED_OIDC_PAT_ISSUERS=
sparkbox=oidc.catnip.sh` turns it on, and `partner_federation.py` exchanges the
token at `POST /v1/auth/actions/exchange` into a personal credential. Its
docstring is explicit about the trust root: *"`github` is the anchor. Sparkbox
emits it only after verifying the login controls a key published at
`github.com/<login>.keys`"*. And: **"The developer must already have a Hivemind
account; we resolve, never create."**

`internal/hivemindpresence` already dials HiveMind from the gateway over that
exchange (`--hivemind-api`, live on CKS). So a sparkbox → HiveMind HTTPS path
exists today and is exercised every minute.

## 1.2 The sparkbox browser session

One artefact, `internal/edgeauth`: a `spark_session` cookie holding
`spk_v1.<base64 claims>.<HMAC>`, keyed by HKDF from the fleet OIDC key, scoped
to `.<domain>` so it covers every subdomain. It asserts exactly `{handle,
email, iat, exp}` and nothing else. Three things mint it today — the pasted
`ssh ctl@<gw> session-token`, a passkey assertion, and nothing else — and all
three converge on `LoginHandler.setSessionCookie`.

Two pieces of that handler are load-bearing for this design and already
written: `safeReturn` (login.go:209), which refuses any return URL outside
`.<domain>`, and the one-time `/enroll` offer that fires when an account has no
passkey yet.

## 1.3 How a sparkbox account comes to exist

| door | who authorises | initial key |
| --- | --- | --- |
| `users.conf` seed | whoever provisions the host (→ operator) | from the file |
| `ssh signup@` | an invite code, or `--open-signup` | the connecting key |
| `ctl user add <login>` | an operator naming a colleague | `github.com/<login>.keys` |
| `ctl user sync-github-org` | an operator + their `read:org` token | same |

Every one of them ends at `users.Store.Create(handle, key, …)`, which **takes a
key and cannot be called without one**. `RemoveKey` then refuses to remove the
last one. There is no keyless account in this schema today, and a browser-only
sign-in produces exactly that.

`ctl user add` is the closest existing precedent and the most useful one. It is
an operator asserting a GitHub login, letting GitHub supply the keys, and it
records the resulting link as `github-keys` — **strong** provenance — because
the account's only credentials *are* the keys GitHub publishes for that login.
`users.HandleForGitHubLogin` already derives the handle.

## 1.4 Provenance, and the thing it gates

`users.GitHubVia` is `github-keys` | `device-flow` | `assertion`.
`StrongGitHubLink` admits the first two. `assertion` is defined, documented,
and **written by nothing** — it was reserved for precisely this handshake.

What a weak link costs is concrete, not theoretical:

- `internal/metadata/server.go:249` keeps it out of the `github` claim, so a
  sandbox owned by an assertion-linked account federates to HiveMind with no
  anchor and `partner_federation` fails closed.
- `internal/ctlops/repo.go:329` and `internal/metadata/repos.go:233` refuse to
  attach or serve GitHub repositories without a strong link.

And `internal/launch` (the `go.<domain>` door) resolves entirely through repo
*attachments*. So:

> **An account created from an assertion alone can sign in, but it cannot
> attach a repository, which means the `go.` path — the exact destination this
> handshake most wants to offer — dead-ends on the "attach it first" screen.**

That is the single most important finding in this spike. The handshake is not
finished when the session cookie is set.

## 1.5 What HiveMind knows, and what it can sign with

- The dashboard session is an HS256 JWT in an `agentstream_token` cookie,
  signed with the shared `JWT_SECRET`. **Symmetric — sparkbox can never verify
  it, and must never hold the key that could.** Any handshake needs new
  material.
- The user row carries `username` (the GitHub login), `github_id`, and
  `github_orgs`, kept current by the `refresh_user_orgs` worker, which re-checks
  membership through the GitHub App installation and *removes* orgs the user has
  left. The MVP gate — `"wandb" in github_orgs` — is a live, maintained fact,
  not a signup-time snapshot.
- HiveMind's GitHub identity comes from its own GitHub OAuth login
  (`handlers/auth.py`), which is a direct proof from GitHub about the human at
  the keyboard.
- **Asymmetric signing already exists there**: `licensing/` mints Ed25519 JWTs
  with a `kid`, public halves committed as `keys/<kid>.pem`, private half in
  Cloud KMS via `asymmetricSign`, rotation documented. A signing key for this
  handshake is a second instance of a pattern that already ships, not a new one.

---

# Part 2 — the shape

The front channel is settled: an **auto-submitting form POST** from HiveMind to
sparkbox, as the user proposed. Not a redirect with a query parameter. The
credential stays out of `Referer`, out of browser history, out of every access
log between here and there, and out of the URL bar a user might paste into a
ticket. This is the SAML POST binding for the same reason SAML uses it.

What travels in that POST is the real question. Two candidates.

## 2.1 Option A — a signed assertion (self-contained)

HiveMind mints a short-lived Ed25519 JWT and posts it. sparkbox verifies the
signature against a pinned public key or a JWKS at a pinned URL.

```
POST https://login.catnip.sh/handoff
  assertion=<jwt>
```

```json
{
  "iss": "https://wandb.hivemind.tools",
  "aud": "https://login.catnip.sh",
  "sub": "hivemind:user:<user_id>",
  "github": "vanpelt", "github_id": 12345,
  "github_verified_via": "github-oauth",
  "orgs": ["wandb"],
  "email": "vanpelt@wandb.com",
  "dest": "https://my.catnip.sh/",
  "jti": "…", "iat": …, "exp": iat+120
}
```

**For**: no callback, so it works even if the gateway cannot reach HiveMind;
verification is offline and fast; it reuses HiveMind's existing licensing key
pattern; it is the shape `github-linking-design.md` §3.3 already argued for.

**Against**: a key to distribute, pin, and rotate; a `jti` replay cache to
build; clock skew to tolerate; and the assertion is a bearer credential that is
briefly valid for *anyone who obtains it*.

## 2.2 Option B — a one-time code, redeemed back-channel (recommended)

HiveMind posts an opaque single-use code with a 60-second life. sparkbox
redeems it over the HTTPS path it already has:

```
POST https://login.catnip.sh/handoff        (browser, form)
  code=hmh_<32 bytes base64url>

POST https://<hivemind-api>/v1/handoff/redeem   (server to server)
  {"code": "hmh_…"}
  → 200 {github, github_id, orgs, email, dest, sub}   — and the code is burned
```

**For**: **no new signing key, no JWKS, no pinning, no rotation, no replay
cache, no clock skew.** Single-use is enforced by the one party that can
enforce it. The code is worthless to a thief the moment it is redeemed, and it
is never valid twice. Revocation is `DELETE` on a Redis key. HiveMind can
observe every redemption, which is the audit trail this handshake wants anyway.
And it is *strictly less code on both sides* — the whole of §2.1's crypto
becomes a Redis `SETEX` and a `GETDEL`.

**Against**: it needs sparkbox → HiveMind egress at sign-in time. That is
already a hard dependency of the CKS deployment (`--hivemind-api`,
`internal/hivemindpresence`), so this is a dependency we already run on, not a
new one. If HiveMind is down, sign-in through *this door* fails — but the
`ssh ctl@ session-token` and passkey doors are untouched, so nobody is locked
out of sparkbox by a HiveMind outage.

**Recommendation: B for the MVP.** It is the same OAuth authorization-code
shape everyone already reasons about, it invents no trust root that isn't
already there, and the whole of Option A remains available later as a
performance or airgap variant — the receiving handler's logic is identical
either way, only the "how do I believe this" step changes.

---

# Part 3 — what sparkbox does when the POST lands

Mount it on the **login handler**, at `login.<domain>/handoff`. Not on `go.`,
and not as its own subdomain. Everything this needs is already in that file:
`setSessionCookie`, `safeReturn`, the passkey enrollment offer, the account
store, and the signer. A second place that mints `spark_session` is exactly the
kind of duplication that drifts.

The order of operations:

1. **Already signed in?** If a valid `spark_session` is present *and* its handle
   is the account the handshake resolves to, do nothing but redirect to `dest`.
   This is the user's "just proceed to where the initiation signaled".
   If it resolves to a *different* account, do **not** silently swap it — see
   §4.1.
2. **Verify the material.** Redeem the code (or check the signature). A failure
   here renders the ordinary login page with a neutral message; it never says
   which half failed.
3. **Authorize.** `"wandb" ∈ orgs`, compared case-insensitively, against a
   configured allowlist (`--hivemind-orgs wandb`), not a literal in the code.
   An empty allowlist disables the door outright rather than admitting
   everyone.
4. **Resolve the account.** `handle := users.HandleForGitHubLogin(github)`.
   - Account exists and its `GitHubLogin` matches (case-insensitively) → this
     is them.
   - Account exists and its login is *different or empty* → **refuse**. This is
     `provisionOne`'s rule (user.go:270) and it is the one that keeps a handle
     collision from being an account takeover.
   - No account → create (§3.1).
   - Account is `disabled` → refuse, the same 403 `edgeauth.Require` gives.
5. **Mint and land.** `setSessionCookie`, then redirect to `safeReturn(dest)`,
   via the `/enroll` passkey offer on first sign-in exactly as the token path
   does — a passkey is what lets them come back without HiveMind in the loop.

## 3.1 Creating the account, and the key problem

`users.Store.Create` needs a key. Two ways to satisfy it, and the right answer
is to try them in order:

**First: adopt `github.com/<login>.keys`.** This is `ctl user add`, with
HiveMind's assertion standing where the operator's typed login stands. It
yields an account that is immediately usable over SSH *and* — because its only
credentials are the keys GitHub publishes for that login — a link honestly
recorded as `github-keys`, which is **strong**, which restores repository
attachment and the `github` claim, which makes the `go.` destination actually
work. The assertion was an accelerator, exactly as §3.3 of the linking design
said it should be. `InvitedBy` is a synthetic `hivemind` inviter, never
`users.OperatorInviter` — that constant carries node administration and
cross-tenant route visibility.

**Second, when GitHub publishes no key:** create the account keyless, record
the link as `assertion`, and say so. This needs `users.Store` to grow a
`CreateKeyless` (the schema already keeps keys in their own table; the
constraint is in the function signature, not the database) and needs `RemoveKey`
to keep its last-key rule only for accounts that have another credential. Such
an account can sign in to the console and hold secrets, but it cannot attach a
repository until it completes `ctl github link` (device flow) or publishes a
key — and the console should say that in one sentence rather than letting them
find out at the `go.` door.

## 3.2 What must NOT happen

`assertion` provenance must not reach the `github` claim.
`metadata/server.go:249` already enforces this and its comment already explains
why: *"if that third party is also the one reading the policy it would be
authorizing against a fact it asserted"*. HiveMind is precisely that third
party — `partner_federation` anchors on `github`. **Do not relax that gate as
part of this work.** The §3.1 ordering is what makes the gate cost nothing in
the common case: the strong link comes from GitHub's published keys, not from
HiveMind's word.

---

# Part 4 — hazards

## 4.1 Login CSRF — the structural cost of IdP-initiated SSO

An attacker who holds a valid handoff code for *their own* account can make a
victim's browser POST it, silently re-pointing the victim's sparkbox session at
the attacker's account — and then the victim's next `ctl secret set`, their next
sandbox, their next repository attachment happens in a box the attacker can
read. This is not exotic; it is the known weakness of every unsolicited-POST
SSO binding.

Three mitigations, and the MVP should take the first two:

1. **Never silently swap an existing session.** If a valid `spark_session` for
   a *different* handle is present, render an interstitial: "You are signed in
   as X. Continue to Y as Z?" — a form the user submits. Costs one click, only
   in the rare case, and turns a silent hijack into a visible question.
2. **Bind the code to the destination.** `dest` is fixed inside the code's
   server-side record at mint time, not read from the POST body. An attacker
   cannot then aim a stolen handoff anywhere they like.
3. (Later) sparkbox-initiated flow — sparkbox sets a state nonce and bounces to
   HiveMind — which removes the class entirely, at the cost of the "click a
   button in HiveMind" ergonomics that motivate this.

## 4.2 Open redirect

`dest` reaches a `Location:` header. Run it through the existing
`safeReturn`, which already refuses anything outside `.<domain>` and any scheme
but https. Do not write a second one.

## 4.3 CSRF middleware will refuse this POST, correctly

`internal/launch/csrf.go` and `edgeauth.RequireMutation` both demand a
first-party `Origin`. This POST's `Origin` is `https://wandb.hivemind.tools` by
design. The handoff endpoint must be explicitly exempt — and that exemption is
sound for a reason worth writing down at the call site: **CSRF protection
exists because a cookie is ambient authority; this request's authority is in
its body and its body is single-use.** Pin the accepted `Origin` to the
configured HiveMind origins anyway, as defence in depth.

Note also `sparkbox-launch-door`'s scar: `Referrer-Policy: no-referrer` makes
Firefox send `Origin: null` and 403 every form POST. Whatever this endpoint
does about `Origin` must be tested in Firefox, not only Chrome.

## 4.4 Membership is a snapshot

`refresh_user_orgs` keeps `github_orgs` fresh, so the gate is good at sign-in
time. Nothing revokes a sparkbox account when someone leaves the org — the
`users.StatusActive` deprovisioning problem `internal/users/githuborg.go`
already names. This handshake does not make it worse, and it does not fix it.
Say so out loud rather than implying membership is enforced continuously.

## 4.5 Scale

The `wandb` org has ~617 members (`sparkbox-user-onboarding`). Every one of them
can now mint themselves an account by clicking a link. Accounts are cheap;
**sandboxes are 25GB each**. The `go.` door already gates creation behind a repo
attachment and per-owner quota, but this is the first door that lets the
population of a whole GitHub org self-serve, and the quota numbers were not
chosen with that in mind.

---

# Part 5 — what shipped, on both sides

M1 is built on both sides — sparkbox on branch `feat/hivemind-signin`
(stacked on `feat/launch-and-terminal-polish`), HiveMind on `agentstream-py`
branch `feat/sparkbox-handoff`. Nothing is deployed. A gateway configured for
this without `--hivemind-api` warns and does not mount the door.

The two halves have been run against each other: `internal/hivemindsignin`'s
real client redeeming a real code from HiveMind's real route, over loopback —
claims parsed, second redemption refused, unknown code refused. What has *not*
happened is a browser going all the way through, because that needs both sides
deployed.

## 5.1 The sparkbox half (built)

| where | what |
| --- | --- |
| `internal/edgeauth/handoff.go` | the door: `POST /handoff` and `/handoff/confirm`, the interstitials, the org gate, the refusal page |
| `internal/edgeauth/ticket.go` | the signed, short-lived, purpose-separated credential that carries a redeemed handoff across an interstitial |
| `internal/hivemindsignin` | the back channel — one POST, no state, no cache |
| `internal/ctlops/federated.go` | `AdmitGitHubLogin`: resolve or create the account, adopt published keys, record the honest provenance |
| `internal/users` | `CreateKeyless`, and the last-key rule relaxed to mean "last *credential*" |
| `cmd/sparkbox`, `deploy/kubernetes` | `--hivemind-signin-orgs`, carried forward on a CKS re-run like `--hivemind-api` |

Three decisions worth knowing without reading the code:

- **The door is on the login handler**, at `login.<domain>/handoff`, because
  `setSessionCookie`, `safeReturn` and the passkey-enrollment offer are already
  there and a second copy of any of them is how one door ends up scoped to the
  zone and another quietly is not.
- **It applies no CSRF check, deliberately.** Those checks stop a cross-site
  page spending *ambient* authority; this request's authority is a single-use
  code in its body. An `Origin` check would read as a guarantee it cannot give
  (an attacker holding a code sends any Origin they like) and would break on
  Firefox's `Origin: null` under `Referrer-Policy: no-referrer` — the scar
  `internal/launch` already carries. The reasoning is written at the top of
  `handoff.go` so it is not re-derived wrongly later.
- **The provenance gate was not touched.** A keyless account records
  `assertion`, `users.StrongGitHubLink` still refuses it, and
  `internal/metadata/server.go:249` still keeps it out of the `github` claim —
  so HiveMind never ends up authorizing against a fact it asserted. The §3.1
  ordering is what makes that gate cost nothing in the common case: the strong
  link comes from `github.com/<login>.keys`, not from HiveMind.

## 5.2 The HiveMind half (built)

Two endpoints and a button, in `agentstream-py` on branch
`feat/sparkbox-handoff`. Its own account of this is `docs/sparkbox-handoff.md`
there; what follows is the contract, which is what this repository depends on.

| where | what |
| --- | --- |
| `backend/api/src/agentstream_api/sparkbox.py` | config, destination validation, the one-time code store (Redis `SETEX` 60s / `GETDEL`, memory fallback) |
| `backend/api/src/agentstream_api/handlers/sparkbox_handoff.py` | `/v1/handoff/config`, `/start`, `/redeem` |
| `backend/dashboard/src/pages/settings/Integrations.tsx` | the button, in Settings → Integrations |

**`POST /v1/handoff/start`** — authenticated by the dashboard session. Mints a
code against `{sub, github, github_id, orgs, email, dest}` under a 60-second
TTL, and returns the code plus this door's URL. `dest` is fixed **there**, from
what the button was for, and never read back out of the browser — the half of
the login-CSRF mitigation (§4.1) HiveMind owns. It refuses a service account
and refuses an impersonating admin, either of which would otherwise create an
account here in a name that is not theirs.

**`POST /v1/handoff/redeem`** — unauthenticated; the code in the body is the
authentication.

```jsonc
// request
{"code": "hmh_<32 bytes base64url>"}

// 200
{
  "sub": "hivemind:user:usr_…",  // audit only; sparkbox resolves on `github`
  "github": "vanpelt",           // REQUIRED — absent fails the sign-in closed
  "github_id": 12345,
  "orgs": ["wandb"],
  "email": "vanpelt@wandb.com",  // optional
  "dest": "https://my.catnip.sh/"
}
```

Unknown, spent and expired all answer **410** — one outcome, because the
browser holding the code cannot act on the difference. Nothing about a bad code
answers 5xx, which this side reads as "try again" rather than "start again";
that is why `code` carries no length limit in the request model, so an
over-long one is a 410 and not pydantic's 422.

**The button** posts an auto-submitted form to `login.<domain>/handoff` with
one hidden `code` field.

Two operator notes:

- Enabling is `SPARKBOX_DOMAIN=catnip.sh` (empty disables everything), and it
  should be turned on only *after* a gateway is running with
  `--hivemind-signin-orgs` — otherwise the button posts at a door that is not
  mounted.
- `SPARKBOX_SIGNIN_ORGS` over there decides who is *shown* the button. It is a
  display hint that should be kept equal to `--hivemind-signin-orgs`; the gate
  stays here, because this is the side that creates accounts.

## 5.3 M2 — the `go.` destination

`dest = https://go.catnip.sh/<owner>/<repo>` should work the moment a strong
link lands, because the launch door's own gate is `edgeauth.Require`. HiveMind
can now ask for it — `POST /v1/handoff/start` takes `{"repo": "owner/name",
"ref": "branch"}` and resolves it to that URL at mint time — but no button
sends it yet, and nothing has been through end to end. Do not assume it.

---

# Part 6 — decisions this spike did not make, and where they landed

1. **Is HiveMind's word enough to create an account with no published key?**
   Answered yes-but-weak, in code: the account exists, `assertion` is recorded,
   and the visitor is told on a notice page that sparkbox cannot attach GitHub
   repositories for them yet and how to fix it.
2. **Does a handoff extend an existing session's TTL?** A handoff onto a session
   that is already the right account does nothing but redirect — no admission,
   no re-mint, no github.com round trip. So: no.
3. **Should `assertion` ever reach `StrongGitHubLink`?** No, and
   `TestAdmitCreatesKeylessWhenGitHubPublishesNothing` now fails if it does.
4. **Deprovisioning.** Still out of scope, still the gap
   `internal/users/githuborg.go` names, and this door is the first thing that
   makes it load-bearing.
