package hostsetup

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Listener opens a listening socket. It is the one seam the port preflight
// needs and the only piece of setup that talks to the network stack, so it is
// an interface for the same reason Probe/Runner/Fetcher are: a test must be
// able to say "this address is busy" without racing a real port on the
// developer's machine.
type Listener interface {
	// Listen binds address on network ("tcp" or "udp"). The returned Closer is
	// released immediately — the point is to prove the address is bindable, not
	// to hold it.
	Listen(network, address string) (io.Closer, error)
}

// netListener is the production Listener.
type netListener struct{}

// NewNetListener returns a Listener backed by the real network stack.
func NewNetListener() Listener { return netListener{} }

func (netListener) Listen(network, address string) (io.Closer, error) {
	// UDP is not a stream network, so it takes the packet-socket path. The
	// wildcard DNS responder serves both, and it is the UDP half that actually
	// answers resolvers — checking only TCP would miss the collision that
	// matters (systemd-resolved on :53 is the classic one).
	if strings.HasPrefix(network, "udp") {
		return net.ListenPacket(network, address)
	}
	return net.Listen(network, address)
}

// splitAddr parses a listen address into its host and numeric port.
//
// net.SplitHostPort rather than a strings.LastIndex, because ":443",
// "10.66.0.1:443" and "[fd7a::1]:443" are all addresses `sparkbox serve` (and
// therefore this preflight, and therefore PROXY_PORT) has to understand, and
// only the IPv6 literal makes the difference visible.
func splitAddr(addr string) (host string, port int, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", 0, fmt.Errorf("empty address")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("%q is not host:port (%v)", addr, err)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		// A named port ("https") resolves fine for net.Listen but not for
		// sparkbox-net.sh, which interpolates PROXY_PORT straight into an
		// iptables --to-ports. Refusing here is the only place that can say so.
		return "", 0, fmt.Errorf("%q has a non-numeric port %q", addr, portStr)
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("%q has port %d out of range 1-65535", addr, port)
	}
	return host, port, nil
}

// addrFlags is the ordered list of listen addresses setup owns: the flag name
// as `sparkbox serve` spells it, a human label for reports, and the networks
// the daemon will actually bind.
var addrFlags = []struct {
	flag     string
	label    string
	networks []string
	get      func(Config) string
}{
	{"--ssh-addr", "ssh gateway", []string{"tcp"}, Config.sshAddr},
	{"--proxy-addr", "http edge", []string{"tcp"}, Config.proxyAddr},
	{"--api-addr", "control api", []string{"tcp"}, Config.apiAddr},
	// dnsedge serves UDP and TCP on the same address (dnsedge.ListenAndServe).
	{"--dns-addr", "wildcard dns", []string{"udp", "tcp"}, Config.dnsAddr},
}

