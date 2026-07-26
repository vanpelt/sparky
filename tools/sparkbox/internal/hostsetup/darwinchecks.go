package hostsetup

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine"
)

// The darwin check batteries.
//
// Every check below is a CLOSURE over *Env and ignores the Probe it is handed.
// That is how a Check carries extra context (here: the machine driver) without
// changing the Check type, and it is deliberate rather than sloppy — the linux
// checks stay pure functions of a Probe and a Config, and the darwin ones that
// reuse them hand over a machineProbe explicitly.
//
// LAYER LABELS. On a Mac there are two hosts in play and a report that mixed
// them would be worse than no report: "disk space" and "kvm device" are both
// true statements about SOMETHING, and which one decides whether the operator
// frees space on their laptop or deletes a VM. So every darwin check name
// carries its layer, and the rule is which layer the RESULT DESCRIBES, not
// where it was measured — `machine: nested virtualization` is read on the Mac
// (from `container inspect`) and describes the machine, so it is a machine
// line.
//
//	mac:      this laptop
//	machine:  the nested linux VM where the gateway actually runs
const (
	macLayer     = "mac: "
	machineLayer = "machine: "
)

// darwinHostChecks are the facts about THIS MAC — the layer a nested machine
// cannot fix and cannot be diagnosed around.
//
// One list, two consumers: `setup`'s preflight gate and `sparkbox doctor`. A
// doctor that asked a different set of host questions from the gate that
// refused the setup would be a second opinion nobody asked for, and the two
// would drift the first time either grew a check.
func darwinHostChecks(e *Env) []Check {
	return []Check{
		{macLayer + "operating system", checkOS},
		{macLayer + "cpu architecture", func(p Probe, _ Config) Result { return checkDarwinArch(p) }},
		{macLayer + "macos version", func(p Probe, _ Config) Result { return checkMacOSVersion(p) }},
		{macLayer + "apple silicon", func(p Probe, _ Config) Result { return checkAppleSilicon(p) }},
		{macLayer + "apple container", func(p Probe, _ Config) Result { return checkContainerCLI(e, p) }},
		{macLayer + "container service", func(Probe, Config) Result { return checkContainerService(e) }},
		{macLayer + "disk space", func(p Probe, c Config) Result { return checkMacDisk(e, p, c) }},
	}
}

// darwinPreflightChecks is what must be true of the MAC before setup starts.
//
// A battery rather than a Step, for a reason worth stating: a Step can report
// itself Satisfied and skip, and a precondition must not be skippable. The
// existing preflight gate already has the right dry-run semantics — advisory in
// --dry-run, fatal otherwise — so these ride it unchanged.
//
// It deliberately does NOT ask about the outer kernel: downloading that is
// step 1 of the run this gate is about to admit, so on a first provision the
// only honest answer would be "absent", and failing on it would make setup
// refuse to do the thing that fixes it. `doctor` asks, because by then it
// should be there.
func darwinPreflightChecks(e *Env) []Check {
	return append(darwinHostChecks(e),
		Check{machineLayer + "name", func(Probe, Config) Result { return checkMachineAvailable(e) }})
}

// checkDarwinArch is darwin-specific rather than checkArch because the answer
// differs: checkArch merely WARNs about an unpublished arch, while an Intel Mac
// is a hard refusal — it has no nested virtualization at all, so there is no
// machine for the gateway to run in.
func checkDarwinArch(p Probe) Result {
	if p.GOARCH() == "arm64" {
		return pass("arm64")
	}
	return fail(p.GOARCH(), "sparkbox on macOS needs Apple Silicon: nested virtualization "+
		"(`container machine --virtualization`) exists only there, and without it the machine has no /dev/kvm")
}

func checkMacOSVersion(p Probe) Result {
	// kern.osproductversion, NOT kern.osrelease: the latter is the Darwin
	// version (25.5.0 where the product version is 26.5.2) and is not
	// comparable with a macOS floor.
	v, err := p.Sysctl("kern.osproductversion")
	if err != nil {
		return warn("unknown", "could not read kern.osproductversion; `container machine --virtualization` needs macOS >= "+minMacOSVersion)
	}
	if machine.VersionAtLeast(v, minMacOSVersion) {
		return pass(v)
	}
	return fail(v, "macOS >= "+minMacOSVersion+" is required for `container machine --virtualization`; upgrade macOS")
}

