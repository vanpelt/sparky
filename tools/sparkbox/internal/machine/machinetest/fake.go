// Package machinetest provides an in-memory machine.Driver so the whole darwin
// provisioning pipeline is testable with no Apple Container, no VM and no Mac —
// which is what lets it run on a linux CI runner under -race.
//
// It is an ordinary (non _test.go) package on purpose: internal/hostsetup's
// tests import it, and a fake that lived in machine's own test files could not
// be reached from there.
package machinetest

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine"
)

// Outcome is a canned answer for one ExecSpec.Op.
//
// Keying on Op rather than on the script text is deliberate: a fake keyed on
// multi-line bash would be a bash interpreter, and every script edit would
// break every test for no reason. What stops the fake from HIDING an edited
// script is the golden-file suite in internal/hostsetup/testdata/scripts, which
// compares the exact bodies.
type Outcome struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// Err, when set, is returned verbatim instead of deriving one from
	// ExitCode — for modelling a transport fault (machine.ErrTransport) rather
	// than a script that failed.
	Err error
	// Apply runs after a successful call so a happy-path row can model the real
	// consequence: the bootstrap populating release.env, the inner setup
	// installing a binary.
	Apply func(*FakeDriver)
}

// FakeDriver is an in-memory machine.Driver.
type FakeDriver struct {
	mu sync.Mutex

	RuntimeInfo machine.Runtime
	RuntimeErr  error

	// Machines is the world: name -> Info. A name that is absent is
	// machine.ErrNotFound.
	Machines   map[string]machine.Info
	Containers map[string]machine.ContainerInfo
	Images     map[string]bool

	// Execs answers by Op; ExecSeq answers by Op with a SEQUENCE (the last
	// entry repeats), for the cases where two identical calls must disagree —
	// the crash-loop liveness probe samples `systemctl show` twice and the
	// entire question it asks is whether the samples differ.
	Execs   map[string]Outcome
	ExecSeq map[string][]Outcome
	// ExecDefault answers any Op with no entry. Nil means an unknown Op is a
	// test failure surfaced as an error, which is what you want: a silently
	// zero-valued answer would let a step "succeed" against a fake that never
	// heard of it.
	ExecDefault *Outcome

	CreateErr error
	StartErr  error
	BuildErr  error

	// Calls is the ordered log of everything that happened, for asserting both
	// the sequence and the absence of destructive calls.
	Calls []string
	// Specs records what Create was asked for, so a golden test can pin the
	// machine shape (cpus, memory, kernel, home-mount, virtualization).
	Specs []machine.Spec
	Execd []machine.ExecSpec
	// Builds records BuildImage requests.
	Builds []machine.BuildSpec

	seqN map[string]int
}

// New returns a FakeDriver with a running runtime and empty world.
func New() *FakeDriver {
	return &FakeDriver{
		RuntimeInfo: machine.Runtime{CLIVersion: "1.1.0", ServiceRunning: true},
		Machines:    map[string]machine.Info{},
		Containers:  map[string]machine.ContainerInfo{},
		Images:      map[string]bool{},
		Execs:       map[string]Outcome{},
		ExecSeq:     map[string][]Outcome{},
		seqN:        map[string]int{},
	}
}

func (f *FakeDriver) log(format string, a ...any) {
	f.Calls = append(f.Calls, fmt.Sprintf(format, a...))
}

// CallLog returns the ordered call log as one newline-joined string, for a
// readable assertion failure.
func (f *FakeDriver) CallLog() string { return strings.Join(f.Calls, "\n") }

// Mutations returns just the calls that changed something, so a dry-run test
// can assert emptiness without listing every read.
func (f *FakeDriver) Mutations() []string {
	var out []string
	for _, c := range f.Calls {
		switch {
		case strings.HasPrefix(c, "create "),
			strings.HasPrefix(c, "start "),
			strings.HasPrefix(c, "build "),
			strings.HasPrefix(c, "exec:"):
			out = append(out, c)
		}
	}
	return out
}

func (f *FakeDriver) Runtime(context.Context) (machine.Runtime, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("runtime")
	return f.RuntimeInfo, f.RuntimeErr
}

func (f *FakeDriver) Inspect(_ context.Context, name string) (machine.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("inspect %s", name)
	info, ok := f.Machines[name]
	if !ok {
		return machine.Info{}, machine.ErrNotFound
	}
	return info, nil
}

