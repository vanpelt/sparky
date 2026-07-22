package routes

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertAndGet(t *testing.T) {
	s := openTemp(t)

	if err := s.Upsert(Route{Subdomain: "myvm", Sandbox: "myvm", Owner: "alice", Port: DefaultPort}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetBySubdomain("myvm")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Sandbox != "myvm" || got.Owner != "alice" || got.Port != 8000 {
		t.Fatalf("unexpected route: %+v", got)
	}

	// Same sandbox can change its own port.
	if err := s.Upsert(Route{Subdomain: "myvm", Sandbox: "myvm", Owner: "alice", Port: 3000}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetBySubdomain("myvm")
	if got.Port != 3000 {
		t.Fatalf("port not updated: %+v", got)
	}
}

func TestSubdomainConflict(t *testing.T) {
	s := openTemp(t)
	if err := s.Upsert(Route{Subdomain: "web", Sandbox: "vm-a", Owner: "alice", Port: 8000}); err != nil {
		t.Fatal(err)
	}
	err := s.Upsert(Route{Subdomain: "web", Sandbox: "vm-b", Owner: "bob", Port: 8000})
	if err != ErrSubdomainTaken {
		t.Fatalf("expected ErrSubdomainTaken, got %v", err)
	}
}

func TestValidation(t *testing.T) {
	s := openTemp(t)
	cases := []Route{
		{Subdomain: "Bad_Caps", Sandbox: "vm", Owner: "a", Port: 8000},
		{Subdomain: "ok", Sandbox: "", Owner: "a", Port: 8000},
		{Subdomain: "ok", Sandbox: "vm", Owner: "a", Port: 0},
		{Subdomain: "ok", Sandbox: "vm", Owner: "a", Port: 99999},
	}
	for i, c := range cases {
		if err := s.Upsert(c); err == nil {
			t.Fatalf("case %d %+v: expected validation error", i, c)
		}
	}
}

func TestListAndDelete(t *testing.T) {
	s := openTemp(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Upsert(Route{Subdomain: "myvm", Sandbox: "myvm", Owner: "alice", Port: 8000}))
	must(s.Upsert(Route{Subdomain: "admin-panel", Sandbox: "myvm", Owner: "alice", Port: 9000}))
	must(s.Upsert(Route{Subdomain: "other", Sandbox: "othervm", Owner: "bob", Port: 8000}))

	rs, err := s.ListBySandbox("myvm")
	must(err)
	if len(rs) != 2 {
		t.Fatalf("expected 2 routes for myvm, got %d", len(rs))
	}

	rs, err = s.ListByOwner("bob")
	must(err)
	if len(rs) != 1 || rs[0].Subdomain != "other" {
		t.Fatalf("unexpected owner listing: %+v", rs)
	}

	must(s.Delete("admin-panel"))
	if _, ok, _ := s.GetBySubdomain("admin-panel"); ok {
		t.Fatal("api route should be gone")
	}

	must(s.DeleteBySandbox("myvm"))
	rs, err = s.ListBySandbox("myvm")
	must(err)
	if len(rs) != 0 {
		t.Fatalf("expected myvm routes cleared, got %d", len(rs))
	}
}

func TestRenameSandbox(t *testing.T) {
	s := openTemp(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Upsert(Route{Subdomain: "myvm", Sandbox: "myvm", Owner: "alice", Port: 8000}))
	must(s.Upsert(Route{Subdomain: "admin-panel", Sandbox: "myvm", Owner: "alice", Port: 9000}))
	must(s.SetVisibility("myvm", VisibilityPublic))

	must(s.RenameSandbox("myvm", "newvm"))

	// The default route followed the name, keeping its visibility.
	got, ok, err := s.GetBySubdomain("newvm")
	must(err)
	if !ok || got.Sandbox != "newvm" || got.Port != 8000 || got.Visibility != VisibilityPublic {
		t.Fatalf("default route after rename: ok=%v %+v", ok, got)
	}
	if _, ok, _ := s.GetBySubdomain("myvm"); ok {
		t.Fatal("old default subdomain should be gone")
	}
	// The custom subdomain stayed put but points at the new name.
	got, ok, err = s.GetBySubdomain("admin-panel")
	must(err)
	if !ok || got.Sandbox != "newvm" || got.Port != 9000 {
		t.Fatalf("custom route after rename: ok=%v %+v", ok, got)
	}
}

func TestRenameSandboxRefusesTakenSubdomain(t *testing.T) {
	s := openTemp(t)
	if err := s.Upsert(Route{Subdomain: "myvm", Sandbox: "myvm", Owner: "alice", Port: 8000}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(Route{Subdomain: "taken", Sandbox: "othervm", Owner: "bob", Port: 8000}); err != nil {
		t.Fatal(err)
	}

	if err := s.RenameSandbox("myvm", "taken"); err != ErrSubdomainTaken {
		t.Fatalf("expected ErrSubdomainTaken, got %v", err)
	}
	// The refused rename left everything untouched (single tx).
	got, ok, _ := s.GetBySubdomain("myvm")
	if !ok || got.Sandbox != "myvm" {
		t.Fatalf("myvm route should be unchanged: ok=%v %+v", ok, got)
	}
	got, ok, _ = s.GetBySubdomain("taken")
	if !ok || got.Sandbox != "othervm" {
		t.Fatalf("taken route should be unchanged: ok=%v %+v", ok, got)
	}

	if err := s.RenameSandbox("myvm", "Bad_Caps"); err == nil {
		t.Fatal("expected validation error for a bad new name")
	}
}

func TestRenameSandboxOntoOwnCustomRoute(t *testing.T) {
	s := openTemp(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// The sandbox already has a custom route named exactly like the new name.
	must(s.Upsert(Route{Subdomain: "myvm", Sandbox: "myvm", Owner: "alice", Port: 8000}))
	must(s.Upsert(Route{Subdomain: "web", Sandbox: "myvm", Owner: "alice", Port: 3000}))

	must(s.RenameSandbox("myvm", "web"))

	// The existing route kept its port and serves web.<domain>; the old
	// default row was dropped so the old subdomain is free again.
	got, ok, _ := s.GetBySubdomain("web")
	if !ok || got.Sandbox != "web" || got.Port != 3000 {
		t.Fatalf("custom route after rename: ok=%v %+v", ok, got)
	}
	if _, ok, _ := s.GetBySubdomain("myvm"); ok {
		t.Fatal("old default subdomain should be gone")
	}
	// A new sandbox can claim the released subdomain.
	must(s.Upsert(Route{Subdomain: "myvm", Sandbox: "myvm", Owner: "bob", Port: 8000}))
}

func TestRenameSandboxIsIdempotent(t *testing.T) {
	s := openTemp(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Upsert(Route{Subdomain: "myvm", Sandbox: "myvm", Owner: "alice", Port: 8000}))
	must(s.Upsert(Route{Subdomain: "admin-panel", Sandbox: "myvm", Owner: "alice", Port: 9000}))

	must(s.RenameSandbox("myvm", "newvm"))
	// Renaming again (the manager's crash-repair path) succeeds and changes
	// nothing: the rows are already in the target state.
	must(s.RenameSandbox("myvm", "newvm"))

	got, ok, _ := s.GetBySubdomain("newvm")
	if !ok || got.Sandbox != "newvm" || got.Port != 8000 {
		t.Fatalf("default route after re-run: ok=%v %+v", ok, got)
	}
	got, ok, _ = s.GetBySubdomain("admin-panel")
	if !ok || got.Sandbox != "newvm" || got.Port != 9000 {
		t.Fatalf("custom route after re-run: ok=%v %+v", ok, got)
	}
	if _, ok, _ := s.GetBySubdomain("myvm"); ok {
		t.Fatal("old default subdomain should stay gone")
	}

	// Same for the custom-route collision shape: after the rename completed,
	// the row at the new name points at the new name, so a re-run is a no-op.
	must(s.Upsert(Route{Subdomain: "vm2", Sandbox: "vm2", Owner: "alice", Port: 8000}))
	must(s.Upsert(Route{Subdomain: "web", Sandbox: "vm2", Owner: "alice", Port: 3000}))
	must(s.RenameSandbox("vm2", "web"))
	must(s.RenameSandbox("vm2", "web"))
	got, ok, _ = s.GetBySubdomain("web")
	if !ok || got.Sandbox != "web" || got.Port != 3000 {
		t.Fatalf("custom route after re-run: ok=%v %+v", ok, got)
	}
	if _, ok, _ := s.GetBySubdomain("vm2"); ok {
		t.Fatal("old default subdomain should stay gone")
	}
}

func TestRenameSandboxRepairsPartialRename(t *testing.T) {
	s := openTemp(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Half-migrated state a crash can leave: the sandbox record was renamed
	// and a route for the new name already exists (e.g. written against the
	// renamed record before the repair), but the old rows never moved.
	must(s.Upsert(Route{Subdomain: "myvm", Sandbox: "myvm", Owner: "alice", Port: 8000}))
	must(s.Upsert(Route{Subdomain: "admin-panel", Sandbox: "myvm", Owner: "alice", Port: 9000}))
	must(s.Upsert(Route{Subdomain: "newvm", Sandbox: "newvm", Owner: "alice", Port: 8000}))

	// Renaming again finishes the migration instead of ErrSubdomainTaken.
	must(s.RenameSandbox("myvm", "newvm"))

	got, ok, _ := s.GetBySubdomain("newvm")
	if !ok || got.Sandbox != "newvm" {
		t.Fatalf("default route after repair: ok=%v %+v", ok, got)
	}
	got, ok, _ = s.GetBySubdomain("admin-panel")
	if !ok || got.Sandbox != "newvm" || got.Port != 9000 {
		t.Fatalf("custom route after repair: ok=%v %+v", ok, got)
	}
	if _, ok, _ := s.GetBySubdomain("myvm"); ok {
		t.Fatal("old default subdomain should be gone after repair")
	}
	// A rename onto a subdomain held by a genuinely different sandbox still
	// refuses.
	must(s.Upsert(Route{Subdomain: "other", Sandbox: "othervm", Owner: "bob", Port: 8000}))
	if err := s.RenameSandbox("newvm", "other"); err != ErrSubdomainTaken {
		t.Fatalf("expected ErrSubdomainTaken, got %v", err)
	}
}

func TestRenameSandboxWithoutDefaultRoute(t *testing.T) {
	s := openTemp(t)
	// Only a custom route: nothing matches subdomain=old, so only sandbox
	// pointers move.
	if err := s.Upsert(Route{Subdomain: "web", Sandbox: "myvm", Owner: "alice", Port: 3000}); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameSandbox("myvm", "newvm"); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := s.GetBySubdomain("web")
	if !ok || got.Sandbox != "newvm" {
		t.Fatalf("custom route after rename: ok=%v %+v", ok, got)
	}
	if _, ok, _ := s.GetBySubdomain("newvm"); ok {
		t.Fatal("no default route should have been created")
	}
}

// A subdomain ending in the browser terminal's suffix is dispatched to the
// terminal handler by the proxy edge before any route lookup runs, so a row
// here would be accepted, listed, and then never served. Refusing it at the
// door is the only way the operator finds out.
func TestReservedTerminalSuffixIsRefused(t *testing.T) {
	for _, sub := range []string{"demo-xterm", "web-xterm", "api.demo-xterm"} {
		if ValidSubdomain(sub) {
			t.Errorf("ValidSubdomain(%q) = true, want false", sub)
		}
	}
	// Only as a trailing segment. "xterm-web" is served normally, and the
	// advertised dotted and hyphenated shapes must keep working.
	for _, sub := range []string{"xterm-web", "xtermite", "web-myvm", "api.myvm", "myvm"} {
		if !ValidSubdomain(sub) {
			t.Errorf("ValidSubdomain(%q) = false, want true", sub)
		}
	}
}

func TestUpsertRefusesReservedSuffix(t *testing.T) {
	s := openTemp(t)
	if err := s.Upsert(Route{Subdomain: "demo-xterm", Sandbox: "demo", Owner: "alice", Port: DefaultPort}); err == nil {
		t.Fatal("upsert accepted a subdomain the edge will never route here")
	}
}
