package routes

import (
	"path/filepath"
	"testing"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck
	return s
}

func mustRoute(t *testing.T, s *Store, sub string, port int, vis string) Route {
	t.Helper()
	if err := s.Upsert(Route{Subdomain: sub, Sandbox: sub, Owner: "alice", Port: port, Visibility: vis}); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.GetBySubdomain(sub)
	if err != nil || !ok {
		t.Fatalf("GetBySubdomain(%q): ok=%v err=%v", sub, ok, err)
	}
	return r
}

// The rule the edge depends on: a port nobody has mentioned is private, whatever
// the route's own port is set to. Making a preview public must not publish the
// debugger listening beside it.
func TestUnmentionedPortsArePrivateEvenOnAPublicRoute(t *testing.T) {
	s := openStore(t)
	r := mustRoute(t, s, "box", 8000, VisibilityPublic)

	if got, err := s.VisibilityForPort(r, 8000); err != nil || got != VisibilityPublic {
		t.Errorf("default port = %q, %v; want public", got, err)
	}
	if got, err := s.VisibilityForPort(r, 5173); err != nil || got != VisibilityPrivate {
		t.Errorf(":5173 = %q, %v; want private", got, err)
	}
}

func TestSetPortVisibilityIsIndependentPerPort(t *testing.T) {
	s := openStore(t)
	r := mustRoute(t, s, "box", 8000, VisibilityPrivate)

	if err := s.SetPortVisibility("box", 5173, VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.VisibilityForPort(r, 5173); got != VisibilityPublic {
		t.Errorf(":5173 = %q, want public", got)
	}
	// Opening one port says nothing about the route's own.
	fresh, _, _ := s.GetBySubdomain("box")
	if fresh.Visibility != VisibilityPrivate {
		t.Errorf("the default port became %q", fresh.Visibility)
	}

	// The route's own port writes through to routes.visibility rather than
	// growing a second opinion in route_ports.
	if err := s.SetPortVisibility("box", 8000, VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	fresh, _, _ = s.GetBySubdomain("box")
	if fresh.Visibility != VisibilityPublic {
		t.Errorf("the default port stayed %q", fresh.Visibility)
	}
	ports, err := s.ListPorts("box")
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 1 || ports[0].Port != 5173 {
		t.Errorf("ListPorts = %+v, want only the non-default 5173", ports)
	}

	if err := s.SetPortVisibility("nope", 5173, VisibilityPublic); err != ErrNoSuchRoute {
		t.Errorf("SetPortVisibility on a missing route = %v, want ErrNoSuchRoute", err)
	}
}

// Promoting a pinned port to default must leave no shadowed row behind: it
// would be ignored while the port is default and would resurface, stale, the
// moment the default moved off it again.
func TestPortChangeClearsTheRowItPromotes(t *testing.T) {
	s := openStore(t)
	mustRoute(t, s, "box", 8000, VisibilityPrivate)
	if err := s.SetPortVisibility("box", 5173, VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	// Move the default port onto 5173. Upsert touches only the port, so the
	// route stays private — and the public row for 5173 must be gone, not
	// merely outvoted.
	if err := s.Upsert(Route{Subdomain: "box", Sandbox: "box", Owner: "alice", Port: 5173}); err != nil {
		t.Fatal(err)
	}
	ports, _ := s.ListPorts("box")
	if len(ports) != 0 {
		t.Fatalf("ListPorts = %+v, want the promoted row dropped", ports)
	}
	r, _, _ := s.GetBySubdomain("box")
	if got, _ := s.VisibilityForPort(r, 5173); got != VisibilityPrivate {
		t.Errorf(":5173 = %q after promotion, want the route's own private", got)
	}
}

func TestPrivatizeAllClosesEveryPortAndKeepsTheListing(t *testing.T) {
	s := openStore(t)
	mustRoute(t, s, "box", 8000, VisibilityPublic)
	for _, p := range []int{5173, 8080} {
		if err := s.SetPortVisibility("box", p, VisibilityPublic); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PrivatizeAll("box")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("PrivatizeAll = %d, want 3 ports", n)
	}
	r, _, _ := s.GetBySubdomain("box")
	if r.Visibility != VisibilityPrivate {
		t.Errorf("default port = %q", r.Visibility)
	}
	ports, _ := s.ListPorts("box")
	if len(ports) != 2 {
		t.Fatalf("PrivatizeAll unpinned the strip: %+v", ports)
	}
	for _, p := range ports {
		if p.Visibility != VisibilityPrivate {
			t.Errorf(":%d stayed %q", p.Port, p.Visibility)
		}
	}
	if _, err := s.PrivatizeAll("nope"); err != ErrNoSuchRoute {
		t.Errorf("PrivatizeAll on a missing route = %v, want ErrNoSuchRoute", err)
	}
}

func TestForgetPortDropsTheListingNotTheGate(t *testing.T) {
	s := openStore(t)
	r := mustRoute(t, s, "box", 8000, VisibilityPrivate)
	if err := s.SetPortVisibility("box", 5173, VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetPort("box", 5173); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.VisibilityForPort(r, 5173); got != VisibilityPrivate {
		t.Errorf(":5173 = %q after forget, want private", got)
	}
	if ports, _ := s.ListPorts("box"); len(ports) != 0 {
		t.Errorf("ListPorts = %+v after forget", ports)
	}
	// Forgetting what was never there is a no-op, not an error: it is what a
	// retry of a forget that already landed looks like.
	if err := s.ForgetPort("box", 4444); err != nil {
		t.Errorf("ForgetPort on an unknown port: %v", err)
	}
}

// A public port must not outlive the subdomain it hangs off. Whoever claims
// that name next inherits the hostname; they must not inherit its open ports.
func TestPortRowsFollowTheirRouteThroughDeleteAndRename(t *testing.T) {
	s := openStore(t)
	mustRoute(t, s, "box", 8000, VisibilityPrivate)
	if err := s.SetPortVisibility("box", 5173, VisibilityPublic); err != nil {
		t.Fatal(err)
	}

	if err := s.RenameSandbox("box", "crate"); err != nil {
		t.Fatal(err)
	}
	if ports, _ := s.ListPorts("box"); len(ports) != 0 {
		t.Errorf("port rows stayed at the old subdomain: %+v", ports)
	}
	moved, err := s.ListPorts("crate")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 || moved[0].Port != 5173 || moved[0].Visibility != VisibilityPublic {
		t.Fatalf("port rows did not follow the rename: %+v", moved)
	}

	if err := s.DeleteBySandbox("crate"); err != nil {
		t.Fatal(err)
	}
	if ports, _ := s.ListPorts("crate"); len(ports) != 0 {
		t.Errorf("port rows survived the sandbox: %+v", ports)
	}
	// The name is free again, and comes back with nothing open on it.
	r := mustRoute(t, s, "crate", 8000, VisibilityPrivate)
	if got, _ := s.VisibilityForPort(r, 5173); got != VisibilityPrivate {
		t.Errorf("a re-used subdomain inherited an open port: :5173 = %q", got)
	}
}

func TestListPortsBySandboxSpansEveryHostname(t *testing.T) {
	s := openStore(t)
	mustRoute(t, s, "box", 8000, VisibilityPrivate)
	if err := s.Upsert(Route{
		Subdomain: "api.box", Sandbox: "box", Owner: "alice", Port: 9000,
	}); err != nil {
		t.Fatal(err)
	}
	for sub, port := range map[string]int{"box": 5173, "api.box": 4000} {
		if err := s.SetPortVisibility(sub, port, VisibilityPublic); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListPortsBySandbox("box")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Subdomain != "api.box" || got[0].Port != 4000 ||
		got[1].Subdomain != "box" || got[1].Port != 5173 {
		t.Fatalf("ListPortsBySandbox = %+v", got)
	}
}
