package restapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// jobBody is the wire shape of a job. ctlops.Job carries JSON tags of its own,
// but its Error field is a *ctlops.Error — which would marshal its Kind as an
// integer and its wrapped cause as an empty object — so the projection exists
// for the same reason apiError does: what leaves this package is a contract,
// and a contract cannot be a struct somebody else is free to reshape.
type jobBody struct {
	ID         string          `json:"id"`
	Op         string          `json:"op"`
	Resource   ctlops.Ref      `json:"resource"`
	State      string          `json:"state"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *apiError       `json:"error,omitempty"`
	Href       string          `json:"href"`
}

// jobBodyOf tolerates a nil job. ctlops never hands one back today, but this
// projection is the last thing between the registry's lifecycle races and a
// nil dereference in a handler goroutine, and an empty body is a far better
// failure than a dropped connection.
func jobBodyOf(j *ctlops.Job) jobBody {
	if j == nil {
		return jobBody{}
	}
	b := jobBody{
		ID: j.ID, Op: j.Op, Resource: j.Resource, State: string(j.State),
		CreatedAt: j.CreatedAt, FinishedAt: j.FinishedAt, Result: j.Result,
		Href: jobHref(j.ID),
	}
	if j.Error != nil {
		e := apiErrorOf(j.Error)
		b.Error = &e
	}
	return b
}

// jobList is an object rather than a bare array so the response has somewhere
// to grow a cursor without becoming a breaking change.
type jobList struct {
	Jobs []jobBody `json:"jobs"`
}

func (h *Handler) listJobs(w http.ResponseWriter, r *http.Request) {
	const op = "jobs.list"
	jobs, err := h.ops.ListJobs(caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	out := make([]jobBody, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobBodyOf(j))
	}
	writeJSON(w, http.StatusOK, jobList{Jobs: out})
}

func (h *Handler) getJob(w http.ResponseWriter, r *http.Request) {
	const op = "jobs.get"
	j, err := h.ops.Job(caller(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, jobBodyOf(j))
}

// cancelJob asks a running job to stop. ctlops waits briefly for the work to
// actually settle, so the usual answer already reads "canceled" rather than
// making the client poll for a transition it just requested. A job that has
// already finished is a 409: there is nothing to cancel, and pretending
// otherwise would let a client believe it had undone a completed archive.
func (h *Handler) cancelJob(w http.ResponseWriter, r *http.Request) {
	const op = "jobs.cancel"
	j, err := h.ops.CancelJob(caller(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, jobBodyOf(j))
}
