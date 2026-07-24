package fleet

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
)

// The compile assertions live here rather than beside the interfaces they
// satisfy: ctlops' own test files are in package ctlops, and fleet imports
// ctlops, so an in-package assertion there would be an import cycle. Keeping
// them together means one place to look when a signature moves.
var (
	_ ctlops.Sandboxes = (*Fleet)(nil)
	_ ctlops.Templates = (*Fleet)(nil)
	_ Node             = (*localNode)(nil)
)

// setPinnedBudget bounds the one mutating operation whose interface signature
// carries an error but no context. It is generous because the call is rare and
// stingy because a `ctl pin` that hangs is worse than one that fails.
const setPinnedBudget = 10 * time.Second

type Options struct {
	// Local is the gateway's own machine. Required: a fleet with nowhere to put
	// the first sandbox is not a fleet.
	Local *host.Manager
	// LocalName is what this machine is called in the ledger and in listings.
	// Empty takes the manager's own node name, so the two cannot disagree about
	// the string the manager already stamps on every record it creates.
	LocalName string
	// LocalArch is the CPU architecture recorded against sandboxes placed here,
	// so a later scheduler can tell an arm64 machine from an amd64 one without
	// asking it.
	LocalArch string
	// Index is the durable name -> node ledger. Nil is a single-node
	// deployment: nothing is placed anywhere but here, so nothing needs
	// recording and the local manager stays the only truth.
	Index *placement.Store
	Log   *slog.Logger
}

// Fleet is the router. It holds no lifecycle state of its own: the local
// manager is the truth for local sandboxes, each node's cache is the truth for
// display of its own, and the ledger is the truth for who owns what and where
// it lives.
type Fleet struct {
	local     Node
	localMgr  *host.Manager
	localName string
	localArch string
	index     *placement.Store
	log       *slog.Logger

	mu    sync.RWMutex
	nodes map[string]Node // linked machines other than this one, by name
}

func New(opts Options) (*Fleet, error) {
	if opts.Local == nil {
		return nil, errors.New("fleet: a local manager is required")
	}
	// Resolved once, here, and handed to Local rather than resolved again: the
	// manager's own node name is never empty (host.NewManager coerces it), so
	// this is the whole ladder.
	name := opts.LocalName
	if name == "" {
		name = opts.Local.NodeName()
	}
	arch := opts.LocalArch
	if arch == "" {
		arch = opts.Local.Capacity().Arch
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	f := &Fleet{
		local:     Local(name, opts.Local),
		localMgr:  opts.Local,
		localName: name,
		localArch: arch,
		index:     opts.Index,
		log:       log,
		nodes:     map[string]Node{},
	}
	if err := f.adoptLocal(); err != nil {
		return nil, err
	}
	return f, nil
}

// Close drops every linked node. The local manager and the ledger belong to
// whoever opened them and are not closed here.
func (f *Fleet) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.nodes)
	return nil
}