func (f *FakeDriver) InspectContainer(_ context.Context, cid string) (machine.ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("inspect-container %s", cid)
	ci, ok := f.Containers[cid]
	if !ok {
		return machine.ContainerInfo{}, machine.ErrNotFound
	}
	return ci, nil
}

func (f *FakeDriver) ImageExists(_ context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("image-inspect %s", ref)
	return f.Images[ref], nil
}

func (f *FakeDriver) BuildImage(_ context.Context, s machine.BuildSpec, out io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("build %s", s.Tag)
	f.Builds = append(f.Builds, s)
	if f.BuildErr != nil {
		return f.BuildErr
	}
	if out != nil {
		fmt.Fprintf(out, "fake build of %s from %s\n", s.Tag, s.ContextDir)
	}
	f.Images[s.Tag] = true
	return nil
}

func (f *FakeDriver) Create(_ context.Context, s machine.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("create %s", s.Name)
	f.Specs = append(f.Specs, s)
	if f.CreateErr != nil {
		return f.CreateErr
	}
	cid := s.Name + "-1000"
	f.Machines[s.Name] = machine.Info{
		Name: s.Name, ContainerID: cid, ImageRef: s.Image, HomeMount: s.HomeMount,
		IPAddress: "192.168.64.9", State: machine.StateRunning,
		CPUs: s.CPUs, MemoryBytes: uint64(s.MemoryGB) << 30,
	}
	f.Containers[cid] = machine.ContainerInfo{Virtualization: s.Virtualization, State: "running"}
	return nil
}

func (f *FakeDriver) Start(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("start %s", name)
	if f.StartErr != nil {
		return f.StartErr
	}
	info, ok := f.Machines[name]
	if !ok {
		return machine.ErrNotFound
	}
	info.State = machine.StateRunning
	f.Machines[name] = info
	if ci, ok := f.Containers[info.ContainerID]; ok {
		ci.State = "running"
		f.Containers[info.ContainerID] = ci
	}
	return nil
}

func (f *FakeDriver) Exec(_ context.Context, s machine.ExecSpec) (machine.ExecResult, error) {
	if err := machine.ValidateExec(s); err != nil {
		return machine.ExecResult{}, err
	}
	f.mu.Lock()
	f.log("exec:%s", s.Op)
	f.Execd = append(f.Execd, s)
	o, ok := f.outcomeLocked(s.Op)
	f.mu.Unlock()
	if !ok {
		return machine.ExecResult{}, fmt.Errorf("machinetest: no canned outcome for op %q (known: %s)",
			s.Op, strings.Join(f.knownOps(), ", "))
	}
	if s.Stream != nil && o.Stdout != "" {
		_, _ = io.WriteString(s.Stream, o.Stdout)
	}
	res := machine.ExecResult{ExitCode: o.ExitCode, Stdout: []byte(o.Stdout), Stderr: []byte(o.Stderr)}
	if o.Err != nil {
		return res, o.Err
	}
	if o.ExitCode != 0 {
		// Same contract as the real driver: any non-zero inner exit is an
		// error, so `if err != nil { return err }` is the safe reflex.
		return res, &machine.ExitError{Op: s.Op, Code: o.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}
	}
	if o.Apply != nil {
		f.mu.Lock()
		o.Apply(f)
		f.mu.Unlock()
	}
	return res, nil
}

func (f *FakeDriver) outcomeLocked(op string) (Outcome, bool) {
	if seq, ok := f.ExecSeq[op]; ok && len(seq) > 0 {
		i := f.seqN[op]
		if i >= len(seq) {
			i = len(seq) - 1
		}
		f.seqN[op]++
		return seq[i], true
	}
	if o, ok := f.Execs[op]; ok {
		return o, true
	}
	if f.ExecDefault != nil {
		return *f.ExecDefault, true
	}
	return Outcome{}, false
}

func (f *FakeDriver) knownOps() []string {
	var ops []string
	for op := range f.Execs {
		ops = append(ops, op)
	}
	for op := range f.ExecSeq {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	return ops
}

// SetExec is a convenience for a successful op with the given stdout.
func (f *FakeDriver) SetExec(op, stdout string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Execs[op] = Outcome{Stdout: stdout}
}

var _ machine.Driver = (*FakeDriver)(nil)
