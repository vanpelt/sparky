package fleet

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// hostDialer is the fall-through. An address that is not a synthetic fleet
// name is on the host network, which is where every dial in a single-box
// deployment goes and always went. The timeouts mirror the proxy's historical
// upstream dialer, since that is by far the busiest caller.
var hostDialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

// DialContext is the fleet dialer, shaped like net.Dialer.DialContext so it
// drops into http.Transport, the SSH gateway and the secret syncer unchanged.
// A "<sandbox>.<node>.sandbox.invalid" host routes through that node; anything
// else is dialed directly, which is what makes installing this on a single-box
// deployment a no-op.
//
// It honours ctx for opening the stream and installs NO close-bound on the
// returned conn. That asymmetry is load-bearing: http.Transport pools a
// connection past the request that dialed it, so a context.AfterFunc(reqCtx,
// conn.Close) here produces intermittent resets under load with no error and
// no log line. Callers that do not pool (the SSH gateway, the PTY bridge, the
// secret syncer) already install their own bound.
func (f *Fleet) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f.dialContext(ctx, network, addr, true)
}

// DialContextNoResume routes like DialContext but never turns a stale
// not-running answer into EnsureReady. It is for background best-effort work
// such as environment synchronization, whose contract explicitly forbids
// waking an idle sandbox.
func (f *Fleet) DialContextNoResume(ctx context.Context, network, addr string) (net.Conn, error) {
	return f.dialContext(ctx, network, addr, false)
}

func (f *Fleet) dialContext(ctx context.Context, network, addr string, resume bool) (net.Conn, error) {
	h, port, err := net.SplitHostPort(addr)
	if err != nil {
		return hostDialer.DialContext(ctx, network, addr)
	}
	sandbox, node, ok := SplitHost(h)
	if !ok {
		return hostDialer.DialContext(ctx, network, addr)
	}
	n, err := f.dialTarget(sandbox, node)
	if err != nil {
		return nil, err
	}
	// The stream names a sandbox and a kind, never an address: the node
	// re-resolves both from its own record, so a cache stale enough to name a
	// sandbox that has moved or paused cannot talk it into dialing something
	// else.
	if port == SSHPort {
		return f.dialGuest(ctx, n, sandbox, nodelink.StreamSSH, 0, resume)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("fleet: %q is not a port on sandbox %q", port, sandbox)
	}
	return f.dialGuest(ctx, n, sandbox, nodelink.StreamTCP, p, resume)
}

// dialGuest retries exactly one stale-state refusal. The node is the authority
// on whether its guest is still running; a cached running record can lag a
// reaper pause, so that typed answer singleflights one resume and then gets one
// fresh dial. Every other failure keeps its existing taxonomy and is returned
// untouched.
func (f *Fleet) dialGuest(ctx context.Context, n Node, sandbox, kind string, port int, resume bool) (net.Conn, error) {
	c, err := n.DialGuest(ctx, sandbox, kind, port)
	if err == nil {
		return f.track(c, nil)
	}
	if !nodelink.IsNotRunning(err) || !resume {
		return nil, err
	}
	if f.metrics != nil {
		f.metrics.IncEnsureReady(n.Name(), "retry")
	}
	if _, readyErr := f.EnsureReady(ctx, sandbox); readyErr != nil {
		return nil, readyErr
	}
	// Re-route after the lifecycle operation rather than retaining a node
	// pointer across it. A reconnect may have replaced the adapter generation.
	n, routeErr := f.route("dial", sandbox)
	if routeErr != nil {
		return nil, routeErr
	}
	return f.track(n.DialGuest(ctx, sandbox, kind, port))
}

// track registers a tunneled conn so Fleet.Close can reach it, and is a
// pass-through for the error.
//
// Only the tunneled branch is tracked. An address that fell through to
// hostDialer above is handed back exactly as net.Dialer minted it, because a
// single-box deployment's data path must stay byte-identical down to the
// concrete type a caller might type-assert on.
//
// This is a second registry on top of the one nodelink.Client keeps, and the
// two answer different questions. The link's registry ends the streams that
// die WITH a machine, and says why. This one ends the streams that outlive the
// FLEET — a gateway shutting down, a test tearing its rig apart — including
// those over links that are perfectly healthy and would happily hold an idle
// pooled connection open forever.
func (f *Fleet) track(c net.Conn, err error) (net.Conn, error) {
	if err != nil {
		return nil, err
	}
	t := &tracked{Conn: c, f: f}
	f.smu.Lock()
	closed := f.streamsClosed
	if !closed {
		f.streams[t] = struct{}{}
	}
	f.smu.Unlock()
	if closed {
		// Dialed through a fleet that is already shutting down. Handing the
		// conn back would leak it past Close; refusing it is the honest answer
		// and the caller sees the same thing it would a moment later anyway.
		c.Close()
		return nil, net.ErrClosed
	}
	return t, nil
}

func (f *Fleet) untrack(t *tracked) {
	f.smu.Lock()
	delete(f.streams, t)
	f.smu.Unlock()
}

// closeStreams ends every tunneled conn this fleet handed out.
func (f *Fleet) closeStreams() {
	f.smu.Lock()
	f.streamsClosed = true
	live := make([]*tracked, 0, len(f.streams))
	for t := range f.streams {
		live = append(live, t)
	}
	clear(f.streams)
	f.smu.Unlock()
	for _, t := range live {
		t.Conn.Close()
	}
}

// tracked is a tunneled conn the fleet can still reach after handing it out.
//
// CloseWrite is forwarded rather than inherited, because embedding net.Conn
// would hide it: envsync half-closes its stdin to signal end-of-script, and
// nodelink's own pipe half-closes each direction as it ends. A wrapper that
// silently dropped CloseWrite would leave a guest reading to EOF that never
// comes — a hang, not an error.
type tracked struct {
	net.Conn
	f    *Fleet
	once sync.Once
}

func (t *tracked) Close() error {
	t.once.Do(func() { t.f.untrack(t) })
	return t.Conn.Close()
}

func (t *tracked) CloseWrite() error {
	cw, ok := t.Conn.(interface{ CloseWrite() error })
	if !ok {
		return fmt.Errorf("fleet: %T cannot half-close", t.Conn)
	}
	return cw.CloseWrite()
}

// dialTarget resolves the machine a synthetic address names. An address for a
// node that is not linked is a node outage rather than a bad address: the
// record it came from is real, the machine is just not answering.
func (f *Fleet) dialTarget(sandbox, node string) (Node, error) {
	if node == f.localName {
		return f.local, nil
	}
	n, ok := f.nodeByName(node)
	if !ok || !n.Online() {
		return nil, Unreachable("dial", sandbox, node)
	}
	return n, nil
}
