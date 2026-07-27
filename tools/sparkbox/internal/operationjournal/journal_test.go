package operationjournal

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestJournal(t *testing.T) *Journal {
	t.Helper()
	j, err := Open(filepath.Join(t.TempDir(), "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func testSpec(id, key string, hash byte) Spec {
	return Spec{
		ID: id, IdempotencyKey: key, RequestHash: bytes.Repeat([]byte{hash}, 32),
		Kind: "create", Target: "demo", Initiator: "alice",
		CreatedAt: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
	}
}

func TestClaimIsDurablyIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.db")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, existing, err := j.Claim(context.Background(), testSpec("op-1", "key-1", 1))
	if err != nil || existing {
		t.Fatalf("first claim = %+v, existing %v, err %v", first, existing, err)
	}
	if first.State != StatePending || first.Sequence != 1 {
		t.Fatalf("first claim = %+v", first)
	}
	j.Close()

	j, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	replayed, existing, err := j.Claim(context.Background(), testSpec("op-1", "key-1", 1))
	if err != nil || !existing {
		t.Fatalf("replay = %+v, existing %v, err %v", replayed, existing, err)
	}
	if replayed.ID != first.ID || replayed.Sequence != first.Sequence {
		t.Fatalf("replayed %+v, want %+v", replayed, first)
	}
}

func TestRecoverInterruptedAfterReopenTerminalizesWithoutReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.db")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, spec := range []Spec{
		testSpec("op-pending", "key-pending", 1),
		testSpec("op-running", "key-running", 2),
		testSpec("op-succeeded", "key-succeeded", 3),
	} {
		if _, _, err := j.Claim(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.Start(ctx, "op-running"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Start(ctx, "op-succeeded"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Succeed(ctx, "op-succeeded", []byte("done")); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	j, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	recovered, err := j.RecoverInterrupted(ctx, Failure{
		Code:      "outcome_indeterminate",
		Message:   "node restarted before outcome was durable",
		Retryable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 2 {
		t.Fatalf("recovered %d operations, want 2: %+v", len(recovered), recovered)
	}
	for _, id := range []string{"op-pending", "op-running"} {
		op, err := j.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if op.State != StateFailed || op.Failure == nil ||
			op.Failure.Code != "outcome_indeterminate" || !op.Failure.Retryable {
			t.Fatalf("%s after recovery = %+v", id, op)
		}
		replayed, existing, err := j.Claim(ctx, testSpec(
			id,
			map[string]string{"op-pending": "key-pending", "op-running": "key-running"}[id],
			map[string]byte{"op-pending": 1, "op-running": 2}[id],
		))
		if err != nil || !existing || replayed.State != StateFailed {
			t.Fatalf("replay %s = %+v, existing %v, err %v", id, replayed, existing, err)
		}
	}
	succeeded, err := j.Get(ctx, "op-succeeded")
	if err != nil {
		t.Fatal(err)
	}
	if succeeded.State != StateSucceeded || string(succeeded.Result) != "done" {
		t.Fatalf("terminal operation changed during recovery: %+v", succeeded)
	}
}

func TestClaimRejectsIdentityReuseWithDifferentRequest(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	if _, _, err := j.Claim(ctx, testSpec("op-1", "key-1", 1)); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []Spec{
		testSpec("op-1", "key-1", 2),
		testSpec("op-1", "other-key", 1),
		testSpec("other-id", "key-1", 1),
	} {
		if _, _, err := j.Claim(ctx, spec); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("Claim(%+v) error = %v, want identity conflict", spec, err)
		}
	}
}

func TestConcurrentClaimHasOneWinner(t *testing.T) {
	j := openTestJournal(t)
	const callers = 100
	var wg sync.WaitGroup
	wg.Add(callers)
	results := make(chan bool, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			defer wg.Done()
			_, existing, err := j.Claim(context.Background(), testSpec("op-1", "key-1", 1))
			results <- existing
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var newClaims int
	for existing := range results {
		if !existing {
			newClaims++
		}
	}
	if newClaims != 1 {
		t.Fatalf("new claims = %d, want 1", newClaims)
	}
}

func TestTransitionsAndWatchSurviveLostReply(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	if _, _, err := j.Claim(ctx, testSpec("op-1", "key-1", 1)); err != nil {
		t.Fatal(err)
	}
	watchCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ch := j.Watch(watchCtx, "op-1", 1)

	running, err := j.Start(ctx, "op-1")
	if err != nil || running.Sequence != 2 {
		t.Fatalf("Start = %+v, %v", running, err)
	}
	got := <-ch
	if got.State != StateRunning || got.Sequence != 2 {
		t.Fatalf("watch running = %+v", got)
	}

	wantResult := []byte(`{"name":"demo"}`)
	done, err := j.Succeed(ctx, "op-1", wantResult)
	if err != nil {
		t.Fatal(err)
	}
	got = <-ch
	if got.State != StateSucceeded || !bytes.Equal(got.Result, wantResult) {
		t.Fatalf("watch succeeded = %+v", got)
	}
	if _, ok := <-ch; ok {
		t.Fatal("watch did not close after terminal state")
	}

	// A gateway that lost the successful reply gets the terminal result by
	// claiming the same identity again, without executing work again.
	replayed, existing, err := j.Claim(ctx, testSpec("op-1", "key-1", 1))
	if err != nil || !existing || replayed.State != StateSucceeded {
		t.Fatalf("replay after lost reply = %+v, existing %v, err %v", replayed, existing, err)
	}
	if !bytes.Equal(done.Result, replayed.Result) {
		t.Fatalf("result = %q, want %q", replayed.Result, done.Result)
	}
}

func TestTerminalOperationCannotBeChanged(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	if _, _, err := j.Claim(ctx, testSpec("op-1", "key-1", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Fail(ctx, "op-1", Failure{Code: "capacity", Message: "full", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Start(ctx, "op-1"); !errors.Is(err, ErrTerminal) {
		t.Fatalf("Start terminal error = %v", err)
	}
	op, err := j.Get(ctx, "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if op.Failure == nil || op.Failure.Code != "capacity" || !op.Failure.Retryable {
		t.Fatalf("failure = %+v", op.Failure)
	}
}
