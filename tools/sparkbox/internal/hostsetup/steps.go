package hostsetup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/deploy"
	xssh "golang.org/x/crypto/ssh"
)

// Runner executes host commands. The production runner shells out; tests inject
// a fake that records calls and returns canned output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Env carries everything a setup step needs. System paths are fields (not
// constants) so tests can redirect them into a tempdir; Config.Root already
// redirects the sparkbox home.
type Env struct {
	Ctx      context.Context
	Cfg      Config
	Run      Runner
	Fetch    Fetcher
	Probe    Probe
	Listen   Listener
	Log      io.Writer
	Manifest Manifest

	SystemdDir string // /etc/systemd/system
	SysctlDir  string // /etc/sysctl.d
	SbinDir    string // /usr/local/sbin
	// SluiceBinPath is where stepSluice installs the egress gateway, and the
	// path its unit's ExecStart names. A field for the same reason as the
	// directories above rather than a Config knob: no operator has ever needed
	// to move it, but a test that could not redirect it would download to (and
	// chmod +x) the developer's real /usr/local/bin/sluice.
	SluiceBinPath string
	FstabPath     string // /etc/fstab
	SwapPath      string // /swapfile
	SSHDConfD     string // /etc/ssh/sshd_config.d
	HomeDir       string // operator-key auto-detect root (~)
	// SelfPath is the binary running this setup — the thing stepInstallBinary
	// copies to Cfg.BinPath. It is a field, filled once in NewEnv, rather than
	// an os.Executable() call inside the step, because inside a test that call
	// answers "the go test binary" and the step would happily hash and install
	// *that*.
	SelfPath string

	// BinaryInstalled records that stepInstallBinary replaced the binary on
	// disk. Steps talk to each other through the shared Env (stepResolveRelease
	// already publishes the manifest this way); this flag exists because a
	// running service keeps executing the *old* inode after the rename, so
	// enable-services has to restart rather than merely start it.
	BinaryInstalled bool

	// UnitsChanged records that stepSystemdUnits (re)wrote a unit file. Same
	// contract as BinaryInstalled and for the same reason: systemd serves the
	// text it parsed at the last daemon-reload, and a running unit keeps the
	// ExecStart it started with, so enable-services has to reload and restart
	// rather than find the unit "already active" and walk away.
	UnitsChanged bool

	// EnvChanged records that stepEnvFile rewrote a managed key in sparkbox.env.
	// That file is the EnvironmentFile= of BOTH units, and systemd reads an
	// EnvironmentFile once, at unit start — so a corrected PROXY_DOMAIN or
	// PROXY_PORT sat on disk doing nothing until the next reboot while setup
	// printed a green banner advertising the value it had just written.
	EnvChanged bool

	// NetChanged records that the packet-filter assets changed:
	// /usr/local/sbin/sparkbox-net.sh, the sysctl drop-in, or sparkbox-net.service
	// itself. sparkbox-net.service is Type=oneshot + RemainAfterExit=yes, so
	// `systemctl enable --now` is a no-op on any host that has ever booted with
	// it — which meant every packet-filter fix (F8's chain names, for one)
	// reached the disk and never the rules until somebody rebooted the box.
	NetChanged bool

	// SluiceChanged records that stepSluice installed or updated the egress
	// gateway (its binary, its unit, or its seed files). Same contract as
	// UnitsChanged, and it names a DIFFERENT unit: enable-services has to
	// enable and restart sluice.service, which no other flag here implies.
	SluiceChanged bool

	// AdoptedLegacy records that reconcileLayout repointed Cfg at a state (and
	// image) directory that was already live on this host, rather than the
	// layout DefaultConfig describes. It is reported in the connect banner: an
	// operator who adopts a layout should be told which one they are running,
	// because every later `setup` on that host needs the same flag.
	AdoptedLegacy bool
}

// NewEnv builds an Env with the real on-host system paths.
func NewEnv(ctx context.Context, cfg Config, run Runner, fetch Fetcher, log io.Writer) *Env {
	home, _ := os.UserHomeDir()
	// EvalSymlinks so a symlinked invocation (/usr/bin/sparkbox -> …) installs
	// the real binary rather than hashing and copying the link's target path
	// under a different identity. Both errors are swallowed the way HomeDir's
	// is: the install step reports a missing SelfPath far more usefully than a
	// constructor that cannot return one.
	self, _ := os.Executable()
	if self != "" {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
	}
	return &Env{
		Ctx: ctx, Cfg: cfg, Run: run, Fetch: fetch, Log: log,
		Probe:         System(),
		Listen:        NewNetListener(),
		SystemdDir:    "/etc/systemd/system",
		SysctlDir:     "/etc/sysctl.d",
		SbinDir:       "/usr/local/sbin",
		SluiceBinPath: sluiceBinPath,
		FstabPath:     "/etc/fstab",
		SwapPath:      "/swapfile",
		SSHDConfD:     "/etc/ssh/sshd_config.d",
		HomeDir:       home,
		SelfPath:      self,
	}
}

// Step is one idempotent unit of provisioning: Satisfied reports whether its
// effect is already present (so Apply is skipped), Plan describes what Apply
// would do (for --dry-run), and Apply performs the mutation.
type Step struct {
	Name      string
	Satisfied func(*Env) (bool, string, error)
	Plan      func(*Env) string
	Apply     func(*Env) error
}

func (e *Env) logf(format string, a ...any) {
	if e.Log != nil {
		fmt.Fprintf(e.Log, format, a...)
	}
}

func (e *Env) run(name string, args ...string) ([]byte, error) {
	return e.Run.Run(e.Ctx, name, args...)
}

