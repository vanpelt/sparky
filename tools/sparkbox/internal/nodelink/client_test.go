package nodelink

// The node half, driven against the real gateway half over a real loopback SSH
// connection: one process, two ends, nothing stubbed but the roster (which
// lives in internal/sshgw and cannot be imported from here without a cycle).
//
// What these tests are really about is what a node does when the gateway is not
// there — waiting for approval, reconnecting after a restart, refusing to fail
// upward — because that is the only part of the link that has to be right for
// the VMs on this machine to be safe.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// waitFor polls rather than sleeping blindly: every deadline in this file is a
// failure with a sentence attached, never a hung test binary.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- the gateway, as a node sees one ---

// gwHarness is a gateway's node door: a real SSH listener whose session handler
// reads a hello with ReadHello and then either refuses it or hands it to Serve.
// The roster is a boolean, because everything roster policy decides has already
// been tested where it lives; what a node cares about is which of the two
// answers it got.
type gwHarness struct {
	addr        string
	upstreamPub string
	// hostKey outlives a restart on purpose: a node pins the first key it is
	// offered, so a gateway that came back with a new one would be refused —
	// which is the pin working, not the reconnect failing.
	hostKey xssh.Signer

	clients     chan *Client
	inventories chan InventoryMsg
	heartbeats  chan Heartbeat
	changed     chan ChangedMsg
	gone        chan GoneMsg
	paused      chan PausedMsg

	mu       sync.Mutex
	approved bool
	hellos   []Hello
	srv      *gssh.Server
}

func newGateway(t *testing.T, approved bool) *gwHarness {
	t.Helper()
	h := &gwHarness{
		upstreamPub: "ssh-ed25519 AAAAgateway-upstream sparkbox\n",
		hostKey:     testSigner(t),
		approved:    approved,
		// Buffered deep enough that a test which never reads a channel cannot
		// block the link it is testing.
		clients:     make(chan *Client, 8),
		inventories: make(chan InventoryMsg, 16),
		heartbeats:  make(chan Heartbeat, 64),
		changed:     make(chan ChangedMsg, 16),
		gone:        make(chan GoneMsg, 16),
		paused:      make(chan PausedMsg, 16),
	}
	h.start(t)
	t.Cleanup(h.stop)
	return h
}

// start listens and serves. Called again by the reconnect test, on the same
// address, standing in for a gateway that was restarted under a live node.
func (h *gwHarness) start(t *testing.T) {
	t.Helper()
	addr := h.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}
	h.addr = ln.Addr().String()

	srv := &gssh.Server{
		Handler:          h.handle,
		PublicKeyHandler: func(gssh.Context, gssh.PublicKey) bool { return true },
	}
	srv.AddHostKey(h.hostKey)
	go srv.Serve(ln) //nolint:errcheck // returns on Close
	h.mu.Lock()
	h.srv = srv
	h.mu.Unlock()
}

func (h *gwHarness) stop() {
	h.mu.Lock()
	srv := h.srv
	h.mu.Unlock()
	if srv != nil {
		srv.Close() //nolint:errcheck
	}
}

func (h *gwHarness) approve() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.approved = true
}

func (h *gwHarness) helloCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.hellos)
}

func (h *gwHarness) lastHello() Hello {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.hellos) == 0 {
		return Hello{}
	}
	return h.hellos[len(h.hellos)-1]
}

