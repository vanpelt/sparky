package ctlops

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestGoDedupesIdenticalWork: firing two archives of one sandbox is never what a
// retry meant, so an identical (owner, op, resource) triple joins the running
// job instead of starting a second. That is what makes every long operation
// idempotent for free.
func TestGoDedupesIdenticalWork(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	ran := make(chan struct{}, 8)

	start := func() *Job {
		return r.ops.Go(alice(), "archive", Ref{Type: "sandbox", Name: "alicebox"}, time.Minute,
			func(ctx context.Context) (any, error) {
				ran <- struct{}{}
				<-release
				return "done", nil
			})
	}
	first := start()
	second := start()
	if first.ID != second.ID {
		t.Fatalf("duplicate work started a second job: %s vs %s", first.ID, second.ID)
	}
	// A different owner asking for the same resource is a different job — the
	// dedup key must not be a cross-tenant rendezvous.
	other := r.ops.Go(mallory(), "archive", Ref{Type: "sandbox", Name: "alicebox"}, time.Minute,
		func(ctx context.Context) (any, error) { ran <- struct{}{}; <-release; return nil, nil })
	if other.ID == first.ID {
		t.Fatal("two owners shared one job")
	}
	close(release)

	done := r.ops.Await(context.Background(), first, 2*time.Second)
	if done.State != JobSucceeded {
		t.Fatalf("state = %s, want succeeded", done.State)
	}
	var result string
	if err := json.Unmarshal(done.Result, &result); err != nil || result != "done" {
		t.Errorf("result = %s (%v), want \"done\"", done.Result, err)
	}
	if len(ran) != 2 {
		t.Errorf("fn ran %d times, want 2 (one per distinct job)", len(ran))
	}
}

// TestAwaitReturnsEarlyAndLate is the sync-first mechanism: the REST edge Awaits
// for the Prefer window and answers 200 or 202 depending on what came back.
func TestAwaitReturnsEarlyAndLate(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	j := r.ops.Go(alice(), "pause", Ref{Type: "sandbox", Name: "alicebox"}, time.Minute,
		func(ctx context.Context) (any, error) { <-release; return nil, nil })

	if got := r.ops.Await(context.Background(), j, 20*time.Millisecond); got.State != JobRunning {
		t.Fatalf("state = %s, want still running after a short window", got.State)
	}
	close(release)
	if got := r.ops.Await(context.Background(), j, 2*time.Second); got.State != JobSucceeded {
		t.Fatalf("state = %s, want succeeded once the work finished", got.State)
	}
}

// TestAwaitHonoursCallerCancellation: the job keeps running (that is the point
// of a detached context), but the caller stops waiting for it.
func TestAwaitHonoursCallerCancellation(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	defer close(release)
	j := r.ops.Go(alice(), "archive", Ref{Type: "sandbox", Name: "alicebox"}, time.Minute,
		func(ctx context.Context) (any, error) { <-release; return nil, nil })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := r.ops.Await(ctx, j, time.Minute); got.State != JobRunning {
		t.Fatalf("state = %s; Await must return the running snapshot, not block", got.State)
	}
}

func TestJobFailureIsClassified(t *testing.T) {
	r := newRig(t)
	j := r.ops.Go(alice(), "resize", Ref{Type: "sandbox", Name: "alicebox"}, time.Minute,
		func(ctx context.Context) (any, error) { return nil, errors.New("resize2fs died") })

	got := r.ops.Await(context.Background(), j, 2*time.Second)
	if got.State != JobFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	if got.Error == nil || got.Error.Kind != KindInternal || got.Error.Op != "resize" {
		t.Fatalf("error = %+v, want a classified KindInternal stamped with the op", got.Error)
	}
}

// TestCancelJob distinguishes "we gave up" from "the work failed", which is the
// whole reason JobCanceled exists.
func TestCancelJob(t *testing.T) {
	r := newRig(t)
	j := r.ops.Go(alice(), "archive", Ref{Type: "sandbox", Name: "alicebox"}, time.Minute,
		func(ctx context.Context) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})

	got, err := r.ops.CancelJob(alice(), j.ID)
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if got.State != JobCanceled {
		t.Fatalf("state = %s, want canceled", got.State)
	}
	if got.Error != nil {
		t.Errorf("a canceled job should not also report an error: %+v", got.Error)
	}

	// Cancelling a finished job is a conflict, not a second cancellation.
	_, err = r.ops.CancelJob(alice(), j.ID)
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindConflict || e.Code != "job_finished" {
		t.Fatalf("second cancel = %v, want KindConflict/job_finished", err)
	}
}

// TestListJobsIsOwnerScoped: the registry must not be a directory of everyone's
// work in progress.
func TestListJobsIsOwnerScoped(t *testing.T) {
	r := newRig(t)
	noop := func(ctx context.Context) (any, error) { return nil, nil }
	a := r.ops.Go(alice(), "pause", Ref{Type: "sandbox", Name: "alicebox"}, time.Minute, noop)
	r.ops.Go(mallory(), "pause", Ref{Type: "sandbox", Name: "mbox"}, time.Minute, noop)
	r.ops.Await(context.Background(), a, time.Second)

	mine, err := r.ops.ListJobs(alice())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != a.ID {
		t.Fatalf("ListJobs(alice) = %d jobs, want only alice's", len(mine))
	}
}

// TestJobRetentionAndEviction covers both bounds on the in-memory registry: the
// time-based reaper and the per-owner cap, which never evicts a running job
// because a job the client can no longer poll is worse than an oversized map.
func TestJobRetentionAndEviction(t *testing.T) {
	r := newRig(t)
	noop := func(ctx context.Context) (any, error) { return nil, nil }

	// A finished job older than JobRetain is reaped.
	j := r.ops.Go(alice(), "pause", Ref{Type: "sandbox", Name: "old"}, time.Minute, noop)
	r.ops.Await(context.Background(), j, time.Second)
	r.ops.jobsMu.Lock()
	old := r.ops.now().Add(-2 * JobRetain)
	r.ops.jobs[j.ID].FinishedAt = &old
	r.ops.jobsMu.Unlock()

	r.ops.reapOnce()
	if _, err := r.ops.Job(alice(), j.ID); !IsKind(err, KindNotFound) {
		t.Errorf("a job older than JobRetain survived the reaper: %v", err)
	}

	// The per-owner cap evicts the oldest finished job first.
	r2 := newRig(t)
	var last string
	for i := 0; i < JobMaxPerOwner+5; i++ {
		jb := r2.ops.Go(alice(), "pause", Ref{Type: "sandbox", Name: "b" + itoaTest(i)}, time.Minute, noop)
		r2.ops.Await(context.Background(), jb, time.Second)
		last = jb.ID
	}
	jobs, err := r2.ops.ListJobs(alice())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) > JobMaxPerOwner {
		t.Errorf("registry holds %d jobs, want at most %d", len(jobs), JobMaxPerOwner)
	}
	if _, err := r2.ops.Job(alice(), last); err != nil {
		t.Errorf("the newest job was evicted: %v", err)
	}
}

// TestCloseIsIdempotent — Close is wired to a defer somewhere and will be called
// twice eventually.
func TestCloseIsIdempotent(t *testing.T) {
	r := newRig(t)
	r.ops.Close()
	r.ops.Close()
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