// validateAddrs rejects listen addresses setup cannot faithfully write down or
// that contradict another flag. It runs before anything is touched, because
// every one of these becomes a systemd ExecStart word or an iptables argument,
// where the failure surfaces as a crash loop at boot rather than as an error.
func validateAddrs(cfg Config) error {
	for _, a := range addrFlags {
		v := a.get(cfg)
		if v == "" {
			continue // only --dns-addr can be empty, and empty means "off"
		}
		// The addresses are templated into ExecStart as bare words, so
		// whitespace would not reach the daemon as one argument — it would
		// become extra arguments and the service would die on an opaque
		// flag-parse error at boot. Same rule as validateNodeFlags.
		if strings.ContainsAny(v, " \t\n") {
			return fmt.Errorf("invalid %s %q: expected host:port with no whitespace", a.flag, v)
		}
		if _, _, err := splitAddr(v); err != nil {
			return fmt.Errorf("invalid %s: %w", a.flag, err)
		}
	}
	// The wildcard DNS responder answers *.<domain> with --dns-answer, and with
	// the IP host of --proxy-addr when that is not given. With neither, `serve`
	// exits at startup on "--dns-addr set but no answer IP" — i.e. a crash loop,
	// discovered at boot, from a flag combination we can see is wrong right here.
	if cfg.dnsAddr() != "" && cfg.DNSAnswer == "" {
		if host, _, err := splitAddr(cfg.proxyAddr()); err == nil && net.ParseIP(host) == nil {
			return fmt.Errorf("--dns-addr %s needs an answer address: the responder answers *.%s with the IP host of --proxy-addr, "+
				"and %q has none. Give the edge a concrete address (--proxy-addr <ip>:<port>, e.g. the dedicated tailnet /32 the "+
				"DNS entry points at), or name the answers explicitly with --dns-answer <ip>[,<ip>...]",
				cfg.dnsAddr(), cfg.ProxyDomain, cfg.proxyAddr())
		}
	}
	if strings.TrimSpace(cfg.SSHAddr) != "" && cfg.MoveAdminSSH {
		// See Config.sshAddr for why this pair is refused rather than resolved:
		// --move-admin-ssh parks the host's sshd on :2222 to free :22, so a
		// gateway asked to bind anything but :22 either lands on top of the
		// sshd we just moved or leaves :22 serving nobody.
		if _, port, err := splitAddr(cfg.SSHAddr); err == nil && port != 22 {
			return fmt.Errorf("--ssh-addr %s cannot be combined with --move-admin-ssh: "+
				"that flag evicts the host's sshd from :22 (onto :2222) so the gateway can own :22, "+
				"so pass --ssh-addr <host>:22, or drop --move-admin-ssh and keep the gateway on %s",
				cfg.SSHAddr, cfg.SSHAddr)
		}
	}
	return nil
}

// --- effective addresses ----------------------------------------------------

// bundleOrder is the order the unit's ExecStart appends the env-file flag
// bundles in. It matters twice: Go's flag package lets a repeated flag win
// last, so this is the precedence, and the preflight has to probe the address
// the daemon will END UP with, not the one setup put in the template.
var bundleOrder = []string{"LOGIN_USER_FLAG", "SUBNET6_FLAG", "OVERCOMMIT_FLAGS", "TLS_FLAGS", "GATEWAY_FLAG", "EXTRA_FLAGS"}

// effectiveAddrs resolves what the gateway will actually bind: this config's
// addresses, with any override smuggled through an existing sparkbox.env
// bundle applied on top, plus a human-readable note for each override.
//
// This exists because setup never rewrites those bundles — they are the
// operator's editing surface (see mergeEnv) — so on an upgraded host the old
// `TLS_FLAGS=--ssh-addr 10.66.0.1:2222 --api-addr 127.0.0.1:8079` is still
// there and still wins over the freshly templated flags. Probing the flag
// values instead of the effective ones would validate ports the service never
// binds — and would report the DGX as conflict-free while it crash-looped on a
// different port entirely.
//
// It is also what managedEnv derives PROXY_PORT and the SSH-port keys from:
// those become iptables arguments, and pointing them at an address the daemon
// does not bind is silent breakage rather than a failure.
func effectiveAddrs(cfg Config, envKV map[string]string) (map[string]string, []string) {
	out := map[string]string{}
	for _, a := range addrFlags {
		out[a.flag] = a.get(cfg)
	}
	var notes []string
	for _, bundle := range bundleOrder {
		for flag, val := range parseFlagBundle(envKV[bundle]) {
			if _, ours := out[flag]; !ours || val == "" {
				continue
			}
			if val != out[flag] {
				notes = append(notes, fmt.Sprintf("%s=%s in sparkbox.env overrides %s %s → %s",
					bundle, flag, flag, orDash(out[flag]), val))
			}
			out[flag] = val
		}
	}
	return out, notes
}

// effectiveAddr is effectiveAddrs for a single flag, for the callers that want
// the answer rather than the reasoning (managedEnv, which turns two of these
// into iptables arguments).
func effectiveAddr(cfg Config, envKV map[string]string, flag string) string {
	addrs, _ := effectiveAddrs(cfg, envKV)
	return addrs[flag]
}

