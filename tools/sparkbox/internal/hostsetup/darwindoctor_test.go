package hostsetup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine/machinetest"
)

// `sparkbox doctor` on a Mac, proved on any platform.
//
// Same rule as darwin_test.go: a Mac is a fakeProbe{goos:"darwin"} plus a
// machinetest.FakeDriver, so this whole suite runs on a linux CI runner with no
// Apple Container, no VM and no laptop — and the file is deliberately NOT named
// *_darwin.go, which would silently delete it from a linux build.

// doctorEnv is a Mac whose machine already exists, is ours, is running with
// nested virtualization, and whose outer kernel is on disk and matches the
// release. Every test below starts from health and breaks exactly one thing.
func doctorEnv(t *testing.T) (*Env, *machinetest.FakeDriver) {
	t.Helper()
	e, fd, _ := darwinTestEnv(t, false)
	startMachine(t, e, fd)
	writeOuterKernel(t, e, testOuterKernel)
	return e, fd
}

func startMachine(t *testing.T, e *Env, fd *machinetest.FakeDriver) {
	t.Helper()
	name := e.Cfg.machineName()
	cid := name + "-1000"
	fd.Images[machineImageRef(e.Cfg)] = true
	fd.Machines[name] = machine.Info{
		Name: name, ContainerID: cid, ImageRef: machineImageRef(e.Cfg), HomeMount: "none",
		IPAddress: "192.168.64.9", State: machine.StateRunning, CPUs: 8, MemoryBytes: 24 << 30,
	}
	fd.Containers[cid] = machine.ContainerInfo{Virtualization: true, State: "running"}
}

