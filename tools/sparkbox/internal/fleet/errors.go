package fleet

import (
	"fmt"
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// codeUnreachable is the stable machine token for "that machine is not
// answering". It is a Code and not a Kind on purpose — see Unreachable.
//
// The token itself lives in ctlops, not here, because the surfaces that have
// to tell a node outage from a guest fault are edges — the HTTP proxy, the
// browser terminal — and making them import the router to ask would drag the
// whole placement ledger into the edge's dependency graph for one string.
const codeUnreachable = ctlops.CodeNodeUnreachable

// Unreachable is the canonical "that machine is not answering" error.
//
// It reuses KindCapacity deliberately. KindCapacity already yields exit 1 and
// HTTP 503, which is exactly the answer a node outage wants, whereas adding a
// Kind would mean editing both the pinned taxonomy table in ctlops and the
// hard `kind` enum in the hand-authored, embedded OpenAPI document — which is
// parsed at package init, so a bad edit panics the binary at startup rather
// than failing a request. The friendly capacity guidance in the SSH gateway
// and the browser terminal branches on errors.As against the concrete
// *host.CapacityError, never on Kind, so nothing misrenders.
//
// This error is only ever reachable after the ownership gate has passed, so it
// can never become an existence oracle: a stranger asking about the same name
// still gets the masked "no sandbox named %q".
func Unreachable(op, sandbox, node string) *ctlops.Error {
	return &ctlops.Error{
		Kind: ctlops.KindCapacity,
		Op:   op,
		Code: codeUnreachable,
		Msg:  fmt.Sprintf("sandbox %q lives on node %q, which is offline", sandbox, node),
		Hint: "It will be reachable again when the node reconnects; nothing was lost.",
		Details: map[string]any{
			"node": node,
		},
		Verbatim: true,
		Exit:     1,
		Status:   http.StatusServiceUnavailable,
	}
}

// nodeOffline is Unreachable's placement-time twin: the machine a caller asked
// to build on is not answering.
//
// It is a second sentence rather than a reuse of Unreachable because
// Unreachable's wording — `sandbox %q lives on node %q` — is about a sandbox
// that already exists somewhere, and a create has not put one anywhere yet.
// Saying a sandbox lives on a machine that has never held it would be the
// gateway telling a user something untrue about where their work is.
//
// Everything a client switches on is the same: the same Code, the same
// KindCapacity, the same exit 1 and HTTP 503, so IsNodeUnreachable answers yes
// for both and any retry logic written against one covers the other.
func nodeOffline(op, node string) *ctlops.Error {
	return &ctlops.Error{
		Kind: ctlops.KindCapacity,
		Op:   op,
		Code: codeUnreachable,
		Msg:  fmt.Sprintf("node %q is offline", node),
		Hint: "It can take sandboxes again when it reconnects; leave --node off to build on the gateway.",
		Details: map[string]any{
			"node": node,
		},
		Verbatim: true,
		Exit:     1,
		Status:   http.StatusServiceUnavailable,
	}
}

// The two answers reconciliation produces. Both are KindConflict — well formed,
// refused by the current state of the fleet — which yields exit 1 and HTTP 409
// with no override needed, and neither is a not-found: the sandbox is still
// visible to its owner in every listing, still theirs, and still holding its
// name. Saying "no such sandbox" instead would tell a user their work had been
// deleted by something that has deliberately deleted nothing.
const (
	codeOrphaned  = "sandbox_orphaned"
	codeContested = "sandbox_contested"
)

// orphanedSandbox is what an operation on a sandbox its own machine no longer
// has reports.
//
// It is a distinct sentence from Unreachable because the two are opposite
// situations that a single "not available" would blur: unreachable means the
// machine is not answering and the sandbox is almost certainly fine, and this
// means the machine IS answering and says it does not have it. The first
// resolves itself when a laptop wakes up; the second never resolves itself and
// needs somebody to go and look.
func orphanedSandbox(op, sandbox, node string) *ctlops.Error {
	return &ctlops.Error{
		Kind: ctlops.KindConflict,
		Op:   op,
		Code: codeOrphaned,
		Msg: fmt.Sprintf("sandbox %q is not on node %q any more: that machine is connected and no longer has it",
			sandbox, node),
		Hint: "Nothing has been deleted here and the name is still yours; ask an operator to check that machine.",
		Details: map[string]any{
			"node": node,
		},
		Verbatim: true,
		Exit:     1,
		Status:   http.StatusConflict,
	}
}

// contestedSandbox is what an operation on a name two machines claim reports.
//
// It is nearly unreachable through the control plane and that is deliberate: a
// contested row is served by no read at all, so the ownership gate answers the
// masked not-found first and this sentence only surfaces to a caller holding
// the router directly. It exists so that such a caller cannot be routed to one
// of two machines on a coin flip — the failure mode this state exists to
// prevent — rather than because a user is expected to read it.
func contestedSandbox(op, sandbox string) *ctlops.Error {
	return &ctlops.Error{
		Kind:     ctlops.KindConflict,
		Op:       op,
		Code:     codeContested,
		Msg:      fmt.Sprintf("two machines claim the sandbox name %q, so this gateway will not act on either", sandbox),
		Hint:     "Nothing has been deleted on either machine; an operator has to decide which one holds your data.",
		Verbatim: true,
		Exit:     1,
		Status:   http.StatusConflict,
	}
}

// IsNodeUnreachable reports whether err is (or wraps) a node outage.
//
// It is ctlops.IsNodeUnreachable under a name this package's own callers
// already use. The predicate moved down there when the edges acquired their
// first consumers of it (W22): internal/proxy renders a node outage as a 503
// that says the machine is offline, instead of the 502 that says the guest app
// is not listening, and it must be able to ask without importing the router.
func IsNodeUnreachable(err error) bool { return ctlops.IsNodeUnreachable(err) }
