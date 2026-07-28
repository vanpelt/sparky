package host_test

// Turbo mode: a sandbox restarted with doubled CPU and RAM for one run.
//
// The three things worth pinning down are all about the "one run" part, which
// is the whole design and the only part a reader cannot see from the signature:
// the doubled figures land in VCPUs/MemMB (so every meter, every admission
// check and the cold boot's own config keep reading one pair of fields), the
// next pause hands them back whoever caused it, and a host that cannot afford
// the boot refuses before anything is torn down rather than after.

import (
	"context"
	"errors"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

func TestTurboDoublesAndColdBoots(t *testing.T) {
	var rd *recordingDriver
	m := newTestManagerWith(t, host.Options{}, func(d *mock.Driver) vmm.Driver {
		rd = &recordingDriver{Driver: d}
		return rd
	})
	ctx := context.Background()
	mustCreate(t, m, "fast", "alice", 512)

	if err := m.SetTurbo(ctx, "fast", true); err != nil {
		t.Fatalf("turbo on: %v", err)
	}
	b, ok := m.Get("fast")
	if !ok || b.State != vmm.StateRunning {
		t.Fatalf("box not running after turbo: %+v ok=%v", b, ok)
	}
	if !b.Turbo || b.VCPUs != 1*host.TurboFactor || b.MemMB != 512*host.TurboFactor {
		t.Fatalf("turbo record = {turbo:%v vcpus:%d mem:%d}, want {true 2 1024}", b.Turbo, b.VCPUs, b.MemMB)
	}
	// The base allocation is remembered, not recomputed: dividing by the factor
	// on the way back would be a different number for anything odd.
	if b.BaseVCPUs != 1 || b.BaseMemMB != 512 {
		t.Fatalf("base = %d vCPU / %d MB, want 1 / 512", b.BaseVCPUs, b.BaseMemMB)
	}
	// A cold boot, not a resume: firecracker has no CPU hotplug, so the only
	// way the guest can come up bigger is a fresh Create off a dropped snapshot.
	calls := rd.recorded()
	if indexOf(calls, "drop fast") == -1 {
		t.Fatalf("snapshots not dropped for turbo: calls %v", calls)
	}
	if indexOf(calls[1:], "create fast") == -1 {
		t.Fatalf("no cold-boot Create after turbo: calls %v", calls)
	}

	// Off again puts every field back, including clearing the base pair.
	if err := m.SetTurbo(ctx, "fast", false); err != nil {
		t.Fatalf("turbo off: %v", err)
	}
	b, _ = m.Get("fast")
	if b.Turbo || b.VCPUs != 1 || b.MemMB != 512 || b.BaseVCPUs != 0 || b.BaseMemMB != 0 {
		t.Fatalf("after turbo off = %+v, want the original 1 vCPU / 512 MB and no base", b)
	}
}

// Turbo lasts one run. Pause is the single place that hands it back, so an
// idle reap, an explicit pause and a reboot all end it — this covers the
// explicit one, and the reaper and Reboot reach the same code path.
func TestTurboEndsAtTheNextPause(t *testing.T) {
	m := newTestManager(t, host.Options{})
	ctx := context.Background()
	mustCreate(t, m, "burst", "alice", 512)

	if err := m.SetTurbo(ctx, "burst", true); err != nil {
		t.Fatalf("turbo on: %v", err)
	}
	if err := m.Pause(ctx, "burst"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	b, _ := m.Get("burst")
	if b.Turbo || b.VCPUs != 1 || b.MemMB != 512 {
		t.Fatalf("pause did not release turbo: %+v", b)
	}
	// And the resume that follows comes up at the sandbox's own size rather
	// than inheriting what the last session borrowed.
	if _, err := m.EnsureReady(ctx, "burst"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	b, _ = m.Get("burst")
	if b.Turbo || b.MemMB != 512 {
		t.Fatalf("resume came back in turbo: %+v", b)
	}
}

// A reboot pauses on its way to the cold boot, so it ends turbo too. Stated as
// its own test because "reboot keeps my turbo" is exactly the assumption a
// user would make, and the answer has to be deliberate rather than incidental.
func TestTurboEndsOnReboot(t *testing.T) {
	m := newTestManager(t, host.Options{})
	ctx := context.Background()
	mustCreate(t, m, "cycle", "alice", 512)

	if err := m.SetTurbo(ctx, "cycle", true); err != nil {
		t.Fatalf("turbo on: %v", err)
	}
	if err := m.Reboot(ctx, "cycle"); err != nil {
		t.Fatalf("reboot: %v", err)
	}
	if b, _ := m.Get("cycle"); b.Turbo || b.MemMB != 512 {
		t.Fatalf("reboot kept turbo: %+v", b)
	}
}

// The admission check runs before the pause. A refusal must leave the sandbox
// exactly as it was — running, at its own size — rather than parked with an
// error, because the request that would have woken it may never come again.
func TestTurboRefusedOverCapacityLeavesTheBoxAlone(t *testing.T) {
	// Budget is 1000 MB (2000 * 50%): one 512 MB box fits, its doubled 1024
	// does not.
	m := newTestManager(t, host.Options{HostMemMB: 2000, MemAdmissionPct: 50})
	ctx := context.Background()
	mustCreate(t, m, "greedy", "alice", 512)

	err := m.SetTurbo(ctx, "greedy", true)
	if err == nil {
		t.Fatal("turbo admitted over the memory budget")
	}
	var capErr *host.CapacityError
	if !errors.As(err, &capErr) {
		t.Fatalf("error = %v, want a *host.CapacityError", err)
	}
	b, _ := m.Get("greedy")
	if b.State != vmm.StateRunning || b.Turbo || b.MemMB != 512 {
		t.Fatalf("refused turbo disturbed the box: %+v", b)
	}
}

func TestTurboRefusals(t *testing.T) {
	m := newTestManager(t, host.Options{Archive: newMemStore()})
	ctx := context.Background()
	mustCreate(t, m, "parked", "alice", 512)
	if err := m.Archive(ctx, "parked"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// An archived box has no rootfs on this host to boot, doubled or otherwise.
	if err := m.SetTurbo(ctx, "parked", true); err == nil || !contains(err.Error(), "archived") {
		t.Fatalf("turbo on an archived box: want an archived error, got %v", err)
	}
	if err := m.SetTurbo(ctx, "ghost", true); err == nil || !contains(err.Error(), "not found") {
		t.Fatalf("turbo on a missing box: want not-found, got %v", err)
	}

	// Turbo cannot be released without a snapshot drop, so a driver that cannot
	// drop must never be able to take it in the first place.
	bare := newTestManagerWith(t, host.Options{}, func(d *mock.Driver) vmm.Driver {
		return bareDriver{d: d}
	})
	mustCreate(t, bare, "plain", "alice", 512)
	if err := bare.SetTurbo(ctx, "plain", true); err == nil || !contains(err.Error(), "not enabled") {
		t.Fatalf("turbo without the snapshot-drop capability: want not-enabled, got %v", err)
	}
}

// Asking for the state a sandbox is already in must not restart it. The most
// expensive possible no-op is still a no-op.
func TestTurboIsIdempotent(t *testing.T) {
	var rd *recordingDriver
	m := newTestManagerWith(t, host.Options{}, func(d *mock.Driver) vmm.Driver {
		rd = &recordingDriver{Driver: d}
		return rd
	})
	ctx := context.Background()
	mustCreate(t, m, "steady", "alice", 512)

	if err := m.SetTurbo(ctx, "steady", false); err != nil {
		t.Fatalf("turbo off on a box that is already off: %v", err)
	}
	// The mock records create/drop/rename; a restart would have shown up as
	// both a snapshot drop and a second Create.
	if calls := rd.recorded(); indexOf(calls, "drop steady") != -1 ||
		indexOf(calls[1:], "create steady") != -1 {
		t.Fatalf("a no-op turbo change restarted the box: calls %v", calls)
	}
	if b, _ := m.Get("steady"); b.State != vmm.StateRunning {
		t.Fatalf("state = %q, want running", b.State)
	}
}
