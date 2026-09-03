package fleet

import (
	"context"
	"net"
	"runtime"
	"strconv"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// localNode presents the gateway's own manager as a Node. Every method is a
// straight pass-through, and the three context-free manager methods (Touch,
// RecordKey, SetPinned) are called synchronously and answer nil: the whole
// point is that a single-box deployment executes exactly the calls it always
// did, in the same order, on the same goroutine.
type localNode struct {
	name      string
	mgr       *host.Manager
	net       NetControl
	startedAt time.Time
}

// NetControl is the local machine's egress gateway, narrowed to what a Node
// asks of it. *netpush.Syncer satisfies it; it is restated rather than imported
// from nodelink because this is the LOCAL half and has no business depending on
// the link package for a shape it shares only by coincidence of both talking to
// the same daemon.
type NetControl interface {
	Enabled() bool
	Apply(ctx context.Context, allow map[string][]string) error
	Usage(ctx context.Context, owner string) (map[string]netpush.VMUsage, error)
}

// Local adapts a *host.Manager to Node. name is what the fleet calls this
// machine; empty falls back to the manager's own node name, which is the
// string it already stamps on every record it creates. That fallback cannot be
// empty either — host.NewManager coerces its own node name to "local" — so the
// literal lives there and nowhere else.
//
// net is this machine's own sluice, or nil where there is none. It is passed at
// construction rather than reached through the Fleet because the gateway is
// just another machine to the egress plane: its VMs are metered by the daemon
// in front of THEIR taps, exactly as a node's are, and the only thing that
// makes it special is that no link is involved.
func Local(name string, mgr *host.Manager, net NetControl) Node {
	if name == "" {
		name = mgr.NodeName()
	}
	return &localNode{name: name, mgr: mgr, net: net, startedAt: time.Now()}
}

func (l *localNode) Name() string { return l.name }

// Online is true by definition: this is the process asking.
func (l *localNode) Online() bool { return true }

// LastSeen is zero because this machine cannot go quiet: a last-seen time is
// what an operator reads to tell how long ago a link stopped answering, and
// this one is the process rendering the answer.
func (l *localNode) LastSeen() time.Time { return time.Time{} }

// Hangup and Revoke do nothing: there is no link to this machine to end, and
// dropping it from the fleet's map is the whole of what could be done to it.
func (l *localNode) Hangup(string, string) {}

func (l *localNode) Revoke(string, error) {}

// Facts fills in what a manager can answer about its own machine. Version,
// Driver, Images and GuestSubnet stay empty because the manager genuinely does
// not know them — they are flags and directory listings the process that built
// it holds — and an invented value here would be reported to an operator as
// fact. Protocol is this build's, which is a fact about this process and not a
// guess about another machine.
func (l *localNode) Facts() Facts {
	c := l.mgr.Capacity()
	return Facts{
		Protocol:  nodelink.Protocol,
		Node:      l.name,
		Arch:      c.Arch,
		OS:        runtime.GOOS,
		Release:   c.Release,
		Archiving: l.mgr.ArchivingEnabled(),
		Snapshots: l.mgr.Snapshotter(),
		// Reported for the same reason a node reports it, and it is a fact this
		// process genuinely holds rather than one it would be guessing at: a
		// syncer was either wired to a socket at startup or it was not.
		Sluice:    l.net != nil && l.net.Enabled(),
		StartedAt: l.startedAt,
	}
}

func (l *localNode) Box(name string) (*host.Sandbox, bool) { return l.mgr.Get(name) }

func (l *localNode) Boxes() []*host.Sandbox { return l.mgr.List() }

func (l *localNode) Templates() []*host.Snapshot { return l.mgr.AllSnapshots() }

func (l *localNode) Capacity() host.NodeCapacity { return l.mgr.Capacity() }

func (l *localNode) Vitals(ctx context.Context, name string) (host.Vitals, error) {
	return l.mgr.Vitals(ctx, name)
}

func (l *localNode) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	return l.mgr.Create(ctx, name, owner, image, vcpus, memMB)
}

func (l *localNode) EnsureReady(ctx context.Context, name string) (*host.Sandbox, error) {
	return l.mgr.EnsureReady(ctx, name)
}

func (l *localNode) Pause(ctx context.Context, name string) error { return l.mgr.Pause(ctx, name) }

func (l *localNode) Archive(ctx context.Context, name string) error { return l.mgr.Archive(ctx, name) }

func (l *localNode) Resize(ctx context.Context, name string, sizeMB int64) error {
	return l.mgr.Resize(ctx, name, sizeMB)
}

func (l *localNode) Reboot(ctx context.Context, name string) error { return l.mgr.Reboot(ctx, name) }

