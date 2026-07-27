// Package fleet routes control-plane operations to the machine that holds a
// sandbox. It declares the Node interface — the only thing in sparkbox that
// crosses a machine boundary — the adapter that presents the gateway's own
// *host.Manager as one, and the Fleet type that satisfies ctlops.Sandboxes,
// ctlops.Templates, sshgw.Sandboxes and xterm.Attacher so no caller can tell
// whether the sandbox it is acting on is on this machine or another.
//
// A single-box deployment is the degenerate case and is meant to be
// byte-identical: with no ledger and no attached nodes, every method here is
// the local manager's own answer, unsorted, unfiltered and uncopied.
package fleet

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// Suffix is the RFC 6761 .invalid label the gateway's synthetic sandbox
// addresses live under. Nothing will ever resolve it, which is the point.
// Every node mints its guests the same 172.30.<idx>.2 addresses, so a real
// address relayed from another machine is not a fleet-wide name: a path that
// still net.Dialed one would reach the gateway's OWN sandbox at that address —
// a cross-tenant bleed with no error and no log line. Failing closed is the
// only safe way to be wrong.
//
// The per-sandbox host string matters for a second reason: http.Transport
// pools idle connections by URL host alone (MaxIdleConnsPerHost 64 in
// internal/proxy), so it is the name that keeps node B's request off an idle
// connection to node A's sandbox.
const Suffix = ".sandbox.invalid"

// SSHPort is the port half of a synthetic SSHAddr. It is a name and not a
// number because only the owning node knows where its guests' sshd listens:
// firecracker reports 172.30.<idx>.2:22, the mock driver an ephemeral
// 127.0.0.1 port. net.SplitHostPort accepts a service name, and the node
// re-resolves it against its own record.
const SSHPort = "ssh"

// Host renders the synthetic address of a sandbox on a node.
func Host(sandbox, node string) string { return sandbox + "." + node + Suffix }

// SplitHost is Host's inverse. It reports false for anything that is not a
// synthetic fleet address, which is how the dialer tells a sandbox on another
// machine from an ordinary host-network address it must dial directly.
func SplitHost(h string) (sandbox, node string, ok bool) {
	rest, found := strings.CutSuffix(h, Suffix)
	if !found {
		return "", "", false
	}
	sandbox, node, found = strings.Cut(rest, ".")
	// Exactly two labels: a sandbox name and a node name, neither of which may
	// contain a dot. Anything else is a name we did not mint.
	if !found || sandbox == "" || node == "" || strings.Contains(node, ".") {
		return "", "", false
	}
	return sandbox, node, true
}

// Node is one machine that runs sandboxes.
//
// Every lifecycle method takes a context and returns an error, deliberately
// unlike ctlops.Sandboxes — whose Get, ListByOwner, Touch and ArchivingEnabled
// can report no network failure at all, and whose Get sits inside every
// authorization decision the control plane makes. That difference is the whole
// reason this interface exists separately: Fleet answers the context-free
// reads out of its own state and crosses a machine boundary only through here,
// so an ownership check can never turn into a blocking, uncancellable RPC.
//
// The inventory reads come in three shapes rather than one because the callers
// do: Fleet.Get wants one record, the listings want every sandbox, and the
// template paths want every snapshot. A single "here is everything" accessor
// made a lookup of one name copy and sort a whole machine's inventory —
// MaxSandboxesPerNode is 1024, and Get sits under every authorized operation
// and every browser terminal request.
type Node interface {
	Name() string
	Facts() Facts
	Online() bool

	// LastSeen is when this machine last said anything, zero for one that
	// cannot go quiet (the local node is this process).
	LastSeen() time.Time

	// Box is one sandbox from the node's last known inventory, served from
	// cache. It is how Fleet.Get answers without a network call.
	Box(name string) (*host.Sandbox, bool)
	// Boxes and Templates are the same cache, whole. They are what the listing
	// paths read; nothing else should, because they copy.
	Boxes() []*host.Sandbox
	Templates() []*host.Snapshot
	Capacity() host.NodeCapacity

	// Hangup ends this machine's link with a stated reason, and Revoke ends it
	// having first failed everything riding on it. They are on the interface
	// rather than reached by a type assertion on the link implementation
	// because a downcast that misses is a silent no-op, which is how a revoked
	// machine keeps its control channel. A node with no link — the local one —
	// answers by doing nothing, which is the honest answer.
	Hangup(code, msg string)
	Revoke(code string, reason error)

	Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error)
	EnsureRunning(ctx context.Context, name string) (*host.Sandbox, error)
	Pause(ctx context.Context, name string) error
	Archive(ctx context.Context, name string) error
	Resize(ctx context.Context, name string, sizeMB int64) error
	Reboot(ctx context.Context, name string) error
	Rename(ctx context.Context, oldName, newName, owner string) error
	Destroy(ctx context.Context, name string) error
	SetPinned(ctx context.Context, name string, pinned bool) error
	ResyncEnv(ctx context.Context, name string) error
	Touch(ctx context.Context, name string) error
	RecordKey(ctx context.Context, name, fp string) error

	Snapshotter(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error)
	DeleteSnapshot(ctx context.Context, snapName, owner string) error
	Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error)

	// DialGuest opens a stream to a port inside a sandbox. kind is
	// nodelink.StreamSSH (port ignored, the node knows where its sshd is) or
	// nodelink.StreamTCP.
	DialGuest(ctx context.Context, sandbox, kind string, port int) (net.Conn, error)

	// NetPolicy replaces this machine's whole egress policy, keyed by sandbox
	// NAME. NetUsage reads back what its VMs have been talking to, keyed the
	// same way.
	//
	// They are on Node rather than reached through the local syncer because
	// egress is enforced and metered per MACHINE — sluice attaches to the taps
	// in front of it and can see no others — so "which machine" is exactly the
	// question this interface exists to answer. Both refuse with a typed
	// nodelink.CodeNoSluice on a machine that runs no egress gateway, which is
	// what lets a caller tell "nothing to report" from "not measured".
	NetPolicy(ctx context.Context, allow map[string][]string) error
	NetUsage(ctx context.Context) (map[string]netpush.VMUsage, error)
}

// Facts is what a node says about itself: everything a placement decision or an
// operator listing needs that is not a live resource number. A node reports
// them once, at hello, which is why this IS the hello — a separate struct was
// the same eleven fields under a second name, and the copy between them could
// only ever lose one. The local adapter fills in what the manager knows and
// leaves the rest empty rather than guessing.
type Facts = nodelink.Hello
