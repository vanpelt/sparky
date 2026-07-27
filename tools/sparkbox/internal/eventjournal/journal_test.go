package eventjournal

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRevisionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	ctx := context.Background()
	j, err := Open(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		event, err := j.Append(ctx, "sandbox.changed", []byte{byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		if event.Revision != uint64(i) {
			t.Fatalf("revision = %d, want %d", event.Revision, i)
		}
	}
	j.Close()

	j, err = Open(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	current, err := j.Current(ctx)
	if err != nil || current != 3 {
		t.Fatalf("Current = %d, %v", current, err)
	}
	event, err := j.Append(ctx, "sandbox.gone", []byte("demo"))
	if err != nil || event.Revision != 4 {
		t.Fatalf("Append after restart = %+v, %v", event, err)
	}
}

func TestReplayGapForcesInventoryReconciliation(t *testing.T) {
	j, err := Open(filepath.Join(t.TempDir(), "events.db"), 3)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := j.Append(ctx, "sandbox.changed", []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := j.After(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Revision != 3 || events[2].Revision != 5 {
		t.Fatalf("retained events = %+v", events)
	}
	_, err = j.After(ctx, 1, 0)
	var gap *GapError
	if !errors.As(err, &gap) {
		t.Fatalf("After pruned revision error = %v, want GapError", err)
	}
	if gap.Oldest != 3 || gap.Current != 5 {
		t.Fatalf("gap = %+v", gap)
	}
}

func TestWatchReplaysThenFollows(t *testing.T) {
	j, err := Open(filepath.Join(t.TempDir(), "events.db"), 8)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := j.Append(ctx, "sandbox.changed", []byte("one")); err != nil {
		t.Fatal(err)
	}
	events, errs := j.Watch(ctx, 0)
	if got := <-events; got.Revision != 1 {
		t.Fatalf("first event = %+v", got)
	}
	if _, err := j.Append(ctx, "sandbox.gone", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if got := <-events; got.Revision != 2 || got.Kind != "sandbox.gone" {
		t.Fatalf("second event = %+v", got)
	}
	cancel()
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("watch error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not stop")
	}
}
