package nodelink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// recordingLog captures what a link says about itself.
//
// Two tests here and in server_test.go are about log lines as an operator
// artefact rather than as debugging noise: one asserts how MANY there are, the
// other asserts one of them is legible enough to act on. Neither can be written
// against a discarding handler, and both need the attrs a derived logger
// carries — the gateway half tags every line with its node — so WithAttrs
// accumulates rather than dropping.
type logLine struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

func (l logLine) String() string { return fmt.Sprintf("%s %q %v", l.Level, l.Msg, l.Attrs) }

type logSink struct {
	mu    sync.Mutex
	lines []logLine
}

type recordingLog struct {
	sink  *logSink
	attrs []slog.Attr
}

func newRecordingLog() *recordingLog { return &recordingLog{sink: &logSink{}} }

func (h *recordingLog) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingLog) Handle(_ context.Context, r slog.Record) error {
	line := logLine{Level: r.Level, Msg: r.Message, Attrs: map[string]string{}}
	for _, a := range h.attrs {
		line.Attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		line.Attrs[a.Key] = a.Value.String()
		return true
	})
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	h.sink.lines = append(h.sink.lines, line)
	return nil
}

func (h *recordingLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &recordingLog{sink: h.sink, attrs: merged}
}

func (h *recordingLog) WithGroup(string) slog.Handler { return h }

// at returns everything logged at one level, newest last.
func (h *recordingLog) at(level slog.Level) []logLine {
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	var out []logLine
	for _, l := range h.sink.lines {
		if l.Level == level {
			out = append(out, l)
		}
	}
	return out
}

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

// stalledWriter is a peer that has stopped reading: the first write parks and
// stays parked until the test lets it go. It is the SSH channel window in
// miniature — past roughly 2 MiB unread, x/crypto's Write does exactly this,
// with no deadline and no context, for as long as the peer stays quiet.
type stalledWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newStalledWriter() *stalledWriter {
	return &stalledWriter{entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *stalledWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

// TestQuietPeerCannotParkTheLink is why a frame is handed to a writer goroutine
// instead of being written under a lock every caller shares.
//
// A node that stops reading — hostile, wedged, or merely swapped out — parks the
// gateway's write forever. If that write happens on the caller's goroutine and
// under a mutex, it takes the whole link with it: Request stops honouring its
// context, the ping loop that exists to notice a dead node parks on the same
// mutex, and Hangup parks behind them both, so the machine's own replacement
// link waits on the corpse of the one it is replacing.
func TestQuietPeerCannotParkTheLink(t *testing.T) {
	w := newStalledWriter()
	t.Cleanup(func() { close(w.release) })
	// No Serve: a peer that is not reading is not answering either, so nothing
	// ever arrives on this link and every wait below must end on its own terms.
	c := NewConn(strings.NewReader(""), w, "g", nil)

	// Park the transport with one frame mid-write, which is a peer whose
	// receive window has filled.
	go c.Event(TypeHeartbeat, Heartbeat{At: time.Now()}) //nolint:errcheck // the assertions below are about what does NOT block
	<-w.entered

	// The ping loop's hangup writes a bye. It runs on the only goroutine that
	// can notice this node is dead, so it must come back.
	sent := make(chan error, 1)
	go func() { sent <- c.Event(TypeBye, Bye{Code: CodeProtocolError, Msg: "no answer to two pings"}) }()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("an event parked on a peer that stopped reading; the ping loop and Hangup both ride this path")
	}

	// And a request must come back on its own context rather than on the
	// peer's goodwill.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Request(ctx, TypePing, PingReq{Nonce: "still there?"}, nil) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Request reported %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Request ignored its context while the peer was not reading")
	}

	// Teardown last: a Close that waited on the peer would hold the fleet's
	// supersession path open behind the machine it is trying to replace.
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close was parked by a write the peer is not reading")
	}
}

// saturatedLink is a link whose peer is holding every bulk slot it has.
type saturatedLink struct {
	gw *Conn
	// results carries one value per held archive, so a test can assert that a
	// machine at its ceiling still FINISHES the work it took.
	results chan error
	// release lets those handlers return. It is idempotent because the cleanup
	// calls it too.
	release func()
}

