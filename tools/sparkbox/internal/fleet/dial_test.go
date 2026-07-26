package fleet_test

// The fleet dialer: which machine a synthetic address routes to, what a caller
// reads when that machine is not there, and — the one that is invisible until
// production — that a pooled connection survives the request that dialed it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// dialingNode is a machine whose guests can actually be reached, which fakeNode
// cannot. It stands in for a link: the gateway hands it a sandbox name, a kind
// and a port and never an address, and it resolves that to something of its own
// — here, one backend it was built around.
//
// dials counts every stream this machine was asked to open, which is what the
// pooling test measures. Counting here rather than at the http.Transport is the
// whole point: the transport is the thing under test, so a counter inside it
// would be measuring itself.
type dialingNode struct {
	*fakeNode
	backend string

	dials  atomic.Int64
	asked  atomic.Value // the last (sandbox, kind, port) it was asked for
	failed error
}

type dialAsk struct {
	sandbox string
	kind    string
	port    int
}

func newDialingNode(name, backend string) *dialingNode {
	return &dialingNode{fakeNode: newFakeNode(name), backend: backend}
}

func (n *dialingNode) DialGuest(ctx context.Context, sandbox, kind string, port int) (net.Conn, error) {
	n.record("dial")
	n.dials.Add(1)
	n.asked.Store(dialAsk{sandbox: sandbox, kind: kind, port: port})
	if n.failed != nil {
		return nil, n.failed
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", n.backend)
}

func (n *dialingNode) lastAsk() dialAsk {
	ask, _ := n.asked.Load().(dialAsk)
	return ask
}

func attachDialer(t *testing.T, f *fleet.Fleet, n *dialingNode) {
	t.Helper()
	detach, err := f.Attach(n)
	if err != nil {
		t.Fatalf("attach %s: %v", n.Name(), err)
	}
	t.Cleanup(detach)
}

// dialRig is a fleet with one reachable machine on it.
type dialRig struct {
	fleet   *fleet.Fleet
	node    *dialingNode
	served  *atomic.Int64
	backend *httptest.Server
}

func newDialRig(t *testing.T) *dialRig {
	t.Helper()
	served := &atomic.Int64{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		fmt.Fprintf(w, "hello from %s", r.Host)
	}))
	t.Cleanup(backend.Close)

	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	n := newDialingNode("boxb", strings.TrimPrefix(backend.URL, "http://"))
	attachDialer(t, f, n)
	return &dialRig{fleet: f, node: n, served: served, backend: backend}
}

