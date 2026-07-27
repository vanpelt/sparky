# Linking a GitHub account

How sparkbox comes to believe that the person holding an account is a
particular GitHub user, what that belief then permits, and what it would take to
let another service establish it for us.

---

# Part 1 — why this is worth getting right

A linked GitHub account is not decoration. It reaches three places:

1. **`keys import-github`** adopts every key `github.com/<login>.keys` publishes
   onto the account. Adopted keys **authenticate**. This is the sharp edge.
2. **The `github` claim in every id token** the gateway mints for a sandbox
   (`internal/oidc/issuer.go`). `docs/identity-federation-design.md` sells it as
   "a strong external anchor" for CEL policies — `claims.github == "vanpelt"` —
   which is a sentence about how much a downstream service is entitled to
   conclude from it.
3. **`X-Forwarded-Email`** and the console's identity panel, where it is only
   ever displayed.

(1) and (2) are why provenance matters. A link is not a fact of one strength;
it is a fact of *whatever strength the evidence was*, and the platform used to
store only that it happened.

---

# Part 2 — what shipped

## 2.1 The key check (unchanged, still the zero-config path)

`ctl keys verify-github <login>` fetches `github.com/<login>.keys` and looks for
one of the caller's **already-registered** keys. Possession of a key GitHub
publishes for an account is what GitHub itself accepts for a git push, so this
proves control without an OAuth app, a client secret or a browser. The
fingerprint is checked against the caller's own key list first — otherwise
anyone could nominate a stranger's key and claim that stranger's login.

Its limit is coverage, not strength: it only works for somebody who publishes a
key on GitHub *and* uses that same key here. People who push over HTTPS, or who
keep a separate key for sandboxes, had nothing.

## 2.2 The device flow (new)

`ctl github link` runs GitHub's OAuth device flow (RFC 8628):

```
open https://github.com/login/device and enter this code:

    WDJB-MJHT

waiting for you to authorize it (ctrl-c to skip)…
✓ linked to github.com/octocat
```

Why this flow and not a redirect: there is **no callback URL and no client
secret**, which is what makes it fit a channel that is an SSH session. There is
also nothing to type back into the terminal, so it needs no PTY — plain
`ssh ctl@host github link` works, which a prompt-based dialog would not.

Three properties worth keeping:

- **No scope is requested at all.** We are asking GitHub one question — who is
  this — and a token that could read repositories would be a token worth
  stealing. An OAuth app's unscoped token reads public profile data; a GitHub
  App's carries only the app's own permissions.
- **The login is GitHub's answer, never the caller's claim.** `FinishGitHubLink`
  takes no login parameter. This is the difference in kind from the key check,
  which verifies a login somebody typed.
- **The device code never reaches a terminal, a log line or an error.** It is
  the credential half of the flow; whoever holds it can collect the token. The
  user code — the thing meant to be read aloud — is what gets printed and logged.

**Configuration.** `--github-client-id` defaults to `Iv23liV6n9amGfGY20Js`, the
Hivemind app. A client id is a public identifier — it travels in the request
that mints the code and is displayed to the person authorizing it — so shipping
one costs nothing and saves every operator an app registration. It does mean the
consent screen names that app rather than the host's; an operator who would
rather it named theirs registers one and passes the flag, and one who wants no
GitHub linking passes the empty string, which returns the host to the state it
was in before this existed.

**The one operator trap**: whatever app the id names must have *Enable Device
Flow* checked in its settings. Without it every attempt fails identically and
forever, so `device_flow_disabled` is mapped to its own error that says an
operator has to fix it.

## 2.3 Provenance, and what it gates

`users.github_via` records how a link was proved:

| value | meaning | may adopt keys |
| --- | --- | --- |
| `github-keys` | a registered key was found on `github.com/<login>.keys` | yes |
| `device-flow` | GitHub issued a token naming this login | yes |
| `assertion` | another service's signed word for it | **no** |

`users.StrongGitHubLink` is the predicate, and `ImportGitHubKeys` is its only
caller today. The reasoning, stated once so it is not re-derived wrongly later:
an adopted key authenticates, so a link established by a channel that could be
wrong about *which human* is on the other end must not reach that verb. If it
could, somebody would claim a stranger's login, pre-load the stranger's
published keys onto their own account, and collect that stranger the next time
they connected.

The migration backfills `github-keys` for every existing link. That is a
statement of fact rather than a default: the key check was the only linking path
that had ever shipped, so every row that can exist was made by it.

`users.github_id` records GitHub's immutable account number alongside. A login
is renameable and, once released, re-registerable by somebody else; the number
is not. The key check learns nothing but a login, so the profile is fetched
after a successful verification — **best-effort**, because refusing to record a
proved link when `api.github.com` is slow would trade a real fact for an
optional one.

## 2.4 Reach

