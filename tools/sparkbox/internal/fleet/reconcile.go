package fleet

// Making the gateway's ledger agree with what a machine says it holds.
//
// A node's inventory is its whole picture, sent when it connects and again
// whenever its event queue overflows, which is what makes convergence
// independent of any individual event arriving. This file is what the gateway
// does with one, and it is the only place the two durable truths in the system
// are compared:
//
//   - the node's sandboxes.json, which is the machine holding the rootfs
//     saying what it has;
//   - the placement ledger, which is this gateway saying who owns what and
//     where it put it.
//
// They can disagree for honest reasons. A gateway that was down while a node
// destroyed a sandbox has a row for something that no longer exists. A gateway
// that crashed mid-create has a row the node never finished building — or,
// worse round, a node finished building one whose row was never written. A
// machine that was restored from a backup has sandboxes nobody placed. Each of
// those has a right answer, and each of the wrong answers loses somebody's
// work.
//
// TWO PROHIBITIONS GOVERN EVERYTHING BELOW, and they are the reason this is a
// hundred lines rather than a diff:
//
//  1. A row a node stops reporting is NEVER deleted. A machine that has been
//     wiped, rolled back, reinstalled, or is simply answering with a state
//     directory it cannot read is indistinguishable from one whose sandbox
//     really is gone — and releasing the row would free the user's name for the
//     next person who asked for it while their disk image may still be sitting
//     on that machine. The row is marked and kept forever. A row that outlives
//     its sandbox is visible, loud and recoverable; a name silently handed to
//     somebody else is neither.
//
//  2. The running->paused downgrade host.NewManager performs at boot
//     (manager.go) is NEVER run against another machine's records. That
//     downgrade is correct for the machine it runs on, because a manager coming
//     up means the process that was running those VMs died with them. It is
//     simply false about a node: the gateway restarting does not stop a single
//     VM on another machine, and writing "paused" over a running remote sandbox
//     would be this gateway inventing a state nobody observed. Nothing here
//     writes a sandbox state at all — the ledger's state column is a
//     reconciliation marker with the vocabulary {"", orphaned, quarantine} and
//     never a vmm.State — and the node's own report is the only source of one.

