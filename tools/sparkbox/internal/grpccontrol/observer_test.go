package grpccontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/eventjournal"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"google.golang.org/protobuf/proto"
)

func TestEventObserverPersistsManagerEventsInOrder(t *testing.T) {
	journal, err := eventjournal.Open(filepath.Join(t.TempDir(), "events.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer := NewEventObserver(ctx, journal, slog.New(slog.NewTextHandler(io.Discard, nil)))

	observer.SandboxChanged(&host.Sandbox{Name: "demo", Repos: []host.RepoStatus{{
		Slug: "wandb/agentstream", Path: "/home/sparky/agentstream", Ahead: 2, State: "stale",
	}}}, "resumed")
	observer.SandboxGone("demo")

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	if err := observer.Flush(flushCtx); err != nil {
		t.Fatal(err)
	}
	current, err := journal.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if current != 2 {
		t.Fatalf("revision = %d, want 2", current)
	}
	events, errs := journal.Watch(context.Background(), 0)
	for wantRevision := uint64(1); wantRevision <= 2; wantRevision++ {
		select {
		case event := <-events:
			if event.Revision != wantRevision {
				t.Fatalf("revision = %d, want %d", event.Revision, wantRevision)
			}
			var decoded nodev1.InventoryEvent
			if err := proto.Unmarshal(event.Payload, &decoded); err != nil {
				t.Fatal(err)
			}
			if wantRevision == 1 {
				box := decoded.GetSandboxChanged().GetSandbox()
				if box.GetName() != "demo" || len(box.GetRepos()) != 1 || box.GetRepos()[0].GetAhead() != 2 {
					t.Fatalf("changed event = %v", &decoded)
				}
			}
			if wantRevision == 2 && decoded.GetSandboxGone().GetName() != "demo" {
				t.Fatalf("gone event = %v", &decoded)
			}
		case err := <-errs:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for persisted event")
		}
	}
}

type flakyEventAppender struct {
	mu       sync.Mutex
	failures int
	calls    int
	events   []queuedEvent
}

func (f *flakyEventAppender) Append(_ context.Context, kind string, payload []byte) (eventjournal.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failures > 0 {
		f.failures--
		return eventjournal.Event{}, errors.New("temporary append failure")
	}
	f.events = append(f.events, queuedEvent{kind: kind, payload: append([]byte(nil), payload...)})
	return eventjournal.Event{Revision: uint64(len(f.events))}, nil
}

func TestEventObserverRetriesTransientAppendFailureWithoutLosingOrder(t *testing.T) {
	appender := &flakyEventAppender{failures: 2}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer := NewEventObserver(ctx, appender, slog.New(slog.NewTextHandler(io.Discard, nil)))

	observer.SandboxChanged(&host.Sandbox{Name: "demo"}, "resumed")
	observer.SandboxGone("demo")

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	if err := observer.Flush(flushCtx); err != nil {
		t.Fatal(err)
	}
	if err := observer.Health(); err != nil {
		t.Fatalf("health remained degraded after retry: %v", err)
	}
	appender.mu.Lock()
	defer appender.mu.Unlock()
	if appender.calls != 4 {
		t.Fatalf("append calls = %d, want 4 (2 failures + 2 events)", appender.calls)
	}
	if len(appender.events) != 2 {
		t.Fatalf("persisted events = %d, want 2", len(appender.events))
	}
	for i, wantKind := range []string{eventSandboxChanged, eventSandboxGone} {
		if appender.events[i].kind != wantKind {
			t.Fatalf("event %d kind = %q, want %q", i, appender.events[i].kind, wantKind)
		}
	}
}

type blockingEventAppender struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	once    sync.Once
	events  []queuedEvent
}

func (b *blockingEventAppender) Append(ctx context.Context, kind string, payload []byte) (eventjournal.Event, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-ctx.Done():
		return eventjournal.Event{}, ctx.Err()
	case <-b.release:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, queuedEvent{kind: kind, payload: append([]byte(nil), payload...)})
	return eventjournal.Event{Revision: uint64(len(b.events))}, nil
}

func TestEventObserverCoalescesRepeatedPendingSandboxChanges(t *testing.T) {
	appender := &blockingEventAppender{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer := NewEventObserver(ctx, appender, slog.New(slog.NewTextHandler(io.Discard, nil)))

	observer.SandboxChanged(&host.Sandbox{Name: "demo", MemMB: 1}, "first")
	select {
	case <-appender.started:
	case <-time.After(time.Second):
		t.Fatal("observer did not begin first append")
	}
	for memory := int64(2); memory <= 100; memory++ {
		observer.SandboxChanged(&host.Sandbox{Name: "demo", MemMB: memory}, "updated")
	}
	close(appender.release)

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	if err := observer.Flush(flushCtx); err != nil {
		t.Fatal(err)
	}
	appender.mu.Lock()
	defer appender.mu.Unlock()
	if len(appender.events) != 2 {
		t.Fatalf("persisted events = %d, want active event plus one coalesced update", len(appender.events))
	}
	var latest nodev1.InventoryEvent
	if err := proto.Unmarshal(appender.events[1].payload, &latest); err != nil {
		t.Fatal(err)
	}
	if got := latest.GetSandboxChanged().GetSandbox().GetMemoryMb(); got != 100 {
		t.Fatalf("coalesced memory = %d, want latest value 100", got)
	}
}

func TestEventObserverBoundsDistinctQueueAndRequiresReconciliation(t *testing.T) {
	appender := &blockingEventAppender{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer := NewEventObserver(ctx, appender, slog.New(slog.NewTextHandler(io.Discard, nil)))

	observer.SandboxGone("active")
	select {
	case <-appender.started:
	case <-time.After(time.Second):
		t.Fatal("observer did not begin first append")
	}
	for i := 0; i <= observerQueueLimit; i++ {
		observer.SandboxGone(fmt.Sprintf("sandbox-%d", i))
	}
	if err := observer.Health(); err == nil {
		t.Fatal("queue overflow did not degrade observer health")
	}
	observer.Acknowledge()
	if err := observer.Health(); err == nil {
		t.Fatal("inventory acknowledgement cleared overflow before old events drained")
	}
	close(appender.release)

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	if err := observer.Flush(flushCtx); err == nil {
		t.Fatal("flush hid the dropped-event reconciliation requirement")
	}
	observer.Acknowledge()
	if err := observer.Health(); err != nil {
		t.Fatalf("health after drained queue and reconciliation = %v", err)
	}
}
