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
	Log      io.Writer
	Manifest Manifest

	SystemdDir string // /etc/systemd/system
	SysctlDir  string // /etc/sysctl.d
	SbinDir    string // /usr/local/sbin
	FstabPath  string // /etc/fstab
	SwapPath   string // /swapfile
	SSHDConfD  string // /etc/ssh/sshd_config.d
	HomeDir    string // operator-key auto-detect root (~)
}

// NewEnv builds an Env with the real on-host system paths.
func NewEnv(ctx context.Context, cfg Config, run Runner, fetch Fetcher, log io.Writer) *Env {
	home, _ := os.UserHomeDir()
	return &Env{
		Ctx: ctx, Cfg: cfg, Run: run, Fetch: fetch, Log: log,
		SystemdDir: "/etc/systemd/system",
		SysctlDir:  "/etc/sysctl.d",
		SbinDir:    "/usr/local/sbin",
		FstabPath:  "/etc/fstab",
		SwapPath:   "/swapfile",
		SSHDConfD:  "/etc/ssh/sshd_config.d",
		HomeDir:    home,
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
	// Preflight: the host-capability subset must pass before we touch anything.
	pre := RunChecks(System(), e.Cfg, preflightChecks())
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

	// Verify and print how to connect.
	e.logf("\n== verify ==\n")
	PrintResults(e.Log, RunChecks(System(), e.Cfg, DefaultChecks()))
	e.printConnect()
	return nil
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
			tag, err := ResolveRelease(e.Ctx, e.Cfg.ArtifactBase, e.Cfg.Release, e.Fetch)
			if err != nil {
				return err
			}
			rc, err := e.Fetch.Get(e.Ctx, ManifestURL(e.Cfg.ArtifactBase, tag))
			if err != nil {
				return fmt.Errorf("fetch manifest: %w", err)
			}
			defer rc.Close()
			m, err := ParseManifest(rc, tag)
			if err != nil {
				return err
			}
			e.Manifest = m
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

func stepSystemdUnits() Step {
	return Step{
		Name: "systemd-units",
		Satisfied: func(e *Env) (bool, string, error) {
			_, a := os.Stat(filepath.Join(e.SystemdDir, "sparkbox.service"))
			_, b := os.Stat(filepath.Join(e.SystemdDir, "sparkbox-net.service"))
			if a == nil && b == nil {
				return true, "units installed", nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			return "install sparkbox.service (standalone) + sparkbox-net.service"
		},
		Apply: func(e *Env) error {
			if err := os.MkdirAll(e.SystemdDir, 0o755); err != nil {
				return err
			}
			svc, err := renderService(e.Cfg)
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(e.SystemdDir, "sparkbox.service"), []byte(svc), 0o644); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(e.SystemdDir, "sparkbox-net.service"), deploy.NetService, 0o644)
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
			out, _ := e.run("systemctl", "is-active", "sparkbox.service")
			if strings.TrimSpace(string(out)) == "active" {
				return true, "sparkbox.service active", nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			return "systemctl daemon-reload; enable --now sparkbox-net.service + sparkbox.service"
		},
		Apply: func(e *Env) error {
			if _, err := e.run("systemctl", "daemon-reload"); err != nil {
				return err
			}
			if _, err := e.run("systemctl", "enable", "--now", "sparkbox-net.service"); err != nil {
				return err
			}
			_, err := e.run("systemctl", "enable", "--now", "sparkbox.service")
			return err
		},
	}
}

// --- helpers ----------------------------------------------------------------

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
	b.WriteString("# Operator console: uncomment to enable console.<domain>.\n")
	b.WriteString("# SPARKBOX_CONSOLE_PASSWORD=change-me\n")
	return b.String()
}

func (e *Env) printConnect() {
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
