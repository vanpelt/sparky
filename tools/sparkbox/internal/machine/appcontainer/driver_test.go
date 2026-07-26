package appcontainer

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine"
)

// call is one recorded invocation of the `container` binary.
type call struct {
	argv  []string
	env   []string
	stdin string
}

// fakeCommander answers from a table keyed by the joined argv, and records
// everything. No `container` binary is involved, which is why this suite runs
// on a linux CI machine.
type fakeCommander struct {
	// replies maps a joined argv prefix to a canned result. Lookup is by exact
	// join first, then by longest matching prefix, so a test can answer
	// "machine inspect" without repeating the name.
	replies map[string]reply
	calls   []call
}

type reply struct {
	stdout string
	stderr string
	code   int
	err    error
}

func (f *fakeCommander) Run(_ context.Context, argv []string, env []string, stdin []byte, stdout, stderr io.Writer) (int, error) {
	f.calls = append(f.calls, call{argv: slices.Clone(argv), env: slices.Clone(env), stdin: string(stdin)})
	key := strings.Join(argv, " ")
	r, ok := f.replies[key]
	if !ok {
		for k, v := range f.replies {
			if strings.HasPrefix(key, k) {
				r, ok = v, true
				break
			}
		}
	}
	if !ok {
		return 1, nil
	}
	if r.stdout != "" {
		_, _ = io.WriteString(stdout, r.stdout)
	}
	if r.stderr != "" {
		_, _ = io.WriteString(stderr, r.stderr)
	}
	return r.code, r.err
}

func (f *fakeCommander) lastArgv() []string {
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1].argv
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInspect(t *testing.T) {
	fc := &fakeCommander{replies: map[string]reply{
		"machine inspect sparkbox-poc": {stdout: fixture(t, "machine-inspect.json")},
	}}
	d := New(fc)
	info, err := d.Inspect(context.Background(), "sparkbox-poc")
	if err != nil {
		t.Fatal(err)
	}
	// Every field below is read off a document captured verbatim from a live
	// Apple Container 1.1.0, so the parser is tested against what the CLI
	// prints rather than what it ought to.
	if info.Name != "sparkbox-poc" {
		t.Errorf("Name = %q", info.Name)
	}
	if info.ContainerID != "sparkbox-poc-109421" {
		t.Errorf("ContainerID = %q (it is <name>-<n>, never the machine name)", info.ContainerID)
	}
	if info.ImageRef != "local/sparkbox-gateway:macos-poc" {
		t.Errorf("ImageRef = %q", info.ImageRef)
	}
	if info.HomeMount != "none" {
		t.Errorf("HomeMount = %q", info.HomeMount)
	}
	if info.State != machine.StateRunning {
		t.Errorf("State = %q (the JSON key is \"status\"; the table column is STATE)", info.State)
	}
	if info.IPAddress != "192.168.64.18" || info.CPUs != 8 {
		t.Errorf("info = %+v", info)
	}
	// The id must ALWAYS be on the command line: a bare `machine inspect`
	// silently describes the DEFAULT machine.
	if got := fc.lastArgv(); !slices.Contains(got, "sparkbox-poc") {
		t.Errorf("argv %v does not name the machine", got)
	}
}

