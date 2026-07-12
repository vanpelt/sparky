package console

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

const testPassword = "s3cret-pw"

func newTestConsole(t *testing.T) (http.Handler, *host.Manager) {
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
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: mock.New(dir, hostKey),
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(mgr, testPassword, false, log).Handler(), mgr
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
	h, _ := newTestConsole(t)

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
	h, mgr := newTestConsole(t)
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
