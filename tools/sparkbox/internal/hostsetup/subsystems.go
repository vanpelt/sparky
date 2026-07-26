package hostsetup

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// The optional `sparkbox serve` subsystems setup can turn on.
//
// The live DGX gateway runs the wildcard DNS responder, R2 archiving, per-VM
// egress control, a Cloudflare DNS-01 wildcard certificate and an advertised
// SSH port of 22 — and not one of those was reachable from a `sparkbox setup`
// flag (F2). They were hand-written into sparkbox.env's flag bundles, which the
// unit appends after the templated flags, so the host's real configuration
// lived in a file setup never rewrites and no invocation could reproduce.
//
// This file is the fix: one ordered list that turns a Config into the flags the
// unit carries, and one validator that refuses the combinations `serve` would
// only reject at boot (where the failure reads as a crash loop rather than as a
// typo).

// guestSubnet is the sandbox address space deploy/sparkbox-net.sh masquerades
// and firecracker carves the per-VM /30s out of. Named here because checkNAT
// asserts the MASQUERADE rule by matching on it.
const guestSubnet = "172.30.0.0/16"

// optionalFlags renders the settings above into one "--flag value" line per
// setting for the unit's line-continued ExecStart, in a FIXED order.
//
// Fixed rather than map-ordered because stepSystemdUnits compares the rendered
// unit byte for byte: a render that shuffled its own output would report drift
// on every run and restart the gateway each time setup was invoked.
//
// A flag and its value share a line because systemd word-splits the ExecStart
// back into argv anyway, and a unit an operator can read is a unit an operator
// can debug. That is only safe because validateSubsystems has already refused
// any value containing whitespace.
func optionalFlags(c Config) []string {
	var out []string
	add := func(flag, val string) {
		if strings.TrimSpace(val) == "" {
			return
		}
		out = append(out, flag+" "+val)
	}
	if c.SSHAdvertisePort > 0 {
		add("--ssh-advertise-port", strconv.Itoa(c.SSHAdvertisePort))
	}
	add("--dns-addr", c.dnsAddr())
	add("--dns-answer", c.DNSAnswer)
	if c.ProxyTLS {
		out = append(out, "--proxy-tls")
	}
	add("--tls-provider", c.TLSProvider)
	add("--tls-email", c.TLSEmail)
	// Through the resolvers, not the raw fields: --sluice supplies both of these
	// when they were not named, and a unit rendered from the raw fields would
	// install the daemon and then never talk to it.
	add("--guest-dns", c.guestDNS())
	add("--sluice-socket", c.sluiceSocket())
	add("--archive-remote", c.ArchiveRemote)
	add("--archive-bucket", c.ArchiveBucket)
	return out
}

// validateSubsystems rejects settings setup cannot faithfully write into the
// unit, and combinations that would be silently ignored.
//
// "Silently ignored" is the important half. `serve` treats an --archive-remote
// with no --archive-bucket as "archiving disabled" (objstore.New returns a nil
// store when either is empty) and a --tls-provider with no --proxy-tls as
// nothing at all, so an operator who typed one of a pair gets a host that comes
// up green and does not do the thing they asked for. Refusing here is the only
// place that can tell them.
func validateSubsystems(cfg Config) error {
	// Everything below is templated into ExecStart as a bare word. Whitespace
	// would not reach the daemon as one argument — it would become extra
	// arguments and the service would die on an opaque flag-parse error at boot.
	// Same rule as validateAddrs and validateNodeFlags.
	for _, f := range []struct{ flag, val string }{
		{"--dns-answer", cfg.DNSAnswer},
		{"--tls-provider", cfg.TLSProvider},
		{"--tls-email", cfg.TLSEmail},
		{"--guest-dns", cfg.guestDNS()},
		{"--sluice-socket", cfg.sluiceSocket()},
		{"--archive-remote", cfg.ArchiveRemote},
		{"--archive-bucket", cfg.ArchiveBucket},
		{"--edge-ip", cfg.EdgeIP},
	} {
		if strings.ContainsAny(f.val, " \t\n") {
			return fmt.Errorf("invalid %s %q: whitespace is not allowed (it would become extra arguments in the unit's ExecStart)", f.flag, f.val)
		}
	}

	if cfg.SSHAdvertisePort < 0 || cfg.SSHAdvertisePort > 65535 {
		return fmt.Errorf("invalid --ssh-advertise-port %d: expected 1-65535 (0 advertises the real listen port)", cfg.SSHAdvertisePort)
	}

	// The wildcard responder needs something to answer with, and `serve` exits
	// at startup — i.e. crash-loops — when it has neither an explicit answer nor
	// a concrete IP in --proxy-addr. validateAddrs makes the same check from the
	// address side; this is the branch that lets --dns-answer satisfy it.
	for _, s := range splitCSV(cfg.DNSAnswer) {
		if net.ParseIP(s) == nil {
			return fmt.Errorf("invalid --dns-answer %q: %q is not an IP address (the responder answers *.%s with these)", cfg.DNSAnswer, s, cfg.ProxyDomain)
		}
	}
	if cfg.DNSAnswer != "" && cfg.dnsAddr() == "" {
		return fmt.Errorf("--dns-answer %s has no effect without --dns-addr: nothing serves the wildcard record", cfg.DNSAnswer)
	}

	// TLS: the provider and the account email are only ever read when the edge
	// terminates TLS.
	if !cfg.ProxyTLS && (cfg.TLSProvider != "" || cfg.TLSEmail != "") {
		return fmt.Errorf("--tls-provider/--tls-email need --proxy-tls: without it the edge serves cleartext and neither flag is read")
	}
	switch cfg.TLSProvider {
	case "", "cloudflare", "autocert":
	default:
		return fmt.Errorf("unknown --tls-provider %q: expected cloudflare (DNS-01 wildcard, needs CLOUDFLARE_API_TOKEN in the unit's environment) or autocert (per-host, on demand)", cfg.TLSProvider)
	}

	// Archiving is all-or-nothing.
	if (cfg.ArchiveRemote == "") != (cfg.ArchiveBucket == "") {
		return fmt.Errorf("--archive-remote and --archive-bucket must be given together (remote=%q bucket=%q): "+
			"with only one of them `sparkbox serve` disables archiving without an error",
			cfg.ArchiveRemote, cfg.ArchiveBucket)
	}

	// The guest resolver is an IP or the "gateway" sentinel; a hostname is
	// refused by the firecracker driver at VM-create time, which is far too late
	// to be a useful error.
	if d := cfg.guestDNS(); d != "" && d != "gateway" && net.ParseIP(d) == nil {
		return fmt.Errorf("invalid --guest-dns %q: expected an IP address (e.g. 172.30.0.53, the sluice resolver) or the literal \"gateway\"", d)
	}
	if s := cfg.sluiceSocket(); s != "" && !strings.HasPrefix(s, "/") {
		return fmt.Errorf("invalid --sluice-socket %q: expected an absolute path (e.g. /run/sluice.sock)", s)
	}
	if err := validateSluice(cfg); err != nil {
		return err
	}

	// The edge address is written into sparkbox.env and interpolated straight
	// into `ip addr add <ip>/32` and an iptables `-d <ip>` by sparkbox-net.sh,
	// where a typo is a boot-time shell error nobody reads.
	if cfg.EdgeIP != "" && net.ParseIP(cfg.EdgeIP) == nil {
		return fmt.Errorf("invalid --edge-ip %q: expected a bare IP address to give the edge its own /32 (e.g. 10.66.0.1)", cfg.EdgeIP)
	}
	return nil
}

