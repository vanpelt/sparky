package hostsetup

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine/machinetest"
	macosassets "github.com/vanpelt/sparky/tools/sparkbox/macos"
)

// Every test in this file runs on ANY platform — a linux CI runner included —
// because a Mac is described entirely by a fakeProbe{goos:"darwin"} and a
// machinetest.FakeDriver. Nothing here shells out, and `container` is never
// invoked. That is the whole reason the machine.Driver seam exists, and it is
// also why none of these files is named *_darwin.go: Go's implicit filename
// build constraint would silently delete this suite from a linux build.

const (
	testRelease       = "v9.9.9"
	testOuterKernel   = "outer kernel bytes\n"
	testMachineBinary = "machine sparkbox bytes\n"
)

// darwinManifest is the manifest-darwin-arm64.env this suite serves. It repeats
// the linux guest keys (a Mac provisions exactly those artifacts into its
// machine) and adds the two darwin-only ones.
func darwinManifest() string {
	return "RELEASE=" + testRelease + "\n" +
		"PLATFORM=darwin\n" +
		"ARCH=arm64\n" +
		"ROOTFS_NAME=universal\n" +
		"ROOTFS_LOGIN_USER=sparky\n" +
		"OUTER_KERNEL_ASSET=vmlinux-macos-arm64\n" +
		"SHA256_OUTER_KERNEL=" + sha256Of(testOuterKernel) + "\n" +
		"MACHINE_SPARKBOX_ASSET=sparkbox-linux-arm64\n" +
		"SHA256_MACHINE_SPARKBOX=" + sha256Of(testMachineBinary) + "\n"
}

// releaseEnvBody is what macos/sparkbox-bootstrap.sh leaves behind, and what
// the Mac reads back to learn what actually got staged.
func releaseEnvBody(version, sha string) string {
	return "SOURCE=release\nRELEASE=" + version + "\nSHA256_SPARKBOX=" + sha +
		"\nVERSION=" + version + "\nBINARY=" + guestStagedBinary + "\n"
}

// darwinTestEnv builds an Env that looks exactly like a Mac with Apple
// Container installed and nothing provisioned yet.
func darwinTestEnv(t *testing.T, dry bool) (*Env, *machinetest.FakeDriver, *bytes.Buffer) {
	t.Helper()
	e, root := testEnv(t, dry)
	buf := &bytes.Buffer{}
	e.Log = buf
	// MacDir into the tempdir, for the same reason testEnv redirects BinPath:
	// left at its default the outer-kernel download and the evidence bundles
	// would land in the developer's real ~/Library.
	e.MacDir = filepath.Join(root, "mac")
	e.Cfg.MachineName = "sparkbox"
	e.Cfg.MachineCPUs = 8
	e.Cfg.MachineMemGB = 24
	e.Cfg.ContainerBin = "container"
	e.Cfg.OperatorKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f op@laptop"
	// ServiceSettle stays zero so the liveness probe still samples twice and
	// never waits.
	e.Cfg.ServiceSettle = 0

	e.Probe = fakeProbe{
		goos: "darwin", goarch: "arm64", uid: 501,
		sysctls: map[string]string{
			// 26.5.2 is deliberate: macOS jumped 15 -> 26, so this row is what
			// catches a lexical version comparison.
			"kern.osproductversion":    "26.5.2",
			"machdep.cpu.brand_string": "Apple M4 Max",
		},
		paths:    map[string]string{"container": "/usr/local/bin/container"},
		diskFree: 500 << 30,
	}
	e.Fetch = mapFetcher{
		ManifestURL("darwin", "arm64", e.Cfg.ArtifactBase, e.Cfg.Release):         darwinManifest(),
		e.Cfg.ArtifactBase + "/download/" + testRelease + "/vmlinux-macos-arm64":  testOuterKernel,
		e.Cfg.ArtifactBase + "/download/" + testRelease + "/sparkbox-linux-arm64": testMachineBinary,
	}

	fd := machinetest.New()
	seedGuest(fd)
	e.Machine = fd
	return e, fd, buf
}

// seedGuest cans every guest call a healthy provision + verify makes.
//
// Keyed by ExecSpec.Op, which is a stable identifier rather than script text —
// so an edit to a script body does not break these tables. What DOES catch such
// an edit is TestGuestScriptsMatchGolden.
func seedGuest(fd *machinetest.FakeDriver) {
	// A fresh machine has never been bootstrapped, so release.env is absent and
	// the script prints nothing. The bootstrap's Apply is what fills it in —
	// modelling the real consequence, so the second run of the same fake is
	// genuinely a no-op.
	fd.Execs["read-release-env"] = machinetest.Outcome{}
	fd.Execs["bootstrap"] = machinetest.Outcome{
		Stdout: "sparkbox-bootstrap: installed " + testRelease + "\n",
		Apply: func(f *machinetest.FakeDriver) {
			f.Execs["read-release-env"] = machinetest.Outcome{
				Stdout: releaseEnvBody(testRelease, sha256Of(testMachineBinary)),
			}
		},
	}
	fd.Execs["push-operator-key"] = machinetest.Outcome{}
	fd.Execs["remove-operator-key"] = machinetest.Outcome{}
	fd.Execs["inner-setup"] = machinetest.Outcome{Stdout: "== install-binary ==\n== enable-services ==\n"}
	fd.Execs["reconcile-binary"] = machinetest.Outcome{Stdout: "sparkbox " + testRelease + " (linux/arm64)\n"}
	fd.Execs["inner-doctor"] = machinetest.Outcome{Stdout: "  [PASS] sparkbox service  active (running), stable\n"}
	fd.Execs["guest-facts"] = machinetest.Outcome{
		Stdout: "kernel_release=6.14.9-sparkbox\nvirtiofs_targets=\ntun=present\n",
	}
	// The in-guest half of the verify pass, through machineProbe.
	fd.Execs["probe-stat:/dev/kvm"] = machinetest.Outcome{Stdout: "21b6 0\n"} // char device
	fd.Execs["probe-writable:/dev/kvm"] = machinetest.Outcome{}
	fd.Execs["probe-stat:"+defaultBinPath] = machinetest.Outcome{Stdout: "81ed 24000000\n"}
	fd.Execs["probe-run:"+showCmd] = machinetest.Outcome{
		Stdout: unitState("active", "running", "0", "800", "50000"),
	}
	fd.Execs["probe-run:systemctl show "+sluiceUnit+" --property="+serviceShowProps] = machinetest.Outcome{
		Stdout: "LoadState=not-found\nActiveState=inactive\n",
	}
	fd.Execs["probe-run:"+defaultBinPath+" version"] = machinetest.Outcome{
		Stdout: "sparkbox " + testRelease + " (linux/arm64)\n",
	}
	fd.Execs["probe-run:/proc/800/exe version"] = machinetest.Outcome{
		Stdout: "sparkbox " + testRelease + " (linux/arm64)\n",
	}
}

// --- dry run -----------------------------------------------------------------

