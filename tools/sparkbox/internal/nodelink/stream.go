package nodelink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

// The two kinds of guest port a stream can name. There is no third: everything
// the gateway reaches inside a sandbox is either its sshd or a TCP port a user
// started something on.
const (
	StreamSSH = "ssh"
	StreamTCP = "tcp"
)

// A Resolver's refusals. They are sentinels rather than free-form errors
// because each maps to a different SSH rejection reason, and the gateway tells
// "the node has never heard of this sandbox" (its own bug — it authorized the
// request first) apart from "it is not running" (a user-visible state) apart
// from "nothing is listening in there" (the app's problem).
var (
	ErrUnknownSandbox = errors.New("unknown sandbox")
	ErrNotRunning     = errors.New("sandbox not running")
)

// StreamOpen is the extra data on a sandbox-stream channel. It is marshalled
// the way direct-tcpip's is (RFC 4254 §5.1), not as JSON, because that is the
// convention every SSH implementation already reads channel payloads with.
//
// It names a sandbox and never an address. The node re-resolves it from its own
// manager, so a stale gateway cache cannot talk a node into dialing an arbitrary
// node-local address, and "which port is sshd on" stays knowledge of the machine
// that booted the VM.
type StreamOpen struct {
	Sandbox string
	Kind    string
	Port    uint32
	// Nonce correlates the node's log line for this stream with the gateway's.
	Nonce string
}

// label renders a stream for a log line and for the synthetic net.Addr.
func (s StreamOpen) label() string {
	port := StreamSSH
	if s.Kind == StreamTCP {
		port = strconv.Itoa(int(s.Port))
	}
	return s.Sandbox + ":" + port
}

// Resolver answers "what address is this sandbox's <kind> port on THIS node".
// It returns ErrUnknownSandbox or ErrNotRunning for the two refusals the
// gateway needs to distinguish.
type Resolver func(sandbox, kind string, port int) (addr string, err error)

// OpenStream opens a sandbox-stream channel and adapts it to net.Conn. This is
// the gateway's side: it is the only end that opens data channels, which is
// what keeps a node's inbound surface to exactly one thing.
//
// An *xssh.OpenChannelError propagates untouched, because that typed carrier is
// how the edge tells a node's "no such sandbox" (503, the machine is confused)
// from a connection refused inside the guest (502, nothing is listening).
func OpenStream(conn xssh.Conn, req StreamOpen) (net.Conn, error) {
	ch, reqs, err := conn.OpenChannel(StreamChannel, xssh.Marshal(&req))
	if err != nil {
		return nil, err
	}
	go xssh.DiscardRequests(reqs)
	return newStreamConn(ch, req), nil
}

// ServeStreams drains chans forever, dispatching each open to its own
// goroutine. This is the node's side, and the goroutine is not an optimisation.
//
// x/crypto's mux hands an incoming channel over with a BLOCKING send into a
// 16-deep queue, from the mux loop goroutine itself. Doing any work inline here
// — a resolve, a dial, a lock — means that on the seventeenth concurrent open
// the mux stops reading the connection entirely: heartbeats, lifecycle replies
// and every other live stream freeze together. So this loop hands off and
// nothing else.
func ServeStreams(ctx context.Context, chans <-chan xssh.NewChannel, resolve Resolver, log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var live atomic.Int64
	for {
		select {
		case <-ctx.Done():
			return
		case nc, ok := <-chans:
			if !ok {
				return
			}
			go serveStream(ctx, nc, resolve, &live, log)
		}
	}
}

