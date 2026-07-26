package appcontainer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine"
)

// exitUsage is swift-argument-parser's EX_USAGE. `container images` (the
// Docker spelling) and `container machine ls --nosuchflag` both produce it, so
// it separates "this CLI build has no such subcommand or flag" from "it ran and
// failed" (exit 1) with no version parsing at all.
const exitUsage = 64

type driver struct{ c Commander }

// New returns a machine.Driver backed by Apple's `container` CLI.
func New(c Commander) machine.Driver { return driver{c: c} }

// run executes one `container …` invocation, capturing both streams
// separately, and translates the exit code into this package's error
// vocabulary.
func (d driver) run(ctx context.Context, argv ...string) (stdout, stderr []byte, code int, err error) {
	var so, se bytes.Buffer
	code, err = d.c.Run(ctx, argv, nil, nil, &so, &se)
	if err != nil {
		return so.Bytes(), se.Bytes(), code, fmt.Errorf("run `container %s`: %w", strings.Join(argv, " "), err)
	}
	if code == exitUsage {
		return so.Bytes(), se.Bytes(), code, fmt.Errorf("`container %s`: %w (%s)",
			strings.Join(argv, " "), machine.ErrUnsupported, firstLine(se.String()))
	}
	return so.Bytes(), se.Bytes(), code, nil
}

func (d driver) Runtime(ctx context.Context) (machine.Runtime, error) {
	out, se, code, err := d.run(ctx, "system", "version", "--format", "json")
	if err != nil {
		return machine.Runtime{}, err
	}
	if code != 0 {
		return machine.Runtime{}, fmt.Errorf("`container system version` exited %d: %s", code, firstLine(string(se)))
	}
	v, err := parseCLIVersion(out)
	if err != nil {
		return machine.Runtime{}, err
	}
	// Service liveness is a separate question from CLI version, and the answer
	// changes what the operator must do: an old CLI needs an upgrade, a stopped
	// service needs `container system start`.
	sout, _, scode, serr := d.run(ctx, "system", "status", "--format", "json")
	running := serr == nil && scode == 0 && parseSystemStatus(sout)
	return machine.Runtime{CLIVersion: v, ServiceRunning: running}, nil
}

func (d driver) Inspect(ctx context.Context, name string) (machine.Info, error) {
	if !machine.ValidName(name) {
		return machine.Info{}, fmt.Errorf("invalid machine name %q", name)
	}
	// The name is ALWAYS passed: a bare `container machine inspect` describes
	// the DEFAULT machine, which on a developer's Mac is somebody else's.
	out, se, code, err := d.run(ctx, "machine", "inspect", name)
	if err != nil {
		return machine.Info{}, err
	}
	if code != 0 {
		if isNotFound(string(se)) {
			return machine.Info{}, machine.ErrNotFound
		}
		return machine.Info{}, fmt.Errorf("`container machine inspect %s` exited %d: %s", name, code, firstLine(string(se)))
	}
	return parseMachineInspect(out, name)
}

func (d driver) InspectContainer(ctx context.Context, cid string) (machine.ContainerInfo, error) {
	if strings.TrimSpace(cid) == "" {
		return machine.ContainerInfo{}, fmt.Errorf("no container id (read it fresh from `machine inspect`; it is <name>-<n>, not the machine name)")
	}
	out, se, code, err := d.run(ctx, "inspect", cid)
	if err != nil {
		return machine.ContainerInfo{}, err
	}
	if code != 0 {
		if isNotFound(string(se)) {
			return machine.ContainerInfo{}, machine.ErrNotFound
		}
		return machine.ContainerInfo{}, fmt.Errorf("`container inspect %s` exited %d: %s", cid, code, firstLine(string(se)))
	}
	return parseContainerInspect(out, cid)
}

