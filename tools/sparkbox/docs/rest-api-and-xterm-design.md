# REST API, shared control core, and the browser terminal — design

Status: **shipped** (2026-07-21) — all four milestones landed and are wired into
`cmd/sparkbox serve`. This document is now the design record; where the code and
this text disagree, the code wins. Deviations worth knowing about:

- `internal/sshgw/attach.go` was **not** written. `internal/xterm` grew its own
  `dialPTY`, so an `AttachTerminal` in sshgw would have been dead code. The seam
  the two packages actually share is `sshgw.SessionConn` +
  `(*sshgw.Gateway).TrackTerminal`, so there is still exactly one live-session
  registry for `host.Manager` to close on pause.
- The `Terminal` seam between `internal/restapi` and `internal/xterm` is a
  structural interface (`ServeTerminal`) satisfied by a three-line adapter in
  `main.go`, rather than `restapi` importing `xterm`. The owner gate for
  `GET /v1/sandboxes/{name}/terminal` lives in `restapi` — `ctlops.Get`, before
  the upgrade — because after the handshake a refusal can only be a close code.
- Two flags, not one: `--api-subdomain` (default `api`) and `--xterm-subdomain`
  (default `xterm`). Empty disables either surface, and `--proxy-addr ""`
  disables the terminal implicitly, which is threaded into
  `ctlops.Config.XtermSubdomain` so `terminal_url` and `capabilities.terminal`
  stay honest.
- **Three `ctl` sentences changed wording**, which the "byte-identical" row in
  *Decisions taken* did not anticipate. `ctlops.ownedSnapshot` pre-resolves the
  snapshot through the caller's own list, so `fork` and `snapshot rm` now print
  the masked `sparkbox: no snapshot named "ghost"` where they used to print
  `sparkbox: <what> failed: snapshot "ghost" not found`; and a whitespace-only
  command to `schedule add` is refused with `a schedule needs a command to run`
  rather than the store's `schedule needs a command to run`. Exit codes are
  unchanged (1), and the new wording is the masking improvement — a wrapper
  script matching on `failed:` is the only thing that notices.
- **`internal/xterm` is strict-owner, with no operator bypass.** The first cut
  admitted operators "as everywhere else on the edge", but the other two doors
  onto the same bridge do not (`ctlops.owned` has no operator concept, and
  `sshgw` compares owners with no operator branch), and `openapi.json` describes
  them as the same session. A PTY is not the metadata authority the userconsole
  bypass grants: it hands over the owner's credentials, agent tokens and repos
  on a cookie alone.
- **The terminal host is `<name>-xterm.<domain>`, not `<name>.xterm.<domain>`**
  (changed 2026-07-22; everything below describing the dotted form is historical).
  The dotted form is two labels, and a wildcard matches exactly one — RFC 4592 in
  DNS, RFC 6125 in certificates — which this design accounted for with a second
  wildcard record and a second ACME order. Both worked. What they could not fix
  is a hosted TLS front end that terminates in front of sparkbox with a
  certificate it issued for the zone: Cloudflare's universal certificate is
  `<domain>, *.<domain>`, the deeper name is not in it, and multi-label coverage
  needs the paid Advanced Certificate Manager. So the public path died inside the
  TLS handshake with `ERR_SSL_VERSION_OR_CIPHER_MISMATCH` — before the tunnel,
  before the origin, with no sparkbox log line — and terminals worked only over
  the tailnet, where split-DNS and the built-in `dnsedge` responder answer the
  whole subtree. One hyphen puts the terminal back under the wildcard that
  already exists for every sandbox front door, and both the second wildcard order
  (`wildcardTLSConfig`) and the wildcard publisher (`publishXtermWildcard`,
  `frontdoor.PublishWildcard`) were deleted rather than kept.

  The cost is a reserved suffix. `proxy.SetReservedSuffix` now claims every
  subdomain ending in `-xterm`, before the route lookup — same ordering, same
  security property, since a route row named `victim-xterm` is as creatable as
  `victim.xterm` was. So `host.Manager` and `routes.ValidSubdomain` refuse names
  ending that way, and `cmd/sparkbox` warns at startup about any that predate the
  rule. If a future surface wants a subtree of its own, it should take a hyphen
  too.

Verified end to end against the mock driver: `POST /v1/sandboxes` →
`terminal_url` → page → `101` upgrade → a live shell prompt; a cross-owner
terminal request answers the same masked 404 an absent sandbox does, and a
cross-origin WebSocket handshake is refused with 403.

## What shipped

The surface an operator or a client actually sees, all of it default-on behind
`--proxy-addr`:

| Where | What | Off switch |
|---|---|---|
| `https://api.<domain>/v1/…` | 40 authenticated endpoints mirroring `ctl@` | `--api-subdomain ""` |
| `https://api.<domain>/docs` | first-party docs page, no CDN, renders the embedded spec | (same) |
| `https://api.<domain>/openapi.{json,yaml}` | the canonical 3.1 document; YAML derived deterministically from the JSON | (same) |
| `https://<name>.xterm.<domain>/` | the xterm.js page, behind the ordinary edge session | `--xterm-subdomain ""` |
| `https://<name>.xterm.<domain>/ws` | the PTY bridge, subprotocol `sparkbox.terminal.v1` | (same) |
| `https://api.<domain>/v1/sandboxes/{name}/terminal` | the same bridge for non-browser clients | (same) |

And the code behind it:

| Package | Role |
|---|---|
| `internal/ctlops` | the transport-agnostic core: one method per `ctl@` command, the ownership gate, the timeout budgets, the error taxonomy, the job registry |
| `internal/restapi` | JSON, status codes, `Prefer`/`Idempotency-Key`, the embedded `openapi.json` and `/docs` |
| `internal/xterm` | the page, the vendored assets, the origin gate and the WebSocket↔PTY bridge |
| `internal/sshgw` | unchanged in behaviour: it now parses arguments and formats text, and calls `ctlops` for everything else |
| `cmd/sparkbox/{tls,wildcarddns}.go` | the second wildcard certificate and the second wildcard DNS record |

`internal/proxy` grew `SetReservedSuffix`, which is what makes a **two-label**
host reach a handler at all; `xterm` and `api` became reserved names in both
`internal/host` and `internal/users`, so no sandbox or handle can shadow either.

## Not built yet

Things the rest of this document describes, or that a reader would reasonably
assume from it, which do not exist. None of them block anything; each entry says
what you get instead.

- **`ctlops.Attach` and `ctlops.Touch` have no callers.** `internal/xterm` grew
  its own `Attacher` and dials `host.Manager` directly, and the REST gate uses
  `ctlops.Get` (an owner check that must *not* resume, so a stranger's probe
  cannot wake a VM). The owner gate is enforced on both doors — just not by the
  method written to hold it. The methods stay because the design names them and
  the next transport should use them rather than write a fourth check.
- **`internal/sshgw/attach.go`** — see the deviation note above; there is no
  `AttachTerminal` and no `sessionConn` adapter. The shared seam is
  `sshgw.SessionConn` + `(*sshgw.Gateway).TrackTerminal`.
- **Secrets over REST.** No endpoint reads or writes secret values; only tags,
  which is what selects them. Secret writes need the user console's
  KEK-from-OIDC-key plumbing and belong in their own review.
- **No `signup` endpoint.** `signup@` stays an interactive SSH dialog; browsers
  use the passkey flow on `login.<domain>`.
- **No operator bypass anywhere in `ctlops`, `restapi` or `xterm`** (except
  `Invite`, which is operator-only). `internal/userconsole` keeps its own and
  was not migrated.
- **No pagination and no cursors**, including on `GET /v1/jobs`, which returns
  the whole retained ledger (one hour, 200 jobs per owner, in memory).
- **Wildcard DNS is automated only for the direct-IP edge shape** — a
  `CLOUDFLARE_API_TOKEN` **and** `--edge-v4`. A Cloudflare Tunnel deployment
  needs a proxied CNAME sparkbox cannot compute; it logs the exact
  `cloudflared tunnel route dns` command and stands down. See `deploy-dgx.md`.
- **The `exe-ssh` CLI wire protocol** (`exe0.` tokens, `/terminal/…` paths,
  tmux-backed named sessions) is still unbuilt and still reserved. A CLI that
  wants a shell today can use `GET /v1/sandboxes/{name}/terminal` with a session
  token instead.
- **`hack/preview-console.py terminal` cannot open a WebSocket**, so the page
  renders in its reconnect state. It is a CSS loop, not a functional preview.
- **`session-token` still emits CRLF**, so `TOKEN=$(ssh ctl@… session-token)`
  leaves a `\r` that makes the `Authorization` header malformed and curl answers
  `400` with nothing that hints at why. It is pre-existing — it shipped with
  bearer-token support — and it is a line ending on an SSH channel, which is
  what an SSH channel is supposed to send. Rather than change it under callers
  who may already strip it, every doc pipes through `tr -d '\r\n'` and the
  command's own stderr notes now show that form. The real fix is to emit `\n` on
  a non-PTY exec session and re-baseline `control_golden_test.go`.

## What this is

Three things ship together because they are one product: a **REST API** at
`api.<domain>` that mirrors the `ssh ctl@<domain>` surface, a **browser
terminal** at `https://<name>.xterm.<domain>`, and the **edge plumbing** that
makes a two-label host resolve, present a valid certificate, and reach a
handler. Underneath all three sits one new package, `internal/ctlops`, which
holds the control-plane logic that today lives in `internal/sshgw`'s command
handlers.

