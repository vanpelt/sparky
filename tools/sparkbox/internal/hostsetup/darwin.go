package hostsetup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine/appcontainer"
	macosassets "github.com/vanpelt/sparky/tools/sparkbox/macos"
)

// `sparkbox setup` on darwin.
//
// A Mac runs the SAME Provision — the same step loop, the same "already
// satisfied" reporting, the same --dry-run branches, the same
// verify-decides-the-exit-code rule — with a different step list, one level
// out. The gateway is not the Mac; it is a nested linux VM (Apple's `container
// machine --virtualization`, which is real KVM on Apple Silicon M3+), and the
// six steps below are what it takes to get from a bare laptop to a running
// gateway inside one.
//
// The division of labour is worth stating plainly, because it is what keeps
// this file small: the Mac fetches ONE artifact for itself (the outer kernel
// its hypervisor boots), builds ONE image, creates ONE VM, and then hands the
// whole job to a copy of this very binary running inside that VM. Everything
// about swap, XFS, systemd, users.conf, the packet filter and sluice is the
// inner setup's business, unchanged, on the filesystem that actually holds the
// gateway.

// Guest paths. Named once, here, because the bootstrap script bakes the same
// strings into the image and a disagreement would be invisible until runtime.
const (
	guestBootstrapDir = "/var/lib/sparkbox-bootstrap"
	// guestStagedBinary is where macos/sparkbox-bootstrap.sh leaves the
	// verified release binary. `setup` is run FROM here, and its own
	// install-binary step (A1) is what puts it at /usr/local/bin/sparkbox — so
	// the binary that runs the box and the release whose artifacts it fetched
	// are the same build by construction.
	guestStagedBinary = guestBootstrapDir + "/sparkbox"
	guestReleaseEnv   = guestBootstrapDir + "/release.env"
	guestBootstrapCmd = "/usr/local/sbin/sparkbox-bootstrap"
	guestOperatorKey  = "/run/sparkbox-operator.pub"
	guestSparkboxBin  = defaultBinPath
)

// machineBootTimeout bounds the wait for a freshly created machine to report
// running.
const machineBootTimeout = 3 * time.Minute

// darwinSteps is the ordered darwin pipeline.
func darwinSteps() []Step {
	return []Step{
		// Reused verbatim from the linux pipeline. It resolves
		// manifest-darwin-arm64.env (which works only because ManifestURL now
		// takes the platform from the Probe) and pins the concrete tag every
		// later URL and the inner --release are built from.
		stepResolveRelease(),
		stepOuterKernel(),
		stepMachineImage(),
		stepMachine(),
		stepMachineSparkbox(),
		stepProvisionInner(),
	}
}

// AttachMachineDriver gives a darwin Env its nested-VM driver, wrapping it for
// --dry-run. On any other platform it is a no-op.
//
// One function so `setup` and `doctor` cannot wire this differently — a doctor
// that talked to a different machine from the one setup provisioned would be
// worse than no doctor. A driver already on the Env (a test's fake) is kept,
// and still gets the dry-run wrapper, so the enforcement applies to fakes too.
func AttachMachineDriver(e *Env) error {
	if e.Probe == nil {
		e.Probe = System()
	}
	if e.Probe.GOOS() != "darwin" {
		return nil
	}
	if err := validateMachine(e.Cfg); err != nil {
		return err
	}
	if strings.TrimSpace(e.MacDir) == "" {
		// NewEnv derives this from os.UserHomeDir and swallows the error. An
		// empty value would make every Mac-side path RELATIVE — the outer kernel
		// would land in whatever directory the operator happened to be in, and
		// the evidence bundle with it.
		return fmt.Errorf("cannot locate this Mac's application-support directory " +
			"(os.UserHomeDir failed, so $HOME is probably unset); set HOME, or pass " +
			"--outer-kernel <path> and run from a directory you own")
	}
	if e.Machine == nil {
		e.Machine = appcontainer.New(appcontainer.NewCommander(e.Cfg.containerBin()))
	}
	if e.Cfg.DryRun {
		// Enforcement, not convention: a darwin step that forgets its own
		// dry-run guard gets ErrDryRun back instead of quietly building an image
		// or booting the operator's machine. See machine.DryRun.
		e.Machine = machine.DryRun(e.Machine)
	}
	return nil
}

// probeSkipped turns machine.ErrDryRun into an honest Satisfied note.
//
// The DryRun wrapper is the enforcement — it refuses the call — and this is the
// graceful landing: a step reports "not probed in --dry-run" rather than
// implying a check it did not make. Same discipline as the "release unresolved
// — shas unchecked" branches in the linux steps.
func probeSkipped(err error) bool { return errors.Is(err, machine.ErrDryRun) }

// --- 1. outer kernel ---------------------------------------------------------

