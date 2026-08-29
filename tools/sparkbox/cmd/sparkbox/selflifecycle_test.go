package main

import (
	"context"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// TestTheGuestActsAsItsOwnersHandleAndNobodyElses pins the one elevation this
// feature performs. Everywhere else on this surface a Caller is proved by an
// SSH key or a verified edge session; here it is synthesized from a sandbox
// record, so the fact that it comes off the RECORD — and not off anything a
// request or a node supplied — is the whole security property.
func TestTheGuestActsAsItsOwnersHandleAndNobodyElses(t *testing.T) {
	got := callerFor(&host.Sandbox{Name: "quiet-lake", Owner: "alice"})
	if got.Handle != "alice" {
		t.Errorf("caller = %+v, want alice's handle from the record", got)
	}
	// No key fingerprint, because there is no key: an audit line claiming one
	// would be claiming a proof that did not happen.
	if got.KeyFP != "" {
		t.Errorf("caller carried a key fingerprint %q for a request made by a machine", got.KeyFP)
	}
}

// TestARetriedCaptureCollapsesOntoTheOneAlreadyRunning. A guest that retries a
// commit — the shell's own `_call`, a person running the command twice — must
// not start a second capture of the same box: the first one has already paused
// it, and the second would compact a disk that is being compacted.
//
// And two DIFFERENT sandboxes capturing onto one tag must stay two jobs, which
// is what Ref.Args is for: without it the second would silently be answered
// with the first's result and never run at all.
func TestARetriedCaptureCollapsesOntoTheOneAlreadyRunning(t *testing.T) {
	fx := newGatewayFixture(t)
	ops := newGatewayOps(fx.stores)
	t.Cleanup(ops.Close)
	life := selfLifecycleOps{ops: ops}

	// A capture that blocks until released, so both attempts overlap the way a
	// retry does in practice.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	blocked := func(c ctlops.Caller, sandbox, name string) *ctlops.Job {
		return ops.Go(c, "snapshot.create",
			ctlops.Ref{Type: "snapshot", Name: name, Args: sandbox},
			ctlops.ArchiveTimeout,
			func(ctx context.Context) (any, error) {
				<-release
				return nil, nil
			})
	}
	alice := callerFor(&host.Sandbox{Name: "quiet-lake", Owner: "alice"})
	first := blocked(alice, "quiet-lake", "web-260829-1412")
	again := blocked(alice, "quiet-lake", "web-260829-1412")
	if first.ID != again.ID {
		t.Errorf("a retried commit started a second capture (%s then %s)", first.ID, again.ID)
	}
	other := blocked(alice, "amber-hill", "web-260829-1412")
	if other.ID == first.ID {
		t.Errorf("two different sandboxes capturing onto one tag collapsed into one job (%s); "+
			"the second would be answered with the first's result and never run", first.ID)
	}

	// And the real path uses those same three fields, so a change to either
	// side of the pair shows up here rather than as a silently skipped capture.
	if err := life.Snapshot(context.Background(), &host.Sandbox{Name: "quiet-lake", Owner: "alice"},
		ctlops.SnapshotToTagArgs{Sandbox: "ignored", Name: "web-260829-1412", Tag: "web"}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	jobs, err := ops.ListJobs(alice)
	if err != nil {
		t.Fatal(err)
	}
	// Two: quiet-lake's capture and amber-hill's. The real call above named
	// quiet-lake and the same snapshot, so it collapsed onto the first rather
	// than starting a third.
	if len(jobs) != 2 {
		t.Errorf("jobs = %d, want 2 (the real call must collapse onto the running capture of the same box)", len(jobs))
	}
	for _, j := range jobs {
		if j.Resource.Type != "snapshot" {
			t.Errorf("job %s names resource %+v, want a snapshot", j.ID, j.Resource)
		}
	}
}

// TestTheCaptureIgnoresTheSandboxNameItWasHanded. Nothing upstream can set it
// to another box today; stating it here means nothing downstream has to be
// trusted to have checked.
func TestTheCaptureIgnoresTheSandboxNameItWasHanded(t *testing.T) {
	fx := newGatewayFixture(t)
	ops := newGatewayOps(fx.stores)
	t.Cleanup(ops.Close)

	life := selfLifecycleOps{ops: ops}
	if err := life.Snapshot(context.Background(), &host.Sandbox{Name: "quiet-lake", Owner: "alice"},
		ctlops.SnapshotToTagArgs{Sandbox: "mallory-box", Name: "web-260829-1412", Tag: "web"}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	jobs, err := ops.ListJobs(ctlops.Caller{Handle: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if jobs[0].Resource.Args != "quiet-lake" {
		t.Errorf("the job was keyed on %q, want the record's own name", jobs[0].Resource.Args)
	}
}
