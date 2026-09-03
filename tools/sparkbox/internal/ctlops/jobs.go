package ctlops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/opidentity"
)

// ---------------------------------------------------------------------------
// Jobs
//
// Long operations over HTTP need somewhere to live that is not a held-open
// connection: 15 minutes is longer than every CDN and cloudflared idle budget on
// the path. Jobs are used ONLY by internal/restapi; the SSH path stays
// synchronous and byte-identical.
// ---------------------------------------------------------------------------

type JobState string

const (
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCanceled  JobState = "canceled"
)

// Ref names what a job acts on, so a client can correlate without parsing Op.
//
// Args is the discriminator for the de-duplicator, not part of the resource's
// identity, which is why it is excluded from JSON: the wire shape stays
// {type,name}. It carries whatever distinguishes two calls of the same
// operation on the same resource — the requested size for a resize, the new
// name for a rename, the source sandbox for a snapshot. Empty is correct for
// the argument-less operations (pause, resume, archive, rm, reboot), where a
// retry genuinely is the same work.
type Ref struct {
	Type string `json:"type"` // "sandbox" | "snapshot" | "environment"
	Name string `json:"name"`
	Args string `json:"-"`
}

// Job is the gateway-local view of asynchronous work. The registry remains
// in-memory, but a keyed REST request carries a stable node operation identity:
// after a gateway restart, retrying that request reattaches to the node's
// durable journal instead of executing the mutation twice.
type Job struct {
	ID         string          `json:"id"`
	Op         string          `json:"op"`
	Owner      string          `json:"-"`
	Resource   Ref             `json:"resource"`
	State      JobState        `json:"state"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *Error          `json:"error,omitempty"`

	// done closes exactly once, when the job leaves JobRunning. Await selects on
	// it; nothing else may close it.
	done chan struct{}
	// cancel tears down the job's own detached context, and canceled records
	// that a human asked for it — the distinction between "we gave up" and "the
	// work failed" is the whole reason JobCanceled exists.
	cancel   context.CancelFunc
	canceled bool
}

// JobRetain is how long a finished job stays readable; JobMaxPerOwner bounds the
// registry, evicting the oldest finished job first.
const (
	JobRetain      = time.Hour
	JobMaxPerOwner = 200
)

// jobCancelGrace is how long CancelJob waits for the work to actually stop
// before answering. Bounded because the caller is an HTTP handler: a driver that
// ignores its context must not turn a cancel into a hung request.
const jobCancelGrace = 2 * time.Second

// Go starts fn on a context detached from the caller's request — an HTTP client
// hanging up must not abort a 15-minute archive — bounded by budget. If an
// identical (owner, op, resource) job is already running it returns THAT job
// instead of starting a second: firing two archives of one sandbox is never what
// a retry meant.
//
// "Identical" includes Ref.Args, and it must. The collapse reports the FIRST
// job's result under an ordinary 2xx, so for an operation whose arguments live
// only in the closure — resize, rename, snapshot.create — matching on
// (owner, op, name) alone would answer a 100 GB resize with the 25 GB one's
// success and never run the second closure at all. Callers pass the arguments
// that distinguish the work; see Ref.Args.
func (o *Ops) Go(c Caller, op string, ref Ref, budget time.Duration,
	fn func(ctx context.Context) (any, error)) *Job {
	return o.GoFrom(context.Background(), c, op, ref, budget, fn)
}

// GoFrom is Go with a caller context used only to preserve a durable operation
// identity. Cancellation is deliberately detached exactly as in Go: closing an
// HTTP request must not abort a long-running node operation.
func (o *Ops) GoFrom(parent context.Context, c Caller, op string, ref Ref, budget time.Duration,
	fn func(ctx context.Context) (any, error)) *Job {
	o.jobsMu.Lock()
	for _, j := range o.jobs {
		if j.State == JobRunning && j.Owner == c.Handle && j.Op == op && j.Resource == ref {
			snap := j.snapshot()
			o.jobsMu.Unlock()
			return snap
		}
	}
	j := &Job{
		ID:        newJobID(),
		Op:        op,
		Owner:     c.Handle,
		Resource:  ref,
		State:     JobRunning,
		CreatedAt: o.now().UTC(),
		done:      make(chan struct{}),
	}
	base := context.Background()
	if identity, ok := opidentity.FromContext(parent); ok {
		base = opidentity.WithContext(base, identity)
	}
	ctx, cancel := context.WithTimeout(base, budget)
	j.cancel = cancel
	o.jobs[j.ID] = j
	o.evictLocked(c.Handle)
	o.jobsMu.Unlock()

	go func() {
		defer cancel()
		res, err := fn(ctx)
		o.finish(j, res, err)
	}()
	return o.snapshot(j.ID)
}

// finish records a completed job. It runs on the worker goroutine, so it takes
// the registry lock rather than assuming it.
func (o *Ops) finish(j *Job, res any, err error) {
	o.jobsMu.Lock()
	defer o.jobsMu.Unlock()
	now := o.now().UTC()
	j.FinishedAt = &now
	switch {
	case j.canceled:
		// A canceled job's error is the cancellation, which the client already
		// knows about — reporting it as a failure would be noise.
		j.State = JobCanceled
	case err != nil:
		j.State = JobFailed
		j.Error = AsError(j.Op, err)
	default:
		j.State = JobSucceeded
		if res != nil {
			if raw, merr := json.Marshal(res); merr == nil {
				j.Result = raw
			} else {
				j.State = JobFailed
				j.Error = AsError(j.Op, merr)
			}
		}
	}
	close(j.done)
	o.log.Info("job finished", "id", j.ID, "op", j.Op, "state", j.State)
}

// Await blocks until j finishes or d elapses, returning j's current snapshot.
// This is the whole sync-first mechanism: the REST edge Awaits for the Prefer
// window and answers 200 or 202 depending on what came back.
func (o *Ops) Await(ctx context.Context, j *Job, d time.Duration) *Job {
	if j == nil {
		return nil
	}
	live := o.live(j.ID)
	if live == nil {
		return j
	}
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-live.done:
		case <-t.C:
		case <-ctx.Done():
		}
	}
	if snap := o.snapshot(j.ID); snap != nil {
		return snap
	}
	// Reaped out from under us. The caller's copy is the last true thing we
	// know about it, which beats returning nil into a marshaller.
	return j
}

func (o *Ops) Job(c Caller, id string) (*Job, error) {
	o.jobsMu.Lock()
	defer o.jobsMu.Unlock()
	j, ok := o.jobs[id]
	// A foreign id is masked exactly like a foreign sandbox name: same error,
	// same status, so the registry is not enumerable.
	if !ok || j.Owner != c.Handle {
		return nil, NotFound("jobs.get", "job", id)
	}
	return j.snapshot(), nil
}

func (o *Ops) ListJobs(c Caller) ([]*Job, error) {
	o.jobsMu.Lock()
	defer o.jobsMu.Unlock()
	out := make([]*Job, 0, len(o.jobs))
	for _, j := range o.jobs {
		if j.Owner == c.Handle {
			out = append(out, j.snapshot())
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.After(out[k].CreatedAt) })
	return out, nil
}

// CancelJob asks a running job to stop and waits briefly for it to do so, so the
// common case answers with the settled state rather than making the client poll
// for a transition it already knows is coming.
func (o *Ops) CancelJob(c Caller, id string) (*Job, error) {
	o.jobsMu.Lock()
	j, ok := o.jobs[id]
	if !ok || j.Owner != c.Handle {
		o.jobsMu.Unlock()
		return nil, NotFound("jobs.cancel", "job", id)
	}
	if j.State != JobRunning {
		snap := j.snapshot()
		o.jobsMu.Unlock()
		return snap, &Error{
			Kind: KindConflict, Op: "jobs.cancel", Code: "job_finished",
			Msg: "that job has already finished", Verbatim: true,
		}
	}
	j.canceled = true
	cancel := j.cancel
	done := j.done
	// Taken while the lock is still held, and kept: the registry mutates while
	// we wait below, and a concurrent Go by this same owner can evict the job we
	// are cancelling the instant it finishes — it is now the oldest FINISHED one,
	// and a long-running job is the only kind worth cancelling. Await guards the
	// same race; without this the snapshot below returns nil into a handler that
	// dereferences it.
	pre := j.snapshot()
	o.jobsMu.Unlock()

	cancel()
	t := time.NewTimer(jobCancelGrace)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
	o.log.Info("job canceled", "id", id, "user", c.Handle)
	if snap := o.snapshot(id); snap != nil {
		return snap, nil
	}
	return pre, nil
}

// live returns the registry's own record, which owns the done channel. Callers
// hold snapshots, and a snapshot's channel field is not the live one.
func (o *Ops) live(id string) *Job {
	o.jobsMu.Lock()
	defer o.jobsMu.Unlock()
	return o.jobs[id]
}

func (o *Ops) snapshot(id string) *Job {
	o.jobsMu.Lock()
	defer o.jobsMu.Unlock()
	if j, ok := o.jobs[id]; ok {
		return j.snapshot()
	}
	return nil
}

// snapshot copies the fields a caller may read. The copy is what leaves the
// package: handing out the live pointer would let a REST handler marshal a
// struct the worker goroutine is concurrently writing.
func (j *Job) snapshot() *Job {
	c := *j
	c.done = nil
	c.cancel = nil
	return &c
}

// evictLocked bounds one owner's registry, dropping the oldest FINISHED job
// first. Running jobs are never evicted — they still describe live work, and a
// job the client can no longer poll is worse than a slightly-over-budget map.
func (o *Ops) evictLocked(owner string) {
	var mine []*Job
	for _, j := range o.jobs {
		if j.Owner == owner {
			mine = append(mine, j)
		}
	}
	if len(mine) <= JobMaxPerOwner {
		return
	}
	sort.Slice(mine, func(i, k int) bool { return mine[i].CreatedAt.Before(mine[k].CreatedAt) })
	drop := len(mine) - JobMaxPerOwner
	for _, j := range mine {
		if drop == 0 {
			return
		}
		if j.State == JobRunning {
			continue
		}
		delete(o.jobs, j.ID)
		drop--
	}
}

// reapJobs drops finished jobs once they are older than JobRetain. It is the
// only goroutine New starts, and Close is what stops it.
func (o *Ops) reapJobs() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-o.stop:
			return
		case <-t.C:
			o.reapOnce()
		}
	}
}

func (o *Ops) reapOnce() {
	cutoff := o.now().Add(-JobRetain)
	o.jobsMu.Lock()
	defer o.jobsMu.Unlock()
	for id, j := range o.jobs {
		if j.FinishedAt != nil && j.FinishedAt.Before(cutoff) {
			delete(o.jobs, id)
		}
	}
}

func newJobID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}