// Provision runs preflight, resolves the release, and applies every step in
// order. In --dry-run it prints the plan and mutates nothing. On success (real
// run) it prints a verification report and connect instructions.
func Provision(e *Env) error {
	if e.Probe == nil {
		e.Probe = System()
	}
	if e.Listen == nil {
		e.Listen = NewNetListener()
	}
	// Contradictory or unparseable listen addresses are a usage error, not a
	// host problem: catch them before the plan is even described, because every
	// one of them ends up as a systemd ExecStart word or an iptables argument
	// where the failure reads as a crash loop rather than as a typo.
	if err := validateAddrs(e.Cfg); err != nil {
		return err
	}
	// Same argument for the optional subsystems, plus one of its own: several of
	// these are silently ignored by `serve` when given in half (an
	// --archive-remote with no bucket disables archiving without an error), so a
	// host would come up green and simply not do what was asked.
	if err := validateSubsystems(e.Cfg); err != nil {
		return err
	}
	// Where does this host's live state ACTUALLY sit? A populated state
	// directory anywhere other than Cfg.StateDir means this run would build a
	// second data root beside a working one (F4) — so adopt it or refuse, and do
	// it here: it rewrites the paths every plan line, unit and download
	// destination below is derived from, and it must be answered before the
	// operator is shown a plan that describes the wrong host.
	if err := reconcileLayout(e); err != nil {
		return err
	}
	// The same question one level up: is this host already the OTHER kind of
	// sparkbox? The gateway/node role lives in sparkbox.env's GATEWAY_FLAG,
	// which setup does not rewrite, so provisioning across it would leave half
	// of each.
	if err := checkRoleSwitch(e); err != nil {
		return err
	}
	// Preflight: the host-capability subset must pass before we touch anything.
	pre := RunChecks(e.Probe, e.Cfg, preflightChecks())
	if AnyFail(pre) {
		if e.Cfg.DryRun {
			e.logf("preflight (advisory in --dry-run):\n")
			PrintResults(e.Log, pre)
		} else {
			e.logf("preflight failed — host is not ready:\n")
			PrintResults(e.Log, pre)
			return fmt.Errorf("preflight failed; fix the FAILs above or run `sparkbox doctor`")
		}
	}

	steps := allSteps()
	if e.Cfg.DryRun {
		e.logf("plan for %s (release %s, domain %s):\n", e.Cfg.Root, e.Cfg.Release, e.Cfg.ProxyDomain)
	} else {
		e.logf("== port-preflight ==\n")
	}
	// Ports before anything expensive. A busy port is the single most common
	// way a provisioned gateway fails, it fails at *boot* (where it reads as a
	// systemd problem rather than a config one), and it costs milliseconds to
	// detect — so detect it before the multi-GB artifact download, not after
	// the unit that names it has been written and started.
	if err := preflightPorts(e); err != nil {
		return err
	}
	for _, s := range steps {
		sat, note, err := s.Satisfied(e)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		if e.Cfg.DryRun {
			if sat {
				e.logf("  - %-16s ✓ already satisfied%s\n", s.Name, suffix(note))
			} else {
				e.logf("  - %-16s → %s\n", s.Name, s.Plan(e))
			}
			continue
		}
		if sat {
			e.logf("== %s: already satisfied%s ==\n", s.Name, suffix(note))
			continue
		}
		e.logf("== %s ==\n", s.Name)
		if err := s.Apply(e); err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
	}

	if e.Cfg.DryRun {
		e.logf("\ndry run — nothing was changed.\n")
		return nil
	}

	// Verify, and let the verdict decide the exit code. Printing the report and
	// then returning nil regardless — which is what this used to do — is how
	// `setup` came to announce "== sparkbox is provisioned ==" over a gateway
	// that had never once stayed up (F7). An operator who sees that walks away.
	e.logf("\n== verify ==\n")
	res := RunChecks(e.Probe, e.Cfg, DefaultChecks())
	PrintResults(e.Log, res)
	if AnyFail(res) {
		e.logf("\n== sparkbox is NOT healthy ==\n")
		e.logf("  every provisioning step ran, but the checks above found a hard failure.\n")
		e.logf("  logs: journalctl -u sparkbox -n 100 --no-pager\n")
		e.logf("  re-check after fixing: sparkbox doctor\n")
		return fmt.Errorf("provisioning completed but the host is not healthy: %d check(s) FAILED (see above)", countFail(res))
	}
	e.printConnect()
	return nil
}

func countFail(results []Result) int {
	n := 0
	for _, r := range results {
		if r.Status == Fail {
			n++
		}
	}
	return n
}

func suffix(note string) string {
	if note == "" {
		return ""
	}
	return " (" + note + ")"
}

// preflightChecks is the host-capability subset run before provisioning: these
// cannot be fixed by setup itself, so they gate the run.
func preflightChecks() []Check {
	return []Check{
		{"operating system", checkOS},
		{"cpu architecture", checkArch},
		{"root privileges", checkRoot},
		{"kvm device", checkKVM},
		{"hardware virtualization", checkVirt},
	}
}

// allSteps is the ordered provisioning pipeline, ported from the shell provisioner
// in deploy/cloud-init.yaml minus the Scaleway Secret Manager / flexible-IP steps.
func allSteps() []Step {
	return []Step{
		stepSwap(),
		stepResolveRelease(),
		// Before the multi-GB artifact download (a read-only /usr/local/bin
		// should fail in seconds, not after a rootfs), and necessarily before
		// the unit that names the installed path is written or started.
		stepInstallBinary(),
		stepDataVolume(),
		stepFetchArtifacts(),
		stepUsersConf(),
		stepEnvFile(),
		stepNetAssets(),
		// Before systemd-units and enable-services, because the gateway's unit
		// carries --sluice-socket and --guest-dns and enable-services starts
		// both daemons: a gateway that came up first would spend its startup
		// failing to push policy at a socket nothing was listening on.
		stepSluice(),
		stepSystemdUnits(),
		stepAdminSSH(),
		stepEnableServices(),
	}
}

// --- steps ------------------------------------------------------------------

