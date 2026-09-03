package userconsole

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/domainmeta"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghuser"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netrules"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/templates"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

const testDomain = "hivemind.tools"

// fakeAccounts satisfies edgeauth.Accounts without a database.
type fakeAccounts map[string]users.User

func (f fakeAccounts) Get(handle string) (users.User, error) {
	u, ok := f[handle]
	if !ok {
		return users.User{}, users.ErrNoSuchUser
	}
	return u, nil
}

// syncRecorder satisfies OwnerSyncer and records which owners were synced, so
// tests can assert the change-time push fired.
type syncRecorder struct {
	mu     sync.Mutex
	owners []string
}

func (s *syncRecorder) SyncOwner(_ context.Context, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners = append(s.owners, owner)
}

func (s *syncRecorder) count(owner string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, o := range s.owners {
		if o == owner {
			n++
		}
	}
	return n
}

type testConsole struct {
	h        http.Handler
	handler  *Handler // the same console before its mux, for the Set* seams
	mgr      *host.Manager
	routes   *routes.Store
	secrets  *secrets.Store
	netrules *netrules.Store
	repos    *repos.Store
	signer   *edgeauth.Signer
	sync     *syncRecorder
}

// unmeteredPlane is a NetPlane whose machines all run without sluice: every
// read refuses with the typed KindDisabled a real one raises, which statusFor
// turns into the 501 this endpoint has always answered. Pushes are accepted
// silently, exactly as a fleet with nothing to enforce against does.
type unmeteredPlane struct{}

func (unmeteredPlane) NetUsage(context.Context, string) (netpush.VMUsage, error) {
	return netpush.VMUsage{}, nodelink.NoSluice("test-box")
}
func (unmeteredPlane) PushNet(context.Context) error { return nil }
func (unmeteredPlane) NetMetered(string) bool        { return false }

func newTestConsole(t *testing.T) *testConsole {
	t.Helper()
	return newTestConsoleDomain(t, testDomain, "xterm")
}

