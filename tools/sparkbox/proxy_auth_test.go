package sparkbox_test

// Authenticated-forwarding tests over the mock stack: private routes redirect
// or 401 without a session, allow the owner/operators through with a session
// token, forward the visitor's identity as X-Forwarded-* headers (stripping any
// client-supplied copies), and honour a port named in the URL (…:PORT).

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/proxy"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
	xssh "golang.org/x/crypto/ssh"
)

type authStack struct {
	mgr    *host.Manager
	store  *routes.Store
	users  *users.Store
	signer *edgeauth.Signer
	proxy  http.Handler
}

func newAuthStack(t *testing.T) *authStack {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	hostKey, err := sshgw.LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatal(err)
	}
	upstreamKey, err := sshgw.LoadOrCreateKey(dir, "gateway_upstream_key")
	if err != nil {
		t.Fatal(err)
	}
	store, err := routes.Open(filepath.Join(dir, "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	userStore, err := users.Open(filepath.Join(dir, "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { userStore.Close() })

	driver := mock.New(dir, hostKey)
	t.Cleanup(func() { driver.Close() })
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver,
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
		Routes: store,
	})
	if err != nil {
		t.Fatal(err)
	}

	signer := edgeauth.NewSigner([]byte("test-key-material"))
	login, err := edgeauth.NewLoginHandler(edgeauth.LoginConfig{
		Signer: signer, Domain: proxyDomain, Gateway: proxyDomain, Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	px := proxy.New(mgr, store, proxyDomain, log)
	px.SetAuth("login", login.Handler(), signer, userStore)

	return &authStack{mgr: mgr, store: store, users: userStore, signer: signer, proxy: px}
}

// mkUser creates a user with a throwaway key. operator marks it operator-seeded.
func (as *authStack) mkUser(t *testing.T, handle, email string, operator bool) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := xssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	invitedBy := "someoneelse"
	if operator {
		invitedBy = users.OperatorInviter
	}
	if err := as.users.Create(handle, pub, "", "seed", invitedBy); err != nil {
		t.Fatal(err)
	}
	if email != "" {
		if err := as.users.SetEmail(handle, email); err != nil {
			t.Fatal(err)
		}
	}
}

func (as *authStack) token(t *testing.T, handle, email string) string {
	t.Helper()
	tok, _, err := as.signer.Mint(edgeauth.Identity{Handle: handle, Email: email}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// req drives one request. host may include a :port. opts mutate the request
// (cookie, bearer, Accept, spoofed headers) before it is served.
func (as *authStack) req(t *testing.T, host string, opts ...func(*http.Request)) *http.Response {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	r.Host = host
	for _, o := range opts {
		o(r)
	}
	rec := httptest.NewRecorder()
	as.proxy.ServeHTTP(rec, r)
	return rec.Result()
}

func withCookie(tok string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: edgeauth.CookieName, Value: tok}) }
}
func withBearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}
func asBrowser(r *http.Request) { r.Header.Set("Accept", "text/html,*/*;q=0.8") }

// echoBackend replies with the identity headers it received, so tests can
// assert what the edge forwarded.
func echoBackend(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "user=%q email=%q preferred=%q",
			r.Header.Get("X-Forwarded-User"), r.Header.Get("X-Forwarded-Email"),
			r.Header.Get("X-Forwarded-Preferred-Username"))
	})}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func (as *authStack) route(t *testing.T, sub, sandbox, owner string, port int, vis string) {
	t.Helper()
	if err := as.store.Upsert(routes.Route{
		Subdomain: sub, Sandbox: sandbox, Owner: owner, Port: port, Visibility: vis,
	}); err != nil {
		t.Fatal(err)
	}
	// Upsert sets visibility only on first insert; mgr.Create may have already
	// laid down a private default route for this subdomain, so force it here the
	// same way `ctl@ share` does.
	if err := as.store.SetVisibility(sub, vis); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateRouteRedirectsBrowserAndChallengesAPI(t *testing.T) {
	as := newAuthStack(t)
	ctx := context.Background()
	if _, err := as.mgr.Create(ctx, "secret", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	port := echoBackend(t)
	as.route(t, "secret", "secret", "alice", port, routes.VisibilityPrivate)

	// Browser with no session -> 303 to the login subdomain, carrying return.
	resp := as.req(t, "secret.hivemind.tools", asBrowser)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "https://login.hivemind.tools/") || !strings.Contains(loc, "return=") {
		t.Fatalf("unexpected redirect target %q", loc)
	}

	// API client (no text/html) -> 401, not a redirect.
	resp = as.req(t, "secret.hivemind.tools")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for API client, got %d", resp.StatusCode)
	}
}

