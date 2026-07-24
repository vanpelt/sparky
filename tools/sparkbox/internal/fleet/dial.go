package fleet

import (
	"context"
	"fmt"
	"net"
	"strconv"
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
		return n.DialGuest(ctx, sandbox, nodelink.StreamSSH, 0)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("fleet: %q is not a port on sandbox %q", port, sandbox)
	}
	return n.DialGuest(ctx, sandbox, nodelink.StreamTCP, p)
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
