package fleet_test

// The gateway's half of the pre-pack secret strip.
//
// A node has no secrets store and never calls SetEnvSync, so
// host.Manager.stripEnvForPack finds a nil hook over there and returns nil —
// silently, and in the direction that looks fine. Everything here is about the
// gateway compensating: it strips for a sandbox on another machine, never for
// one of its own, it REFUSES the pack when it cannot, and it holds the box
// against its own env push until the machine holding it is done.
//
// This is the only half that would ever run on CKS, where the gateway is
// control-plane-only and every sandbox is remote.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// recordingStripper is the envsync syncer wearing both its hats: the fleet
// finds the stripper by asserting on the pusher it was given, exactly as
// host.Manager does, so a fake that implements only one of them would prove
// nothing about the wiring.
type recordingStripper struct {
	recordingPusher
	mu      sync.Mutex
	boxes   []host.Sandbox
	err     error
	observe func() // runs inside the call, before it returns
}

func (s *recordingStripper) StripEnv(_ context.Context, b *host.Sandbox) error {
	s.mu.Lock()
	s.boxes = append(s.boxes, *b)
	err, observe := s.err, s.observe
	s.mu.Unlock()
	if observe != nil {
		observe()
	}
	return err
}

func (s *recordingStripper) stripped() []host.Sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]host.Sandbox(nil), s.boxes...)
}

