package frontdoor

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
)

// Plumber applies the Linux host networking that makes front doors reachable:
//
//   - one AnyIP local route for the whole range, so the kernel accepts
//     connections to any front-door address on the gateway's existing
//     wildcard listeners (SSH :2222, proxy :443) without per-address binds;
//   - a proxy-NDP entry per address on the uplink, because the provider
//     delivers the routed /64 on-link and the host must answer neighbor
//     discovery for addresses that live on no real interface (the same
//     dance fc.go does for guest addresses).
//
// Everything is best-effort and idempotent: on a dev machine (no `ip`, no
// v6 uplink) calls log a warning and the service runs fine on username
// routing alone. Plumber implements host.FrontDoor.
type Plumber struct {
	mapper *Mapper
	uplink string // iface backing the v6 default route; "" = no NDP proxying
	log    *slog.Logger
}

func NewPlumber(m *Mapper, log *slog.Logger) *Plumber {
	return &Plumber{mapper: m, uplink: defaultRoute6Dev(), log: log}
}

// EnsureRange claims the front-door range on loopback (AnyIP). Call once at
// startup, before Ensure'ing individual names.
func (p *Plumber) EnsureRange(ctx context.Context) {
	p.run(ctx, "ip", "-6", "route", "replace", "local", p.mapper.Range(), "dev", "lo")
	if p.uplink != "" {
		p.run(ctx, "sysctl", "-qw", "net.ipv6.conf."+p.uplink+".proxy_ndp=1")
	}
}

// Ensure makes name's front door answer NDP on the uplink. Idempotent;
// call on create and for every existing sandbox at startup.
func (p *Plumber) Ensure(ctx context.Context, name string) {
	if p.uplink == "" {
		return
	}
	addr := p.mapper.Addr(name).String()
	// del-then-add keeps it idempotent (same pattern as the fc driver).
	exec.CommandContext(ctx, "ip", "-6", "neigh", "del", "proxy", addr, "dev", p.uplink).Run() //nolint:errcheck
	p.run(ctx, "ip", "-6", "neigh", "add", "proxy", addr, "dev", p.uplink)
}

// Remove drops name's NDP proxy entry (sandbox destroyed).
func (p *Plumber) Remove(ctx context.Context, name string) {
	if p.uplink == "" {
		return
	}
	exec.CommandContext(ctx, "ip", "-6", "neigh", "del", "proxy", p.mapper.Addr(name).String(), "dev", p.uplink).Run() //nolint:errcheck
}

func (p *Plumber) run(ctx context.Context, cmd ...string) {
	if out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
		p.log.Warn("frontdoor plumbing failed", "cmd", strings.Join(cmd, " "), "err", err, "out", string(out))
	}
}

// defaultRoute6Dev returns the interface backing the IPv6 default route, or
// "" when there is none (mirrors the firecracker driver's uplink detection).
func defaultRoute6Dev() string {
	out, err := exec.Command("ip", "-6", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