func checkAppleSilicon(p Probe) Result {
	brand, err := p.Sysctl("machdep.cpu.brand_string")
	if err != nil {
		return warn("unknown", "could not read machdep.cpu.brand_string; nested virtualization needs Apple M"+
			fmt.Sprint(minAppleGeneration)+" or newer")
	}
	gen, ok := machine.AppleGeneration(brand)
	if !ok {
		return fail(brand, "this is not an Apple M-series CPU; nested virtualization needs Apple M"+
			fmt.Sprint(minAppleGeneration)+" or newer")
	}
	if gen < minAppleGeneration {
		return fail(brand, fmt.Sprintf("nested virtualization needs Apple M%d or newer; this machine cannot host "+
			"a KVM-capable guest, so firecracker has nowhere to run", minAppleGeneration))
	}
	return pass(brand)
}

func checkContainerCLI(e *Env, p Probe) Result {
	bin := e.Cfg.containerBin()
	if _, err := p.LookPath(bin); err != nil {
		return fail(bin+" not found on PATH",
			"install Apple's container runtime (https://github.com/apple/container) — sparkbox needs >= "+minContainerVersion)
	}
	if e.Machine == nil {
		return warn("version not checked", "no container driver was wired up (internal)")
	}
	rt, err := e.Machine.Runtime(e.Ctx)
	if err != nil {
		if errors.Is(err, machine.ErrUnsupported) {
			return fail("your `container` CLI is too old",
				"it does not accept `system version --format json`; sparkbox needs >= "+minContainerVersion)
		}
		return fail("could not ask "+bin+" for its version: "+err.Error(),
			"check the install, then re-run `sparkbox doctor`")
	}
	// Refuse below the floor rather than degrade. Everything sparkbox relies on
	// about this transport — argv joined and re-parsed by bash, stdin dropped
	// without -i, exit-code propagation, `--format json` on machine ls, exit 64
	// for usage errors — was measured on 1.1.0 only. Given what 1.1.0 already
	// does silently, an older build's failure modes are unknown and plausibly
	// worse.
	if !machine.VersionAtLeast(rt.CLIVersion, minContainerVersion) {
		return fail("container "+rt.CLIVersion,
			"sparkbox needs Apple Container >= "+minContainerVersion+"; upgrade it (older builds are not merely "+
				"missing features, their transport behaviour is untested here)")
	}
	return pass("container " + rt.CLIVersion)
}

// checkContainerService is a FAIL, not a WARN.
//
// poc.sh only WARNs here, which means a doctor PASS there does not imply
// `container machine …` will work at all — every subsequent step then fails
// with a service error the report said nothing about.
func checkContainerService(e *Env) Result {
	if e.Machine == nil {
		return warn("not checked", "no container driver was wired up (internal)")
	}
	rt, err := e.Machine.Runtime(e.Ctx)
	if err != nil {
		return fail("could not reach the container runtime: "+err.Error(), "start it with `"+e.Cfg.containerBin()+" system start`")
	}
	if !rt.ServiceRunning {
		return fail("the container runtime service is not running",
			"start it with `"+e.Cfg.containerBin()+" system start`, then re-run")
	}
	return pass("running")
}

// checkMacDisk asks about the MAC's filesystem, which is where the machine's
// whole virtual disk lives.
func checkMacDisk(e *Env, p Probe, cfg Config) Result {
	path := e.MacDir
	if path == "" {
		path = "/"
	}
	if _, err := p.Stat(path); err != nil {
		path = "/"
	}
	free, err := p.DiskFreeBytes(path)
	if err != nil || free == 0 {
		return warn("unknown", "the machine's data volume is a file on this filesystem; make sure it has room")
	}
	want := uint64(cfg.DataVolumeGB) << 30
	gib := float64(free) / (1 << 30)
	if want > 0 && free < want {
		return warn(fmt.Sprintf("%.0f GiB free on %s", gib, path),
			fmt.Sprintf("the machine is asked for a %dG data volume and this filesystem has less; "+
				"lower --data-volume-gb or free space", cfg.DataVolumeGB))
	}
	return pass(fmt.Sprintf("%.0f GiB free on %s", gib, path))
}

