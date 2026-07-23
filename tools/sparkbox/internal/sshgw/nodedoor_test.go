package sshgw

// Tests for the `node@` door, driven by a real SSH client against a real
// gateway listener: enrolment must grant nothing, an unknown key must be
// refused at every other door exactly as before, and an approved node must
// become an observable member of the fleet whose sandboxes survive its link.

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// frameWait bounds every read a test makes off a link. A frame that never
// arrives is a failure with a message, not a test binary that hangs until the
// package timeout.
const frameWait = 5 * time.Second

// testAdmissionBudget is the production nodeAdmissionBudget, shrunk: these
// tests care that an unapproved machine is eventually hung up on, not about the
// particular number of seconds. It stays comfortably longer than a loopback
// hello round trip, so an unapproved node still reliably gets its refusal.
const testAdmissionBudget = 2 * time.Second

type nodeStack struct {
	gw          *Gateway
	addr        string
	roster      *nodes.Store
	flt         *fleet.Fleet
	index       *placement.Store
	upstreamPub string
}

// newNodeStack stands up a gateway with the fleet door open: a real roster, a
// real Fleet as the joiner, and a listener a client can dial. enrol mirrors the
// --no-node-enrol flag.
func newNodeStack(t *testing.T, enrol bool) *nodeStack {
	t.Helper()
	return newNodeStackLogging(t, enrol, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newNodeStackLogging is the same stack with the gateway's log handler chosen by
// the caller, so a test can count what an unapproved peer makes this door write.
func newNodeStackLogging(t *testing.T, enrol bool, handler slog.Handler) *nodeStack {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(handler)

	hostKey, err := LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatal(err)
	}
	upstreamKey, err := LoadOrCreateKey(dir, "gateway_upstream_key")
	if err != nil {
		t.Fatal(err)
	}
	driver := mock.New(dir, hostKey)
	t.Cleanup(func() { driver.Close() })
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver, NodeName: "gw",
		GatewayPublicKey: PublicKeyLine(upstreamKey), Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}

	db := filepath.Join(dir, "sparkbox.db")
	userStore, err := users.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { userStore.Close() })
	roster, err := nodes.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roster.Close() })
	index, err := placement.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })

	flt, err := fleet.New(fleet.Options{Local: mgr, LocalName: "gw", LocalArch: "arm64", Index: index, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { flt.Close() })

	gw := New(GatewayOptions{
		Manager: mgr, Users: userStore, HostKey: hostKey, UpstreamKey: upstreamKey,
		DefaultImage: "ubuntu", Logger: log, Domain: "hivemind.tools",
		Nodes: roster, NodeJoiner: flt, NodeEnrol: enrol,
	})
	// The 30-second production budget is a backstop, not a protocol deadline, so
	// waiting it out here would only make the suite slow. Written before the
	// listener exists, which is what keeps the server goroutine from racing it.
	gw.admissionBudget = testAdmissionBudget
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := gw.Server("")
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })

	return &nodeStack{
		gw: gw, addr: ln.Addr().String(), roster: roster, flt: flt, index: index,
		upstreamPub: PublicKeyLine(upstreamKey),
	}
}

