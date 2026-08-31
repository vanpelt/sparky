package xterm

// Gate tests: who may reach the terminal at all. Everything here stops before
// a PTY is opened, so none of it needs a VM — the fake manager below records
// whether EnsureRunning was even reached, which is how the cross-owner test
// proves a probe cannot wake someone else's sandbox.

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// fakeManager is the Attacher slice in memory. It counts the mutating call so
// tests can assert it was never reached, not merely that the answer was 404.
type fakeManager struct {
	mu      sync.Mutex
	boxes   map[string]*host.Sandbox
	resumes []string
	touches []string
}

func newFakeManager(boxes ...*host.Sandbox) *fakeManager {
	m := &fakeManager{boxes: map[string]*host.Sandbox{}}
	for _, b := range boxes {
		m.boxes[b.Name] = b
	}
	return m
}

func (m *fakeManager) Get(name string) (*host.Sandbox, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	return b, ok
}

func (m *fakeManager) EnsureReady(_ context.Context, name string) (*host.Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resumes = append(m.resumes, name)
	b, ok := m.boxes[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	b.State = vmm.StateRunning
	return b, nil
}

func (m *fakeManager) MarkActive(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touches = append(m.touches, name)
}

func (m *fakeManager) counts() (resumes, touches int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.resumes), len(m.touches)
}

// fakeAccounts satisfies edgeauth.Accounts; opsy is the operator.
type fakeAccounts map[string]users.User

func (f fakeAccounts) Get(handle string) (users.User, error) {
	u, ok := f[handle]
	if !ok {
		return users.User{}, os.ErrNotExist
	}
	return u, nil
}

const testDomain = "hivemind.tools"

type harness struct {
	h      *Handler
	mgr    *fakeManager
	signer *edgeauth.Signer
}

func newHarness(t *testing.T, boxes ...*host.Sandbox) *harness {
	t.Helper()
	return newHarnessWith(t, nil, boxes...)
}

// newHarnessWith is newHarness with the optional turbo capability wired. Left
// nil — which is every other test here — the handler serves no turbo button and
// its endpoint answers 501, which is the shape a host that has not enabled it
// should have.
func newHarnessWith(t *testing.T, turbo Turbocharger, boxes ...*host.Sandbox) *harness {
	t.Helper()
	hz := &harness{
		mgr:    newFakeManager(boxes...),
		signer: edgeauth.NewSigner([]byte("test-oidc-ikm")),
	}
	hz.h = New(Config{
		Sandboxes: hz.mgr,
		// Status matters: edgeauth.Require refuses a session whose account is
		// not active, so a fake that left it blank would model a disabled user.
		Accounts: fakeAccounts{
			"alice":   {Handle: "alice", Status: users.StatusActive, InvitedBy: "bob"},
			"mallory": {Handle: "mallory", Status: users.StatusActive, InvitedBy: "bob"},
			"opsy":    {Handle: "opsy", Status: users.StatusActive, InvitedBy: users.OperatorInviter},
		},
		Sessions: hz.signer,
		Turbo:    turbo,
		Domain:   testDomain,
		LoginURL: "https://login." + testDomain + "/",
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Track: func(_ string, _ SessionConn, isPTY bool) func() {
			if !isPTY {
				t.Errorf("browser terminal registered with isPTY=false: terminalRestore would be skipped")
			}
			return func() {}
		},
	})
	return hz
}

