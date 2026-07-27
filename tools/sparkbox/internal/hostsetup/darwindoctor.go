package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// `sparkbox doctor` on a Mac.
//
// Before this, checks.go hard-FAILED any non-linux host, which made "run
// `sparkbox doctor`" an untruthful instruction on the one platform that needed
// it most: the command could only ever tell a Mac owner that their OS was
// wrong, while `sparkbox setup` on the same laptop was busy provisioning a
// perfectly good gateway inside a nested machine.
//
// What doctor has to answer on a Mac is two questions at once, and they have
// different answers and different fixes:
//
//  1. is THIS LAPTOP able to host a machine at all? (macOS >= 15, Apple
//     Silicon M3+, Apple Container >= 1.1.0 and its service, disk, and the
//     outer kernel the hypervisor boots)
//  2. is THE GATEWAY — which lives inside the nested machine — healthy?
//
// The second is answered by the machine layer of darwinVerifyChecks (the same
// battery the setup run's verify pass uses, so doctor and setup cannot disagree
// about what "healthy" means) and, at the end of it, by relaying the gateway's
// OWN `sparkbox doctor` out of the machine. Both layers print into one report,
// and every line says which layer it describes — see the label comment in
// darwinchecks.go.
func darwinDoctorChecks(e *Env) []Check {
	host := append(darwinHostChecks(e),
		// Doctor asks and preflight does not: on a first run the kernel is what
		// setup is about to download, and by doctor time it should be on disk.
		Check{macLayer + "outer kernel", func(p Probe, _ Config) Result { return checkOuterKernel(e) }})

	if e.Machine == nil {
		return append(host, Check{machineLayer + "gateway", func(Probe, Config) Result {
			return warn("not checked", "no container driver was wired up (internal)")
		}})
	}
	// If the runtime itself is unusable, every machine-layer check below would
	// fail for one reason and say so a dozen ways — and `machine: state` would
	// report "does not exist" about a machine that may well exist and simply
	// cannot be asked. One honest line instead, with the host checks above
	// carrying the actual fault and the actual fix.
	if note, ok := containerReachable(e); !ok {
		return append(host, Check{machineLayer + "gateway", func(Probe, Config) Result { return note }})
	}
	return append(host, darwinVerifyChecks(e)...)
}

// containerReachable reports whether it is worth asking the container runtime
// anything at all, and what to say when it is not.
//
// It re-uses the same two facts the `mac: apple container` and `mac: container
// service` checks report rather than inventing a third opinion, and it uses
// e.Probe — the same Probe RunChecks will hand every check — because the
// decision is which checks to BUILD, which happens before any of them runs.
func containerReachable(e *Env) (Result, bool) {
	bin := e.Cfg.containerBin()
	if e.Probe != nil {
		if _, err := e.Probe.LookPath(bin); err != nil {
			return warn("not checked — "+bin+" is not installed on this Mac",
				"the gateway runs inside a machine only Apple Container can create; install it "+
					"(https://github.com/apple/container), then re-run `sparkbox doctor`"), false
		}
	}
	rt, err := e.Machine.Runtime(e.Ctx)
	switch {
	case err != nil:
		return warn("not checked — the container runtime did not answer: "+err.Error(),
			"start it with `"+bin+" system start`, then re-run `sparkbox doctor`"), false
	case !rt.ServiceRunning:
		return warn("not checked — the container runtime service is not running",
			"start it with `"+bin+" system start`, then re-run `sparkbox doctor`"), false
	}
	return Result{}, true
}

