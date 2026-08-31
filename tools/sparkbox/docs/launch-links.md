# Launch links

A launch link is a button in a pull-request comment that turns into a sandbox.
Somebody posts one line of markdown; a reader clicks it, signs in as themselves,
and lands in a browser terminal on a VM of their own with the repository already
checked out on the branch the button named. There is a door for it
(`internal/launch`, mounted at `go.<domain>`), a static badge image served from
that same door, and one command that prints the markdown:

```sh
ssh ctl@<domain> badge wandb/hivemind --ref feat/x
```

Almost everything in this document follows from one fact: **a GitHub comment is
immutable and outlives this deployment.** We cannot edit a URL somebody pasted
in March, we cannot take back a link that turned out to select the wrong tag,
and we cannot ask a reader to retype a URL with an escaping mistake in it. So
the URL grammar is a contract, the badge is one invariant image, and the flag
that names the door carries a warning about renaming it.

Part 1 is the URL. Part 2 is the markdown and what GitHub's sanitizer does to
it. Part 3 is the parameter that does not exist and why. Part 4 is what a click
actually does. Part 5 is the reuse rule. Part 6 is the failure users will
actually hit. **Part 7 is the pre-deploy check, and it is not optional.**

---

# Part 1 — the URL is the contract

```
https://go.<domain>/<owner>/<repo>[?ref=<git ref>]
```

Two parameters, both of them names of things on github.com:

| where | what | validated by |
|---|---|---|
| path, required | `owner/repo` | `repos.ValidSlug` (`internal/repos/store.go`) — the owner half is a GitHub login grammar, the name half allows dots, underscores and a leading digit, so `node.js` works and `..` does not |
| query, optional | `ref` | `repos.ValidRef` — the regexp **plus** the separate `..` check the regexp does not cover. The leading-alphanumeric rule is load-bearing: the ref ends up as the argument of `git clone --branch <ref>`, where a leading `-` is an option and not a branch |

## Why the repository is in the path

Because of the `&`.

The alternative shape, `go.<domain>/?repo=wandb/hivemind&ref=feat/x`, is not
worse to parse — it is worse to hand around. GitHub's own sanitizer escapes the
`&` to `&amp;` for you (verified, Part 2), so on GitHub either shape renders.
Everywhere else is where it goes wrong: a wiki or chat client with a different
renderer, a bot that composes the HTML itself and escapes the `&` a second time,
somebody retyping the URL out of a screenshot. Each of those produces
`&amp;ref=feat/x`, which is a query parameter named `amp;ref` — and since
unknown parameters are ignored rather than refused (they have to be, see the
forward-compatibility rules below), the link still works and quietly checks out
the default branch instead of the one somebody meant. A silent wrong answer, in
a comment nobody can edit.

With the repository in the path the no-ref form — the one a `README` badge uses,
and the one `ctl badge` emits when you pass no `--ref` — **contains no `&` at
all**, and the ref form contains exactly one. There is precisely one place a
human can get the escaping wrong, and most links do not even have that place.

The `/` inside `?ref=feat/x` is left literal rather than percent-encoded: it is
legal in a query value per RFC 3986, no browser touches it, and `%2F` is one
more thing a person copying the URL by hand gets wrong.

## Parameters that will never be added

| not accepted | why not |
|---|---|
| `tag=` | see Part 3. It is a secret-selection primitive over the *clicker's* account |
| `node=` | a stranger's link must not choose whose hardware runs the work |
| `name=` | a link that names a sandbox is a link that collides; `Ops.GenerateName()` decides |
| `handle=` | the owner comes only from the verified `edgeauth` session, never from the URL |
| a command to run | `ctlops.CreateArgs` has no Command field, deliberately |
| anything URL-valued | no `next=`, `return=`, `redirect=`. The only two redirect targets this package can produce are the configured login URL and `SandboxInfo.TerminalURL`. Open redirect is closed by construction, not by a validator somebody has to keep correct |

## Forward compatibility, because the comment outlives the host

1. **Versionless.** Unknown query parameters are neither read nor refused. A
   `?ref=x&utm_source=slack` behaves exactly like `?ref=x`; the extra parameter
   is logged at debug and otherwise ignored. A host built in 2031 must still
   honour a comment written today.
2. **Every future parameter is additive.** Its absence must mean exactly today's
   behaviour, or every already-posted link changes meaning on a deploy.
3. **Nothing lives after `#`.** Fragments never reach a server, and the sign-in
   bounce rebuilds the return URL from `r.URL.RequestURI()` — path and query
   only.