func TestDarwinDryRunMutatesNothing(t *testing.T) {
	e, fd, buf := darwinTestEnv(t, true)
	if err := Provision(e); err != nil {
		t.Fatalf("dry-run Provision: %v", err)
	}
	out := buf.String()

	// Every darwin step must be named, in order — a plan an operator cannot
	// read is not a plan.
	for _, name := range []string{"resolve-release", "outer-kernel", "machine-image", "machine", "machine-sparkbox", "provision-inner"} {
		if !strings.Contains(out, name) {
			t.Errorf("plan is missing step %q:\n%s", name, out)
		}
	}
	// The three things a Mac operator most needs to see before committing.
	for _, want := range []string{
		e.Cfg.machineName(),
		e.Cfg.outerKernelPath(e.MacDir),
		guestStagedBinary + " setup ", // the EXACT inner command line
		"--operator-key",
		"skipping layout/role/port preflight",
		"dry run — nothing was changed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan is missing %q:\n%s", want, out)
		}
	}
	// Nothing mutating may have happened — not a build, not a create, not one
	// Exec. The DryRun wrapper enforces it; this asserts the enforcement holds
	// end to end.
	if m := fd.Mutations(); len(m) != 0 {
		t.Errorf("dry run performed mutations: %v", m)
	}
	if len(fd.Execd) != 0 {
		t.Errorf("dry run ran %d guest script(s); a plan must boot nothing", len(fd.Execd))
	}
	// And nothing on the Mac's own disk.
	if _, err := os.Stat(e.MacDir); err == nil {
		t.Errorf("dry run created %s", e.MacDir)
	}
	// The port preflight belongs to the machine, so the Mac must not bind.
	if fl, ok := e.Listen.(*fakeListener); ok && len(fl.calls) != 0 {
		t.Errorf("dry run bound ports on the Mac: %v", fl.calls)
	}
}

// --- the full happy path -----------------------------------------------------

func TestDarwinProvisionFromEmpty(t *testing.T) {
	e, fd, buf := darwinTestEnv(t, false)
	if err := Provision(e); err != nil {
		t.Fatalf("Provision: %v\n%s", err, buf.String())
	}

	// The ordered spine of a first run. Asserted as a SUBSEQUENCE (reads and
	// verify calls interleave) so the test pins the order that matters without
	// freezing every incidental lookup.
	wantOrder := []string{
		"image-inspect", "build", "create",
		"exec:read-release-env", "exec:bootstrap",
		"exec:push-operator-key", "exec:inner-setup",
		"exec:reconcile-binary",
		// The key removal is deferred, so it lands after the reconcile — but
		// still before Apply returns, which is the property that matters.
		"exec:remove-operator-key",
		"exec:inner-doctor",
	}
	assertSubsequence(t, fd.Calls, wantOrder)

	// The machine was created the ONLY way that can host firecracker.
	if len(fd.Specs) != 1 {
		t.Fatalf("expected exactly one machine create, got %d", len(fd.Specs))
	}
	spec := fd.Specs[0]
	if !spec.Virtualization {
		t.Error("machine created without --virtualization: it would boot fine and never get /dev/kvm")
	}
	if spec.HomeMount != "none" {
		t.Errorf("home-mount = %q, want none (this is what pairs with the no-/Users assertion)", spec.HomeMount)
	}
	if spec.KernelPath != e.Cfg.outerKernelPath(e.MacDir) {
		t.Errorf("kernel = %q, want the downloaded outer kernel", spec.KernelPath)
	}
	if spec.CPUs != 8 || spec.MemoryGB != 24 {
		t.Errorf("machine sized %d cpu / %d GiB", spec.CPUs, spec.MemoryGB)
	}

	// The outer kernel really landed, verified against the manifest checksum.
	got, err := os.ReadFile(e.Cfg.outerKernelPath(e.MacDir))
	if err != nil || string(got) != testOuterKernel {
		t.Errorf("outer kernel = %q, err %v", got, err)
	}
	// The bootstrap was pinned to the RESOLVED tag, never "latest".
	var boot machine.ExecSpec
	for _, s := range fd.Execd {
		if s.Op == "bootstrap" {
			boot = s
		}
	}
	if boot.Env["SPARKBOX_RELEASE_TAG"] != testRelease {
		t.Errorf("bootstrap tag = %q, want the resolved %s (never 'latest')", boot.Env["SPARKBOX_RELEASE_TAG"], testRelease)
	}
	// The banner names the machine and does NOT name journalctl on the Mac.
	out := buf.String()
	if !strings.Contains(out, "== sparkbox is provisioned (in machine sparkbox) ==") {
		t.Errorf("no darwin banner:\n%s", out)
	}
	if strings.Contains(out, "  logs:              journalctl -u sparkbox -f\n") {
		t.Error("the banner names a journald that does not exist on a Mac")
	}
	// Evidence is written for a successful run too.
	if e.EvidenceDir == "" {
		t.Fatal("no evidence directory")
	}
	for _, f := range []string{"bootstrap.txt", "inner-setup.txt", "inner-doctor.txt"} {
		if _, err := os.Stat(filepath.Join(e.EvidenceDir, f)); err != nil {
			t.Errorf("missing evidence %s: %v", f, err)
		}
	}
}