// checkMachineAvailable answers the ownership question BEFORE anything is
// built: is the name free, is it ours, or does it belong to somebody else?
func checkMachineAvailable(e *Env) Result {
	if e.Machine == nil {
		return warn("not checked", "no container driver was wired up (internal)")
	}
	name := e.Cfg.machineName()
	info, err := e.Machine.Inspect(e.Ctx, name)
	switch {
	case errors.Is(err, machine.ErrNotFound):
		return pass(name + " is available")
	case err != nil:
		return fail("could not ask about machine "+name+": "+err.Error(),
			"is the container service running? `"+e.Cfg.containerBin()+" system status`")
	}
	if !machineIsOurs(info, e.Cfg) {
		return fail(fmt.Sprintf("%s exists but is not ours (image %s, home-mount %s)",
			name, orDash(info.ImageRef), orDash(info.HomeMount)),
			fmt.Sprintf("setup will not delete it — it may hold running sandboxes. "+
				"Use another name (--machine-name <other>), or remove it deliberately: %s machine delete %s",
				e.Cfg.containerBin(), name))
	}
	return pass(fmt.Sprintf("%s is an existing sparkbox machine (%s, home-mount none)", name, info.State))
}

// --- verify ------------------------------------------------------------------

// darwinVerifyChecks is the battery that decides a darwin run's exit code.
//
// Three layers, in the order an operator would ask them:
//
//  1. Mac-side facts about the machine itself (exists, ours, running, has
//     nested virtualization).
//  2. The EXISTING linux checks, run INSIDE the machine through a machineProbe.
//     This is where A1's crash-loop detector lands: a green inner setup over a
//     dead gateway fails the darwin run, which is F7's original fix applied one
//     layer out.
//  3. The inner `sparkbox doctor`, relayed. Its EXIT CODE is the verdict and
//     its text becomes Result.Output.
//
// On (3): deliberately NOT `doctor --json`. The binary inside the machine is
// the resolved RELEASE, which today has no such flag, so a JSON relay would
// fail on every release cut before it existed — and it would make Result a wire
// format between two binary versions. The exit code is stable on every release
// ever published.
func darwinVerifyChecks(e *Env) []Check {
	if e.Machine == nil {
		return []Check{{machineLayer + "gateway", func(Probe, Config) Result {
			return warn("not checked", "no container driver was wired up (internal)")
		}}}
	}
	macSide := []Check{
		{machineLayer + "state", func(Probe, Config) Result { return checkMachineHealthy(e) }},
		{machineLayer + "nested virtualization", func(Probe, Config) Result { return checkNestedVirt(e) }},
		{machineLayer + "image", func(Probe, Config) Result { return checkMachineImageCurrent(e) }},
	}
	// If the machine is not there, every guest check below would fail for one
	// reason — there is nothing to ask — and would say so eight different ways,
	// several of them actively misleading (a reused /dev/kvm check blaming this
	// Mac's BIOS, a systemd check suggesting `systemctl start`). A check battery
	// that cries wolf teaches the operator to skim the report, so say the one
	// true thing once. Inspect, never Exec: a probe must not boot what it
	// measures.
	if info, err := e.Machine.Inspect(e.Ctx, e.Cfg.machineName()); err != nil || info.State != machine.StateRunning {
		return append(macSide, Check{machineLayer + "gateway", func(Probe, Config) Result {
			return machineDownNote(e, err, info.State)
		}})
	}
	mp := newMachineProbe(e.Ctx, e.Machine, e.Cfg.machineName(), e.Manifest.arch())
	ic := innerConfig(e)
	return append(macSide,
		// The existing checks, unmodified, pointed at the machine.
		Check{machineLayer + "kvm device", func(Probe, Config) Result { return checkKVM(mp, ic) }},
		Check{machineLayer + "sparkbox service", func(Probe, Config) Result { return checkService(mp, ic) }},
		Check{machineLayer + "sluice service", func(Probe, Config) Result { return checkSluiceService(mp, ic) }},
		Check{machineLayer + "sparkbox version", func(Probe, Config) Result { return checkVersions(mp, ic) }},
		// The guest facts gateway-verify.sh asserts that no existing check covers.
		Check{machineLayer + "kernel", func(Probe, Config) Result { return checkGuestFacts(e, factKernel) }},
		Check{machineLayer + "isolation", func(Probe, Config) Result { return checkGuestFacts(e, factVirtiofs) }},
		Check{machineLayer + "tun device", func(Probe, Config) Result { return checkGuestFacts(e, factTun) }},
		Check{machineLayer + "gateway doctor", func(Probe, Config) Result { return checkInnerDoctor(e) }},
	)
}

