package fleet

import (
	"context"
	"net"
	"runtime"
	"strconv"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
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
	startedAt time.Time
}

// Local adapts a *host.Manager to Node. name is what the fleet calls this
// machine; empty falls back to the manager's own node name, which is the
// string it already stamps on every record it creates. That fallback cannot be
// empty either — host.NewManager coerces its own node name to "local" — so the
// literal lives there and nowhere else.
func Local(name string, mgr *host.Manager) Node {
	if name == "" {
		name = mgr.NodeName()
	}
	return &localNode{name: name, mgr: mgr, startedAt: time.Now()}
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
		StartedAt: l.startedAt,
	}
}

func (l *localNode) Box(name string) (*host.Sandbox, bool) { return l.mgr.Get(name) }

func (l *localNode) Boxes() []*host.Sandbox { return l.mgr.List() }

func (l *localNode) Templates() []*host.Snapshot { return l.mgr.AllSnapshots() }

func (l *localNode) Capacity() host.NodeCapacity { return l.mgr.Capacity() }

func (l *localNode) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	return l.mgr.Create(ctx, name, owner, image, vcpus, memMB)
}

func (l *localNode) EnsureRunning(ctx context.Context, name string) (*host.Sandbox, error) {
	return l.mgr.EnsureRunning(ctx, name)
}

func (l *localNode) Pause(ctx context.Context, name string) error { return l.mgr.Pause(ctx, name) }

func (l *localNode) Archive(ctx context.Context, name string) error { return l.mgr.Archive(ctx, name) }

func (l *localNode) Resize(ctx context.Context, name string, sizeMB int64) error {
	return l.mgr.Resize(ctx, name, sizeMB)
}

func (l *localNode) Reboot(ctx context.Context, name string) error { return l.mgr.Reboot(ctx, name) }

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

func (l *localNode) Touch(_ context.Context, name string) error {
	l.mgr.Touch(name)
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
