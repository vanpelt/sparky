package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/vanpelt/sparky/tools/sluice/internal/allowlist"
	"github.com/vanpelt/sparky/tools/sluice/internal/control"
	"github.com/vanpelt/sparky/tools/sluice/internal/dnsproxy"
	"github.com/vanpelt/sparky/tools/sluice/internal/ipmap"
	"github.com/vanpelt/sparky/tools/sluice/internal/meter"
	"github.com/vanpelt/sparky/tools/sluice/internal/policy"
	"github.com/vanpelt/sparky/tools/sluice/internal/report"
)

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return fs
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*m = append(*m, p)
		}
	}
	return nil
}

type runOpts struct {
	allowlist      string
	dnsListen      string
	upstreams      multiFlag
	tapPrefix      string
	staticAllowIPs multiFlag
	enforce        bool
	openUntagged   bool
	denyMode       string
	logFormat      string
	minTTL         time.Duration
	syncInterval   time.Duration
	reportInterval time.Duration
	apiSocket      string
}

func runCmd(args []string) int {
	var o runOpts
	fs := newFlagSet("run")
	fs.StringVar(&o.allowlist, "allowlist", "", "path to the allowlist file (required)")
	fs.StringVar(&o.dnsListen, "dns-listen", ":53", "address for the DNS resolver to bind")
	fs.Var(&o.upstreams, "upstream", "upstream resolver host:port (repeatable; default 1.1.1.1:53,8.8.8.8:53)")
	fs.StringVar(&o.tapPrefix, "tap-prefix", "sbtap", "attach the meter to interfaces with this name prefix")
	fs.Var(&o.staticAllowIPs, "allow-ip", "always-reachable IP, bypassing DNS (repeatable)")
	fs.BoolVar(&o.enforce, "enforce", false, "drop guest egress to addresses the allowlist never resolved")
	fs.BoolVar(&o.openUntagged, "open-untagged", false, "only enforce taps that have a per-tap policy pushed over the control socket; taps with none keep unrestricted egress (an untagged sandbox is unlimited). Without this every tap is enforced against the base allowlist")
	fs.StringVar(&o.denyMode, "deny", "nxdomain", "reply for blocked names: nxdomain|refused")
	fs.StringVar(&o.logFormat, "log", "json", "log format: json|text")
	fs.DurationVar(&o.minTTL, "min-ttl", ipmap.DefaultMinTTL, "floor for how long a resolved IP stays reachable")
	fs.DurationVar(&o.syncInterval, "sync-interval", 5*time.Second, "tap-discovery and allow-set sync period")
	fs.DurationVar(&o.reportInterval, "report-interval", 30*time.Second, "per-domain bandwidth report period (0 disables)")
	fs.StringVar(&o.apiSocket, "api-listen", "", "path to a Unix control socket serving per-VM bandwidth + accepting per-tap policy (empty disables)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.allowlist == "" {
		fmt.Fprintln(os.Stderr, "sluice run: --allowlist is required")
		return 2
	}
	if len(o.upstreams) == 0 {
		o.upstreams = multiFlag{"1.1.1.1:53", "8.8.8.8:53"}
	}

	log := newLogger(o.logFormat)

	list, err := allowlist.Load(o.allowlist)
	if err != nil {
		log.Error("load allowlist", "err", err)
		return 1
	}
	log.Info("allowlist loaded", "rules", list.Len(), "file", o.allowlist)

	// The base file applies to every tap; the control socket layers per-tap
	// policy on top as VMs' tags change. In open-untagged mode a tap with no
	// per-tap policy is left unrestricted (untagged sandbox = unlimited egress).
	pol := policy.New(list)
	pol.SetDefaultAllow(o.openUntagged)

	im := ipmap.New()
	im.MinTTL = o.minTTL

	// Static always-allow IPs (e.g. an infra endpoint reached by IP).
	for _, s := range o.staticAllowIPs {
		a, err := netip.ParseAddr(s)
		if err != nil {
			log.Error("bad --allow-ip", "value", s, "err", err)
			return 2
		}
		im.Pin("static", a)
	}

	var deny dnsproxy.DenyMode
	switch strings.ToLower(o.denyMode) {
	case "nxdomain":
		deny = dnsproxy.DenyNXDOMAIN
	case "refused":
		deny = dnsproxy.DenyREFUSED
	default:
		fmt.Fprintf(os.Stderr, "sluice run: --deny must be nxdomain or refused, got %q\n", o.denyMode)
		return 2
	}
	proxy, err := dnsproxy.New(dnsproxy.Config{
		Allow: pol, IPMap: im, Upstreams: o.upstreams, Deny: deny, Logger: log,
	})
	if err != nil {
		log.Error("dns proxy", "err", err)
		return 1
	}

	ctx, cancel := notifyContext()
	defer cancel()

	// DNS servers (udp + tcp).
	servers := proxy.Servers(o.dnsListen)
	for _, s := range servers {
		s := s
		go func() {
			if err := s.ListenAndServe(); err != nil {
				log.Error("dns server", "net", s.Net, "addr", s.Addr, "err", err)
				cancel()
			}
		}()
	}
	log.Info("dns gateway listening", "addr", o.dnsListen, "upstreams", strings.Join(o.upstreams, ","), "deny", deny.String())

	// eBPF meter. Optional for observability, required for enforcement.
	mtr, err := meter.Load()
	if err != nil {
		if o.enforce {
			log.Error("meter load failed but --enforce set; refusing to run without enforcement", "err", err)
			shutdownDNS(servers)
			return 1
		}
		log.Warn("metering/enforcement disabled (eBPF unavailable)", "err", err)
		mtr = nil
	} else {
		defer mtr.Close()
		if err := mtr.SetEnforce(o.enforce); err != nil {
			log.Error("set enforce", "err", err)
		}
		log.Info("eBPF meter loaded", "enforce", o.enforce, "open_untagged", o.openUntagged, "tap_prefix", o.tapPrefix)
	}

	// poke lets a policy push trigger an immediate reconcile instead of waiting
	// for the next sync tick. Buffered + non-blocking so a push never stalls.
	poke := make(chan struct{}, 1)
	kick := func() {
		select {
		case poke <- struct{}{}:
		default:
		}
	}

	// Background loops.
	if mtr != nil {
		go syncLoop(ctx, o, mtr, im, pol, poke, log)
		if o.reportInterval > 0 {
			go reportLoop(ctx, o.reportInterval, mtr, im, log)
		}
	}

	// Host-local control socket: serves per-VM bandwidth, accepts per-tap policy.
	if o.apiSocket != "" {
		var cm control.Meter
		if mtr != nil {
			cm = mtr
		}
		srv := control.New(cm, im, pol, kick, log)
		go func() {
			if err := srv.Serve(ctx, o.apiSocket); err != nil {
				log.Error("control socket", "path", o.apiSocket, "err", err)
				cancel()
			}
		}()
	}

	<-ctx.Done()
	log.Info("shutting down")
	shutdownDNS(servers)
	return 0
}

