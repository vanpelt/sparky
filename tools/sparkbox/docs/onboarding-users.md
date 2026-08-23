# Onboarding users

How somebody who is not you gets an account, and what they have to do before an
agent inside their first sandbox actually works.

---

# Part 1 — the doors

An SSH public key is the sole credential. Passkeys and session tokens are
conveniences minted *from* an authenticated account, never a way in. So every
door below is a question of authorization — who may have an account — and never
of authentication, which is always "hold the private key".

| door | makes | needs |
| --- | --- | --- |
| `users.conf` seed (`--users-file`) | an **operator** | host provisioning access |
| `ctl user add` / `ctl user sync-github-org` | an ordinary user | an operator, and a GitHub login |
| `ssh signup@<domain>` + invite code | an ordinary user | a code from `ctl invite` |
| `--open-signup` | an ordinary user | nothing — anyone who can reach port 22 |

## Do not seed colleagues into `users.conf`

It is the obvious shortcut and it is the wrong door. `users.Store.Create` stamps
`invited_by = "operator"` for every seeded entry, and `User.IsOperator()` is
what gates:

- `ctl node approve` / `node rm` — fleet administration, and
- `proxy.mayView` — **opening any other user's private sandbox URLs.**

An operator is a co-administrator of the deployment. Use one of the other doors
for people who are not.

---

# Part 2 — admitting people from GitHub

```sh
# One person. No token needed: github.com publishes an account's ssh keys.
ssh ctl@ssh.<domain> user add adrnswanberg

# A team. Your own read:org token, on stdin, used once and never stored.
gh auth token | ssh ctl@ssh.<domain> user sync-github-org wandb --team hivemind-team

# See what it would do first.
gh auth token | ssh ctl@ssh.<domain> user sync-github-org wandb --team infra --dry-run

# The roster afterwards.
ssh ctl@ssh.<domain> user ls
```

An account is created per login, named after the login, holding **every SSH key
github.com publishes for it**. The person then connects with nothing to type:

```sh
ssh new@ssh.<domain>
```

## Why importing published keys is a sound way in

It is the claim `ctl keys verify-github` already makes, in the other direction.
GitHub publishes an account's public keys at `github.com/<login>.keys`, and
holding the private half of one is what GitHub itself accepts to authenticate a
git push. An account whose keys are exactly what GitHub publishes for `<login>`
can therefore be authenticated by exactly one person: whoever GitHub would let
push as `<login>`.

The reversal is the only new thing. `verify-github` is a user proving a login
they typed; this is an operator asserting a login and letting GitHub supply the
keys. The operator is trusted to name their own colleagues, and if they name the
wrong one the result is an unused account — not access, because no key the
operator controls is involved anywhere.

The link is recorded with provenance `github-keys`, which is a statement of fact
rather than a convenience: the account's keys *are* what GitHub publishes, so
the claim that provenance makes is true by construction. That in turn makes
`StrongGitHubLink` hold, so the user can later run `ctl keys import-github`
themselves to pick up a new laptop key without an operator.

## What the org sync needs, and why the token is yours

`GET /orgs/<org>/members` needs authentication, and this is not incidental.
GitHub serves an org's *public* members without a credential, but public
membership is opt-in and almost nobody opts in — on the org this was built for,
neither the operator nor the first colleague was a public member, so the free
check reports "not a member" about two people who are.

So the roster read authenticates, and it does so with **your** token, passed on
stdin per invocation and never stored. The alternative — a standing `read:org`
credential in the gateway's configuration — would buy live membership checks at
signup time and cost a permanent GitHub credential on an internet-facing host.
`gh auth token` already prints a token with `read:org` in its default scope set.

## Prefer `--team` over a whole org

A company org is not a guest list. `wandb` has **617 members** visible to a
member's token, nearly all of whom will never open a sandbox, and provisioning
all of them fills the roster with accounts nobody uses and spends 617 requests
on github.com finding that out. So a run of more than 200 people is refused, and
the refusal names the way through:

```
sparkbox: "wandb" has 617 members, more than this will provision at once (200).
          a team is usually the group that actually wants this: --team <slug>
```

Find the slugs you can read with:

```sh
gh api '/user/teams?per_page=100' \
  --jq '.[] | select(.organization.login=="wandb") | "\(.slug) (\(.members_count))"'
```

Two consequences worth stating plainly:

- **It is a snapshot, not a policy.** Somebody who leaves the org keeps their
  account until you do something about it. Deprovisioning is manual either way
  today.
- **An empty roster is reported as an error, not a clean run.** A token missing
  `read:org` — or one not authorized for a SAML-enforcing org — sees only public
  members, and "0 accounts, all good" would send you looking in the wrong place.

## Outcomes

Every login examined gets a line, including the ones nothing happened to: those
are exactly the people who still need an invite.

| outcome | meaning |
| --- | --- |
| `created` | the account did not exist and now does |
| `keys-added` | it existed and gained keys GitHub lists that it lacked |
| `current` | it exists and already holds every listed key |
| `no-keys` | GitHub publishes no SSH key for this login — they need an invite |
| `skipped` | the handle can't be derived, or belongs to somebody else |
| `failed` | GitHub or the database refused, for this login only |

Re-running is free and is the mechanism for picking up a colleague's new key.

## When somebody has no published key

