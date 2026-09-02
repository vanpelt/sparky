package host_test

// The owner rollup is the arithmetic behind the user console's footprint card,
// and it is the only place two things are true at once: an owner's disk is
// charged NET of the template blocks their forks share, and the sandboxes being
// charged may live on machines the caller is not. Both halves are tested here
// rather than through a manager, because the second one has no manager to go
// through — a gateway folds records for VMs it does not run.

import (
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

func box(name, owner string, state vmm.State, vcpus, memMB, diskMB, baseMB, capMB int64) *host.Sandbox {
	return &host.Sandbox{
		Name: name, Owner: owner, State: state, VCPUs: vcpus, MemMB: memMB,
		DiskMB: diskMB, BaseDiskMB: baseMB, DiskTotalMB: capMB,
	}
}

// TestRollUpOwnerChargesWrittenBlocksNotCopies is the card's headline claim.
// Three sandboxes forked from the same 6 GB template, each having written a
// little, occupy ~19 GB by their own reckoning and a shade over 1 GB in fact.
func TestRollUpOwnerChargesWrittenBlocksNotCopies(t *testing.T) {
	boxes := []*host.Sandbox{
		box("a", "alice", vmm.StateRunning, 4, 8192, 6500, 6144, 25600),
		box("b", "alice", vmm.StateRunning, 2, 4096, 6300, 6144, 25600),
		box("c", "alice", vmm.StatePaused, 2, 4096, 6244, 6144, 25600),
		box("d", "bob", vmm.StateRunning, 8, 16384, 9000, 6144, 25600),
	}
	c := host.RollUpOwner("alice", boxes, host.OwnerPolicy{DiskPoolMB: 102400}, nil)

	if c.TotalSandboxes != 3 {
		t.Fatalf("TotalSandboxes = %d, want 3 (bob's must not be folded in)", c.TotalSandboxes)
	}
	if want := int64(6500 + 6300 + 6244); c.RawDiskMB != want {
		t.Fatalf("RawDiskMB = %d, want %d", c.RawDiskMB, want)
	}
	if want := int64(356 + 156 + 100); c.UsedDiskMB != want {
		t.Fatalf("UsedDiskMB = %d, want %d — the template must be subtracted once per fork", c.UsedDiskMB, want)
	}
	if c.CapacityDiskMB != 3*25600 {
		t.Fatalf("CapacityDiskMB = %d, want %d", c.CapacityDiskMB, 3*25600)
	}
	// Paused sandboxes cost disk, not RAM or CPU — the same rule admission uses.
	if c.RunningSandboxes != 2 || c.AllocatedMemMB != 8192+4096 || c.AllocatedVCPUs != 6 {
		t.Fatalf("running allocation wrong: %+v", c)
	}
}

// TestRollUpOwnerFloorsAnOversizedBaseline. A baseline larger than the disk
// measured against it is transient — the two figures are sampled separately,
// and a template can be replaced between them — and must never produce a
// negative charge that a sibling's real usage then cancels out.
func TestRollUpOwnerFloorsAnOversizedBaseline(t *testing.T) {
	boxes := []*host.Sandbox{
		box("small", "alice", vmm.StateRunning, 2, 4096, 5000, 9000, 25600),
		box("real", "alice", vmm.StateRunning, 2, 4096, 8000, 6000, 25600),
	}
	c := host.RollUpOwner("alice", boxes, host.OwnerPolicy{}, nil)
	if c.UsedDiskMB != 2000 {
		t.Fatalf("UsedDiskMB = %d, want 2000 — an over-large baseline must floor at 0, not go negative", c.UsedDiskMB)
	}
}

// TestRollUpOwnerWithoutBaselinesChargesRaw is the older-node case: a machine
// that does not send a template baseline yields no sharing dividend at all,
// rather than a wrong one.
func TestRollUpOwnerWithoutBaselinesChargesRaw(t *testing.T) {
	boxes := []*host.Sandbox{box("a", "alice", vmm.StateRunning, 2, 4096, 6500, 0, 25600)}
	c := host.RollUpOwner("alice", boxes, host.OwnerPolicy{}, nil)
	if c.UsedDiskMB != c.RawDiskMB || c.UsedDiskMB != 6500 {
		t.Fatalf("used %d / raw %d, want both 6500", c.UsedDiskMB, c.RawDiskMB)
	}
}

// TestRollUpOwnerCountsArchivedForDiskOnly. An archived sandbox has no live
// filesystem: DiskMB is the size of the object it was uploaded as, which is
// real storage the owner is consuming, but it holds no RAM and no vCPU.
func TestRollUpOwnerCountsArchivedForDiskOnly(t *testing.T) {
	boxes := []*host.Sandbox{box("parked", "alice", vmm.StateArchived, 4, 8192, 3200, 0, 25600)}
	c := host.RollUpOwner("alice", boxes, host.OwnerPolicy{}, nil)
	if c.ArchivedSandboxes != 1 || c.TotalSandboxes != 1 {
		t.Fatalf("archived accounting wrong: %+v", c)
	}
	if c.UsedDiskMB != 3200 {
		t.Fatalf("UsedDiskMB = %d, want 3200", c.UsedDiskMB)
	}
	if c.RunningSandboxes != 0 || c.AllocatedMemMB != 0 || c.AllocatedVCPUs != 0 {
		t.Fatalf("an archived sandbox must charge no RAM or CPU: %+v", c)
	}
}

// TestRollUpOwnerChargesReserveUnderOvercommit: without a live balloon reading,
// a running guest is charged the working-set floor, not its ceiling — which is
// what the host is actually holding back for it.
func TestRollUpOwnerChargesReserveUnderOvercommit(t *testing.T) {
	boxes := []*host.Sandbox{
		box("a", "alice", vmm.StateRunning, 4, 16384, 0, 0, 0),
		box("b", "alice", vmm.StateRunning, 4, 16384, 0, 0, 0),
	}
	c := host.RollUpOwner("alice", boxes, host.OwnerPolicy{ReserveMemMB: 2048, MemoryPoolMB: 8192}, nil)
	if c.AllocatedMemMB != 32768 {
		t.Fatalf("AllocatedMemMB = %d, want 32768 (the ceiling still shows)", c.AllocatedMemMB)
	}
	if c.EffectiveMemMB != 4096 {
		t.Fatalf("EffectiveMemMB = %d, want 4096 (2 × the reserve)", c.EffectiveMemMB)
	}
	// With no reading supplied, resident stands in as the admission charge —
	// never as the ceiling, which would erase the overcommit the card exists
	// to show.
	if c.ResidentMemMB != 4096 {
		t.Fatalf("ResidentMemMB = %d, want the admission charge 4096", c.ResidentMemMB)
	}
	if c.BorrowedMemMB != 0 {
		t.Fatalf("BorrowedMemMB = %d, want 0 — 4 GB charged against an 8 GB pool", c.BorrowedMemMB)
	}
}

// TestRollUpOwnerUsesTheLiveReadingWhenGiven.
func TestRollUpOwnerUsesTheLiveReadingWhenGiven(t *testing.T) {
	boxes := []*host.Sandbox{box("a", "alice", vmm.StateRunning, 4, 16384, 0, 0, 0)}
	c := host.RollUpOwner("alice", boxes, host.OwnerPolicy{ReserveMemMB: 2048},
		func(*host.Sandbox) int64 { return 900 })
	if c.ResidentMemMB != 900 {
		t.Fatalf("ResidentMemMB = %d, want the live 900", c.ResidentMemMB)
	}
	if c.EffectiveMemMB != 2048 {
		t.Fatalf("EffectiveMemMB = %d, want the unchanged admission charge 2048", c.EffectiveMemMB)
	}
}

// TestMergeAddsUsageAndDoesNotAddPools is the whole reason Merge exists. An
// owner whose sandboxes straddle two machines has not been granted two disk
// pools; every node in a fleet is configured with the same numbers.
func TestMergeAddsUsageAndDoesNotAddPools(t *testing.T) {
	policy := host.OwnerPolicy{
		MemoryPoolMB: 8192, MemoryBurstMB: 16384, DiskPoolMB: 102400,
		ReserveMemMB: 2048, MaxRunning: 4, MaxSandboxes: 8,
	}
	here := host.RollUpOwner("alice", []*host.Sandbox{
		box("a", "alice", vmm.StateRunning, 4, 8192, 7000, 6144, 25600),
	}, policy, nil)
	// The other machine states no per-owner sandbox caps: a capacity report
	// does not carry them, so they must survive the merge from this side.
	there := host.RollUpOwner("alice", []*host.Sandbox{
		box("b", "alice", vmm.StateRunning, 2, 8192, 6500, 6144, 25600),
		box("c", "alice", vmm.StatePaused, 2, 4096, 6200, 6144, 25600),
	}, host.OwnerPolicy{
		MemoryPoolMB: 8192, MemoryBurstMB: 16384, DiskPoolMB: 102400, ReserveMemMB: 2048,
	}, nil)

	c := here.Merge(there)
	if c.Owner != "alice" {
		t.Fatalf("Owner = %q", c.Owner)
	}
	if c.DiskPoolMB != 102400 || c.MemoryPoolMB != 8192 || c.MemoryBurstMB != 16384 {
		t.Fatalf("pools must not sum across machines: %+v", c)
	}
	if c.MaxRunning != 4 || c.MaxSandboxes != 8 {
		t.Fatalf("caps stated by one machine must survive the merge: %+v", c)
	}
	if c.TotalSandboxes != 3 || c.RunningSandboxes != 2 || c.Nodes != 2 {
		t.Fatalf("counts wrong: %+v", c)
	}
	if want := int64(856 + 356 + 56); c.UsedDiskMB != want {
		t.Fatalf("UsedDiskMB = %d, want %d", c.UsedDiskMB, want)
	}
	if want := int64(7000 + 6500 + 6200); c.RawDiskMB != want {
		t.Fatalf("RawDiskMB = %d, want %d", c.RawDiskMB, want)
	}
	if c.AllocatedVCPUs != 6 || c.AllocatedMemMB != 16384 {
		t.Fatalf("running allocation wrong: %+v", c)
	}
}

// TestMergeRecomputesBorrowingAgainstOnePool. Borrowing is derived, so adding
// two halves that each fit inside the pool must not report zero when the total
// does not fit — and must not add two borrowings computed against the same pool.
func TestMergeRecomputesBorrowingAgainstOnePool(t *testing.T) {
	policy := host.OwnerPolicy{MemoryPoolMB: 8192}
	half := func(name string) host.OwnerCapacity {
		return host.RollUpOwner("alice", []*host.Sandbox{
			box(name, "alice", vmm.StateRunning, 2, 6144, 0, 0, 0),
		}, policy, nil)
	}
	if b := half("a").BorrowedMemMB; b != 0 {
		t.Fatalf("one 6 GB guest in an 8 GB pool borrowed %d, want 0", b)
	}
	c := half("a").Merge(half("b"))
	if c.EffectiveMemMB != 12288 {
		t.Fatalf("EffectiveMemMB = %d, want 12288", c.EffectiveMemMB)
	}
	if c.BorrowedMemMB != 4096 {
		t.Fatalf("BorrowedMemMB = %d, want 4096 (12 GB charged against one 8 GB pool)", c.BorrowedMemMB)
	}
}

// TestMergeIntoAnEmptyRollup is the shape a gateway with no local sandboxes
// takes: its own manager holds nothing and states no pools, and the answer must
// come out as the node's, not as zeroes.
func TestMergeIntoAnEmptyRollup(t *testing.T) {
	empty := host.RollUpOwner("alice", nil, host.OwnerPolicy{}, nil)
	if empty.Nodes != 0 {
		t.Fatalf("Nodes = %d, want 0 for a machine holding none of this owner's sandboxes", empty.Nodes)
	}
	node := host.RollUpOwner("alice", []*host.Sandbox{
		box("a", "alice", vmm.StateRunning, 4, 8192, 7000, 6144, 25600),
	}, host.OwnerPolicy{MemoryPoolMB: 8192, DiskPoolMB: 102400}, nil)

	c := empty.Merge(node)
	if c.Owner != "alice" || c.DiskPoolMB != 102400 || c.MemoryPoolMB != 8192 {
		t.Fatalf("an empty local rollup must not erase the node's policy: %+v", c)
	}
	if c.UsedDiskMB != 856 || c.TotalSandboxes != 1 || c.Nodes != 1 {
		t.Fatalf("usage wrong: %+v", c)
	}
}
