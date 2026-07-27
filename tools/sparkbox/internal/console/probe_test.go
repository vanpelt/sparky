package console

// The listening probe's dialer seam and its budget, and the router the console
// drives. TestConsoleRouteListeningStatus already covers the default
// host-network path against real sockets; what matters here is that an injected
// dialer is asked verbatim — on a fleet the address only means something on the
// machine that minted it — that the wider tunneled budget is spent only on the
// sandboxes that really are on another machine, and that every lifecycle action
// goes through whoever was installed rather than past them into the local
// manager.

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

func newProbeHandler(t *testing.T, dial Dialer) *Handler {
	t.Helper()
	h := New(nil, nil, "hivemind.tools", testPassword, false, slog.New(slog.DiscardHandler))
	if dial != nil {
		h.SetDialer(dial)
	}
	return h
}

func TestListeningAsksTheConfiguredDialer(t *testing.T) {
	const addr = "demo.node1.sandbox.invalid:8080"
	tests := []struct {
		name string
		conn func() (net.Conn, error)
		want bool
	}{
		{"accepted", func() (net.Conn, error) {
			ours, theirs := net.Pipe()
			t.Cleanup(func() { theirs.Close() }) //nolint:errcheck
			return ours, nil
		}, true},
		{"refused", func() (net.Conn, error) { return nil, errors.New("connection refused") }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			h := newProbeHandler(t, func(_ context.Context, network, a string) (net.Conn, error) {
				got = append(got, network+" "+a)
				return tc.conn()
			})
			if listening := h.listening(context.Background(), addr, true); listening != tc.want {
				t.Fatalf("listening = %v, want %v", listening, tc.want)
			}
			// The synthetic host must reach the dialer untouched: resolving it
			// here would be an NXDOMAIN, and resolving it as a local address
			// would probe this machine's own sandbox instead.
			if len(got) != 1 || got[0] != "tcp "+addr {
				t.Fatalf("dialer saw %v, want one [tcp %s]", got, addr)
			}
		})
	}
}

// The fleet dialer is installed on every deployment, fleet or not, so the
// budget cannot key off having one: a machine whose sandboxes are all its own
// must answer its dashboard in the 300ms it always did.
func TestListeningBudgetWidensOnlyForRemoteSandboxes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote bool
		want   time.Duration
	}{
		{"local", false, probeTimeout},
		{"remote", true, tunneledProbeTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var budget time.Duration
			h := newProbeHandler(t, func(ctx context.Context, _, _ string) (net.Conn, error) {
				dl, ok := ctx.Deadline()
				if !ok {
					t.Fatal("the probe context carried no deadline")
				}
				budget = time.Until(dl)
				return nil, errors.New("connection refused")
			})
			h.listening(context.Background(), "10.0.0.1:80", tc.remote)
			if budget > tc.want || budget < tc.want-50*time.Millisecond {
				t.Fatalf("probe budget = %v, want about %v", budget, tc.want)
			}
		})
	}
}

// fakeBoxes stands in for the fleet router: it answers reads from a fixed list
// and records every lifecycle call, so a test can tell whether the console
// asked the router or reached past it into the local manager.
type fakeBoxes struct {
	boxes []*host.Sandbox
	calls []string
}

func (f *fakeBoxes) Get(name string) (*host.Sandbox, bool) {
	for _, b := range f.boxes {
		if b.Name == name {
			return b, true
		}
	}
	return nil, false
}

func (f *fakeBoxes) List() []*host.Sandbox { return f.boxes }

func (f *fakeBoxes) EnsureReady(_ context.Context, name string) (*host.Sandbox, error) {
	f.calls = append(f.calls, "resume "+name)
	b, _ := f.Get(name)
	return b, nil
}

func (f *fakeBoxes) Pause(_ context.Context, name string) error {
	f.calls = append(f.calls, "pause "+name)
	return nil
}

func (f *fakeBoxes) Archive(_ context.Context, name string) error {
	f.calls = append(f.calls, "archive "+name)
	return nil
}

func (f *fakeBoxes) Destroy(_ context.Context, name string) error {
	f.calls = append(f.calls, "destroy "+name)
	return nil
}

func (f *fakeBoxes) SetPinned(name string, _ bool) error {
	f.calls = append(f.calls, "pin "+name)
	return nil
}

func (f *fakeBoxes) Snapshot(_ context.Context, box, snapName, owner string) (*host.Snapshot, error) {
	f.calls = append(f.calls, "snapshot "+box)
	return &host.Snapshot{Name: snapName, Owner: owner, FromBox: box}, nil
}

func (f *fakeBoxes) DeleteSnapshot(_ context.Context, snapName, _ string) error {
	f.calls = append(f.calls, "snapshot.rm "+snapName)
	return nil
}

func (f *fakeBoxes) Fork(_ context.Context, snapName, newName, owner string, _, _ int64) (*host.Sandbox, error) {
	f.calls = append(f.calls, "fork "+snapName+" "+newName)
	return &host.Sandbox{Name: newName, Owner: owner}, nil
}