func (d driver) ImageExists(ctx context.Context, ref string) (bool, error) {
	// `image inspect`, never `image ls` — see doc.go. Exit 0/1 is the whole
	// answer; the JSON is not even parsed.
	_, se, code, err := d.run(ctx, "image", "inspect", ref)
	if err != nil {
		return false, err
	}
	if code == 0 {
		return true, nil
	}
	if isNotFound(string(se)) || strings.Contains(se2lower(se), "image not found") {
		return false, nil
	}
	// A non-zero exit that is not "no such image" is a broken content store or
	// a stopped service, and reporting it as "absent" would make the next step
	// rebuild an image that is already there (or fail confusingly).
	return false, fmt.Errorf("`container image inspect %s` exited %d: %s", ref, code, firstLine(string(se)))
}

func (d driver) BuildImage(ctx context.Context, s machine.BuildSpec, out io.Writer) error {
	argv := []string{"build", "--arch", orDefault(s.Arch, "arm64")}
	if s.CPUs > 0 {
		argv = append(argv, "--cpus", itoa(s.CPUs))
	}
	if s.MemoryGB > 0 {
		argv = append(argv, "--memory", itoa(s.MemoryGB)+"G")
	}
	argv = append(argv, "--file", s.File, "--tag", s.Tag)
	// Sorted so the command line — and therefore the golden test pinning it —
	// does not shuffle with Go's map iteration order.
	keys := make([]string, 0, len(s.BuildArgs))
	for k := range s.BuildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "--build-arg", k+"="+s.BuildArgs[k])
	}
	argv = append(argv, s.ContextDir)

	// `container build` has no structured output at all, so the progress text
	// is streamed straight through to the caller's writer.
	var se bytes.Buffer
	if out == nil {
		out = io.Discard
	}
	code, err := d.c.Run(ctx, argv, nil, nil, out, io.MultiWriter(out, &se))
	if err != nil {
		return fmt.Errorf("run `container build`: %w", err)
	}
	if code == exitUsage {
		return fmt.Errorf("`container build`: %w (%s)", machine.ErrUnsupported, firstLine(se.String()))
	}
	if code != 0 {
		return fmt.Errorf("`container build --tag %s` exited %d: %s", s.Tag, code, firstLine(se.String()))
	}
	return nil
}

func (d driver) Create(ctx context.Context, s machine.Spec) error {
	if !machine.ValidName(s.Name) {
		return fmt.Errorf("invalid machine name %q", s.Name)
	}
	argv := []string{"machine", "create"}
	if s.Virtualization {
		// NOT optional and NOT the default: `container system property list`
		// reports `[machine] virtualization = false`, so a machine created
		// without this boots fine, never gets /dev/kvm, and can never run
		// firecracker — while looking identical in `machine inspect`.
		argv = append(argv, "--virtualization")
	}
	if s.KernelPath != "" {
		argv = append(argv, "--kernel", s.KernelPath)
	}
	if s.CPUs > 0 {
		argv = append(argv, "--cpus", itoa(s.CPUs))
	}
	if s.MemoryGB > 0 {
		argv = append(argv, "--memory", itoa(s.MemoryGB)+"G")
	}
	if s.HomeMount != "" {
		argv = append(argv, "--home-mount", s.HomeMount)
	}
	argv = append(argv, "--name", s.Name, s.Image)
	_, se, code, err := d.run(ctx, argv...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("`container machine create %s` exited %d: %s", s.Name, code, firstLine(string(se)))
	}
	return nil
}

func (d driver) Start(ctx context.Context, name string) error {
	if !machine.ValidName(name) {
		return fmt.Errorf("invalid machine name %q", name)
	}
	// There is no `container machine start`. `machine run` boots the machine if
	// necessary, which is exactly what is wanted HERE — and exactly why it must
	// never be used as a state probe elsewhere.
	_, se, code, err := d.run(ctx, "machine", "run", "--name", name, "--root", "--", "/bin/true")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("`container machine run --name %s -- /bin/true` (boot) exited %d: %s", name, code, firstLine(string(se)))
	}
	return nil
}

