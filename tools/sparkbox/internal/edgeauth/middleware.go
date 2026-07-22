package edgeauth

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// MutationHeader marks a request as first-party console script. Browsers
// refuse to attach custom headers cross-site without a CORS preflight, so its
// presence defeats CSRF the same way a matching Origin does.
const MutationHeader = "X-Sparkbox-Console"

// Accounts is the slice of the user store the middleware needs: resolving a
// handle to its record, from which only operator status is read. *users.Store
// satisfies it.
type Accounts interface {
	Get(handle string) (users.User, error)
}

// Session is what Require stores in the request context: the verified visitor
// plus their operator status, resolved once per request.
type Session struct {
	Identity
	Operator bool
}

type ctxKey int

const sessionKey ctxKey = iota

// From returns the Session that Require stored in ctx for the authenticated
// request being handled.
func From(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey).(Session)
	return s, ok
}

// Require gates a handler behind a valid session (cookie or Authorization:
// Bearer). Unauthenticated browsers are 303'd to loginURL with the original
// URL to return to; API clients get a 401. On success the verified Session is
// available via From. Every response — allowed or not — carries
// Cache-Control: no-store, since anything behind the gate is per-visitor.
func Require(signer *Signer, accounts Accounts, loginURL string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			id, ok := signer.IdentityFrom(r)
			if !ok {
				challenge(w, r, loginURL)
				return
			}
			sess := Session{Identity: id}
			if accounts != nil {
				u, err := accounts.Get(id.Handle)
				// A disabled account's outstanding cookie must stop working
				// everywhere, not just where the next credential is issued: the
				// token's MAC key is fleet-wide, so there is nothing else to
				// revoke short of rotating the OIDC key for every user. A store
				// error is deliberately NOT fail-closed — a transient sqlite
				// hiccup would sign every visitor out of the whole edge — so only
				// a status we positively read as inactive refuses.
				if err == nil && !u.Active() {
					http.Error(w, "sparkbox: this account is disabled", http.StatusForbidden)
					return
				}
				if err == nil {
					sess.Operator = u.IsOperator()
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
		})
	}
}

// RequireMutation is Require plus a CSRF gate for state-changing endpoints.
// The session cookie rides every same-site request — and SameSite=Lax does
// not help here, because sandbox subdomains are same-site with the console —
// so a mutation must also prove first-party intent: an Origin header matching
// origin (e.g. "https://my.hivemind.tools"), the MutationHeader custom
// header, or Bearer auth (a header no cross-site request can carry).
func RequireMutation(signer *Signer, accounts Accounts, loginURL, origin string) func(http.Handler) http.Handler {
	require := Require(signer, accounts, loginURL)
	return func(next http.Handler) http.Handler {
		return require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Origin") != origin &&
				r.Header.Get(MutationHeader) != "1" &&
				!strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				http.Error(w, "sparkbox: cross-origin mutation refused — send header "+
					MutationHeader+": 1 or authenticate with a Bearer token", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}

// challenge answers an unauthenticated request: browsers are sent to the
// login page with the original URL to return to; API clients (no text/html in
// Accept) get a plain 401.
func challenge(w http.ResponseWriter, r *http.Request, loginURL string) {
	if loginURL == "" || !strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Error(w, "sparkbox: authentication required — sign in and retry, or send a session token as `Authorization: Bearer <token>`",
			http.StatusUnauthorized)
		return
	}
	ret := "https://" + r.Host + r.URL.RequestURI()
	http.Redirect(w, r, strings.TrimSuffix(loginURL, "/")+"/?return="+url.QueryEscape(ret), http.StatusSeeOther)
}
