package nodelink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
)

// ErrLinkClosed is what pending and future requests report once Close has been
// called. A caller that wants a better sentence than this — "that machine is
// not answering", naming the node — calls Fail with one before closing, which
// is exactly what the gateway half does.
var ErrLinkClosed = errors.New("node link closed")

// Handler answers one request type. Its ctx is derived from the Conn's base
// context and the frame's deadline, never from the link.
type Handler func(ctx context.Context, body json.RawMessage) (any, error)

// The two ceilings a link answers requests under, and why there are two.
//
// A slot is a goroutine plus the frame body it holds, so both numbers are memory
// bounds before they are anything else, and neither is a queue depth: a request
// that arrives with no slot free is answered CodeNodeBusy rather than parked,
// because silent backpressure only moves the failure to somewhere the caller
// cannot see it.
//
// They are split because the work behind them fails differently.
//
// Bulk work — copying a rootfs, streaming an archive to object storage, forking
// a template — serializes on one machine's disk. Eight at once is already past
// the point where more means faster, so MaxInFlightOps stays where it was and
// the ninth archive is told to come back in a moment. That refusal is true.
//
// Everything else is somebody at a terminal. `ssh box@gateway` resumes a paused
// sandbox through this link, and a node holding a hundred of them can easily
// have more than eight people arrive at once — a morning, a CI fan-out, a
// gateway restart that resumes what it finds. Refusing those for want of a slot
// would refuse them with the WRONG REASON: nothing on that machine is exhausted,
// and when something genuinely is, the node's own host.Manager answers with a
// *host.CapacityError naming the megabytes it could not find. A transport
// ceiling must never pre-empt that, so the ceiling for user-facing work is set
// where a real machine's hardware refuses first.
//
// The worst case a hostile peer can pin is (MaxInFlightSessionOps +
// MaxInFlightOps) frame bodies, and a body is bounded by MaxFrameBytes — 72 MiB
// on a machine that runs virtual machines, from a peer that has already
// authenticated as this gateway's node or as this node's gateway.
const MaxInFlightSessionOps = 64

// bulkOps are the request types that hold a disk for a long time. It is an
// exception list rather than a classification of every verb: a request type
// nobody has thought about is a user waiting for it, which is the side to err
// on, and a new long-running verb is one line here.
var bulkOps = map[string]bool{
	TypeCreate:         true,
	TypeArchive:        true,
	TypeResize:         true,
	TypeSnapshotCreate: true,
	TypeSnapshotFork:   true,
}

// busyLogEvery bounds how often a link says out loud that it is at a ceiling.
//
// The refusal itself is per-request and stays that way — every caller gets its
// own typed answer. The LINE is one machine-level fact, and a peer that keeps
// asking while the ceiling holds would otherwise write one per frame: a
// five-hundred-frame flood produced four hundred and ninety-two lines, which is
// a peer being handed the gateway's disk. So the first refusal in a window is
// logged at once — an operator learns immediately — and the rest are counted
// into the next line.
const busyLogEvery = 30 * time.Second

// busySignal aggregates refusals at a ceiling into a bounded number of lines.
type busySignal struct {
	mu sync.Mutex
	// n counts refusals not yet spoken for, since is when that count started,
	// and last is when a line was written.
	n     int
	since time.Time
	last  time.Time
}

// record folds one refusal in and reports whether it is time to say something.
// The count it returns covers every refusal since the previous line, this one
// included, so no refusal is invisible for longer than one window. The window is
// the caller's because the two conditions this type is used for are read at
// different rates — see tooManySandboxes.
func (b *busySignal) record(now time.Time, every time.Duration) (refused int, window time.Duration, say bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	if b.since.IsZero() {
		b.since = now
	}
	if !b.last.IsZero() && now.Sub(b.last) < every {
		return 0, 0, false
	}
	refused, window = b.n, now.Sub(b.since)
	b.n, b.since, b.last = 0, time.Time{}, now
	return refused, window, true
}

