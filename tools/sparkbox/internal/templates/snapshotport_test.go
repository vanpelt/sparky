package templates

// The port half of a template: the default port a snapshot's source sandbox
// served on, so a sandbox booted from it lands there too.
//
// The table is deliberately sparse — only a port that differs from the stock
// default is stored — so most of what these tests pin is that ABSENCE reads as
// "the stock default" everywhere, and never as an error a caller has to handle.

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSnapshotPortRoundTrip(t *testing.T) {
	s := openTest(t)

	// A snapshot nobody recorded a port for answers 0, not an error. This is
	// the overwhelmingly common case and every caller indexes it directly.
	port, err := s.SnapshotPort("alice", "nosuch")
	must(t, err)
	if port != 0 {
		t.Errorf("port of an unrecorded snapshot = %d, want 0", port)
	}

	must(t, s.SetSnapshotPort("alice", "websnap", 5173))
	port, err = s.SnapshotPort("alice", "websnap")
	must(t, err)
	if port != 5173 {
		t.Errorf("port = %d, want 5173", port)
	}

	// Re-capturing under a name that already exists REPLACES. Accumulating
	// would leave the next create picking one of two ports by row order.
	must(t, s.SetSnapshotPort("alice", "websnap", 3000))
	port, _ = s.SnapshotPort("alice", "websnap")
	if port != 3000 {
		t.Errorf("port after re-capture = %d, want 3000", port)
	}
}

// The key is (owner, snapshot), so two owners may hold the same snapshot name
// with different ports — which they routinely do, since the namespace is
// per-owner everywhere else in this store too.
func TestSnapshotPortIsPerOwner(t *testing.T) {
	s := openTest(t)
	must(t, s.SetSnapshotPort("alice", "websnap", 5173))
	must(t, s.SetSnapshotPort("bob", "websnap", 3000))

	if p, _ := s.SnapshotPort("alice", "websnap"); p != 5173 {
		t.Errorf("alice's port = %d, want 5173", p)
	}
	if p, _ := s.SnapshotPort("bob", "websnap"); p != 3000 {
		t.Errorf("bob's port = %d, want 3000", p)
	}
	ports, err := s.SnapshotPorts("alice")
	must(t, err)
	if !reflect.DeepEqual(ports, map[string]int{"websnap": 5173}) {
		t.Errorf("alice's ports = %v, want only her own", ports)
	}
}

// SnapshotPorts is the listing path's read. Only snapshots WITH a port appear,
// so indexing it for one that has none gives the same 0 SnapshotPort does —
// which is what lets `snapshot ls` use either without reading them differently.
func TestSnapshotPortsListsOnlyWhatWasRecorded(t *testing.T) {
	s := openTest(t)
	ports, err := s.SnapshotPorts("alice")
	must(t, err)
	if len(ports) != 0 {
		t.Errorf("ports of an owner with none = %v, want empty", ports)
	}

	must(t, s.SetSnapshotPort("alice", "websnap", 5173))
	must(t, s.SetSnapshotPort("alice", "apisnap", 8080))
	ports, err = s.SnapshotPorts("alice")
	must(t, err)
	if !reflect.DeepEqual(ports, map[string]int{"websnap": 5173, "apisnap": 8080}) {
		t.Errorf("ports = %v", ports)
	}
	if ports["stocksnap"] != 0 {
		t.Errorf("an unrecorded snapshot indexes to %d, want 0", ports["stocksnap"])
	}
}

func TestSetSnapshotPortValidates(t *testing.T) {
	s := openTest(t)
	for _, tc := range []struct {
		name     string
		owner    string
		snapshot string
		port     int
	}{
		{"no owner", "", "websnap", 5173},
		{"bad snapshot name", "alice", "Web Snap", 5173},
		{"port zero", "alice", "websnap", 0},
		{"negative port", "alice", "websnap", -1},
		{"port above the range", "alice", "websnap", 65536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.SetSnapshotPort(tc.owner, tc.snapshot, tc.port)
			// The whole family wraps one sentinel so a transport can map it to
			// 400 without matching on the message.
			if !errors.Is(err, ErrInvalidBinding) {
				t.Fatalf("err = %v, want ErrInvalidBinding", err)
			}
		})
	}
	// Nothing was written by any of them.
	ports, err := s.SnapshotPorts("alice")
	must(t, err)
	if len(ports) != 0 {
		t.Errorf("a refused write still landed: %v", ports)
	}
}

// Forgetting is what keeps a deleted snapshot's port from being inherited by
// the next capture to take its name — a create landing on a port from a
// template its owner already threw away.
func TestForgetSnapshotPort(t *testing.T) {
	s := openTest(t)
	must(t, s.SetSnapshotPort("alice", "websnap", 5173))
	must(t, s.ForgetSnapshotPort("alice", "websnap"))
	if p, _ := s.SnapshotPort("alice", "websnap"); p != 0 {
		t.Errorf("port after forget = %d, want 0", p)
	}

	// Forgetting one that was never recorded is not an error: most snapshots
	// have no row, and a delete path that had to know which kind it was
	// deleting would have to ask first.
	must(t, s.ForgetSnapshotPort("alice", "websnap"))
	must(t, s.ForgetSnapshotPort("alice", "neverexisted"))

	// And it reaches only the named owner's row.
	must(t, s.SetSnapshotPort("alice", "shared", 5173))
	must(t, s.SetSnapshotPort("bob", "shared", 3000))
	must(t, s.ForgetSnapshotPort("alice", "shared"))
	if p, _ := s.SnapshotPort("bob", "shared"); p != 3000 {
		t.Errorf("bob's port = %d after alice forgot hers, want 3000", p)
	}
}

// A port outlives the process. CREATE TABLE IF NOT EXISTS is the whole
// migration for this table, so a store opened on a file that predates it gains
// it on the next boot with every existing binding intact.
func TestSnapshotPortsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	s := openAt(t, path)
	must(t, s.SetSnapshotPort("alice", "websnap", 5173))
	if _, _, err := s.Bind("alice", "web", "websnap"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	must(t, s.Close())

	again := openAt(t, path)
	if p, _ := again.SnapshotPort("alice", "websnap"); p != 5173 {
		t.Errorf("port after reopen = %d, want 5173", p)
	}
	bs, err := again.BindingsForOwner("alice")
	must(t, err)
	if len(bs) != 1 || bs[0].Snapshot != "websnap" {
		t.Errorf("bindings after reopen = %+v", bs)
	}
}
