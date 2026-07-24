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
	// ARM64 does not advertise x86's vmx/svm flags. A usable /dev/kvm is the
	// architecture-appropriate signal that the host exposed virtualization;
	// checkKVM separately reports writability for the current user.
	if p.GOARCH() == "arm64" {
		if _, err := p.Stat("/dev/kvm"); err != nil {
			return fail("/dev/kvm missing on arm64",
				"enable nested virtualization; ARM64 reports availability through /dev/kvm, not vmx/svm CPU flags")
		}
		return pass("/dev/kvm present (ARM64 does not use vmx/svm CPU flags)")
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

func checkService(p Probe, _ Config) Result {
	// `systemctl is-active` exits non-zero for inactive units, so read the
	// output rather than treating the error as fatal. Only the known one-word
	// states are meaningful; anything else (e.g. "System has not been booted
	// with systemd") means we can't tell, so report it as unknown, not a state.
	out, _ := p.Run("systemctl", "is-active", "sparkbox.service")
	switch firstLine(strings.TrimSpace(out)) {
	case "active":
		return pass("active")
	case "inactive", "failed", "activating", "deactivating":
		return warn(out, "start it with `systemctl start sparkbox` (after `sparkbox setup`)")
	default:
		return warn("unknown", "not provisioned yet? run `sparkbox setup`, then this reports the live state")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
