package fleet_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
)

// A single-box deployment is the degenerate fleet, and it has to stay
// indistinguishable from no fleet at all: same records, same order, same empty
// answers. This drives a whole lifecycle through the Fleet and compares its
// four context-free reads against the manager underneath after every step —
// with and without a ledger, because a gateway that gains one must not start
// answering differently.
func TestParityWithTheBareManager(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		name := "no ledger"
		if indexed {
			name = "with a ledger"
		}
		t.Run(name, func(t *testing.T) {
			mgr := newManager(t, host.Options{})
			var index *placement.Store
			if indexed {
				index = newIndex(t)
			}
			f := newFleet(t, mgr, index)
			ctx := context.Background()

			owners := []string{"alice", "bob", "nobody"}
			names := []string{"a1", "a2", "a3", "a3x", "b1", "ghost"}

			check := func(step string) {
				t.Helper()
				for _, n := range names {
					gotBox, gotOK := f.Get(n)
					wantBox, wantOK := mgr.Get(n)
					if gotOK != wantOK || !reflect.DeepEqual(gotBox, wantBox) {
						t.Fatalf("after %s: Get(%q) = (%+v, %v), manager says (%+v, %v)",
							step, n, gotBox, gotOK, wantBox, wantOK)
					}
				}
				if got, want := f.List(), mgr.List(); !reflect.DeepEqual(got, want) {
					t.Fatalf("after %s: List = %s, manager says %s", step, boxNames(got), boxNames(want))
				}
				for _, o := range owners {
					got, want := f.ListByOwner(o), mgr.ListByOwner(o)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("after %s: ListByOwner(%q) = %s, manager says %s",
							step, o, boxNames(got), boxNames(want))
					}
					if got, want := f.Snapshots(o), mgr.Snapshots(o); !reflect.DeepEqual(got, want) {
						t.Fatalf("after %s: Snapshots(%q) = %+v, manager says %+v", step, o, got, want)
					}
				}
				if f.ArchivingEnabled() != mgr.ArchivingEnabled() {
					t.Fatalf("after %s: ArchivingEnabled = %v, manager says %v",
						step, f.ArchivingEnabled(), mgr.ArchivingEnabled())
				}
				if f.Snapshotter() != mgr.Snapshotter() {
					t.Fatalf("after %s: Snapshotter = %v, manager says %v",
						step, f.Snapshotter(), mgr.Snapshotter())
				}
			}

			check("boot")

			steps := []struct {
				what string
				do   func() error
			}{
				{"create a1", func() error { _, err := f.Create(ctx, "a1", "alice", "ubuntu", 1, 512); return err }},
				{"create a2", func() error { _, err := f.Create(ctx, "a2", "alice", "ubuntu", 1, 512); return err }},
				{"create b1", func() error { _, err := f.Create(ctx, "b1", "bob", "ubuntu", 1, 512); return err }},
				{"pause a1", func() error { return f.Pause(ctx, "a1") }},
				{"resume a1", func() error { _, err := f.EnsureReady(ctx, "a1"); return err }},
				{"pin a2", func() error { return f.SetPinned("a2", true) }},
				{"unpin a2", func() error { return f.SetPinned("a2", false) }},
				{"touch a2", func() error { f.MarkActive("a2"); return nil }},
				{"record a key on a2", func() error { f.RecordKey("a2", "SHA256:abc"); return nil }},
				{"resync a2", func() error { f.ResyncEnv(ctx, "a2"); return nil }},
				{"reboot b1", func() error { return f.Reboot(ctx, "b1") }},
				{"grow b1's disk", func() error { return f.Resize(ctx, "b1", 30720) }},
				{"snapshot a2", func() error { _, err := f.Snapshot(ctx, "a2", "golden", "alice"); return err }},
				{"fork golden", func() error { _, err := f.Fork(ctx, "golden", "a3", "alice", 1, 512); return err }},
				{"rename a3", func() error { return f.Rename(ctx, "a3", "a3x", "alice") }},
				{"delete golden", func() error { return f.DeleteSnapshot(ctx, "golden", "alice") }},
				{"destroy b1", func() error { return f.Destroy(ctx, "b1") }},
			}
			for _, s := range steps {
				if err := s.do(); err != nil {
					t.Fatalf("%s: %v", s.what, err)
				}
				check(s.what)
			}

			// Nothing above went anywhere but this machine, so every surviving
			// record still names it and nothing is flagged unreachable.
			for _, b := range f.List() {
				if b.Node != mgr.NodeName() || b.Unreachable {
					t.Fatalf("%s: node=%q unreachable=%v", b.Name, b.Node, b.Unreachable)
				}
			}
			if got := f.Capacities(); len(got) != 1 || got[0].Node != mgr.NodeName() {
				t.Fatalf("a single-box fleet reports %+v", got)
			}
			if node, ok := f.NodeOf("a1"); !ok || node != mgr.NodeName() {
				t.Fatalf("NodeOf(a1) = (%q, %v)", node, ok)
			}
		})
	}
}

// TestLedgerFollowsTheLifecycle pins the other half of the same matrix: with a
// ledger, every name the manager holds has exactly one row on this machine, and
// no name it does not hold has any.
func TestLedgerFollowsTheLifecycle(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	ctx := context.Background()

	mustCreate(t, f, "a1", "alice")
	mustCreate(t, f, "b1", "bob")
	if _, err := f.Snapshot(ctx, "a1", "golden", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fork(ctx, "golden", "a2", "alice", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := f.Rename(ctx, "a2", "a2x", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := f.Destroy(ctx, "b1"); err != nil {
		t.Fatal(err)
	}

	rows, err := index.List()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]placement.Row{}
	for _, r := range rows {
		got[r.Name] = r
	}
	if len(got) != 2 {
		t.Fatalf("ledger holds %+v", rows)
	}
	for _, name := range []string{"a1", "a2x"} {
		r, ok := got[name]
		if !ok {
			t.Fatalf("%s has no placement", name)
		}
		if r.Node != "boxa" || r.Owner != "alice" || r.Arch != "arm64" {
			t.Fatalf("unexpected row for %s: %+v", name, r)
		}
	}
	// The fork's row records the template it came from, which is what a later
	// scheduler needs to know it is architecture-pinned.
	if img := got["a2x"].Image; img != "snap-alice-golden" {
		t.Fatalf("fork placed under image %q", img)
	}
}

// A fleet built without a name falls back to the manager's own, so the string
// the ledger records and the string every sandbox record already carries cannot
// drift apart.
func TestLocalNameDefaultsToTheManagers(t *testing.T) {
	mgr := newManager(t, host.Options{NodeName: "dgx"})
	f, err := fleet.New(fleet.Options{Local: mgr, Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := f.Create(context.Background(), "brave-otter", "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatal(err)
	}
	if b.Node != "dgx" {
		t.Fatalf("record names node %q", b.Node)
	}
	if node, ok := f.NodeOf("brave-otter"); !ok || node != "dgx" {
		t.Fatalf("NodeOf = (%q, %v)", node, ok)
	}
}

func boxNames(boxes []*host.Sandbox) []string {
	out := make([]string, 0, len(boxes))
	for _, b := range boxes {
		out = append(out, b.Name)
	}
	return out
}