func TestDarwinSecondRunIsANoOp(t *testing.T) {
	e, fd, _ := darwinTestEnv(t, false)
	if err := Provision(e); err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	// A fresh Env against the SAME world: this is `sparkbox setup` run twice,
	// which is what an upgrade or a config change looks like.
	e2, _, buf2 := darwinTestEnv(t, false)
	e2.MacDir = e.MacDir
	e2.Machine = fd
	before := len(fd.Calls)
	fd.Calls = nil
	if err := Provision(e2); err != nil {
		t.Fatalf("second Provision: %v\n%s", err, buf2.String())
	}
	_ = before

	for _, forbidden := range []string{"build ", "create ", "start "} {
		for _, c := range fd.Calls {
			if strings.HasPrefix(c, forbidden) {
				t.Errorf("second run performed %q; the world had not changed", c)
			}
		}
	}
	// provision-inner is deliberately never satisfied — re-running the inner
	// setup is how a changed flag reaches an existing machine — but everything
	// before it must report itself already done.
	out := buf2.String()
	for _, name := range []string{"outer-kernel", "machine-image", "machine", "machine-sparkbox"} {
		if !strings.Contains(out, "== "+name+": already satisfied") {
			t.Errorf("step %q did not report itself satisfied on the second run:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "== provision-inner ==") {
		t.Errorf("provision-inner must always run (the inner setup is itself idempotent):\n%s", out)
	}
}

// --- THE regression test this workstream exists for --------------------------

// TestInnerSetupFailureIsNeverSuccess enumerates every way the transport or the
// inner command can go wrong, and demands that not one of them produces a green
// setup. Reporting success over an inner failure is F7 in a new place, and it is
// the single worst outcome this code can have.
func TestInnerSetupFailureIsNeverSuccess(t *testing.T) {
	tests := []struct {
		name    string
		outcome machinetest.Outcome
		wantErr string
	}{
		{
			name:    "inner setup exits 1",
			outcome: machinetest.Outcome{ExitCode: 1, Stdout: "preflight failed\n"},
			wantErr: "exit 1",
		},
		{
			name:    "inner setup exits 2",
			outcome: machinetest.Outcome{ExitCode: 2, Stdout: "provisioning completed but the host is not healthy\n"},
			wantErr: "exit 2",
		},
		{
			// bash's "binary not there": the staged binary vanished.
			name:    "inner binary missing (127)",
			outcome: machinetest.Outcome{ExitCode: 127},
			wantErr: "exit 127",
		},
		{
			// The measured "-i omitted" shape: the CLI exits 0 having silently
			// discarded stdin, so the guest ran nothing at all.
			name: "exit 0 with no receipt",
			outcome: machinetest.Outcome{
				Err: fmt.Errorf("inner-setup: %w — the guest shell never acknowledged the script (exit 0)", machine.ErrTransport),
			},
			wantErr: "did not run the inner setup at all",
		},
		{
			// Truncation or a signal: the script began and its exit trap never
			// fired, so the status is unknown.
			name: "began but no trailer",
			outcome: machinetest.Outcome{
				Err: fmt.Errorf("inner-setup: %w — the script started but its exit trap never fired", machine.ErrTransport),
			},
			wantErr: "did not run the inner setup at all",
		},
		{
			// The shell-join shape: the guest and the transport disagree about
			// what happened, so neither is trusted.
			name: "receipt rc disagrees with the process exit",
			outcome: machinetest.Outcome{
				Err: fmt.Errorf("inner-setup: %w — the guest reported status 7 but the transport reported 0", machine.ErrTransport),
			},
			wantErr: "did not run the inner setup at all",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, fd, buf := darwinTestEnv(t, false)
			fd.Execs["inner-setup"] = tc.outcome

			err := Provision(e)
			if err == nil {
				t.Fatalf("Provision returned nil over a failed inner setup:\n%s", buf.String())
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
			out := buf.String()
			if strings.Contains(out, "== sparkbox is provisioned") {
				t.Errorf("a green banner was printed over a failed inner setup:\n%s", out)
			}
			// The transcript still has to exist: whoever debugs this needs it,
			// and poc.sh's habit of returning before collecting anything is
			// exactly what made a failed provision opaque.
			if e.EvidenceDir == "" {
				t.Fatal("no evidence directory was created for the failed run")
			}
			if _, serr := os.Stat(filepath.Join(e.EvidenceDir, "inner-setup.txt")); serr != nil {
				t.Errorf("no transcript for the failed run: %v", serr)
			}
		})
	}
}

// TestOperatorKeyRemovedOnFailure: the key is pushed into the machine's /run and
// must be removed whatever happens — including on the path that returns an
// error, and before the caller sees it.
func TestOperatorKeyRemovedOnFailure(t *testing.T) {
	e, fd, _ := darwinTestEnv(t, false)
	fd.Execs["inner-setup"] = machinetest.Outcome{ExitCode: 1}
	if err := Provision(e); err == nil {
		t.Fatal("expected a failure")
	}
	if !slices.Contains(fd.Calls, "exec:remove-operator-key") {
		t.Errorf("the operator key was left inside the machine after a failed run:\n%s", fd.CallLog())
	}
}

// TestOperatorKeyNeverInArgv: the public key travels by env name, never on a
// command line the guest's bash would re-parse.
func TestOperatorKeyNeverInArgv(t *testing.T) {
	e, fd, _ := darwinTestEnv(t, false)
	if err := Provision(e); err != nil {
		t.Fatal(err)
	}
	keyBody := "AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"
	for _, s := range fd.Execd {
		if strings.Contains(s.Script, keyBody) {
			t.Errorf("op %q has the operator key inline in its script", s.Op)
		}
		if s.Op == "push-operator-key" {
			if !strings.Contains(s.Env["SPARKBOX_OPERATOR_KEY"], keyBody) {
				t.Error("push-operator-key does not carry the key in its env")
			}
			continue
		}
		for k, v := range s.Env {
			if strings.Contains(v, keyBody) {
				t.Errorf("op %q leaks the operator key through env %s", s.Op, k)
			}
		}
	}
	// And the inner setup names the PATH, not the key.
	for _, s := range fd.Execd {
		if s.Op != "inner-setup" {
			continue
		}
		argv, err := machine.DecodeArgv(s.Env["SPARKBOX_ARGV_B64"])
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(argv, guestOperatorKey) {
			t.Errorf("inner argv %q does not name the staged key path", argv)
		}
		for _, a := range argv {
			if strings.Contains(a, keyBody) {
				t.Errorf("inner argv carries the key text: %q", a)
			}
		}
	}
}

// --- verify ------------------------------------------------------------------

func TestVerifyRelaysInnerDoctorFailure(t *testing.T) {
	e, fd, buf := darwinTestEnv(t, false)
	fd.Execs["inner-doctor"] = machinetest.Outcome{
		ExitCode: 1,
		Stdout:   "  [FAIL] rootfs template  no rootfs template at /srv/sparkbox/data/images/universal.ext4\n",
	}
	err := Provision(e)
	if err == nil {
		t.Fatalf("a failing inner doctor must fail the darwin run:\n%s", buf.String())
	}
	out := buf.String()
	if strings.Contains(out, "== sparkbox is provisioned") {
		t.Error("banner printed over a failing gateway")
	}
	// Its own words, relayed — not re-said in ours.
	if !strings.Contains(out, "no rootfs template at") {
		t.Errorf("the machine's own report was not relayed:\n%s", out)
	}
}

// TestGreenSetupOverCrashLoopingGuestFails is F7's original fix, applied one
// layer out: the inner setup succeeds, and A1's two-sample liveness probe —
// running INSIDE the machine through machineProbe — catches the crash loop.
func TestGreenSetupOverCrashLoopingGuestFails(t *testing.T) {
	e, fd, buf := darwinTestEnv(t, false)
	// NRestarts climbing between the two samples is the whole signal. It works
	// only because machineProbe never memoises Run.
	fd.ExecSeq["probe-run:"+showCmd] = []machinetest.Outcome{
		{Stdout: unitState("active", "running", "3", "800", "50000")},
		{Stdout: unitState("active", "running", "5", "930", "70000")},
	}
	fd.Execs["probe-run:journalctl -u "+serviceUnit+" -n 20 --no-pager"] = machinetest.Outcome{
		Stdout: "sparkbox: listen tcp 127.0.0.1:8079: bind: address already in use\n",
	}
	err := Provision(e)
	if err == nil {
		t.Fatalf("a crash-looping gateway inside the machine must fail the run:\n%s", buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "crash-looping") {
		t.Errorf("the report should name the crash loop:\n%s", out)
	}
	if !strings.Contains(out, "bind: address already in use") {
		t.Errorf("the guest's journal should be inlined:\n%s", out)
	}
	if strings.Contains(out, "== sparkbox is provisioned") {
		t.Error("banner printed over a crash-looping gateway")
	}
}

// --- the machine step --------------------------------------------------------

func TestUnownedMachineRefused(t *testing.T) {
	tests := []struct {
		name string
		info machine.Info
	}{
		{"foreign image", machine.Info{Name: "sparkbox", ImageRef: "docker.io/library/ubuntu:24.04", HomeMount: "none", State: machine.StateRunning}},
		// home-mount rw means the operator's files are inside the VM, which is
		// a different security posture, not a cosmetic difference.
		{"home mounted", machine.Info{Name: "sparkbox", ImageRef: machineImageRepo + ":abc", HomeMount: "rw", State: machine.StateRunning}},
		{"wrong id", machine.Info{Name: "somebody-else", ImageRef: machineImageRepo + ":abc", HomeMount: "none", State: machine.StateRunning}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, fd, buf := darwinTestEnv(t, false)
			fd.Machines["sparkbox"] = tc.info
			err := Provision(e)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			// Never auto-delete, and never create over it: the machine may hold
			// somebody's running sandboxes.
			for _, c := range fd.Calls {
				if strings.HasPrefix(c, "delete ") || strings.HasPrefix(c, "create ") {
					t.Errorf("setup touched a machine it does not own: %q", c)
				}
			}
			// The refusal happens in the preflight battery, so the way forward
			// is in the report rather than in the returned error.
			out := buf.String()
			if !strings.Contains(out, "--machine-name") || !strings.Contains(out, "machine delete") {
				t.Errorf("the refusal must name a way forward:\n%s", out)
			}
		})
	}
}

func TestStoppedMachineIsStartedNotRecreated(t *testing.T) {
	e, fd, _ := darwinTestEnv(t, false)
	fd.Images[machineImageRef(e.Cfg)] = true
	fd.Machines["sparkbox"] = machine.Info{
		Name: "sparkbox", ContainerID: "sparkbox-77", ImageRef: machineImageRef(e.Cfg),
		HomeMount: "none", State: machine.StateStopped,
	}
	fd.Containers["sparkbox-77"] = machine.ContainerInfo{Virtualization: true, State: "stopped"}
	if err := Provision(e); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !slices.Contains(fd.Calls, "start sparkbox") {
		t.Errorf("a stopped machine must be started:\n%s", fd.CallLog())
	}
	for _, c := range fd.Calls {
		if strings.HasPrefix(c, "create ") {
			t.Error("a stopped machine was recreated, which would have destroyed its state")
		}
	}
}

// TestProbingNeverBoots: `container machine run` boots a stopped machine, so a
// state probe that used it would change what it measures. The machine step's
// Satisfied must not exec, and must not start.
func TestProbingNeverBoots(t *testing.T) {
	e, fd, _ := darwinTestEnv(t, false)
	fd.Machines["sparkbox"] = machine.Info{
		Name: "sparkbox", ContainerID: "sparkbox-77", ImageRef: machineImageRef(e.Cfg),
		HomeMount: "none", State: machine.StateStopped,
	}
	s := stepMachine()
	if _, _, err := s.Satisfied(e); err != nil {
		t.Fatal(err)
	}
	_ = s.Plan(e)
	for _, c := range fd.Calls {
		if strings.HasPrefix(c, "exec:") || strings.HasPrefix(c, "start ") {
			t.Errorf("probing the machine ran %q, which boots it", c)
		}
	}
}

// --- outer kernel ------------------------------------------------------------

func TestOuterKernelSatisfiedRules(t *testing.T) {
	tests := []struct {
		name     string
		onDisk   string // "" means absent
		manifest Manifest
		want     bool
		wantNote string
	}{
		{name: "absent", manifest: Manifest{Release: testRelease, SHA256OuterKernel: sha256Of(testOuterKernel)}},
		{
			name: "present and matching", onDisk: testOuterKernel,
			manifest: Manifest{Release: testRelease, SHA256OuterKernel: sha256Of(testOuterKernel)},
			want:     true, wantNote: "matches " + testRelease,
		},
		{
			// A previous release's kernel is not this release's kernel, and a
			// mere existence check is how every other stale artifact survived
			// an upgrade.
			name: "present but stale", onDisk: "some older kernel\n",
			manifest: Manifest{Release: testRelease, SHA256OuterKernel: sha256Of(testOuterKernel)},
		},
		{
			// --dry-run: nothing resolved the manifest, so say so rather than
			// implying a checksum comparison happened.
			name: "present, release unresolved", onDisk: testOuterKernel,
			manifest: Manifest{},
			want:     true, wantNote: "checksum unchecked",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := darwinTestEnv(t, false)
			e.Manifest = tc.manifest
			path := e.Cfg.outerKernelPath(e.MacDir)
			if tc.onDisk != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.onDisk), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			sat, note, err := stepOuterKernel().Satisfied(e)
			if err != nil {
				t.Fatal(err)
			}
			if sat != tc.want {
				t.Errorf("satisfied = %v, want %v (note %q)", sat, tc.want, note)
			}
			if tc.wantNote != "" && !strings.Contains(note, tc.wantNote) {
				t.Errorf("note = %q, want it to contain %q", note, tc.wantNote)
			}
		})
	}
}

