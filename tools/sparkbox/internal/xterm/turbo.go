package xterm

// The turbo button's endpoint: one same-origin POST that restarts the sandbox
// this page is attached to with doubled CPU and RAM, or back at its own size.
//
// It is the one mutation this package has, and it is deliberately not on
// Attacher. That interface is narrow on purpose — nothing reached from a
// terminal page should be able to pause, destroy or re-tag a box — so the
// capability arrives as its own optional dependency, left nil by every test and
// by any deployment that would rather not offer it. Unwired, the route answers
// 501 and the page hides the button.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// Turbocharger restarts a sandbox with doubled CPU and RAM (on), or back at its
// own size. Satisfied by *fleet.Fleet and by *host.Manager.
type Turbocharger interface {
	SetTurbo(ctx context.Context, name string, on bool) error
}

var _ Turbocharger = (*host.Manager)(nil)

// turboTimeout matches the user console's action budget: the round trip is a
// pause — which writes the guest's whole memory image — plus a cold boot.
const turboTimeout = 3 * time.Minute

type turboReq struct {
	On bool `json:"on"`
}

// turbo answers POST /turbo.
//
// The CSRF gate is spelled out here rather than taken from
// edgeauth.RequireMutation because that middleware compares against one fixed
// first-party origin, and this handler has a different origin per sandbox —
// `<name>-xterm.<zone>` is the whole isolation model. So the two accepted
// proofs are checked directly: an Origin equal to the host this request was
// served on, or the custom header, which no cross-site form can set and which
// a cross-origin fetch can only send after a preflight nothing here answers.
func (h *Handler) turbo(w http.ResponseWriter, r *http.Request) {
	box, ok := h.resolve(w, r)
	if !ok {
		return
	}
	if h.turbocharger == nil {
		http.Error(w, "sparkbox: turbo is not enabled on this host", http.StatusNotImplemented)
		return
	}
	if !firstParty(r) {
		http.Error(w, "sparkbox: cross-origin mutation refused — send header "+
			edgeauth.MutationHeader+": 1", http.StatusForbidden)
		return
	}
	var req turboReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "sparkbox: malformed request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), turboTimeout)
	defer cancel()
	if err := h.turbocharger.SetTurbo(ctx, box.Name, req.On); err != nil {
		h.log.Warn("turbo failed", "sandbox", box.Name, "on", req.On, "err", err)
		// The message is the manager's own — "is archived", "turbo is not
		// enabled on this host", a capacity refusal naming the numbers — and the
		// page shows it verbatim, because every one of them tells the owner
		// something they can act on.
		http.Error(w, "sparkbox: "+err.Error(), http.StatusConflict)
		return
	}
	h.log.Info("terminal set turbo", "sandbox", box.Name, "on", req.On)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	json.NewEncoder(w).Encode(map[string]bool{"turbo": req.On}) //nolint:errcheck
}

// firstParty reports whether a mutation carries proof it was made by this
// page. See turbo for why the check is here rather than in the middleware.
func firstParty(r *http.Request) bool {
	if r.Header.Get(edgeauth.MutationHeader) == "1" {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	scheme := "https://"
	if r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "http://"
	}
	return origin == scheme+r.Host
}
