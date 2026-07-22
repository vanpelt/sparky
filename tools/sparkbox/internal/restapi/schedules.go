package restapi

import (
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

type scheduleList struct {
	Schedules []ctlops.ScheduleInfo `json:"schedules"`
}

type scheduleRequest struct {
	Sandbox string `json:"sandbox"`
	Spec    string `json:"spec"`    // a 5-field cron expression or @daily/@every 30m
	Command string `json:"command"` // run through the guest's shell
}

func (h *Handler) listSchedules(w http.ResponseWriter, r *http.Request) {
	const op = "schedule.list"
	entries, err := h.ops.ListSchedules(r.Context(), caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, scheduleList{Schedules: entries})
}

// addSchedule is synchronous: it writes one row and computes a next-run time.
// The scheduler itself resumes the sandbox when the entry fires, so adding one
// to a paused box is legal and deliberate.
func (h *Handler) addSchedule(w http.ResponseWriter, r *http.Request) {
	const op = "schedule.add"
	var req scheduleRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	e, err := h.ops.AddSchedule(r.Context(), caller(r), ctlops.ScheduleArgs{
		Sandbox: req.Sandbox, Spec: req.Spec, Command: req.Command,
	})
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (h *Handler) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	const op = "schedule.rm"
	id := r.PathValue("id")
	if err := h.ops.DeleteSchedule(r.Context(), caller(r), id); err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, deleted{Name: id, Deleted: true})
}