func (h *gwHarness) handle(s gssh.Session) {
	if cmd := s.Command(); len(cmd) != 1 || cmd[0] != LinkCommand {
		s.Exit(2) //nolint:errcheck
		return
	}
	conn, _ := s.Context().Value(gssh.ContextKeyConn).(xssh.Conn)

	g, err := ReadHello(s.Context(), s, HelloTimeout)
	if err != nil {
		_ = SendBye(s, CodeProtocolError, err.Error())
		s.Exit(2) //nolint:errcheck
		return
	}
	h.mu.Lock()
	h.hellos = append(h.hellos, g.Hello)
	approved := h.approved
	h.mu.Unlock()

	if !approved {
		_ = Refuse(s, g, Refusal(CodeNodePending,
			"node %q (SHA256:test) is waiting for approval. An operator runs:  ssh ctl@gw node approve %s",
			g.Hello.Node, g.Hello.Node))
		s.Exit(1) //nolint:errcheck
		return
	}

	client, wait, err := Serve(s.Context(), ServerOptions{
		Node:     g.Hello.Node,
		Greeting: g,
		Session:  s,
		Stderr:   s.Stderr(),
		Conn:     conn,
		Welcome:  Welcome{GatewayUpstreamPub: h.upstreamPub, Domain: "hivemind.tools"},
		Hooks: Hooks{
			OnInventory: func(_ string, inv InventoryMsg) InventoryAck {
				send(h.inventories, inv)
				return InventoryAck{}
			},
			OnHeartbeat: func(_ string, hb Heartbeat) { send(h.heartbeats, hb) },
			OnChanged:   func(_ string, m ChangedMsg) { send(h.changed, m) },
			OnGone:      func(_ string, m GoneMsg) { send(h.gone, m) },
			OnPaused:    func(_ string, m PausedMsg) { send(h.paused, m) },
		},
	})
	if err != nil {
		s.Exit(1) //nolint:errcheck
		return
	}
	defer client.Close()
	send(h.clients, client)
	_ = wait()
	s.Exit(0) //nolint:errcheck
}

// send records a message without ever blocking the link's read loop, which is
// the same contract the real hooks run under.
func send[T any](ch chan T, v T) {
	select {
	case ch <- v:
	default:
	}
}

func recv[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		var zero T
		t.Fatalf("no %s arrived", what)
		return zero
	}
}

// --- the node ---

// fakeManager is this machine's host manager, narrowed to what a link asks of
// it. A real *host.Manager satisfies the same interface (asserted in
// client.go); this one exists so a link test needs no driver and no VM.
type fakeManager struct {
	mu       sync.Mutex
	capacity host.NodeCapacity
	boxes    []*host.Sandbox
	snaps    []*host.Snapshot
}

func (m *fakeManager) Capacity() host.NodeCapacity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capacity
}

func (m *fakeManager) List() []*host.Sandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*host.Sandbox(nil), m.boxes...)
}

func (m *fakeManager) AllSnapshots() []*host.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*host.Snapshot(nil), m.snaps...)
}

// testManager is the whole fake a link now asks for: the reporting half below
// plus the lifecycle half in nodeops_test.go, which is where a test that drives
// a verb keeps its recorder.
func testManager() *opsManager {
	return &opsManager{fakeManager: &fakeManager{
		capacity: host.NodeCapacity{Node: "node-b", TotalMemMB: 8192, BudgetMemMB: 6144, TotalVCPUs: 8},
		boxes: []*host.Sandbox{{
			Name: "demo", Owner: "alice", Image: "ubuntu", State: vmm.StateRunning,
			VCPUs: 2, MemMB: 2048, HostIP: "172.30.7.2", SSHAddr: "172.30.7.2:22",
			CreatedAt: time.Now().Add(-time.Hour), LastActive: time.Now(),
		}},
		snaps: []*host.Snapshot{{
			Name: "base", Owner: "alice", Image: "snap-alice-base", FromBox: "demo",
			CreatedAt: time.Now().Add(-time.Hour),
		}},
	}}
}

// logBuffer captures log output for the tests that assert on a sentence an
// operator has to read. It takes a lock because slog handlers write from
// whichever goroutine logged.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runNode starts a supervisor and returns the channel its return value lands
// on. Nothing in this file may assume that channel ever fires: the contract is
// that it fires only on cancellation.
func runNode(t *testing.T, ctx context.Context, opts ClientOptions) chan error {
	t.Helper()
	if opts.Manager == nil {
		opts.Manager = testManager()
	}
	if opts.NodeName == "" {
		opts.NodeName = "node-b"
	}
	if opts.Key == nil {
		opts.Key = testSigner(t)
	}
	if opts.BackoffMin == 0 {
		opts.BackoffMin = 10 * time.Millisecond
	}
	if opts.BackoffMax == 0 {
		opts.BackoffMax = 50 * time.Millisecond
	}
	if opts.Heartbeat == 0 {
		opts.Heartbeat = 40 * time.Millisecond
	}
	done := make(chan error, 1)
	go func() { done <- RunClient(ctx, opts) }()
	return done
}

func testContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}

// TestNodeLinksAndReportsItself is the happy path from the node's side: dial,
// hello, welcome, the whole picture, then a heartbeat every beat — and a link
// the gateway can ask things of.
func TestNodeLinksAndReportsItself(t *testing.T) {
	gw := newGateway(t, true)
	ctx, _ := testContext(t)

	mgr := testManager()
	var welcomed Welcome
	welcomeSeen := make(chan struct{})
	runNode(t, ctx, ClientOptions{
		Gateway: gw.addr,
		Manager: mgr,
		Hello: func() Hello {
			return Hello{Driver: "mock", Version: "test", Release: "2026-07-22", GuestSubnet: "172.30.0.0/16"}
		},
		OnWelcome: func(w Welcome) error {
			welcomed = w
			close(welcomeSeen)
			return nil
		},
	})

	client := recv(t, gw.clients, "link")

	select {
	case <-welcomeSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("the node never accepted a welcome")
	}
	if welcomed.GatewayUpstreamPub != gw.upstreamPub {
		t.Errorf("welcome carried %q, want the gateway's upstream key %q", welcomed.GatewayUpstreamPub, gw.upstreamPub)
	}
	if welcomed.Node != "node-b" || welcomed.Protocol != Protocol {
		t.Errorf("welcome = %+v, want node-b on protocol %d", welcomed, Protocol)
	}

	// The hello carries what only the node process knows, and the two fields no
	// caller may state differently are stamped over whatever it supplied.
	hello := gw.lastHello()
	if hello.Node != "node-b" || hello.Protocol != Protocol || hello.Driver != "mock" {
		t.Errorf("hello = %+v, want node-b on protocol %d running mock", hello, Protocol)
	}
	if hello.Arch == "" || hello.OS == "" || hello.StartedAt.IsZero() {
		t.Errorf("hello omits the facts a scheduler needs: %+v", hello)
	}

	inv := recv(t, gw.inventories, "inventory")
	if len(inv.Sandboxes) != 1 || inv.Sandboxes[0].Name != "demo" || inv.Sandboxes[0].Owner != "alice" {
		t.Fatalf("inventory sandboxes = %+v, want the node's one record", inv.Sandboxes)
	}
	if inv.Sandboxes[0].State != string(vmm.StateRunning) {
		t.Errorf("inventory state = %q, want %q", inv.Sandboxes[0].State, vmm.StateRunning)
	}
	if len(inv.Snapshots) != 1 || inv.Snapshots[0].Name != "base" {
		t.Errorf("inventory snapshots = %+v, want the node's one template", inv.Snapshots)
	}

	// Heartbeats keep coming unprompted, carrying what the machine has left.
	for i := 0; i < 2; i++ {
		hb := recv(t, gw.heartbeats, "heartbeat")
		if hb.Capacity.TotalMemMB != 8192 {
			t.Fatalf("heartbeat %d capacity = %+v, want the node's 8192MB", i, hb.Capacity)
		}
	}
	waitFor(t, "the gateway to consider the node online", client.Online)
	if got := client.Capacity().TotalMemMB; got != 8192 {
		t.Errorf("the link reports %dMB, want the node's 8192MB", got)
	}
	boxes, snaps := client.Snapshot()
	if len(boxes) != 1 || len(snaps) != 1 {
		t.Errorf("the link caches %d sandboxes and %d snapshots, want one of each", len(boxes), len(snaps))
	}

	// And the node answers what the gateway asks. The ping is the liveness
	// probe the link is dropped over after two misses, so an unanswered one is
	// not a missing feature but a node that gets hung up on every minute.
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var echo PingReq
	if err := client.Do(rctx, TypePing, PingReq{Nonce: "abc"}, &echo); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if echo.Nonce != "abc" {
		t.Errorf("ping echoed %q, want %q", echo.Nonce, "abc")
	}
	var asked InventoryMsg
	if err := client.Do(rctx, TypeInventory, struct{}{}, &asked); err != nil {
		t.Fatalf("inventory request: %v", err)
	}
	if len(asked.Sandboxes) != 1 || asked.Node != "node-b" {
		t.Errorf("the node answered an inventory request with %+v", asked)
	}
}

