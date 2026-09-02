package fleet_test

// Fleet.CapacityForOwner is what the user console's footprint card reads. The
// property worth pinning is that it charges each machine's sandboxes against
// THAT machine's configuration: on a Kubernetes deployment the owner pools are
// declared on the node Deployment and the gateway sets none at all, so a rollup
// that used the local manager's policy would report every quota as absent.

import (
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

func TestOwnerCapacityChargesEachMachineAgainstItsOwnPools(t *testing.T) {
	// A gateway with no pool configuration of its own — the CKS shape.
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)

	nodeb := newFakeNode("boxb")
	nodeb.capacity = host.NodeCapacity{
		OwnerMemoryPoolMB: 8192, OwnerMemoryBurstMB: 16384,
		DiskPoolMBPerOwner: 102400, ReserveMemMB: 2048,
	}
	attach(t, f, nodeb, &host.Sandbox{
		Name: "far-one", Owner: "bob", Node: "boxb", Image: "ubuntu", State: vmm.StateRunning,
		VCPUs: 4, MemMB: 16384, DiskMB: 7000, BaseDiskMB: 6144, DiskTotalMB: 25600,
	}, &host.Sandbox{
		Name: "far-two", Owner: "bob", Node: "boxb", Image: "ubuntu", State: vmm.StatePaused,
		VCPUs: 2, MemMB: 8192, DiskMB: 6400, BaseDiskMB: 6144, DiskTotalMB: 25600,
	})
	place(t, index, "far-one", "bob", "boxb")
	place(t, index, "far-two", "bob", "boxb")

	c := f.CapacityForOwner("bob")

	if c.DiskPoolMB != 102400 || c.MemoryPoolMB != 8192 || c.MemoryBurstMB != 16384 {
		t.Fatalf("the node's pools did not reach the rollup: %+v", c)
	}
	if c.TotalSandboxes != 2 || c.RunningSandboxes != 1 || c.Nodes != 1 {
		t.Fatalf("counts wrong: %+v", c)
	}
	if c.RawDiskMB != 13400 {
		t.Fatalf("RawDiskMB = %d, want 13400", c.RawDiskMB)
	}
	// 856 + 256: the template's 6 GB is physically stored once, not twice.
	if c.UsedDiskMB != 1112 {
		t.Fatalf("UsedDiskMB = %d, want 1112 — the reflink baseline must cross the link", c.UsedDiskMB)
	}
	// The node runs with overcommit, so its running guest is charged the 2 GB
	// working-set floor rather than its 16 GB ceiling. Reading the gateway's
	// own (unset) reserve would have charged the whole ceiling.
	if c.AllocatedMemMB != 16384 {
		t.Fatalf("AllocatedMemMB = %d, want the 16384 ceiling", c.AllocatedMemMB)
	}
	if c.EffectiveMemMB != 2048 {
		t.Fatalf("EffectiveMemMB = %d, want the node's 2048 reserve", c.EffectiveMemMB)
	}
	if c.AllocatedVCPUs != 4 {
		t.Fatalf("AllocatedVCPUs = %d, want 4", c.AllocatedVCPUs)
	}
	// Somebody else's sandboxes are not bob's.
	if other := f.CapacityForOwner("alice"); other.TotalSandboxes != 0 {
		t.Fatalf("alice was charged bob's machines: %+v", other)
	}
}

// TestOwnerCapacitySumsAcrossMachinesWithoutSummingPools. An owner whose
// sandboxes straddle the gateway and a node uses one pool, not two.
func TestOwnerCapacitySumsAcrossMachinesWithoutSummingPools(t *testing.T) {
	mgr := newManager(t, host.Options{
		OwnerMemoryPoolMB: 8192, OwnerMemoryBurstMB: 16384, DiskPoolMBPerOwner: 102400,
	})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	mustCreate(t, f, "here", "bob")

	nodeb := newFakeNode("boxb")
	nodeb.capacity = host.NodeCapacity{
		OwnerMemoryPoolMB: 8192, OwnerMemoryBurstMB: 16384, DiskPoolMBPerOwner: 102400,
	}
	attach(t, f, nodeb, &host.Sandbox{
		Name: "there", Owner: "bob", Node: "boxb", Image: "ubuntu", State: vmm.StateRunning,
		VCPUs: 2, MemMB: 4096, DiskMB: 7000, BaseDiskMB: 6144,
	})
	place(t, index, "there", "bob", "boxb")

	c := f.CapacityForOwner("bob")
	if c.TotalSandboxes != 2 {
		t.Fatalf("TotalSandboxes = %d, want both machines' (got %+v)", c.TotalSandboxes, c)
	}
	if c.Nodes != 2 {
		t.Fatalf("Nodes = %d, want 2", c.Nodes)
	}
	if c.DiskPoolMB != 102400 || c.MemoryPoolMB != 8192 || c.MemoryBurstMB != 16384 {
		t.Fatalf("pools doubled across machines: %+v", c)
	}
	if c.UsedDiskMB < 856 {
		t.Fatalf("UsedDiskMB = %d, want at least the remote 856", c.UsedDiskMB)
	}
}

// TestOwnerCapacityOnASingleMachineIsTheManagersAnswer: parity with the
// deployment that has no fleet at all, which is the contract every other
// listing method here holds to.
func TestOwnerCapacityOnASingleMachineIsTheManagersAnswer(t *testing.T) {
	mgr := newManager(t, host.Options{OwnerMemoryPoolMB: 8192, DiskPoolMBPerOwner: 102400})
	f := newFleet(t, mgr, newIndex(t))
	mustCreate(t, f, "only", "alice")

	got, want := f.CapacityForOwner("alice"), mgr.CapacityForOwner("alice")
	if got != want {
		t.Fatalf("fleet rollup diverged from the manager's:\n got %+v\nwant %+v", got, want)
	}
}

// TestOwnerCapacityCountsSandboxesOnAnUnreachableMachine. Their disk is
// unknown, not zero — but the sandboxes themselves still exist, and dropping
// them from the count would tell someone they had fewer machines than they do.
func TestOwnerCapacityCountsSandboxesOnAnUnreachableMachine(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)

	nodeb := newFakeNode("boxb")
	attach(t, f, nodeb, &host.Sandbox{
		Name: "dark", Owner: "bob", Node: "boxb", Image: "ubuntu", State: vmm.StateRunning,
		VCPUs: 2, MemMB: 4096, DiskMB: 7000, BaseDiskMB: 6144,
	})
	place(t, index, "dark", "bob", "boxb")
	nodeb.setOnline(false)

	c := f.CapacityForOwner("bob")
	if c.TotalSandboxes != 1 {
		t.Fatalf("TotalSandboxes = %d, want 1 even with the machine down: %+v", c.TotalSandboxes, c)
	}
}
