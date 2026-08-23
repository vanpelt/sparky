package edgeauth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestLogin() (*LoginHandler, *Signer) {
	s := NewSigner([]byte("k"))
	h, err := NewLoginHandler(LoginConfig{Signer: s, Domain: "hivemind.tools", Gateway: "hivemind.tools", Secure: true})
	if err != nil {
		panic(err)
	}
	return h, s
}

func TestLoginPageRenders(t *testing.T) {
	h, _ := newTestLogin()
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "https://login.hivemind.tools/?return=https://x.hivemind.tools/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ssh ctl@hivemind.tools session-token") {
		t.Fatalf("login page missing mint instructions: %s", body)
	}
}

func TestLoginSessionSetsCookieAndRedirects(t *testing.T) {
	h, s := newTestLogin()
	tok, _, _ := s.Mint(Identity{Handle: "vanpelt"}, time.Hour)

	form := url.Values{"token": {tok}, "return": {"https://app.hivemind.tools/dash"}}
	req := httptest.NewRequest("POST", "https://login.hivemind.tools/session", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://app.hivemind.tools/dash" {
		t.Fatalf("unexpected redirect %q", loc)
	}
	var got *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			got = c
		}
	}
	if got == nil {
		t.Fatal("no session cookie set")
	}
	// The parser strips the leading dot (RFC 6265); "hivemind.tools" still
	// covers every subdomain, which is the whole point of the parent-domain scope.
	if got.Value != tok || got.Domain != "hivemind.tools" || !got.HttpOnly || !got.Secure {
		t.Fatalf("cookie attributes wrong: %+v", got)
	}
}

func TestLoginRejectsBadTokenAndOpenRedirect(t *testing.T) {
	h, _ := newTestLogin()

	// Bad token: bounce back to the form with err=1, no cookie.
	form := url.Values{"token": {"spk_v1.garbage"}, "return": {"https://app.hivemind.tools/"}}
	req := httptest.NewRequest("POST", "https://login.hivemind.tools/session", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "err=1") {
		t.Fatalf("bad token should bounce to form: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName && c.Value != "" {
			t.Fatal("a cookie was set for an invalid token")
		}
	}

	// Open-redirect guard: an off-zone return collapses to the zone root.
	if got := h.safeReturn("https://evil.com/steal"); got != "https://hivemind.tools/" {
		t.Fatalf("open redirect not blocked: %q", got)
	}
	if got := h.safeReturn("https://ok.hivemind.tools/x"); got != "https://ok.hivemind.tools/x" {
		t.Fatalf("legit return rejected: %q", got)
	}
	if got := h.safeReturn("https://ok.hivemind.tools:6454/x"); got != "https://ok.hivemind.tools:6454/x" {
		t.Fatalf("legit non-default HTTPS port return rejected: %q", got)
	}
	// A TLS edge stays https-only: an http return collapses to the zone root.
	if got := h.safeReturn("http://ok.hivemind.tools/x"); got != "https://hivemind.tools/" {
		t.Fatalf("http return on a TLS edge not blocked: %q", got)
	}
}

// A non-TLS edge (the mock-driver dev loop on localtest.me) serves the zone
// over http, so safeReturn must accept http return URLs there — still on-zone
// only — and its fallback must be reachable (http, not https).
func TestSafeReturnHTTPOnNonTLSEdge(t *testing.T) {
	s := NewSigner([]byte("k"))
	h, err := NewLoginHandler(LoginConfig{Signer: s, Domain: "localtest.me", Gateway: "localtest.me", Secure: false})
	if err != nil {
		t.Fatal(err)
	}

	if got := h.safeReturn("http://my.localtest.me:8081/x"); got != "http://my.localtest.me:8081/x" {
		t.Fatalf("dev-loop http return rejected: %q", got)
	}
	if got := h.safeReturn("https://my.localtest.me/x"); got != "https://my.localtest.me/x" {
		t.Fatalf("https return rejected on non-TLS edge: %q", got)
	}
	if got := h.safeReturn("http://evil.com/steal"); got != "http://localtest.me/" {
		t.Fatalf("off-zone http return not collapsed: %q", got)
	}
	if got := h.safeReturn(""); got != "http://localtest.me/" {
		t.Fatalf("fallback on non-TLS edge should be http: %q", got)
	}
}