// parseFlagBundle pulls "--flag value" and "--flag=value" pairs out of one
// whitespace-split env bundle. Only the flags in addrFlags are of interest, so
// anything else is skipped rather than parsed — this is not a flag parser, it
// is a "did somebody re-set one of our addresses here" scanner.
func parseFlagBundle(s string) map[string]string {
	out := map[string]string{}
	fields := strings.Fields(s)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if !strings.HasPrefix(f, "--") {
			continue
		}
		name, val, hasEq := strings.Cut(f, "=")
		if !isAddrFlag(name) {
			continue
		}
		if hasEq {
			out[name] = val
			continue
		}
		if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "--") {
			out[name] = fields[i+1]
			i++
		}
	}
	return out
}

func isAddrFlag(name string) bool {
	for _, a := range addrFlags {
		if a.flag == name {
			return true
		}
	}
	return false
}

// readEnvKV reads the existing sparkbox.env, if any. A missing file is not an
// error — it is the fresh-host case, where nothing can be overriding anything.
func readEnvKV(cfg Config) map[string]string {
	b, err := os.ReadFile(cfg.envPath())
	if err != nil {
		return map[string]string{}
	}
	kv, err := parseEnv(strings.NewReader(string(b)))
	if err != nil {
		return map[string]string{}
	}
	return kv
}

// --- the preflight ----------------------------------------------------------

// portProbe is one address the gateway will bind, and therefore one address
// setup has to prove is free before it writes a unit that dies on it.
type portProbe struct {
	label   string
	flag    string
	network string
	addr    string
}

// wantedPorts expands the effective addresses into one probe per socket.
func wantedPorts(addrs map[string]string) []portProbe {
	var out []portProbe
	for _, a := range addrFlags {
		v := addrs[a.flag]
		if v == "" {
			continue
		}
		for _, n := range a.networks {
			out = append(out, portProbe{label: a.label, flag: a.flag, network: n, addr: v})
		}
	}
	return out
}

// preflightPorts proves every address this host will bind is actually
// bindable, and fails here — naming the address and, where the host can tell
// us, the process holding it — rather than at first boot.
//
// The failure this replaces: `setup` completed, printed a green report, and
// left a gateway restarting every two seconds on "listen tcp 127.0.0.1:8080:
// bind: address already in use" because an unrelated python process already
// had the port. A1's liveness probe catches that after the fact; this catches
// it before the multi-GB download, and says which port and whose.
//
// It runs on a gateway only. A fleet node opens none of these listeners —
// serveNode returns before the SSH door, the edge, the API and the DNS
// responder exist — so probing them there would invent conflicts.
func preflightPorts(e *Env) error {
	if e.Cfg.Gateway != "" {
		return nil
	}
	if e.Listen == nil {
		e.Listen = NewNetListener()
	}
	kv := readEnvKV(e.Cfg)
	addrs, notes := effectiveAddrs(e.Cfg, kv)
	for _, n := range notes {
		e.logf("   note: %s\n", n)
	}
	probes := wantedPorts(addrs)

	if e.Cfg.DryRun {
		// Reported, not attempted: a dry run must not open a socket any more
		// than it writes a file, and an operator planning a change on a live
		// host would otherwise see phantom conflicts against their own gateway.
		var parts []string
		for _, p := range probes {
			parts = append(parts, fmt.Sprintf("%s/%s (%s)", p.network, p.addr, p.flag))
		}
		e.logf("  - %-16s → would probe %s\n", "port-preflight", strings.Join(parts, ", "))
		return nil
	}

	warnProxyPortSkew(e, kv, addrs)

	// The main PID of our own service, so a re-run on a live host does not trip
	// over the gateway it provisioned last time. Looked up lazily and once: on
	// the overwhelmingly common path every port is free and nobody needs to
	// know, and `systemctl show` is not free.
	mainPID, pidKnown := "", false
	ourPID := func() string {
		if !pidKnown {
			pidKnown = true
			if e.Probe != nil {
				if pid := showService(e.Probe).mainPID; isPID(pid) {
					mainPID = pid
				}
			}
		}
		return mainPID
	}

	var conflicts []string
	for _, p := range probes {
		c, err := e.Listen.Listen(p.network, p.addr)
		if err == nil {
			_ = c.Close()
			e.logf("   %s %s/%s is free\n", p.flag, p.network, p.addr)
			continue
		}
		// Only "address already in use" is a conflict. Everything else this
		// bind can tell us is a fact about the host, not about a squatter, and
		// acting on it would block provisioning for the wrong reason:
		//
		//   EADDRNOTAVAIL — the address is not on this host YET. Expected for a
		//   dedicated edge IP: sparkbox-net.sh creates the `sparkedge` dummy
		//   interface and its /32 from SPARKBOX_EDGE_IP at boot, i.e. after the
		//   step that is running right now. Failing here would make the DGX's
		//   own configuration un-provisionable.
		//
		//   EACCES — a privileged port without the privilege. doctor's root
		//   check already reports that, and it is the same answer for every
		//   address rather than a fact about this one.
		//
		// Reported and stepped over, so the operator still sees a typo'd IP.
		if !isAddrInUse(err) {
			e.logf("   %s %s/%s could not be probed (%v) — not a conflict; continuing\n", p.flag, p.network, p.addr, err)
			continue
		}
		found, ok := listenerOwner(e, p.network, p.addr)
		who, mine, detail := diagnoseBusy(found, ok, ourPID())
		if mine {
			e.logf("   %s %s/%s is held by %s — that is this host's own gateway; it will rebind on restart\n",
				p.flag, p.network, p.addr, who)
			continue
		}
		if movingAdminSSHD(e.Cfg, p, found) {
			e.logf("   %s %s/%s is held by %s — that is the admin sshd --move-admin-ssh evicts (stepAdminSSH, later in this run)\n",
				p.flag, p.network, p.addr, orUnknownOwner(found))
			continue
		}
		conflicts = append(conflicts, fmt.Sprintf("%s %s (%s/%s): %s", p.flag, p.addr, p.network, p.label, detail))
	}
	if len(conflicts) > 0 {
		// Every conflict at once. Reporting the first would make an operator
		// re-run setup once per busy port, and each re-run is a fresh chance to
		// half-provision the host.
		return fmt.Errorf("port preflight failed — the gateway cannot bind:\n    %s\n  "+
			"free the port(s), or pick others with --ssh-addr/--proxy-addr/--api-addr/--dns-addr",
			strings.Join(conflicts, "\n    "))
	}
	return nil
}

