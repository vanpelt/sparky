package ctlops

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

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

	// kindCount is the sentinel a new Kind is appended in front of. It exists so
	// a test can assert that kindNames names every Kind: a Kind added to this
	// block and forgotten in the table would otherwise stringify as "internal"
	// and cross a node link as a 500, which is precisely the failure the two
	// hand-maintained tables used to allow.
	kindCount
)

// kindNames is the ONE table of Kind's wire form. Kind.String() reads it and
// ParseKind inverts it, so the two directions cannot disagree about a Kind the
// way two hand-written switches could.
//
// These tokens are a released wire format: rename one and every peer running an
// older build classifies that failure as internal. Add, never rename.
var kindNames = [...]string{
	KindInternal: "internal",
	KindInvalid:  "invalid",
	KindNotFound: "not_found",
	KindDenied:   "denied",
	KindConflict: "conflict",
	KindDisabled: "disabled",
	KindLimit:    "limit",
	KindCapacity: "capacity",
	KindQuota:    "quota",
	KindUpstream: "upstream",
}

// kindByName is kindNames inverted once at package load rather than scanned per
// call: FromWire runs on every failure that crosses a link.
var kindByName = func() map[string]Kind {
	m := make(map[string]Kind, len(kindNames))
	for i, name := range kindNames {
		if name != "" {
			m[name] = Kind(i)
		}
	}
	return m
}()

// String is total: a Kind with no token — one appended to the iota block and
// left out of the table — reads as "internal", which is how every other
// unclassifiable failure reads.
func (k Kind) String() string {
	if k < 0 || int(k) >= len(kindNames) || kindNames[k] == "" {
		return kindNames[KindInternal]
	}
	return kindNames[k]
}

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
//
// The override is honoured only inside [1, 255]. FromWire already bounds what a
// node can put there, but this is the accessor every caller of a *Error goes
// through, wherever that error came from, so pinning the invariant here is what
// makes it true for an error built by some future in-process path as well —
// rather than true only for the one boundary somebody remembered. Outside the
// window the Kind decides, which is the same answer as an absent override.
func (e *Error) ExitCode() int {
	if e.Exit >= 1 && e.Exit <= 255 {
		return e.Exit
	}
	if e.Kind == KindInvalid {
		return 2
	}
	return 1
}

// statusClientClosed is nginx's 499: the request was abandoned before we could
// answer it. Go has no constant for it and the REST edge writes no body, but
// distinguishing it from a genuine 500 is what keeps a hung-up browser out of
// the error budget.
const statusClientClosed = 499