// validateSluice rejects --sluice combinations that would install a daemon
// which cannot start, or which starts and filters nothing.
//
// Both failure modes are quiet, which is why they are refused here rather than
// discovered on the box: sluice's unit sets Restart=always, so a resolver that
// cannot bind is a permanent restart loop that `systemctl is-active` calls
// "active"; and a resolver nobody is pointed at is an egress filter with no
// traffic through it, which reports healthy in every way.
func validateSluice(cfg Config) error {
	if !cfg.Sluice {
		// --sluice-dns-addr without --sluice is a typo with a plausible
		// reading ("surely that turns it on"), and its actual effect is
		// nothing at all. Say so rather than ignoring it.
		if strings.TrimSpace(cfg.SluiceDNSAddr) != "" {
			return fmt.Errorf("--sluice-dns-addr %s has no effect without --sluice: nothing installs the resolver it would bind",
				cfg.SluiceDNSAddr)
		}
		return nil
	}
	// The addresses are ExecStart words in the sluice unit, same rule as every
	// other one. validateAddrs parses this one (it is in addrFlags), so this is
	// only the whitespace half.
	if strings.ContainsAny(cfg.SluiceDNSAddr, " \t\n") {
		return fmt.Errorf("invalid --sluice-dns-addr %q: expected host:port with no whitespace", cfg.SluiceDNSAddr)
	}

	// Two DNS servers, one host. sparkbox's own wildcard responder (--dns-addr)
	// answers *.<domain> for the edge; sluice's answers the allowlist for
	// guests. They are different servers with different jobs, and on the DGX
	// they collided on :53 — which is why that box ended up giving the sluice
	// resolver a dedicated 172.30.0.53 on a dummy interface and passing
	// --guest-dns as that literal.
	//
	// addrsOverlap is the same predicate the port preflight uses: a wildcard on
	// either side collides with everything, otherwise the hosts must match.
	// (The preflight would catch this too, but only after the multi-GB
	// download, and only as "address in use" without saying which of our own
	// two servers is the other one.)
	if sd := cfg.sluiceDNSAddr(); sd != "" && cfg.dnsAddr() != "" {
		sh, sp, serr := splitAddr(sd)
		eh, ep, eerr := splitAddr(cfg.dnsAddr())
		if serr == nil && eerr == nil && sp == ep && addrsOverlap(sh, eh) {
			return fmt.Errorf("--sluice-dns-addr %s collides with --dns-addr %s: "+
				"they are two different DNS servers (sluice answers the guest allowlist, the sparkbox responder answers *.%s) "+
				"and only one can hold the port. Give sluice an address of its own, e.g. --sluice-dns-addr 172.30.0.53:53 "+
				"(guests are then handed 172.30.0.53 automatically, and sparkbox-net.sh puts that address on a dummy "+
				"interface at boot so the bind has something to bind to)",
				sd, cfg.dnsAddr(), cfg.ProxyDomain)
		}
	}

	// A concrete resolver address that no guest can reach is an allowlist that
	// silently never applies: the guest gets the IP in its kernel args, its
	// resolver times out, and the sandbox looks like it has no network rather
	// than a filtered one. 127.0.0.0/8 is the way to get there by accident,
	// because it is what "bind it privately" looks like on every other flag in
	// this file — and a guest's loopback is its own.
	if host, _, err := splitAddr(cfg.sluiceDNSAddr()); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return fmt.Errorf("invalid --sluice-dns-addr %s: guests reach the resolver across their tap, and %s is the loopback of whichever machine asks — "+
				"no guest can ever reach it. Use the wildcard (:53), or an address the host holds on a guest-facing interface (e.g. 172.30.0.53:53)",
				cfg.sluiceDNSAddr(), host)
		}
	}
	return nil
}

// splitCSV splits a comma-separated list the way `serve` does, dropping empties
// so a trailing comma is not an error.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