// One listing, two machines: the sandbox on this one is dialed on the local
// budget and the one elsewhere on the tunneled budget.
func TestListProbesEachSandboxOnItsOwnBudget(t *testing.T) {
	h, _, store := newTestHandler(t)
	h.SetSandboxes(&fakeBoxes{boxes: []*host.Sandbox{
		{Name: "here", Owner: "alice", State: vmm.StateRunning, Node: "testnode", HostIP: "172.30.1.2"},
		{Name: "there", Owner: "alice", State: vmm.StateRunning, Node: "nodeb", HostIP: "there.nodeb.sandbox.invalid"},
	}})
	var mu sync.Mutex
	budgets := map[string]time.Duration{}
	h.SetDialer(func(ctx context.Context, _, addr string) (net.Conn, error) {
		guest, _, _ := net.SplitHostPort(addr)
		dl, _ := ctx.Deadline()
		mu.Lock()
		budgets[guest] = time.Until(dl)
		mu.Unlock()
		return nil, errors.New("connection refused")
	})
	for _, name := range []string{"here", "there"} {
		if err := store.Upsert(routes.Route{Subdomain: name, Sandbox: name, Owner: "alice", Port: 8000}); err != nil {
			t.Fatal(err)
		}
	}

	srv := h.Handler()
	req := httptest.NewRequest("GET", "/api/sandboxes", nil)
	req.AddCookie(login(t, srv, testPassword))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, want 200", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := budgets["172.30.1.2"]; got > probeTimeout || got < probeTimeout-50*time.Millisecond {
		t.Fatalf("local probe budget = %v, want about %v", got, probeTimeout)
	}
	if got := budgets["there.nodeb.sandbox.invalid"]; got > tunneledProbeTimeout || got < tunneledProbeTimeout-50*time.Millisecond {
		t.Fatalf("remote probe budget = %v, want about %v", got, tunneledProbeTimeout)
	}
}

// Every lifecycle action must reach the installed router, because that is what
// keeps the placement ledger honest: a destroy that went straight to the local
// manager would leave the sandbox's name reserved to a machine that no longer
// holds it, and nobody could ever use the name again.
func TestLifecycleGoesThroughTheInstalledRouter(t *testing.T) {
	h, mgr, _ := newTestHandler(t)
	if _, err := mgr.Create(context.Background(), "swift-otter", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	fake := &fakeBoxes{boxes: []*host.Sandbox{{Name: "swift-otter", Owner: "alice", State: vmm.StateRunning}}}
	h.SetSandboxes(fake)
	srv := h.Handler()
	cookie := login(t, srv, testPassword)

	for _, call := range []struct{ method, path, want string }{
		{"POST", "/api/sandboxes/swift-otter/pause", "pause swift-otter"},
		{"POST", "/api/sandboxes/swift-otter/resume", "resume swift-otter"},
		{"POST", "/api/sandboxes/swift-otter/archive", "archive swift-otter"},
		{"POST", "/api/sandboxes/swift-otter/pin", "pin swift-otter"},
		{"POST", "/api/sandboxes/swift-otter/unpin", "pin swift-otter"},
		{"DELETE", "/api/sandboxes/swift-otter", "destroy swift-otter"},
	} {
		fake.calls = nil
		req := httptest.NewRequest(call.method, call.path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d, want 200", call.method, call.path, rec.Code)
		}
		if len(fake.calls) == 0 || fake.calls[0] != call.want {
			t.Fatalf("%s %s drove %v, want %q first", call.method, call.path, fake.calls, call.want)
		}
	}
	// The local manager was never asked, so a fleet deployment's ledger and its
	// machines cannot drift apart behind the console's back.
	if _, ok := mgr.Get("swift-otter"); !ok {
		t.Fatal("the console reached past the router into the local manager")
	}
}

// The dashboard must not serialize guest addresses. Once the console is pointed
// at the fleet they are the synthetic .sandbox.invalid names minted for another
// machine's sandboxes: they resolve nowhere, they mean nothing in a browser,
// and shipping them is how something ends up dialing one.
func TestDashboardDropsGuestAddresses(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.SetSandboxes(&fakeBoxes{boxes: []*host.Sandbox{{
		Name: "there", Owner: "alice", State: vmm.StateRunning, Node: "nodeb",
		HostIP:  "there.nodeb.sandbox.invalid",
		SSHAddr: "there.nodeb.sandbox.invalid:ssh",
		GuestV6: "2001:db8::2",
	}}})
	srv := h.Handler()

	for _, call := range []struct{ method, path string }{
		{"GET", "/api/sandboxes"},
		{"POST", "/api/sandboxes/there/resume"},
	} {
		req := httptest.NewRequest(call.method, call.path, nil)
		req.AddCookie(login(t, srv, testPassword))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", call.path, rec.Code)
		}
		body := rec.Body.String()
		for _, k := range []string{"ssh_addr", "host_ip", "guest_v6"} {
			if strings.Contains(body, `"`+k+`"`) {
				t.Fatalf("%s leaked %s: %s", call.path, k, body)
			}
		}
		// The node name is not an address, it is what the page shows in its own
		// column, and it must survive the stripping.
		if !strings.Contains(body, `"node":"nodeb"`) {
			t.Fatalf("%s dropped the node name: %s", call.path, body)
		}
	}
}