// stepOuterKernel downloads the ONE artifact a Mac fetches for itself: the
// KVM-capable arm64 Image that `container machine` boots, inside which the
// gateway runs firecracker.
//
// Not to be confused with Cfg.KernelPath, the GUEST kernel a microVM boots.
// Two kernels ship in a release and confusing them is easy, which is why the
// manifest names this one for the platform rather than the arch.
func stepOuterKernel() Step {
	return Step{
		Name: "outer-kernel",
		Satisfied: func(e *Env) (bool, string, error) {
			path := e.Cfg.outerKernelPath(e.MacDir)
			fi, err := os.Stat(path)
			if err != nil || fi.Size() == 0 {
				return false, "", nil
			}
			if e.Manifest.Release == "" {
				// --dry-run: resolve-release only ever runs its Apply, so there
				// is no checksum to have compared against. Do not imply one.
				return true, "present at " + path + " (release unresolved — checksum unchecked)", nil
			}
			want := e.Manifest.SHA256OuterKernel
			if want == "" {
				// The manifest names no kernel; Apply refuses with the reason.
				return false, "", nil
			}
			have, herr := sha256File(path)
			if herr != nil {
				return false, "", fmt.Errorf("hash %s: %w", path, herr)
			}
			if have != want {
				return false, "", nil
			}
			return true, "matches " + e.Manifest.Release, nil
		},
		Plan: func(e *Env) string {
			path := e.Cfg.outerKernelPath(e.MacDir)
			if e.Manifest.Release == "" {
				return "download the macOS outer kernel → " + path
			}
			return fmt.Sprintf("download %s → %s (sha256 %s)",
				firstNonEmpty(e.Manifest.OuterKernelAsset, outerKernelName), path, short(e.Manifest.SHA256OuterKernel))
		},
		Apply: func(e *Env) error {
			if e.Manifest.Release == "" {
				return fmt.Errorf("no manifest resolved (resolve-release must run first)")
			}
			path := e.Cfg.outerKernelPath(e.MacDir)
			art, ok := e.Manifest.OuterKernel(e.Cfg.ArtifactBase, path)
			if !ok {
				// The manifest is the authority on what a release contains, so
				// this is a fact rather than a guess — and guessing the URL for
				// the kernel the hypervisor is about to boot would be the worst
				// place to start.
				return fmt.Errorf("release %s publishes no macOS outer kernel "+
					"(its manifest has no OUTER_KERNEL_ASSET/SHA256_OUTER_KERNEL), so there is nothing to download: "+
					"pin a release that ships one (--release <tag>), or build it locally with "+
					"`SPARKBOX_KERNEL_SOURCE=build ./macos/poc.sh build` and pass --outer-kernel <path>",
					e.Manifest.Release)
			}
			dl, err := downloadVerify(e.Ctx, e.Fetch, art)
			if err != nil {
				return err
			}
			verb := "present"
			if dl {
				verb = "downloaded"
			}
			e.logf("   outer kernel: %s (%s)\n", verb, path)
			return nil
		},
	}
}

// --- 2. machine image --------------------------------------------------------

// stepMachineImage builds the gateway image from the three files embedded in
// this binary.
//
// The image contains NO sparkbox (see macos/Containerfile.gateway): baking one
// is what let a machine built at v0.3.0 provision v0.4.0's artifacts and report
// PASS. It carries systemd, the tools the gateway needs, and two scripts.
func stepMachineImage() Step {
	return Step{
		Name: "machine-image",
		Satisfied: func(e *Env) (bool, string, error) {
			ref := machineImageRef(e.Cfg)
			ok, err := e.Machine.ImageExists(e.Ctx, ref)
			if err != nil {
				if probeSkipped(err) {
					return false, "", nil
				}
				return false, "", err
			}
			if !ok {
				return false, "", nil
			}
			// The tag IS the content hash, so its presence is a content
			// comparison — not the existence check that let every other stale
			// asset survive an upgrade.
			return true, ref + " present (tag is the context hash)", nil
		},
		Plan: func(e *Env) string {
			return fmt.Sprintf("build %s from %d embedded files (base %s)",
				machineImageRef(e.Cfg), len(macosassets.Files()), macosassets.UbuntuImage)
		},
		Apply: func(e *Env) error {
			ref := machineImageRef(e.Cfg)
			ctxDir := filepath.Join(e.MacDir, "image-context")
			// Rebuilt from scratch every time: the context is three small files
			// and a leftover from an older sparkbox would be silently baked in.
			if err := os.RemoveAll(ctxDir); err != nil {
				return err
			}
			if err := os.MkdirAll(ctxDir, 0o755); err != nil {
				return err
			}
			for _, f := range macosassets.Files() {
				if err := os.WriteFile(filepath.Join(ctxDir, f.Name), f.Body, os.FileMode(f.Mode)); err != nil {
					return err
				}
			}
			e.logf("   staged %d context files in %s\n", len(macosassets.Files()), ctxDir)
			spec := machine.BuildSpec{
				ContextDir: ctxDir,
				File:       filepath.Join(ctxDir, macosassets.ContainerfileName),
				Tag:        ref,
				Arch:       "arm64",
				BuildArgs:  map[string]string{"UBUNTU_IMAGE": macosassets.UbuntuImage},
				CPUs:       e.Cfg.MachineCPUs,
				MemoryGB:   8,
			}
			if err := e.Machine.BuildImage(e.Ctx, spec, prefixWriter(e.Log, "   build| ")); err != nil {
				return err
			}
			// `container build` has no structured output, so the only honest
			// confirmation that the tag now exists is to ask.
			ok, err := e.Machine.ImageExists(e.Ctx, ref)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("the build reported success but %s is still not present", ref)
			}
			e.logf("   image: %s\n", ref)
			return nil
		},
	}
}

