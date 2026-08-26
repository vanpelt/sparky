# GitHub webhooks

There is a receiver — `internal/ghwebhook`, mounted at `hooks.<domain>/github` on
the gateway. It proves a delivery came from GitHub, writes one line about it, and
answers 204. Nothing else in the platform changes because a webhook arrived: no
cache is invalidated, no attachment is rewritten, no token is revoked.

So the interesting question is not how to paste a URL into a form. It is why this
platform should hear from GitHub at all, given that everything it needs it can
already pull. That argument is Part 1, the honest limits of it are Part 2, and the
events worth subscribing to are Part 3. The form-filling is Part 4.

---

# Part 1 — the argument: three caches are the revocation boundary

Every repository credential a sandbox gets is decided by three in-memory caches on
the gateway (`internal/ghapp`). `metadata.Local.Credential` walks all three, in
this order, on every single request: resolve the installation, authorize the
handle against it, mint or reuse a token.

| Cache | Key | Lifetime | What it decides |
|---|---|---|---|
| `insts` (`InstallationFor`) | lowercased `owner/name` | `installTTL` = **10 minutes** | which installation covers a repository, and therefore which account or org the binding is checked against |
| `members` (`membership`) | `<installation id>/<org>/<login>` | `membershipTTL` = **10 minutes** | whether an org installation may be used on this handle's behalf. **Both answers are cached** — the negative as well as the positive |
| `tokens` (`token`) | `<installation id>\0<repos>\0<perms>` | until `tokenRefreshLead` = **5 minutes** before GitHub's expiry, so about **55 minutes** of a one-hour token | the credential itself |

None of these is an optimization that could be turned off. The comment at the top
of `cache.go` says so out loud: a guest's `git fetch` loop runs on the path
guest → node → gateway → github under a 10-second write timeout, and a mint per
fetch would spend that budget on GitHub's rate limiter. They are load-bearing, they
are staying, and so their TTLs are the platform's revocation latency.

Make it concrete. Someone is removed from the `wandb` org at 12:00.

`Authorize` runs on every credential request, so it is not that the check is
skipped — it is that the check is answered from an entry filled up to ten minutes
ago. GitHub started saying "not a member" at 12:00; sparkbox goes on saying
`active` until **12:10** at the latest. Inside that window a sandbox belonging to
that person can still mint a *fresh* token, and a fresh token is good for an hour
from the moment it is minted. Worst case, an ex-member's agent holds working
repository credentials until **13:10** — ten minutes of continued minting plus
sixty minutes of the last thing minted.

Uninstalling the App has the same shape through `insts`, and pulling one repository
out of an installation the same shape again: ten minutes during which the gateway
answers from a snapshot of a world that changed.

Nothing else covers this window. There is no background re-sync of installations or
memberships; `ctl user sync-github-org` is operator-run and provisions *accounts*,
not repository authorization. Failures are never cached, so waiting does not help —
and a gateway restart, which does flush all three, is not an instrument anyone
should be reaching for to deprovision one person.

**That is the whole argument for a webhook.** It adds no capability. It replaces a
ten-minute timer with a message: GitHub delivers within seconds of the click that
caused the change, and a handler that drops the matching cache entry turns "up to
ten minutes" into "as long as the delivery takes". Everything in Part 3 is a
variation on that one sentence.

(The nuclear option is already instant and stays that way: regenerating the App's
private key in its settings kills every copy immediately, because GitHub verifies
each assertion against the keys currently registered. It is per-user revocation
that is slow, and per-user revocation is the common case.)

---

# Part 2 — what a webhook cannot fix

A token that has **already been minted lives up to an hour, and nothing described
here shortens that.**

The reason is structural rather than an omission. An installation access token
belongs to the *installation*, not to a person: it was minted for installation
#12345 scoped to `wandb/hivemind` with `contents: read`. GitHub has no idea that
sparkbox handed it to `vanpelt`, so removing `vanpelt` from the org is not an event
that could invalidate it even in principle. Dropping our cached copy does not help
either — the guest already has it. The credential helper handed it to `git`, and by
design it was never ours to take back.

Closing that second window is possible and is not built. It would mean tracking
minted tokens and calling `DELETE /installation/token` for each one, authenticated
as the token being revoked. Some of the substrate is already there: the `tokens`
cache key begins with the installation id, so "every token for installation N" is a
prefix scan, and the same is true of `members`. What is missing is a way to delete
from the cache at all (it has `get` and eviction and nothing else), the revoke call,
and — the hard part — any record of tokens that have already left the cache through
eviction, expiry-of-servability, or a gateway restart. Keeping such a record means
storing live credentials on the gateway, which is precisely what
`github-repos-design.md` §3.4 set out to delete. The honest compromise is
best-effort: revoke what is in memory, and accept a residual hour for the rest.