func serveStream(ctx context.Context, nc xssh.NewChannel, resolve Resolver, live *atomic.Int64, log *slog.Logger) {
	if nc.ChannelType() != StreamChannel {
		_ = nc.Reject(xssh.UnknownChannelType, "")
		return
	}
	var req StreamOpen
	if err := xssh.Unmarshal(nc.ExtraData(), &req); err != nil {
		_ = nc.Reject(xssh.ConnectionFailed, "bad stream request")
		return
	}
	if n := live.Add(1); n > MaxLiveStreams {
		live.Add(-1)
		_ = nc.Reject(xssh.ResourceShortage, "stream limit")
		return
	}
	defer live.Add(-1)

	log = log.With("sandbox", req.Sandbox, "kind", req.Kind, "nonce", req.Nonce)
	addr, err := resolve(req.Sandbox, req.Kind, int(req.Port))
	switch {
	case errors.Is(err, ErrUnknownSandbox):
		// The gateway authorized this before it dialed, so a sandbox this node
		// has never heard of means the two disagree about placement. That is a
		// control-plane fault, not a user error, and it is logged as one.
		log.Error("nodelink: stream for a sandbox this node does not have")
		_ = nc.Reject(xssh.Prohibited, "unknown sandbox")
		return
	case err != nil || addr == "":
		_ = nc.Reject(xssh.Prohibited, "sandbox not running")
		return
	}

	up, err := net.DialTimeout("tcp", addr, StreamDialTimeout)
	if err != nil {
		_ = nc.Reject(xssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		up.Close()
		return
	}
	go xssh.DiscardRequests(reqs)

	// Bounding the stream by the node's process context is right here and wrong
	// on the gateway: this ctx is the node's lifetime, not one request's, so
	// nothing pooled is torn down under a caller that still holds it.
	stop := context.AfterFunc(ctx, func() {
		ch.Close()
		up.Close()
	})
	defer stop()

	pipe(ch, up)
}

// pipe copies both ways and half-closes each direction as it ends, so a guest
// that reads until EOF sees one.
func pipe(ch xssh.Channel, up net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, up)
		_ = ch.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(up, ch)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			up.Close()
		}
	}()
	wg.Wait()
	ch.Close()
	up.Close()
}

// fleetAddr keeps log lines legible: a tunneled connection has no host address
// worth printing, and printing the SSH transport's would name the wrong machine.
type fleetAddr struct {
	network string
	addr    string
}

func (a fleetAddr) Network() string { return a.network }
func (a fleetAddr) String() string  { return a.addr }

// streamConn adapts an SSH channel to net.Conn.
//
// Its deadlines are close-on-expiry: a timer that shuts the channel, disarmed
// by the zero time. That is coarser than a *net.TCPConn, whose expired deadline
// is recoverable, and it is deliberately better than x/crypto's own tunneled
// conn, which refuses both setters outright. It is safe because the only
// deadline setters in this tree arm one around an SSH handshake and mean
// exactly "give up on this connection" — and because net/http's client
// transport, which is the one caller that pools these, sets none at all.
type streamConn struct {
	ch     xssh.Channel
	local  fleetAddr
	remote fleetAddr

	mu sync.Mutex
	rd *time.Timer
	wr *time.Timer

	// deadlineCalls counts every setter. It exists for the test that pins the
	// assumption above: the day net/http starts setting deadlines on pooled
	// connections, close-on-expiry stops being safe and this package has to
	// learn something new.
	deadlineCalls atomic.Int64
}

func newStreamConn(ch xssh.Channel, req StreamOpen) *streamConn {
	return &streamConn{
		ch:     ch,
		local:  fleetAddr{network: "sandbox", addr: "gateway"},
		remote: fleetAddr{network: "sandbox", addr: req.label()},
	}
}

func (c *streamConn) Read(p []byte) (int, error)  { return c.ch.Read(p) }
func (c *streamConn) Write(p []byte) (int, error) { return c.ch.Write(p) }
func (c *streamConn) LocalAddr() net.Addr         { return c.local }
func (c *streamConn) RemoteAddr() net.Addr        { return c.remote }

// CloseWrite half-closes, so an upstream reading to EOF gets one without the
// whole stream going away.
func (c *streamConn) CloseWrite() error { return c.ch.CloseWrite() }

func (c *streamConn) Close() error {
	c.mu.Lock()
	stopTimer(&c.rd)
	stopTimer(&c.wr)
	c.mu.Unlock()
	return c.ch.Close()
}

func (c *streamConn) SetDeadline(t time.Time) error {
	c.deadlineCalls.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.arm(&c.rd, t)
	c.arm(&c.wr, t)
	return nil
}

func (c *streamConn) SetReadDeadline(t time.Time) error {
	c.deadlineCalls.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.arm(&c.rd, t)
	return nil
}

func (c *streamConn) SetWriteDeadline(t time.Time) error {
	c.deadlineCalls.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.arm(&c.wr, t)
	return nil
}

// arm is called with c.mu held.
func (c *streamConn) arm(tp **time.Timer, t time.Time) {
	stopTimer(tp)
	if t.IsZero() {
		return
	}
	if d := time.Until(t); d > 0 {
		*tp = time.AfterFunc(d, func() { c.ch.Close() })
	} else {
		c.ch.Close()
	}
}

func stopTimer(tp **time.Timer) {
	if *tp != nil {
		(*tp).Stop()
		*tp = nil
	}
}