func writeOuterKernel(t *testing.T, e *Env, body string) {
	t.Helper()
	path := e.Cfg.outerKernelPath(e.MacDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pinRelease is `sparkbox doctor --release <tag>`: a concrete tag AND a
// manifest served at that tag's URL, since ManifestURL keys "latest" and a
// pinned tag differently (the first is GitHub's /latest/download redirect).
func pinRelease(t *testing.T, e *Env, tag string) {
	t.Helper()
	e.Cfg.Release = tag
	f, ok := e.Fetch.(mapFetcher)
	if !ok {
		t.Fatalf("fetcher is %T, want mapFetcher", e.Fetch)
	}
	f[ManifestURL("darwin", "arm64", e.Cfg.ArtifactBase, tag)] = darwinManifest()
}

func doctorReport(t *testing.T, e *Env) ([]Result, string) {
	t.Helper()
	results := RunChecks(e.Probe, e.Cfg, DoctorChecksFor(e))
	var b strings.Builder
	PrintResults(&b, results)
	return results, b.String()
}

// find returns the result whose name matches, failing the test if the battery
// never asked. "The check is missing" and "the check passed" must never be the
// same observation.
func find(t *testing.T, results []Result, name string) Result {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	var names []string
	for _, r := range results {
		names = append(names, r.Name)
	}
	t.Fatalf("no check named %q in the darwin doctor battery; it asked: %s", name, strings.Join(names, ", "))
	return Result{}
}

func execCalls(fd *machinetest.FakeDriver) []string {
	var out []string
	for _, c := range fd.Calls {
		if strings.HasPrefix(c, "exec:") {
			out = append(out, c)
		}
	}
	return out
}

// --- the healthy Mac ---------------------------------------------------------

// TestDarwinDoctorReportsBothLayers is the whole point of B5: one report, two
// hosts, and never any doubt about which host a line describes.
func TestDarwinDoctorReportsBothLayers(t *testing.T) {
	e, _ := doctorEnv(t)
	results, report := doctorReport(t, e)

	if AnyFail(results) {
		t.Fatalf("a healthy Mac + healthy machine must not FAIL:\n%s", report)
	}
	// The host layer — the questions poc.sh's doctor asked and the Go doctor
	// could not.
	for _, name := range []string{
		macLayer + "macos version", macLayer + "apple silicon", macLayer + "apple container",
		macLayer + "container service", macLayer + "disk space", macLayer + "outer kernel",
	} {
		if got := find(t, results, name); got.Status != Pass {
			t.Errorf("%s = %s (%s)", name, got.Status, got.Detail)
		}
	}
	// The machine layer, including the relay.
	for _, name := range []string{
		machineLayer + "state", machineLayer + "nested virtualization",
		machineLayer + "kvm device", machineLayer + "sparkbox service", machineLayer + "gateway doctor",
	} {
		if got := find(t, results, name); got.Status == Fail {
			t.Errorf("%s = FAIL (%s / %s)", name, got.Detail, got.Hint)
		}
	}
	// Every single line carries its layer: a report where "disk space" could
	// mean either host is a report that sends someone to free space on the
	// wrong machine.
	for _, r := range results {
		if !strings.HasPrefix(r.Name, macLayer) && !strings.HasPrefix(r.Name, machineLayer) {
			t.Errorf("check %q names no layer", r.Name)
		}
	}
	// The gateway's own words, under a banner saying whose words they are.
	inner := find(t, results, machineLayer+"gateway doctor")
	if !strings.Contains(inner.Output, "inside machine sparkbox") {
		t.Errorf("the relayed report is not labelled:\n%s", inner.Output)
	}
	if !strings.Contains(inner.Output, "sparkbox service  active (running), stable") {
		t.Errorf("the machine's own report was not relayed:\n%s", inner.Output)
	}
	if !strings.Contains(report, "│ ") {
		t.Errorf("the relayed report should be printed indented under its result:\n%s", report)
	}
}

// TestDarwinDoctorLeavesNoEvidenceOnAPassingRun: doctor is run casually and
// often, and a timestamped directory per invocation is litter. A FAILING run
// still writes one — that is the run whose transcript somebody wants.
func TestDarwinDoctorLeavesNoEvidenceOnAPassingRun(t *testing.T) {
	e, fd := doctorEnv(t)
	if _, report := doctorReport(t, e); AnyFail(nil) {
		t.Fatal(report)
	}
	if _, err := os.Stat(filepath.Join(e.MacDir, "results")); err == nil {
		t.Error("a passing doctor wrote an evidence directory")
	}

	fd.Execs["inner-doctor"] = machinetest.Outcome{ExitCode: 1, Stdout: "  [FAIL] kvm device  absent\n"}
	doctorReport(t, e)
	if _, err := os.Stat(filepath.Join(e.MacDir, "results")); err != nil {
		t.Error("a failing doctor kept no transcript")
	}
}

// --- no machine, stopped machine ---------------------------------------------

// TestDarwinDoctorWithNoMachine: the machine layer must say which of "does not
// exist" / "is stopped" / "could not be asked" happened, and must not run
// anything inside a machine that is not there.
func TestDarwinDoctorWithNoMachine(t *testing.T) {
	e, fd, _ := darwinTestEnv(t, false)
	writeOuterKernel(t, e, testOuterKernel)
	results, report := doctorReport(t, e)

	if !AnyFail(results) {
		t.Fatalf("a Mac with no machine has no gateway; that is a FAIL:\n%s", report)
	}
	state := find(t, results, machineLayer+"state")
	if state.Status != Fail || !strings.Contains(state.Detail, "does not exist") {
		t.Errorf("machine state = %s (%s), want a FAIL naming the absence", state.Status, state.Detail)
	}
	if !strings.Contains(state.Hint, "sparkbox setup") {
		t.Errorf("the FAIL must name the next command, got %q", state.Hint)
	}
	// One honest line instead of eight misleading ones — and NOT an empty
	// section that reads as health.
	note := find(t, results, machineLayer+"gateway")
	if note.Detail == "" || !strings.Contains(note.Detail, "does not exist") {
		t.Errorf("the gateway note must say why nothing was checked, got %q", note.Detail)
	}
	if strings.Contains(report, machineLayer+"gateway doctor") {
		t.Errorf("the inner doctor was reported against a machine that does not exist:\n%s", report)
	}
	if calls := execCalls(fd); len(calls) != 0 {
		t.Errorf("doctor executed %v inside an absent machine", calls)
	}
}

func TestDarwinDoctorWithAStoppedMachine(t *testing.T) {
	e, fd := doctorEnv(t)
	info := fd.Machines[e.Cfg.machineName()]
	info.State = machine.StateStopped
	fd.Machines[e.Cfg.machineName()] = info

	results, report := doctorReport(t, e)
	if !AnyFail(results) {
		t.Fatalf("a stopped machine means no gateway; that is a FAIL:\n%s", report)
	}
	state := find(t, results, machineLayer+"state")
	if state.Status != Fail || !strings.Contains(state.Detail, "stopped") {
		t.Errorf("machine state = %s (%s), want a FAIL naming the state", state.Status, state.Detail)
	}
	note := find(t, results, machineLayer+"gateway")
	if !strings.Contains(note.Detail, "is stopped") {
		t.Errorf("the gateway note must name the reason, got %q", note.Detail)
	}
	if !strings.Contains(note.Hint, "machine run") {
		t.Errorf("the note must name the command that starts it, got %q", note.Hint)
	}
	// A probe must not boot what it measures: `container machine run` starts a
	// stopped machine, so doctor asking the guest anything here would change
	// the very state it is reporting.
	if calls := execCalls(fd); len(calls) != 0 {
		t.Errorf("doctor executed %v inside a stopped machine (and would have booted it)", calls)
	}
	for _, c := range fd.Calls {
		if strings.HasPrefix(c, "start ") || strings.HasPrefix(c, "create ") {
			t.Errorf("doctor changed the machine it was measuring: %q\n%s", c, report)
		}
	}
}

// --- the relay ---------------------------------------------------------------

func TestDarwinDoctorRelaysAFailingGateway(t *testing.T) {
	e, fd := doctorEnv(t)
	fd.Execs["inner-doctor"] = machinetest.Outcome{
		ExitCode: 2,
		Stdout:   "  [FAIL] rootfs template  no rootfs template at /srv/sparkbox/data/images/universal.ext4\n",
	}
	results, report := doctorReport(t, e)
	if !AnyFail(results) {
		t.Fatalf("a failing gateway doctor must fail the darwin doctor:\n%s", report)
	}
	r := find(t, results, machineLayer+"gateway doctor")
	if r.Status != Fail {
		t.Fatalf("gateway doctor = %s, want FAIL", r.Status)
	}
	// Its own words, relayed rather than re-said in ours.
	if !strings.Contains(r.Output, "no rootfs template at") {
		t.Errorf("the machine's report was not relayed:\n%s", r.Output)
	}
	if !strings.Contains(r.Detail, "exit 2") {
		t.Errorf("the inner exit status was not honoured, got %q", r.Detail)
	}
	// The exit code is a STATUS, not a tally. `sparkbox doctor` exits 1 for any
	// number of failing checks (and 2 for a flag-parse failure, which is what
	// this row models), so a summary that read the code as a count would tell
	// the operator "2 failing check(s)" about a gateway with one — or six.
	if strings.Contains(r.Detail, "check(s)") || strings.Contains(r.Detail, "2 failing") {
		t.Errorf("the summary reports the exit code as a count of checks: %q", r.Detail)
	}
}

// TestInnerDoctorIsNeverSilentlyEmpty is the F7 lesson applied to a relay: an
// empty section that prints as a PASS says "the gateway is healthy" when it
// means "nobody asked the gateway anything". Every way the relay can fail to
// produce a report is a FAIL with its own reason and its own next command.
func TestInnerDoctorIsNeverSilentlyEmpty(t *testing.T) {
	tests := []struct {
		name     string
		outcome  machinetest.Outcome
		wantText string
	}{
		{
			name:     "exit 0 with no output",
			outcome:  machinetest.Outcome{Stdout: ""},
			wantText: "printed nothing",
		},
		{
			name:     "only whitespace",
			outcome:  machinetest.Outcome{Stdout: "\n  \n"},
			wantText: "printed nothing",
		},
		{
			// The guest shell's "command not found". Reading 127 as "127
			// failing checks" would be nonsense; the binary is simply absent.
			name:     "the inner sparkbox is missing",
			outcome:  machinetest.Outcome{ExitCode: 127, Stderr: "bash: /usr/local/bin/sparkbox: No such file or directory\n"},
			wantText: "no runnable " + guestSparkboxBin,
		},
		{
			name:     "the script never reached the machine",
			outcome:  machinetest.Outcome{Err: fmt.Errorf("inner-doctor: %w", machine.ErrTransport)},
			wantText: "did not run the gateway's doctor at all",
		},
		{
			name:     "the container CLI is too old to run it",
			outcome:  machinetest.Outcome{Err: fmt.Errorf("inner-doctor: %w", machine.ErrUnsupported)},
			wantText: minContainerVersion,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, fd := doctorEnv(t)
			fd.Execs["inner-doctor"] = tc.outcome
			results, report := doctorReport(t, e)
			r := find(t, results, machineLayer+"gateway doctor")
			if r.Status != Fail {
				t.Errorf("gateway doctor = %s (%q), want FAIL — silence is not health", r.Status, r.Detail)
			}
			if !strings.Contains(r.Detail+" "+r.Hint, tc.wantText) {
				t.Errorf("the result should say %q, got %q / %q", tc.wantText, r.Detail, r.Hint)
			}
			if !AnyFail(results) {
				t.Errorf("doctor exited zero over an unanswered gateway:\n%s", report)
			}
		})
	}
}

// --- the host layer ----------------------------------------------------------

// TestDarwinDoctorWithoutTheContainerCLI: with no `container` there is no
// machine layer to report at all, and pretending otherwise would print
// "machine does not exist" about a machine nobody could ask after.
func TestDarwinDoctorWithoutTheContainerCLI(t *testing.T) {
	e, fd := doctorEnv(t)
	p := e.Probe.(fakeProbe)
	p.paths = map[string]string{}
	e.Probe = p

	results, report := doctorReport(t, e)
	if !AnyFail(results) {
		t.Fatalf("a Mac without Apple Container cannot host a gateway:\n%s", report)
	}
	cli := find(t, results, macLayer+"apple container")
	if cli.Status != Fail || !strings.Contains(cli.Hint, "container") {
		t.Errorf("apple container = %s (%s / %s), want a FAIL naming the install", cli.Status, cli.Detail, cli.Hint)
	}
	note := find(t, results, machineLayer+"gateway")
	if !strings.Contains(note.Detail, "not installed") {
		t.Errorf("the machine layer must say it was not checked and why, got %q", note.Detail)
	}
	for _, name := range []string{machineLayer + "state", machineLayer + "gateway doctor"} {
		for _, r := range results {
			if r.Name == name {
				t.Errorf("%s was reported without a container CLI to ask: %q", name, r.Detail)
			}
		}
	}
	if calls := execCalls(fd); len(calls) != 0 {
		t.Errorf("doctor executed %v with no container CLI", calls)
	}
}

// TestDarwinDoctorOnAnOldMac: the host layer fails, and the machine layer still
// reports — the machine may well be running; what it cannot be is supported.
func TestDarwinDoctorOnAnOldMac(t *testing.T) {
	e, _ := doctorEnv(t)
	setSysctl(e, "kern.osproductversion", "14.7")

	results, report := doctorReport(t, e)
	if !AnyFail(results) {
		t.Fatalf("macOS 14.7 is below the floor for --virtualization:\n%s", report)
	}
	v := find(t, results, macLayer+"macos version")
	if v.Status != Fail || !strings.Contains(v.Detail, "14.7") {
		t.Errorf("macos version = %s (%s), want a FAIL naming the version", v.Status, v.Detail)
	}
	if !strings.Contains(v.Hint, minMacOSVersion) {
		t.Errorf("the FAIL must name the floor, got %q", v.Hint)
	}
	// The fault is on the Mac, and the report must not smear it across the
	// machine layer.
	if got := find(t, results, machineLayer+"gateway doctor"); got.Status != Pass {
		t.Errorf("gateway doctor = %s (%s); an old Mac does not make the gateway sick", got.Status, got.Detail)
	}
}

// --- the outer kernel --------------------------------------------------------

func TestCheckOuterKernel(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*testing.T, *Env)
		want     Status
		wantText string
	}{
		{
			name:     "absent",
			setup:    func(*testing.T, *Env) {},
			want:     Fail,
			wantText: "sparkbox setup",
		},
		{
			name:     "empty file",
			setup:    func(t *testing.T, e *Env) { writeOuterKernel(t, e, "") },
			want:     Fail,
			wantText: "empty",
		},
		{
			name:     "matches the release",
			setup:    func(t *testing.T, e *Env) { writeOuterKernel(t, e, testOuterKernel) },
			want:     Pass,
			wantText: "verified against " + testRelease,
		},
		{
			// The file the hypervisor boots differs from the one the release
			// the operator ASSERTED publishes. Not a warning: this is the
			// kernel that decides whether /dev/kvm exists inside the machine at
			// all, and the operator named the tag it should have come from.
			name: "a different kernel, with a release asserted",
			setup: func(t *testing.T, e *Env) {
				pinRelease(t, e, testRelease)
				writeOuterKernel(t, e, "some other kernel\n")
			},
			want:     Fail,
			wantText: "publishes",
		},
		{
			// The same bytes on disk, with NO --release: `doctor` has resolved
			// `latest` merely to have something to compare against, so the
			// difference may be nothing worse than a Mac that is deliberately a
			// release behind, or one booting a locally built kernel from the
			// documented SPARKBOX_KERNEL_SOURCE=build escape hatch. A check
			// that goes red on every Mac in the field the day a release ships
			// is the F8 pattern — it teaches operators to ignore the exit code.
			name:     "a different kernel, with no release asserted, is a WARN",
			setup:    func(t *testing.T, e *Env) { writeOuterKernel(t, e, "some other kernel\n") },
			want:     Warn,
			wantText: "NOT verified",
		},
		{
			// The precise field case: this Mac was set up from v9.9.9, whose
			// kernel is on disk and correct, and `latest` has since moved to a
			// rebuilt v10.0.0 kernel. Nothing is wrong with the laptop.
			name: "a Mac a release behind is not a failing Mac",
			setup: func(t *testing.T, e *Env) {
				writeOuterKernel(t, e, testOuterKernel)
				e.Cfg.Release = "" // exactly what `sparkbox doctor` passes with no --release
				e.Fetch = mapFetcher{
					ManifestURL("darwin", "arm64", e.Cfg.ArtifactBase, ""): strings.Replace(
						strings.Replace(darwinManifest(), "RELEASE="+testRelease, "RELEASE=v10.0.0", 1),
						"SHA256_OUTER_KERNEL="+sha256Of(testOuterKernel),
						"SHA256_OUTER_KERNEL="+sha256Of("a rebuilt kernel\n"), 1),
				}
			},
			want:     Warn,
			wantText: "v10.0.0",
		},
		{
			// The other way a release gets asserted: a setup run that already
			// resolved a tag onto e.Manifest and is verifying what it just
			// downloaded. There a mismatch really is broken.
			name: "a setup-resolved manifest asserts too",
			setup: func(t *testing.T, e *Env) {
				writeOuterKernel(t, e, "some other kernel\n")
				m, err := ParseManifest(strings.NewReader(darwinManifest()), testRelease)
				if err != nil {
					t.Fatal(err)
				}
				e.Manifest = m
			},
			want:     Fail,
			wantText: "publishes",
		},
		{
			// Offline is not broken. Report the local hash, say plainly that
			// nothing verified it, and do not fail a laptop on a plane.
			name: "manifest unreachable",
			setup: func(t *testing.T, e *Env) {
				writeOuterKernel(t, e, testOuterKernel)
				e.Fetch = mapFetcher{}
			},
			want:     Warn,
			wantText: "NOT verified",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := darwinTestEnv(t, false)
			tc.setup(t, e)
			got := checkOuterKernel(e)
			if got.Status != tc.want {
				t.Errorf("outer kernel = %s (%s / %s), want %s", got.Status, got.Detail, got.Hint, tc.want)
			}
			if !strings.Contains(got.Detail+" "+got.Hint, tc.wantText) {
				t.Errorf("should mention %q, got %q / %q", tc.wantText, got.Detail, got.Hint)
			}
		})
	}
}

