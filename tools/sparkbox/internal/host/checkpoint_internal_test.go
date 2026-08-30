package host

// White-box tests for the disk-lock probe: it reaches lockDiskOperation
// directly, because what it pins is that the probe reports the lock's state
// without ever taking it.

import (
	"context"
	"testing"
	"time"
)

// TestDiskOperationReportsWithoutBlocking is the whole contract of the probe.
//
// ctlops.PlanSelfSnapshot calls it while a guest is waiting on an HTTP request,
// to warn that a capture will queue behind an archive already in flight —
// lockDiskOperation is a plain blocking mutex with no busy error, so without the
// warning the guest is told its session is about to end and then keeps running
// for up to fifteen minutes. A probe that took the lock to answer would make
// that warning cost the fifteen minutes it is warning about.
func TestDiskOperationReportsWithoutBlocking(t *testing.T) {
	m := &Manager{}

	if op, busy := m.DiskOperation("quiet-lake"); busy || op != "" {
		t.Errorf("an idle sandbox reported %q busy=%v", op, busy)
	}

	unlock := m.lockDiskOperation("quiet-lake")

	// Answered from another goroutine, with a deadline: a probe that blocked on
	// the held mutex would hang here rather than fail.
	type answer struct {
		op   string
		busy bool
	}
	got := make(chan answer, 1)
	go func() {
		op, busy := m.DiskOperation("quiet-lake")
		got <- answer{op, busy}
	}()
	select {
	case a := <-got:
		if !a.busy {
			t.Errorf("a sandbox with its disk lock held reported idle")
		}
		// The name comes off the call stack, so this also pins that the stack
		// walk did not pick up lockDiskOperation itself or a closure.
		if a.op != "testdiskoperationreportswithoutblocking" {
			t.Errorf("operation = %q, want the name of whoever took the lock", a.op)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DiskOperation blocked on the lock it is only supposed to report on")
	}

	// A different sandbox is unaffected: the lock is per box, and so is this.
	if op, busy := m.DiskOperation("other-box"); busy || op != "" {
		t.Errorf("another sandbox reported %q busy=%v", op, busy)
	}

	unlock()
	if op, busy := m.DiskOperation("quiet-lake"); busy || op != "" {
		t.Errorf("after the unlock it still reported %q busy=%v", op, busy)
	}

	// And the mutex is still the real serialization: the record is bookkeeping
	// beside it, never a replacement for it.
	second := m.lockDiskOperation("quiet-lake")
	defer second()
	if _, busy := m.DiskOperation("quiet-lake"); !busy {
		t.Error("re-taking the lock did not record a new operation")
	}
}

// diskOpWatcher is a pre-capture tool refresh that answers one question: what
// does the probe say while a REAL capture holds the lock.
type diskOpWatcher struct {
	m   *Manager
	op  string
	hit bool
}

func (w *diskOpWatcher) RefreshTools(_ context.Context, b *Sandbox) error {
	w.op, w.hit = w.m.DiskOperation(b.Name)
	return nil
}

// TestDiskOperationNamesTheRealCapture. The name is read off the call stack, so
// this is what pins that the offset is right for a production call site and not
// only for a test that calls lockDiskOperation itself — and "snapshot" is the
// word a guest actually reads in the disk-busy warning.
func TestDiskOperationNamesTheRealCapture(t *testing.T) {
	m := internalManager(t, Options{})
	watcher := &diskOpWatcher{m: m}
	m.SetToolSync(watcher)
	ctx := context.Background()
	if _, err := m.Create(ctx, "quiet-lake", "alice", "ubuntu", 1, 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(ctx, "quiet-lake", "web-260829-1412", "alice"); err != nil {
		t.Fatal(err)
	}
	if !watcher.hit {
		t.Fatal("a capture in progress reported its own disk as idle")
	}
	if watcher.op != "snapshot" {
		t.Errorf("operation = %q, want %q — this is the word the guest's warning prints", watcher.op, "snapshot")
	}
}