4. **The path namespace is not permanently consumed.** A future two-segment
   literal such as `GET /orgs/{name}` matches a strict subset of
   `/{owner}/{repo}` and wins under Go's specificity rule with no panic. By
   contract any future non-repository first segment starts with `_` or `.`,
   which `users.ValidGitHubOrg` already refuses, so it can never be shadowed by
   a real owner.

Redirects are **303, never 301**: a cached permanent redirect to one sandbox
would survive that sandbox's destruction, and there is no way to take it back.

URLs are **portless, always**. `SPARKBOX_EDGE_REDIRECT` DNATs the uplink's
1024–65535 range onto the one edge listener and reserved dispatch precedes the
port lookup, so `https://go.<domain>:5173/wandb/hivemind` would in fact serve
the launch page — which is exactly why a port must never appear in a link: it
would work, and it would pin an immutable comment to a deployment detail.

## Renaming the door breaks comments already written

`--launch-subdomain` defaults to `go` (`launch.DefaultSubdomain`, threaded from
the owning package the way `--webhook-subdomain` sources
`ghwebhook.DefaultSubdomain`). Both hostnames in the markdown — the link and the
badge `src` — are built from that label and from `--proxy-domain`, never from
the literals `go` and `xterm`, so a relabelled host stops handing out URLs that
404 the day it is relabelled.

What it cannot fix is the links already posted. Moving the label breaks every
comment already written, in a place we cannot edit. Treat `--launch-subdomain`
as a one-time decision.

The door disables itself when `--xterm-subdomain` is empty, and says which of
the two reasons it was in the startup log — `launch links disabled
reason="--launch-subdomain is empty"` or `reason="no browser terminals to land
in (--xterm-subdomain is empty)"`.
A launch link's entire payoff is a terminal to land in, and
`SandboxInfo.TerminalURL` is empty unless both the zone and the xterm label are
configured — the door would otherwise be a button that leads nowhere.

---

# Part 2 — the markdown, and what GitHub actually does to it

`ctl badge` prints exactly one line to stdout, with **zero leading whitespace**,
and everything else to stderr — so `ssh ctl@<domain> badge wandb/hivemind |
pbcopy` puts precisely the right bytes on the clipboard.

With a branch:

```html
<a href="https://go.catnip.sh/wandb/hivemind?ref=feat/x"><img align="right" src="https://go.catnip.sh/badge.svg" alt="Open in Sparkbox" height="28"></a>
```

Without one — the `README` / default-branch form:

```html
<a href="https://go.catnip.sh/wandb/hivemind"><img align="right" src="https://go.catnip.sh/badge.svg" alt="Open in Sparkbox" height="28"></a>
```

**One line, zero indent, both non-negotiable.** Four leading spaces in a
markdown comment makes an indented code block, and the button renders as
literal text. Zero is the only indent nobody can break by pasting it somewhere
else, and it is why `badge.go` assembles the line by concatenation rather than
writing it as a raw string literal — a raw literal is where a real `\n` gets in,
on a channel whose golden test fails on any surviving bare newline.

## What was actually tested

The elements and attributes below were rendered through GitHub's own
`POST /markdown` (`mode=gfm`) on 2026-08-30 rather than assumed. Three probes;
nothing in them was stripped.

| probe | result | what we do with it |
|---|---|---|
| `<a href="…?a=1&b=2">` | kept; `&` escaped to `&amp;`; `rel="nofollow"` added | query strings are safe in a comment. We still keep the repository in the path (Part 1) — the sanitizer protects the *rendered* link, not the person retyping it |
| `<img align="right" height="28" width="124">` | `align`, `height` and `width` all kept | `height` is the one we use. `style` is **not** on the allowlist, which is why the badge carries its own appearance inside the SVG |
| `<div align="right">…</div>` | kept; the button becomes a block that STACKS above whatever follows | the shape we do **not** use. It costs the button a line of its own and puts it above the heading rather than beside it |
| `<a target="_blank" rel="noopener">` | **both stripped**; the sanitizer then adds its own `rel="nofollow"` | the badge cannot open a new tab. See below — this one is asked often enough to be worth writing down |
| `align="right"` on the `<img>` itself | kept; floats the image, so following content flows up beside it | **this is the shape we emit.** Paste it as the first thing in the comment and the button lands in the top-right corner with the heading beside it — the placement a reader's eye already goes to |
| `<picture><source media="(prefers-color-scheme: dark)" …><img …></picture>` | kept, wrapped in GitHub's `<themed-picture>` | a dark-mode variant demonstrably works. Declined — see below |
| every `src` | rewritten to `https://camo.githubusercontent.com/<sha256>/<hex of the url>` | decisive; see the next two sections |
| a bare `<img>` with **both** `width` and `height` | GitHub injects `max-width:100%; height:auto; max-height:<h>px; aspect-ratio:<w>/<h>; background-color: var(--bgColor-muted); border-radius:6px` | the ratio is computed from the *declared attributes*, not from the image |
| a bare `<img>` with **`height` only** | GitHub injects `max-width:100%; height:auto; max-height:<h>px` — and nothing else | **this is the form we emit.** No `aspect-ratio` is injected, so the badge's own intrinsic ratio governs and it cannot be letterboxed |

