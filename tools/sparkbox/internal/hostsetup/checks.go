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
		{"fleet keys", checkFleetKeys},
		{"users.conf", checkUsers},
		{"disk space", checkDisk},
		{"sandbox NAT rules", checkNAT},
		{"sparkbox service", checkService},
		// Last: it reads the running service's PID, so it wants the liveness
		// check's verdict already on screen above it.
		{"sparkbox version", checkVersions},
	}
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
	if p.GOOS() != "linux" {
		return fail(p.GOOS(), "sparkbox hosts must run Linux (firecracker needs KVM); "+
			"on this machine you can still develop against `sparkbox serve --driver mock`")
	}
	return pass("linux")
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
		return pass("present in " + dir)
	}
	// Absence is not fatal on a standalone host: `sparkbox serve` mints these on
	// first start (LoadOrCreateKey). It only matters on a fleet host with
	// --require-keys, which is out of scope for `setup`.
	return warn(fmt.Sprintf("missing %s in %s", strings.Join(missing, ", "), dir),
		"generated automatically on first `sparkbox serve` (LoadOrCreateKey); nothing to do for a standalone host")
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

func checkNAT(p Probe, _ Config) Result {
	// The boot unit installs the SPARKBOX_EDGE chain (deploy/sparkbox-net.sh).
	out, err := p.Run("iptables", "-t", "nat", "-nL", "SPARKBOX_EDGE")
	if err != nil || !strings.Contains(out, "SPARKBOX_EDGE") {
		return warn("SPARKBOX_EDGE chain not found",
			"sandbox NAT/any-port rules install with `sparkbox setup` (sparkbox-net.service)")
	}
	return pass("SPARKBOX_EDGE chain present")
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

// failWithJournal builds a FAIL carrying the tail of the unit's journal, so the
// operator sees the actual error without running a second command. journalctl
// needs privilege to read the system journal and doctor is routinely run
// unprivileged (checkRoot is only a WARN), so a refusal is reported as such
// rather than silently reading as "no evidence".
func failWithJournal(p Probe, detail, hint string) Result {
	r := fail(detail, hint)
	out, err := p.Run("journalctl", "-u", serviceUnit, "-n", "20", "--no-pager")
	switch {
	case strings.TrimSpace(out) != "":
		r.Output = out
	case err != nil:
		r.Output = "(journal unavailable: " + err.Error() + " — re-run as root, or `journalctl -u sparkbox -n 50`)"
	default:
		r.Output = "(no journal entries for " + serviceUnit + ")"
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