func TestPrivateRouteAllowsOwnerAndForwardsIdentity(t *testing.T) {
	as := newAuthStack(t)
	ctx := context.Background()
	as.mkUser(t, "alice", "alice@wandb.com", false)
	if _, err := as.mgr.Create(ctx, "secret", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	port := echoBackend(t)
	as.route(t, "secret", "secret", "alice", port, routes.VisibilityPrivate)

	// Owner via cookie, and spoofed identity headers that MUST be stripped.
	resp := as.req(t, "secret.hivemind.tools",
		withCookie(as.token(t, "alice", "alice@wandb.com")),
		func(r *http.Request) { r.Header.Set("X-Forwarded-User", "root") })
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for owner, got %d (%s)", resp.StatusCode, body)
	}
	want := `user="alice" email="alice@wandb.com" preferred="alice"`
	if body != want {
		t.Fatalf("identity not forwarded correctly.\n got: %s\nwant: %s", body, want)
	}

	// Same via Authorization: Bearer (the programmatic path).
	resp = as.req(t, "secret.hivemind.tools", withBearer(as.token(t, "alice", "alice@wandb.com")))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer path: expected 200, got %d", resp.StatusCode)
	}
}

func TestPrivateRouteOperatorAllowedStrangerForbidden(t *testing.T) {
	as := newAuthStack(t)
	ctx := context.Background()
	as.mkUser(t, "alice", "", false)
	as.mkUser(t, "opsy", "ops@wandb.com", true)
	as.mkUser(t, "bob", "bob@wandb.com", false)
	if _, err := as.mgr.Create(ctx, "secret", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	port := echoBackend(t)
	as.route(t, "secret", "secret", "alice", port, routes.VisibilityPrivate)

	// An operator who doesn't own it gets in.
	if resp := as.req(t, "secret.hivemind.tools", withCookie(as.token(t, "opsy", "ops@wandb.com"))); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator should be allowed, got %d", resp.StatusCode)
	}
	// A signed-in stranger is 403 (not redirected — they are authenticated).
	if resp := as.req(t, "secret.hivemind.tools", asBrowser, withCookie(as.token(t, "bob", "bob@wandb.com"))); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stranger should be 403, got %d", resp.StatusCode)
	}
}

func TestPublicRouteServesUngatedWithNoIdentity(t *testing.T) {
	as := newAuthStack(t)
	ctx := context.Background()
	if _, err := as.mgr.Create(ctx, "demo", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	port := echoBackend(t)
	as.route(t, "demo", "demo", "alice", port, routes.VisibilityPublic)

	resp := as.req(t, "demo.hivemind.tools",
		func(r *http.Request) { r.Header.Set("X-Forwarded-User", "spoofed") })
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public route should serve, got %d", resp.StatusCode)
	}
	// No session -> no identity forwarded, and the spoofed header is stripped.
	if want := `user="" email="" preferred=""`; body != want {
		t.Fatalf("public route leaked identity: %s", body)
	}
}

// cookieEchoBackend replies with the raw Cookie header it received, so tests
// can assert what the edge forwarded.
func cookieEchoBackend(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "cookie=%q", r.Header.Get("Cookie"))
	})}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func TestSessionCookieNeverReachesUpstream(t *testing.T) {
	as := newAuthStack(t)
	ctx := context.Background()
	as.mkUser(t, "alice", "", false)
	if _, err := as.mgr.Create(ctx, "secret", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	port := cookieEchoBackend(t)
	as.route(t, "secret", "secret", "alice", port, routes.VisibilityPrivate)

	// The session cookie authenticates the visitor but must be stripped before
	// the request reaches the guest; the app's own cookie passes through.
	resp := as.req(t, "secret.hivemind.tools",
		withCookie(as.token(t, "alice", "")),
		func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "app_pref", Value: "dark"}) })
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for owner, got %d (%s)", resp.StatusCode, body)
	}
	if want := `cookie="app_pref=dark"`; body != want {
		t.Fatalf("session cookie leaked upstream (or app cookie lost).\n got: %s\nwant: %s", body, want)
	}

	// A public route with only the session cookie forwards no Cookie header at
	// all, not an empty one.
	if _, err := as.mgr.Create(ctx, "demo", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	as.route(t, "demo", "demo", "alice", port, routes.VisibilityPublic)
	resp = as.req(t, "demo.hivemind.tools", withCookie(as.token(t, "alice", "")))
	if body := readBody(t, resp); body != `cookie=""` {
		t.Fatalf("public route leaked the session cookie: %s", body)
	}
}

