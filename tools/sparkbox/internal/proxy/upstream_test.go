package proxy

// The upstream transport and its connection pool. A sandbox on another machine
// is reached through a dialer rather than a direct TCP connect, and the record
// it comes back on carries a synthetic per-sandbox host name instead of an
// address — because every machine in a fleet mints the same guest IPs. These
// tests pin the two properties that makes work: the pool is keyed per sandbox,
// and a Server's dialer belongs to that Server.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

// placedManager stands in for a fleet router: EnsureRunning answers with the
// synthetic host name a sandbox on machine `node` carries, never an address.
type placedManager map[string]string // sandbox -> node

func (m placedManager) EnsureRunning(_ context.Context, name string) (*host.Sandbox, error) {
	node, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("no sandbox named %q", name)
	}
	return &host.Sandbox{Name: name, HostIP: name + "." + node + ".sandbox.invalid"}, nil
}

// backend is a stand-in for a guest app that names itself in every response and
// counts the TCP connections it was reached over, so a test can tell a reused
// connection from a fresh one.
type backend struct {
	addr  string
	conns atomic.Int64
}

func newBackend(t *testing.T, name string) *backend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{addr: ln.Addr().String()}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, name) //nolint:errcheck
		}),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				b.conns.Add(1)
			}
		},
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return b
}

// TestUpstreamPoolIsPerSandbox proves that two sandboxes whose forwarded
// address differs only by the synthetic host name never share a pooled
// connection. Both routes point at port 8000, so the host name is the ONLY
// distinguishing byte — which is exactly the shape a fleet produces, since
// every machine hands its guests the same 172.30.<idx>.2. A transport keyed on
// a shared address would let the second sandbox's request be answered by the
// first sandbox's backend, with no error and no log line.
func TestUpstreamPoolIsPerSandbox(t *testing.T) {
	alpha := newBackend(t, "alpha")
	beta := newBackend(t, "beta")

	// The dialer is the fleet seam: it resolves a synthetic host to the machine
	// that holds it. Both sandboxes are dialed on the same port on purpose.
	const port = 8000
	upstreams := map[string]string{
		fmt.Sprintf("alpha.n1.sandbox.invalid:%d", port): alpha.addr,
		fmt.Sprintf("beta.n2.sandbox.invalid:%d", port):  beta.addr,
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		to, ok := upstreams[addr]
		if !ok {
			return nil, fmt.Errorf("nothing placed at %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, to)
	}

	store, err := routes.Open(filepath.Join(t.TempDir(), "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	for _, sub := range []string{"alpha", "beta"} {
		if err := store.Upsert(routes.Route{Subdomain: sub, Sandbox: sub, Owner: "alice", Port: port}); err != nil {
			t.Fatal(err)
		}
	}

	s := New(placedManager{"alpha": "n1", "beta": "n2"}, store, "hivemind.tools",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetDialer(dial)

	// Interleaved so every request after the first two finds a warm pool to
	// pick the wrong connection out of.
	const rounds = 6
	for i := range rounds {
		for _, want := range []string{"alpha", "beta"} {
			host := want + ".hivemind.tools"
			req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
			req.Host = host
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			body, _ := io.ReadAll(rec.Result().Body)
			if rec.Code != http.StatusOK {
				t.Fatalf("round %d: %s answered %d: %s", i, host, rec.Code, body)
			}
			if got := string(body); got != want {
				t.Fatalf("round %d: %s was served by %q, want %q", i, host, got, want)
			}
		}
	}

	// One connection each: the requests really were pooled, so the check above
	// was exercising reuse rather than a fresh dial every time.
	if n := alpha.conns.Load(); n != 1 {
		t.Errorf("alpha was dialed %d times, want 1 (no connection reuse — the pool test proved nothing)", n)
	}
	if n := beta.conns.Load(); n != 1 {
		t.Errorf("beta was dialed %d times, want 1 (no connection reuse — the pool test proved nothing)", n)
	}
}

// TestDefaultTransportDialsDirectly pins that a Server built without SetDialer
// makes an ordinary TCP connection: the single-box path must not depend on
// anything the fleet installs.
func TestDefaultTransportDialsDirectly(t *testing.T) {
	app := newBackend(t, "direct")
	host, portStr, err := net.SplitHostPort(app.addr)
	if err != nil {
		t.Fatal(err)
	}
	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}

	store, err := routes.Open(filepath.Join(t.TempDir(), "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Upsert(routes.Route{Subdomain: "local", Sandbox: "local", Owner: "alice", Port: port}); err != nil {
		t.Fatal(err)
	}

	s := New(localManager(host), store, "hivemind.tools", slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "http://local.hivemind.tools/", nil)
	req.Host = "local.hivemind.tools"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	if rec.Code != http.StatusOK || string(body) != "direct" {
		t.Fatalf("direct dial answered %d %q", rec.Code, body)
	}
}

// localManager is the single-box shape: a real guest IP, no fleet in sight.
type localManager string

func (ip localManager) EnsureRunning(_ context.Context, name string) (*host.Sandbox, error) {
	return &host.Sandbox{Name: name, HostIP: string(ip)}, nil
}

// TestSetDialerRebuildsTransport pins that SetDialer takes effect on the
// reverse proxy itself, not just on a field nothing reads.
func TestSetDialerRebuildsTransport(t *testing.T) {
	s := New(placedManager{}, nil, "hivemind.tools", slog.New(slog.NewTextHandler(io.Discard, nil)))
	before := s.rp.Transport
	s.SetDialer(func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("unused")
	})
	if s.rp.Transport == before {
		t.Fatal("SetDialer left the reverse proxy on the old transport")
	}
	if s.rp.Transport != http.RoundTripper(s.transport) {
		t.Fatal("SetDialer left the reverse proxy and the Server disagreeing about the transport")
	}
	// The tuned fields are the reason this transport exists at all; a rebuild
	// that quietly reverted to http.Transport's defaults would cost every guest
	// app a fresh handshake on its third concurrent request.
	if s.transport.MaxIdleConnsPerHost != 64 || !s.transport.DisableCompression {
		t.Errorf("rebuilt transport lost its tuning: %+v", s.transport)
	}
}

// TestUnplacedSandboxFailsClosed pins the .invalid contract from the other
// side: when no dialer is installed, a synthetic host name resolves nowhere, so
// a stray remote record can never be served by the local machine's sandbox at
// the same guest IP.
func TestUnplacedSandboxFailsClosed(t *testing.T) {
	store, err := routes.Open(filepath.Join(t.TempDir(), "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Upsert(routes.Route{Subdomain: "ghost", Sandbox: "ghost", Owner: "alice", Port: 8000}); err != nil {
		t.Fatal(err)
	}

	s := New(placedManager{"ghost": "n9"}, store, "hivemind.tools", slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "http://ghost.hivemind.tools/", nil)
	req.Host = "ghost.hivemind.tools"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unresolvable upstream answered %d, want 502", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Result().Body); strings.Contains(string(body), "sandbox.invalid") {
		t.Errorf("error page leaked the fleet address: %s", body)
	}
}