// machineDownNote is the ONE line that replaces the whole guest section when
// there is no machine to ask.
//
// It names which of the three reasons applies, because "not checked" on its own
// is the empty section this work item exists to prevent: a reader skimming a
// report cannot tell a machine that is merely stopped (start it) from one that
// was never created (run setup) from a runtime that refused to answer at all
// (fix the runtime). The verdict is a WARN rather than a FAIL because the
// machine-layer checks ABOVE this one already failed with the real cause — two
// FAILs for one fault would just teach the reader to skim.
func machineDownNote(e *Env, err error, state machine.State) Result {
	name := e.Cfg.machineName()
	const tail = "the gateway lives inside the machine, so nothing about it can be diagnosed until the machine is up"
	switch {
	case errors.Is(err, machine.ErrNotFound):
		return warn("not checked — machine "+name+" does not exist",
			"run `sparkbox setup` on this Mac: it creates the machine and provisions the gateway inside it")
	case err != nil:
		return warn("not checked — machine "+name+" could not be inspected: "+err.Error(),
			"is the container service running? `"+e.Cfg.containerBin()+" system status`")
	}
	return warn(fmt.Sprintf("not checked — machine %s is %s", name, orDash(string(state))),
		fmt.Sprintf("start it (%s machine run --name %s --root -- /bin/true) and re-run; %s",
			e.Cfg.containerBin(), name, tail))
}

// innerConfig is the Config the machine was asked to become — which is
// e.Cfg itself, because on darwin every layout and subsystem field already
// describes the machine and is forwarded verbatim by innerSetupArgs.
//
// One derivation, two consumers (what we ask for, what we then check against),
// so the two cannot drift. Only the fields the Mac rewrites at the boundary are
// adjusted here, and they are the same ones innerSetupArgs rewrites.
func innerConfig(e *Env) Config {
	c := e.Cfg
	// --bin-path is refused on darwin, so inside the machine it is the default.
	c.BinPath = defaultBinPath
	c.MoveAdminSSH = false
	if e.Manifest.Release != "" {
		c.Release = e.Manifest.Release
		c.Version = e.Manifest.Release
	}
	return c
}

func checkMachineHealthy(e *Env) Result {
	name := e.Cfg.machineName()
	info, err := e.Machine.Inspect(e.Ctx, name)
	switch {
	case errors.Is(err, machine.ErrNotFound):
		return fail("machine "+name+" does not exist", "run `sparkbox setup` — the machine step should have created it")
	case err != nil:
		return fail("could not inspect machine "+name+": "+err.Error(),
			"is the container service running? `"+e.Cfg.containerBin()+" system status`")
	}
	if !machineIsOurs(info, e.Cfg) {
		return fail(fmt.Sprintf("%s is not a sparkbox machine (image %s, home-mount %s)",
			name, orDash(info.ImageRef), orDash(info.HomeMount)),
			"provision under another name with --machine-name, or remove this one deliberately")
	}
	if info.State != machine.StateRunning {
		return fail(fmt.Sprintf("machine %s is %s", name, info.State),
			fmt.Sprintf("start it: %s machine run --name %s --root -- /bin/true", e.Cfg.containerBin(), name))
	}
	return pass(fmt.Sprintf("%s running at %s (%d cpu, %d GiB)", name, info.IPAddress, info.CPUs, info.MemoryBytes>>30))
}

// checkNestedVirt reads the CONTAINER view, which is the only place the answer
// exists: `container machine inspect` reports neither virtualization nor the
// kernel path, and `[machine] virtualization = false` is the system default —
// so a machine created without it boots perfectly and can never run firecracker.
func checkNestedVirt(e *Env) Result {
	info, err := e.Machine.Inspect(e.Ctx, e.Cfg.machineName())
	if err != nil {
		return warn("unknown", "the machine could not be inspected, so nested virtualization is unverified")
	}
	if info.ContainerID == "" {
		return warn("unknown", "the machine reported no container id, so nested virtualization cannot be read back")
	}
	ci, err := e.Machine.InspectContainer(e.Ctx, info.ContainerID)
	if err != nil {
		return warn("unknown ("+err.Error()+")",
			"`container inspect <containerId>` is the only supported readback for this; the /dev/kvm check below is the fallback")
	}
	if !ci.Virtualization {
		return fail("the machine was created WITHOUT --virtualization",
			"it will boot fine and can never run firecracker. Delete it and re-run setup: `"+
				e.Cfg.containerBin()+" machine delete "+e.Cfg.machineName()+"`")
	}
	return pass("enabled")
}

