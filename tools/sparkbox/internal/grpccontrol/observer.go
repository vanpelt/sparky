package grpccontrol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/eventjournal"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"google.golang.org/protobuf/proto"
)

const (
	observerQueueLimit = 1024
	observerRetryMin   = 10 * time.Millisecond
	observerRetryMax   = time.Second
)

type eventAppender interface {
	Append(context.Context, string, []byte) (eventjournal.Event, error)
}

// EventObserver turns every node-local manager change into a durable,
// revisioned gRPC inventory event. Manager observers must never block while
// holding the lifecycle lock, so callbacks only append to an in-memory queue;
// one worker performs SQLite writes in order.
type EventObserver struct {
	ctx    context.Context
	events eventAppender
	log    *slog.Logger

	mu          sync.Mutex
	pending     map[string]queuedEvent
	order       []string
	active      *queuedEvent
	appendErr   error
	overflowErr error
	wakeup      chan struct{}
	done        chan struct{}
}

type queuedEvent struct {
	key     string
	kind    string
	payload []byte
}

func NewEventObserver(ctx context.Context, events eventAppender, log *slog.Logger) *EventObserver {
	if ctx == nil {
		ctx = context.Background()
	}
	if log == nil {
		log = slog.Default()
	}
	o := &EventObserver{
		ctx: ctx, events: events, log: log,
		pending: make(map[string]queuedEvent),
		wakeup:  make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	go o.run()
	return o
}

func (o *EventObserver) SandboxChanged(box *host.Sandbox, reason string) {
	if o == nil || o.events == nil || box == nil {
		return
	}
	o.offer(eventSandboxChanged+":"+box.Name, eventSandboxChanged, &nodev1.InventoryEvent{
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
	o.offer(eventSandboxGone+":"+name, eventSandboxGone, &nodev1.InventoryEvent{
		Event: &nodev1.InventoryEvent_SandboxGone{
			SandboxGone: &nodev1.SandboxGone{Name: name},
		},
	})
}

func (o *EventObserver) offer(key, kind string, event *nodev1.InventoryEvent) {
	payload, err := proto.Marshal(event)
	if err != nil {
		o.log.Error("encode gRPC inventory event", "kind", kind, "err", err)
		return
	}
	o.mu.Lock()
	if _, exists := o.pending[key]; !exists {
		if len(o.pending) >= observerQueueLimit {
			if o.overflowErr == nil {
				o.overflowErr = fmt.Errorf(
					"gRPC inventory observer queue exceeded %d distinct pending changes",
					observerQueueLimit,
				)
			}
			o.mu.Unlock()
			return
		}
		o.order = append(o.order, key)
	}
	o.pending[key] = queuedEvent{key: key, kind: kind, payload: payload}
	o.mu.Unlock()
	select {
	case o.wakeup <- struct{}{}:
	default:
	}
}

func (o *EventObserver) run() {
	defer close(o.done)
	retryDelay := observerRetryMin
	for {
		select {
		case <-o.ctx.Done():
			return
		case <-o.wakeup:
		}
		for {
			o.mu.Lock()
			if o.active == nil && len(o.order) > 0 {
				key := o.order[0]
				copy(o.order, o.order[1:])
				o.order[len(o.order)-1] = ""
				o.order = o.order[:len(o.order)-1]
				next := o.pending[key]
				delete(o.pending, key)
				o.active = &next
			}
			if o.active == nil {
				o.mu.Unlock()
				break
			}
			next := *o.active
			o.mu.Unlock()
			if _, err := o.events.Append(o.ctx, next.kind, next.payload); err != nil {
				o.mu.Lock()
				firstFailure := o.appendErr == nil
				o.appendErr = err
				o.mu.Unlock()
				if o.ctx.Err() != nil {
					return
				}
				if firstFailure {
					o.log.Warn("persist gRPC inventory event; retrying",
						"kind", next.kind, "retry_in", retryDelay, "err", err)
				}
				timer := time.NewTimer(retryDelay)
				select {
				case <-o.ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				if retryDelay < observerRetryMax {
					retryDelay *= 2
					if retryDelay > observerRetryMax {
						retryDelay = observerRetryMax
					}
				}
				continue
			}
			o.mu.Lock()
			o.active = nil
			o.appendErr = nil
			o.mu.Unlock()
			retryDelay = observerRetryMin
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
		idle := len(o.pending) == 0 && o.active == nil
		overflowErr := o.overflowErr
		o.mu.Unlock()
		if idle {
			return overflowErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Health reports whether event durability is currently impaired. A transient
// append error clears after the worker successfully retries the same event.
func (o *EventObserver) Health() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return errors.Join(o.overflowErr, o.appendErr)
}

// Acknowledge clears a bounded-queue overflow after the queue has drained and
// an authoritative inventory has been served. Keeping health degraded while
// old queued changes remain forces reconciliation to occur after those
// revisions, rather than allowing them to regress a just-served snapshot.
// Append failures are only cleared by a successful retry.
func (o *EventObserver) Acknowledge() {
	if o == nil {
		return
	}
	o.mu.Lock()
	if len(o.pending) == 0 && o.active == nil {
		o.overflowErr = nil
	}
	o.mu.Unlock()
}

// Wait waits for the observer worker to exit after its context is cancelled.
func (o *EventObserver) Wait(ctx context.Context) error {
	if o == nil {
		return nil
	}
	select {
	case <-o.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ host.Observer = (*EventObserver)(nil)
var _ eventHealthReporter = (*EventObserver)(nil)