// TestPendingNodeWaitsAndSaysHowToBeApproved: a node whose key is not approved
// yet is the normal first day of its life. It must retry quietly, forever, and
// the one log line it prints has to carry the command that unblocks it — that
// line is the only place anyone will look.
func TestPendingNodeWaitsAndSaysHowToBeApproved(t *testing.T) {
	gw := newGateway(t, false)
	ctx, _ := testContext(t)

	logs := &logBuffer{}
	done := runNode(t, ctx, ClientOptions{
		Gateway: gw.addr,
		Log:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})

	waitFor(t, "the node to retry after being refused", func() bool { return gw.helloCount() >= 2 })
	if !strings.Contains(logs.String(), "node approve node-b") {
		t.Errorf("the node never printed how to approve it:\n%s", logs.String())
	}
	// Refused is not fatal: waiting for an operator is a state, not a failure.
	if !strings.Contains(logs.String(), "level=INFO") || strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a pending node logged at the wrong level:\n%s", logs.String())
	}
	select {
	case err := <-done:
		t.Fatalf("RunClient returned %v while waiting for approval; it must retry forever", err)
	default:
	}

	// And approval needs nothing on the node: the next attempt links.
	gw.approve()
	recv(t, gw.clients, "link after approval")
}

// TestNodeReconnectsAfterTheGatewayRestarts is the property the whole
// supervisor exists for. A gateway restart is routine; it must cost this
// machine a reconnect and nothing else.
func TestNodeReconnectsAfterTheGatewayRestarts(t *testing.T) {
	gw := newGateway(t, true)
	ctx, _ := testContext(t)

	done := runNode(t, ctx, ClientOptions{Gateway: gw.addr})
	first := recv(t, gw.clients, "first link")
	waitFor(t, "the first link to come online", first.Online)

	gw.stop()
	waitFor(t, "the gateway to drop the link", func() bool { return !first.Online() })
	select {
	case err := <-done:
		t.Fatalf("RunClient returned %v when the gateway went away; the VMs on this machine do not care", err)
	case <-time.After(100 * time.Millisecond):
	}

	gw.start(t)
	second := recv(t, gw.clients, "link after the gateway came back")
	waitFor(t, "the second link to come online", second.Online)
	// The reconnect is a whole new link: the node re-introduces itself and
	// re-sends its picture rather than assuming the gateway remembers it.
	inv := recv(t, gw.inventories, "inventory after reconnect")
	if inv.Node != "node-b" {
		t.Errorf("inventory after reconnect = %+v, want node-b's", inv)
	}
}

// TestRunClientReturnsOnlyWhenCancelled pins the contract cmd/sparkbox depends
// on: serve() returns on the first value in its error channel, so a supervisor
// that reported a transport failure upward would cold-restart every VM on this
// machine every time the gateway bounced.
func TestRunClientReturnsOnlyWhenCancelled(t *testing.T) {
	// An address nothing is listening on: every attempt fails at the dial.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := testContext(t)
	done := runNode(t, ctx, ClientOptions{Gateway: addr})

	select {
	case err := <-done:
		t.Fatalf("RunClient returned %v against an unreachable gateway", err)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("RunClient returned %v on cancellation, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunClient did not return after its context was cancelled")
	}
}

