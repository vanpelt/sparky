package userconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/domainmeta"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netrules"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
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
	signer   *edgeauth.Signer
	sync     *syncRecorder
}

// nilFleet is a Fleet that never lists anything — enough for a no-op Syncer
// (nil client) whose Fleet is never consulted.
type nilFleet struct{}

func (nilFleet) List() []netpush.Sandbox { return nil }

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
	accounts := fakeAccounts{
		"alice":   {Handle: "alice", Status: users.StatusActive, InvitedBy: "bob"},
		"mallory": {Handle: "mallory", Status: users.StatusActive, InvitedBy: "bob"},
		"opsy":    {Handle: "opsy", Status: users.StatusActive, InvitedBy: users.OperatorInviter},
	}
	rec := &syncRecorder{}
	// netrules shares the same DB file as secrets (that owns sandbox_tags).
	netStore, err := netrules.Open(filepath.Join(dir, "secrets.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netStore.Close() })
	// A favicon cache with a stub fetcher so tests never hit the network.
	favicons := domainmeta.NewFaviconCache(filepath.Join(dir, "favicons"),
		func(ctx context.Context, reg string) ([]byte, string, bool) { return testPNG, "image/png", true })
	// A syncer with no sluice socket: Push/Usage are no-ops, so bandwidth is 501.
	netSync := netpush.NewSyncer(nil, nilFleet{}, netStore, log)
	h := New(mgr, routeStore, secretStore, netStore, netSync, favicons, accounts, signer, rec, "my", domain, xtermSub, false, log)
	return &testConsole{h: h.Handler(), handler: h, mgr: mgr, routes: routeStore, secrets: secretStore, netrules: netStore, signer: signer, sync: rec}
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
	{"POST", "/api/machines/somebox/pause"},
	{"POST", "/api/machines/somebox/resume"},
	{"DELETE", "/api/machines/somebox"},
	{"POST", "/api/machines/somebox/archive"},
	{"POST", "/api/machines/somebox/pin"},
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
	if _, err := tc.mgr.EnsureRunning(ctx, "alices-box"); err != nil {
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
	if len(views) != 1 || len(views[0].Routes) != 1 {
		t.Fatalf("unexpected machine list: %+v", views)
	}
	if r := views[0].Routes[0]; r.Port != 9090 || r.Visibility != "public" {
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
	if len(v.Routes) != 1 || v.Routes[0].Visibility != routes.VisibilityPrivate {
		t.Fatalf("expected one private default route, got %+v", v.Routes)
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
	for _, want := range []string{`id="error-view"`, `id="error-retry"`, "portSuffix()"} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html missing %s", want)
		}
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