// Conn is one framed control channel: a reader goroutine, a writer goroutine, a
// table of waiters keyed by request ID, and two bounded pools of slots for the
// requests it answers. It is transport-agnostic on purpose — the SSH session it
// rides in production is nothing more than an io.Reader and an io.Writer to it.
type Conn struct {
	out       *writer
	dec       *decoder
	log       *slog.Logger
	idPrefix  string
	metrics   *fleetmetrics.Registry
	node      string
	transport string

	// slots and bulkSlots bound how many of this peer's requests may be in
	// flight at once, split by what the work behind them costs — see
	// MaxInFlightSessionOps. Buffered channels rather than a semaphore type
	// because the interesting operation is the one that must NOT wait: a full
	// pool is answered, not queued. They are independent, so a machine whose
	// disk is saturated still answers everybody arriving at a terminal.
	slots     chan struct{}
	bulkSlots chan struct{}
	// busy rate-limits the log line the two pools share. The refusals are not
	// rate-limited; only what is said about them is.
	busy busySignal
	// now is the clock the rate limit is measured on, so a test can drive a
	// window without waiting one out.
	now func() time.Time

	// closeOnce guards the underlying transport, which Close and a ctx
	// cancellation both race to shut.
	closeOnce sync.Once
	closers   []io.Closer

	mu       sync.Mutex
	seq      uint64
	pending  map[string]chan *Frame
	inflight map[string]context.CancelFunc
	handlers map[string]Handler
	events   map[string]func(json.RawMessage)
	base     context.Context
	failure  error
	done     chan struct{}
}

// SetMetrics attaches optional transport-neutral instrumentation to this
// connection. Existing constructors stay unchanged, and a caller that does not
// opt in pays only nil checks. Call this before the connection is served.
func (c *Conn) SetMetrics(m *fleetmetrics.Registry, node, transport string) {
	c.metrics = m
	c.node = node
	c.transport = transport
	c.out.setMetrics(m, node, transport)
}

// NewConn wraps one direction each of a duplex transport. idPrefix is the
// single character that makes the two sides' request IDs disjoint — "g" on the
// gateway, "n" on the node — so neither side ever has to consider whether an
// ID it is looking up might be the other's.
func NewConn(r io.Reader, w io.Writer, idPrefix string, log *slog.Logger) *Conn {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Conn{
		dec:       newDecoder(r),
		log:       log,
		idPrefix:  idPrefix,
		slots:     make(chan struct{}, MaxInFlightSessionOps),
		bulkSlots: make(chan struct{}, MaxInFlightOps),
		now:       time.Now,
		pending:   map[string]chan *Frame{},
		inflight:  map[string]context.CancelFunc{},
		handlers:  map[string]Handler{},
		events:    map[string]func(json.RawMessage){},
		base:      context.Background(),
		done:      make(chan struct{}),
	}
	// The writer reports a dead socket through Fail, so a peer that stopped
	// reading wakes every waiter with the write's own error rather than leaving
	// each to discover it when its deadline expires.
	c.out = newWriter(w, c.Fail)
	// A duplex transport is usually one object presented twice (an SSH channel, a
	// net.Pipe half); a node's session presents two. Collect whichever halves can
	// be shut, and only once, so Close does not report a second close as a fault.
	rc, _ := r.(io.Closer)
	if rc != nil {
		c.closers = append(c.closers, rc)
	}
	if wc, ok := w.(io.Closer); ok && wc != rc {
		c.closers = append(c.closers, wc)
	}
	return c
}

// SetBaseContext installs the context handlers derive their work from. It
// defaults to context.Background() and must never default to Serve's, because
// on the gateway Serve runs on the SSH session's context: binding a node's
// fifteen-minute archive to the session that asked for it would abandon the
// operation the moment a laptop lid closed. A node passes its process context
// here so that a shutdown — and only a shutdown — cancels work in flight.
func (c *Conn) SetBaseContext(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.base = ctx
}