This closes M3 of `terminal-over-https-design.md`, which named the
`<name>.xterm.<domain>` routing gap ("`*.<domain>` is single-label — it does not
cover `<name>.xterm.<domain>`") as future work. It does it for the browser
rather than for the `exe-ssh` CLI; the `exe0.`/`/terminal/…` wire protocol that
document specifies stays unclaimed and unbroken. Its close code `4001` **is**
adopted, with exactly the meaning it assigns — "the shell exited normally" —
because one code with two meanings on one server is the outcome worth avoiding,
not one code shared by two handlers that agree. (The design originally said the
opposite; see *Close codes* in Part 4.)

## Why a shared core, and why now

`ssh ctl@<domain> pause foo` is thirteen lines of `internal/sshgw/control.go`
that do four things: check the arity, resolve the sandbox, **check that the
caller owns it**, and format the result. Only the third is policy. Today that
third step is hand-rolled in six places — `control.go:275`, `:531`, `:557`,
`:604`, `control_auth.go:82`, `gateway.go:272` — each emitting the same
carefully identical `no sandbox named %q` for both "does not exist" and "exists
but is someone else's", because distinguishing them leaks the namespace. A
seventh transport is exactly the moment somebody writes a seventh copy and gets
it wrong.

There are three further invariants that are currently comments rather than
code:

- **Tags are stamped before `Create`/`Fork`**, because `host.Manager.Create`
  fires the secret-env push on a goroutine and the sandbox's tags decide what
  that push contains. `internal/userconsole`'s fork already violates this.
- **`secrets.Store.SetTags`/`TagsFor` perform no ownership check at all.** A
  caller that forwards a sandbox name to them without gating first is a silent
  cross-tenant read/write.
- **The owner check strictly precedes `EnsureRunning`**, so a cross-owner probe
  is never a free way to wake a stranger's VM.

`internal/ctlops` makes all four mechanical. After it lands, `internal/sshgw`
parses arguments and formats text; `internal/restapi` decodes JSON and picks
status codes; neither holds policy.

## Decisions taken

Where the design candidates disagreed, this is the call and the reason.

| Question | Decision | Why |
|---|---|---|
| ctlops dependencies | Narrow interfaces ctlops declares itself, satisfied structurally by the real stores | It is the only way CORE's tests run in milliseconds against fakes with no sqlite, no temp dirs, and no VM driver, which is what lets four agents work in parallel. |
| Result types | Purpose-built `SandboxInfo`/`SnapshotInfo`/… , never `*host.Sandbox` | `api.<domain>` is a public documented contract and must not move when an internal struct grows a field, and `host.Sandbox` carries `SSHAddr`/`HostIP`/`GuestV6` topology that has no business on the edge. |
| Long operations (archive/resize/resume) | Sync-first with automatic escalation to a job resource, controlled by `Prefer: wait=N` | A 15-minute synchronous HTTP response 524s behind cloudflared every single time, and forcing 202 on a two-second create would make the common path miserable. |
| Does the SSH path use jobs? | No — `ctl` stays fully synchronous and byte-identical | The `ctl` output text and exit codes are a shipped contract; the job model is an HTTP concern only. |
| `pin` partial success | ctlops `SetPinned` sets the flag and nothing else; `ctl pin` composes flag+resume in the renderer and keeps its exit-1-on-failed-resume | This removes the need to invent an HTTP status for "the write landed but the follow-up did not" while leaving `ctl` unchanged. |
| Pagination | None — plain slices | Nobody has 100 sandboxes; a signed cursor is generality that costs a mechanism and buys nothing today. |
| Operator bypass | ctlops has none, except `Invite`; `internal/userconsole` keeps its own bypass and is **not** migrated | Folding userconsole in would silently change the security model of a shipped, tested surface; that belongs in its own commit. |
| Secrets over REST | Out of scope for v1 | Secret writes need the console's KEK-from-OIDC-key plumbing, and dragging that into a new public surface is a separate review. |
| Terminal seam | `ctlops.Attach` gates and resumes → `sshgw.AttachTerminal` dials, PTYs and registers → `internal/xterm` frames | A second live-session registry means `mgr.CloseSandboxSessions` never hangs up browser terminals on pause, and a blocking `Close` deadlocks the daemon under the manager's mutex. |
| WebSocket CSRF | `internal/xterm` implements its own credential-keyed origin gate; `internal/edgeauth` is **not** modified | `RequireMutation`'s origin is a fixed string and its `X-Sparkbox-Console` escape hatch is unsettable on a handshake, so reuse would be a lie; edgeauth is shared with a shipped console and stays untouched. |
| OpenAPI source of truth | Hand-authored `openapi.json`, with a ~60-line deterministic JSON→YAML emitter for `/openapi.yaml` | The integrator owns `go.mod` and no YAML encoder is a direct dependency; the emitter deletes cleanly if one is ever promoted. |
| Error body | `{"error":{"code","message","op","details"}}` | A new documented surface should give clients a stable machine token instead of the substring-matching the consoles are stuck with. |

---

# Part 1 — package `internal/ctlops`

This is a contract. Implementers type it verbatim; the bodies are theirs.

```go
// Package ctlops is the transport-agnostic core of the sparkbox control plane:
// one method per `ssh ctl@<gateway>` command, taking the caller's handle and
// typed arguments and returning typed results and one typed error.
//
// It exists because the same operation is now reachable three ways — the SSH
// ctl channel, the REST API at api.<domain>, and the browser terminal's owner
// gate — and the ownership check, the timeout budget, and the
// tags-before-create ordering are each things a caller can silently forget.
// They live here so that no caller can. internal/sshgw keeps argument parsing
// and text formatting; internal/restapi keeps JSON and status codes; neither
// keeps policy.
//
// ctlops authenticates nothing. The transport has already proved who is asking
// (an SSH public key, or a verified edge session); ctlops only authorizes.
package ctlops

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// ---------------------------------------------------------------------------
// Narrow dependency interfaces
//
// Each is the slice of a real store that ctlops actually drives, stated as an
// interface so the package's own tests run against in-memory fakes with no
// sqlite, no temp dir, and no VM driver. *host.Manager, *users.Store,
// *secrets.Store, *schedule.Store, *routes.Store and *edgeauth.Signer satisfy
// them structurally — there are no adapters to write and nothing to keep in
// sync. Note that every method here is owner-agnostic: ctlops does the
// ownership check before it calls any of them, which is the whole point.
// ---------------------------------------------------------------------------

// Sandboxes is the VM-lifecycle slice of host.Manager.
type Sandboxes interface {
	Get(name string) (*host.Sandbox, bool)
	ListByOwner(owner string) []*host.Sandbox
	Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error)
	EnsureRunning(ctx context.Context, name string) (*host.Sandbox, error)
	Pause(ctx context.Context, name string) error
	Archive(ctx context.Context, name string) error
	Resize(ctx context.Context, name string, sizeMB int64) error
	Reboot(ctx context.Context, name string) error
	Rename(ctx context.Context, oldName, newName, owner string) error
	Destroy(ctx context.Context, name string) error
	SetPinned(name string, pinned bool) error
	ResyncEnv(ctx context.Context, name string)
	Touch(name string)
	ArchivingEnabled() bool
}

// Templates is the snapshot/fork slice, separate because a host whose driver
// cannot archive has snapshots disabled while ordinary sandboxes still work.
type Templates interface {
	Snapshots(owner string) []*host.Snapshot
	Snapshot(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error)
	DeleteSnapshot(ctx context.Context, snapName, owner string) error
	Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error)
	Snapshotter() bool
}

// Accounts is the identity slice of users.Store.
type Accounts interface {
	Get(handle string) (users.User, error)
	Keys(handle string) ([]users.Key, error)
	AddKey(handle string, key xssh.PublicKey, label, via string) error
	RemoveKey(handle, fp string) error
	LinkGitHub(handle, login string) error
	SetEmail(handle, email string) error
	Passkeys(handle string) ([]users.Passkey, error)
	RemovePasskey(handle, idPrefix string) error
	NewInvite(createdBy string) (string, error)
	InviteCount(handle string) (int, error)
}

// Tagger is the tag half of secrets.Store. Deliberately identical to the
// existing sshgw.SandboxTagger so *secrets.Store keeps satisfying both. Neither
// method checks ownership — that is exactly why nothing outside ctlops may hold
// a reference to one.
type Tagger interface {
	TagsFor(sandbox string) ([]string, error)
	SetTags(sandbox, owner string, tags []string) error
}

// Schedules is the platform-cron store. A nil one makes every schedule
// operation answer KindDisabled.
type Schedules interface {
	Add(e schedule.Entry) (schedule.Entry, error)
	Get(id string) (schedule.Entry, error)
	ListByOwner(owner string) ([]schedule.Entry, error)
	Delete(id string) error
}

// Routes is the web-route store, driven only by the `share` commands.
type Routes interface {
	ListBySandbox(sandbox string) ([]routes.Route, error)
	SetVisibility(subdomain, visibility string) error
}

// Minter mints edge session tokens; *edgeauth.Signer satisfies it.
type Minter interface {
	Mint(id edgeauth.Identity, ttl time.Duration) (string, time.Time, error)
}

// GitHubKeys is the github.com dependency, behind an interface so no test in
// this package ever makes a network call. A nil one defaults to
// users.FetchGitHubKeys / users.VerifyGitHubKey.
type GitHubKeys interface {
	Fetch(ctx context.Context, login string) ([]xssh.PublicKey, error)
	Verify(ctx context.Context, login string, key xssh.PublicKey) (bool, error)
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

// Ops is the control-plane core. One per process; safe for concurrent use
// because every store it holds already is.
type Ops struct{ /* unexported */ }

// Config wires the stores. The optional ones are optional in the same way and
// for the same reasons they are on the Gateway today: a nil store makes its
// commands answer KindDisabled rather than panic, which is what a unit test and
// a minimally-configured host both want.
type Config struct {
	Sandboxes Sandboxes // required
	Templates Templates // required
	Accounts  Accounts  // required

	Tags      Tagger     // nil: tag operations are KindDisabled
	Schedules Schedules  // nil: schedule operations are KindDisabled
	Routes    Routes     // nil: share operations are KindDisabled
	Sessions  Minter     // nil: MintSessionToken is KindDisabled
	GitHub    GitHubKeys // nil: the real github.com client

	DefaultImage   string // rootfs template new sandboxes get
	Domain         string // base zone, for the URL fields on results; "" omits them
	XtermSubdomain string // "xterm" when browser terminals are served; "" omits TerminalURL
	InvitesPerUser int    // non-operator invite quota; 0 means operators only

	NewName func() string    // nil: the built-in adjective-noun generator
	Now     func() time.Time // nil: time.Now — injectable so schedule next-run is testable
	Log     *slog.Logger     // required; one audit line per mutation
}

func New(cfg Config) *Ops

// Close stops the job reaper. Idempotent.
func (o *Ops) Close()

// Caller is who is asking. Handle is already authenticated by the transport.
// KeyFP is the fingerprint of the SSH key on this session — audit only, echoed
// by Whoami, and used as the default GitHub proof on the SSH path; it is empty
// for HTTP callers. Operator status is deliberately NOT a field: ctlops resolves
// it from the account store when (and only when) a command needs it, so a
// transport that forgets to populate it cannot widen anyone's authority.
type Caller struct {
	Handle string
	KeyFP  string
}

// Capabilities reports what this host actually has configured, so a client can
// avoid provoking a KindDisabled instead of discovering it by trial.
type Capabilities struct {
	Archiving     bool `json:"archiving"`
	Snapshots     bool `json:"snapshots"`
	Scheduling    bool `json:"scheduling"`
	Tags          bool `json:"tags"`
	Routes        bool `json:"routes"`
	SessionTokens bool `json:"session_tokens"`
	Terminal      bool `json:"terminal"`
}

func (o *Ops) Capabilities() Capabilities

// ---------------------------------------------------------------------------
// Budgets
//
// Exported so both transports and the OpenAPI document quote the same numbers
// rather than copying them. Every method applies its own budget through an
// internal withBudget(ctx, d) that is a no-op when ctx already carries an
// earlier deadline — so a disconnecting client still cancels, and a caller that
// wants a tighter ceiling can impose one without ctlops fighting it.
// ---------------------------------------------------------------------------

const (
	PauseTimeout   = 3 * time.Minute  // a full guest memory snapshot
	ArchiveTimeout = 15 * time.Minute // fsck + zerofree + zstd of 25 GB, then transfer
	ResizeTimeout  = 10 * time.Minute // e2fsck + resize2fs + cold boot
	DialTimeout    = 15 * time.Second // create/attach: reaching a freshly booted guest
)

const (
	MaxTagsPerSandbox      = 32
	MaxDiskMB              = 1 << 20 // 1 TB, a fat-finger guard on resize
	SessionTokenMaxTTL     = 7 * 24 * time.Hour
	DefaultSessionTokenTTL = 12 * time.Hour
)

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// Kind classifies a failure so each transport renders it without re-deriving
// intent from message text. It is what replaces the three copy-pasted
// statusFor() functions that switch on substrings of err.Error().
type Kind int

const (
	KindInternal Kind = iota // an unexpected store or driver fault
	KindInvalid              // the caller's arguments are malformed
	KindNotFound             // absent, or someone else's — one message for both
	KindDenied               // authenticated, not permitted
	KindConflict             // well-formed, refused by current state
	KindDisabled             // the feature is not configured on this host
	KindLimit                // the per-owner running-sandbox cap
	KindCapacity             // the host RAM admission budget
	KindQuota                // the per-owner disk pool
	KindUpstream             // an external dependency (github.com) failed
)

func (k Kind) String() string

// Error is the single failure type every Ops method returns.
type Error struct {
	Kind Kind
	// Op is the command name — "pause", "snapshot.create". It is the <what> in
	// the SSH channel's "sparkbox: <what> failed: <err>" line, the OpenAPI
	// operationId, and the "op" field of the JSON error body, so the three
	// cannot drift.
	Op string
	// Code is a stable machine token for API clients: "sandbox_not_found",
	// "running_limit", "invite_quota_exhausted". Never localized, never changed
	// once shipped.
	Code string
	// Msg is a complete, already-curated user-facing sentence with no
	// "sparkbox: " prefix and no trailing newline.
	Msg string
	// Hint is the actionable second line — which sandboxes to pause, how to
	// enable the feature. It is what failStart() prints today.
	Hint string
	// Details carries structured facts a client can act on: running[] for a
	// KindLimit, budget_mb for a KindCapacity, matches[] for an ambiguous
	// passkey prefix. Rendered as-is into the JSON error body; ignored by SSH.
	Details map[string]any
	// Verbatim reports that Msg is already the whole sentence and must be
	// printed as-is on the SSH channel rather than wrapped in fail()'s
	// "sparkbox: <Op> failed: <Msg>" shape. This is not cosmetic: it is exactly
	// what keeps `no sandbox named "x"` and `pause failed: …` byte-identical
	// after the refactor, and there is no way to infer it from Kind.
	Verbatim bool
	// Exit overrides the exit code Kind implies; 0 means "use Kind's". It exists
	// for one shipped inconsistency — `keys add` answers a malformed key line
	// with exit 1, not the 2 every other bad invocation gets — preserved
	// deliberately, because other tooling may key off it, rather than tidied
	// away.
	Exit int
	// Status overrides the HTTP status Kind implies; 0 means "use Kind's". Same
	// escape hatch in the other direction, for the same `keys add` case, which
	// is plainly a 400 over HTTP however it exits over SSH.
	Status int
	// Err is the wrapped cause. Logged, never rendered to a client — the
	// proxy's rule, not the console's, because this surface faces strangers.
	Err error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// ExitCode is the ctl@ contract in one place: 2 means the invocation was wrong,
// 1 means it was right and failed.
func (e *Error) ExitCode() int

// HTTPStatus is the REST edge's contract in one place.
func (e *Error) HTTPStatus() int

// AsError classifies anything into an *Error, synthesising a KindInternal one
// for a stray error so no transport has to nil-check. It recognises
// *host.LimitError (KindLimit, running[] in Details), *host.CapacityError
// (KindCapacity), *host.DiskQuotaError (KindQuota — the case the SSH path
// misses today), users.ErrKeyLinked, users.ErrLastKey, users.ErrNoSuchPasskey,
// users.ErrAmbiguousPasskey, and context.Canceled.
func AsError(op string, err error) *Error

// IsKind is the predicate both edges use instead of comparing sentinels.
func IsKind(err error, k Kind) bool

// NotFound is the ONE constructor for the masked answer. Existence and
// ownership share it, from a single line of code per object kind, so there is no
// second path on which a distinguishing 403 could appear.
//   kind is "sandbox" | "snapshot" | "schedule" | "key" | "passkey" | "route".
func NotFound(op, kind, name string) *Error

func Invalid(op, code, format string, a ...any) *Error
func Disabled(op, sentence string) *Error // the host-not-configured sentences, verbatim
func Denied(op, code, sentence string) *Error
func Fail(op string, err error) *Error // classifies via AsError and wraps

// ---------------------------------------------------------------------------
// Result types
//
// Deliberately not *host.Sandbox: SSHAddr, HostIP, GuestV6 and KeyFP are
// internal topology, and api.<domain> is a documented public contract that must
// not move when an internal struct does.
// ---------------------------------------------------------------------------

type SandboxInfo struct {
	Name        string    `json:"name"`
	Owner       string    `json:"owner"`
	State       string    `json:"state"`
	Pinned      bool      `json:"pinned"`
	Ballooned   bool      `json:"ballooned,omitempty"`
	Tags        []string  `json:"tags"` // never nil
	VCPUs       int64     `json:"vcpus"`
	MemMB       int64     `json:"mem_mb"`
	DiskMB      int64     `json:"disk_mb,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	LastActive  time.Time `json:"last_active"`
	URL         string    `json:"url,omitempty"`          // https://<name>.<domain>
	TerminalURL string    `json:"terminal_url,omitempty"` // https://<name>.xterm.<domain>
}

