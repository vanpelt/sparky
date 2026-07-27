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
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
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

// VitalsReader reads a sandbox's live CPU, memory and network counters,
// wherever it runs. Satisfied by *fleet.Fleet, which routes the read to the
// machine holding the VM, and by *host.Manager for a build with no fleet.
//
// It is here, next to the probe, because the three surfaces that draw a meter —
// both consoles and the browser terminal — used to ask the LOCAL manager and so
// drew nothing at all for a sandbox on another machine. That was correct (a
// balloon and a VMM process can only be asked of the host running them) and
// permanently blank, which is the same policy problem this file already exists
// to keep from being stated three times.
type VitalsReader interface {
	Vitals(ctx context.Context, name string) (host.Vitals, error)
}

// Vitals reads one sandbox's counters under the budget its placement deserves,
// and answers the empty reading for everything that is not an answer.
//
// A sandbox on this machine is three concurrent reads of /proc, sysfs and a VMM
// socket — the 300ms every local probe gets. One on another machine is a round
// trip to that machine before its balloon is even touched, which is what the
// tunneled budget is for. Giving every reading the remote budget lets one wedged
// local VMM stall a dashboard for two seconds; giving every reading the local
// one times out exactly the remote sandboxes this routing exists to reach.
//
// Not running, no reader wired, and a machine that is not answering all produce
// the same empty reading, because every surface renders them identically: no
// meter. The error comes back only so a caller can log it — nothing should
// branch on it, and a caller that ignores it entirely is correct.
func (p *Probe) Vitals(ctx context.Context, r VitalsReader, b *host.Sandbox) (host.Vitals, error) {
	if r == nil || b == nil || b.State != vmm.StateRunning {
		return host.Vitals{}, nil
	}
	budget := ProbeTimeout
	if p.Remote(b) {
		budget = TunneledProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	v, err := r.Vitals(ctx, b.Name)
	if err != nil {
		return host.Vitals{}, err
	}
	return v, nil
}

// Public copies a sandbox record for the wire with its addresses dropped. The
// pages have never shown them, and once a console is pointed at the fleet they
// are the synthetic <sandbox>.<node>.sandbox.invalid names the gateway mints
// for sandboxes on other machines: guaranteed not to resolve, meaningless to a
// browser, and an invitation for something to dial them. ctlops.info drops the
// same three fields for the same reason. nil in, nil out — the callers hand it
// whatever Get returned.
func Public(b *host.Sandbox) *host.Sandbox { return b.Public() }