// checkMachineImageCurrent reports tag drift as a WARN.
//
// A WARN and not a FAIL on purpose: the tag is the hash of three baked scripts,
// the sparkbox binary is fetched fresh on every provision, and forcing a
// machine deletion (with whatever sandboxes it holds) over a script revision
// would be a worse outcome than a stale script.
func checkMachineImageCurrent(e *Env) Result {
	want := machineImageRef(e.Cfg)
	info, err := e.Machine.Inspect(e.Ctx, e.Cfg.machineName())
	if err != nil {
		return warn("unknown", "the machine could not be inspected")
	}
	if info.ImageRef == want {
		return pass(want)
	}
	return warn(fmt.Sprintf("machine runs %s, this sparkbox builds %s", orDash(info.ImageRef), want),
		fmt.Sprintf("the machine's baked scripts are from an older build. Harmless day to day — the sparkbox "+
			"binary is fetched fresh every run — but to refresh them, delete the machine and re-run setup: "+
			"%s machine delete %s", e.Cfg.containerBin(), e.Cfg.machineName()))
}

// guestFact names one of the three facts scriptGuestFacts reports.
type guestFact int

const (
	factKernel guestFact = iota
	factVirtiofs
	factTun
)

func checkGuestFacts(e *Env, which guestFact) Result {
	res, err := e.Machine.Exec(e.Ctx, machine.ExecSpec{
		Machine: e.Cfg.machineName(), Op: "guest-facts",
		Script: scriptGuestFacts, ReadOnly: true, Timeout: time.Minute,
	})
	if err != nil {
		return warn("unknown ("+err.Error()+")", "the machine could not be asked; the checks above have the reason")
	}
	kv, perr := parseEnv(strings.NewReader(string(res.Stdout)))
	if perr != nil {
		return warn("unreadable", "could not parse the machine's own report")
	}
	switch which {
	case factKernel:
		rel := kv["kernel_release"]
		if rel == "" {
			return warn("unknown", "the machine did not report a kernel release")
		}
		// Reported, and asserted only when the manifest publishes an expected
		// release. poc.sh hardcodes "6.14.9-sparkbox-poc" in TWO files, so
		// bumping the kernel in one turns a working machine into a failing
		// preflight; naming the absence is better than duplicating the literal.
		if e.Manifest.OuterKernelRelease == "" {
			return pass(rel + " (this release names no expected kernel, so it is reported, not asserted)")
		}
		if rel != e.Manifest.OuterKernelRelease {
			return fail("machine kernel is "+rel+", release "+e.Manifest.Release+" names "+e.Manifest.OuterKernelRelease,
				"the machine booted a different outer kernel than this release publishes; delete the machine and re-run setup")
		}
		return pass(rel)
	case factVirtiofs:
		targets := strings.Trim(kv["virtiofs_targets"], ",")
		for _, t := range strings.Split(targets, ",") {
			if t == "/Users" || strings.HasPrefix(t, "/Users/") {
				return fail("the machine has your home directory mounted at "+t,
					"it was created without --home-mount none, so a sandbox escape reaches your files. "+
						"Delete it and re-run setup")
			}
		}
		return pass("no /Users mount inside the machine")
	default:
		if kv["tun"] == "present" {
			return pass("/dev/net/tun present")
		}
		return fail("/dev/net/tun missing inside the machine",
			"sandbox networking needs TAP devices; the outer kernel was built without CONFIG_TUN")
	}
}

