package webui

// The console-server half of what the two dashboards share: how long a port
// probe may take, how to tell a sandbox on this machine from one elsewhere,
// and which fields must never reach a browser. It lives beside the design
// system because the alternative is stating the same policy twice — widen the
// tunneled budget for the fleet and one console keeps timing remote sandboxes
// out at 300ms; add a field to host.Sandbox and one console keeps leaking it.

import (
	"context"
	"net"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

const (
	// ProbeTimeout bounds the per-route TCP dial that checks whether anything
	// is listening on a forwarded port, and the per-sandbox stat reads beside
	// it. Guest IPs are on a local bridge, so a live listener answers in
	// microseconds; anything slower is effectively down.
	ProbeTimeout = 300 * time.Millisecond
	// TunneledProbeTimeout is the same ceiling for a sandbox on another
	// machine, where the question costs a round trip to that machine before its
	// bridge is even touched. It applies per sandbox and only to the remote
	// ones: a box with no fleet must not pay a 2s worst case to learn what it
	// has always learned in 300ms.
	TunneledProbeTimeout = 2 * time.Second
)

// Dialer is net.Dialer.DialContext's shape — see Probe.Dial.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Probe is a console's view of the guest network: which machine it is serving
// from, and how a sandbox's forwarded port is reached from here.
type Probe struct {
	// Node is this machine's name, which is how a local row is told from a
	// remote one.
	Node string
	// Dial routes the listening probe through the fleet instead of dialing the
	// guest's address on the host network. It matters for more than
	// reachability: every machine in a fleet hands out the same 172.30.x.y
	// guest addresses, so a gateway probing a remote sandbox's address
	// directly would answer with whatever its OWN sandbox at that address is
	// doing — a green badge for a port nobody is listening on. nil dials the
	// host network, which is every deployment that has one machine.
	Dial Dialer
}

// Remote reports whether a sandbox lives on another machine. A record with no
// node at all is one this machine's manager wrote before nodes were named, so
// it is ours.
func (p *Probe) Remote(b *host.Sandbox) bool { return b.Node != "" && b.Node != p.Node }

// Listening reports whether anything accepts a connection at addr. A refusal
// and a timeout are the same answer here — the badge says "something is
// serving this port", and nothing else about the failure reaches the page.
func (p *Probe) Listening(ctx context.Context, addr string, remote bool) bool {
	budget := ProbeTimeout
	if remote {
		budget = TunneledProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	conn.Close() //nolint:errcheck
	return true
}

// Public copies a sandbox record for the wire with its addresses dropped. The
// pages have never shown them, and once a console is pointed at the fleet they
// are the synthetic <sandbox>.<node>.sandbox.invalid names the gateway mints
// for sandboxes on other machines: guaranteed not to resolve, meaningless to a
// browser, and an invitation for something to dial them. ctlops.info drops the
// same three fields for the same reason. nil in, nil out — the callers hand it
// whatever Get returned.
func Public(b *host.Sandbox) *host.Sandbox { return b.Public() }
