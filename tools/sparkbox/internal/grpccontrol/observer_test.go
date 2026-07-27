package grpccontrol

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
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

	observer.SandboxChanged(&host.Sandbox{Name: "demo"}, "resumed")
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
			if wantRevision == 1 && decoded.GetSandboxChanged().GetSandbox().GetName() != "demo" {
				t.Fatalf("changed event = %v", &decoded)
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
