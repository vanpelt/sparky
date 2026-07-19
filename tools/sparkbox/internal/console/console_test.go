package console

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

const testPassword = "s3cret-pw"

func newTestConsole(t *testing.T) (http.Handler, *host.Manager, *routes.Store) {
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
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: mock.New(dir, hostKey),
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
		Routes:          store,
		NodeName:        "testnode",
		HostVCPUs:       8,
		HostMemMB:       16384,
		MemAdmissionPct: 85,
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(mgr, store, "hivemind.tools", testPassword, false, log).Handler(), mgr, store
}

// login returns the session cookie for the given password.
func login(t *testing.T, h http.Handler, password string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Password: password})
	req := httptest.NewRequest("POST", "/login", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login(%q): status %d, want 200", password, rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatalf("login(%q): no %s cookie set", password, cookieName)
	return nil
}

func TestConsoleAuthGate(t *testing.T) {
	h, _, _ := newTestConsole(t)

	// No cookie -> API is 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/sandboxes", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: status %d, want 401", rec.Code)
	}

	// Wrong password -> 401, no cookie.
	body, _ := json.Marshal(loginRequest{Password: "nope"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/login", strings.NewReader(string(body))))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login: status %d, want 401", rec.Code)
	}

	// The index page is always served (it renders the login form client-side).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sparkbox console") {
		t.Fatalf("index page: status %d", rec.Code)
	}
}

func TestConsoleListAndPause(t *testing.T) {
	h, mgr, _ := newTestConsole(t)
	ctx := context.Background()
	if _, err := mgr.Create(ctx, "swift-otter", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}

	cookie := login(t, h, testPassword)

	// Authenticated list returns the sandbox.
	req := httptest.NewRequest("GET", "/api/sandboxes", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, want 200", rec.Code)
	}
	var list []host.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].Name != "swift-otter" {
		t.Fatalf("unexpected list: %+v", list)
	}
	if list[0].State != vmm.StateRunning {
		t.Fatalf("new sandbox should be running, got %s", list[0].State)
	}

	// Pause it through the console.
	req = httptest.NewRequest("POST", "/api/sandboxes/swift-otter/pause", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: status %d, want 200", rec.Code)
	}
	if box, _ := mgr.Get("swift-otter"); box.State != vmm.StatePaused {
		t.Fatalf("expected paused after console pause, got %s", box.State)
	}

	// Pause without the cookie must be rejected.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sandboxes/swift-otter/pause", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated pause: status %d, want 401", rec.Code)
	}
}

func TestConsoleDestroy(t *testing.T) {
	h, mgr, _ := newTestConsole(t)
	ctx := context.Background()
	if _, err := mgr.Create(ctx, "gone-gull", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	cookie := login(t, h, testPassword)

	// DELETE without the cookie is rejected — and leaves the sandbox intact.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/sandboxes/gone-gull", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated destroy: status %d, want 401", rec.Code)
	}
	if _, ok := mgr.Get("gone-gull"); !ok {
		t.Fatal("sandbox destroyed by unauthenticated request")
	}

	// Authenticated DELETE removes it.
	req := httptest.NewRequest("DELETE", "/api/sandboxes/gone-gull", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("destroy: status %d, want 200", rec.Code)
	}
	if _, ok := mgr.Get("gone-gull"); ok {
		t.Fatal("sandbox still present after destroy")
	}

	// Destroying a missing sandbox is a 404.
	req = httptest.NewRequest("DELETE", "/api/sandboxes/gone-gull", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("destroy missing: status %d, want 404", rec.Code)
	}
}

func TestConsoleClusterCapacity(t *testing.T) {
	h, mgr, _ := newTestConsole(t)
	ctx := context.Background()
	if _, err := mgr.Create(ctx, "webby", "alice", "ubuntu", 2, 1024); err != nil {
		t.Fatal(err)
	}

	// Unauthenticated -> 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/cluster", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated cluster: status %d, want 401", rec.Code)
	}

	req := httptest.NewRequest("GET", "/api/cluster", nil)
	req.AddCookie(login(t, h, testPassword))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cluster: status %d, want 200", rec.Code)
	}
	var c clusterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode cluster: %v", err)
	}
	if c.Domain != "hivemind.tools" || len(c.Nodes) != 1 {
		t.Fatalf("unexpected cluster response: %+v", c)
	}
	n := c.Nodes[0]
	if n.Node != "testnode" || n.TotalVCPUs != 8 || n.TotalMemMB != 16384 {
		t.Fatalf("unexpected node totals: %+v", n)
	}
	if want := int64(16384 * 85 / 100); n.BudgetMemMB != want {
		t.Fatalf("budget = %d, want %d", n.BudgetMemMB, want)
	}
	if n.UsedVCPUs != 2 || n.UsedMemMB != 1024 || n.Running != 1 || n.Sandboxes != 1 {
		t.Fatalf("unexpected node usage: %+v", n)
	}
}

func TestConsoleRouteListeningStatus(t *testing.T) {
	h, mgr, store := newTestConsole(t)
	ctx := context.Background()
	if _, err := mgr.Create(ctx, "webby", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}

	// A live listener on the mock driver's HostIP (127.0.0.1): the kernel
	// completes handshakes from the accept backlog, no server loop needed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	livePort := ln.Addr().(*net.TCPAddr).Port

	// A dead port: bind then close, so nothing is listening there.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := dead.Addr().(*net.TCPAddr).Port
	dead.Close()

	for sub, port := range map[string]int{"webby": livePort, "webby-dead": deadPort} {
		if err := store.Upsert(routes.Route{Subdomain: sub, Sandbox: "webby", Owner: "alice", Port: port}); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest("GET", "/api/sandboxes", nil)
	req.AddCookie(login(t, h, testPassword))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, want 200", rec.Code)
	}
	var list []struct {
		Name   string        `json:"name"`
		Routes []routeStatus `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || len(list[0].Routes) != 2 {
		t.Fatalf("expected 1 sandbox with 2 routes, got %+v", list)
	}
	got := map[string]bool{}
	for _, r := range list[0].Routes {
		got[r.Subdomain] = r.Listening
	}
	if !got["webby"] {
		t.Fatal("live route should report listening=true")
	}
	if got["webby-dead"] {
		t.Fatal("dead route should report listening=false")
	}
}
