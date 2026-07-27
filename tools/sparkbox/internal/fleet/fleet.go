package fleet

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"golang.org/x/sync/singleflight"
)

// The compile assertions live here rather than beside the interfaces they
// satisfy: ctlops' own test files are in package ctlops, and fleet imports
// ctlops, so an in-package assertion there would be an import cycle. Keeping
// them together means one place to look when a signature moves.
var (
	_ ctlops.Sandboxes = (*Fleet)(nil)
	_ ctlops.Templates = (*Fleet)(nil)
	_ host.Accessor    = (*Fleet)(nil)
	_ Node             = (*localNode)(nil)
	// The shape ctlops type-asserts a sandbox store to when a create names a
	// machine. It is restated rather than exported from there because that
	// interface exists precisely to tell this type from *host.Manager, and a
	// store that fails the assertion does not fail to compile — it answers
	// "this host runs a single machine", which is the right answer for a
	// manager and a silent regression for a fleet. The two are kept honest
	// together by the two-machine tests, which drive --node through ctlops onto
	// a real link.
	_ interface {
		CreateOn(ctx context.Context, node, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error)
	} = (*Fleet)(nil)
)

// setPinnedBudget bounds the one mutating operation whose interface signature
// carries an error but no context. It is generous because the call is rare and
// stingy because a `ctl pin` that hangs is worse than one that fails.
const setPinnedBudget = 10 * time.Second

const (
	markActiveInterval = 10 * time.Second
	markActiveBudget   = 5 * time.Second
)

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
	// LocalNet is the gateway's own egress gateway — the same *netpush.Syncer
	// the console has always pushed to. Nil is a gateway with no sluice, which
	// refuses the two egress verbs with the same sentence a node without one
	// does, rather than answering an empty report.
	LocalNet NetControl
	// Index is the durable name -> node ledger. Nil is a single-node
	// deployment: nothing is placed anywhere but here, so nothing needs
	// recording and the local manager stays the only truth.
	Index *placement.Store
	Log   *slog.Logger
	// Metrics is optional transport-neutral fleet instrumentation.
	Metrics *fleetmetrics.Registry
	// OnCertificateEnroll signs a certificate request from an SSH-authenticated
	// roster node. Nil leaves certificate enrollment disabled. The authenticated
	// node name is supplied by nodelink, never taken from the request payload.
	OnCertificateEnroll func(
		ctx context.Context,
		node string,
		req nodelink.CertificateEnrollRequest,
	) (nodelink.CertificateEnrollResponse, error)

	// Routes, Schedules, Tags and FrontDoor are the gateway-owned stores keyed
	// by a sandbox's NAME. They must be the SAME objects host.Options is given:
	// one set of rows per deployment, reached from here only for the sandboxes
	// the local manager will never be told about. Any of them may be nil, which
	// is what a unit test's fleet and a deployment with no front door pass.
	//
	// A single-box deployment never uses them — every placement is local, and
	// the local manager does its own half — so leaving them unwired costs it
	// nothing. A deployment with a second machine that leaves them unwired
	// silently strands a remote sandbox's routes, schedules and tags. See
	// sidestores.go.
	Routes    RouteRows
	Schedules SandboxRows
	Tags      SandboxRows
	FrontDoor host.FrontDoor

	// NodeGrace is how long a machine that has gone quiet still counts as
	// online. Zero takes nodelink.DefaultGrace (45s, three missed heartbeats).
	//
	// It is a fleet-level setting rather than a per-link one because it is a
	// policy about this deployment's network, not about one machine: a fleet
	// over a tailnet that dips wants a longer one than a fleet on a rack
	// switch. ServeLink hands it to each link it serves, so there is one place
	// to set it. Offline is never a verdict about the sandboxes on a machine —
	// they are still running, on something that stopped talking — it only
	// decides whether an operation is sent or refused with the offline
	// sentence, and whether the record is flagged unreachable.
	NodeGrace time.Duration
	// ReconcileGrace is how long a freshly reserved name may go unreported by
	// the machine it was placed on before that placement is marked orphaned.
	// Zero takes DefaultReconcileGrace; see reconcile.go.
	ReconcileGrace time.Duration
	// Now is the clock reconciliation judges a placement's age against; nil is
	// time.Now. It is settable for the same reason nodelink.ServerOptions.Now
	// is: the alternative is a test that waits out a real grace period.
	Now func() time.Time
}

