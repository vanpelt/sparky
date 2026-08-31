package ctlops

// A template carries the port its source sandbox served on, so a box booted
// from it is reachable at its own URL without anybody typing `sparkbox port`
// first. That is the whole reason to bind a snapshot to a tag: it should just
// work out of the box.
//
// The port is host-side state — a row in internal/routes, not a file in the
// rootfs — so nothing about carrying it forward is implied by copying a disk.
// These tests pin the two halves that make it travel: reading it off the source
// at capture, and correcting the fresh sandbox's route after the build.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

// setPort points a sandbox's own default route (subdomain == name) at a port,
// the way `sparkbox port` does from inside the guest.
func (r *rig) setPort(t *testing.T, sandbox string, port int) {
	t.Helper()
	list := r.routes.rs[sandbox]
	for i := range list {
		if list[i].Subdomain == sandbox {
			r.routes.rs[sandbox][i].Port = port
			return
		}
	}
	t.Fatalf("no default route for %q to point at %d", sandbox, port)
}

// portOf reads the port a sandbox's default route currently carries.
func (r *rig) portOf(t *testing.T, sandbox string) int {
	t.Helper()
	for _, route := range r.routes.rs[sandbox] {
		if route.Subdomain == sandbox {
			return route.Port
		}
	}
	return 0
}

// The capture half: a box serving on 5173 yields a template that says so.
func TestSnapshotCarriesTheSourceBoxDefaultPort(t *testing.T) {
	r := newRig(t)
	r.setPort(t, "alicebox", 5173)
	r.calls.reset()

	si, err := r.ops.CreateSnapshot(context.Background(), alice(), "alicebox", "websnap")
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if si.Port != 5173 {
		t.Errorf("SnapshotInfo.Port = %d, want 5173", si.Port)
	}
	if !r.calls.has("SetSnapshotPort alice snapshot=websnap port=5173") {
		t.Errorf("the port was not recorded: %v", r.calls.all())
	}
	// Read at capture, from the box being captured, and not from any other.
	if !r.calls.has("GetBySubdomain alicebox") {
		t.Errorf("the source box's route was never read: %v", r.calls.all())
	}
}

// A box nobody moved off the stock port records nothing. The table is sparse on
// purpose: absence means "whatever this host serves by default", which is the
// right answer for a template captured from a box that never chose one.
func TestSnapshotOnTheStockPortRecordsNothing(t *testing.T) {
	r := newRig(t)
	r.calls.reset()

	si, err := r.ops.CreateSnapshot(context.Background(), alice(), "alicebox", "stocksnap")
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if si.Port != 0 {
		t.Errorf("SnapshotInfo.Port = %d, want 0 for a stock-port capture", si.Port)
	}
	for _, c := range r.calls.all() {
		if len(c) >= 15 && c[:15] == "SetSnapshotPort" {
			t.Errorf("a stock-port capture wrote a row: %q", c)
		}
	}
}

// Only the sandbox's OWN default route travels. The custom routes an owner adds
// with `share` are furniture for one box, and cloning them onto every fork
// would hand out subdomains nobody asked for.
func TestSnapshotIgnoresCustomShareRoutes(t *testing.T) {
	r := newRig(t)
	r.routes.rs["alicebox"] = append(r.routes.rs["alicebox"], routes.Route{
		Subdomain: "demo", Sandbox: "alicebox", Owner: "alice",
		Port: 5173, Visibility: routes.VisibilityPublic,
	})
	r.calls.reset()

	si, err := r.ops.CreateSnapshot(context.Background(), alice(), "alicebox", "websnap")
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if si.Port != 0 {
		t.Errorf("port = %d; a custom route's port must not become the template's", si.Port)
	}
}