func (l *localNode) SetTurbo(ctx context.Context, name string, on bool) error {
	return l.mgr.SetTurbo(ctx, name, on)
}

func (l *localNode) Rename(ctx context.Context, oldName, newName, owner string) error {
	return l.mgr.Rename(ctx, oldName, newName, owner)
}

func (l *localNode) Destroy(ctx context.Context, name string) error { return l.mgr.Destroy(ctx, name) }

func (l *localNode) SetPinned(_ context.Context, name string, pinned bool) error {
	return l.mgr.SetPinned(name, pinned)
}

func (l *localNode) ResyncEnv(ctx context.Context, name string) error {
	l.mgr.ResyncEnv(ctx, name)
	return nil
}

func (l *localNode) AwaitEnv(ctx context.Context, name string) error {
	return l.mgr.AwaitEnv(ctx, name)
}

func (l *localNode) MarkActive(_ context.Context, name string) error {
	l.mgr.MarkActive(name)
	return nil
}

func (l *localNode) RecordKey(_ context.Context, name, fp string) error {
	l.mgr.RecordKey(name, fp)
	return nil
}

func (l *localNode) Snapshotter(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error) {
	return l.mgr.Snapshot(ctx, box, snapName, owner)
}

func (l *localNode) DeleteSnapshot(ctx context.Context, snapName, owner string) error {
	return l.mgr.DeleteSnapshot(ctx, snapName, owner)
}

func (l *localNode) Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error) {
	return l.mgr.Fork(ctx, snapName, newName, owner, vcpus, memMB)
}

// DialGuest resolves the address from this machine's own record and dials it
// over the host network — the same net.Dialer the proxy has always used, so a
// single-box deployment's data path is unchanged. Resolving here rather than
// from a caller-supplied address is what the remote implementation must do
// anyway (a node never dials an address the gateway hands it), and keeping the
// two halves the same shape keeps them honest about each other.
//
// The one difference from the node half — nodelink.StreamResolver checks State
// and this does not — is deliberate and costs nothing. The manager clears
// SSHAddr, HostIP and GuestV6 on every transition out of running (Pause, and
// the boot-time downgrade), so a record that is not running has no address and
// the empty-address branch below already refuses it. The node half checks state
// as well because it is answering ANOTHER machine's request, and §2.6's reject
// table wants those two refusals to arrive as different messages.
func (l *localNode) DialGuest(ctx context.Context, sandbox, kind string, port int) (net.Conn, error) {
	b, ok := l.mgr.Get(sandbox)
	if !ok {
		return nil, nodelink.ErrUnknownSandbox
	}
	addr := b.SSHAddr
	if kind == nodelink.StreamTCP {
		if b.HostIP == "" {
			return nil, nodelink.ErrNotRunning
		}
		addr = net.JoinHostPort(b.HostIP, strconv.Itoa(port))
	}
	if addr == "" {
		return nil, nodelink.ErrNotRunning
	}
	return hostDialer.DialContext(ctx, "tcp", addr)
}

// NetPolicy and NetUsage go to this machine's own sluice with no link in the
// middle. They refuse in the SAME sentence a node does, built by the same
// function, because "this machine runs no egress gateway" is a fact about a
// machine and the gateway is one — an owner reading an empty bandwidth panel
// should not be told a different story depending on where their VM landed.
func (l *localNode) NetPolicy(ctx context.Context, allow map[string][]string) error {
	if l.net == nil || !l.net.Enabled() {
		return nodelink.NoSluice(l.name)
	}
	return l.net.Apply(ctx, allow)
}

func (l *localNode) NetUsage(ctx context.Context) (map[string]netpush.VMUsage, error) {
	if l.net == nil || !l.net.Enabled() {
		return nil, nodelink.NoSluice(l.name)
	}
	// Unfiltered, like the node half: the caller holds the ledger that decides
	// who may see what, and a second owner check here would be a staler copy of
	// a decision that gates one user's view of another's VM.
	return l.net.Usage(ctx, "")
}

func (l *localNode) NetDenials(ctx context.Context, sandbox string, reset bool) (netpush.DenialCapture, error) {
	if l.net == nil || !l.net.Enabled() {
		return netpush.DenialCapture{}, nodelink.NoSluice(l.name)
	}
	capture, ok := l.net.(interface {
		StartDenialCapture(context.Context, string) error
		FinishDenialCapture(context.Context, string) (netpush.DenialCapture, error)
	})
	if !ok {
		return netpush.DenialCapture{}, nodelink.NoSluice(l.name)
	}
	if reset {
		return netpush.DenialCapture{}, capture.StartDenialCapture(ctx, sandbox)
	}
	return capture.FinishDenialCapture(ctx, sandbox)
}