// --- 3. the machine ----------------------------------------------------------

// machineIsOurs is the ownership predicate, ported from poc.sh's jq expression
// with its one hazard fixed: poc.sh compares `.[0].id == "sparkbox-poc"` as a
// HARDCODED literal, which was harmless only while the name was a constant.
//
// Three conditions, and homeMount is not cosmetic: it is what
// gateway-verify.sh's "no /Users virtiofs mount" assertion pairs with — a
// machine with the operator's home mounted in is a different security posture,
// not a cosmetic difference.
//
// The TAG is deliberately not part of it. The tag is content-addressed, so
// including it would force a machine DELETION on every edit to a baked script,
// and a machine whose scripts are one revision old is a soft problem: the
// sparkbox binary is fetched fresh on every provision. Tag drift is a WARN at
// verify, naming the destroy command, not a refusal.
func machineIsOurs(info machine.Info, cfg Config) bool {
	return info.Name == cfg.machineName() &&
		imageRepo(info.ImageRef) == machineImageRepo &&
		info.HomeMount == "none"
}

// imageRepo strips the tag from an image reference. Splitting on the LAST colon
// only when it comes after the last slash, so a registry with a port
// (host:5000/repo) is not mangled.
func imageRepo(ref string) string {
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon > slash {
		return ref[:colon]
	}
	return ref
}

func stepMachine() Step {
	return Step{
		Name: "machine",
		Satisfied: func(e *Env) (bool, string, error) {
			info, err := e.Machine.Inspect(e.Ctx, e.Cfg.machineName())
			switch {
			case errors.Is(err, machine.ErrNotFound):
				return false, "", nil
			case probeSkipped(err):
				return false, "", nil
			case err != nil:
				return false, "", err
			}
			if !machineIsOurs(info, e.Cfg) {
				// The deliberate THIRD state a bool loses, and the one poc.sh
				// needed `return 2` for at four call sites. Never auto-delete:
				// this VM may hold somebody's sandboxes.
				return false, "", fmt.Errorf("a machine named %q already exists but is not ours "+
					"(image %s, home-mount %s; expected the %s repository and home-mount none).\n"+
					"  setup will not delete it — it may hold running sandboxes.\n"+
					"  Provision under another name: sparkbox setup --machine-name <other>\n"+
					"  Or remove it deliberately: %s machine delete %s",
					info.Name, orDash(info.ImageRef), orDash(info.HomeMount), machineImageRepo,
					e.Cfg.containerBin(), info.Name)
			}
			if info.State == machine.StateRunning {
				return true, "reusing " + info.Name + " (" + info.IPAddress + ")", nil
			}
			return false, string(info.State), nil
		},
		Plan: func(e *Env) string {
			name := e.Cfg.machineName()
			info, err := e.Machine.Inspect(e.Ctx, name)
			switch {
			case err == nil && info.State == machine.StateRunning:
				return "adopt running machine " + name
			case err == nil:
				return "start existing machine " + name
			}
			return fmt.Sprintf("create machine %s (%d cpu, %dG, --virtualization, kernel %s, home-mount none) from %s",
				name, e.Cfg.MachineCPUs, e.Cfg.MachineMemGB, e.Cfg.outerKernelPath(e.MacDir), machineImageRef(e.Cfg))
		},
		Apply: func(e *Env) error {
			name := e.Cfg.machineName()
			info, err := e.Machine.Inspect(e.Ctx, name)
			switch {
			case errors.Is(err, machine.ErrNotFound):
				spec := machine.Spec{
					Name: name, Image: machineImageRef(e.Cfg),
					KernelPath: e.Cfg.outerKernelPath(e.MacDir),
					HomeMount:  "none",
					CPUs:       e.Cfg.MachineCPUs, MemoryGB: e.Cfg.MachineMemGB,
					// Not the default: `container system property list` reports
					// `[machine] virtualization = false`, and a machine created
					// without it boots fine and can never run firecracker.
					Virtualization: true,
				}
				if err := e.Machine.Create(e.Ctx, spec); err != nil {
					return err
				}
				e.logf("   created machine %s\n", name)
			case err != nil:
				return err
			default:
				if err := e.Machine.Start(e.Ctx, name); err != nil {
					return err
				}
				e.logf("   started machine %s (was %s)\n", name, info.State)
			}
			return waitRunning(e, name)
		},
	}
}