State it plainly wherever it matters: **a webhook shortens the window in which new
credentials can be minted, from ten minutes to seconds. It does not shorten the life
of a credential a guest already holds.**

---

# Part 3 — which events, and what each one would actually do

GitHub's descriptions of these events are not the question. The question is which
cache entry or which stored row each delivery would let this platform correct.

## Tier 1 — authorization freshness

This tier is the reason to build the thing at all.

| Event | Actions | What it invalidates here |
|---|---|---|
| `installation` | `deleted`, `suspend` | every `tokens` entry with that installation id, and the `insts` entries resolving to it. Without this, the gateway keeps serving a cached token for an installation that no longer exists, for up to 55 minutes |
| `installation` | `created`, `unsuspend`, `new_permissions_accepted` | **nothing** — and that is worth writing down so nobody adds a flush that does not need to exist. Failures are never cached (`cache.get` deletes a failed entry), and neither is the 403 from a missing `members: read`, so the "I just fixed it on GitHub" retry already works on the next request |
| `installation_repositories` | `removed` | the `insts` entry for each named repository, plus that installation's `tokens` entries. Otherwise a repository pulled out of the installation still resolves from cache and the mint fails at GitHub with the 422 that `mintError` reports as "does not cover" |
| `installation_repositories` | `added` | nothing, for the same reason `created` needs nothing |
| `organization` | `member_removed` | the `members` entry whose key is `<installation id>/<org>/<login>`. **This is the wandb case from Part 1 and the single highest-value handler on the list** |
| `organization` | `member_added` | the same entry — because the negative is cached too. A new hire who is refused and retries would otherwise be told for ten minutes that they are not in the org they just joined |
| `member` | `added`, `removed`, `edited` | nothing today. Repository-level collaboration is not part of the binding: a personal installation is bound by account id, an org installation by org membership, and `Authorize` consults neither collaborator list. Subscribe if you want the log line; it becomes load-bearing only if a per-repository rule ever enters `Authorize` |
| `github_app_authorization` | `revoked` (delivered unconditionally — there is no checkbox for it) | every `members` entry ending in `/<login>` — the payload carries only `sender`, which is enough for that sweep. The larger question this event raises, whether a revoked authorization should void a `StrongGitHubLink` and force a re-link, belongs to `github-linking-design.md` and should be decided there rather than inside a webhook handler |

Two of these rows say "nothing". They are in the table on purpose: half the value of
reading the caches before writing handlers is learning which events do not need one.

## Tier 2 — attachment integrity

### `repository` — `renamed`, `transferred`, `deleted`, `archived`

`internal/repos` stores the **slug and only the slug**. The schema in `store.go` is
`slug TEXT NOT NULL COLLATE NOCASE` with `UNIQUE (owner, host, slug)`; there is no
numeric repository id column, and nothing else in the row identifies the repository.
Everything downstream re-derives from that string: `metadata` splits it with
`repos.SplitSlug`, `InstallationFor` takes the two halves as URL path segments, and
`MintToken` puts the bare repository *name* into the `repositories` array.

A rename therefore breaks the attachment, but not where you would first look, so it
is worth being exact about which half survives:

* **The clone survives.** `git` follows GitHub's redirect from the old path, and so
  does the installation lookup — `ghapp` uses a default `http.Client`, which follows
  redirects on GET.
* **The mint does not.** `POST /app/installations/{id}/access_tokens` matches
  `repositories` by name against the installation's *current* repositories. There is
  no redirect there. The old name is not covered, GitHub answers 422, `mintError`
  turns that into `ErrNotInstalled`, and `metadata` reports it to the guest as "no
  such attachment".

The result is the failure mode `github-app-setup.md` already warns about in another
context: it surfaces as an authentication failure on a clone inside a VM at boot,
with the motd reporting a failed clone and nothing anywhere naming the rename that
caused it. Meanwhile `ctl repo ls` still lists the old name and looks perfectly
healthy, because the store is the last thing to find out.

**Would storing the repository id make this self-healing?** Partly, and the part it
does not fix is the interesting one. A `renamed` delivery carries
`changes.repository.name.from` and a `transferred` delivery carries
`changes.owner.from`, so a slug-keyed store can already match the row it needs to
rewrite — an id is not required for the deliveries you receive. What an id buys is
the ability to *reconcile deliveries you missed*: a webhook that was down, a host
that was paused, a secret rotated mid-flight. With the numeric id in the row,
`GET /repositories/{id}` re-derives the current slug from scratch at any later
moment; without it, a row whose repository was renamed while nobody was listening
has no stable handle left to ask about, and only a human can fix it. That is the
argument for adding the column, and it is a schema change worth making *before*
handlers start depending on rename deliveries being complete.

