package launch

import (
	"net/http"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
)

// mutation refuses a create that carries no proof it was made by this page.
//
// # Why this is not edgeauth.RequireMutation
//
// It is not a fork of the house rule. It is the SECOND house rule, already
// written twice in this tree — internal/xterm/turbo.go:88-100 and
// internal/xterm/ws.go:486-495 — whose own comment says "See turbo for why the
// check is here rather than in the middleware". This is the third surface that
// needs it, and it needs it for a reason of its own.
//
// RequireMutation compares Origin against ONE hardcoded first-party string,
// built as `"https://" + sub + "." + domain` with no port. On a --proxy-tls=false
// development run the browser sends `http://go.localtest.me:8081`, which can
// never equal that, so the check fails. Everywhere else in the product that is
// survivable: the two consoles are JavaScript, so the middleware's own remedy —
// "send header X-Sparkbox-Console: 1" — is a line of fetch() away. This page has
// no JavaScript AT ALL, which is precisely what buys it an honest
// `default-src 'none'` Content-Security-Policy, and a bare <form> cannot set a
// custom header on any browser ever made. So under RequireMutation the create
// button on a dev host would be permanently 403 with a remedy the page has no
// way to obey, and the only fix available to whoever hit it would be to weaken
// the CSP and add a script — trading a real security property for a broken one.
//
// # Why the check is still sound
//
// The third clause below compares Origin against the origin the request was
// actually addressed to, rendered from X-Forwarded-Proto (or the connection)
// plus r.Host — the identical rendering the terminal WebSocket handshake uses,
// so one page cannot be first-party to one route and a stranger to the other.
// That is not a hole: a cross-site form POST carries the INITIATING document's
// Origin, not the target's, and every current browser sends Origin on a form
// POST. An attacker's page at evil.example posting here sends
// `Origin: https://evil.example`, which matches neither the configured origin
// nor this request's own, and is refused. An absent Origin is refused outright
// rather than waved through — "no header" must never be the way to skip a
// check, which is the same rule ws.go states at the same place.
//
// # Why auth runs first
//
// Handler() wraps this in edgeauth.Require and not the reverse. A session that
// expired while somebody read the confirm page has to produce the sign-in
// bounce — a 303 that returns them here as a GET, so they land back on the page
// and press the button again — rather than a 403 about cross-origin requests,
// which would be both wrong and unactionable.
func (h *Handler) mutation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !firstParty(r, h.origin) {
			// No Origin echoed back and no detail about which clause failed: a
			// refusal on this surface is read by the attacker's page as often
			// as by a person.
			http.Error(w, "sparkbox: this create did not come from the launch page — open the link in your browser and press the button there",
				http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// firstParty reports whether a mutation carries proof it was made by this page.
//
// The three accepted proofs, in the order they are cheapest to check:
//
//  1. edgeauth.MutationHeader. A browser cannot attach a custom header to a
//     cross-site request without a CORS preflight, and nothing here answers
//     one. This page never sends it — it has no script — but the header is the
//     house's own proof and an API client or a test may hold it.
//  2. An Origin byte-equal to the configured origin. This is
//     RequireMutation's exact rule (internal/edgeauth/middleware.go:89), kept
//     so a correctly-configured production host is accepted by the same
//     comparison every other mutation in the product uses.
//  3. An Origin equal, case-insensitively, to the origin this request was
//     addressed to. This is the clause the zero-JS page actually lives on, and
//     the one RequireMutation does not have. See mutation for why.
//
// Byte equality on clause 2 and EqualFold on clause 3 is not an oversight: the
// configured origin is a string this process built and lower-cased itself, so
// there is nothing to fold; requestOrigin's host half comes from the wire, and
// a browser is entitled to vary the case of a hostname.
func firstParty(r *http.Request, origin string) bool {
	if r.Header.Get(edgeauth.MutationHeader) == "1" {
		return true
	}
	got := r.Header.Get("Origin")
	if got == "" {
		return false
	}
	if got == origin {
		return true
	}
	return strings.EqualFold(got, requestOrigin(r))
}

// requestOrigin renders the origin this request was addressed to.
//
// Lifted from internal/xterm/ws.go, deliberately rather than imported: it is
// unexported there, and the two copies exist because the rule they encode is
// one every edge that terminates its own CSRF check has to apply identically.
//
// The scheme comes from X-Forwarded-Proto when something in front terminated
// TLS, and otherwise from the connection. https is NOT assumed, which is the
// entire reason this works on a plain http://127.0.0.1 dev loop — and it is not
// a hole either, because the value it produces is only ever compared against an
// Origin the browser sent, never trusted on its own. Only the first element of
// a comma-separated X-Forwarded-Proto is read, since a chain of proxies appends
// and the nearest hop is the one that describes this connection.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(fwd, ",")[0]))
	}
	return scheme + "://" + r.Host
}