// Fleet is the router. It holds no lifecycle state of its own: the local
// manager is the truth for local sandboxes, each node's cache is the truth for
// display of its own, and the ledger is the truth for who owns what and where
// it lives.
type Fleet struct {
	local             Node
	localMgr          *host.Manager
	localName         string
	localArch         string
	index             *placement.Store
	log               *slog.Logger
	metrics           *fleetmetrics.Registry
	enrollCertificate func(
		ctx context.Context,
		node string,
		req nodelink.CertificateEnrollRequest,
	) (nodelink.CertificateEnrollResponse, error)
	// sides is the gateway's half of a remote sandbox's lifecycle: the stores
	// keyed by its name that no node has. See sidestores.go.
	sides sides

	// nodeGrace and reconcileGrace are Options' two timers, resolved once; now
	// is the clock the second is measured on. See Options.
	nodeGrace      time.Duration
	reconcileGrace time.Duration
	now            func() time.Time

	mu           sync.RWMutex
	nodes        map[string]Node // linked machines other than this one, by name
	grpcControls map[string]*grpcBinding
	routedGuests map[string]*routedGuestBinding
	placer       Placer // nil is defaultPlacer; see place.go
	// sessions is the registry of interactive sessions attached to sandboxes,
	// so a pause that happens on another machine can hang up the terminals this
	// gateway holds for it. Nil until SetSessions; see link.go.
	sessions host.SessionCloser
	// envPush delivers an owner's secret environment into a sandbox on another
	// machine, which no node can do for itself. Nil until SetEnvPusher; see
	// envsync.go.
	envPush host.EnvPusher
	// rules resolves a sandbox's egress allow-set from its tags. Nil until
	// SetRules; see netplane.go.
	rules Rules
	// identity mints workload credentials for a sandbox on any machine. Nil
	// until SetIdentity — a deployment with no OIDC key — and a node asking is
	// then told so rather than left waiting. See identity.go.
	identity Identity

	// foreign latches once this fleet can hold a record that is not on this
	// machine. See hasRemote.
	foreign atomic.Bool

	// ready coalesces resume/restore operations before they become node RPCs.
	// marks is the independent warm-path gate: one lossy, fire-and-forget
	// activity event per sandbox every ten seconds is enough to beat a
	// minute-scale idle policy.
	ready singleflight.Group
	amu   sync.Mutex
	marks map[string]time.Time

	// The tunneled conns DialContext has handed out, under their own lock.
	//
	// Deliberately not f.mu: that is the lock Get takes, and Get sits under
	// every authorization decision the control plane makes. A proxy opening and
	// closing connections must never be able to make an ownership check wait.
	// See dial.go.
	smu           sync.Mutex
	streams       map[*tracked]struct{}
	streamsClosed bool
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
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	f := &Fleet{
		local:             Local(name, opts.Local, opts.LocalNet),
		localMgr:          opts.Local,
		localName:         name,
		localArch:         arch,
		index:             opts.Index,
		log:               log,
		metrics:           opts.Metrics,
		enrollCertificate: opts.OnCertificateEnroll,
		sides: sides{
			routes:    opts.Routes,
			schedules: opts.Schedules,
			tags:      opts.Tags,
			frontDoor: opts.FrontDoor,
		},
		nodeGrace:      orDuration(opts.NodeGrace, nodelink.DefaultGrace),
		reconcileGrace: orDuration(opts.ReconcileGrace, DefaultReconcileGrace),
		now:            now,
		nodes:          map[string]Node{},
		grpcControls:   map[string]*grpcBinding{},
		routedGuests:   map[string]*routedGuestBinding{},
		marks:          map[string]time.Time{},
		streams:        map[*tracked]struct{}{},
	}
	if err := f.adoptLocal(); err != nil {
		return nil, err
	}
	return f, nil
}

