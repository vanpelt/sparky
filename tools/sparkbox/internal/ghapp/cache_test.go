package ghapp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func at(t time.Time) func() time.Time { return func() time.Time { return t } }

// One fill for however many callers arrive while it is running. This is the
// property that keeps five parallel clones from becoming five mints.
func TestCacheSingleFlightsMisses(t *testing.T) {
	c := newCache[int](maxCacheEntries)
	var fills atomic.Int64
	entered := make(chan struct{}, 1)
	release := make(chan struct{})

	fill := func(context.Context) (int, time.Time, error) {
		fills.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return 7, fixedNow.Add(time.Hour), nil
	}

	var wg sync.WaitGroup
	run := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.get(context.Background(), at(fixedNow), "k", fill)
			if err != nil || got != 7 {
				t.Errorf("get = (%v, %v), want (7, nil)", got, err)
			}
		}()
	}
	run()
	<-entered
	for i := 0; i < 20; i++ {
		run()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := fills.Load(); n != 1 {
		t.Errorf("filled %d times for 21 callers, want 1", n)
	}
}

// Every failure this package produces is either transient or one an operator
// fixes in the next minute, and both are made much worse by being remembered.
func TestCacheDoesNotRememberFailures(t *testing.T) {
	c := newCache[int](maxCacheEntries)
	calls := 0
	boom := errors.New("boom")
	fill := func(context.Context) (int, time.Time, error) {
		calls++
		if calls < 3 {
			return 0, time.Time{}, boom
		}
		return 9, fixedNow.Add(time.Hour), nil
	}
	for i := 0; i < 2; i++ {
		if _, err := c.get(context.Background(), at(fixedNow), "k", fill); !errors.Is(err, boom) {
			t.Fatalf("call %d: err = %v", i, err)
		}
	}
	if got, err := c.get(context.Background(), at(fixedNow), "k", fill); err != nil || got != 9 {
		t.Fatalf("get = (%v, %v), want the value once the fill succeeds", got, err)
	}
	if calls != 3 {
		t.Errorf("filled %d times, want 3", calls)
	}
}

func TestCacheServesUntilItsDeadline(t *testing.T) {
	c := newCache[int](maxCacheEntries)
	good := fixedNow.Add(10 * time.Minute)
	calls := 0
	fill := func(context.Context) (int, time.Time, error) {
		calls++
		return calls, good, nil
	}
	if got, _ := c.get(context.Background(), at(fixedNow), "k", fill); got != 1 {
		t.Fatalf("first get = %d", got)
	}
	if got, _ := c.get(context.Background(), at(good.Add(-time.Second)), "k", fill); got != 1 {
		t.Errorf("a value one second inside its window was refetched")
	}
	if got, _ := c.get(context.Background(), at(good), "k", fill); got != 2 {
		t.Errorf("a value at its deadline was still served: got %d", got)
	}
}

// The cap exists so that a caller looping over generated names cannot grow the
// map without limit. It is allowed to be crude about what it drops.
func TestCacheIsBounded(t *testing.T) {
	c := newCache[int](16)
	for i := 0; i < 200; i++ {
		if _, err := c.get(context.Background(), at(fixedNow), fmt.Sprintf("k%d", i), func(context.Context) (int, time.Time, error) {
			return i, fixedNow.Add(time.Hour), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n > 16 {
		t.Errorf("cache holds %d entries, want at most 16", n)
	}
}

// A waiter that gives up leaves the leader running — somebody else is probably
// still waiting on it — but does not wait past its own caller's deadline.
func TestCacheWaiterHonoursItsOwnContext(t *testing.T) {
	c := newCache[int](maxCacheEntries)
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	go func() {
		//nolint:errcheck // the leader's answer is not what this test is about
		c.get(context.Background(), at(fixedNow), "k", func(context.Context) (int, time.Time, error) {
			close(entered)
			<-release
			return 1, fixedNow.Add(time.Hour), nil
		})
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.get(ctx, at(fixedNow), "k", func(context.Context) (int, time.Time, error) {
			t.Error("a waiter started its own fill")
			return 0, time.Time{}, nil
		})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a waiter with a dead context blocked on the leader")
	}
}
