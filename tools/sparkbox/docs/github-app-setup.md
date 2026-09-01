# Setting up the GitHub App

Sparkbox uses a GitHub App for two different things, and the difference decides
whether you need one app or two.

| | what it needs from the app | who holds what |
|---|---|---|
| **Linking** (shipped) | a **client id**, and *Enable Device Flow* checked | the client id is public; sparkbox stores no secret |
| **Repos** (shipped) | a **private key**, repository permissions, Device Flow, web OAuth, and expiring user tokens | the gateway holds the private key, OAuth client secret, and encrypted rotating user grants |

Linking asks GitHub one question — who is this — and requests **no scope at
all**. Repos always supports narrowly scoped installation tokens, and an owner
can additionally authorize one write attachment with `sparkbox repo authorize
owner/name` so GitHub operations, including pull-request creation, are
attributed to that user. The Repos tab offers the same authorization through
GitHub's browser OAuth flow when the gateway has the App client secret.

---

# Part 1 — one app or two?

The shipped default client id, `Iv23liV6n9amGfGY20Js`, is the **Hivemind** app
(`hivemind.wandb.tools`). It works fine for linking on any host — a client id is
a public identifier and the device flow has no callback URL, so nothing about it
is bound to a domain. That is why catnip.sh has been using it all along.

It does **not** work for repos, and the reason is worth being precise about.

Repo access needs two things the Hivemind app cannot give you:

1. **Its private key.** Installation tokens are minted by signing a JWT with the
   app's private key. Whoever holds that key can mint a token for *every*
   installation of that app. Putting Hivemind's key on the catnip.sh gateway
   would mean the catnip.sh host can mint credentials for every repo anyone ever
   granted Hivemind.
2. **Repository permissions.** An app's permissions are set per app, not per
   consent. Adding `contents: read/write` to the Hivemind app silently widens
   what every past authorization meant — people who clicked "authorize" for
   identity alone would find the app now able to request repo tokens.

So: **make a second app.** It costs one extra install per user and it keeps the
two blast radiuses apart. Keep using the Hivemind client id for linking, or
point linking at the new app too — both work, and using one app for both is the
nicer consent story if you are willing to have people re-link.

---

# Part 2 — create the app

Go to **https://github.com/settings/apps/new** (for a personal app) or
`https://github.com/organizations/<org>/settings/apps/new` (for an org-owned
one, which is what you want if the repos live in an org).

Fill in:

| Field | Value | Why |
|---|---|---|
| **GitHub App name** | `sparkbox` (or `catnip-sparkbox` if taken — names are global) | shown on the consent and install screens |
| **Homepage URL** | `https://catnip.sh` | required, cosmetic |
| **Callback URL** | `https://my.catnip.sh/github/repo/callback` | returns browser authorization to the signed-in Repos tab; substitute your console host |
| **Enable Device Flow** | ☑ **checked** | **the one setting that silently breaks everything if missed** |
| **Optional Features → User-to-server token expiration** | ☑ **opted in** | issues the rotating `ghu_` / `ghr_` pair Sparkbox stores on the gateway; non-expiring user tokens are refused |
| **Webhook → Active** | ☐ **unchecked** | leave it off — unless you are also setting up webhooks, which is its own decision: see `docs/github-webhooks.md` and the note below |
| **Where can this app be installed?** | *Any account* | so org installs are possible |

