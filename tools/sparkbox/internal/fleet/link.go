package fleet

// Joining a machine to the fleet: what happens after the SSH door has resolved
// a key to a roster row and before anything is placed on it.

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

var _ ctlops.NodeEvicter = (*Fleet)(nil)

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
	opts.Hooks = nodelink.Hooks{
		OnInventory: f.Reconcile,
		OnChanged:   f.ApplyChanged,
		OnGone:      f.ApplyGone,
		OnPaused:    f.ApplyPaused,
		// The two that answer the node rather than record what it said. Wired
		// unconditionally: their own first act is to refuse when this
		// deployment has no signing path, which is a sentence the node can log,
		// where a nil hook would be the unregistered-type error a version skew
		// produces and would send an operator looking for the wrong thing.
		OnIdentityToken:     f.IdentityToken,
		OnIdentityDoc:       f.IdentityDoc,
		OnSelfVisibility:    f.SelfVisibility,
		OnSelfPort:          f.SelfPort,
		OnSelfRepos:         f.SelfRepos,
		OnSelfRepoCred:      f.SelfRepoCredential,
		OnSelfRepoAuthStart: f.SelfRepoAuthorizationStart,
		OnSelfRepoAuthPoll:  f.SelfRepoAuthorizationPoll,
		OnSelfPause:         f.SelfPause,
		OnSelfSnapshotPlan:  f.SelfSnapshotPlan,
		OnSelfSnapshot:      f.SelfSnapshot,
		OnSelfSetup:         f.SelfSetup,
		OnSelfSetupResult:   f.SelfSetupResult,
		OnCertificateEnroll: f.enrollCertificate,
	}
	if opts.Grace == 0 {
		// How long this machine still counts as online after it goes quiet.
		// The door builds the ServerOptions and knows nothing about fleet
		// policy, so the fleet fills it in here — and only when the caller left
		// it unset, so a caller with a reason to differ still can.
		opts.Grace = f.nodeGrace
	}

	client, wait, err := nodelink.Serve(ctx, opts)
	if err != nil {
		return err
	}
	defer client.Close()

	n := Remote(client)
	detach := f.linkUp(n)
	defer detach()

	facts := n.Facts()
	f.log.Info("node linked", "node", n.Name(), "arch", facts.Arch, "os", facts.OS,
		"release", facts.Release, "driver", facts.Driver)
	err = wait()
	f.log.Info("node link ended", "node", n.Name(), "err", err)
	return err
}

// ServeDataLane binds an authenticated SSH data connection to the live remote
// control client for its roster node. A lane can race the control link's final
// registration by a few milliseconds; refusing that attempt is intentional,
// because the node supervises lanes independently and retries without
// disturbing control.
func (f *Fleet) ServeDataLane(ctx context.Context, opts nodelink.DataServerOptions) error {
	f.mu.RLock()
	n := f.nodes[opts.Node]
	f.mu.RUnlock()
	remote, ok := n.(*linkedRemote)
	if !ok {
		err := fmt.Errorf("fleet: node %q has no live control generation", opts.Node)
		_ = nodelink.RefuseDataLane(opts.Session, err)
		return err
	}
	return remote.client.ServeDataLane(ctx, opts)
}

// ---------------------------------------------------------------------------
// What a machine tells its gateway between requests
// ---------------------------------------------------------------------------
//
// The node's Emitter sends three events as they happen — sandbox.changed,
// sandbox.gone and sandbox.paused — and nodelink.Client has already done the
// display half of the work by the time any of the hooks below run: its cache is
// updated first, so a listing converges without this file existing at all.
//
// What is left is the half only the gateway can do, and all three share one
// rule. An event is attributed to the AUTHENTICATED link it arrived on, never
// to the node named in its payload, and it may only speak for the sandboxes the
// ledger places on that link. Without the second half, a machine that is merely
// misconfigured — two nodes, one state directory, one duplicated sandbox name —
// hangs up a stranger's terminal every time it pauses, and a machine that is
// compromised does it on purpose to any name it can guess. With it, a node's
// reach is exactly the set of sandboxes the gateway itself put there, which is
// the same boundary the ledger's owner column draws everywhere else.
//
// Every hook runs on the link's read goroutine (see nodelink.Hooks): heartbeats,
// replies and every frame behind it wait on the slowest thing done here.

// SetSessions installs the registry of interactive sessions attached to
// sandboxes, so a pause on another machine can release the terminals this
// gateway is holding for it. Nil — every deployment that has not wired one —
// simply skips the courtesy, exactly as it does on host.Manager.
//
// It takes the same object host.Manager.SetSessions is given, and cmd/sparkbox
// passes the one *sshgw.Gateway to both. There must remain exactly one registry
// on a gateway: sessions are tracked in whichever one the door registered them
// with, so a second would mean a pause hanging up the half of a user's
// terminals that happened to land in the registry it could see. That is the
// invariant internal/xterm depends on when it registers browser terminals
// through the gateway rather than starting a registry of its own.
func (f *Fleet) SetSessions(c host.SessionCloser) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = c
}

