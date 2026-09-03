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
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// fakeBoxes maps guest IP -> sandbox, the same resolution host.Manager does
// over its running records.
type fakeBoxes map[string]*host.Sandbox

func (f fakeBoxes) GetByHostIP(ip string) (*host.Sandbox, bool) {
	b, ok := f[ip]
	return b, ok
}

func (f fakeBoxes) SetPinned(name string, pinned bool) error {
	for _, b := range f {
		if b.Name == name {
			b.Pinned = pinned
			return nil
		}
	}
	return errors.New("no such sandbox")
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

type fakeRouteControl struct {
	visibility string
	visPort    int // the port the visibility change named, 0 for the whole sandbox
	port       int
}

type fakeRepoStatusSink struct {
	name string
	rows []host.RepoStatus
	at   time.Time
}

type fakeVitalsReader struct{ reading host.Vitals }

func (f fakeVitalsReader) Vitals(context.Context, string) (host.Vitals, error) {
	return f.reading, nil
}

func (f *fakeRepoStatusSink) SetRepoStatus(name string, rows []host.RepoStatus, at time.Time) error {
	f.name, f.rows, f.at = name, append([]host.RepoStatus(nil), rows...), at
	return nil
}

func (f *fakeRouteControl) SetVisibility(_ context.Context, box *host.Sandbox, visibility string, port int) (RouteVisibility, error) {
	f.visibility, f.visPort = visibility, port
	return RouteVisibility{Sandbox: box.Name, Visibility: visibility, Port: port, Routes: 2}, nil
}

func (f *fakeRouteControl) SetPort(_ context.Context, box *host.Sandbox, port int) (RoutePort, error) {
	f.port = port
	return RoutePort{Sandbox: box.Name, Port: port}, nil
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
				"alice": {Handle: "alice", Status: "active", GitHubLogin: "alice-gh",
					GitHubID:         271676,
					GitHubVerifiedAt: &verified, GitHubVia: users.GitHubViaKeys},
				"bob": {Handle: "bob", Status: "active"}, // never linked GitHub
			},
			NodeName: "test-box",
		},
		RouteControl:    &fakeRouteControl{},
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultAudience: "https://hivemind.wandb.tools",
	})
}

// request drives a handler as if a guest at src had connected to dst.
func request(s *Server, path, src, dst string) *httptest.ResponseRecorder {
	return requestMethod(s, http.MethodGet, path, src, dst)
}

func requestMethod(s *Server, method, path, src, dst string) *httptest.ResponseRecorder {
	return requestBody(s, method, path, "", src, dst)
}

