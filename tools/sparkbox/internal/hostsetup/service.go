package hostsetup

import (
	"strconv"
	"strings"
)

// serviceUnit is the one place the gateway unit is named. Spelling it twice
// (once in a check, once in a step) is how the NAT check drifted from the chain
// the net script actually builds, so both the liveness probe and the
// enable-services step read this.
const serviceUnit = "sparkbox.service"

// netUnit is the boot unit that builds the host packet filter (it runs
// deploy/sparkbox-net.sh). Named here for the same reason as serviceUnit: the
// steps that install it, the step that restarts it and the plan line that
// promises both have to agree on one string.
const netUnit = "sparkbox-net.service"

// serviceShowProps is the property list the liveness probe asks systemd for.
//
// ActiveState alone is a *state* sample, not liveness: the unit sets
// Restart=always, RestartSec=2 and StartLimitIntervalSec=0 (deliberately — the
// gateway crash-loops until its ports are free), so a permanently dying service
// reads "active" at almost any instant. NRestarts and the main process's start
// timestamp are the two quantities that move when it is looping.
//
// LoadState separates "no such unit — not provisioned yet" from "installed and
// dying": `systemctl show` exits 0 for a unit that does not exist and cheerfully
// reports ActiveState=inactive, so without LoadState the two are indistinguishable.
//
// The *Monotonic* timestamp is requested rather than ExecMainStartTimestamp
// because the human-readable form is empty in the gap between restarts, while
// the monotonic form is a plain microsecond integer that is always comparable.
const serviceShowProps = "LoadState,ActiveState,SubState,NRestarts,ExecMainPID,ExecMainStartTimestampMonotonic"

// serviceSample is one `systemctl show` reading of the gateway unit.
type serviceSample struct {
	load      string // LoadState: loaded | not-found | masked | error | ""
	active    string // ActiveState: active | activating | inactive | failed | deactivating
	sub       string // SubState: running | start | auto-restart | dead | failed
	restarts  string // NRestarts; "" when systemd is older than v235
	mainPID   string // ExecMainPID; "0" while no main process is running
	startedAt string // ExecMainStartTimestampMonotonic; "0" when never started
	err       error  // systemctl itself failed (absent, no dbus, …)
	raw       string
}

// showService samples the gateway unit.
func showService(p Probe) serviceSample { return showUnit(p, serviceUnit) }

// showUnit samples any unit setup owns. A non-zero exit is kept rather than
// returned: on a host without systemd the output ("System has not been booted
// with systemd…") is the only thing that explains the result to an operator.
//
// Parameterised on the unit name because sparkbox.service is no longer the only
// thing `setup` starts and therefore no longer the only thing it has to prove
// came UP. sluice.service is Type=simple with Restart=always and
// StartLimitIntervalSec=0, exactly like the gateway, so `enable --now` returns 0
// the instant the fork succeeds and a daemon that dies on every start reads as a
// successful one — F7's shape on the newer unit. The liveness comparison below
// (and checkSluiceService) is the only thing that can tell the difference, and
// it needed a sampler that is not hardwired to one unit.
func showUnit(p Probe, unit string) serviceSample {
	out, err := p.Run("systemctl", "show", unit, "--property="+serviceShowProps)
	kv := parseKV(out)
	return serviceSample{
		load:      kv["LoadState"],
		active:    kv["ActiveState"],
		sub:       kv["SubState"],
		restarts:  kv["NRestarts"],
		mainPID:   kv["ExecMainPID"],
		startedAt: kv["ExecMainStartTimestampMonotonic"],
		err:       err,
		raw:       out,
	}
}

// parseKV splits `systemctl show`'s KEY=VALUE lines into a map. Ordering is not
// relied on: systemd is free to reorder, omit unset properties, and (for
// NRestarts) not know the key at all.
func parseKV(out string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.IndexByte(line, '='); i > 0 {
			m[line[:i]] = strings.TrimSpace(line[i+1:])
		}
	}
	return m
}

