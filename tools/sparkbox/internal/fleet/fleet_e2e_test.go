package fleet_test

// The gateway's half of the pre-capture agent-tool refresh.
//
// A node holds only the gateway's upstream PUBLIC key, so it cannot open a
// session into its own guests and its manager's refresh hook is nil by
// construction. Everything here is therefore about the split: the gateway does
// it for a sandbox on another machine, never for one of its own, and never at
// the cost of the capture itself.
//
// On CKS every sandbox is remote, so this is the only half that would ever run
// there — were snapshot capture not already refused outright by the firecracker
// driver under --disable-host-rootfs-mounts.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// snapshottingNode is a machine whose capture SUCCEEDS, which fakeNode's does
// not: the questions below are about what happens around a capture, so the
// capture itself has to be able to complete.
type snapshottingNode struct {
	*buildingNode
	// beforeSnapshot runs inside Snapshotter, so a test can read the world at
	// the moment the node starts pausing the guest.
	beforeSnapshot func()
}

func (n *snapshottingNode) Snapshotter(_ context.Context, box, snapName, owner string) (*host.Snapshot, error) {
	if n.beforeSnapshot != nil {
		n.beforeSnapshot()
	}
	n.record("snapshot")
	return &host.Snapshot{
		Name: snapName, Owner: owner, Image: "snap-" + owner + "-" + snapName,
		FromBox: box, CreatedAt: time.Now().UTC(), Node: n.Name(),
	}, nil
}

func attachSnapshotter(t *testing.T, f *fleet.Fleet, n *snapshottingNode) {
	t.Helper()
	detach, err := f.Attach(n)
	if err != nil {
		t.Fatalf("attach %s: %v", n.Name(), err)
	}
	t.Cleanup(detach)
}

// recordingRefresher is the envsync syncer's RefreshTools, reduced to what it
// was asked to do — and to when.
type recordingRefresher struct {
	mu        sync.Mutex
	boxes     []host.Sandbox
	deadlines []time.Time // the zero value means the call arrived unbudgeted
	err       error
	observe   func() // runs inside the call, before it returns
}

func (r *recordingRefresher) RefreshTools(ctx context.Context, b *host.Sandbox) error {
	deadline, _ := ctx.Deadline()
	r.mu.Lock()
	r.boxes = append(r.boxes, *b)
	r.deadlines = append(r.deadlines, deadline)
	err, observe := r.err, r.observe
	r.mu.Unlock()
	if observe != nil {
		observe()
	}
	return err
}

func (r *recordingRefresher) refreshed() []host.Sandbox {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]host.Sandbox(nil), r.boxes...)
}

func (r *recordingRefresher) budgets() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.deadlines...)
}

// The remote refresh spends its OWN budget, not the whole capture's.
//
// This one is invisible without a test, because dropping the cap breaks nothing
// that any other test observes. envsync.RefreshTools installs its 10-minute
// fallback only when the context carries NO deadline, and the context here
// always carries one — ctlops.CreateSnapshot opens the capture on
// ArchiveTimeout, 15 minutes. So an uncapped refresh inherits all fifteen and
// can leave nothing for the e2fsck + zerofree + reflink that is the actual
// point, and the failure surfaces as "snapshots got slower", pointing at the
// wrong end of the operation.
//
// The assertion is a bound rather than an equality: what matters is that the
// refresh cannot eat the caller's whole budget, not which constant it uses.
func TestRemoteToolRefreshCarriesItsOwnBudget(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	tools := &recordingRefresher{}
	f.SetToolSync(tools)
	nodeb := &snapshottingNode{buildingNode: newBuildingNode("boxb")}
	attachSnapshotter(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}

	// The real caller's shape: the whole capture on ArchiveTimeout.
	const capture = 15 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), capture)
	defer cancel()
	start := time.Now()
	if _, err := f.Snapshot(ctx, "far-away", "golden", "alice"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	got := tools.budgets()
	if len(got) != 1 {
		t.Fatalf("%d tool refreshes, want exactly 1", len(got))
	}
	if got[0].IsZero() {
		t.Fatal("the remote refresh ran with no deadline at all")
	}
	// Half the capture is the discriminator, not equality with it: an inherited
	// deadline reads a hair UNDER the full budget (the context is created a few
	// microseconds before start), so "did it get all fifteen minutes" cannot be
	// asked directly. Anything at or above half is inherited; the real budget is
	// a third of it.
	if left := got[0].Sub(start); left > capture/2 {
		t.Errorf("the refresh was handed %v of the capture's %v, i.e. the caller's own deadline; it must "+
			"carry its own, shorter budget or a slow install starves the e2fsck + zerofree + reflink "+
			"that follows it", left, capture)
	}
}