func TestInspectNotFound(t *testing.T) {
	fc := &fakeCommander{replies: map[string]reply{
		"machine inspect gone": {code: 1, stderr: fixture(t, "notfound-machine.txt")},
	}}
	if _, err := New(fc).Inspect(context.Background(), "gone"); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUsageExitMapsToUnsupported(t *testing.T) {
	// Exit 64 is EX_USAGE: this CLI build has no such subcommand or flag. It is
	// version-skew detection with no version string to parse, and it must not
	// be confused with exit 1 ("it ran and failed").
	fc := &fakeCommander{replies: map[string]reply{
		"machine inspect sparkbox": {code: 64, stderr: fixture(t, "usage-64.txt")},
	}}
	_, err := New(fc).Inspect(context.Background(), "sparkbox")
	if !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if errors.Is(err, machine.ErrNotFound) {
		t.Error("a usage error must not read as \"machine does not exist\"")
	}
}

func TestInspectContainerReadsVirtualization(t *testing.T) {
	fc := &fakeCommander{replies: map[string]reply{
		"inspect sparkbox-poc-109421": {stdout: fixture(t, "container-inspect.json")},
	}}
	ci, err := New(fc).InspectContainer(context.Background(), "sparkbox-poc-109421")
	if err != nil {
		t.Fatal(err)
	}
	// This is the ONLY supported readback for nested virtualization: `machine
	// inspect` reports neither it nor the kernel path, and the system default
	// is virtualization = false.
	if !ci.Virtualization {
		t.Error("Virtualization = false on a machine created with --virtualization")
	}
	if ci.State != "running" {
		t.Errorf("State = %q", ci.State)
	}
}

func TestImageExistsNeverUsesImageLs(t *testing.T) {
	fc := &fakeCommander{replies: map[string]reply{
		"image inspect local/sparkbox-gateway:abc": {stdout: "[]"},
		"image inspect no/such:image":              {code: 1, stderr: fixture(t, "notfound-image.txt")},
		// If the driver ever reaches for `image ls`, it gets the failure that
		// is live on the target Mac today: one bad content blob takes the whole
		// listing down in table AND --format json.
		"image ls": {code: 1, stderr: fixture(t, "image-ls-broken.txt")},
	}}
	d := New(fc)
	ok, err := d.ImageExists(context.Background(), "local/sparkbox-gateway:abc")
	if err != nil || !ok {
		t.Fatalf("present image: ok=%v err=%v", ok, err)
	}
	ok, err = d.ImageExists(context.Background(), "no/such:image")
	if err != nil || ok {
		t.Fatalf("absent image: ok=%v err=%v", ok, err)
	}
	for _, c := range fc.calls {
		if len(c.argv) >= 2 && c.argv[0] == "image" && (c.argv[1] == "ls" || c.argv[1] == "list") {
			t.Fatalf("driver used `image %s`, which is broken on real hosts; use `image inspect`", c.argv[1])
		}
	}
}

func TestRuntimeSelectsTheCLINotTheAPIServer(t *testing.T) {
	fc := &fakeCommander{replies: map[string]reply{
		"system version --format json": {stdout: fixture(t, "system-version.json")},
		"system status --format json":  {stdout: fixture(t, "system-status.json")},
	}}
	rt, err := New(fc).Runtime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The same array formats container-apiserver's "version" as prose
	// ("container-apiserver version 1.1.0 (build: …)"); only the element with
	// appName == "container" carries a clean number.
	if rt.CLIVersion != "1.1.0" {
		t.Errorf("CLIVersion = %q, want a clean 1.1.0 (did it read the apiserver element?)", rt.CLIVersion)
	}
	if !rt.ServiceRunning {
		t.Error("ServiceRunning = false on a status document that says running")
	}
}

func TestRuntimeReportsAStoppedService(t *testing.T) {
	fc := &fakeCommander{replies: map[string]reply{
		"system version --format json": {stdout: fixture(t, "system-version.json")},
		"system status --format json":  {code: 1, stderr: "Error: no such file or directory\n"},
	}}
	rt, err := New(fc).Runtime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rt.ServiceRunning {
		t.Error("ServiceRunning = true although `system status` failed")
	}
}

// TestCreateArgvGolden pins the exact machine-create command line, so a flag
// drift shows up as a diff rather than as a machine that boots without KVM.
func TestCreateArgvGolden(t *testing.T) {
	fc := &fakeCommander{replies: map[string]reply{"machine create": {}}}
	err := New(fc).Create(context.Background(), machine.Spec{
		Name: "sparkbox", Image: "local/sparkbox-gateway:abc123def456",
		KernelPath: "/Users/x/Library/Application Support/sparkbox/vmlinux-macos-arm64",
		HomeMount:  "none", CPUs: 8, MemoryGB: 24, Virtualization: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"machine", "create", "--virtualization",
		"--kernel", "/Users/x/Library/Application Support/sparkbox/vmlinux-macos-arm64",
		"--cpus", "8", "--memory", "24G", "--home-mount", "none",
		"--name", "sparkbox", "local/sparkbox-gateway:abc123def456",
	}
	if got := fc.lastArgv(); !slices.Equal(got, want) {
		t.Errorf("create argv:\n got %q\nwant %q", got, want)
	}
}

// TestExecArgvIsTheBlessedLiteral is the guard on the transport itself.
func TestExecArgvIsTheBlessedLiteral(t *testing.T) {
	var seen call
	fc := &fakeCommander{replies: map[string]reply{}}
	fc.replies["machine run"] = reply{} // filled in below via the closure trick
	d := New(fc)

	// The fake cannot compute the nonce, so answer with whatever the driver
	// sent: echo the receipt back by reading the stdin it recorded. Simplest
	// way is a second commander that inspects stdin.
	echo := &receiptEchoCommander{inner: fc}
	d = New(echo)

	res, err := d.Exec(context.Background(), machine.ExecSpec{
		Machine: "sparkbox", Op: "probe", Script: "echo hello",
		Env: map[string]string{"SPARKBOX_B": "b value", "SPARKBOX_A": "a;echo INJECT"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "hello\n" {
		t.Errorf("stdout = %q, want the body's output with the receipt stripped", res.Stdout)
	}
	seen = fc.calls[0]

	want := []string{
		"machine", "run", "-i", "--root", "--name", "sparkbox",
		"-e", "SPARKBOX_A", "-e", "SPARKBOX_B", // sorted, so the line is stable
		"--", "bash", "-s",
	}
	if !slices.Equal(seen.argv, want) {
		t.Errorf("exec argv:\n got %q\nwant %q", seen.argv, want)
	}
	// -t allocates a pty even over a pipe, merging the streams and rewriting LF
	// as CRLF. It must never appear.
	if slices.Contains(seen.argv, "-t") || slices.Contains(seen.argv, "--tty") {
		t.Error("exec argv carries -t")
	}
	// The script rides stdin, never argv, and never gets re-parsed by the
	// guest's bash -c join.
	if !strings.Contains(seen.stdin, "echo hello") {
		t.Errorf("script did not ride stdin: %q", seen.stdin)
	}
	if !strings.HasPrefix(seen.stdin, "set -euo pipefail\n") {
		t.Errorf("stdin does not start with the mandatory preamble: %q", seen.stdin)
	}
	for _, a := range seen.argv {
		if strings.Contains(a, "echo hello") || strings.Contains(a, "INJECT") {
			t.Errorf("argv carries script or value text: %q", a)
		}
	}
	// Values travel by name in the child environment, byte-exact.
	if !slices.Contains(seen.env, "SPARKBOX_A=a;echo INJECT") {
		t.Errorf("env = %q, want the hostile value verbatim", seen.env)
	}
}

// receiptEchoCommander delegates recording to an inner fake but synthesises a
// well-formed receipt, since only the driver knows the per-call nonce.
type receiptEchoCommander struct {
	inner    *fakeCommander
	stdout   string
	exitCode int
}

func (c *receiptEchoCommander) Run(ctx context.Context, argv []string, env []string, stdin []byte, stdout, stderr io.Writer) (int, error) {
	_, _ = c.inner.Run(ctx, argv, env, stdin, io.Discard, io.Discard)
	nonce := nonceFromScript(string(stdin))
	body := c.stdout
	if body == "" {
		body = "hello\n"
	}
	_, _ = io.WriteString(stdout, nonce+" begin\n"+body+"\n"+nonce+" rc="+itoa(c.exitCode)+"\n")
	return c.exitCode, nil
}

// nonceFromScript pulls the receipt marker back out of the wrapped script, the
// way a real guest shell would print it.
func nonceFromScript(script string) string {
	const marker = "printf '%s begin\\n' '"
	i := strings.Index(script, marker)
	if i < 0 {
		return ""
	}
	rest := script[i+len(marker):]
	j := strings.IndexByte(rest, '\'')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestExecRefusesAnOversizeScript proves the cap is enforced BEFORE anything is
// spawned — the deadlock it prevents is not interruptible.
func TestExecRefusesAnOversizeScript(t *testing.T) {
	fc := &fakeCommander{replies: map[string]reply{}}
	_, err := New(fc).Exec(context.Background(), machine.ExecSpec{
		Machine: "sparkbox", Op: "huge", Script: strings.Repeat("x", 200<<10),
	})
	if err == nil || !strings.Contains(err.Error(), "stdin limit") {
		t.Fatalf("err = %v, want a refusal naming the stdin limit", err)
	}
	if len(fc.calls) != 0 {
		t.Fatal("the oversize script reached the process; the deadlock it causes cannot be killed")
	}
}

// TestExecTransportFailureIsNotSuccess is the F7 guard at the adapter level.
func TestExecTransportFailureIsNotSuccess(t *testing.T) {
	// A commander that exits 0 and prints nothing is exactly what the CLI does
	// when -i is missing: stdin is silently discarded and the guest runs
	// nothing.
	fc := &fakeCommander{replies: map[string]reply{"machine run": {code: 0}}}
	_, err := New(fc).Exec(context.Background(), machine.ExecSpec{
		Machine: "sparkbox", Op: "inner-setup", Script: "true",
	})
	if !errors.Is(err, machine.ErrTransport) {
		t.Fatalf("err = %v, want ErrTransport", err)
	}
}

func TestExecPropagatesTheInnerExitCode(t *testing.T) {
	echo := &receiptEchoCommander{inner: &fakeCommander{replies: map[string]reply{}}, exitCode: 2, stdout: "boom\n"}
	_, err := New(echo).Exec(context.Background(), machine.ExecSpec{
		Machine: "sparkbox", Op: "inner-setup", Script: "false",
	})
	var ee *machine.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *machine.ExitError", err)
	}
	if ee.Code != 2 {
		t.Errorf("code = %d, want 2", ee.Code)
	}
}

func TestStreamFilterHidesTheReceipt(t *testing.T) {
	var sb strings.Builder
	echo := &receiptEchoCommander{inner: &fakeCommander{replies: map[string]reply{}}, stdout: "step one\nstep two\n"}
	if _, err := New(echo).Exec(context.Background(), machine.ExecSpec{
		Machine: "sparkbox", Op: "inner-setup", Script: "true", Stream: &sb,
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "begin") || strings.Contains(sb.String(), "rc=") {
		t.Errorf("the operator saw the protocol:\n%s", sb.String())
	}
	if !strings.Contains(sb.String(), "step one") {
		t.Errorf("the operator saw no output at all:\n%s", sb.String())
	}
}

// TestExecOnAMissingMachineIsNotFoundNotTransport: the CLI answers a missing
// machine with `failed to boot container machine (cause: "notFound: …")` and a
// non-zero exit, and the guest shell of course never acknowledged anything.
// Without the narrow mapping, the receipt check would blame stdin for a machine
// that simply is not there, and every diagnostic downstream would be about the
// wrong problem.
func TestExecOnAMissingMachineIsNotFoundNotTransport(t *testing.T) {
	fc := &fakeCommander{replies: map[string]reply{
		"machine run": {code: 1, stderr: `Error: failed to boot container machine (cause: "notFound: \"container machine with ID sparkbox not found\"")` + "\n"},
	}}
	_, err := New(fc).Exec(context.Background(), machine.ExecSpec{
		Machine: "sparkbox", Op: "probe", Script: "true", ReadOnly: true,
	})
	if !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if errors.Is(err, machine.ErrTransport) {
		t.Error("an absent machine must not read as a transport fault")
	}
}

// ...but a script that legitimately prints "not found" and fails must still be
// an honest inner failure, because it produced a receipt.
func TestExecKeepsAnHonestFailureThatMentionsNotFound(t *testing.T) {
	echo := &receiptEchoCommander{
		inner:    &fakeCommander{replies: map[string]reply{}},
		exitCode: 1,
		stdout:   "cat: /etc/nope: not found\n",
	}
	_, err := New(echo).Exec(context.Background(), machine.ExecSpec{
		Machine: "sparkbox", Op: "probe", Script: "cat /etc/nope",
	})
	var ee *machine.ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("err = %v, want an ExitError with code 1", err)
	}
}