They come in the original way: `ssh ctl@ssh.<domain> invite` mints a single-use
code, good for seven days, and they run `ssh signup@ssh.<domain>`. The signup
dialog offers GitHub linking (device flow — no published key required) and an
email at the end.

---

# Part 3 — first run: getting an agent signed in

A fresh sandbox ships `claude`, `codex`, `pi`, `hivemind` and `gh`. None of them
are authenticated. The platform's answer is a per-owner secret, encrypted at
rest and delivered into the guest's `/etc/environment`.

```sh
claude setup-token | ssh ctl@ssh.<domain> secret set CLAUDE_CODE_OAUTH_TOKEN
ssh ctl@ssh.<domain> secret ls
ssh ctl@ssh.<domain> secret rm CLAUDE_CODE_OAUTH_TOKEN
```

**The value is read from stdin and is never an argument.** `secret set NAME
VALUE` is refused on purpose: the value would land in your shell history, in
your local ssh process's argv, and in anything in between that logs commands.
With a terminal (`ssh -t`) you are prompted instead, unechoed.

Setting a secret re-pushes it into every running sandbox that selects it, so it
takes effect immediately rather than at the next resume — which for a pinned box
is never. Deleting one strips it from those sandboxes the same way.

## Tags, and the `default` tag

A secret reaches a sandbox only if the two **share a tag**. That inner join used
to be a silent failure: save a token, run `ssh new@`, and get an empty
environment with nothing anywhere saying why.

Both halves now default to the tag `default` — an untagged secret gets it, and a
new sandbox is stamped with it — so the common case works without knowing tags
exist. Name tags explicitly to opt out:

```sh
gh auth token | ssh ctl@ssh.<domain> secret set GITHUB_TOKEN --tag ci
ssh ctl@ssh.<domain> tags mybox ci          # only this box gets it
ssh ctl@ssh.<domain> tags mybox             # clear its tags entirely
```

`default` is an ordinary tag, not a wildcard. One thing to know before writing
an egress rule-set against that name: `internal/netrules` shares the same
`sandbox_tags` table, so a **rule-set** tagged `default` would begin governing
every sandbox created since this shipped. A `default` tag with no rule-set bound
to it leaves egress unrestricted, which is the state everything is in today.

## Claude

`claude setup-token` prints a long-lived OAuth token. Store it as
`CLAUDE_CODE_OAUTH_TOKEN`. `ANTHROPIC_API_KEY` works too if you bill by API.

The guest image is already conditioned for it: `~/.claude.json` is seeded past
the theme picker and `CLAUDE_CODE_SANDBOXED=1` satisfies the trust dialog, so a
box carrying the token drops straight into a working `claude`.

## Codex

Codex reads three variables, so it has the same shape of answer:

| variable | source | notes |
| --- | --- | --- |
| `OPENAI_API_KEY` | platform.openai.com | API billing; does not expire |
| `CODEX_API_KEY` | as above | Codex-specific alternative |
| `CODEX_ACCESS_TOKEN` | `~/.codex/auth.json` after `codex login` | ChatGPT subscription auth |

```sh
printenv OPENAI_API_KEY | ssh ctl@ssh.<domain> secret set OPENAI_API_KEY

# or, on a ChatGPT plan, after `codex login` locally:
jq -r .tokens.access_token ~/.codex/auth.json |
  ssh ctl@ssh.<domain> secret set CODEX_ACCESS_TOKEN
```

**The access token expires.** Measured lifetime is 240 hours (10 days) from
mint, and the refresh token that would renew it lives in `auth.json`, not in the
environment — so the ChatGPT path needs re-running every so often. `OPENAI_API_KEY`
is the one that does not go stale.

Codex has not been given the equivalent of Claude's first-run conditioning, so
expect its own approval prompts on first use inside a box.

## GitHub credentials

`gh auth token | secret set GITHUB_TOKEN` works and is the quickest thing, but
know what you are pushing: a `gh` token typically carries `repo` scope, and
every process in that VM can read it out of the environment. Three options, best
first:

**1. `gh auth login` inside the sandbox.** `gh` is already in the guest image,
the guest has egress, and the disk is persistent — so the credential survives
pause/resume and never becomes an environment variable at all. The device flow
lets you approve exactly the org you mean, and it configures git's credential
helper on the way through. Nothing to set up on the platform side.

**2. A fine-grained PAT for non-interactive use.** Create one at
`github.com/settings/personal-access-tokens/new` scoped to specific repositories
with only the permissions you need, and give it a narrow tag so it reaches only
the sandboxes that should have it.

**3. Not built: GitHub App installation tokens.** The genuinely right answer —
tokens scoped to the repos an installation covers, expiring in an hour, revoked
by uninstalling. It needs an app private key as a new fleet secret and, because
of that one-hour expiry, delivery through the guest metadata service rather than
a static environment variable. Worth doing; not done.

---

# Part 4 — a colleague, end to end

```sh
# You, once:
ssh ctl@ssh.<domain> user add their-github-login

# Them, first connection — no invite code, nothing to paste:
ssh new@ssh.<domain>

# Them, once, to sign their agents in:
claude setup-token | ssh ctl@ssh.<domain> secret set CLAUDE_CODE_OAUTH_TOKEN
```

The sandbox they created in step two carries the `default` tag, and the secret
in step three gets the same tag, so it is pushed to that box immediately — no
reconnect, no retag.
