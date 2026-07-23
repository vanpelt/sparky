package nodelink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// newPipePair is the control channel with the SSH taken away: a Conn wants an
// io.Reader and an io.Writer and nothing else, so everything about framing,
// correlation and deadlines is testable without a socket.
func newPipePair(t *testing.T, setupA, setupB func(*Conn)) (*Conn, *Conn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	pa, pb := net.Pipe()
	a := NewConn(pa, pa, "g", nil)
	b := NewConn(pb, pb, "n", nil)
	if setupA != nil {
		setupA(a)
	}
	if setupB != nil {
		setupB(b)
	}
	go a.Serve(ctx) //nolint:errcheck // the tests that care read Err()
	go b.Serve(ctx) //nolint:errcheck
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func TestRequestReply(t *testing.T) {
	a, _ := newPipePair(t, nil, pingHandler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out PingReq
	if err := a.Request(ctx, TypePing, PingReq{Nonce: "abc"}, &out); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if out.Nonce != "abc" {
		t.Fatalf("nonce = %q, want %q", out.Nonce, "abc")
	}
}

// TestTypedErrorSurvivesTheHop is why this transport carries ctlops.WireError
// rather than a string: sshgw's and xterm's capacity renderers switch on the
// concrete *host.CapacityError, and they must keep firing on an error that came
// off a wire without either of them learning that nodes exist.
func TestTypedErrorSurvivesTheHop(t *testing.T) {
	want := &host.CapacityError{RequestedMB: 4096, UsedMB: 12288, BudgetMB: 14336}
	a, _ := newPipePair(t, nil, func(c *Conn) {
		c.Handle(TypeCreate, func(context.Context, json.RawMessage) (any, error) { return nil, want })
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := a.Request(ctx, TypeCreate, CreateReq{Name: "demo"}, nil)
	if err == nil {
		t.Fatal("Request succeeded, want the node's capacity refusal")
	}

	var ce *ctlops.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *ctlops.Error", err, err)
	}
	if ce.Kind != ctlops.KindCapacity {
		t.Errorf("kind = %v, want %v", ce.Kind, ctlops.KindCapacity)
	}
	if ce.Op != TypeCreate {
		t.Errorf("op = %q, want %q", ce.Op, TypeCreate)
	}
	var got *host.CapacityError
	if !errors.As(err, &got) {
		t.Fatalf("errors.As did not reach *host.CapacityError through %v", err)
	}
	if *got != *want {
		t.Errorf("capacity error = %+v, want %+v", *got, *want)
	}
}

// TestRequestCarriesDeadline: the responder must bound its work by the
// requester's remaining budget, less a margin so it gives up first and the
// requester gets a typed answer instead of a timeout to interpret.
func TestRequestCarriesDeadline(t *testing.T) {
	seen := make(chan time.Duration, 1)
	a, _ := newPipePair(t, nil, func(c *Conn) {
		c.Handle(TypePing, func(ctx context.Context, _ json.RawMessage) (any, error) {
			dl, ok := ctx.Deadline()
			if !ok {
				seen <- 0
				return EmptyResp{}, nil
			}
			seen <- time.Until(dl)
			return EmptyResp{}, nil
		})
	})

	const budget = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := a.Request(ctx, TypePing, PingReq{}, nil); err != nil {
		t.Fatalf("Request: %v", err)
	}
	got := <-seen
	if got == 0 {
		t.Fatal("the handler ran with no deadline at all")
	}
	if got > budget-LinkMargin || got < budget-LinkMargin-2*time.Second {
		t.Errorf("handler budget %v, want about %v", got, budget-LinkMargin)
	}
}

// TestHandlerOutlivesTheLink is the rule the whole design rests on. A node's
// work is derived from its own process context plus the frame's deadline, never
// from the link, so a gateway that restarts mid-archive leaves the archive
// running and discards the reply. Binding it to the link would tear down
// running VMs for want of a control plane.
func TestHandlerOutlivesTheLink(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan error, 1)
	a, _ := newPipePair(t, nil, func(c *Conn) {
		c.Handle(TypeArchive, func(ctx context.Context, _ json.RawMessage) (any, error) {
			<-release
			finished <- ctx.Err()
			return EmptyResp{}, nil
		})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sent := make(chan error, 1)
	go func() { sent <- a.Request(ctx, TypeArchive, NameReq{Name: "demo"}, nil) }()

	// Give the handler time to be entered, then take the link away underneath it.
	time.Sleep(100 * time.Millisecond)
	a.Close()

	select {
	case err := <-sent:
		if err == nil {
			t.Fatal("the caller's request succeeded after the link died")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the caller was not woken by the link's death")
	}

	close(release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("the node's work was cancelled by the link dropping: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the node's work never finished")
	}
}

func TestUnknownRequestTypeIsAnswered(t *testing.T) {
	a, _ := newPipePair(t, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := a.Request(ctx, "sandbox.teleport", NameReq{Name: "demo"}, nil)
	var ce *ctlops.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *ctlops.Error", err, err)
	}
	if ce.Kind != ctlops.KindInvalid || ce.Code != "unknown_request" {
		t.Fatalf("err = %+v, want an invalid/unknown_request", ce)
	}
}

func TestEvents(t *testing.T) {
	got := make(chan GoneMsg, 1)
	a, b := newPipePair(t, nil, func(c *Conn) {
		c.OnEvent(TypeGone, func(body json.RawMessage) {
			var m GoneMsg
			json.Unmarshal(body, &m) //nolint:errcheck // a bad body fails the assertion below
			got <- m
		})
	})

	// An event nobody registered for is how a newer peer tells an older one
	// something it does not need to know: dropped, not an error.
	if err := a.Event("sandbox.teleported", NameReq{Name: "demo"}); err != nil {
		t.Fatalf("unknown event: %v", err)
	}
	if err := a.Event(TypeGone, GoneMsg{Node: "node-b", Name: "demo", Reason: "destroyed"}); err != nil {
		t.Fatalf("Event: %v", err)
	}
	select {
	case m := <-got:
		if m.Name != "demo" || m.Node != "node-b" {
			t.Fatalf("gone = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event never arrived")
	}
	if err := b.Err(); err != nil {
		t.Fatalf("an unknown event killed the link: %v", err)
	}
}

// TestCancelWithdrawsInterest: a caller who has given up says so, so the node
// can stop early. It stays advisory — the deadline is the backstop — because a
// cancel that loses the race to a finished operation must be a no-op.
func TestCancelWithdrawsInterest(t *testing.T) {
	entered := make(chan struct{})
	observed := make(chan error, 1)
	a, _ := newPipePair(t, nil, func(c *Conn) {
		c.Handle(TypeArchive, func(ctx context.Context, _ json.RawMessage) (any, error) {
			close(entered)
			<-ctx.Done()
			observed <- ctx.Err()
			return EmptyResp{}, nil
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Request(ctx, TypeArchive, NameReq{Name: "demo"}, nil) }()
	<-entered
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the caller was not released by its own cancellation")
	}
	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handler saw %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel event never reached the handler")
	}
}

// TestFailNamesTheReason: Fail is how the gateway turns a dead socket into a
// sentence about a machine, which this package is deliberately unable to write
// on its own.
func TestFailNamesTheReason(t *testing.T) {
	unreachable := errors.New(`sandbox "demo" lives on node "node-b", which is offline`)
	a, _ := newPipePair(t, nil, func(c *Conn) {
		c.Handle(TypeArchive, func(ctx context.Context, _ json.RawMessage) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Request(ctx, TypeArchive, NameReq{Name: "demo"}, nil) }()
	time.Sleep(50 * time.Millisecond)
	a.Fail(unreachable)

	select {
	case err := <-done:
		if !errors.Is(err, unreachable) {
			t.Fatalf("pending request reported %v, want %v", err, unreachable)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Fail did not wake the pending request")
	}
	// And a request made after the link died gets the same sentence rather than
	// a bare io.EOF a caller would have to translate.
	if err := a.Request(ctx, TypePing, PingReq{}, nil); !errors.Is(err, unreachable) {
		t.Fatalf("post-mortem request reported %v, want %v", err, unreachable)
	}
}

// TestOversizedLineIsBounded: a peer that never sends a newline must be refused
// at MaxFrameBytes rather than buffered until the process dies.
func TestOversizedLineIsBounded(t *testing.T) {
	d := newDecoder(strings.NewReader(strings.Repeat("x", 2<<20) + "\n"))
	_, err := d.next()
	if err == nil {
		t.Fatal("a 2 MiB line decoded successfully")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want a bounded frame-size error", err)
	}
}

// TestOversizedFrameEndsTheLink is the same property end to end: the link
// closes with an error rather than growing a buffer.
func TestOversizedFrameEndsTheLink(t *testing.T) {
	p := newLinkPair(t, linkOptions{})

	// Written from a goroutine because the far side stops reading the moment it
	// gives up, and a test must not be able to block on its own bad input.
	go func() {
		p.nodeStdin.Write([]byte(strings.Repeat("x", 2<<20) + "\n")) //nolint:errcheck // the reader is expected to hang up
	}()

	select {
	case err := <-p.gwServeErr:
		if err == nil {
			t.Fatal("Serve returned cleanly on a 2 MiB line")
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("Serve returned %v, want a bounded frame-size error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the link stayed up on a 2 MiB line")
	}
	if p.gw.Err() == nil {
		t.Error("the link is still reporting itself healthy")
	}
}

// TestLinkEndsWhenTheGatewayGoesAway: a node must notice, because its whole
// supervisor loop is "serve until this returns, then back off and redial".
func TestLinkEndsWhenTheGatewayGoesAway(t *testing.T) {
	p := newLinkPair(t, linkOptions{nodeSetup: pingHandler})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.gw.Request(ctx, TypePing, PingReq{Nonce: "up"}, nil); err != nil {
		t.Fatalf("the link was not up to begin with: %v", err)
	}

	p.srv.Close()
	select {
	case err := <-p.ndServeErr:
		// A gateway that went away is routine, not a fault: the node must be
		// able to tell this from a protocol error it should complain about.
		if err != nil {
			t.Fatalf("node Serve returned %v, want nil on a clean disconnect", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the node's serve loop hung after the gateway went away")
	}
	if p.node.Err() == nil {
		t.Error("the node still thinks its link is alive")
	}
}

// TestConcurrentRequests: request IDs are minted per side and never reused, so
// two hundred callers on one link get two hundred distinct answers.
func TestConcurrentRequests(t *testing.T) {
	const callers = 200
	p := newLinkPair(t, linkOptions{nodeSetup: pingHandler})

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			want := fmt.Sprintf("nonce-%d", i)
			var out PingReq
			if err := p.gw.Request(ctx, TypePing, PingReq{Nonce: want}, &out); err != nil {
				errs <- fmt.Errorf("caller %d: %w", i, err)
				return
			}
			if out.Nonce != want {
				errs <- fmt.Errorf("caller %d got %q, want %q — replies are crossing", i, out.Nonce, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestRequestIDsAreSidePrefixed: each side only ever looks up IDs it issued, so
// the two spaces cannot collide however either side numbers them.
func TestRequestIDsAreSidePrefixed(t *testing.T) {
	c := NewConn(strings.NewReader(""), io.Discard, "g", nil)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := c.nextID()
		if !strings.HasPrefix(id, "g") {
			t.Fatalf("id %q is not prefixed with its side", id)
		}
		if seen[id] {
			t.Fatalf("id %q was minted twice", id)
		}
		seen[id] = true
	}
}
