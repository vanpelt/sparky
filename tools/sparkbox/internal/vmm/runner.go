package vmm

import "fmt"

// Runner names a VMM: the thing a node runs, and the thing an environment may
// require of the node its sandboxes land on.
//
// The driver names were bare string literals in six places before this type
// existed — main.go's flag help, node.go's switch, ClaimStateDir's marker file,
// the helper's backend flag and both entrypoint scripts. Nothing checked that
// they agreed, and the one that mattered most was the marker: a typo there
// reads as "a different VMM owns this tree" and refuses to start a node whose
// disks are perfectly fine. One type, one parser, one place to add the third.
type Runner string

const (
	// RunnerAny is the zero value and means "no requirement". It is what every
	// environment created before this field existed carries, so it must keep
	// meaning what those environments meant: place me anywhere.
	RunnerAny Runner = ""
	// RunnerMock is the in-process fake. It boots no guest and needs no KVM,
	// and it is the documented standalone development path (AGENTS.md).
	RunnerMock Runner = "mock"
	// RunnerFirecracker is the default real driver.
	RunnerFirecracker Runner = "firecracker"
	// RunnerQEMU is the second real driver.
	RunnerQEMU Runner = "qemu"
)

// ParseRunner maps an operator-facing or API-facing string onto a Runner.
// The empty string is RunnerAny and is not an error: it is the ordinary
// "unspecified" that every existing row and request carries.
func ParseRunner(v string) (Runner, error) {
	switch Runner(v) {
	case RunnerAny, RunnerMock, RunnerFirecracker, RunnerQEMU:
		return Runner(v), nil
	default:
		return "", fmt.Errorf("unknown vmm runner %q (want %q, %q or %q)",
			v, RunnerFirecracker, RunnerQEMU, RunnerMock)
	}
}

// ParseRequirement is ParseRunner for the environment-facing field, and it
// differs in exactly one way: it refuses RunnerMock.
//
// A node may BE mock — that is the standalone dev path. An environment asking
// for mock is something else: a request that the platform give it a fake guest,
// which is never what anyone means and which would silently produce a sandbox
// that runs nothing. The asymmetry is deliberate and is the reason there are
// two parsers rather than one with a boolean.
func ParseRequirement(v string) (Runner, error) {
	r, err := ParseRunner(v)
	if err != nil {
		return "", err
	}
	if r == RunnerMock {
		return "", fmt.Errorf("an environment cannot require the %q runner (want %q or %q)",
			RunnerMock, RunnerFirecracker, RunnerQEMU)
	}
	return r, nil
}

// SatisfiedBy reports whether a node running `have` may host a sandbox whose
// environment requires `want`.
//
// Two rules, and the second is the interesting one:
//
//   - RunnerAny is satisfied by anything. This is what keeps every environment
//     that predates the field placeable on every node that predates it.
//
//   - A mock node satisfies EVERY requirement. Without this, adding a runner to
//     an environment would make it unschedulable on `sparkbox serve --driver
//     mock`, which is the portable development and integration path AGENTS.md
//     tells us to preserve — so the first person to set `--runner qemu` would
//     find their whole dev loop refusing to place anything, with a scheduling
//     error that reads like a fleet outage.
//
//     The cost is that a mock node in a REAL fleet attracts sandboxes that
//     wanted a real VMM. That is a misconfiguration either way: a mock node
//     boots no guests at all, so it is not a machine anybody reaches a working
//     sandbox on, and the failure is immediate and local rather than subtle.
//     Trading a loud second failure for a broken dev path is not worth it.
func (want Runner) SatisfiedBy(have Runner) bool {
	return want == RunnerAny || have == RunnerMock || want == have
}

// String makes Runner printable in the flag help and error messages that used
// to interpolate bare literals.
func (r Runner) String() string {
	if r == RunnerAny {
		return "any"
	}
	return string(r)
}
