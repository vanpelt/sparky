package sparkbox_test

// End-to-end test of the HTTP proxy edge over the mock stack: a route maps a
// subdomain to a sandbox + port, and a request to <sub>.hivemind.tools is
// reverse-proxied to a backend standing in for a service inside the VM. Also
// covers resume-on-connect: hitting a paused sandbox's URL wakes it.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/proxy"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
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

// The round trip, the resume-on-request, the dead-port 502 and the default
// route moved to placement_e2e_test.go, where each runs twice — once against a
// sandbox on this machine and once against one on another. Every one of them
// reaches into a guest, and reaching into a guest is what a placement changes.
// What stays here is the edge's own dispatch: what happens when there is no
// sandbox at the other end at all, and which handler wins for a given host.

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

// TestProxyErrorPages is the content-negotiation half: the same missing route
// renders as a styled page for a browser and as plain text for curl. The 502
// for a running sandbox whose port has no listener is in
// placement_e2e_test.go, because it is an assertion about a guest.
func TestProxyErrorPages(t *testing.T) {
	ps := newProxyStack(t)

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
}

func TestReservedSubdomainBeatsRouteLookup(t *testing.T) {
	ps := newProxyStack(t)
	ctx := context.Background()

	// A sandbox whose default route claims the same subdomain must lose to the
	// reserved handler — reserved dispatch runs before route lookup.
	if _, err := ps.mgr.Create(ctx, "taken", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	ps.proxy.(*proxy.Server).SetReserved("taken", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "reserved-handler")
	}))

	code, body := ps.get(t, "taken.hivemind.tools")
	if code != http.StatusOK || body != "reserved-handler" {
		t.Fatalf("reserved subdomain not dispatched to handler: %d %q", code, body)
	}

	// A reserved SUFFIX must beat the route table for the same reason, and the
	// attack is real rather than theoretical: subdomains may contain hyphens
	// (the advertised `web-myvm` shape), so a row named "victim-xterm" is the
	// exact host alice's browser terminal lives on. ValidSubdomain refuses that
	// name now, but this store write bypasses it deliberately — rows predating
	// the rule exist, and the edge must not depend on the store to be safe.
	// Route-lookup-first would hand mallory the shell page.
	ps.proxy.(*proxy.Server).SetReservedSuffix("xterm", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := proxy.SuffixName(r)
		if !ok {
			t.Error("suffix handler reached without a name in context")
		}
		fmt.Fprint(w, "xterm-handler:"+name)
	}))
	// The store refuses the name outright — the first line of defence, and the
	// one that keeps an operator from creating this by accident.
	if err := ps.store.Upsert(routes.Route{
		Subdomain: "victim-xterm", Sandbox: "taken", Owner: "mallory", Port: 8000,
	}); err == nil {
		t.Fatal("store accepted a route ending in the reserved terminal suffix")
	}
	// Which is exactly why the row here is written behind its back: a database
	// predating the rule can hold one, and the edge's ordering must hold on its
	// own rather than by trusting whatever put the row there.
	insertRouteRaw(t, ps.dir, "victim-xterm", "taken", "mallory", 8000)

	code, body = ps.get(t, "victim-xterm.hivemind.tools")
	if code != http.StatusOK || body != "xterm-handler:victim" {
		t.Fatalf("hostile route shadowed the xterm suffix: %d %q", code, body)
	}
}

// insertRouteRaw writes a routes row straight to sqlite, bypassing the store's
// validation. Only for constructing states the store will no longer produce but
// an older one might have left on disk.
func insertRouteRaw(t *testing.T, dir, subdomain, sandbox, owner string, port int) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(
		`INSERT INTO routes (subdomain, sandbox, owner, port, visibility, created_at) VALUES (?,?,?,?,?,?)`,
		subdomain, sandbox, owner, port, routes.VisibilityPrivate, time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
}