// hangUpDrain bounds how long a fleet operation waits for the goodbyes it just
// triggered. Long enough that a client which is still reading gets its bytes
// (sshgw's own per-session write grace is 2s), short enough that a wedged
// browser tab cannot hold somebody's pause open.
const hangUpDrain = 3 * time.Second

// hangUpBefore releases the terminals THIS gateway is holding for a sandbox
// that the machine n is about to stop running, and waits for the goodbyes to be
// written before returning.
//
// It exists because the ordering the local path gets for free does not survive
// a link. host.Manager.pause closes attached sessions and only then calls the
// driver, so the goodbye is written while the guest's sshd is still answering;
// that is what turns a pause into "sparkbox: sandbox %q was paused" plus the
// escape sequences that undo a full-screen TUI, rather than a terminal that
// simply stops. On another machine the same code still runs — but its
// SessionCloser is nodelink's Emitter, which by contract only ENQUEUES a
// sandbox.paused event and returns, and the node then pauses its driver
// immediately. The event still has to cross the wire and be handled here before
// anything is hung up, while the guest's sshd is already gone and the SSH
// channel carrying the terminal with it. The bridge unwinds first, the hang-up
// arrives to an empty registry, and the user gets "shell exited with 1" with no
// goodbye and no terminal restore. Round-trip latency makes the gateway lose
// that race more often, not less, so it cannot be left to timing.
//
// The gateway therefore does its half BEFORE dispatching, which is the same
// ordering the manager uses and the same sidestores rule everywhere else in the
// fleet: the gateway does the part a node cannot, and only for a sandbox on
// another machine. Doing it for a local sandbox would double up with the
// manager's own call for no gain.
//
// The wording is "was paused" — Manager.Pause's exact fragment — because every
// operation routed through here pauses the box on the way (Reboot, Resize,
// Rename, Archive and Snapshot all call Manager.Pause locally), and a user
// whose terminal just closed should read the same sentence on either machine.
// The node's own sandbox.paused event arrives a moment later carrying the same
// news; by then there is nothing left to close and ApplyPaused is a no-op,
// which is exactly what a lossy event stream is allowed to be.
//
// A failure after this point leaves terminals closed on a sandbox that is still
// running. That is not new and not a regression: the local path has the same
// property, because Manager.pause closes sessions before a driver.Pause that
// may itself fail.
func (f *Fleet) hangUpBefore(n Node, sandbox string) {
	if n == nil || n.Name() == f.localName {
		return
	}
	f.mu.RLock()
	sessions := f.sessions
	f.mu.RUnlock()
	d, ok := sessions.(host.SessionDrainer)
	if !ok {
		// Either no registry at all, or one that cannot be waited on. Nothing
		// to do rather than something racy: CloseSandboxSessions here would
		// return before the goodbye was written, which is the very thing this
		// function exists to stop being true.
		if sessions != nil {
			sessions.CloseSandboxSessions(sandbox, "was paused")
		}
		return
	}
	d.DrainSandboxSessions(sandbox, "was paused", hangUpDrain)
}

// ApplyPaused hangs up the sessions attached to a sandbox that another machine
// is about to stop running.
//
// This is the whole reason the event exists. A pause severs a sandbox from
// everything attached to it, and every interactive session in a fleet
// terminates at the gateway — the node holds none — so the machine doing the
// pausing cannot close them and the gateway does not otherwise find out until
// the user's terminal has already been left pointing at a VM that no longer
// answers.
//
// It is a BACKSTOP, not the ordering guarantee, and the difference matters. The
// node emits this before its driver pauses anything, but emitting is not
// delivering: the event still has to cross the link and be handled here, while
// over there the driver pause is already tearing down the guest's sshd and the
// channel every attached session rides on. A pause this gateway asked for is
// therefore hung up by hangUpBefore, on the dispatch path, where the ordering
// is real. What is left for this hook is the pauses the gateway did not ask
// for — a node's own idle reaper, chiefly — where losing the race and closing a
// terminal a moment late still beats leaving it pointed at a stopped VM. By the
// time it runs for a pause the gateway initiated there is nothing left to
// close, and it is a no-op.
//
// The reason is the NODE'S OWN WORDING, relayed. "was paused" and "went idle
// for 30m" are sentences a machine composes about its own policy — the reaper's
// threshold is a node's setting, and this gateway does not know it — and the
// gateway interpolates whichever arrives into the one goodbye it has always
// written. A locally invented fragment would be a plausible-looking guess at
// something the machine already said, and would go quietly wrong the moment the
// two disagreed. It has been scrubbed of anything that could reach a terminal
// as an escape sequence on the way in (nodelink's TypePaused reader).
//
// Called inline, never on a goroutine: CloseSandboxSessions is contractually
// non-blocking — it is called from inside the manager's lock on the local path,
// which is a stronger constraint than this one — and spawning would put the
// goodbye in a race with whatever the link says next about the same sandbox.
func (f *Fleet) ApplyPaused(node string, m nodelink.PausedMsg) {
	if !f.speaksFor(node, m.Name, "pause") {
		return
	}
	f.mu.RLock()
	sessions := f.sessions
	f.mu.RUnlock()
	if sessions == nil {
		return
	}
	f.log.Info("a node paused a sandbox; releasing its attached sessions",
		"node", node, "sandbox", m.Name, "reason", m.Reason)
	sessions.CloseSandboxSessions(m.Name, m.Reason)
}

