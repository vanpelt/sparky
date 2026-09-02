package grpccontrol

// The inventory conversion is the node's half of the reflink accounting. A
// baseline that stays home makes a gateway charge every remote fork for a copy
// that was never made, and the failure is silent — the number is simply four
// times too large — so it is pinned here rather than left to an integration
// test to notice.

import (
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

func TestSandboxToProtoCarriesTheTemplateBaseline(t *testing.T) {
	box := &host.Sandbox{
		Name: "brave-meadow", Owner: "alice", Image: "universal", State: vmm.StateRunning,
		VCPUs: 4, MemMB: 8192, DiskMB: 6800, BaseDiskMB: 6144, DiskTotalMB: 25600,
	}
	wire := sandboxToProto(box)
	if wire.GetBaseDiskMb() != 6144 {
		t.Fatalf("base_disk_mb = %d, want 6144", wire.GetBaseDiskMb())
	}
	if wire.GetDiskMb() != 6800 || wire.GetDiskTotalMb() != 25600 {
		t.Fatalf("the other disk figures moved: %+v", wire)
	}
}

// TestSandboxToProtoLeavesAnUnmeasuredBaselineZero. A driver that cannot
// measure templates must send 0 rather than anything invented: 0 is what the
// gateway reads as "charge this one raw".
func TestSandboxToProtoLeavesAnUnmeasuredBaselineZero(t *testing.T) {
	wire := sandboxToProto(&host.Sandbox{Name: "plain", DiskMB: 6800})
	if wire.GetBaseDiskMb() != 0 {
		t.Fatalf("base_disk_mb = %d, want 0", wire.GetBaseDiskMb())
	}
}