// TestOuterKernelRefusesWhenTheReleaseHasNone: a fact read out of the manifest,
// never a URL we invented — this is the kernel the hypervisor boots.
func TestOuterKernelRefusesWhenTheReleaseHasNone(t *testing.T) {
	e, _, _ := darwinTestEnv(t, false)
	e.Manifest = Manifest{Release: "v0.3.0"} // pre-B2: no OUTER_KERNEL_ASSET
	err := stepOuterKernel().Apply(e)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"v0.3.0", "OUTER_KERNEL_ASSET", "--outer-kernel"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q: %v", want, err)
		}
	}
}

// --- the machine's binary ----------------------------------------------------

func TestMachineSparkboxCrossCheck(t *testing.T) {
	tests := []struct {
		name       string
		releaseEnv string
		wantErr    string
	}{
		{
			name:       "version skew",
			releaseEnv: releaseEnvBody("v0.3.0", sha256Of(testMachineBinary)),
			wantErr:    "staged sparkbox v0.3.0 but this Mac pinned release " + testRelease,
		},
		{
			name:       "checksum skew",
			releaseEnv: releaseEnvBody(testRelease, sha256Of("something else")),
			wantErr:    "the two manifests disagree",
		},
		{
			name:       "nothing staged",
			releaseEnv: "",
			wantErr:    "is empty or unreadable after the bootstrap ran",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, fd, _ := darwinTestEnv(t, false)
			e.Manifest, _ = ParseManifest(strings.NewReader(darwinManifest()), testRelease)
			fd.Machines["sparkbox"] = machine.Info{
				Name: "sparkbox", ContainerID: "sparkbox-1", ImageRef: machineImageRef(e.Cfg),
				HomeMount: "none", State: machine.StateRunning,
			}
			fd.Execs["bootstrap"] = machinetest.Outcome{
				Apply: func(f *machinetest.FakeDriver) {
					f.Execs["read-release-env"] = machinetest.Outcome{Stdout: tc.releaseEnv}
				},
			}
			err := stepMachineSparkbox().Apply(e)
			if err == nil {
				t.Fatal("expected the cross-check to refuse")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestReconcileBinaryCatchesTheF0Skew: the check whose absence let a v0.3.0
// binary drive v0.4.0's artifacts and report PASS.
func TestReconcileBinaryCatchesTheF0Skew(t *testing.T) {
	e, fd, _ := darwinTestEnv(t, false)
	fd.Execs["reconcile-binary"] = machinetest.Outcome{Stdout: "sparkbox v0.3.0 (linux/arm64)\n"}
	err := Provision(e)
	if err == nil {
		t.Fatal("expected the version skew to fail the run")
	}
	if !strings.Contains(err.Error(), "v0.3.0") || !strings.Contains(err.Error(), testRelease) {
		t.Errorf("the error must name both versions: %v", err)
	}
	if !strings.Contains(err.Error(), "machine delete") {
		t.Errorf("the error must name the way out: %v", err)
	}
}

// --- inner argv --------------------------------------------------------------

func TestInnerSetupArgs(t *testing.T) {
	base := func() Config {
		c := DefaultConfig()
		c.ProxyDomain = "example.test"
		c.DataVolumeGB = 300
		c.FlagsGiven = map[string]bool{}
		return c
	}
	tests := []struct {
		name      string
		cfg       func(Config) Config
		wantHas   []string
		wantLacks []string
		wantErr   string
		wantPairs [][2]string
	}{
		{
			name:    "standalone gateway",
			cfg:     func(c Config) Config { return c },
			wantHas: []string{"setup", "--release", testRelease, "--operator-handle", "operator"},
			// The key PATH inside the machine, never the key text.
			wantPairs: [][2]string{{"--operator-key", guestOperatorKey}, {"--swap-gb", "0"}},
			wantLacks: []string{"--gateway", "--node-name"},
		},
		{
			// The upgrade run that used to rewrite a live machine's domain.
			// --proxy-domain always carries a value (the compiled-in
			// hivemind.tools if nothing else), and the inner setup rebuilds
			// FlagsGiven from ITS argv — so emitting it unconditionally made
			// every plain `sparkbox setup` re-run count as "the operator named
			// the domain" and rewrite PROXY_DOMAIN in the machine's live
			// sparkbox.env. Untyped means absent.
			name:      "an untyped --proxy-domain is not forwarded",
			cfg:       func(c Config) Config { return c },
			wantLacks: []string{"--proxy-domain", "example.test"},
		},
		{
			name: "a typed --proxy-domain is forwarded",
			cfg: func(c Config) Config {
				c.FlagsGiven["proxy-domain"] = true
				return c
			},
			wantPairs: [][2]string{{"--proxy-domain", "example.test"}},
		},
		{
			// The one-command fleet node: --gateway and --node-name have to
			// reach the inner setup or a Mac can never be a node.
			name: "fleet node",
			cfg: func(c Config) Config {
				c.Gateway = "gw.example.com:2222"
				c.NodeName = "laptop"
				return c
			},
			wantPairs: [][2]string{{"--gateway", "gw.example.com:2222"}, {"--node-name", "laptop"}},
			// A node holds no accounts, so demanding an operator key would turn
			// a good run into a demand for something never used.
			wantLacks: []string{"--operator-handle", "--operator-key"},
		},
		{
			// "latest" must never reach the machine: a release published
			// mid-setup would straddle the Mac's kernel and the machine's rootfs.
			name: "latest is replaced by the resolved tag",
			cfg: func(c Config) Config {
				c.Release = "latest"
				return c
			},
			wantPairs: [][2]string{{"--release", testRelease}},
			wantLacks: []string{"latest"},
		},
		{
			name: "an explicit swap-gb wins over the darwin default",
			cfg: func(c Config) Config {
				c.SwapGB = 4
				c.FlagsGiven["swap-gb"] = true
				return c
			},
			wantPairs: [][2]string{{"--swap-gb", "4"}},
		},
		{
			// A TYPED ZERO is a value, not an absence. It reaches here as the
			// same int a defaulted flag would, so the only thing that can tell
			// them apart is FlagsGiven — and dropping this one would hand the
			// machine the inner default of 16 GiB, a swapfile inside a
			// thin-provisioned virtual disk that is charged twice. That is the
			// exact outcome the --swap-gb 0 default above exists to prevent, so
			// asking for it explicitly must not be the one way to lose it.
			name: "an explicit swap-gb 0 is forwarded, not swallowed as an absence",
			cfg: func(c Config) Config {
				c.SwapGB = 0
				c.FlagsGiven["swap-gb"] = true
				return c
			},
			wantPairs: [][2]string{{"--swap-gb", "0"}},
		},
		{
			// Same shape, the other int flag: 0 means "advertise the real listen
			// port", which is a choice an operator can make explicitly.
			name: "an explicit ssh-advertise-port 0 is forwarded",
			cfg: func(c Config) Config {
				c.SSHAdvertisePort = 0
				c.FlagsGiven["ssh-advertise-port"] = true
				return c
			},
			wantPairs: [][2]string{{"--ssh-advertise-port", "0"}},
		},
		{
			// FlagsGiven is what stops the Mac's compiled-in defaults from
			// overwriting a live machine's sparkbox.env on an upgrade run.
			name: "untyped subsystem flags are not forwarded",
			cfg: func(c Config) Config {
				c.ProxyAddr = ":8081" // the default, not typed
				return c
			},
			wantLacks: []string{"--proxy-addr"},
		},
		{
			name: "typed subsystem flags are forwarded",
			cfg: func(c Config) Config {
				c.Sluice = true
				c.ProxyTLS = true
				c.TLSEmail = "ops@example.test"
				c.EdgeIP = "10.66.0.1"
				c.FlagsGiven["sluice"] = true
				c.FlagsGiven["proxy-tls"] = true
				c.FlagsGiven["tls-email"] = true
				c.FlagsGiven["edge-ip"] = true
				return c
			},
			wantHas:   []string{"--sluice", "--proxy-tls"},
			wantPairs: [][2]string{{"--tls-email", "ops@example.test"}, {"--edge-ip", "10.66.0.1"}},
		},
		{
			name: "a refused flag is an error, not a silent drop",
			cfg: func(c Config) Config {
				c.MoveAdminSSH = true
				c.FlagsGiven["move-admin-ssh"] = true
				return c
			},
			wantErr: "--move-admin-ssh cannot be used on macOS",
		},
		{
			name: "--bin-path is refused too",
			cfg: func(c Config) Config {
				c.FlagsGiven["bin-path"] = true
				return c
			},
			wantErr: "--bin-path cannot be used on macOS",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, err := innerSetupArgs(tc.cfg(base()), testRelease, guestOperatorKey)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if args[0] != "setup" {
				t.Errorf("argv[0] = %q, want setup", args[0])
			}
			for _, w := range tc.wantHas {
				if !slices.Contains(args, w) {
					t.Errorf("argv %q is missing %q", args, w)
				}
			}
			for _, w := range tc.wantLacks {
				if slices.Contains(args, w) {
					t.Errorf("argv %q should not contain %q", args, w)
				}
			}
			for _, p := range tc.wantPairs {
				if !hasPair(args, p[0], p[1]) {
					t.Errorf("argv %q is missing the pair %q %q", args, p[0], p[1])
				}
			}
		})
	}
}

// TestGatewayPassthroughEndToEnd: a Mac provisioned as a fleet node in ONE
// command. Asserted through the whole pipeline, not just the pure function,
// because the flags have to survive the base64 argv codec too.
func TestGatewayPassthroughEndToEnd(t *testing.T) {
	e, fd, buf := darwinTestEnv(t, false)
	e.Cfg.Gateway = "gw.example.com:2222"
	e.Cfg.NodeName = "laptop"
	e.Cfg.FlagsGiven = map[string]bool{"gateway": true, "node-name": true}
	if err := Provision(e); err != nil {
		t.Fatalf("Provision: %v\n%s", err, buf.String())
	}
	var argv []string
	for _, s := range fd.Execd {
		if s.Op == "inner-setup" {
			var err error
			argv, err = machine.DecodeArgv(s.Env["SPARKBOX_ARGV_B64"])
			if err != nil {
				t.Fatal(err)
			}
			// The length integrity check the guest re-verifies.
			if s.Env["SPARKBOX_ARGV_N"] != fmt.Sprint(len(argv)) {
				t.Errorf("SPARKBOX_ARGV_N = %q but argv has %d words", s.Env["SPARKBOX_ARGV_N"], len(argv))
			}
		}
	}
	if !hasPair(argv, "--gateway", "gw.example.com:2222") || !hasPair(argv, "--node-name", "laptop") {
		t.Errorf("the fleet flags did not reach the machine: %q", argv)
	}
	// A node authenticates nobody, so no key is staged at all.
	if slices.Contains(fd.Calls, "exec:push-operator-key") {
		t.Error("an operator key was pushed into a fleet node")
	}
	if !strings.Contains(buf.String(), "fleet node is provisioned") {
		t.Errorf("the node banner is missing:\n%s", buf.String())
	}
}

// --- checks ------------------------------------------------------------------

func TestDarwinPreflightChecks(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Env, *machinetest.FakeDriver)
		check    string
		want     Status
		wantText string
	}{
		{name: "modern macOS passes", check: macLayer + "macos version", want: Pass},
		{
			name:  "macOS 14.7 fails",
			check: macLayer + "macos version", want: Fail,
			mutate: func(e *Env, _ *machinetest.FakeDriver) { setSysctl(e, "kern.osproductversion", "14.7") },
		},
		{
			// The row a lexical comparison gets wrong: macOS went 15 -> 26.
			name:  "macOS 26.5.2 clears a floor of 15",
			check: macLayer + "macos version", want: Pass,
			mutate: func(e *Env, _ *machinetest.FakeDriver) { setSysctl(e, "kern.osproductversion", "26.5.2") },
		},
		{name: "M4 passes", check: macLayer + "apple silicon", want: Pass},
		{
			name:  "M2 fails (no nested virtualization)",
			check: macLayer + "apple silicon", want: Fail,
			mutate: func(e *Env, _ *machinetest.FakeDriver) { setSysctl(e, "machdep.cpu.brand_string", "Apple M2 Pro") },
		},
		{
			name:  "an Intel Mac fails",
			check: macLayer + "apple silicon", want: Fail,
			mutate: func(e *Env, _ *machinetest.FakeDriver) {
				setSysctl(e, "machdep.cpu.brand_string", "Intel(R) Core(TM) i9-9880H CPU @ 2.30GHz")
			},
		},
		{name: "arm64 passes", check: macLayer + "cpu architecture", want: Pass},
		{
			name:  "amd64 fails outright, not merely warns",
			check: macLayer + "cpu architecture", want: Fail,
			mutate: func(e *Env, _ *machinetest.FakeDriver) {
				p := e.Probe.(fakeProbe)
				p.goarch = "amd64"
				e.Probe = p
			},
		},
		{name: "container 1.1.0 passes", check: macLayer + "apple container", want: Pass},
		{
			name:  "container 1.0.9 fails",
			check: macLayer + "apple container", want: Fail,
			mutate: func(_ *Env, fd *machinetest.FakeDriver) {
				fd.RuntimeInfo = machine.Runtime{CLIVersion: "1.0.9", ServiceRunning: true}
			},
			wantText: "1.1.0",
		},
		{
			name:  "an ErrUnsupported reads as too old",
			check: macLayer + "apple container", want: Fail,
			mutate:   func(_ *Env, fd *machinetest.FakeDriver) { fd.RuntimeErr = machine.ErrUnsupported },
			wantText: "too old",
		},
		{
			name:  "container not on PATH fails",
			check: macLayer + "apple container", want: Fail,
			mutate: func(e *Env, _ *machinetest.FakeDriver) {
				p := e.Probe.(fakeProbe)
				p.paths = map[string]string{}
				e.Probe = p
			},
		},
		{
			// poc.sh only WARNs here, so a doctor PASS there does not imply
			// `container machine …` will work at all.
			name:  "a stopped container service is a FAIL",
			check: macLayer + "container service", want: Fail,
			mutate: func(_ *Env, fd *machinetest.FakeDriver) {
				fd.RuntimeInfo = machine.Runtime{CLIVersion: "1.1.0", ServiceRunning: false}
			},
			wantText: "system start",
		},
		{name: "a free machine name passes", check: machineLayer + "name", want: Pass},
		{
			name:  "a foreign machine fails and names the way out",
			check: machineLayer + "name", want: Fail,
			mutate: func(_ *Env, fd *machinetest.FakeDriver) {
				fd.Machines["sparkbox"] = machine.Info{Name: "sparkbox", ImageRef: "ubuntu:24.04", HomeMount: "rw"}
			},
			wantText: "--machine-name",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, fd, _ := darwinTestEnv(t, false)
			if tc.mutate != nil {
				tc.mutate(e, fd)
			}
			results := RunChecks(e.Probe, e.Cfg, darwinPreflightChecks(e))
			var got *Result
			for i := range results {
				if results[i].Name == tc.check {
					got = &results[i]
				}
			}
			if got == nil {
				t.Fatalf("no check named %q in the darwin preflight battery", tc.check)
			}
			if got.Status != tc.want {
				t.Errorf("%s = %s (%s / %s), want %s", tc.check, got.Status, got.Detail, got.Hint, tc.want)
			}
			if tc.wantText != "" && !strings.Contains(got.Detail+" "+got.Hint, tc.wantText) {
				t.Errorf("%s should mention %q, got %q / %q", tc.check, tc.wantText, got.Detail, got.Hint)
			}
		})
	}
}