func (d driver) Exec(ctx context.Context, s machine.ExecSpec) (machine.ExecResult, error) {
	if err := machine.ValidateExec(s); err != nil {
		return machine.ExecResult{}, err
	}
	nonce := machine.NewNonce()
	script := machine.WrapScript(s.Script, nonce)
	if len(script) > machine.MaxScriptBytes {
		// Refused before the process starts: a payload at or above ~192 KiB
		// deadlocks the CLI in a way that does not respond to SIGTERM, so there
		// is no recovery once it is sent.
		return machine.ExecResult{}, fmt.Errorf("%s: script is %d bytes, over the %d-byte stdin limit "+
			"(64 KiB and 128 KiB were measured byte-exact on Apple Container 1.1.0; 192 KiB and 1 MiB DEADLOCK "+
			"un-interruptibly). Send a smaller script, or have the machine fetch what it needs itself",
			s.Op, len(script), machine.MaxScriptBytes)
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = machine.DefaultExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Sorted so the command line is stable across runs (Go map order is not) and
	// the golden argv test has something to pin.
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var env []string
	for _, k := range keys {
		// `-e NAME` is inherit-by-name: the VALUE never touches the command
		// line, so it survives spaces, `$`, `;` and quotes byte-exact (measured).
		env = append(env, k+"="+s.Env[k])
	}
	argv := buildExecArgv(s.Machine, keys)

	var so, se bytes.Buffer
	var stdout io.Writer = &so
	if s.Stream != nil {
		stdout = io.MultiWriter(&so, &streamFilter{w: s.Stream, nonce: nonce})
	}
	code, err := d.c.Run(ctx, argv, env, []byte(script), stdout, &se)
	if err != nil {
		return machine.ExecResult{}, fmt.Errorf("%s: run `container machine run`: %w", s.Op, err)
	}
	if code == exitUsage {
		return machine.ExecResult{}, fmt.Errorf("%s: %w — `container machine run` rejected its own arguments (%s); "+
			"sparkbox needs Apple Container >= 1.1.0", s.Op, machine.ErrUnsupported, firstLine(se.String()))
	}
	// "There is no such machine" must not be reported as a transport fault.
	// The CLI answers a missing machine with `failed to boot container machine
	// (cause: "notFound: …")` and a non-zero exit, and the guest shell of course
	// never acknowledged anything — so without this the receipt check would
	// blame stdin for a machine that simply is not there, and every diagnostic
	// downstream would be about the wrong problem. Narrow on purpose: only when
	// the script demonstrably never started (no `begin`) does the CLI's own
	// stderr get to decide.
	if code != 0 {
		if _, r := machine.StripReceipt(so.Bytes(), nonce); !r.Begin && isNotFound(se.String()) {
			return machine.ExecResult{}, fmt.Errorf("%s: machine %q: %w", s.Op, s.Machine, machine.ErrNotFound)
		}
	}
	return machine.Verdict(s.Op, nonce, code, so.Bytes(), se.Bytes())
}

// buildExecArgv is THE blessed exec command line, assembled here and nowhere
// else so the golden test that pins it has a single subject. Every word is a
// literal except the machine name, which ValidateExec has already restricted to
// [a-z0-9-] — which is what makes the bash -c join that happens inside the
// machine provably a no-op on this command line.
func buildExecArgv(name string, envKeys []string) []string {
	argv := []string{"machine", "run", "-i", "--root", "--name", name}
	for _, k := range envKeys {
		argv = append(argv, "-e", k)
	}
	// -t is deliberately absent: it allocates a pty even when stdout is a pipe,
	// which merges the two streams and rewrites every LF as CRLF.
	return append(argv, "--", "bash", "-s")
}

// streamFilter relays guest output live while dropping the receipt lines, so an
// operator watching a ten-minute inner setup never sees the protocol.
type streamFilter struct {
	w     io.Writer
	nonce string
	buf   []byte
}

func (f *streamFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		line := f.buf[:i+1]
		f.buf = f.buf[i+1:]
		if strings.Contains(string(line), f.nonce) {
			continue
		}
		if _, err := f.w.Write(line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// isNotFound recognises the CLI's miss text. Both spellings appear:
// `failed to inspect container machine (cause: "notFound: …")` for a machine and
// `image not found: no/such:image` for an image.
func isNotFound(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "notfound") || strings.Contains(s, "not found")
}

func se2lower(b []byte) string { return strings.ToLower(string(b)) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
