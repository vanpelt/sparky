package schedule

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// jobTimeout bounds a single scheduled run end to end (resume + exec). A job
// that overruns is cancelled and recorded as an error rather than pinning the
// sandbox warm forever.
const jobTimeout = 10 * time.Minute

// Runner executes a command inside a sandbox, resuming it first if paused. The
// SSH gateway implements this (it owns the upstream key and the dial path); the
// scheduler depends only on this narrow interface so it needn't import sshgw.
type Runner interface {
	RunInSandbox(ctx context.Context, sandbox, cmd string) (exit int, output string, err error)
}

// Scheduler walks the store on a fixed interval and fires due jobs. It is the
// host-side timer that makes periodic work reliable without keeping the sandbox
// warm: the VM only wakes when a job is actually due.
type Scheduler struct {
	store  *Store
	runner Runner
	log    *slog.Logger

	mu       sync.Mutex
	inflight map[string]struct{} // entry IDs currently running, to avoid overlap
}

func NewScheduler(store *Store, runner Runner, log *slog.Logger) *Scheduler {
	return &Scheduler{store: store, runner: runner, log: log, inflight: map[string]struct{}{}}
}

// Run ticks every interval until ctx is done, firing any entry whose next
// activation has elapsed. Blocks; call in a goroutine.
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx, time.Now().UTC())
		}
	}
}

// tick fires every entry due at `now`. A job runs in its own goroutine so one
// slow resume doesn't hold up the others, and an in-flight guard prevents the
// next tick from launching an entry that's still running.
func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	entries, err := s.store.List()
	if err != nil {
		s.log.Error("scheduler list failed", "err", err)
		return
	}
	for _, e := range entries {
		if !s.due(e, now) {
			continue
		}
		if !s.claim(e.ID) {
			continue // already running from a previous tick
		}
		go s.fire(ctx, e, now)
	}
}

// due reports whether entry e should fire at `now`: the first activation after
// its last run (or after creation, if it has never run) has arrived. Comparing
// against last_run means a host that was down through several windows fires
// once on return, not a backlog storm — schedules are advisory, not a queue.
func (s *Scheduler) due(e Entry, now time.Time) bool {
	base := e.LastRun
	if base.IsZero() {
		base = e.CreatedAt
	}
	next, err := NextRun(e.Spec, base)
	if err != nil {
		s.log.Warn("skipping unparseable schedule", "id", e.ID, "spec", e.Spec, "err", err)
		return false
	}
	return !next.After(now)
}

// claim marks an entry in-flight, returning false if it already was.
func (s *Scheduler) claim(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, running := s.inflight[id]; running {
		return false
	}
	s.inflight[id] = struct{}{}
	return true
}

func (s *Scheduler) release(id string) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
}

// fire runs one job and records its outcome. last_run is stamped with the tick
// time regardless of success, so a persistently failing job retries on its
// schedule rather than every tick.
func (s *Scheduler) fire(ctx context.Context, e Entry, now time.Time) {
	defer s.release(e.ID)

	jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	s.log.Info("scheduled job starting", "id", e.ID, "sandbox", e.Sandbox, "spec", e.Spec)
	exit, output, err := s.runner.RunInSandbox(jobCtx, e.Sandbox, e.Command)

	errMsg := ""
	switch {
	case err != nil:
		errMsg = err.Error()
		s.log.Warn("scheduled job failed", "id", e.ID, "sandbox", e.Sandbox, "err", err)
	case exit != 0:
		// Keep a short tail of output so the failure is diagnosable from `ctl
		// schedule list` / the console without shipping unbounded logs.
		errMsg = "exit " + strconv.Itoa(exit) + ": " + tail(output, 200)
		s.log.Warn("scheduled job exited non-zero", "id", e.ID, "sandbox", e.Sandbox, "exit", exit)
	default:
		s.log.Info("scheduled job done", "id", e.ID, "sandbox", e.Sandbox)
	}
	if err := s.store.RecordRun(e.ID, now, exit, errMsg); err != nil {
		s.log.Error("scheduler record failed", "id", e.ID, "err", err)
	}
}

// tail returns the last n bytes of s, single-lined, for compact error display.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