// The capture of a sandbox on another machine gets its tools refreshed by the
// GATEWAY, with the ledger's record, while the guest is still up.
//
// All three clauses are load-bearing. The gateway, because the node has no
// signer with which to reach its own guest. The ledger's record, because the
// synthetic fleet address is the only one the dialer can route and the ledger's
// owner is the only owner this package acts on. Still up, because the node
// pauses the guest the moment Snapshotter is called — which is why the refresh
// has to have happened before it.
func TestSnapshotRefreshesARemoteGuestFromTheGateway(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	tools := &recordingRefresher{}
	f.SetToolSync(tools)
	nodeb := &snapshottingNode{buildingNode: newBuildingNode("boxb")}
	attachSnapshotter(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	// Read at the moment the node begins its capture: by then the refresh must
	// already be done, not merely started.
	var refreshesAtCapture int
	nodeb.beforeSnapshot = func() { refreshesAtCapture = len(tools.refreshed()) }

	if _, err := f.Snapshot(context.Background(), "far-away", "golden", "alice"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got := tools.refreshed()
	if len(got) != 1 {
		t.Fatalf("%d tool refreshes, want exactly 1", len(got))
	}
	if refreshesAtCapture != 1 {
		t.Fatalf("the node started capturing with %d refreshes done; the guest is paused from that moment on", refreshesAtCapture)
	}
	if got[0].Owner != "alice" {
		t.Errorf("refreshed for owner %q, want the ledger's", got[0].Owner)
	}
	if got[0].Node != "boxb" {
		t.Errorf("refreshed with node %q, want the ledger's", got[0].Node)
	}
	if !strings.HasSuffix(got[0].SSHAddr, ".sandbox.invalid:"+fleet.SSHPort) {
		t.Errorf("refreshed with SSHAddr %q, want the synthetic fleet address the dialer resolves", got[0].SSHAddr)
	}
	if got[0].State != vmm.StateRunning {
		t.Errorf("refreshed a %q sandbox; only a running guest can be installed into", got[0].State)
	}
}

// The other half of the split, and the one that is silent when it breaks.
//
// host.Manager.Snapshot runs its own refresh for a sandbox it holds — inside
// its disk lock, and after the pre-pack strip has safely woken the guest,
// neither of which this side can do. A second installer here would mean two
// processes writing one guest's /usr/local/bin with no ordering between them,
// and since both install the same versions nearly always, the day it mattered
// would be the day a tool had just been released.
func TestSnapshotDoesNotDoubleRefreshALocalSandbox(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	tools := &recordingRefresher{}
	f.SetToolSync(tools)

	b := mustCreate(t, f, "brave-otter", "alice")
	if b.Node != mgr.NodeName() {
		t.Fatalf("an unnamed create landed on %q", b.Node)
	}
	if _, err := f.Snapshot(context.Background(), "brave-otter", "golden", "alice"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := tools.refreshed(); len(got) != 0 {
		t.Fatalf("the fleet refreshed a local sandbox (%d times); that is the manager's job, and two writers race over one /usr/local/bin", len(got))
	}
}

// Best-effort, from this side too. The refresh reaches across another machine's
// tunnel into a guest, and none of what can go wrong there is a reason to
// refuse somebody a template of the machine they just set up. A stale template
// still forks; a refused capture leaves them with nothing.
func TestSnapshotSurvivesAnUnreachableRemoteRefresh(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	tools := &recordingRefresher{err: errors.New("dial far-away: no route to host")}
	f.SetToolSync(tools)
	nodeb := &snapshottingNode{buildingNode: newBuildingNode("boxb")}
	attachSnapshotter(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	snap, err := f.Snapshot(context.Background(), "far-away", "golden", "alice")
	if err != nil {
		t.Fatalf("Snapshot after an unreachable refresh: %v", err)
	}
	if snap == nil || snap.Name != "golden" {
		t.Fatalf("Snapshot returned %+v, want the template", snap)
	}
	if !nodeb.took("snapshot") {
		t.Fatal("the machine holding the sandbox was never asked to capture it")
	}
	if len(tools.refreshed()) != 1 {
		t.Fatalf("%d refresh attempts, want 1", len(tools.refreshed()))
	}
}
