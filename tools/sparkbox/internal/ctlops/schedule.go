package ctlops

import (
	"context"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
)

// schedulingDisabled is the sentence ctl@ has printed since the scheduler
// shipped. Kept whole, and in one place, so the SSH and REST answers to "why
// did nothing happen" are the same answer.
const schedulingDisabled = "platform scheduling isn't enabled on this host."

func (o *Ops) ListSchedules(ctx context.Context, c Caller) ([]ScheduleInfo, error) {
	const op = "schedule.list"
	if o.schedules == nil {
		return nil, Disabled(op, schedulingDisabled)
	}
	entries, err := o.schedules.ListByOwner(c.Handle)
	if err != nil {
		return nil, Fail(op, err)
	}
	now := o.now()
	out := make([]ScheduleInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, o.scheduleInfo(e, now))
	}
	return out, nil
}

func (o *Ops) AddSchedule(ctx context.Context, c Caller, a ScheduleArgs) (ScheduleInfo, error) {
	const op = "schedule.add"
	if o.schedules == nil {
		return ScheduleInfo{}, Disabled(op, schedulingDisabled)
	}
	if _, err := o.owned(op, a.Sandbox, c); err != nil {
		return ScheduleInfo{}, err
	}
	// Validating the spec here rather than letting the store do it is what makes
	// a typo'd cron expression a 400 instead of a 500.
	if _, err := schedule.Parse(a.Spec); err != nil {
		return ScheduleInfo{}, Invalid(op, "bad_cron", "invalid schedule %q: %v", a.Spec, err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return ScheduleInfo{}, Invalid(op, "bad_command", "a schedule needs a command to run")
	}
	e, err := o.schedules.Add(schedule.Entry{
		Sandbox: a.Sandbox, Owner: c.Handle, Spec: a.Spec, Command: a.Command,
	})
	if err != nil {
		return ScheduleInfo{}, Fail(op, err)
	}
	o.log.Info("schedule added", "user", c.Handle, "sandbox", a.Sandbox, "id", e.ID, "spec", a.Spec)
	return o.scheduleInfo(e, o.now()), nil
}

// DeleteSchedule masks a foreign id exactly like a foreign sandbox name: a
// fetch failure and a wrong owner produce the same sentence, so schedule ids
// stay unenumerable.
func (o *Ops) DeleteSchedule(ctx context.Context, c Caller, id string) error {
	const op = "schedule.rm"
	if o.schedules == nil {
		return Disabled(op, schedulingDisabled)
	}
	e, err := o.schedules.Get(id)
	if err != nil || e.Owner != c.Handle {
		return NotFound(op, "schedule", id)
	}
	if err := o.schedules.Delete(id); err != nil {
		return Fail(op, err)
	}
	o.log.Info("schedule removed", "user", c.Handle, "id", id)
	return nil
}

func (o *Ops) scheduleInfo(e schedule.Entry, now time.Time) ScheduleInfo {
	si := ScheduleInfo{
		ID: e.ID, Sandbox: e.Sandbox, Spec: e.Spec, Command: e.Command,
		LastExit: e.LastExit, LastError: e.LastError,
	}
	// A spec that no longer parses (a cron library upgrade, a hand-edited row)
	// leaves NextRun nil rather than reporting a zero time as "1 Jan year 1".
	if next, err := schedule.NextRun(e.Spec, now); err == nil {
		si.NextRun = &next
	}
	if !e.LastRun.IsZero() {
		last := e.LastRun
		si.LastRun = &last
	}
	return si
}