func (hz *harness) token(t *testing.T, handle string) string {
	t.Helper()
	tok, _, err := hz.signer.Mint(edgeauth.Identity{Handle: handle}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// get issues a request to host as handle ("" = unauthenticated).
func (hz *harness) get(t *testing.T, host, path, handle string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	if handle != "" {
		req.AddCookie(&http.Cookie{Name: edgeauth.CookieName, Value: hz.token(t, handle)})
	}
	rec := httptest.NewRecorder()
	hz.h.ServeHTTP(rec, req)
	return rec
}

func newBox(name, owner string, state vmm.State) *host.Sandbox {
	return &host.Sandbox{Name: name, Owner: owner, State: state, SSHAddr: "127.0.0.1:1", SSHUser: "sparky"}
}

// ---------------------------------------------------------------------------

func TestSandboxNameFromHost(t *testing.T) {
	hz := newHarness(t)
	cases := []struct {
		host string
		want string
		ok   bool
	}{
		{"demo-xterm." + testDomain, "demo", true},
		{"DEMO-XTERM." + strings.ToUpper(testDomain), "demo", true},
		{"demo-xterm." + testDomain + ":8443", "demo", true},
		// A sandbox name may itself contain hyphens — the generator emits
		// nothing else — so only the LAST one separates name from label.
		{"fuzzy-otter-2-xterm." + testDomain, "fuzzy-otter-2", true},
		// The zone is checked, so a forged Host cannot point the origin gate at
		// a hostname this server does not actually answer for.
		{"demo-xterm.evil.example", "", false},
		// Bare label, wrong reserved label, the sandbox's own front door, and
		// the console's host.
		{"xterm." + testDomain, "", false},
		{"demo-term." + testDomain, "", false},
		{"demo." + testDomain, "", false},
		{"my." + testDomain, "", false},
		// Deeper than one label: the edge claims these (a route could otherwise
		// be squatted at one) and hands them here to be refused. Answering
		// "demo" for a.demo-xterm would give one sandbox two origins, which is
		// exactly what the WebSocket origin gate assumes cannot happen.
		{"a.demo-xterm." + testDomain, "", false},
		// Charset: the name reaches host.Manager as a lookup key.
		{"-lead-xterm." + testDomain, "", false},
		{"Up_per-xterm." + testDomain, "", false},
		{"-xterm." + testDomain, "", false},
	}
	for _, c := range cases {
		got, ok := hz.h.SandboxName(c.host)
		if got != c.want || ok != c.ok {
			t.Errorf("SandboxName(%q) = %q, %v; want %q, %v", c.host, got, ok, c.want, c.ok)
		}
	}
}

func TestPageRequiresAuth(t *testing.T) {
	hz := newHarness(t, newBox("demo", "alice", vmm.StateRunning))
	host := "demo-xterm." + testDomain

	// A browser is sent to log in and comes back to exactly where it was.
	rec := hz.get(t, host, "/", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated page = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://login."+testDomain) || !strings.Contains(loc, "demo-xterm") {
		t.Errorf("login redirect = %q, want a return to the terminal", loc)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("denial not marked no-store")
	}

	// The WebSocket path must never answer a redirect: a handshake carries no
	// Accept: text/html, so it takes the 401 branch and the page can tell an
	// expired session from a dropped link.
	req := httptest.NewRequest(http.MethodGet, "https://"+host+"/ws", nil)
	req.Host = host
	rec = httptest.NewRecorder()
	hz.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /ws = %d, want 401", rec.Code)
	}

	if resumes, _ := hz.mgr.counts(); resumes != 0 {
		t.Errorf("an unauthenticated request reached EnsureRunning")
	}
}

func TestOwnerScoping(t *testing.T) {
	hz := newHarness(t, newBox("demo", "alice", vmm.StatePaused))
	host := "demo-xterm." + testDomain

	if rec := hz.get(t, host, "/", "alice"); rec.Code != http.StatusOK {
		t.Fatalf("owner page = %d, want 200", rec.Code)
	}
	// An operator is refused exactly like anyone else. The metadata surfaces let
	// operators through; a shell inside somebody's VM is a different class of
	// authority, and the other two doors onto this same bridge (ctlops.Get for
	// the REST endpoint, sshgw's owner compare for `ssh <name>.<domain>`) have
	// never granted it.
	if rec := hz.get(t, host, "/", "opsy"); rec.Code != http.StatusNotFound {
		t.Fatalf("operator page = %d, want 404 — the terminal is strict-owner", rec.Code)
	}

	// A stranger's answer must be byte-identical to a genuinely absent
	// sandbox's, or the difference between them is a name-existence oracle.
	foreign := hz.get(t, host, "/", "mallory")
	missing := hz.get(t, "nosuchbox-xterm."+testDomain, "/", "mallory")
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("cross-owner page = %d, want 404", foreign.Code)
	}
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing page = %d, want 404", missing.Code)
	}
	if got := foreign.Body.String(); got != "sparkbox: no sandbox named \"demo\"\n" {
		t.Errorf("cross-owner body = %q", got)
	}
	if strings.Contains(missing.Body.String(), "demo") {
		t.Errorf("missing body mentions another sandbox: %q", missing.Body.String())
	}

	// And the probe must not have woken anything: the ownership check runs
	// strictly before EnsureRunning.
	if resumes, _ := hz.mgr.counts(); resumes != 0 {
		t.Errorf("a cross-owner probe reached EnsureRunning (%d times)", resumes)
	}
}

