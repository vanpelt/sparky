package edgeauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// fakePasskeys is an in-memory PasskeyStore for handler tests.
type fakePasskeys struct {
	users map[string]users.User
	keys  map[string][]users.Passkey
}

func (f *fakePasskeys) Get(handle string) (users.User, error) {
	u, ok := f.users[handle]
	if !ok {
		return users.User{}, users.ErrNoSuchUser
	}
	return u, nil
}
func (f *fakePasskeys) Passkeys(handle string) ([]users.Passkey, error) {
	return f.keys[handle], nil
}
func (f *fakePasskeys) HasPasskeys(handle string) (bool, error) {
	return len(f.keys[handle]) > 0, nil
}
func (f *fakePasskeys) AddPasskey(handle, label string, cred webauthn.Credential) error {
	f.keys[handle] = append(f.keys[handle], users.Passkey{Label: label, Credential: cred})
	return nil
}
func (f *fakePasskeys) UpdatePasskey(handle string, cred webauthn.Credential) error { return nil }

func newPasskeyLogin(t *testing.T) (*LoginHandler, *Signer, *fakePasskeys) {
	t.Helper()
	s := NewSigner([]byte("k"))
	store := &fakePasskeys{
		users: map[string]users.User{"vanpelt": {Handle: "vanpelt", Status: "active"}},
		keys:  map[string][]users.Passkey{},
	}
	h, err := NewLoginHandler(LoginConfig{
		Signer: s, Domain: "hivemind.tools", Gateway: "hivemind.tools", Secure: true,
		Passkeys: store, Subdomain: "login",
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, s, store
}

func TestLoginPageOffersPasskeys(t *testing.T) {
	h, _, _ := newPasskeyLogin(t)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "https://login.hivemind.tools/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Sign in with a passkey") {
		t.Error("passkey button missing from login page")
	}
	if !strings.Contains(body, "session-token") {
		t.Error("token escape hatch missing from login page")
	}
}

func TestLoginPageTokenOnlyWithoutStore(t *testing.T) {
	h, _ := newTestLogin() // no Passkeys configured
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "https://login.hivemind.tools/", nil))
	if strings.Contains(rec.Body.String(), "Sign in with a passkey") {
		t.Error("passkey button rendered with passkeys disabled")
	}
}

func TestLoginBeginIssuesChallengeAndCeremonyCookie(t *testing.T) {
	h, _, _ := newPasskeyLogin(t)
	req := httptest.NewRequest("POST", "https://login.hivemind.tools/webauthn/login/begin", nil)
	req.Header.Set("Origin", "https://login.hivemind.tools")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("begin = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RPID      string `json:"rpId"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PublicKey.Challenge == "" || body.PublicKey.RPID != "hivemind.tools" {
		t.Fatalf("unexpected assertion options: %+v", body)
	}
	var ceremony *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == ceremonyCookie {
			ceremony = c
		}
	}
	if ceremony == nil || ceremony.Value == "" || !ceremony.HttpOnly {
		t.Fatalf("ceremony cookie not set: %+v", ceremony)
	}
}

func TestCeremonyEndpointsRefuseCrossOrigin(t *testing.T) {
	h, _, _ := newPasskeyLogin(t)
	req := httptest.NewRequest("POST", "https://login.hivemind.tools/webauthn/login/begin", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin begin = %d; want 403", rec.Code)
	}
}

func TestRegisterBeginRequiresSession(t *testing.T) {
	h, s, _ := newPasskeyLogin(t)
	mk := func(cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "https://login.hivemind.tools/webauthn/register/begin", nil)
		req.Header.Set("Origin", "https://login.hivemind.tools")
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := mk(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous register begin = %d; want 401", rec.Code)
	}
	tok, _, _ := s.Mint(Identity{Handle: "vanpelt"}, time.Hour)
	rec := mk(tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed register begin = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PublicKey struct {
			User struct {
				Name string `json:"name"`
			} `json:"user"`
			AuthenticatorSelection struct {
				ResidentKey string `json:"residentKey"`
			} `json:"authenticatorSelection"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PublicKey.User.Name != "vanpelt" {
		t.Errorf("user.name = %q; want vanpelt", body.PublicKey.User.Name)
	}
	if body.PublicKey.AuthenticatorSelection.ResidentKey != "required" {
		t.Errorf("residentKey = %q; want required (discoverable sign-in depends on it)",
			body.PublicKey.AuthenticatorSelection.ResidentKey)
	}
}

// A token sign-in for an account without passkeys detours through the
// enrollment offer; with a passkey enrolled it goes straight through.
func TestSessionDetoursToEnroll(t *testing.T) {
	h, s, store := newPasskeyLogin(t)
	tok, _, _ := s.Mint(Identity{Handle: "vanpelt"}, time.Hour)

	post := func() string {
		form := url.Values{"token": {tok}, "return": {"https://app.hivemind.tools/x"}}
		req := httptest.NewRequest("POST", "https://login.hivemind.tools/session", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("session = %d", rec.Code)
		}
		return rec.Header().Get("Location")
	}
	if loc := post(); !strings.HasPrefix(loc, "/enroll?return=") {
		t.Fatalf("no-passkey sign-in should offer enrollment, got %q", loc)
	}
	store.keys["vanpelt"] = []users.Passkey{{Credential: webauthn.Credential{ID: []byte("x")}}}
	if loc := post(); loc != "https://app.hivemind.tools/x" {
		t.Fatalf("passkey-holder sign-in should go straight through, got %q", loc)
	}
}

func TestEnrollPageRequiresSession(t *testing.T) {
	h, s, _ := newPasskeyLogin(t)
	req := httptest.NewRequest("GET", "https://login.hivemind.tools/enroll?return=https://app.hivemind.tools/x", nil)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("anonymous enroll = %d; want redirect to login", rec.Code)
	}

	tok, _, _ := s.Mint(Identity{Handle: "vanpelt"}, time.Hour)
	req = httptest.NewRequest("GET", "https://login.hivemind.tools/enroll?return=https://app.hivemind.tools/x", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	rec = httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "vanpelt") {
		t.Fatalf("authed enroll page = %d", rec.Code)
	}
}