// TestOneDialServesTwoKeepAliveRequests is the pooling regression, and it is
// here because the bug it guards against is invisible without it.
//
// A tunneled connection that is torn down when the request that dialed it ends
// still works: every request succeeds, nothing logs, nothing errors. What it
// does is turn every remote web request into a fresh SSH channel open — a round
// trip to the node and a resolve on it — which on a page with fifty assets is
// fifty of them. The failure is a performance cliff with no symptom, so the
// only thing that can catch it is a test that counts.
//
// Two sequential keep-alive requests, exactly one dial. The bodies are read to
// completion and closed because an unread body is never returned to the pool:
// skipping that would make this test pass for the wrong reason and go on
// passing after the bug came back.
func TestOneDialServesTwoKeepAliveRequests(t *testing.T) {
	rig := newDialRig(t)
	client := &http.Client{Transport: &http.Transport{DialContext: rig.fleet.DialContext}}
	t.Cleanup(client.CloseIdleConnections)

	url := "http://" + fleet.Host("demo", "boxb") + ":8000/"
	for i := 0; i < 2; i++ {
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("request %d: body: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
		// The guest is told the name the browser asked for, never the synthetic
		// one — asserted here so a dial that started rewriting Host would fail
		// in the same test that watches the pool.
		if want := "hello from " + fleet.Host("demo", "boxb") + ":8000"; string(body) != want {
			t.Fatalf("request %d: body %q, want %q", i, body, want)
		}
	}
	if got := rig.served.Load(); got != 2 {
		t.Fatalf("the backend served %d requests, want 2", got)
	}
	if got := rig.node.dials.Load(); got != 1 {
		t.Fatalf("the machine was asked to open %d streams for 2 keep-alive requests, want exactly 1", got)
	}
}

// TestTheDialerNamesASandboxAndNeverAnAddress pins the reverse stream's whole
// reason for existing. Everything above the dialer holds a synthetic .invalid
// name, so there is no guest address at this layer to leak or to get stale —
// the machine that booted the VM is the only thing that resolves one.
func TestTheDialerNamesASandboxAndNeverAnAddress(t *testing.T) {
	rig := newDialRig(t)

	cases := []struct {
		name string
		addr string
		want dialAsk
	}{
		{
			name: "the ssh port is a name, because only the node knows the number",
			addr: net.JoinHostPort(fleet.Host("demo", "boxb"), fleet.SSHPort),
			want: dialAsk{sandbox: "demo", kind: nodelink.StreamSSH, port: 0},
		},
		{
			name: "a numeric port is a guest tcp port",
			addr: net.JoinHostPort(fleet.Host("demo", "boxb"), "8080"),
			want: dialAsk{sandbox: "demo", kind: nodelink.StreamTCP, port: 8080},
		},
		{
			name: "a different sandbox on the same machine",
			addr: net.JoinHostPort(fleet.Host("other", "boxb"), "1"),
			want: dialAsk{sandbox: "other", kind: nodelink.StreamTCP, port: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := rig.fleet.DialContext(context.Background(), "tcp", tc.addr)
			if err != nil {
				t.Fatalf("DialContext(%q): %v", tc.addr, err)
			}
			defer conn.Close()
			if got := rig.node.lastAsk(); got != tc.want {
				t.Errorf("the machine was asked for %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestAPortThatIsNotAPortIsRefusedHere is the one refusal the gateway makes on
// its own: a port outside the range is a name nothing minted, and sending it to
// a machine would spend a round trip to be told the same thing.
func TestAPortThatIsNotAPortIsRefusedHere(t *testing.T) {
	rig := newDialRig(t)
	for _, port := range []string{"0", "65536", "-1", "http"} {
		addr := net.JoinHostPort(fleet.Host("demo", "boxb"), port)
		conn, err := rig.fleet.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			conn.Close()
			t.Errorf("DialContext(%q) succeeded", addr)
			continue
		}
		if rig.node.took("dial") {
			t.Errorf("port %q reached the machine", port)
		}
	}
}

// TestAnOfflineMachineIsAnsweredAtOnce — a dial for a machine that is not
// linked must be the typed offline answer, and it must arrive immediately.
//
// The timing is the assertion that matters. The SSH gateway spends up to
// fifteen seconds retrying a dial that might yet come good; a node that is not
// there is not going to come good in that window, and a user sitting at a
// prompt for fifteen seconds before being told the machine is offline is the
// difference between an outage and a hang.
func TestAnOfflineMachineIsAnsweredAtOnce(t *testing.T) {
	cases := []struct {
		name string
		node string
		prep func(*dialRig)
	}{
		{
			name: "a machine that has gone quiet",
			node: "boxb",
			prep: func(r *dialRig) { r.node.setOnline(false) },
		},
		{
			name: "a machine this gateway has never linked",
			node: "boxc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newDialRig(t)
			if tc.prep != nil {
				tc.prep(rig)
			}
			addr := net.JoinHostPort(fleet.Host("demo", tc.node), fleet.SSHPort)
			started := time.Now()
			conn, err := rig.fleet.DialContext(context.Background(), "tcp", addr)
			took := time.Since(started)
			if err == nil {
				conn.Close()
				t.Fatal("DialContext reached a machine that is not there")
			}
			if !fleet.IsNodeUnreachable(err) {
				t.Fatalf("err = %v (%T), want the node-unreachable answer", err, err)
			}
			e := ctlops.AsError("dial", err)
			if e.Status != http.StatusServiceUnavailable || e.Exit != 1 {
				t.Errorf("offline answered %d/exit %d, want 503/exit 1", e.Status, e.Exit)
			}
			if !strings.Contains(e.Msg, tc.node) || !strings.Contains(e.Msg, "demo") {
				t.Errorf("msg = %q, want it to name the sandbox and the machine", e.Msg)
			}
			if took > time.Second {
				t.Errorf("the answer took %v; an offline machine must not cost a dial budget", took)
			}
			if rig.node.took("dial") {
				t.Error("an offline machine was asked to open a stream")
			}
		})
	}
}

// TestAnOrdinaryAddressIsDialedDirectly is the single-box no-op guarantee.
//
// Every deployment with one machine keeps real addresses on its records, so
// every dial falls through to the package's own net.Dialer — and it must fall
// through UNCHANGED, down to the concrete type, because installing this dialer
// must not be able to alter a data path that was never remote.
func TestAnOrdinaryAddressIsDialedDirectly(t *testing.T) {
	rig := newDialRig(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }() //nolint:errcheck // the assertion is on the read side
		}
	}()

	conn, err := rig.fleet.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()
	if _, ok := conn.(*net.TCPConn); !ok {
		t.Errorf("a host-network dial came back as %T, want the *net.TCPConn net.Dialer minted", conn)
	}
	if rig.node.took("dial") {
		t.Error("a host-network address was routed to a machine")
	}
	if _, err := conn.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("read back %q, %v", buf, err)
	}
}

// TestASandboxOnThisMachineIsDialedLocally is the other half of that guarantee,
// for the address form a local record could still take: the local adapter
// resolves it from the local manager and dials the host network, so nothing is
// tunneled to reach a VM in this process.
func TestASandboxOnThisMachineIsDialedLocally(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	b := mustCreate(t, f, "here", "alice")
	if strings.Contains(b.SSHAddr, fleet.Suffix) {
		t.Fatalf("a local record was given the synthetic address %q", b.SSHAddr)
	}

	addr := net.JoinHostPort(fleet.Host("here", mgr.NodeName()), fleet.SSHPort)
	conn, err := f.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("DialContext(%q): %v", addr, err)
	}
	defer conn.Close()
	if got, want := conn.RemoteAddr().String(), b.SSHAddr; got != want {
		t.Errorf("dialed %s, want the guest's real address %s", got, want)
	}
}

// TestCloseTearsEveryStreamDown. A tunneled conn's lifetime belongs to whoever
// holds it — that is the whole of DialContext's no-close-bound rule — and the
// busiest holder is an idle connection pool that will keep one for a minute and
// a half after the request that dialed it. So a fleet that shuts down without
// closing them leaves connections riding links it has stopped accounting for.
func TestCloseTearsEveryStreamDown(t *testing.T) {
	rig := newDialRig(t)

	var conns []net.Conn
	for i := 0; i < 3; i++ {
		c, err := rig.fleet.DialContext(context.Background(), "tcp", net.JoinHostPort(fleet.Host("demo", "boxb"), "80"))
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	if err := rig.fleet.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i, c := range conns {
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Errorf("stream %d was still readable after the fleet closed", i)
		}
	}
	// And a dial afterwards is refused rather than handed a conn nobody will
	// ever tear down.
	if c, err := rig.fleet.DialContext(context.Background(), "tcp", net.JoinHostPort(fleet.Host("demo", "boxb"), "80")); err == nil {
		c.Close()
		t.Error("a closed fleet opened a new stream")
	}
}

// TestAClosedStreamIsForgotten pins that the registry does not grow. A gateway
// that remembered every connection it ever dialed would leak one entry per
// request for as long as it ran.
func TestAClosedStreamIsForgotten(t *testing.T) {
	rig := newDialRig(t)
	addr := net.JoinHostPort(fleet.Host("demo", "boxb"), "80")

	// Opened and closed far more times than could be held at once, so a
	// registry that never forgot would be visible in the count below.
	for i := 0; i < 200; i++ {
		c, err := rig.fleet.DialContext(context.Background(), "tcp", addr)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		c.Close()
		c.Close() // idempotent: net/http closes a conn it has already given up on
	}
	if n := fleet.LiveStreams(rig.fleet); n != 0 {
		t.Fatalf("the fleet is still holding %d streams it was told to forget", n)
	}
}

// TestAHalfCloseReachesTheGuest. The tracked wrapper must forward CloseWrite:
// the secret syncer half-closes its stdin to say "that is the whole script",
// and a guest reading to EOF that never comes hangs rather than fails.
func TestAHalfCloseReachesTheGuest(t *testing.T) {
	rig := newDialRig(t)

	read := make(chan []byte, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c) // returns only when the far side half-closes
		read <- b
	}()
	rig.node.backend = ln.Addr().String()

	conn, err := rig.fleet.DialContext(context.Background(), "tcp", net.JoinHostPort(fleet.Host("demo", "boxb"), "80"))
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("script")); err != nil {
		t.Fatalf("write: %v", err)
	}
	cw, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("the dialer handed back a conn that cannot half-close")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	select {
	case got := <-read:
		if string(got) != "script" {
			t.Fatalf("the guest read %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guest never saw end-of-input; the half-close was swallowed")
	}
}

// TestADialFailureFromTheMachineIsNotRewritten. The machine's own answer is the
// only thing that tells "no such sandbox over there" from "nothing is listening
// inside the guest", so the dialer must not launder it.
func TestADialFailureFromTheMachineIsNotRewritten(t *testing.T) {
	rig := newDialRig(t)
	sentinel := errors.New("connection refused inside the guest")
	rig.node.failed = sentinel

	_, err := rig.fleet.DialContext(context.Background(), "tcp", net.JoinHostPort(fleet.Host("demo", "boxb"), "80"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the machine's own answer", err)
	}
}
