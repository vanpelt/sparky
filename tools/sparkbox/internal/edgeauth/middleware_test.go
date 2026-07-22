package edgeauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// fakeAccounts satisfies Accounts without a database.
type fakeAccounts map[string]users.User

func (f fakeAccounts) Get(handle string) (users.User, error) {
	u, ok := f[handle]
	if !ok {
		return users.User{}, users.ErrNoSuchUser
	}
	return u, nil
}

const loginURL = "https://login.hivemind.tools/"

// echoSession reports what From found in the context, so tests can assert the
// identity and operator flag the middleware resolved.
var echoSession = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	s, ok := From(r.Context())
	if !ok {
		http.Error(w, "no session in context", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "handle=%s operator=%v", s.Handle, s.Operator)
})

func newTestRequire() (*Signer, http.Handler) {
	s := NewSigner([]byte("k"))
	accounts := fakeAccounts{
		"alice":    {Handle: "alice", Status: users.StatusActive, InvitedBy: "someoneelse"},
		"opsy":     {Handle: "opsy", Status: users.StatusActive, InvitedBy: users.OperatorInviter},
		"departed": {Handle: "departed", Status: "disabled", InvitedBy: "someoneelse"},
	}
	return s, Require(s, accounts, loginURL)(echoSession)
}

func serve(h http.Handler, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "https://my.hivemind.tools/api/machines", nil)
	for _, o := range opts {
		o(r)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func asCookie(tok string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: tok}) }
}

func asBearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func asBrowser(r *http.Request) { r.Header.Set("Accept", "text/html,*/*;q=0.8") }

func TestRequireAcceptsCookieAndBearer(t *testing.T) {
	s, h := newTestRequire()
	tok, _, _ := s.Mint(Identity{Handle: "alice"}, time.Hour)

	for name, opt := range map[string]func(*http.Request){"cookie": asCookie(tok), "bearer": asBearer(tok)} {
		rec := serve(h, opt)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d (%s)", name, rec.Code, rec.Body)
		}
		if got, want := rec.Body.String(), "handle=alice operator=false"; got != want {
			t.Fatalf("%s: session wrong: got %q want %q", name, got, want)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: allowed response must be no-store", name)
		}
	}
}

func TestRequireResolvesOperator(t *testing.T) {
	s, h := newTestRequire()

	tok, _, _ := s.Mint(Identity{Handle: "opsy"}, time.Hour)
	if rec := serve(h, asCookie(tok)); rec.Body.String() != "handle=opsy operator=true" {
		t.Fatalf("operator not resolved: %s", rec.Body)
	}
	// A handle with no account record is authenticated but never an operator.
	tok, _, _ = s.Mint(Identity{Handle: "ghost"}, time.Hour)
	if rec := serve(h, asCookie(tok)); rec.Body.String() != "handle=ghost operator=false" {
		t.Fatalf("unknown account should not be operator: %s", rec.Body)
	}
}

// TestRequireRefusesDisabledAccount: status = 'disabled' is the only
// deprovisioning mechanism the user schema offers, and the SSH and passkey paths
// both honour it. An outstanding cookie must not outlive it — the session
// token's MAC key is fleet-wide, so the alternative to checking here is rotating
// the OIDC key for every user on the host.
func TestRequireRefusesDisabledAccount(t *testing.T) {
	s, h := newTestRequire()
	tok, _, _ := s.Mint(Identity{Handle: "departed"}, time.Hour)

	for name, opt := range map[string]func(*http.Request){
		"cookie": asCookie(tok), "bearer": asBearer(tok),
	} {
		rec := serve(h, opt)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: disabled account got %d, want 403", name, rec.Code)
		}
	}
	// A store that cannot answer is NOT fail-closed: a transient sqlite error
	// must not sign every visitor out of the whole edge, so an unresolvable
	// handle keeps working (without operator status).
	tok, _, _ = s.Mint(Identity{Handle: "ghost"}, time.Hour)
	if rec := serve(h, asCookie(tok)); rec.Code != http.StatusOK {
		t.Fatalf("unresolvable account got %d, want 200", rec.Code)
	}
}

func TestRequireChallengesBrowserAndAPI(t *testing.T) {
	s, h := newTestRequire()

	// Browser with no session -> 303 to the login page, carrying return.
	rec := serve(h, asBrowser)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 for browser, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, loginURL+"?return=") || !strings.Contains(loc, "my.hivemind.tools") {
		t.Fatalf("unexpected redirect target %q", loc)
	}

	// API client (no text/html) -> 401; an expired/garbage token is the same.
	if rec := serve(h); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for API client, got %d", rec.Code)
	}
	expired, _, _ := s.Mint(Identity{Handle: "alice"}, -time.Hour)
	rec = serve(h, asCookie(expired))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired token, got %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("challenge response must be no-store")
	}
}

func TestRequireMutationGate(t *testing.T) {
	s := NewSigner([]byte("k"))
	h := RequireMutation(s, fakeAccounts{}, loginURL, "https://my.hivemind.tools")(echoSession)
	tok, _, _ := s.Mint(Identity{Handle: "alice"}, time.Hour)

	// A valid cookie alone (what a cross-site form or fetch carries) is refused.
	if rec := serve(h, asCookie(tok)); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie without first-party proof should be 403, got %d", rec.Code)
	}
	// A foreign Origin is refused too.
	rec := serve(h, asCookie(tok), func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.hivemind.tools")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin should be 403, got %d", rec.Code)
	}

	// Each first-party proof is sufficient on its own.
	allowed := map[string]func(*http.Request){
		"origin": func(r *http.Request) { r.Header.Set("Origin", "https://my.hivemind.tools") },
		"header": func(r *http.Request) { r.Header.Set(MutationHeader, "1") },
	}
	for name, opt := range allowed {
		if rec := serve(h, asCookie(tok), opt); rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d (%s)", name, rec.Code, rec.Body)
		}
	}
	if rec := serve(h, asBearer(tok)); rec.Code != http.StatusOK {
		t.Fatalf("bearer: expected 200, got %d (%s)", rec.Code, rec.Body)
	}

	// The auth gate still runs first: no session at all is 401, not 403.
	if rec := serve(h, func(r *http.Request) { r.Header.Set(MutationHeader, "1") }); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated mutation should be 401, got %d", rec.Code)
	}
}
