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
)

// ErrLinkClosed is what pending and future requests report once Close has been
// called. A caller that wants a better sentence than this — "that machine is
// not answering", naming the node — calls Fail with one before closing, which
// is exactly what the gateway half does.
var ErrLinkClosed = errors.New("node link closed")

// Handler answers one request type. Its ctx is derived from the Conn's base
// context and the frame's deadline, never from the link.
type Handler func(ctx context.Context, body json.RawMessage) (any, error)

// Conn is one framed control channel: a reader goroutine, a table of waiters
// keyed by request ID, and a mutex-guarded encoder. It is transport-agnostic
// on purpose — the SSH session it rides in production is nothing more than an
// io.Reader and an io.Writer to it.
type Conn struct {
	enc      *encoder
	dec      *decoder
	log      *slog.Logger
	idPrefix string

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

// NewConn wraps one direction each of a duplex transport. idPrefix is the
// single character that makes the two sides' request IDs disjoint — "g" on the
// gateway, "n" on the node — so neither side ever has to consider whether an
// ID it is looking up might be the other's.
func NewConn(r io.Reader, w io.Writer, idPrefix string, log *slog.Logger) *Conn {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Conn{
		enc:      newEncoder(w),
		dec:      newDecoder(r),
		log:      log,
		idPrefix: idPrefix,
		pending:  map[string]chan *Frame{},
		inflight: map[string]context.CancelFunc{},
		handlers: map[string]Handler{},
		events:   map[string]func(json.RawMessage){},
		base:     context.Background(),
		done:     make(chan struct{}),
	}
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
func (c *Conn) Request(ctx context.Context, typ string, body, out any) error {
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
	defer func() {
		c.mu.Lock()
		delete(c.pending, f.ID)
		c.mu.Unlock()
	}()

	if err := c.enc.encode(f); err != nil {
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
func (c *Conn) Event(typ string, body any) error {
	raw, err := marshalBody(body)
	if err != nil {
		return err
	}
	return c.enc.encode(&Frame{Type: typ, Body: raw})
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
// already parked on a buffered channel — while a request gets its own goroutine
// so a slow operation cannot stall the frames behind it.
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
			return
		}
		// Non-blocking: the waiter's channel holds one, and a peer that replied
		// twice to the same ID must not be able to park the reader goroutine.
		select {
		case ch <- f:
		default:
			c.log.Debug("nodelink: duplicate reply dropped", "id", f.ID)
		}
	case f.ID != "":
		go c.serveRequest(f)
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
	defer func() {
		c.mu.Lock()
		delete(c.inflight, f.ID)
		c.mu.Unlock()
		cancel()
	}()

	out, err := h(ctx, f.Body)
	c.reply(f, out, err)
}

// reply answers one request. A send failure means the link died while the work
// ran, which is not an error in the operation: the far side is gone, the reply
// is discarded, and — deliberately — nothing is rolled back.
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
	if serr := c.enc.encode(f); serr != nil {
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