// TestBackoffGrowsAndStaysJittered: the doubling is what keeps a node away from
// a gateway that is down, the cap is what brings it back within a minute of one
// that is up, and the jitter is what stops a rack of them arriving together.
func TestBackoffGrowsAndStaysJittered(t *testing.T) {
	c, err := newNodeClient(ClientOptions{
		Gateway: "gw:2222", NodeName: "node-b", Key: testSigner(t), Manager: testManager(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.backoffMin != DefaultBackoffMin || c.backoffMax != DefaultBackoffMax {
		t.Fatalf("backoff bounds = %v..%v, want the package defaults", c.backoffMin, c.backoffMax)
	}

	delay := c.backoffMin
	steps := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, want := range steps {
		if got := delay; got != want*time.Second {
			t.Fatalf("attempt %d waits %v, want %v", i, got, want*time.Second)
		}
		delay = min(delay*2, c.backoffMax)
	}

	for _, d := range []time.Duration{time.Second, 10 * time.Second, DefaultBackoffMax} {
		var lo, hi time.Duration
		for i := 0; i < 200; i++ {
			j := jitter(d)
			if j < time.Duration(float64(d)*0.8) || j > time.Duration(float64(d)*1.2) {
				t.Fatalf("jitter(%v) = %v, want within ±20%%", d, j)
			}
			if lo == 0 || j < lo {
				lo = j
			}
			if j > hi {
				hi = j
			}
		}
		if lo == hi {
			t.Errorf("jitter(%v) never varied; a fleet would reconnect in lockstep", d)
		}
	}
}

// --- the emitter ---

// blockedWriter is a link nobody is reading: writes never return. It stands in
// for the failure mode the emitter exists to survive, a gateway that has
// stopped draining its socket.
type blockedWriter struct{ release chan struct{} }

func (w blockedWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

// TestEmitterDropsRatherThanBlocks is the emitter's whole contract. These hooks
// fire inside the node manager's lock, so an emitter that waited on a wedged
// link would freeze every lifecycle operation on the machine.
func TestEmitterDropsRatherThanBlocks(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	conn := NewConn(strings.NewReader(""), blockedWriter{release: release}, "n", nil)
	e := NewEmitter(nil)
	detach := e.attach(conn, "node-b")
	defer detach()

	box := &host.Sandbox{Name: "demo", Owner: "alice", State: vmm.StateRunning}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more than the queue holds, against a link that has taken one
		// event and stalled inside the write.
		for i := 0; i < eventQueue*4; i++ {
			e.SandboxChanged(box, "touched")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the emitter blocked on a wedged link; it must drop instead")
	}

	select {
	case <-e.stale():
	case <-time.After(time.Second):
		t.Fatal("events were dropped and no resync was asked for")
	}
}

// TestCloseSandboxSessionsDoesNotBlock mirrors the gateway's own assertion
// (sshgw/livesessions_test.go): the manager calls this with its lock held, and
// a node holds no sessions to close, so the only correct answer is an immediate
// zero — the news goes out asynchronously.
func TestCloseSandboxSessionsDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	conn := NewConn(strings.NewReader(""), blockedWriter{release: release}, "n", nil)
	e := NewEmitter(nil)
	detach := e.attach(conn, "node-b")
	defer detach()

	done := make(chan int, 1)
	go func() { done <- e.CloseSandboxSessions("demo", "went idle for 30m") }()
	select {
	case n := <-done:
		if n != 0 {
			t.Fatalf("CloseSandboxSessions closed %d sessions; a node holds none", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseSandboxSessions blocked; the manager is holding its lock")
	}
}

// TestEmitterEventsReachTheGateway: the three things a node reports without
// being asked, each stamped with the node's name and carrying no address.
func TestEmitterEventsReachTheGateway(t *testing.T) {
	var (
		mu   sync.Mutex
		got  []string
		last = map[string]json.RawMessage{}
	)
	record := func(typ string) func(json.RawMessage) {
		return func(raw json.RawMessage) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, typ)
			last[typ] = append(json.RawMessage(nil), raw...)
		}
	}
	_, node := newPipePair(t, func(c *Conn) {
		c.OnEvent(TypeChanged, record(TypeChanged))
		c.OnEvent(TypeGone, record(TypeGone))
		c.OnEvent(TypePaused, record(TypePaused))
	}, nil)

	e := NewEmitter(nil)
	detach := e.attach(node, "node-b")
	defer detach()

	e.SandboxChanged(&host.Sandbox{
		Name: "demo", Owner: "alice", Image: "ubuntu", State: vmm.StatePaused,
		HostIP: "172.30.7.2", SSHAddr: "172.30.7.2:22", MemMB: 2048,
	}, "paused")
	e.CloseSandboxSessions("demo", "went idle for 30m")
	e.SandboxGone("demo")

	waitFor(t, "all three events", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 3
	})

	mu.Lock()
	defer mu.Unlock()
	var changed ChangedMsg
	if err := json.Unmarshal(last[TypeChanged], &changed); err != nil {
		t.Fatal(err)
	}
	if changed.Node != "node-b" || changed.Reason != "paused" || changed.Sandbox.Name != "demo" {
		t.Errorf("changed event = %+v", changed)
	}
	if changed.Sandbox.State != string(vmm.StatePaused) || changed.Sandbox.MemMB != 2048 {
		t.Errorf("changed event lost the record: %+v", changed.Sandbox)
	}
	// No address crosses the link, ever: every node mints the same guest
	// addresses, so one the gateway holds is one something up there can dial.
	if raw := string(last[TypeChanged]); strings.Contains(raw, "172.30.7.2") {
		t.Errorf("a guest address crossed the link: %s", raw)
	}

	var paused PausedMsg
	if err := json.Unmarshal(last[TypePaused], &paused); err != nil {
		t.Fatal(err)
	}
	if paused.Node != "node-b" || paused.Name != "demo" || paused.Reason != "went idle for 30m" {
		t.Errorf("paused event = %+v, want the node's own wording", paused)
	}

	var gone GoneMsg
	if err := json.Unmarshal(last[TypeGone], &gone); err != nil {
		t.Fatal(err)
	}
	if gone.Node != "node-b" || gone.Name != "demo" || gone.Reason != "destroyed" {
		t.Errorf("gone event = %+v", gone)
	}
}

// TestDetachedEmitterCostsNothing: a node between links — or one that has never
// reached a gateway — must not accumulate events to replay later. The next
// handshake sends the whole picture, which supersedes anything queued.
func TestDetachedEmitterCostsNothing(t *testing.T) {
	e := NewEmitter(nil)
	built := 0
	e.send(TypeChanged, func(string) any {
		built++
		return ChangedMsg{}
	})
	if built != 0 {
		t.Errorf("an unlinked emitter built %d event bodies, want none", built)
	}
	select {
	case <-e.stale():
		t.Error("an unlinked emitter asked for a resync; the handshake already sends one")
	default:
	}

	gw, node := newPipePair(t, nil, nil)
	arrived := make(chan struct{}, 1)
	gw.OnEvent(TypeGone, func(json.RawMessage) { arrived <- struct{}{} })
	detach := e.attach(node, "node-b")
	e.SandboxGone("demo")
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("an attached emitter delivered nothing")
	}

	// And after detaching it is quiet again.
	detach()
	e.SandboxGone("demo")
	select {
	case <-arrived:
		t.Error("a detached emitter kept writing to a link that is gone")
	case <-time.After(100 * time.Millisecond):
	}
}

// --- host key pinning ---

// TestHostKeyTrustedOnFirstUseIsRemembered: a node has no operator at the
// keyboard to answer the usual prompt, so the first key offered is taken — and
// written down, because a pin that dies with the process is a first use on
// every restart.
func TestHostKeyTrustedOnFirstUseIsRemembered(t *testing.T) {
	path := t.TempDir() + "/gateway_host_key.pub"
	logs := &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	pin, err := newHostKeyPin(nil, path, log)
	if err != nil {
		t.Fatal(err)
	}
	gwKey := testSigner(t).PublicKey()
	if err := pin.check("gw:2222", nil, gwKey); err != nil {
		t.Fatalf("first use refused: %v", err)
	}
	if !strings.Contains(logs.String(), xssh.FingerprintSHA256(gwKey)) {
		t.Errorf("the fingerprint was never logged:\n%s", logs.String())
	}

	// A second process reads the pin back and is no longer trusting anybody.
	reopened, err := newHostKeyPin(nil, path, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.check("gw:2222", nil, gwKey); err != nil {
		t.Errorf("the remembered key was refused: %v", err)
	}

	err = reopened.check("gw:2222", nil, testSigner(t).PublicKey())
	if err == nil {
		t.Fatal("a different host key was accepted; the pin bought nothing")
	}
	// Both fingerprints, because the operator's next act is to compare them.
	if !strings.Contains(err.Error(), xssh.FingerprintSHA256(gwKey)) || !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal does not say what to compare or what to delete: %v", err)
	}
}

// TestSeededHostKeyPinRefusesAnImposter covers --gateway-host-key: a pin
// supplied out of band is never overwritten by whatever answers on the address.
func TestSeededHostKeyPinRefusesAnImposter(t *testing.T) {
	want := testSigner(t).PublicKey()
	pin, err := newHostKeyPin(want, "", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := pin.check("gw:2222", nil, want); err != nil {
		t.Errorf("the seeded key was refused: %v", err)
	}
	if err := pin.check("gw:2222", nil, testSigner(t).PublicKey()); err == nil {
		t.Fatal("a seeded pin accepted a different key")
	}
}

// TestNodeRefusesToStartWithoutItsIdentity: every one of these is a wiring
// mistake in cmd/sparkbox, and a supervisor that retried one forever would look
// exactly like a gateway that is down.
func TestNodeRefusesToStartWithoutItsIdentity(t *testing.T) {
	full := ClientOptions{Gateway: "gw:2222", NodeName: "node-b", Key: testSigner(t), Manager: testManager()}
	for _, tc := range []struct {
		name string
		mut  func(*ClientOptions)
		want string
	}{
		{"no gateway", func(o *ClientOptions) { o.Gateway = "" }, "gateway address"},
		{"no name", func(o *ClientOptions) { o.NodeName = "" }, "a name"},
		{"no key", func(o *ClientOptions) { o.Key = nil }, "own key"},
		{"no manager", func(o *ClientOptions) { o.Manager = nil }, "host manager"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := full
			tc.mut(&opts)
			err := RunClient(context.Background(), opts)
			if err == nil {
				t.Fatal("RunClient started with a hole in its configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestWelcomeRefusalDropsTheLink: the only thing a node does with a welcome is
// install the key its guests will trust, so a machine that could not do that
// must not go on to boot VMs nobody can log into — it retries instead.
func TestWelcomeRefusalDropsTheLink(t *testing.T) {
	gw := newGateway(t, true)
	ctx, _ := testContext(t)

	var attempts int
	var mu sync.Mutex
	done := runNode(t, ctx, ClientOptions{
		Gateway: gw.addr,
		OnWelcome: func(Welcome) error {
			mu.Lock()
			defer mu.Unlock()
			attempts++
			return errors.New("the disk is full")
		},
	})

	waitFor(t, "the node to retry after a welcome it could not accept", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts >= 2
	})
	select {
	case err := <-done:
		t.Fatalf("RunClient returned %v; a node that cannot store the key retries", err)
	default:
	}
}

// TestUnknownRequestIsAnswered: a gateway one release ahead will ask for things
// this node has never heard of, and the answer has to be a sentence rather than
// a hung request — the gateway drops a link after two unanswered pings.
func TestUnknownRequestIsAnswered(t *testing.T) {
	gw := newGateway(t, true)
	ctx, _ := testContext(t)
	runNode(t, ctx, ClientOptions{Gateway: gw.addr})

	client := recv(t, gw.clients, "link")
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err := client.Do(rctx, "sandbox.teleport", NameReq{Name: "demo"}, nil)
	if err == nil {
		t.Fatal("the node claimed to speak a verb that does not exist")
	}
	var e *ctlops.Error
	if !errors.As(err, &e) || e.Kind != ctlops.KindInvalid {
		t.Fatalf("err = %v (%T), want an invalid-request *ctlops.Error", err, err)
	}
	// And the link is still usable afterwards.
	var echo PingReq
	if err := client.Do(rctx, TypePing, PingReq{Nonce: "still here"}, &echo); err != nil {
		t.Fatalf("the link died over an unknown verb: %v", err)
	}
}

// The fake stands in for a real *host.Manager, which client.go asserts
// satisfies the same interface.
var _ Manager = (*opsManager)(nil)
