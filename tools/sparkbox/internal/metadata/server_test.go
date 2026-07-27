package metadata

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// fakeBoxes maps guest IP -> sandbox, the same resolution host.Manager does
// over its running records.
type fakeBoxes map[string]*host.Sandbox

func (f fakeBoxes) GetByHostIP(ip string) (*host.Sandbox, bool) {
	b, ok := f[ip]
	return b, ok
}

// fakeAccounts is a handle -> user record lookup.
type fakeAccounts map[string]users.User

func (f fakeAccounts) Get(handle string) (users.User, error) {
	u, ok := f[handle]
	if !ok {
		return users.User{}, users.ErrNoSuchUser
	}
	return u, nil
}

// fixture builds a server over two running sandboxes in adjacent network
// slots: alice's in slot 5 (172.30.5.2) and bob's in slot 9 (172.30.9.2).
func fixture(t *testing.T, auds ...string) *Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := oidc.New(oidc.Options{
		IssuerURL: "https://oidc.example.test", Signer: key, Audiences: auds,
	})
	if err != nil {
		t.Fatal(err)
	}
	verified := time.Now().UTC()
	return New(Options{
		Manager: fakeBoxes{
			"172.30.5.2": {Name: "alice-box", Owner: "alice", Image: "universal", HostIP: "172.30.5.2", KeyFP: "SHA256:aaa"},
			"172.30.9.2": {Name: "bob-box", Owner: "bob", Image: "universal", HostIP: "172.30.9.2"},
		},
		Identity: Local{
			Issuer: iss,
			Users: fakeAccounts{
				"alice": {Handle: "alice", Status: "active", GitHubLogin: "alice-gh", GitHubVerifiedAt: &verified},
				"bob":   {Handle: "bob", Status: "active"}, // never linked GitHub
			},
			NodeName: "test-box",
		},
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultAudience: "https://hivemind.wandb.tools",
	})
}

