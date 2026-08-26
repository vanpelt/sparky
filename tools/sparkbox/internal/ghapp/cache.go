package ghapp

// A TTL cache whose misses are single-flighted.
//
// Both halves are load-bearing and neither is an optimization. The caching half
// is what keeps a guest's `git fetch` loop from minting a token per fetch, on a
// call path that runs guest -> node -> gateway -> github under the guest's own
// 10s write timeout. The single-flight half is what keeps a sandbox booting
// five clones in parallel — or five sandboxes coming back from a pause at once
// — from becoming five identical mints, which is both a waste of github's rate
// limit and five tokens where one was wanted.
//
// x/sync/singleflight is already in the tree (internal/host uses it) and is not
// used here: it does not cache, and it does not let a waiter give up when its
// own caller's context ends. This needs both, and both together are forty
// lines.

import (
	"context"
	"sync"
	"time"
)

type cache[T any] struct {
	mu  sync.Mutex
	m   map[string]*entry[T]
	max int
}

// entry is either in flight (done is open) or settled (done is closed, and val
// is servable until good). Readers that find an in-flight entry wait on done
// rather than starting a second fill; the close is what publishes val and err
// to them.
type entry[T any] struct {
	done chan struct{}
	val  T
	err  error
	good time.Time
}

func newCache[T any](max int) *cache[T] {
	return &cache[T]{m: map[string]*entry[T]{}, max: max}
}

func (e *entry[T]) inFlight() bool {
	select {
	case <-e.done:
		return false
	default:
		return true
	}
}

// get serves key from the cache, calling fill exactly once across concurrent
// callers when there is nothing usable to serve. fill returns the value and the
// instant it stops being servable.
//
// A failed fill is never cached. Every failure this package can produce is
// either transient (github is down) or one an operator fixes in the next minute
// (the App is not installed yet, a permission is not granted yet), and both
// classes are made much worse by being remembered: somebody who installs the
// App and retries immediately must not be told for ten minutes that they did
// not.
//
// A waiter takes the leader's error, including the case where the leader's own
// context was cancelled. That is the standard single-flight trade and it is the
// right one here: the alternative is every waiter starting its own request the
// moment the leader gives up, which is exactly the stampede this exists to
// prevent. The failure is not cached, so the next call retries cleanly.
func (c *cache[T]) get(ctx context.Context, now func() time.Time, key string, fill func(context.Context) (T, time.Time, error)) (T, error) {
	c.mu.Lock()
	if e := c.m[key]; e != nil {
		if e.inFlight() {
			c.mu.Unlock()
			return e.wait(ctx)
		}
		if e.err == nil && now().Before(e.good) {
			val := e.val
			c.mu.Unlock()
			return val, nil
		}
		delete(c.m, key)
	}
	e := &entry[T]{done: make(chan struct{})}
	c.evictLocked(now())
	c.m[key] = e
	c.mu.Unlock()

	e.val, e.good, e.err = fill(ctx)
	close(e.done)

	c.mu.Lock()
	if e.err != nil && c.m[key] == e {
		delete(c.m, key)
	}
	c.mu.Unlock()
	return e.val, e.err
}

// wait blocks for the in-flight leader, or gives up when the caller's own
// context ends. A caller that walks away leaves the leader running: somebody
// else is probably still waiting for it, and it is about to populate the cache
// either way.
func (e *entry[T]) wait(ctx context.Context) (T, error) {
	select {
	case <-e.done:
		return e.val, e.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// evictLocked keeps the map under max. It drops what is already stale first,
// and only then drops live entries at random — an evicted token costs one extra
// mint, so the eviction policy is allowed to be crude; what it is not allowed
// to do is grow without bound, or evict an entry somebody is currently waiting
// on, which would strand the waiters' result outside the map and let the next
// caller start a duplicate fill.
func (c *cache[T]) evictLocked(now time.Time) {
	if len(c.m) < c.max {
		return
	}
	for k, e := range c.m {
		if !e.inFlight() && (e.err != nil || !now.Before(e.good)) {
			delete(c.m, k)
		}
	}
	for k, e := range c.m {
		if len(c.m) < c.max {
			return
		}
		if !e.inFlight() {
			delete(c.m, k)
		}
	}
}