// The apply half, and the case the feature exists for: `ssh new@gw web` on a
// tag bound to a template captured from a box on 5173.
func TestCreateOnABoundTagAdoptsItsPort(t *testing.T) {
	r := newRig(t)
	r.bindings.bind("alice", "web", "websnap")
	if err := r.bindings.SetSnapshotPort("alice", "websnap", 5173); err != nil {
		t.Fatalf("seed port: %v", err)
	}
	r.tmpl.snaps["alice/websnap"] = &host.Snapshot{
		Name: "websnap", Owner: "alice", FromBox: "alicebox",
		Image: "snap-alice-websnap", CreatedAt: time.Unix(0, 0).UTC(),
	}
	r.calls.reset()

	if _, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "worker", Tags: []string{"web"}}); err != nil {
		t.Fatalf("create on a bound tag: %v", err)
	}
	if got := r.portOf(t, "worker"); got != 5173 {
		t.Errorf("the new sandbox's default route is on %d, want 5173", got)
	}
	// AFTER the build: the row this corrects does not exist until the manager
	// has made it.
	all := r.calls.all()
	build := indexOfCall(t, all, "Create worker owner=alice image=snap-alice-websnap")
	upsert := indexOfCall(t, all, "Upsert worker -> worker:5173")
	if build < 0 || upsert < 0 {
		t.Fatalf("build=%d upsert=%d in %v", build, upsert, all)
	}
	if upsert < build {
		t.Errorf("the route was corrected before the sandbox existed: %v", all)
	}
}

// A tag bound to a template with no recorded port leaves the route exactly
// where the manager put it — no second write at all.
func TestCreateOnATemplateWithNoPortWritesNoRoute(t *testing.T) {
	r := newRig(t)
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.calls.reset()

	if _, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "worker", Tags: []string{"cuda"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, c := range r.calls.all() {
		if len(c) >= 6 && c[:6] == "Upsert" {
			t.Errorf("a template with no port still rewrote a route: %q", c)
		}
	}
}

// A fork consults the snapshot it was told to boot and never a binding — that
// is the distinction between the two verbs — so its port comes from the same
// place its disk did, by name.
func TestForkAdoptsItsSnapshotsPort(t *testing.T) {
	r := newRig(t)
	if err := r.bindings.SetSnapshotPort("alice", "alicesnap", 3000); err != nil {
		t.Fatalf("seed port: %v", err)
	}
	r.calls.reset()

	if _, err := r.ops.Fork(context.Background(), alice(),
		ForkArgs{Snapshot: "alicesnap", Name: "forked"}); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if got := r.portOf(t, "forked"); got != 3000 {
		t.Errorf("the forked sandbox's default route is on %d, want 3000", got)
	}
}

// Adopting a port must not disturb anything else on the route. routes.Upsert's
// ON CONFLICT touches only the port for exactly this reason: a box whose owner
// made its URL public must not be quietly re-privatised by the port it
// inherited. The fake reproduces that clause, so what this pins at this layer
// is that ctlops goes through it rather than around it.
func TestAdoptingAPortLeavesVisibilityAlone(t *testing.T) {
	r := newRig(t)
	if err := r.bindings.SetSnapshotPort("alice", "alicesnap", 3000); err != nil {
		t.Fatalf("seed port: %v", err)
	}
	// The row the manager would have written, already made public.
	r.routes.rs["forked"] = []routes.Route{{
		Subdomain: "forked", Sandbox: "forked", Owner: "alice",
		Port: routes.DefaultPort, Visibility: routes.VisibilityPublic,
	}}

	if _, err := r.ops.Fork(context.Background(), alice(),
		ForkArgs{Snapshot: "alicesnap", Name: "forked"}); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	got := r.routes.rs["forked"]
	if len(got) != 1 {
		t.Fatalf("routes for the fork = %+v, want the one row corrected in place", got)
	}
	if got[0].Port != 3000 {
		t.Errorf("port = %d, want 3000", got[0].Port)
	}
	if got[0].Visibility != routes.VisibilityPublic {
		t.Errorf("visibility = %q, want it untouched at public", got[0].Visibility)
	}
}

// A port row that outlived its snapshot would be inherited by the next capture
// to take that name.
func TestDeleteSnapshotForgetsItsPort(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	if err := r.bindings.SetSnapshotPort("alice", "alicesnap", 5173); err != nil {
		t.Fatalf("seed port: %v", err)
	}
	r.calls.reset()

	if err := r.ops.DeleteSnapshot(ctx, alice(), "alicesnap"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if !r.calls.has("ForgetSnapshotPort alice snapshot=alicesnap") {
		t.Errorf("the port row was left behind: %v", r.calls.all())
	}
	if p, _ := r.bindings.SnapshotPort("alice", "alicesnap"); p != 0 {
		t.Errorf("port after delete = %d, want 0", p)
	}
	// The template file goes first: a port forgotten for a snapshot the driver
	// then refused to delete would leave the template silently on the stock
	// port, which is the failure this ordering avoids.
	all := r.calls.all()
	del := indexOfCall(t, all, "DeleteSnapshot alicesnap owner=alice")
	forget := indexOfCall(t, all, "ForgetSnapshotPort alice snapshot=alicesnap")
	if del < 0 || forget < 0 || forget < del {
		t.Errorf("delete=%d forget=%d, want the disk removed first: %v", del, forget, all)
	}
}

// Recording the port is best effort, and deliberately so: by the time it runs,
// the expensive and unrepeatable half of a capture has already succeeded. The
// snapshot is kept, and the port simply does not travel — which the empty
// column in `snapshot ls` is what announces.
func TestCaptureSurvivesAPortStoreFailure(t *testing.T) {
	r := newRig(t)
	r.setPort(t, "alicebox", 5173)
	r.bindings.portErr = errors.New("database is locked")

	si, err := r.ops.CreateSnapshot(context.Background(), alice(), "alicebox", "websnap")
	if err != nil {
		t.Fatalf("a port-store failure took the whole capture down: %v", err)
	}
	if si.Name != "websnap" {
		t.Errorf("snapshot = %q, want the capture to have happened anyway", si.Name)
	}
	if si.Port != 0 {
		t.Errorf("port = %d, want 0 — nothing was recorded", si.Port)
	}
}

// Nor does a failure to read one stop a create: the alternative is refusing to
// make a sandbox over a routing detail the user can change in one command.
func TestCreateSurvivesAPortStoreFailure(t *testing.T) {
	r := newRig(t)
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.bindings.portErr = errors.New("database is locked")

	if _, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "worker", Tags: []string{"cuda"}}); err != nil {
		t.Fatalf("a port-store failure took the create down: %v", err)
	}
}

