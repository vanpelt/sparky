package ctlops

// The fleet's roster, as an operator drives it: list the machines, approve one
// that has enrolled itself, remove one that is leaving.
//
// These are the only operator-gated commands here besides Invite, and they are
// gated for two different reasons. Approving a machine hands it the right to
// run other people's sandboxes, which is the largest grant this control plane
// makes. Listing is gated because a node name is fleet topology: it names a
// machine, it appears in every sandbox record placed on it, and a user who
// cannot approve one has no use for the list.

import (
	"context"
	"fmt"
)

// notAFleet is the KindDisabled sentence every node command answers on a host
// with no roster. It is the whole documentation some operators will read, so it
// says what the host is rather than which store is missing.
const notAFleet = "this host is not a fleet gateway."

// NodeEvicter is the half of a roster that can reach a machine's live link.
//
// It is asserted for rather than folded into NodeRoster because a roster and a
// fleet are two different things: the roster is a table, and only a wiring that
// has joined it to the running fleet can close a connection. Every roster that
// cannot — a unit test's fake, a listing-only adapter — stays a valid
// NodeRoster and simply has no link to revoke.
//
// It takes a sentence rather than an error because the reason is policy's to
// state and the rendering is the link's: ctlops knows why the approval went
// away, and only the fleet knows what a node has to be told and in what shape.
type NodeEvicter interface {
	// EvictNode tears down whatever link the named machine holds and reports
	// whether there was one.
	EvictNode(name, reason string) bool
}

// revokeLink tears down a machine's live link after its approval has been taken
// away — by a removal here, and by whatever else ever withdraws one.
//
// Approval is read once, at the door: admitNode consults the row when a node
// connects and never again. So the durable write is only half of a revocation,
// and the machine that is already connected would otherwise keep its control
// channel, its capacity in the fleet's totals and its data channels for as long
// as it cared to. It runs after the store write, never before: a link dropped
// against a row that is still approved would simply reconnect.
func (o *Ops) revokeLink(name, reason string) {
	ev, ok := o.nodes.(NodeEvicter)
	if !ok {
		return
	}
	if ev.EvictNode(name, reason) {
		o.log.Info("node link closed", "node", name, "reason", reason)
	}
}

// ListNodes reports every machine in the fleet, including this one.
func (o *Ops) ListNodes(ctx context.Context, c Caller) ([]NodeInfo, error) {
	const op = "nodes.list"
	if o.nodes == nil {
		return nil, Disabled(op, notAFleet)
	}
	if err := o.operatorOnly(op, c, "only operators can list the machines in this fleet."); err != nil {
		return nil, err
	}
	list, err := o.nodes.ListNodes()
	if err != nil {
		return nil, Fail(op, err)
	}
	if list == nil {
		// Never nil: a REST client that reads `nodes: null` has to special-case
		// it, and every other listing here already promises an empty array.
		list = []NodeInfo{}
	}
	return list, nil
}

// ApproveNode blesses an enrolled machine. It is idempotent — approving an
// already-approved node re-stamps who approved it and answers with the row —
// because the operator's mental model is "make sure this is approved", and a
// failure there would only invite them to remove and re-enrol a working node.
func (o *Ops) ApproveNode(ctx context.Context, c Caller, name string) (NodeInfo, error) {
	const op = "nodes.approve"
	if o.nodes == nil {
		return NodeInfo{}, Disabled(op, notAFleet)
	}
	if err := o.operatorOnly(op, c, "only operators can approve a machine into this fleet."); err != nil {
		return NodeInfo{}, err
	}
	if name == "" {
		return NodeInfo{}, Invalid(op, "missing_name", "a node name is required")
	}
	// Resolved before the write so an unknown name gets the same
	// `no node named %q` sentence every other missing object gets, rather than
	// whatever the roster store happens to say.
	if _, err := o.node(op, name); err != nil {
		return NodeInfo{}, err
	}
	n, err := o.nodes.ApproveNode(name, c.Handle)
	if err != nil {
		return NodeInfo{}, Fail(op, err)
	}
	o.log.Info("node approved", "node", name, "by", c.Handle, "fp", n.FP)
	return n, nil
}

// RemoveNode drops a machine from the roster. It does not blacklist the key:
// the machine may enrol again and wait for approval, which is what makes this
// the safe thing to do with a node an operator no longer recognises.
func (o *Ops) RemoveNode(ctx context.Context, c Caller, name string) error {
	const op = "nodes.rm"
	if o.nodes == nil {
		return Disabled(op, notAFleet)
	}
	if err := o.operatorOnly(op, c, "only operators can remove a machine from this fleet."); err != nil {
		return err
	}
	if name == "" {
		return Invalid(op, "missing_name", "a node name is required")
	}
	n, err := o.node(op, name)
	if err != nil {
		return err
	}
	if n.Local {
		return &Error{
			Kind: KindConflict, Op: op, Code: "node_is_local",
			Msg:      fmt.Sprintf("node %q is this gateway — it cannot remove itself from its own fleet.", name),
			Verbatim: true,
		}
	}
	if n.Sandboxes > 0 {
		// The sandboxes are on that machine's disk. Removing the row would not
		// delete them, it would only make them unreachable under a name nothing
		// in the fleet claims — so the count is the message, and the operator
		// decides what to do with them.
		return &Error{
			Kind: KindConflict, Op: op, Code: "node_has_sandboxes",
			Msg: fmt.Sprintf("node %q still holds %s.", name, countSandboxes(n.Sandboxes)),
			Hint: "Move or delete them first — removing the node would strand them on a machine " +
				"the fleet no longer knows about.",
			Details:  map[string]any{"sandboxes": n.Sandboxes},
			Verbatim: true,
		}
	}
	if err := o.nodes.RemoveNode(name); err != nil {
		return Fail(op, err)
	}
	o.log.Info("node removed", "node", name, "by", c.Handle)
	// A machine that was connected when it was removed is still connected now.
	// Removal that left it that way would be an approval an operator revoked and
	// a machine that carried on regardless.
	o.revokeLink(name, "it was removed from this fleet")
	return nil
}

// node resolves one roster row through the same listing the operator reads, so
// there is one lookup path and one masked answer for a name that is not there.
func (o *Ops) node(op, name string) (NodeInfo, error) {
	list, err := o.nodes.ListNodes()
	if err != nil {
		return NodeInfo{}, Fail(op, err)
	}
	for _, n := range list {
		if n.Name == name {
			return n, nil
		}
	}
	return NodeInfo{}, NotFound(op, "node", name)
}

// operatorOnly resolves the operator bit from the account store, exactly as
// Invite does and for the same reason: Caller carries no operator field, so a
// transport that has authenticated somebody cannot also decide what they are.
func (o *Ops) operatorOnly(op string, c Caller, sentence string) error {
	u, err := o.accounts.Get(c.Handle)
	if err != nil {
		return Fail(op, err)
	}
	if !u.IsOperator() {
		return Denied(op, "not_operator", sentence)
	}
	return nil
}

// countSandboxes renders the count with its noun. "1 sandboxes" in a refusal is
// the kind of sloppiness that makes an operator distrust the number itself.
func countSandboxes(n int) string {
	if n == 1 {
		return "1 sandbox"
	}
	return fmt.Sprintf("%d sandboxes", n)
}