// ApplyChanged records a lifecycle transition another machine reports.
//
// The record itself needs nothing done to it here: nodelink.Client has already
// replaced its cached row, and every listing renders that row through serve(),
// which stamps owner and node from the ledger. So what is left for this hook is
// the ledger-facing half — a name arriving from a machine the ledger does not
// place it on is either a sandbox this gateway should adopt or two machines
// claiming one name — and that decision belongs to reconciliation, where it can
// be made once against a node's whole inventory instead of one event at a time.
// Deciding it here, per event, would mean adopting a name on the strength of a
// single message that may have arrived out of order and cannot be re-read.
//
// Until then the disagreement is logged rather than acted on, which is the
// conservative answer: nothing is created and nothing is deleted on a node's
// say-so, and an operator watching a machine come up with somebody else's
// sandbox name on it can see that it did.
// One thing IS done here, and it is the half no other path can cover: a
// sandbox that reached running on the machine's OWN initiative needs its
// secret environment, and the gateway's only notice of that is this event.
// The gateway-driven starts (Create, EnsureRunning) push from the call itself;
// a node rebooting and resuming its pinned sandboxes, or restoring one, tells
// nobody and would otherwise bring every box back with an empty managed block.
//
// The filter is the REASON and not the state. Every running sandbox on a node
// emits "touched" on every reaper tick, so triggering on state would mean an
// SSH exec into every remote guest on every tick, forever. host.ReachedRunning
// owns the vocabulary because the manager is what stamps it.
func (f *Fleet) ApplyChanged(node string, m nodelink.ChangedMsg) {
	if !f.speaksFor(node, m.Sandbox.Name, "change") {
		return
	}
	f.log.Debug("node reported a change", "node", node,
		"sandbox", m.Sandbox.Name, "state", m.Sandbox.State, "reason", m.Reason)
	if host.ReachedRunning(m.Reason) {
		// Not the caller's context: this hook has none, and the push must not
		// be cancellable by the link's read goroutine moving on. pushEnv
		// detaches and bounds it.
		f.pushEnvByName(context.Background(), m.Sandbox.Name)
	}
}

// ApplyGone records a sandbox another machine no longer has.
//
// Nothing is deleted here, and that is deliberate rather than unfinished: the
// ledger row is the fleet's name allocation and the user's record of where
// their sandbox went, and releasing it because a machine said so would let a
// node that was wiped, rolled back or merely confused destroy a placement the
// gateway authored. The row outliving its sandbox is visible and recoverable;
// the reverse is a name that silently becomes available to somebody else.
//
// The gateway's own destroy path releases the row after the node has let go
// (Fleet.Destroy), so an ordinary rm has already released it by the time this
// arrives and finds nothing to speak for.
func (f *Fleet) ApplyGone(node string, m nodelink.GoneMsg) {
	if !f.speaksFor(node, m.Name, "gone") {
		return
	}
	f.log.Info("node reported a sandbox as gone", "node", node, "sandbox", m.Name, "reason", m.Reason)
}

