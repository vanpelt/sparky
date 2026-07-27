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

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
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

const notRunningRefusal = "sandbox not running"

// IsNotRunning reports the one guest-dial failure for which resuming and
// retrying is valid. It accepts both the local resolver sentinel and the typed
// SSH channel rejection sent by a node. Other Prohibited answers, especially
// "unknown sandbox", are placement faults and must not be retried.
func IsNotRunning(err error) bool {
	if errors.Is(err, ErrNotRunning) {
		return true
	}
	var refused *xssh.OpenChannelError
	return errors.As(err, &refused) &&
		refused.Reason == xssh.Prohibited &&
		refused.Message == notRunningRefusal
}

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

// StreamResolver is the resolver every node runs: its own manager, and nothing
// else. It is the enforcement of the rule StreamOpen only states — the payload
// names a sandbox, so the address must come from the machine that booted it.
//
// The state check is here even though the gateway calls EnsureRunning before it
// dials, and it is not belt-and-braces. Between that call and this one a reaper
// can pause the box, and a paused sandbox's SSHAddr still names the port its
// sshd used to listen on. Under the mock driver that port is ephemeral and gets
// recycled; under firecracker the guest IP is reused by the next VM to take that
// tap index. Refusing on state rather than trusting a stale address is the
// difference between a stream that fails and a stream that quietly lands in
// somebody else's sandbox.
//
// A kind this node does not serve is refused as not-running rather than as
// unknown-sandbox, because the sandbox is fine: it is the request that names
// something this machine does not offer, and ErrUnknownSandbox is reserved for
// the one case that means the two machines disagree about placement (which the
// node logs at Error, see serveStream).
func StreamResolver(mgr Manager) Resolver {
	return func(sandbox, kind string, port int) (string, error) {
		b, ok := mgr.Get(sandbox)
		if !ok {
			return "", ErrUnknownSandbox
		}
		if b.State != vmm.StateRunning {
			return "", ErrNotRunning
		}
		switch kind {
		case StreamSSH:
			// Verbatim, because only this machine knows where its guests' sshd
			// listens: 172.30.<idx>.2:22 under firecracker, an ephemeral
			// 127.0.0.1 port under the mock driver.
			if b.SSHAddr == "" {
				return "", ErrNotRunning
			}
			return b.SSHAddr, nil
		case StreamTCP:
			if b.HostIP == "" {
				return "", ErrNotRunning
			}
			return net.JoinHostPort(b.HostIP, strconv.Itoa(port)), nil
		default:
			return "", ErrNotRunning
		}
	}
}

// OpenStream opens a sandbox-stream channel and adapts it to net.Conn. This is
// the gateway's side: it is the only end that opens data channels, which is
// what keeps a node's inbound surface to exactly one thing.
//
// An *xssh.OpenChannelError propagates untouched, because that typed carrier is
// how the edge tells a node's "no such sandbox" (503, the machine is confused)
// from a connection refused inside the guest (502, nothing is listening).
func OpenStream(conn xssh.Conn, req StreamOpen) (net.Conn, error) {
	c, err := openStream(conn, req)
	if err != nil {
		// Returned as an untyped nil rather than as (*streamConn)(nil): a typed
		// nil in a net.Conn is not == nil, and every caller here tests the error
		// first only by convention.
		return nil, err
	}
	return c, nil
}

// openStream is OpenStream with the concrete type kept, for the one caller that
// has to hold on to the stream after handing it out — Client, which tears its
// live streams down when the link under them dies.
func openStream(conn xssh.Conn, req StreamOpen) (*streamConn, error) {
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
	ServeStreamsWithMetrics(ctx, chans, resolve, log, nil, "")
}

// ServeStreamsWithMetrics is ServeStreams with optional process-local
// instrumentation. The original entry point remains source compatible.
func ServeStreamsWithMetrics(
	ctx context.Context,
	chans <-chan xssh.NewChannel,
	resolve Resolver,
	log *slog.Logger,
	metrics *fleetmetrics.Registry,
	node string,
) {
	ServeStreamsWithOptions(ctx, chans, resolve, log, metrics, node, "ssh", nil, nil)
}

// StreamLimiter is shared by every data lane belonging to one node process.
// Keeping it outside an individual accept loop is what makes MaxLiveStreams a
// node-wide ceiling when two or more SSH connections are serving channels.
type StreamLimiter struct {
	live atomic.Int64
	max  int64
}

func NewStreamLimiter(max int) *StreamLimiter {
	if max <= 0 {
		max = MaxLiveStreams
	}
	return &StreamLimiter{max: int64(max)}
}

func (l *StreamLimiter) acquire() bool {
	if l == nil {
		return true
	}
	if n := l.live.Add(1); n > l.max {
		l.live.Add(-1)
		return false
	}
	return true
}

func (l *StreamLimiter) release() {
	if l != nil {
		l.live.Add(-1)
	}
}

