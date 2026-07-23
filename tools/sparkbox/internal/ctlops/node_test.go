package ctlops

// What removing a machine from the roster has to do to the machine.

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// liveRoster is a roster wired to a running fleet, which is what makes it able
// to answer the one question a plain table cannot: is that machine connected
// right now, and can you hang up on it. The fakes elsewhere in this suite are
// deliberately not — a roster with no fleet behind it is a real shape, and the
// test below pins that it still works.
type liveRoster struct {
	list    []NodeInfo
	linked  map[string]bool
	evicted []string
	reason  string
}

func (r *liveRoster) ListNodes() ([]NodeInfo, error) {
	return append([]NodeInfo(nil), r.list...), nil
}

func (r *liveRoster) ApproveNode(name, by string) (NodeInfo, error) {
	for i := range r.list {
		if r.list[i].Name == name {
			r.list[i].Status = "approved"
			r.list[i].ApprovedBy = by
			return r.list[i], nil
		}
	}
	return NodeInfo{}, errors.New("no such node")
}

func (r *liveRoster) RemoveNode(name string) error {
	for i := range r.list {
		if r.list[i].Name == name {
			r.list = append(r.list[:i:i], r.list[i+1:]...)
			return nil
		}
	}
	return errors.New("no such node")
}

func (r *liveRoster) EvictNode(name, reason string) bool {
	r.evicted = append(r.evicted, name)
	r.reason = reason
	if !r.linked[name] {
		return false
	}
	delete(r.linked, name)
	return true
}

var _ NodeEvicter = (*liveRoster)(nil)

// TestRemovingANodeClosesItsLiveLink is the difference between revoking an
// approval and merely writing down that you did.
//
// The roster row is read at one moment only — when a machine connects. Nothing
// re-reads it afterwards, and the link is built with no idle timeout so that a
// node may stay connected for weeks. A removal that stopped at the store would
// therefore leave the removed machine holding its control channel, reporting
// capacity and serving streams, indefinitely.
func TestRemovingANodeClosesItsLiveLink(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	roster := &liveRoster{
		list: []NodeInfo{
			{Name: "here", Status: "approved", Online: true, Local: true},
			{Name: "node-b", Status: "approved", Online: true, FP: "SHA256:bbbb"},
		},
		linked: map[string]bool{"node-b": true},
	}
	r.ops.nodes = roster

	if err := r.ops.RemoveNode(ctx, Caller{Handle: "opsy"}, "node-b"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	if !slices.Equal(roster.evicted, []string{"node-b"}) {
		t.Fatalf("evictions = %v, want the link for node-b closed", roster.evicted)
	}
	if roster.reason == "" {
		t.Error("the node was hung up on with no reason to log")
	}
	if roster.linked["node-b"] {
		t.Error("node-b was removed from the roster and is still linked")
	}
}

// TestRemovingANodeWithoutALiveFleetStillWorks is the other half of making the
// eviction optional: a roster that is only a table — every fake in this suite,
// and any wiring that has not been joined to a running fleet — has no link to
// close, and a removal through it must be exactly what it always was.
func TestRemovingANodeWithoutALiveFleetStillWorks(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.withNodes()

	// "newcomer" is the pending row: it holds nothing, so the sandbox guard
	// does not fire and the removal reaches the store.
	if err := r.ops.RemoveNode(ctx, Caller{Handle: "opsy"}, "newcomer"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	list, err := r.ops.ListNodes(ctx, Caller{Handle: "opsy"})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	for _, n := range list {
		if n.Name == "newcomer" {
			t.Fatal("newcomer survived its own removal")
		}
	}
}
