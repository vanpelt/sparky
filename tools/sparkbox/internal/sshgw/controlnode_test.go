package sshgw

import (
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// The egress marker exists for exactly one state — a fleet that meters some
// machines and not others — because that is the one that renders as an owner's
// bandwidth panel of zeroes with nothing anywhere else explaining it. Every
// other combination must leave the listing as it was.
func TestNodeListFlagsOnlyAMixedFleetsUnmeteredMachines(t *testing.T) {
	metered := ctlops.NodeInfo{Name: "sparky", Status: "approved", Online: true, Egress: true}
	bare := ctlops.NodeInfo{Name: "laptop", Status: "approved", Online: true}
	pending := ctlops.NodeInfo{Name: "newcomer", Status: "pending"}

	for _, tc := range []struct {
		name  string
		nodes []ctlops.NodeInfo
		want  bool
	}{
		{"nothing meters: a deployment choice doctor already reports",
			[]ctlops.NodeInfo{bare, bare}, false},
		{"everything meters", []ctlops.NodeInfo{metered, metered}, false},
		{"half-metered: the state worth a word", []ctlops.NodeInfo{metered, bare}, true},
		// A machine that has not linked has reported no facts at all, so
		// counting it as unmetered would flag every fleet with a pending node.
		{"an unlinked machine is not evidence", []ctlops.NodeInfo{metered, pending}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mixedEgress(tc.nodes); got != tc.want {
				t.Errorf("mixedEgress = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNodeLineMarksTheUnmeteredMachine(t *testing.T) {
	bare := ctlops.NodeInfo{Name: "laptop", Status: "approved", Online: true}
	if line := nodeLine(bare, true); !strings.Contains(line, "no-egress-control") {
		t.Errorf("unmetered line = %q, want the marker", line)
	}
	if line := nodeLine(bare, false); strings.Contains(line, "no-egress-control") {
		t.Errorf("line = %q, want no marker on a uniformly unmetered fleet", line)
	}
	metered := ctlops.NodeInfo{Name: "sparky", Status: "approved", Online: true, Egress: true}
	if line := nodeLine(metered, true); strings.Contains(line, "no-egress-control") {
		t.Errorf("metered line = %q, want no marker", line)
	}
}

// The listing has to answer "which machines run what", because that is the
// question `no online VM node runs qemu` sends an operator here to ask.
func TestNodeLineNamesTheVMM(t *testing.T) {
	qemu := ctlops.NodeInfo{Name: "g084f44", Status: "approved", Online: true, Runner: "qemu"}
	if line := nodeLine(qemu, false); !strings.Contains(line, "vmm=qemu") {
		t.Errorf("line = %q, want vmm=qemu", line)
	}
	// A machine that has not said shows nothing rather than a guess. Every
	// unlinked roster row is in this state, and so is a node linked by a build
	// that predates the capacity field.
	silent := ctlops.NodeInfo{Name: "laptop", Status: "pending"}
	if line := nodeLine(silent, false); strings.Contains(line, "vmm=") {
		t.Errorf("line = %q, want no vmm for a machine that did not say", line)
	}
}
