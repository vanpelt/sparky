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

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
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

func (s *fakeRoutes) ListBySandbox(sandbox string) ([]routes.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []routes.Route
	for _, r := range s.byID {
		if r.Sandbox == sandbox {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeRoutes) SetVisibility(subdomain, visibility string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[subdomain]
	if !ok {
		return routes.ErrNoSuchRoute
	}
	r.Visibility = visibility
	s.byID[subdomain] = r
	return nil
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

func TestRemoteSandboxCanManageOnlyItsOwnGatewayRoutes(t *testing.T) {
	r := newSideRig(t)
	r.linkBuilder(t)
	if _, err := r.f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := r.routes.Upsert(routes.Route{
		Subdomain: "web-far-away", Sandbox: "far-away", Owner: "alice", Port: 9000,
		Visibility: routes.VisibilityPrivate,
	}); err != nil {
		t.Fatal(err)
	}

	visibility, err := r.f.SelfVisibility(context.Background(), "boxb", nodelink.SelfVisibilityReq{
		Sandbox: "far-away", Visibility: routes.VisibilityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if visibility.Routes != 2 || visibility.Visibility != routes.VisibilityPublic {
		t.Errorf("visibility = %+v", visibility)
	}
	for _, sub := range []string{"far-away", "web-far-away"} {
		row, _, _ := r.routes.GetBySubdomain(sub)
		if row.Visibility != routes.VisibilityPublic {
			t.Errorf("route %s stayed %s", sub, row.Visibility)
		}
	}

	port, err := r.f.SelfPort(context.Background(), "boxb", nodelink.SelfPortReq{Sandbox: "far-away", Port: 5173})
	if err != nil || port.Port != 5173 {
		t.Fatalf("port = %+v, %v", port, err)
	}
	defaultRoute, _, _ := r.routes.GetBySubdomain("far-away")
	customRoute, _, _ := r.routes.GetBySubdomain("web-far-away")
	if defaultRoute.Port != 5173 || customRoute.Port != 9000 {
		t.Errorf("default/custom ports = %d/%d", defaultRoute.Port, customRoute.Port)
	}

	if _, err := r.f.SelfVisibility(context.Background(), "intruder", nodelink.SelfVisibilityReq{
		Sandbox: "far-away", Visibility: routes.VisibilityPrivate,
	}); err == nil {
		t.Fatal("another node changed route visibility")
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

// recordingSelfLifecycle is a control plane that only remembers which sandbox
// RECORD it was handed. The record is the whole security property: the owner
// the operation runs as is read off it, so a node that could reach one it does
// not hold would be choosing whose authority its guests act under.
type recordingSelfLifecycle struct {
	boxes   []string
	args    []ctlops.SnapshotToTagArgs
	plan    ctlops.SelfSnapshotPlan
	planTag string
}

func (l *recordingSelfLifecycle) Pause(_ context.Context, box *host.Sandbox) error {
	l.boxes = append(l.boxes, "pause:"+box.Name+"@"+box.Owner)
	return nil
}

// PlanSnapshot echoes the tag and name it was asked for, the way the real one
// does once it has authorized them. planTag overrides that echo, which is how a
// test says "the control plane plans something else than it was asked for".
func (l *recordingSelfLifecycle) PlanSnapshot(_ context.Context, box *host.Sandbox, tag, name string) (ctlops.SelfSnapshotPlan, error) {
	l.boxes = append(l.boxes, "plan:"+box.Name+"@"+box.Owner)
	p := l.plan
	p.Sandbox, p.Tag = box.Name, tag
	if name != "" {
		p.Snapshot = name
	}
	if l.planTag != "" {
		p.Tag = l.planTag
	}
	return p, nil
}

func (l *recordingSelfLifecycle) Snapshot(_ context.Context, box *host.Sandbox, a ctlops.SnapshotToTagArgs) error {
	l.boxes = append(l.boxes, "capture:"+box.Name+"@"+box.Owner)
	l.args = append(l.args, a)
	return nil
}

// TestRemoteSandboxCanRunOnlyItsOwnLifecycleVerbs extends the same
// placement-ledger rule to the two verbs that stop a VM. It is the identical
// check SelfVisibility gets, and it has to be, because these two are strictly
// sharper: one stops somebody's machine and the other re-points what their
// future sandboxes boot from.
func TestRemoteSandboxCanRunOnlyItsOwnLifecycleVerbs(t *testing.T) {
	r := newSideRig(t)
	r.linkBuilder(t)
	if _, err := r.f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	life := &recordingSelfLifecycle{plan: ctlops.SelfSnapshotPlan{Snapshot: "web-260829-1412", Token: "tok"}}
	r.f.SetSelfLifecycle(life)
	ctx := context.Background()

	if _, err := r.f.SelfPause(ctx, "boxb", nodelink.SelfPauseReq{Sandbox: "far-away"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	plan, err := r.f.SelfSnapshotPlan(ctx, "boxb", nodelink.SelfSnapshotPlanReq{Sandbox: "far-away", Tag: "web"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Snapshot != "web-260829-1412" || plan.Token != "tok" {
		t.Errorf("plan crossed the fleet as %+v", plan)
	}
	if _, err := r.f.SelfSnapshot(ctx, "boxb", nodelink.SelfSnapshotReq{
		Sandbox: "far-away", Tag: "web", Name: "web-260829-1412",
	}); err != nil {
		t.Fatalf("capture: %v", err)
	}
	// The owner comes off the ledger's record, not off anything the node said.
	// The second "plan" is the commit's own: SelfSnapshot re-derives the tag and
	// the name on this side before it captures anything.
	want := []string{"pause:far-away@alice", "plan:far-away@alice",
		"plan:far-away@alice", "capture:far-away@alice"}
	if !equalStrings(life.boxes, want) {
		t.Errorf("lifecycle saw %v, want %v", life.boxes, want)
	}
	if len(life.args) != 1 || life.args[0].Sandbox != "far-away" {
		t.Errorf("capture args = %+v", life.args)
	}

	for name, call := range map[string]func() error{
		"pause": func() error {
			_, err := r.f.SelfPause(ctx, "intruder", nodelink.SelfPauseReq{Sandbox: "far-away"})
			return err
		},
		"plan": func() error {
			_, err := r.f.SelfSnapshotPlan(ctx, "intruder", nodelink.SelfSnapshotPlanReq{Sandbox: "far-away"})
			return err
		},
		"capture": func() error {
			_, err := r.f.SelfSnapshot(ctx, "intruder", nodelink.SelfSnapshotReq{
				Sandbox: "far-away", Tag: "web", Name: "web-260829-1412"})
			return err
		},
	} {
		if err := call(); codeOf(err) != nodelink.CodeNotYours {
			t.Errorf("cross-node %s = %v (code %q), want %s", name, err, codeOf(err), nodelink.CodeNotYours)
		}
	}
	if len(life.boxes) != 4 {
		t.Errorf("a cross-node request reached the control plane: %v", life.boxes)
	}
}

// TestAGatewayWillNotCaptureIntoATagItsOwnPlanDoesNotName.
//
// The plan token is checked by the metadata service that answered the guest,
// and on a node that service runs on the node. So the tag and the name a node
// hands up are, by themselves, a node's assertion. This is the check that makes
// them the gateway's answer instead: a guest may re-point only a tag its own
// sandbox carries (ctlops.PlanSelfSnapshot), and without a re-plan here an
// enrolled node could re-point any tag belonging to an owner whose sandbox it
// happens to run — which then hands that tag's secrets to every sandbox created
// on it afterwards, on any machine.
func TestAGatewayWillNotCaptureIntoATagItsOwnPlanDoesNotName(t *testing.T) {
	r := newSideRig(t)
	r.linkBuilder(t)
	if _, err := r.f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	// The control plane plans `web` — the tag this sandbox actually carries —
	// whatever it is asked for.
	life := &recordingSelfLifecycle{
		plan:    ctlops.SelfSnapshotPlan{Snapshot: "web-260829-1412", Token: "tok"},
		planTag: "web",
	}
	r.f.SetSelfLifecycle(life)
	ctx := context.Background()

	_, err := r.f.SelfSnapshot(ctx, "boxb", nodelink.SelfSnapshotReq{
		Sandbox: "far-away", Tag: "prod", Name: "web-260829-1412"})
	if code := codeOf(err); code != "plan_stale" {
		t.Fatalf("a relayed tag the gateway's plan does not name = %v (code %q), want plan_stale", err, code)
	}
	if len(life.args) != 0 {
		t.Errorf("the capture ran anyway: %+v", life.args)
	}

	// And a commit that never planned at all is refused rather than run under a
	// name the gateway derives on the spot: the plan is what names the snapshot,
	// so a capture whose name nobody was shown is not the gesture this is.
	life.boxes, life.args = nil, nil
	_, err = r.f.SelfSnapshot(ctx, "boxb", nodelink.SelfSnapshotReq{Sandbox: "far-away", Tag: "web"})
	if code := codeOf(err); code != "plan_stale" {
		t.Fatalf("a commit with no planned name = %v (code %q), want plan_stale", err, code)
	}
	if len(life.args) != 0 {
		t.Errorf("the capture ran anyway: %+v", life.args)
	}

	// And the pair the gateway's own plan does produce still goes through, so
	// this refuses forgery rather than the feature.
	if _, err := r.f.SelfSnapshot(ctx, "boxb", nodelink.SelfSnapshotReq{
		Sandbox: "far-away", Tag: "web", Name: "web-260829-1412"}); err != nil {
		t.Fatalf("the honest capture was refused: %v", err)
	}
	if len(life.args) != 1 || life.args[0].Tag != "web" || life.args[0].Name != "web-260829-1412" {
		t.Errorf("capture args = %+v", life.args)
	}
}

// TestGuestLifecycleWithoutAControlPlaneIsRefusedNotDropped: a gateway with no
// Ops installed answers a node in a sentence it can turn into a 501, rather
// than leaving a guest waiting on a hook that will never answer.
func TestGuestLifecycleWithoutAControlPlaneIsRefusedNotDropped(t *testing.T) {
	r := newSideRig(t)
	r.linkBuilder(t)
	if _, err := r.f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"pause": func() error {
			_, err := r.f.SelfPause(ctx, "boxb", nodelink.SelfPauseReq{Sandbox: "far-away"})
			return err
		},
		"plan": func() error {
			_, err := r.f.SelfSnapshotPlan(ctx, "boxb", nodelink.SelfSnapshotPlanReq{Sandbox: "far-away"})
			return err
		},
		"capture": func() error {
			_, err := r.f.SelfSnapshot(ctx, "boxb", nodelink.SelfSnapshotReq{
				Sandbox: "far-away", Tag: "web", Name: "web-260829-1412"})
			return err
		},
	} {
		if err := call(); !ctlops.IsKind(err, ctlops.KindDisabled) {
			t.Errorf("%s with no control plane = %v, want KindDisabled", name, err)
		}
	}
}
