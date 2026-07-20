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
	"github.com/vanpelt/sparky/tools/sluice/internal/dnsproxy"
	"github.com/vanpelt/sparky/tools/sluice/internal/ipmap"
	"github.com/vanpelt/sparky/tools/sluice/internal/meter"
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
	denyMode       string
	logFormat      string
	minTTL         time.Duration
	syncInterval   time.Duration
	reportInterval time.Duration
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
	fs.StringVar(&o.denyMode, "deny", "nxdomain", "reply for blocked names: nxdomain|refused")
	fs.StringVar(&o.logFormat, "log", "json", "log format: json|text")
	fs.DurationVar(&o.minTTL, "min-ttl", ipmap.DefaultMinTTL, "floor for how long a resolved IP stays reachable")
	fs.DurationVar(&o.syncInterval, "sync-interval", 5*time.Second, "tap-discovery and allow-set sync period")
	fs.DurationVar(&o.reportInterval, "report-interval", 30*time.Second, "per-domain bandwidth report period (0 disables)")
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

	deny := dnsproxy.DenyNXDOMAIN
	if strings.EqualFold(o.denyMode, "refused") {
		deny = dnsproxy.DenyREFUSED
	}
	proxy, err := dnsproxy.New(dnsproxy.Config{
		Allow: list, IPMap: im, Upstreams: o.upstreams, Deny: deny, Logger: log,
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
		log.Info("eBPF meter loaded", "enforce", o.enforce, "tap_prefix", o.tapPrefix)
	}

	// Background loops.
	if mtr != nil {
		go syncLoop(ctx, o, mtr, im, log)
		if o.reportInterval > 0 {
			go reportLoop(ctx, o.reportInterval, mtr, im, log)
		}
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

// syncLoop keeps tap attachments and the kernel allow-set current.
func syncLoop(ctx context.Context, o runOpts, mtr *meter.Meter, im *ipmap.Map, log *slog.Logger) {
	t := time.NewTicker(o.syncInterval)
	defer t.Stop()
	reconcile(o, mtr, im, log) // once immediately
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcile(o, mtr, im, log)
		}
	}
}

func reconcile(o runOpts, mtr *meter.Meter, im *ipmap.Map, log *slog.Logger) {
	// Attach to any new taps; pin their host (gateway) addresses so guest→gateway
	// traffic (DNS, metadata, ssh) is never dropped in enforce mode.
	for _, name := range tapInterfaces(o.tapPrefix) {
		if mtr.Attached(name) {
			continue
		}
		if err := mtr.Attach(name); err != nil {
			log.Warn("attach tap", "iface", name, "err", err)
			continue
		}
		for _, a := range interfaceAddrs(name) {
			im.Pin("gateway", a)
		}
		log.Info("attached tap", "iface", name)
	}

	// Expire stale DNS entries, then mirror the live allow-set into the kernel.
	im.Sweep()
	snap := im.Snapshot()
	addrs := make([]netip.Addr, len(snap))
	for i, e := range snap {
		addrs[i] = e.Addr
	}
	if err := mtr.SyncAllowed(addrs); err != nil {
		log.Warn("sync allow-set", "err", err)
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
