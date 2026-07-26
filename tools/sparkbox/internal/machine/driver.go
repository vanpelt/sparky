package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// State is a machine's lifecycle state as the runtime reports it.
type State string

const (
	StateMissing State = "missing"
	StateStopped State = "stopped"
	StateRunning State = "running"
	StateUnknown State = "unknown"
)

// Info is the machine view of a nested VM.
type Info struct {
	Name        string
	ContainerID string // the container-level id, e.g. "sparkbox-1234"; NOT the name
	ImageRef    string
	HomeMount   string // "none" | "ro" | "rw"
	IPAddress   string
	State       State
	CPUs        int
	MemoryBytes uint64
}

// ContainerInfo is the CONTAINER view of a machine, reached by Info.ContainerID.
//
// It exists for exactly one field. `container machine inspect` reports neither
// virtualization nor the kernel path — its whole key set is containerId, cpus,
// createdDate, diskSize, homeMount, id, image, ipAddress, memory, platform,
// startedDate, status, userSetup — while `[machine] virtualization = false` is
// the system default. So a machine created without --virtualization boots
// perfectly, can never run firecracker, and is indistinguishable at the machine
// level from one that can. The container document carries `virtualization`, and
// that is the only supported readback.
type ContainerInfo struct {
	Virtualization bool
	State          string
}

// Spec is a machine to create.
type Spec struct {
	Name           string
	Image          string
	KernelPath     string
	HomeMount      string // "none" — paired with gateway-verify.sh's "no /Users virtiofs mount" assertion
	CPUs           int
	MemoryGB       int
	Virtualization bool
}

// BuildSpec is an image build.
type BuildSpec struct {
	ContextDir string
	File       string
	Tag        string
	Arch       string
	BuildArgs  map[string]string
	CPUs       int
	MemoryGB   int
}

// Runtime describes the host-side container runtime itself.
type Runtime struct {
	CLIVersion     string // "1.1.0"
	ServiceRunning bool
}

// ExecSpec is the ONLY way to run anything inside a machine.
//
// There is deliberately no argv field. `container machine run` joins argv with
// single spaces and evaluates the result as `/bin/bash -c`, so any
// caller-supplied word is live shell text — `$`, `;`, `|`, `*`, quotes and
// backslashes all get a second interpretation. Script bodies ride stdin (where
// nothing re-parses them); scalar values ride Env (inherit-by-name, measured
// byte-exact against a value containing spaces, `$HOME` and `;echo INJECT`);
// vectors ride a base64 NUL-joined blob (see argv.go).
type ExecSpec struct {
	Machine string
	// Op is a stable identifier for this call: it prefixes the log line, names
	// the evidence file, and is the key a test fake answers on. Keep it stable
	// across edits to Script — a fake keyed on script text would be a bash
	// interpreter, and golden files (not the fake) are what catch script edits.
	Op string
	// Script is the BODY only. WrapScript adds the preamble and the receipt, so
	// a call site cannot forget `set -euo pipefail` — see exec.go.
	Script string
	// Env keys must match ^[A-Z][A-Z0-9_]*$. Values are visible in
	// /proc/<pid>/environ inside the guest, which is fine for a public key and
	// wrong for anything genuinely secret.
	Env map[string]string
	// ReadOnly documents that this call only observes. DryRun refuses every
	// Exec regardless, read-only included: a dry run must be instant and must
	// boot nothing.
	ReadOnly bool
	Timeout  time.Duration // zero => DefaultExecTimeout
	// Stream, when set, receives the guest's output live, with the receipt
	// lines filtered out. The inner setup downloads multi-GB artifacts; ten
	// silent minutes is indistinguishable from the deadlock described in doc.go.
	Stream io.Writer
}

// ExecResult is a completed guest run. Stdout has the receipt lines removed.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Driver is the seam every darwin provisioning step talks to. The production
// implementation shells out to Apple's `container` CLI (machine/appcontainer);
// tests use machinetest.FakeDriver, which is why the whole darwin pipeline is
// testable on a linux CI runner with no Apple Container anywhere.
type Driver interface {
	Runtime(ctx context.Context) (Runtime, error)
	// Inspect returns ErrNotFound when no machine of that name exists.
	Inspect(ctx context.Context, name string) (Info, error)
	InspectContainer(ctx context.Context, containerID string) (ContainerInfo, error)
	ImageExists(ctx context.Context, ref string) (bool, error)
	BuildImage(ctx context.Context, s BuildSpec, out io.Writer) error
	// Create creates AND boots the machine (there is no separate start).
	Create(ctx context.Context, s Spec) error
	Start(ctx context.Context, name string) error
	Exec(ctx context.Context, s ExecSpec) (ExecResult, error)
}

var (
	ErrNotFound     = errors.New("machine: not found")
	ErrNotAvailable = errors.New("machine: runtime service not available")
	// ErrUnsupported is what an EX_USAGE (64) exit maps to: the CLI build in
	// front of us has no such subcommand or flag. Free version-skew detection
	// with no version string to parse.
	ErrUnsupported = errors.New("machine: unsupported by this CLI build")
	// ErrTransport means the machine did not run our script — NOT that the
	// script failed. See exec.go's receipt protocol.
	ErrTransport = errors.New("machine: transport did not deliver the script")
	ErrDryRun    = errors.New("machine: refused in --dry-run")
)

// ExitError is returned from Exec for ANY non-zero inner exit, so the default
// Go reflex (`if err != nil { return err }`) is the safe one. A caller that
// legitimately tolerates a non-zero code has to say so with errors.As — which
// is exactly the asymmetry F7 needed and did not have.
type ExitError struct {
	Op     string
	Code   int
	Stdout []byte
	Stderr []byte
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("%s: exited %d inside the machine", e.Op, e.Code)
}

// nameRe is the machine-name rule. It is not cosmetic: because argv words are
// re-parsed by bash inside the machine (see doc.go), restricting the one
// caller-supplied word that reaches that command line to [a-z0-9-] is what
// makes the literal-argv exec provably free of shell metacharacters.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidName reports whether s is a usable machine name.
func ValidName(s string) bool { return nameRe.MatchString(s) }
