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
	"strings"
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

// ApproveNode blesses an enrolled machine, keyed on the fingerprint of the key
// that machine authenticates with.
//
// The fingerprint rather than the name is the security of the whole ceremony.
// A node picks its own name at enrolment and the gateway has nothing to check
// it against, so approving a name means trusting a string a stranger chose: a
// stranger who enrols `gpu-01` before the real gpu-01 comes up holds that name,
// the real machine is refused with ErrNameTaken, and the operator who was told
// to expect `gpu-01` approves the wrong machine. A fingerprint cannot be
// claimed that way — it is derived from the key — so the operator reads it off
// the machine's own console, compares it against `node ls`, and what they type
// can only ever bless the machine holding that key.
//
// It is idempotent — approving an already-approved node re-stamps who approved
// it and answers with the row — because the operator's mental model is "make
// sure this is approved", and a failure there would only invite them to remove
// and re-enrol a working node.
func (o *Ops) ApproveNode(ctx context.Context, c Caller, fp string) (NodeInfo, error) {
	const op = "nodes.approve"
	if o.nodes == nil {
		return NodeInfo{}, Disabled(op, notAFleet)
	}
	if err := o.operatorOnly(op, c, "only operators can approve a machine into this fleet."); err != nil {
		return NodeInfo{}, err
	}
	canon, err := canonicalFP(op, fp)
	if err != nil {
		return NodeInfo{}, err
	}
	// Resolved before the write so an unknown fingerprint gets the same masked
	// sentence every other missing object gets, rather than whatever the roster
	// store happens to say.
	if _, err := o.nodeByFP(op, canon); err != nil {
		return NodeInfo{}, err
	}
	n, err := o.nodes.ApproveNode(canon, c.Handle)
	if err != nil {
		return NodeInfo{}, Fail(op, err)
	}
	o.log.Info("node approved", "node", n.Name, "by", c.Handle, "fp", n.FP)
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
			Msg: fmt.Sprintf("node %q still holds %s.", name, CountSandboxes(n.Sandboxes)),
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

// nodeByFP resolves one roster row by the fingerprint of its key, through the
// same listing the operator compares against.
//
// A row with no fingerprint is never a candidate. That is this gateway's own
// entry: it holds no roster row and no key, and is not a machine anybody
// approves. Skipping it explicitly means a caller that reaches here with an
// empty fingerprint gets a not-found rather than the local node.
func (o *Ops) nodeByFP(op, fp string) (NodeInfo, error) {
	list, err := o.nodes.ListNodes()
	if err != nil {
		return NodeInfo{}, Fail(op, err)
	}
	for _, n := range list {
		if n.FP != "" && n.FP == fp {
			return n, nil
		}
	}
	return NodeInfo{}, NotFound(op, "node_fp", fp)
}

// fpPrefix is the hash label every SSH fingerprint on this platform carries.
const fpPrefix = "SHA256:"

// fpBodyLen is how many characters follow that label: SHA-256 is 32 bytes, and
// unpadded base64 encodes 32 bytes in 43 characters.
const fpBodyLen = 43

// canonicalFP normalises what an operator typed into the `SHA256:…` form the
// roster stores, and refuses anything that is not a fingerprint at all.
//
// The label is optional because it is the invariant half of every fingerprint
// here, and an operator who copied only the interesting part has not made a
// mistake worth an exit 2. The body is passed through untouched: it is base64,
// whose alphabet is case-sensitive, so "helpfully" folding its case would turn
// a correct paste into a not-found.
//
// The shape is checked rather than left to the lookup because the failure this
// guards is an operator typing a name — the thing that used to work — and
// `no node in this fleet holds the key gpu-01` explains nothing. What they need
// to be told is that the name was never the safe thing to approve.
func canonicalFP(op, s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", Invalid(op, "missing_fingerprint", "a node fingerprint is required")
	}
	body := strings.TrimPrefix(s, fpPrefix)
	if len(body) == len(s) {
		// Tolerate the label in any case, but only as a label: a body that
		// merely starts with those letters is not one.
		if len(s) > len(fpPrefix) && strings.EqualFold(s[:len(fpPrefix)], fpPrefix) {
			body = s[len(fpPrefix):]
		}
	}
	if !validFPBody(body) {
		return "", &Error{
			Kind: KindInvalid, Op: op, Code: "bad_fingerprint",
			Msg: fmt.Sprintf("%q is not an SSH key fingerprint. A machine is approved by the key it "+
				"holds, not by the name it asked for — the name is chosen by whoever is enrolling, so "+
				"approving one would trust a stranger's word. Read the fingerprint off the machine "+
				"itself (it prints one at startup), check it against `node ls`, and approve that.", s),
			Hint:     "Fingerprints look like SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU.",
			Verbatim: true,
		}
	}
	return fpPrefix + body, nil
}

// validFPBody reports whether s is exactly the unpadded-base64 body of a
// SHA-256 fingerprint. Length is part of the check: a prefix of one is not a
// fingerprint, and accepting prefixes would mean an operator could approve a
// machine having compared only the first few characters of its key — which is
// the property this whole change exists to establish.
func validFPBody(s string) bool {
	if len(s) != fpBodyLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '/':
		default:
			return false
		}
	}
	return true
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

// CountSandboxes renders the count with its noun. "1 sandboxes" in a refusal is
// the kind of sloppiness that makes an operator distrust the number itself.
//
// Exported because the same phrase is the sandbox column of `node ls` in sshgw,
// and an operator reads that column and this refusal side by side — one is the
// answer to the other. Two spellings of the count would read as two different
// numbers.
func CountSandboxes(n int) string {
	if n == 1 {
		return "1 sandbox"
	}
	return fmt.Sprintf("%d sandboxes", n)
}