func TestCrossOwnerWebSocketIsRefusedBeforeResume(t *testing.T) {
	// opsy is an operator. A PTY inside somebody else's VM hands over their
	// credentials, agent tokens and repos on a cookie alone, with no SSH key or
	// passkey re-proof — an authority no other door to this bridge grants, and
	// one the published OpenAPI description says this endpoint does not either.
	for _, handle := range []string{"mallory", "opsy"} {
		t.Run(handle, func(t *testing.T) {
			hz := newHarness(t, newBox("demo", "alice", vmm.StatePaused))
			host := "demo-xterm." + testDomain

			req := httptest.NewRequest(http.MethodGet, "https://"+host+"/ws", nil)
			req.Host = host
			req.Header.Set("Origin", "https://"+host)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			req.AddCookie(&http.Cookie{Name: edgeauth.CookieName, Value: hz.token(t, handle)})
			rec := httptest.NewRecorder()
			hz.h.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("cross-owner /ws as %s = %d, want 404", handle, rec.Code)
			}
			if resumes, _ := hz.mgr.counts(); resumes != 0 {
				t.Errorf("a cross-owner upgrade as %s reached EnsureRunning", handle)
			}
		})
	}
}

// TestOriginGate is the single most important table in this package: it is what
// stops a page anywhere on the zone — including a sandbox's own web route,
// which is arbitrary user code on a same-site origin — from opening an
// authenticated shell with the visitor's cookie.
func TestOriginGate(t *testing.T) {
	self := "demo-xterm." + testDomain
	cases := []struct {
		name   string
		origin string
		bearer bool
		want   int
	}{
		{"same origin", "https://" + self, false, http.StatusOK},
		{"another sandbox's page", "https://demo." + testDomain, false, http.StatusForbidden},
		{"another terminal", "https://other-xterm." + testDomain, false, http.StatusForbidden},
		{"the user console", "https://my." + testDomain, false, http.StatusForbidden},
		{"off-zone attacker", "https://evil.example", false, http.StatusForbidden},
		// A prefix match would let "https://demo-xterm.hivemind.tools.evil.example"
		// through; the comparison is exact.
		{"suffix trick", "https://" + self + ".evil.example", false, http.StatusForbidden},
		{"scheme downgrade", "http://" + self, false, http.StatusForbidden},
		// No Origin means no browser. It is allowed only with the credential a
		// cross-site browser request cannot carry.
		{"no origin, cookie only", "", false, http.StatusForbidden},
		{"no origin, bearer", "", true, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hz := newHarness(t, newBox("demo", "alice", vmm.StateRunning))
			req := httptest.NewRequest(http.MethodGet, "https://"+self+"/ws", nil)
			req.Host = self
			req.Header.Set("X-Forwarded-Proto", "https")
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			tok := hz.token(t, "alice")
			if c.bearer {
				req.Header.Set("Authorization", "Bearer "+tok)
			} else {
				req.AddCookie(&http.Cookie{Name: edgeauth.CookieName, Value: tok})
			}
			rec := httptest.NewRecorder()
			hz.h.ServeHTTP(rec, req)

			if c.want == http.StatusForbidden {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("origin %q = %d, want 403", c.origin, rec.Code)
				}
				if resumes, _ := hz.mgr.counts(); resumes != 0 {
					t.Errorf("a refused origin still resumed the sandbox")
				}
				return
			}
			// An accepted origin gets past the gate and fails at the hijack
			// instead, because httptest.ResponseRecorder is not a Hijacker.
			// That is the pass signal: the request reached websocket.Accept.
			if rec.Code == http.StatusForbidden {
				t.Fatalf("origin %q was refused: %s", c.origin, rec.Body.String())
			}
		})
	}
}