// newGatewayWithoutFleet is the same gateway with the node door shut, which is
// every single-box deployment. It exists so a test can compare failure shapes
// against a build that has never heard of a fleet.
func newGatewayWithoutFleet(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hostKey, err := LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatal(err)
	}
	upstreamKey, err := LoadOrCreateKey(dir, "gateway_upstream_key")
	if err != nil {
		t.Fatal(err)
	}
	driver := mock.New(dir, hostKey)
	t.Cleanup(func() { driver.Close() })
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver,
		GatewayPublicKey: PublicKeyLine(upstreamKey), Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	userStore, err := users.Open(filepath.Join(dir, "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { userStore.Close() })
	gw := New(GatewayOptions{
		Manager: mgr, Users: userStore, HostKey: hostKey, UpstreamKey: upstreamKey,
		DefaultImage: "ubuntu", Logger: log, Domain: "hivemind.tools",
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := gw.Server("")
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func newNodeKey(t *testing.T) xssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func dialAs(addr, user string, key xssh.Signer) (*xssh.Client, error) {
	return xssh.Dial("tcp", addr, &xssh.ClientConfig{
		User:            user,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(key)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), //nolint:gosec // test
		Timeout:         5 * time.Second,
	})
}

// link is one node's side of the control channel: the session running
// sparkbox-link/1, with frames going in and coming out.
type link struct {
	client *xssh.Client
	sess   *xssh.Session
	in     io.WriteCloser
	dec    *json.Decoder
}

func (s *nodeStack) open(t *testing.T, key xssh.Signer) *link {
	t.Helper()
	client, err := dialAs(s.addr, NodeUser, key)
	if err != nil {
		t.Fatalf("dial node door: %v", err)
	}
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	in, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(nodelink.LinkCommand); err != nil {
		t.Fatal(err)
	}
	l := &link{client: client, sess: sess, in: in, dec: json.NewDecoder(bufio.NewReader(out))}
	t.Cleanup(func() { l.close() })
	return l
}

func (l *link) close() {
	l.sess.Close()   //nolint:errcheck
	l.client.Close() //nolint:errcheck
}

func (l *link) send(t *testing.T, f nodelink.Frame) {
	t.Helper()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.in.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write %s frame: %v", f.Type, err)
	}
}

// next reads one frame, failing rather than blocking forever. The read runs on
// its own goroutine because an SSH session has no read deadline to set.
func (l *link) next(t *testing.T) nodelink.Frame {
	t.Helper()
	type result struct {
		f   nodelink.Frame
		err error
	}
	done := make(chan result, 1)
	go func() {
		var f nodelink.Frame
		err := l.dec.Decode(&f)
		done <- result{f, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("read frame: %v", res.err)
		}
		return res.f
	case <-time.After(frameWait):
		t.Fatal("no frame from the gateway within the wait")
		return nodelink.Frame{}
	}
}

func helloFrame(t *testing.T, name string) nodelink.Frame {
	t.Helper()
	body, err := json.Marshal(nodelink.Hello{
		Protocol: nodelink.Protocol, Node: name, Arch: "arm64", OS: "linux",
		Release: "2026-07-22", Version: "test", Driver: "mock",
		GuestSubnet: "172.30.0.0/16", StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return nodelink.Frame{ID: "n0000000000000001", Type: nodelink.TypeHello, Body: body}
}

// hello introduces a node and returns the gateway's answer to it.
func (l *link) hello(t *testing.T, name string) nodelink.Frame {
	t.Helper()
	l.send(t, helloFrame(t, name))
	return l.next(t)
}

// TestNodeEnrolsOnceAndIsRefused is the enrolment contract: first contact
// records a pending row and buys nothing at all, and reconnecting neither
// creates a second row nor upgrades the first.
func TestNodeEnrolsOnceAndIsRefused(t *testing.T) {
	s := newNodeStack(t, true)
	key := newNodeKey(t)

	reply := s.open(t, key).hello(t, "node-b")
	if reply.Err == nil {
		t.Fatalf("an unapproved node was welcomed: %s", reply.Body)
	}
	if reply.Err.Code != nodelink.CodeNodePending {
		t.Errorf("refusal code = %q, want %q", reply.Err.Code, nodelink.CodeNodePending)
	}
	// The sentence carries the command that unblocks the node, because the
	// node's log line is the only place anyone will look for it.
	if !strings.Contains(reply.Err.Msg, "node approve node-b") {
		t.Errorf("refusal does not say how to approve the node: %q", reply.Err.Msg)
	}

	row, err := s.roster.Get("node-b")
	if err != nil {
		t.Fatalf("first contact recorded no roster row: %v", err)
	}
	if row.Status != nodes.StatusPending {
		t.Errorf("enrolled node status = %q, want %q", row.Status, nodes.StatusPending)
	}

	// A pending node reconnecting is the common case (it retries forever), and
	// it must not burn a second row out of the enrolment budget.
	again := s.open(t, key).hello(t, "node-b")
	if again.Err == nil || again.Err.Code != nodelink.CodeNodePending {
		t.Fatalf("reconnect answer = %+v, want another %q", again.Err, nodelink.CodeNodePending)
	}
	list, err := s.roster.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("roster holds %d rows after two connections, want 1", len(list))
	}

	// Enrolling is not capacity: nothing joined the fleet.
	if got := len(s.flt.Capacities()); got != 1 {
		t.Errorf("fleet reports %d machines, want only this one", got)
	}
}

// TestNodeEnrolmentCanBeRefusedOutright covers --no-node-enrol: an unknown key
// is then refused at the node door by the public-key check, which is the same
// answer every other door gives it.
func TestNodeEnrolmentCanBeRefusedOutright(t *testing.T) {
	s := newNodeStack(t, false)
	if _, err := dialAs(s.addr, NodeUser, newNodeKey(t)); err == nil {
		t.Fatal("an unknown key was admitted at the node door with enrolment off")
	}
	list, err := s.roster.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("roster grew to %d rows with enrolment off", len(list))
	}
}

// TestUnknownKeyRefusedIdenticallyElsewhere is the door's blast radius: the
// node branch of the public-key check must open exactly one door and must not
// change what any other one says. Indistinguishable is the requirement — a
// caller must not be able to tell a fleet gateway from a single-box one by the
// shape of a rejection.
func TestUnknownKeyRefusedIdenticallyElsewhere(t *testing.T) {
	s := newNodeStack(t, true)
	plain := newGatewayWithoutFleet(t)
	key := newNodeKey(t)

	client, baseline := dialAs(plain, "somebox", key)
	if baseline == nil {
		client.Close()
		t.Fatal("an unknown key was admitted by a gateway with no fleet")
	}
	for _, user := range []string{"somebox", ControlUser, NewSandboxUser, "ctl-ish"} {
		client, err := dialAs(s.addr, user, key)
		if err == nil {
			client.Close()
			t.Fatalf("an unknown key was admitted as %q by the fleet gateway", user)
		}
		if err.Error() != baseline.Error() {
			t.Errorf("rejection at %q reads %q, want the fleet-free gateway's %q", user, err, baseline)
		}
	}
	// And the node door itself did not enrol anybody: it was never the door
	// those connections were aimed at.
	list, err := s.roster.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("connections to other doors enrolled %d nodes", len(list))
	}
}

// approved enrols a node, approves it, and links it, returning the live link.
func (s *nodeStack) approved(t *testing.T, key xssh.Signer, name string) *link {
	t.Helper()
	s.open(t, key).hello(t, name)
	if err := s.roster.Approve(name, "operator"); err != nil {
		t.Fatal(err)
	}
	l := s.open(t, key)
	reply := l.hello(t, name)
	if reply.Err != nil {
		t.Fatalf("approved node refused: %+v", reply.Err)
	}
	return l
}

// TestApprovedNodeJoinsTheFleet is the happy path end to end: hello, welcome,
// inventory, heartbeat, and a machine the fleet can see.
func TestApprovedNodeJoinsTheFleet(t *testing.T) {
	s := newNodeStack(t, true)
	key := newNodeKey(t)
	s.open(t, key).hello(t, "node-b")
	if err := s.roster.Approve("node-b", "operator"); err != nil {
		t.Fatal(err)
	}

	l := s.open(t, key)
	reply := l.hello(t, "node-b")
	if reply.Err != nil {
		t.Fatalf("approved node was refused: %+v", reply.Err)
	}
	var w nodelink.Welcome
	if err := json.Unmarshal(reply.Body, &w); err != nil {
		t.Fatal(err)
	}
	if w.Protocol != nodelink.Protocol || w.Node != "node-b" {
		t.Errorf("welcome = %+v, want protocol %d for node-b", w, nodelink.Protocol)
	}
	if w.GatewayUpstreamPub != s.upstreamPub {
		t.Errorf("welcome carries %q, want the gateway's upstream public key %q", w.GatewayUpstreamPub, s.upstreamPub)
	}
	if w.HeartbeatSeconds != int(nodelink.DefaultHeartbeat/time.Second) {
		t.Errorf("welcome asks for a %ds heartbeat, want %v", w.HeartbeatSeconds, nodelink.DefaultHeartbeat)
	}

	// The inventory is a request, so the gateway has to answer it; a node that
	// got no reply would keep resending its whole picture.
	inv, err := json.Marshal(nodelink.InventoryMsg{
		Node: "node-b", Capacity: host.NodeCapacity{Node: "node-b", TotalMemMB: 8192}, At: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	l.send(t, nodelink.Frame{ID: "n0000000000000002", Type: nodelink.TypeInventory, Body: inv})
	ack := l.next(t)
	if ack.Type != nodelink.TypeReply || ack.ID != "n0000000000000002" || ack.Err != nil {
		t.Fatalf("inventory was answered with %+v", ack)
	}

	beat, err := json.Marshal(nodelink.Heartbeat{
		Capacity: host.NodeCapacity{Node: "lies", TotalMemMB: 4096, BudgetMemMB: 3072}, At: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	l.send(t, nodelink.Frame{Type: nodelink.TypeHeartbeat, Body: beat})

	waitFor(t, "node-b to report capacity", func() bool {
		for _, c := range s.flt.Capacities() {
			if c.Node == "node-b" && c.TotalMemMB == 4096 {
				return true
			}
		}
		return false
	})
	if !s.flt.Online("node-b") {
		t.Error("a node that just introduced itself is not online")
	}

	// The roster row records what the machine said about itself.
	row, err := s.roster.Get("node-b")
	if err != nil {
		t.Fatal(err)
	}
	if row.Arch != "arm64" || row.LastSeen == nil {
		t.Errorf("roster row = %+v, want arch arm64 and a last-seen stamp", row)
	}

	// A machine naming itself something else in its own capacity report is
	// still filed under the name its key resolved to.
	nodesSeen := s.flt.Nodes()
	if len(nodesSeen) != 2 {
		t.Fatalf("fleet lists %d machines, want this one and node-b", len(nodesSeen))
	}
	remote := nodesSeen[1]
	if remote.Name != "node-b" || remote.Local || !remote.Online {
		t.Errorf("node-b listed as %+v", remote)
	}
	if remote.Capacity.Node != "node-b" {
		t.Errorf("node-b's capacity is filed under %q; the authenticated name must win", remote.Capacity.Node)
	}
}

// TestSecondLinkSupersedesTheFirst: incumbent-wins would let one half-open TCP
// socket lock a machine out of its own fleet until a timeout it cannot
// influence expires, so the newest link is the real one.
func TestSecondLinkSupersedesTheFirst(t *testing.T) {
	s := newNodeStack(t, true)
	key := newNodeKey(t)
	first := s.approved(t, key, "node-b")

	second := s.open(t, key)
	if reply := second.hello(t, "node-b"); reply.Err != nil {
		t.Fatalf("the second link was refused: %+v", reply.Err)
	}

	bye := first.next(t)
	if bye.Type != nodelink.TypeBye {
		t.Fatalf("the superseded link got %q, want a bye", bye.Type)
	}
	var msg nodelink.Bye
	if err := json.Unmarshal(bye.Body, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Code != nodelink.CodeSuperseded {
		t.Errorf("superseded link told %q, want %q", msg.Code, nodelink.CodeSuperseded)
	}

	// One machine, one link: the old one tearing itself down must not take the
	// replacement's registration with it.
	waitFor(t, "the fleet to settle on one link for node-b", func() bool {
		return len(s.flt.Capacities()) == 2 && s.flt.Online("node-b")
	})
}

// TestKilledLinkGoesOfflineAndKeepsPlacements: offline is a statement about a
// machine, never about the sandboxes on it. The rootfs is still over there.
func TestKilledLinkGoesOfflineAndKeepsPlacements(t *testing.T) {
	s := newNodeStack(t, true)
	key := newNodeKey(t)
	l := s.approved(t, key, "node-b")
	if err := s.index.Reserve("demo", "alice", "node-b", "ubuntu", "arm64"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "node-b to come online", func() bool { return s.flt.Online("node-b") })

	l.close()

	waitFor(t, "node-b to go offline", func() bool { return !s.flt.Online("node-b") })
	row, ok, err := s.index.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || row.Node != "node-b" || row.Owner != "alice" {
		t.Fatalf("placement after the link died = %+v (found %v); it must survive untouched", row, ok)
	}
	// The gateway is still serving: a machine going away is not an event that
	// takes the control plane with it.
	if _, err := s.roster.Get("node-b"); err != nil {
		t.Fatalf("roster unusable after a link died: %v", err)
	}
}

// TestNodeDoorTellsHumansWhatItIs: the door checks for a literal command so a
// person who types `ssh node@gateway` is answered with a sentence instead of
// silence.
func TestNodeDoorTellsHumansWhatItIs(t *testing.T) {
	s := newNodeStack(t, true)
	client, err := dialAs(s.addr, NodeUser, newNodeKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var stderr strings.Builder
	sess.Stderr = &stderr
	err = sess.Run("")

	var exit *xssh.ExitError
	if err == nil {
		t.Fatal("a shell at the node door exited 0")
	} else if !errors.As(err, &exit) || exit.ExitStatus() != 2 {
		t.Fatalf("shell at the node door exited with %v, want status 2", err)
	}
	if !strings.Contains(stderr.String(), nodelink.LinkCommand) {
		t.Errorf("the door did not say what it is: %q", stderr.String())
	}
}

// TestServerHasNoSessionTimeouts pins the only reason a link can live for
// weeks: gliderlabs enforces IdleTimeout and MaxTimeout per session, and either
// one set here would silently cut every node loose on a schedule.
func TestServerHasNoSessionTimeouts(t *testing.T) {
	gw, _, _ := newDoorGateway(t)
	srv := gw.Server("")
	if srv.IdleTimeout != 0 {
		t.Errorf("Server sets IdleTimeout %v; a node link is idle between heartbeats", srv.IdleTimeout)
	}
	if srv.MaxTimeout != 0 {
		t.Errorf("Server sets MaxTimeout %v; a node link is meant to outlive it", srv.MaxTimeout)
	}
}

// fillEnrolmentQueue enrols nodes.MaxPending throwaway keys, which is every row
// the roster will hold, and returns their names oldest first.
func (s *nodeStack) fillEnrolmentQueue(t *testing.T) []string {
	t.Helper()
	var names []string
	for i := 0; i < nodes.MaxPending; i++ {
		name := fmt.Sprintf("squat-%02d", i)
		reply := s.open(t, newNodeKey(t)).hello(t, name)
		if reply.Err == nil || reply.Err.Code != nodelink.CodeNodePending {
			t.Fatalf("filling the roster: squatter %d got %+v, want a pending row", i, reply.Err)
		}
		names = append(names, name)
	}
	return names
}

// goQuiet backdates each row's contact stamp past nodeEnrolStale, staggered, so
// the roster holds what it would hold after a fire-and-forget flood walked away
// and nothing behind those keys ever knocked again.
func (s *nodeStack) goQuiet(t *testing.T, names []string) {
	t.Helper()
	quiet := time.Now().Add(-2 * nodeEnrolStale)
	for i, name := range names {
		if err := s.roster.Seen(name, "", "", quiet.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("backdating %s: %v", name, err)
		}
	}
}

// TestEnrolmentBudgetCannotBeSquatted: nodes.MaxPending is a ceiling on rows,
// not on people, and anyone who can reach the listener can offer a key. Filling
// it with throwaway keys and walking away must not be a way to lock every
// genuine machine out of enrolment — rows nobody is knocking on age out.
func TestEnrolmentBudgetCannotBeSquatted(t *testing.T) {
	s := newNodeStack(t, true)
	s.goQuiet(t, s.fillEnrolmentQueue(t))

	// A machine the operator is actually waiting for now knocks.
	reply := s.open(t, newNodeKey(t)).hello(t, "node-b")
	if reply.Err == nil || reply.Err.Code != nodelink.CodeNodePending {
		t.Fatalf("a genuine node was answered %+v, want %q — the squatters locked the door",
			reply.Err, nodelink.CodeNodePending)
	}
	row, err := s.roster.Get("node-b")
	if err != nil {
		t.Fatalf("the genuine node recorded no roster row, so an operator has nothing to approve: %v", err)
	}
	if row.Status != nodes.StatusPending {
		t.Errorf("node-b enrolled as %q, want %q", row.Status, nodes.StatusPending)
	}

	// Exactly one squatter was displaced: the budget is recycled, not raised.
	list, err := s.roster.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != nodes.MaxPending {
		t.Errorf("roster holds %d rows, want the %d the ceiling allows", len(list), nodes.MaxPending)
	}
	if _, err := s.roster.Get("squat-00"); err == nil {
		t.Error("the row that had been quietest survived; something else was dropped instead")
	}
}

// TestFullQueueOfKnockingMachinesRefusesRatherThanDisplaces is the other side of
// that bargain, and the reason the name-substitution attack has nowhere to
// start: while every waiting machine is still knocking there is nothing here to
// reclaim, so the door refuses a new machine instead of evicting one that is
// there. That refusal is a delay somebody has to keep paying for; an eviction
// would be a name handed to whoever asked last.
func TestFullQueueOfKnockingMachinesRefusesRatherThanDisplaces(t *testing.T) {
	s := newNodeStack(t, true)
	waiting := s.fillEnrolmentQueue(t)

	reply := s.open(t, newNodeKey(t)).hello(t, "node-b")
	if reply.Err == nil || reply.Err.Code != nodelink.CodeNodeEnrolFull {
		t.Fatalf("the newcomer was answered %+v, want %q", reply.Err, nodelink.CodeNodeEnrolFull)
	}
	if !strings.Contains(reply.Err.Msg, "still knocking") {
		t.Errorf("the refusal does not say why the queue is full: %q", reply.Err.Msg)
	}
	if _, err := s.roster.Get("node-b"); err == nil {
		t.Error("the newcomer took a row anyway, so the ceiling is not a ceiling")
	}
	for _, name := range waiting {
		if _, err := s.roster.Get(name); err != nil {
			t.Fatalf("%s was displaced while it was still knocking: %v", name, err)
		}
	}
}

// TestApprovedNodeIsNeverRecycled: reclaiming a quiet enrolment row is safe only
// because it touches nothing an operator has decided about. An approved machine
// that has been quiet longer than every pending one must still never be the row
// that is dropped.
func TestApprovedNodeIsNeverRecycled(t *testing.T) {
	s := newNodeStack(t, true)
	key := newNodeKey(t)
	s.open(t, key).hello(t, "node-b")
	if err := s.roster.Approve("node-b", "operator"); err != nil {
		t.Fatal(err)
	}
	waiting := s.fillEnrolmentQueue(t)
	// node-b has been silent longer than any of them, which is what makes it the
	// row a rule about silence alone would pick.
	if err := s.roster.Seen("node-b", "", "", time.Now().Add(-100*nodeEnrolStale)); err != nil {
		t.Fatal(err)
	}
	s.goQuiet(t, waiting)

	s.open(t, newNodeKey(t)).hello(t, "newcomer")

	row, err := s.roster.Get("node-b")
	if err != nil {
		t.Fatalf("the approved node was dropped from the roster: %v", err)
	}
	if row.Status != nodes.StatusApproved {
		t.Errorf("node-b is %q after the roster churned, want %q", row.Status, nodes.StatusApproved)
	}
	if _, err := s.roster.Get("squat-00"); err == nil {
		t.Error("the quietest pending row survived, so nothing was recycled at all")
	}
}

// TestUnapprovedNodeCannotHoldTheDoorOpen: the server sets no session timeouts
// on purpose, so without a per-connection bound a stranger who authenticates at
// the node door and then says nothing holds a gateway goroutine, a reader
// blocked on a frame that never comes, and a socket, for as long as it likes.
func TestUnapprovedNodeCannotHoldTheDoorOpen(t *testing.T) {
	s := newNodeStack(t, true)
	client, err := dialAs(s.addr, NodeUser, newNodeKey(t))
	if err != nil {
		t.Fatalf("dial node door: %v", err)
	}
	defer client.Close()

	// Nothing further: no session, no hello, no command. Wait returns when the
	// connection goes away, which is the thing being asserted.
	closed := make(chan struct{})
	go func() {
		client.Wait() //nolint:errcheck
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * testAdmissionBudget):
		t.Fatal("an unapproved machine held its connection past the admission budget")
	}
}

// TestApprovedLinkIsNotOnTheClock is the other half: the bound above must never
// reach a machine the operator blessed, whose link is meant to sit idle between
// heartbeats for weeks.
func TestApprovedLinkIsNotOnTheClock(t *testing.T) {
	s := newNodeStack(t, true)
	l := s.approved(t, newNodeKey(t), "node-b")

	closed := make(chan struct{})
	go func() {
		l.client.Wait() //nolint:errcheck
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("an approved node's link was hung up on; it is the one connection with no deadline")
	case <-time.After(3 * testAdmissionBudget):
	}
	if !s.flt.Online("node-b") {
		t.Error("the approved node is no longer online")
	}
}

// TestFailStartSurvivesALimitErrorWithNoNames: in a fleet a start refusal can
// arrive off the wire, rebuilt from JSON the *node* authored, so the running
// set is not something this gateway produced. A node that replies with a limit
// error and no running list used to panic the session goroutine on the index of
// the first name.
func TestFailStartSurvivesALimitErrorWithNoNames(t *testing.T) {
	gw, _, _ := newDoorGateway(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := &ctlContext{Context: context.Background(), vals: map[any]any{}}
	s := &ctlSession{ctx: ctx}

	gw.failStart(s, log, "resume", &host.LimitError{Max: 3})

	if !s.exited || s.code != 1 {
		t.Errorf("session exited %d (exited=%v), want status 1", s.code, s.exited)
	}
	out := s.stderr.String()
	if !strings.Contains(out, "(max 3)") {
		t.Errorf("the refusal does not state the limit: %q", out)
	}
	if !strings.Contains(out, "ssh "+ControlUser+"@hivemind.tools") {
		t.Errorf("the refusal does not say how to free a slot: %q", out)
	}

	// And the sentence a single-box deployment has always printed is unchanged,
	// byte for byte: the guard above is for the case that could not happen
	// before there were nodes, and must not be visible in the one that could.
	local := &ctlSession{ctx: &ctlContext{Context: context.Background(), vals: map[any]any{}}}
	gw.failStart(local, log, "resume", &host.LimitError{Max: 2, Running: []string{"a", "b"}})
	const want = "sparkbox: you already have 2 running sandboxes (max 2): a, b\r\n" +
		"Pause one to free a slot, e.g.:  ssh ctl@hivemind.tools pause a\r\n"
	if got := local.stderr.String(); got != want {
		t.Errorf("the ordinary limit refusal reads\n%q\nwant\n%q", got, want)
	}
}

// TestNodeDoorIsShutWithoutAFleet: with no roster and no joiner, `node` is just
// a sandbox name nobody has, which is what every single-box deployment sees.
func TestNodeDoorIsShutWithoutAFleet(t *testing.T) {
	gw, _, doors := newDoorGateway(t)
	if gw.nodeDoorOpen() {
		t.Fatal("the node door is open on a gateway with no roster")
	}
	// The username is still recognised — a front-door address never is, since a
	// node dials the SSH listener by name and `node` mints no door of its own.
	if !gw.isNodeDoor(NodeUser, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222}) {
		t.Error("isNodeDoor no longer recognises the node username")
	}
	if gw.isNodeDoor(NodeUser, tcp(doors.Addr(NewSandboxUser))) {
		t.Error("a front-door address was taken for the node door")
	}
}

// TestRecyclingNeverHandsANameToAStranger is the escalation the enrolment
// recycler has to not be: a name an operator is about to approve must not be
// takeable by whoever asks for it last.
//
// The whole trust ceremony rests on `node approve <name>` approving the machine
// the operator brought up. If a stranger can flood the enrolment queue until the
// honest machine's row is recycled and then enrol that same name with their own
// key, the operator's yes lands on the stranger — a privilege escalation anyone
// who can open a TCP connection to this listener can drive.
func TestRecyclingNeverHandsANameToAStranger(t *testing.T) {
	s := newNodeStack(t, true)

	honest := newNodeKey(t)
	if reply := s.open(t, honest).hello(t, "gpu-01"); reply.Err == nil || reply.Err.Code != nodelink.CodeNodePending {
		t.Fatalf("the honest machine was answered %+v, want a pending row", reply.Err)
	}
	waiting, err := s.roster.Get("gpu-01")
	if err != nil {
		t.Fatalf("the honest machine recorded no row: %v", err)
	}

	// A stranger fills the enrolment budget with throwaway keys. Each one past
	// the ceiling ages another row out, and the oldest row is the honest
	// machine's.
	for i := 0; i <= nodes.MaxPending; i++ {
		s.open(t, newNodeKey(t)).hello(t, fmt.Sprintf("squat-%02d", i))
	}

	// And now asks for the name the operator is waiting to approve.
	thief := newNodeKey(t)
	reply := s.open(t, thief).hello(t, "gpu-01")

	row, err := s.roster.Get("gpu-01")
	if err != nil {
		t.Fatalf("gpu-01 was dropped from the roster, so the name is free for anyone: %v", err)
	}
	if row.FP != waiting.FP {
		t.Errorf("`node approve gpu-01` would now approve %s — the machine that asked last — instead of %s",
			row.FP, waiting.FP)
	}
	if reply.Err == nil || reply.Err.Code != nodelink.CodeNodeNameTaken {
		t.Errorf("the stranger was answered %+v, want %q", reply.Err, nodelink.CodeNodeNameTaken)
	}
}

// TestKnockingKeepsAPendingRowFresh: a machine waiting for approval retries
// forever, and that retry is the only evidence the gateway has that it is still
// there. Recording it is what lets the door tell a machine that is waiting from
// one that was abandoned — and it is what an operator reads in `node ls` to see
// that the machine they are about to approve is actually knocking.
func TestKnockingKeepsAPendingRowFresh(t *testing.T) {
	s := newNodeStack(t, true)
	key := newNodeKey(t)
	s.open(t, key).hello(t, "gpu-01")

	// Backdate the row to what the roster would hold after the machine had been
	// quiet for a long time, then let it knock again.
	if err := s.roster.Seen("gpu-01", "", "", time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if reply := s.open(t, key).hello(t, "gpu-01"); reply.Err == nil || reply.Err.Code != nodelink.CodeNodePending {
		t.Fatalf("the waiting machine was answered %+v on its retry", reply.Err)
	}

	row, err := s.roster.Get("gpu-01")
	if err != nil {
		t.Fatal(err)
	}
	if row.LastSeen == nil || time.Since(*row.LastSeen) > time.Minute {
		t.Fatalf("a waiting machine's retry left its row looking abandoned (last seen %v)", row.LastSeen)
	}
	if row.Arch != "arm64" {
		t.Errorf("the row an operator is about to approve says arch %q, want what the machine reported", row.Arch)
	}
}

// countingHandler counts log lines by message. WithAttrs and WithGroup return
// the same handler because the door decorates its logger per session and those
// lines are exactly the ones being counted.
type countingHandler struct {
	mu sync.Mutex
	n  map[string]int
}

func newCountingHandler() *countingHandler { return &countingHandler{n: map[string]int{}} }

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n[r.Message]++
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n[msg]
}

// TestNodeDoorLogsAreRateLimited: every line this door writes about a machine it
// has not approved is written on that machine's command. Without a bound, a peer
// that opens sessions in a loop turns the gateway's own log into the thing that
// fills its disk, which costs the attacker nothing and the operator everything.
func TestNodeDoorLogsAreRateLimited(t *testing.T) {
	handler := newCountingHandler()
	s := newNodeStackLogging(t, true, handler)
	// The bucket is process-wide, so start from a known state rather than from
	// whatever the tests before this one spent.
	doorNoise.reset()

	const flood = 150
	const line = "node link did not introduce itself"
	sent, redials := 0, 0
	var client *xssh.Client
	for sent < flood {
		if client == nil {
			c, err := dialAs(s.addr, NodeUser, newNodeKey(t))
			if err != nil {
				t.Fatalf("dial node door: %v", err)
			}
			client = c
			defer client.Close() //nolint:errcheck
			redials++
			if redials > 25 {
				t.Fatalf("gave up after %d connections with %d sessions sent", redials, sent)
			}
		}
		// A session that starts the link command and then says nothing is the
		// cheapest line an unapproved peer can make the door write.
		sess, err := client.NewSession()
		if err != nil {
			client = nil // the admission watchdog hung this connection up
			continue
		}
		in, err := sess.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Start(nodelink.LinkCommand); err != nil {
			sess.Close() //nolint:errcheck
			client = nil
			continue
		}
		in.Close()   //nolint:errcheck
		sess.Wait()  //nolint:errcheck // it exits 2 by design
		sess.Close() //nolint:errcheck
		sent++
	}

	// The ceiling is a burst allowance, not silence: a whole fleet arriving at
	// once still gets logged, and a flood gets a bounded fraction of itself.
	if got := handler.count(line); got > nodes.MaxPending+8 {
		t.Errorf("%d sessions that said nothing wrote %d %q lines; the door has no bound on what a stranger can make it log",
			flood, got, line)
	}
	if handler.count(line) == 0 {
		t.Errorf("the door logged nothing at all about %d peers that never introduced themselves", flood)
	}
}