func setSysctl(e *Env, k, v string) {
	p := e.Probe.(fakeProbe)
	m := map[string]string{}
	for kk, vv := range p.sysctls {
		m[kk] = vv
	}
	m[k] = v
	p.sysctls = m
	e.Probe = p
}

// TestDarwinVerifyCatchesAMachineWithoutVirtualization: the failure `machine
// inspect` cannot see, because it reports neither virtualization nor the kernel
// path — and `virtualization = false` is the SYSTEM DEFAULT.
func TestDarwinVerifyCatchesAMachineWithoutVirtualization(t *testing.T) {
	e, fd, buf := darwinTestEnv(t, false)
	fd.Images[machineImageRef(e.Cfg)] = true
	fd.Machines["sparkbox"] = machine.Info{
		Name: "sparkbox", ContainerID: "sparkbox-5", ImageRef: machineImageRef(e.Cfg),
		HomeMount: "none", State: machine.StateRunning,
	}
	fd.Containers["sparkbox-5"] = machine.ContainerInfo{Virtualization: false, State: "running"}
	if err := Provision(e); err == nil {
		t.Fatalf("a machine without nested virtualization must fail verify:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "WITHOUT --virtualization") {
		t.Errorf("the report should name the cause:\n%s", buf.String())
	}
}

func TestDarwinVerifyCatchesAHomeMountedMachine(t *testing.T) {
	e, fd, buf := darwinTestEnv(t, false)
	fd.Execs["guest-facts"] = machinetest.Outcome{
		Stdout: "kernel_release=6.14.9\nvirtiofs_targets=/Users,/sbin.machine,\ntun=present\n",
	}
	if err := Provision(e); err == nil {
		t.Fatalf("a machine with /Users mounted must fail verify:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "home directory mounted") {
		t.Errorf("the report should name the cause:\n%s", buf.String())
	}
}

// --- machineProbe ------------------------------------------------------------

// TestMachineProbeMemoRules pins the subtle half: idempotent reads are cached,
// and Run is NOT — because the liveness probe deliberately issues the same
// command twice and a cache would collapse it into one, silently reinstating
// the crash-loop blindness A1 removed.
func TestMachineProbeMemoRules(t *testing.T) {
	fd := machinetest.New()
	fd.ExecDefault = &machinetest.Outcome{Stdout: "ok\n"}
	mp := newMachineProbe(t.Context(), fd, "sparkbox", "arm64")

	if _, err := mp.ReadFile("/etc/hostname"); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.ReadFile("/etc/hostname"); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(fd.Calls, "exec:probe-read:/etc/hostname"); n != 1 {
		t.Errorf("ReadFile twice made %d guest calls, want 1 (it is memoised)", n)
	}

	if _, err := mp.Run("systemctl", "show", "sparkbox.service"); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.Run("systemctl", "show", "sparkbox.service"); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(fd.Calls, "exec:probe-run:systemctl show sparkbox.service"); n != 2 {
		t.Errorf("Run twice made %d guest calls, want 2 (it must NEVER be memoised)", n)
	}
}

func TestMachineProbeRefusesQuotedWords(t *testing.T) {
	fd := machinetest.New()
	fd.ExecDefault = &machinetest.Outcome{}
	mp := newMachineProbe(t.Context(), fd, "sparkbox", "arm64")
	if _, err := mp.Run("echo", "it's"); err == nil {
		t.Error("a word containing a single quote must be refused, not spliced")
	}
}

func TestMachineProbeParsesGuestAnswers(t *testing.T) {
	fd := machinetest.New()
	fd.Execs["probe-stat:/srv"] = machinetest.Outcome{Stdout: "41ed 4096\n"} // directory
	fd.Execs["probe-df:/srv"] = machinetest.Outcome{Stdout: "/dev/loop0 26214400 1048576 25165824 5% /srv\n"}
	fd.Execs["probe-sysctl:net.ipv4.ip_forward"] = machinetest.Outcome{Stdout: "1\n"}
	fd.Execs["probe-lookpath:firecracker"] = machinetest.Outcome{Stdout: "/usr/local/bin/firecracker\n"}
	mp := newMachineProbe(t.Context(), fd, "sparkbox", "arm64")

	fi, err := mp.Stat("/srv")
	if err != nil || !fi.IsDir() {
		t.Errorf("Stat(/srv) = %v, %v; want a directory", fi, err)
	}
	free, err := mp.DiskFreeBytes("/srv")
	if err != nil || free != 25165824*1024 {
		t.Errorf("DiskFreeBytes = %d, %v", free, err)
	}
	if v, err := mp.Sysctl("net.ipv4.ip_forward"); err != nil || v != "1" {
		t.Errorf("Sysctl = %q, %v", v, err)
	}
	if p, err := mp.LookPath("firecracker"); err != nil || p != "/usr/local/bin/firecracker" {
		t.Errorf("LookPath = %q, %v", p, err)
	}
	// The machine is linux and we always exec as root.
	if mp.GOOS() != "linux" || mp.Uid() != 0 {
		t.Errorf("machineProbe reports %s/uid %d", mp.GOOS(), mp.Uid())
	}
}

// --- the linux path is untouched ---------------------------------------------

// TestLinuxStepsUnchanged pins the linux pipeline's names and order, so darwin
// work provably cannot perturb it.
func TestLinuxStepsUnchanged(t *testing.T) {
	want := []string{
		"swapfile", "resolve-release", "install-binary", "data-volume", "fetch-artifacts",
		"users.conf", "host-config", "net-rules", "sluice", "systemd-units", "admin-ssh", "enable-services",
	}
	var got []string
	for _, s := range allSteps() {
		got = append(got, s.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("allSteps():\n got %q\nwant %q", got, want)
	}
	// And stepsFor picks it for a linux probe.
	e, _ := testEnv(t, true)
	var dispatched []string
	for _, s := range stepsFor(e) {
		dispatched = append(dispatched, s.Name)
	}
	if !slices.Equal(dispatched, want) {
		t.Errorf("stepsFor(linux) = %q, want the linux pipeline", dispatched)
	}
}

func TestStepsForDarwin(t *testing.T) {
	e, _, _ := darwinTestEnv(t, true)
	var got []string
	for _, s := range stepsFor(e) {
		got = append(got, s.Name)
	}
	want := []string{"resolve-release", "outer-kernel", "machine-image", "machine", "machine-sparkbox", "provision-inner"}
	if !slices.Equal(got, want) {
		t.Errorf("stepsFor(darwin) = %q, want %q", got, want)
	}
}

// TestCheckOSAcceptsDarwin: the single line that used to make setup refuse to
// run on a Mac at step 5 of Provision, before any step ran.
func TestCheckOSAcceptsDarwin(t *testing.T) {
	tests := []struct {
		goos string
		want Status
	}{
		{"linux", Pass},
		{"darwin", Pass},
		{"windows", Fail},
	}
	for _, tc := range tests {
		t.Run(tc.goos, func(t *testing.T) {
			if got := checkOS(fakeProbe{goos: tc.goos}, Config{}); got.Status != tc.want {
				t.Errorf("checkOS(%s) = %s (%s), want %s", tc.goos, got.Status, got.Detail, tc.want)
			}
		})
	}
}

// --- golden guest scripts ----------------------------------------------------

// TestGuestScriptsMatchGolden is what stops the Op-keyed fake from hiding an
// edited script. These files are also the artefact a human reviews for quoting
// bugs, which is the failure mode the whole transport design is about.
func TestGuestScriptsMatchGolden(t *testing.T) {
	scripts := map[string]string{
		"read-release-env":    scriptReadReleaseEnv,
		"bootstrap":           scriptBootstrap,
		"push-operator-key":   scriptPushOperatorKey,
		"remove-operator-key": scriptRemoveOperatorKey,
		"inner-setup":         scriptInnerSetup,
		"reconcile-binary":    scriptReconcileBinary,
		"inner-journal":       scriptInnerJournal,
		"guest-facts":         scriptGuestFacts,
		"inner-doctor":        scriptInnerDoctor,
	}
	for name, body := range scripts {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("testdata", "scripts", name+".golden")
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (regenerate with -run TestGuestScriptsMatchGolden and copy the body)", err)
			}
			if string(want) != body {
				t.Errorf("%s drifted from its golden file:\n--- golden ---\n%s\n--- code ---\n%s", path, want, body)
			}
			// Nothing may reference a Go value: every script is a plain
			// constant, and values travel through ExecSpec.Env. A `+` splice
			// would show up here as text the golden file does not have, which
			// the comparison above already catches — this is the belt to that
			// braces, aimed at the one shape that would still look right:
			// a shell variable nobody sets.
			for _, ref := range envRefs(body) {
				if !strings.HasPrefix(ref, "SPARKBOX_") {
					t.Errorf("%s reads $%s, which is not one of the SPARKBOX_* values the Mac supplies", name, ref)
				}
			}
		})
	}
}