type SnapshotInfo struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	FromBox   string    `json:"from_sandbox"`
	CreatedAt time.Time `json:"created_at"`
}

type ScheduleInfo struct {
	ID        string     `json:"id"`
	Sandbox   string     `json:"sandbox"`
	Spec      string     `json:"spec"`
	Command   string     `json:"command"`
	NextRun   *time.Time `json:"next_run,omitempty"` // nil when the spec no longer parses
	LastRun   *time.Time `json:"last_run,omitempty"`
	LastExit  int        `json:"last_exit"`
	LastError string     `json:"last_error,omitempty"`
}

type RouteInfo struct {
	Subdomain  string `json:"subdomain"`
	Sandbox    string `json:"sandbox"`
	Port       int    `json:"port"`
	Visibility string `json:"visibility"`
	URL        string `json:"url,omitempty"`
}

type Whoami struct {
	Handle           string     `json:"handle"`
	Status           string     `json:"status"`
	Operator         bool       `json:"operator"`
	Email            string     `json:"email,omitempty"`
	GitHubLogin      string     `json:"github_login,omitempty"`
	GitHubVerifiedAt *time.Time `json:"github_verified_at,omitempty"`
	Subject          string     `json:"subject"`          // oidc.SubjectFor(handle)
	KeyFP            string     `json:"key_fp,omitempty"` // the key on THIS session; "" over HTTP
}

type KeyInfo struct {
	FP      string    `json:"fingerprint"`
	Label   string    `json:"label,omitempty"`
	Via     string    `json:"via"`
	AddedAt time.Time `json:"added_at"`
	Current bool      `json:"current,omitempty"` // matches Caller.KeyFP
}

type PasskeyInfo struct {
	ID         string     `json:"id"`
	Label      string     `json:"label,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// ImportResult reports skipped fingerprints rather than printing them: ctl
// writes one stderr note each, REST returns the list.
type ImportResult struct {
	Login    string   `json:"login"`
	Imported int      `json:"imported"` // genuinely new keys; AddKey is idempotent
	Listed   int      `json:"listed"`
	Skipped  []string `json:"skipped"` // already linked elsewhere; never nil
}

type TokenResult struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type InviteResult struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type VisibilityResult struct {
	Routes  []RouteInfo `json:"routes"` // never nil
	Changed int         `json:"changed"`
}

// Endpoint is where to dial a running sandbox's sshd. It is the one result type
// carrying internal topology, exists solely for the terminal bridge, and has no
// JSON tags precisely so it can never be serialized onto the edge by accident.
type Endpoint struct {
	Name    string
	SSHAddr string
	SSHUser string
}

// ---------------------------------------------------------------------------
// Argument types
// ---------------------------------------------------------------------------

// CreateArgs mirrors the new@ door with its ambiguity removed. The door has to
// read bare words as tags because ssh(1) eats leading-dash arguments, but a JSON
// body has named fields — so there is deliberately no Command field here, which
// is the bug execsCommand exists to prevent.
type CreateArgs struct {
	Name  string   // "" generates an adjective-noun name
	Tags  []string // normalized and stamped BEFORE Create, rolled back on failure
	VCPUs int64    // 0 takes the manager default (4)
	MemMB int64    // 0 takes the manager default (12288)
}

type ForkArgs struct {
	Snapshot string
	Name     string
	Tags     []string // same ordering constraint as CreateArgs
}

type ScheduleArgs struct {
	Sandbox string
	Spec    string
	Command string
}

// ---------------------------------------------------------------------------
// Sandboxes
// ---------------------------------------------------------------------------

// Get resolves a sandbox the caller may act on. Missing and not-yours return the
// identical *Error, and every method below calls the same internal gate before
// touching the manager — so a cross-owner probe can never confirm a name and can
// never wake a VM.
func (o *Ops) Get(ctx context.Context, c Caller, name string) (SandboxInfo, error)
func (o *Ops) List(ctx context.Context, c Caller) ([]SandboxInfo, error)

// Create stamps tags BEFORE Sandboxes.Create, because Create fires the
// secret-env push asynchronously and the tags decide its contents; it clears the
// tag rows again if the create fails. This is the ordering userconsole's fork
// gets wrong, fixed once here for every caller.
func (o *Ops) Create(ctx context.Context, c Caller, a CreateArgs) (SandboxInfo, error)

func (o *Ops) Pause(ctx context.Context, c Caller, name string) (SandboxInfo, error)

// Resume is EnsureRunning: it starts a paused box and folds in the
// download+unpack for an archived one, which is why ctl calls it "restore".
func (o *Ops) Resume(ctx context.Context, c Caller, name string) (SandboxInfo, error)

func (o *Ops) Archive(ctx context.Context, c Caller, name string) (SandboxInfo, error)

// Resize grows the root disk. It pauses, DISCARDS the memory snapshot, resizes
// and cold-boots, so in-guest processes die; surfacing that warning is the
// caller's job.
func (o *Ops) Resize(ctx context.Context, c Caller, name string, sizeMB int64) (SandboxInfo, error)

func (o *Ops) Reboot(ctx context.Context, c Caller, name string) (SandboxInfo, error)
func (o *Ops) Rename(ctx context.Context, c Caller, name, newName string) (SandboxInfo, error)
func (o *Ops) Destroy(ctx context.Context, c Caller, name string) error

// SetPinned sets the always-on flag and NOTHING else. `ctl pin` composes it with
// Resume in its renderer and keeps its half-succeeded exit 1; the REST API keeps
// them separate so there is no partial state to invent a status code for.
func (o *Ops) SetPinned(ctx context.Context, c Caller, name string, pinned bool) (SandboxInfo, error)

func (o *Ops) Tags(ctx context.Context, c Caller, name string) ([]string, error)

// SetTags replaces the whole set (nil or empty clears it) and then ResyncEnv's
// the box, so the guest's secret env matches its new tags immediately rather
// than at the next resume.
func (o *Ops) SetTags(ctx context.Context, c Caller, name string, tags []string) ([]string, error)

// ---------------------------------------------------------------------------
// Snapshots
// ---------------------------------------------------------------------------

func (o *Ops) ListSnapshots(ctx context.Context, c Caller) ([]SnapshotInfo, error)

// CreateSnapshot implicitly PAUSES a running sandbox: the manager strips the
// managed env block, pauses, then compacts.
func (o *Ops) CreateSnapshot(ctx context.Context, c Caller, sandbox, name string) (SnapshotInfo, error)

func (o *Ops) DeleteSnapshot(ctx context.Context, c Caller, name string) error
func (o *Ops) Fork(ctx context.Context, c Caller, a ForkArgs) (SandboxInfo, error)

// ---------------------------------------------------------------------------
// Schedules
// ---------------------------------------------------------------------------

func (o *Ops) ListSchedules(ctx context.Context, c Caller) ([]ScheduleInfo, error)
func (o *Ops) AddSchedule(ctx context.Context, c Caller, a ScheduleArgs) (ScheduleInfo, error)

// DeleteSchedule masks a foreign id exactly like a foreign sandbox name.
func (o *Ops) DeleteSchedule(ctx context.Context, c Caller, id string) error

// ---------------------------------------------------------------------------
// Sharing
// ---------------------------------------------------------------------------

func (o *Ops) Visibility(ctx context.Context, c Caller, name string) ([]RouteInfo, error)

// SetVisibility flips EVERY route of a sandbox together — the per-sandbox
// granularity `ctl share` has always had. The user console's per-route endpoint
// is a different operation and stays where it is.
func (o *Ops) SetVisibility(ctx context.Context, c Caller, name, visibility string) (VisibilityResult, error)

// ---------------------------------------------------------------------------
// Account
// ---------------------------------------------------------------------------

func (o *Ops) Whoami(ctx context.Context, c Caller) (Whoami, error)
func (o *Ops) ListKeys(ctx context.Context, c Caller) ([]KeyInfo, error)

// AddKey parses one authorized_keys line. A malformed line is KindInvalid with
// Exit:1 — the deliberate CLI inconsistency — and Status:400.
func (o *Ops) AddKey(ctx context.Context, c Caller, authorizedKeyLine string) (KeyInfo, error)

func (o *Ops) RemoveKey(ctx context.Context, c Caller, fp string) error
func (o *Ops) ImportGitHubKeys(ctx context.Context, c Caller) (ImportResult, error)

// VerifyGitHub proves the link by finding one of the caller's ALREADY-REGISTERED
// keys on github.com/<login>.keys. proofFP names which one: the SSH path passes
// the session key's fingerprint, and an HTTP caller must name one explicitly
// because there is no session key to imply. ctlops verifies the fingerprint
// belongs to the caller before using it — otherwise a stranger could claim any
// login. An empty login falls back to the stored one.
func (o *Ops) VerifyGitHub(ctx context.Context, c Caller, login, proofFP string) (Whoami, error)

func (o *Ops) ListPasskeys(ctx context.Context, c Caller) ([]PasskeyInfo, error)
func (o *Ops) RemovePasskey(ctx context.Context, c Caller, idPrefix string) error
func (o *Ops) Email(ctx context.Context, c Caller) (string, error)
func (o *Ops) SetEmail(ctx context.Context, c Caller, addr string) (string, error) // "" clears

// MintSessionToken silently clamps ttl to SessionTokenMaxTTL, as ctl does — a
// week-and-a-day request is a rounding error, not a user error. A ttl <= 0 takes
// DefaultSessionTokenTTL.
func (o *Ops) MintSessionToken(ctx context.Context, c Caller, ttl time.Duration) (TokenResult, error)

// Invite is the ONLY operator-gated operation in ctlops. Operator status is
// resolved from the account store inside this method, never taken from the
// caller, so no transport can assert it.
func (o *Ops) Invite(ctx context.Context, c Caller) (InviteResult, error)

// ---------------------------------------------------------------------------
// Terminal
// ---------------------------------------------------------------------------

// Attach is the owner gate plus resume for an interactive session. It is the
// only method that returns an SSH address, and it exists so the terminal bridge
// cannot skip the check every other command performs. The ownership check
// strictly precedes EnsureRunning, so a cross-owner probe can never wake a
// stranger's VM — and the returned Endpoint comes from the RESUMED record,
// because the pre-resume one has SSHAddr and HostIP cleared while paused.
func (o *Ops) Attach(ctx context.Context, c Caller, name string) (Endpoint, error)

// Touch marks a sandbox active. Called once at session end by the terminal
// bridge, exactly as the SSH gateway defers mgr.Touch — never on a keepalive,
// because a ping-driven Touch turns a forgotten browser tab into a permanently
// pinned VM.
func (o *Ops) Touch(name string)

// ---------------------------------------------------------------------------
// Jobs
//
// Long operations over HTTP need somewhere to live that is not a held-open
// connection: 15 minutes is longer than every CDN and cloudflared idle budget on
// the path. Jobs are used ONLY by internal/restapi; the SSH path stays
// synchronous and byte-identical.
// ---------------------------------------------------------------------------

type JobState string

const (
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCanceled  JobState = "canceled"
)

// Ref names what a job acts on, so a client can correlate without parsing Op.
type Ref struct {
	Type string `json:"type"` // "sandbox" | "snapshot"
	Name string `json:"name"`
}

// Job is deliberately in-memory and not persisted: a control-plane restart also
// kills the operation the job describes, so a surviving "running" row would be a
// lie. Documented as such in the OpenAPI spec.
type Job struct {
	ID         string          `json:"id"`
	Op         string          `json:"op"`
	Owner      string          `json:"-"`
	Resource   Ref             `json:"resource"`
	State      JobState        `json:"state"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *Error          `json:"error,omitempty"`
}

// JobRetain is how long a finished job stays readable; JobMaxPerOwner bounds the
// registry, evicting the oldest finished job first.
const (
	JobRetain      = time.Hour
	JobMaxPerOwner = 200
)