func TestHTTP2IsRefusedClearly(t *testing.T) {
	hz := newHarness(t, newBox("demo", "alice", vmm.StateRunning))
	self := "demo-xterm." + testDomain
	req := httptest.NewRequest(http.MethodGet, "https://"+self+"/ws", nil)
	req.Host = self
	req.ProtoMajor, req.ProtoMinor = 2, 0
	req.Header.Set("Origin", "https://"+self)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.AddCookie(&http.Cookie{Name: edgeauth.CookieName, Value: hz.token(t, "alice")})
	rec := httptest.NewRecorder()
	hz.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusHTTPVersionNotSupported {
		t.Fatalf("HTTP/2 upgrade = %d, want 505", rec.Code)
	}
}

func TestPageCarriesCSPAndTerminal(t *testing.T) {
	hz := newHarness(t, newBox("demo", "alice", vmm.StateRunning))
	rec := hz.get(t, "demo-xterm."+testDomain, "/", "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("page = %d", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "connect-src 'self'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("page is cacheable")
	}
	// Framing is refused twice over. frame-ancestors does NOT fall back to
	// default-src, so 'self' above covers nothing here — and the session cookie
	// is Domain=".<zone>" and SameSite=Lax, which means a page on any sandbox's
	// own web route (arbitrary user code, same-site) could otherwise embed this
	// one with the visitor's cookie attached, overlay a decoy, and typejack a
	// root shell.
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP %q does not refuse framing", csp)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"/assets/xterm.js", "/assets/xterm.css", "sparkbox.terminal.v1", "id=\"term\"", "id=\"proxy-link\""} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// The shared design system really was composed in, not left as a marker.
	if strings.Contains(body, "SHARED_CSS") || strings.Contains(body, "SHARED_JS") {
		t.Errorf("page still contains an unreplaced webui marker")
	}
}

func TestAssetsAreServedImmutably(t *testing.T) {
	hz := newHarness(t)
	// Ungated on purpose: no session, and they must still load.
	for _, c := range []struct{ path, ctype string }{
		{"/assets/xterm.js", "text/javascript"},
		{"/assets/xterm.css", "text/css"},
		{"/assets/addon-fit.js", "text/javascript"},
		{"/assets/addon-web-links.js", "text/javascript"},
	} {
		rec := hz.get(t, "demo-xterm."+testDomain, c.path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", c.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, c.ctype) {
			t.Errorf("%s content-type = %q, want %q", c.path, ct, c.ctype)
		}
		if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("%s is not cached immutably", c.path)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s is empty", c.path)
		}
	}
}

func TestUnknownPathIs404(t *testing.T) {
	hz := newHarness(t, newBox("demo", "alice", vmm.StateRunning))
	if rec := hz.get(t, "demo-xterm."+testDomain, "/terminal/ws", "alice"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path = %d, want 404", rec.Code)
	}
}

func TestRequestOrigin(t *testing.T) {
	// The scheme must follow the edge, not an assumption: a plain-HTTP dev
	// server has to compare against http://, and a TLS-terminating edge that
	// says https:// must win over the cleartext hop behind it.
	cases := []struct {
		tls  bool
		fwd  string
		want string
	}{
		{false, "", "http://demo-xterm.hivemind.tools"},
		{true, "", "https://demo-xterm.hivemind.tools"},
		{false, "https", "https://demo-xterm.hivemind.tools"},
		{true, "http", "http://demo-xterm.hivemind.tools"},
		{false, "https, http", "https://demo-xterm.hivemind.tools"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://demo-xterm.hivemind.tools/ws", nil)
		r.Host = "demo-xterm.hivemind.tools"
		r.TLS = nil
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if c.fwd != "" {
			r.Header.Set("X-Forwarded-Proto", c.fwd)
		}
		if got := requestOrigin(r); got != c.want {
			t.Errorf("requestOrigin(tls=%v, fwd=%q) = %q, want %q", c.tls, c.fwd, got, c.want)
		}
	}
}
