package fleet

// A machine on the other end of a link, presented as a Node.
//
// Every method below is the same five lines: build the request body, hand it to
// Client.Do, project the answer. The uniformity is the payoff for Node being
// the remotable subset of what a manager can do — but two things are enforced
// here and NOWHERE else, and both are one-liners that would be easy to leave
// out of the sixteenth near-identical method:
//
//  1. Every record this file produces has Node set from the AUTHENTICATED link
//     name and its addresses synthesised. See box.
//  2. Every failure that is not the far side's own typed answer becomes
//     Unreachable. See fail.
//
// Neither can be delegated upward. Fleet.serve stamps owner and node from the
// ledger for everything it renders out of a node's cache, but Create,
// EnsureRunning and Fork hand their record straight back to the caller without
// passing through it; and nothing above this file can tell an io.EOF that means
// "the machine is gone" from one that means anything else.
//
// A third thing is enforced here for the same reason: every node-authored
// string on a record is clamped or scrubbed before it becomes one. See record.

import (
	"context"
	"errors"
	"net"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/reserved"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// maxDisplayText bounds one node-authored display string — an image reference,
// a login name, a key fingerprint. It is ctlops' own maxWireText, restated
// rather than exported: these are the same kind of value the error wire caps,
// arriving from the same machine, and both end up on one line of somebody's
// terminal.
const maxDisplayText = 128

var (
	_ ControlPlane = (*remoteNode)(nil)
	_ GuestDialer  = (*remoteNode)(nil)
	_ Node         = (*linkedRemote)(nil)
)

// remoteNode presents a connected machine as a Node.
type remoteNode struct {
	client *nodelink.Client
}

// linkedRemote is the live-link metadata kept alongside the reusable
// ControlPlane/GuestDialer composition. The two interfaces happen to share the
// SSH implementation during phase 3, but callers no longer assume they do.
type linkedRemote struct {
	Node
	client        *nodelink.Client
	selector      *ControlSelector
	guestSelector *GuestSelector
	ssh           ControlPlane
	sshGuest      GuestDialer
}

// Remote adapts a live link to Node. The link is already authenticated: the
// door resolved an SSH key to a roster row before it ever built one, and that
// row's name is the only name this machine is known by up here.
func Remote(c *nodelink.Client) Node {
	ssh := &remoteNode{client: c}
	selector := &ControlSelector{mode: ControlTransportSSH, ssh: ssh}
	guestSelector := &GuestSelector{mode: GuestTransportSSH, ssh: ssh, canaryPercent: 100}
	return &linkedRemote{
		Node: ComposeNode(selector, guestSelector), client: c,
		selector: selector, guestSelector: guestSelector, ssh: ssh, sshGuest: ssh,
	}
}

func (r *remoteNode) Name() string { return r.client.Name() }

func (r *remoteNode) Online() bool { return r.client.Online() }

func (r *remoteNode) LastSeen() time.Time { return r.client.LastSeen() }

func (r *remoteNode) Hangup(code, msg string) { r.client.Hangup(code, msg) }

func (r *remoteNode) Revoke(code string, reason error) { r.client.Revoke(code, reason) }

// Facts are what the machine said about itself when it connected, with the name
// taken from the roster rather than from the hello: the roster row is what the
// SSH key resolved to, and the hello's own name is advisory.
func (r *remoteNode) Facts() Facts {
	h := r.client.Hello()
	h.Node = r.client.Name()
	return h
}

func (r *remoteNode) Capacity() host.NodeCapacity { return r.client.Capacity() }

// ---------------------------------------------------------------------------
// The two invariants
// ---------------------------------------------------------------------------

// record is the ONE place a row from another machine becomes a record this
// gateway will hand out, and it is the whole of invariant 1 plus the string
// hygiene that goes with it.
//
// Node is set from the authenticated link name and never from the payload. The
// wire's SandboxRow carries no node field at all, which is deliberate — see
// nodelink.SandboxRow — and this is what keeps it that way: a future field, a
// hand-crafted reply, or a node that has been told a different name for itself
// cannot make one of its records claim to live somewhere else. A record that
// could would be a record the ledger's owner column no longer governs, which is
// how one machine comes to answer for another's sandboxes.
//
// name and owner are the GATEWAY'S, passed in by the caller, never read off the
// row. For a cached row they are the key it was filed under and the owner
// beside it, both of which the gateway itself wrote; for an RPC reply they are
// the name and owner the request carried, which this gateway chose and already
// reserved in the ledger. Nothing is gained by believing the answer, and what is
// lost by believing it is real: a create reply naming
// "demo\r\n\x1b[2Ksparkbox: ..." reaches sshgw's reconnect hint, which
// concatenates a sandbox name into a sentence written to a terminal in raw mode.
//
// Everything else node-authored is clamped or scrubbed rather than trusted:
//
//   - State is held to the vmm vocabulary. It is not prose, it is a word this
//     gateway switches on and prints in a fixed-width column (sshgw's `ctl
//     list`), so an unknown one is dropped rather than shown. Empty then means
//     the same thing it means on an unplaced row — nobody has said — which is
//     the honest answer for a word we would not repeat.
//   - Image, SSHUser and KeyFP are free-form on the wire and land on the same
//     line of the same terminal, so they go through the scrubber ctlops already
//     applies to a node's error prose and to a pause reason.
//
// The addresses are synthesised for the same reason they are absent from the
// wire: every node mints its guests the same 172.30.<idx>.2 address, so an
// address relayed from another machine would resolve, up here, to one of THIS
// gateway's sandboxes. Synthesising them here rather than only in Fleet.serve
// means a record that leaves by an RPC — Create, EnsureRunning, Fork — is
// addressed correctly too. GuestV6, ArchiveKey and RenamedFrom stay empty
// because the wire does not carry them and inventing one would be a fact
// nobody observed.
func (r *remoteNode) record(row nodelink.SandboxRow, name, owner string) *host.Sandbox {
	node := r.client.Name()
	b := &host.Sandbox{
		Name:        name,
		Owner:       owner,
		Image:       ctlops.SafeText(row.Image, maxDisplayText),
		VCPUs:       row.VCPUs,
		MemMB:       row.MemMB,
		State:       safeState(row.State),
		SSHUser:     ctlops.SafeText(row.SSHUser, maxDisplayText),
		CreatedAt:   row.CreatedAt,
		LastActive:  row.LastActive,
		Pinned:      row.Pinned,
		Ballooned:   row.Ballooned,
		KeyFP:       ctlops.SafeText(row.KeyFP, maxDisplayText),
		NetRxBytes:  row.NetRxBytes,
		NetTxBytes:  row.NetTxBytes,
		ArchivedAt:  row.ArchivedAt,
		DiskMB:      row.DiskMB,
		DiskTotalMB: row.DiskTotalMB,
		Turbo:       row.Turbo,
		Node:        node,
	}
	b.HostIP = Host(b.Name, node)
	b.SSHAddr = net.JoinHostPort(b.HostIP, SSHPort)
	return b
}

// box projects a row out of the link's cache, and answers nil for one this
// gateway will not put a name to.
//
// The name is the row's own here — there is nothing else it could be, a cache
// read being a lookup by name — so it is the one place a node-authored name
// becomes a record's, and it is held to the shape this platform issues. That is
// the same test reconcile.adopt applies before a node-authored name may enter
// the ledger, and for the same reason: a sandbox name becomes a DNS label, a
// subdomain, and a column in a listing written to a raw terminal. A row that
// fails it is dropped rather than shown, which leaves the placement ledger to
// render whatever it has under that name (see Fleet.unplaced).
//
// Owner rides as the node reported it and is advisory only. The ledger's owner
// column is the authorization input, and Fleet.serve overwrites this field from
// it before any record reaches a listing or an ownership check.
func (r *remoteNode) box(row nodelink.SandboxRow) *host.Sandbox {
	if !reserved.ValidLabel(row.Name) {
		return nil
	}
	return r.record(row, row.Name, safeOwner(row.Owner))
}

// safeOwner holds a node-authored owner to a handle this platform issues. It is
// advisory everywhere it survives — Fleet.serve overwrites it from the ledger
// before any listing or ownership check sees it — but "advisory" is not
// "arbitrary bytes", and the one record that skips serve() (a resume's reply)
// carries it onward.
func safeOwner(s string) string {
	if users.ValidHandle(s) {
		return s
	}
	return ""
}

// template projects a fork template, and answers nil for one this gateway will
// not put a name to. Node is stamped for the same reason a sandbox's is: a
// snapshot is a reflink source in one machine's image directory, so which
// machine holds it decides where a fork can happen at all.
//
// A template gets the same treatment as a sandbox row and needs it more. There
// is no snapshot ledger, so nothing downstream re-derives these strings from a
// gateway-authored row — Fleet.Snapshots filters on the node's own owner field
// and hands the rest straight to `ctl snapshot ls`, which prints Name and
// FromBox side by side at a raw terminal.
func (r *remoteNode) template(row nodelink.SnapshotRow) *host.Snapshot {
	if !reserved.ValidLabel(row.Name) {
		return nil
	}
	from := row.FromBox
	if !reserved.ValidLabel(from) {
		// Blanked rather than dropping the whole template: which sandbox a
		// template was taken from is a nicety, and the template itself is still
		// forkable without it.
		from = ""
	}
	return &host.Snapshot{
		Name:      row.Name,
		Owner:     row.Owner,
		Image:     ctlops.SafeText(row.Image, maxDisplayText),
		FromBox:   from,
		CreatedAt: row.CreatedAt,
		Node:      r.client.Name(),
	}
}

// safeState clamps a node's state word to the vocabulary this gateway knows.
//
// The vmm states are the whole list on purpose: the empty string is already the
// "nobody has said" that unplaced rows carry, and every reader either switches
// on a known value or prints it. An unrecognised word from a newer node is
// therefore shown as no state rather than as itself — which is a small display
// regression on a mixed-version fleet and the only way to keep this from being
// a channel for arbitrary bytes.
func safeState(s string) vmm.State {
	switch st := vmm.State(s); st {
	case vmm.StateRunning, vmm.StatePaused, vmm.StateArchived:
		return st
	default:
		return ""
	}
}

// fail turns whatever went wrong into something a person can read, and it is
// the whole of invariant 2.
//
// The failure that must never escape is io.EOF. Conn.Serve records exactly that
// on a clean end of stream (conn.go), which is what a gateway restart, a node
// restart, a superseded link and a killed process all look like — and Request
// hands it back verbatim from three places: a request made after the link died,
// a send onto a dead writer, and a request that was in flight when the stream
// ended. Rendered, it reads `sparkbox: rm failed: EOF`, which tells a user
// nothing and is easily mistaken for "there is no such sandbox".
//
// Three classes, and only three:
//
//   - An error that is already a *ctlops.Error passes through untouched. That
//     is the node's own answer (rebuilt by ctlops.FromWire with its concrete
//     host cause intact, so sshgw.failStart's errors.As switches still fire), or
//     the sentence Client.Revoke installed when an operator took this machine's
//     approval away — both of which say more than "offline" does.
//   - A context error is the CALLER's own budget expiring or its own
//     cancellation, and it is classified exactly as it would be for a sandbox on
//     this machine. Calling that a node outage would blame the wrong thing.
//   - Everything else is the link: io.EOF, ErrLinkClosed, ErrLinkBacklogged, a
//     write error, a reply this build cannot parse. The machine is not
//     answering, whatever the socket called it.
func (r *remoteNode) fail(op, sandbox string, err error) error {
	if err == nil {
		return nil
	}
	var typed *ctlops.Error
	switch {
	case errors.As(err, &typed):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return Unreachable(op, sandbox, r.client.Name())
	}
}

// ---------------------------------------------------------------------------
// Inventory, served from the link's cache
// ---------------------------------------------------------------------------

// Box, Boxes and Templates read the picture the link has been keeping since the
// node's first inventory. They are cache reads and never requests, because
// Fleet.Get sits inside every authorization decision the control plane makes
// and an ownership check that could block on a network hop is an ownership
// check that can hang a session.
func (r *remoteNode) Box(name string) (*host.Sandbox, bool) {
	row, ok := r.client.Box(name)
	if !ok {
		return nil, false
	}
	b := r.box(row)
	if b == nil {
		return nil, false
	}
	return b, true
}

func (r *remoteNode) Boxes() []*host.Sandbox {
	rows, _ := r.client.Snapshot()
	out := make([]*host.Sandbox, 0, len(rows))
	for _, row := range rows {
		if b := r.box(row); b != nil {
			out = append(out, b)
		}
	}
	return out
}

// Vitals is the exception to the paragraph above: a live counter cannot come
// out of a cache, so this one does cross the wire. It is safe to do so from an
// ownership-checked page rather than only from an operation because it is a
// read that changes nothing and carries its caller's budget — the terminal's
// poll gives it webui.TunneledProbeTimeout, so a node that has gone quiet costs
// a viewer one late frame and not a hung request.
func (r *remoteNode) Vitals(ctx context.Context, name string) (host.Vitals, error) {
	var resp nodelink.VitalsResp
	if err := r.client.Do(ctx, nodelink.TypeVitals, nodelink.NameReq{Name: name}, &resp); err != nil {
		return host.Vitals{}, r.fail("vitals", name, err)
	}
	// Copied field by field rather than passed through: these are numbers a
	// meter divides by, and the ceilings they are divided by (VCPUs, MemMB) come
	// from the gateway's own record. Nothing here is clamped because there is no
	// clamp that would be honest — a machine that lies about its own CPU seconds
	// produces a wrong sparkline for its own sandbox, which is the whole of the
	// damage, and a plausible-looking invented ceiling would be worse.
	return host.Vitals{
		CPUSeconds: resp.CPUSeconds,
		MemUsedMB:  resp.MemUsedMB,
		NetRxBytes: resp.NetRxBytes,
		NetTxBytes: resp.NetTxBytes,
	}, nil
}

func (r *remoteNode) Templates() []*host.Snapshot {
	_, rows := r.client.Snapshot()
	out := make([]*host.Snapshot, 0, len(rows))
	for _, row := range rows {
		if s := r.template(row); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------
//
// The op strings are the ones Fleet.route uses when it raises Unreachable for a
// machine that is not linked at all, so the same outage reads identically
// whether it was noticed before the request or during it.
//
// The three that answer with a record — Create, EnsureRunning and Fork — build
// it with the name and owner THIS gateway asked for rather than the ones the
// node echoed back. Those records skip Fleet.serve, which is what re-derives
// both from the ledger for everything else, so this is the only place they
// could be re-derived at all; and the gateway is already the authority on both,
// having reserved the ledger row under them a moment earlier. EnsureRunning
// carries no owner — resume is not an ownership-changing operation, and its
// caller already passed the ownership gate on the ledger's column — so the
// node's advisory one rides on, as it does for a cached row.

func (r *remoteNode) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	var resp nodelink.SandboxResp
	req := nodelink.CreateReq{Name: name, Owner: owner, Image: image, VCPUs: vcpus, MemMB: memMB}
	if err := r.client.Do(ctx, nodelink.TypeCreate, req, &resp); err != nil {
		return nil, r.fail("create", name, err)
	}
	return r.record(resp.Sandbox, name, owner), nil
}

func (r *remoteNode) EnsureReady(ctx context.Context, name string) (*host.Sandbox, error) {
	var resp nodelink.SandboxResp
	if err := r.client.Do(ctx, nodelink.TypeEnsureRunning, nodelink.NameReq{Name: name}, &resp); err != nil {
		return nil, r.fail("restore", name, err)
	}
	return r.record(resp.Sandbox, name, safeOwner(resp.Sandbox.Owner)), nil
}

func (r *remoteNode) Pause(ctx context.Context, name string) error {
	return r.named(ctx, nodelink.TypePause, "pause", name)
}

func (r *remoteNode) Archive(ctx context.Context, name string) error {
	return r.named(ctx, nodelink.TypeArchive, "archive", name)
}

func (r *remoteNode) Reboot(ctx context.Context, name string) error {
	return r.named(ctx, nodelink.TypeReboot, "reboot", name)
}

func (r *remoteNode) SetTurbo(ctx context.Context, name string, on bool) error {
	var resp nodelink.EmptyResp
	req := nodelink.TurboReq{Name: name, On: on}
	if err := r.client.Do(ctx, nodelink.TypeTurbo, req, &resp); err != nil {
		return r.fail("turbo", name, err)
	}
	return nil
}

func (r *remoteNode) Destroy(ctx context.Context, name string) error {
	return r.named(ctx, nodelink.TypeDestroy, "rm", name)
}

// named runs the four verbs whose whole request is a sandbox name and whose
// whole answer is whether it worked.
func (r *remoteNode) named(ctx context.Context, typ, op, name string) error {
	var resp nodelink.EmptyResp
	if err := r.client.Do(ctx, typ, nodelink.NameReq{Name: name}, &resp); err != nil {
		return r.fail(op, name, err)
	}
	return nil
}

func (r *remoteNode) Resize(ctx context.Context, name string, sizeMB int64) error {
	var resp nodelink.EmptyResp
	req := nodelink.ResizeReq{Name: name, SizeMB: sizeMB}
	if err := r.client.Do(ctx, nodelink.TypeResize, req, &resp); err != nil {
		return r.fail("resize", name, err)
	}
	return nil
}

// Rename sends only the node's half. The gateway has already moved the
// placement row and will roll it back if this fails — see Fleet.Rename — and
// the side stores that follow a sandbox's name are the gateway's own.
func (r *remoteNode) Rename(ctx context.Context, oldName, newName, owner string) error {
	var resp nodelink.EmptyResp
	req := nodelink.RenameReq{Name: oldName, NewName: newName, Owner: owner}
	if err := r.client.Do(ctx, nodelink.TypeRename, req, &resp); err != nil {
		return r.fail("rename", oldName, err)
	}
	return nil
}

func (r *remoteNode) SetPinned(ctx context.Context, name string, pinned bool) error {
	op := "unpin"
	if pinned {
		op = "pin"
	}
	var resp nodelink.EmptyResp
	req := nodelink.PinReq{Name: name, Pinned: pinned}
	if err := r.client.Do(ctx, nodelink.TypeSetPinned, req, &resp); err != nil {
		return r.fail(op, name, err)
	}
	return nil
}

func (r *remoteNode) ResyncEnv(ctx context.Context, name string) error {
	var resp nodelink.EmptyResp
	if err := r.client.Do(ctx, nodelink.TypeResyncEnv, nodelink.NameReq{Name: name}, &resp); err != nil {
		return r.fail("secrets.sync", name, err)
	}
	return nil
}

// Touch and RecordKey are the two writes that must never cost a caller a round
// trip: one fires on every SSH session teardown, the other on every browser
// keystroke batch. They ride as events and answer nil immediately — including
// when the link is dead, because there is no failure to report to a caller that
// was never going to look. The node applies them off its own read goroutine.
func (r *remoteNode) MarkActive(_ context.Context, name string) error {
	r.client.Cast(nodelink.TypeTouch, nodelink.NameReq{Name: name})
	return nil
}

func (r *remoteNode) RecordKey(_ context.Context, name, fp string) error {
	r.client.Cast(nodelink.TypeRecordKey, nodelink.KeyReq{Name: name, KeyFP: fp})
	return nil
}

func (r *remoteNode) Snapshotter(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error) {
	var resp nodelink.SnapshotResp
	req := nodelink.SnapshotReq{Sandbox: box, Snapshot: snapName, Owner: owner}
	if err := r.client.Do(ctx, nodelink.TypeSnapshotCreate, req, &resp); err != nil {
		return nil, r.fail("snapshot.create", box, err)
	}
	s := r.template(resp.Snapshot)
	if s == nil {
		// The node answered success with a template this gateway will not name.
		// The snapshot may well exist on that machine, but nothing here can
		// refer to it, so the honest answer is that the machine is not making
		// sense rather than a record with a name nobody asked for.
		return nil, Unreachable("snapshot.create", box, r.client.Name())
	}
	// The name and the sandbox it came from are this gateway's own: they were
	// the request. See record.
	s.Name, s.FromBox, s.Owner = snapName, box, owner
	return s, nil
}

func (r *remoteNode) DeleteSnapshot(ctx context.Context, snapName, owner string) error {
	var resp nodelink.EmptyResp
	req := nodelink.DeleteSnapshotReq{Snapshot: snapName, Owner: owner}
	if err := r.client.Do(ctx, nodelink.TypeSnapshotDelete, req, &resp); err != nil {
		return r.fail("snapshot.rm", snapName, err)
	}
	return nil
}

func (r *remoteNode) Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error) {
	var resp nodelink.SandboxResp
	req := nodelink.ForkReq{Snapshot: snapName, Name: newName, Owner: owner, VCPUs: vcpus, MemMB: memMB}
	if err := r.client.Do(ctx, nodelink.TypeSnapshotFork, req, &resp); err != nil {
		return nil, r.fail("fork", newName, err)
	}
	return r.record(resp.Sandbox, newName, owner), nil
}

// DialGuest opens a stream to a port inside a guest on the other machine.
//
// It names a sandbox and never an address, which is the whole point of the
// reverse stream: the node re-resolves from the record IT holds, so nothing this
// gateway believes — a cached row, a sandbox that has since moved or paused —
// can talk another machine into dialing something of its own choosing. It is
// also why a remote sandbox's HostIP and SSHAddr are synthetic .invalid names:
// there is no address here to leak, because there never was one.
//
// The error handling is the one deliberate exception to invariant 2, and it is
// load-bearing rather than an oversight:
//
//   - An *xssh.OpenChannelError is the NODE'S OWN typed answer, carried back by
//     the protocol. It is the only thing that tells "that machine has never
//     heard of this sandbox" and "it is not running" (Prohibited — the machine
//     is confused or the box is down, a 503) apart from "connection refused
//     inside the guest" (ConnectionFailed — nothing is listening on that port,
//     the existing 502). Passing it through r.fail would collapse both into
//     Unreachable and destroy the distinction the edge's error pages are built
//     on. It is therefore returned untouched.
//   - Everything else is the link dying under the open — io.EOF, a closed
//     connection, a link that carries no data channels at all — and becomes
//     Unreachable like every other transport death in this file.
//
// There is no retry. A channel-open rejection is an answer, not a hiccup, and
// the one caller that retries at all (sshgw.DialUpstreamVia) already fast-fails
// on both of these types rather than spending its 15-second budget on them.
func (r *remoteNode) DialGuest(ctx context.Context, sandbox, kind string, port int) (net.Conn, error) {
	c, err := r.client.DialSandbox(ctx, sandbox, kind, port)
	if err != nil {
		var refused *xssh.OpenChannelError
		if errors.As(err, &refused) {
			return nil, err
		}
		return nil, r.fail("dial", sandbox, err)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// The egress plane
// ---------------------------------------------------------------------------

// NetPolicy pushes this machine's whole egress policy over the link, and
// NetUsage reads its meter back.
//
// Both name a machine rather than a sandbox in their failures, which is the one
// place this file's usual `r.fail(op, sandbox, err)` shape does not fit: a
// policy push carries the machine's entire fleet and a usage read asks about no
// sandbox in particular, so there is no single name that could honestly go in
// the sentence. Passing the empty string is what Unreachable already expects
// when the subject is the machine itself.
func (r *remoteNode) NetPolicy(ctx context.Context, allow map[string][]string) error {
	var resp nodelink.EmptyResp
	req := nodelink.NetPolicyReq{Allow: allow}
	if err := r.client.Do(ctx, nodelink.TypeNetPolicy, req, &resp); err != nil {
		return r.fail("net.policy", "", err)
	}
	return nil
}

func (r *remoteNode) NetUsage(ctx context.Context) (map[string]netpush.VMUsage, error) {
	var resp nodelink.NetUsageResp
	if err := r.client.Do(ctx, nodelink.TypeNetUsage, struct{}{}, &resp); err != nil {
		return nil, r.fail("net.usage", "", err)
	}
	// Re-keyed here rather than on the wire so the node sends a list — a map
	// whose keys are node-authored strings would be a map this gateway indexes
	// by something it has not clamped. The names are checked against the
	// ledger by the caller, which drops anything not placed on this machine.
	out := make(map[string]netpush.VMUsage, len(resp.VMs))
	for _, u := range resp.VMs {
		if u.Name == "" {
			continue
		}
		out[u.Name] = u
	}
	return out, nil
}