// Go starts fn on a context detached from the caller's request — an HTTP client
// hanging up must not abort a 15-minute archive — bounded by budget. If an
// identical (owner, op, resource) job is already running it returns THAT job
// instead of starting a second: firing two archives of one sandbox is never what
// a retry meant, which makes every long operation idempotent for free.
func (o *Ops) Go(c Caller, op string, ref Ref, budget time.Duration,
	fn func(ctx context.Context) (any, error)) *Job

// Await blocks until j finishes or d elapses, returning j's current snapshot.
// This is the whole sync-first mechanism: the REST edge Awaits for the Prefer
// window and answers 200 or 202 depending on what came back.
func (o *Ops) Await(ctx context.Context, j *Job, d time.Duration) *Job

func (o *Ops) Job(c Caller, id string) (*Job, error) // foreign ids are KindNotFound
func (o *Ops) ListJobs(c Caller) ([]*Job, error)
func (o *Ops) CancelJob(c Caller, id string) (*Job, error) // a finished job is KindConflict

// ---------------------------------------------------------------------------
// Shared parsing — domain validation both transports need
// ---------------------------------------------------------------------------

// ParseSize reads "25G" / "25GB" / "512M" / a bare number meaning GB into MiB
// and enforces MaxDiskMB. Moved here verbatim from sshgw so the REST `size`
// field and `ctl resize` cannot drift, error text included.
func ParseSize(arg string) (int64, error)

// NormalizeTags lowercases, splits on commas, trims, dedupes, sorts and enforces
// MaxTagsPerSandbox. Every write path calls it, so the cap cannot be bypassed by
// a transport that does not parse flags. Idempotent, so sshgw calling it first
// costs nothing. It does NOT parse flags — `--tag` handling is CLI syntax and
// stays in sshgw.
func NormalizeTags(in []string) ([]string, error)

// GenerateName returns an unused adjective-noun name, falling back to a hex
// suffix when the pool keeps colliding. Moved out of sshgw because "if you don't
// name it, the platform names it" is a control-plane rule, not an SSH one.
func (o *Ops) GenerateName() string
```

---

# Part 2 — Error taxonomy

One table, two contracts. `Error.Exit` and `Error.Status` override a cell only
where a shipped inconsistency demands it; both are called out below the table.

| Kind | `Code` examples | SSH exit | HTTP | Meaning |
|---|---|---|---|---|
| `KindInvalid` | `bad_size`, `bad_cron`, `bad_visibility`, `bad_tag`, `bad_ttl`, `bad_email` | **2** | **400** | The caller's invocation is wrong. Over SSH this prints the usage text. |
| `KindNotFound` | `sandbox_not_found`, `snapshot_not_found`, `schedule_not_found`, `key_not_found`, `passkey_not_found` | 1 | **404** | Absent, or someone else's. One message for both; never a 403. |
| `KindDenied` | `not_operator`, `invite_quota_exhausted`, `github_key_not_listed` | 1 | **403** | Authenticated, not permitted. |
| `KindConflict` | `name_taken`, `name_reserved`, `last_key`, `key_linked_elsewhere`, `passkey_ambiguous`, `already_archived`, `job_finished` | 1 | **409** | Well-formed, refused by current state. |
| `KindDisabled` | `archiving_disabled`, `scheduling_disabled`, `tags_disabled`, `routes_disabled`, `sessions_disabled`, `snapshots_disabled` | 1 | **501** | The feature is not configured on this host. |
| `KindLimit` | `running_limit` (`details.running[]`, `details.max`) | 1 | **429** + `Retry-After: 60` | The per-owner running-sandbox cap. Actionable by the caller: pause something. |
| `KindCapacity` | `host_at_capacity` (`details.used_mb`, `requested_mb`, `budget_mb`) | 1 | **503** + `Retry-After: 60` | The host RAM admission budget. Not the caller's fault. |
| `KindQuota` | `disk_pool_full` (`details.used_mb`, `requested_mb`, `pool_mb`) | 1 | **507** | The per-owner disk pool. `failStart` misses this today; `AsError` fixes it. |
| `KindUpstream` | `github_unreachable` | 1 | **502** | An external dependency failed. |
| `KindInternal` | `internal` | 1 | **500** | Anything else. `Err` is logged; only `Msg` reaches the client. |

Two documented overrides:

- **`keys add` with an unparseable key line** is `KindInvalid` with `Exit: 1`.
  The CLI has always answered 1 there where every other bad invocation answers 2;
  other tooling may depend on it, so it is preserved rather than tidied. HTTP
  correctly sees 400.
- **A canceled context** (`context.Canceled`) becomes `KindInternal` with
  `Status: 499`. The REST edge does not write a body for it — the client is gone.

`RequireMutation`'s CSRF rejection is a **plain-text 403** emitted by
`http.Error`, not the JSON envelope, because `internal/edgeauth` is shared with
the shipped user console and is not being forked. The OpenAPI spec documents
that response as `text/plain`; a client parsing JSON unconditionally must handle
it.

---

# Part 3 — the REST API

Served at `https://api.<domain>` as a reserved subdomain on the proxy edge,
exactly like the consoles. `internal/api` (the unauthenticated loopback control
API on `--api-addr`) is **not** this, is not mounted on the edge, and gains one
line of package doc saying so.

## Conventions *(as built — this section was corrected after implementation)*

- **Auth.** Reads go through `edgeauth.Require`; writes through
  `edgeauth.RequireMutation` with origin `https://api.<domain>`. The credential
  is the existing `spark_session` cookie or `Authorization: Bearer spk_v1.…`,
  minted by `ssh ctl@<domain> session-token` or by `POST /v1/account/tokens`.
- **`Cache-Control: no-store`** on every `/v1` response, set by `Require`.
- **Collections are a wrapper object, not a bare array** —
  `{"sandboxes":[…]}`, `{"snapshots":[…]}`, `{"jobs":[…]}`, `{"keys":[…]}` — so
  a future field can be added without breaking every client's parser. The slice
  is never `null`. Single resources are the bare object.
- **`DELETE` returns `200` with a body**, not `204`: `{"name":…,"deleted":true}`
  for a sandbox, the affected resource or job otherwise. A `204` would have
  meant re-deriving what happened from the request.
- **Error body** is
  `{"error":{"kind":"…","op":"…","code":"…","message":"…","hint":"…","details":{…}}}`,
  with `hint` and `details` omitted when empty. `kind` is the `ctlops.Kind`
  string, `op` is the `operationId`, `code` is the stable machine token.
  The two exceptions are the auth gate's own refusals, which are `text/plain`
  because they come from `edgeauth` and predate this package.
- **`Prefer: wait=N`** (seconds, default `10`, clamped to `60`) sets how long a
  slow endpoint waits before escalating; `Prefer: respond-async` forces an
  immediate `202`. The applied value comes back in `Preference-Applied`.
  Endpoints marked ⏳ can escalate: `200`/`201` with the final resource if the
  work finished inside the window, or `202` with a `Job` body and
  `Location: /v1/jobs/{id}` if it did not.
- **`Idempotency-Key`** is honoured on **every** mutating endpoint, not just
  creates: a repeat within 24 h replays the first response verbatim and sets
  `Idempotency-Replayed`. A key reused with a *different* method, path or body
  is a `409`, because replaying the first answer would silently drop the second
  request. Long operations are additionally idempotent for free — `ctlops.Go`
  returns the running job when the same owner asks for the same work with the
  same arguments.
- **A key fingerprint is a request field, not a path segment.**
  `DELETE /v1/account/keys` takes `{"fingerprint":"SHA256:…"}`, which avoids
  percent-encoding a `:` and `/` into a path.

## Routes *(as built)*

Generated from `internal/restapi/server.go`'s route table, which
`openapi_test.go` holds in a proven bijection with `openapi.json` — so this
table, the served document and the mux cannot drift apart without a test
failure. ⏳ marks an endpoint that can escalate to a job.

| Method | Path | `operationId` | Auth |
|---|---|---|---|
| `GET` | `/` | `root` | public → `303 /docs` |
| `GET` | `/docs` | `docs` | public |
| `GET` | `/openapi.json` | `openapi.json` | public |
| `GET` | `/openapi.yaml` | `openapi.yaml` | public |
| `GET` | `/v1/capabilities` | `capabilities` | read |
| `GET` | `/v1/whoami` | `whoami` | read |
| `GET` | `/v1/sandboxes` | `list` | read |
| `POST` ⏳ | `/v1/sandboxes` | `create` | mutate |
| `GET` | `/v1/sandboxes/{name}` | `get` | read |
| `DELETE` ⏳ | `/v1/sandboxes/{name}` | `rm` | mutate |
| `POST` ⏳ | `/v1/sandboxes/{name}/pause` | `pause` | mutate |
| `POST` ⏳ | `/v1/sandboxes/{name}/resume` | `restore` | mutate |
| `POST` ⏳ | `/v1/sandboxes/{name}/archive` | `archive` | mutate |
| `POST` ⏳ | `/v1/sandboxes/{name}/resize` | `resize` | mutate |
| `POST` ⏳ | `/v1/sandboxes/{name}/reboot` | `reboot` | mutate |
| `POST` ⏳ | `/v1/sandboxes/{name}/rename` | `rename` | mutate |
| `POST` | `/v1/sandboxes/{name}/pin` | `pin` | mutate |
| `POST` | `/v1/sandboxes/{name}/unpin` | `unpin` | mutate |
| `GET` | `/v1/sandboxes/{name}/tags` | `tags.get` | read |
| `PUT` | `/v1/sandboxes/{name}/tags` | `tags.set` | mutate |
| `GET` | `/v1/sandboxes/{name}/visibility` | `share.get` | read |
| `PUT` | `/v1/sandboxes/{name}/visibility` | `share.set` | mutate |
| `GET` | `/v1/sandboxes/{name}/terminal` | `attach` | read (WebSocket) |
| `GET` | `/v1/snapshots` | `snapshot.list` | read |
| `POST` ⏳ | `/v1/snapshots` | `snapshot.create` | mutate |
| `DELETE` | `/v1/snapshots/{name}` | `snapshot.rm` | mutate |
| `POST` ⏳ | `/v1/snapshots/{name}/fork` | `fork` | mutate |
| `GET` | `/v1/schedules` | `schedule.list` | read |
| `POST` | `/v1/schedules` | `schedule.add` | mutate |
| `DELETE` | `/v1/schedules/{id}` | `schedule.rm` | mutate |
| `GET` | `/v1/account/keys` | `keys.list` | read |
| `POST` | `/v1/account/keys` | `keys.add` | mutate |
| `DELETE` | `/v1/account/keys` | `keys.rm` | mutate |
| `POST` | `/v1/account/keys/import-github` | `keys.import-github` | mutate |
| `POST` | `/v1/account/github` | `keys.verify-github` | mutate |
| `GET` | `/v1/account/passkeys` | `passkey.list` | read |
| `DELETE` | `/v1/account/passkeys/{id}` | `passkey.rm` | mutate |
| `GET` | `/v1/account/email` | `email.get` | read |
| `PUT` | `/v1/account/email` | `email.set` | mutate |
| `POST` | `/v1/account/tokens` | `session-token` | mutate |
| `POST` | `/v1/account/invites` | `invite` | mutate |
| `GET` | `/v1/jobs` | `jobs.list` | read |
| `GET` | `/v1/jobs/{id}` | `jobs.get` | read |
| `DELETE` | `/v1/jobs/{id}` | `jobs.cancel` | mutate |

