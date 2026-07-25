package hostsetup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	Log      io.Writer
	Manifest Manifest

	SystemdDir string // /etc/systemd/system
	SysctlDir  string // /etc/sysctl.d
	SbinDir    string // /usr/local/sbin
	FstabPath  string // /etc/fstab
	SwapPath   string // /swapfile
	SSHDConfD  string // /etc/ssh/sshd_config.d
	HomeDir    string // operator-key auto-detect root (~)
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
		Probe:      System(),
		SystemdDir: "/etc/systemd/system",
		SysctlDir:  "/etc/sysctl.d",
		SbinDir:    "/usr/local/sbin",
		FstabPath:  "/etc/fstab",
		SwapPath:   "/swapfile",
		SSHDConfD:  "/etc/ssh/sshd_config.d",
		HomeDir:    home,
		SelfPath:   self,
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
		stepSystemdUnits(),
		stepAdminSSH(),
		stepEnableServices(),
	}
}

// --- steps ------------------------------------------------------------------

func stepSwap() Step {
	return Step{
		Name: "swapfile",
		Satisfied: func(e *Env) (bool, string, error) {
			if e.Cfg.SwapGB <= 0 {
				return true, "disabled", nil
			}
			if _, err := os.Stat(e.SwapPath); err == nil {
				return true, e.SwapPath + " exists", nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			return fmt.Sprintf("create %dG %s (overcommit safety valve) + swapon + fstab", e.Cfg.SwapGB, e.SwapPath)
		},
		Apply: func(e *Env) error {
			// dd (not fallocate) so swapon never trips over holes.
			if _, err := e.run("dd", "if=/dev/zero", "of="+e.SwapPath, "bs=1M",
				fmt.Sprintf("count=%d", e.Cfg.SwapGB*1024), "status=none"); err != nil {
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
			if _, err := e.run("mountpoint", "-q", e.Cfg.dataDir()); err == nil {
				return true, "mounted at " + e.Cfg.dataDir(), nil
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
		Satisfied: func(e *Env) (bool, string, error) {
			// Cheap idempotency: the kernel + decompressed rootfs already on disk.
			// downloadVerify re-verifies shas on Apply, so this is only a fast path.
			_, kerr := os.Stat(e.Cfg.KernelPath)
			_, rerr := os.Stat(e.Cfg.rootfsPath())
			if kerr == nil && rerr == nil {
				return true, "kernel + rootfs present", nil
			}
			return false, "", nil
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

func stepEnvFile() Step {
	return Step{
		Name: "host-config",
		Satisfied: func(e *Env) (bool, string, error) {
			if _, err := os.Stat(e.Cfg.envPath()); err == nil {
				return true, e.Cfg.envPath() + " exists", nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string { return "write " + e.Cfg.envPath() },
		Apply: func(e *Env) error {
			if err := os.MkdirAll(filepath.Dir(e.Cfg.envPath()), 0o755); err != nil {
				return err
			}
			return os.WriteFile(e.Cfg.envPath(), []byte(e.renderEnvFile()), 0o644)
		},
	}
}

func stepNetAssets() Step {
	return Step{
		Name: "net-rules",
		Satisfied: func(e *Env) (bool, string, error) {
			_, a := os.Stat(filepath.Join(e.SbinDir, "sparkbox-net.sh"))
			_, b := os.Stat(filepath.Join(e.SysctlDir, "99-sparkbox.conf"))
			if a == nil && b == nil {
				return true, "scripts + sysctl installed", nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			return fmt.Sprintf("install sparkbox-net.sh + %s/99-sparkbox.conf, apply sysctl", e.SysctlDir)
		},
		Apply: func(e *Env) error {
			if err := os.MkdirAll(e.SbinDir, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(e.SbinDir, "sparkbox-net.sh"), deploy.NetScript, 0o755); err != nil {
				return err
			}
			if err := os.MkdirAll(e.SysctlDir, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(e.SysctlDir, "99-sparkbox.conf"), deploy.SysctlConf, 0o644); err != nil {
				return err
			}
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
				if err := os.WriteFile(filepath.Join(e.SystemdDir, u[0]), []byte(u[1]), 0o644); err != nil {
					return err
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

func stepAdminSSH() Step {
	return Step{
		Name: "admin-ssh",
		Satisfied: func(e *Env) (bool, string, error) {
			if !e.Cfg.MoveAdminSSH {
				return true, "skipped (--move-admin-ssh not set; gateway binds :2222)", nil
			}
			if _, err := os.Stat(filepath.Join(e.SSHDConfD, "sparkbox-admin-port.conf")); err == nil {
				return true, "admin sshd already on :2222", nil
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
			if err := os.WriteFile(filepath.Join(e.SSHDConfD, "sparkbox-admin-port.conf"), []byte("Port 2222\n"), 0o644); err != nil {
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
			// old --bin-path included). Apply below turns both into a restart.
			if e.BinaryInstalled || e.UnitsChanged {
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
			return "systemctl daemon-reload; enable --now sparkbox-net.service + " + serviceUnit
		},
		Apply: func(e *Env) error {
			if _, err := e.run("systemctl", "daemon-reload"); err != nil {
				return err
			}
			if _, err := e.run("systemctl", "enable", "--now", "sparkbox-net.service"); err != nil {
				return err
			}
			if _, err := e.run("systemctl", "enable", "--now", serviceUnit); err != nil {
				return err
			}
			// `enable --now` is a no-op on a unit that is already running, so a
			// swapped binary — or a rewritten unit — needs an explicit restart to
			// actually take effect. Unconditional after a change (rather than
			// only when it was already active) because "was it running a moment
			// ago" is a race we would lose silently; one extra restart of a
			// just-started unit is cheap.
			if e.BinaryInstalled || e.UnitsChanged {
				what := "binary"
				if e.UnitsChanged {
					what = "unit"
					if e.BinaryInstalled {
						what = "binary + unit"
					}
				}
				e.logf("   %s changed — restarting %s so it runs the new build\n", what, serviceUnit)
				if _, err := e.run("systemctl", "restart", serviceUnit); err != nil {
					return err
				}
			}
			return nil
		},
	}
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
	var b strings.Builder
	b.WriteString("# sparkbox host config, sourced by sparkbox.service + sparkbox-net.service.\n")
	b.WriteString("# Flags MUST be referenced unbraced in the units ($TLS_FLAGS, not ${TLS_FLAGS}).\n")
	fmt.Fprintf(&b, "PROXY_DOMAIN=%s\n", e.Cfg.ProxyDomain)
	fmt.Fprintf(&b, "LOGIN_USER_FLAG=%s\n", login)
	b.WriteString("SUBNET6_FLAG=\n")
	b.WriteString("SUBNET6=\n")
	fmt.Fprintf(&b, "PROXY_PORT=%d\n", proxyPort)
	b.WriteString("# Live memory overcommit + density defaults (retune with hack/measure-density.py):\n")
	b.WriteString("OVERCOMMIT_FLAGS=--mem-reserve-mb 1024 --max-running-per-owner 50\n")
	b.WriteString("# HTTPS edge: set e.g. TLS_FLAGS=--proxy-addr :443 --proxy-tls --tls-provider autocert --tls-email you@example.com\n")
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
	port := "2222"
	if e.Cfg.MoveAdminSSH {
		port = "22"
	}
	e.logf("\n== sparkbox is provisioned ==\n")
	e.logf("  create a sandbox:  ssh -p %s new@<this-host>\n", port)
	e.logf("  shell into it:     ssh -p %s <name>@<this-host>\n", port)
	e.logf("  health check:      sparkbox doctor\n")
	e.logf("  logs:              journalctl -u sparkbox -f\n")
	if !e.Cfg.MoveAdminSSH {
		e.logf("  (gateway is on :2222; run setup with --move-admin-ssh to free :22 for bare `ssh new@host`)\n")
	}
	e.logf("  web routes:        https://<name>.%s  (add --proxy-tls via TLS_FLAGS in %s)\n", e.Cfg.ProxyDomain, e.Cfg.envPath())
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