// checkInnerDoctor relays `sparkbox doctor` from inside the machine.
//
// The exit code is the verdict and the text is the evidence, printed indented
// under the result by PrintResults. That keeps the gateway's own diagnosis in
// the gateway's own wording — a doctor run on the Mac and a doctor run in the
// machine say the same things about the same host, because they are the same
// code.
//
// EVERY branch below produces a verdict AND a reason, and none of them can
// produce an empty section. That is not tidiness: a relayed report that came
// back blank and printed as a bare PASS would read as "the gateway is healthy"
// while meaning "nobody asked the gateway anything" — F7 with a different
// subject. So a missing binary, a transport fault, a CLI too old, and a doctor
// that exited 0 while printing nothing are four distinct FAILs with four
// distinct next commands, rather than one silence.
func checkInnerDoctor(e *Env) Result {
	name := e.Cfg.machineName()
	env := map[string]string{"SPARKBOX_BIN": guestSparkboxBin}
	if e.Cfg.Gateway != "" {
		env["SPARKBOX_DOCTOR_GATEWAY"] = e.Cfg.Gateway
	}
	res, err := e.Machine.Exec(e.Ctx, machine.ExecSpec{
		Machine: name, Op: "inner-doctor",
		Script: scriptInnerDoctor, ReadOnly: true, Timeout: 5 * time.Minute,
		Env: env,
	})
	// Evidence only when there is something worth keeping. `sparkbox doctor` is
	// run casually and often; a passing run should not leave a timestamped
	// directory in ~/Library each time, while a failing one is exactly the run
	// whose transcript somebody wants. A provision (which has already opened an
	// evidence dir) keeps both.
	if err != nil || e.EvidenceDir != "" {
		e.evidence("inner-doctor.txt", res.Stdout, res.Stderr)
	}

	var ee *machine.ExitError
	switch {
	case errors.As(err, &ee) && ee.Code == 127:
		// 127 is the guest shell's "command not found". The relay ran, the
		// binary did not exist — which is a completely different fault from a
		// doctor that ran and found problems, and reporting it as "127 failing
		// checks" (a plain exit-code reading would) is nonsense on stilts.
		r := fail("there is no runnable "+guestSparkboxBin+" inside machine "+name+" (exit 127)",
			"the gateway binary is missing, so it cannot diagnose itself: re-run `sparkbox setup` "+
				"(its install-binary step inside the machine is what puts it there)")
		r.Output = innerReport(name, res.Stdout, res.Stderr)
		return r
	case errors.As(err, &ee):
		// The exit STATUS, never a count of anything. `sparkbox doctor` returns
		// one error for any number of failing checks and main exits 1, so
		// reading the code as a tally would report a gateway with six FAILs as
		// "1 failing check(s)" — and a flag-parse failure (flag.ExitOnError
		// exits 2, a live risk when the relay passes --gateway to an older
		// inner binary) as "2 failing check(s)". Same reasoning as the 127
		// branch above; the relayed transcript below carries the real count.
		r := fail(fmt.Sprintf("the gateway's own doctor FAILED (exit %d); its report is below", ee.Code),
			"the machine's report is below, in its own words; fix it there and re-run `sparkbox doctor`")
		r.Output = innerReport(name, res.Stdout, res.Stderr)
		return r
	case errors.Is(err, machine.ErrTransport):
		// The script never reached the guest, so nothing at all is known about
		// the gateway — the one state that must never be rendered as a pass.
		return fail("machine "+name+" did not run the gateway's doctor at all: "+err.Error(),
			"this is a transport fault, not a gateway fault — nothing about the gateway was measured. "+
				"Report it with the version from `"+e.Cfg.containerBin()+" system version`")
	case errors.Is(err, machine.ErrUnsupported):
		return fail("your Apple Container build cannot run the gateway's doctor: "+err.Error(),
			"sparkbox needs Apple Container >= "+minContainerVersion+"; upgrade it")
	case errors.Is(err, machine.ErrDryRun):
		return warn("not probed in --dry-run", "a plan boots nothing and asks the machine nothing")
	case err != nil:
		return fail("could not run `sparkbox doctor` inside machine "+name+": "+err.Error(),
			"the machine checks above have the reason; nothing about the gateway itself was measured")
	}

	if strings.TrimSpace(string(res.Stdout)) == "" {
		return fail("`sparkbox doctor` inside machine "+name+" exited 0 but printed nothing",
			"a doctor that says nothing is not a doctor that passed. Run it by hand: "+
				e.Cfg.containerBin()+" machine run --name "+name+" --root -- "+guestSparkboxBin+" doctor")
	}
	r := pass("the gateway's own doctor passes")
	r.Output = innerReport(name, res.Stdout, res.Stderr)
	return r
}

// innerReport is the relayed text, headed by a line naming whose report it is.
//
// The header is load-bearing. The inner report's own lines are "  [PASS] …"
// lines in exactly the format of the OUTER report, so without a banner an
// operator reading a wall of results has no way to tell the machine's verdict
// about /dev/kvm from the Mac's. PrintResults indents everything here behind
// "│ ", and the header says which layer is speaking.
func innerReport(name string, stdout, stderr []byte) string {
	body := strings.TrimRight(string(stdout), "\n")
	if s := strings.TrimSpace(string(stderr)); s != "" {
		body += "\n--- stderr ---\n" + s
	}
	if strings.TrimSpace(body) == "" {
		// Never return "" from a branch whose caller prints it as evidence: an
		// empty evidence block under a FAIL reads as if the failure had no
		// detail, when in fact the detail is that there was none.
		body = "(the machine printed nothing)"
	}
	return "── sparkbox doctor, inside machine " + name + " ──\n" + tail(body, 40)
}

// tail keeps the last n lines, so a long inner report does not bury the Mac's
// own results.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