// Handle registers the answer to one request type. Unregistered types are
// answered with an invalid-request error rather than silence, so a version skew
// reads as a sentence in a log instead of a hung caller.
//
// Every handler but one runs on a goroutine drawn from one of the two pools
// described above MaxInFlightSessionOps — the bulk pool if its type is listed in
// bulkOps, the session pool otherwise — so it may take as long as its deadline
// allows. The exception is TypePing, which runs on the read goroutine — see
// dispatch for why — and must therefore do nothing but echo.
func (c *Conn) Handle(typ string, fn Handler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[typ] = fn
}

// OnEvent registers an event sink. Unregistered events are dropped without
// comment: an event nobody asked about is how a newer peer tells an older one
// something it does not need to know.
func (c *Conn) OnEvent(typ string, fn func(json.RawMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events[typ] = fn
}

// Serve reads frames until the transport ends or desyncs, then fails every
// waiter. It returns nil on a clean end of stream — a gateway restart is
// routine, and the node's supervisor must be able to tell it apart from a
// protocol fault it should complain about.
func (c *Conn) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	for {
		f, err := c.dec.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.Fail(io.EOF)
				return nil
			}
			c.Fail(err)
			return err
		}
		c.dispatch(f)
	}
}

// Request sends one and waits for its reply. The wait ends on the reply, on the
// caller's context, or on the link's death — never on nothing, which is the
// only reason a control plane can afford to sit behind a network hop at all.
// That holds for the send as much as for the wait: the frame is handed to the
// writer goroutine under the same context, so a peer that has stopped reading
// costs this caller its deadline and nothing else.
func (c *Conn) Request(ctx context.Context, typ string, body, out any) (err error) {
	started := time.Now()
	defer func() {
		c.metrics.ObserveControlRPC(c.node, c.transport, typ, metricOutcome(err), time.Since(started))
	}()
	raw, err := marshalBody(body)
	if err != nil {
		return err
	}
	f := &Frame{ID: c.nextID(), Type: typ, Body: raw}
	if dl, ok := ctx.Deadline(); ok {
		budget := time.Until(dl)
		if budget <= 0 {
			return context.DeadlineExceeded
		}
		// The margin makes the responder give up first, so the requester gets a
		// typed answer instead of a timeout it has to interpret. A budget
		// already smaller than the margin rides whole rather than being refused
		// outright: an almost-expired request is still the caller's to make.
		if budget > LinkMargin {
			budget -= LinkMargin
		}
		f.DeadlineMS = max(budget.Milliseconds(), 1)
	}

	ch := make(chan *Frame, 1)
	c.mu.Lock()
	if c.failure != nil {
		err := c.failure
		c.mu.Unlock()
		return err
	}
	c.pending[f.ID] = ch
	c.mu.Unlock()
	c.metrics.AddPending(c.node, c.transport, 1)
	defer func() {
		c.mu.Lock()
		delete(c.pending, f.ID)
		c.mu.Unlock()
		c.metrics.AddPending(c.node, c.transport, -1)
	}()

	if err := c.out.send(ctx, f); err != nil {
		// A write that fails because the link is gone reports the link's reason,
		// not the socket's: "that machine is offline" is what a caller can act on,
		// and io.EOF is what it would have to translate.
		if lerr := c.Err(); lerr != nil {
			return lerr
		}
		return err
	}

	select {
	case reply := <-ch:
		if reply.Err != nil {
			return ctlops.FromWire(reply.Err)
		}
		if out == nil || len(reply.Body) == 0 {
			return nil
		}
		return json.Unmarshal(reply.Body, out)
	case <-ctx.Done():
		// Advisory: the responder's deadline is the backstop, and a cancel that
		// loses the race to a finished operation is a no-op on the far side.
		_ = c.Event(TypeCancel, Cancel{ID: f.ID})
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}

// Event sends a frame nobody will reply to. The two highest-frequency writes in
// the system — touch and record_key — go this way because making either of them
// a round trip would put a network hop inside every SSH session teardown and
// every browser keystroke batch.
//
// It never waits. An event whose caller is a manager lock, a reaper hook or the
// ping loop on its way to hanging up must not be parkable by the peer it is
// about — so a backlogged link reports ErrLinkBacklogged and the caller decides,
// which for every caller in this tree means a log line and a resync.
func (c *Conn) Event(typ string, body any) error {
	raw, err := marshalBody(body)
	if err != nil {
		return err
	}
	return c.out.post(&Frame{Type: typ, Body: raw})
}

// Fail records the error pending and future requests report. It is how the
// gateway half turns a dead socket into "sandbox %q lives on node %q, which is
// offline" without this package having to know what a node is.
//
// The first call wins, so a caller that wants its own wording states it before
// the link can die of natural causes; everything after is Serve recording
// whatever ended the stream.
func (c *Conn) Fail(err error) {
	if err == nil {
		err = ErrLinkClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return
	}
	c.failure = err
	close(c.done)
	c.metrics.IncDisconnect(c.node, c.transport, metricDisconnectReason(err))
}

// Err reports why the link is dead, or nil while it lives.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failure
}

