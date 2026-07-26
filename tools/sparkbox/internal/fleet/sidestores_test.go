package fleet_test

// The gateway's half of a remote sandbox's lifecycle.
//
// Every test here exists because the same operation on a LOCAL sandbox is
// already correct and invisible: host.Manager mints the default route on create,
// moves routes/schedules/tags/front-door on rename and deletes them on destroy,
// because it is holding all four stores. A node holds none of them, so unless
// the fleet does that work for a sandbox on another machine, nobody does — and
// the failures are silent and durable rather than loud: a tag row stranded under
// a dead name means a sandbox that quietly stops getting its owner's secrets,
// and a route row stranded under a name the ledger has since handed to somebody
// else means one user's subdomain answering into another user's sandbox with the
// first user's handle in the owner column.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeRows is a name-keyed side store: the schedules table, the tags table.
// Only the two operations the fleet performs on one are modelled, because only
// those two are what is under test — the real stores' SQL is covered where they
// live.
type fakeRows struct {
	mu   sync.Mutex
	rows map[string][]string // sandbox -> the row labels filed under it
	fail error               // what RenameSandbox answers, if anything
}

func newRows(seed map[string][]string) *fakeRows {
	if seed == nil {
		seed = map[string][]string{}
	}
	return &fakeRows{rows: seed}
}

func (s *fakeRows) DeleteBySandbox(sandbox string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, sandbox)
	return nil
}

func (s *fakeRows) RenameSandbox(old, next string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if v, ok := s.rows[old]; ok {
		delete(s.rows, old)
		s.rows[next] = v
	}
	return nil
}

func (s *fakeRows) under(sandbox string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.rows[sandbox]...)
}

// fakeRoutes is the one side store that is authorization-bearing, so it is
// modelled by subdomain as well as by sandbox — a route row's owner column is
// what gates a private route, and the collision check a rename makes reads it.
type fakeRoutes struct {
	mu   sync.Mutex
	byID map[string]routes.Route // subdomain -> row
	fail error
}

func newRoutes() *fakeRoutes { return &fakeRoutes{byID: map[string]routes.Route{}} }

func (s *fakeRoutes) Upsert(r routes.Route) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.byID[r.Subdomain]; ok && cur.Sandbox != r.Sandbox {
		return routes.ErrSubdomainTaken
	}
	s.byID[r.Subdomain] = r
	return nil
}

func (s *fakeRoutes) GetBySubdomain(sub string) (routes.Route, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[sub]
	return r, ok, nil
}

func (s *fakeRoutes) DeleteBySandbox(sandbox string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub, r := range s.byID {
		if r.Sandbox == sandbox {
			delete(s.byID, sub)
		}
	}
	return nil
}

func (s *fakeRoutes) RenameSandbox(old, next string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	for sub, r := range s.byID {
		if r.Sandbox != old {
			continue
		}
		delete(s.byID, sub)
		r.Sandbox = next
		if sub == old {
			// The default route follows the name, exactly as the real store's
			// RenameSandbox moves <old>.<domain> to <new>.<domain>.
			sub = next
			r.Subdomain = next
		}
		s.byID[sub] = r
	}
	return nil
}

func (s *fakeRoutes) subdomains() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for sub, r := range s.byID {
		out = append(out, sub+"->"+r.Sandbox+"@"+r.Owner)
	}
	sort.Strings(out)
	return out
}

// fakeDoor records the per-sandbox DNS the edge would publish.
type fakeDoor struct {
	mu    sync.Mutex
	names map[string]bool
	log   []string
}

func newDoor() *fakeDoor { return &fakeDoor{names: map[string]bool{}} }

func (d *fakeDoor) Ensure(_ context.Context, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.names[name] = true
	d.log = append(d.log, "+"+name)
}

func (d *fakeDoor) Remove(_ context.Context, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.names, name)
	d.log = append(d.log, "-"+name)
}

func (d *fakeDoor) published() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for n := range d.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// sideRig is a two-machine fleet with every gateway-side store wired.
type sideRig struct {
	f         *fleet.Fleet
	index     *placement.Store
	nodeb     *buildingNode
	routes    *fakeRoutes
	schedules *fakeRows
	tags      *fakeRows
	door      *fakeDoor
}