import (
	"errors"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/reserved"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// DefaultReconcileGrace is how long a freshly reserved name is allowed not to
// exist on the machine it was placed on before the gateway concludes it never
// will.
//
// It exists because the reservation deliberately comes FIRST: Fleet.placed
// takes the name in the ledger and only then asks the machine to build
// anything, so that two creates cannot both win a name. Between those two steps
// there is a real window — a firecracker create copies a rootfs — in which the
// ledger says a name is on a machine and the machine, truthfully, has never
// heard of it. An inventory crossing that window (a reconnect, a queue
// overflow) would otherwise orphan a sandbox that is being built at that
// moment, and orphaning refuses every operation that would finish it.
//
// Two minutes is chosen to comfortably outlast a create on a slow disk while
// still being short enough that a genuinely lost row is marked within one
// operator's attention span. It is deliberately not tied to a create's own
// timeout: it also has to cover a gateway that was restarted mid-create, where
// there is no call in flight to read a deadline off.
const DefaultReconcileGrace = 2 * time.Minute

// maxAckNames bounds what the gateway sends back. The ack is a diagnostic — the
// node logs it and acts on none of it — and a machine holding
// MaxSandboxesPerNode sandboxes that the ledger disagrees with wholesale would
// otherwise put a thousand names on one log line at both ends. The disagreement
// itself is logged in full here, one line per name, where an operator is
// already looking.
const maxAckNames = 64

// Reconcile folds one machine's whole picture into the ledger and reports what
// the gateway made of it.
//
// node is the AUTHENTICATED roster name the link was admitted under. The
// inventory's own Node field is a claim and is never read, exactly as the name
// in a hello is never read: a machine that has been told a different name for
// itself, or one hand-crafting frames, must not be able to reconcile against
// another machine's rows.
//
// Four cases, one pass:
//
//  1. The ledger places a name here and the machine reports it — they agree. A
//     row that had been given up on is un-marked, which is what makes every
//     marking below self-healing rather than permanent.
//  2. The ledger places a name here and the machine does not report it — the
//     row is marked orphaned and kept. See the prohibition above.
//  3. The machine reports a name the ledger does not place anywhere — adopt it,
//     if the name is free. This is what makes an interrupted create converge:
//     the REST job that was building it died with the gateway process
//     (ctlops/jobs.go keeps them in memory), but the sandbox on the machine
//     did not, and without adoption it would be a running VM no user could ever
//     reach again.
//  4. Rows for machines that have not connected are not touched at all — they
//     are not in this node's rows and this pass never reads them. A machine
//     being switched off is not its sandboxes ceasing to exist, and they keep
//     rendering out of the ledger alone (see Fleet.unplaced) until it is back.
//
// Snapshots are not reconciled: there is no snapshot ledger. A template is a
// reflink source in one machine's image directory, it is only ever forked
// there, and the link's cache is the whole of the gateway's picture of one.
func (f *Fleet) Reconcile(node string, inv nodelink.InventoryMsg) nodelink.InventoryAck {
	f.log.Debug("node inventory", "node", node,
		"sandboxes", len(inv.Sandboxes), "snapshots", len(inv.Snapshots))

	// A gateway with no ledger has placed nothing anywhere and has nowhere to
	// record an adoption, so there is nothing for an inventory to agree or
	// disagree with. The local name cannot arrive here — ServeLink refuses a
	// link claiming it — but reconciling this machine's own rows against
	// somebody else's report would be the one mistake in this file that could
	// orphan a sandbox that is running in this very process, so it is refused
	// twice.
	if f.index == nil || node == "" || node == f.localName {
		return nodelink.InventoryAck{}
	}

	rows, err := f.index.ByNode(node)
	if err != nil {
		// A ledger that cannot be read is a reason to do nothing, not a reason
		// to conclude that a machine holds nothing: acting on the empty answer
		// would orphan every sandbox on it.
		f.log.Error("could not read the placement ledger to reconcile a node's inventory",
			"node", node, "err", err)
		return nodelink.InventoryAck{}
	}

	reported := make(map[string]bool, len(inv.Sandboxes))
	for _, b := range inv.Sandboxes {
		reported[b.Name] = true
	}

	var ack nodelink.InventoryAck
	now := f.now()
	placed := make(map[string]bool, len(rows))
	for _, row := range rows {
		placed[row.Name] = true
		if reported[row.Name] {
			f.agreed(row)
			continue
		}
		if f.orphan(row, now) {
			ack.Orphaned = append(ack.Orphaned, row.Name)
		}
	}
	for _, b := range inv.Sandboxes {
		if placed[b.Name] {
			continue
		}
		if !f.adopt(node, b) {
			ack.Quarantined = append(ack.Quarantined, b.Name)
		}
	}
	ack.Orphaned = bound(ack.Orphaned)
	ack.Quarantined = bound(ack.Quarantined)
	return ack
}

// agreed is case 1: the ledger and the machine say the same thing.
//
// Nothing has to be done to the record itself. nodelink.Client replaced its
// cached row before this hook ran, and every listing renders that row through
// Fleet.serve, which stamps owner and node from the ledger — so the display
// half has already converged and duplicating it here would create a second
// picture that could disagree with the first.
//
// What is left is the marker, and clearing it is what keeps every marking in
// this file reversible. A machine that was rebooting while its gateway ran a
// reconciliation, or one whose state directory was not mounted yet, comes back
// with its sandboxes and its rows go back to being ordinary. Including a
// quarantine: the row names this machine, this machine says it has it, and the
// contradiction that made the name ambiguous is over. Whatever else was
// claiming that name holds no row and is served to nobody.
func (f *Fleet) agreed(row placement.Row) {
	if row.State == placement.StateOK {
		return
	}
	switch applied, err := f.mark(row, placement.StateOK); {
	case err != nil:
		f.log.Error("could not clear a placement's reconciliation marker",
			"name", row.Name, "node", row.Node, "was", row.State, "err", err)
		return
	case !applied:
		return
	}
	f.log.Info("a sandbox the gateway had given up on is back on its machine",
		"name", row.Name, "node", row.Node, "owner", row.Owner, "was", row.State)
}

// orphan is case 2, and it reports whether the disagreement is one to tell the
// machine about.
//
// The row is kept. Every read still serves it — Fleet.Get and the listings
// render it out of the ledger alone, flagged unreachable, so its owner sees
// their sandbox and is not told it never existed — and every mutation is
// refused with a typed conflict (see Fleet.route) rather than sent to a machine
// that would answer "no such sandbox". Deleting it is the one thing that cannot
// be undone, so it is the one thing not done.
//
// The grace covers a create still in flight: the name is reserved before the
// machine is asked to build anything, so a row younger than the grace has an
// honest reason not to exist yet. It is measured from CreatedAt rather than
// UpdatedAt because UpdatedAt moves when this function marks the row, and a
// grace that reset itself on every marking would keep re-arming forever.
func (f *Fleet) orphan(row placement.Row, now time.Time) bool {
	if row.State == placement.StateOK && now.Sub(row.CreatedAt) < f.reconcileGrace {
		f.log.Debug("a machine does not report a sandbox that was placed on it a moment ago; still building",
			"name", row.Name, "node", row.Node, "age", now.Sub(row.CreatedAt).Round(time.Second))
		// Not reported to the machine either: it is not a disagreement, it is
		// a create the machine has not been asked to finish yet.
		return false
	}
	switch row.State {
	case placement.StateOrphaned, placement.StateQuarantine:
		// Already marked. Said again to the machine, because the disagreement
		// is still live, but not to the log: an inventory arrives on every
		// reconnect, and a node in a backoff loop would otherwise write a line
		// per sandbox per attempt about a condition nobody has resolved yet.
		// Quarantine is not downgraded to orphaned — it is the stronger
		// statement, and only agreed() takes either of them off.
		f.log.Debug("a machine still does not have a sandbox the ledger places on it",
			"name", row.Name, "node", row.Node, "state", row.State)
		return true
	}
	switch applied, err := f.mark(row, placement.StateOrphaned); {
	case err != nil:
		f.log.Error("could not mark a placement orphaned",
			"name", row.Name, "node", row.Node, "err", err)
		return true
	case !applied:
		// The row moved out from under this pass. It is somebody else's row
		// now, so it is not this machine's disagreement to be told about.
		return false
	}
	f.log.Error("a machine no longer has a sandbox the gateway placed on it; the placement is kept, not deleted",
		"name", row.Name, "node", row.Node, "owner", row.Owner,
		"placed", row.CreatedAt.UTC().Format(time.RFC3339),
		"next", "the name stays reserved and the sandbox stays visible to its owner as unreachable; check that machine's state directory before releasing it")
	return true
}

// adopt is case 3: a machine holds a sandbox the ledger does not place
// anywhere. It reports whether the claim was taken up.
//
// This is the ONE place a node-authored owner is read, and it is read only to
// fill a row that does not exist yet — never to change one that does. That
// narrowness is what keeps "a machine lying can only affect its own sandboxes"
// structural: everything downstream authorizes on the ledger's owner column, so
// the worst an adoption can do is create a row for a sandbox that machine
// already holds and already serves. It cannot move, rename or re-own anything
// that was placed by a user.
//
// Which is also why the name and the owner are validated before they are
// written. They are the only two node-authored strings in the system that
// become gateway-authored by being stored, and both flow onward into places
// that assume the platform issued them: a sandbox name becomes a DNS label in
// the synthetic <sandbox>.<node>.sandbox.invalid addressing and a subdomain at
// the edge, and an owner is compared against a session's handle by every
// ownership check there is.
func (f *Fleet) adopt(node string, b nodelink.SandboxRow) bool {
	switch {
	case !reserved.ValidLabel(b.Name) || reserved.Name(b.Name):
		f.log.Error("a machine reports a sandbox under a name this platform does not issue; not adopting it",
			"name", b.Name, "node", node)
		return false
	case !users.ValidHandle(b.Owner):
		// Including the empty owner, which the ledger refuses outright: a
		// placement with no owner is a sandbox nobody can be shown to own, and
		// inventing one would be this gateway handing somebody a machine's
		// sandbox. The local mirror of this case is in adoptLocal, and takes
		// the same answer.
		f.log.Error("a machine reports a sandbox whose owner this gateway does not recognise; not adopting it",
			"name", b.Name, "node", node, "owner", b.Owner)
		return false
	case f.heldLocally(b.Name):
		// This gateway's own sandbox, by that name. It normally has a row and
		// the INSERT below would refuse on its own, but a record adoptLocal
		// could not place — one with no owner — has none, and the ledger would
		// hand its name to another machine. See heldLocally.
		f.log.Error("a machine claims a sandbox name this gateway itself holds; not adopting it",
			"name", b.Name, "node", node)
		return false
	}

	arch := ""
	if n, linked := f.nodeByName(node); linked {
		arch = f.archOf(n)
	}
	switch err := f.index.Reserve(b.Name, b.Owner, node, b.Image, arch); {
	case err == nil:
		// Latched here for the same reason reserve latches: a row for another
		// machine now exists, and a listing that ran before the latch would be
		// told this gateway has nothing elsewhere while the ledger already said
		// otherwise.
		f.foreign.Store(true)
		f.log.Info("adopted a sandbox a machine was already holding",
			"name", b.Name, "node", node, "owner", b.Owner, "image", b.Image, "state", b.State)
		return true
	case errors.Is(err, placement.ErrTaken):
		row, ok := f.rowFor(b.Name)
		if ok && row.Node == node {
			// A create for this machine committed between the snapshot at the
			// top of Reconcile and this insert. The row says what this pass was
			// about to say; there is nothing to disagree about.
			return true
		}
		f.contested(node, b, row, ok)
		return false
	default:
		f.log.Error("could not adopt a sandbox a machine is holding",
			"name", b.Name, "node", node, "err", err)
		return false
	}
}

// contested is the other half of case 3: the name is taken, and not by the
// machine reporting it. Two machines hold a rootfs under one name.
//
// The blueprint's word for this is quarantine, and the question it does not
// answer is WHOSE row gets marked. It matters, because a quarantined row is
// served to nobody: marking the incumbent on a claim alone would let any
// approved machine take any sandbox in the fleet out of service by reporting an
// inventory full of guessed names, which is the cross-tenant reach the ledger
// exists to bound.
//
// So the ledger's answer stands while it is a usable answer. A healthy row was
// authored by this gateway when a user asked for a sandbox; the machine it
// names is where that sandbox is, the claim is refused, and the claimant is
// told so in the ack and in this log. The claimant serves nobody either way,
// because nothing routes to a machine without a row pointing at it.
//
// The row is only marked when it has stopped being an answer: its own machine
// is connected and has disclaimed the name (orphaned), and now another machine
// says it has it. At that point the gateway genuinely cannot tell which
// disk holds the user's data — an operator moved it, or one of the two is
// wrong — and routing to either would be a coin flip with somebody's work on
// it. Neither is served until the contradiction ends, and agreed() ends it the
// moment the row's own machine reports the name again.
func (f *Fleet) contested(node string, b nodelink.SandboxRow, row placement.Row, ok bool) {
	if !ok {
		// The row was released between the failed INSERT and this read. The
		// name is free again and the next inventory will adopt it.
		f.log.Debug("a name was taken and released while adopting it", "name", b.Name, "node", node)
		return
	}
	if row.State != placement.StateOrphaned {
		f.log.Error("a machine claims a sandbox name the ledger places on another machine; ignoring the claim",
			"name", b.Name, "claimed_by", node, "placed_on", row.Node, "owner", row.Owner,
			"next", "nothing was changed and nothing was deleted; that sandbox on "+node+" is not served by this gateway")
		return
	}
	// Compared against the row as it was READ, and against the machine the
	// LEDGER names rather than the one making the claim: the row being marked is
	// the incumbent's, and it is only markable because it was found orphaned.
	switch applied, err := f.mark(row, placement.StateQuarantine); {
	case err != nil:
		f.log.Error("could not quarantine a contested sandbox name",
			"name", row.Name, "node", node, "err", err)
		return
	case !applied:
		return
	}
	f.log.Error("two machines claim one sandbox name and the machine the ledger names has disclaimed it; serving neither",
		"name", row.Name, "placed_on", row.Node, "also_claimed_by", node, "owner", row.Owner,
		"next", "both machines still hold whatever they hold; decide which one has the user's data and move the placement to it")
}

// mark writes a reconciliation marker onto a row this pass READ, and does
// nothing at all if that row has since stopped being the row it read.
//
// This is the one write in the file that has to survive its own snapshot going
// stale. Reconcile reads a machine's rows once (ByNode) and then walks them, one
// statement at a time, holding nothing: it cannot hold a lock, because a pass
// over a machine that has just been rebuilt marks up to MaxSandboxesPerNode rows
// in that many separate sqlite transactions, and every session dispatching an
// operation writes the same table throughout. So a name can be released by a
// `ctl rm` and re-reserved by somebody else's create — on another machine, for
// another user — between the SELECT and this UPDATE. Keyed on the name alone,
// this would stamp "orphaned" onto that new placement, and an orphaned row
// refuses every mutation including the resume that would make it usable, until
// its machine happens to send a whole inventory. The state half of the
// comparison closes the same window for a row that was re-marked by a
// SUPERSEDED link's pass still running against the same machine.
//
// A no-op is therefore the right answer and not a failure: whoever wrote the row
// after us knew more than this pass did. It is reported back as applied=false so
// the caller's own operator-facing line — "a machine no longer has a sandbox the
// gateway placed on it" — is not printed about a row this pass did not touch.
func (f *Fleet) mark(row placement.Row, state string) (applied bool, err error) {
	switch err := f.index.SetRowState(row.Name, row.Node, row.State, state); {
	case err == nil:
		return true, nil
	case errors.Is(err, placement.ErrNoSuchRow):
		f.log.Debug("a placement changed under a reconciliation pass; leaving it alone",
			"name", row.Name, "node", row.Node, "was", row.State, "would_be", state)
		return false, nil
	default:
		return false, err
	}
}

// bound caps a diagnostic list. See maxAckNames.
func bound(names []string) []string {
	if len(names) <= maxAckNames {
		return names
	}
	return names[:maxAckNames]
}