// Close ends the link. The transport is closed too when it can be — an SSH
// channel can, a pipe half cannot — because a reader blocked in Read is
// otherwise unreachable.
func (c *Conn) Close() error {
	c.Fail(ErrLinkClosed)
	var err error
	c.closeOnce.Do(func() {
		// The writer gets its moment first: the frame most likely to be queued
		// when a link is closed is the bye that explains why, and a peer that
		// never receives it has to wait out a grace period to infer it.
		c.out.shutdown()
		for _, cl := range c.closers {
			if cerr := cl.Close(); cerr != nil && err == nil {
				err = cerr
			}
		}
	})
	return err
}

func (c *Conn) nextID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return fmt.Sprintf("%s%016x", c.idPrefix, c.seq)
}

// dispatch routes one frame. A reply is delivered inline — the waiter is
// already parked on a buffered channel — while a request gets one of a bounded
// number of goroutines, so a slow operation cannot stall the frames behind it
// and a fast peer cannot mint goroutines faster than they finish.
func (c *Conn) dispatch(f *Frame) {
	switch {
	case f.Type == TypeReply:
		c.mu.Lock()
		ch := c.pending[f.ID]
		c.mu.Unlock()
		if ch == nil {
			// A reply to a request whose caller has already given up. Expected
			// whenever a deadline fires, so it is not worth a log line above Debug.
			c.log.Debug("nodelink: reply for unknown request", "id", f.ID)
			c.recordDrop("reply")
			return
		}
		// Non-blocking: the waiter's channel holds one, and a peer that replied
		// twice to the same ID must not be able to park the reader goroutine.
		select {
		case ch <- f:
		default:
			c.log.Debug("nodelink: duplicate reply dropped", "id", f.ID)
			c.recordDrop("reply")
		}
	case f.Type == TypePing && f.ID != "":
		// The liveness probe is answered on the read goroutine itself, and it is
		// the only request type that is. Two structural reasons, both about not
		// letting load look like death: a machine working at its op ceiling must
		// still be able to prove it is alive, and refusing a ping for want of a
		// slot would have the gateway hang up on a node whose only fault is
		// being busy; and a peer that floods pings must not be able to mint a
		// goroutine per frame. A ping handler is a bounded echo by contract —
		// PingReq in, PingReq out — so nothing it does can stall what is behind
		// it, and the reply it produces is queued rather than written here.
		c.serveRequest(f)
	case f.ID != "":
		pool, limit, what := c.slots, MaxInFlightSessionOps, "requests"
		if bulkOps[f.Type] {
			pool, limit, what = c.bulkSlots, MaxInFlightOps, "long operations (archives, clones, resizes)"
		}
		select {
		case pool <- struct{}{}:
			go func() {
				defer func() { <-pool }()
				c.serveRequest(f)
			}()
		default:
			// Refused, not queued and not dropped. Silent backpressure would
			// leave the caller waiting out a deadline it cannot see, and a drop
			// would leave it waiting forever; a typed refusal is something a
			// gateway can retry, place elsewhere, or show to a person.
			c.refuseBusy(f, limit, what)
		}
	case f.Type == TypeCancel:
		c.cancelInflight(f)
	default:
		c.mu.Lock()
		fn := c.events[f.Type]
		c.mu.Unlock()
		if fn != nil {
			fn(f.Body)
		}
	}
}