func TestCookieStripPreservesGuestCookiesByteForByte(t *testing.T) {
	as := newAuthStack(t)
	ctx := context.Background()
	if _, err := as.mgr.Create(ctx, "demo", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	port := cookieEchoBackend(t)
	as.route(t, "demo", "demo", "alice", port, routes.VisibilityPublic)

	// Pairs Go's cookie parser cannot round-trip (raw UTF-8 value, non-token
	// name, unquoted space) must survive the session-pair removal untouched.
	raw := "greeting=héllo; " + edgeauth.CookieName + "=" + as.token(t, "alice", "") + "; {legacy}=1; v=a b"
	resp := as.req(t, "demo.hivemind.tools",
		func(r *http.Request) { r.Header.Set("Cookie", raw) })
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", resp.StatusCode, body)
	}
	if want := `cookie="greeting=héllo; {legacy}=1; v=a b"`; body != want {
		t.Fatalf("guest cookies mangled by session strip.\n got: %s\nwant: %s", body, want)
	}

	// A header without the session pair passes through byte-for-byte, odd
	// spacing and all.
	raw = "greeting=héllo;  {legacy}=1;v=a b"
	resp = as.req(t, "demo.hivemind.tools",
		func(r *http.Request) { r.Header.Set("Cookie", raw) })
	if body := readBody(t, resp); body != `cookie="`+raw+`"` {
		t.Fatalf("session-free Cookie header not preserved verbatim: %s", body)
	}
}

// authEchoBackend replies with the raw Authorization header it received, so
// tests can assert what the edge forwarded.
func authEchoBackend(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "auth=%q", r.Header.Get("Authorization"))
	})}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func TestSessionBearerTokenNeverReachesUpstream(t *testing.T) {
	as := newAuthStack(t)
	ctx := context.Background()
	as.mkUser(t, "alice", "", false)
	if _, err := as.mgr.Create(ctx, "secret", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	port := authEchoBackend(t)
	as.route(t, "secret", "secret", "alice", port, routes.VisibilityPrivate)

	// The bearer session token authenticates the visitor but must be stripped
	// before the request reaches the guest — it is the same zone-wide
	// credential as the cookie.
	resp := as.req(t, "secret.hivemind.tools", withBearer(as.token(t, "alice", "")))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for owner, got %d (%s)", resp.StatusCode, body)
	}
	if want := `auth=""`; body != want {
		t.Fatalf("session bearer token leaked upstream.\n got: %s\nwant: %s", body, want)
	}

	// A guest app's own bearer token is not ours to strip: authenticate via
	// cookie and the unrelated Authorization header passes through untouched.
	resp = as.req(t, "secret.hivemind.tools",
		withCookie(as.token(t, "alice", "")),
		withBearer("guest-app-token"))
	if body := readBody(t, resp); body != `auth="Bearer guest-app-token"` {
		t.Fatalf("guest bearer token mangled: %s", body)
	}

	// A stale or forged spk_v1. token is still a session token to strip, even
	// on a public route where the gate never ran.
	if _, err := as.mgr.Create(ctx, "demo", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	as.route(t, "demo", "demo", "alice", port, routes.VisibilityPublic)
	resp = as.req(t, "demo.hivemind.tools", withBearer("spk_v1.bogus.bogus"))
	if body := readBody(t, resp); body != `auth=""` {
		t.Fatalf("spk_v1-prefixed token leaked upstream: %s", body)
	}
}

func TestAnyPortURLOverridesRoutePort(t *testing.T) {
	as := newAuthStack(t)
	ctx := context.Background()
	if _, err := as.mgr.Create(ctx, "multi", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	// Route's configured port points nowhere; the URL names the live one.
	livePort := echoBackend(t)
	as.route(t, "multi", "multi", "alice", 9999, routes.VisibilityPublic)

	resp := as.req(t, fmt.Sprintf("multi.hivemind.tools:%d", livePort))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("any-port URL should forward to the port in the host, got %d", resp.StatusCode)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
