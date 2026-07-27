package fleet

// The egress plane, fleet-wide: pushing every machine its policy, and reading
// one sandbox's meter back off the machine that holds it.
//
// The single fact this file is arranged around is that sluice is a PER-MACHINE
// daemon. It attaches to the taps in front of it and can see no others, and its
// PUT /policy replaces the whole set it enforces. So there is no fleet-wide
// policy object anywhere: there are N machines, each of which must be handed
// exactly its own sandboxes and nothing else. Sending a machine a policy that
// mentions a sandbox on another machine is not merely useless — the names would
// resolve to no tap there, and on the receiving end the snapshot semantics mean
// anything omitted reverts to unrestricted.
//
// The owner column used to resolve rules comes from Fleet.List, which stamps it
// from the LEDGER (see serve). That is not incidental: tags are per-owner, so
// resolving a remote sandbox's rules against the owner a node claimed for it
// would let a machine choose which user's egress policy its VMs run under.

import (
	"context"
	"errors"
	"sort"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// Rules resolves a sandbox's merged egress allow-set from its tags, and whether
// it is governed at all. Satisfied by *netrules.Store.AllowForSandbox, which is
// the same method netpush.Rules names — restated rather than imported so this
// package does not depend on netpush for a one-method interface it uses in a
// different direction.
type Rules interface {
	AllowForSandbox(sandbox, owner string) (allow []string, governed bool, err error)
}

// SetRules installs the egress rule source. Nil — the default — makes PushNet a
// no-op, which is what a deployment with no netrules store has always had.
func (f *Fleet) SetRules(r Rules) { f.rules = r }

// PushNet hands every machine in the fleet its own sandboxes' egress policy.
//
// Best-effort per machine, and deliberately so: one node being offline must not
// stop the gateway reconciling its own VMs, and the snapshot semantics mean the
// missed machine converges on the next push with no state carried between them.
// A machine that runs no sluice refuses with nodelink.CodeNoSluice, which is
// not an error worth a line every thirty seconds — it is a deployment choice,
// and doctor is where it belongs.
//
// It returns the first real failure so a caller doing this on demand (a rule
// edit in the console) can say something, while the periodic loop ignores it.
func (f *Fleet) PushNet(ctx context.Context) error {
	if f.rules == nil {
		return nil
	}
	byNode := map[string]map[string][]string{}
	// Every machine that could hold a sandbox gets an entry, including ones
	// holding none. An empty push is meaningful — it is what REVOKES the last
	// rule off a machine whose sandboxes have all been destroyed or untagged —
	// and skipping it would leave the final policy latched until a restart.
	for _, n := range f.machines() {
		byNode[n.Name()] = map[string][]string{}
	}
	for _, b := range f.List() {
		// A VM with no live tap has nothing to enforce against. Paused and
		// archived ones are skipped here rather than at the machine so the push
		// stays the same size as the running fleet.
		if b.State != vmm.StateRunning {
			continue
		}
		node := b.Node
		if node == "" {
			node = f.localName
		}
		allow, ok := byNode[node]
		if !ok {
			continue // a ledger row for a machine that is not attached
		}
		set, governed, err := f.rules.AllowForSandbox(b.Name, b.Owner)
		if err != nil {
			f.log.Warn("resolve allow-set", "sandbox", b.Name, "owner", b.Owner, "err", err)
			continue
		}
		if !governed {
			continue // untagged → left unrestricted, by omission
		}
		if set == nil {
			set = []string{} // governed deny-all, distinct from ungoverned
		}
		allow[b.Name] = set
	}

	var firstErr error
	for _, n := range f.machines() {
		if err := n.NetPolicy(ctx, byNode[n.Name()]); err != nil {
			if isNoSluice(err) {
				continue
			}
			f.log.Warn("could not push egress policy", "node", n.Name(), "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// NetUsage returns one sandbox's per-domain bandwidth, read from the machine
// that holds it.
//
// It routes by name exactly as every operation in this package does, so a
// sandbox on a node is answered by that node's meter and one on the gateway by
// the gateway's, with the caller unable to tell which happened. The ownership
// gate is the CALLER'S — the console resolves the box through Fleet.Get first,
// which masks a stranger's lookup as a miss — and is not repeated here.
//
// A machine that reports usage for a name it was not asked about is ignored:
// only the requested sandbox is projected out of the answer.
func (f *Fleet) NetUsage(ctx context.Context, name string) (netpush.VMUsage, error) {
	n, err := f.route("net.usage", name)
	if err != nil {
		return netpush.VMUsage{}, err
	}
	byName, err := n.NetUsage(ctx)
	if err != nil {
		return netpush.VMUsage{}, err
	}
	u, ok := byName[name]
	if !ok {
		// The machine meters, and has nothing for this VM. That is a real
		// answer — a sandbox that has just booted, or one whose tap the meter
		// has not seen a packet on — and it is NOT the same as a machine with
		// no meter, which refused above.
		return netpush.VMUsage{Name: name, Domains: []netpush.DomainUsage{}}, nil
	}
	u.Name = name
	return u, nil
}

// NetMetered reports whether the machine holding a sandbox meters it at all, so
// a caller can render "not measured here" rather than a panel of zeroes. It
// costs a round trip and is meant for the surface that is about to render one.
func (f *Fleet) NetMetered(name string) bool {
	n, err := f.route("net.usage", name)
	if err != nil {
		return false
	}
	return n.Facts().Sluice
}

// machines is every attached machine, the gateway's own included, name-sorted.
// The local node is not in f.nodes — nothing links to it — so it is added here,
// which is the same shape nodeByName uses.
func (f *Fleet) machines() []Node {
	out := append([]Node{f.local}, f.linked()...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// isNoSluice recognises the one refusal that is a configuration statement
// rather than a fault, wherever it was raised — the local adapter builds it
// in process and a node's arrives rebuilt from the wire, and both carry the
// same code because ctlops.FromWire preserves it.
func isNoSluice(err error) bool {
	var typed *ctlops.Error
	if !errors.As(err, &typed) {
		return false
	}
	return typed.Code == nodelink.CodeNoSluice
}