// TestDoctorExitsZeroOnAMacAReleaseBehind is the exit-code half of the same
// rule, asserted end to end rather than on one Result: doctor's non-zero exit
// is the signal this whole workstream is built on, so a healthy Mac that is
// merely older than `latest` must not turn it red.
func TestDoctorExitsZeroOnAMacAReleaseBehind(t *testing.T) {
	e, _ := doctorEnv(t) // the kernel on disk is testRelease's, and correct
	e.Cfg.Release = ""   // what `sparkbox doctor` passes with no --release
	// `latest` has since moved to a rebuilt kernel with different bytes.
	e.Fetch = mapFetcher{
		ManifestURL("darwin", "arm64", e.Cfg.ArtifactBase, ""): strings.Replace(
			strings.Replace(darwinManifest(), "RELEASE="+testRelease, "RELEASE=v10.0.0", 1),
			"SHA256_OUTER_KERNEL="+sha256Of(testOuterKernel),
			"SHA256_OUTER_KERNEL="+sha256Of("a rebuilt kernel\n"), 1),
	}
	results, report := doctorReport(t, e)
	if AnyFail(results) {
		t.Errorf("doctor would exit non-zero on a healthy Mac one release behind:\n%s", report)
	}
	if got := find(t, results, macLayer+"outer kernel"); got.Status != Warn {
		t.Errorf("outer kernel = %s (%s), want a WARN", got.Status, got.Detail)
	}
}

