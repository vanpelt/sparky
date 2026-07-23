package host_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// writeState drops a hand-written state file into a manager's dir, so a test
// can open a manager over records that predate a field.
func writeState(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadBackfillsNodeName pins the upgrade path: a box that has been running
// since before records carried a node name must not come back belonging to no
// machine at all, and a name someone else already wrote must survive.
func TestLoadBackfillsNodeName(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, "sandboxes.json", `{
	  "old":     {"name":"old","owner":"alice","image":"ubuntu","state":"paused"},
	  "flagged": {"name":"flagged","owner":"alice","image":"ubuntu","state":"paused","unreachable":true},
	  "kept":    {"name":"kept","owner":"bob","image":"ubuntu","state":"paused","node":"nodec"}
	}`)
	writeState(t, dir, "snapshots.json", `{
	  "snap-alice-golden": {"name":"golden","owner":"alice","image":"snap-alice-golden","from_box":"old"},
	  "snap-bob-golden":   {"name":"golden","owner":"bob","image":"snap-bob-golden","from_box":"kept","node":"nodec"}
	}`)

	m := newManagerInDir(t, dir, host.Options{NodeName: "nodeb"})

	for _, tc := range []struct {
		box  string
		node string
	}{
		{"old", "nodeb"},
		{"flagged", "nodeb"},
		{"kept", "nodec"},
	} {
		b, ok := m.Get(tc.box)
		if !ok {
			t.Fatalf("%s missing after load", tc.box)
		}
		if b.Node != tc.node {
			t.Errorf("%s node = %q, want %q", tc.box, b.Node, tc.node)
		}
		// Unreachable is a verdict a routing layer makes about a machine it
		// cannot reach; loading one off disk would resurrect a stale outage.
		if b.Unreachable {
			t.Errorf("%s came back unreachable", tc.box)
		}
	}

	snaps := map[string]string{}
	for _, s := range append(m.Snapshots("alice"), m.Snapshots("bob")...) {
		snaps[s.Image] = s.Node
	}
	if got := snaps["snap-alice-golden"]; got != "nodeb" {
		t.Errorf("unnamed snapshot node = %q, want %q", got, "nodeb")
	}
	if got := snaps["snap-bob-golden"]; got != "nodec" {
		t.Errorf("named snapshot node = %q, want %q", got, "nodec")
	}
}

// TestCreateStampsNode asserts new records carry the name of the machine that
// made them, without which nothing downstream can route back to it.
func TestCreateStampsNode(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, host.Options{NodeName: "nodeb"})

	b, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatal(err)
	}
	if b.Node != "nodeb" {
		t.Errorf("create returned node %q, want %q", b.Node, "nodeb")
	}
	if got, ok := m.Get("box"); !ok || got.Node != "nodeb" {
		t.Errorf("stored record node = %q ok=%v, want %q", got.Node, ok, "nodeb")
	}
	if got := m.ListByOwner("alice"); len(got) != 1 || got[0].Node != "nodeb" {
		t.Errorf("listed record node = %+v, want one on nodeb", got)
	}
	if b.Unreachable {
		t.Error("a freshly created sandbox reported itself unreachable")
	}

	snap, err := m.Snapshot(ctx, "box", "golden", "alice")
	if err != nil {
		t.Fatal(err)
	}
	// A snapshot is a reflink source on one machine's disk, so it is only
	// fork-able where it was taken.
	if snap.Node != "nodeb" {
		t.Errorf("snapshot node = %q, want %q", snap.Node, "nodeb")
	}
}

// TestNodeNameDefaults pins the single-box default: an unnamed host still calls
// itself something, and it is the same string it has always reported.
func TestNodeNameDefaults(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, host.Options{})

	if got := m.NodeName(); got != "local" {
		t.Errorf("NodeName() = %q, want %q", got, "local")
	}
	b, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatal(err)
	}
	if b.Node != "local" {
		t.Errorf("create returned node %q, want %q", b.Node, "local")
	}
}

// TestCapacityReportsArch covers what an aggregator reads off a capacity report
// to tell one machine from another.
func TestCapacityReportsArch(t *testing.T) {
	for _, tc := range []struct {
		name             string
		opts             host.Options
		node, arch, rel  string
		wantJSONContains []string
		wantJSONMissing  []string
	}{
		{
			name: "described host",
			opts: host.Options{NodeName: "nodeb", Arch: "arm64", Release: "2026-07-22-1200"},
			node: "nodeb", arch: "arm64", rel: "2026-07-22-1200",
			wantJSONContains: []string{`"arch":"arm64"`, `"release":"2026-07-22-1200"`, `"online":true`},
		},
		{
			// The single-box payload must not grow keys it has nothing to say
			// about; both new strings are omitempty.
			name:            "undescribed host",
			opts:            host.Options{},
			node:            "local",
			wantJSONMissing: []string{`"arch"`, `"release"`, `"last_seen_at"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestManager(t, tc.opts).Capacity()
			if c.Node != tc.node || c.Arch != tc.arch || c.Release != tc.rel {
				t.Errorf("capacity = node %q arch %q release %q, want %q %q %q",
					c.Node, c.Arch, c.Release, tc.node, tc.arch, tc.rel)
			}
			// A manager reporting on itself is online by definition, and "now"
			// carries no information, so it leaves LastSeenAt to the aggregator.
			if !c.Online {
				t.Error("a host reported itself offline")
			}
			if c.LastSeenAt != nil {
				t.Errorf("LastSeenAt = %v, want nil", c.LastSeenAt)
			}
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.wantJSONContains {
				if !strings.Contains(string(raw), want) {
					t.Errorf("capacity JSON %s missing %s", raw, want)
				}
			}
			for _, unwanted := range tc.wantJSONMissing {
				if strings.Contains(string(raw), unwanted) {
					t.Errorf("capacity JSON %s should not carry %s", raw, unwanted)
				}
			}
		})
	}
}
