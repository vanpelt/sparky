package schedule

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "schedules.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestParseAndNextRun(t *testing.T) {
	// A standard 5-field spec parses and yields a future activation.
	base := time.Date(2026, 7, 16, 10, 5, 0, 0, time.UTC)
	next, err := NextRun("*/30 * * * *", base)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 7, 16, 10, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
	// Descriptors are accepted too.
	if _, err := NextRun("@every 30m", base); err != nil {
		t.Fatalf("@every: %v", err)
	}
	// Garbage is rejected.
	if _, err := Parse("not a cron"); err == nil {
		t.Fatal("expected parse error for bad spec")
	}
}

func TestAddValidatesAndPersists(t *testing.T) {
	st := testStore(t)

	if _, err := st.Add(Entry{Sandbox: "box", Owner: "alice", Spec: "bad", Command: "echo hi"}); err == nil {
		t.Fatal("Add should reject an unparseable spec")
	}
	if _, err := st.Add(Entry{Sandbox: "box", Owner: "alice", Spec: "@hourly", Command: "  "}); err == nil {
		t.Fatal("Add should reject an empty command")
	}

	e, err := st.Add(Entry{Sandbox: "box", Owner: "alice", Spec: "@hourly", Command: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID == "" || e.CreatedAt.IsZero() {
		t.Fatalf("Add didn't stamp ID/CreatedAt: %+v", e)
	}

	got, err := st.Get(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "echo hi" || got.Owner != "alice" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.LastRun.IsZero() {
		t.Fatalf("fresh entry should have no LastRun: %v", got.LastRun)
	}
}

func TestListScopingAndDelete(t *testing.T) {
	st := testStore(t)
	a, _ := st.Add(Entry{Sandbox: "abox", Owner: "alice", Spec: "@daily", Command: "a"})
	st.Add(Entry{Sandbox: "abox2", Owner: "alice", Spec: "@daily", Command: "a2"}) //nolint:errcheck
	st.Add(Entry{Sandbox: "bbox", Owner: "bob", Spec: "@daily", Command: "b"})     //nolint:errcheck

	if got, _ := st.ListByOwner("alice"); len(got) != 2 {
		t.Fatalf("alice owns %d schedules, want 2", len(got))
	}
	if got, _ := st.ListBySandbox("abox"); len(got) != 1 {
		t.Fatalf("abox has %d schedules, want 1", len(got))
	}

	if err := st.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(a.ID); err != ErrNotFound {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}

	// DeleteBySandbox clears a destroyed sandbox's jobs.
	if err := st.DeleteBySandbox("bbox"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ListByOwner("bob"); len(got) != 0 {
		t.Fatalf("bob still owns %d schedules after DeleteBySandbox", len(got))
	}
}

func TestRecordRun(t *testing.T) {
	st := testStore(t)
	e, _ := st.Add(Entry{Sandbox: "box", Owner: "alice", Spec: "@hourly", Command: "echo"})
	at := time.Now().UTC().Truncate(time.Second)
	if err := st.RecordRun(e.ID, at, 3, "exit 3: boom"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(e.ID)
	if !got.LastRun.Equal(at) || got.LastExit != 3 || got.LastError != "exit 3: boom" {
		t.Fatalf("RecordRun not persisted: %+v", got)
	}
}

// fakeRunner records calls and returns a scripted result.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	exit  int
	out   string
	err   error
}

func (f *fakeRunner) RunInSandbox(_ context.Context, sandbox, cmd string) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sandbox+": "+cmd)
	return f.exit, f.out, f.err
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSchedulerFiresDueJobOnce(t *testing.T) {
	st := testStore(t)
	runner := &fakeRunner{}
	sc := NewScheduler(st, runner, discardLog())

	// A spec that fires every minute, created a minute in the past so it's due.
	e, _ := st.Add(Entry{Sandbox: "box", Owner: "alice", Spec: "* * * * *", Command: "job"})
	// Backdate CreatedAt so the previous minute boundary counts as due.
	past := time.Now().Add(-2 * time.Minute).UTC()
	if _, err := st.db.Exec(`UPDATE schedules SET created_at = ? WHERE id = ?`, past, e.ID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	sc.tick(context.Background(), now)

	// The job runs asynchronously; wait for it to land and be recorded.
	waitFor(t, func() bool { return runner.count() == 1 })
	waitFor(t, func() bool {
		g, _ := st.Get(e.ID)
		return !g.LastRun.IsZero()
	})

	// A second tick at the same instant must not re-fire: last_run has advanced
	// past this window.
	sc.tick(context.Background(), now)
	time.Sleep(20 * time.Millisecond)
	if c := runner.count(); c != 1 {
		t.Fatalf("job fired %d times, want 1 (no double-fire in one window)", c)
	}
}

func TestSchedulerRecordsNonZeroExit(t *testing.T) {
	st := testStore(t)
	runner := &fakeRunner{exit: 2, out: "kaboom"}
	sc := NewScheduler(st, runner, discardLog())

	e, _ := st.Add(Entry{Sandbox: "box", Owner: "alice", Spec: "* * * * *", Command: "flaky"})
	past := time.Now().Add(-2 * time.Minute).UTC()
	st.db.Exec(`UPDATE schedules SET created_at = ? WHERE id = ?`, past, e.ID) //nolint:errcheck

	sc.tick(context.Background(), time.Now().UTC())
	waitFor(t, func() bool {
		g, _ := st.Get(e.ID)
		return g.LastExit == 2
	})
	g, _ := st.Get(e.ID)
	if g.LastError == "" {
		t.Fatal("non-zero exit should record an error tail")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

func TestRenameSandbox(t *testing.T) {
	st := testStore(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Add(Entry{Sandbox: "myvm", Owner: "alice", Spec: "@hourly", Command: "echo a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add(Entry{Sandbox: "myvm", Owner: "alice", Spec: "@daily", Command: "echo b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add(Entry{Sandbox: "othervm", Owner: "bob", Spec: "@hourly", Command: "echo c"}); err != nil {
		t.Fatal(err)
	}

	must(st.RenameSandbox("myvm", "newvm"))

	moved, err := st.ListBySandbox("newvm")
	must(err)
	if len(moved) != 2 {
		t.Fatalf("expected 2 entries under newvm, got %d", len(moved))
	}
	old, err := st.ListBySandbox("myvm")
	must(err)
	if len(old) != 0 {
		t.Fatalf("expected no entries left under myvm, got %d", len(old))
	}
	// Other sandboxes are untouched.
	other, err := st.ListBySandbox("othervm")
	must(err)
	if len(other) != 1 {
		t.Fatalf("expected othervm's entry untouched, got %d", len(other))
	}
}