// saturatedPair holds MaxInFlightOps archives inside handlers that will not
// return until the test says so, which is what "the node's disk is at its
// ceiling" means with no sleep anywhere in it.
func saturatedPair(t *testing.T) *saturatedLink {
	t.Helper()
	entered := make(chan struct{}, 4*MaxInFlightOps)
	release := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(release) }) }
	t.Cleanup(stop)

	gw, _ := newPipePair(t, nil, func(c *Conn) {
		pingHandler(c)
		// A short, user-facing operation. Every `ssh box@gateway` resumes its
		// sandbox through one of these, so the tests below can ask what a busy
		// machine does to a person arriving at a terminal.
		c.Handle(TypeEnsureRunning, func(context.Context, json.RawMessage) (any, error) {
			return EmptyResp{}, nil
		})
		c.Handle(TypeArchive, func(ctx context.Context, _ json.RawMessage) (any, error) {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return EmptyResp{}, nil
		})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	results := make(chan error, MaxInFlightOps)
	for i := 0; i < MaxInFlightOps; i++ {
		go func(i int) {
			results <- gw.Request(ctx, TypeArchive, NameReq{Name: fmt.Sprintf("box-%d", i)}, nil)
		}(i)
	}
	for i := 0; i < MaxInFlightOps; i++ {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d handlers were entered", i, MaxInFlightOps)
		}
	}
	return &saturatedLink{gw: gw, results: results, release: stop}
}

// TestRequestsPastTheCeilingAreRefused: one goroutine per inbound request, with
// no ceiling, is a peer's invitation to allocate as it pleases — every one of
// them retains its frame body, and a body may be MaxFrameBytes. The ceiling is
// MaxInFlightOps and passing it is an answer, not a silence: a caller told
// node_busy can retry or place the work elsewhere, where one silently queued or
// dropped can only wait out a deadline.
func TestRequestsPastTheCeilingAreRefused(t *testing.T) {
	s := saturatedPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	busy := make(chan error, 1)
	go func() { busy <- s.gw.Request(ctx, TypeArchive, NameReq{Name: "one-too-many"}, nil) }()

	select {
	case err := <-busy:
		var ce *ctlops.Error
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v (%T), want *ctlops.Error", err, err)
		}
		if ce.Kind != ctlops.KindCapacity || ce.Code != CodeNodeBusy {
			t.Fatalf("err = %+v, want a capacity/%s refusal", ce, CodeNodeBusy)
		}
		if ce.Op != TypeArchive {
			t.Errorf("op = %q, want %q", ce.Op, TypeArchive)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the request past %d in flight was accepted: nothing bounds the goroutines or the frame bodies a peer can make this side hold",
			MaxInFlightOps)
	}
}

// TestLivenessSurvivesASaturatedPeer: the ceiling must not turn a busy machine
// into one that looks dead. The gateway's only liveness detector is a ping that
// demands an answer, and a node refusing pings because it is archiving is a node
// the gateway hangs up on for working.
func TestLivenessSurvivesASaturatedPeer(t *testing.T) {
	s := saturatedPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out PingReq
	if err := s.gw.Request(ctx, TypePing, PingReq{Nonce: "alive"}, &out); err != nil {
		t.Fatalf("a machine at its op ceiling did not answer a ping: %v", err)
	}
	if out.Nonce != "alive" {
		t.Fatalf("nonce = %q, want %q", out.Nonce, "alive")
	}
}