// movingAdminSSHD reports whether a busy --ssh-addr probe is the host's own
// sshd on :22 that --move-admin-ssh is about to relocate.
//
// Without this the flag could not be used at all. --move-admin-ssh means
// exactly "take :22 from the distro sshd", Config.sshAddr therefore answers
// ":22", and the preflight runs before every step — including stepAdminSSH,
// the step that writes the `Port 2222` drop-in and restarts ssh.service. So the
// probe found sshd holding :22, called it a foreign conflict, and aborted the
// run whose whole purpose was to move it, offering a remedy ("pick another with
// --ssh-addr") that validateAddrs refuses in the same breath.
//
// The owner test is deliberately loose: Ubuntu 24.04 socket-activates ssh, so
// the listener belongs to systemd (pid 1) rather than sshd, and a host with
// neither `ss` nor `lsof` cannot name it at all. Guessing wrong here costs
// nothing that is not already covered — stepAdminSSH restarts ssh.service and
// the post-apply liveness check FAILs, with the journal's "address already in
// use" inlined, if :22 turns out to be held by something else.
func movingAdminSSHD(cfg Config, p portProbe, who owner) bool {
	if !cfg.MoveAdminSSH || p.flag != "--ssh-addr" {
		return false
	}
	if _, port, err := splitAddr(p.addr); err != nil || port != 22 {
		return false
	}
	switch who.name {
	case "", "sshd", "ssh", "systemd", "sshd-session":
		return true
	}
	return false
}

func orUnknownOwner(o owner) string {
	if o.name == "" && o.pid == "" {
		return "an unidentified listener"
	}
	return o.String()
}

// isAddrInUse reports whether a failed bind means somebody else already holds
// the address, as opposed to the address not existing on this host or the
// process not being allowed to bind it. net wraps the errno in an *OpError, so
// errors.Is (not a string match on "address already in use", which is
// localised by some libcs) is the way to ask.
func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }

// diagnoseBusy identifies who holds a busy address and whether it is us.
//
// "Is it us?" has to be answered generously, because the alternative is a tool
// that cannot be re-run on any host it has already provisioned: a live gateway
// holds every one of these ports by design. Three signals, in order of
// authority:
//
//  1. the owning PID is the service's own MainPID — conclusive;
//  2. the owning process is *named* sparkbox — the fallback for hosts where
//     systemd cannot be queried (a container, an unprivileged doctor run);
//  3. nobody could be identified at all, but our service is running — presumed
//     ours, because on a live host that is overwhelmingly what it is.
//
// Signals 2 and 3 can be wrong (a hand-started `sparkbox serve` in a terminal,
// or an unidentifiable squatter on a host that also happens to be running the
// service). That is deliberate: the cost of a false conflict is a host that
// cannot be re-provisioned, while the cost of a missed one is bounded — the
// post-apply liveness check still FAILs on the crash loop and inlines the
// "address already in use" line from the journal.
func diagnoseBusy(owner owner, found bool, mainPID string) (who string, mine bool, detail string) {
	switch {
	case found && mainPID != "" && owner.pid == mainPID:
		return owner.String() + ", the running " + serviceUnit, true, ""
	case found && owner.name == "sparkbox":
		return owner.String(), true, ""
	case found:
		return owner.String(), false, "in use by " + owner.String()
	case mainPID != "":
		return "pid " + mainPID + " (" + serviceUnit + ")", true, ""
	default:
		return "", false, "in use (could not identify the owner; `ss -lntup` and `lsof` were unavailable or said nothing)"
	}
}

// warnProxyPortSkew reports an existing sparkbox.env whose PROXY_PORT no longer
// matches the edge address, at the point in the run where the operator can still
// stop it. It is the silent breakage the proxyPort constant existed to prevent
// and never did: the packet filter forwards every any-port connection to
// PROXY_PORT, so a skew means each sandbox web route lands on a port nothing is
// listening on, with no error anywhere.
//
// It is a warning rather than a failure because stepEnvFile is about to FIX it
// (managedEnv derives PROXY_PORT from the same effective address compared here,
// and enable-services then restarts sparkbox-net so the new rules are live).
// The warning survives that because the two run at different times: this is the
// only thing an operator sees before the multi-GB download, and a host that
// fails later in the run is left with the skew this names.
func warnProxyPortSkew(e *Env, kv map[string]string, addrs map[string]string) {
	have, ok := kv["PROXY_PORT"]
	if !ok || have == "" {
		return
	}
	_, want, err := splitAddr(addrs["--proxy-addr"])
	if err != nil || strconv.Itoa(want) == have {
		return
	}
	e.logf("   WARNING: %s says PROXY_PORT=%s but the edge binds %s.\n"+
		"            sparkbox-net.sh forwards any-port traffic to PROXY_PORT, so every\n"+
		"            sandbox web route would land on a port nothing listens on.\n"+
		"            This run's host-config step corrects it to %d and restarts sparkbox-net;\n"+
		"            if the run does not reach that step, fix it by hand:\n"+
		"            sed -i 's/^PROXY_PORT=.*/PROXY_PORT=%d/' %s && systemctl restart sparkbox-net\n",
		e.Cfg.envPath(), have, addrs["--proxy-addr"], want, want, e.Cfg.envPath())
}

// --- who owns the port ------------------------------------------------------

// owner is a process holding a listening socket.
type owner struct {
	name string
	pid  string
}

func (o owner) String() string {
	switch {
	case o.name != "" && o.pid != "":
		return fmt.Sprintf("%s (pid %s)", o.name, o.pid)
	case o.name != "":
		return o.name
	case o.pid != "":
		return "pid " + o.pid
	}
	return "an unknown process"
}