// waitRunning polls Inspect until the machine reports running.
//
// Inspect, never Exec: `container machine run` BOOTS a stopped machine ("failed
// to boot container machine" is literally its miss path), so using it as a
// readiness probe would make the probe change what it measures.
func waitRunning(e *Env, name string) error {
	deadline := time.Now().Add(machineBootTimeout)
	for {
		info, err := e.Machine.Inspect(e.Ctx, name)
		if err == nil && info.State == machine.StateRunning {
			e.logf("   machine %s is running (%s)\n", name, info.IPAddress)
			return nil
		}
		if time.Now().After(deadline) {
			state := "unknown"
			if err == nil {
				state = string(info.State)
			}
			return fmt.Errorf("machine %s did not reach running within %s (state %s); "+
				"see `%s machine logs %s --boot`", name, machineBootTimeout, state, e.Cfg.containerBin(), name)
		}
		e.Probe.Sleep(2 * time.Second)
	}
}

// --- 4. the machine's sparkbox binary ---------------------------------------

// stepMachineSparkbox stages the released linux sparkbox inside the machine.
//
// The machine fetches it ITSELF, from a URL and a checksum the Mac pinned, by
// running the bootstrap script baked into the image (B3). That is deliberate:
// the alternatives are pushing 24 MB through a transport that deadlocks above
// ~192 KiB, or `container cp`, whose host→machine direction is unverified. What
// the Mac contributes is the pin — a concrete tag, never "latest" — and the
// cross-check afterwards.
func stepMachineSparkbox() Step {
	return Step{
		Name: "machine-sparkbox",
		Satisfied: func(e *Env) (bool, string, error) {
			if e.Manifest.Release == "" {
				return false, "", nil // dry run: nothing to compare against
			}
			art, ok := e.Manifest.MachineSparkbox(e.Cfg.ArtifactBase, guestStagedBinary)
			if !ok {
				// Apply refuses with the full reason; Satisfied only has to say
				// "not done".
				return false, "", nil
			}
			kv, err := readGuestReleaseEnv(e)
			if err != nil {
				if probeSkipped(err) {
					return false, "not probed in --dry-run", nil
				}
				return false, "", err
			}
			// Hoisted out of the bootstrap script so the plan line is truthful:
			// poc.sh buries this comparison inside the bootstrap, so a re-run
			// always re-enters it and always claims to be doing work.
			if kv["VERSION"] == e.Manifest.Release && kv["SHA256_SPARKBOX"] == art.SHA256 {
				return true, "sparkbox " + kv["VERSION"] + " staged in " + e.Cfg.machineName(), nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			tag := e.Manifest.Release
			if tag == "" {
				tag = e.Cfg.Release
			}
			return fmt.Sprintf("stage sparkbox %s in %s from %s (verified against the release manifest)",
				tag, e.Cfg.machineName(), e.Cfg.ArtifactBase)
		},
		Apply: func(e *Env) error {
			if e.Manifest.Release == "" {
				return fmt.Errorf("no manifest resolved (resolve-release must run first)")
			}
			art, ok := e.Manifest.MachineSparkbox(e.Cfg.ArtifactBase, guestStagedBinary)
			if !ok {
				return fmt.Errorf("release %s publishes no linux sparkbox for the machine "+
					"(its manifest has no MACHINE_SPARKBOX_ASSET/SHA256_MACHINE_SPARKBOX), "+
					"so there is no verified binary to install: pin a release that ships one (--release <tag>)",
					e.Manifest.Release)
			}
			// The RESOLVED tag, never "latest": the bootstrap would otherwise
			// resolve its own latest and could land on a different release from
			// the one whose outer kernel this Mac just downloaded.
			res, err := e.Machine.Exec(e.Ctx, machine.ExecSpec{
				Machine: e.Cfg.machineName(), Op: "bootstrap",
				Script: scriptBootstrap,
				Env: map[string]string{
					"SPARKBOX_BOOTSTRAP":     guestBootstrapCmd,
					"SPARKBOX_RELEASE_TAG":   e.Manifest.Release,
					"SPARKBOX_ARTIFACT_BASE": e.Cfg.ArtifactBase,
				},
				Stream:  prefixWriter(e.Log, "   machine| "),
				Timeout: 10 * time.Minute,
			})
			e.evidence("bootstrap.txt", res.Stdout, res.Stderr)
			if err != nil {
				return fmt.Errorf("could not stage the released sparkbox in machine %q: %w\n"+
					"  the machine needs outbound HTTPS to %s for this step",
					e.Cfg.machineName(), err, e.Cfg.ArtifactBase)
			}

			// Read the provenance back OUT rather than inferring it from what
			// was requested — and check it against the manifest the MAC pinned.
			// macos/sparkbox-bootstrap.sh's header describes this as an
			// invariant between the two halves; nothing enforced it across them
			// until now.
			kv, err := readGuestReleaseEnv(e)
			if err != nil {
				return err
			}
			if kv["VERSION"] == "" {
				return fmt.Errorf("%s is empty or unreadable after the bootstrap ran, "+
					"so what got staged in %s is unknown", guestReleaseEnv, e.Cfg.machineName())
			}
			if kv["VERSION"] != e.Manifest.Release {
				return fmt.Errorf("the machine staged sparkbox %s but this Mac pinned release %s: "+
					"the binary and the artifacts it will fetch would be from different builds "+
					"(that skew is exactly what the bootstrap exists to prevent)",
					kv["VERSION"], e.Manifest.Release)
			}
			if kv["SHA256_SPARKBOX"] != art.SHA256 {
				return fmt.Errorf("the machine staged a sparkbox with sha256 %s but the darwin manifest names %s "+
					"for the same release — the two manifests disagree about what %s contains",
					short(kv["SHA256_SPARKBOX"]), short(art.SHA256), e.Manifest.Release)
			}
			e.logf("   staged sparkbox %s (sha256 %s) in %s\n", kv["VERSION"], short(art.SHA256), e.Cfg.machineName())
			return nil
		},
	}
}

// readGuestReleaseEnv reads the bootstrap's result file out of the machine. An
// ABSENT file is not an error — that is a machine that has never been
// bootstrapped — so the script tolerates it and returns nothing.
func readGuestReleaseEnv(e *Env) (map[string]string, error) {
	res, err := e.Machine.Exec(e.Ctx, machine.ExecSpec{
		Machine: e.Cfg.machineName(), Op: "read-release-env",
		Script: scriptReadReleaseEnv, ReadOnly: true,
		Env:     map[string]string{"SPARKBOX_RELEASE_ENV": guestReleaseEnv},
		Timeout: time.Minute,
	})
	if err != nil {
		return nil, err
	}
	return parseEnv(strings.NewReader(string(res.Stdout)))
}

// --- 5. the inner setup ------------------------------------------------------

// stepProvisionInner runs `sparkbox setup` inside the machine.
//
// Never satisfied: the inner setup is itself idempotent and Satisfied-gated per
// step, so re-running it is the point — it is how a changed --proxy-domain or a
// new --sluice reaches a machine that already exists.
func stepProvisionInner() Step {
	return Step{
		Name: "provision-inner",
		Satisfied: func(e *Env) (bool, string, error) {
			return false, "", nil
		},
		Plan: func(e *Env) string {
			tag := e.Manifest.Release
			if tag == "" {
				tag = e.Cfg.Release
			}
			args, err := innerSetupArgs(e.Cfg, tag, guestOperatorKey)
			if err != nil {
				return "REFUSED: " + err.Error()
			}
			// The exact inner command line is most of the value of a dry run on
			// a Mac: it is the thing an operator cannot otherwise see.
			return "in machine " + e.Cfg.machineName() + ": " + guestStagedBinary + " " + strings.Join(args, " ")
		},
		Apply: func(e *Env) error {
			if e.Manifest.Release == "" {
				return fmt.Errorf("no manifest resolved (resolve-release must run first)")
			}
			args, err := innerSetupArgs(e.Cfg, e.Manifest.Release, guestOperatorKey)
			if err != nil {
				return err
			}

			// The operator's public key: staged over stdin-as-env, written 0600
			// inside the machine, and removed UNCONDITIONALLY — including on
			// failure, and before the exit status is returned.
			if e.Cfg.Gateway == "" {
				keyText, kerr := e.operatorKeyLine()
				if kerr != nil {
					return kerr
				}
				// operatorKeyLine returns "<handle> <key>"; the machine wants
				// just the key, since --operator-handle carries the other half.
				_, key, _ := strings.Cut(keyText, " ")
				if _, err := e.Machine.Exec(e.Ctx, machine.ExecSpec{
					Machine: e.Cfg.machineName(), Op: "push-operator-key",
					Script: scriptPushOperatorKey,
					Env: map[string]string{
						"SPARKBOX_KEY_PATH":     guestOperatorKey,
						"SPARKBOX_OPERATOR_KEY": key,
					},
					Timeout: time.Minute,
				}); err != nil {
					return err
				}
				defer func() {
					if _, err := e.Machine.Exec(e.Ctx, machine.ExecSpec{
						Machine: e.Cfg.machineName(), Op: "remove-operator-key",
						Script:  scriptRemoveOperatorKey,
						Env:     map[string]string{"SPARKBOX_KEY_PATH": guestOperatorKey},
						Timeout: time.Minute,
					}); err != nil {
						e.logf("   WARNING: could not remove %s from the machine: %v\n", guestOperatorKey, err)
					}
				}()
			}

			e.logf("   running: %s %s\n", guestStagedBinary, strings.Join(args, " "))
			var transcript strings.Builder
			res, err := e.Machine.Exec(e.Ctx, machine.ExecSpec{
				Machine: e.Cfg.machineName(), Op: "inner-setup",
				Script: scriptInnerSetup,
				Env: map[string]string{
					"SPARKBOX_BIN":      guestStagedBinary,
					"SPARKBOX_ARGV_B64": machine.EncodeArgv(args),
					"SPARKBOX_ARGV_N":   fmt.Sprint(len(args)),
				},
				// Live, prefixed, because the inner setup downloads multi-GB
				// artifacts and the prefix is how an operator tells which layer
				// is speaking (both print "== step ==" headers).
				Stream:  io.MultiWriter(prefixWriter(e.Log, "   machine| "), &transcript),
				Timeout: 90 * time.Minute,
			})
			e.evidence("inner-setup.txt", []byte(transcript.String()), res.Stderr)
			if err != nil {
				// Evidence FIRST, then the error. poc.sh returns setup's exit
				// code before collecting anything, which leaves whoever has to
				// debug a failed provision with the one file they already saw
				// scroll past.
				e.collectFailureEvidence()
				return innerSetupError(e, err)
			}

			// The check whose absence WAS F0: what is at /usr/local/bin/sparkbox
			// after setup ran, and is it the release we pinned?
			return reconcileMachineBinary(e)
		},
	}
}

// innerSetupError turns a failed inner run into a message that names the next
// command, per this package's convention.
func innerSetupError(e *Env, err error) error {
	name := e.Cfg.machineName()
	var ee *machine.ExitError
	switch {
	case errors.As(err, &ee) && ee.Code == 127:
		return fmt.Errorf("the inner setup could not be executed inside machine %q (exit 127): "+
			"%s is missing or not executable. The machine-sparkbox step staged it moments ago, "+
			"so something removed it — re-run, and if it recurs delete the machine (%s machine delete %s)",
			name, guestStagedBinary, e.Cfg.containerBin(), name)
	case errors.As(err, &ee):
		return fmt.Errorf("inner setup failed inside machine %q (exit %d)\n"+
			"  transcript: %s\n"+
			"  journal:    %s machine run --name %s --root -- journalctl -u sparkbox -n 100 --no-pager",
			name, ee.Code, e.evidencePath("inner-setup.txt"), e.Cfg.containerBin(), name)
	case errors.Is(err, machine.ErrTransport):
		return fmt.Errorf("machine %q did not run the inner setup at all: %w\n"+
			"  Nothing was provisioned. This is a transport fault, not a setup failure — "+
			"report it with the `container` version from `%s system version`",
			name, err, e.Cfg.containerBin())
	case errors.Is(err, machine.ErrUnsupported):
		return fmt.Errorf("your Apple Container build does not accept the arguments sparkbox needs "+
			"(it needs >= %s): %w", minContainerVersion, err)
	}
	return fmt.Errorf("inner setup inside machine %q: %w", name, err)
}

// collectFailureEvidence gathers what a human will want after a failed inner
// setup. Best-effort by design: each of these can itself fail on a machine that
// is in a bad way, and a failure to collect evidence must not replace the
// error that caused it.
func (e *Env) collectFailureEvidence() {
	res, err := e.Machine.Exec(e.Ctx, machine.ExecSpec{
		Machine: e.Cfg.machineName(), Op: "inner-journal",
		Script: scriptInnerJournal, ReadOnly: true, Timeout: 2 * time.Minute,
	})
	if err == nil {
		e.evidence("inner-journal.txt", res.Stdout, res.Stderr)
	}
	if info, ierr := e.Machine.Inspect(e.Ctx, e.Cfg.machineName()); ierr == nil {
		e.evidence("machine.txt", []byte(fmt.Sprintf(
			"name=%s\ncontainer_id=%s\nimage=%s\nhome_mount=%s\nstate=%s\nip=%s\ncpus=%d\nmemory_bytes=%d\n",
			info.Name, info.ContainerID, info.ImageRef, info.HomeMount, info.State, info.IPAddress,
			info.CPUs, info.MemoryBytes)), nil)
	}
	if dir := e.EvidenceDir; dir != "" {
		e.logf("   evidence for this failed run: %s\n", dir)
	}
}

// reconcileMachineBinary asks the machine what actually ended up at
// /usr/local/bin/sparkbox, and refuses a mismatch.
//
// This is poc.sh's reconcile_installed_binary, and its absence is precisely
// what let a v0.3.0 binary provision v0.4.0's artifacts and report PASS. Asking
// the binary is the only honest answer: the tag is linked in with -X
// main.version and nothing on disk records it.
func reconcileMachineBinary(e *Env) error {
	res, err := e.Machine.Exec(e.Ctx, machine.ExecSpec{
		Machine: e.Cfg.machineName(), Op: "reconcile-binary",
		Script: scriptReconcileBinary, ReadOnly: true,
		Env:     map[string]string{"SPARKBOX_BIN": guestSparkboxBin},
		Timeout: time.Minute,
	})
	if err != nil {
		return fmt.Errorf("there is no runnable %s in machine %q after setup ran, "+
			"so its install-binary step did not happen: %w", guestSparkboxBin, e.Cfg.machineName(), err)
	}
	got := versionFromBanner(string(res.Stdout))
	if got == "" {
		return fmt.Errorf("%s in machine %q did not report a version (`sparkbox version` said %q)",
			guestSparkboxBin, e.Cfg.machineName(), firstLine(string(res.Stdout)))
	}
	if got != e.Manifest.Release {
		return fmt.Errorf("machine %q is running sparkbox %s but this run provisioned release %s: "+
			"the gateway would drive %s's kernel, firecracker and rootfs with a %s control plane.\n"+
			"  Delete the machine and provision again: %s machine delete %s",
			e.Cfg.machineName(), got, e.Manifest.Release, e.Manifest.Release, got,
			e.Cfg.containerBin(), e.Cfg.machineName())
	}
	e.logf("   binary check: %s is %s, the release that provisioned it\n", guestSparkboxBin, got)
	return nil
}

// versionFromBanner parses "sparkbox <tag> (<os>/<arch>)" — the same shape
// binaryVersion reads, done here because the parsing belongs on the Mac rather
// than in an awk inside a guest script.
func versionFromBanner(out string) string {
	f := strings.Fields(firstLine(strings.TrimSpace(out)))
	if len(f) < 2 || f[0] != "sparkbox" {
		return ""
	}
	return f[1]
}

// --- guest scripts -----------------------------------------------------------
//
// Bodies only: machine.WrapScript adds `set -euo pipefail`, the begin marker
// and the exit-status trap, so no call site can forget them. Every value these
// read arrives through ExecSpec.Env (inherit-by-name), never on a command line
// the guest's bash would re-parse. Golden copies live in
// testdata/scripts/*.golden — the Op-keyed fake cannot notice an edit here, and
// those files are also what a human reviews for quoting bugs.

const scriptReadReleaseEnv = `# Absent is not an error: that is a machine nobody has bootstrapped yet.
if [ -r "$SPARKBOX_RELEASE_ENV" ]; then
  cat "$SPARKBOX_RELEASE_ENV"
fi
`

const scriptBootstrap = `"$SPARKBOX_BOOTSTRAP" release "$SPARKBOX_RELEASE_TAG" "$SPARKBOX_ARTIFACT_BASE"
`

const scriptPushOperatorKey = `umask 077
printf '%s\n' "$SPARKBOX_OPERATOR_KEY" > "$SPARKBOX_KEY_PATH"
chmod 0600 "$SPARKBOX_KEY_PATH"
`

const scriptRemoveOperatorKey = `rm -f "$SPARKBOX_KEY_PATH"
`

// scriptInnerSetup reassembles the flag vector from its base64 NUL-joined form
// and runs setup from the STAGED binary — not from /usr/local/bin/sparkbox,
// which is what setup's own install-binary step is about to create.
const scriptInnerSetup = machine.DecodeArgvSnippet + `"$SPARKBOX_BIN" "${args[@]}"
`

const scriptReconcileBinary = `"$SPARKBOX_BIN" version
`

// scriptInnerJournal is best-effort evidence: `|| true` so a machine without a
// working journal still yields the rest of the bundle.
const scriptInnerJournal = `journalctl --no-pager -n 200 -u sparkbox-net.service -u sparkbox.service 2>&1 || true
`

// scriptGuestFacts reports, in one round trip, the three things
// gateway-verify.sh asserts that no existing check covers.
const scriptGuestFacts = `printf 'kernel_release=%s\n' "$(uname -r)"
printf 'virtiofs_targets=%s\n' "$(findmnt -rn -t virtiofs -o TARGET 2>/dev/null | tr '\n' ',' || true)"
if [ -c /dev/net/tun ]; then printf 'tun=present\n'; else printf 'tun=absent\n'; fi
`

const scriptInnerDoctor = `if [ -n "${SPARKBOX_DOCTOR_GATEWAY:-}" ]; then
  "$SPARKBOX_BIN" doctor --gateway "$SPARKBOX_DOCTOR_GATEWAY"
else
  "$SPARKBOX_BIN" doctor
fi
`

// --- evidence ----------------------------------------------------------------

// evidence writes one artifact into this run's bundle. Lazily created, never in
// --dry-run, never overwritten (each run gets its own timestamped directory),
// and — unlike poc.sh — written even when the run FAILS, which is the run whose
// evidence anyone actually needs.
func (e *Env) evidence(name string, stdout, stderr []byte) {
	if e.Cfg.DryRun || e.MacDir == "" {
		return
	}
	if e.EvidenceDir == "" {
		e.EvidenceDir = filepath.Join(e.MacDir, "results", time.Now().UTC().Format("20060102T150405Z"))
	}
	if err := os.MkdirAll(e.EvidenceDir, 0o755); err != nil {
		e.logf("   note: could not create the evidence directory %s: %v\n", e.EvidenceDir, err)
		return
	}
	body := stdout
	if len(stderr) > 0 {
		body = append(append(append([]byte{}, stdout...), []byte("\n--- stderr ---\n")...), stderr...)
	}
	path := filepath.Join(e.EvidenceDir, name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		e.logf("   note: could not write %s: %v\n", path, err)
	}
}

func (e *Env) evidencePath(name string) string {
	if e.EvidenceDir == "" {
		return "(no evidence directory)"
	}
	return filepath.Join(e.EvidenceDir, name)
}

// --- connect banner ----------------------------------------------------------

// printConnectDarwin is the darwin banner, and it is not optional politeness.
//
// The linux banner tells the operator `journalctl -u sparkbox -f`, which on a
// Mac names a journald that does not exist — a green report ending in an
// instruction that cannot work is F7's genus in a cheap costume.
func (e *Env) printConnectDarwin() {
	name := e.Cfg.machineName()
	ip := "<machine-ip>"
	if info, err := e.Machine.Inspect(e.Ctx, name); err == nil && info.IPAddress != "" {
		ip = info.IPAddress
	}
	if e.Cfg.Gateway != "" {
		node := e.Cfg.NodeName
		if node == "" {
			node = "<machine-hostname>"
		}
		e.logf("\n== sparkbox fleet node is provisioned (in machine %s) ==\n", name)
		e.logf("  node:               %s\n", node)
		e.logf("  gateway:            %s\n", e.Cfg.Gateway)
		e.logf("  approve at gateway: ssh ctl@<gateway> node approve <SHA256:...>\n")
		e.logf("  health check:       sparkbox doctor --gateway %s\n", e.Cfg.Gateway)
		e.logf("  logs:               %s machine run --name %s --root -- journalctl -u sparkbox -f\n",
			e.Cfg.containerBin(), name)
		return
	}
	_, port, err := splitAddr(e.Cfg.sshAddr())
	if err != nil {
		port = 2222
	}
	if e.Cfg.SSHAdvertisePort > 0 {
		port = e.Cfg.SSHAdvertisePort
	}
	e.logf("\n== sparkbox is provisioned (in machine %s) ==\n", name)
	e.logf("  machine:           %s at %s (%s machine ls)\n", name, ip, e.Cfg.containerBin())
	e.logf("  create a sandbox:  ssh -p %d new@%s\n", port, ip)
	e.logf("  shell into it:     ssh -p %d <name>@%s\n", port, ip)
	e.logf("  health check:      sparkbox doctor\n")
	e.logf("  logs:              %s machine run --name %s --root -- journalctl -u sparkbox -f\n",
		e.Cfg.containerBin(), name)
	scheme := "http"
	if e.Cfg.ProxyTLS {
		scheme = "https"
	}
	e.logf("  web routes:        %s://<name>.%s — but ONLY if something on this Mac resolves\n", scheme, e.Cfg.ProxyDomain)
	e.logf("                     *.%s to %s (an /etc/hosts line, a local resolver, or a tunnel).\n", e.Cfg.ProxyDomain, ip)
	e.logf("                     The machine's address is private to this laptop; nothing outside it can reach %s.\n", ip)
	if e.EvidenceDir != "" {
		e.logf("  evidence:          %s\n", e.EvidenceDir)
	}
}

// --- small helpers -----------------------------------------------------------

// prefixWriter tags every line of a streamed sub-process with a prefix, so an
// operator can tell the machine's output from the Mac's. Both layers print
// "== step ==" headers; without this they interleave into one confusing report.
func prefixWriter(w io.Writer, prefix string) io.Writer {
	if w == nil {
		return io.Discard
	}
	return &linePrefixer{w: w, prefix: prefix, atLineStart: true}
}

type linePrefixer struct {
	w           io.Writer
	prefix      string
	atLineStart bool
}

func (p *linePrefixer) Write(b []byte) (int, error) {
	for _, c := range b {
		if p.atLineStart {
			if _, err := io.WriteString(p.w, p.prefix); err != nil {
				return 0, err
			}
			p.atLineStart = false
		}
		if _, err := p.w.Write([]byte{c}); err != nil {
			return 0, err
		}
		if c == '\n' {
			p.atLineStart = true
		}
	}
	return len(b), nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
