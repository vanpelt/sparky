package restapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// The sync-first window.
//
// A pause is usually seconds and an archive is usually minutes, and the client
// wants the fast case to feel like an ordinary request. So every long operation
// starts as a job and then WAITS: if the work lands inside the window the
// caller gets the finished resource with an ordinary 2xx and never learns a job
// existed; if it does not, the caller gets 202 and a job to poll.
//
// defaultWait is chosen against the network, not the work — it must be
// comfortably below the shortest idle timeout on the path (cloudflared's is 90
// seconds, a CDN's can be 30) so the escalation is a deliberate 202 rather than
// a truncated response nobody can distinguish from a crash.
const (
	defaultWait = 10 * time.Second
	maxWait     = 60 * time.Second
)

// prefer is the subset of RFC 7240 this API understands: `respond-async` to
// skip the wait entirely, and `wait=<seconds>` to widen or narrow it.
type prefer struct {
	async    bool
	wait     time.Duration
	waitSeen bool
}

func parsePrefer(r *http.Request) prefer {
	p := prefer{wait: defaultWait}
	for _, field := range strings.Split(r.Header.Get("Prefer"), ",") {
		field = strings.TrimSpace(field)
		switch {
		case strings.EqualFold(field, "respond-async"):
			p.async = true
		case strings.HasPrefix(strings.ToLower(field), "wait="):
			// An unparseable wait is ignored rather than refused: Prefer is
			// advisory by definition, and failing a pause because a proxy
			// appended something is a worse outcome than waiting 10 seconds.
			if n, err := strconv.Atoi(strings.TrimSpace(field[len("wait="):])); err == nil && n >= 0 {
				d := time.Duration(n) * time.Second
				if d > maxWait {
					d = maxWait
				}
				p.wait, p.waitSeen = d, true
			}
		}
	}
	if p.async {
		p.wait = 0
	}
	return p
}

// runJob is how every operation with a budget longer than a request answers.
// It starts the work on a context detached from this request — a client that
// hangs up mid-archive must not abort the archive — waits out the Prefer
// window, and then renders whichever of the four outcomes happened.
//
// okStatus is what a completed operation returns (200, or 201 for a create).
// A job that is still running answers 202 with a Location pointing at
// /v1/jobs/{id}; because ctlops.Go de-duplicates identical in-flight work, a
// client that retries the same call gets the SAME job back rather than a second
// archive of the same sandbox.
func (h *Handler) runJob(w http.ResponseWriter, r *http.Request, op string, ref ctlops.Ref,
	budget time.Duration, okStatus int, fn func(ctx context.Context) (any, error)) {

	c := caller(r)
	p := parsePrefer(r)

	job := h.ops.Go(c, op, ref, budget, fn)
	job = h.ops.Await(r.Context(), job, p.wait)

	switch job.State {
	case ctlops.JobSucceeded:
		if p.waitSeen || p.async {
			w.Header().Set("Preference-Applied", preferenceApplied(p))
		}
		writeRaw(w, okStatus, job.Result)
	case ctlops.JobFailed:
		h.fail(w, r, op, job.Error)
	case ctlops.JobCanceled:
		// Somebody canceled this job between starting it and now — possible
		// only if the client raced its own DELETE /v1/jobs/{id}. Reporting the
		// state is more useful than inventing a failure for it.
		h.accepted(w, r, job, p)
	default:
		h.accepted(w, r, job, p)
	}
}

// accepted answers 202 with the job resource. Retry-After is deliberately
// short: the jobs endpoint is a map lookup, and the alternative is clients
// inventing their own poll intervals.
func (h *Handler) accepted(w http.ResponseWriter, r *http.Request, job *ctlops.Job, p prefer) {
	w.Header().Set("Location", jobHref(job.ID))
	w.Header().Set("Retry-After", "2")
	if p.async || p.waitSeen {
		w.Header().Set("Preference-Applied", preferenceApplied(p))
	}
	writeJSON(w, http.StatusAccepted, jobBodyOf(job))
}

func preferenceApplied(p prefer) string {
	if p.async {
		return "respond-async"
	}
	return "wait=" + strconv.Itoa(int(p.wait/time.Second))
}

func jobHref(id string) string { return "/v1/jobs/" + id }
