package host

// White-box tests for Manager.Resize. The invariant under test is the pairing:
// a resize must never leave a sandbox able to resume from a memory snapshot
// that was taken against the old disk geometry.

import (
	"context"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

func TestResizeGrowsAndRestarts(t *testing.T) {
	m := internalManager(t, Options{})
	ctx := context.Background()
	if _, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 2048); err != nil {
		t.Fatal(err)
	}

	if err := m.Resize(ctx, "box", 51200); err != nil {
		t.Fatal(err)
	}

	if got := m.boxes["box"].State; got != vmm.StateRunning {
		t.Fatalf("resized sandbox left in %v, want running — resize brings it back up", got)
	}
	if got := m.boxes["box"].DiskTotalMB; got != 51200 {
		t.Fatalf("DiskTotalMB = %d, want 51200; the new ceiling should be visible without waiting for a reaper tick", got)
	}
}

// TestResizeDropsSnapshotBeforeResizing is the safety property. A guest's block
// device geometry is baked into its memory snapshot, so resuming one onto a
// grown filesystem yields an ext4 superblock claiming more blocks than the
// device reports. The mock's DropSnapshots forgets the VM entirely, so a
// surviving driver record after a resize means the snapshot was never dropped.
func TestResizeDropsSnapshotBeforeResizing(t *testing.T) {
	m := internalManager(t, Options{})
	ctx := context.Background()
	if _, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 2048); err != nil {
		t.Fatal(err)
	}
	driver := m.driver.(*mock.Driver)
	// Pause so a snapshot conceptually exists, as it would for any warm box.
	if err := m.Pause(ctx, "box"); err != nil {
		t.Fatal(err)
	}

	if err := m.Resize(ctx, "box", 51200); err != nil {
		t.Fatal(err)
	}

	// After resize the box is running again from a cold boot. If the resize had
	// happened without dropping the snapshot, the mock would have refused it as
	// a running VM, or left the pre-resize record in place.
	if _, err := driver.DiskCapacityMB(ctx, "box"); err != nil {
		t.Fatal(err)
	}
	if got := m.boxes["box"].DiskTotalMB; got != 51200 {
		t.Fatalf("DiskTotalMB = %d, want 51200", got)
	}
}

// TestResizeRefusesShrink: shrinking needs the opposite operation order and
// destroys the disk if the data doesn't fit, so it is not offered.
func TestResizeRefusesShrink(t *testing.T) {
	m := internalManager(t, Options{})
	ctx := context.Background()
	if _, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 2048); err != nil {
		t.Fatal(err)
	}

	// The mock's default ceiling is 25600 MB.
	if err := m.Resize(ctx, "box", 8192); err == nil {
		t.Fatal("shrink was accepted; resize must grow only")
	}
	// A refused resize still leaves a usable sandbox.
	if _, err := m.EnsureRunning(ctx, "box"); err != nil {
		t.Fatalf("sandbox unusable after a refused resize: %v", err)
	}
}

// TestResizeRejectsArchived: an archived box has no local rootfs to resize.
func TestResizeRejectsArchived(t *testing.T) {
	m := internalManager(t, Options{})
	ctx := context.Background()
	if _, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 2048); err != nil {
		t.Fatal(err)
	}
	m.boxes["box"].State = vmm.StateArchived

	if err := m.Resize(ctx, "box", 51200); err == nil {
		t.Fatal("resize of an archived sandbox was accepted")
	}
}

func TestResizeUnknownSandbox(t *testing.T) {
	m := internalManager(t, Options{})
	if err := m.Resize(context.Background(), "nope", 51200); err == nil {
		t.Fatal("resize of an unknown sandbox was accepted")
	}
}