// orDuration is nodelink's own zero-means-the-default helper, restated rather
// than exported from there: a negative duration is a configuration mistake and
// must not become a grace that has already expired.
func orDuration(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// Close drops every linked node and tears down every tunneled connection this
// fleet handed out. The local manager and the ledger belong to whoever opened
// them and are not closed here.
//
// The streams go first, and they go at all because nothing else would end them.
// A tunneled conn's lifetime belongs to whoever holds it — that asymmetry is
// the whole of DialContext's no-close-bound rule — and the busiest holder is an
// http.Transport idle pool, which will keep a connection for a minute and a half
// after the request that dialed it. Dropping the node map without closing them
// leaves those conns riding a link this fleet has stopped accounting for.
func (f *Fleet) Close() error {
	f.closeStreams()
	f.mu.Lock()
	controls := make([]*GRPCControl, 0, len(f.grpcControls))
	for _, binding := range f.grpcControls {
		controls = append(controls, binding.control)
	}
	clear(f.nodes)
	clear(f.grpcControls)
	clear(f.routedGuests)
	f.mu.Unlock()
	for _, control := range controls {
		_ = control.Close()
	}
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
	// One scan, at boot, to learn whether this ledger already places anything
	// elsewhere. Everything written after this point goes through reserve or
	// arrives with a link, and both latch on their own.
	all, err := f.index.List()
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.Node != f.localName {
			f.foreign.Store(true)
			break
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
	f.foreign.Store(true)
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
	// What reconciliation found, refused. Both answers come after the machine's
	// reachability rather than before it, because a machine that is not
	// answering is the more immediate fact and the more actionable sentence —
	// and because "the machine is up and does not have it" would be a lie about
	// one that has since gone away.
	//
	// Both are also strictly after the ownership gate, which resolves through
	// the context-free Fleet.Get: a stranger asking about either of these names
	// is compared against the ledger's owner column and gets the masked
	// "no sandbox named %q" they have always got, having reached no machine and
	// no branch here.
	switch row.State {
	case placement.StateOrphaned:
		return nil, orphanedSandbox(op, name, row.Node)
	case placement.StateQuarantine:
		return nil, contestedSandbox(op, name)
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

// unplaced renders a ledger row no machine is currently answering for.
//
// It exists because the alternative is worse than an incomplete record: a row
// the gateway cannot ask about is still the user's sandbox, and dropping it
// would answer "no sandbox named %q" for something that exists, is running, and
// will be back the moment its machine reconnects. That answer is also the one a
// stranger gets, so leaving it in place would make an outage indistinguishable
// from a deletion for the owner while telling them nothing.
//
// Everything the ledger records is filled in and nothing else is invented. In
// particular State stays empty rather than becoming "paused" or "running": no
// machine has said, this process holds no durable cache of what one last said,
// and a state nobody observed would be a lie that a later reader could mistake
// for an authorization input. Unreachable is what carries the meaning, and it
// is set by serve.
func (f *Fleet) unplaced(row placement.Row) *host.Sandbox {
	return f.serve(&host.Sandbox{
		Name:      row.Name,
		Image:     row.Image,
		CreatedAt: row.CreatedAt,
	}, row, false)
}

// remoteRows renders ledger rows held by other machines. A row whose node is
// not linked, or is linked but has not reported that name, is rendered from the
// ledger alone — see unplaced. Quarantined rows — a name two machines claim —
// are never served at all.
func (f *Fleet) remoteRows(rows []placement.Row) []*host.Sandbox {
	var out []*host.Sandbox
	byNode := map[string]map[string]*host.Sandbox{}
	for _, row := range rows {
		if row.Node == f.localName || row.State == placement.StateQuarantine {
			continue
		}
		n, linked := f.nodeByName(row.Node)
		if !linked {
			out = append(out, f.unplaced(row))
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
			out = append(out, f.unplaced(row))
			continue
		}
		out = append(out, f.serve(b, row, n.Online()))
	}
	return out
}

// hasRemote reports whether this fleet can hold a record that is not on this
// machine. It is a latch, and it only ever goes from false to true.
//
// It used to ask whether any machine was linked right now, which made every
// remote record vanish the instant its link dropped — including from its
// owner's own listing, and including from the ownership check that decides
// whether that owner is told "offline" or the masked "no sandbox named". A
// machine being asleep is not a machine's sandboxes ceasing to exist.
//
// It stays a latch rather than a ledger read because the read it replaces is on
// the path of every `ctl list`, every door listing, every REST list and every
// lookup of a name this machine does not hold. A gateway that has a ledger but
// has never been joined to anything is the ordinary single-box deployment, and
// it should not query sqlite to be told so. Three things set it: a boot scan of
// the ledger (adoptLocal), a machine joining (Attach and linkUp), and a
// placement written for another machine (reserve). Nothing else writes a
// foreign row, so nothing else can be missed.
func (f *Fleet) hasRemote() bool { return f.foreign.Load() }

// remoteAll and remoteByOwner are the two listing reads. Both answer nil with
// no ledger, which is what makes a single-box deployment's listings the local
// manager's own slice, untouched, and nil again when nothing has ever been
// placed elsewhere — see hasRemote.
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
//
// A row whose machine is not answering is still served, out of the ledger
// alone. That is what makes the masking rule hold in both directions when a
// node goes down: the owner's operation gets as far as the router and is told
// the machine is offline, while a stranger asking about the same name is
// compared against the ledger's owner column and gets the byte-identical
// "no sandbox named %q" they have always got — having reached no machine at
// all. Answering "not found" here instead would have given the OWNER that same
// masked sentence, turning a reboot into a disappearance.
func (f *Fleet) Get(name string) (*host.Sandbox, bool) {
	if b, ok := f.localMgr.Get(name); ok {
		return b, true
	}
	// A deployment that has never placed anything elsewhere has nothing to
	// render whatever the ledger holds, and should not read sqlite on every
	// lookup of a name it does not hold.
	if !f.hasRemote() {
		return nil, false
	}
	row, ok := f.rowFor(name)
	if !ok || row.Node == f.localName || row.State == placement.StateQuarantine {
		return nil, false
	}
	if n, linked := f.nodeByName(row.Node); linked {
		if b, ok := n.Box(name); ok {
			return f.serve(b, row, n.Online()), true
		}
	}
	return f.unplaced(row), true
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
//
// Where it goes is the Placer's answer rather than a hardcoded f.local, which
// is the whole of this method's change: the default policy still says "here",
// so a single-box deployment is unaffected, but the decision now has a seam a
// scheduler can be dropped into without touching the sequence below it.
func (f *Fleet) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	return f.create(ctx, "", name, owner, image, vcpus, memMB)
}

// CreateOn is Create with the machine named by the caller — `--node` on the
// new@ door, `"node"` in a REST create body.
//
// It is a separate method rather than an argument on Create because Create's
// signature is ctlops.Sandboxes', which *host.Manager satisfies structurally: a
// single-box gateway wired straight to its manager has no fleet, no ledger and
// no second machine, and must not be made to grow a parameter it can only
// answer one way. ctlops asks whether its store can place by name and says so
// plainly when it cannot.
func (f *Fleet) CreateOn(ctx context.Context, node, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	return f.create(ctx, node, name, owner, image, vcpus, memMB)
}

func (f *Fleet) create(ctx context.Context, prefer, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	n, err := f.pick(Request{
		Owner: owner, Image: image, PreferNode: prefer, VCPUs: vcpus, MemMB: memMB,
	})
	if err != nil {
		return nil, err
	}
	return f.placed(ctx, n, name, owner, image, func() (*host.Sandbox, error) {
		return n.Create(ctx, name, owner, image, vcpus, memMB)
	})
}

// pick resolves a placement request to the machine that will build it.
//
// The shortcut is the single-box path and is exactly the default policy's own
// answer, taken without building a candidate list: with no preference and no
// placer installed, defaultPlacer returns the local machine and nothing else
// can. It is skipped the moment either of those is true, so a deployment that
// has installed a placer always consults it.
func (f *Fleet) pick(req Request) (Node, error) {
	f.mu.RLock()
	p := f.placer
	f.mu.RUnlock()
	if p == nil {
		if req.PreferNode == "" {
			return f.local, nil
		}
		p = defaultPlacer{}
	}
	chosen, err := p.Place(req, f.candidates())
	if err != nil {
		return nil, err
	}
	n, ok := f.nodeByName(chosen)
	if !ok {
		// A placer naming a machine this fleet does not have. It is the same
		// answer a user's own bad --node gets, because from here the two are
		// indistinguishable and inventing a second phrasing for an operator's
		// misconfiguration would only make it harder to search for.
		return nil, ctlops.NotFound("create", "node", chosen)
	}
	return n, nil
}

// placed is the reserve/build/undo sequence both ways of making a sandbox
// follow: take the name in the ledger first, then ask the machine, and hand the
// name back if the machine says no. Create and Fork differ only in what build
// does and in which machine it runs on.
//
// A sandbox built on another machine also gets the gateway-side rows
// Manager.Create mints for a local one — its default subdomain and its
// front-door name — after the build, never before: a create that fails releases
// the name, and rows minted for a sandbox that was never built would outlive it
// under a name somebody else can now take.
func (f *Fleet) placed(ctx context.Context, n Node, name, owner, image string, build func() (*host.Sandbox, error)) (*host.Sandbox, error) {
	release, err := f.reserve(name, owner, image, n)
	if err != nil {
		return nil, err
	}
	b, err := build()
	if err != nil {
		release()
		return nil, err
	}
	if n.Name() != f.localName {
		f.mint(ctx, name, owner)
		// The manager fires this for a sandbox built here; on a node the same
		// hook is nil by construction, so a remote sandbox would boot with an
		// empty managed block and stay that way. See envsync.go.
		f.pushEnv(ctx, b)
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
	if n.Name() != f.localName {
		// Latched before the insert rather than after it: the row exists from
		// the moment the INSERT commits, and a reader that arrived between the
		// commit and a later latch would be told this gateway has nothing
		// elsewhere while the ledger already said otherwise.
		f.foreign.Store(true)
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

// EnsureRunning resumes a sandbox wherever it lives, and — for one on another
// machine that was NOT already running — pushes its secret environment
// afterwards, because the hook that does that automatically only exists on the
// machine that holds the secrets store. See envsync.go.
//
// The push is gated on the state BEFORE the call, and reading it before is the
// whole of the gate: after a successful EnsureRunning the sandbox is running by
// definition, so a gate on the returned record would fire every time. That is
// not the cheap redundancy envsync.go accepts between its case 1 and case 2 —
// this method is on the per-request path. proxy.Server.ServeHTTP calls it for
// EVERY request, sshgw for every session, xterm for every attach, the scheduler
// for every job, so pushing on "it is running now" means one SSH dial,
// handshake and `sudo -n /bin/sh` rewrite of /etc/environment per subresource
// of every page a remote sandbox serves, queued on envsync's per-box mutex and
// each carrying a three-minute budget. It is exactly what
// TestAHeartbeatIsNotATransition rejects for the event path, arriving by a
// hotter door: the trigger has to be the transition, not the state.
//
// A stale cache costs nothing here. If this gateway thought the box was running
// and the machine had to resume it, the machine says so — host.Manager stamps
// "resumed" and the node's emitter sends sandbox.changed, which ApplyChanged
// turns into the push (link.go). Missing a push is a sandbox that gets its
// environment a moment later; pushing on every request is an SSH exec per HTTP
// hit forever.
func (f *Fleet) EnsureReady(ctx context.Context, name string) (*host.Sandbox, error) {
	v, err, _ := f.ready.Do(name, func() (any, error) {
		return f.ensureReady(ctx, name)
	})
	if err != nil {
		return nil, err
	}
	b := *v.(*host.Sandbox)
	return &b, nil
}

func (f *Fleet) ensureReady(ctx context.Context, name string) (*host.Sandbox, error) {
	n, err := f.route("restore", name)
	if err != nil {
		return nil, err
	}
	remote := n.Name() != f.localName
	// The machine's last known picture of this sandbox, which is the only thing
	// that can distinguish a resume from a no-op once the call has returned.
	// Unknown counts as "not running": a name this link has never reported is a
	// box the gateway cannot claim already had its environment.
	wasRunning := false
	if remote {
		if before, ok := n.Box(name); ok && before.State == vmm.StateRunning {
			wasRunning = true
		}
	}
	b, err := n.EnsureReady(ctx, name)
	if err != nil {
		return nil, err
	}
	if remote && !wasRunning {
		f.pushEnv(ctx, b)
	}
	return b, nil
}

// Vitals reads one sandbox's live counters from whichever machine runs it.
//
// It is routed rather than answered from the local manager because a balloon
// and a VMM process can only be asked of the host holding them: before this
// existed, every meter on a sandbox placed on a node drew empty — correctly,
// but permanently, and the further the fleet spreads the more of the platform's
// instrumentation goes dark.
//
// Three things it deliberately does NOT do, each of which would be an easy
// "improvement" that breaks something:
//
//   - It never resumes and never touches. A reading is watching, not working; a
//     terminal tab left open overnight must not keep a sandbox awake, which is
//     the promise /vitals has made since it shipped and the reason it resolves
//     through Get.
//   - It does not fall back to the local manager when the owning machine is
//     unreachable. The local manager holds a DIFFERENT sandbox for any name it
//     happens to share, and drawing that one's CPU under this one's name is a
//     cross-tenant reading with no error and no log line.
//   - It does not cache. The caller polls at its own cadence and a stale
//     instrument is worse than a blank one.
//
// The error is the caller's to swallow: every surface renders "no reading" and
// "the machine is not answering" identically, on purpose, because a viewer can
// act on neither.
func (f *Fleet) Vitals(ctx context.Context, name string) (host.Vitals, error) {
	n, err := f.route("vitals", name)
	if err != nil {
		return host.Vitals{}, err
	}
	return n.Vitals(ctx, name)
}

func (f *Fleet) Pause(ctx context.Context, name string) error {
	n, err := f.route("pause", name)
	if err != nil {
		return err
	}
	f.hangUpBefore(n, name)
	return n.Pause(ctx, name)
}

func (f *Fleet) Archive(ctx context.Context, name string) error {
	n, err := f.route("archive", name)
	if err != nil {
		return err
	}
	f.hangUpBefore(n, name)
	return n.Archive(ctx, name)
}

func (f *Fleet) Resize(ctx context.Context, name string, sizeMB int64) error {
	n, err := f.route("resize", name)
	if err != nil {
		return err
	}
	f.hangUpBefore(n, name)
	return n.Resize(ctx, name, sizeMB)
}

func (f *Fleet) Reboot(ctx context.Context, name string) error {
	n, err := f.route("reboot", name)
	if err != nil {
		return err
	}
	f.hangUpBefore(n, name)
	return n.Reboot(ctx, name)
}

// Rename moves the ledger row before the machine renames anything, so a crash
// between the two halves leaves the name pointing at the machine that holds
// the rootfs rather than stranding it under a name nothing claims. A refusal
// from the machine rolls the row back.
//
// For a sandbox on another machine the ledger row is only half of what the
// gateway has to move. The routes, schedules, tags and front-door rows that
// follow a sandbox's name are all HERE — a node is given none of them, so
// Manager.Rename's own hooks are no-ops over there — and every one of them is
// keyed by the name that is about to change. Left behind, an owner's tag rows
// select nothing for the renamed sandbox (it silently loses every secret those
// tags carried) and a route row keeps pointing, with its owner column, at a name
// that no longer exists. So the gateway does its half before dispatching and
// takes it back if the machine refuses, which is the split W17 describes.
func (f *Fleet) Rename(ctx context.Context, oldName, newName, owner string) error {
	n, err := f.route("rename", oldName)
	if err != nil {
		return err
	}
	remote := n.Name() != f.localName
	if remote {
		// Renaming another machine's sandbox onto a name this one holds is the
		// same hole reserve guards: the ledger is the only thing consulted, and
		// a name it has no row for reads as free. See heldLocally.
		if f.heldLocally(newName) {
			return &host.NameError{Problem: host.NameTaken, Noun: "sandbox", Name: newName}
		}
		// And onto a subdomain somebody else's custom route holds. The manager
		// makes this check with its own routes store, which a node does not
		// have, so on the remote path it is made here or nowhere.
		switch free, err := f.subdomainFree(oldName, newName, owner); {
		case err != nil:
			return err
		case !free:
			return &host.StateError{Code: "subdomain_taken",
				Msg: "subdomain " + strconv.Quote(newName) + " is already taken"}
		}
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
	// Undoing the ledger move is needed on every failure from here down, and
	// forgetting it on one of them is how a sandbox ends up filed under a name
	// its machine has never heard of.
	rollback := func() {
		if moved {
			if rb := f.index.Rename(newName, oldName); rb != nil {
				f.log.Error("could not roll back a placement rename",
					"from", oldName, "to", newName, "err", rb)
			}
		}
	}
	undo := func() {}
	if remote {
		back, err := f.carry(ctx, oldName, newName)
		if err != nil {
			// Routes are fatal (see carry). Nothing else has moved yet and the
			// machine has not been asked to do anything.
			rollback()
			return err
		}
		undo = back
	}
	f.hangUpBefore(n, oldName)
	if err := n.Rename(ctx, oldName, newName, owner); err != nil {
		undo()
		rollback()
		return err
	}
	f.amu.Lock()
	if at, ok := f.marks[oldName]; ok {
		f.marks[newName] = at
		delete(f.marks, oldName)
	}
	f.amu.Unlock()
	return nil
}

// Destroy releases the name only after the machine has actually let go of it.
// The other order would hand the name to somebody else while the rootfs is
// still being deleted.
//
// The gateway's own rows for that sandbox go in between, and the sandwich is
// deliberate. Manager.Destroy deletes a sandbox's routes, schedules and tags
// itself; a node has none of those stores, so for a remote sandbox nothing would
// delete them at all — and Release then hands the NAME to the next person who
// asks for it, on top of one route row that still carries the previous owner's
// handle (their subdomain now answers into the new owner's sandbox) and one
// schedule row that still runs the previous owner's command in it. Sweeping
// while the placement row is still held is what makes "every row keyed by this
// name belongs to the sandbox being destroyed" true at the moment it is read.
func (f *Fleet) Destroy(ctx context.Context, name string) error {
	n, err := f.route("rm", name)
	if err != nil {
		return err
	}
	if err := n.Destroy(ctx, name); err != nil {
		return err
	}
	if n.Name() != f.localName {
		f.sweep(ctx, name)
	}
	if f.index != nil {
		if err := f.index.Release(name); err != nil {
			// The sandbox is gone either way. A stranded row only blocks the
			// name, which is worth a loud log rather than a failed command the
			// user cannot act on.
			f.log.Error("could not release a destroyed sandbox's placement", "name", name, "err", err)
		}
	}
	f.amu.Lock()
	delete(f.marks, name)
	f.amu.Unlock()
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
//
// For a sandbox on another machine the gateway pushes rather than relays.
// Relaying is what the code used to do and it delivered nothing at all: the
// frame reaches the node's Manager.ResyncEnv, which fires a hook that is nil
// over there because a node has no secrets store — so `ctl tag set` on a remote
// sandbox silently changed which secrets it was entitled to and never changed
// the secrets it had. See envsync.go.
func (f *Fleet) ResyncEnv(ctx context.Context, name string) {
	n, err := f.route("secrets.sync", name)
	if err != nil {
		return
	}
	if n.Name() != f.localName {
		f.pushEnvByName(ctx, name)
		return
	}
	if err := n.ResyncEnv(ctx, name); err != nil {
		f.log.Warn("could not resync a sandbox's environment", "name", name, "err", err)
	}
}

// MarkActive is the highest-frequency write in the system. It is coalesced
// before routing so the common suppressed case does not even read the
// placement ledger, then routed and sent asynchronously. gRPC MarkActive is a
// unary RPC rather than the legacy SSH event, so keeping the whole delivery
// off the caller prevents a slow node, placement-store lock, or reconnect from
// re-entering the warm request path.
func (f *Fleet) MarkActive(name string) {
	now := f.now()
	f.amu.Lock()
	if last := f.marks[name]; !last.IsZero() && now.Sub(last) < markActiveInterval {
		f.amu.Unlock()
		return
	}
	f.marks[name] = now
	f.amu.Unlock()

	// Keep the single-host/local behavior exactly synchronous: Manager's
	// implementation only updates in-memory activity and returns immediately,
	// which preserves read-after-mark semantics without a ledger lookup.
	if _, ok := f.localMgr.Get(name); ok {
		if err := f.local.MarkActive(context.Background(), name); err != nil {
			f.log.Warn("could not record sandbox activity", "name", name, "err", err)
		}
		return
	}

	go func() {
		n, err := f.route("touch", name)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), markActiveBudget)
		defer cancel()
		if err := n.MarkActive(ctx, name); err != nil {
			f.log.Warn("could not record sandbox activity", "name", name, "err", err)
		}
	}()
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
	f.hangUpBefore(n, box)
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
	return f.placed(ctx, n, newName, owner, image, func() (*host.Sandbox, error) {
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
