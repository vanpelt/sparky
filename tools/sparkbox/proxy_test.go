package sparkbox_test

// End-to-end test of the HTTP proxy edge over the mock stack: a route maps a
// subdomain to a sandbox + port, and a request to <sub>.hivemind.tools is
// reverse-proxied to a backend standing in for a service inside the VM. Also
// covers resume-on-connect: hitting a paused sandbox's URL wakes it.

import (
	"context"
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

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/proxy"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

const proxyDomain = "hivemind.tools"

type proxyStack struct {
	mgr   *host.Manager
	store *routes.Store
	proxy http.Handler
	dir   string
}

func newProxyStack(t *testing.T) *proxyStack {
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
	return &proxyStack{
		mgr:   mgr,
		store: store,
		proxy: proxy.New(mgr, store, proxyDomain, log),
		dir:   dir,
	}
}

// get issues a request to the proxy for host, returning status and body.
func (ps *proxyStack) get(t *testing.T, host string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	ps.proxy.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

// backendOn starts an HTTP server bound to a fixed loopback port (the mock
// driver's HostIP is 127.0.0.1) so the proxy can reach it as if it were a
// service inside the VM. Returns the port.
func backendOn(t *testing.T, reply string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s host=%s", reply, r.Host)
	})}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func TestProxyRoutesToBackend(t *testing.T) {
	ps := newProxyStack(t)
	ctx := context.Background()

	if _, err := ps.mgr.Create(ctx, "webvm", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	port := backendOn(t, "hello-from-app")
	// Point the sandbox's default subdomain at our backend port.
	if err := ps.store.Upsert(routes.Route{
		Subdomain: "webvm", Sandbox: "webvm", Owner: "alice", Port: port,
	}); err != nil {
		t.Fatal(err)
	}

	code, body := ps.get(t, "webvm.hivemind.tools")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", code, body)
	}
	if want := "hello-from-app"; body[:len(want)] != want {
		t.Fatalf("unexpected body %q", body)
	}
	// Host header must be preserved for the backend, not rewritten to the guest IP.
	if wantHost := "host=webvm.hivemind.tools"; body[len(body)-len(wantHost):] != wantHost {
		t.Fatalf("Host not preserved: %q", body)
	}
}

func TestProxyResumesPausedSandbox(t *testing.T) {
	ps := newProxyStack(t)
	ctx := context.Background()

	if _, err := ps.mgr.Create(ctx, "sleepy", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	port := backendOn(t, "awake")
	if err := ps.store.Upsert(routes.Route{
		Subdomain: "sleepy", Sandbox: "sleepy", Owner: "alice", Port: port,
	}); err != nil {
		t.Fatal(err)
	}

	if err := ps.mgr.Pause(ctx, "sleepy"); err != nil {
		t.Fatal(err)
	}
	if box, _ := ps.mgr.Get("sleepy"); box.State != vmm.StatePaused {
		t.Fatalf("expected paused, got %s", box.State)
	}

	code, body := ps.get(t, "sleepy.hivemind.tools")
	if code != http.StatusOK {
		t.Fatalf("expected 200 after resume, got %d (%s)", code, body)
	}
	if box, _ := ps.mgr.Get("sleepy"); box.State != vmm.StateRunning {
		t.Fatalf("proxy hit should have resumed sandbox, state=%s", box.State)
	}
}

func TestProxyUnknownHosts(t *testing.T) {
	ps := newProxyStack(t)

	// No route registered for this subdomain.
	if code, _ := ps.get(t, "ghost.hivemind.tools"); code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown route, got %d", code)
	}
	// Host outside the proxy domain.
	if code, _ := ps.get(t, "example.com"); code != http.StatusNotFound {
		t.Fatalf("expected 404 for foreign host, got %d", code)
	}
}

// getHTML issues a request with a browser-style Accept header.
func (ps *proxyStack) getHTML(t *testing.T, host string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	req.Host = host
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	rec := httptest.NewRecorder()
	ps.proxy.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body), rec.Header().Get("Content-Type")
}

func TestProxyErrorPages(t *testing.T) {
	ps := newProxyStack(t)
	ctx := context.Background()

	// Unknown route + browser Accept -> styled HTML 404.
	code, body, ct := ps.getHTML(t, "ghost.hivemind.tools")
	if code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", code)
	}
	if !strings.Contains(ct, "text/html") || !strings.Contains(body, "Nothing is forwarded here") {
		t.Fatalf("expected HTML error page, got ct=%q body=%q", ct, body)
	}

	// Same URL without a browser Accept -> plain text for curl and friends.
	code, body = ps.get(t, "ghost.hivemind.tools")
	if code != http.StatusNotFound || strings.Contains(body, "<html") {
		t.Fatalf("expected plain-text 404, got %d %q", code, body)
	}

	// Running sandbox whose forwarded port has no listener -> 502 naming the port.
	if _, err := ps.mgr.Create(ctx, "deadport", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // free the port so nothing is listening
	if err := ps.store.Upsert(routes.Route{
		Subdomain: "deadport", Sandbox: "deadport", Owner: "alice", Port: port,
	}); err != nil {
		t.Fatal(err)
	}
	code, body, ct = ps.getHTML(t, "deadport.hivemind.tools")
	if code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d (%s)", code, body)
	}
	if !strings.Contains(ct, "text/html") || !strings.Contains(body, fmt.Sprintf("Nothing is listening on port %d", port)) {
		t.Fatalf("expected HTML 502 naming port %d, got ct=%q body=%q", port, ct, body)
	}
}

func TestDefaultRouteCreatedOnCreate(t *testing.T) {
	ps := newProxyStack(t)
	ctx := context.Background()

	if _, err := ps.mgr.Create(ctx, "auto", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	r, ok, err := ps.store.GetBySubdomain("auto")
	if err != nil || !ok {
		t.Fatalf("default route missing: ok=%v err=%v", ok, err)
	}
	if r.Sandbox != "auto" || r.Port != routes.DefaultPort {
		t.Fatalf("unexpected default route: %+v", r)
	}

	// Destroy must clean routes up.
	if err := ps.mgr.Destroy(ctx, "auto"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ps.store.GetBySubdomain("auto"); ok {
		t.Fatal("route should be removed on destroy")
	}
}
