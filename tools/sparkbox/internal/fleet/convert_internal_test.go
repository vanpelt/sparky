package fleet

// The gateway's half of the reflink accounting: a template baseline the node
// sent has to survive both hops — the proto into the link's row cache, and the
// row cache into the record the console folds. Losing it at either point makes
// the footprint card claim an owner is holding a copy of the template per
// sandbox, which is the exact thing the card exists to disprove.

import (
	"testing"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
)

func TestTemplateBaselineSurvivesTheWireAndTheRowCache(t *testing.T) {
	row := sandboxRowFromProto(&nodev1.Sandbox{
		Name: "far-away", Owner: "bob", Image: "universal",
		State:    nodev1.SandboxState_SANDBOX_STATE_RUNNING,
		Vcpus:    4,
		MemoryMb: 8192, DiskMb: 6800, BaseDiskMb: 6144, DiskTotalMb: 25600,
	})
	if row.BaseDiskMB != 6144 {
		t.Fatalf("row.BaseDiskMB = %d, want 6144", row.BaseDiskMB)
	}

	// remoteNode.record is the second hop: the row cache into the record every
	// listing and every RPC reply is built from. A bare client is enough — the
	// only thing record asks it for is the node's name.
	box := (&remoteNode{client: &nodelink.Client{}}).record(row, "far-away", "bob")
	if box.BaseDiskMB != 6144 {
		t.Fatalf("sandbox.BaseDiskMB = %d, want 6144", box.BaseDiskMB)
	}
	if box.DiskMB != 6800 || box.DiskTotalMB != 25600 {
		t.Fatalf("the other disk figures moved: %+v", box)
	}
}

// TestServeKeepsTheBaseline. serve rewrites owner, node and the addresses and
// copies everything else; "everything else" now has to include the baseline.
func TestServeKeepsTheBaseline(t *testing.T) {
	f := &Fleet{}
	row := sandboxRowFromProto(&nodev1.Sandbox{
		Name: "far-away", DiskMb: 6800, BaseDiskMb: 6144,
	})
	box := (&remoteNode{client: &nodelink.Client{}}).record(row, "far-away", "bob")
	served := f.serve(box, placement.Row{Name: "far-away", Owner: "bob", Node: "boxb"}, true)
	if served.BaseDiskMB != 6144 {
		t.Fatalf("served.BaseDiskMB = %d, want 6144", served.BaseDiskMB)
	}
}
