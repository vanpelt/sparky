package restapi

import (
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// terminalWS is the WebSocket door onto a sandbox's shell — the same session
// the page at https://<name>-xterm.<domain> opens, documented here so a
// non-browser client can drive it too.
//
// It is a GET, so it passes through edgeauth.Require rather than
// RequireMutation. That is not laziness: a browser cannot attach a custom
// header or an Authorization header to a WebSocket handshake, so the CSRF gate
// would reject exactly the client this endpoint exists for. The compensating
// control is the bridge's own Origin check, which is why the origin gate lives
// there and not here — one place, on the code path that actually upgrades.
//
// The owner gate runs HERE, before the handshake, and not inside the bridge.
// Two reasons, both load-bearing. A refusal has to be an HTTP status: once the
// connection is upgraded the only way to say "not yours" is a close code, which
// no client reads as a 404 and which this package could not render through its
// error envelope. And the bridge is shared with the browser page, which has
// already gated on its own — a check duplicated in two packages is a check that
// eventually disagrees with itself.
//
// It is ctlops.Get rather than ctlops.Attach because Get answers from the store
// without waking anything: a cross-owner probe must not resume a stranger's VM,
// and a legitimate owner's resume belongs after the upgrade, where the bridge
// can report "starting…" instead of holding the handshake open for the minutes
// an archived sandbox takes to come back.
func (h *Handler) terminalWS(w http.ResponseWriter, r *http.Request) {
	const op = "attach"
	if h.terminal == nil {
		h.fail(w, r, op, ctlops.Disabled(op, "browser terminals are not enabled on this host"))
		return
	}
	c, name := caller(r), r.PathValue("name")
	if _, err := h.ops.Get(r.Context(), c, name); err != nil {
		// Re-stamp the op: the gate is Get's, but the operation the client
		// asked for is attach, and the envelope's "op" is documented to equal
		// the operationId. Copied rather than mutated in place — the value
		// ctlops handed back may be a shared sentinel.
		e := *ctlops.AsError(op, err)
		e.Op = op
		h.fail(w, r, op, &e)
		return
	}
	h.terminal.ServeTerminal(w, r, c, name)
}