// A route store that cannot be written must not take the create with it either.
func TestCreateSurvivesARouteUpsertFailure(t *testing.T) {
	r := newRig(t)
	r.bindings.bind("alice", "cuda", "alicesnap")
	if err := r.bindings.SetSnapshotPort("alice", "alicesnap", 5173); err != nil {
		t.Fatalf("seed port: %v", err)
	}
	r.routes.err = errors.New("database is locked")

	if _, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "worker", Tags: []string{"cuda"}}); err != nil {
		t.Fatalf("a route write failure took the create down: %v", err)
	}
}

// The listing column, and its one rule — shared with bound tags: the port is
// decoration on the row, not its subject, so a store hiccup drops the column
// rather than the listing.
func TestListSnapshotsCarriesThePort(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	if err := r.bindings.SetSnapshotPort("alice", "alicesnap", 5173); err != nil {
		t.Fatalf("seed port: %v", err)
	}

	snaps, err := r.ops.ListSnapshots(ctx, alice())
	if err != nil || len(snaps) != 1 {
		t.Fatalf("ListSnapshots = %v, %v", snaps, err)
	}
	if snaps[0].Port != 5173 {
		t.Errorf("port = %d, want 5173", snaps[0].Port)
	}
	// One read for the whole listing, not one per row.
	if n := countCalls(r.calls.all(), "SnapshotPorts alice"); n != 1 {
		t.Errorf("the port table was read %d times for one listing, want 1", n)
	}

	r.bindings.portErr = errors.New("database is locked")
	snaps, err = r.ops.ListSnapshots(ctx, alice())
	if err != nil {
		t.Fatalf("a port-store failure took the listing down: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Port != 0 {
		t.Errorf("degraded listing = %+v, want the row with an empty column", snaps)
	}
}

func countCalls(all []string, want string) int {
	n := 0
	for _, c := range all {
		if c == want {
			n++
		}
	}
	return n
}
