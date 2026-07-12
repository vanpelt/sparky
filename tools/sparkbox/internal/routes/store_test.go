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
	must(s.Upsert(Route{Subdomain: "api", Sandbox: "myvm", Owner: "alice", Port: 9000}))
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

	must(s.Delete("api"))
	if _, ok, _ := s.GetBySubdomain("api"); ok {
		t.Fatal("api route should be gone")
	}

	must(s.DeleteBySandbox("myvm"))
	rs, err = s.ListBySandbox("myvm")
	must(err)
	if len(rs) != 0 {
		t.Fatalf("expected myvm routes cleared, got %d", len(rs))
	}
}