// HTTPStatus is the REST edge's contract in one place.
//
// The override is honoured only inside the error range, for the same reason
// ExitCode bounds its own: this is what restapi.fail hands to WriteHeader, and
// WriteHeader panics outside [100, 999] — a killed request — while a 2xx or a
// 3xx inside it is worse than a panic, because it makes a failure read as a
// success to every API client. A status this method cannot honour falls through
// to the Kind, which is the classification the caller actually made.
func (e *Error) HTTPStatus() int {
	if e.Status >= 400 && e.Status <= 599 {
		return e.Status
	}
	switch e.Kind {
	case KindInvalid:
		return http.StatusBadRequest
	case KindNotFound:
		return http.StatusNotFound
	case KindDenied:
		return http.StatusForbidden
	case KindConflict:
		return http.StatusConflict
	case KindDisabled:
		return http.StatusNotImplemented
	case KindLimit:
		return http.StatusTooManyRequests
	case KindCapacity:
		return http.StatusServiceUnavailable
	case KindQuota:
		return http.StatusInsufficientStorage
	case KindUpstream:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// notFoundMsg is the masked answer, one sentence per object kind. These strings
// are the ctl@ wire format: `no sandbox named %q` and `no schedule %q` are what
// the shipped CLI prints today, and the whole point of routing every existence
// and ownership failure through this one table is that no future caller can
// invent a second, distinguishable phrasing.
var notFoundMsg = map[string]string{
	"sandbox":  "no sandbox named %q",
	"snapshot": "no snapshot named %q",
	"schedule": "no schedule %q",
	"key":      "no key %s on this account",
	"passkey":  "no passkey matches %q — see `passkey list`",
	"route":    "no route %q",
	"job":      "no job %q",
	"node":     "no node named %q",
	// Keyed on the fingerprint, so it cannot borrow the sentence above: an
	// operator who mistypes a fingerprint and reads `no node named "SHA256:…"`
	// will go looking for a node whose *name* is a fingerprint.
	"node_fp": "no node in this fleet holds the key %s",
}

// NotFound is the ONE constructor for the masked answer. Existence and
// ownership share it, from a single line of code per object kind, so there is
// no second path on which a distinguishing 403 could appear.
//
// kind is "sandbox" | "snapshot" | "schedule" | "key" | "passkey" | "route" |
// "job" | "node" — the keys of notFoundMsg. An unlisted kind still works, on a
// generic phrasing, so a new object kind cannot accidentally take a second
// code path; adding it to the table is what buys it the wire wording.
func NotFound(op, kind, name string) *Error {
	format, ok := notFoundMsg[kind]
	if !ok {
		format = "no " + kind + " %q"
	}
	return &Error{
		Kind:     KindNotFound,
		Op:       op,
		Code:     kind + "_not_found",
		Msg:      fmt.Sprintf(format, name),
		Verbatim: true,
	}
}

// Invalid reports a malformed invocation: exit 2 over SSH, 400 over HTTP.
func Invalid(op, code, format string, a ...any) *Error {
	return &Error{Kind: KindInvalid, Op: op, Code: code, Msg: fmt.Sprintf(format, a...), Verbatim: true}
}

// Disabled reports a feature this host has not configured. The sentence is
// passed in whole because these are the exact strings ctl@ has always printed
// ("platform scheduling isn't enabled on this host.") and they are the only
// documentation some operators will read.
func Disabled(op, sentence string) *Error {
	return &Error{Kind: KindDisabled, Op: op, Code: codeFor(op, "disabled"), Msg: sentence, Verbatim: true}
}

// Denied reports an authenticated caller who may not do this.
func Denied(op, code, sentence string) *Error {
	return &Error{Kind: KindDenied, Op: op, Code: code, Msg: sentence, Verbatim: true}
}

// Fail classifies err via AsError and stamps op on it. It is the catch-all for
// store and driver faults, and the result is deliberately NOT Verbatim: over
// SSH it renders as `sparkbox: <op> failed: <msg>`, which is what fail() does
// today.
func Fail(op string, err error) *Error { return AsError(op, err) }

// codeFor derives a stable token from an op when the caller had no better one,
// so a KindDisabled from `schedule.add` is `schedule_disabled` rather than a
// bare `disabled` that no client can switch on.
func codeFor(op, suffix string) string {
	base := op
	for i := 0; i < len(base); i++ {
		if base[i] == '.' || base[i] == '-' {
			base = base[:i]
			break
		}
	}
	if base == "" {
		return suffix
	}
	return base + "_" + suffix
}

// AsError classifies anything into an *Error, synthesising a KindInternal one
// for a stray error so no transport has to nil-check. The recognised cases are
// exactly the ones a user can act on: the three admission errors the manager
// raises (including *host.DiskQuotaError, which the SSH path silently renders
// as a generic failure today), the users-store sentinels, and a canceled
// request.
func AsError(op string, err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		// Already classified. Stamp the op only if nobody has — an inner
		// operation's name is more specific than its caller's.
		if e.Op == "" {
			e.Op = op
		}
		return e
	}

	var limit *host.LimitError
	if errors.As(err, &limit) {
		return &Error{
			Kind: KindLimit, Op: op, Code: "running_limit",
			Msg: fmt.Sprintf("you already have %d running sandboxes (max %d): %s",
				len(limit.Running), limit.Max, joinNames(limit.Running)),
			Hint:     "Pause one to free a slot.",
			Details:  map[string]any{"running": limit.Running, "max": limit.Max},
			Verbatim: true,
			Err:      err,
		}
	}
	var capacity *host.CapacityError
	if errors.As(err, &capacity) {
		if capacity.Owner != "" {
			return &Error{
				Kind: KindCapacity, Op: op, Code: "owner_memory_pool_full",
				Msg: fmt.Sprintf("your memory pool is full (%d MB running + %d MB requested exceeds %d MB)",
					capacity.UsedMB, capacity.RequestedMB, capacity.BudgetMB),
				Hint: "Pause a sandbox to return memory to your pool.",
				Details: map[string]any{
					"owner": capacity.Owner, "used_mb": capacity.UsedMB,
					"requested_mb": capacity.RequestedMB, "pool_mb": capacity.BudgetMB,
				},
				Verbatim: true,
				Err:      err,
			}
		}
		return &Error{
			Kind: KindCapacity, Op: op, Code: "host_at_capacity",
			Msg: fmt.Sprintf("host is at capacity (%d/%d MB allocated)",
				capacity.UsedMB, capacity.BudgetMB),
			Hint: "Try again shortly, or pause a sandbox.",
			Details: map[string]any{
				"used_mb": capacity.UsedMB, "requested_mb": capacity.RequestedMB,
				"budget_mb": capacity.BudgetMB,
			},
			Verbatim: true,
			Err:      err,
		}
	}
	var quota *host.DiskQuotaError
	if errors.As(err, &quota) {
		return &Error{
			Kind: KindQuota, Op: op, Code: "disk_pool_full",
			Msg: fmt.Sprintf("your disk pool is full (%d MB used + %d MB requested exceeds %d MB)",
				quota.UsedMB, quota.RequestedMB, quota.PoolMB),
			Hint: "Archive or delete a sandbox to reclaim pool space.",
			Details: map[string]any{
				"used_mb": quota.UsedMB, "requested_mb": quota.RequestedMB,
				"pool_mb": quota.PoolMB,
			},
			Verbatim: true,
			Err:      err,
		}
	}

	// The three shapes rename raises alongside NameError. Verbatim stays false on
	// all of them so the SSH channel keeps printing `<op> failed: <msg>`, which
	// is exactly what it printed when these were bare fmt.Errorf values; only
	// the HTTP status and the log level change.
	var missing *host.MissingError
	if errors.As(err, &missing) {
		return &Error{Kind: KindNotFound, Op: op, Code: missing.Noun + "_not_found",
			Msg: err.Error(), Err: err}
	}
	var state *host.StateError
	if errors.As(err, &state) {
		// Exit 1 for the same reason NameError pins it: KindConflict already
		// exits 1, but saying so here means a future Kind change cannot silently
		// alter a released exit code.
		return &Error{Kind: KindConflict, Op: op, Code: state.Code,
			Msg: err.Error(), Exit: 1, Err: err}
	}
	var off *host.DisabledError
	if errors.As(err, &off) {
		return &Error{Kind: KindDisabled, Op: op, Code: off.Code, Msg: err.Error(), Err: err}
	}

	var name *host.NameError
	if errors.As(err, &name) {
		// A refused name is the caller's mistake, so it must not be a 500 the
		// client is invited to retry. The SSH side is held where it shipped:
		// Verbatim stays false so fail() still wraps it, and Exit is pinned to
		// 1 because KindInvalid would otherwise change a released exit code.
		e := &Error{Kind: KindConflict, Op: op, Code: "name_taken", Msg: err.Error(), Exit: 1, Err: err}
		switch name.Problem {
		case host.NameInvalid:
			e.Kind, e.Code = KindInvalid, "invalid_name"
		case host.NameReserved:
			e.Code = "name_reserved"
		}
		return e
	}

	switch {
	case errors.Is(err, users.ErrKeyLinked):
		return &Error{Kind: KindConflict, Op: op, Code: "key_linked_elsewhere",
			Msg: err.Error(), Verbatim: true, Err: err}
	case errors.Is(err, users.ErrLastKey):
		return &Error{Kind: KindConflict, Op: op, Code: "last_key",
			Msg: err.Error(), Verbatim: true, Err: err}
	case errors.Is(err, users.ErrNoSuchPasskey):
		return &Error{Kind: KindNotFound, Op: op, Code: "passkey_not_found",
			Msg: err.Error(), Verbatim: true, Err: err}
	case errors.Is(err, users.ErrAmbiguousPasskey):
		return &Error{Kind: KindConflict, Op: op, Code: "passkey_ambiguous",
			Msg: err.Error(), Verbatim: true, Err: err}
	case errors.Is(err, schedule.ErrNotFound):
		return &Error{Kind: KindNotFound, Op: op, Code: "schedule_not_found",
			Msg: err.Error(), Verbatim: true, Err: err}
	case errors.Is(err, context.Canceled):
		// The caller hung up. Not a fault, and the REST edge writes no body for
		// it — 499 is there so it is distinguishable from a real 500 in logs.
		return &Error{Kind: KindInternal, Op: op, Code: "canceled",
			Msg: "the request was canceled", Status: statusClientClosed, Err: err}
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Kind: KindInternal, Op: op, Code: "timeout",
			Msg: "the operation ran out of time", Status: http.StatusGatewayTimeout, Err: err}
	}

	return &Error{Kind: KindInternal, Op: op, Code: "internal", Msg: err.Error(), Err: err}
}