### The badge cannot open a new tab, and nothing can change that

Re-verified against GitHub's `POST /markdown` on 2026-08-30:

```
in:  <a href="https://example.com" target="_blank" rel="noopener"><img …></a>
out: <a href="https://example.com" rel="nofollow"><img …></a>
```

`target` is dropped and the author's `rel` is replaced with GitHub's own. So a
click navigates the tab the reader is in, away from the pull request, and no
spelling of the markdown changes that — it is the sanitizer, not the renderer,
and it is applied to every comment on the site.

Three workarounds were considered and all three are worse:

- **A landing page that calls `window.open`.** Popup blockers require a user
  gesture in the *same* task; a script running after a navigation has none, so
  this is blocked in Chrome and Safari — silently, which is the worst kind.
- **`target="_blank"` on the confirm page's own form** (that one survives, it is
  our HTML). It would leave a stale confirm page in the original tab and does
  nothing at all for the fast path, which is a 303 on the GET with no form in
  it.
- **A `javascript:` or intermediate-redirect trick.** Refused by the same
  sanitizer, and by this door's `default-src 'none'`.

What is true and worth telling people: **Back returns to the pull request in one
press.** The GET redirect and the POST redirect are both 303s, which replace
rather than stack, so there is no redirect loop to fight through. `⌘`/`ctrl`- or
middle-clicking the badge opens a new tab the way it does for any link.

### Camo is why the badge is one invariant image

Camo sends no cookies and no user identity, and its path is a pure function of
the canonical URL. Two consequences, both settled:

- **A per-viewer badge is impossible.** Not hard — impossible. The image request
  carries nothing that identifies a reader.
- **A parameterized badge multiplies a heavily-cached object into one per PR
  comment** for information the reader already has: they are looking at the pull
  request.

So `GET https://go.<domain>/badge.svg` takes no query string and does not vary
by repository, branch or viewer. It is served with
`Cache-Control: public, max-age=3600`, a compile-time `ETag` (the first 16 hex
of the sha256 of the embedded bytes), `Vary: Accept-Encoding` and
`X-Content-Type-Options: nosniff`, through `http.ServeContent` so `HEAD`, `Range`
and `If-None-Match` are all free.

It is mounted **outside** the auth wrapper, and it has to be:
`edgeauth.Require` stamps `Cache-Control: no-store` on every response including
the ones it allows (`internal/edgeauth/middleware.go:50`), which alone defeats
camo — and an uncredentialed fetch gets either a 303 to an HTML sign-in page or
a 401, chosen by a **substring** test on `Accept`
(`middleware.go:105`, `strings.Contains(…, "text/html")`). Camo's exact `Accept`
header is not knowable from here and does not matter: both branches are a broken
image.

### Why no `<picture>` dark variant

It survives the sanitizer, so this is a choice and not a limitation. A themed
badge needs a second badge URL (`badge.svg?theme=dark`), which reintroduces the
parameterized image the previous section rules out. Instead the badge brings its
own opaque dark ground — a `#27272A`→`#18181B` pill with a `#3F3F46` border,
`#FAFAFA` text and a `#FACC15` rocket, the same pinned pair every console uses
for its `.logo` tile in *both* themes — so one image reads correctly on a light
and a dark GitHub. Inside a `<picture>`, GitHub's styling is not injected at all
and the SVG must carry its own rounded corners anyway; ours does.

