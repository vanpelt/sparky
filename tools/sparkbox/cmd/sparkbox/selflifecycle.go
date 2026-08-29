package main

// The one adapter that turns "a machine on a tap" into "the person who owns it".

import (
	"context"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// selfLifecycleOps is the fleet's guest self-service lifecycle: the closure
// main.go installs on the fleet after Ops is built.
//
// It is THE place ctlops.Caller{Handle: box.Owner} is constructed for a guest,
// and that is written down rather than incidental. Everywhere else on this
// surface a Caller comes from an SSH key or a verified edge session; here it
// comes from a sandbox record the gateway resolved from its own placement
// ledger. The elevation is real — a compromised guest acts as its owner for
// these two verbs — and it is bounded by three things that are NOT here: the
// tag restrictions and the refusals live in ctlops.PlanSelfSnapshot, the rate
// limit lives in internal/metadata, and the record itself comes from
// fleet.selfServiceBox.
//
// It exposes exactly three self-scoped methods and renders nothing. There is
// deliberately no unbind and no snapshot rm on it: the guest surface is capture
// and re-point only, so the worst a compromised box can do to a tag it already
// carries is change what that tag boots — never take another tag's binding
// away, and never delete a template somebody else's sandboxes depend on.
type selfLifecycleOps struct{ ops *ctlops.Ops }

func (s selfLifecycleOps) Pause(ctx context.Context, box *host.Sandbox) error {
	_, err := s.ops.Pause(ctx, callerFor(box), box.Name)
	return err
}

func (s selfLifecycleOps) PlanSnapshot(ctx context.Context, box *host.Sandbox, tag, name string) (ctlops.SelfSnapshotPlan, error) {
	return s.ops.PlanSelfSnapshot(ctx, callerFor(box), box.Name, tag, name)
}

// Snapshot starts the capture as a JOB and returns.
//
// The job is not a handle for the guest — its ability to poll ends the moment
// the pause lands, two steps into a two-minute operation. It exists so the
// OWNER has somewhere to read the outcome from outside, and so a retried commit
// collapses onto the capture already running instead of starting a second one:
// Ref.Args carries the sandbox, which is what makes two different boxes
// capturing onto one tag two jobs while two attempts from one box are one.
func (s selfLifecycleOps) Snapshot(_ context.Context, box *host.Sandbox, a ctlops.SnapshotToTagArgs) error {
	c := callerFor(box)
	// The sandbox is the RECORD's, never the argument's. Nothing upstream can
	// set it to another box, but stating it here means nothing downstream has to
	// be trusted to have checked.
	a.Sandbox = box.Name
	s.ops.Go(c, "snapshot.create",
		ctlops.Ref{Type: "snapshot", Name: a.Name, Args: box.Name},
		ctlops.ArchiveTimeout,
		func(ctx context.Context) (any, error) { return s.ops.SnapshotToTag(ctx, c, a) })
	return nil
}

// callerFor is the elevation, in one line so it can be found and read. The
// handle comes off the manager's record — which on a fleet is stamped from the
// gateway's placement ledger, over whatever a node reported — and never from
// anything on the request.
func callerFor(box *host.Sandbox) ctlops.Caller { return ctlops.Caller{Handle: box.Owner} }
