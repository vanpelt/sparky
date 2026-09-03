package ctlops

import (
	"context"
	"sort"
	"strconv"

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

// SetVisibility is the whole-sandbox flip, and it is deliberately ASYMMETRIC
// now that visibility is settled per port.
//
// `private` still means what it always did — every port of every route of this
// sandbox, the panic button that leaves nothing exposed. `public` no longer
// does: it opens the sandbox's DEFAULT port only, because the alternative is a
// command that publishes whatever happened to be listening at the time it ran.
// Naming a port (SetPortVisibility) is how anything else is opened.
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
	if visibility == routes.VisibilityPrivate {
		for _, r := range rs {
			n, err := o.routes.PrivatizeAll(r.Subdomain)
			if err != nil {
				return VisibilityResult{}, Fail(op, err)
			}
			changed += n
		}
	} else {
		def, err := defaultRoute(op, name, rs)
		if err != nil {
			return VisibilityResult{}, err
		}
		if err := o.routes.SetVisibility(def.Subdomain, visibility); err != nil {
			return VisibilityResult{}, Fail(op, err)
		}
		changed = 1
	}
	o.log.Info("route visibility changed",
		"user", c.Handle, "sandbox", name, "visibility", visibility, "ports", changed)
	// Re-read rather than patching the copies in hand: the store is the truth
	// about what a route now is, and a partially-applied flip should say so.
	infos, err := o.routeInfos(op, name)
	if err != nil {
		return VisibilityResult{}, err
	}
	return VisibilityResult{Routes: infos, Changed: changed}, nil
}

// SetPortVisibility opens or closes ONE port of a sandbox's default hostname —
// the ports the edge serves as https://<name>.<domain>:<port> with no route row
// of their own.
//
// Setting a port private is not the same as never having mentioned it: the
// store keeps the row, which is what holds the port on the console's strip so
// it can be pre-authorised before anything listens on it. ForgetPort is the
// way back to nothing.
func (o *Ops) SetPortVisibility(ctx context.Context, c Caller, name string, port int, visibility string) (VisibilityResult, error) {
	const op = "share.set"
	def, err := o.portTarget(op, c, name, port)
	if err != nil {
		return VisibilityResult{}, err
	}
	if !routes.ValidVisibility(visibility) {
		return VisibilityResult{}, Invalid(op, "bad_visibility",
			"visibility must be 'public' or 'private', not %q", visibility)
	}
	if err := o.routes.SetPortVisibility(def.Subdomain, port, visibility); err != nil {
		return VisibilityResult{}, Fail(op, err)
	}
	o.log.Info("port visibility changed",
		"user", c.Handle, "sandbox", name, "subdomain", def.Subdomain, "port", port, "visibility", visibility)
	infos, err := o.routeInfos(op, name)
	if err != nil {
		return VisibilityResult{}, err
	}
	return VisibilityResult{Routes: infos, Changed: 1}, nil
}

// ForgetPort drops a port the owner had said something about. It is not
// "make it private" — that leaves a private row behind, and the row is what
// keeps the port listed. This removes the listing; the port goes back to
// private-because-nobody-said, like every port that was never mentioned.
func (o *Ops) ForgetPort(ctx context.Context, c Caller, name string, port int) (VisibilityResult, error) {
	const op = "share.set"
	def, err := o.portTarget(op, c, name, port)
	if err != nil {
		return VisibilityResult{}, err
	}
	if port == def.Port {
		return VisibilityResult{}, Invalid(op, "default_port",
			"port %d is %s's default port — it always has a visibility. Set it private instead.", port, name)
	}
	if err := o.routes.ForgetPort(def.Subdomain, port); err != nil {
		return VisibilityResult{}, Fail(op, err)
	}
	o.log.Info("port forgotten", "user", c.Handle, "sandbox", name, "subdomain", def.Subdomain, "port", port)
	infos, err := o.routeInfos(op, name)
	if err != nil {
		return VisibilityResult{}, err
	}
	return VisibilityResult{Routes: infos, Changed: 1}, nil
}

// portTarget is the shared preamble of the two per-port writes: the proxy is
// enabled, the caller owns the sandbox, the port is a port, and the sandbox has
// a default route for the port to hang off.
func (o *Ops) portTarget(op string, c Caller, name string, port int) (routes.Route, error) {
	if o.routes == nil {
		return routes.Route{}, Disabled(op, proxyDisabled)
	}
	if _, err := o.owned(op, name, c); err != nil {
		return routes.Route{}, err
	}
	if port < 1 || port > 65535 {
		return routes.Route{}, Invalid(op, "bad_port", "port must be from 1 through 65535, not %d", port)
	}
	rs, err := o.routes.ListBySandbox(name)
	if err != nil {
		return routes.Route{}, Fail(op, err)
	}
	return defaultRoute(op, name, rs)
}

// defaultRoute picks the hostname a bare `share <name> …` and every per-port
// write act on: the route named after the sandbox, which is the one every
// sandbox is created with and the only one a guest or the console can address
// by port. A sandbox with a single differently-named route has an unambiguous
// answer too; anything else has to be told which.
func defaultRoute(op, name string, rs []routes.Route) (routes.Route, error) {
	if len(rs) == 0 {
		return routes.Route{}, Invalid(op, "no_routes", "%s has no web routes.", name)
	}
	for _, r := range rs {
		if r.Subdomain == name {
			return r, nil
		}
	}
	if len(rs) == 1 {
		return rs[0], nil
	}
	return routes.Route{}, Invalid(op, "ambiguous_default",
		"%s has no route named after it, and several others — nothing here can tell which is its default.", name)
}

// routeInfos flattens a sandbox into one entry per addressable PORT: each
// route's own port, followed by the extra ports configured under it. The
// sandbox's default hostname comes first — it is the URL people have, and the
// one the console pins to the head of its strip.
func (o *Ops) routeInfos(op, sandbox string) ([]RouteInfo, error) {
	rs, err := o.routes.ListBySandbox(sandbox)
	if err != nil {
		return nil, Fail(op, err)
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if (rs[i].Subdomain == sandbox) != (rs[j].Subdomain == sandbox) {
			return rs[i].Subdomain == sandbox
		}
		return rs[i].Subdomain < rs[j].Subdomain
	})
	extra, err := o.routes.ListPortsBySandbox(sandbox)
	if err != nil {
		return nil, Fail(op, err)
	}
	bySub := make(map[string][]routes.PortRoute, len(rs))
	for _, p := range extra {
		bySub[p.Subdomain] = append(bySub[p.Subdomain], p)
	}
	out := make([]RouteInfo, 0, len(rs)+len(extra))
	for _, r := range rs {
		out = append(out, o.routeInfo(r.Subdomain, r.Sandbox, r.Port, r.Visibility, true))
		for _, p := range bySub[r.Subdomain] {
			out = append(out, o.routeInfo(r.Subdomain, r.Sandbox, p.Port, p.Visibility, false))
		}
	}
	return out, nil
}

func (o *Ops) routeInfo(subdomain, sandbox string, port int, visibility string, isDefault bool) RouteInfo {
	ri := RouteInfo{
		Subdomain: subdomain, Sandbox: sandbox, Port: port,
		Visibility: visibility, Default: isDefault,
	}
	if o.domain != "" {
		ri.URL = "https://" + subdomain + "." + o.domain
		if !isDefault {
			// An any-port URL is the same hostname with the guest port on it;
			// there is no second DNS name and no second route row.
			ri.URL += ":" + strconv.Itoa(port)
		}
	}
	return ri
}
