package grpccontrol

import (
	"context"
	"log/slog"
	"sync"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/eventjournal"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"google.golang.org/protobuf/proto"
)

// EventObserver turns every node-local manager change into a durable,
// revisioned gRPC inventory event. Manager observers must never block while
// holding the lifecycle lock, so callbacks only append to an in-memory queue;
// one worker performs SQLite writes in order.
type EventObserver struct {
	ctx    context.Context
	events *eventjournal.Journal
	log    *slog.Logger

	mu     sync.Mutex
	queue  []queuedEvent
	active bool
	wakeup chan struct{}
}

type queuedEvent struct {
	kind    string
	payload []byte
}

func NewEventObserver(ctx context.Context, events *eventjournal.Journal, log *slog.Logger) *EventObserver {
	if ctx == nil {
		ctx = context.Background()
	}
	if log == nil {
		log = slog.Default()
	}
	o := &EventObserver{
		ctx: ctx, events: events, log: log,
		wakeup: make(chan struct{}, 1),
	}
	go o.run()
	return o
}

func (o *EventObserver) SandboxChanged(box *host.Sandbox, reason string) {
	if o == nil || o.events == nil || box == nil {
		return
	}
	o.offer(eventSandboxChanged, &nodev1.InventoryEvent{
		Event: &nodev1.InventoryEvent_SandboxChanged{
			SandboxChanged: &nodev1.SandboxChanged{
				Sandbox: sandboxToProto(box),
				Reason:  reason,
			},
		},
	})
}

func (o *EventObserver) SandboxGone(name string) {
	if o == nil || o.events == nil || name == "" {
		return
	}
	o.offer(eventSandboxGone, &nodev1.InventoryEvent{
		Event: &nodev1.InventoryEvent_SandboxGone{
			SandboxGone: &nodev1.SandboxGone{Name: name},
		},
	})
}

func (o *EventObserver) offer(kind string, event *nodev1.InventoryEvent) {
	payload, err := proto.Marshal(event)
	if err != nil {
		o.log.Error("encode gRPC inventory event", "kind", kind, "err", err)
		return
	}
	o.mu.Lock()
	o.queue = append(o.queue, queuedEvent{kind: kind, payload: payload})
	o.mu.Unlock()
	select {
	case o.wakeup <- struct{}{}:
	default:
	}
}

func (o *EventObserver) run() {
	for {
		select {
		case <-o.ctx.Done():
			return
		case <-o.wakeup:
		}
		for {
			o.mu.Lock()
			if len(o.queue) == 0 {
				o.mu.Unlock()
				break
			}
			next := o.queue[0]
			o.queue[0] = queuedEvent{}
			o.queue = o.queue[1:]
			o.active = true
			o.mu.Unlock()
			if _, err := o.events.Append(o.ctx, next.kind, next.payload); err != nil {
				o.mu.Lock()
				o.active = false
				o.mu.Unlock()
				if o.ctx.Err() == nil {
					o.log.Error("persist gRPC inventory event", "kind", next.kind, "err", err)
				}
				return
			}
			o.mu.Lock()
			o.active = false
			o.mu.Unlock()
		}
	}
}

// Flush waits until every event offered before the call is durable.
func (o *EventObserver) Flush(ctx context.Context) error {
	if o == nil {
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		o.mu.Lock()
		idle := len(o.queue) == 0 && !o.active
		o.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

var _ host.Observer = (*EventObserver)(nil)
