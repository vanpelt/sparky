package fleet

// Guest self-service operations that cross from a node to gateway-owned route
// state. The node identifies the sandbox from the source address on its tap;
// this layer independently checks the gateway placement ledger before writing.

import (
	"context"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

func (f *Fleet) SelfVisibility(_ context.Context, node string, req nodelink.SelfVisibilityReq) (nodelink.SelfVisibilityResp, error) {
	box, err := f.selfServiceBox(node, req.Sandbox)
	if err != nil {
		return nodelink.SelfVisibilityResp{}, err
	}
	if f.sides.routes == nil {
		return nodelink.SelfVisibilityResp{}, ctlops.Disabled(nodelink.OpLink, "route visibility is not enabled on this gateway")
	}
	if !routes.ValidVisibility(req.Visibility) {
		return nodelink.SelfVisibilityResp{}, ctlops.Invalid(nodelink.OpLink, "bad_visibility", "visibility must be public or private")
	}
	rows, err := f.sides.routes.ListBySandbox(box.Name)
	if err != nil {
		return nodelink.SelfVisibilityResp{}, ctlops.Fail(nodelink.OpLink, err)
	}
	if len(rows) == 0 {
		return nodelink.SelfVisibilityResp{}, ctlops.Fail(nodelink.OpLink, routes.ErrNoSuchRoute)
	}
	for _, row := range rows {
		if row.Owner != box.Owner {
			return nodelink.SelfVisibilityResp{}, ctlops.Denied(nodelink.OpLink, "route_owner_mismatch", "a route for this sandbox belongs to another owner")
		}
		if err := f.sides.routes.SetVisibility(row.Subdomain, req.Visibility); err != nil {
			return nodelink.SelfVisibilityResp{}, ctlops.Fail(nodelink.OpLink, err)
		}
	}
	f.log.Info("sandbox changed its own route visibility", "sandbox", box.Name, "owner", box.Owner, "visibility", req.Visibility, "routes", len(rows))
	return nodelink.SelfVisibilityResp{Sandbox: box.Name, Visibility: req.Visibility, Routes: len(rows)}, nil
}

func (f *Fleet) SelfPort(_ context.Context, node string, req nodelink.SelfPortReq) (nodelink.SelfPortResp, error) {
	box, err := f.selfServiceBox(node, req.Sandbox)
	if err != nil {
		return nodelink.SelfPortResp{}, err
	}
	if f.sides.routes == nil {
		return nodelink.SelfPortResp{}, ctlops.Disabled(nodelink.OpLink, "default route configuration is not enabled on this gateway")
	}
	if req.Port < 1 || req.Port > 65535 {
		return nodelink.SelfPortResp{}, ctlops.Invalid(nodelink.OpLink, "bad_port", "port must be from 1 through 65535")
	}
	row, ok, err := f.sides.routes.GetBySubdomain(box.Name)
	if err != nil {
		return nodelink.SelfPortResp{}, ctlops.Fail(nodelink.OpLink, err)
	}
	if !ok || row.Sandbox != box.Name || row.Owner != box.Owner {
		return nodelink.SelfPortResp{}, ctlops.Denied(nodelink.OpLink, "default_route_mismatch", "this sandbox has no owned default route")
	}
	row.Port = req.Port
	if err := f.sides.routes.Upsert(row); err != nil {
		return nodelink.SelfPortResp{}, ctlops.Fail(nodelink.OpLink, err)
	}
	f.log.Info("sandbox changed its own default port", "sandbox", box.Name, "owner", box.Owner, "port", req.Port)
	return nodelink.SelfPortResp{Sandbox: box.Name, Port: req.Port}, nil
}

// SelfLifecycle is the control-plane half of the two verbs a guest can run
// against its own VM. cmd/sparkbox installs one after Ops is constructed (see
// SetSelfLifecycle); nil is a deployment with no control plane on its fleet.
//
// Every method takes the sandbox RECORD this package resolved from its own
// placement ledger, never a name from the request. That is what makes the
// elevation safe: the implementation reads the owner off the record, so a node
// relaying a guest's request never chooses whose authority the work runs under.
type SelfLifecycle interface {
	Pause(ctx context.Context, box *host.Sandbox) error
	PlanSnapshot(ctx context.Context, box *host.Sandbox, tag, name string) (ctlops.SelfSnapshotPlan, error)
	Snapshot(ctx context.Context, box *host.Sandbox, a ctlops.SnapshotToTagArgs) error
}

// SetSelfLifecycle installs it. A setter rather than an Options field for the
// reason SetIdentity and SetRepos are setters: construction order. Ops is built
// with this fleet as its sandbox store, so the fleet exists first and the thing
// it delegates to cannot be handed to New.
func (f *Fleet) SetSelfLifecycle(s SelfLifecycle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selfLife = s
}

func (f *Fleet) selfLifecycle() SelfLifecycle {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.selfLife
}

// SelfPause is a guest pausing itself, from a node or from this gateway's own
// metadata service — both take this path, so there is one authorization story
// rather than two that can drift.
//
// It returns as soon as the pause is ACCEPTED, not when the VM has stopped. The
// guest has already been sent its acceptance and read it; blocking here would
// hold a node's uplink request open across a full memory snapshot for a reply
// nobody is waiting on.
func (f *Fleet) SelfPause(ctx context.Context, node string, req nodelink.SelfPauseReq) (nodelink.SelfPauseResp, error) {
	box, err := f.selfServiceBox(node, req.Sandbox)
	if err != nil {
		return nodelink.SelfPauseResp{}, err
	}
	life := f.selfLifecycle()
	if life == nil {
		return nodelink.SelfPauseResp{}, ctlops.Disabled(nodelink.OpLink,
			"guest self-service lifecycle is not enabled on this gateway")
	}
	if err := life.Pause(ctx, box); err != nil {
		return nodelink.SelfPauseResp{}, err
	}
	f.log.Info("sandbox paused itself", "sandbox", box.Name, "owner", box.Owner, "node", node)
	return nodelink.SelfPauseResp{Sandbox: box.Name}, nil
}

// SelfSnapshotPlan answers what a capture from inside this sandbox would do.
// A pure read: it is where every refusal lands, while the guest's VM is still
// running and its session is still open.
func (f *Fleet) SelfSnapshotPlan(ctx context.Context, node string, req nodelink.SelfSnapshotPlanReq) (nodelink.SelfSnapshotPlanResp, error) {
	box, err := f.selfServiceBox(node, req.Sandbox)
	if err != nil {
		return nodelink.SelfSnapshotPlanResp{}, err
	}
	life := f.selfLifecycle()
	if life == nil {
		return nodelink.SelfSnapshotPlanResp{}, ctlops.Disabled(nodelink.OpLink,
			"guest self-service lifecycle is not enabled on this gateway")
	}
	plan, err := life.PlanSnapshot(ctx, box, req.Tag, req.Name)
	if err != nil {
		return nodelink.SelfSnapshotPlanResp{}, err
	}
	return nodelink.SelfSnapshotPlanFrom(plan), nil
}

// SelfSnapshot commits a capture the guest was already shown a plan for.
//
// Tag and Name come from that plan, and the plan's own token is checked by the
// metadata service that answered the guest — which on a node is the NODE. So
// they are re-derived here before anything is captured, and a pair the control
// plane's own plan does not produce is refused.
//
// That re-plan is the only thing standing between an enrolled node and the
// tags of the owners whose sandboxes it holds. The cap on this whole door is
// ctlops.PlanSelfSnapshot's rule that a guest may re-point only a tag its own
// sandbox already carries — and that rule is enforced where the plan is made.
// Without this call a node could hand up any tag of any owner it runs a box
// for, and the gateway would capture that box and re-point that tag: not a new
// image (the node already authors those bytes) but a new REACH, since the tag
// then hands its secrets to every sandbox created on it afterwards, wherever
// they land. It costs one read on a path that is about to spend minutes.
func (f *Fleet) SelfSnapshot(ctx context.Context, node string, req nodelink.SelfSnapshotReq) (nodelink.SelfSnapshotResp, error) {
	box, err := f.selfServiceBox(node, req.Sandbox)
	if err != nil {
		return nodelink.SelfSnapshotResp{}, err
	}
	life := f.selfLifecycle()
	if life == nil {
		return nodelink.SelfSnapshotResp{}, ctlops.Disabled(nodelink.OpLink,
			"guest self-service lifecycle is not enabled on this gateway")
	}
	plan, err := life.PlanSnapshot(ctx, box, req.Tag, req.Name)
	if err != nil {
		return nodelink.SelfSnapshotResp{}, err
	}
	if plan.Tag != req.Tag || plan.Snapshot != req.Name {
		f.log.Warn("refused a capture whose tag or name this gateway's own plan does not produce",
			"node", node, "sandbox", box.Name, "owner", box.Owner,
			"asked_tag", req.Tag, "planned_tag", plan.Tag,
			"asked_name", req.Name, "planned_name", plan.Snapshot)
		// plan_stale rather than a new code: from every honest caller's point of
		// view that is exactly what happened — the plan it holds is not the plan
		// this gateway makes now. A dishonest one is not owed a more precise
		// sentence, and it never reaches a guest either way, because the guest
		// was handed its acceptance before this ran.
		return nodelink.SelfSnapshotResp{}, &ctlops.Error{
			Kind: ctlops.KindConflict, Op: nodelink.OpLink, Code: "plan_stale", Verbatim: true,
			Msg: "the capture this gateway would plan for that sandbox is not the one it was asked for. " +
				"Nothing was captured.",
		}
	}
	if err := life.Snapshot(ctx, box, ctlops.SnapshotToTagArgs{
		Sandbox: box.Name, Name: req.Name, Tag: req.Tag,
	}); err != nil {
		return nodelink.SelfSnapshotResp{}, err
	}
	f.log.Info("sandbox asked to be captured as a template",
		"sandbox", box.Name, "owner", box.Owner, "node", node, "snapshot", req.Name, "tag", req.Tag)
	return nodelink.SelfSnapshotResp{Sandbox: box.Name, Snapshot: req.Name, Tag: req.Tag}, nil
}

func (f *Fleet) selfServiceBox(node, name string) (*host.Sandbox, error) {
	box, ok := f.Get(name)
	if !ok || box.Node != node || box.Owner == "" {
		f.log.Warn("refused guest self-service for a sandbox the machine does not hold",
			"node", node, "sandbox", name, "found", ok, "placed_on", placedOn(box, ok))
		return nil, &ctlops.Error{
			Kind: ctlops.KindDenied, Op: nodelink.OpLink, Code: nodelink.CodeNotYours, Verbatim: true,
			Msg: "this gateway does not place that sandbox on the requesting node.",
		}
	}
	return box, nil
}
