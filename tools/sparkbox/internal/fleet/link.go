package fleet

// Joining a machine to the fleet: what happens after the SSH door has resolved
// a key to a roster row and before anything is placed on it.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

var (
	_ Node               = (*linkNode)(nil)
	_ ctlops.NodeEvicter = (*Fleet)(nil)
)

// ServeLink owns one node's link for its lifetime and returns when it ends.
// opts.Node is the authenticated roster name; the name in the hello is
// advisory, and nothing here consults it.
//
// It returns nil when the link simply ended: a node reconnecting after a
// gateway restart is routine, and the caller — an SSH session handler — must
// not report it as a failure.
func (f *Fleet) ServeLink(ctx context.Context, opts nodelink.ServerOptions) error {
	if opts.Node == f.localName {
		// The gateway's own name. Admitting it would make "which machine holds
		// this" ambiguous for every row already placed here, and the router
		// resolves the local name to the local manager before it ever looks at
		// the linked machines, so the link could never carry anything anyway.
		err := nodelink.Refusal(nodelink.CodeNodeNameTaken,
			"%q is this gateway's own node name — give the node a different --node-name.", opts.Node)
		_ = nodelink.Refuse(opts.Session, opts.Greeting, err)
		return err
	}
	if opts.Log == nil {
		opts.Log = f.log
	}
	opts.Hooks = nodelink.Hooks{OnInventory: f.linkInventory}

	client, wait, err := nodelink.Serve(ctx, opts)
	if err != nil {
		return err
	}
	defer client.Close()

	n := &linkNode{client: client}
	detach := f.linkUp(n)
	defer detach()

	facts := n.Facts()
	f.log.Info("node linked", "node", n.Name(), "arch", facts.Arch, "os", facts.OS,
		"release", facts.Release, "driver", facts.Driver)
	err = wait()
	f.log.Info("node link ended", "node", n.Name(), "err", err)
	return err
}

// linkInventory is what the gateway makes of a node's picture of itself.
//
// Nothing durable, yet: the ledger's rows are gateway-authored, and until the
// gateway can place a sandbox on another machine there is no row for a node's
// inventory to agree or disagree with. Adopting names out of it now would
// invent placements nobody asked for. The link's own cache still records it,
// which is what makes the node observable.
func (f *Fleet) linkInventory(node string, inv nodelink.InventoryMsg) nodelink.InventoryAck {
	f.log.Debug("node inventory", "node", node,
		"sandboxes", len(inv.Sandboxes), "snapshots", len(inv.Snapshots))
	return nodelink.InventoryAck{}
}

// linkUp registers a link and returns the function that removes it again.
//
// It is Attach with the opposite answer to a name that is already linked: the
// new link wins and the old one is told it was superseded. Attach refuses a
// duplicate because two callers registering one name is a wiring mistake,
// whereas here the name is authenticated — the same machine is on the other end
// of both links, and one half-open TCP socket must not be able to lock a node
// out of its own fleet until a timeout it cannot influence expires.
//
// Detach only ever removes the node it was minted for, so the superseded link
// tearing itself down cannot unregister its replacement.
func (f *Fleet) linkUp(n *linkNode) func() {
	name := n.Name()
	f.mu.Lock()
	old, dup := f.nodes[name]
	f.nodes[name] = n
	f.mu.Unlock()

	if dup {
		f.log.Info("node reconnected; superseding the previous link", "node", name)
		old.Hangup(nodelink.CodeSuperseded, "a newer link for this node replaced this one")
	}
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.nodes[name] == Node(n) {
			delete(f.nodes, name)
		}
	}
}

