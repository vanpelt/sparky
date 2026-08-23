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