// --- helpers -----------------------------------------------------------------

func hasPair(argv []string, flag, val string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == val {
			return true
		}
	}
	return false
}

func countCalls(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

// assertSubsequence checks that want appears in got in order (matching by
// prefix, so "create" matches "create sparkbox"), allowing other calls in
// between — it pins the order that matters without freezing every read.
func assertSubsequence(t *testing.T, got, want []string) {
	t.Helper()
	i := 0
	for _, c := range got {
		if i < len(want) && strings.HasPrefix(c, want[i]) {
			i++
		}
	}
	if i != len(want) {
		t.Errorf("call log does not contain %q in order (stopped at %q):\n%s",
			want, want[min(i, len(want)-1)], strings.Join(got, "\n"))
	}
}

// TestMacosAssetsAreTheImageContext is a belt-and-braces check that the step
// really builds from the embedded files and nothing else.
func TestMacosAssetsAreTheImageContext(t *testing.T) {
	e, fd, _ := darwinTestEnv(t, false)
	if err := Provision(e); err != nil {
		t.Fatal(err)
	}
	if len(fd.Builds) != 1 {
		t.Fatalf("expected one build, got %d", len(fd.Builds))
	}
	b := fd.Builds[0]
	entries, err := os.ReadDir(b.ContextDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(macosassets.Files()) {
		t.Errorf("build context has %d files, want %d", len(entries), len(macosassets.Files()))
	}
	if b.BuildArgs["UBUNTU_IMAGE"] != macosassets.UbuntuImage {
		t.Errorf("UBUNTU_IMAGE = %q, want the pinned digest", b.BuildArgs["UBUNTU_IMAGE"])
	}
	// The tag is the content hash, which is what turns "is it current?" into a
	// content comparison.
	if !strings.HasPrefix(b.Tag, machineImageRepo+":") || len(b.Tag) != len(machineImageRepo)+13 {
		t.Errorf("tag = %q, want %s:<12 hex>", b.Tag, machineImageRepo)
	}
}

// envRefs finds the $NAME / ${NAME} references in a guest script, ignoring
// bash's own ($?, $$, ${#args[@]}, positional parameters).
func envRefs(body string) []string {
	var out []string
	for i := 0; i < len(body); i++ {
		if body[i] != '$' {
			continue
		}
		j := i + 1
		if j < len(body) && body[j] == '{' {
			j++
		}
		start := j
		for j < len(body) && (body[j] == '_' || (body[j] >= 'A' && body[j] <= 'Z') ||
			(body[j] >= 'a' && body[j] <= 'z') || (body[j] >= '0' && body[j] <= '9')) {
			j++
		}
		name := body[start:j]
		if name == "" || strings.ToUpper(name) != name {
			continue // bash internals and lower-case locals, e.g. ${#args[@]}
		}
		out = append(out, name)
		i = j
	}
	return out
}

// TestVerifySkipsGuestChecksWhenTheMachineIsDown: a battery that cries wolf
// teaches the operator to skim it. With no machine there is exactly one true
// thing to say, and saying it eight ways — including a reused /dev/kvm check
// blaming this Mac's BIOS — is worse than saying it once.
func TestVerifySkipsGuestChecksWhenTheMachineIsDown(t *testing.T) {
	e, fd, _ := darwinTestEnv(t, false)
	results := RunChecks(e.Probe, e.Cfg, darwinVerifyChecks(e))
	var names []string
	for _, r := range results {
		names = append(names, r.Name)
	}
	if slices.Contains(names, machineLayer+"kvm device") {
		t.Errorf("guest checks ran against a machine that does not exist: %q", names)
	}
	if !slices.Contains(names, machineLayer+"gateway") {
		t.Errorf("the one honest note is missing: %q", names)
	}
	// And nothing was executed inside a machine that is not there.
	for _, c := range fd.Calls {
		if strings.HasPrefix(c, "exec:") {
			t.Errorf("verify ran %q against an absent machine", c)
		}
	}
}

// TestAttachMachineDriverIsALinuxNoOp: the linux path must not grow a driver,
// a dry-run wrapper, or a validation it never had.
func TestAttachMachineDriverIsALinuxNoOp(t *testing.T) {
	e, _ := testEnv(t, false)
	// A machine name that would be refused on darwin.
	e.Cfg.MachineName = "NOT A VALID NAME"
	if err := AttachMachineDriver(e); err != nil {
		t.Fatalf("AttachMachineDriver must do nothing on linux, got %v", err)
	}
	if e.Machine != nil {
		t.Error("a linux Env grew a machine driver")
	}
}

// TestDarwinRefusesABadMachineName: the name is the one caller-supplied word on
// a command line the guest's bash re-parses, and an EMPTY one would address the
// DEFAULT machine — somebody else's VM.
func TestDarwinRefusesABadMachineName(t *testing.T) {
	for _, name := range []string{"Sparkbox", "spark;box", "spark box", "-leading-hyphen"} {
		t.Run(name, func(t *testing.T) {
			e, _, _ := darwinTestEnv(t, true)
			e.Cfg.MachineName = name
			if err := Provision(e); err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
}