// speaksFor reports whether a link may say anything at all about a name.
//
// The answer is the ledger's and only the ledger's: the row must exist and must
// place the name on the machine the event arrived over. node is the
// authenticated roster name the link was admitted under, not the node field in
// the payload — nothing above ever reads that, exactly as nothing reads the
// name a node puts in its hello.
//
// A gateway with no ledger has placed nothing anywhere, so a linked machine's
// events can only be about sandboxes it holds on its own initiative, which this
// gateway is not routing anyone to. Refusing them costs nothing and keeps the
// single-box path from depending on a table that is not open.
// The two ways it says no are logged differently, and the difference is a rate
// as much as a severity. A row that exists and names another machine is a
// genuine conflict — two claimants for one name — which is rare, bounded by the
// ledger, and worth an operator's attention. No row at all is routine until
// reconciliation adopts what a machine already holds, and it is emitted per
// event: a node's manager reports "touched" for every running sandbox on every
// reaper tick, so warning about those would be a line per sandbox per tick,
// forever, about a state nobody can act on yet.
func (f *Fleet) speaksFor(node, sandbox, event string) bool {
	row, ok := f.rowFor(sandbox)
	switch {
	case !ok:
		f.log.Debug("ignoring a node event for a sandbox this gateway has not placed",
			"node", node, "sandbox", sandbox, "event", event)
		return false
	case row.Node != node:
		f.log.Warn("ignoring a node event for a sandbox the ledger places on another machine",
			"node", node, "sandbox", sandbox, "event", event, "placed_on", row.Node)
		return false
	}
	return true
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
func (f *Fleet) linkUp(n Node) func() {
	name := n.Name()
	f.mu.Lock()
	if linked, ok := n.(*linkedRemote); ok {
		linked.guestSelector.setMetrics(name, f.metrics)
		linked.selector.setMetrics(name, f.metrics)
		if binding := f.grpcControls[name]; binding != nil {
			if err := linked.selector.Configure(binding.mode, binding.control); err != nil {
				f.log.Error("could not apply the node's gRPC control selection",
					"node", name, "mode", binding.mode, "err", err)
			}
			if err := linked.selector.ConfigureRollout(binding.rollout); err != nil {
				f.log.Error("could not apply the node's gRPC operation rollout",
					"node", name, "err", err)
			}
			linked.selector.ConfigureShadowInventory(binding.shadow.enabled, binding.shadow.observer)
		}
		if binding := f.routedGuests[name]; binding != nil {
			health := binding.resolver.get()
			if err := linked.guestSelector.Configure(binding.mode, health, binding.dialer); err != nil {
				f.log.Error("could not apply the node's routed guest selection",
					"node", name, "mode", binding.mode, "err", err)
			}
			if err := linked.guestSelector.ConfigureCanary(binding.canaryPercent); err != nil {
				f.log.Error("could not apply the node's routed guest canary",
					"node", name, "percent", binding.canaryPercent, "err", err)
			}
		}
	}
	old, dup := f.nodes[name]
	f.nodes[name] = n
	f.mu.Unlock()
	f.foreign.Store(true)

	if dup {
		f.log.Info("node reconnected; superseding the previous link", "node", name)
		// A linkedRemote may be composed with a durable gRPC control that is
		// shared across SSH link generations. Supersession retires only the old
		// SSH generation; revocation and explicit fleet shutdown still use the
		// composed Node and therefore close both transports.
		if linked, ok := old.(*linkedRemote); ok {
			linked.ssh.Hangup(nodelink.CodeSuperseded, "a newer link for this node replaced this one")
		} else {
			old.Hangup(nodelink.CodeSuperseded, "a newer link for this node replaced this one")
		}
	}
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.nodes[name] == n {
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
	grpc := f.grpcControls[name]
	if grpc != nil {
		delete(f.grpcControls, name)
	}
	if routed := f.routedGuests[name]; routed != nil {
		routed.resolver.set(nil)
		delete(f.routedGuests, name)
	}
	f.mu.Unlock()
	if !linked && grpc == nil {
		return false
	}
	f.log.Info("node link revoked", "node", name, "reason", reason)
	revocation := revoked(name, reason)
	if linked {
		n.Revoke(nodelink.CodeRevoked, revocation)
	}
	if grpc != nil {
		grpc.control.Revoke(nodelink.CodeRevoked, revocation)
	}
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
	// Egress is whether this machine runs a sluice, so `ctl node ls` shows
	// which machines filter and meter and which do not. It is a per-machine
	// fact and there is no fleet-wide answer: a gateway with one and a node
	// without is a normal, working, half-metered fleet, and nothing else says
	// so.
	Egress bool `json:"egress"`
	// Runner is the VMM this machine runs, "" when it has not said. It is on
	// NodeStatus rather than on the embedded nodes.Node because the roster row
	// is written when a node LINKS and this is a live fact — and because an
	// environment can require a VMM, which makes "which machines run what" the
	// first question an operator asks after a placement is refused. A listing
	// that could not answer it would leave them guessing node by node.
	Runner vmm.Runner `json:"runner,omitempty"`
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
		Sandboxes: len(boxes), Running: running, Egress: facts.Sluice,
	}
	st.Name = n.Name()
	st.Arch = facts.Arch
	st.Release = facts.Release
	st.Runner = vmm.Runner(facts.Driver)
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