// EvictNode tears down the link a machine is holding right now, because the
// gateway has taken its approval away. It reports whether there was one.
//
// This exists because approval is only ever read at the door. A row that has
// been removed or disabled is consulted when a node connects and never again,
// so without this a revoked machine keeps its control channel, keeps reporting
// capacity into Capacities() and Nodes(), keeps its data channels open, and —
// the link sets no idle timeout on purpose, so that a node may stay connected
// for weeks — keeps them for as long as it likes.
//
// The node leaves the fleet's map before its link is told anything. The
// teardown is asynchronous by nature (the read goroutine has to notice a closed
// transport, and the far side may be a machine that has stopped listening), and
// a revoked node that was still answering "how much capacity does this fleet
// have" in the meantime is exactly the thing an operator ran the command to
// stop. Detach is minted per link and only removes the node it was minted for,
// so the link tidying up after itself later is a no-op rather than a race.
func (f *Fleet) EvictNode(name, reason string) bool {
	f.mu.Lock()
	n, linked := f.nodes[name]
	if linked {
		delete(f.nodes, name)
	}
	f.mu.Unlock()
	if !linked {
		return false
	}
	f.log.Info("node link revoked", "node", name, "reason", reason)
	n.Revoke(nodelink.CodeRevoked, revoked(name, reason))
	return true
}

// revoked is what an operation already in flight over a link reports once that
// link has been revoked under it.
//
// It is KindCapacity for the same reason Unreachable is (see errors.go): the
// honest rendering is "this could not be done, on a machine that is no longer
// part of this fleet", which is exit 1 and HTTP 503, and a new Kind would mean
// editing both the pinned taxonomy and the embedded OpenAPI enum for something
// that renders identically to a node outage.
//
// Its Code is the same token the node is sent as its bye code, because it is
// the same event described to the two audiences that have to correlate it: the
// operator's client and the machine's own log.
func revoked(node, reason string) *ctlops.Error {
	msg := fmt.Sprintf("node %q is no longer part of this fleet", node)
	if reason != "" {
		msg += ": " + reason
	}
	return &ctlops.Error{
		Kind:     ctlops.KindCapacity,
		Op:       "node.revoke",
		Code:     nodelink.CodeRevoked,
		Msg:      msg,
		Hint:     "Nothing on that machine was deleted; it can enrol again and be approved.",
		Details:  map[string]any{"node": node},
		Verbatim: true,
		Exit:     1,
		Status:   http.StatusServiceUnavailable,
	}
}

// NodeStatus is one machine as an operator sees it: what the roster knows about
// it joined to what its link says it is doing.
//
// The roster columns a link cannot know — the fingerprint, who approved it,
// when it first appeared — are left empty here. They belong to the node store,
// and the listing that wants them joins the two.
type NodeStatus struct {
	nodes.Node
	Online    bool              `json:"online"`
	Local     bool              `json:"local"`
	Capacity  host.NodeCapacity `json:"capacity"`
	Sandboxes int               `json:"sandboxes"`
	Running   int               `json:"running"`
}

// Nodes is every machine in this fleet, this one first and the rest name-sorted.
func (f *Fleet) Nodes() []NodeStatus {
	out := []NodeStatus{f.statusOf(f.local, true)}
	for _, n := range f.linked() {
		out = append(out, f.statusOf(n, false))
	}
	return out
}

func (f *Fleet) statusOf(n Node, local bool) NodeStatus {
	facts := n.Facts()
	boxes := n.Boxes()
	running := 0
	for _, b := range boxes {
		if b.State == vmm.StateRunning {
			running++
		}
	}
	online := n.Online()
	capacity := n.Capacity()
	capacity.Online = online
	st := NodeStatus{
		Online: online, Local: local, Capacity: capacity,
		Sandboxes: len(boxes), Running: running,
	}
	st.Name = n.Name()
	st.Arch = facts.Arch
	st.Release = facts.Release
	// A machine that is linked (or is this one) has been approved by
	// definition: an unapproved key never gets past the door. Pending and
	// disabled rows exist only in the roster, which is where a listing that
	// wants to show them reads them from.
	st.Status = nodes.StatusApproved
	if t := n.LastSeen(); !t.IsZero() {
		st.LastSeen = &t
	}
	return st
}