// listenerOwner asks the host who is listening on addr. Best-effort by
// construction: `ss` is the modern answer and `lsof` the portable fallback, and
// a host with neither still gets a useful failure ("in use", address named) —
// it just cannot name the culprit. Both run through the injected Runner, so a
// test cans the output rather than standing up a real listener.
func listenerOwner(e *Env, network, addr string) (owner, bool) {
	host, port, err := splitAddr(addr)
	if err != nil {
		return owner{}, false
	}
	if o, ok := ssOwner(e, network, host, port); ok {
		return o, true
	}
	return lsofOwner(e, port)
}

func ssOwner(e *Env, network, host string, port int) (owner, bool) {
	// -l listening, -n numeric, -t TCP, -u UDP, -p process. Called only for an
	// address that already failed to bind, which on a healthy host is never.
	out, err := e.run("ss", "-lntup")
	if err != nil && len(out) == 0 {
		return owner{}, false
	}
	var loose owner
	var haveLoose bool
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// The Netid column is present with -t -u together, absent otherwise;
		// rather than count columns, find the first field that parses as
		// host:port with a NUMERIC port. The peer column is always "addr:*",
		// so it can never be mistaken for the local one.
		lhost, lport, ok := "", 0, false
		for _, f := range fields {
			h, p, perr := splitAddr(f)
			if perr == nil {
				lhost, lport, ok = h, p, true
				break
			}
		}
		if !ok || lport != port {
			continue
		}
		if network != "" && (strings.HasPrefix(line, "udp") || strings.HasPrefix(line, "tcp")) &&
			!strings.HasPrefix(line, network) {
			continue
		}
		o := parseSSProcess(line)
		if addrsOverlap(host, lhost) {
			return o, true
		}
		// Right port, a host we would not actually have collided with. Keep it
		// as evidence anyway — something took the port, and naming it is more
		// use to the operator than "unknown".
		loose, haveLoose = o, true
	}
	return loose, haveLoose
}

// parseSSProcess pulls the process out of ss's users:(("name",pid=123,fd=7))
// column. A missing column (ss without privilege) yields a zero owner, which
// still reads as "found the socket, could not name the process".
func parseSSProcess(line string) owner {
	i := strings.Index(line, `users:((`)
	if i < 0 {
		return owner{}
	}
	rest := line[i+len(`users:((`):]
	var o owner
	if j := strings.Index(rest, `"`); j >= 0 {
		rest = rest[j+1:]
		if k := strings.Index(rest, `"`); k >= 0 {
			o.name = rest[:k]
			rest = rest[k:]
		}
	}
	if j := strings.Index(rest, "pid="); j >= 0 {
		rest = rest[j+len("pid="):]
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		o.pid = rest[:end]
	}
	return o
}

// lsofOwner is the fallback for hosts without iproute2 (and for macOS, where
// the darwin setup path will want the same answer). `lsof -i:<port>` lists the
// command and pid in the first two columns.
func lsofOwner(e *Env, port int) (owner, bool) {
	out, err := e.run("lsof", "-nP", "-iTCP:"+strconv.Itoa(port)+",UDP:"+strconv.Itoa(port), "-sTCP:LISTEN")
	if err != nil && len(out) == 0 {
		return owner{}, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "COMMAND" {
			continue
		}
		if _, err := strconv.Atoi(fields[1]); err != nil {
			continue
		}
		return owner{name: fields[0], pid: fields[1]}, true
	}
	return owner{}, false
}

// addrsOverlap reports whether a socket bound to `have` would block a bind of
// `want` on the same port. A wildcard on either side collides with everything;
// otherwise the addresses have to be the same. This is why the DGX binds a
// dedicated /32 in the first place.
func addrsOverlap(want, have string) bool {
	return isWildcardHost(want) || isWildcardHost(have) || normalizeHost(want) == normalizeHost(have)
}

func isWildcardHost(h string) bool {
	switch normalizeHost(h) {
	case "", "*", "0.0.0.0", "::":
		return true
	}
	return false
}

// normalizeHost strips the decoration ss adds (brackets, %iface scopes) so a
// literal from a flag and a literal from a listing compare equal.
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	if i := strings.Index(h, "%"); i >= 0 {
		h = h[:i]
	}
	return h
}