The offer used to live inside the `signup@` dialog and nowhere else, which
reached exactly the people who arrived through that door — not the operators
seeded from `users.conf`, not anybody who skipped it once. So:

- One dialog (`sshgw/githublink.go`) entered by signup, `ctl github link`, and
  the nudges. Signup now acts *as the new account* through `ctlops`, so it gets
  the same provenance and the same audit line as every other path.
- A one-line nudge for accounts with no link, on `session-token` (where somebody
  is heading to the browser for the first time) and after a sandbox create (the
  door with the traffic). It goes on stderr, so `$(…)` still captures nothing
  but the credential, and it stops the moment the question has been answered.
- `whoami` shows the provenance, because it decides whether `import-github` will
  work and somebody refused there should be able to read why.

## 2.5 Not done

- **No REST endpoints for the device flow.** The wait is a long poll, which the
  API's budget model has no shape for yet; `POST /v1/keys/verify-github` is
  still the HTTP path. The ops layer is transport-neutral, so adding two
  endpoints later is mechanical — the question is what a non-blocking `finish`
  should return, not where the logic goes.
- **No browser path.** The user console could offer the same flow, but a device
  code in a page the user is already signed into is a worse ceremony than a
  redirect, and a redirect needs a client secret.

---

# Part 3 — the Hivemind handshake (design note, not built)

Hivemind already knows its users' GitHub identities, and sparkbox already
federates *to* Hivemind over OIDC. The proposal: Hivemind signs an assertion
binding a sparkbox account to a GitHub login and id, sparkbox verifies it, and
the user never types anything.

It is a good idea and it is an **accelerator, not a verifier**. Stating that
plainly is most of this note.

## 3.1 What the assertion is actually worth

In the form where sparkbox still checks `github.com/<login>.keys` after reading
the assertion, the assertion supplies *which login to check* and the key check
supplies the proof. That is exactly the assurance `keys verify-github <login>`
has today; what is removed is the typing. Real convenience, no change in
strength — and it inherits the key check's coverage gap, since a user with no
published key still cannot complete it.

In the form where the assertion alone establishes the link, Hivemind becomes an
**identity authority for sparkbox**. That is a defensible choice, but it is a
different system than the one running today, and it needs to be made
deliberately rather than arrived at.

## 3.2 Two hazards specific to this pairing

**It closes a loop.** `github` is a claim sparkbox *issues*, and
`identity-federation-design.md` recommends Hivemind policies match on it. If
Hivemind is also the source of that claim, Hivemind ends up authorizing against
a fact it asserted. Today the claim's root is GitHub itself, which is what makes
it worth anything to a relying party. Any assertion-derived link must therefore
either be excluded from the `github` claim, or the claim must carry its
provenance so a policy can require `github-keys`/`device-flow`.

**It reaches key adoption.** See §2.3. This is why `assertion` is not a strong
provenance, and it is the constraint that survives whatever else is decided.

## 3.3 The shape to build, if it is built

Do **not** give Hivemind a key that can act on any account. Instead:

- The user's **own sparkbox credential** authorizes the write — a session token
  on `POST /v1/account/github`, or a passkey assertion. This is the half
  Hivemind genuinely cannot speak to, and it is what the passkey idea is good
  for: it proves the right person is accepting the binding, not that the GitHub
  account is theirs.
- **Hivemind's signature attests only the fact**: `{sparkbox_sub, github_login,
  github_id, verified_at}`, bound to that subject so it cannot be replayed onto
  another account. Its key is then a *witness*, not a credential — a compromise
  forges facts about users who are actively linking, rather than about everyone.
- On receipt, sparkbox tries the key check with the asserted login. If it
  passes, the link is recorded `github-keys` and the assertion was pure
  convenience. If the user publishes no keys, fall straight into the device flow
  **with the login already known**, and record `device-flow`. Recording
  `assertion` at all is the fallback for a deployment that has decided
  Hivemind's word is enough, and it stays weak.

Best case that is zero-touch; worst case it is the flow we already have with one
less thing to type. Neither case invents a new trust root.

## 3.4 Questions for the Hivemind side

1. **Key distribution and rotation.** Where does sparkbox learn Hivemind's
   public key, and how is it rotated without a flag day? A JWKS at a pinned
   URL is the obvious answer and needs an owner.
2. **What is the subject?** Assertions must bind to the sparkbox `sub`
   (`oidc.SubjectFor(handle)`), not to a handle or an email, or a handle rename
   silently re-points one.
3. **Replay and freshness.** A `jti` and a short `exp`, and a decision about
   whether an assertion may be presented more than once.
4. **What does Hivemind actually know?** Did the user complete a GitHub OAuth
   login there, or did they type a username into a profile field? These are not
   the same claim and the assertion should say which.
5. **Revocation.** If Hivemind later learns the binding was wrong, does anything
   propagate? Today nothing does, and the honest answer may be "re-link".