// checkOuterKernel is the one artifact a Mac downloads FOR ITSELF: the
// KVM-capable arm64 Image that `container machine --virtualization` boots, and
// inside which firecracker runs. Not Cfg.KernelPath, which is the guest kernel
// a sandbox boots, inside the machine — two kernels, and confusing them is easy.
//
// "Verified" means verified against the release manifest, so doctor resolves
// one if the setup run has not already (doctor is normally a standalone
// process, so e.Manifest is the zero value). That fetch is best-effort and
// short-fused: a laptop on a plane should get a WARN naming the local hash, not
// a FAIL about a checksum nobody could reach GitHub to read.
func checkOuterKernel(e *Env) Result {
	path := e.Cfg.outerKernelPath(e.MacDir)
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return fail("no outer kernel at "+path,
			"the machine cannot boot without it: run `sparkbox setup` on this Mac (its outer-kernel step "+
				"downloads it), or point at one you already have with --outer-kernel <path>")
	case fi.Size() == 0:
		return fail("the outer kernel at "+path+" is empty",
			"an interrupted download; delete it and re-run `sparkbox setup`")
	}
	have, herr := sha256File(path)
	if herr != nil {
		return warn("present at "+path+" but unreadable: "+herr.Error(),
			"check the file's permissions; setup will not overwrite what it cannot hash")
	}

	m, merr := doctorManifest(e)
	size := humanBytes(uint64(fi.Size()))
	if merr != nil {
		return warn(fmt.Sprintf("present at %s (%s, sha256 %s) — NOT verified", path, size, short(have)),
			"the release manifest could not be fetched, so nothing compared this against a published "+
				"checksum ("+merr.Error()+")")
	}
	if m.SHA256OuterKernel == "" {
		return warn(fmt.Sprintf("present at %s (%s, sha256 %s) — NOT verified", path, size, short(have)),
			"release "+m.Release+" publishes no outer-kernel checksum, so there is nothing to compare against")
	}
	if have != m.SHA256OuterKernel {
		detail := fmt.Sprintf("the outer kernel at %s is sha256 %s, but release %s publishes %s",
			path, short(have), m.Release, short(m.SHA256OuterKernel))
		// A mismatch is only a FAULT when the operator asserted a release. With
		// no --release, m.Release is whatever `latest` redirects to TODAY, and
		// three ordinary situations differ from it through nobody's fault: a
		// Mac deliberately a release behind, a Mac provisioned before the next
		// tag shipped, and the documented SPARKBOX_KERNEL_SOURCE=build /
		// --outer-kernel escape hatch, which is not necessarily the artifact
		// published for the release being checked. The published checksum is
		// the download-integrity contract; its adjacent kernel manifest carries
		// the separate build-provenance information.
		//
		// FAILing on that would go red on every Mac in the field the moment a
		// release ships, which is the F8 pattern: a check that cries wolf
		// forever teaches the operator to skim the report and ignore the exit
		// code F7 exists to make meaningful. So this answers "did the operator
		// assert a release?" the same way checkVersions does, and reports
		// rather than asserts when the answer is no.
		if !releaseAsserted(e) {
			return warn(detail+" — NOT verified",
				"no --release was asserted, so this compared against whatever the newest release publishes "+
					"today ("+m.Release+"), and a Mac deliberately a release behind — or booting a locally "+
					"built kernel — differs for no fault of its own. Re-run as `sparkbox doctor --release "+
					"<the tag this Mac was set up with>` to assert a tag, or `sparkbox setup` to move this "+
					"Mac onto "+m.Release)
		}
		return fail(detail,
			"the machine is booting a kernel this release did not publish. If that is deliberate keep it; "+
				"otherwise delete the file and re-run `sparkbox setup` to fetch the published one")
	}
	return pass(fmt.Sprintf("%s (%s) verified against %s", path, size, m.Release))
}

// releaseAsserted reports whether anything in this run actually PINNED a
// release, and therefore whether a difference from what that release publishes
// is a fault rather than ordinary drift.
//
// Two ways to pin: the operator typed a concrete `--release v0.4.0` (doctor's
// flag defaults to "", and DefaultConfig's "latest" is not a pin either — both
// mean "assert nothing", which is why this goes through concreteVersion rather
// than a non-empty test), or a setup run already resolved a tag onto e.Manifest
// and is verifying the artifacts it just installed. The second case is the one
// where a mismatch really is broken: setup downloaded that kernel minutes ago.
func releaseAsserted(e *Env) bool {
	return concreteVersion(e.Cfg.Release) || concreteVersion(e.Manifest.Release)
}

// doctorManifestTimeout bounds doctor's one network call. NewHTTPFetcher uses
// http.DefaultClient, which has NO timeout, so without this a Mac on a
// captive-portal wifi would hang a read-only diagnostic indefinitely.
const doctorManifestTimeout = 15 * time.Second

// doctorManifest returns the release manifest, reusing one a setup run already
// resolved and otherwise fetching it.
//
// Deliberately returned rather than stored on e.Manifest. Publishing it would
// silently turn doctor's "no release asserted" (cfg.Release is empty unless the
// operator passed --release) into an assertion against whatever `latest` is
// today, and checkVersions would then FAIL every host that is deliberately a
// release behind. The only consumer is the outer-kernel checksum.
func doctorManifest(e *Env) (Manifest, error) {
	if e.Manifest.Release != "" {
		return e.Manifest, nil
	}
	if e.Fetch == nil {
		return Manifest{}, errors.New("no fetcher configured")
	}
	ctx, cancel := context.WithTimeout(e.Ctx, doctorManifestTimeout)
	defer cancel()
	rc, err := e.Fetch.Get(ctx, ManifestURL(e.Probe.GOOS(), e.Probe.GOARCH(), e.Cfg.ArtifactBase, e.Cfg.Release))
	if err != nil {
		return Manifest{}, err
	}
	defer rc.Close()
	m, err := ParseManifest(rc, e.Cfg.Release)
	if err != nil {
		return Manifest{}, err
	}
	// The same guard stepResolveRelease applies: a manifest for the wrong OS
	// parses perfectly and every checksum in it is right — for somebody else's
	// binaries.
	if err := m.CheckPlatform(e.Probe.GOOS()); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// DoctorHeader is the line `sparkbox doctor` prints above its report.
//
// On linux it is byte-for-byte what doctor has always printed. On darwin it
// must not be: cfg.Root is /srv/sparkbox, a path that does not exist on the Mac
// and never will — printing it as this host's sparkbox home would put a
// misleading fact at the top of a report whose whole job is telling two hosts
// apart.
func DoctorHeader(e *Env) string {
	if e != nil && e.Probe != nil && e.Probe.GOOS() == "darwin" {
		return fmt.Sprintf("sparkbox doctor — this Mac and machine %q (guest paths are inside it, under %s)",
			e.Cfg.machineName(), e.Cfg.Root)
	}
	return "sparkbox doctor — " + e.Cfg.Root
}