`deleted` and `archived` invalidate nothing — a deleted repository's attachment
simply fails, and an archived one clones fine and refuses pushes. They are the two
events where the missing surface is not a cache but a user-visible one: an
attachment marked broken in `ctl repo ls` and in the console, which is a product
decision, not a webhook one.

### `meta` — `deleted`

The App itself was deleted. There is nothing to invalidate, because every mint from
that moment fails at GitHub anyway, and nothing to repair from this side. It earns
its subscription for one reason: it is the only delivery that explains an entire
fleet's repo feature going dead at once — and it is the last delivery that App will
ever send.

## Tier 3 — product surface, deliberately not now

**`push`** is the tempting one: pull new commits into a running sandbox, or wake a
paused one so it is warm when its owner looks. Note the shape of what that asks for.
A busy monorepo delivers on every push to every branch, and unlike Tier 1 — where a
delivery costs a map lookup — every one of these deliveries is a *request that this
platform do work*: resume a VM, run a fetch inside it, spend disk and CPU. The
schedule is set by whoever can push to the repository, not by the sandbox's owner.
Any real version of this needs a per-owner rate limit and a rule that a paused box
stays paused unless its owner asked otherwise; waking boxes on push also runs
straight into the reaper's argument (`internal/host` vitals), which is that an idle
box should be paused — a box woken by other people's commits is never idle and never
reaped.

**`pull_request` and `check_suite`** are agent triggering: open a PR, an agent picks
it up. That is a product with its own authorization model — who may cause an agent
to run, in whose sandbox, spending whose credentials — and not something to stub out
in advance. Subscribing to them before that exists produces deliveries that are
logged and dropped, which teaches everyone to ignore the log, which is the one thing
this receiver currently has going for it.

---

# Part 4 — operator setup

## 1. Mint the secret and store it

```sh
SECRETS_DIR=./staged deploy/sync-fleet-secrets.sh push
```

`push` generates `github-webhook-secret` when the vault has none and **prints it** —
`openssl rand -hex 32`, hex rather than base64 because the value is pasted into a web
form and compared byte for byte. This is the one secret in the manifest that is both
generated locally and echoed, and `docs/secret-management.md` explains why: GitHub
neither issues nor learns a webhook secret, so a locally minted random string is not
a stand-in for the real thing, it *is* the real thing. (Contrast `github-app-key`,
one line above it in the same script, which `push` refuses to generate.)

Keep the printed value. It goes onto the host in step 2 and into the App in step 4.

## 2. Get it onto the host *before* setting the URL

Order matters. A host without `SPARKBOX_GITHUB_WEBHOOK_SECRET` serves **nothing** at
the webhook URL rather than serving an endpoint that accepts anything — `New`
returns `ErrNoSecret` and `cmd/sparkbox` mounts no receiver. If the App is pointed
at the URL first, GitHub records every delivery as failed, and enough consecutive
failures disable the webhook without telling anybody.

* **Pull-model hosts (the DGX):** `sparkbox fetch-secrets` writes it into the systemd
  `EnvironmentFile` alongside `SPARKBOX_CONSOLE_PASSWORD`; restart the unit.
* **CKS:** the gateway manifest has **no env entry for it today**. Add one mirroring
  the `sparkbox-console` block in `deploy/kubernetes/gateway-deployment.yaml` —
  a `secretKeyRef` marked `optional: true` — and roll the deployment. Until that
  exists, the receiver stays disabled on CKS no matter what the vault holds.

The subdomain is `--webhook-subdomain`, defaulting to `hooks`; setting it empty
disables the receiver independently of the secret. On startup the gateway logs
either `github webhook receiver enabled` with the full URL, or
`github webhook receiver disabled` with the reason — check that line before touching
GitHub.

## 3. Check the URL answers, before the App points at it

The URL is `https://hooks.<domain>/github`. For the live deployment that is
**`https://hooks.catnip.sh/github`**, and it needs no new DNS: `docs/deploy-cks.md`
has the zone carrying `A * → <LoadBalancer IPv4>`, and a wildcard matches exactly one
label, which `hooks` is. The certificate is a softer claim and worth knowing: that
deployment runs `autocert`, which issues **a separate certificate per hostname, on
demand**, so `hooks.<domain>` gets its own the first time somebody asks for it.
Nothing to configure — but the first request pays an ACME issuance, and GitHub gives
a delivery ten seconds. Warm it by hand rather than letting the `ping` be the first
request:

```sh
curl -i -X POST https://hooks.catnip.sh/github     # expect 401
curl -i https://hooks.catnip.sh/github             # expect 405
```

| What you get | What it means |
|---|---|
| `401 signature verification failed` | mounted and verifying. This is the correct answer to an unsigned POST |
| `405 Method Not Allowed` | mounted; the mux serves `POST /github` and only that |
| `404 Nothing is forwarded here` | **not mounted** — no secret on the host, or `--webhook-subdomain` is empty. Go back to step 2 |
| a TLS error | the certificate has not been issued yet; retry, and check port 80 reaches the edge for HTTP-01 |

## 4. Configure the App

In the App's settings, under **Webhook**:

| Field | Value |
|---|---|
| **Active** | ☑ ticked |
| **Webhook URL** | `https://hooks.<domain>/github` |
| **Secret** | the value from step 1, byte for byte |
| **SSL verification** | ☑ enabled (the default; there is no reason to disable it against a public certificate) |

One thing to know if this URL is ever *also* pointed at from a repository or
organization webhook, which does offer a **Content type** choice: it must be
`application/json`. The form-encoded alternative wraps the JSON in a `payload=`
parameter, and the failure is invisible from GitHub's side — the signature covers
the raw body either way, so the delivery verifies and answers 204 — while
`json.Unmarshal` fails here and every log line comes out with an empty event,
action, sender and installation. A receiver that logs nothing useful while
reporting success is the worst of both.

Then subscribe to the events. By their names in the **Subscribe to events** list:

| GitHub's checkbox | Tier |
|---|---|
| Installation | 1 |
| Installation repositories | 1 |
| Organization | 1 |
| Member | 1 (logs only, today) |
| Repository | 2 |
| Meta | 2 |

`github_app_authorization` is not in that list and does not need to be: GitHub
sends it to every App and does not let one unsubscribe. If a `revoked` line shows
up in the host log before any handler exists for it, that is why.

Leave **Push**, **Pull request** and **Check suite** unchecked. Part 3 says why.

## 5. Verify

Saving the webhook makes GitHub send exactly one `ping`. Confirm it from both ends:

* **GitHub:** the App's **Advanced → Recent Deliveries** tab. A green `ping` with a
  `204` is the whole chain — DNS, TLS, routing, and the shared secret — confirmed in
  one row. This tab is also where a mismatch shows up later: a wrong secret is a red
  `401`, and **Redeliver** replays it after you fix one side, without waiting for a
  real event.
* **The host:** one `github webhook delivery` line at Info, with `event=ping`. Match
  the delivery's `X-GitHub-Delivery` uuid against the line's `delivery=` field — that
  pairing is the point of logging it.

The line carries `event`, `action`, `delivery`, `installation`, `sender`,
`repositories` and `bytes`, and deliberately nothing else. `repositories` is a
*count*, covering both payload shapes; no repository is ever named. That is not
squeamishness about secrets — a delivery carries private repository names, branch
names, member logins and PR titles, all of it other people's data arriving on an
unauthenticated port, and a host log is somewhere it would sit for months and get
shipped to a collector. A refused delivery logs a `Warn` with the delivery id and
never the body.

---

# Part 5 — what has to change before any of Part 3 happens

Three things, none of them large, all of them worth naming so the next change is not
also a design:

1. **The cache needs a way to forget.** `internal/ghapp/cache.go` has `get` and
   eviction and nothing else. The shapes the keys already support: a prefix scan on
   the installation id for `tokens` and `members`, and — because `insts` is keyed by
   slug with no id in the key — either a walk over the values, whose `Installation`
   carries the id, or a flush of that one map. At 512 entries per cache, either is
   fine; the point is only that this is a deliberate choice with a comment on it.
2. **No handler may do the work inline.** GitHub gives a delivery ten seconds, and
   enough failures disable the webhook silently. `receive`'s `default` arm already
   says so. Answer 204, then work.
3. **Nothing needs to cross the fleet link.** The receiver is gateway-only and the
   caches are gateway-only, and that is not a coincidence — it is the same reason the
   App key lives on the gateway (`github-repos-design.md` §3.5). A node relays a
   credential request to the gateway; the invalidation it depends on happens on the
   same machine that holds the cache, with no new fleet message to design.

---

See also: `docs/github-app-setup.md` (creating the App, its permissions, and the
private key), `docs/github-repos-design.md` (what the credential is and why),
`docs/secret-management.md` (the fleet secret and how a host obtains it).