// ServeStreamsWithOptions is the node-side accept loop used by both combined
// links and dedicated data lanes. enabled, when non-nil, is checked for every
// open; the split control connection keeps draining and explicitly rejecting
// guest channels rather than leaving x/crypto's channel queue to stall its
// mux. limiter may be shared across any number of lanes.
func ServeStreamsWithOptions(
	ctx context.Context,
	chans <-chan xssh.NewChannel,
	resolve Resolver,
	log *slog.Logger,
	metrics *fleetmetrics.Registry,
	node string,
	transport string,
	limiter *StreamLimiter,
	enabled *atomic.Bool,
) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if limiter == nil {
		limiter = NewStreamLimiter(MaxLiveStreams)
	}
	if transport == "" {
		transport = "ssh"
	}
	for {
		select {
		case <-ctx.Done():
			return
		case nc, ok := <-chans:
			if !ok {
				return
			}
			if enabled != nil && !enabled.Load() {
				_ = nc.Reject(xssh.Prohibited, "guest streams use dedicated data lanes")
				continue
			}
			go serveStream(ctx, nc, resolve, limiter, log, metrics, node, transport)
		}
	}
}

func serveStream(
	ctx context.Context,
	nc xssh.NewChannel,
	resolve Resolver,
	limiter *StreamLimiter,
	log *slog.Logger,
	metrics *fleetmetrics.Registry,
	node string,
	transport string,
) {
	started := time.Now()
	kind, outcome := "unknown", "error"
	defer func() {
		metrics.ObserveStreamOpen(node, transport, kind, outcome, time.Since(started))
	}()
	if nc.ChannelType() != StreamChannel {
		outcome = "wrong_type"
		_ = nc.Reject(xssh.UnknownChannelType, "")
		return
	}
	var req StreamOpen
	if err := xssh.Unmarshal(nc.ExtraData(), &req); err != nil {
		outcome = "bad_request"
		_ = nc.Reject(xssh.ConnectionFailed, "bad stream request")
		return
	}
	kind = metricStreamKind(req.Kind)
	if !limiter.acquire() {
		outcome = "limit"
		_ = nc.Reject(xssh.ResourceShortage, "stream limit")
		return
	}
	defer limiter.release()

	log = log.With("sandbox", req.Sandbox, "kind", req.Kind, "nonce", req.Nonce)
	addr, err := resolve(req.Sandbox, req.Kind, int(req.Port))
	switch {
	case errors.Is(err, ErrUnknownSandbox):
		outcome = "unknown_sandbox"
		// The gateway authorized this before it dialed, so a sandbox this node
		// has never heard of means the two disagree about placement. That is a
		// control-plane fault, not a user error, and it is logged as one.
		log.Error("nodelink: stream for a sandbox this node does not have")
		_ = nc.Reject(xssh.Prohibited, "unknown sandbox")
		return
	case err != nil || addr == "":
		outcome = "not_running"
		_ = nc.Reject(xssh.Prohibited, notRunningRefusal)
		return
	}

	up, err := net.DialTimeout("tcp", addr, StreamDialTimeout)
	if err != nil {
		outcome = "dial_error"
		// The rejection message is the node's OWN WORDS about its own network,
		// so it is the one string on this path that has to be composed rather
		// than relayed. net's dial error names the address it tried —
		// 172.30.<idx>.2:8000 under firecracker, 127.0.0.1:<ephemeral> under
		// the mock driver — and that address is fleet-internal: it means
		// nothing on the gateway (every machine mints the same guest IPs) and
		// it travels from here into a public error page and a user's terminal.
		// The gateway needs the CLASS of failure, which the rejection reason
		// already carries; the address stays in this machine's log.
		log.Info("nodelink: could not reach the guest", "addr", addr, "err", err)
		_ = nc.Reject(xssh.ConnectionFailed, refusalWords(err))
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		up.Close()
		outcome = "accept_error"
		return
	}
	go xssh.DiscardRequests(reqs)
	outcome = "ok"
	metrics.AddLiveStreams(node, transport, kind, 1)
	defer metrics.AddLiveStreams(node, transport, kind, -1)

	// Bounding the stream by the node's process context is right here and wrong
	// on the gateway: this ctx is the node's lifetime, not one request's, so
	// nothing pooled is torn down under a caller that still holds it.
	stop := context.AfterFunc(ctx, func() {
		ch.Close()
		up.Close()
	})
	defer stop()

	pipeWithTransportMetrics(ch, up, metrics, node, transport, kind)
}

// refusalWords renders a failed guest dial for the gateway with no address in
// it. Two phrasings, because they are the two things an owner would do
// something different about: a refusal means the port is not being served, and
// a timeout means something in the guest is wedged or firewalling.
//
// It deliberately does not fall back to err.Error() for the "other" case. The
// whole point is that nothing composed by net reaches the wire, and an
// unforeseen error class is exactly where an address would slip through.
func refusalWords(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out connecting to that port in the sandbox"
	}
	return "nothing accepted a connection on that port in the sandbox"
}