func newSideRig(t *testing.T) *sideRig {
	t.Helper()
	r := &sideRig{
		index: newIndex(t), routes: newRoutes(),
		schedules: newRows(nil), tags: newRows(nil), door: newDoor(),
	}
	// The local manager gets NO side stores, which is what makes every
	// assertion below unambiguous: any row that moves was moved by the fleet.
	mgr := newManager(t, host.Options{})
	f, err := fleet.New(fleet.Options{
		Local: mgr, LocalName: mgr.NodeName(), LocalArch: "arm64", Index: r.index,
		Log: discardLog(), Routes: r.routes, Schedules: r.schedules, Tags: r.tags,
		FrontDoor: r.door,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	r.f = f
	r.nodeb = newBuildingNode("boxb")
	return r
}

// linkBuilder attaches boxb, which is a machine that can build.
func (r *sideRig) linkBuilder(t *testing.T) {
	t.Helper()
	attachBuilder(t, r.f, r.nodeb)
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// A sandbox built on another machine gets the same default subdomain and
// front-door name Manager.Create gives one built here. Beyond reachability
// (which is M3's), the default route row is what CLAIMS the name in the routes
// table — a name claimed nowhere is one a later custom route can be pointed at.
func TestRemoteCreateMintsTheGatewaySideRows(t *testing.T) {
	r := newSideRig(t)
	r.linkBuilder(t)

	if _, err := r.f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("create on boxb: %v", err)
	}
	want := []string{"far-away->far-away@alice"}
	if got := r.routes.subdomains(); !equalStrings(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
	if got := r.door.published(); !equalStrings(got, []string{"far-away"}) {
		t.Errorf("front door published %v, want far-away", got)
	}
}

// The local half of the same rule: the fleet must NOT do this work for a
// sandbox on this machine, because the manager already does it and doing it
// twice means two writers racing over one row. This rig's manager has no stores
// wired, so a route appearing here could only have come from the fleet.
func TestLocalCreateLeavesTheSideStoresToTheManager(t *testing.T) {
	r := newSideRig(t)
	mustCreate(t, r.f, "brave-otter", "alice")
	if got := r.routes.subdomains(); got != nil {
		t.Errorf("the fleet wrote %v for a LOCAL sandbox; that is the manager's half", got)
	}
	if got := r.door.published(); got != nil {
		t.Errorf("the fleet published %v for a LOCAL sandbox", got)
	}
}

// ---------------------------------------------------------------------------
// Rename
// ---------------------------------------------------------------------------

// The gateway's half of a remote rename, which is all four stores. Without it
// the tag rows stay under the dead name and the sandbox silently loses every
// secret those tags selected on its next resume.
func TestRemoteRenameCarriesTheGatewaySideRows(t *testing.T) {
	r := newSideRig(t)
	seedRemote(t, r, "far-away", "alice")

	if err := r.f.Rename(context.Background(), "far-away", "far-away-2", "alice"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if !r.nodeb.took("rename") {
		t.Fatal("the machine was never asked to do its half")
	}
	want := []string{"far-away-2->far-away-2@alice"}
	if got := r.routes.subdomains(); !equalStrings(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
	if got := r.tags.under("far-away-2"); !equalStrings(got, []string{"prod"}) {
		t.Errorf("tags under the new name = %v, want the ones the old name had", got)
	}
	if got := r.tags.under("far-away"); got != nil {
		t.Errorf("tags left stranded under the dead name: %v", got)
	}
	if got := r.schedules.under("far-away-2"); !equalStrings(got, []string{"nightly"}) {
		t.Errorf("schedules under the new name = %v", got)
	}
	if got := r.door.published(); !equalStrings(got, []string{"far-away-2"}) {
		t.Errorf("front door published %v, want only the new name", got)
	}
}

// A refusal from the machine has to take the gateway's half back with it, or the
// sandbox keeps its old name while its rows sit under a name nothing has.
func TestRemoteRenameRollsTheSideRowsBackWhenTheMachineRefuses(t *testing.T) {
	r := newSideRig(t)
	seedRemote(t, r, "far-away", "alice")
	r.nodeb.renameErr = errors.New("no")

	if err := r.f.Rename(context.Background(), "far-away", "far-away-2", "alice"); err == nil {
		t.Fatal("a refused rename reported success")
	}
	want := []string{"far-away->far-away@alice"}
	if got := r.routes.subdomains(); !equalStrings(got, want) {
		t.Errorf("routes = %v, want them back at %v", got, want)
	}
	if got := r.tags.under("far-away"); !equalStrings(got, []string{"prod"}) {
		t.Errorf("tags under the old name = %v, want them back", got)
	}
	if got := r.door.published(); !equalStrings(got, []string{"far-away"}) {
		t.Errorf("front door published %v, want the old name back", got)
	}
	if row, ok, _ := r.index.Get("far-away"); !ok || row.Node != "boxb" {
		t.Errorf("the ledger row was not rolled back: ok=%v row=%+v", ok, row)
	}
}

// A route store that refuses is FATAL and refuses the whole rename, because a
// route row carries the owner column that gates private-route auth: one left
// under the old name is an authorization record pointing at a sandbox that no
// longer exists. Nothing else may have moved, and the machine must never have
// been asked.
func TestRemoteRenameStopsWhenTheRoutesRefuse(t *testing.T) {
	r := newSideRig(t)
	seedRemote(t, r, "far-away", "alice")
	r.routes.fail = errors.New("locked")

	if err := r.f.Rename(context.Background(), "far-away", "far-away-2", "alice"); err == nil {
		t.Fatal("a rename whose routes could not move reported success")
	}
	if r.nodeb.took("rename") {
		t.Fatal("the machine was asked to rename after the gateway's half had already failed")
	}
	if got := r.tags.under("far-away"); !equalStrings(got, []string{"prod"}) {
		t.Errorf("tags moved after the fatal step: %v", got)
	}
	if row, ok, _ := r.index.Get("far-away"); !ok || row.Node != "boxb" {
		t.Errorf("the ledger row was not rolled back: ok=%v row=%+v", ok, row)
	}
}

// Manager.renameChecks refuses a rename onto a subdomain somebody else's custom
// route holds. That check reads m.routes, which on a node is nil, so on the
// remote path the gateway makes it or nobody does.
func TestRemoteRenameRefusesASubdomainSomebodyElseHolds(t *testing.T) {
	r := newSideRig(t)
	seedRemote(t, r, "far-away", "alice")
	if err := r.routes.Upsert(routes.Route{
		Subdomain: "far-away-2", Sandbox: "bobs-box", Owner: "bob", Port: routes.DefaultPort,
	}); err != nil {
		t.Fatal(err)
	}

	err := r.f.Rename(context.Background(), "far-away", "far-away-2", "alice")
	if err == nil {
		t.Fatal("a rename onto another user's subdomain was allowed")
	}
	if !strings.Contains(err.Error(), "far-away-2") {
		t.Errorf("error = %v; it should name the subdomain", err)
	}
	if r.nodeb.took("rename") {
		t.Fatal("the machine was asked despite the collision")
	}
	if _, ok, _ := r.index.Get("far-away-2"); ok {
		t.Error("the ledger took the new name despite the collision")
	}
}

// ---------------------------------------------------------------------------
// Destroy
// ---------------------------------------------------------------------------

// The worst of the four, because Destroy also RELEASES the name. A route row
// left behind keeps its old owner's handle and starts answering for whoever
// takes the name next; a schedule row left behind runs the old owner's command
// inside the new owner's sandbox.
func TestRemoteDestroySweepsTheGatewaySideRows(t *testing.T) {
	r := newSideRig(t)
	seedRemote(t, r, "far-away", "alice")

	if err := r.f.Destroy(context.Background(), "far-away"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got := r.routes.subdomains(); got != nil {
		t.Errorf("routes survived a destroy: %v", got)
	}
	if got := r.tags.under("far-away"); got != nil {
		t.Errorf("tag rows survived a destroy: %v", got)
	}
	if got := r.schedules.under("far-away"); got != nil {
		t.Errorf("schedule rows survived a destroy: %v", got)
	}
	if got := r.door.published(); got != nil {
		t.Errorf("the front-door name survived a destroy: %v", got)
	}
	if _, ok, _ := r.index.Get("far-away"); ok {
		t.Error("the placement row was not released")
	}

	// And the name really is reusable by somebody else, with nothing of the
	// previous owner's left pointing at it — which is the whole reason the sweep
	// runs while the placement row is still held.
	if _, err := r.f.CreateOn(context.Background(), "boxb", "far-away", "bob", "ubuntu", 1, 512); err != nil {
		t.Fatalf("bob could not take the freed name: %v", err)
	}
	want := []string{"far-away->far-away@bob"}
	if got := r.routes.subdomains(); !equalStrings(got, want) {
		t.Errorf("routes = %v, want %v — alice's row must not have survived", got, want)
	}
}

// seedRemote places a sandbox on boxb with the gateway-side rows a real one
// accumulates: its default route, a tag, a schedule and a front-door name.
func seedRemote(t *testing.T, r *sideRig, name, owner string) {
	t.Helper()
	r.nodeb.mu.Lock()
	r.nodeb.boxes = append(r.nodeb.boxes, &host.Sandbox{
		Name: name, Owner: owner, Image: "ubuntu", State: vmm.StateRunning,
	})
	r.nodeb.mu.Unlock()
	r.linkBuilder(t)
	place(t, r.index, name, owner, "boxb")
	if err := r.routes.Upsert(routes.Route{
		Subdomain: name, Sandbox: name, Owner: owner, Port: routes.DefaultPort,
	}); err != nil {
		t.Fatal(err)
	}
	r.tags.rows[name] = []string{"prod"}
	r.schedules.rows[name] = []string{"nightly"}
	r.door.Ensure(context.Background(), name)
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