func shutdownDNS(servers []*dns.Server) {
	for _, s := range servers {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.ShutdownContext(ctx)
		cancel()
	}
}

// syncLoop keeps tap attachments and the kernel allow-set current. It also wakes
// on a poke (a policy push) so new grants apply without waiting for the tick.
func syncLoop(ctx context.Context, o runOpts, mtr *meter.Meter, im *ipmap.Map, pol *policy.Policy, poke <-chan struct{}, log *slog.Logger) {
	t := time.NewTicker(o.syncInterval)
	defer t.Stop()
	reconcile(o, mtr, im, pol, log) // once immediately
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcile(o, mtr, im, pol, log)
		case <-poke:
			reconcile(o, mtr, im, pol, log)
		}
	}
}

func reconcile(o runOpts, mtr *meter.Meter, im *ipmap.Map, pol *policy.Policy, log *slog.Logger) {
	// Attach to any new (or recreated) taps; pin their host (gateway) addresses
	// so guest→gateway traffic (DNS, metadata, ssh) is never dropped in enforce
	// mode. Attach re-attaches when a name's tap was torn down and rebuilt under
	// the same sbtap<idx> — it reports whether it actually (re)attached.
	present := make(map[string]struct{})
	for _, name := range tapInterfaces(o.tapPrefix) {
		present[name] = struct{}{}
		attached, err := mtr.Attach(name)
		if err != nil {
			log.Warn("attach tap", "iface", name, "err", err)
			continue
		}
		if attached {
			for _, a := range interfaceAddrs(name) {
				im.Pin("gateway", a)
			}
			log.Info("attached tap", "iface", name)
		}
	}
	// Detach taps whose VM went away, freeing their kernel links and dropping
	// their per-ifindex allow-set/counter entries in the meter.
	for _, name := range mtr.AttachedNames() {
		if _, ok := present[name]; !ok {
			mtr.Detach(name)
			log.Info("detached tap", "iface", name)
		}
	}

	// Expire stale DNS entries, then mirror the live policy into the kernel:
	// the base + pinned infra as the ifindex-0 wildcard, and each tap's own
	// resolved grants keyed by its ifindex. Enforcement is gated per tap: in
	// open-untagged mode only taps that carry a per-tap policy are enforced;
	// otherwise (classic mode) every tap is enforced against the base list.
	im.Sweep()
	snap := im.Snapshot()
	if err := mtr.SyncAllowed(pol.BaseGrants(snap)); err != nil {
		log.Warn("sync base allow-set", "err", err)
	}
	for ifindex, name := range mtr.Ifaces() {
		if err := mtr.SyncAllowedFor(ifindex, pol.TapGrants(name, snap)); err != nil {
			log.Warn("sync tap allow-set", "iface", name, "err", err)
		}
		enforced := o.enforce && (!o.openUntagged || pol.IsEnforced(name))
		if err := mtr.SetEnforceFor(ifindex, enforced); err != nil {
			log.Warn("set tap enforcement", "iface", name, "err", err)
		}
	}
}

// reportLoop periodically emits a per-domain bandwidth breakdown.
func reportLoop(ctx context.Context, every time.Duration, mtr *meter.Meter, im *ipmap.Map, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			flows, err := mtr.Flows()
			if err != nil {
				log.Warn("read flows", "err", err)
				continue
			}
			usage := report.Aggregate(flows, im)
			if len(usage) == 0 {
				continue
			}
			fmt.Fprintf(os.Stdout, "\n=== per-domain bandwidth @ %s ===\n", time.Now().Format(time.Kitchen))
			report.WriteTable(os.Stdout, usage)
			// Also emit the top talker as a structured log line.
			top := usage[0]
			log.Info("bandwidth report", "domains", len(usage),
				"top_domain", top.Domain, "top_total_bytes", top.Total())
		}
	}
}

// tapInterfaces lists up interfaces whose name starts with prefix.
func tapInterfaces(prefix string) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range ifaces {
		if strings.HasPrefix(i.Name, prefix) {
			out = append(out, i.Name)
		}
	}
	return out
}

// interfaceAddrs returns the IP addresses configured on an interface.
func interfaceAddrs(name string) []netip.Addr {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(ipn.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out
}
