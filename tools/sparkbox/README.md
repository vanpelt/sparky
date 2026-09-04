# sparkbox

A single-host MVP of an [exe.dev](https://exe.dev)-style agentic sandbox
service in Go: on-demand sandbox VMs behind a **smart SSH gateway** with
resume-on-connect. Companion implementation to
[`docs/agentic-sandbox-design.md`](../../docs/agentic-sandbox-design.md).

```
ssh signup@gateway         → registers your key (invite code + handle)
ssh new@gateway            → creates a sandbox, tells you its name
ssh -t new+foo@gateway bar → names it `foo`, tags it `bar` (-t: see below)
ssh <name>@gateway         → resumes it if suspended, drops you in
ssh ctl@gateway ls         → your sandboxes; `help` indexes the rest, `help <topic>` drills in
https://<name>.hivemind.tools       → reverse-proxy to a port inside the sandbox
https://<name>.hivemind.tools:4444  → …any port; private by default, login-gated
POST /v1/sandboxes         → control API (create/list/pause/resume/destroy/routes)
```

SSH has no Host header, so the sandbox name travels in the SSH *username*;
the *user* is identified purely by their public key (the exe.dev model). HTTP
*does* have a Host header, so web traffic routes by subdomain instead:
`<subdomain>.hivemind.tools` maps to a sandbox and a guest port (default `:8000`,
subdomain defaults to the sandbox name). Idle sandboxes are automatically
paused by a reaper and transparently resumed on the next connection — over SSH
*or* HTTP.

Proxy requests and real SSH stream traffic refresh the idle clock. The reaper
also treats sustained tap traffic above `--activity-net-kb` as work, which
protects unattended agents talking to remote services. Host CPU use is not an
activity signal by default: idle Docker and Compose stacks commonly consume a
few percent of a core and would otherwise keep unrelated VMs warm in lockstep.
Operators running unattended CPU-only jobs can opt back in with
`--activity-cpu-pct`; pinning remains the explicit always-on policy.

One wrinkle on the `new@` / `new+<name>@` door: the words after it are read as
**tags**, never as a command, because a freshly created sandbox always gets a
shell. But `ssh host word` makes your ssh client skip terminal allocation, and
a shell with no terminal prints no prompt and gives you no line editing — you
end up typing blind into a pipe. Pass `-t` whenever you pass tags:

```
ssh -t new+foo@gateway claude   # sandbox `foo`, tagged `claude`, working prompt
```

Connecting to an existing sandbox (`ssh foo@gateway`) needs no `-t`, and
`ssh foo@gateway <cmd>` does run `<cmd>` in the guest as you'd expect.

## Identity

Users are SSH public keys and nothing else — no passwords. (A browser session
for a private web route is a cookie, but its value is a token you mint from that
same SSH key; see *Authenticated forwarding* below.) An account (handle +
keyring + optional *verified* GitHub link, plus an optional email) lives in the
sqlite store next to the routes; `users.conf` remains the bootstrap seed, and
the accounts it names are the operators. Registration happens over the only
door a stranger has, SSH: `ssh signup@<domain>` runs a short dialog gated by a
single-use invite code (`ssh ctl@<domain> invite` mints one). GitHub linking
needs no OAuth app — it checks the key you're connected with against
`github.com/<login>.keys`, which is the same evidence GitHub accepts for a
push.

An operator can also admit people straight from GitHub, adopting the keys
github.com publishes for a login so there is no code to deliver and nothing to
type — one person with `ssh ctl@<domain> user add <login>`, or a whole
organization with `gh auth token | ssh ctl@<domain> user sync-github-org <org>`.
Provisioned accounts are ordinary users, never operators. See
[`docs/onboarding-users.md`](docs/onboarding-users.md), which also covers signing
the agent CLIs in.

Each sandbox can also prove who it belongs to. Sparkbox is an OIDC issuer at
`oidc.<domain>`, and every guest gets a fresh 1h ES256 id token dropped at
`/var/run/secrets/hivemind/token` — the path hivemind's auth chain already
reads — so `hivemind start` federates with no secret in the VM and nothing
pasted. The guest is authenticated by its network position (it can only reach
the metadata endpoint over its own tap), exactly like a cloud IMDS. See
[`docs/identity-federation-design.md`](docs/identity-federation-design.md).

Sparkbox can also keep an otherwise-idle VM reachable while HiveMind reports a
live agent session inside it:

```text
--hivemind-api=https://hivemind.wandb.tools
--hivemind-audience=https://hivemind.wandb.tools
--hivemind-presence-interval=1m
```

The feature is off unless `--hivemind-api` is set. Each host exchanges a fresh
device-bound OIDC token, asks HiveMind only about that sandbox, and installs
the returned short lease in the idle reaper. The lightweight lease query runs
at the configured interval; the heavier paginated session history and count
refresh at most once every ten minutes. The lease prevents pause but still
allows memory ballooning, and a history-query failure does not discard a lease
already received. A successful history response refreshes an in-memory,
non-persisted list of that VM's HiveMind sessions, including titles and
dashboard links. Fleet nodes run the same loop locally and relay token minting
to the gateway, so the signing key remains gateway-only. If
`--hivemind-audience` is customized, add the same value to the gateway's
`--oidc-audiences` allowlist.

## Authenticated forwarding

Web routes are **private by default**: a visitor to `<name>.hivemind.tools`
(any port) is redirected to sign in and must be the sandbox's owner or an
operator. The login credential is a session token you mint from your SSH key —
`ssh ctl@<domain> session-token` — and paste into the sign-in page (or send as
`Authorization: Bearer <token>` for API access). It is a keyed MAC whose key is
HKDF-derived from the OIDC signing key, so this adds no new fleet secret and no
server-side session store. Once authorised, the edge forwards the visitor's
identity upstream as `X-Forwarded-User` / `X-Forwarded-Email` /
`X-Forwarded-Preferred-Username` (oauth2-proxy names; client-supplied copies are
stripped first), so the app behind the port can do its own authorization.

Somebody already signed in to HiveMind can skip the paste entirely. With

```text
--hivemind-signin-orgs=wandb
```

(and `--hivemind-api`, which is the back channel), the edge mounts a federated
door at `https://login.<domain>/handoff`: HiveMind's dashboard POSTs a
single-use code, sparkbox redeems it server-to-server, checks the visitor is in
one of the named GitHub orgs, and signs them in — creating the account on first
arrival from the keys `github.com/<login>` publishes. It never silently swaps a
session for a different account or creates one without asking. An empty org list
is off, never everyone. See
[`docs/hivemind-signin-design.md`](docs/hivemind-signin-design.md).

Publish a port to the world with `ssh ctl@<domain> share <name> <port> public`
(`private` re-gates it). Visibility is settled per port, so opening a preview
never opens the debugger beside it; with no port, `public` opens only the
machine's default port while `private` closes every one. Set the forwarded
address with `ssh ctl@<domain> email set you@example.com`. Any port works
without pre-registering a route: a boot-time
`iptables REDIRECT` (`deploy/sparkbox-net.sh`) funnels the private port range to
the single TLS edge, which recovers the dialed port via `SO_ORIGINAL_DST`. See
[`docs/authenticated-proxy-design.md`](docs/authenticated-proxy-design.md).

## The same sandboxes, without ssh

Everything `ssh ctl@<domain>` does is also a REST call, and every sandbox also
has a shell in a browser tab. Both authenticate with the session token above,
and both run through the same control-plane core (`internal/ctlops`) as the SSH
channel — one ownership check, one set of timeout budgets, three transports.

```sh
# `session-token` writes the token to stdout and its notes to stderr, with the
# ctl channel's CRLF line endings — strip the \r or the header is malformed.
TOKEN=$(ssh ctl@hivemind.tools session-token | tr -d '\r\n')
API=https://api.hivemind.tools
AUTH="Authorization: Bearer $TOKEN"

# who am I, and what does this host actually have configured?
curl -sH "$AUTH" $API/v1/whoami
curl -sH "$AUTH" $API/v1/capabilities
# {"archiving":true,"snapshots":true,"scheduling":true,"tags":true,
#  "routes":true,"session_tokens":true,"terminal":true,"template_tags":true}

# create one (unnamed → an adjective-noun name), then list them
curl -sH "$AUTH" -H 'Content-Type: application/json' \
     -d '{"tags":["prod"]}' $API/v1/sandboxes
# {"name":"plucky-panda","state":"running","tags":["prod"],…,
#  "url":"https://plucky-panda.hivemind.tools",
#  "terminal_url":"https://plucky-panda-xterm.hivemind.tools"}
curl -sH "$AUTH" $API/v1/sandboxes          # {"sandboxes":[…]}

# act on one; retype the tag set; free the slot
curl -sH "$AUTH" -X PUT -H 'Content-Type: application/json' \
     -d '{"tags":["prod","gpu"]}' $API/v1/sandboxes/plucky-panda/tags
curl -sH "$AUTH" -X POST $API/v1/sandboxes/plucky-panda/pause
```

Browsable docs and the machine-readable contract live on the same host:
`https://api.<domain>/docs`, `/openapi.json`, `/openapi.yaml`. Collections come
back wrapped (`{"sandboxes":[…]}`) so a field can be added later without
breaking every parser, and failures past the auth gate are all one envelope —
`{"error":{"kind","op","code","message"}}` — with a stable `code` to match on
instead of a message to grep.

Operations that can take minutes (archive, resize, restore) answer
synchronously when they finish fast and escalate to `202` + a job resource when
they don't, because no proxy on the path tolerates a fifteen-minute request.
`Prefer: wait=0` forces the `202` immediately; `Prefer: respond-async` does too;
either way you poll `/v1/jobs/{id}`. Asking twice for the same work on the same
sandbox returns the job already running rather than starting a second one, and
`Idempotency-Key` replays any mutation for 24 hours.

The browser terminal is `https://<name>-xterm.<domain>` — an xterm.js page over
an authenticated WebSocket into a real PTY in that sandbox, with the same
resume-on-connect as `ssh <name>@<domain>`. Open it and you get a shell; nothing
to install, and the sign-in is the same passkey/token flow as any private route.
When the presence monitor has observed a HiveMind session for the VM, the
terminal header links its most recently active session by title.
Its hamburger menu links straight to the sandbox's default proxy endpoint at
`https://<name>.<domain>`, following whichever guest port the default route
currently selects. The owning VM host lightly probes Sparkbox's supported
browser ports and returns those speaking HTTP in the terminal's vitals. Any
syntactically valid HTTP response counts, including an API's entirely normal
`404 /`; HTML titles, product headers, and JSON media
types supply a best-effort service name for the menu. Live non-default ports
appear as additional Proxy rows;
if the default port is not HTTP-ready, its row explains which port needs a
service instead of opening a known-dead endpoint. A three-second TCP check
notices listeners coming and going, while HTTP classification and metadata are
refreshed no more than once a minute unless the port first closes. Probes never
resume or touch an idle sandbox.
One host per sandbox, so the browser's own origin isolation keeps one sandbox's
page from scripting another's socket. The identical bridge is mounted at
`wss://api.<domain>/v1/sandboxes/<name>/terminal` for clients that are not
browsers — bearer token, no `Origin`, subprotocol `sparkbox.terminal.v1`, raw
binary frames each way.

Pause the sandbox under a live terminal and the session is hung up cleanly: the
page restores your terminal modes, prints why, and offers Reconnect rather than
reconnecting on its own and resurrecting the VM the reaper just parked. Typing
counts as activity (throttled to once a minute); merely having the tab open does
not, so a forgotten tab cannot pin a sandbox warm forever.

The guest login banner lists every attached repository with its checkout path,
current branch, and compact ahead/behind/dirty markers. `sparkbox status` shows
the same resource and lifecycle snapshot used by the xterm `/vitals` strip,
plus the host's latest repository map and cached HiveMind session catalog
(`--json` is the stable agent-facing form); `sparkbox repos` inspects the
filesystem immediately, and
`sparkbox repos sync` is the only post-boot command that may safely fast-forward
a clean checkout. A fetch-only guest timer refreshes the gateway's advisory repo
state every five minutes, including unpushed commits and divergence, so consoles
can warn before destructive actions. The browser terminal's **Start a new
shell** action opens a separate xterm tab and leaves the current PTY and
scrollback intact.

`--api-subdomain` and `--xterm-subdomain` move or disable either surface. See
[`docs/rest-api-and-xterm-design.md`](docs/rest-api-and-xterm-design.md).

> The terminal host is one label on purpose. A wildcard matches exactly one
> label — RFC 4592 in DNS, RFC 6125 in certificates — so the dotted
> `<name>.xterm.<domain>` would need a second wildcard in *both*, and hosted TLS
> front ends generally will not issue it: Cloudflare's universal certificate is
> `<domain>, *.<domain>` and stops there, so the deeper name dies inside the TLS
> handshake with `ERR_SSL_VERSION_OR_CIPHER_MISMATCH` and no sparkbox log line
> to explain it. `<name>-xterm.<domain>` needs nothing beyond the `*.<domain>`
> record and certificate every sandbox front door already uses. The cost is a
> reserved name suffix: sandboxes and routes may not end in `-xterm`.

The third way in needs neither an ssh client nor a token: a **button in a pull
request**. `ssh ctl@<domain> badge wandb/hivemind --ref feat/x` prints one line
of markdown to stdout (everything else goes to stderr, so `| pbcopy` gives you
exactly the snippet), and whoever clicks it signs in, gets *their own* sandbox
with the repository checked out on that branch, and lands in the browser
terminal above.

```
https://go.<domain>/<owner>/<repo>[?ref=<branch>]
```

The repository is in the path so that the no-branch form contains no `&` at all
and the branch form contains exactly one — a comment is immutable, and `&amp;`
is then the only escaping a human retyping the link can get wrong, in one place.
There is no `tag=`, `node=` or `name=` parameter and there will not be: tags
decide which of the clicker's secrets are pushed into the VM
(`secrets.Store.EnvForSandbox` joins on `sandbox_tags`), so a tag in a public
comment would be a stranger choosing which of *your* credentials meet a branch
*they* picked. Tags come only from your own attachment. Click the same button
tomorrow and it finds the sandbox it made you rather than building a second one:
a link's branch is matched against the effective ref the checkout manifest is
built from, with the attachment's own default folded out on both sides.

The badge is one static, parameterless SVG served outside the auth gate, because
GitHub proxies every image through camo — no cookies, no identity, one heavily
cached object per URL — and a gated image is a broken image on both of the
middleware's branches. The landing page ships zero JavaScript, which is what
lets it send an honest `default-src 'none'` and makes the create a bare form
POST.

`--launch-subdomain` moves or disables the door (empty disables it; so does an
empty `--xterm-subdomain`, since the whole payoff is a terminal to land in). It
defaults to `go` and should be treated as a one-time decision — the label is
inside links people paste into places nobody can edit. **Before mounting it on a
live host, run the squatter check in
[`docs/launch-links.md`](docs/launch-links.md#part-7--pre-deploy-check-do-this-before-the-mount-ships):**
reserved dispatch wins before the route lookup and names are validated only at
create time, so a sandbox or route already called `go` goes dark silently at the
next deploy, with one warning line to explain it.

## A tag can also name the disk you boot from

A tag on a sandbox already selects three things: the secrets pushed into it, the
repositories checked out, and the egress it is allowed. It can also select the
**rootfs**. Bind a snapshot to a tag and every sandbox you create carrying that
tag starts as a reflink copy of it — a fork you do not have to remember the name
of.

```
ssh ctl@<domain> snapshot create dev-box cuda-base   # capture a customized box
ssh ctl@<domain> snapshot bind cuda-base --tag cuda  # point the tag at it
ssh -t new@<domain> cuda                             # boots from cuda-base
ssh ctl@<domain> snapshot unbind --tag cuda          # back to the default image
```

`snapshot ls` shows which snapshots are bound. On REST it is
`PUT`/`DELETE /v1/templates/{tag}`.

A tag has exactly one base image, so binding again re-points it and reports what
it replaced; sandboxes already created from the old one keep the disk they were
built from. A create whose tags bind two *different* snapshots is refused with
both named rather than resolved by a precedence rule — a sandbox has one disk,
and a coin flip means somebody finds out twenty minutes later that they have the
wrong CUDA. `default` cannot be bound: every sandbox you create carries it, so
the binding would quietly become the base image for all of them.

On a fleet a binding is also a placement directive, because a snapshot is a file
in one machine's image directory: a tagged create lands on the machine holding
the template, and an explicit `--node` naming a different one is refused rather
than silently overridden either way.

Two things follow from templates being frozen disks. The agent CLIs in a template
are whatever was current the day it was captured, so `snapshot create` now
refreshes them from the host's verified cache first, and a long-lived sandbox can
do the same on demand with `sparkbox update-tools` from inside — served by its
own host over the metadata tap, so it works on a sandbox whose egress is filtered
by its tag, and no artifact crosses the fleet link. And the person who knows a box
is worth keeping is the one sitting in it, so they can capture it themselves:

```
sparkbox snapshot cuda      # from inside the VM: prints the plan, asks, then
                            # pauses this box and captures it
```

That prints what it will re-point, which of your sandboxes carry the tag, and
that re-pointing re-bases none of them, then asks — with no terminal to ask at it
refuses rather than proceeding. A sandbox may only re-point a tag it already
carries, so a compromised box gains persistence over tags it already held the
secrets for and nothing wider. `--guest-self-snapshot=false` turns the door off.

Design and the full refusal list:
[`docs/tag-templates-design.md`](docs/tag-templates-design.md). Hardened hosts
using `--disable-host-rootfs-mounts` support the same flow: the required secret,
machine-identity, and journal cleanup runs inside the guest before it is paused,
so the host never mounts a guest-authored filesystem.

When diagnosing a boot unit from inside a guest, `sudo journalctl -m -u <unit>`
is the useful default. `--merge` reads every machine-id directory on the disk,
including the boot immediately before the current one; plain `journalctl` only
selects the current machine id.

## Environments: one name for the whole way of working

A tag composes four things — secrets, checkouts, egress, disk — and until
recently the only place that name existed was as an argument to five unrelated
verbs. An **environment** is that name as an object: it owns exactly one tag,
*its name is the tag*, and it carries a description, some plain (non-secret)
variables, and the setup script the project needs.

```
ssh ctl@<domain> env create web --repo wandb/hivemind --secret GITHUB_TOKEN
ssh ctl@<domain> env set web --var NODE_ENV=test
ssh ctl@<domain> env build web         # returns as soon as the build STARTS
ssh ctl@<domain> env show web          # where it got to
ssh -t new@<domain> -- --env web       # a sandbox booted from the disk it built
```

`env build` boots one ordinary sandbox called `<name>-build` from the stock
image, runs the environment's setup script inside the primary checkout as the
login user, and — when the script succeeds — the *gateway* captures that sandbox
and binds the capture to the tag, so every later `--env web` starts from the
finished disk. The builder is then destroyed. The script is the one stored with
`env script web --set`, or, when there is none, `.sparkbox/setup.sh` read out of
an attached repository through the GitHub App and recorded on the environment so
the next build is the same build.

It is asynchronous on purpose. The verb returns once the builder exists and its
guest has taken the job; the run itself is minutes and survives your
disconnecting. A build that fails leaves its builder **paused**, holding the
half-built disk and the log, and `env show` prints the way out:

```
ssh web-build@<domain>                 # fix what was missing, by hand
ssh ctl@<domain> env capture web       # keep exactly that disk
```

**With no script anywhere, `env build` has an agent write one.** It runs
`claude -p` in the builder against `sparkbox docs dev-environment`, gets the
project running, and keeps the `.sparkbox/setup.sh` the agent leaves behind —
which is the deliverable, not the box: commit that file and every later build of
the environment runs it instead of running an agent. It needs a
`CLAUDE_CODE_OAUTH_TOKEN` the builder will carry, and says so up front rather
than after booting a VM to find out.

**And then it runs what the agent wrote**, in the same builder, before calling
the build done. An agent does the work interactively and writes the script at
the end from memory, so the mistakes it makes are the ones that come from
that — a directory it created by hand, a path that only ever existed in the
session — and none of them show up until somebody rebuilds months later. The
first run gets at most one fresh recovery agent: if it wrote no script, the
recovery pass inspects the running processes and logs it left behind and
finishes the deliverable; if it wrote a script that fails, the recovery pass
gets that failure to fix. If the second pass still leaves no working script,
the build fails, with any script recorded and the builder paused, so `env
capture` still adopts the box the agent did get working. Deferred monitors and
scheduled wakeups are disabled for these one-turn agents because there is no
live agent process to receive them after `claude -p` returns.

**A script build runs the script from `.sparkbox/setup.sh` in the checkout**,
not from a staging copy, because that is the path it was written to be run
from. Most setup scripts open by finding their project from their own location
— `cd "$(dirname "${BASH_SOURCE[0]}")/.."` — which is correct for a file in
`.sparkbox/` and resolves to `/run` for a copy staged there. An agent build
never saw this, because it verifies by running `.sparkbox/setup.sh` in the
checkout: the script passed the moment it was written and failed the first time
the same environment was rebuilt from it. When the environment's stored script
and the checkout's file are the same bytes — the ordinary case, since the
stored one was seeded out of that repository — nothing is written and the
checkout's own file is what runs.

**And a script that fails gets one repair agent.** A setup script that worked
on the machine it was written on can still fail on a fresh microVM — a package
the base image does not have, a tool that was only ever installed by hand — and
the box that discovers it is a box with an agent in it. So one agent gets one
pass: it is shown the failure, does the work by hand until the project is
genuinely up, rewrites the file, and the script is run again. If it exits 0 the
build is captured as usual and the repaired script is what the environment
keeps, so **commit it back to the project** or the row and the repository will
disagree. If it still fails, the build fails as before with the builder paused.
This is best-effort and never a refusal: a builder with no `claude` or no agent
credential reports the script's own error exactly as it used to, naming the
`secret set CLAUDE_CODE_OAUTH_TOKEN --tag <env>` that would turn repair on.

**A build's script comes from the environment, not from github** — that is what
makes a rebuild reproducible — and the cost of that used to be silent: an
environment seeded from a repository in March went on building March's script
for as long as it existed, however much was committed afterwards. So a stored
script that is **still a clean copy of what its repository gave it** is
refreshed on every build. Commit a change to `.sparkbox/setup.sh`, run `env
rebuild`, and that is the script that runs.

Once it has been changed here — by the repair pass above, or by `env script
--set` — nothing overwrites it, because the row is then sometimes the only copy
of a fix nobody committed back. `env show` and the console card say the two
disagree, and name both ways out: commit yours, or take the repository's with
`env script <name> --from-repo`. The environment records what the repository
last gave it, which is what tells those two cases apart; an environment older
than that record, or one whose script was typed in, is always the second case.

`env rebuild <name>` is a second name for `env build` — a build already boots
the stock image and runs the current script, never the environment's own last
snapshot, so an environment cannot accumulate. The old image stays bound until
the new one is captured, so a rebuild that fails costs only the time.

A new environment gets an **egress rule-set named after it**, so its sandboxes
reach the package registries, github and the model API and not the rest of the
internet. Widen it in the console's Network panel, or pass `--open-egress` on
create to have no rules at all. While a build runs, Sparkbox captures the exact
DNS names that rule-set blocks and keeps the bounded summary on the environment
row, so the console and API can show which dependency hosts need review.

All of it is on the other two doors too: an **Environments** tab on
`my.<domain>` beside Secrets, Network and Repos — the four panels are one object
viewed four ways, and this is the one that says so — and `/v1/environments` on
the REST API, including the build and the script.

`--env-build-timeout` (default 45m) is how long a build may sit in `building`
before a periodic sweep gives up on it. A *script* build's builder is left
paused, so the recovery path above is unchanged; an *agent* build's builder is
**destroyed**, because it holds an unattended agent with your credentials and,
by definition, has not written the script that was the point. Design:
[`docs/environments-design.md`](docs/environments-design.md).

## Architecture

```
cmd/sparkbox            single binary: `sparkbox serve`
internal/sshgw          smart SSH proxy (gliderlabs/ssh): pubkey auth,
                        username routing, resume-on-connect, session piping
internal/proxy          HTTP edge: <sub>.hivemind.tools -> guest IP:port,
                        resume-on-connect, reverse proxy (websockets ok)
internal/routes         sqlite route store (subdomain -> sandbox:port),
                        pure-Go modernc.org/sqlite, no cgo
internal/host           manager: sandbox records, JSON state, idle reaper
internal/ctlops         control-plane core: one method per ctl@ command,
                        shared by the SSH channel, the REST API and the terminal
internal/restapi        authenticated REST surface at api.<domain> + OpenAPI
internal/xterm          browser terminal at <name>-xterm.<domain> (WebSocket→PTY)
internal/api            legacy loopback-only CRUD API — never mount on the edge
internal/vmm            driver interface
internal/vmm/mock       fake VMs (in-process ssh servers) — runs anywhere
internal/vmm/firecracker real microVMs via firecracker-go-sdk — needs /dev/kvm
```

## Web routing

Every sandbox is reachable over HTTP at `<name>.hivemind.tools`, forwarded to
port `8000` inside the VM by default. Change the port or add extra subdomains
through the control API:

```sh
# forward myvm.hivemind.tools to :3000 instead of :8000
curl -XPOST localhost:8080/v1/sandboxes/myvm/routes -d '{"port":3000}'
# expose a second subdomain -> :9000
curl -XPOST localhost:8080/v1/sandboxes/myvm/routes -d '{"subdomain":"api-myvm","port":9000}'
curl       localhost:8080/v1/sandboxes/myvm/routes    # list
curl -XDELETE localhost:8080/v1/routes/api-myvm       # remove
```

Route state lives in `<state-dir>/sparkbox.db` (sqlite). The proxy listens on
`--proxy-addr` (default `:8081`) under `--proxy-domain` (default
`hivemind.tools`). Previews are **public** — anyone with the URL reaches the
app, the same model as exe.dev.

### Operator console

A password-gated web console lists every sandbox on the host and lets you
pause a running one (or resume a paused one). It rides the proxy edge at
`console.<domain>`, so it's covered by the same wildcard cert/DNS as sandbox
routes — no extra record needed.

```sh
# enable it; empty password (the default) disables the console entirely
sparkbox serve ... --console-password s3cret        # or SPARKBOX_CONSOLE_PASSWORD=…
# then browse to https://console.hivemind.tools
```

The password is a single shared secret; prefer the `SPARKBOX_CONSOLE_PASSWORD`
env var over the flag so it stays out of `ps`/`systemd status`. A correct login
sets an HMAC-derived cookie (no server-side session store), `Secure` whenever
the edge serves TLS. The console UI is a single `go:embed`-ed page — no frontend
build. **Run it behind `--proxy-tls`**: the password crosses the wire on login.

### TLS

For a real public edge, point a wildcard DNS record `*.hivemind.tools` at the
host and add `--proxy-tls`. Two providers:

- **`--tls-provider cloudflare`** (default) — obtains a single
  `*.hivemind.tools` wildcard certificate via the ACME **DNS-01** challenge and
  auto-renews it (via [CertMagic](https://github.com/caddyserver/certmagic) +
  the Cloudflare libdns provider). One cert covers every sandbox subdomain, so
  no per-name issuance and no brush with Let's Encrypt rate limits regardless of
  how many ephemeral sandboxes churn — but *rebuilding the host* does brush one:
  the wildcard is the same certificate every time, and duplicates are capped at
  five per week, so back up `<state-dir>/certmagic` before you wipe anything
  ([getting-started](docs/getting-started.md#rebuilding-a-host-keep-the-certificate-cache)).
  Needs a scoped `Zone.DNS:Edit` token in
  `CLOUDFLARE_API_TOKEN`; no inbound port 80/443 needed for issuance (DNS-01).
  That one pair is everything — browser terminals live at
  `<name>-xterm.hivemind.tools`, a single label, so they are covered by the same
  wildcard rather than needing one of their own. The startup log line
  `tls certificates managed` reports what was actually obtained.
- **`--tls-provider autocert`** — per-host certificates issued on demand via
  TLS-ALPN-01/HTTP-01 (no DNS API needed, but needs `:443` + port `80`
  reachable, and each new subdomain is a separate cert subject to rate limits).

Run TLS on `:443`: `--proxy-addr :443 --proxy-tls`. Full walkthrough (Cloudflare
setup, nameserver move, records, IPv6): [`docs/deploy-dns.md`](docs/deploy-dns.md).

### IPv6

If your host has a **routed IPv6 `/64`** (Scaleway Elastic Metal, Hetzner, etc.
delegate one), pass it with `--subnet6` and every sandbox becomes dual-stack:
it keeps its internal IPv4 (for the SSH gateway and proxy hops) and additionally
gets a **globally-routable `/128`** carved from the block — no NAT, real v6
egress and a real v6 identity (surfaced as `guest_v6` in the sandbox API).

```sh
sparkbox serve --driver firecracker --subnet6 2001:bc8:702:1c7::/64 ...
```

Per slot the driver assigns a point-to-point `/127` (host `::2`, guest `::3`,
next VM `::4`/`::5`, …), leaving `::1` free for the host's own edge address. The
host must have IPv6 forwarding on (`net.ipv6.conf.all.forwarding=1`); the guest
side is configured at boot by the `sparkbox-netcfg` hook baked into the rootfs
(the kernel `ip=` arg is IPv4-only). No NAT — the `/64` is routed to the host,
which forwards inbound to each guest via the per-VM `/127` route.

**DNS:** the edge is dual-stack, so point records at the *host*, not at VMs —
`AAAA *.hivemind.tools → <host's own v6, e.g. 2001:bc8:702:1c7::1>` alongside
`A *.hivemind.tools → <host v4>`. Keep both **grey-cloud (DNS-only)** in
Cloudflare so sparkbox terminates TLS directly.

## Quickstart (no KVM needed — mock driver)

```sh
go build ./cmd/sparkbox

ssh-keygen -t ed25519 -f mykey -N ''
echo "myuser $(cat mykey.pub)" > users.conf

./sparkbox serve --driver mock --state-dir ./state --users users.conf
# in another terminal:
ssh -i mykey -p 2222 new@localhost          # create a sandbox
ssh -i mykey -p 2222 <name>@localhost       # interactive shell / commands
```

The mock driver emulates each VM as an in-process SSH server executing in a
per-sandbox workdir — the full control plane, gateway, pause/resume, and
reaper paths are real; only the isolation is fake. `go test ./...` runs an
end-to-end suite over exactly this stack.

## Deploy on your own host

The `sparkbox` binary is its own installer. On any KVM-capable Linux host:

```sh
sparkbox doctor        # is this host ready? (PASS/WARN/FAIL per prerequisite)
sudo sparkbox setup --proxy-domain yourdomain.tools   # provision + start (idempotent; --dry-run to preview)
```

`setup` installs the binary you ran it from to `/usr/local/bin/sparkbox`
(`--bin-path`) so the unit runs exactly the build that provisioned the host,
fetches a prebuilt release (guest kernel, firecracker, rootfs — all
sha256-verified), lays down an XFS reflink volume, seeds your operator SSH key,
installs the systemd units, and starts the gateway. It then verifies the result
and **exits non-zero** if the gateway is not alive: a crash-looping service is a
FAIL carrying the tail of its journal, not a PASS. `doctor` runs the same
battery at any time.

It also **bakes the agent CLIs into the rootfs template** — `claude`, `codex`,
`pi` and `hivemind`, plus the guest workload-identity unit — and installs the
daily timer that keeps them current. That is what makes a sandbox worth creating,
and it is on by default (`--agent-tools=false` opts out). A released rootfs
carries a toolchain and no agent; the tools are patched in afterwards, which
takes about a minute instead of the ~65 of an image rebuild. The claim is checked
against the **template itself** — `/etc/sparkbox/tools-rev` inside the image —
rather than a stamp file beside it, so a template replaced by a release upgrade
is re-baked instead of being declared current, and `doctor`'s `agent tooling`
line reports a bare template rather than staying green over one. Full
walkthrough — prerequisites, TLS, port 22, day-2 ops — in
[`docs/getting-started.md`](docs/getting-started.md). For a Scaleway zero-touch
fleet instead, see [`docs/deploy-scaleway.md`](docs/deploy-scaleway.md). Where
the fleet's keys live — 1Password or Scaleway Secret Manager, staged at
provisioning time or fetched into tmpfs at boot — is
[`docs/secret-management.md`](docs/secret-management.md).

For the Firecracker proof of concept on CoreWeave Kubernetes Service — an
unprivileged public gateway and a capability-scoped VM node, split across two
Pods and pinned to one bare-metal CPU Node — see
[`docs/deploy-cks.md`](docs/deploy-cks.md).

### On a Mac (Apple Silicon)

Same binary-is-the-installer story, same two commands:

```sh
curl -fLO https://github.com/vanpelt/sparky/releases/latest/download/sparkbox-darwin-arm64
chmod +x sparkbox-darwin-arm64 && sudo mv sparkbox-darwin-arm64 /usr/local/bin/sparkbox

sparkbox doctor
sparkbox setup --proxy-domain yourdomain.tools
```

macOS has no KVM, so a Mac cannot run Firecracker directly. `setup` therefore
provisions a **nested Linux machine** with Apple's
[`container`](https://github.com/apple/container) CLI and runs the real Linux
`sparkbox setup` *inside* it — the same steps, the same release, the same
`sparkbox.env`. What you get is an ordinary sparkbox gateway that happens to
live one layer down.

Prerequisites. `sparkbox setup` checks every one of these *before* it touches
anything and refuses with the reason. `sparkbox setup --dry-run` runs the same
preflight and changes nothing, so it is the safe way to ask whether this Mac
qualifies — a host that passes prints no preflight section at all, and anything
that does not is listed with its fix:

- **Apple Silicon M3 or newer** — nested virtualization is an M3+ feature, and
  it is what lets Firecracker run inside the machine. An M1/M2 fails here.
- **macOS 15 or newer**, and Apple's `container` CLI **1.1.0 or newer**, with
  its service running (`container system start`).
- Disk for the machine's data volume (`--data-volume-gb`).

`sparkbox doctor` answers the *other* question — how are both hosts doing now.
It reports the Mac (`mac:` lines — the same prerequisites, plus the outer kernel
verified against the release's checksum) and the nested machine (`machine:`
lines — state, nested virtualization, `/dev/kvm`, the gateway service), ending
with the gateway's **own `doctor` relayed out of the machine**. It exits
non-zero if either layer is wrong, and when the machine is missing or stopped it
says so in one line rather than printing an empty section. Use `setup --dry-run`
before provisioning, `doctor` after.

Everything you would pass on Linux still means the same thing and is forwarded
to the gateway inside the machine — `--proxy-domain`, `--operator-key`,
`--sluice`, `--gateway`/`--node-name`, the listen addresses, TLS flags. Only
these describe the Mac itself:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--machine-name` | `sparkbox` | the nested VM to create/adopt |
| `--machine-cpus` / `--machine-memory-gb` | 8 / 24 | the machine's whole budget |
| `--machine-image` | content-addressed | override the gateway image reference |
| `--outer-kernel` | `~/Library/Application Support/sparkbox/vmlinux-macos-arm64` | where the outer kernel is stored |
| `--container-bin` | `container` | path to Apple's CLI |

Two flags are **refused** on darwin rather than ignored: `--move-admin-ssh`
(the machine runs one process and nothing competes for `:22` inside it) and
`--bin-path` (the gateway's binary is installed by the inner `setup`, not by
the Mac). `--dry-run` prints the whole plan — including the exact inner `setup`
command line — and touches nothing.

> **Two kernels, do not confuse them.** `vmlinux-macos-arm64` is the *outer*
> KVM-capable kernel Apple's `container machine` boots; `vmlinux-<arch>` is the
> *guest* kernel each Firecracker sandbox boots, one layer further in. `setup`
> fetches and sha256-verifies both.

An existing machine is adopted only if its name, image repository and
`home-mount=none` all match, so `setup` never takes over a machine you created
for something else.

> **What is proven, and what is not.** CI builds and unit-tests the darwin paths
> on a real Apple Silicon runner, but GitHub's hosted Macs have no `container`
> CLI and cannot nest VMs, so **no CI anywhere creates a real machine** — the
> lifecycle tests drive a fake. `macos/poc.sh` remains in the tree as the
> fallback and as the reference the Go port was written from; it uses a separate
> machine (`sparkbox-poc`), so the two coexist on one Mac.

### Adding a second machine

A second host joins an existing gateway as a **node**. Provision the gateway
first, exactly as above, then run `setup` on the new machine with `--gateway`:

```sh
sudo sparkbox setup --gateway <gateway-host>:2222 --node-name laptop \
  --guest-subnet 10.201.0.0/20
```

`--gateway` is the whole difference: instead of standing up a gateway of its
own, the machine mints a node key, dials *out* to that address (so a node behind
NAT needs no inbound anything), enrolls, and parks as **pending**. Nothing is
trusted yet. It logs its own identity at startup and then the exact command that
unblocks it:

```
node identity  node=laptop fingerprint=SHA256:IZWmZrHR+PrPOFr5DI5b93scC2XC+0uEZ2pD76MJpnM
this node is enrolled and waiting for approval  node=laptop gateway=10.66.0.1:2222
  ssh ctl@catnip.sh node approve SHA256:IZWmZrHR+PrPOFr5DI5b93scC2XC+0uEZ2pD76MJpnM --guest-subnet 10.201.0.0/20
  — after checking that fingerprint against the one this machine printed at startup.
```

On the gateway, as an operator, read the roster and approve — by **fingerprint**,
never by name, because a node chooses its own name and only the key is evidence:

```sh
ssh ctl@<gateway> node ls        # name, status, presence, arch, sandbox count, fingerprint
ssh ctl@<gateway> node approve SHA256:IZWmZrHR+PrPOFr5DI5b93scC2XC+0uEZ2pD76MJpnM --guest-subnet 10.201.0.0/20
ssh ctl@<gateway> node rm laptop # drop one; refused while it still holds sandboxes
```

Compare the fingerprint against the one the node printed before you say yes —
that out-of-band comparison is the identity trust decision. The approved subnet
must exactly match the node's `--guest-subnet`; with gRPC enabled, also approve
the exact reported tailnet listener using `--grpc-addr <host:port>`. The node retries on
its own backoff (~30s), so approval needs no restart: it reconnects, logs
`linked to the gateway`, and starts heartbeating. The same roster is
`GET /v1/nodes` over the REST API.

### Placing work on a node

Once a node is approved, name it and the sandbox is built there:

```sh
ssh new+webapp@<gateway> -- --node laptop     # build on the node called laptop
ssh webapp@<gateway>                          # …and reach it exactly as if it were local
```

Everything after that is meant to be indistinguishable from a local sandbox: the
shell, the web routes at `webapp.<domain>`, the browser terminal, pause/resume,
snapshots and `ctl` all work through the gateway, which holds the session and
relays it over the node link. A node behind NAT needs no inbound anything — it
dialled out to enroll and the same connection carries the work.

> **The default is still "build here", on purpose.** With no `--node`, `ssh
> new@<gateway>` creates on the gateway itself, exactly as a single-box
> deployment always did. There is no scheduler and no best-fit spreading: a
> gateway that started moving people's work across machines the day a second one
> joined would be surprising in the one direction that costs someone their
> afternoon. Placement is a thing you ask for.

> **Egress rules and guest identity both reach nodes**, but a node has to be
> *equipped* for the first one. Tag-to-rule bindings live in the gateway's
> store; the gateway resolves each machine's share against the placement
> ledger's owner column and pushes it down, and reads that machine's meter back
> for the bandwidth panel. Both need a sluice on the node —
> `sparkbox setup --sluice` on that machine — and a node that has none **refuses**
> the push rather than accepting it silently, so `ctl node ls` marks it
> `no-egress-control` whenever the fleet is metered unevenly. A tagged sandbox
> on an unequipped node is unfiltered, and now something says so.
>
> Guest identity needs no equipment. A node runs the metadata service exactly as
> a gateway does — deciding *which* sandbox is asking is a property of the tap
> the request arrived on — and relays only the signing step, since the fleet's
> OIDC key never leaves the gateway. The gateway resolves the owner, the image
> and the `box` claim from its own ledger, and refuses outright if the ledger
> does not place that sandbox on the machine that asked. So `hivemind start`
> federates on a node with nothing pasted, exactly as it does locally.
>
> Two things worth knowing when you turn sluice on for a node:
> `--guest-dns` rides in as a kernel boot arg, so an already-running VM needs a
> pause/resume before its lookups are attributed to domains; and while the
> gateway is unreachable a guest's token refresh is answered `503`, which its
> own timer retries out of — the token lives an hour and the timer fires every
> 45 minutes, so a gateway restart costs nothing.

## Real microVMs

`--driver firecracker` boots actual Firecracker microVMs (rootfs templates
built from OCI images by `hack/build-rootfs.sh`, CoW reflink per-VM copies,
static tap networking, snapshot-to-disk on pause). Requires `/dev/kvm`, root,
a vmlinux, and has **not yet been exercised on real hardware** — see
[`docs/deploy-hetzner.md`](docs/deploy-hetzner.md) for bring-up and the
production gap list (warm snapshots, disk-parser isolation, rate limits).

`--state-dir` holds control state. Set `--vm-state-dir` when guest disks should
live on a separate hot volume; it defaults to `--state-dir` for existing
installations. Firecracker requires reflinks for both template clones and
snapshot staging, so the image and VM-state directories must be on the same
reflink-capable filesystem.

Firecracker cannot give a sandbox nested virtualization (no `/dev/kvm` inside
the guest). [`docs/cloud-hypervisor-feasibility.md`](docs/cloud-hypervisor-feasibility.md)
is the spike on swapping the VMM for Cloud Hypervisor to get it, what that
touches, and why nested has to be per-sandbox opt-in on CKS;
`hack/probe-nested-virt.sh` is its host preflight.

The default base image is **self-built** from [`images/Dockerfile`](images/Dockerfile)
(a lean Ubuntu 24.04 + Go/Python·uv/Node, Kind/kubectl, direnv, headless Chrome
and the agent-browser CLI, ~4GB — replacing the
~30GB `codex-universal`). It logs in as a non-root **`sparky`** user, declared by
the image's `sparkbox.login-user` label and honored end-to-end (build-rootfs bakes
the gateway key into `/home/sparky`, the release manifest carries
`ROOTFS_LOGIN_USER`, and `sparkbox serve --default-login-user` follows it).

## Releases

Artifacts ship as **GitHub Releases, built for linux/amd64, linux/arm64 and
darwin/arm64** — the same tag provisions an x86 cloud VM, an aarch64 DGX Spark
or an Apple Silicon Mac. Push a `v*` tag and
[`.github/workflows/build-artifacts.yml`](../../.github/workflows/build-artifacts.yml)
does the rest: Depot builds the `images/Dockerfile` base for both linux platforms
and pushes one multi-arch tag to GHCR, then a matrix of native runners
(`ubuntu-24.04` / `ubuntu-24.04-arm`) each compile the guest kernel, build the
`sparkbox` and `sluice` binaries, and flatten their arch's image into an ext4
template; alongside them a native arm64 runner compiles the macOS *outer* kernel
once, and a cheap cross-compile leg emits the Mac's own binary and manifest. The
release only goes public once **every** platform lands, so `setup` can never
resolve a half-populated `latest`.

```sh
git tag v0.4.1 && git push origin v0.4.1    # cut a release
gh workflow run "sparkbox release"          # ad-hoc dev-<ts> prerelease (doesn't move `latest`)
```

A release's asset namespace is flat, so every name carries the platform it is
for. `sparkbox setup` picks its own set, pinned to the tag the manifest names:

| Asset | What | Who fetches it |
| --- | --- | --- |
| `sparkbox-linux-<arch>` | control-plane binary | curl'd by the operator; `setup` then installs the binary it is *running* |
| `sluice-linux-<arch>` | egress gateway (DNS allowlist + eBPF meter) | `setup --sluice`, on either platform — it is a linux daemon even when a Mac puts it inside the nested machine |
| `vmlinux-<arch>` | **guest** kernel a microVM boots | `setup` |
| `firecracker-<arch>` | the VMM | `setup` |
| `jailer-<arch>` | matching Firecracker chroot / privilege-drop launcher | CKS and jailer-enabled hosts |
| `universal-<arch>.ext4.zst` | guest rootfs template | `setup` |
| `manifest-<arch>.env` | sha256s + metadata; unqualified name means **linux** | `setup`, `macos/sparkbox-bootstrap.sh` |
| `sparkbox-darwin-arm64` | the binary a Mac runs | the operator on macOS |
| `manifest-darwin-arm64.env` | the Mac's manifest — repeats the linux arm64 checksums it provisions, plus `MACHINE_SPARKBOX_ASSET` and `OUTER_KERNEL_ASSET` | `setup` on darwin, `macos/kernel/fetch.sh` |
| `vmlinux-macos-arm64` (+ `.config`) | the **outer** KVM kernel Apple's `container machine` boots — a different kernel from `vmlinux-<arch>` | `macos/kernel/fetch.sh` |

To build a release by hand on a build host, `hack/stage-artifacts.sh` stages one
linux arch into `OUT_DIR` (blank `IMAGE` = build the base image locally; set
`IMAGE=` to flatten a prebuilt one instead). `FIRECRACKER_BIN` and `JAILER_BIN`
must name the matching release pair. `hack/stage-darwin-artifacts.sh`
derives the darwin pair from the arm64 manifest that produced.

## Status / roadmap

- [x] Driver interface + mock driver, manager, JSON persistence, idle reaper
- [x] SSH gateway: pubkey identity, username routing, resume-on-connect,
      PTY + winch + env + exit-code piping, `new@` provisioning
- [x] Control API (sandbox + route CRUD)
- [x] Firecracker driver (compiles; untested pending a KVM host)
- [x] HTTPS edge proxy for in-sandbox web servers (`<sub>.hivemind.tools`,
      sqlite route store, resume-on-connect; TLS via Cloudflare DNS-01 wildcard
      cert or autocert on-demand)
- [ ] Validate on a Hetzner auction box; measure cold-create and resume times
      (incl. the proxy path — the host reaches the guest directly over the
      tap's /30, so no extra NAT is needed, but it's untested on hardware)
- [x] IPv6-native: dual-stack guests with a routable /128 per sandbox from a
      delegated /64 (`--subnet6`), no NAT
- [x] Password-gated operator console at `console.<domain>` (list + pause/resume + pin)
- [x] Pinned "always-on" tier: reaper-exempt sandboxes so in-VM cron/daemons keep
      running (`ctl pin <name>`), resumed automatically on host boot
- [x] Platform scheduler: cron jobs the host fires by waking a scale-to-zero
      sandbox, so periodic work is reliable without keeping it warm
      (`ctl schedule add <box> "*/30 * * * *" <cmd>`); next wake shown in console
- [x] User accounts in sqlite: `signup@` over SSH, invite codes, multi-key
      keyrings (`ctl keys`), GitHub linking verified against `<login>.keys`
- [x] OIDC workload identity: issuer at `oidc.<domain>`, per-sandbox id tokens
      over an IMDS-style metadata endpoint, so `hivemind start` federates with
      no secret in the guest
- [x] Live memory overcommit: `deflate_on_oom` balloon per VM + two-stage idle
      reaper (balloon-down → pause) + working-set admission (`--mem-reserve-mb`),
      so idle VMs return RAM to the host while staying live. Measure the real
      per-VM cost + KSM savings with `hack/measure-density.py`
- [x] Vitals-based idleness: the reaper samples each VM's tap byte counters, so
      an unattended networked agent counts as active with no inbound traffic
      (`--activity-net-kb`). Host CPU activity is opt-in
      (`--activity-cpu-pct`) because background VM and container overhead is
      not reliable evidence of user work. Lifetime bytes in/out are metered per
      sandbox and shown in both consoles — the basis for future egress limits
- [x] Clean hang-up on pause: attached terminals get their modes restored
      (mouse reporting, alternate screen, bracketed paste) and a reason, instead
      of being left wedged against a VM that stopped answering
- [ ] Warm-snapshot pool (restore instead of cold boot on create)
- [ ] KSM host tuning + per-VM cgroup cpu.max; I/O limits; net isolation
- [x] Fleet link layer: a second machine joins with `setup --gateway host:port`,
      enrolls by its own key, is approved by fingerprint (`ctl node approve`),
      and reports arch + capacity over a heartbeat — see *Adding a second
      machine*
- [x] Multi-host placement + data plane: `ssh new@<gw> -- --node <name>` builds
      on a node, and the shell, web routes, terminal, pause/resume, snapshots
      and `ctl` all reach it through the gateway. Placement is explicit by
      design — with no `--node` the gateway still builds locally
- [ ] Best-fit scheduling (flyd pattern): a placer that picks the node itself
      from the roster's capacity, instead of the operator naming one
- [x] Egress policy and per-domain bandwidth on nodes: the gateway resolves each
      machine's share from the ledger and pushes it down, and reads that
      machine's own meter back. Needs a sluice on the node; one without it
      refuses rather than silently accepting, and `ctl node ls` says so
- [x] Guest identity on nodes: the node runs the metadata service and relays
      only the signing step, which the gateway answers after checking its ledger
      places that sandbox on the machine that asked
- [x] Tag templates: a tag names the rootfs its sandboxes boot from
      (`snapshot bind <name> --tag <t>`), placement follows the machine holding
      the template, ambiguous and `default` bindings are refused, and pooled disk
      no longer charges every fork for the template's shared blocks. Captures
      refresh the agent CLIs first, a VM updates its own with
      `sparkbox update-tools`, and a VM can capture itself into a tag it carries
      — all of it inert wherever host rootfs mounts are disabled
- [ ] Strip the managed secret block on a capture taken on a *node*: the strip
      is installed on the gateway only, so a template captured on a node carries
      plaintext secrets into every fork (`docs/security-hardening.md`)
- [ ] Template replication across machines, and delta archives for forks —
      both written down and deliberately unbuilt
      ([`docs/tag-templates-design.md`](docs/tag-templates-design.md))