// adoptLocal makes the ledger agree with this machine at boot. The local
// manager is the truth for local sandboxes — it is the thing holding the
// rootfs — so a sandbox with no row gets one, and a row for a local sandbox
// that no longer exists is released rather than left to block the name
// forever. A row claiming one of our names for another machine is not resolved
// here: that is two machines claiming one name, and silently picking a winner
// is how a user loses a sandbox.
//
// No single record may stop the gateway from booting. Adoption is a
// convergence step, not a precondition: a sandbox this machine already runs
// keeps running whether or not the ledger learns about it, so a record the
// ledger will not accept is logged and left unplaced rather than turned into a
// failed serve() on an existing deployment.
func (f *Fleet) adoptLocal() error {
	if f.index == nil {
		return nil
	}
	here := map[string]bool{}
	for _, b := range f.localMgr.List() {
		here[b.Name] = true
		row, ok, err := f.index.Get(b.Name)
		if err != nil {
			return err
		}
		if ok {
			if row.Node != f.localName {
				f.log.Error("a sandbox on this machine is placed on another one",
					"name", b.Name, "placed_on", row.Node, "local", f.localName)
			}
			continue
		}
		if b.Owner == "" {
			// A sandbox from before records carried an owner, or one that has
			// lost it. The ledger refuses an ownerless placement because owner
			// is an authorization input, and inventing one here would be the
			// gateway handing a sandbox to somebody. Leaving it unplaced costs
			// nothing this machine needs: the local manager answers every read
			// and every operation on it first, and the name is still defended
			// both ways — the local manager refuses a second sandbox by that
			// name here, and heldLocally refuses to hand it to another machine,
			// which the ledger cannot do for a row that does not exist. What the
			// deployment loses is the ability to place that one name on
			// another machine, which is worth a loud log and no more.
			f.log.Error("a sandbox on this machine has no owner and cannot be placed",
				"name", b.Name, "node", f.localName)
			continue
		}
		switch err := f.index.Reserve(b.Name, b.Owner, f.localName, b.Image, f.localArch); {
		case err == nil:
		case errors.Is(err, placement.ErrTaken):
			// Somebody claimed the name between the Get above and this insert,
			// which is the same two-machines-one-name situation the branch
			// above reports and is resolved the same way: not here.
			f.log.Error("a sandbox on this machine lost a race for its own name", "name", b.Name)
		default:
			return err
		}
	}
	rows, err := f.index.ByNode(f.localName)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if here[r.Name] {
			continue
		}
		f.log.Info("releasing a placement this machine no longer holds", "name", r.Name)
		if err := f.index.Release(r.Name); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Membership
// ---------------------------------------------------------------------------

// Attach adds a linked machine and returns the function that removes it again.
// Detaching is idempotent and only ever removes the node it was minted for, so
// a link that is superseded while it is shutting down cannot unregister its
// replacement.
//
// It is the entry point for a Node this process holds directly — a test's
// in-memory machine, or an embedder's — and not the one a real link takes:
// ServeLink registers through linkUp, which answers a duplicate name the other
// way round (see link.go). Nothing in cmd/sparkbox calls this.
func (f *Fleet) Attach(n Node) (detach func(), err error) {
	name := n.Name()
	if name == "" {
		return nil, errors.New("fleet: a node needs a name")
	}
	if name == f.localName {
		return nil, errors.New("fleet: " + name + " is this machine")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.nodes[name]; dup {
		return nil, errors.New("fleet: node " + name + " is already linked")
	}
	f.nodes[name] = n
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.nodes[name] == n {
			delete(f.nodes, name)
		}
	}, nil
}

func (f *Fleet) nodeByName(name string) (Node, bool) {
	if name == f.localName {
		return f.local, true
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	n, ok := f.nodes[name]
	return n, ok
}

// linked returns the attached machines, name-sorted, so every listing this
// package produces is in a stable order whatever the map iteration does.
func (f *Fleet) linked() []Node {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// NodeOf reports which machine holds a sandbox.
func (f *Fleet) NodeOf(sandbox string) (string, bool) {
	if _, ok := f.localMgr.Get(sandbox); ok {
		return f.localName, true
	}
	row, ok := f.rowFor(sandbox)
	if !ok {
		return "", false
	}
	return row.Node, true
}

// Online reports whether a machine is answering. The local one always is.
func (f *Fleet) Online(node string) bool {
	n, ok := f.nodeByName(node)
	return ok && n.Online()
}

// Capacities is every machine's resource picture, this one first. A single-box
// deployment gets a one-element slice — exactly what the operator console's
// cluster endpoint has always returned.
func (f *Fleet) Capacities() []host.NodeCapacity {
	out := []host.NodeCapacity{f.localMgr.Capacity()}
	for _, n := range f.linked() {
		c := n.Capacity()
		c.Online = n.Online()
		out = append(out, c)
	}
	return out
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

// route resolves the machine that holds a name. The local manager is asked
// first and its answer is final, so a single-box deployment never reads the
// ledger on an operation and never depends on a cache.
//
// A name nobody has resolves to the local node on purpose: the local manager
// then raises its own "sandbox %q not found", which is the sentence every
// caller already renders, rather than this package inventing a second one.
func (f *Fleet) route(op, name string) (Node, error) {
	if _, ok := f.localMgr.Get(name); ok {
		return f.local, nil
	}
	if f.index == nil {
		return f.local, nil
	}
	row, ok, err := f.index.Get(name)
	if err != nil {
		return nil, err
	}
	if !ok || row.Node == f.localName {
		return f.local, nil
	}
	n, ok := f.nodeByName(row.Node)
	if !ok || !n.Online() {
		return nil, Unreachable(op, name, row.Node)
	}
	return n, nil
}

// rowFor reads one name's ledger row. It takes no "nothing is linked" shortcut
// the way the listing reads do: NodeOf must still answer with the machine a row
// names even when that machine is not connected, exactly as route must still
// raise Unreachable rather than "not found". The shortcut belongs to the
// callers that would discard the row anyway — see Get.
func (f *Fleet) rowFor(name string) (placement.Row, bool) {
	if f.index == nil {
		return placement.Row{}, false
	}
	row, ok, err := f.index.Get(name)
	if err != nil {
		f.log.Error("could not read the placement ledger", "name", name, "err", err)
		return placement.Row{}, false
	}
	return row, ok
}

// archOf is the architecture recorded against a placement. The local node's
// manager may not have been told its own arch, so the fleet's configured value
// stands in.
func (f *Fleet) archOf(n Node) string {
	if a := n.Facts().Arch; a != "" {
		return a
	}
	if n.Name() == f.localName {
		return f.localArch
	}
	return ""
}

// serve renders one machine's record as the gateway hands it out.
//
// Owner and Node come back from the ledger and not from the node. They are
// gateway-authored and are the only authorization inputs, which is what makes
// "a node lying can only affect its own sandboxes" structural rather than a
// review rule. Everything else — state, disk, counters, last activity — is
// node-authored and display-only.
//
// The addresses are synthesised, never relayed: see Suffix. GuestV6 is dropped
// for the same reason, being an address minted out of another machine's prefix.
func (f *Fleet) serve(b *host.Sandbox, row placement.Row, online bool) *host.Sandbox {
	c := *b
	c.Owner = row.Owner
	c.Node = row.Node
	c.HostIP = Host(c.Name, row.Node)
	c.SSHAddr = net.JoinHostPort(c.HostIP, SSHPort)
	c.GuestV6 = ""
	c.Unreachable = !online
	return &c
}

// remoteRows renders ledger rows held by other machines. Rows whose node has
// never reported an inventory are skipped: there is nothing to say about them
// yet, and a placeholder would be a state nobody observed. Quarantined rows —
// a name two machines claim — are never served at all.
func (f *Fleet) remoteRows(rows []placement.Row) []*host.Sandbox {
	var out []*host.Sandbox
	byNode := map[string]map[string]*host.Sandbox{}
	for _, row := range rows {
		if row.Node == f.localName || row.State == placement.StateQuarantine {
			continue
		}
		n, ok := f.nodeByName(row.Node)
		if !ok {
			continue
		}
		cache, seen := byNode[row.Node]
		if !seen {
			cache = map[string]*host.Sandbox{}
			for _, b := range n.Boxes() {
				cache[b.Name] = b
			}
			byNode[row.Node] = cache
		}
		b, ok := cache[row.Name]
		if !ok {
			continue
		}
		out = append(out, f.serve(b, row, n.Online()))
	}
	return out
}

// hasRemote reports whether any other machine is linked. remoteRows can only
// render a row against an attached node, so with none attached the answer to
// both listing reads is nil whatever the ledger holds.
func (f *Fleet) hasRemote() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.nodes) > 0
}

// remoteAll and remoteByOwner are the two listing reads. Both answer nil with
// no ledger, which is what makes a single-box deployment's listings the local
// manager's own slice, untouched. They answer nil the same way when nothing is
// linked, because a box that has a ledger but has not been joined to anything
// is the ordinary single-machine deployment, and scanning the whole placements
// table on every `ctl list`, every door listing and every REST list only to
// discard every row is a cost it should not pay.
func (f *Fleet) remoteAll() []*host.Sandbox {
	if f.index == nil || !f.hasRemote() {
		return nil
	}
	rows, err := f.index.List()
	if err != nil {
		f.log.Error("could not read the placement ledger", "err", err)
		return nil
	}
	return f.remoteRows(rows)
}

func (f *Fleet) remoteByOwner(owner string) []*host.Sandbox {
	if f.index == nil || !f.hasRemote() {
		return nil
	}
	rows, err := f.index.ByOwner(owner)
	if err != nil {
		f.log.Error("could not read the placement ledger", "owner", owner, "err", err)
		return nil
	}
	return f.remoteRows(rows)
}

// ---------------------------------------------------------------------------
// ctlops.Sandboxes
// ---------------------------------------------------------------------------

// Get is the shape every read here follows: the local manager is consulted
// first and its answer is authoritative, so a single-box deployment never
// reads a cache, never sees a stale record and never touches the ledger. The
// index exists for other machines' sandboxes and for name allocation.
//
// It asks the holding machine for one record rather than for its inventory.
// This read sits under every authorized operation (ctlops.owned) and every
// browser terminal request, and a machine may hold MaxSandboxesPerNode of them,
// so a whole-inventory copy per lookup is the wrong shape however small the
// fleet is today. The quarantine and node-match rules are remoteRows' — a name
// two machines claim is served by neither.
func (f *Fleet) Get(name string) (*host.Sandbox, bool) {
	if b, ok := f.localMgr.Get(name); ok {
		return b, true
	}
	// Nothing linked means nothing to render whatever the ledger holds, and a
	// box that has a ledger but has not been joined to anything is the ordinary
	// single-machine deployment: it should not read sqlite on every lookup of a
	// name it does not hold.
	if !f.hasRemote() {
		return nil, false
	}
	row, ok := f.rowFor(name)
	if !ok || row.Node == f.localName || row.State == placement.StateQuarantine {
		return nil, false
	}
	n, ok := f.nodeByName(row.Node)
	if !ok {
		return nil, false
	}
	b, ok := n.Box(name)
	if !ok {
		return nil, false
	}
	return f.serve(b, row, n.Online()), true
}

func (f *Fleet) List() []*host.Sandbox {
	out := f.localMgr.List()
	remote := f.remoteAll()
	if len(remote) == 0 {
		return out
	}
	out = append(out, remote...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (f *Fleet) ListByOwner(owner string) []*host.Sandbox {
	out := f.localMgr.ListByOwner(owner)
	remote := f.remoteByOwner(owner)
	if len(remote) == 0 {
		// Handed back exactly as the manager built it — nil when the owner has
		// nothing, in manager order — because parity with the single-box
		// deployment is the contract.
		return out
	}
	out = append(out, remote...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Create places a sandbox and records the placement before the machine is
// asked to build anything. The reservation is the fleet's name allocator: the
// ledger's PRIMARY KEY decides, so two concurrent creates cannot both win, and
// there is no read-then-write window between checking a name and taking it.
func (f *Fleet) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	return f.placed(f.local, name, owner, image, func() (*host.Sandbox, error) {
		return f.local.Create(ctx, name, owner, image, vcpus, memMB)
	})
}

// placed is the reserve/build/undo sequence both ways of making a sandbox
// follow: take the name in the ledger first, then ask the machine, and hand the
// name back if the machine says no. Create and Fork differ only in what build
// does and in which machine it runs on.
func (f *Fleet) placed(n Node, name, owner, image string, build func() (*host.Sandbox, error)) (*host.Sandbox, error) {
	release, err := f.reserve(name, owner, image, n)
	if err != nil {
		return nil, err
	}
	b, err := build()
	if err != nil {
		release()
		return nil, err
	}
	return b, nil
}

// heldLocally reports whether this machine's manager already has a sandbox by
// this name, archived ones included — anything holding a rootfs here holds the
// name with it.
//
// It exists for the one case the ledger cannot answer. Every local sandbox
// normally has a row, so the PRIMARY KEY refuses on its own; a record adoption
// could not place (see adoptLocal) has none, and a name with no row is a free
// name as far as the ledger is concerned. Only the local manager knows better,
// and only it is asked — a remote machine's inventory is a cache, so trusting it
// to veto a name would make a stale entry into a name nobody can use.
func (f *Fleet) heldLocally(name string) bool {
	_, ok := f.localMgr.Get(name)
	return ok
}

// reserve claims a name and returns the undo. The undo must run on every
// failure path: a name reserved for a sandbox that was never built is a name
// nobody can ever use again.
func (f *Fleet) reserve(name, owner, image string, n Node) (release func(), err error) {
	// Placing on another machine is decided by the ledger alone — the manager
	// that would refuse the name never gets asked — so an unplaced local name
	// has to be defended here or the fleet ends up with that name on two
	// machines at once. A placement here needs no such guard: the local manager
	// refuses it a moment later, with its own wording, exactly as it always has.
	if n.Name() != f.localName && f.heldLocally(name) {
		return nil, &host.NameError{Problem: host.NameTaken, Noun: "sandbox", Name: name}
	}
	if f.index == nil {
		return func() {}, nil
	}
	if err := f.index.Reserve(name, owner, n.Name(), image, f.archOf(n)); err != nil {
		if errors.Is(err, placement.ErrTaken) {
			// The manager raises exactly this error for a name it already
			// holds, so colliding with another machine's sandbox reads
			// identically to colliding with one of ours — same message, same
			// exit code, same HTTP status.
			return nil, &host.NameError{Problem: host.NameTaken, Noun: "sandbox", Name: name}
		}
		return nil, err
	}
	return func() {
		if err := f.index.Release(name); err != nil {
			f.log.Error("could not release a reserved sandbox name", "name", name, "err", err)
		}
	}, nil
}

func (f *Fleet) EnsureRunning(ctx context.Context, name string) (*host.Sandbox, error) {
	n, err := f.route("restore", name)
	if err != nil {
		return nil, err
	}
	return n.EnsureRunning(ctx, name)
}

func (f *Fleet) Pause(ctx context.Context, name string) error {
	n, err := f.route("pause", name)
	if err != nil {
		return err
	}
	return n.Pause(ctx, name)
}

func (f *Fleet) Archive(ctx context.Context, name string) error {
	n, err := f.route("archive", name)
	if err != nil {
		return err
	}
	return n.Archive(ctx, name)
}

func (f *Fleet) Resize(ctx context.Context, name string, sizeMB int64) error {
	n, err := f.route("resize", name)
	if err != nil {
		return err
	}
	return n.Resize(ctx, name, sizeMB)
}

func (f *Fleet) Reboot(ctx context.Context, name string) error {
	n, err := f.route("reboot", name)
	if err != nil {
		return err
	}
	return n.Reboot(ctx, name)
}

// Rename moves the ledger row before the machine renames anything, so a crash
// between the two halves leaves the name pointing at the machine that holds
// the rootfs rather than stranding it under a name nothing claims. A refusal
// from the machine rolls the row back.
func (f *Fleet) Rename(ctx context.Context, oldName, newName, owner string) error {
	n, err := f.route("rename", oldName)
	if err != nil {
		return err
	}
	// Renaming another machine's sandbox onto a name this one holds is the same
	// hole reserve guards: the ledger is the only thing consulted, and a name it
	// has no row for reads as free. See heldLocally.
	if n.Name() != f.localName && f.heldLocally(newName) {
		return &host.NameError{Problem: host.NameTaken, Noun: "sandbox", Name: newName}
	}
	moved := false
	if f.index != nil {
		switch err := f.index.Rename(oldName, newName); {
		case err == nil:
			moved = true
		case errors.Is(err, placement.ErrTaken):
			return &host.NameError{Problem: host.NameTaken, Noun: "sandbox", Name: newName}
		case errors.Is(err, placement.ErrNoSuchRow):
			// A sandbox the ledger never learned about. The machine still owns
			// the rename; there is simply no row to move.
		default:
			return err
		}
	}
	if err := n.Rename(ctx, oldName, newName, owner); err != nil {
		if moved {
			if rb := f.index.Rename(newName, oldName); rb != nil {
				f.log.Error("could not roll back a placement rename",
					"from", oldName, "to", newName, "err", rb)
			}
		}
		return err
	}
	return nil
}

// Destroy releases the name only after the machine has actually let go of it.
// The other order would hand the name to somebody else while the rootfs is
// still being deleted.
func (f *Fleet) Destroy(ctx context.Context, name string) error {
	n, err := f.route("rm", name)
	if err != nil {
		return err
	}
	if err := n.Destroy(ctx, name); err != nil {
		return err
	}
	if f.index != nil {
		if err := f.index.Release(name); err != nil {
			// The sandbox is gone either way. A stranded row only blocks the
			// name, which is worth a loud log rather than a failed command the
			// user cannot act on.
			f.log.Error("could not release a destroyed sandbox's placement", "name", name, "err", err)
		}
	}
	return nil
}

// SetPinned carries an error but no context, so it bounds itself: this is the
// one mutating operation whose caller cannot cancel it.
func (f *Fleet) SetPinned(name string, pinned bool) error {
	op := "unpin"
	if pinned {
		op = "pin"
	}
	n, err := f.route(op, name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), setPinnedBudget)
	defer cancel()
	return n.SetPinned(ctx, name, pinned)
}

// ResyncEnv reports nothing, so a machine that is not answering is simply not
// told: the secrets it is missing are pushed again when it reconnects.
func (f *Fleet) ResyncEnv(ctx context.Context, name string) {
	n, err := f.route("secrets.sync", name)
	if err != nil {
		return
	}
	if err := n.ResyncEnv(ctx, name); err != nil {
		f.log.Warn("could not resync a sandbox's environment", "name", name, "err", err)
	}
}

// Touch is the highest-frequency write in the system — every SSH session
// teardown and every terminal keystroke batch — so it reports nothing and
// fails silently.
func (f *Fleet) Touch(name string) {
	n, err := f.route("touch", name)
	if err != nil {
		return
	}
	if err := n.Touch(context.Background(), name); err != nil {
		f.log.Warn("could not record sandbox activity", "name", name, "err", err)
	}
}

// RecordKey is best-effort bookkeeping for the id token's key_fp claim; a
// sandbox is never failed over it.
func (f *Fleet) RecordKey(name, fp string) {
	n, err := f.route("keys.record", name)
	if err != nil {
		return
	}
	if err := n.RecordKey(context.Background(), name, fp); err != nil {
		f.log.Warn("could not record a session key fingerprint", "name", name, "err", err)
	}
}

// ArchivingEnabled answers for the fleet, not for one machine: some machine
// here can archive. The per-machine truth surfaces when the operation is
// actually placed, as the *host.DisabledError the taxonomy already renders —
// making the answer per-sandbox instead would break *host.Manager's structural
// satisfaction of this interface for no gain.
func (f *Fleet) ArchivingEnabled() bool {
	if f.localMgr.ArchivingEnabled() {
		return true
	}
	for _, n := range f.linked() {
		if n.Online() && n.Facts().Archiving {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// ctlops.Templates
// ---------------------------------------------------------------------------

func (f *Fleet) Snapshotter() bool {
	if f.localMgr.Snapshotter() {
		return true
	}
	for _, n := range f.linked() {
		if n.Online() && n.Facts().Snapshots {
			return true
		}
	}
	return false
}

// Snapshots lists an owner's templates across the fleet, newest first, which
// is the order the manager returns its own in.
func (f *Fleet) Snapshots(owner string) []*host.Snapshot {
	out := f.localMgr.Snapshots(owner)
	var remote []*host.Snapshot
	for _, n := range f.linked() {
		for _, s := range n.Templates() {
			if s.Owner != owner {
				continue
			}
			c := *s
			c.Node = n.Name()
			remote = append(remote, &c)
		}
	}
	if len(remote) == 0 {
		return out
	}
	out = append(out, remote...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (f *Fleet) Snapshot(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error) {
	n, err := f.route("snapshot.create", box)
	if err != nil {
		return nil, err
	}
	return n.Snapshotter(ctx, box, snapName, owner)
}

func (f *Fleet) DeleteSnapshot(ctx context.Context, snapName, owner string) error {
	n, _ := f.templateNode(owner, snapName)
	return n.DeleteSnapshot(ctx, snapName, owner)
}

// Fork places the new sandbox on the machine holding the template: a snapshot
// is a reflink source in one machine's image directory, so it can only be
// forked there, and it is architecture-pinned by construction.
func (f *Fleet) Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error) {
	n, image := f.templateNode(owner, snapName)
	return f.placed(n, newName, owner, image, func() (*host.Sandbox, error) {
		return n.Fork(ctx, snapName, newName, owner, vcpus, memMB)
	})
}

// templateNode resolves the machine holding an owner's template, and the
// template's image name for the ledger. A template nobody has resolves to the
// local machine, which answers with the same "snapshot %q not found" a
// single-box deployment always did.
func (f *Fleet) templateNode(owner, snapName string) (Node, string) {
	if s, ok := f.localMgr.SnapshotByName(owner, snapName); ok {
		return f.local, s.Image
	}
	for _, n := range f.linked() {
		for _, s := range n.Templates() {
			if s.Owner == owner && s.Name == snapName {
				return n, s.Image
			}
		}
	}
	return f.local, ""
}