// TestDoctorDoesNotAssertAReleaseItMerelyResolved: doctor fetches the manifest
// to verify the outer kernel's checksum, and that must NOT leak into e.Manifest
// — publishing it would turn "--release not given, so assert nothing" into an
// assertion against whatever `latest` is today, and every host deliberately a
// release behind would start FAILing its version check.
func TestDoctorDoesNotAssertAReleaseItMerelyResolved(t *testing.T) {
	e, _ := doctorEnv(t)
	e.Cfg.Release = "" // what `sparkbox doctor` passes with no --release
	e.Cfg.Version = testRelease
	if _, report := doctorReport(t, e); report == "" {
		t.Fatal("no report")
	}
	if e.Manifest.Release != "" {
		t.Errorf("doctor published a resolved release onto the Env: %q", e.Manifest.Release)
	}
}

// --- the linux report is untouched -------------------------------------------

// TestLinuxDoctorBatteryUnchanged: B5 must be invisible on linux. Same checks,
// same order, same names, and the same header line doctor has always printed.
func TestLinuxDoctorBatteryUnchanged(t *testing.T) {
	e, _ := testEnv(t, false) // fakeProbe defaults to linux/amd64
	got := DoctorChecksFor(e)
	want := DefaultChecks()
	if len(got) != len(want) {
		t.Fatalf("linux doctor battery is %d checks, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name {
			t.Errorf("check %d is %q, want %q", i, got[i].Name, want[i].Name)
		}
	}
	// A nil Env (or one with no Probe) is the linux battery too: doctor must
	// never become un-runnable because a driver could not be wired up.
	if len(DoctorChecksFor(nil)) != len(want) {
		t.Error("DoctorChecksFor(nil) is not the linux battery")
	}
	if h := DoctorHeader(e); h != "sparkbox doctor — "+e.Cfg.Root {
		t.Errorf("linux header changed: %q", h)
	}
}

func TestDarwinDoctorHeaderNamesBothHosts(t *testing.T) {
	e, _ := doctorEnv(t)
	h := DoctorHeader(e)
	// cfg.Root is /srv/sparkbox — a path INSIDE the machine. Printed alone at
	// the top of a Mac's report it reads as this laptop's sparkbox home.
	if !strings.Contains(h, e.Cfg.machineName()) || !strings.Contains(h, "Mac") {
		t.Errorf("darwin header must name both hosts, got %q", h)
	}
}

// TestDoctorUsesOneDriver: B4's machine.Driver is the ONLY path to the
// container CLI. A doctor that shelled out on its own could diagnose a
// different machine from the one setup provisioned.
func TestDoctorUsesOneDriver(t *testing.T) {
	e, fd := doctorEnv(t)
	e.Run = &fakeRunner{} // a fresh recorder: nothing on a Mac may use it
	doctorReport(t, e)
	if fr, ok := e.Run.(*fakeRunner); ok && len(fr.calls) > 0 {
		t.Errorf("darwin doctor shelled out directly: %v", fr.calls)
	}
	if len(fd.Calls) == 0 {
		t.Error("darwin doctor never asked the machine driver anything")
	}
}
