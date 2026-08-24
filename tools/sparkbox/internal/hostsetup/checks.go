package hostsetup

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/bootsecrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// Status is a check outcome. A Fail means the host cannot run sparkbox as
// configured; a Warn is a soft problem (often self-healing, e.g. fleet keys are
// generated on first serve); Pass is all-clear.
type Status int

const (
	Pass Status = iota
	Warn
	Fail
)

func (s Status) String() string {
	switch s {
	case Pass:
		return "PASS"
	case Warn:
		return "WARN"
	default:
		return "FAIL"
	}
}

// Result is one check's outcome. Hint is a one-line remediation shown for
// anything that is not a Pass.
type Result struct {
	Name   string
	Status Status
	Detail string
	Hint   string
	// Output is multi-line supporting evidence (today: the tail of the unit's
	// journal behind a crash-loop FAIL), printed indented under the result. It
	// is a separate field rather than newlines inside Detail because
	// PrintResults aligns Detail against the name column — a newline there
	// prints flush-left and wrecks the report.
	Output string
}

// Check is a named, pure diagnostic: it reads the host only through the Probe,
// so it runs identically against a real host and an in-memory fake.
type Check struct {
	Name string
	Run  func(Probe, Config) Result
}

// The fleet key PEM basenames sparkbox serve loads, owned by bootsecrets so
// the check can't drift from what a fleet boot writes.
var fleetKeyFiles = bootsecrets.KeyFiles()

// The keys a host may have and runs fine without. Reported, never failed on:
// see bootsecrets.OptionalKeyFiles for why they are a separate list.
var optionalFleetKeyFiles = bootsecrets.OptionalKeyFiles()

// DefaultChecks is the ordered doctor battery. Environment checks come first
// (they need no config), then artifact/config checks, then live-service checks.
func DefaultChecks() []Check {
	return []Check{
		{"operating system", checkOS},
		{"cpu architecture", checkArch},
		{"root privileges", checkRoot},
		{"kvm device", checkKVM},
		{"hardware virtualization", checkVirt},
		{"ipv4 forwarding", checkIPForward},
		{"reverse-path filter", checkRPFilter},
		{"firecracker binary", checkFirecracker},
		{"guest kernel", checkKernel},
		{"rootfs template", checkRootfs},
		// Right after the template, because it is a question ABOUT the template
		// and the reader has just been told whether there is one.
		{"agent tooling", checkAgentTools},
		{"fleet keys", checkFleetKeys},
		{"users.conf", checkUsers},
		{"disk space", checkDisk},
		{"sandbox NAT rules", checkNAT},
		{"tailscale status", checkTailscaleStatus},
		{"tailscale preferences", checkTailscalePreferences},
		{"tailscale routes", checkTailscaleRoutes},
		{"node control reachability", checkNodeControlReachability},
		{nodeControlMTLSCheckName, checkNodeControlMTLSUnavailable},
		{"sparkbox service", checkService},
		// The SECOND unit setup starts, and for a long time the only one nothing
		// ever proved had come up (see checkSluiceService).
		{"sluice service", checkSluiceService},
		// After the liveness verdict: it reads the running gateway's command
		// line, so its "running service" / "service not running" wording only
		// makes sense once the reader has seen whether the service is up.
		{"egress control", checkEgress},
		// Last: it reads the running service's PID, so it wants the liveness
		// check's verdict already on screen above it.
		{"sparkbox version", checkVersions},
	}
}

// preflightChecksFor is the gate Provision runs before it touches anything, for
// THIS host's platform. It sits beside preflightChecks() rather than replacing
// it, so the linux battery is provably untouched.
func preflightChecksFor(e *Env) []Check {
	if e.Probe.GOOS() == "darwin" {
		return darwinPreflightChecks(e)
	}
	return preflightChecks()
}

// verifyChecksFor is the battery Provision's verify pass runs — the one that
// decides the exit code.
//
// On darwin this must NOT be DefaultChecks(): those interrogate the local
// systemd, the local iptables and the local /dev/kvm, so a darwin run would end
// in a green report about the laptop rather than about the gateway it just
// provisioned. That is F7's exact shape, one layer out.
func verifyChecksFor(e *Env) []Check {
	if e.Probe.GOOS() == "darwin" {
		return darwinVerifyChecks(e)
	}
	return checksWithNodeControlHealth(DefaultChecks(), e)
}

// DoctorChecksFor is the battery `sparkbox doctor` runs.
//
// On linux it is DefaultChecks(), including the fleet-route checks added for
// configurable guest subnets.
//
// On darwin it is the host layer PLUS the machine layer, which is one more
// section than the verify pass runs. Not a second opinion: doctor is a
// STANDALONE process, so nothing has already asked whether this Mac can host a
// machine, whereas Provision's verify pass runs minutes after its own preflight
// asked exactly that. Both sections come from the same two functions, so the
// questions cannot drift.
func DoctorChecksFor(e *Env) []Check {
	if e == nil || e.Probe == nil {
		return DefaultChecks()
	}
	if e.Probe.GOOS() == "darwin" {
		return darwinDoctorChecks(e)
	}
	return checksWithNodeControlHealth(DefaultChecks(), e)
}

func checksWithNodeControlHealth(checks []Check, e *Env) []Check {
	if e.NodeControlHealth != nil && e.NodeControlHealth.Run != nil {
		for i := range checks {
			if checks[i].Name == nodeControlMTLSCheckName {
				checks[i] = *e.NodeControlHealth
				if checks[i].Name == "" {
					checks[i].Name = nodeControlMTLSCheckName
				}
				break
			}
		}
	}
	return checks
}

// RunChecks evaluates every check against the probe and config.
func RunChecks(p Probe, cfg Config, checks []Check) []Result {
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		r := c.Run(p, cfg)
		if r.Name == "" {
			r.Name = c.Name
		}
		out = append(out, r)
	}
	return out
}

// AnyFail reports whether any result is a hard failure — doctor's exit code and
// setup's preflight gate both key off this.
func AnyFail(results []Result) bool {
	for _, r := range results {
		if r.Status == Fail {
			return true
		}
	}
	return false
}