func requestBody(s *Server, method, path, body, src, dst string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = net.JoinHostPort(src, "40000")
	r = r.WithContext(context.WithValue(r.Context(), localAddrKey{},
		&net.TCPAddr{IP: net.ParseIP(dst), Port: DefaultPort}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// TestDocsAreServedOverMetadataToo covers the reason this mount exists: the
// public docs.<domain> DNS name can resolve to this fleet's own edge, which a
// guest's tap firewall has no route to reach directly, so the metadata port —
// already open guest-to-host — carries the same content instead.
func TestDocsAreServedOverMetadataToo(t *testing.T) {
	s := fixture(t)
	for _, tc := range []struct{ path, want string }{
		{"/docs/docs.md", "## Pinning this VM"},
		{"/docs/proxy.md", "## Wake on request"},
		{"/docs/dev-environment.md", "## Allow the proxy's Host header"},
	} {
		// No guest identity established: this content is unauthenticated, same
		// trust boundary as /healthz.
		rec := request(s, tc.path, "0.0.0.0", "0.0.0.0")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("GET %s = %d %q, want 200 containing %q", tc.path, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestGuestPublishesStructuredRepoStatus(t *testing.T) {
	s := fixture(t)
	sink := &fakeRepoStatusSink{}
	s.repoStatus = sink
	sep := "\x1f"
	body := strings.Join([]string{
		"wandb/agentstream", "/home/sparky/src/wandb/agentstream", "feat/timeline",
		"origin/feat/timeline", "2", "1", "1", "stale",
	}, sep) + "\n"
	rec := requestBody(s, http.MethodPost, "/repos/status", body, "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /repos/status = %d: %s", rec.Code, rec.Body)
	}
	if sink.name != "alice-box" || len(sink.rows) != 1 {
		t.Fatalf("published name=%q rows=%+v", sink.name, sink.rows)
	}
	got := sink.rows[0]
	if got.Branch != "feat/timeline" || got.Ahead != 2 || got.Behind != 1 || !got.Dirty ||
		got.Path != "/home/sparky/src/wandb/agentstream" {
		t.Errorf("published row = %+v", got)
	}
	if sink.at.IsZero() {
		t.Error("publish did not record its observation time")
	}

	bad := requestBody(s, http.MethodPost, "/repos/status", "not-a-row", "172.30.5.2", "172.30.5.1")
	if bad.Code != http.StatusBadRequest {
		t.Errorf("malformed report = %d, want 400", bad.Code)
	}
}

func TestSelfStatusIncludesReportedReposForHumansAndAgents(t *testing.T) {
	s := fixture(t)
	box, _ := s.mgr.(fakeBoxes).GetByHostIP("172.30.5.2")
	box.State = vmm.StateRunning
	box.VCPUs, box.MemMB = 4, 4096
	box.RepoStatusAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	box.Repos = []host.RepoStatus{{
		Slug: "wandb/agentstream", Path: "/home/sparky/agentstream", Branch: "feat/x",
		Upstream: "origin/feat/x", Ahead: 2, Dirty: true, State: "stale",
	}}
	cpu, mem, rx, tx := 12.5, int64(1024), uint64(7000), uint64(4000)
	s.vitals = fakeVitalsReader{reading: host.Vitals{
		CPUSeconds: &cpu, MemUsedMB: &mem, NetRxBytes: &rx, NetTxBytes: &tx,
		ListeningPorts: []int{8080}, PortsChecked: true,
	}}

	human := request(s, "/self?format=text", "172.30.5.2", "172.30.5.1")
	for _, want := range []string{"state: running", "allocation: 4 vCPU, 4096 MiB memory", "cpu time: 12.5 seconds", "memory: 1024 MiB used (25%)", "listening ports: 8080", "wandb/agentstream", "/home/sparky/agentstream", "feat/x", "2 unpushed", "dirty"} {
		if !strings.Contains(human.Body.String(), want) {
			t.Errorf("human status missing %q:\n%s", want, human.Body)
		}
	}
	agent := request(s, "/self", "172.30.5.2", "172.30.5.1")
	for _, want := range []string{`"state":"running"`, `"vcpus":4`, `"mem_used_mb":1024`, `"repositories":{"wandb/agentstream"`, `"ahead":2`, `"dirty":true`} {
		if !strings.Contains(agent.Body.String(), want) {
			t.Errorf("JSON status missing %q: %s", want, agent.Body)
		}
	}
}

func TestSandboxCanPinAndUnpinItself(t *testing.T) {
	s := fixture(t)
	for _, tc := range []struct {
		path   string
		pinned bool
	}{
		{"/self/pin", true},
		{"/self/unpin", false},
	} {
		rec := requestMethod(s, http.MethodPost, tc.path, "172.30.5.2", "172.30.5.1")
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d: %s", tc.path, rec.Code, rec.Body)
		}
		var got selfDoc
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Sandbox != "alice-box" || got.Pinned != tc.pinned {
			t.Errorf("POST %s = %+v", tc.path, got)
		}
	}
	if rec := request(s, "/self", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pinned":false`) {
		t.Errorf("GET /self = %d %s", rec.Code, rec.Body)
	}
}

func TestSandboxCannotPinAnotherSandbox(t *testing.T) {
	s := fixture(t)
	rec := requestMethod(s, http.MethodPost, "/self/pin", "172.30.5.2", "172.30.9.1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-slot pin = %d, want 403", rec.Code)
	}
	if box, _ := s.mgr.(fakeBoxes).GetByHostIP("172.30.9.2"); box.Pinned {
		t.Fatal("alice pinned bob by dialing bob's gateway")
	}
}

func TestSandboxCanManageItsOwnRoutes(t *testing.T) {
	s := fixture(t)
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/self/visibility/public", `"visibility":"public"`},
		{"/self/visibility/private", `"visibility":"private"`},
		// ?port= narrows a visibility change to one guest port; the answer
		// echoes it back so the guest can see what it actually changed.
		{"/self/visibility/public?port=5173", `"port":5173`},
		{"/self/port/5173", `"port":5173`},
	} {
		rec := requestMethod(s, http.MethodPost, tc.path, "172.30.5.2", "172.30.5.1")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("POST %s = %d %s", tc.path, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{
		"/self/visibility/secret", "/self/port/0", "/self/port/65536", "/self/port/nope",
		"/self/visibility/public?port=0", "/self/visibility/public?port=99999",
		"/self/visibility/public?port=nope",
	} {
		if rec := requestMethod(s, http.MethodPost, path, "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400", path, rec.Code)
		}
	}
}

func TestSandboxCannotManageAnotherSandboxRoutes(t *testing.T) {
	s := fixture(t)
	for _, path := range []string{"/self/visibility/public", "/self/port/5173"} {
		if rec := requestMethod(s, http.MethodPost, path, "172.30.5.2", "172.30.9.1"); rec.Code != http.StatusForbidden {
			t.Errorf("cross-slot POST %s = %d, want 403", path, rec.Code)
		}
	}
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

func TestCallerUsesConfiguredGuestSubnet(t *testing.T) {
	s := fixture(t)
	configured, err := NewChecked(Options{
		Manager: fakeBoxes{
			"10.44.17.6": {
				Name: "alice-box", Owner: "alice", Image: "universal",
				HostIP: "10.44.17.6", KeyFP: "SHA256:aaa",
			},
		},
		Identity:        s.id,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultAudience: "https://hivemind.wandb.tools",
		GuestSubnet:     "10.44.16.9/20", // normalized to 10.44.16.0/20
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := request(configured, "/token", "10.44.17.6", "10.44.17.5"); rec.Code != http.StatusOK {
		t.Fatalf("configured subnet request = %d: %s", rec.Code, rec.Body)
	}
	if rec := request(configured, "/token", "10.44.17.6", "10.44.17.1"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-slot request = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec := request(configured, "/token", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusForbidden {
		t.Fatalf("default-subnet request = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestNewCheckedRejectsInvalidGuestSubnet(t *testing.T) {
	if _, err := NewChecked(Options{GuestSubnet: "10.0.0.0/31"}); err == nil {
		t.Fatal("NewChecked accepted a prefix with no /30 slot")
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

// A link somebody else vouched for is not the same fact as a link GitHub
// proved, and only the second one may ride in a token.
//
// docs/identity-federation-design.md tells relying parties this claim is a
// strong external anchor and invites them to write `claims.github == "x"` in a
// policy. An assertion-derived link would make it mean "somebody said so" — and
// when the somebody is the same service reading the policy, it would be
// authorizing against a fact it asserted. The console may still display such a
// link; a token may not carry it.
func TestGitHubClaimRequiresStrongProvenance(t *testing.T) {
	verified := time.Unix(0, 0).UTC()
	s := fixture(t)
	local := s.id.(Local)
	local.Users = fakeAccounts{"alice": {
		Handle: "alice", Status: "active", GitHubLogin: "alice-gh",
		GitHubVerifiedAt: &verified, GitHubVia: users.GitHubViaAssertion,
	}}
	s.id = local
	rec := request(s, "/token", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /token = %d", rec.Code)
	}
	if _, present := decodeClaims(t, rec.Body.String())["github"]; present {
		t.Error("a third party's word for a github account reached the token claims")
	}
}

// TestIdentityDocCarriesTheGitHubAccountNumber pins the fact the guest needs to
// write a commit address github.com will attribute.
//
// The number is in the DOCUMENT and deliberately not in the token. Nothing
// federates on it — a relying party matching an account number instead of the
// login this platform actually proved would be reading the wrong fact — but a
// guest cannot build `<id>+<login>@users.noreply.github.com` without it, and
// that is the only noreply form github.com links to an account created after
// 2017-07-18. See deploy/install-guest-identity.sh.
func TestIdentityDocCarriesTheGitHubAccountNumber(t *testing.T) {
	s := fixture(t)
	rec := request(s, "/identity", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /identity = %d", rec.Code)
	}
	var doc Doc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.GitHub != "alice-gh" || doc.GitHubID != 271676 {
		t.Errorf("identity doc = github %q id %d, want alice-gh / 271676", doc.GitHub, doc.GitHubID)
	}

	// It must not have reached the signed claims on its way there.
	tok := request(s, "/token", "172.30.5.2", "172.30.5.1")
	if tok.Code != http.StatusOK {
		t.Fatalf("GET /token = %d", tok.Code)
	}
	if _, present := decodeClaims(t, tok.Body.String())["github_id"]; present {
		t.Error("the account number widened the token's federation surface")
	}
}

// TestGitHubAccountNumberRidesTheSameGateAsTheLogin: a number recorded beside a
// link nobody proved is no better evidence than the login was, so it must be
// withheld on exactly the terms the login is. Otherwise a guest writes commits
// attributing them to an account this host never verified its owner controls.
func TestGitHubAccountNumberRidesTheSameGateAsTheLogin(t *testing.T) {
	verified := time.Unix(0, 0).UTC()
	s := fixture(t)
	local := s.id.(Local)
	local.Users = fakeAccounts{"alice": {
		Handle: "alice", Status: "active", GitHubLogin: "alice-gh", GitHubID: 271676,
		GitHubVerifiedAt: &verified, GitHubVia: users.GitHubViaAssertion,
	}}
	s.id = local
	rec := request(s, "/identity", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /identity = %d", rec.Code)
	}
	var doc Doc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.GitHub != "" || doc.GitHubID != 0 {
		t.Errorf("assertion-linked account leaked github %q id %d", doc.GitHub, doc.GitHubID)
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