// restarted reports whether the unit's main process was replaced between two
// samples, and a human-readable reason.
//
// Two independent signals, because neither is universally available: NRestarts
// needs systemd >= 235 (a missing key must read as "unknown", never as 0), and
// the start timestamp is 0 until a main process exists. A first sample taken
// before the process started (mainPID/startedAt == "0") followed by a real
// start is a *boot*, not a restart — that is what `enable --now` looks like —
// so it must not be reported as a crash loop.
func restarted(a, b serviceSample) (bool, string) {
	if a.restarts != "" && b.restarts != "" && a.restarts != b.restarts {
		na, erra := strconv.Atoi(a.restarts)
		nb, errb := strconv.Atoi(b.restarts)
		if erra == nil && errb == nil && nb > na {
			return true, "NRestarts climbed " + a.restarts + " → " + b.restarts
		}
	}
	if isStarted(a.startedAt) && isStarted(b.startedAt) && a.startedAt != b.startedAt {
		return true, "the main process restarted (ExecMainStartTimestampMonotonic " + a.startedAt + " → " + b.startedAt + ")"
	}
	return false, ""
}

func isStarted(ts string) bool { return ts != "" && ts != "0" }

// --- version comparison -----------------------------------------------------

// binaryVersion asks a sparkbox binary what release it is, by running
// `<path> version` and parsing "sparkbox <tag> (<os>/<arch>)". Anything else —
// the path is missing, is not executable, or is some other program that happens
// to live there — yields "" rather than an error, because every caller's answer
// to "I could not tell" is the same: say so and carry on.
//
// Executing the file is the only honest answer to "what version is this?" (the
// tag is linked in with -X main.version; nothing on disk records it), and it
// grants nothing new: the path is the one the unit's ExecStart already runs as
// root, so anyone who can write there has already won.
func binaryVersion(run func(string, ...string) (string, error), path string) string {
	if path == "" {
		return ""
	}
	out, err := run(path, "version")
	if err != nil {
		return ""
	}
	f := strings.Fields(firstLine(strings.TrimSpace(out)))
	if len(f) < 2 || f[0] != "sparkbox" {
		return ""
	}
	return f[1]
}

// concreteVersion reports whether v is a real release tag that can be compared
// with another. A hand-built binary says "dev" and `--release` defaults to
// "latest": comparing either of those would warn on every developer machine and
// on every `setup --release latest`, which is the common path.
func concreteVersion(v string) bool {
	switch strings.TrimSpace(v) {
	case "", "dev", "latest", "unknown":
		return false
	}
	return true
}

// compareVersions orders two release tags (v0.4.0, 0.4.1, v0.5.0-rc1) the way
// semver does for the parts we actually publish: numeric segments left to
// right, and a build with a pre-release suffix sorts *below* the same numbers
// without one. ok is false when either side is not a tag we can read, and every
// caller must then treat the comparison as "cannot tell" rather than as equal —
// refusing an install because a version string was unfamiliar would be worse
// than the downgrade the check exists to prevent.
func compareVersions(a, b string) (cmp int, ok bool) {
	an, apre, aok := splitVersion(a)
	bn, bpre, bok := splitVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := 0; i < len(an) || i < len(bn); i++ {
		var x, y int
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case apre == bpre:
		return 0, true
	case apre != "" && bpre == "":
		return -1, true // 1.0.0-rc1 < 1.0.0
	case apre == "" && bpre != "":
		return 1, true
	case apre < bpre:
		return -1, true
	default:
		return 1, true
	}
}

// splitVersion parses "v0.4.1-rc2" into ([0 4 1], "rc2", true).
func splitVersion(v string) (nums []int, pre string, ok bool) {
	v = strings.TrimSpace(v)
	if !concreteVersion(v) {
		return nil, "", false
	}
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		pre, v = v[i+1:], v[:i]
	}
	if v == "" {
		return nil, "", false
	}
	for _, seg := range strings.Split(v, ".") {
		n, err := strconv.Atoi(seg)
		if err != nil {
			return nil, "", false
		}
		nums = append(nums, n)
	}
	return nums, pre, true
}