// PrintResults writes an aligned report. Each non-pass line carries its hint.
func PrintResults(w io.Writer, results []Result) {
	width := 0
	for _, r := range results {
		if len(r.Name) > width {
			width = len(r.Name)
		}
	}
	var pass, warn, fail int
	for _, r := range results {
		switch r.Status {
		case Pass:
			pass++
		case Warn:
			warn++
		case Fail:
			fail++
		}
	}
	for _, r := range results {
		fmt.Fprintf(w, "  [%s] %-*s  %s\n", r.Status, width, r.Name, r.Detail)
		if r.Status != Pass && r.Hint != "" {
			fmt.Fprintf(w, "         %-*s  ↳ %s\n", width, "", r.Hint)
		}
		// Evidence is inlined so the operator sees the actual error (e.g.
		// "bind: address already in use") without running a second command —
		// the whole point of the crash-loop FAIL.
		for _, line := range strings.Split(strings.TrimRight(r.Output, "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			fmt.Fprintf(w, "         %-*s  │ %s\n", width, "", line)
		}
	}
	fmt.Fprintf(w, "\n  %d passed, %d warnings, %d failed\n", pass, warn, fail)
}

// --- individual checks ------------------------------------------------------

func pass(detail string) Result       { return Result{Status: Pass, Detail: detail} }
func warn(detail, hint string) Result { return Result{Status: Warn, Detail: detail, Hint: hint} }
func fail(detail, hint string) Result { return Result{Status: Fail, Detail: detail, Hint: hint} }

func checkOS(p Probe, _ Config) Result {
	switch p.GOOS() {
	case "linux":
		return pass("linux")
	case "darwin":
		// A Mac is a first-class sparkbox host — it just is not the host the
		// gateway runs ON. `sparkbox setup` creates a nested linux machine with
		// KVM (Apple's `container machine --virtualization`) and provisions the
		// gateway inside it, so the KVM requirement is satisfied one layer down
		// and checked there (see darwinVerifyChecks).
		return pass("darwin (macOS host; the gateway runs in a nested linux machine)")
	default:
		return fail(p.GOOS(), "sparkbox hosts must run Linux (firecracker needs KVM) or macOS on Apple Silicon "+
			"(where setup provisions a nested linux machine); "+
			"on this machine you can still develop against `sparkbox serve --driver mock`")
	}
}

func checkArch(p Probe, _ Config) Result {
	switch a := p.GOARCH(); a {
	case "amd64", "arm64":
		return pass(a)
	default:
		return warn(a, "only amd64 and arm64 have published artifact releases")
	}
}

func checkRoot(p Probe, _ Config) Result {
	if p.Uid() == 0 {
		return pass("root")
	}
	return warn(fmt.Sprintf("uid %d", p.Uid()),
		"run `sparkbox setup`/`serve` as root — firecracker needs /dev/kvm and tap devices")
}

func checkKVM(p Probe, _ Config) Result {
	if _, err := p.Stat("/dev/kvm"); err != nil {
		return fail("/dev/kvm missing",
			"enable nested virtualization / VT-x in BIOS or the cloud instance; not a KVM-capable host")
	}
	if p.Uid() == 0 && !p.Writable("/dev/kvm") {
		return warn("/dev/kvm present but not writable",
			"add the service user to the kvm group, or run as root")
	}
	return pass("/dev/kvm present")
}

func checkVirt(p Probe, _ Config) Result {
	// ARM64 does not advertise x86's vmx/svm flags. checkKVM immediately before
	// this check already reports whether /dev/kvm exists and is writable, so
	// avoid repeating the same failure here.
	if p.GOARCH() == "arm64" {
		return pass("not applicable (ARM64 availability is reported by the /dev/kvm check)")
	}

	b, err := p.ReadFile("/proc/cpuinfo")
	if err != nil {
		return warn("could not read /proc/cpuinfo", "check the host manually for VT-x/AMD-V")
	}
	if strings.Contains(string(b), "vmx") || strings.Contains(string(b), "svm") {
		return pass("VT-x/AMD-V available")
	}
	return fail("no vmx/svm flag in /proc/cpuinfo",
		"CPU virtualization is disabled or unavailable; firecracker cannot boot guests")
}

func checkIPForward(p Probe, _ Config) Result {
	v, err := p.Sysctl("net.ipv4.ip_forward")
	if err != nil {
		return warn("unknown", "`sparkbox setup` sets net.ipv4.ip_forward=1")
	}
	if v == "1" {
		return pass("enabled")
	}
	return warn("disabled (net.ipv4.ip_forward="+v+")",
		"sandbox egress needs routing; `sparkbox setup` sets it, or `sysctl -w net.ipv4.ip_forward=1`")
}

func checkRPFilter(p Probe, _ Config) Result {
	v, err := p.Sysctl("net.ipv4.conf.all.rp_filter")
	if err != nil {
		return warn("unknown", "`sparkbox setup` sets rp_filter=1 (strict) to stop guest source-spoofing")
	}
	if v == "1" {
		return pass("strict")
	}
	return warn("not strict (rp_filter="+v+")",
		"the metadata service trusts the source address; set net.ipv4.conf.all.rp_filter=1")
}

func checkFirecracker(p Probe, cfg Config) Result {
	bin := "firecracker"
	if _, err := p.LookPath(bin); err != nil {
		// Fall back to the install path setup uses, in case PATH lacks it.
		if _, serr := p.Stat(cfg.FirecrackerBin); serr == nil {
			bin = cfg.FirecrackerBin
		} else {
			return fail("firecracker not found",
				"`sparkbox setup` fetches it from the release; or install from github.com/firecracker-microvm/firecracker")
		}
	}
	out, err := p.Run(bin, "--version")
	if err != nil {
		return warn("present but `--version` failed", "verify the firecracker binary is runnable on this arch")
	}
	return pass(firstLine(out))
}

func checkKernel(p Probe, cfg Config) Result {
	if fi, err := p.Stat(cfg.KernelPath); err == nil && !fi.IsDir() {
		return pass(cfg.KernelPath)
	}
	return fail("no vmlinux at "+cfg.KernelPath,
		"`sparkbox setup` fetches the guest kernel from the release")
}

func checkRootfs(p Probe, cfg Config) Result {
	path := cfg.rootfsPath()
	if fi, err := p.Stat(path); err == nil && !fi.IsDir() {
		return pass(path)
	}
	return fail("no rootfs template at "+path,
		"`sparkbox setup` fetches + decompresses the rootfs; or set --default-image to a template you have")
}

// checkAgentTools reports whether the rootfs template this host clones actually
// carries the agent CLIs.
//
// It is the diagnostic that was missing when it mattered. The DGX ran for a day
// creating sandboxes with no claude, no codex, no pi and no hivemind in them: the
// refresher was installed, its timer had run that morning and logged "templates
// already current", and doctor was green — because nothing on the box was
// asking the template, only a host-side stamp that a release upgrade had
// replaced the template underneath. The versions were right and the file they
// described was gone.
//
// So the question is asked of the image, with debugfs — read-only, no loop
// device, safe against a template that sandboxes are being reflinked from right
// now. WARN and not FAIL: a host in this state is healthy in every other way and
// serves sandboxes fine, they are simply empty, and that is the operator's call
// to make about a box they may have provisioned with --agent-tools=false.
func checkAgentTools(p Probe, cfg Config) Result {
	path := cfg.rootfsPath()
	if fi, err := p.Stat(path); err != nil || fi.IsDir() {
		// checkRootfs, one line above, has already failed on this and said what
		// to do. Repeating it as a second failure would only add noise.
		return pass("no template to inspect (see rootfs template above)")
	}
	out, err := p.Run("debugfs", "-R", "cat "+templateToolsStamp, path)
	if err != nil && len(out) == 0 {
		return warn("could not read "+path+" (debugfs not available?)",
			"install e2fsprogs so this can be checked; the refresher needs it too")
	}
	stamp := stampLine(out)
	if stamp == "" {
		return warn("the rootfs template does not carry the complete agent CLI set — every sandbox created from it may be missing claude, codex, pi or hivemind",
			"run "+filepath.Join("/usr/local/sbin", refreshToolsScript)+" (or `sparkbox setup`, which installs and runs it) — "+
				"this is also what a template replaced by a release upgrade looks like")
	}
	return pass(stamp)
}

func checkFleetKeys(p Probe, cfg Config) Result {
	if cfg.Gateway != "" {
		return pass("not used on fleet node (node identity is generated locally)")
	}
	dir := cfg.keyDir()
	var missing []string
	for _, f := range fleetKeyFiles {
		if _, err := p.Stat(filepath.Join(dir, f)); err != nil {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return pass("present in " + dir + optionalKeyNote(p, dir))
	}
	// Absence is not fatal on a standalone host: `sparkbox serve` mints these on
	// first start (LoadOrCreateKey). It only matters on a fleet host with
	// --require-keys, which is out of scope for `setup`.
	return warn(fmt.Sprintf("missing %s in %s", strings.Join(missing, ", "), dir),
		"generated automatically on first `sparkbox serve` (LoadOrCreateKey); nothing to do for a standalone host")
}

// optionalKeyNote reports which optional fleet keys this host does not have, as
// a clause on the fleet-keys line rather than a check of its own — their
// absence is a configuration statement, not a problem, and a host with none of
// them is the ordinary case. It is worth one clause because each one silently
// disables a feature: no node_ca_*.pem is an SSH-only control plane, and no
// github_app_key.pem is repo attachment without credentials, which surfaces
// only as a failed clone inside a VM.
func optionalKeyNote(p Probe, dir string) string {
	var absent []string
	for _, f := range optionalFleetKeyFiles {
		if _, err := p.Stat(filepath.Join(dir, f)); err != nil {
			absent = append(absent, f)
		}
	}
	if len(absent) == 0 {
		return ""
	}
	return "; optional and not set: " + strings.Join(absent, ", ")
}

func checkUsers(p Probe, cfg Config) Result {
	if cfg.Gateway != "" {
		return pass("not used on fleet node (accounts live on gateway)")
	}
	b, err := p.ReadFile(cfg.UsersPath)
	if err != nil {
		return fail("no users.conf at "+cfg.UsersPath,
			"`sparkbox setup --operator-key <path>` writes it; the first user is the operator")
	}
	n, perr := countUsers(b)
	if perr != nil {
		return fail("users.conf parse error: "+perr.Error(),
			"each line must be '<handle> <ssh-ed25519 AAAA... comment>'")
	}
	if n == 0 {
		return fail("users.conf has no entries",
			"add at least one '<handle> <public key>' line — nobody can log in otherwise")
	}
	return pass(fmt.Sprintf("%d user(s)", n))
}

// countUsers counts users.conf entries via users.ParseSeedLine — the same
// parser SeedFile uses — so doctor validates the file without opening the
// sqlite store and can never drift from what serve accepts.
func countUsers(b []byte) (int, error) {
	n := 0
	for i, raw := range strings.Split(string(b), "\n") {
		_, _, _, ok, err := users.ParseSeedLine(raw)
		if err != nil {
			return 0, fmt.Errorf("line %d: %w", i+1, err)
		}
		if ok {
			n++
		}
	}
	return n, nil
}

func checkDisk(p Probe, cfg Config) Result {
	// Check the data dir if it exists, else its parent (pre-provision).
	path := cfg.dataDir()
	if _, err := p.Stat(path); err != nil {
		path = cfg.Root
		if _, err := p.Stat(path); err != nil {
			path = "/"
		}
	}
	free, err := p.DiskFreeBytes(path)
	if err != nil || free == 0 {
		return warn("unknown", "ensure the data volume has room for the rootfs template + per-sandbox copies")
	}
	const min = 40 << 30 // 40 GiB: rootfs template + a few sandbox copies + snapshots
	gib := float64(free) / (1 << 30)
	if free < min {
		return warn(fmt.Sprintf("%.0f GiB free on %s", gib, path),
			"low on space; the universal rootfs alone is several GB and each sandbox adds write deltas")
	}
	return pass(fmt.Sprintf("%.0f GiB free on %s", gib, path))
}

// checkNAT reports whether this host has the packet-filter rules sandboxes
// need, and the any-port forwarding chain THIS host's mode actually builds.
//
// It used to assert the SPARKBOX_EDGE chain unconditionally. But
// deploy/sparkbox-net.sh only builds that chain in uplink-REDIRECT mode
// (SPARKBOX_EDGE_REDIRECT=1); with a dedicated edge IP — the mode the DGX runs
// and docs/dedicated-edge-ip-cutover.md recommends — it builds SPARKBOX_TNET
// with DNAT rules instead and never creates SPARKBOX_EDGE at all. So a
// perfectly healthy gateway reported "[WARN] sandbox NAT rules  SPARKBOX_EDGE
// chain not found" on every single run, and the remedy it suggested
// ("rules install with `sparkbox setup`") re-installed the same script that
// deliberately skips that chain (F8). A check that cries wolf forever is worse
// than no check: it teaches the operator to skim the report.
//
// The mode is not knowable from iptables, only from the variables the script
// reads — so this reads them from the same place the script does. A Check may
// only touch the host through the Probe, and Probe.ReadFile is exactly that:
// sparkbox-net.service sources sparkbox.env via EnvironmentFile=, so parsing
// that file gives the check the script's own inputs rather than a guess.
//
// It also asserts what the check's NAME has always claimed and never verified:
// the POSTROUTING MASQUERADE that gives every sandbox its egress. Neither
// any-port chain has anything to do with that, and its absence is the failure
// that actually kills sandboxes.
func checkNAT(p Probe, cfg Config) Result {
	mode := readNATMode(p, cfg)
	// Read one built-in chain first. If iptables cannot be run at all — absent,
	// or (far more often) doctor running unprivileged, since checkRoot is only a
	// WARN — then every question below answers "no" for a reason that has
	// nothing to do with the rules, and reporting a missing MASQUERADE on a host
	// that has one is the same lie in a new place.
	//
	// The error alone is the test, deliberately. Probe.Run is CombinedOutput, and
	// an unprivileged iptables is not silent: it prints "iptables v1.8.10
	// (nf_tables): Could not fetch rule set generation id: Permission denied (you
	// must be root)" and exits 4. Guarding on an EMPTY output as well only caught
	// the binary-is-absent case and let the far commoner privilege case straight
	// through, so `sparkbox doctor` as a normal user on a healthy gateway
	// announced that its sandbox egress NAT was missing. Listing a built-in nat
	// chain does not fail on a working host, so err != nil is enough.
	postrouting, err := p.Run("iptables", "-t", "nat", "-nL", "POSTROUTING")
	if err != nil {
		return warn("could not read the nat table ("+err.Error()+")",
			"reading iptables needs privilege — re-run `sparkbox doctor` as root")
	}
	subnet := mode.guestSubnet
	if subnet == "" {
		subnet = cfg.guestSubnet()
	}
	masq := strings.Contains(postrouting, subnet)
	edge := natChainExists(p, edgeChain)
	tnet := natChainExists(p, tnetChain)

	var missing, found, unexpected []string
	if masq {
		found = append(found, "sandbox MASQUERADE ("+subnet+")")
	} else {
		missing = append(missing, "the "+subnet+" POSTROUTING MASQUERADE that carries all sandbox egress")
	}
	switch {
	case !mode.known:
		// No sparkbox.env to read: this host was provisioned by hand, or --root
		// points somewhere else, or doctor is running before setup ever did.
		// Demanding a specific chain here is exactly the mistake this check is
		// being fixed for, so report what is actually there and say plainly that
		// the mode could not be determined.
		switch {
		case edge:
			found = append(found, edgeChain+" present")
		case tnet:
			found = append(found, tnetChain+" present")
		default:
			missing = append(missing, "any-port forwarding (neither "+edgeChain+" nor "+tnetChain+")")
		}
	default:
		if mode.redirect {
			if edge {
				found = append(found, edgeChain+" (uplink REDIRECT mode)")
			} else {
				missing = append(missing, edgeChain+" (SPARKBOX_EDGE_REDIRECT is on)")
			}
		} else if edge {
			// The script SKIPS building this chain in tunnel mode; it never
			// flushes one that is already there, and it never removes the
			// PREROUTING hook. So a host flipped from redirect to tunnel mode
			// keeps hijacking uplink TCP until it is rebooted or cleaned by hand.
			unexpected = append(unexpected, edgeChain+" is still installed although SPARKBOX_EDGE_REDIRECT is off")
		}
		if mode.tnet {
			if tnet {
				found = append(found, tnetChain+" ("+mode.why+")")
			} else {
				missing = append(missing, tnetChain+" ("+mode.why+")")
			}
		}
		if !mode.redirect && !mode.tnet {
			// A legitimate configuration: behind a reverse tunnel with no
			// dedicated edge IP, web traffic arrives on loopback and no
			// forwarding chain is wanted. Saying so is the point — the old check
			// called this a fault.
			found = append(found, "any-port forwarding off (no uplink REDIRECT, no edge IP)")
		}
	}

	detail := strings.Join(found, "; ")
	if len(missing) == 0 && len(unexpected) == 0 {
		if !mode.known {
			return pass(detail + "; mode not determined (no " + cfg.envPath() + ")")
		}
		return pass(detail)
	}
	var hints []string
	if len(unexpected) > 0 {
		hints = append(hints, "the packet-filter script only skips a stale chain, it never tears one down — "+
			"drop its PREROUTING hook and `iptables -t nat -F "+edgeChain+"`, or reboot")
	}
	if len(missing) > 0 {
		if mode.known {
			hints = append(hints, "these install with `sparkbox setup` and are rebuilt at every boot by "+
				"sparkbox-net.service — `systemctl status sparkbox-net` for why they were not")
		} else {
			hints = append(hints, "there is no "+cfg.envPath()+", so the any-port mode cannot be determined: "+
				"run `sparkbox setup` (with --edge-ip <ip> for a dedicated edge address), or point --root at "+
				"this host's sparkbox home")
		}
	}
	return warn(strings.Join(append(unexpected, missing...), "; "), strings.Join(hints, "; "))
}

// The two chains deploy/sparkbox-net.sh can build, named once so the check and
// the script cannot drift apart again (which is what F8 was).
const (
	edgeChain = "SPARKBOX_EDGE" // uplink REDIRECT mode
	tnetChain = "SPARKBOX_TNET" // dedicated edge IP / tailnet mode
)

// natSelectors is which any-port chains this host's sparkbox.env asks for.
type natSelectors struct {
	// known is false when sparkbox.env could not be read at all, in which case
	// neither flag below means anything.
	known       bool
	redirect    bool   // SPARKBOX_EDGE_REDIRECT: build SPARKBOX_EDGE
	tnet        bool   // SPARKBOX_EDGE_IP or SPARKBOX_TAILNET_IF: build SPARKBOX_TNET
	why         string // which variable turned tnet on, for the report
	guestSubnet string
}

// readNATMode mirrors deploy/sparkbox-net.sh's own selectors, exactly.
//
// The two are NOT mutually exclusive: the tailnet block sits outside the
// `if SPARKBOX_EDGE_REDIRECT` fence, so a host with both set builds both chains
// and a check written as if/else would be wrong on it.
//
// The shell writes "${SPARKBOX_EDGE_REDIRECT:-1}" != 1, and `:-` substitutes
// the default for an EMPTY value as well as a missing one. So a bare
// `SPARKBOX_EDGE_REDIRECT=` line means the redirect is ON. Only a literal
// non-"1" value turns it off.
func readNATMode(p Probe, cfg Config) natSelectors {
	b, err := p.ReadFile(cfg.envPath())
	if err != nil {
		return natSelectors{guestSubnet: cfg.guestSubnet()}
	}
	kv, err := parseEnv(strings.NewReader(string(b)))
	if err != nil {
		return natSelectors{guestSubnet: cfg.guestSubnet()}
	}
	m := natSelectors{
		known: true, redirect: kv["SPARKBOX_EDGE_REDIRECT"] == "" || kv["SPARKBOX_EDGE_REDIRECT"] == "1",
		guestSubnet: effectiveGuestSubnet(cfg, kv),
	}
	switch {
	case kv["SPARKBOX_EDGE_IP"] != "":
		m.tnet, m.why = true, "dedicated edge IP "+kv["SPARKBOX_EDGE_IP"]
	case kv["SPARKBOX_TAILNET_IF"] != "":
		m.tnet, m.why = true, "tailnet interface "+kv["SPARKBOX_TAILNET_IF"]
	}
	return m
}

// natChainExists reports whether a nat chain is installed. Substring matching
// on `iptables -nL` output rather than parsing it: the question is only "is the
// chain there", and a rule-by-rule parser for a format that differs between
// iptables-legacy and -nft would be more code with more ways to be wrong.
func natChainExists(p Probe, chain string) bool {
	out, err := p.Run("iptables", "-t", "nat", "-nL", chain)
	return err == nil && strings.Contains(out, chain)
}

// checkEgress reports whether this gateway actually has per-VM egress control.
//
// It is the F2 headline. A gateway with no --sluice-socket wires a nil syncer
// (cmd/sparkbox/main.go) and one with no --guest-dns leaves guests on public
// DNS: in either case every sandbox reaches the whole internet, no error is
// logged, no check failed, and nothing anywhere says so. "Silently loses egress
// filtering" is not a state a diagnostic tool should be unable to report.
//
// The gateway's OWN command line is the truth, not this config: doctor is
// routinely run with no flags at all, and on an upgraded host the operator's
// sparkbox.env bundles still override whatever setup templated. /proc/<pid>/cmdline
// is what the daemon is running with, bundles and all — the same reason
// checkVersions reads /proc/<pid>/exe rather than trusting BinPath.
//
// This check used to carry a TODO recording why `setup` could not install
// sluice: tools/sluice is a separate Go module, no CI built it, and no release
// published a binary, so there was nothing to fetch and inventing a URL would
// have been worse than admitting it. That gap is closed — a release now ships
// sluice-linux-<arch> with SHA256_SLUICE in the manifest, go.yml builds and
// tests the module, and `sparkbox setup --sluice` installs it (see sluice.go).
// So the remediation below names a command that exists.
//
// What the check still cannot do is decide anything for the operator. Egress
// filtering changes what running sandboxes can reach, so it stays opt-in, and
// the unfiltered default stays a WARN that says so in full.
func checkEgress(p Probe, cfg Config) Result {
	flags, source := gatewayFlags(p, cfg)
	socket, guestDNS := flags["--sluice-socket"], flags["--guest-dns"]

	switch {
	case socket == "" && guestDNS == "":
		return warn("this gateway has no egress control — every sandbox reaches the whole internet ("+source+")",
			"that is the default and it is silent: sandboxes are unfiltered. Turn it on with "+
				"`sparkbox setup --sluice`, which installs the egress gateway from this release, seeds an allowlist, "+
				"and points guests at it (untagged sandboxes stay unrestricted; only tagged ones are filtered)")
	case socket == "":
		return warn("guests resolve through "+guestDNS+" but no --sluice-socket: the gateway pushes no egress policy ("+source+")",
			"add --sluice-socket /run/sluice.sock so the gateway can program the allowlist; without it the resolver enforces only its own defaults")
	case guestDNS == "":
		return warn("--sluice-socket "+socket+" is set but --guest-dns is not: guests are on public DNS and bypass the allowlist ("+source+")",
			"add --guest-dns <resolver-ip> (the address sluice's DNS listener binds, e.g. 172.30.0.53) — the socket alone enforces nothing")
	}
	// Both halves are configured; the remaining question is whether the daemon
	// on the other end of the socket is actually there. A socket path that does
	// not exist means every policy push fails and the gateway carries on.
	//
	// FAIL rather than WARN when THIS run asked for --sluice: setup has just
	// installed and started the daemon, so an absent socket is not a host
	// somebody else configured oddly, it is this run's own work not having
	// happened — and a WARN there is what let `setup --sluice` exit 0 and print
	// "egress: sluice on …" over a daemon that never answered. A doctor run
	// (which has no --sluice flag) still gets the WARN, because a hand-installed
	// sluice that is merely stopped is the operator's business, not a broken
	// provision.
	if _, err := p.Stat(socket); err != nil {
		detail := "--sluice-socket " + socket + " does not exist — sluice is not answering (" + source + ")"
		if cfg.Sluice {
			return fail(detail, sluiceUnitAdvice(p))
		}
		return warn(detail, sluiceUnitAdvice(p))
	}
	return pass("sluice at " + socket + ", guests resolve through " + guestDNS + " (" + source + ")")
}

// sluiceUnitAdvice explains WHY the control socket is missing by asking systemd
// about the unit, instead of telling every operator to go read `systemctl
// status` for themselves.
//
// The condition-failed case is the one worth the code. sluice.service carries
// ConditionKernelVersion=>=6.6 (its meter attaches with a TCX link, which does
// not exist below that), and systemd SKIPS a unit whose condition fails while
// exiting 0 for `systemctl start` — so the unit sits at inactive/dead with
// ConditionResult=no and every surface that samples ActiveState reads it as
// "not started yet". Without this, a host whose kernel is simply too old would
// be told to "start it", which would go on quietly succeeding and doing nothing.
func sluiceUnitAdvice(p Probe) string {
	out, _ := p.Run("systemctl", "show", sluiceUnit,
		"--property=LoadState,ActiveState,SubState,ConditionResult,NRestarts")
	kv := parseKV(out)
	switch {
	case kv["LoadState"] == "" && kv["ActiveState"] == "":
		return "could not ask systemd about " + sluiceUnit + "; until sluice answers on that socket, " +
			"the gateway's policy pushes go nowhere and sandboxes are unfiltered"
	case kv["LoadState"] == "not-found":
		return sluiceUnit + " is not installed on this host — the --sluice-socket flag names a daemon that was never put here. " +
			"Run `sparkbox setup --sluice` to install and enable it, or drop the flag so the gateway stops claiming an egress filter it does not have"
	case kv["ConditionResult"] == "no":
		return sluiceUnit + " is installed but systemd SKIPPED it because its start condition failed " +
			"(ConditionKernelVersion=>=6.6 — sluice's eBPF meter attaches with a TCX link, which older kernels do not have). " +
			"`systemctl start sluice` will keep exiting 0 without running anything: upgrade the kernel, " +
			"or drop --sluice-socket/--guest-dns so this host is honestly unfiltered rather than silently so"
	case kv["ActiveState"] == "failed" || kv["SubState"] == "auto-restart":
		return sluiceUnit + " is installed and failing (" + orDash(kv["ActiveState"]) + "/" + orDash(kv["SubState"]) +
			", " + orDash(kv["NRestarts"]) + " restarts) — read `journalctl -u sluice -n 50`. " +
			"The usual causes are a missing allowlist file (sluice exits 1 when the path its --allowlist names cannot be " +
			"opened, so the journal reads status=1/FAILURE), a resolver address that is busy or is not on this host " +
			"(\"bind: address already in use\" / \"cannot assign requested address\"), and an eBPF load this kernel refused"
	case kv["ActiveState"] == "active":
		return sluiceUnit + " is active but the socket is absent: it is either still starting, or was started with a " +
			"different --api-listen than the gateway's --sluice-socket. Compare `systemctl cat sluice` with the gateway's flags"
	default:
		return sluiceUnit + " is " + orDash(kv["ActiveState"]) + " — start it with `systemctl start sluice` " +
			"(or `sparkbox setup --sluice` to install its allowlist and unit properly). Until it answers, sandboxes are unfiltered"
	}
}

// checkSluiceService proves sluice is ALIVE, for the same reason checkService
// exists for the gateway — and it was the missing half of that lesson.
//
// The failure it closes, in full, because it is F7 reintroduced on the newer
// unit: `stepEnableServices` runs `systemctl enable --now sluice.service`, the
// unit is Type=simple, so systemd returns 0 the moment the fork succeeds. The
// process then dies, Restart=always plus StartLimitIntervalSec=0 restarts it
// every two seconds forever, and NOTHING in the run looked again: the A1
// liveness probe only ever sampled sparkbox.service, checkEgress could only
// WARN, so AnyFail was false, `setup` exited 0, and printConnect announced
// "egress: sluice on 172.30.0.53:53 … tagged ones are filtered" over a daemon
// that had never once served a query — while guests handed --guest-dns had no
// resolver at all and so had no working DNS whatsoever.
//
// Three ways in, none of them exotic, and all of them silent before this check:
//
//   - The resolver address does not exist on the host. validateSluice
//     RECOMMENDS `--sluice-dns-addr 172.30.0.53:53` for a box that also runs the
//     wildcard responder, and the bind then fails with EADDRNOTAVAIL. (The
//     packet-filter script now creates that address on a dummy interface — see
//     SLUICE_DNS_IP in deploy/sparkbox-net.sh — so this is no longer the default
//     outcome, but an operator's own address on a host whose sparkbox-net did
//     not run still gets there.) The port preflight deliberately steps over
//     EADDRNOTAVAIL, so it cannot be the thing that catches this.
//   - The eBPF meter fails to load, which with the seeded `SLUICE_ARGS=--enforce
//     --open-untagged` is exit 1 and a permanent loop, with no flag from the
//     operator at all.
//   - The allowlist file is absent (exit 1, see sluiceAllowlistPath).
//
// The first of those does not even exit non-zero — sluice cancels its root
// context and returns 0 — so the unit never reaches ActiveState=failed. It
// oscillates active/auto-restart with NRestarts climbing, which is precisely the
// state a single is-active sample calls healthy. Hence the same two-sample probe
// the gateway gets: anything that moved between the samples is a crash loop.
func checkSluiceService(p Probe, cfg Config) Result {
	first := showUnit(p, sluiceUnit)

	// `systemctl show` exits 0 even for a unit that does not exist, so empty
	// LoadState AND ActiveState means systemctl itself could not answer.
	if first.load == "" && first.active == "" {
		if !cfg.Sluice {
			return pass("not checked (no --sluice, and systemd could not be queried)")
		}
		return warn("could not query systemd about "+sluiceUnit,
			"--sluice was asked for but this host's systemd cannot be queried, so whether the egress gateway is running is unknown")
	}
	if first.load != "loaded" {
		if !cfg.Sluice {
			// The overwhelmingly common case: a host with no egress gateway.
			// checkEgress is the check that has an opinion about that; saying it
			// twice would only teach the operator to skim the report.
			return pass(sluiceUnit + " not installed (LoadState=" + orDash(first.load) + "); this host has no egress gateway")
		}
		return fail(fmt.Sprintf("--sluice was requested but %s is not installed (LoadState=%s)", sluiceUnit, orDash(first.load)),
			sluiceUnitAdvice(p))
	}

	switch first.active {
	case "failed":
		return failWithUnitJournal(p, sluiceUnit, fmt.Sprintf("%s failed (%s)", sluiceUnit, orDash(first.sub)),
			sluiceUnitAdvice(p))
	case "inactive", "deactivating":
		detail := fmt.Sprintf("%s %s (%s)", sluiceUnit, first.active, orDash(first.sub))
		// With --sluice this run just ran `enable --now` on it, so "not running"
		// is this run having failed, not a host in a state of its own. It is also
		// how a condition-skipped unit looks (ConditionKernelVersion=>=6.6:
		// systemd skips it and `start` still exits 0) — sluiceUnitAdvice asks
		// systemd for ConditionResult and says which of the two it is.
		if cfg.Sluice {
			return failWithUnitJournal(p, sluiceUnit, detail, sluiceUnitAdvice(p))
		}
		return warn(detail, sluiceUnitAdvice(p))
	case "active", "activating":
		// fall through to the liveness comparison below
	default:
		return warn("unknown state "+orDash(first.active), sluiceUnitAdvice(p))
	}

	// Only now is the settle window worth paying for — every branch above
	// returned immediately, so a doctor run on a host with no sluice stays
	// instant and never sleeps.
	if cfg.ServiceSettle > 0 {
		p.Sleep(cfg.ServiceSettle)
	}
	second := showUnit(p, sluiceUnit)

	const loopHint = "sluice is restarting faster than it can serve, so guests pointed at --guest-dns have NO resolver: " +
		"the journal below has the reason (a missing allowlist file, an address the host does not hold, " +
		"a busy :53, or an eBPF load this kernel refused)"
	if looped, why := restarted(first, second); looped {
		return failWithUnitJournal(p, sluiceUnit, fmt.Sprintf("%s is crash-looping: %s", sluiceUnit, why), loopHint)
	}
	// Same reasoning as checkService: a sample taken inside the RestartSec gap
	// reads activating/auto-restart and moves NEITHER restart signal, so without
	// this a loop that straddles the end of the window reports "stable".
	if first.sub == "auto-restart" || second.sub == "auto-restart" {
		return failWithUnitJournal(p, sluiceUnit,
			fmt.Sprintf("%s is crash-looping: systemd has it in a restart backoff (SubState=auto-restart)", sluiceUnit), loopHint)
	}
	switch second.active {
	case "failed":
		return failWithUnitJournal(p, sluiceUnit, fmt.Sprintf("%s failed during the settle window (%s)", sluiceUnit, orDash(second.sub)), loopHint)
	case "inactive", "deactivating":
		return failWithUnitJournal(p, sluiceUnit, fmt.Sprintf("%s went %s during the settle window", sluiceUnit, second.active), loopHint)
	case "activating":
		if first.active == "active" {
			return failWithUnitJournal(p, sluiceUnit,
				fmt.Sprintf("%s restarted during the settle window (now %s/%s)", sluiceUnit, second.active, orDash(second.sub)), loopHint)
		}
	}
	if first.active == "activating" && second.active == "activating" {
		return warn(fmt.Sprintf("%s still starting after %s (%s)", sluiceUnit, cfg.ServiceSettle, orDash(second.sub)),
			"give it a moment and re-run `sparkbox doctor`; if it never leaves activating, read `journalctl -u sluice`")
	}
	detail := fmt.Sprintf("active (%s), stable", orDash(second.sub))
	if cfg.ServiceSettle > 0 {
		detail = fmt.Sprintf("active (%s), stable across a %s window", orDash(second.sub), cfg.ServiceSettle)
	}
	if second.restarts != "" && second.restarts != "0" {
		detail += fmt.Sprintf(" (%s lifetime restarts)", second.restarts)
	}
	return pass(detail)
}

// gatewayFlags reads the flags the running gateway was started with, falling
// back to this config when there is no live process to ask.
//
// The returned label names which of the two it is, because the difference
// matters to the operator: "configured" is a claim about what the next start
// will do, "running" is a fact about now.
func gatewayFlags(p Probe, cfg Config) (map[string]string, string) {
	if s := showService(p); isPID(s.mainPID) {
		if b, err := p.ReadFile("/proc/" + s.mainPID + "/cmdline"); err == nil && len(b) > 0 {
			// /proc/<pid>/cmdline is NUL-separated with a trailing NUL, which is
			// better than a shell would give us: every argument is already split
			// exactly as the kernel received it, so a value containing spaces
			// cannot be mis-parsed here.
			return flagPairs(strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")), "running service"
		}
	}
	return flagPairs(optionalFlagArgs(cfg)), "configured, service not running"
}

// optionalFlagArgs re-splits the rendered unit flags into argv words, so the
// fallback above reads the same shape as a real command line.
func optionalFlagArgs(cfg Config) []string {
	var out []string
	for _, line := range optionalFlags(cfg) {
		out = append(out, strings.Fields(line)...)
	}
	return out
}

// flagPairs collects "--flag value" and "--flag=value" from an argv slice. A
// flag with no value (--proxy-tls) maps to "true" so presence is testable.
func flagPairs(argv []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		if name, val, ok := strings.Cut(a, "="); ok {
			out[name] = val
			continue
		}
		if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
			out[a] = argv[i+1]
			i++
			continue
		}
		out[a] = "true"
	}
	return out
}

// checkService proves the gateway is *alive*, not merely that systemd currently
// labels it active.
//
// The old check was a single `systemctl is-active`. Because the unit restarts
// forever with a two-second backoff and no start-rate limit, that reported
// "active" over a gateway that had been dying on `bind: address already in use`
// every two seconds since it was installed — and `setup` printed
// "== sparkbox is provisioned ==" on top of it. (Sampling twice is not
// paranoia: during one such loop consecutive samples read "active" and
// "activating".)
//
// So: sample the unit, wait Config.ServiceSettle, sample again. If the main
// process was replaced in between — a climbing NRestarts, a moved start
// timestamp, a sample caught in the restart backoff (SubState=auto-restart), or
// a unit that was active and is activating again — the service is crash-looping
// and that is a FAIL, with the tail of its journal inlined so the operator reads
// the real error here.
func checkService(p Probe, cfg Config) Result {
	first := showService(p)

	// `systemctl show` exits 0 even for a unit that does not exist, so an error
	// here means systemctl itself could not answer (not installed, no dbus, not
	// booted with systemd). We genuinely cannot tell, so say so.
	if first.load == "" && first.active == "" {
		detail := "unknown"
		if first.err != nil && first.raw != "" {
			detail = firstLine(first.raw)
		}
		return warn(detail, "could not query systemd; on a systemd host run `sparkbox setup`, then this reports live state")
	}
	if first.load != "loaded" {
		return warn(fmt.Sprintf("unit %s (LoadState=%s)", serviceUnit, orDash(first.load)),
			"not provisioned yet? run `sparkbox setup` to install and start "+serviceUnit)
	}

	switch first.active {
	case "failed":
		return failWithJournal(p, fmt.Sprintf("%s failed (%s)", serviceUnit, orDash(first.sub)),
			"the gateway is down; the journal below has the reason — fix it, then `systemctl start sparkbox`")
	case "inactive", "deactivating":
		return warn(fmt.Sprintf("%s %s (%s)", serviceUnit, first.active, orDash(first.sub)),
			"start it with `systemctl start sparkbox` (after `sparkbox setup`)")
	case "active", "activating":
		// fall through to the liveness comparison below
	default:
		return warn("unknown state "+orDash(first.active),
			"not provisioned yet? run `sparkbox setup`, then this reports the live state")
	}

	// Only now is a wait worth paying for: the unit exists and claims to be
	// running, which is exactly the case the single sample got wrong. Every
	// path above returns immediately, so a doctor run on a machine with no
	// systemd stays instant.
	if cfg.ServiceSettle > 0 {
		p.Sleep(cfg.ServiceSettle)
	}
	second := showService(p)

	if looped, why := restarted(first, second); looped {
		return failWithJournal(p, fmt.Sprintf("%s is crash-looping: %s", serviceUnit, why),
			"the gateway restarts faster than it can serve; the journal below has the reason (a busy port is the usual one)")
	}
	// SubState is evidence, not decoration. A sample taken inside the RestartSec
	// gap reads ActiveState=activating SubState=auto-restart with no main process
	// — systemd's own `Active: activating (auto-restart)` — and *neither* restart
	// signal moves across it: ExecMainStartTimestampMonotonic still holds the last
	// start (it only advances when a new main process starts) and NRestarts is
	// bumped when the restart is issued, not when the unit enters the backoff. So
	// a gateway that dies in the last two seconds of the settle window used to
	// fall through and print the self-contradicting "active (auto-restart),
	// stable" — F7 verbatim on a host whose systemd is older than v235 (no
	// NRestarts at all) or whose loop happened to straddle the window's end.
	if first.sub == "auto-restart" || second.sub == "auto-restart" {
		return failWithJournal(p, fmt.Sprintf("%s is crash-looping: systemd has it in a restart backoff (SubState=auto-restart)", serviceUnit),
			"the gateway restarts faster than it can serve; the journal below has the reason (a busy port is the usual one)")
	}
	switch second.active {
	case "failed":
		return failWithJournal(p, fmt.Sprintf("%s failed during the settle window (%s)", serviceUnit, orDash(second.sub)),
			"the gateway died while we watched it; the journal below has the reason")
	case "inactive", "deactivating":
		return failWithJournal(p, fmt.Sprintf("%s went %s during the settle window", serviceUnit, second.active),
			"the gateway stopped while we watched it; the journal below has the reason")
	case "activating":
		// It was up when we started watching and is starting again now: the
		// process we sampled first is gone. That is a restart even when the
		// replacement has not come up far enough to move the start timestamp,
		// which is the other way the fall-through below produced a PASS.
		if first.active == "active" {
			return failWithJournal(p, fmt.Sprintf("%s restarted during the settle window (now %s/%s)", serviceUnit, second.active, orDash(second.sub)),
				"the gateway was running and is starting again; the journal below has the reason it went down")
		}
	}
	if first.active == "activating" && second.active == "activating" {
		return warn(fmt.Sprintf("%s still starting after %s (%s)", serviceUnit, cfg.ServiceSettle, orDash(second.sub)),
			"give it a moment and re-run `sparkbox doctor`; if it never leaves activating, read `journalctl -u sparkbox`")
	}
	detail := fmt.Sprintf("active (%s), stable", orDash(second.sub))
	if cfg.ServiceSettle > 0 {
		detail = fmt.Sprintf("active (%s), stable across a %s window", orDash(second.sub), cfg.ServiceSettle)
	}
	if second.restarts != "" && second.restarts != "0" {
		// Restarts that stopped climbing are history, not a fault, but they are
		// worth showing: they are how an operator learns the box had a bad boot.
		detail += fmt.Sprintf(" (%s lifetime restarts)", second.restarts)
	}
	return pass(detail)
}

// checkVersions compares the three sparkbox versions that must agree on a
// healthy host: the binary installed at BinPath, the one the *running service*
// is actually executing, and the release the operator asked for.
//
// This is the check that would have caught the DGX: a "v0.4.0" setup left a
// stale v0.3.0 at /usr/local/bin/sparkbox, and everything else reported healthy
// while the gateway silently lacked every feature the release was cut for.
// The running version is read through /proc/<pid>/exe rather than BinPath
// because that is the inode the process actually holds — after an in-place
// upgrade the two differ until the unit is restarted, which is the whole point.
//
// A skew is a WARN, not a FAIL: the host is running, just not what was asked
// for. Anything that cannot be compared (a hand-built "dev" binary, the default
// `--release latest`) is skipped rather than warned about, or every developer
// machine would warn forever.
func checkVersions(p Probe, cfg Config) Result {
	if cfg.BinPath == "" {
		return pass("not checked (no --bin-path)")
	}
	if _, err := p.Stat(cfg.BinPath); err != nil {
		return warn("no sparkbox binary at "+cfg.BinPath,
			"the systemd unit's ExecStart points there; `sparkbox setup` installs the binary it runs from")
	}
	onDisk := binaryVersion(p.Run, cfg.BinPath)

	running := ""
	if s := showService(p); isPID(s.mainPID) {
		running = binaryVersion(p.Run, "/proc/"+s.mainPID+"/exe")
	}

	parts := []string{fmt.Sprintf("binary %s %s", cfg.BinPath, orUnknown(onDisk))}
	if running != "" {
		parts = append(parts, "running "+running)
	}
	if concreteVersion(cfg.Release) {
		parts = append(parts, "requested "+cfg.Release)
	}
	detail := strings.Join(parts, ", ")

	var skew []string
	if concreteVersion(onDisk) && concreteVersion(running) && onDisk != running {
		skew = append(skew, fmt.Sprintf("the running service is %s but %s is %s (restart it: `systemctl restart sparkbox`)", running, cfg.BinPath, onDisk))
	}
	if concreteVersion(cfg.Release) && concreteVersion(onDisk) && cfg.Release != onDisk {
		skew = append(skew, fmt.Sprintf("%s is %s but the requested release is %s (re-run `sparkbox setup` with the %s binary)", cfg.BinPath, onDisk, cfg.Release, cfg.Release))
	}
	if len(skew) > 0 {
		return warn(detail, strings.Join(skew, "; "))
	}
	return pass(detail)
}

// failWithJournal builds a FAIL carrying the tail of the gateway unit's journal.
func failWithJournal(p Probe, detail, hint string) Result {
	return failWithUnitJournal(p, serviceUnit, detail, hint)
}

// failWithUnitJournal builds a FAIL carrying the tail of a unit's journal, so
// the operator sees the actual error without running a second command.
// journalctl needs privilege to read the system journal and doctor is routinely
// run unprivileged (checkRoot is only a WARN), so a refusal is reported as such
// rather than silently reading as "no evidence".
//
// Takes the unit because sluice's crash loop needs the same treatment as the
// gateway's, and for the same reason: the one line that names the cause
// ("cannot assign requested address", "load allowlist") is in ITS journal, and
// an operator who has to go and find it is an operator who will read
// "sluice.service is crash-looping" as a mystery.
func failWithUnitJournal(p Probe, unit, detail, hint string) Result {
	r := fail(detail, hint)
	out, err := p.Run("journalctl", "-u", unit, "-n", "20", "--no-pager")
	name := strings.TrimSuffix(unit, ".service")
	switch {
	case strings.TrimSpace(out) != "":
		r.Output = out
	case err != nil:
		r.Output = "(journal unavailable: " + err.Error() + " — re-run as root, or `journalctl -u " + name + " -n 50`)"
	default:
		r.Output = "(no journal entries for " + unit + ")"
	}
	return r
}

func isPID(s string) bool { return s != "" && s != "0" }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "(version unknown)"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
