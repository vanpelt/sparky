package nodelink

// The egress plane's node half: what a machine does when its gateway pushes it
// a policy or asks it what its VMs have been talking to.
//
// Both directions are gateway -> node, and the split is not arbitrary. The
// RULES that produce a policy — which tag means what, which sandbox carries
// which tag — live in the gateway's store, and a node holds none of it. The
// METER that produces usage is an eBPF program attached to this machine's own
// tap devices, and the gateway can no more read it from a distance than it can
// read this machine's disk. So policy is pushed and usage is pulled, and each
// travels from the only place that has it.
//
// What crosses the link is sandbox NAMES, never taps. See NetPolicyReq.

import (
	"context"
	"sort"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
)

// NetControl is this machine's egress gateway, narrowed to the two things a
// link asks of it. *netpush.Syncer satisfies it.
//
// Apply takes a SANDBOX-NAME-keyed allow map and is responsible for resolving
// those names to its own taps — a mapping only this machine has, because it
// assigned the slots.
type NetControl interface {
	Enabled() bool
	Apply(ctx context.Context, allow map[string][]string) error
	Usage(ctx context.Context, owner string) (map[string]netpush.VMUsage, error)
}

// CodeNoSluice is what a machine with no egress gateway answers both requests
// with.
//
// It is a refusal and not an empty success, which is the whole reason this code
// exists. An empty policy push "succeeding" would tell a gateway a tagged
// sandbox is filtered when nothing on that machine filters anything; an empty
// usage report would render in an owner's console as a VM that has sent no
// traffic, which is indistinguishable from an idle one and wrong in a way
// nobody would think to question. Both failure modes are silent, and both are
// the kind an operator only finds by not finding them.
const CodeNoSluice = "egress_not_enabled"

// NoSluice is exported because the gateway's own machine refuses with it too:
// internal/fleet's local adapter has no link and no need of this package's
// handlers, but an owner whose VM is on the gateway and whose gateway runs no
// sluice must read the same sentence as one whose VM is on a node.
func NoSluice(node string) error {
	return &ctlops.Error{
		Kind: ctlops.KindDisabled, Op: OpLink, Code: CodeNoSluice, Verbatim: true,
		Msg: "egress control is not enabled on " + node + ": it runs no sluice, so nothing there is " +
			"filtered or metered. Install one with `sparkbox setup --sluice` on that machine.",
	}
}

// registerNetOps installs the egress verbs. net may be nil — a machine with no
// sluice — and both verbs then refuse in the same sentence rather than being
// absent, so a gateway learns WHY instead of getting the unregistered-type
// error a version skew produces.
func registerNetOps(conn *Conn, node string, net NetControl) {
	handle(conn, TypeNetPolicy, func(ctx context.Context, req NetPolicyReq) (EmptyResp, error) {
		if net == nil || !net.Enabled() {
			return EmptyResp{}, NoSluice(node)
		}
		// A nil map and an empty one are the same push and both are legitimate:
		// they mean nothing on this machine is governed, which sluice reads as
		// leaving every tap unrestricted. It is a full snapshot either way.
		return EmptyResp{}, net.Apply(ctx, req.Allow)
	})

	handle(conn, TypeNetUsage, func(ctx context.Context, _ struct{}) (NetUsageResp, error) {
		if net == nil || !net.Enabled() {
			return NetUsageResp{}, NoSluice(node)
		}
		// No owner filter here. Ownership is the gateway's question — it holds
		// the ledger that answers it — and a node filtering on the owner column
		// of its own cache would be a second, staler answer to something that
		// gates what one user is shown about another's VM.
		byName, err := net.Usage(ctx, "")
		if err != nil {
			return NetUsageResp{}, err
		}
		out := NetUsageResp{VMs: make([]netpush.VMUsage, 0, len(byName))}
		for _, u := range byName {
			out.VMs = append(out.VMs, u)
		}
		// Sorted because the source is a map: the gateway re-keys by name and
		// does not care, but a payload that reshuffles on every poll is one
		// nobody can diff between two captures.
		sort.Slice(out.VMs, func(i, j int) bool { return out.VMs[i].Name < out.VMs[j].Name })
		return out, nil
	})
}