// linkNode presents a connected machine as a Node.
//
// It is deliberately observation-only at this milestone: the fleet can see what
// the node is and what it has left, and can do nothing to it. Nothing routes
// here yet either — the router resolves a machine from a ledger row, and no row
// names another machine until placement can put one there.
type linkNode struct {
	client *nodelink.Client
}

func (l *linkNode) Name() string { return l.client.Name() }

func (l *linkNode) Online() bool { return l.client.Online() }

func (l *linkNode) LastSeen() time.Time { return l.client.LastSeen() }

func (l *linkNode) Hangup(code, msg string) { l.client.Hangup(code, msg) }

func (l *linkNode) Revoke(code string, reason error) { l.client.Revoke(code, reason) }

// Facts are what the machine said about itself when it connected, with the name
// taken from the roster rather than from the hello: the roster row is what the
// SSH key resolved to, and the hello's own name is advisory.
func (l *linkNode) Facts() Facts {
	h := l.client.Hello()
	h.Node = l.client.Name()
	return h
}

func (l *linkNode) Capacity() host.NodeCapacity { return l.client.Capacity() }

// Box, Boxes and Templates answer with nothing on purpose.
//
// The link caches every row the node reports, but projecting those rows into
// the records the gateway serves is what makes a remote sandbox visible in a
// listing — and a sandbox visible before the ledger can place one would be a
// record no authorization decision could be made about. The projection lands
// with the placement it belongs to.
func (l *linkNode) Box(string) (*host.Sandbox, bool) { return nil, false }

func (l *linkNode) Boxes() []*host.Sandbox { return nil }

func (l *linkNode) Templates() []*host.Snapshot { return nil }

// notYet is the answer to every operation that would have to cross the link.
//
// It is unreachable: the router only resolves to a linked machine for a name
// the ledger places there, and nothing places one yet. It exists so that if
// something does route here, the answer is a sentence an operator can read
// rather than a nil dereference.
func notYet(op string) error {
	return ctlops.Disabled(op, "operating a sandbox on another machine isn't enabled on this gateway yet.")
}

func (l *linkNode) Create(context.Context, string, string, string, int64, int64) (*host.Sandbox, error) {
	return nil, notYet("create")
}

func (l *linkNode) EnsureRunning(context.Context, string) (*host.Sandbox, error) {
	return nil, notYet("restore")
}

func (l *linkNode) Pause(context.Context, string) error { return notYet("pause") }

func (l *linkNode) Archive(context.Context, string) error { return notYet("archive") }

func (l *linkNode) Resize(context.Context, string, int64) error { return notYet("resize") }

func (l *linkNode) Reboot(context.Context, string) error { return notYet("reboot") }

func (l *linkNode) Rename(context.Context, string, string, string) error { return notYet("rename") }

func (l *linkNode) Destroy(context.Context, string) error { return notYet("rm") }

func (l *linkNode) SetPinned(context.Context, string, bool) error { return notYet("pin") }

func (l *linkNode) ResyncEnv(context.Context, string) error { return notYet("secrets.sync") }

func (l *linkNode) Touch(context.Context, string) error { return notYet("touch") }

func (l *linkNode) RecordKey(context.Context, string, string) error { return notYet("keys.record") }

func (l *linkNode) Snapshotter(context.Context, string, string, string) (*host.Snapshot, error) {
	return nil, notYet("snapshot.create")
}

func (l *linkNode) DeleteSnapshot(context.Context, string, string) error {
	return notYet("snapshot.rm")
}

func (l *linkNode) Fork(context.Context, string, string, string, int64, int64) (*host.Sandbox, error) {
	return nil, notYet("fork")
}

// DialGuest opens a stream to a port inside one of that machine's guests. It
// names the sandbox rather than an address, which is what keeps "where does
// this guest's sshd listen" knowledge of the machine that booted it.
func (l *linkNode) DialGuest(ctx context.Context, sandbox, kind string, port int) (net.Conn, error) {
	return l.client.DialSandbox(ctx, sandbox, kind, port)
}
