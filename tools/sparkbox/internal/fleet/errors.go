package fleet

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// codeUnreachable is the stable machine token for "that machine is not
// answering". It is a Code and not a Kind on purpose — see Unreachable.
const codeUnreachable = "node_unreachable"

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

// IsNodeUnreachable reports whether err is (or wraps) a node outage.
//
// Nothing branches on it yet: the edge still renders a node outage as the
// generic 502 it shows when a guest app is not listening. Telling the two apart
// there — "the machine holding this is offline" — is M2 work, and this is the
// predicate it will ask, kept here so the answer is not re-derived from the
// Code string at each call site.
func IsNodeUnreachable(err error) bool {
	var e *ctlops.Error
	return errors.As(err, &e) && e.Code == codeUnreachable
}
