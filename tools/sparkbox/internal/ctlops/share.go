package ctlops

import (
	"context"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

// proxyDisabled is what ctl@ says when there is no route store: the HTTP proxy
// is the thing that owns visibility, so its absence is the honest explanation.
const proxyDisabled = "the HTTP proxy isn't enabled on this host."

func (o *Ops) Visibility(ctx context.Context, c Caller, name string) ([]RouteInfo, error) {
	const op = "share.get"
	if o.routes == nil {
		return nil, Disabled(op, proxyDisabled)
	}
	if _, err := o.owned(op, name, c); err != nil {
		return nil, err
	}
	return o.routeInfos(op, name)
}

// SetVisibility flips EVERY route of a sandbox together — the per-sandbox
// granularity `ctl share` has always had, matching the "who can reach this VM"
// mental model. The user console's per-route endpoint is a different operation
// and stays where it is.
func (o *Ops) SetVisibility(ctx context.Context, c Caller, name, visibility string) (VisibilityResult, error) {
	const op = "share.set"
	if o.routes == nil {
		return VisibilityResult{}, Disabled(op, proxyDisabled)
	}
	if _, err := o.owned(op, name, c); err != nil {
		return VisibilityResult{}, err
	}
	if !routes.ValidVisibility(visibility) {
		return VisibilityResult{}, Invalid(op, "bad_visibility",
			"visibility must be 'public' or 'private', not %q", visibility)
	}
	rs, err := o.routes.ListBySandbox(name)
	if err != nil {
		return VisibilityResult{}, Fail(op, err)
	}
	changed := 0
	for _, r := range rs {
		if err := o.routes.SetVisibility(r.Subdomain, visibility); err != nil {
			return VisibilityResult{}, Fail(op, err)
		}
		changed++
	}
	o.log.Info("route visibility changed",
		"user", c.Handle, "sandbox", name, "visibility", visibility, "routes", changed)
	// Re-read rather than patching the copies in hand: the store is the truth
	// about what a route now is, and a partially-applied flip should say so.
	infos, err := o.routeInfos(op, name)
	if err != nil {
		return VisibilityResult{}, err
	}
	return VisibilityResult{Routes: infos, Changed: changed}, nil
}

func (o *Ops) routeInfos(op, sandbox string) ([]RouteInfo, error) {
	rs, err := o.routes.ListBySandbox(sandbox)
	if err != nil {
		return nil, Fail(op, err)
	}
	out := make([]RouteInfo, 0, len(rs))
	for _, r := range rs {
		ri := RouteInfo{
			Subdomain: r.Subdomain, Sandbox: r.Sandbox, Port: r.Port, Visibility: r.Visibility,
		}
		if o.domain != "" {
			ri.URL = "https://" + r.Subdomain + "." + o.domain
		}
		out = append(out, ri)
	}
	return out, nil
}