// TestASaturatedNodeFinishesTheWorkItTook is the other half of the ceiling's
// contract, and the half a refusal policy is easy to break: refusing the ninth
// operation is only defensible if the eight already accepted all complete and
// the link they arrived on is still live afterwards. An honest machine working
// flat out must come out of this indistinguishable from an idle one.
func TestASaturatedNodeFinishesTheWorkItTook(t *testing.T) {
	s := saturatedPair(t)
	s.release()

	for i := 0; i < MaxInFlightOps; i++ {
		select {
		case err := <-s.results:
			if err != nil {
				t.Fatalf("an operation the node accepted failed: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d accepted operations finished", i, MaxInFlightOps)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.gw.Request(ctx, TypePing, PingReq{Nonce: "still here"}, nil); err != nil {
		t.Fatalf("the link did not survive a machine working at its ceiling: %v", err)
	}
}

// TestUserWorkIsNotRefusedByTheBulkCeiling is the ceiling's operational defect.
//
// A node holding many sandboxes runs long, disk-bound work — archives, clones,
// resizes — and it also serves everybody arriving at a terminal, because
// `ssh box@gateway` resumes its sandbox through this same link. With one pool
// for both, eight archives make the ninth PERSON the ceiling refuses, and it
// refuses them with the wrong reason: nothing on that machine is exhausted, and
// when something genuinely is, the host's own admission check answers with a
// capacity error naming the megabytes. Bulk work and user work therefore get
// separate ceilings.
func TestUserWorkIsNotRefusedByTheBulkCeiling(t *testing.T) {
	s := saturatedPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const arrivals = 3 * MaxInFlightOps
	var wg sync.WaitGroup
	errs := make(chan error, arrivals)
	for i := 0; i < arrivals; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("box-%d", i)
			if err := s.gw.Request(ctx, TypeEnsureRunning, NameReq{Name: name}, nil); err != nil {
				errs <- fmt.Errorf("%s: %w", name, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	refused := 0
	for err := range errs {
		refused++
		if refused == 1 {
			t.Errorf("a person resuming a sandbox was refused while the node's disk was busy: %v", err)
		}
	}
	if refused > 0 {
		t.Errorf("%d of %d arrivals were refused for want of a slot held by bulk work", refused, arrivals)
	}
}

// TestRefusalsAtTheCeilingDoNotFloodTheLog: a peer that keeps asking while the
// ceiling holds must not be able to write the gateway's disk full through the
// log. The refusal itself is per-frame — every caller gets its own typed answer
// — but the LINE about it is one machine-level fact, so it is rate-limited and
// the suppressed ones are counted into the next.
func TestRefusalsAtTheCeilingDoNotFloodTheLog(t *testing.T) {
	rec := newRecordingLog()
	c := NewConn(strings.NewReader(""), io.Discard, "n", slog.New(rec))

	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, MaxInFlightOps)
	c.Handle(TypeArchive, func(context.Context, json.RawMessage) (any, error) {
		entered <- struct{}{}
		<-release
		return EmptyResp{}, nil
	})
	for i := 0; i < MaxInFlightOps; i++ {
		c.dispatch(&Frame{ID: fmt.Sprintf("g%03d", i), Type: TypeArchive})
	}
	for i := 0; i < MaxInFlightOps; i++ {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d slots were taken", i, MaxInFlightOps)
		}
	}

	const flood = 500
	for i := 0; i < flood; i++ {
		c.dispatch(&Frame{ID: fmt.Sprintf("f%03d", i), Type: TypeArchive})
	}

	lines := rec.at(slog.LevelWarn)
	if len(lines) == 0 {
		t.Fatal("a link refusing every request said nothing at all")
	}
	// A handful, not one per frame: the exact number depends on where the rate
	// limit's window falls, and the property is that it does not scale with the
	// flood.
	if len(lines) > 4 {
		t.Errorf("%d log lines for %d refused frames: %v", len(lines), flood, lines[0])
	}
}

// TestBusySignalAggregatesWhatItSuppressed is the other half of the rate limit.
// Dropping lines is only acceptable if the ones that survive account for them,
// so a window's worth of silence is paid back in the next line's count rather
// than forgotten.
func TestBusySignalAggregatesWhatItSuppressed(t *testing.T) {
	var b busySignal
	base := time.Unix(1_700_000_000, 0)

	if n, _, say := b.record(base, busyLogEvery); !say || n != 1 {
		t.Fatalf("first refusal: n=%d say=%v, want 1/true — an operator must learn at once", n, say)
	}
	for i := 0; i < 499; i++ {
		if _, _, say := b.record(base.Add(time.Millisecond), busyLogEvery); say {
			t.Fatalf("refusal %d inside the window was logged; the flood is what this bounds", i+2)
		}
	}
	n, window, say := b.record(base.Add(busyLogEvery), busyLogEvery)
	if !say {
		t.Fatal("nothing was said after the window expired, so 500 refusals went unreported")
	}
	if n != 500 {
		t.Errorf("the line after the window reported %d refusals, want the 499 it swallowed plus this one", n)
	}
	// The window runs from the first refusal the line covers, not from the
	// previous line, so it is a hair under the interval.
	if window < busyLogEvery-time.Second {
		t.Errorf("window = %v, want about %v so the count can be read as a rate", window, busyLogEvery)
	}
}

// TestTheBulkCeilingHoldsOnlyDiskWork guards the classification itself.
//
// The two pools are only worth having if the right things are in them, and the
// failure is silent both ways: a long operation in the session pool means
// sixty-four concurrent archives on one disk, and a user-facing operation in the
// bulk pool means the ninth person to resume a sandbox is told the machine is
// busy while it is doing nothing of the sort. Neither shows up as a test failure
// anywhere else, so the list is asserted rather than trusted.
func TestTheBulkCeilingHoldsOnlyDiskWork(t *testing.T) {
	for _, typ := range []string{TypeCreate, TypeArchive, TypeResize, TypeSnapshotCreate, TypeSnapshotFork} {
		if !bulkOps[typ] {
			t.Errorf("%s copies or streams a whole rootfs; %d of those at once is not faster, it is thrashing",
				typ, MaxInFlightSessionOps)
		}
	}
	// Everything a person waits at a terminal for, plus the bookkeeping that
	// rides behind them. None of these may be refused because a disk is busy.
	for _, typ := range []string{
		TypeEnsureRunning, TypePause, TypeReboot, TypeRename, TypeDestroy,
		TypeSetPinned, TypeResyncEnv, TypeTouch, TypeRecordKey,
		TypeInventory, TypeSnapshotDelete,
	} {
		if bulkOps[typ] {
			t.Errorf("%s is on somebody's path to a shell; putting it behind the %d-slot disk ceiling refuses people for a reason that is not true",
				typ, MaxInFlightOps)
		}
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