// A capture of a sandbox on another machine has its secrets cleared by the
// GATEWAY, with the ledger's record, before that machine freezes the disk.
//
// Every clause is load-bearing, and the last one most: the node pauses the
// guest the instant Snapshotter is called, so a strip that merely started by
// then is a strip that did not happen.
func TestSnapshotStripsARemoteGuestBeforeTheNodeCaptures(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	strip := &recordingStripper{}
	f.SetEnvPusher(strip)
	nodeb := &snapshottingNode{buildingNode: newBuildingNode("boxb")}
	attachSnapshotter(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	var strippedAtCapture int
	nodeb.beforeSnapshot = func() { strippedAtCapture = len(strip.stripped()) }

	if _, err := f.Snapshot(context.Background(), "far-away", "golden", "alice"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got := strip.stripped()
	if len(got) != 1 {
		t.Fatalf("%d strips, want exactly 1", len(got))
	}
	if strippedAtCapture != 1 {
		t.Fatalf("the node began capturing with %d strips done; every fork of that template copies whatever was in /etc/environment at this moment", strippedAtCapture)
	}
	if got[0].Owner != "alice" {
		t.Errorf("stripped for owner %q, want the ledger's", got[0].Owner)
	}
	if !strings.HasSuffix(got[0].SSHAddr, ".sandbox.invalid:"+fleet.SSHPort) {
		t.Errorf("stripped with SSHAddr %q, want the synthetic fleet address the dialer resolves", got[0].SSHAddr)
	}
	if got[0].State != vmm.StateRunning {
		t.Errorf("stripped a %q sandbox; only a running guest can be rewritten", got[0].State)
	}
}

// An archive wants it as much as a template does: it is packed into object
// storage and can outlive every sandbox it came from.
func TestArchiveStripsARemoteGuestFirst(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	strip := &recordingStripper{}
	f.SetEnvPusher(strip)
	nodeb := newBuildingNode("boxb")
	attachBuilder(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	if err := f.Archive(context.Background(), "far-away"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if got := strip.stripped(); len(got) != 1 {
		t.Fatalf("%d strips before an archive, want 1; an unstripped archive puts plaintext secrets in object storage", len(got))
	}
	if !nodeb.took("archive") {
		t.Fatal("the machine holding the sandbox was never asked to archive it")
	}
}

// The difference from the tool refresh beside it, and the whole reason this is
// a separate function: a failed strip REFUSES the pack.
//
// host.Manager.stripEnvForPack fails its caller rather than packing an uncleared
// disk. Which machine a sandbox happens to sit on must not change that answer.
func TestSnapshotIsRefusedWhenTheRemoteStripFails(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	strip := &recordingStripper{err: errors.New("dial far-away: no route to host")}
	f.SetEnvPusher(strip)
	nodeb := &snapshottingNode{buildingNode: newBuildingNode("boxb")}
	attachSnapshotter(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	snap, err := f.Snapshot(context.Background(), "far-away", "golden", "alice")
	if err == nil {
		t.Fatalf("Snapshot returned %+v after a failed strip; it must refuse rather than capture a disk with secrets in it", snap)
	}
	if nodeb.took("snapshot") {
		t.Fatal("the node was asked to capture anyway after the strip failed")
	}
}

// A paused remote guest is woken to be stripped, and put back if the strip then
// fails — so a refused pack leaves the sandbox as it found it.
//
// Archiving a paused sandbox is the ordinary case, not an exception: people
// archive what they are not using. Skipping the strip for one would make the
// common path the unsafe one.
func TestStripWakesAPausedRemoteGuestAndRepausesItOnFailure(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	strip := &recordingStripper{err: errors.New("guest refused the rewrite")}
	f.SetEnvPusher(strip)
	nodeb := newBuildingNode("boxb")
	attachBuilder(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	nodeb.stopped("far-away", vmm.StatePaused)

	if err := f.Archive(context.Background(), "far-away"); err == nil {
		t.Fatal("Archive succeeded after a failed strip")
	}
	if !nodeb.took("ensure_running") {
		t.Error("a paused guest was never woken; a sleeping box cannot be stripped, so the pack would have carried its secrets")
	}
	if !nodeb.took("pause") {
		t.Error("the guest woken for the strip was left running after the pack was refused")
	}
	if got := strip.stripped(); len(got) != 1 {
		t.Fatalf("%d strips, want 1 (on the woken guest)", len(got))
	}
}

// The other half of the split, silent when it breaks.
//
// host.Manager.Snapshot strips for a sandbox it holds, inside its disk lock and
// with the resumeOrRecreate wake only it can do. A second stripper here would
// be two writers on one guest's /etc/environment with no ordering between them.
func TestFleetDoesNotStripALocalSandbox(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	strip := &recordingStripper{}
	f.SetEnvPusher(strip)

	b := mustCreate(t, f, "brave-otter", "alice")
	if b.Node != mgr.NodeName() {
		t.Fatalf("an unnamed create landed on %q", b.Node)
	}
	if _, err := f.Snapshot(context.Background(), "brave-otter", "golden", "alice"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := strip.stripped(); len(got) != 0 {
		t.Fatalf("the fleet stripped a local sandbox (%d times); that is the manager's job, and two writers race over one /etc/environment", len(got))
	}
}

// The race the pack hold exists for, and the one nothing else in this package
// would catch.
//
// The strip usually has to wake the guest. The node announces that wake as a
// sandbox.changed, the gateway's own ApplyChanged turns the announcement into a
// lifecycle PushEnv, and that push writes the secrets back — after the strip,
// before the capture. The existing envsync `quiesced` flag does not stop it:
// that gates CHANGE-time pushes, while PushEnv clears it by design.
//
// Fired from inside the capture, which is the exact window: the strip is done
// and the disk is not yet frozen.
func TestAPackHoldsOffTheGatewaysOwnEnvPush(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	strip := &recordingStripper{}
	f.SetEnvPusher(strip)
	nodeb := &snapshottingNode{buildingNode: newBuildingNode("boxb")}
	attachSnapshotter(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	// The create's own push is legitimate and asynchronous; drain it so the
	// assertion below is about the one fired during the pack.
	awaitPush(t, &strip.recordingPusher, "far-away", "a create on another machine")
	before := len(strip.pushed())

	nodeb.beforeSnapshot = func() {
		// What a node's emitter sends when the machine puts a sandbox back into
		// running — which is precisely what the strip's own wake causes.
		f.ApplyChanged(nodeb.Name(), nodelink.ChangedMsg{
			Node:    nodeb.Name(),
			Sandbox: nodelink.SandboxRow{Name: "far-away", State: string(vmm.StateRunning)},
			Reason:  "resumed",
		})
	}
	if _, err := f.Snapshot(context.Background(), "far-away", "golden", "alice"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// A push is a goroutine, so "it did not happen" is only worth asserting
	// after giving it a moment to.
	time.Sleep(50 * time.Millisecond)
	if got := len(strip.pushed()); got != before {
		t.Fatalf("the gateway pushed the secret environment into a sandbox it was packing (%d -> %d); "+
			"that refills the block the strip just cleared, and the template captures it", before, got)
	}
}
