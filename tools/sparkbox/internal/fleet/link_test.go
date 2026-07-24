package fleet_test

// Revocation, end to end within one process: a real ServeLink over a pipe, and
// what an operator taking a machine's approval away actually does to it.
//
// The transport is net.Pipe rather than SSH because none of this is about the
// transport — the frames are newline-delimited JSON either way, and what is
// under test is whether a machine that has been told to leave is still in the
// fleet's answers a moment later.

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// linkedNode is the machine's end of a link the fleet is serving.
type linkedNode struct {
	served chan error
	frames chan nodelink.Frame
}

// linkTo joins a machine to a fleet and returns the node's end of it.
//
// The node end drains continuously: a net.Pipe is unbuffered, so the welcome —
// and later the bye — blocks the gateway until somebody reads it, and a test
// that never read would hang the teardown it is here to observe.
func linkTo(t *testing.T, ctx context.Context, f *fleet.Fleet, name string) *linkedNode {
	t.Helper()
	gwEnd, nodeEnd := net.Pipe()
	t.Cleanup(func() { gwEnd.Close(); nodeEnd.Close() })

	n := &linkedNode{served: make(chan error, 1), frames: make(chan nodelink.Frame, 64)}
	go func() {
		dec := json.NewDecoder(nodeEnd)
		for {
			var f nodelink.Frame
			if err := dec.Decode(&f); err != nil {
				return
			}
			select {
			case n.frames <- f:
			default:
			}
		}
	}()

	hello, err := json.Marshal(nodelink.Hello{Protocol: nodelink.Protocol, Node: name, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	go json.NewEncoder(nodeEnd).Encode(nodelink.Frame{ //nolint:errcheck // ReadHello reports it
		ID: "n1", Type: nodelink.TypeHello, Body: hello,
	})

	greeting, err := nodelink.ReadHello(ctx, gwEnd, 5*time.Second)
	if err != nil {
		t.Fatalf("ReadHello: %v", err)
	}
	go func() {
		n.served <- f.ServeLink(ctx, nodelink.ServerOptions{
			Node: name, Greeting: greeting, Session: gwEnd, Log: discardLog(),
			// Far longer than the test, so the liveness prober never becomes the
			// reason a link ended.
			PingEvery: time.Hour,
		})
	}()

	waitFor(t, "the node to join the fleet", func() bool { return f.Online(name) })
	return n
}

// awaitBye reads until the gateway says goodbye.
func (n *linkedNode) awaitBye(t *testing.T) nodelink.Bye {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-n.frames:
			if f.Type != nodelink.TypeBye {
				continue
			}
			var b nodelink.Bye
			if err := json.Unmarshal(f.Body, &b); err != nil {
				t.Fatalf("bye body: %v", err)
			}
			return b
		case <-deadline:
			t.Fatal("timed out waiting for a bye")
		}
	}
}

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

// TestEvictingANodeTakesItOutOfTheFleet is what makes a revocation real.
//
// Approval is read once, at the door. Nothing consults the roster again while a
// link is up, and the link carries no idle timeout by design — so a machine
// removed or disabled while connected would otherwise keep its control channel,
// keep its capacity in the fleet's totals, and keep both for as long as it
// pleased.
func TestEvictingANodeTakesItOutOfTheFleet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := newFleet(t, newManager(t, host.Options{}), newIndex(t))
	node := linkTo(t, ctx, f, "node-b")

	if got := len(f.Capacities()); got != 2 {
		t.Fatalf("capacities before eviction = %d, want 2 (this gateway and node-b)", got)
	}

	if !f.EvictNode("node-b", "it was removed from this fleet") {
		t.Fatal("EvictNode reported no link for a machine that was linked")
	}

	// Checked immediately, not after a wait: the teardown of the connection is
	// asynchronous, and a revoked machine that is still counted as capacity in
	// the meantime is exactly what an operator ran the command to stop.
	if got := len(f.Capacities()); got != 1 {
		t.Errorf("capacities after eviction = %d, want 1 (this gateway alone)", got)
	}
	for _, c := range f.Capacities() {
		if c.Node == "node-b" {
			t.Error("a revoked node is still reporting capacity into the fleet")
		}
	}
	if got := len(f.Nodes()); got != 1 {
		t.Errorf("fleet lists %d machines after eviction, want 1", got)
	}
	if f.Online("node-b") {
		t.Error("a revoked node still reads as online")
	}

	if b := node.awaitBye(t); b.Code != nodelink.CodeRevoked {
		t.Errorf("bye code = %q, want %q", b.Code, nodelink.CodeRevoked)
	}
	select {
	case <-node.served:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeLink outlived the link it was serving")
	}
}

// TestEvictingAMachineWithNoLinkIsHarmless is the single-box shape: nothing is
// attached, so there is nothing to close and the answer is no rather than a
// panic on a nil node.
func TestEvictingAMachineWithNoLinkIsHarmless(t *testing.T) {
	f := newFleet(t, newManager(t, host.Options{}), newIndex(t))
	if f.EvictNode("nobody", "it was removed from this fleet") {
		t.Error("EvictNode claimed to close a link that never existed")
	}
	if got := len(f.Capacities()); got != 1 {
		t.Errorf("capacities = %d, want 1", got)
	}
}