// pipe copies both ways and half-closes each direction as it ends, so a guest
// that reads until EOF sees one.
func pipe(ch xssh.Channel, up net.Conn) {
	pipeWithMetrics(ch, up, nil, "", "unknown")
}

func pipeWithMetrics(ch xssh.Channel, up net.Conn, metrics *fleetmetrics.Registry, node, kind string) {
	pipeWithTransportMetrics(ch, up, metrics, node, "ssh", kind)
}

func pipeWithTransportMetrics(
	ch xssh.Channel,
	up net.Conn,
	metrics *fleetmetrics.Registry,
	node, transport, kind string,
) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(metricWriter{Writer: ch, record: func(n int) {
			metrics.AddStreamBytes(node, transport, kind, "from_guest", n)
		}}, up)
		_ = ch.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(metricWriter{Writer: up, record: func(n int) {
			metrics.AddStreamBytes(node, transport, kind, "to_guest", n)
		}}, ch)
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

type metricWriter struct {
	io.Writer
	record func(int)
}

func (w metricWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		w.record(n)
	}
	return n, err
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
	kind   string

	metrics                     *fleetmetrics.Registry
	metricNode, metricTransport string
	metricKind                  string

	mu sync.Mutex
	rd *time.Timer
	wr *time.Timer
	// reason is why this stream ended, when it ended for a reason worse than
	// the guest hanging up. See fail.
	reason error
	// released runs once, when this stream stops being live, so whoever is
	// holding a registry of streams can forget it. It is set before the conn is
	// published to anyone and never afterwards.
	released func()
	gone     bool

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
		kind:   req.Kind,
	}
}

func (c *streamConn) setMetrics(m *fleetmetrics.Registry, node, transport, kind string) {
	c.metrics, c.metricNode, c.metricTransport, c.metricKind = m, node, transport, kind
}

// Read and Write substitute the recorded reason for whatever x/crypto says once
// the link has been torn out from under this stream.
//
// Without this a link that dies mid-transfer is indistinguishable from a guest
// that finished talking: x/crypto closes every channel on a dead connection,
// and a closed channel reads io.EOF and writes io.EOF. A truncated HTTP
// response body that ends in a clean EOF is the worst failure this package can
// produce — the proxy relays a short 200, the browser renders half a page, and
// nothing anywhere logs a thing. It is also invariant 2 (an io.EOF may not
// leak) applied to the data plane: fleet.Unreachable cannot wrap this, because
// by the time it happens the dial has long since returned.
func (c *streamConn) Read(p []byte) (int, error) {
	n, err := c.ch.Read(p)
	c.metrics.AddStreamBytes(c.metricNode, c.metricTransport, c.metricKind, "from_guest", n)
	if err != nil {
		if r := c.failure("read"); r != nil {
			return n, r
		}
	}
	return n, err
}

func (c *streamConn) Write(p []byte) (int, error) {
	n, err := c.ch.Write(p)
	c.metrics.AddStreamBytes(c.metricNode, c.metricTransport, c.metricKind, "to_guest", n)
	if err != nil {
		if r := c.failure("write"); r != nil {
			return n, r
		}
	}
	return n, err
}

func (c *streamConn) LocalAddr() net.Addr  { return c.local }
func (c *streamConn) RemoteAddr() net.Addr { return c.remote }

// failure renders the recorded reason as a *net.OpError, which is the shape
// every caller here already handles: net/http's transport, x/crypto's client
// handshake and io.Copy all treat it as an ordinary network failure, and
// errors.Is reaches the sentinel underneath for anything that wants to branch.
func (c *streamConn) failure(op string) error {
	c.mu.Lock()
	reason := c.reason
	c.mu.Unlock()
	if reason == nil {
		return nil
	}
	return &net.OpError{Op: op, Net: c.remote.Network(), Addr: c.remote, Err: reason}
}

// fail records why this stream is ending and then ends it. The first reason
// wins, for the same reason Conn.Fail keeps the first one: the close that
// follows would otherwise overwrite the diagnosis with its own side effect.
func (c *streamConn) fail(reason error) {
	c.mu.Lock()
	if c.reason == nil {
		c.reason = reason
	}
	c.mu.Unlock()
	_ = c.Close()
}

// CloseWrite half-closes, so an upstream reading to EOF gets one without the
// whole stream going away.
func (c *streamConn) CloseWrite() error { return c.ch.CloseWrite() }

func (c *streamConn) Close() error {
	c.mu.Lock()
	stopTimer(&c.rd)
	stopTimer(&c.wr)
	release := c.released
	if c.gone {
		release = nil
	}
	c.gone = true
	c.mu.Unlock()
	if release != nil {
		release()
	}
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