// refuseBusy answers one request that found its pool full, and says so at a
// bounded rate. The answer is per-request; the line is per-window, because the
// caller needs an error and the operator needs a fact.
func (c *Conn) refuseBusy(f *Frame, limit int, what string) {
	if refused, window, say := c.busy.record(c.now(), busyLogEvery); say {
		c.log.Warn("nodelink: refusing requests at the in-flight ceiling",
			"type", f.Type, "max", limit, "refused", refused, "over", window.Round(time.Millisecond))
	}
	c.reply(f, nil, &ctlops.Error{
		Kind: ctlops.KindCapacity,
		Op:   f.Type,
		Code: CodeNodeBusy,
		Msg: fmt.Sprintf("that machine is already running %d %s for this gateway; try again in a moment",
			limit, what),
	})
}

func (c *Conn) serveRequest(f *Frame) {
	c.mu.Lock()
	h := c.handlers[f.Type]
	base := c.base
	c.mu.Unlock()
	if h == nil {
		c.reply(f, nil, ctlops.Invalid(f.Type, "unknown_request",
			"this link does not speak %q", f.Type))
		return
	}

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if f.DeadlineMS > 0 {
		ctx, cancel = context.WithTimeout(base, time.Duration(f.DeadlineMS)*time.Millisecond)
	} else {
		ctx, cancel = context.WithCancel(base)
	}
	c.mu.Lock()
	c.inflight[f.ID] = cancel
	c.mu.Unlock()
	c.metrics.AddInFlight(c.node, c.transport, f.Type, 1)
	defer func() {
		c.mu.Lock()
		delete(c.inflight, f.ID)
		c.mu.Unlock()
		c.metrics.AddInFlight(c.node, c.transport, f.Type, -1)
		cancel()
	}()

	out, err := h(ctx, f.Body)
	c.reply(f, out, err)
}

func (c *Conn) recordDrop(kind string) {
	c.metrics.IncDropped(c.node, c.transport, kind)
}

// reply answers one request. A send failure means the link died or fell far
// enough behind that its queue is full while the work ran, which is not an error
// in the operation: the far side is not listening, the reply is discarded, its
// caller's deadline is the backstop, and — deliberately — nothing is rolled
// back.
func (c *Conn) reply(req *Frame, out any, err error) {
	f := &Frame{ID: req.ID, Type: TypeReply}
	if err != nil {
		f.Err = ctlops.ToWire(req.Type, err)
	} else {
		raw, merr := marshalBody(out)
		if merr != nil {
			f.Err = ctlops.ToWire(req.Type, merr)
		} else {
			f.Body = raw
		}
	}
	if serr := c.out.post(f); serr != nil {
		c.log.Debug("nodelink: reply not delivered", "type", req.Type, "err", serr)
	}
}

func (c *Conn) cancelInflight(f *Frame) {
	var msg Cancel
	if len(f.Body) > 0 {
		if err := json.Unmarshal(f.Body, &msg); err != nil {
			return
		}
	}
	c.mu.Lock()
	cancel := c.inflight[msg.ID]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// marshalBody keeps a nil body off the wire entirely rather than sending the
// four bytes "null", so an event with nothing to say is one field shorter.
func marshalBody(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return raw, nil
}