// request drives a handler as if a guest at src had connected to dst.
func request(s *Server, path, src, dst string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = net.JoinHostPort(src, "40000")
	r = r.WithContext(context.WithValue(r.Context(), localAddrKey{},
		&net.TCPAddr{IP: net.ParseIP(dst), Port: DefaultPort}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func TestTokenIsMintedForTheCallingSandbox(t *testing.T) {
	s := fixture(t)
	rec := request(s, "/token", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /token = %d: %s", rec.Code, rec.Body)
	}
	claims := decodeClaims(t, rec.Body.String())
	for field, want := range map[string]string{
		"owner": "alice", "sandbox": "alice-box", "sub": "sparkbox:user:alice",
		"box": "test-box", "image": "universal", "key_fp": "SHA256:aaa",
		"aud": "https://hivemind.wandb.tools", "github": "alice-gh",
	} {
		if got, _ := claims[field].(string); got != want {
			t.Errorf("claim %s = %q, want %q", field, got, want)
		}
	}
}

// The whole security model in one test. The host forwards between taps and
// Linux accepts packets for any local address on any interface, so alice CAN
// open a connection to bob's gateway address. Identifying the caller by that
// (attacker-chosen) destination would hand alice a token minted for bob. The
// source address is the end alice cannot forge, so it is the end we trust.
func TestGuestCannotMintATokenForAnotherSandbox(t *testing.T) {
	s := fixture(t)

	rec := request(s, "/token", "172.30.5.2", "172.30.9.1")
	if rec.Code == http.StatusOK {
		claims := decodeClaims(t, rec.Body.String())
		t.Fatalf("alice got a token for owner=%v sandbox=%v by dialing bob's gateway",
			claims["owner"], claims["sandbox"])
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-slot /token = %d, want 403", rec.Code)
	}
	if rec := request(s, "/identity", "172.30.5.2", "172.30.9.1"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-slot /identity = %d, want 403", rec.Code)
	}
}

func TestNonSandboxCallersAreRefused(t *testing.T) {
	s := fixture(t)
	for _, tc := range []struct{ name, src, dst string }{
		// Someone reaching the port on the host's public NIC.
		{"public source", "203.0.113.7", "192.0.2.1"},
		// In-range source, but no running sandbox holds that address — which is
		// also the paused case: a paused sandbox's record carries no IP.
		{"unknown slot", "172.30.77.2", "172.30.77.1"},
		// A guest addressing something that isn't its own gateway.
		{"wrong gateway", "172.30.5.2", "172.30.5.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := request(s, "/token", tc.src, tc.dst); rec.Code != http.StatusForbidden {
				t.Errorf("got %d, want 403", rec.Code)
			}
		})
	}
}

func TestAudienceOutsideTheAllowlistIsRefused(t *testing.T) {
	s := fixture(t, "https://hivemind.wandb.tools")
	if rec := request(s, "/token?aud=https://evil.example", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusBadRequest {
		t.Errorf("disallowed audience = %d, want 400", rec.Code)
	}
	rec := request(s, "/token?aud=https://hivemind.wandb.tools", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("allowlisted audience = %d, want 200", rec.Code)
	}
	if aud := decodeClaims(t, rec.Body.String())["aud"]; aud != "https://hivemind.wandb.tools" {
		t.Errorf("aud = %v", aud)
	}
}

func TestRateLimitCapsMintingPerSandbox(t *testing.T) {
	s := fixture(t)
	for i := 0; i < rateBurst; i++ {
		if rec := request(s, "/token", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, rec.Code)
		}
	}
	if rec := request(s, "/token", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("over-budget request = %d, want 429", rec.Code)
	}
	// The limit is per sandbox: bob is unaffected by alice's spending.
	if rec := request(s, "/token", "172.30.9.2", "172.30.9.1"); rec.Code != http.StatusOK {
		t.Errorf("bob = %d, want 200 (the limit must be per-sandbox)", rec.Code)
	}
}

// /identity answers "who am I" without burning a single-use jti, so it must
// not be rate limited alongside minting.
func TestIdentityReportsClaimsWithoutMinting(t *testing.T) {
	s := fixture(t)
	for i := 0; i < rateBurst+5; i++ {
		if rec := request(s, "/identity", "172.30.9.2", "172.30.9.1"); rec.Code != http.StatusOK {
			t.Fatalf("GET /identity #%d = %d: %s", i, rec.Code, rec.Body)
		}
	}
	rec := request(s, "/identity", "172.30.9.2", "172.30.9.1")
	var doc Doc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Owner != "bob" || doc.Sandbox != "bob-box" || doc.Subject != "sparkbox:user:bob" {
		t.Errorf("identity = %+v", doc)
	}
	if doc.Issuer != "https://oidc.example.test" {
		t.Errorf("iss = %q", doc.Issuer)
	}
	// bob never linked GitHub, so the claim must be absent — a policy matching
	// on it has to fail closed.
	if doc.GitHub != "" {
		t.Errorf("github = %q, want empty for an unlinked owner", doc.GitHub)
	}
}

// An owner whose GitHub link was never verified must not get the claim even if
// a login string is somehow on the record.
func TestGitHubClaimRequiresVerification(t *testing.T) {
	s := fixture(t)
	// Reach through the Identity the fixture installed and swap its accounts:
	// the claim assembly lives there now, not on the server.
	local := s.id.(Local)
	local.Users = fakeAccounts{"alice": {Handle: "alice", GitHubLogin: "alice-gh"}} // no GitHubVerifiedAt
	s.id = local
	rec := request(s, "/token", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /token = %d", rec.Code)
	}
	if _, present := decodeClaims(t, rec.Body.String())["github"]; present {
		t.Error("unverified github login leaked into the token claims")
	}
}

// decodeClaims pulls the payload out of a compact JWS without verifying it —
// the signature is oidc's contract and is tested there.
func decodeClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// ---------------------------------------------------------------------------
// A node's relayed identity
// ---------------------------------------------------------------------------

// relayStub is a node's Identity: it signs nothing and fails the way a real
// relay fails.
type relayStub struct{ err error }

func (r relayStub) Issue(context.Context, *host.Sandbox, string) (Token, error) {
	if r.err != nil {
		return Token{}, r.err
	}
	return Token{JWT: "relayed.jwt", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (r relayStub) Describe(context.Context, *host.Sandbox) (Doc, error) {
	if r.err != nil {
		return Doc{}, r.err
	}
	return Doc{Issuer: "https://oidc.example.test", Owner: "bob", Sandbox: "bob-box"}, nil
}

func relayFixture(t *testing.T, err error) *Server {
	t.Helper()
	return New(Options{
		Manager: fakeBoxes{
			"172.30.9.2": {Name: "bob-box", Owner: "bob", Image: "universal", HostIP: "172.30.9.2"},
		},
		Identity: relayStub{err: err},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// The caller-identification half is unchanged on a node: the same tap check,
// the same 403s, and a token that came from somewhere else entirely.
func TestRelayedIdentityServesTheGuestNormally(t *testing.T) {
	s := relayFixture(t, nil)
	rec := request(s, "/token", "172.30.9.2", "172.30.9.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /token = %d: %s", rec.Code, rec.Body)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "relayed.jwt" {
		t.Errorf("token = %q", got)
	}
	// And a guest still cannot ask about another slot, which is a property of
	// this machine and owes nothing to the gateway.
	if rec := request(s, "/token", "172.30.9.2", "172.30.5.1"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-slot on a node = %d, want 403", rec.Code)
	}
}

// The status codes are the whole point of the error split. A guest's
// sparkbox-token unit retries on failure with a StartLimitBurst, so an outage
// that fixes itself must not be reported as something permanent.
func TestRelayFailuresMapToTheRightStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"gateway unreachable", fmt.Errorf("%w: no link", ErrNoIssuer), http.StatusServiceUnavailable},
		{"audience refused", fmt.Errorf("%w: %q", ErrAudience, "https://evil"), http.StatusBadRequest},
		{"anything else", errors.New("the gateway is confused"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := relayFixture(t, tc.err)
			if rec := request(s, "/token", "172.30.9.2", "172.30.9.1"); rec.Code != tc.want {
				t.Errorf("GET /token = %d, want %d", rec.Code, tc.want)
			}
			if rec := request(s, "/identity", "172.30.9.2", "172.30.9.1"); rec.Code != tc.want {
				t.Errorf("GET /identity = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// An internal cause must not reach the guest: the sentence a relay failure
// carries can name the gateway's address or an internal code.
func TestRelayFailureDoesNotLeakItsCause(t *testing.T) {
	s := relayFixture(t, errors.New("dial 10.66.0.1:2222: connection refused"))
	rec := request(s, "/token", "172.30.9.2", "172.30.9.1")
	if strings.Contains(rec.Body.String(), "10.66.0.1") {
		t.Errorf("the gateway's address leaked into a guest-facing error: %s", rec.Body)
	}
}