The rocket is an inline `<path>`, not a `🚀` glyph. An SVG loaded through `<img>`
is an isolated document: no webfont, no `@import`, no `<image href>`, no
`xlink:href`, nothing external of any kind resolves. Text is drawn in a
hardcoded system stack with `textLength` + `lengthAdjust="spacingAndGlyphs"`
(shields.io's trick) set slightly *under* the natural Helvetica width, so a
machine with a wider fallback font compresses the label instead of spilling it
out of the pill.

`alt="Open in Sparkbox"` reads as an action, so a blocked or broken image
degrades to a sentence somebody can still act on rather than to a filename.

### Why the snippet declares `height` and never `width`

This looked for a while like an open risk, and it is not — it is a settled
reason to keep the snippet exactly as it is.

Declaring **both** attributes makes GitHub inject `aspect-ratio: <width>/<height>`
computed from the numbers you typed, not from the image. So a snippet that says
`width="172" height="28"` pins the badge to 172×28 forever in every comment
already posted, and redrawing the badge wider later would letterbox every one of
them. Declaring **`height` only** makes GitHub inject `max-width:100%;
height:auto; max-height:28px` and no ratio at all, so the SVG's own intrinsic
172×28 governs — and a future redraw at a different width simply renders at that
width in comments written years earlier.

That is the whole argument for the asymmetry, and it runs opposite to the
intuition that pinning both dimensions is the safer choice. Do not "fix" this by
adding a `width`.

The one thing height-only gives up is the `border-radius:6px` and muted
background GitHub injects for the two-attribute form. The badge does not need
them: it draws its own `rx="7"` pill on its own opaque ground, which is also what
makes it render identically inside `<picture>`, in a plain `<img>`, and anywhere
else that is not GitHub at all.

---

# Part 3 — there is no `tag=` parameter, and there will not be one

This is a security property with a named mechanism, not a matter of taste.

Tags on a sandbox select three things: the secrets pushed into it, the
repositories cloned into it, and the egress it is allowed. The first one is what
closes this question.

- `ctlops.Ops.Create` stamps tags *before* the sandbox is built, and its own
  comment says why: "Create fires the secret-env push asynchronously and the
  tags decide its contents" (`internal/ctlops/sandbox.go:33-36`).
- The push contents come from `secrets.Store.EnvForSandbox`
  (`internal/secrets/store.go:569-578`), which selects every secret of the owner
  that shares a tag with the sandbox:

  ```sql
  JOIN secret_tags st ON s.id = st.secret_id
  JOIN sandbox_tags bt ON bt.tag = st.tag AND bt.owner = s.owner
  WHERE bt.sandbox = ? AND s.owner = ?
  ```

A `tag=` in a URL is therefore a control the *author of a public comment* holds
over which of the **clicker's** decrypted secrets are pushed into a VM whose
checkout sits at a branch that same author chose. Constraining the parameter to
a tag that selects the named repository does not close it — a repository carried
on both `dev` and `prod` still lets the author pick — and it adds a second place
for the tag rule to live, encoding a decision that goes stale the moment the
attachment's tags change.

**Tags come from the matched attachment's stored `Tags` and from nowhere else.**
The confirm page prints the exact set the create will carry — `{default} ∪
att.Tags`, because `default` is stamped on every create — along with an
inventory of the other repositories those tags will drag in, since `default` is
usually where the surprise lives.

The related residual, stated rather than left implicit: the person who posts a
button chooses the branch, and an agent started in that checkout reads
instructions written by whoever chose it, with a credentialed `gh` on its `PATH`.
This is not remote code execution — the clone is `git clone --filter=blob:none
--branch "$ref"` with no submodule init and no hook execution, and the ref
grammar forbids a leading `-` so there is no option injection — but it is real
and social. It is said out loud in three places: the confirm page, the last
`ctl badge` note, and here.

---

# Part 4 — what a click does

**Step 0, before any human.** GitHub's camo fetches
`GET https://go.<domain>/badge.svg` with no cookies and an unknown `Accept`.
200, `image/svg+xml`, publicly cacheable. No session is created, nothing is
looked up, nothing is written.

**Step 1, the click.** `https://go.<domain>/wandb/hivemind?ref=feat/x` reaches
the proxy, which resolves the subdomain and finds the launch handler in its
reserved map (`internal/proxy/proxy.go:559`) — before the apex redirect, before
the `-xterm` suffix split, before the route table, and before the proxy's own
authorization, which only runs on the route path. The handler owns its own gate.
DNS and TLS cost nothing extra because `go` is one label under the wildcard the
zone already has.

**Step 2, the sign-in bounce.** No session cookie means `edgeauth.Require` hands
off to `challenge`, and a browser (`Accept` contains `text/html`) gets:

```
303 See Other
Location: https://login.<domain>/?return=https%3A%2F%2Fgo.<domain>%2Fwandb%2Fhivemind%3Fref%3Dfeat%2Fx
```

The return URL is `r.URL.RequestURI()` — path *and* query — so the branch
survives the round trip, and the login page admits it because the host is under
the zone. A **first-ever** sign-in inserts one more hop (`POST /session` →
`/enroll?return=…`); the launch page is idempotent, so arriving after two
redirects is indistinguishable from arriving after one. Somebody already signed
in at `my.<domain>` skips all of this: the session cookie's domain is the whole
zone.

A non-browser client (no `text/html` in `Accept`) gets 401 and no redirect.

**Step 3, the resolve.** Store reads and a control-plane list — and no writes of
any kind — under the handle from the verified session, never from the URL:

1. `ListRepos(handle)` — the only call that distinguishes "you never attached
   this" from "attached, but no sandbox of yours carries a selecting tag".
2. `SandboxesForRepo(handle, "github.com", att.Slug)`.
3. `Ops.List` indexed by name, then `ReposForSandbox` per name for the effective
   branch.

**Step 4a, a match — the common case.** 303 straight to
`SandboxInfo.TerminalURL`, taken verbatim from the record. No page paints at
all. This is the repeat click, the one that happens fifty times a day: a cookie
check, three reads, one header.

**Step 4b, no match — the confirm page.** 200, server-rendered, and **exactly
one `<script>`** — `internal/launch/progress.js`, inline — admitted by the
sha256 of its bytes and by nothing else:
`default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:;
script-src 'sha256-…'; form-action 'self' https://*.<domain>; base-uri 'none';
frame-ancestors 'none'`, next to `no-store`, `X-Frame-Options: DENY`,
`Referrer-Policy: same-origin` and `nosniff`.

The digest is computed at package init from the **rendered** page, not from the
file on disk, and that is load-bearing: `html/template` lexes a `<script>`
element as JavaScript and strips its comments on the way out, so the bytes a
browser hashes are never the bytes in the source file for any script carrying a
comment. Hashing the file yields a policy that is right about the intent and
wrong about the document, and the failure mode is the whole script silently not
running in production while every test passes.

`Referrer-Policy` is `same-origin` and **not** `no-referrer`, which looks like
the weaker choice and is the only working one: under `no-referrer` a browser
serializes the `Origin` of this page's own same-origin form POST as the literal
`null`, which the first-party check refuses — so the button would 403 every
time, in real browsers only, where a test that sets `Origin` by hand can never
see it.

It names
the repository under the casing its owner attached it with, the branch (or "its
default branch"), the exact tag set, the also-clones inventory, the sentence
about running a stranger's branch, and "Your sandboxes on this repository" —
every near miss, with its branch and state, each one a link to its own terminal.

The create control is a bare form with no fields and no hidden inputs:

```html
<form method="post" action="/wandb/hivemind?ref=feat/x" data-busy="Creating…">
  <button type="submit">Create a sandbox</button>
</form>
```

`progress.js` is the whole reason there is any script here. The create behind
that button takes up to fifteen seconds and a plain form POST paints nothing
while it runs — the page simply sits there, and what people do with a page that
sits there is press the button again. On submit it puts `creating` on the
`<body>`, which spins the button, relabels it, and reveals "Building your
sandbox — this takes a few seconds. Keep this tab open." It never calls
`preventDefault` on the first submit: with JavaScript off the form posts exactly
as it did before and only the spinner is lost. It is the presentational half of
the same fix `singleflight` is the server half of.

**Step 5, the POST.** Gated by `edgeauth.Require` *wrapping* a launch-local
first-party check, in that order — so an expired session is a sign-in bounce and
never a 403. The check passes on the `X-Sparkbox-Console` header, an `Origin`
equal to the configured origin, **or** an `Origin` matching the request's own
origin (X-Forwarded-Proto aware). That third clause is lifted from
`internal/xterm` (`turbo.go`, `ws.go`), which forked the same rule for the same
reason: a zero-JS page can only ever supply an `Origin`, and the shared
middleware's hardcoded `https` origin can never match on a `--proxy-tls=false`
dev loop, which would leave the page permanently 403 with a remedy it has no
script to obey.

Then, collapsed by `singleflight` on `handle \0 lower(slug) \0 ref`:

1. the whole resolve runs **again** — that is the idempotency. A double-click, a
   second tab, or a retry a minute later finds the first request's sandbox and
   redirects to it instead of building a second one;
2. `Ops.Create` with `Tags` taken verbatim from the attachment and the *scoped*
   ref form `[]RepoRef{{Slug: att.Slug, Ref: want}}` (nil when the branch folds
   to the attachment's own default), so an `ambiguous_ref` failure is
   structurally impossible even after somebody adds a second repository to
   `default`;
3. one `Ops.Get` before answering — we never hand out a URL for a record we
   cannot read back;
4. 303 to the terminal.

The whole create is synchronous and capped at 15 seconds by `Ops.Create` itself.

**Step 6, the handoff.** The browser lands on `<name>-xterm.<domain>`, same
zone, same cookie, no second sign-in — and, when the sandbox holds exactly one
checkout, **in that checkout's directory**. The clone worker publishes the path
to `/run/sparkbox/repos.dir` and `/etc/profile.d/50-sparkbox-repo.sh` cds a
login shell into it, guarded on being interactive, on starting in `$HOME`, and
on the path being a directory under that home (`SPARKBOX_NO_REPO_CD=1` opts
out). With more than one checkout there is no unambiguous answer — a launch
link's sandbox also clones everything the clicker keeps on `default`, and
nothing in the guest knows which repository was clicked for — so the login stays
in `$HOME` and the banner names the directory they share:

```
repos: 1 ready in ~/hivemind
repos: 3 ready in ~/src
repos: 2 ready, 1 failed — run `sparkbox repos`
``` The launch package calls nothing else: the
terminal's own WebSocket owns the boot, under a 15-minute budget that covers a
cold start and even an archive restore, and it renders progress while it waits.
Blocking a browser on the resume here instead would die behind a proxy's idle
timeout while *hiding* progress the terminal already shows.

## What a GET may never do

`GET /`, `GET /badge.svg` and `GET /{owner}/{repo}` never create a sandbox,
never resume or wake or restore a VM, never write a tag or ref row, never attach
a repository, never mint or extend a session (this package writes no
`Set-Cookie` at all), never consume a quota slot, and never fetch a third-party
URL. That is pinned by a test driving every GET route against a fake whose
`Create` calls `t.Fatal`, not by review.

## The other screens

- **Not attached (400).** The most-visited state this door will ever have: a
  first-time reader arriving from a public comment. Buttonless, and it teaches
  rather than reports — it carries the literal `ssh ctl@<domain> repo add
  <owner>/<repo> --tag <t>` and explains that a sandbox clones *their*
  attachments, not the link author's. Auto-attaching is not available:
  `AttachGate` refuses any account whose GitHub link is a weak assertion, which
  is exactly this visitor.
- **Sandbox limit (429).** Part 6.
- **Anything else.** One generic screen. A `KindInternal` message is a store or
  driver string, so it goes to the log and the visitor gets a generic sentence;
  a malformed link additionally gets the URL grammar, because the reader is
  looking at a URL somebody typed by hand and the correct shape is the whole
  answer.

---

# Part 5 — reuse or create

Clicking the same button twice must land in the same sandbox. The rule is
applied **symmetrically at both ends**, and that symmetry is the whole trick:

```
normalize(attRef, x) = "" if attRef != "" && x == attRef, else x

reuse  ⟺  normalize(att.Ref, effective_ref_of_box) == normalize(att.Ref, link_ref)
```

The box's effective ref comes from `repos.Store.ReposForSandbox`, which selects
`COALESCE(NULLIF(o.ref, ''), r.ref)` (`internal/repos/store.go:399-411`) — the
per-sandbox override already folded over the attachment's default. It is the
same query the guest's checkout manifest is built from, so the page matches one
authority rather than a second reconstruction of it.

Never build this from `SandboxRefs`: that table records **overrides only**, so a
sandbox sitting on the attachment's default has zero rows there and would be
invisible to the match.

| the link says | attachment's ref | the box's effective ref | verdict |
|---|---|---|---|
| (none) | `""` | `""` | **reuse** |
| (none) | `main` | `main` | **reuse** — the row a naive `eff == want` gets wrong |
| `?ref=main` | `main` | `main` | **reuse** |
| `?ref=feat/x` | `main` | `feat/x` | **reuse** |
| `?ref=feat/x` | `""` | `""` | create |
| `?ref=main` | `""` | `""` | create — the one residual |

Refs are compared **byte for byte**, never folded: nothing in this tree folds a
ref, and `feat/X` and `feat/x` are two branches. Slugs *are* matched
case-insensitively, because the store's slug columns are `COLLATE NOCASE`; the
casing that gets displayed is always the stored one.

Ranking, when more than one sandbox matches: reachable and running first, then
paused (a warm restore), then archived last (an object-storage download and a
cold boot); ties break on last-active, newest first. Unreachable boxes are
dropped entirely when a reachable one exists — that node is not answering the
control plane, so its terminal would hang.

## The residual duplicate, and both mitigations

The last row is real: an attachment recorded with **no** default branch, plus a
link that spells the default branch out loud, plus a sandbox already sitting on
that unnamed default. Nothing in this codebase knows what a repository's default
branch is called, so the two cannot be folded together, and the only complete
fix is a GitHub App call on the click path — a network round trip, a token
`AttachGate` refuses for assertion-linked accounts, and a new failure mode on
the fastest surface in the product. Deliberately not done. Mitigated from both
ends instead:

1. **Emitting.** `ctl badge` refuses to mint a ref it can prove is redundant: if
   the repository is attached, the attachment has a ref, and `--ref` equals it,
   the ref is dropped from the URL and a note says so. This is the emitting half
   of the same normalize rule.
2. **Receiving.** The confirm page lists "Your sandboxes on this repository" —
   every near miss with its branch and state, each a link straight into its
   terminal. A human who is about to create an accidental duplicate can see the
   original and click it instead.

The cheapest structural fix is on the attachment, not the link: attach with an
explicit branch (`repo add wandb/hivemind --ref main --tag hm`) and the residual
row cannot occur for that repository at all.

---

# Part 6 — "you are already running as many sandboxes as this host allows"

This is the failure a click-to-create button hits routinely, so it gets a
first-class screen rather than a bare 429.

`--max-running-per-owner` defaults to **2** (`cmd/sparkbox/main.go:126`),
enforced by `host.Manager.admitCost`, surfaced by `ctlops` as `KindLimit`, which
is HTTP **429**. A visitor with two running sandboxes who clicks a third badge
gets a page that:

- names one of **their own** running sandboxes in a command they can select and
  run: `ssh ctl@<domain> pause <name>` (the names come from the error's own
  details, resolved under the caller's handle, so nothing is disclosed that they
  cannot already list);
- says that pausing loses nothing — the disk and everything on it stays, and the
  sandbox comes back warm;
- keeps the button, as "Try again", because nothing was created and there is
  nothing to clean up.

The neighbouring kinds map the way `ctlops` already defines them: `KindQuota` →
507 is the **disk** pool, not the sandbox count; `KindCapacity` → 503;
`KindInvalid` → 400.

Note that `--max-sandboxes-per-owner` defaults to `0`, meaning unlimited, so
paused and archived sandboxes accumulate even though running ones are capped. On
a host where launch links are public that is worth setting deliberately rather
than inheriting.

---

# Part 7 — PRE-DEPLOY CHECK (do this before the mount ships)

**Read this part even if you read nothing else.**

Mounting the door claims `go.<domain>` unconditionally. Three things in this
platform can already be called `go` — a sandbox, a route row, and a user
handle — and `internal/reserved` is the single list that refuses all three. But:

- `reserved.Name` is consulted **at create and rename time only**
  (`internal/users/store.go:57`, `internal/routes/store.go:72`,
  `internal/host/manager.go:39`). Nothing is validated retroactively, so a name
  that predates the reservation is still sitting there.
- `warnSubdomainCollision` (`cmd/sparkbox/main.go:1468`) **only logs**. It
  checks the manager and the route table, writes a `log.Warn`, and returns. It
  blocks nothing, removes nothing, and refuses to start nothing.
- Reserved dispatch wins in `proxy.Server.ServeHTTP`
  (`internal/proxy/proxy.go:559`) **before** the route lookup.

So a squatter does not produce an error. It goes **dark, silently**, at the next
deploy: the sandbox or route that used to answer at `go.<domain>` now reaches
the launch handler instead, with one warning line in a startup log nobody is
tailing to explain it. The fix is a rename **before** the mount ships, not
after.

Run the checks below on **every** host that will get this deploy — on the DGX
and on CKS, not on whichever one you happened to be logged into.

### 1. The cheapest probe — from anywhere, before deploying

```sh
curl -sS -o /dev/null -w '%{http_code}\n' https://go.<domain>/
```

`404` means nothing answers there and you are clear. **Anything else** — a 200,
a 302/303 to the sign-in page (a private route answers that way), a 502 — means
something already holds the name. Investigate before deploying.

### 2. The three stores, on the host

Both answers live under `--state-dir`: the control stores are one SQLite file,
`<state-dir>/sparkbox.db`, and the sandbox ledger is
`<state-dir>/sandboxes.json`, a JSON object keyed by sandbox name. (`--vm-state-dir`
holds disks, sockets and memory snapshots, and is not where to look.)

```sh
# routes and handles (both live in sparkbox.db)
sqlite3 <state-dir>/sparkbox.db \
  "SELECT subdomain, sandbox, owner FROM routes WHERE lower(subdomain) = 'go';"
sqlite3 <state-dir>/sparkbox.db \
  "SELECT handle, status FROM users WHERE lower(handle) = 'go';"

# sandboxes (the gateway's ledger covers boxes placed on fleet nodes too)
jq -r 'keys[]' <state-dir>/sandboxes.json | grep -ix go
```

`ssh ctl@<domain> user ls` answers the handle question without a shell, if you
have operator rights. If the host has no `sqlite3` or `jq`, copy the two files
off and query them somewhere that does — do not settle for grepping the sqlite
file, which will match the string `go` inside dozens of unrelated columns and
tell you nothing.

Note what each query gives you besides a yes/no: the route row and the
`sandboxes.json` record both name an **owner**, and you will need it for step 3.

Where the paths are, on the two live hosts:

| host | `<state-dir>` | how to get there |
|---|---|---|
| DGX (`sparky`) | `/srv/sparkbox/state` | ssh to the box; the exact flags are in the systemd unit |
| CKS | `$SPARKBOX_DATA_DIR/control`, default `/var/lib/sparkbox/control` (`deploy/kubernetes/entrypoint.sh:28`) | `kubectl -n sparkbox-poc exec deployment/sparkbox-gateway -- …` |

### 3. Fix what you find, before the deploy

| squatter | what breaks at the mount | the fix |
|---|---|---|
| a sandbox named `go` | its web front door; the sandbox itself keeps running and stays reachable over ssh | its **owner** runs `ssh ctl@<domain> rename go <newname>` — the URLs and the ssh name move with it. `ctlops` gates every sandbox verb on strict ownership (`internal/ctlops/ops.go:510-519`), so an operator cannot do this on somebody's behalf from the ctl channel; that is why step 2 gives you the owner to go and ask. The rename pauses the sandbox and it cold-boots after, so processes running inside it do not survive |
| a route row at `go` | that route, silently | `curl -sX DELETE http://127.0.0.1:8080/v1/routes/go` on the host (the local control API, `--api-addr`, loopback and unauthenticated) and re-add it under another subdomain |
| a handle `go` | nothing today — no per-account hostname is served | still record it. It is a pre-reservation leftover that `reserved.Name` would refuse now, and the reservation exists so a future per-account hostname is not blocked. Renaming a handle re-seals that account's secrets, so it is a deliberate procedure, not a quick fix |

If you cannot clear a squatter before the deploy, the honest move is to mount
the door under a different label (`--launch-subdomain sparkbox`, say) — and then
live with that label forever, per Part 1.

---

# Part 8 — deploying, and the first badge

**No manifest change is needed.** Both container entrypoints end their
`sparkbox` invocation with `"$@"` (`deploy/kubernetes/entrypoint.sh:426`,
`gateway-entrypoint.sh:81`), and the flag defaults to `go`, so the door comes up
with no edit at all. Nothing needs to be re-passed on a redeploy either — unlike
the tag-scoped flags this deployment has been bitten by.

**Warm the certificate on CKS, deliberately.** CKS defaults to the `autocert`
TLS provider (`SPARKBOX_TLS_PROVIDER`, `entrypoint.sh:50`), which issues
**per-SNI on the first handshake**. So the first click after the deploy pays a
live ACME round trip, and if the LoadBalancer does not forward port 80, HTTP-01
cannot complete: `go.<domain>` then fails *inside the TLS handshake*, with no
request, no log line, and every existing hostname still green. Hit it yourself
before anybody else does. The DGX runs certmagic over `[domain, *.domain]` and
is not affected.

**Verify after the roll:**

```sh
curl -sI https://go.<domain>/badge.svg
#   200, content-type: image/svg+xml, cache-control: public, max-age=3600
curl -sI -H 'Accept: text/html' https://go.<domain>/wandb/hivemind
#   303 to https://login.<domain>/?return=…
```

and confirm the startup log carries `launch links enabled` with its `url` and
`badge` fields, and **no** subdomain-collision warning.

**Then post one badge in a private pull request and click it end to end** —
signed out, so the sign-in bounce is exercised too — before any public one is
written — the badge, the bounce, the confirm page and the terminal handoff are
four separate surfaces and a private PR exercises all of them for free.

One last flag interaction worth knowing: on a host with `--open-signup`, the
startup log warns that anyone who can sign up can now create sandboxes from a
link in a public comment. It warns rather than refuses, because that combination
is a legitimate configuration for an internal host — but it is a combination
somebody should have chosen on purpose.

---

## See also

- `ssh ctl@<domain> help repos` — `badge` lives on that page.
- [`docs/github-repos-design.md`](github-repos-design.md) — attachments, tags
  and the tokens a checkout uses.
- [`docs/rest-api-and-xterm-design.md`](rest-api-and-xterm-design.md) — the
  browser terminal a launch link hands off to.
