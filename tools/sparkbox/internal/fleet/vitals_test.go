package fleet_test

// Where a live counter comes from.
//
// A balloon and a VMM process can only be asked of the machine running them, and
// for as long as the fleet has existed every meter in the platform asked the
// LOCAL manager. That was correct for the gateway's own sandboxes and blank for
// everyone else's — the further the fleet spreads, the more of the platform's
// instrumentation goes dark. These tests pin the routing that fixes it, and the
// two refusals that must survive it.

import (
	"context"
	"errors"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// vitalsNode is a buildingNode that also reports readings, so a test can create
// a sandbox on it and then ask for that sandbox's counters.
func vitalsNode(t *testing.T, f *fleet.Fleet, name string, cpu map[string]float64) *buildingNode {
	t.Helper()
	n := newBuildingNode(name)
	n.vitals = cpu
	attachBuilder(t, f, n)
	return n
}

// The item in one test: the reading comes from the machine holding the sandbox,
// not from the machine that was asked.
func TestVitalsComeFromTheMachineHoldingTheSandbox(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	nodeb := vitalsNode(t, f, "boxb", map[string]float64{"far-away": 42})

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}

	v, err := f.Vitals(context.Background(), "far-away")
	if err != nil {
		t.Fatalf("Vitals: %v", err)
	}
	if v.CPUSeconds == nil {
		t.Fatal("no reading at all — the gateway answered for a sandbox it does not run")
	}
	if *v.CPUSeconds != 42 {
		t.Errorf("cpu_seconds = %v, want boxb's 42", *v.CPUSeconds)
	}
	if !nodeb.took("vitals") {
		t.Error("the machine holding the sandbox was never asked")
	}
}

// A sandbox on this machine is still answered here, with no node consulted and
// no link involved. It is the case every single-machine deployment is, and the
// one a routing change is most likely to make pay for a fleet it does not have.
func TestVitalsForALocalSandboxNeverLeaveTheMachine(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	nodeb := vitalsNode(t, f, "boxb", map[string]float64{"here": 99})

	mustCreate(t, f, "here", "alice")

	if _, err := f.Vitals(context.Background(), "here"); err != nil {
		t.Fatalf("Vitals: %v", err)
	}
	if nodeb.took("vitals") {
		t.Error("a local sandbox's counters were read off another machine")
	}
}

// The refusal that matters more than the reading: when the owning machine is
// not answering, the answer is that machine's outage — never a reading from
// here.
//
// Falling back to the local manager would be the easy mistake, and it is a
// cross-tenant one. Every machine in a fleet mints its guests the same
// 172.30.x.y addresses and the local manager holds a DIFFERENT sandbox for any
// name it happens to share, so a "helpful" local answer draws one person's CPU
// under another person's name, with no error and no log line.
func TestVitalsRefuseRatherThanAnswerFromTheWrongMachine(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	nodeb := vitalsNode(t, f, "boxb", map[string]float64{"far-away": 42})

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	nodeb.setOnline(false)

	v, err := f.Vitals(context.Background(), "far-away")
	if err == nil {
		t.Fatalf("an offline machine's sandbox answered %+v, want its outage", v)
	}
	var typed *ctlops.Error
	if !errors.As(err, &typed) || typed.Kind != ctlops.KindCapacity {
		t.Fatalf("err = %v (%T), want the typed unreachable-node error", err, err)
	}
	if !v.Empty() {
		t.Errorf("reading = %+v, want nothing at all beside the error", v)
	}
}

// A machine that is reachable and simply has no numbers — a stats-less driver,
// a sandbox it does not run — answers the empty reading and NOT an error. The
// two are rendered identically by every surface, but only one of them is worth
// a log line, and a caller that logged both would print one a second per open
// terminal tab forever.
func TestVitalsWithNoReadingAreNotAnError(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	vitalsNode(t, f, "boxb", nil)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}

	v, err := f.Vitals(context.Background(), "far-away")
	if err != nil {
		t.Fatalf("a machine with no counters raised %v, want the empty reading", err)
	}
	if !v.Empty() {
		t.Errorf("reading = %+v, want nothing", v)
	}
}