Differences from the plan below, all of them the code's call: account
operations live under `/v1/account/…` rather than at the root; pinning is
`POST …/pin` and `POST …/unpin` rather than `PUT …/pinned`, so a client never
has to encode "the write landed, the resume did not"; there is no `/healthz`
(the loopback `internal/api` already has one and this host is not a health
probe's business); and a request the table does not claim gets the error
envelope too — `404 unknown_endpoint`, or `405 method_not_allowed` with an
`Allow` header when the path exists but the verb does not.

### Response codes

`openapi.json` is the authoritative list per operation. The mapping that
produces them is `ctlops.Kind` → `Error.HTTPStatus()`, in one place:

| Kind | Status | Typical `code` |
|---|---|---|
| `invalid` | `400` | `invalid_name`, `malformed_body` |
| `denied` | `403` | `not_operator`, `account_disabled` |
| `not_found` | `404` | `sandbox_not_found` (also for someone else's) |
| `conflict` | `409` | `name_taken`, `name_reserved`, `last_key` |
| `limit` | `429` | `running_limit` |
| `disabled` | `501` | `archive_disabled`, `tags_disabled` (derived from the op) |
| `capacity` | `503` | `host_at_capacity` |
| `quota` | `507` | `disk_pool_full` |
| `upstream` | `502` | github.com failed |
| `internal` | `500` | — |

## Routes *(as designed — superseded by the table above)*

Ungated:

| Method | Path | Response |
|---|---|---|
| `GET` | `/healthz` | `200 {"ok":true,"version":"…"}` |
| `GET` | `/openapi.json` | `200 application/json` — the embedded spec, byte-for-byte |
| `GET` | `/openapi.yaml` | `200 application/yaml` — deterministically derived from it |
| `GET` | `/docs` | `200 text/html`, CSP `default-src 'self'` |
| `GET` | `/` | `302` → `/docs` |

Account (`Require` on reads, `RequireMutation` on writes):

| Method | Path | Request | Success | Failure |
|---|---|---|---|---|
| `GET` | `/v1/whoami` | — | `200 Whoami` | 401 |
| `GET` | `/v1/capabilities` | — | `200 Capabilities` | 401 |
| `GET` | `/v1/keys` | — | `200 [KeyInfo]` | 401 |
| `POST` | `/v1/keys` | `{"authorized_key":"ssh-ed25519 AAAA… label"}` | `201 KeyInfo` | 400, 401, 403, 409 `key_linked_elsewhere` |
| `DELETE` | `/v1/keys/{fingerprint}` | — | `204` | 401, 403, 404, 409 `last_key` |
| `POST` | `/v1/keys/import-github` | — | `200 ImportResult` | 401, 403, 409 `github_not_linked`, 502 |
| `POST` | `/v1/keys/verify-github` | `{"login":"?","fingerprint":"SHA256:…"}` | `200 Whoami` | 400, 401, 403 `github_key_not_listed`, 404, 502 |
| `GET` | `/v1/passkeys` | — | `200 [PasskeyInfo]` | 401 |
| `DELETE` | `/v1/passkeys/{id}` | — | `204` | 401, 403, 404, 409 `passkey_ambiguous` |
| `GET` | `/v1/email` | — | `200 {"email":""}` | 401 |
| `PUT` | `/v1/email` | `{"email":"you@example.com"}` | `200 {"email":"…"}` | 400, 401, 403 |
| `DELETE` | `/v1/email` | — | `204` | 401, 403 |
| `POST` | `/v1/session-tokens` | `{"ttl":"12h"}` | `201 TokenResult` | 400, 401, 403, 501 |
| `POST` | `/v1/invites` | — | `201 InviteResult` | 401, 403 `not_operator` / `invite_quota_exhausted` |

Sandboxes:

| Method | Path | Request | Success | Failure |
|---|---|---|---|---|
| `GET` | `/v1/sandboxes` | — | `200 [SandboxInfo]` | 401 |
| `POST` ⏳ | `/v1/sandboxes` | `{"name":"?","tags":[],"vcpus":0,"mem_mb":0}` | `201 SandboxInfo` + `Location` | 400, 401, 403, 409, 429, 503, 507 |
| `GET` | `/v1/sandboxes/{name}` | — | `200 SandboxInfo` | 401, 404 |
| `DELETE` ⏳ | `/v1/sandboxes/{name}` | — | `204` | 401, 403, 404 |
| `POST` ⏳ | `/v1/sandboxes/{name}/pause` | — | `200 SandboxInfo` | 401, 403, 404, 500 |
| `POST` ⏳ | `/v1/sandboxes/{name}/resume` | — | `200 SandboxInfo` | 401, 403, 404, 429, 503, 507 |
| `POST` ⏳ | `/v1/sandboxes/{name}/archive` | — | `200 SandboxInfo` | 401, 403, 404, 501 |
| `POST` ⏳ | `/v1/sandboxes/{name}/resize` | `{"size":"25G"}` or `{"size_mb":25600}` | `200 SandboxInfo` | 400, 401, 403, 404, 409, 501 |
| `POST` ⏳ | `/v1/sandboxes/{name}/reboot` | — | `200 SandboxInfo` | 401, 403, 404 |
| `POST` ⏳ | `/v1/sandboxes/{name}/rename` | `{"new_name":"…"}` | `200 SandboxInfo` + `Content-Location` | 400, 401, 403, 404, 409 |
| `GET` | `/v1/sandboxes/{name}/tags` | — | `200 {"tags":[]}` | 401, 404, 501 |
| `PUT` | `/v1/sandboxes/{name}/tags` | `{"tags":["ml"]}` (`[]` clears) | `200 {"tags":[]}` | 400, 401, 403, 404, 501 |
| `PUT` | `/v1/sandboxes/{name}/pinned` | `{"pinned":true}` | `200 SandboxInfo` | 401, 403, 404 |
| `GET` | `/v1/sandboxes/{name}/visibility` | — | `200 {"routes":[]}` | 401, 404, 501 |
| `PUT` | `/v1/sandboxes/{name}/visibility` | `{"visibility":"public"}` | `200 VisibilityResult` | 400, 401, 403, 404, 501 |
| `GET` | `/v1/sandboxes/{name}/terminal` | WebSocket upgrade | `101` | 400, 401, 403, 404, 426, 501 |

Snapshots, schedules, jobs:

| Method | Path | Request | Success | Failure |
|---|---|---|---|---|
| `GET` | `/v1/snapshots` | — | `200 [SnapshotInfo]` | 401 |
| `POST` ⏳ | `/v1/snapshots` | `{"sandbox":"x","name":"y"}` | `201 SnapshotInfo` | 400, 401, 403, 404, 409, 501 |
| `DELETE` | `/v1/snapshots/{name}` | — | `204` | 401, 403, 404 |
| `POST` ⏳ | `/v1/snapshots/{name}/fork` | `{"name":"z","tags":[]}` | `201 SandboxInfo` | 400, 401, 403, 404, 409, 429, 503, 507 |
| `GET` | `/v1/schedules` | — | `200 [ScheduleInfo]` | 401, 501 |
| `POST` | `/v1/schedules` | `{"sandbox":"x","spec":"*/30 * * * *","command":"…"}` | `201 ScheduleInfo` | 400, 401, 403, 404, 501 |
| `DELETE` | `/v1/schedules/{id}` | — | `204` | 401, 403, 404, 501 |
| `GET` | `/v1/jobs` | — | `200 [Job]` | 401 |
| `GET` | `/v1/jobs/{id}` | — | `200 Job` | 401, 404 |
| `DELETE` | `/v1/jobs/{id}` | — | `202 Job` (canceling) | 401, 404, 409 `job_finished` |

## Deliberate divergences from `ctl`, all stated in the spec's `description` fields

- **`POST …/pin` does not start the sandbox.** `ctl pin` composes pin+resume;
  REST keeps them separate so there is no half-succeeded state to encode.
- **`verify-github` requires an explicit `fingerprint`** of a key already on the
  caller's account, because there is no session key over HTTP to imply one.
- **There is no `signup` endpoint.** `signup@` is a four-step interactive PTY
  dialog that redeems an invite as a reservation; it stays SSH-only, and the
  browser path stays the passkey flow on `login.<domain>`.
- **`resize` warns in the spec, not in the response.** Growing the disk pauses
  the guest and discards its memory snapshot, so running processes die. `ctl`
  prints that sentence to a terminal; there is no terminal here, so it is
  documented on the operation.
- **`email` clears with `PUT {"email":""}`.** The design proposed a `DELETE`;
  there is only the one verb, because two ways to write one field is two things
  to keep in agreement.

## `/docs`

Hand-authored against `internal/webui`'s shared design system: literal
`/*SHARED_CSS*/` and `/*SHARED_JS*/` markers inside its own `<style>`/`<script>`
blocks, literal `</head>` and `<body>` tags so `hack/preview-console.py` keeps
working, `webui.Build` at package init. It fetches same-origin `/openapi.json`
and renders operations grouped by tag with request/response schemas expanded
inline. **No CDN, no vendored Swagger blob** — a strict first-party CSP is the
point.

## The spec-honesty test

`internal/restapi/openapi_test.go` walks the mux's registered `METHOD /path`
patterns and asserts a **bijection** with the spec's `paths × methods` in both
directions, that every `operationId` names a real `ctlops` op string, and that
every documented status code is one `Kind.HTTPStatus()` can actually produce. A
route added without a spec entry, or a spec entry without a route, fails the
test. That is what makes the document accurate rather than decorative.

---

# Part 4 — the browser terminal

The page and its socket are served from `https://<name>.xterm.<domain>`:

| Path | Handler |
|---|---|
| `GET /` | the xterm.js page, behind `edgeauth.Require` |
| `GET /assets/{file}` | the vendored xterm.js 5.5.0 / addon-fit 0.10.0 / addon-web-links 0.11.0 / addon-clipboard 0.2.0 / xterm.css, `//go:embed`, `Cache-Control: public, max-age=31536000, immutable` |
| `GET /ws` | the PTY bridge, subprotocol `sparkbox.terminal.v1` |

There is **no** `/api/session` endpoint; the pre-flight the design proposed
became a bare `fetch('/ws')`, which is described under *The 401-invisible-to-JS
problem* below.

The identical bridge is mounted at `GET https://api.<domain>/v1/sandboxes/{name}/terminal`
for CLI clients. Both go through the same code with the same rules.

## Auth and origin

**The credential rides automatically.** `spark_session` has `Domain=".<domain>"`,
so the browser attaches it to a handshake against `<name>.xterm.<domain>` with no
changes anywhere. This is the answer to "browser JS cannot set headers on a
WebSocket handshake": it does not need to.

**A token never goes in the query string** — query strings land in access logs,
`Referer`, and browser history, and a session token is a seven-day bearer
credential. `Sec-WebSocket-Protocol` is never a credential either: it is the one
header cross-origin JS *can* set, which would reintroduce exactly the CSRF the
origin check closes.

**The origin rule, as built:**

```
if an Origin header is present
        it must equal, case-insensitively, the origin this request was
        addressed to (scheme from X-Forwarded-Proto or the TLS state,
        plus r.Host). Anything else -> 403. A Bearer token does NOT
        excuse a mismatched Origin.
else (no Origin at all)
        the caller must present Authorization: Bearer — every browser
        sends an Origin on a WebSocket handshake, so its absence is a
        claim to be a non-browser, and the token is what backs the claim.
        Otherwise -> 403.
```

This is stricter than the design proposed, which would have accepted any bearer
request without looking at `Origin`. Not requiring `Origin` at all is the hole:
`coder/websocket`'s default check *allows* a missing one. The explicit check
runs **before** `websocket.Accept` so a refusal is a readable `403` rather than
a mystery handshake failure, and so it is directly testable; `Accept` then runs
with neither `OriginPatterns` nor `InsecureSkipVerify`, so the library's own
default (`Origin` host must equal the request host) is a second, independent
implementation of the same rule rather than a configuration of ours.

Concretely this rejects a sandbox app a stranger made public at
`evil.<domain>` — same-site with us, so `SameSite=Lax` fences off nothing, and
the cookie is sent — on origin inequality alone.

## Handler order, which is also the security argument

1. `signer.IdentityFrom(r)` → `401` on failure. A handshake sends no
   `Accept: text/html`, so there is no login redirect here; see the pre-flight
   below.
2. `r.ProtoMajor == 2` → **`505 HTTP Version Not Supported`** (the design said
   `426`; `ws.go` answers `505`, which is the honest one — the client's protocol
   version, not its lack of an upgrade, is the problem). `websocket.Accept`
   needs `http.Hijacker` and the hijack failure would otherwise be unreadable.
3. The origin rule → `403`.
4. `ops.Attach(ctx, Caller{Handle: sess.Handle}, name)` → `404` for both missing
   and not-yours, **before any VM work**, with the byte-identical body a
   genuinely missing sandbox produces. Rendered as an HTTP status, never as a
   close code, so it is greppable in logs and visible to `curl`.
5. `websocket.Accept` → `101`. Only now does a shell exist.

The sandbox name comes from the **Host**, via `proxy.SuffixName(ctx)` on the
xterm host or the `{name}` path segment on `api.<domain>`. There is no `?name=`
parameter anywhere.

## The 401-invisible-to-JS problem

A failed handshake surfaces to browser JS as an opaque error with no status
code, so the page can never say *why*. Two fixes: `GET /` is wrapped in
`edgeauth.Require`, so a browser with no session sends `Accept: text/html`, gets
the `303` to `login.<domain>/?return=…`, and never reaches a broken socket; and
when a socket does drop, the page probes with a plain `fetch('/ws')` before
deciding what happened, reloading (and so redirecting to login) only on a `401`.

**Only `401` means expired**, and that is not pedantry. The auth gate runs
first and answers `401`; everything past it answers `403` — including the origin
gate, which refuses this very probe, because `fetch()` does not attach an
`Origin` to a same-origin `GET` and the gate insists on an `Origin` or a bearer
token. So a `403` here is what a *perfectly valid* session looks like. An early
cut treated it as expiry and reload-looped forever behind a proxy that answers
the handshake but never upgrades, throwing away the scrollback the reconnect
logic exists to preserve.

## Wire protocol

| Direction | Frame | Payload |
|---|---|---|
| client → server | **binary** | raw keystroke bytes for the guest PTY |
| client → server | **text** | `{"type":"resize","rows":N,"cols":N}` |
| server → client | **binary** | raw PTY output bytes |
| server → client | **text** | `{"type":"exit","code":N}`, `{"type":"notice","text":"…"}` |

PTY bytes are binary in both directions and the page sets
`ws.binaryType = 'arraybuffer'`. A text frame must be valid UTF-8; a PTY read
*will* split a multi-byte rune across frames and the browser fails the
connection when it does. xterm.js accepts a `Uint8Array` and reassembles partial
UTF-8 itself.

Other bridge requirements, each closing a specific failure:

- `conn.SetReadLimit(1 << 20)` — the default 32 KiB turns a large paste into a
  `StatusMessageTooBig` disconnect.
- Every server→client `Write` is wrapped in a **10 s bounded context**, so one
  stalled tab cannot wedge the read-from-guest goroutine and back-pressure the
  SSH channel window into the guest.
- `rows`/`cols` are clamped to `[1,1000] × [1,1000]` before they become a
  `TIOCSWINSZ` in the guest. The page sends one resize immediately after the fit
  addon's first measurement, and debounces `window.resize`.
- A **30 s `Ping` ticker** runs concurrently with the always-live read loop —
  which is required, because the pong is consumed by the reader — and is what
  keeps the socket alive through cloudflared. `CloseRead` is never used.

## Close codes

**Superseded by what shipped.** The table below is the design's proposal; the
codes `ws.go` actually sends, which `index.html` and `openapi.json` both key on,
are `4001`/`4002`/`4003` — see `terminal-over-https-design.md`, which was updated
in the same landing and is the authoritative record for the reserved numbering.

| Code | Meaning | Page behaviour |
|---|---|---|
| `4001` | the shell exited; preceded by a `{"type":"exit","code":N}` text frame | print `[exited N]`, offer **New shell** |
| `4002` | hung up by the control plane (the sandbox was paused or the host is shutting down) | print the goodbye already streamed, offer **Reconnect** — never auto-reconnect, which would resurrect the VM the reaper just paused |
| `4003` | could not attach (the sandbox never came up, or the guest's sshd was unreachable) | print, offer **Try again** |

**`4001` IS used, deliberately.** `terminal-over-https-design.md` assigns it the
exe-ssh meaning "the shell exited normally", and this bridge means exactly that
by it — one code, one meaning, on one server. The design's original claim that
`4001` was off-limits (and its `4401`/`4409`/`1011`/`1000` allocation) was
reversed during implementation; `1000` is never sent.

Close reasons are capped at 123 bytes by the protocol, so the real explanation
goes into the terminal stream, not the reason string.

## Live-session integration

The browser terminal joins the **existing** registry in `internal/sshgw`. A
second map would mean `mgr.CloseSandboxSessions` never hangs up browser
terminals on pause and a redeploy strands them. `internal/xterm` owns HTTP,
assets, and framing; `internal/sshgw` owns the dial, the PTY, and the
registration, behind:

```go
// package sshgw

// WindowSize is a PTY geometry. Rows and Cols, in that order, because
// RequestPty and WindowChange both take them that way and transposing them is
// the easiest mistake to make here.
type WindowSize struct{ Rows, Cols int }

// Terminal is the non-SSH end of an interactive session.
type Terminal interface {
	// Read yields client keystrokes; io.EOF closes the guest's stdin.
	// Write receives raw PTY output.
	io.ReadWriter
	// Control receives control-plane bytes: terminalRestore and the hang-up
	// goodbye. It is separate from Write so a bridge can frame them differently
	// and so a non-PTY consumer could drop them.
	Control() io.Writer
	// Resize yields geometry changes until the session ends.
	Resize() <-chan WindowSize
	// Close must be idempotent and MUST NOT BLOCK: it is called from hangUp's
	// goroutine while host.Manager holds its mutex, and the teardown path takes
	// that same mutex on the way out.
	Close() error
}

type AttachRequest struct {
	Caller  ctlops.Caller
	Sandbox string
	Term    string     // TERM for the guest PTY; "" means "xterm-256color"
	Size    WindowSize // the fit addon's first measurement
	Conn    Terminal
}

// AttachTerminal is the headless twin of the interactive SSH path: it calls
// ctlops.Attach (owner gate, then resume), registers with the live-session
// tracker BEFORE dialing so a racing pause still finds and closes the session,
// dials the guest with DialUpstream under a bounded context, opens a PTY, pumps
// bytes, and returns the guest's exit status. It defers ops.Touch(name) to
// session end, exactly as the SSH gateway does.
func (g *Gateway) AttachTerminal(ctx context.Context, req AttachRequest) (exitCode int, err error)
```

`internal/xterm` declares a two-method `Attacher` interface it satisfies with
`*sshgw.Gateway`, so it never imports the gateway's internals.

Three contracts the adapter must honour:

- **`isPTY` is unconditionally true.** `terminalRestore` is written only when it
  is set, and those escapes matter just as much in xterm.js as in a real
  terminal: the mouse-tracking and bracketed-paste modes are set by the *remote*
  TUI and live in the emulator, so killing a session from outside without them
  leaves a browser terminal spewing `35;24;36M` on every mouse move.
- **`Close` is `sync.Once` + `conn.CloseNow()`**, which the library documents as
  unblocking every goroutine on the connection. It must return immediately: the
  2 s `closeGrace` becomes a hang otherwise, and `CloseSandboxSessions` runs
  under the manager's mutex.
- **The session context is detached from the request** with an explicit cancel
  fired when the bridge exits, so nothing in the HTTP server's lifecycle
  management can cut a live shell, and the dial still gets its own bounded child
  context (`DialUpstream` retries every 250 ms until the context expires — an
  unbounded one is an infinite dial loop). What shipped is
  `context.Background()`, not the `context.WithoutCancel(r.Context())` written
  here: after a hijack the request context carries no value the bridge reads and
  is no longer a liveness signal, so inheriting from it would only be
  decorative.

## Idle policy — reversed during implementation

The design said the bridge would **not** call `Touch` at all. What shipped
touches on real client input, throttled to once a minute (`ws.go`'s `touch`),
and nowhere else: not on pings, not on keepalives, and deliberately not on
resizes, which a window manager fires with nobody at the keyboard. The reasoning
that survived is the same one — a forgotten tab must not pin a VM forever — but
the conclusion moved: merely being attached keeps nothing warm, while *typing*
is work, and treating a person mid-command as idle was the worse failure. What
keeps an unattended sandbox warm is still in-guest CPU and network, which the
reaper samples itself. When the reaper does pause an attached box, the terminal
gets `terminalRestore` plus the goodbye and closes with `4002`, and the page
offers Reconnect — which resumes it, because `EnsureRunning` refreshes
`LastActive`.

The hang-up goodbye's `reconnect with: ssh <name>.<domain>` line gains a
browser form (`https://<name>.xterm.<domain>`), selected by a `via` field on the
tracked session.

## The instrument strip — added after the terminal shipped

The header carries three live readings of the sandbox: a CPU sparkline, a RAM
capsule, and a mirrored network plot. Five decisions are worth recording.

**A route, not a WebSocket frame.** `GET /vitals` sits beside `/ws` rather than
adding a `{"type":"vitals"}` text frame to the bridge. The wire protocol above
is *published* — `openapi.json` documents it, and `internal/restapi` hands the
same `Bridge` to non-browser clients — so unsolicited frames would change a
contract every client reads in order to decorate one page's chrome. The route
also keeps reporting while the socket is between reconnects, which is exactly
when a person wants to know whether the machine is still doing anything.

**Raw counters out, rates derived in the page.** The response carries
`cpu_seconds`, the tap's `net_rx_bytes`/`net_tx_bytes` and `mem_used_mb`, plus
`at_ms` — never a rate. The server stays stateless (no per-viewer sample to keep
or expire), and the delta is computed in the one place that knows whether its
own two samples were contiguous, which the server cannot know for a tab that was
backgrounded for ten minutes. `at_ms` is the divisor rather than the browser's
clock, so request latency never shows up as a CPU spike. This is the same split
the user console already makes with `cpu_seconds`.

The payload also carries `repositories`, an object keyed by configured repo
slug, and `repo_status_at`. This is the gateway's latest guest-published git
survey rather than a filesystem read performed by the poll. The in-guest
`sparkbox status --json` response uses the same field names and resource
counters, so the xterm strip, people at a shell, and agents all consume the
same status vocabulary.

**It is strictly passive.** The handler resolves through `Get`, never
`EnsureRunning`, and never calls `Touch` — `TestVitalsNeverResumesOrTouches`
holds the line. A tab polls this once a second; if any of it counted as activity
the terminal would keep resurrecting a box the reaper is trying to put away, and
the page's deliberate refusal to auto-reconnect after a pause (`4002`) would be
pointless. Watching a meter is not work.

**`Config.Vitals` was the local manager, and is now the fleet.** The first
build read `mgr`: a balloon and a VMM process can only be asked of the machine
running them, so a sandbox placed on another node missed in the local manager's
maps and every reader answered `ok=false` — no special case, and the same
degradation both consoles had. That was correct and permanently blank, and the
further the fleet spreads the more of the platform's instrumentation goes dark
with it. *Which* machine to ask is a question the fleet answers, so the read is
routable after all; see "Vitals across the fleet" below. The response still
carries the ceilings, so "no reading", "paused", "that machine is not
answering" and "no `VitalsReader` wired" all render as one state instead of
four.

**Two scales, chosen per metric.** CPU is pinned to 0–100% (normalised by
vCPUs, so 100% is every core busy): autoscaling a percentage would make an idle
machine's noise look identical to a pegged one. Network autoscales to the
window's peak on a *square-root* scale, because traffic spans orders of
magnitude — an `apt-get` against a keystroke of SSH — and on a linear scale one
burst flattens the rest of the minute into the axis. The root reorders nothing
and the exact rate is printed beside the plot either way. Both directions share
one axis; direction is what distinguishes them, which is why the two rate
readouts sit in the same order as the halves they name.

Colour follows the rest of the chrome: recessive ink, crossing into `--amber`
and `--red` only at the 70/90 thresholds both consoles already use. Nothing is
encoded by colour alone — the height carries the value and the number beside it
carries it again — so the strip stays readable when colour means nothing.

### Condensing the bar — a second pass

The first build put label and value side by side and kept the status pill's
word, and the whole thing needed 900px. Four changes took that to ~305px for the
CPU cell alone, which is what makes the strip survive on a phone:

- **Each readout is a two-line stack**, label over value, the way a menu-bar
  meter does it. The label stops paying for horizontal room it was only using to
  sit beside a number.
- **The status pill became a lamp.** Its word cost ~65px of a 44px bar to say
  something the screen already says louder: every state that is *not* connected
  puts a full overlay over the terminal explaining itself. The text moved into
  the `title` and into an `sr-only` span under `role="status"` — a live region,
  so a screen reader is told when the connection changes. An `aria-label` would
  not be: a changed label is not announced, changed live-region content is.
- **The dividers became wells.** Each plot sits on a `--muted` rounded
  background; the wells group a plot with its numbers, so the hairline rules
  between cells could go. Grouping is now carried by spacing alone — 14px
  between cells against 6px inside one.
- **`header { min-width: 0 }`**, which is the whole mobile fix. The header is a
  grid item, and a grid item's automatic minimum size is its min-content width,
  so it refused to be narrower than its contents and pushed Reconnect off the
  right edge of a phone instead of ellipsizing the name. Note that
  `html, body { overflow: hidden }` hides this from the usual
  `scrollWidth > clientWidth` check — the overflow is clipped, not scrollable,
  so the bug is invisible to the obvious test and has to be caught by measuring
  the last child's right edge.

The strip then sheds cells one at a time rather than vanishing at a single
threshold — network at 980px, memory at 700px — ordered by value per pixel. The
CPU sparkline is the last thing standing, because "is this machine doing
anything" is the question the strip exists to answer. Verified down to 360px.

## The menu — a third pass at the bar

The bar carried three controls in a row: a turbo switch, `Clear` and
`Reconnect`. That spent a third of a 44px strip on things nobody presses in a
normal session, hid two of them below 560px (`hide-sm`) so a phone could not
reach them at all, and left a button reading **Reconnect** in permanent view of
a terminal that was plainly connected. They now live behind one hamburger, and
the room that buys pays for the rest of this section.

**Every item says what it does.** A 44px strip had space for the word "Clear"
and none at all to say that it clears the *window* and not the sandbox. A menu
row is two lines — the action, then the consequence — which is also what let the
turbo switch drop its tooltip: the sentence beside it now names the sizes
(`On — 8 vCPU and 24 GB, until this sandbox pauses`) rather than a factor, and
"2× resources" is only meaningful to somebody who already knows what 1× is on
this host. The switch itself keeps the physical push-button it has everywhere
else in the product; the point of that shape is that it is the same switch
wherever you meet it.

**The connection item is worded for the state it is in.** On a live shell it
reads *Start a new shell* and opens the same xterm URL in a new tab, preserving
the current PTY and scrollback. Only a disconnected page calls it *Reconnect*
and reconnects in place. `setStatus` paints it, so it cannot drift from the lamp
beside it. The overlay keeps its own button, which is the path anybody
disconnected actually takes.

**`ssh <name>@<host>`, copied.** The first row is the command that reaches the
same shell from a terminal, rendered in full so it can be read without being
copied, with the whole row as the copy button. `navigator.clipboard` is
unavailable outside a secure context — every `--proxy-tls=false` dev loop — so
there is a `document.execCommand` fallback behind it and a failure says
"select the command and copy it" rather than silently doing nothing.

**Three facts the page cannot derive, so `/vitals` carries them.** The response
gained `name`, `ssh` and `console`, and each is on the server side for a
reason:

- `name` — the host is `<name>-<subdomain>.<zone>`, ONE label (that is what the
  wildcard certificate covers), so the page's `hostname.split(".")[0]` yields
  `demo-xterm`. That is what the header and the turbo dialog used to say. The
  subdomain is configurable, so there is no suffix the page could safely strip
  either.
- `ssh` — only the server knows the ADVERTISED host and port. An edge DNAT
  means the port people type is not the port the gateway binds, and `main`
  already computes that pair for the login page and the `ctl` channel; it is
  threaded in rather than recomposed so the three cannot disagree.
- `console` — the user console's label is configurable, and a link to a
  hostname nothing serves is worse than no link. Same string the launch door is
  handed, computed once in `main`.

They are constants for the life of the page, so it applies them on the first
reading and skips the work thereafter. Absent means "no such thing on this
host", which the page renders by leaving the row out entirely — an empty `ssh`
string would draw "Copy the ssh command" over a blank line.

**The console link is the only link off this page, and it opens a tab of its
own.** The shell in the grid behind the menu is a live session; navigating away
from it in the same tab ends it.

Focus is the detail that makes the menu usable rather than merely present.
Opening it takes focus off the terminal, so closing it has to give focus back —
otherwise the next keystroke goes nowhere and the shell looks frozen. Escape
returns focus to the button that opened it (a keyboard user is still
navigating); every other route back returns it to the shell.

## Vitals across the fleet — added after the strip shipped

The strip shipped reading the local manager, which meant a sandbox on a fleet
node drew no meters at all. That was the honest answer for a build with no way
to ask another machine, and it was also the beginning of a pattern worth
refusing: every surface that instruments a sandbox goes blind the moment the
sandbox leaves the gateway, and the answer to "why is this empty" becomes "where
is your VM", which is exactly what the fleet exists to stop anyone having to
know.

**One reading, not three.** `host.Manager` grew `Vitals`, which fans the three
existing readers out concurrently and returns a `host.Vitals` of four pointers.
That struct — rather than the three `(value, ok)` methods — is what crosses the
machine boundary, because the alternative is three round trips a second per open
tab. Pointers all the way down, because a missing counter and a zero counter are
different facts at every layer: absent `cpu_seconds` means this machine has no
CPU stats for that sandbox; a present `0.0` means it has used none.

**A new nodelink verb, `sandbox.vitals`.** It is the only read in that catalogue
not served from the inventory cache, and it has to be. The inventory a node
pushes is a lifecycle picture — state, sizes, lifetime totals — refreshed when
something *happens*; these are instrument readings whose whole value is being
current to the second. Folding them into the inventory would either have every
node broadcast a CPU sample a second to a gateway with nobody watching, or make
the meters as stale as the last lifecycle event. It is also the only verb a
*viewer* drives rather than an operator, which is why it changes nothing on the
far machine and never touches `last_active`: passivity had to survive the hop,
not just the handler.

**`Fleet.Vitals` routes and does not fall back.** When the owning machine is
offline the answer is that machine's outage, never a reading from here. The
fallback would be the easy mistake and it is a cross-tenant one — every machine
mints its guests the same `172.30.x.y` addresses and the local manager holds a
*different* sandbox for any name it happens to share, so a helpful local answer
draws one person's CPU under another person's name with no error and no log
line. `TestVitalsRefuseRatherThanAnswerFromTheWrongMachine` holds it.

**The budget is the placement's, and it is stated once.** `webui.Probe.Vitals`
gives a local sandbox the 300ms every local probe gets and a remote one the
tunneled 2s, for the same reason the port probe already did: the remote question
costs a round trip before the balloon is even touched. Giving every reading the
remote budget lets one wedged local VMM stall a dashboard; giving every reading
the local one times out exactly the sandboxes this work exists to reach. Putting
it on `Probe` rather than in three handlers is what keeps the browser terminal
and both consoles from drifting on it — the same argument that put `Remote` and
`Public` there.

**Both consoles came along.** `console` and `userconsole` each held a
`*host.Manager` purely for the balloon and CPU reads, with a comment explaining
that those were not routable. They are now `SetVitals(flt)`, and the dashboards
show memory and CPU for every sandbox an owner has, wherever it landed. The
`SetVitals` shape matches `SetSandboxes`/`SetDialer`: a single-machine build
constructs with the manager and never calls the setter.

**What still does not travel.** Bandwidth accounting stays with the egress plane
(`net.usage`), which is metered per machine by the daemon in front of its own
taps; this verb reports the tap counters the guest sees, not sluice's ledger.
And a paused sandbox reports nothing from anywhere, which is not a fleet
property at all — there is no VMM process to ask.

---

# Part 5 — the edge

## Suffix dispatch

`proxy.subdomainOf` strips `.<domain>` and returns everything before it as one
string, so `demo.xterm.catnip.sh` arrives as `sub == "demo.xterm"` and misses the
exact-keyed `reserved` map entirely. The fix:

```go
// package proxy

// SetReservedSuffix registers h for every "<name>.<label>" subdomain. The
// sandbox name is put on the request context for the handler to read with
// SuffixName.
func (s *Server) SetReservedSuffix(label string, h http.Handler)

// SuffixName returns the "<name>" part matched by SetReservedSuffix.
func SuffixName(ctx context.Context) (string, bool)
```

Dispatch is inserted in `ServeHTTP` **immediately after the exact-`reserved`
miss and before `store.GetBySubdomain`**, using
`strings.CutSuffix(sub, "."+label)` with a non-empty remainder. The ordering is
a security invariant, not a preference: `routes.ValidSubdomain` permits dotted
subdomains (`api.myvm` is an advertised use case) and the loopback control API
accepts an arbitrary one, so a route row literally named `foo.xterm` is
creatable today — if route lookup ran first, one user could shadow another
user's terminal host. `TestReservedSubdomainBeatsRouteLookup` is the existing
guard for this ordering; extend it with a sibling that plants a hostile
`foo.xterm` route row and asserts the suffix handler still wins.

## Certificate

`cmd/sparkbox/tls.go` mints the Cloudflare wildcard from a literal
`[]string{p.domain, "*." + p.domain}`. That is single-label and does **not**
cover `<name>.xterm.<domain>`; without the extra SAN the browser fails inside the
TLS handshake, showing a full-page certificate interstitial with no server-side
log line — which looks exactly like a DNS bug.

Issuance is **two-phase**:

1. `ManageSync(ctx, []string{domain, "*."+domain})` — unchanged, still fatal.
2. `ManageSync(ctx, []string{"*.xterm."+domain})` — **non-fatal**. On failure it
   logs `WARN browser terminals will not be reachable over https` with the name
   and the ACME error, and carries on.

`ManageSync` blocks and its error propagates out of `serve()`; a zone whose
DNS-01 cannot validate the extra name would otherwise turn a working deployment
into a boot loop. `setupProxyTLS` returns the names it actually obtained and
`serve()` logs them as `tls certificates managed`, which is the only place the
outcome is visible. The `autocert` path needs no change at all — its
`HostPolicy` already accepts any depth under the zone, so it issues
`<name>.xterm.<domain>` on first SNI and reports no name list.

**As built, two details differ from the plan above.** The handler is mounted
before TLS is configured, so a failed second order does **not** skip
`SetReservedSuffix` — the route stays live and fails in the handshake instead of
404ing. And `Capabilities.Terminal` follows the `--xterm-subdomain` flag, not
the certificate: a tailnet or plaintext edge serves terminals perfectly well
with no ACME order at all, so deriving the capability from issuance would lie in
the other direction. The honest signal for "is my terminal presentable over
https" is the `tls certificates managed` line.

## DNS

Three deployment shapes, three answers:

- **Tailnet / `dnsedge`:** nothing to do. One mux handler owns the whole zone
  subtree and `a.b.c.<domain>` is already a passing test.
- **Public direct-IP edge:** publish `*.xterm` once at startup as a grey-cloud
  `libdns.Address` at the edge address, in `cmd/sparkbox/wildcarddns.go`, gated
  on `CLOUDFLARE_API_TOKEN` being present and **independent of `--subnet6`**.
  As built it needs `--edge-v4` too, because that flag is the only place the
  edge's address is stated; without it there is nothing to point the record at
  and it logs a `WARN` naming the missing flag. This is a one-shot publish with
  **no Remove path** — it must never go through `frontdoor.Publisher`, whose
  per-sandbox `Remove` would delete the zone-wide wildcard when a sandbox is
  destroyed.
- **Cloudflare Tunnel:** out-of-band, a docs change only. The record must be a
  *proxied* CNAME to `<uuid>.cfargotunnel.com`, sparkbox does not know the tunnel
  UUID, and `docs/deploy-dgx.md` explicitly forbids giving sparkbox a
  `CLOUDFLARE_API_TOKEN` in that mode. Documented as
  `cloudflared tunnel route dns <t> '*.xterm.<domain>'` plus an ingress rule.
  There is one code path that assumes a token — `PublishWildcard` reads the zone
  first and, finding a CNAME already at the name, logs whose it is and returns
  without writing. That is belt and braces for the deployment where a token gets
  added later for some other reason (the live DGX box has one, for its own
  wildcard certificate); withholding the token is still the rule.

Never publish a per-sandbox record under the `xterm` label. RFC 4592 says an
explicit record at `foo.xterm.<domain>` disables the `*.xterm` wildcard for that
name across **all** types — the same trap the per-name AAAA already sprang.

## Reserved names

`"xterm"` and `"api"` are added to **both** `host.reservedNames`
(`internal/host/manager.go`) and `users.reserved`
(`internal/users/store.go`). Those two lists are deliberate duplicates by
convention; updating one leaves the reservation half-enforced. `sshgw.ReservedUsers`
(`new`/`ctl`/`signup`) is **not** touched — those are SSH doors and each gets a
front-door DNS record at startup.

Mounting logs the same warn-don't-fail collision message the user console gets
when a reserved subdomain already names a sandbox or route.

## Flags

Joining the existing subdomain family (`--console-subdomain`,
`--login-subdomain`, `--user-console-subdomain`, `--oidc-subdomain`):

| Flag | Default | Effect |
|---|---|---|
| `--api-subdomain` | `"api"` | serves the REST API and `/docs` at `<this>.<domain>`; empty disables it |
| `--xterm-subdomain` | `"xterm"` | serves browser terminals at `<name>.<this>.<domain>`; empty disables them, and so does `--proxy-addr ""` |

`--edge-v4` gained a second job in the same change: it is the address the
`*.xterm` wildcard is published at, so it is now a co-requisite of
`CLOUDFLARE_API_TOKEN` for that record rather than only a front-door concern.

`--api-addr`'s usage string is unchanged; the warning went into `internal/api`'s
package doc instead, where the next person to add an endpoint will read it:
*legacy, takes the owner from a request field, checks nothing, must never be
mounted on the edge.*

Both are wired inside the existing `proxy.New` … `http.Server` window in
`serve()`. Remember that systemd `ExecStart` bundles must reference flag
variables **unbraced** (`$XTERM_FLAGS`, never `${XTERM_FLAGS}`): a braced empty
variable expands to one empty argument that terminates Go's flag parsing and
silently discards every flag after it.

---

# Part 6 — the regression net

There is no CI that runs `go test`, so local verification is the only
verification. These must pass **with zero edits**, and they are the proof the
refactor is behaviour-preserving:

- `e2e_test.go` — `TestControlWhoamiAndKeys`, `TestOnlyOperatorsCanInvite`,
  `TestOwnershipAndAuth`, `TestNewSandboxOnConnect` drive the real `ctl` surface
  over real SSH and assert its output text.
- `internal/sshgw/resize_test.go` (all three, including
  `TestControlUsageListsEveryCommand`), `exec_test.go`, `frontdoor_test.go`, all
  eleven of `livesessions_test.go`, `tags_test.go` minus the one relocated
  function.
- `proxy_test.go`, `proxy_auth_test.go`, `proxy_stream_test.go`.
- All of `internal/userconsole/console_test.go`.

New tests that are themselves contracts:

- **`internal/ctlops/ownership_test.go`** — an explicit table of every method
  that takes a sandbox name, snapshot name, or schedule id, driven as `mallory`
  against `alice`'s object, asserting `KindNotFound`, a `Msg` byte-identical to
  the genuinely-missing case, and that the fake recorded **zero** state-changing
  calls. A reflection pass over `reflect.TypeOf(&Ops{})`'s method set asserts the
  table is complete, so a new method must be either covered or explicitly
  exempted with a comment.
- **`internal/ctlops/create_test.go`** — `calls == ["SetTags","Create"]` for
  `Create` and `["SetTags","Fork"]` for `Fork`, plus a compensating
  `SetTags(name, owner, nil)` when the create fails. The fake `Tagger` and fake
  `Sandboxes` share one `calls []string` recorder; that shared slice is the
  entire mechanism.
- **`internal/sshgw/control_golden_test.go`** — **land this before touching a
  line of sshgw.** A table driving every `ctl` command and every failure mode
  through a real in-process SSH client against the mock stack, capturing stdout,
  stderr and exit code into a golden file. Refactor until it passes
  byte-for-byte unchanged.
- **`internal/restapi/server_test.go`** — `TestEveryEndpointRequiresAuth` (401 +
  `no-store` on every route), `TestCrossOwnerIs404` (every owner-scoped route as
  `mallory`, then assert alice's sandbox/snapshot/schedule untouched),
  `TestMutationCSRFGate`, `TestPreferAsyncEscalates`, `TestIdempotencyKeyReplays`.
- **`internal/xterm/xterm_test.go`** — the origin table, as built: correct
  Origin → accept; another sandbox's page, another terminal, the user console,
  an off-zone attacker, a suffix trick (`…hivemind.tools.evil.example`) and a
  scheme downgrade → 403; no Origin with only a cookie → 403; no Origin with a
  Bearer → accept. Note the shipped rule refuses a **mismatched** Origin even
  with a Bearer token, which the design's version would have let through. Every
  refusal additionally asserts the mock driver recorded no resume, and a
  cross-owner request is a 404 before any VM work.
- ~~`internal/sshgw/attach_test.go`~~ — not written; `AttachTerminal` does not
  exist. The equivalent guarantee — a tracked terminal receives
  `terminalRestore` and is closed promptly by `CloseSandboxSessions`, without
  blocking on a wedged `Write` — is covered by `livesessions_test.go` for the
  registry and `internal/xterm/bridge_test.go` for the browser end.

Verification command for every agent, on their own package only:

```
cd tools/sparkbox && go build ./... && go vet ./internal/<pkg>/... && go test ./internal/<pkg>/...
```

`gofmt -l` was **not** clean at the branch point (`internal/userconsole/console.go`
and `internal/domainmeta/domainmeta_test.go` were committed unformatted, comment
alignment only). Both were formatted as part of this landing, so `gofmt -l .` is
clean now and can be used as a plain gate.

---

# Part 7 — file ownership

Six owners, one owner per file. No file appears twice. Nobody edits a file they
do not own, and nobody runs `go get`, `go mod tidy`, or edits `go.mod`/`go.sum` —
the **INTEGRATOR** promotes `github.com/coder/websocket` from indirect.

*This table is the build plan, kept as a record of how the work was split. Two
of its rows were never written — `internal/sshgw/attach.go` and its test, for
the reason given at the top of this document — and the implementers added files
it does not list, notably `internal/ctlops/types.go`, `internal/xterm/pty.go`,
and the reserved-name and wildcard-DNS tests under `internal/host`,
`internal/users` and `internal/frontdoor`. Read the tree, not the table.*

| File | Owner | Action |
|---|---|---|
| `internal/ctlops/ops.go` | CORE | new — `Ops`, `Config`, `New`, `Close`, `Caller`, `Capabilities`, all eight dependency interfaces, budgets, the internal owner gate and `withBudget` |
| `internal/ctlops/errors.go` | CORE | new — `Kind`, `Error`, `AsError`, `IsKind`, `NotFound`, `Invalid`, `Disabled`, `Denied`, `Fail`, `ExitCode`, `HTTPStatus` |
| `internal/ctlops/sandbox.go` | CORE | new — `Get`, `List`, `Create`, `Pause`, `Resume`, `Archive`, `Resize`, `Reboot`, `Rename`, `Destroy`, `SetPinned`, `Tags`, `SetTags`, `Attach`, `Touch` |
| `internal/ctlops/template.go` | CORE | new — `ListSnapshots`, `CreateSnapshot`, `DeleteSnapshot`, `Fork` |
| `internal/ctlops/schedule.go` | CORE | new — `ListSchedules`, `AddSchedule`, `DeleteSchedule` |
| `internal/ctlops/account.go` | CORE | new — whoami, keys, GitHub, passkeys, email, session-token, invite |
| `internal/ctlops/share.go` | CORE | new — `Visibility`, `SetVisibility` |
| `internal/ctlops/jobs.go` | CORE | new — `Job`, `Go`, `Await`, `Job`, `ListJobs`, `CancelJob`, the retention reaper |
| `internal/ctlops/parse.go` | CORE | new — `ParseSize`, `NormalizeTags`, the constants |
| `internal/ctlops/names.go` | CORE | new — moved verbatim from `internal/sshgw/names.go`, plus `GenerateName` |
| `internal/ctlops/fakes_test.go` | CORE | new — the in-memory fakes with the shared `calls` recorder |
| `internal/ctlops/ownership_test.go` | CORE | new — the masking-invariant table + reflection completeness check |
| `internal/ctlops/create_test.go` | CORE | new — tags-before-create ordering and rollback |
| `internal/ctlops/errors_test.go` | CORE | new — the Kind → exit/status table; `host.LimitError` classification |
| `internal/ctlops/jobs_test.go` | CORE | new — dedup, early `Await`, cancellation, retention eviction |
| `internal/sshgw/control.go` | SSHGW | rewrite each case as parse → call → format; `controlUsage` byte-unchanged |
| `internal/sshgw/control_auth.go` | SSHGW | same; `session-token` keeps token-on-stdout / notes-on-stderr |
| `internal/sshgw/gateway.go` | SSHGW | `Gateway` gains `ops`; `GatewayOptions.Ops` optional (nil builds one from the existing fields, so existing tests compile untouched); `new@` collapses to `parseTags` → `ops.Create`; `failStart` renders `*ctlops.Error` |
| `internal/sshgw/tags.go` | SSHGW | `applyTags` deleted; `dedupeTags`/`normalizeTags` delegate to `ctlops.NormalizeTags`; `parseTags` stays (it is CLI syntax) |
| `internal/sshgw/names.go` | SSHGW | **deleted** (content moved to CORE) |
| `internal/sshgw/attach.go` | SSHGW | new — `Terminal`, `WindowSize`, `AttachRequest`, `AttachTerminal`, the `sessionConn` adapter |
| `internal/sshgw/livesessions.go` | SSHGW | `reconnectHint` gains a browser form, selected by a `via` field |
| `internal/sshgw/attach_test.go` | SSHGW | new — mock-driver attach round-trip; non-blocking hang-up |
| `internal/sshgw/control_golden_test.go` | SSHGW | new — **land first**; the byte-for-byte ctl golden file |
| `internal/sshgw/tags_test.go` | SSHGW | remove only `TestApplyTagsWithoutStoreErrors` (it becomes a CORE test) |
| `internal/sshgw/resize_test.go` | SSHGW | `parseSizeMB` becomes a one-line wrapper over `ctlops.ParseSize` so this file needs no edit beyond adding `passkey` to the usage-completeness list |
| `internal/restapi/server.go` | RESTAPI | new — `Handler`, `New`, the mux, `require`/`mutate`, `writeJSON`, `writeErr`, `Prefer` parsing |
| `internal/restapi/sandboxes.go` | RESTAPI | new |
| `internal/restapi/snapshots.go` | RESTAPI | new |
| `internal/restapi/schedules.go` | RESTAPI | new |
| `internal/restapi/account.go` | RESTAPI | new |
| `internal/restapi/jobs.go` | RESTAPI | new |
| `internal/restapi/async.go` | RESTAPI | new — `runSync(w, r, op, ref, budget, fn)`, the sync-first/202 escalation |
| `internal/restapi/idempotency.go` | RESTAPI | new — the 24 h `Idempotency-Key` replay cache |
| `internal/restapi/terminal.go` | RESTAPI | new — `GET /v1/sandboxes/{name}/terminal`, delegating to `xterm.Bridge` |
| `internal/restapi/openapi.json` | RESTAPI | new — hand-authored 3.1, canonical |
| `internal/restapi/openapi.go` | RESTAPI | new — `//go:embed`, validate at init, serve `/openapi.json` |
| `internal/restapi/yaml.go` | RESTAPI | new — the deterministic JSON→YAML emitter for `/openapi.yaml` |
| `internal/restapi/docs.html` | RESTAPI | new — the `/docs` page, `webui` markers |
| `internal/restapi/server_test.go` | RESTAPI | new |
| `internal/restapi/openapi_test.go` | RESTAPI | new — the spec-honesty bijection |
| `internal/xterm/xterm.go` | XTERM | new — `Handler`, `New`, the `Attacher` interface, the mux, CSP, host→name |
| `internal/xterm/ws.go` | XTERM | new — origin gate, upgrade, `Bridge` (exported for RESTAPI), framing, close codes |
| `internal/xterm/conn.go` | XTERM | new — the `sshgw.Terminal` adapter; `sync.Once` + `CloseNow` |
| `internal/xterm/assets.go` | XTERM | new — `//go:embed assets/*` and the immutable file server |
| `internal/xterm/index.html` | XTERM | new — the terminal page |
| `internal/xterm/xterm_test.go` | XTERM | new — auth, the origin table, cross-owner-without-resume, asset content types |
| `internal/xterm/bridge_test.go` | XTERM | new — framing, resize clamping, oversized paste, close codes, over `io.Pipe` with no VM |
| `internal/xterm/assets/*` | XTERM | vendored, **do not modify** |
| `internal/proxy/proxy.go` | EDGE | `reservedSuffix`, `SetReservedSuffix`, `SuffixName`, the dispatch insertion |
| `proxy_test.go` | EDGE | extend `TestReservedSubdomainBeatsRouteLookup` with the hostile `foo.xterm` route row |
| `cmd/sparkbox/tls.go` | EDGE | two-phase issuance; report the obtained wildcards |
| `cmd/sparkbox/wildcarddns.go` | EDGE | new — the one-shot `*.xterm` publish, no Remove |
| `internal/host/manager.go` | EDGE | `reservedNames` += `"xterm"`, `"api"` |
| `internal/users/store.go` | EDGE | `reserved` += `"xterm"`, `"api"` |
| `cmd/sparkbox/main.go` | INTEGRATOR | the two flags, `ctlops.New`, wiring all three handlers, collision warnings |
| `go.mod`, `go.sum` | INTEGRATOR | promote `coder/websocket` to direct |
| `internal/api/server.go` | INTEGRATOR | one package-doc line: legacy, loopback-only, must not be mounted on the edge |
| `hack/preview-console.py` | INTEGRATOR | parameterize the hard-coded `INDEX` so `/docs` and the terminal page get the same preview loop |
| `docs/rest-api-and-xterm-design.md` | INTEGRATOR | this file; status updates as milestones land |
| `docs/terminal-over-https-design.md` | INTEGRATOR | mark M3 done; point at the browser terminal; note `4001` and `/terminal/…` stay reserved |
| `docs/deploy-dns.md`, `docs/deploy-dgx.md` | INTEGRATOR | the `*.xterm` wildcard, the tunnel-mode `cloudflared tunnel route dns` incantation |
| `README.md`, `docs/getting-started.md` | INTEGRATOR | the two new flags; a REST quickstart via `ssh ctl@<domain> session-token` |

**Not touched by anyone:** `internal/userconsole/*`, `internal/edgeauth/*`,
`internal/secrets/*`.

---

# Part 8 — the three risks worth naming

**1. The `ctl` surface drifts by a byte and nobody notices.** No CI runs
`go test`; the messages are hand-rolled in a dozen places; the exit codes are a
contract other tooling may depend on (`$(ssh ctl@host session-token)` is real
usage). The mitigation is ordering: `control_golden_test.go` lands **before** any
sshgw edit, and the `Verbatim` and `Exit` fields on `ctlops.Error` exist
precisely so it can pass — they are not generality, they are two facts that
cannot be inferred from a `Kind`.

**2. Widening the certificate SAN set boot-loops a live host.** `ManageSync`
blocks and its error kills `serve()`; the user-visible failure of the *other*
branch is a full-page TLS interstitial with no server log line. Two-phase,
non-fatal issuance means the feature can be absent without the host being down.
The second half of the mitigation moved: `Capabilities.Terminal` follows the
flag rather than the obtained names, because a tailnet or plaintext edge serves
terminals with no ACME order at all. So sparkbox *can* advertise a URL whose
certificate it failed to get — the compensating control is the
`tls certificates managed` log line, which is the only server-side evidence that
exists. See the note in Part 5.

**3. Cross-tenant leakage through three new doors at once.**
`secrets.Store.SetTags`/`TagsFor` check nothing, so a ctlops method that forwards
a name without gating first is a silent cross-tenant read/write;
`routes.ValidSubdomain` permits `foo.xterm`, so dispatch ordering decides whether
one user can shadow another's terminal host; and a WebSocket handshake with no
origin check is a one-click authenticated shell for any guest app on the zone.
The mitigations are structural rather than procedural: one internal owner gate
and one `NotFound` constructor, enforced by a reflection-complete ownership
table; suffix dispatch inserted before `store.GetBySubdomain` with a hostile
route row in the test; and the credential-keyed origin gate running before
`websocket.Accept`, with a nine-case table asserting each rejection.