There *is* a receiver now (`internal/ghwebhook`), and for an App you are setting
up today it is still right to leave the box unchecked. It verifies a delivery's
signature, writes one line about it, and returns 204; no repo, token or cache
changes because a webhook arrived. Turn it on if you want that log line, or if
you are working on what should act on a delivery —
**`docs/github-webhooks.md`** makes the case for that (in short: three
authorization caches on the gateway hold for ten minutes, and today that is the
whole of this platform's revocation latency), and carries the full setup, the
event list worth subscribing to, and the verification steps. The short version,
if you only want the log line:

* set **Webhook URL** to `https://hooks.<your-domain>/github`,
* set **Secret** to the value `deploy/sync-fleet-secrets.sh push` printed for
  `github-webhook-secret`, and make sure the host has it in
  `SPARKBOX_GITHUB_WEBHOOK_SECRET` **first** — a host without it serves nothing
  at that URL, so GitHub will record every delivery as failed,
* expect the *ping* GitHub sends when you save to answer 204.

## Permissions

**Repository permissions** — set only these:

| Permission | Access | Needed for |
|---|---|---|
| **Contents** | **Read and write** | `git clone`, `git fetch`, `git push` |
| **Metadata** | Read-only (mandatory, auto-selected) | resolving a repo to its installation |
| **Pull requests** | **Read and write** | `gh pr create`, `gh pr list`, `gh pr view` |
| **Issues** | Read and write (optional) | `gh issue` |

**Organization permissions** — one, and only if you will install on an org:

| Permission | Access | Needed for |
|---|---|---|
| **Members** | Read-only | proving a sparkbox user is actually in the org |

That one needs explaining, because it is the difference between a working org
install and a confusing one. A *personal* installation is bound to a sparkbox
account by comparing GitHub's immutable account number against the one recorded
when the user linked — no API call, no permission. An *organization* installation
has no such number to compare: the installation belongs to the org, not to a
person, so sparkbox has to ask whether this particular user is in it.

It asks with `GET /orgs/{org}/memberships/{user}`, which needs `members: read`.
Without it GitHub answers 403, and sparkbox reports that as *the operator did not
grant this permission* — never as "you are not a member", because those are
different facts and conflating them would lock out real members. If you only ever
install on personal repos, skip this permission entirely.

Set **Contents** to read-and-write at the *app* level even though most
attachments will be read-only. The app permission is a ceiling, not a grant:
sparkbox down-scopes every minted token per request, so a `read` attachment gets
a read-only token regardless. An app capped at read cannot ever be raised
without every user re-consenting.

**Pull requests** is what makes `gh` useful inside a sandbox. The CLI speaks no
credential-helper protocol, so it runs on the same per-repository token `git`
does (a wrapper in the guest hands it over as `GH_TOKEN` for the
length of one command and writes nothing to disk). A token that can push a
branch but cannot open a pull request for it is a strange half-grant, so the
minted set follows the attachment's access level across all of these
permissions: `--write` attachments get write, everything else gets read.

Every one of them is narrowed to what the app actually holds before the token is
requested, so **granting fewer of these is safe** — an app with Contents alone
still clones, and `gh` simply cannot open PRs with it. That matters because
GitHub refuses a token request naming a permission the installation lacks
*outright* rather than trimming it: without the narrowing step, adding Pull
requests to this list would have broken every clone on an app that predates it.

Leave **Account permissions** entirely empty.

## Per-repository user attribution

The Repos tab is the normal browser path: click **Authorize as me** next
to a read + push attachment. Sparkbox sends the browser through GitHub's web
application flow with PKCE and a one-time state bound to the signed-in account.
Add `https://my.<your-domain>/github/repo/callback` to the App's callback URLs,
generate a client secret in the App settings, and deliver it only to the
gateway as `SPARKBOX_GITHUB_APP_CLIENT_SECRET` (boot-secret item
`github-app-client-secret`).

The client secret is optional. Without it the browser button is omitted, while
the VM Device Flow below and installation-token fallback continue normally.

Inside a VM, authorize each write attachment whose GitHub activity should be
performed as you:

```sh
sparkbox repo authorize wandb/hivemind
```

The VM prints GitHub's public device code and polls its own metadata endpoint.
The device code, access token, and rotating refresh token remain on the gateway;
only the public user code crosses into the VM. Sparkbox verifies both the
immutable linked GitHub account id and that GitHub exposed exactly the requested
repository before saving the grant encrypted in `sparkbox.db`.

The browser flow lands in that same store and applies the same immutable-user
and exact-one-repository verification before accepting GitHub's token.

Authorization is per repository. In a VM containing two repositories, one can
use the user's token while the other continues to use the App installation
token. Missing, expired, revoked, or temporarily unrefreshable user grants never
break git: Sparkbox falls back to the one-hour bot token for that repository.

Click **Create GitHub App**.

---

# Part 3 — collect the four values

On the app's settings page:

1. **Client ID** — shown at the top, looks like `Iv23li…`. Public.
2. **App ID** — a number, shown just above it. Public.
3. **Private key** — scroll to *Private keys* → **Generate a private key**. A
   `.pem` downloads once and is never shown again. This is the secret.
4. **Client secret** — under *Client secrets*, generate one. This enables the
   Repos tab's browser flow and is not needed by the VM Device Flow.

The downloaded key is PKCS#1 (`-----BEGIN RSA PRIVATE KEY-----`). Store it
verbatim; do not re-encode it.

---

# Part 4 — put the credentials in the fleet vault

They are two of the eleven secrets in `docs/secret-management.md`. The key's blast
radius: mints installation tokens for every installation of the app. Unlike the
OIDC signing key it is **revocable in one click** — regenerate it in the app's
settings and every copy dies.

```sh
# 1Password (the DGX / catnip.sh path)
op item create --category=password --vault Sparkbox \
  --title github-app-key "password=$(cat ~/Downloads/sparkbox.*.private-key.pem)"

# Save the copied client secret without putting it in shell history or argv,
# then let the fleet sync script write the 1Password item.
install -d -m 700 ~/.sparkbox/secrets
${EDITOR:-vi} ~/.sparkbox/secrets/github_app_client_secret
chmod 600 ~/.sparkbox/secrets/github_app_client_secret
deploy/sync-fleet-secrets.sh push
```

Then deliver it to the host the same way as the rest:

```sh
SECRETS_DIR=./staged deploy/sync-fleet-secrets.sh pull
scp ./staged/github_app_key.pem root@catnip.sh:/srv/sparkbox/state/
# If the host does not use fetch-secrets, also expose the staged client secret
# to the gateway as SPARKBOX_GITHUB_APP_CLIENT_SECRET.
```

A fleet with no `github-app-key` keeps working — repo attachment answers
"not enabled on this host", the same shape tagging already uses.

On Kubernetes, keep both GitHub-issued values in `sparkbox-github-app` as
`private-key.pem` and `client-secret`. Do not add the App key to
`sparkbox-identity`; that Secret is reserved for long-lived fleet identity.

---

# Part 5 — install it, and tell sparkbox about it

**You install the app on the repos you want reachable**, at
`https://github.com/apps/<app-name>/installations/new`. Pick *Only select
repositories* unless you mean all of them — this is the outer boundary on
everything sparkbox can ever mint, so it is worth being narrow here even though
tokens are scoped again per request.

`ssh ctl@catnip.sh github install` prints that URL for the app this host is
configured with.

Nothing else is needed on GitHub's side. Sparkbox discovers the installation
itself: `GET /repos/{owner}/{repo}/installation`, authenticated as the app.

---

# Part 6 — the gotchas, in the order they bite

1. **Device Flow unchecked.** Every link attempt fails identically and forever.
   Sparkbox maps `device_flow_disabled` to its own error saying an operator has
   to fix it, precisely because the raw GitHub message names a checkbox rather
   than the setting it belongs to.
2. **Installing on a repo you don't admin.** An org repo needs an org owner to
   approve. A member can *request* the install; nothing works until someone
   approves it. `ctl repo add` runs a reachability check and says so rather than
   letting the failure surface as a broken clone inside a VM at boot.
3. **The private key is shown once.** Losing it means generating a new one; the
   old one keeps working until you delete it, so rotation has no flag day.
4. **App ID vs Client ID as the JWT issuer.** Both are accepted by GitHub as
   `iss`. Sparkbox uses the client id, because that is the value already
   configured for linking and having one identifier is better than two.
5. **Clock skew.** The app JWT is rejected if `iat` is in the future by GitHub's
   clock. Sparkbox backdates `iat` by 60s, so a host that is a little fast still
   works — but a host that is *minutes* off will fail every mint, and the error
   GitHub returns says only "'Issued at' claim is in the future".