// IsKind is the predicate both edges use instead of comparing sentinels.
func IsKind(err error, k Kind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == k
}

// CodeNodeUnreachable is the stable machine token for "the machine holding
// this is not answering". internal/fleet mints the errors that carry it; it is
// declared HERE, with the rest of the taxonomy, because a Code is part of the
// contract a client switches on — it is in every JSON error body — and because
// the surfaces that have to tell a node outage from a guest fault are edges
// (internal/proxy, internal/xterm) that must not have to import the router to
// ask. A Code and not a Kind for the reason fleet.Unreachable spells out:
// KindCapacity already means exit 1 and HTTP 503, and a new Kind would mean
// editing the embedded OpenAPI enum that is parsed at package init.
const CodeNodeUnreachable = "node_unreachable"

// IsNodeUnreachable reports whether err is (or wraps) a node outage — the
// machine holding a sandbox is not answering, so nothing could be tried.
//
// The distinction it draws at an edge is the difference between two answers a
// user reads very differently: "your app is not listening on that port" (the
// sandbox is fine, the guest is not serving) and "the machine your sandbox
// lives on is offline" (nothing about the sandbox is wrong and there is
// nothing to fix).
func IsNodeUnreachable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == CodeNodeUnreachable
}

// MachineNamed reports whether err's sentence is about a particular machine,
// and which one.
//
// It is Details["node"], asked as a question rather than read as a map, so that
// the one decision it feeds — may this reader be told which machine? — is a
// call a reader can find and grep for. Every error in this tree whose Msg names
// a machine also carries the name in Details, and that is the contract this
// depends on: a node outage, a sandbox its own machine no longer has, a create
// aimed at a machine that is not answering.
//
// A machine's name is fleet topology. It is the useful half of the sentence for
// the owner of the sandbox and it is a map of the deployment for a stranger who
// merely typed a public URL. See internal/proxy's error pages.
func MachineNamed(err error) (string, bool) {
	var e *Error
	if !errors.As(err, &e) {
		return "", false
	}
	node, _ := e.Details["node"].(string)
	return node, node != ""
}

// UnreachableNode is MachineNamed narrowed to a node outage: the machine is not
// answering, as opposed to answering with something a caller did not want.
func UnreachableNode(err error) (string, bool) {
	if !IsNodeUnreachable(err) {
		return "", false
	}
	return MachineNamed(err)
}

// ParseKind is the inverse of Kind.String(), derived from the same table rather
// than restated. The string form is what crosses the node link precisely because
// the Kind constants are iota-ordered: a future insertion would silently
// renumber a wire format already in the field. New Kinds are APPEND-ONLY, and
// adding one also means naming it in kindNames and editing
// components.schemas.Error's `kind` enum in internal/restapi/openapi.json.
//
// An unrecognised string answers (KindInternal, false) rather than an error: a
// peer that has learned a Kind this build has not still raised a failure this
// build must render, and a 500 is the honest rendering of one it cannot
// classify.
func ParseKind(s string) (Kind, bool) {
	k, ok := kindByName[s]
	if !ok {
		return KindInternal, false
	}
	return k, true
}

func joinNames(in []string) string {
	out := ""
	for i, n := range in {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