// newTestConsoleDomain builds the console for an explicit --proxy-domain and
// --xterm-subdomain, so tests can drive operator-supplied variants like a
// leading dot, or a host with browser terminals switched off.
func newTestConsoleDomain(t *testing.T, domain, xtermSub string) *testConsole {
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
	driver := mock.New(dir, hostKey)
	t.Cleanup(func() { driver.Close() })
	routeStore, err := routes.Open(filepath.Join(dir, "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { routeStore.Close() })
	secretStore, err := secrets.Open(filepath.Join(dir, "secrets.db"), secrets.DeriveKEK([]byte("test-ikm")), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secretStore.Close() })
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver,
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
		Routes: routeStore, Tags: secretStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	signer := edgeauth.NewSigner([]byte("test-oidc-ikm"))
	// Status matters: edgeauth.Require refuses a session whose account is not
	// active, so a fake that left it blank would model three disabled users.
	linkedAt := time.Now().Add(-time.Hour)
	accounts := fakeAccounts{
		// alice carries a GitHub link because the Repos panel's App probe binds
		// forward from the handle to the stored id — an account with none is a
		// different test (and a refusal), not the default one.
		// alice carries a STRONG link: the Repos panel binds forward from the
		// handle to the stored id for its App probe, and ctlops.AttachGate
		// refuses to attach at all without a verified, directly-proved link.
		// An account with a weak or absent link is a different test (and a
		// refusal), not the default one — see TestRepoAttachNeedsAStrongLink.
		"alice": {Handle: "alice", Status: users.StatusActive, InvitedBy: "bob",
			GitHubLogin: "alice-gh", GitHubID: 7,
			GitHubVia: users.GitHubViaKeys, GitHubVerifiedAt: &linkedAt},
		"mallory": {Handle: "mallory", Status: users.StatusActive, InvitedBy: "bob",
			GitHubLogin: "mallory-gh", GitHubID: 9,
			GitHubVia: users.GitHubViaKeys, GitHubVerifiedAt: &linkedAt},
		// weakly carries an assertion-provenance link: displayable, but not
		// proved directly with GitHub, so it may not attach a repository.
		"weakly": {Handle: "weakly", Status: users.StatusActive, InvitedBy: "bob",
			GitHubLogin: "weakly-gh", GitHubID: 11,
			GitHubVia: users.GitHubViaAssertion, GitHubVerifiedAt: &linkedAt},
		// unlinked has no GitHub account at all.
		"unlinked": {Handle: "unlinked", Status: users.StatusActive, InvitedBy: "bob"},
		"opsy":     {Handle: "opsy", Status: users.StatusActive, InvitedBy: users.OperatorInviter},
	}
	rec := &syncRecorder{}
	// netrules shares the same DB file as secrets (that owns sandbox_tags).
	netStore, err := netrules.Open(filepath.Join(dir, "secrets.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netStore.Close() })
	// repos joins the same sandbox_tags table, so it must open the same file
	// the tags live in — on a different one every ReposForSandbox join comes
	// back empty, silently and with no error.
	repoStore, err := repos.Open(filepath.Join(dir, "secrets.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repoStore.Close() })
	// A favicon cache with a stub fetcher so tests never hit the network.
	favicons := domainmeta.NewFaviconCache(filepath.Join(dir, "favicons"),
		func(ctx context.Context, reg string) ([]byte, string, bool) { return testPNG, "image/png", true })
	// A net plane whose machines run no sluice, so bandwidth is 501 — the same
	// answer the old no-socket syncer gave, now raised by the machine holding
	// the sandbox rather than by the console.
	h := New(mgr, routeStore, secretStore, netStore, repoStore, unmeteredPlane{}, favicons, accounts, signer, rec, "my", domain, xtermSub, false, log)
	return &testConsole{h: h.Handler(), handler: h, mgr: mgr, routes: routeStore, secrets: secretStore, netrules: netStore, repos: repoStore, signer: signer, sync: rec}
}

// session mints a cookie for handle, the browser path the SPA rides.
func (tc *testConsole) session(t *testing.T, handle string) *http.Cookie {
	t.Helper()
	tok, _, err := tc.signer.Mint(edgeauth.Identity{Handle: handle}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: edgeauth.CookieName, Value: tok}
}

// do issues an API request as handle ("" = unauthenticated). Authenticated
// requests carry the SPA's CSRF header so mutations pass the gate.
func (tc *testConsole) do(t *testing.T, method, path, handle string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if handle != "" {
		req.AddCookie(tc.session(t, handle))
		req.Header.Set(edgeauth.MutationHeader, "1")
	}
	rec := httptest.NewRecorder()
	tc.h.ServeHTTP(rec, req)
	return rec
}

func (tc *testConsole) create(t *testing.T, name, owner string) *host.Sandbox {
	t.Helper()
	box, err := tc.mgr.Create(context.Background(), name, owner, "ubuntu", 1, 512)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// apiEndpoints is every API route the console serves, with a representative
// path. The auth-gate test drives all of them.
var apiEndpoints = []struct{ method, path string }{
	{"GET", "/api/me"},
	{"POST", "/api/logout"},
	{"GET", "/api/machines"},
	{"GET", "/api/usage"},
	{"POST", "/api/machines/somebox/pause"},
	{"POST", "/api/machines/somebox/resume"},
	{"DELETE", "/api/machines/somebox"},
	{"POST", "/api/machines/somebox/archive"},
	{"POST", "/api/machines/somebox/pin"},
	{"POST", "/api/machines/somebox/turbo"},
	{"POST", "/api/machines/somebox/unpin"},
	{"POST", "/api/machines/somebox/snapshot"},
	{"POST", "/api/machines/somebox/rename"},
	{"POST", "/api/machines/somebox/reboot"},
	{"POST", "/api/machines/somebox/port"},
	{"PUT", "/api/machines/somebox/tags"},
	{"POST", "/api/routes/somebox/visibility"},
	{"GET", "/api/snapshots"},
	{"POST", "/api/snapshots/somesnap/fork"},
	{"POST", "/api/snapshots/somesnap/delete"},
	{"GET", "/api/secrets"},
	{"PUT", "/api/secrets/SOME_SECRET"},
	{"DELETE", "/api/secrets/SOME_SECRET"},
	{"GET", "/api/network-rules"},
	{"PUT", "/api/network-rules/somerule"},
	{"DELETE", "/api/network-rules/somerule"},
	{"GET", "/api/repos"},
	{"POST", "/api/repos/wandb%2Fhivemind/authorize"},
	{"PUT", "/api/repos/wandb%2Fhivemind"},
	{"DELETE", "/api/repos/wandb%2Fhivemind"},
	{"GET", "/github/repo/callback"},
	{"GET", "/api/machines/somebox/bandwidth"},
	{"GET", "/api/favicon"},
}

func TestEveryEndpointRequiresAuth(t *testing.T) {
	tc := newTestConsole(t)
	for _, ep := range apiEndpoints {
		rec := tc.do(t, ep.method, ep.path, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated: status %d, want 401", ep.method, ep.path, rec.Code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s: missing Cache-Control: no-store", ep.method, ep.path)
		}
	}

	// The SPA itself is always served; it renders the sign-in state on 401.
	rec := tc.do(t, "GET", "/", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sparkbox") {
		t.Fatalf("index page: status %d", rec.Code)
	}
}

func TestMachinesAreNewestActivityFirst(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "alpha", "alice")
	time.Sleep(time.Millisecond)
	tc.create(t, "zulu", "alice")

	var views []sandboxView
	rec := tc.do(t, "GET", "/api/machines", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("machines status = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].Name != "zulu" || views[1].Name != "alpha" {
		t.Fatalf("machine order = %+v, want newest activity first", views)
	}
}

// TestCrossOwnerIs404 drives every owner-scoped endpoint against alice's
// resources as mallory: each must answer 404 with the not-found body — never
// 403, which would confirm the name exists.
func TestCrossOwnerIs404(t *testing.T) {
	tc := newTestConsole(t)
	ctx := context.Background()
	tc.create(t, "alices-box", "alice")
	if _, err := tc.mgr.Snapshot(ctx, "alices-box", "alices-snap", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := tc.secrets.PutSecret("alice", "ALICE_TOKEN", "hunter2", nil); err != nil {
		t.Fatal(err)
	}
	if err := tc.repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"hm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.mgr.EnsureReady(ctx, "alices-box"); err != nil {
		t.Fatal(err)
	}

	attempts := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/machines/alices-box/pause", nil},
		{"POST", "/api/machines/alices-box/resume", nil},
		{"POST", "/api/machines/alices-box/archive", nil},
		{"POST", "/api/machines/alices-box/pin", nil},
		{"POST", "/api/machines/alices-box/unpin", nil},
		{"POST", "/api/machines/alices-box/snapshot", map[string]string{"snapshot_name": "steal"}},
		{"POST", "/api/machines/alices-box/rename", map[string]string{"new_name": "mine-now"}},
		{"POST", "/api/machines/alices-box/reboot", nil},
		{"DELETE", "/api/machines/alices-box", nil},
		{"POST", "/api/machines/alices-box/port", map[string]int{"port": 8080}},
		{"PUT", "/api/machines/alices-box/tags", map[string][]string{"tags": {"prod"}}},
		{"POST", "/api/routes/alices-box/visibility", map[string]string{"visibility": "public"}},
		{"POST", "/api/snapshots/alices-snap/fork", map[string]string{"name": "forked"}},
		{"POST", "/api/snapshots/alices-snap/delete", nil},
		{"DELETE", "/api/secrets/ALICE_TOKEN", nil},
		{"DELETE", "/api/repos/wandb%2Fhivemind", nil},
	}
	for _, a := range attempts {
		rec := tc.do(t, a.method, a.path, "mallory", a.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as mallory: status %d, want 404 (body %s)", a.method, a.path, rec.Code, rec.Body)
		}
	}

	// Nothing was actually touched: the box still runs under its name, the
	// snapshot and secret survive.
	if box, ok := tc.mgr.Get("alices-box"); !ok || box.State != vmm.StateRunning {
		t.Fatalf("alice's box was disturbed: %+v", box)
	}
	if _, ok := tc.mgr.SnapshotByName("alice", "alices-snap"); !ok {
		t.Fatal("alice's snapshot disappeared")
	}
	if metas, err := tc.secrets.ListSecrets("alice"); err != nil || len(metas) != 1 {
		t.Fatalf("alice's secret disappeared: %v %v", metas, err)
	}
	if list, err := tc.repos.ListRepos("alice"); err != nil || len(list) != 1 {
		t.Fatalf("alice's repo attachment disappeared: %v %v", list, err)
	}

	// Listing endpoints are owner-filtered rather than 404: mallory sees none
	// of alice's resources.
	var boxes []json.RawMessage
	if err := json.Unmarshal(tc.do(t, "GET", "/api/machines", "mallory", nil).Body.Bytes(), &boxes); err != nil || len(boxes) != 0 {
		t.Fatalf("mallory should list no machines, got %d (%v)", len(boxes), err)
	}
	var snaps []json.RawMessage
	if err := json.Unmarshal(tc.do(t, "GET", "/api/snapshots", "mallory", nil).Body.Bytes(), &snaps); err != nil || len(snaps) != 0 {
		t.Fatalf("mallory should list no snapshots, got %d (%v)", len(snaps), err)
	}
	var metas []json.RawMessage
	if err := json.Unmarshal(tc.do(t, "GET", "/api/secrets", "mallory", nil).Body.Bytes(), &metas); err != nil || len(metas) != 0 {
		t.Fatalf("mallory should list no secrets, got %d (%v)", len(metas), err)
	}

	// An operator bypasses the owner check on sandbox actions.
	if rec := tc.do(t, "POST", "/api/machines/alices-box/pause", "opsy", nil); rec.Code != http.StatusOK {
		t.Fatalf("operator pause: status %d (%s)", rec.Code, rec.Body)
	}
}

// TestMutationCSRFGate: a valid session cookie alone (what a cross-site fetch
// carries) must not move state; the SPA's custom header or a first-party
// Origin must.
func TestMutationCSRFGate(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "webby", "alice")

	post := func(hdr func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/machines/webby/pause", nil)
		req.AddCookie(tc.session(t, "alice"))
		if hdr != nil {
			hdr(req)
		}
		rec := httptest.NewRecorder()
		tc.h.ServeHTTP(rec, req)
		return rec
	}

	if rec := post(nil); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie-only mutation: status %d, want 403", rec.Code)
	}
	rec := post(func(r *http.Request) { r.Header.Set("Origin", "https://evil."+testDomain) })
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign-origin mutation: status %d, want 403", rec.Code)
	}
	if box, _ := tc.mgr.Get("webby"); box.State != vmm.StateRunning {
		t.Fatal("CSRF-refused request must not change state")
	}

	rec = post(func(r *http.Request) { r.Header.Set("Origin", "https://my."+testDomain) })
	if rec.Code != http.StatusOK {
		t.Fatalf("first-party origin mutation: status %d (%s)", rec.Code, rec.Body)
	}
	rec = post(func(r *http.Request) { r.Header.Set(edgeauth.MutationHeader, "1") })
	if rec.Code != http.StatusOK {
		t.Fatalf("header-proved mutation: status %d (%s)", rec.Code, rec.Body)
	}
}

// TestSecretValuesNeverInGETBodies: after storing a secret and wiring it to a
// running tagged sandbox, no GET on the API may carry the value.
func TestSecretValuesNeverInGETBodies(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "webby", "alice")
	const canary = "sw0rdfish-t0psecret-value"

	rec := tc.do(t, "PUT", "/api/secrets/API_KEY", "alice", map[string]any{"value": canary, "tags": []string{"prod"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("put secret: status %d (%s)", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), canary) {
		t.Fatal("put secret response echoed the value")
	}
	rec = tc.do(t, "PUT", "/api/machines/webby/tags", "alice", map[string][]string{"tags": {"prod"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set tags: status %d (%s)", rec.Code, rec.Body)
	}

	for _, path := range []string{"/api/me", "/api/machines", "/api/snapshots", "/api/secrets"} {
		rec := tc.do(t, "GET", path, "alice", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d (%s)", path, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), canary) {
			t.Errorf("GET %s leaked the secret value", path)
		}
	}

	// The metadata list still describes the secret.
	var metas []secrets.SecretMeta
	if err := json.Unmarshal(tc.do(t, "GET", "/api/secrets", "alice", nil).Body.Bytes(), &metas); err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Name != "API_KEY" || len(metas[0].Tags) != 1 {
		t.Fatalf("unexpected secret metadata: %+v", metas)
	}
}

// TestPortChangePreservesVisibility: making a route public and then moving
// its port must not silently re-privatise it (Upsert updates only the port).
func TestPortChangePreservesVisibility(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "webby", "alice")

	rec := tc.do(t, "POST", "/api/routes/webby/visibility", "alice", map[string]string{"visibility": "public"})
	if rec.Code != http.StatusOK {
		t.Fatalf("set visibility: status %d (%s)", rec.Code, rec.Body)
	}
	rec = tc.do(t, "POST", "/api/machines/webby/port", "alice", map[string]int{"port": 9090})
	if rec.Code != http.StatusOK {
		t.Fatalf("set port: status %d (%s)", rec.Code, rec.Body)
	}

	rt, found, err := tc.routes.GetBySubdomain("webby")
	if err != nil || !found {
		t.Fatalf("route lookup: found=%v err=%v", found, err)
	}
	if rt.Port != 9090 {
		t.Fatalf("port = %d, want 9090", rt.Port)
	}
	if rt.Visibility != routes.VisibilityPublic {
		t.Fatalf("visibility = %q after port change, want public", rt.Visibility)
	}

	// And the list reflects both.
	var views []struct {
		Name   string        `json:"name"`
		Routes []routeStatus `json:"routes"`
	}
	if err := json.Unmarshal(tc.do(t, "GET", "/api/machines", "alice", nil).Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	// The strip also carries whatever the box is listening on, which is not
	// this test's business — but the default port is always its first entry.
	if len(views) != 1 || len(views[0].Routes) == 0 {
		t.Fatalf("unexpected machine list: %+v", views)
	}
	if r := views[0].Routes[0]; r.Port != 9090 || r.Visibility != "public" || !r.Default {
		t.Fatalf("unexpected route view: %+v", r)
	}

	// A bad visibility value is rejected up front.
	rec = tc.do(t, "POST", "/api/routes/webby/visibility", "alice", map[string]string{"visibility": "friends-only"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad visibility: status %d, want 400", rec.Code)
	}
}

// TestUserConsoleDestroy: the owner can remove their machine, its routes are
// swept with it, and it drops off the list. (Object-storage cleanup of an
// archived box is exercised in host/archive_test.go's TestDestroyArchivedDropsObject.)
func TestUserConsoleDestroy(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "webby", "alice")

	rec := tc.do(t, "DELETE", "/api/machines/webby", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("destroy: status %d (%s)", rec.Code, rec.Body)
	}
	if _, ok := tc.mgr.Get("webby"); ok {
		t.Fatal("machine still present after destroy")
	}
	// The default route the manager created is gone too.
	if _, found, err := tc.routes.GetBySubdomain("webby"); err != nil || found {
		t.Fatalf("route survived destroy: found=%v err=%v", found, err)
	}
	// It no longer appears in the owner's machine list.
	var views []json.RawMessage
	if err := json.Unmarshal(tc.do(t, "GET", "/api/machines", "alice", nil).Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 0 {
		t.Fatalf("destroyed machine still listed: %d", len(views))
	}
	// Removing a machine that isn't there answers 404 with the shared body.
	if rec := tc.do(t, "DELETE", "/api/machines/webby", "alice", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("destroy missing: status %d, want 404", rec.Code)
	}
}

func TestMachineListShowsTagsStatsAndState(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "webby", "alice")
	if err := tc.secrets.SetTags("webby", "alice", []string{"prod", "web"}); err != nil {
		t.Fatal(err)
	}

	var views []sandboxView
	if err := json.Unmarshal(tc.do(t, "GET", "/api/machines", "alice", nil).Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "webby" {
		t.Fatalf("unexpected list: %+v", views)
	}
	v := views[0]
	if v.State != vmm.StateRunning {
		t.Fatalf("new sandbox should be running, got %s", v.State)
	}
	if len(v.Tags) != 2 || v.Tags[0] != "prod" || v.Tags[1] != "web" {
		t.Fatalf("tags = %v, want [prod web]", v.Tags)
	}
	// The mock driver's CPUStatser is a monotonic synthetic counter, so a
	// running box always reports cumulative cpu_seconds.
	if v.CPUSeconds == nil {
		t.Fatal("running sandbox should report cpu_seconds")
	}
	// The default port heads the strip and is private until somebody says
	// otherwise. Anything after it is whatever the box is listening on, which
	// depends on the machine running the test.
	if len(v.Routes) == 0 || !v.Routes[0].Default || v.Routes[0].Visibility != routes.VisibilityPrivate {
		t.Fatalf("expected a private default port first, got %+v", v.Routes)
	}
	for _, r := range v.Routes[1:] {
		if r.Visibility != routes.VisibilityPrivate {
			t.Errorf("a port nobody configured is %s, want private: %+v", r.Visibility, r)
		}
	}
}

// Visibility is per port: the console can open one without touching the rest,
// a port can be listed before anything is on it, and forgetting it takes it
// off the strip without ever having exposed it.
func TestConsoleSettlesVisibilityPerPort(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "webby", "alice")

	// Add a port nothing is listening on, then open it.
	for _, vis := range []string{"private", "public"} {
		rec := tc.do(t, "POST", "/api/routes/webby/visibility", "alice",
			map[string]any{"visibility": vis, "port": 5173})
		if rec.Code != http.StatusOK {
			t.Fatalf("set :5173 %s: status %d (%s)", vis, rec.Code, rec.Body)
		}
	}
	rt, _, err := tc.routes.GetBySubdomain("webby")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Visibility != routes.VisibilityPrivate {
		t.Fatalf("opening :5173 changed the default port to %q", rt.Visibility)
	}
	if got, err := tc.routes.VisibilityForPort(rt, 5173); err != nil || got != routes.VisibilityPublic {
		t.Fatalf("VisibilityForPort(5173) = %q, %v", got, err)
	}

	var views []sandboxView
	if err := json.Unmarshal(tc.do(t, "GET", "/api/machines", "alice", nil).Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	var found *routeStatus
	for i, r := range views[0].Routes {
		if r.Port == 5173 {
			found = &views[0].Routes[i]
		}
	}
	if found == nil {
		t.Fatalf(":5173 is not on the strip: %+v", views[0].Routes)
	}
	if !found.Pinned || found.Default || found.Visibility != routes.VisibilityPublic {
		t.Fatalf(":5173 = %+v", *found)
	}

	// Forgetting takes it off the strip; the default port cannot be forgotten,
	// because its visibility is the route's and there is no row to drop.
	if rec := tc.do(t, "DELETE", "/api/routes/webby/ports/5173", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("forget :5173: status %d (%s)", rec.Code, rec.Body)
	}
	if got, err := tc.routes.VisibilityForPort(rt, 5173); err != nil || got != routes.VisibilityPrivate {
		t.Fatalf("VisibilityForPort(5173) after forget = %q, %v", got, err)
	}
	if rec := tc.do(t, "DELETE", "/api/routes/webby/ports/"+strconv.Itoa(rt.Port), "alice", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("forgetting the default port: status %d, want 400", rec.Code)
	}
	// Another owner may not touch any of it, and is told the same "no such
	// route" as if it did not exist.
	if rec := tc.do(t, "POST", "/api/routes/webby/visibility", "bob",
		map[string]any{"visibility": "public", "port": 5173}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner set: status %d, want 404", rec.Code)
	}
	if rec := tc.do(t, "DELETE", "/api/routes/webby/ports/5173", "bob", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner forget: status %d, want 404", rec.Code)
	}
}

func TestLifecycleActionsDriveManager(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "webby", "alice")

	if rec := tc.do(t, "POST", "/api/machines/webby/pause", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("pause: status %d (%s)", rec.Code, rec.Body)
	}
	if box, _ := tc.mgr.Get("webby"); box.State != vmm.StatePaused {
		t.Fatalf("expected paused, got %s", box.State)
	}
	if rec := tc.do(t, "POST", "/api/machines/webby/resume", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("resume: status %d (%s)", rec.Code, rec.Body)
	}
	if box, _ := tc.mgr.Get("webby"); box.State != vmm.StateRunning {
		t.Fatalf("expected running, got %s", box.State)
	}
	if rec := tc.do(t, "POST", "/api/machines/webby/reboot", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("reboot: status %d (%s)", rec.Code, rec.Body)
	}
	if box, _ := tc.mgr.Get("webby"); box.State != vmm.StateRunning {
		t.Fatalf("expected running after reboot, got %s", box.State)
	}

	// Rename moves the sandbox and its default route.
	rec := tc.do(t, "POST", "/api/machines/webby/rename", "alice", map[string]string{"new_name": "zippy"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: status %d (%s)", rec.Code, rec.Body)
	}
	if _, ok := tc.mgr.Get("webby"); ok {
		t.Fatal("old name still exists after rename")
	}
	box, ok := tc.mgr.Get("zippy")
	if !ok {
		t.Fatal("renamed sandbox missing")
	}
	var out host.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Name != "zippy" {
		t.Fatalf("rename response: %s (err %v)", rec.Body, err)
	}

	// Pin resumes and flags; unpin clears.
	if rec := tc.do(t, "POST", "/api/machines/zippy/pin", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("pin: status %d (%s)", rec.Code, rec.Body)
	}
	if box, _ = tc.mgr.Get("zippy"); !box.Pinned || box.State != vmm.StateRunning {
		t.Fatalf("expected pinned+running, got %+v", box)
	}
	if rec := tc.do(t, "POST", "/api/machines/zippy/unpin", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("unpin: status %d (%s)", rec.Code, rec.Body)
	}
	if box, _ = tc.mgr.Get("zippy"); box.Pinned {
		t.Fatal("expected unpinned")
	}

	// Snapshot → fork → delete, all session-scoped.
	if rec := tc.do(t, "POST", "/api/machines/zippy/snapshot", "alice", map[string]string{"snapshot_name": "base"}); rec.Code != http.StatusCreated {
		t.Fatalf("snapshot: status %d (%s)", rec.Code, rec.Body)
	}
	if rec := tc.do(t, "POST", "/api/snapshots/base/fork", "alice", map[string]string{"name": "zippy-2"}); rec.Code != http.StatusCreated {
		t.Fatalf("fork: status %d (%s)", rec.Code, rec.Body)
	}
	if box, ok := tc.mgr.Get("zippy-2"); !ok || box.Owner != "alice" {
		t.Fatalf("fork missing or mis-owned: %+v", box)
	}
	if rec := tc.do(t, "POST", "/api/snapshots/base/delete", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete snapshot: status %d (%s)", rec.Code, rec.Body)
	}
	if _, ok := tc.mgr.SnapshotByName("alice", "base"); ok {
		t.Fatal("snapshot still exists after delete")
	}
}

// TestSecretMutationsFireSyncOwner: tag and secret changes must trigger the
// change-time push for the owner (delivery itself is envsync's concern).
func TestSecretMutationsFireSyncOwner(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "webby", "alice")

	if rec := tc.do(t, "PUT", "/api/secrets/API_KEY", "alice", map[string]any{"value": "v1", "tags": []string{"prod"}}); rec.Code != http.StatusOK {
		t.Fatalf("put secret: status %d (%s)", rec.Code, rec.Body)
	}
	if got := tc.sync.count("alice"); got != 1 {
		t.Fatalf("SyncOwner after put: %d calls, want 1", got)
	}
	if rec := tc.do(t, "PUT", "/api/machines/webby/tags", "alice", map[string][]string{"tags": {"prod"}}); rec.Code != http.StatusOK {
		t.Fatalf("set tags: status %d (%s)", rec.Code, rec.Body)
	}
	if got := tc.sync.count("alice"); got != 2 {
		t.Fatalf("SyncOwner after tags: %d calls, want 2", got)
	}
	if rec := tc.do(t, "DELETE", "/api/secrets/API_KEY", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete secret: status %d (%s)", rec.Code, rec.Body)
	}
	if got := tc.sync.count("alice"); got != 3 {
		t.Fatalf("SyncOwner after delete: %d calls, want 3", got)
	}

	// Deleting a secret that never existed is 404 and no push.
	if rec := tc.do(t, "DELETE", "/api/secrets/NEVER_WAS", "alice", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing secret: status %d, want 404", rec.Code)
	}
	if got := tc.sync.count("alice"); got != 3 {
		t.Fatalf("SyncOwner after failed delete: %d calls, want 3", got)
	}
}

func TestMeAndLogout(t *testing.T) {
	tc := newTestConsole(t)

	var me meResponse
	if err := json.Unmarshal(tc.do(t, "GET", "/api/me", "alice", nil).Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Handle != "alice" || me.Operator {
		t.Fatalf("unexpected /api/me: %+v", me)
	}
	if err := json.Unmarshal(tc.do(t, "GET", "/api/me", "opsy", nil).Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if !me.Operator {
		t.Fatalf("operator flag not set: %+v", me)
	}

	rec := tc.do(t, "POST", "/api/logout", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: status %d", rec.Code)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == edgeauth.CookieName && c.Value == "" && c.MaxAge < 0 && c.Domain == testDomain {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("logout did not clear the zone cookie: %v", rec.Result().Cookies())
	}
}

// TestLeadingDotDomainNormalized: a --proxy-domain written with a leading dot
// (".hivemind.tools") must behave like the bare zone. Login stores the session
// cookie under Domain ".hivemind.tools", so logout must clear that exact
// Domain — an unnormalized "..hivemind.tools" never matches and the session
// survives — and the CSRF gate must accept the real first-party Origin.
func TestLeadingDotDomainNormalized(t *testing.T) {
	tc := newTestConsoleDomain(t, "."+testDomain, "xterm")
	tc.create(t, "webby", "alice")

	rec := tc.do(t, "POST", "/api/logout", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: status %d", rec.Code)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == edgeauth.CookieName && c.Value == "" && c.MaxAge < 0 && c.Domain == testDomain {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("logout with a leading-dot domain did not clear the zone cookie: %v", rec.Result().Cookies())
	}

	// Origin-proved mutation: h.origin must be https://my.<zone>, not the
	// malformed https://my..<zone> a raw leading-dot domain would build.
	req := httptest.NewRequest("POST", "/api/machines/webby/pause", nil)
	req.AddCookie(tc.session(t, "alice"))
	req.Header.Set("Origin", "https://my."+testDomain)
	w := httptest.NewRecorder()
	tc.h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first-party origin mutation: status %d (%s)", w.Code, w.Body)
	}
}

// TestValidationErrorsAreBadRequest exercises the statusFor mapping for the
// stores' validation failures.
func TestValidationErrorsAreBadRequest(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "webby", "alice")

	cases := []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/secrets/lower_case", map[string]any{"value": "v"}},
		{"PUT", "/api/secrets/TOO_BIG", map[string]any{"value": strings.Repeat("x", 5000)}},
		{"PUT", "/api/machines/webby/tags", map[string][]string{"tags": {"Bad Tag!"}}},
		{"POST", "/api/machines/webby/port", map[string]int{"port": 70000}},
		{"POST", "/api/machines/webby/rename", map[string]string{"new_name": "Bad Name"}},
	}
	for _, c := range cases {
		rec := tc.do(t, c.method, c.path, "alice", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status %d, want 400 (body %s)", c.method, c.path, rec.Code, rec.Body)
		}
	}

	// Renaming onto an existing sandbox is a conflict, not a 500.
	tc.create(t, "taken", "alice")
	rec := tc.do(t, "POST", "/api/machines/webby/rename", "alice", map[string]string{"new_name": "taken"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("rename onto existing: status %d, want 409 (body %s)", rec.Code, rec.Body)
	}
}

// TestIndexShipsRecoveryAffordances: the embedded SPA must carry the
// retryable load-failure view (a non-401 error on first load renders it
// instead of a blank page) and the port-preserving link helper that keeps
// route/login links reachable on a non-default-port deployment.
func TestIndexShipsRecoveryAffordances(t *testing.T) {
	tc := newTestConsole(t)
	body := tc.do(t, "GET", "/", "", nil).Body.String()
	for _, want := range []string{`id="error-view"`, `id="error-retry"`, "portSuffix()", `/sparkbox-logo.png`} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html missing %s", want)
		}
	}
}

func TestLogoIsPublicAndPNG(t *testing.T) {
	rec := newTestConsole(t).do(t, "GET", "/sparkbox-logo.png", "", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("logo: status %d content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("logo response is not PNG data")
	}
}

// TestErrorBodyShape: errors ride {"error": msg} so the SPA can surface them.
func TestErrorBodyShape(t *testing.T) {
	tc := newTestConsole(t)
	rec := tc.do(t, "POST", "/api/machines/nope/pause", "alice", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	var e map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("no sandbox named %q", "nope"); e["error"] != want {
		t.Fatalf("error body %q, want %q", e["error"], want)
	}
}

// testPNG is a minimal PNG so the stub favicon fetcher returns real image bytes.
var testPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
}

func TestNetworkRuleCRUD(t *testing.T) {
	tc := newTestConsole(t)
	// Create.
	rec := tc.do(t, "PUT", "/api/network-rules/CI%20egress", "alice",
		map[string]any{"allow": []string{"github.com", "*.githubusercontent.com"}, "tags": []string{"ci"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("put rule: %d %s", rec.Code, rec.Body.String())
	}
	// List returns it in full.
	rec = tc.do(t, "GET", "/api/network-rules", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var rules []netrules.RuleMeta
	if err := json.Unmarshal(rec.Body.Bytes(), &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Name != "CI egress" || len(rules[0].Spec.Allow) != 2 {
		t.Fatalf("unexpected rules: %+v", rules)
	}
	// A bad pattern is a 400.
	rec = tc.do(t, "PUT", "/api/network-rules/bad", "alice", map[string]any{"allow": []string{"*"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad pattern: %d, want 400", rec.Code)
	}
	// Delete, then delete again is 404.
	if rec = tc.do(t, "DELETE", "/api/network-rules/CI%20egress", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec = tc.do(t, "DELETE", "/api/network-rules/CI%20egress", "alice", nil); rec.Code != http.StatusNotFound {
		t.Errorf("second delete: %d, want 404", rec.Code)
	}
}

func TestNetworkRulesAreOwnerScoped(t *testing.T) {
	tc := newTestConsole(t)
	tc.do(t, "PUT", "/api/network-rules/mine", "alice", map[string]any{"allow": []string{"github.com"}, "tags": []string{"ci"}})
	// mallory sees none of alice's rules...
	rec := tc.do(t, "GET", "/api/network-rules", "mallory", nil)
	var rules []netrules.RuleMeta
	json.Unmarshal(rec.Body.Bytes(), &rules) //nolint:errcheck
	if len(rules) != 0 {
		t.Errorf("mallory saw alice's rules: %+v", rules)
	}
	// ...and deleting a name she doesn't own is a 404, not a cross-owner delete.
	if rec := tc.do(t, "DELETE", "/api/network-rules/mine", "mallory", nil); rec.Code != http.StatusNotFound {
		t.Errorf("cross-owner delete: %d, want 404", rec.Code)
	}
	// alice's rule survived.
	rec = tc.do(t, "GET", "/api/network-rules", "alice", nil)
	json.Unmarshal(rec.Body.Bytes(), &rules) //nolint:errcheck
	if len(rules) != 1 {
		t.Errorf("alice's rule was clobbered: %+v", rules)
	}
}

// repoRow is the wire shape the SPA reads: repos.Repo's fields flattened, plus
// the install state the panel renders. Declared here rather than decoding into
// repoView so a rename of an unexported field cannot quietly change the JSON
// the page is written against.
type repoRow struct {
	Slug            string   `json:"slug"`
	Ref             string   `json:"ref"`
	Path            string   `json:"path"`
	Access          string   `json:"access"`
	Tags            []string `json:"tags"`
	App             string   `json:"app"`
	AppNote         string   `json:"app_note"`
	InstallURL      string   `json:"install_url"`
	UserAuth        string   `json:"user_auth"`
	UserAuthEnabled bool     `json:"user_auth_enabled"`
	GitHubLogin     string   `json:"github_login"`
}

func TestRepoCRUD(t *testing.T) {
	tc := newTestConsole(t)
	// An owner with nothing attached must serialise as [], not null: the SPA's
	// Array.isArray guard renders null as an empty table with no explanation.
	rec := tc.do(t, "GET", "/api/repos", "alice", nil)
	if got := rec.Body.String(); got != "[]\n" {
		t.Fatalf("empty listing body %q, want []", got)
	}
	// The slug rides one path segment percent-encoded; the mux must hand the
	// handler back the slash, or every attachment lands under a mangled name.
	rec = tc.do(t, "PUT", "/api/repos/wandb%2Fhivemind", "alice",
		map[string]any{"ref": "main", "path": "src/hm", "access": "write", "tags": []string{"hm"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("attach: %d %s", rec.Code, rec.Body.String())
	}
	rec = tc.do(t, "GET", "/api/repos", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var list []repoRow
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("unexpected listing: %+v", list)
	}
	got := list[0]
	if got.Slug != "wandb/hivemind" || got.Ref != "main" || got.Path != "src/hm" ||
		got.Access != "write" || len(got.Tags) != 1 || got.Tags[0] != "hm" {
		t.Fatalf("round trip lost something: %+v", got)
	}
	// No App is configured on a test console, so the panel is told exactly
	// that rather than being left to guess an install state.
	if got.App != appOff || got.InstallURL != "" {
		t.Errorf("app state without an App: %q url %q, want %q and no url", got.App, got.InstallURL, appOff)
	}
	if got.UserAuth != "bot" || got.UserAuthEnabled || got.GitHubLogin != "alice-gh" {
		t.Errorf("user auth without browser flow: %+v", got)
	}
	// An attachment is egress-relevant, so the mutation re-pushes policy — and
	// it is not a secret, so it must NOT fire the env sync.
	if n := tc.sync.count("alice"); n != 0 {
		t.Errorf("attaching a repo fired the secret env push %d times", n)
	}
	// A slug that is not owner/name is a 400 from the store's grammar.
	if rec = tc.do(t, "PUT", "/api/repos/hivemind", "alice", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Errorf("bare name: %d, want 400", rec.Code)
	}
	// Delete, then delete again is 404 — the same answer a name that never
	// existed gets.
	if rec = tc.do(t, "DELETE", "/api/repos/wandb%2Fhivemind", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("detach: %d %s", rec.Code, rec.Body.String())
	}
	if rec = tc.do(t, "DELETE", "/api/repos/wandb%2Fhivemind", "alice", nil); rec.Code != http.StatusNotFound {
		t.Errorf("second detach: %d, want 404", rec.Code)
	}
}

func TestRepoListShowsUserCredentialAndBrowserAvailability(t *testing.T) {
	tc := newTestConsole(t)
	if rec := tc.do(t, "PUT", "/api/repos/wandb%2Fhivemind", "alice",
		map[string]any{"access": "write", "tags": []string{"hm"}}); rec.Code != http.StatusOK {
		t.Fatalf("attach: %d %s", rec.Code, rec.Body.String())
	}
	client, err := ghuser.NewClient(ghuser.Config{ClientID: "Iv23liTEST", ClientSecret: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := ghuser.Open(filepath.Join(t.TempDir(), "grants.db"), ghuser.DeriveKEK([]byte("test signing material")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Put(ghuser.Grant{
		Owner: "alice", GitHubID: 7, InstallationID: 42, RepoID: 99, Slug: "wandb/hivemind",
		Token: ghuser.Token{AccessToken: "ghu_secret", RefreshToken: "ghr_secret",
			AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(24 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := ghapp.New(ghapp.Config{ClientID: "Iv23liTEST", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	tc.handler.SetGitHubApp(app)
	tc.handler.appSeen = map[string]appProbe{
		probeKey("alice", defaultRepoHost, "wandb/hivemind"): {state: appReady, at: time.Now()},
	}
	tc.handler.SetGitHubUserAuth(ghuser.NewManager(client, store, nil))
	rec := tc.do(t, "GET", "/api/repos", "alice", nil)
	var rows []repoRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].UserAuth != "user" || !rows[0].UserAuthEnabled || rows[0].GitHubLogin != "alice-gh" {
		t.Fatalf("repo auth row = %+v", rows)
	}
}

func TestRepoBrowserOAuthAuthorizesExactAttachment(t *testing.T) {
	tc := newTestConsole(t)
	if rec := tc.do(t, "PUT", "/api/repos/wandb%2Fhivemind", "alice",
		map[string]any{"access": "write", "tags": []string{"hm"}}); rec.Code != http.StatusOK {
		t.Fatalf("attach: %d %s", rec.Code, rec.Body.String())
	}
	var exchangedRepo string
	var scopedRepo int64
	var scopedTarget string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/wandb/hivemind/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":42,"app_slug":"sparkbox","account":{"id":7,"login":"alice-gh","type":"User"},"permissions":{"metadata":"read","contents":"write"}}`)
	})
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"token":"ghs_installation","expires_at":"2030-01-01T00:00:00Z"}`)
	})
	mux.HandleFunc("GET /repos/wandb/hivemind", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":99}`)
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		exchangedRepo = r.Form.Get("repository_id")
		fmt.Fprint(w, `{"access_token":"ghu_web_broad","expires_in":28800,"refresh_token":"ghr_web","refresh_token_expires_in":15897600}`)
	})
	mux.HandleFunc("POST /applications/Iv23liTEST/token/scoped", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Target        string  `json:"target"`
			RepositoryIDs []int64 `json:"repository_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		scopedTarget = in.Target
		if len(in.RepositoryIDs) == 1 {
			scopedRepo = in.RepositoryIDs[0]
		}
		fmt.Fprint(w, `{"token":"ghu_web_scoped","expires_at":"2030-01-01T00:00:00Z"}`)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"id":7}`) })
	mux.HandleFunc("GET /repositories/99", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":99}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := ghapp.New(ghapp.Config{ClientID: "Iv23liTEST", Key: key, BaseURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client, err := ghuser.NewClient(ghuser.Config{
		ClientID: "Iv23liTEST", ClientSecret: "client-secret", HTTPClient: srv.Client(),
		AuthorizeURL: srv.URL + "/login/oauth/authorize", TokenURL: srv.URL + "/login/oauth/access_token", APIURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := ghuser.Open(filepath.Join(t.TempDir(), "grants.db"), ghuser.DeriveKEK([]byte("test signing material")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr := ghuser.NewManager(client, store, nil)
	tc.handler.SetGitHubApp(app)
	tc.handler.SetGitHubUserAuth(mgr)

	rec := tc.do(t, "POST", "/api/repos/wandb%2Fhivemind/authorize", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	var started map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	authorizeURL, err := url.Parse(started["url"])
	if err != nil {
		t.Fatal(err)
	}
	state := authorizeURL.Query().Get("state")
	if state == "" || authorizeURL.Query().Get("redirect_uri") != "https://my.hivemind.tools/github/repo/callback" {
		t.Fatalf("authorize URL = %s", authorizeURL)
	}
	rec = tc.do(t, "GET", "/github/repo/callback?state="+url.QueryEscape(state)+"&code=oauth-code", "alice", nil)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "github=authorized") {
		t.Fatalf("callback: %d location %q body %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if exchangedRepo != "" || scopedRepo != 99 || scopedTarget != "alice-gh" || !mgr.Authorized("alice", "wandb/hivemind", 7) {
		t.Fatalf("repo exchange = %q scoped = (%q, %d) authorized = %v", exchangedRepo, scopedTarget, scopedRepo,
			mgr.Authorized("alice", "wandb/hivemind", 7))
	}
}

func TestReposAreOwnerScoped(t *testing.T) {
	tc := newTestConsole(t)
	tc.do(t, "PUT", "/api/repos/wandb%2Fhivemind", "alice", map[string]any{"tags": []string{"hm"}})
	// mallory sees none of alice's attachments...
	rec := tc.do(t, "GET", "/api/repos", "mallory", nil)
	var list []repoRow
	json.Unmarshal(rec.Body.Bytes(), &list) //nolint:errcheck
	if len(list) != 0 {
		t.Errorf("mallory saw alice's repos: %+v", list)
	}
	// ...and detaching a slug she does not own is a 404, never a cross-owner
	// delete and never a 403 that would confirm alice has it attached.
	if rec := tc.do(t, "DELETE", "/api/repos/wandb%2Fhivemind", "mallory", nil); rec.Code != http.StatusNotFound {
		t.Errorf("cross-owner detach: %d, want 404", rec.Code)
	}
	// mallory may attach the same repository herself: the two rows are
	// separate attachments, and hers must not disturb alice's.
	if rec := tc.do(t, "PUT", "/api/repos/wandb%2Fhivemind", "mallory", map[string]any{"tags": []string{"ml"}}); rec.Code != http.StatusOK {
		t.Fatalf("mallory attach: %d %s", rec.Code, rec.Body.String())
	}
	rec = tc.do(t, "GET", "/api/repos", "alice", nil)
	json.Unmarshal(rec.Body.Bytes(), &list) //nolint:errcheck
	if len(list) != 1 || len(list[0].Tags) != 1 || list[0].Tags[0] != "hm" {
		t.Errorf("alice's attachment was clobbered: %+v", list)
	}
}

// TestRepoAttachNeedsAStrongLink covers the third door onto the attach verb.
// ctl and the REST API route through ctlops.AttachGate; the console did not,
// which meant an account whose GitHub link was never proved directly with
// GitHub could create attachments the credential path would then refuse to
// honour — and could widen its tags' effective egress on the way, through the
// netrules overlay, without ever passing the gate.
//
// The two refusals are deliberately different statuses: 409 is "go and link an
// account", 403 is "the link you have is not good enough".
func TestRepoAttachNeedsAStrongLink(t *testing.T) {
	tc := newTestConsole(t)

	if rec := tc.do(t, "PUT", "/api/repos/wandb%2Fhivemind", "unlinked",
		map[string]any{"tags": []string{"ml"}}); rec.Code != http.StatusConflict {
		t.Errorf("attach with no link: %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if rec := tc.do(t, "PUT", "/api/repos/wandb%2Fhivemind", "weakly",
		map[string]any{"tags": []string{"ml"}}); rec.Code != http.StatusForbidden {
		t.Errorf("attach with an assertion link: %d, want 403 (%s)", rec.Code, rec.Body)
	}
	// Neither refusal may leave a row behind: a listing that shows an
	// attachment nothing will ever clone is worse than the refusal.
	for _, who := range []string{"unlinked", "weakly"} {
		var list []repoView
		rec := tc.do(t, "GET", "/api/repos", who, nil)
		json.Unmarshal(rec.Body.Bytes(), &list) //nolint:errcheck
		if len(list) != 0 {
			t.Errorf("%s: refused attach still stored %+v", who, list)
		}
	}
}

// TestRepoAppStateIsProbedAndCached drives the three answers a configured App
// can give about one owner's attachments, through a fake github.com.
//
// The states are the point: "not installed" is a thing to go and fix and
// carries the URL that fixes it, "refused" is an installation this account may
// not use (bound forward from the handle to the stored github id, never a
// reverse lookup), and "installed" is the only one that promises a clone will
// work. The hit count is the other point — the SPA re-lists every four seconds,
// and a panel that asked github.com on the request path would spend an App's
// whole rate limit on a tab somebody left open.
func TestRepoAppStateIsProbedAndCached(t *testing.T) {
	tc := newTestConsole(t)
	var hits int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/wandb/hivemind/installation":
			// A User installation owned by the account alice is linked to.
			io.WriteString(w, `{"id":42,"app_slug":"sparkbox","account":{"id":7,"login":"alice-gh","type":"User"}}`) //nolint:errcheck
		case "/repos/wandb/somebody-elses/installation":
			// A User installation owned by somebody else entirely.
			io.WriteString(w, `{"id":43,"app_slug":"sparkbox","account":{"id":99,"login":"carol","type":"User"}}`) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Not Found"}`) //nolint:errcheck
		}
	}))
	defer gh.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := ghapp.New(ghapp.Config{
		ClientID: "Iv23liTEST", Key: key, BaseURL: gh.URL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	tc.handler.SetGitHubApp(app)

	for _, slug := range []string{"wandb/hivemind", "wandb/not-attached-anywhere", "wandb/somebody-elses"} {
		if rec := tc.do(t, "PUT", "/api/repos/"+url.PathEscape(slug), "alice",
			map[string]any{"tags": []string{"hm"}}); rec.Code != http.StatusOK {
			t.Fatalf("attach %s: %d %s", slug, rec.Code, rec.Body.String())
		}
	}

	// The first listing cannot know yet — the probe runs behind it — so poll
	// until every row has settled rather than sleeping a guessed interval.
	var rows []repoRow
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := tc.do(t, "GET", "/api/repos", "alice", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		rows = nil
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		settled := true
		for _, r := range rows {
			if r.App == appChecking {
				settled = false
			}
		}
		if settled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("app states never settled: %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}

	byslug := map[string]repoRow{}
	for _, r := range rows {
		byslug[r.Slug] = r
	}
	if got := byslug["wandb/hivemind"]; got.App != appReady || got.InstallURL != "" {
		t.Errorf("installed repo: app %q note %q url %q, want %q", got.App, got.AppNote, got.InstallURL, appReady)
	}
	if got := byslug["wandb/not-attached-anywhere"]; got.App != appMissing || got.InstallURL == "" {
		t.Errorf("uninstalled repo: app %q url %q, want %q with an install url", got.App, got.InstallURL, appMissing)
	}
	if got := byslug["wandb/somebody-elses"]; got.App != appBlocked || got.AppNote == "" {
		t.Errorf("someone else's installation: app %q note %q, want %q with a reason", got.App, got.AppNote, appBlocked)
	}

	// Every further listing is served from the console's own cache, including
	// the "not installed" row that ghapp itself refuses to remember.
	settledHits := atomic.LoadInt64(&hits)
	for i := 0; i < 5; i++ {
		tc.do(t, "GET", "/api/repos", "alice", nil)
	}
	if now := atomic.LoadInt64(&hits); now != settledHits {
		t.Errorf("five more listings cost %d github calls, want 0", now-settledHits)
	}

	// A mutation forgets the answer, because "install it and save again" is
	// exactly the gesture that follows a "not installed" row.
	if rec := tc.do(t, "PUT", "/api/repos/wandb%2Fnot-attached-anywhere", "alice",
		map[string]any{"tags": []string{"hm"}}); rec.Code != http.StatusOK {
		t.Fatalf("re-attach: %d %s", rec.Code, rec.Body.String())
	}
	rec := tc.do(t, "GET", "/api/repos", "alice", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	rechecked := false
	for _, r := range rows {
		if r.Slug == "wandb/not-attached-anywhere" && r.App == appChecking {
			rechecked = true
		}
	}
	if !rechecked {
		t.Errorf("saving an attachment did not re-ask about it: %+v", rows)
	}
}

// TestReposDisabledWithoutStore: a host that opened no repos store answers 501
// with the "not enabled" wording every other optional subsystem uses — and,
// crucially, does not dereference the nil store and take the console down.
func TestReposDisabledWithoutStore(t *testing.T) {
	tc := newTestConsole(t)
	// The mux holds method values bound to the handler, so clearing the field
	// after Handler() is exactly the shape of a host built without the store.
	tc.handler.repos = nil
	for _, ep := range []struct{ method, path string }{
		{"GET", "/api/repos"},
		{"PUT", "/api/repos/wandb%2Fhivemind"},
		{"DELETE", "/api/repos/wandb%2Fhivemind"},
	} {
		rec := tc.do(t, ep.method, ep.path, "alice", map[string]any{})
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: %d, want 501", ep.method, ep.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "not enabled") {
			t.Errorf("%s %s body %q: want the \"not enabled\" wording", ep.method, ep.path, rec.Body.String())
		}
	}
}

func TestBandwidthDisabledWithoutSluice(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "alices-box", "alice")
	rec := tc.do(t, "GET", "/api/machines/alices-box/bandwidth", "alice", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("bandwidth without sluice: %d, want 501", rec.Code)
	}
	// Cross-owner is still 404, never 501 (existence not leaked).
	if rec := tc.do(t, "GET", "/api/machines/alices-box/bandwidth", "mallory", nil); rec.Code != http.StatusNotFound {
		t.Errorf("cross-owner bandwidth: %d, want 404", rec.Code)
	}
}

func TestFaviconServesImageAndFallsBack(t *testing.T) {
	tc := newTestConsole(t)
	rec := tc.do(t, "GET", "/api/favicon?domain=github.com", "alice", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("favicon: %d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Body.Len() == 0 {
		t.Error("empty favicon body")
	}
	// No domain -> globe fallback, still 200.
	rec = tc.do(t, "GET", "/api/favicon", "alice", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Errorf("favicon fallback: %d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestIndexSetsCSP(t *testing.T) {
	tc := newTestConsole(t)
	rec := tc.do(t, "GET", "/", "", nil)
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' data:") || !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP missing or wrong: %q", csp)
	}
}

// TestMeAdvertisesTerminalSubdomain: the SPA builds every Terminal link from
// this one field, and hides the button when it is absent. Both halves matter —
// a host with no proxy edge (or --xterm-subdomain "") serves no terminals, and
// a button linking to a name nothing answers is worse than no button.
func TestMeAdvertisesTerminalSubdomain(t *testing.T) {
	var me struct {
		Handle            string `json:"handle"`
		TerminalSubdomain string `json:"terminal_subdomain"`
	}
	rec := newTestConsole(t).do(t, "GET", "/api/me", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: status %d (%s)", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Handle != "alice" || me.TerminalSubdomain != "xterm" {
		t.Fatalf("me = %+v, want handle alice and terminal_subdomain xterm", me)
	}

	// Terminals off: the field is omitted entirely, not sent empty-but-present.
	off := newTestConsoleDomain(t, testDomain, "")
	rec = off.do(t, "GET", "/api/me", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me (terminals off): status %d (%s)", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "terminal_subdomain") {
		t.Errorf("terminals disabled but /api/me advertised one: %s", rec.Body)
	}
}

// The console's turbo endpoint restarts a machine at double size and reports
// the record that came back, so the SPA's lamp is lit by the manager's answer
// rather than by what the page just asked for.
func TestTurboEndpointDoublesAndIsOwnerScoped(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "webby", "alice")

	rec := tc.do(t, "POST", "/api/machines/webby/turbo", "alice", map[string]bool{"on": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("turbo on: status %d (%s)", rec.Code, rec.Body)
	}
	var view host.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Turbo || view.VCPUs != host.TurboFactor || view.MemMB != 512*host.TurboFactor {
		t.Fatalf("response = {turbo:%v vcpus:%d mem:%d}, want the doubled record",
			view.Turbo, view.VCPUs, view.MemMB)
	}
	if box, _ := tc.mgr.Get("webby"); !box.Turbo || box.State != vmm.StateRunning {
		t.Fatalf("manager record after turbo = %+v", box)
	}

	// Somebody else's machine answers exactly like a missing one, and — the
	// point of checking ownership before the manager call — is not restarted.
	if rec := tc.do(t, "POST", "/api/machines/webby/turbo", "mallory",
		map[string]bool{"on": false}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner turbo: status %d, want 404", rec.Code)
	}
	if box, _ := tc.mgr.Get("webby"); !box.Turbo {
		t.Fatal("a cross-owner request changed the machine")
	}

	if rec := tc.do(t, "POST", "/api/machines/webby/turbo", "alice",
		map[string]bool{"on": false}); rec.Code != http.StatusOK {
		t.Fatalf("turbo off: status %d (%s)", rec.Code, rec.Body)
	}
	if box, _ := tc.mgr.Get("webby"); box.Turbo || box.MemMB != 512 {
		t.Fatalf("turbo off left %+v", box)
	}
}

// fakeBindings is the one question the Snapshots panel asks of the binding
// store, answered from a map. The real store is not used here because what is
// under test is the projection — that one new key appears and none of the
// shipped ones move — not sqlite.
type fakeBindings struct {
	rows  []templates.Binding
	ports map[string]int // "owner\x00snapshot" -> port, sparse like the store
	err   error
}

func (f fakeBindings) BindingsForOwner(owner string) ([]templates.Binding, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []templates.Binding
	for _, b := range f.rows {
		if b.Owner == owner {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f fakeBindings) SnapshotPorts(owner string) (map[string]int, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]int{}
	for k, port := range f.ports {
		if o, snap, ok := strings.Cut(k, "\x00"); ok && o == owner {
			out[snap] = port
		}
	}
	return out, nil
}

// TestSnapshotListStaysDecodableAsHostSnapshots is the compatibility half of
// the bound-tags column: the payload gained a key and lost nothing, so anything
// that decoded this endpoint into []*host.Snapshot before still does.
//
// It is asserted rather than assumed because the obvious implementation — a
// fresh struct with the four fields somebody remembered — silently drops
// whatever host.Snapshot grows next, and nothing else here would notice.
func TestSnapshotListStaysDecodableAsHostSnapshots(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "zippy", "alice")
	ctx := context.Background()
	if _, err := tc.mgr.Snapshot(ctx, "zippy", "base", "alice"); err != nil {
		t.Fatal(err)
	}

	// Before the seam is set: a host with no binding store serves the same
	// shape, with no bound_tags key at all.
	body := tc.do(t, "GET", "/api/snapshots", "alice", nil).Body.Bytes()
	var plain []*host.Snapshot
	if err := json.Unmarshal(body, &plain); err != nil {
		t.Fatalf("decoding into []*host.Snapshot: %v (%s)", err, body)
	}
	if len(plain) != 1 || plain[0].Name != "base" || plain[0].FromBox != "zippy" {
		t.Fatalf("the snapshot's own fields did not survive the embedding: %s", body)
	}
	if strings.Contains(string(body), "bound_tags") {
		t.Errorf("an unbound snapshot on a store-less host carries bound_tags: %s", body)
	}

	tc.handler.SetTemplateTags(fakeBindings{rows: []templates.Binding{
		{Owner: "alice", Tag: "cuda", Snapshot: "base"},
		{Owner: "alice", Tag: "ml", Snapshot: "base"},
		// mallory's binding names the same snapshot name on purpose: the store
		// is owner-scoped, and so is this projection.
		{Owner: "mallory", Tag: "theirs", Snapshot: "base"},
	}, ports: map[string]int{
		"alice\x00base": 5173,
		// Same snapshot name under another owner, for the same reason the
		// binding above uses one: both columns are owner-scoped.
		"mallory\x00base": 3000,
	}})

	var rows []struct {
		Name      string   `json:"name"`
		FromBox   string   `json:"from_box"`
		BoundTags []string `json:"bound_tags"`
		Port      int      `json:"port"`
	}
	body = tc.do(t, "GET", "/api/snapshots", "alice", nil).Body.Bytes()
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].FromBox != "zippy" {
		t.Fatalf("the row changed shape: %s", body)
	}
	if len(rows[0].BoundTags) != 2 || rows[0].BoundTags[0] != "cuda" || rows[0].BoundTags[1] != "ml" {
		t.Errorf("bound_tags = %v, want [cuda ml] and nothing of mallory's", rows[0].BoundTags)
	}
	if rows[0].Port != 5173 {
		t.Errorf("port = %d, want alice's 5173 and not mallory's 3000", rows[0].Port)
	}
	if err := json.Unmarshal(body, &plain); err != nil {
		t.Fatalf("the bound payload no longer decodes into []*host.Snapshot: %v (%s)", err, body)
	}
}

// TestSnapshotListSurvivesABindingStoreError: the bindings are decoration, so a
// store that cannot answer costs the column and not the panel.
func TestSnapshotListSurvivesABindingStoreError(t *testing.T) {
	tc := newTestConsole(t)
	tc.create(t, "zippy", "alice")
	if _, err := tc.mgr.Snapshot(context.Background(), "zippy", "base", "alice"); err != nil {
		t.Fatal(err)
	}
	tc.handler.SetTemplateTags(fakeBindings{err: errors.New("database is locked")})

	rec := tc.do(t, "GET", "/api/snapshots", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want the rows anyway (%s)", rec.Code, rec.Body)
	}
	var plain []*host.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &plain); err != nil || len(plain) != 1 {
		t.Fatalf("rows = %v (err %v), want the snapshot listed", plain, err)
	}
}
