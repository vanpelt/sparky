package edgeauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMintVerifyRoundTrip(t *testing.T) {
	s := NewSigner([]byte("test-oidc-key-material"))
	tok, exp, err := s.Mint(Identity{Handle: "vanpelt", Email: "vanpelt@wandb.com"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.After(time.Now()) {
		t.Fatalf("expiry not in the future: %v", exp)
	}
	id, ok := s.Verify(tok)
	if !ok {
		t.Fatal("valid token failed verification")
	}
	if id.Handle != "vanpelt" || id.Email != "vanpelt@wandb.com" {
		t.Fatalf("identity round-trip wrong: %+v", id)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	s := NewSigner([]byte("k"))
	tok, _, _ := s.Mint(Identity{Handle: "a"}, time.Hour)

	// Flip a byte in the payload — the MAC must no longer match.
	b := []byte(tok)
	b[len(TokenPrefix)+2] ^= 0x01
	if _, ok := s.Verify(string(b)); ok {
		t.Fatal("tampered token verified")
	}

	// A different key must reject a token it didn't sign.
	other := NewSigner([]byte("different"))
	if _, ok := other.Verify(tok); ok {
		t.Fatal("token verified under the wrong key")
	}

	for _, junk := range []string{"", "spk_v1.", "spk_v1.abc", "nope", tok + "x"} {
		if _, ok := s.Verify(junk); ok {
			t.Fatalf("junk %q verified", junk)
		}
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	s := NewSigner([]byte("k"))
	tok, _, _ := s.Mint(Identity{Handle: "a"}, -time.Second) // already expired
	if _, ok := s.Verify(tok); ok {
		t.Fatal("expired token verified")
	}
}

func TestIdentityFromCookieAndBearer(t *testing.T) {
	s := NewSigner([]byte("k"))
	tok, _, _ := s.Mint(Identity{Handle: "vanpelt"}, time.Hour)

	rCookie := httptest.NewRequest("GET", "https://x.hivemind.tools/", nil)
	rCookie.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	if id, ok := s.IdentityFrom(rCookie); !ok || id.Handle != "vanpelt" {
		t.Fatalf("cookie identity failed: %+v ok=%v", id, ok)
	}

	rBearer := httptest.NewRequest("GET", "https://x.hivemind.tools/", nil)
	rBearer.Header.Set("Authorization", "Bearer "+tok)
	if id, ok := s.IdentityFrom(rBearer); !ok || id.Handle != "vanpelt" {
		t.Fatalf("bearer identity failed: %+v ok=%v", id, ok)
	}

	rNone := httptest.NewRequest("GET", "https://x.hivemind.tools/", nil)
	if _, ok := s.IdentityFrom(rNone); ok {
		t.Fatal("unauthenticated request produced an identity")
	}
}