// stepSwap makes sure the host has the requested amount of swap — the
// overcommit safety valve that lets a box run more sandboxes than it has RAM.
//
// It used to ask "does /swapfile exist". That is not the question. Ubuntu ships
// a 16 GiB /swap.img and has it swapped on before setup ever runs, so the stat
// of our own path found nothing, dd wrote a SECOND 16 GiB file beside it, and a
// host that asked for 16 GiB of swap ended up with 32 and two fstab lines
// (F4b). The question is how much swap the KERNEL currently has on, whatever
// the areas happen to be called.
func stepSwap() Step {
	return Step{
		Name: "swapfile",
		Satisfied: func(e *Env) (bool, string, error) {
			if e.Cfg.SwapGB <= 0 {
				return true, "disabled", nil
			}
			want := uint64(e.Cfg.SwapGB) << 30
			areas, err := readSwapAreas(e.Probe)
			if err != nil {
				// No /proc/swaps — not Linux, or a sandbox that hides it. Fall
				// back to the old own-path check, and say WHY in the note rather
				// than silently reverting to the behaviour this step exists to
				// fix.
				if _, statErr := os.Stat(e.SwapPath); statErr == nil {
					return true, fmt.Sprintf("%s exists (%s unreadable: %v)", e.SwapPath, procSwaps, err), nil
				}
				return false, "", nil
			}
			total := totalSwapBytes(areas)
			if total+swapSlack >= want {
				return true, fmt.Sprintf("%s active (%s) — no second swapfile needed",
					humanBytes(total), describeSwap(areas)), nil
			}
			if a, ok := swapActive(areas, e.SwapPath); ok {
				// Our own file is live and the total is still short. dd'ing over
				// a swapfile the kernel is paging to corrupts it, and swapoff on
				// a loaded host can OOM it — so this is a report, not a repair.
				return true, fmt.Sprintf("%s active at %s; total %s is short of the requested %dG, "+
					"and setup will not resize a live swapfile",
					e.SwapPath, humanBytes(a.bytes), humanBytes(total), e.Cfg.SwapGB), nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			need, have := swapNeed(e)
			if have > 0 {
				return fmt.Sprintf("add %s at %s (found %s already active, want %dG) + swapon + fstab",
					humanBytes(need), e.SwapPath, humanBytes(have), e.Cfg.SwapGB)
			}
			return fmt.Sprintf("create %s %s (overcommit safety valve) + swapon + fstab",
				humanBytes(need), e.SwapPath)
		},
		Apply: func(e *Env) error {
			// Belt and braces around the Satisfied branch above: whatever
			// happened between the probe and here, never write over an area the
			// kernel is paging to.
			if areas, err := readSwapAreas(e.Probe); err == nil {
				if a, ok := swapActive(areas, e.SwapPath); ok {
					return fmt.Errorf("%s is active swap (%s); refusing to overwrite a file the kernel is paging to — "+
						"`swapoff %s` first, or re-run with --swap-gb 0", e.SwapPath, humanBytes(a.bytes), e.SwapPath)
				}
			}
			need, have := swapNeed(e)
			mib := (need + (1 << 20) - 1) >> 20
			if mib == 0 {
				return nil
			}
			if have > 0 {
				e.logf("   %s of swap already active; adding %s at %s\n", humanBytes(have), humanBytes(need), e.SwapPath)
			}
			// dd (not fallocate) so swapon never trips over holes.
			if _, err := e.run("dd", "if=/dev/zero", "of="+e.SwapPath, "bs=1M",
				fmt.Sprintf("count=%d", mib), "status=none"); err != nil {
				return err
			}
			if err := os.Chmod(e.SwapPath, 0o600); err != nil {
				return err
			}
			if _, err := e.run("mkswap", e.SwapPath); err != nil {
				return err
			}
			if _, err := e.run("swapon", e.SwapPath); err != nil {
				return err
			}
			return appendLineIfMissing(e.FstabPath, e.SwapPath+" none swap sw 0 0")
		},
	}
}

// swapNeed is how much swap Apply should add, and how much is already active.
// Both Plan and Apply read it so the size an operator is promised and the size
// dd writes cannot disagree.
func swapNeed(e *Env) (need, have uint64) {
	if e.Cfg.SwapGB <= 0 {
		return 0, 0
	}
	want := uint64(e.Cfg.SwapGB) << 30
	if areas, err := readSwapAreas(e.Probe); err == nil {
		have = totalSwapBytes(areas)
	}
	if have+swapSlack >= want {
		return 0, have
	}
	return want - have, have
}

func stepResolveRelease() Step {
	return Step{
		Name: "resolve-release",
		Satisfied: func(e *Env) (bool, string, error) {
			// Always resolve so downstream steps see the manifest; cheap (one GET).
			return false, "", nil
		},
		Plan: func(e *Env) string {
			return fmt.Sprintf("resolve %q from %s and fetch the release manifest", e.Cfg.Release, e.Cfg.ArtifactBase)
		},
		Apply: func(e *Env) error {
			// One GET does both jobs: "latest" rides GitHub's
			// /releases/latest/download redirect, and the manifest that comes
			// back names the concrete tag every artifact URL is then built from.
			rc, err := e.Fetch.Get(e.Ctx, ManifestURL(e.Cfg.ArtifactBase, e.Cfg.Release))
			if err != nil {
				return fmt.Errorf("fetch manifest: %w", err)
			}
			defer rc.Close()
			m, err := ParseManifest(rc, e.Cfg.Release)
			if err != nil {
				return err
			}
			// A manifest for the wrong OS parses perfectly and every checksum
			// in it is right — for somebody else's binaries. The PLATFORM key
			// is the only tell, so check it here, once, before anything is
			// downloaded on the strength of it.
			if err := m.CheckPlatform(hostOS()); err != nil {
				return err
			}
			e.Manifest = m
			tag := m.Release
			// The gateway's --default-image must match the template basename the
			// manifest ships, so downstream env/unit steps use it.
			if m.RootfsName != "" {
				e.Cfg.DefaultImage = m.RootfsName
			}
			e.logf("   release %s (rootfs %s, login user %s)\n", tag, m.RootfsName, m.RootfsLogin)
			return nil
		},
	}
}

// stepInstallBinary copies the running sparkbox binary to Cfg.BinPath — the
// path the systemd unit's ExecStart names.
//
// Nothing used to do this. The unit hardcoded /usr/local/bin/sparkbox while
// setup only ever fetched the kernel, firecracker and the rootfs, so following
// the README literally (curl the binary into $PWD, `sudo ./sparkbox setup`)
// produced a unit pointing at a file that did not exist. Where a *stale* binary
// happened to be there already it was worse: setup succeeded, the service came
// up, and the box quietly ran the previous release.
//
// The copy is tmp-file + rename rather than a write in place because the
// destination is very likely open and executing: rename swaps the directory
// entry atomically, the running process keeps the old inode until it is
// restarted (which enable-services then does), and no reader ever sees a
// half-written binary. The sha comparison is what keeps a re-run from churning
// the file under a live service for no reason.
func stepInstallBinary() Step {
	return Step{
		Name: "install-binary",
		Satisfied: func(e *Env) (bool, string, error) {
			if e.Cfg.BinPath == "" {
				return true, "skipped (no --bin-path)", nil
			}
			if e.SelfPath == "" {
				return false, "", fmt.Errorf("cannot locate the running binary (os.Executable failed); " +
					"install it manually and re-run with --bin-path \"\" to skip this step")
			}
			// Setup run *from* the install path: the file is already what we
			// would copy, and copying it onto itself is pointless work on a
			// binary that may be executing.
			if same, err := sameFile(e.SelfPath, e.Cfg.BinPath); err != nil {
				return false, "", err
			} else if same {
				return true, "running from " + e.Cfg.BinPath, nil
			}
			src, err := sha256File(e.SelfPath)
			if err != nil {
				return false, "", fmt.Errorf("hash %s: %w", e.SelfPath, err)
			}
			// sha256File answers "" for a missing destination, so a fresh host
			// simply falls through to "not satisfied".
			dst, err := sha256File(e.Cfg.BinPath)
			if err != nil {
				return false, "", fmt.Errorf("hash %s: %w", e.Cfg.BinPath, err)
			}
			if dst != "" && dst == src {
				return true, "identical binary at " + e.Cfg.BinPath, nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			return fmt.Sprintf("install this sparkbox binary (%s) to %s", orUnknown(e.Cfg.Version), e.Cfg.BinPath)
		},
		Apply: func(e *Env) error {
			dest := e.Cfg.BinPath
			// Refuse a downgrade. Re-running last month's installer on an
			// upgraded host would otherwise roll the gateway back silently —
			// the same class of surprise as F0 itself, pointed the other way.
			// The destination's version is read by running it, which is the
			// only honest source: the file could be any build, or not sparkbox
			// at all (binaryVersion answers "" for all of those, and an
			// uncomparable pair is reported and then overwritten, because
			// refusing on an unfamiliar version string would strand the host).
			if !e.Cfg.Force {
				have := binaryVersion(e.probeRun, dest)
				cmp, ok := compareVersions(have, e.Cfg.Version)
				switch {
				case ok && cmp > 0:
					return fmt.Errorf("%s is version %s, newer than this binary (%s); "+
						"run the %s installer instead, or pass --force to overwrite it",
						dest, have, e.Cfg.Version, have)
				case have != "" && !ok:
					e.logf("   %s reports version %q — not comparable with %q, overwriting\n", dest, have, orUnknown(e.Cfg.Version))
				}
			}
			if err := installFile(e.SelfPath, dest); err != nil {
				return err
			}
			e.BinaryInstalled = true
			e.logf("   installed %s → %s (%s)\n", e.SelfPath, dest, orUnknown(e.Cfg.Version))
			return nil
		},
	}
}

func stepDataVolume() Step {
	return Step{
		Name: "data-volume",
		Satisfied: func(e *Env) (bool, string, error) {
			data := e.Cfg.dataDir()
			// The volume exists to hold state/ and images/. When neither lives
			// under it — an adopted legacy layout, or an explicit --state-dir on
			// a real disk — building it would be hundreds of gigabytes of
			// nothing, and mounting a filesystem into the path of a host that is
			// working fine is not a no-op.
			if !underDir(data, e.Cfg.StateDir) && !underDir(data, e.Cfg.ImageDir) {
				return true, "not applicable (state and images live outside " + data + ")", nil
			}
			if _, err := e.run("mountpoint", "-q", data); err == nil {
				// WHICH filesystem is mounted there is not knowable from here:
				// `mountpoint` answers "a filesystem", not "our data.img with
				// reflink=1". That is a genuine limit of this probe rather than
				// an omission — a wrong volume shows up as the state and rootfs
				// checks below failing, not as something to detect here.
				return true, "mounted at " + data, nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			return fmt.Sprintf("%dG XFS reflink volume at %s (+ state/ images/)", e.Cfg.DataVolumeGB, e.Cfg.dataDir())
		},
		Apply: func(e *Env) error {
			img := filepath.Join(e.Cfg.Root, "data.img")
			data := e.Cfg.dataDir()
			if err := os.MkdirAll(data, 0o755); err != nil {
				return err
			}
			// Mounting over a populated directory does not merge with it or
			// fail — it HIDES it. A host that was provisioned once without the
			// loop volume has its whole state (sqlite DB, fleet keys, the
			// certmagic cache) sitting right here, and the only symptom of
			// burying it is a gateway that comes up empty and re-issues its
			// certificates. Refuse instead, and name what is in the way.
			if payload := dirPayload(data); len(payload) > 0 {
				return fmt.Errorf("refusing to mount the data volume over the non-empty %s (contains %s): "+
					"mounting hides what is already there rather than merging with it. "+
					"If that IS this host's data, use it in place — `sparkbox setup --state-dir %s --image-dir %s` "+
					"(or --adopt-legacy) — otherwise move it aside: mv %s %s.old",
					data, strings.Join(payload, ", "),
					filepath.Join(data, "state"), filepath.Join(data, "images"), data, data)
			}
			if _, err := os.Stat(img); os.IsNotExist(err) {
				if _, err := e.run("truncate", "-s", fmt.Sprintf("%dG", e.Cfg.DataVolumeGB), img); err != nil {
					return err
				}
				if _, err := e.run("mkfs.xfs", "-q", "-m", "reflink=1", img); err != nil {
					return err
				}
			}
			if _, err := e.run("mount", "-o", "loop", img, data); err != nil {
				return err
			}
			if err := appendLineIfMissing(e.FstabPath, img+" "+data+" xfs loop 0 0"); err != nil {
				return err
			}
			return os.MkdirAll(e.Cfg.ImageDir, 0o755) // state dir is created by serve/StateDir mkdir
		},
	}
}

func stepFetchArtifacts() Step {
	return Step{
		Name: "fetch-artifacts",
		// "The files are there" is a weaker claim than "the files are THIS
		// release's". A host holding a previous release's kernel read as
		// satisfied, so an upgrade silently kept booting guests on the old one —
		// F0's staleness, one directory down. And firecracker was not looked at
		// here at all: a host with a kernel and a rootfs but no firecracker
		// binary reported "present" and the step never ran.
		Satisfied: func(e *Env) (bool, string, error) {
			for _, w := range []struct{ name, path, sha string }{
				{"kernel", e.Cfg.KernelPath, e.Manifest.SHA256Vmlinux},
				{"firecracker", e.Cfg.FirecrackerBin, e.Manifest.SHA256Firecrkr},
				// The rootfs sha in the manifest covers the COMPRESSED asset,
				// which downloadVerify streams straight through into the
				// decompressed image and never keeps — so there is nothing on
				// disk to compare it against. Existence plus a non-zero size is
				// the strongest cheap check available here, and saying so is
				// better than implying a verification that is not happening.
				{"rootfs", e.Cfg.rootfsPath(), ""},
			} {
				fi, err := os.Stat(w.path)
				if err != nil || fi.Size() == 0 {
					return false, "", nil
				}
				if w.sha == "" {
					continue
				}
				have, herr := sha256File(w.path)
				if herr != nil {
					return false, "", fmt.Errorf("hash %s: %w", w.path, herr)
				}
				if have != w.sha {
					return false, "", nil
				}
			}
			if e.Manifest.Release == "" {
				// --dry-run: resolve-release only ever runs its Apply, so there
				// is no manifest to compare against. Do not pretend otherwise.
				return true, "kernel + firecracker + rootfs present (release unresolved — shas unchecked)", nil
			}
			return true, "kernel + firecracker match " + e.Manifest.Release + ", rootfs present", nil
		},
		Plan: func(e *Env) string {
			return "download + sha256-verify vmlinux, firecracker, rootfs (decompress)"
		},
		Apply: func(e *Env) error {
			if e.Manifest.Release == "" {
				return fmt.Errorf("no manifest resolved (resolve-release must run first)")
			}
			if err := os.MkdirAll(e.Cfg.ImageDir, 0o755); err != nil {
				return err
			}
			for _, a := range e.Manifest.Artifacts(e.Cfg.ArtifactBase, e.Cfg) {
				dl, err := downloadVerify(e.Ctx, e.Fetch, a)
				if err != nil {
					return err
				}
				verb := "present"
				if dl {
					verb = "downloaded"
				}
				e.logf("   %s: %s\n", a.Name, verb)
			}
			return nil
		},
	}
}

func stepUsersConf() Step {
	return Step{
		Name: "users.conf",
		Satisfied: func(e *Env) (bool, string, error) {
			// A fleet node holds no accounts — every identity lives on the
			// gateway — and this step hard-fails without an operator key, so on
			// a node it would turn a perfectly good provisioning run into a
			// demand for a key that would never be used.
			if e.Cfg.Gateway != "" {
				return true, "skipped (fleet node; accounts live on the gateway)", nil
			}
			b, err := os.ReadFile(e.Cfg.UsersPath)
			if err != nil {
				return false, "", nil
			}
			if n, perr := countUsers(b); perr == nil && n > 0 {
				return true, fmt.Sprintf("%d user(s)", n), nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			src := e.Cfg.OperatorKey
			if src == "" {
				src = "~/.ssh/*.pub (auto-detect)"
			}
			return fmt.Sprintf("seed %s with operator %q from %s", e.Cfg.UsersPath, e.Cfg.OperatorHandle, src)
		},
		Apply: func(e *Env) error {
			line, err := e.operatorKeyLine()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(e.Cfg.UsersPath), 0o755); err != nil {
				return err
			}
			return os.WriteFile(e.Cfg.UsersPath, []byte(line+"\n"), 0o644)
		},
	}
}

// stepEnvFile writes (or reconciles) /srv/sparkbox/sparkbox.env.
//
// This file is the operator's editing surface — EXTRA_FLAGS, the console
// password, overcommit tuning — so it is emphatically NOT re-rendered the way
// the units are. But it also carries settings that are pure functions of setup's
// own flags, and those going stale is silent breakage: PROXY_PORT is what
// sparkbox-net.sh forwards every any-port connection to, so a host whose edge
// moved to :443 while this file still said 8081 sent all sandbox web traffic to
// a port nothing was bound to, with no error anywhere.
//
// So Satisfied compares the MANAGED keys, and Apply merges them into whatever is
// on disk instead of overwriting it.
func stepEnvFile() Step {
	return Step{
		Name: "host-config",
		Satisfied: func(e *Env) (bool, string, error) {
			kv, ok := readEnvFile(e.Cfg.envPath())
			if !ok {
				return false, "", nil
			}
			if drift := envDrift(kv, e.managedEnv(kv)); len(drift) > 0 {
				return false, "", nil
			}
			return true, e.Cfg.envPath() + " current", nil
		},
		Plan: func(e *Env) string {
			kv, ok := readEnvFile(e.Cfg.envPath())
			if !ok {
				return "write " + e.Cfg.envPath()
			}
			drift := envDrift(kv, e.managedEnv(kv))
			if len(drift) == 0 {
				return "write " + e.Cfg.envPath()
			}
			return fmt.Sprintf("update %s (%s); operator settings preserved",
				e.Cfg.envPath(), strings.Join(drift, ", "))
		},
		Apply: func(e *Env) error {
			if err := os.MkdirAll(filepath.Dir(e.Cfg.envPath()), 0o755); err != nil {
				return err
			}
			old, err := os.ReadFile(e.Cfg.envPath())
			if err != nil {
				if werr := os.WriteFile(e.Cfg.envPath(), []byte(e.renderEnvFile()), 0o644); werr != nil {
					return werr
				}
				e.EnvChanged = true
				return nil
			}
			kv, _ := readEnvFile(e.Cfg.envPath())
			merged, changed := mergeEnv(string(old), e.managedEnv(kv))
			for _, c := range changed {
				e.logf("   %s\n", c)
			}
			if err := os.WriteFile(e.Cfg.envPath(), []byte(merged), 0o644); err != nil {
				return err
			}
			// Both units source this file, and both read it only at start. Tell
			// enable-services so the values just written are the ones actually
			// running, rather than the ones that will be running after the next
			// reboot — see Env.EnvChanged.
			if len(changed) > 0 {
				e.EnvChanged = true
			}
			return nil
		},
	}
}

// fileAsset is one embedded file setup lays down verbatim.
type fileAsset struct {
	path string
	body []byte
	mode os.FileMode
}

// wantedNetAssets is what this release's packet-filter assets should contain.
// Same contract as wantedUnits: one list feeds both the comparison and the
// write, so "what we check" and "what we install" cannot drift.
func wantedNetAssets(e *Env) []fileAsset {
	return []fileAsset{
		{filepath.Join(e.SbinDir, "sparkbox-net.sh"), deploy.NetScript, 0o755},
		{filepath.Join(e.SysctlDir, "99-sparkbox.conf"), deploy.SysctlConf, 0o644},
	}
}

func stepNetAssets() Step {
	return Step{
		Name: "net-rules",
		// Byte-compare, not stat. These files ship with the release, so an
		// existence check meant every packet-filter fix stopped dead at the
		// first host that had ever been provisioned — the host most likely to
		// need it. F8's chain-name work is exactly such a fix.
		Satisfied: func(e *Env) (bool, string, error) {
			for _, a := range wantedNetAssets(e) {
				got, err := os.ReadFile(a.path)
				if err != nil || !bytes.Equal(got, a.body) {
					return false, "", nil
				}
			}
			return true, "scripts + sysctl installed and current", nil
		},
		Plan: func(e *Env) string {
			return fmt.Sprintf("install sparkbox-net.sh + %s/99-sparkbox.conf, apply sysctl", e.SysctlDir)
		},
		Apply: func(e *Env) error {
			for _, a := range wantedNetAssets(e) {
				if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(a.path, a.body, a.mode); err != nil {
					return err
				}
			}
			// Writing the script is not running it: the rules in the kernel are
			// the ones the last boot's script built. See Env.NetChanged.
			e.NetChanged = true
			_, err := e.run("sysctl", "-q", "--system")
			return err
		},
	}
}

// wantedUnits is what this config's units should contain, in write order. Both
// the Satisfied comparison and Apply read it, so "what we would install" and
// "what we compare against" cannot drift.
func wantedUnits(cfg Config) ([][2]string, error) {
	svc, err := renderService(cfg)
	if err != nil {
		return nil, err
	}
	return [][2]string{
		{"sparkbox.service", svc},
		{"sparkbox-net.service", string(deploy.NetService)},
	}, nil
}

func stepSystemdUnits() Step {
	return Step{
		Name: "systemd-units",
		// "The files exist" is not the same claim as "the files say what this
		// config asks for". Existence alone meant a second `sparkbox setup
		// --bin-path /opt/sparkbox` left ExecStart pointing at the *old* path
		// (install-binary duly wrote the new binary somewhere the unit never ran
		// from), and it meant every unit fix shipped in a release stopped at the
		// first host that had ever been provisioned — the same silent staleness
		// as F0. So compare the rendered content, byte for byte.
		Satisfied: func(e *Env) (bool, string, error) {
			units, err := wantedUnits(e.Cfg)
			if err != nil {
				return false, "", err
			}
			for _, u := range units {
				got, err := os.ReadFile(filepath.Join(e.SystemdDir, u[0]))
				if err != nil {
					// Missing (fresh host) or unreadable: either way Apply is the
					// answer, and it reports a write failure far better than a
					// Satisfied that refuses to run.
					return false, "", nil
				}
				if string(got) != u[1] {
					return false, "", nil
				}
			}
			return true, "units installed and current", nil
		},
		Plan: func(e *Env) string {
			return "install sparkbox.service (standalone, ExecStart " + e.Cfg.binPath() + ") + sparkbox-net.service"
		},
		Apply: func(e *Env) error {
			if err := os.MkdirAll(e.SystemdDir, 0o755); err != nil {
				return err
			}
			units, err := wantedUnits(e.Cfg)
			if err != nil {
				return err
			}
			for _, u := range units {
				path := filepath.Join(e.SystemdDir, u[0])
				// Read before writing so the net unit's own restart is triggered
				// only when the net unit actually moved: it is a oneshot that
				// rebuilds the whole packet filter, and re-running it on every
				// --bin-path tweak would flush and rebuild the rules of a host
				// nothing asked to change.
				if got, rerr := os.ReadFile(path); rerr == nil && string(got) == u[1] {
					continue
				}
				if err := os.WriteFile(path, []byte(u[1]), 0o644); err != nil {
					return err
				}
				if u[0] == netUnit {
					e.NetChanged = true
				}
			}
			// systemd is still holding the previous text until daemon-reload, and
			// a running unit keeps its old ExecStart until it is restarted; both
			// are enable-services' job, so tell it the units moved.
			e.UnitsChanged = true
			return nil
		},
	}
}

// adminSSHDDropIn is the sshd_config.d fragment that relocates the host's own
// sshd. Named once so the comparison and the write cannot disagree about what
// "already moved" means.
const adminSSHDDropIn = "Port 2222\n"

func stepAdminSSH() Step {
	return Step{
		Name: "admin-ssh",
		Satisfied: func(e *Env) (bool, string, error) {
			if !e.Cfg.MoveAdminSSH {
				return true, "skipped (--move-admin-ssh not set; gateway binds " + e.Cfg.sshAddr() + ")", nil
			}
			// Content, not existence — a drop-in naming some other port is not
			// this config. What this cannot tell is whether sshd is actually
			// LISTENING there: sshd may have failed to reload, or another
			// drop-in later in the lexical order may override the port. That is
			// a live-state question the verify pass owns, not this file check.
			got, err := os.ReadFile(filepath.Join(e.SSHDConfD, "sparkbox-admin-port.conf"))
			if err == nil && string(got) == adminSSHDDropIn {
				return true, "admin sshd already moved to :2222", nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			return "move host admin sshd to :2222 so the gateway can own :22 (DANGEROUS: keep a session open)"
		},
		Apply: func(e *Env) error {
			e.logf("   WARNING: relocating admin sshd to :2222; reconnect with `ssh -p 2222` if disconnected\n")
			if err := os.MkdirAll(e.SSHDConfD, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(e.SSHDConfD, "sparkbox-admin-port.conf"), []byte(adminSSHDDropIn), 0o644); err != nil {
				return err
			}
			// Ubuntu 24.04 socket-activates ssh; a bare Port is ignored while
			// ssh.socket owns :22, so disable + mask the socket and run ssh.service.
			_, _ = e.run("systemctl", "disable", "--now", "ssh.socket")
			_, _ = e.run("systemctl", "mask", "ssh.socket")
			_, _ = e.run("systemctl", "restart", "ssh.service")
			return nil
		},
	}
}

func stepEnableServices() Step {
	return Step{
		Name: "enable-services",
		Satisfied: func(e *Env) (bool, string, error) {
			// An already-active unit is NOT satisfied once the binary under it
			// changed: the running process holds the old inode until something
			// restarts it, which is exactly how a "v0.4.0 setup" left a live
			// v0.3.0 gateway behind. A rewritten unit file is the same problem
			// one level up — the live process still runs the old ExecStart (the
			// old --bin-path included) — and so are a rewritten sparkbox.env
			// (read once, at unit start) and a rewritten packet-filter script
			// (the kernel holds the last boot's rules). Apply turns all four
			// into a restart of whichever unit is affected.
			if e.somethingChanged() {
				return false, "", nil
			}
			// Deliberately a single is-active sample rather than the full
			// liveness probe: this runs on every setup (dry runs included) and
			// the settle window would tax them all, while restarting a
			// crash-looping unit does not fix anything. Liveness is judged once,
			// authoritatively, by the verify pass at the end of Provision —
			// which now decides the exit code.
			out, _ := e.run("systemctl", "is-active", serviceUnit)
			if strings.TrimSpace(string(out)) == "active" {
				return true, serviceUnit + " active", nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			units := netUnit
			if e.Cfg.Sluice {
				units += " + " + sluiceUnit
			}
			units += " + " + serviceUnit
			return "systemctl daemon-reload; enable --now " + units +
				" (restarting any whose binary, unit, sparkbox.env or rules changed)"
		},
		Apply: func(e *Env) error {
			if _, err := e.run("systemctl", "daemon-reload"); err != nil {
				return err
			}
			if _, err := e.run("systemctl", "enable", "--now", netUnit); err != nil {
				return err
			}
			// sluice before the gateway, and only when this config installs it.
			// The gateway pushes egress policy at --sluice-socket as soon as it
			// starts, so bringing it up first means a startup spent failing to
			// reach a socket nothing is bound to.
			//
			// It is NOT disabled when --sluice is absent: a host that has sluice
			// running from a hand install (which was the only way to have it
			// until this step existed) must not silently lose its egress filter
			// because somebody re-ran setup without the new flag. Turning it off
			// is `systemctl disable --now sluice`, typed on purpose.
			if e.Cfg.Sluice {
				if _, err := e.run("systemctl", "enable", "--now", sluiceUnit); err != nil {
					return err
				}
			}
			if _, err := e.run("systemctl", "enable", "--now", serviceUnit); err != nil {
				return err
			}
			// `enable --now` is a no-op on a unit that is already running, so a
			// swapped binary — or a rewritten unit, env file or script — needs an
			// explicit restart to actually take effect. Unconditional after a
			// change (rather than only when it was already active) because "was
			// it running a moment ago" is a race we would lose silently; one
			// extra restart of a just-started unit is cheap.
			//
			// The packet filter goes FIRST, and it is the half that used to be
			// missed entirely: sparkbox-net.service is Type=oneshot +
			// RemainAfterExit=yes, so it stays `active (exited)` for the life of
			// the boot and `enable --now` above starts nothing. Its script is
			// written to be re-runnable (each chain is created-or-flushed), so a
			// restart is how a corrected PROXY_PORT, a new --edge-ip or a fixed
			// chain name reaches the kernel in the run that asked for it instead
			// of at the next reboot. Before the gateway, so the edge does not
			// come up ahead of the DNATs that carry traffic to it.
			if e.NetChanged || e.EnvChanged {
				e.logf("   %s changed — restarting %s so the live rules match\n",
					changedWhat(map[string]bool{"packet-filter assets": e.NetChanged, "sparkbox.env": e.EnvChanged}), netUnit)
				if _, err := e.run("systemctl", "restart", netUnit); err != nil {
					return err
				}
			}
			// Then sluice, for the same reason and with the same argument about
			// ordering: `enable --now` is a no-op on a unit that is already
			// running, so a re-fetched binary or a re-rendered unit reaches the
			// kernel only through an explicit restart. Still ahead of the
			// gateway, so the socket exists before anything pushes to it.
			if e.Cfg.Sluice && e.SluiceChanged {
				e.logf("   sluice binary or unit changed — restarting %s\n", sluiceUnit)
				if _, err := e.run("systemctl", "restart", sluiceUnit); err != nil {
					return err
				}
			}
			if e.BinaryInstalled || e.UnitsChanged || e.EnvChanged {
				e.logf("   %s changed — restarting %s so it runs the new configuration\n",
					changedWhat(map[string]bool{"binary": e.BinaryInstalled, "unit": e.UnitsChanged, "sparkbox.env": e.EnvChanged}), serviceUnit)
				if _, err := e.run("systemctl", "restart", serviceUnit); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// somethingChanged reports whether this run has already rewritten anything a
// running unit would not pick up on its own.
func (e *Env) somethingChanged() bool {
	return e.BinaryInstalled || e.UnitsChanged || e.EnvChanged || e.NetChanged || e.SluiceChanged
}

// changedWhat names the things that changed, in a stable order, for the restart
// log line. Sorted because a map range order that shuffled "binary + unit" into
// "unit + binary" between runs would make the output diff for no reason.
func changedWhat(what map[string]bool) string {
	var names []string
	for name, changed := range what {
		if changed {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, " + ")
}

// --- helpers ----------------------------------------------------------------

// probeRun adapts the Runner to the (string, error) shape binaryVersion wants,
// so setup asks the destination binary its version through the same injected
// Runner every other shell-out uses and a test can can the answer.
func (e *Env) probeRun(name string, args ...string) (string, error) {
	out, err := e.run(name, args...)
	return strings.TrimSpace(string(out)), err
}

// sameFile reports whether two paths are the same file on disk (the setup
// binary already living at the install path). A missing destination is not an
// error — it is the fresh-host case.
func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// installFile copies src over dest atomically: a temp file in the DESTINATION
// directory (so the rename never crosses a filesystem — /tmp usually is one),
// chmod before the rename (so the destination is never briefly present and
// non-executable, which systemd could catch), then rename. Every failure path
// removes the temp file: a stray sparkbox.tmp in /usr/local/bin is exactly the
// kind of half-state a failed provision must not leave behind.
func installFile(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dest + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// operatorKeyLine resolves the operator's public key (explicit path, literal
// "ssh-..." text, or an auto-detected ~/.ssh/*.pub) into a validated
// "<handle> <authorized_keys line>" for users.conf.
func (e *Env) operatorKeyLine() (string, error) {
	keyText, err := e.resolveOperatorKey()
	if err != nil {
		return "", err
	}
	keyText = strings.TrimSpace(keyText)
	if _, _, _, _, perr := xssh.ParseAuthorizedKey([]byte(keyText)); perr != nil {
		return "", fmt.Errorf("operator key is not a valid SSH public key: %w", perr)
	}
	return e.Cfg.OperatorHandle + " " + keyText, nil
}

func (e *Env) resolveOperatorKey() (string, error) {
	k := e.Cfg.OperatorKey
	if strings.HasPrefix(strings.TrimSpace(k), "ssh-") {
		return k, nil // literal key text
	}
	if k != "" {
		b, err := os.ReadFile(k)
		if err != nil {
			return "", fmt.Errorf("read operator key %s: %w", k, err)
		}
		return string(b), nil
	}
	// Auto-detect a public key in the invoking user's ~/.ssh.
	for _, name := range []string{"id_ed25519.pub", "id_rsa.pub", "id_ecdsa.pub"} {
		p := filepath.Join(e.HomeDir, ".ssh", name)
		if b, err := os.ReadFile(p); err == nil {
			e.logf("   using operator key %s\n", p)
			return string(b), nil
		}
	}
	return "", fmt.Errorf("no operator key: pass --operator-key <path|\"ssh-... key\"> (none found in %s/.ssh)", e.HomeDir)
}

// renderEnvFile builds /srv/sparkbox/sparkbox.env — the non-secret host config
// the systemd units source. Flags default to whitespace-empty (zero args); the
// operator turns on TLS/overcommit here later.
func (e *Env) renderEnvFile() string {
	login := ""
	if e.Manifest.RootfsLogin != "" {
		login = "--default-login-user=" + e.Manifest.RootfsLogin
	}
	// A whole flag bundle rather than one --gateway, because turning a host into
	// a node usually comes with a --node-name and sometimes a --gateway-host-key
	// pin, and an operator who has to edit this file anyway should not have to
	// wait for a new variable to carry each of them.
	gateway := ""
	if e.Cfg.Gateway != "" {
		gateway = "--gateway " + e.Cfg.Gateway
		if e.Cfg.NodeName != "" {
			gateway += " --node-name " + e.Cfg.NodeName
		}
	}
	// PROXY_PORT is derived from the SAME address the unit's --proxy-addr gets,
	// never from a second constant. sparkbox-net.sh forwards every any-port
	// connection to PROXY_PORT, so the two disagreeing does not fail — it
	// silently sends all sandbox web traffic to a port nothing is listening on.
	// A malformed address cannot reach here (validateAddrs runs first in
	// Provision), so the fallback is only for a hand-built Config in a test.
	port, err := e.Cfg.proxyPortNum()
	if err != nil {
		port = 0
	}
	var b strings.Builder
	b.WriteString("# sparkbox host config, sourced by sparkbox.service + sparkbox-net.service.\n")
	b.WriteString("# Flags MUST be referenced unbraced in the units ($EXTRA_FLAGS, not ${EXTRA_FLAGS}).\n")
	fmt.Fprintf(&b, "PROXY_DOMAIN=%s\n", e.Cfg.ProxyDomain)
	fmt.Fprintf(&b, "LOGIN_USER_FLAG=%s\n", login)
	b.WriteString("SUBNET6_FLAG=\n")
	b.WriteString("SUBNET6=\n")
	// Deliberately does NOT quote the current edge address: a later `setup
	// --proxy-addr` reconciles the PROXY_PORT line in place (mergeEnv) and
	// leaves comments alone, so an address baked into this comment would be the
	// one stale thing in the file.
	b.WriteString("# sparkbox-net.sh forwards any-port traffic to this port, so it must keep matching the\n")
	b.WriteString("# unit's --proxy-addr. `sparkbox setup --proxy-addr <addr>` moves both together.\n")
	fmt.Fprintf(&b, "PROXY_PORT=%d\n", port)
	// Any-port forwarding mode, read by sparkbox-net.sh (never by the gateway).
	// Written on a fresh host whichever way --edge-ip went, so an operator can
	// see both knobs and what this host chose; mergeEnv only ever CORRECTS them
	// when --edge-ip is given, so a later hand-edit survives an upgrade.
	b.WriteString("# Any-port web forwarding, read by sparkbox-net.sh. Two mutually exclusive modes:\n")
	b.WriteString("#   uplink REDIRECT (SPARKBOX_EDGE_REDIRECT=1, the default) hijacks every inbound TCP\n")
	b.WriteString("#     port above 1024 on the default route into the edge — right for a box with a\n")
	b.WriteString("#     public IP of its own, wrong for a shared/home machine or a reverse tunnel.\n")
	b.WriteString("#   dedicated edge IP (SPARKBOX_EDGE_IP=<ip>) gives the edge its own /32 on a dummy\n")
	b.WriteString("#     interface and DNATs by destination, so it cannot collide with host services.\n")
	b.WriteString("#     `sparkbox setup --edge-ip <ip>` sets both lines together.\n")
	fmt.Fprintf(&b, "SPARKBOX_EDGE_IP=%s\n", e.Cfg.EdgeIP)
	fmt.Fprintf(&b, "SPARKBOX_EDGE_REDIRECT=%s\n", edgeRedirectValue(e.Cfg))
	// Where the SSH gateway listens, for sparkbox-net.sh — which otherwise
	// assumes :2222 in three places. Emitted through the SAME helper managedEnv
	// reconciles with (and, like it, only when the port is not the one the script
	// already assumes), so a fresh host and an upgraded one cannot end up
	// describing different SSH ports to the packet filter.
	if ss := sshNetSettings(e.Cfg, nil); len(ss) > 0 {
		b.WriteString("# The gateway's SSH port, for sparkbox-net.sh: it must be spared from any-port\n")
		b.WriteString("# forwarding (or dialling it lands in the web edge), and with a dedicated edge IP\n")
		b.WriteString("# it is where that IP's :22 is DNATed. `sparkbox setup --ssh-addr` moves all of them.\n")
		for _, s := range ss {
			fmt.Fprintf(&b, "%s=%s\n", s.key, s.val)
		}
	}
	// The sluice resolver's own address, which sparkbox-net.sh puts on a dummy
	// interface at boot. Emitted only when there is one, and through the same
	// derivation managedEnv reconciles with, so a fresh host and an upgraded one
	// cannot describe different resolver addresses to the packet filter.
	if ip := e.Cfg.sluiceResolverIP(); ip != "" {
		b.WriteString("# sluice's allowlist resolver binds this address and guests are handed it as their\n")
		b.WriteString("# DNS server, so it has to EXIST: sparkbox-net.sh creates it on a dummy interface\n")
		b.WriteString("# (skipping it if the host already holds it). Moved by `sparkbox setup --sluice-dns-addr`.\n")
		fmt.Fprintf(&b, "SLUICE_DNS_IP=%s\n", ip)
	}
	b.WriteString("# Live memory overcommit + density defaults (retune with hack/measure-density.py):\n")
	b.WriteString("OVERCOMMIT_FLAGS=--mem-reserve-mb 1024 --max-running-per-owner 50\n")
	b.WriteString("# Any `sparkbox serve` flag setup has none of its own for. Appended LAST in the\n")
	b.WriteString("# unit, and a repeated flag wins in Go, so anything here overrides the unit above.\n")
	b.WriteString("# Prefer a real setup flag where one exists — the edge, TLS, the DNS responder,\n")
	b.WriteString("# archiving, egress and the advertised SSH port all have one now, and a flag also\n")
	b.WriteString("# keeps the lines above (PROXY_PORT, the edge mode) in step, which an override\n")
	b.WriteString("# here would not:\n")
	b.WriteString("#   sparkbox setup --proxy-addr :443 --proxy-tls --tls-provider autocert --tls-email you@example.com\n")
	b.WriteString("EXTRA_FLAGS=\n")
	b.WriteString("# Legacy name for the same escape hatch, still honoured by the unit (ahead of\n")
	b.WriteString("# EXTRA_FLAGS) so hosts provisioned before EXTRA_FLAGS existed keep working.\n")
	b.WriteString("TLS_FLAGS=\n")
	b.WriteString("# Fleet node: set e.g. GATEWAY_FLAG=--gateway gw.example.com:2222 --node-name box-b\n")
	b.WriteString("# to run this host as a node of that gateway instead of as a gateway itself.\n")
	b.WriteString("# Everything above (TLS, proxy, console) is then ignored: a node serves no edge.\n")
	fmt.Fprintf(&b, "GATEWAY_FLAG=%s\n", gateway)
	b.WriteString("# Operator console: uncomment to enable console.<domain>.\n")
	b.WriteString("# SPARKBOX_CONSOLE_PASSWORD=change-me\n")
	return b.String()
}

func (e *Env) printConnect() {
	if e.Cfg.Gateway != "" {
		name := e.Cfg.NodeName
		if name == "" {
			name = "<this-hostname>"
		}
		e.logf("\n== sparkbox fleet node is provisioned ==\n")
		e.logf("  node:              %s\n", name)
		e.logf("  gateway:           %s\n", e.Cfg.Gateway)
		e.logf("  enrollment:        compare this node's logged fingerprint with `ssh ctl@<gateway> node ls`\n")
		e.logf("  approve at gateway: ssh ctl@<gateway> node approve <SHA256:...>\n")
		e.logf("  health check:      sparkbox doctor --gateway %s\n", e.Cfg.Gateway)
		e.logf("  logs:              journalctl -u sparkbox -f\n")
		return
	}
	// Read the banner's port off the address the unit was actually rendered
	// with. It used to be re-derived from MoveAdminSSH, which was right only
	// while :2222/:22 were the only two possibilities — with --ssh-addr it
	// would have told the operator to connect to a port the gateway does not
	// listen on, which is the same "setup lies" failure as F7, just cheaper.
	host, port, err := splitAddr(e.Cfg.sshAddr())
	if err != nil {
		host, port = "", 2222
	}
	if isWildcardHost(host) {
		host = "<this-host>"
	}
	// --ssh-advertise-port exists precisely because the port an operator dials
	// and the port the gateway binds differ when a DNAT sits in front (the DGX
	// takes :22 on its edge /32 and forwards it to :2222). `serve` already tells
	// users the advertised port everywhere it prints instructions; a banner that
	// went on naming the listen port would be the one place still lying.
	listen := port
	if e.Cfg.SSHAdvertisePort > 0 {
		port = e.Cfg.SSHAdvertisePort
	}
	e.logf("\n== sparkbox is provisioned ==\n")
	if e.AdoptedLegacy {
		// Every later setup on this host needs the same flag, and an operator
		// who is not told which layout they adopted will hit the refusal next
		// time and have to rediscover why.
		e.logf("  layout:            adopted (--adopt-legacy) — state %s, images %s\n", e.Cfg.StateDir, e.Cfg.ImageDir)
		e.logf("                     pass --adopt-legacy on every future `sparkbox setup` for this host\n")
	}
	e.logf("  create a sandbox:  ssh -p %d new@%s\n", port, host)
	e.logf("  shell into it:     ssh -p %d <name>@%s\n", port, host)
	e.logf("  health check:      sparkbox doctor\n")
	e.logf("  logs:              journalctl -u sparkbox -f\n")
	if port != listen {
		e.logf("  (advertised as :%d; the gateway itself listens on :%d — something in front must forward it,\n", port, listen)
		e.logf("   e.g. the :22 → :%d DNAT sparkbox-net.sh installs for a dedicated --edge-ip)\n", listen)
	} else if port != 22 {
		e.logf("  (gateway is on :%d; run setup with --move-admin-ssh to free :22 for bare `ssh new@host`,\n", port)
		e.logf("   or point it somewhere of its own with --ssh-addr <ip>:22)\n")
	}
	scheme, tlsNote := "http", "  (add --proxy-tls to serve them over HTTPS)"
	if e.Cfg.ProxyTLS {
		scheme, tlsNote = "https", ""
	}
	e.logf("  web routes:        %s://<name>.%s%s\n", scheme, e.Cfg.ProxyDomain, tlsNote)
	switch {
	case e.Cfg.sluiceSocket() == "" || e.Cfg.guestDNS() == "":
		// The same thing checkEgress says, said once where the operator is
		// actually looking. A gateway with no egress control is the default and
		// nothing about a green report suggests it.
		e.logf("  egress:            UNFILTERED — sandboxes reach the whole internet\n")
		e.logf("                     (re-run with --sluice to install and enable the egress gateway)\n")
	case e.Cfg.Sluice:
		// This is a claim of FACT ("tagged ones are filtered"), and it is only
		// printed because Provision returns before this banner on any FAIL and
		// checkSluiceService FAILs on a sluice that is not alive. It used to be
		// printed unconditionally, over a daemon that had crash-looped since the
		// moment it was installed — F7's exact shape on the newer unit. If this
		// branch ever stops being gated by that check, it goes back to lying.
		e.logf("  egress:            sluice on %s, guests resolve through %s\n", e.Cfg.sluiceDNSAddr(), e.Cfg.guestDNS())
		e.logf("                     allowlist: %s (edit, then `systemctl restart %s`)\n", e.Cfg.sluiceAllowlistPath(), sluiceUnit)
		e.logf("                     untagged sandboxes stay unrestricted; tagged ones are filtered\n")
	default:
		// Both halves configured but nothing installed here — the pre-existing
		// hand-installed shape. Nothing to warn about, nothing to claim credit
		// for either.
		e.logf("  egress:            pushing policy to %s, guests resolve through %s\n", e.Cfg.sluiceSocket(), e.Cfg.guestDNS())
	}
}

// appendLineIfMissing appends line (with a trailing newline) to path unless an
// exact line already exists. Creates the file if absent.
func appendLineIfMissing(path, line string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == strings.TrimSpace(line) {
			return nil
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}
