package hostsetup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/deploy"
)

// fakeRunner records commands and returns canned results keyed by the first arg
// pair. A missing key returns a non-zero-style error so "mountpoint -q" reports
// "not mounted" by default.
type fakeRunner struct {
	results map[string]struct {
		out []byte
		err error
	}
	calls []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, key)
	if r, ok := f.results[key]; ok {
		return r.out, r.err
	}
	return nil, os.ErrNotExist
}

// testEnv builds an Env rooted entirely in a tempdir so no real host path is
// touched. BinPath and SelfPath in particular: left at their defaults the
// install step would hash the go test binary and write it to the developer's
// real /usr/local/bin/sparkbox.
func testEnv(t *testing.T, dry bool) (*Env, string) {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Root = filepath.Join(root, "srv")
	cfg.StateDir = filepath.Join(cfg.Root, "data", "state")
	cfg.ImageDir = filepath.Join(cfg.Root, "data", "images")
	cfg.KernelPath = filepath.Join(cfg.Root, "vmlinux")
	cfg.UsersPath = filepath.Join(cfg.Root, "users.conf")
	cfg.BinPath = filepath.Join(root, "bin", "sparkbox")
	cfg.Version = "v0.4.0"
	cfg.DryRun = dry
	self := filepath.Join(root, "self", "sparkbox")
	if err := os.MkdirAll(filepath.Dir(self), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self, []byte("#!/bin/false\nsparkbox v0.4.0 build\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Env{
		Ctx: context.Background(), Cfg: cfg,
		Run:        &fakeRunner{},
		Fetch:      mapFetcher{},
		Probe:      fakeProbe{},
		Log:        &bytes.Buffer{},
		SystemdDir: filepath.Join(root, "systemd"),
		SysctlDir:  filepath.Join(root, "sysctl"),
		SbinDir:    filepath.Join(root, "sbin"),
		FstabPath:  filepath.Join(root, "fstab"),
		SwapPath:   filepath.Join(root, "swapfile"),
		SSHDConfD:  filepath.Join(root, "sshd_config.d"),
		HomeDir:    filepath.Join(root, "home"),
		SelfPath:   self,
	}
	return e, root
}

func TestDryRunMutatesNothing(t *testing.T) {
	e, root := testEnv(t, true)
	buf := &bytes.Buffer{}
	e.Log = buf
	// Resolve works offline via the map fetcher.
	e.Fetch = mapFetcher{
		ManifestURL(e.Cfg.ArtifactBase, e.Cfg.Release): "RELEASE=r1\nARCH=" + runtime.GOARCH + "\n",
	}
	if err := Provision(e); err != nil {
		t.Fatalf("dry-run Provision: %v", err)
	}
	out := buf.String()
	for _, name := range []string{"swapfile", "install-binary", "data-volume", "fetch-artifacts", "users.conf", "systemd-units", "enable-services"} {
		if !strings.Contains(out, name) {
			t.Errorf("plan missing step %q\n%s", name, out)
		}
	}
	// The install must be *named* in the plan, not just listed: F0 was invisible
	// precisely because nothing in setup's output ever mentioned the binary.
	if !strings.Contains(out, e.Cfg.BinPath) {
		t.Errorf("plan should say where the binary is installed (%s)\n%s", e.Cfg.BinPath, out)
	}
	if !strings.Contains(out, "dry run — nothing was changed") {
		t.Error("dry-run should announce it changed nothing")
	}
	// Nothing under any system dir should have been created.
	for _, d := range []string{"systemd", "sysctl", "sbin", "bin"} {
		if _, err := os.Stat(filepath.Join(root, d)); err == nil {
			t.Errorf("dry-run created %s/", d)
		}
	}
	if _, err := os.Stat(e.Cfg.UsersPath); err == nil {
		t.Error("dry-run wrote users.conf")
	}
}

func TestStepSwapIdempotent(t *testing.T) {
	e, _ := testEnv(t, false)
	s := stepSwap()
	// Not satisfied initially.
	if sat, _, _ := s.Satisfied(e); sat {
		t.Fatal("swap should be unsatisfied before apply")
	}
	// Simulate the swapfile existing → satisfied, Apply must be skippable.
	os.WriteFile(e.SwapPath, []byte("x"), 0o600)
	if sat, _, _ := s.Satisfied(e); !sat {
		t.Fatal("swap should be satisfied once the file exists")
	}
	// Disabled swap is trivially satisfied.
	e.Cfg.SwapGB = 0
	os.Remove(e.SwapPath)
	if sat, note, _ := s.Satisfied(e); !sat || note != "disabled" {
		t.Fatalf("SwapGB=0 should be satisfied/disabled, got sat=%v note=%q", sat, note)
	}
}

func TestStepUsersConfWritesAndValidates(t *testing.T) {
	e, _ := testEnv(t, false)
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f op@laptop"
	e.Cfg.OperatorKey = key // literal
	s := stepUsersConf()
	if sat, _, _ := s.Satisfied(e); sat {
		t.Fatal("users.conf should be unsatisfied before apply")
	}
	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.Cfg.UsersPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), e.Cfg.OperatorHandle+" ssh-ed25519 ") {
		t.Errorf("users.conf = %q", b)
	}
	if sat, _, _ := s.Satisfied(e); !sat {
		t.Error("users.conf should be satisfied after apply")
	}
	// A bad key is rejected.
	e.Cfg.OperatorKey = "not-a-key"
	if err := s.Apply(e); err == nil {
		t.Error("invalid operator key should error")
	}
}

func TestStepAdminSSHGatedOff(t *testing.T) {
	e, _ := testEnv(t, false)
	// Default: MoveAdminSSH is false → the step is satisfied (a no-op) and never
	// touches sshd. This is the safety default: setup must not evict :22 unasked.
	e.Cfg.MoveAdminSSH = false
	s := stepAdminSSH()
	sat, note, err := s.Satisfied(e)
	if err != nil || !sat {
		t.Fatalf("admin-ssh should be satisfied (skipped) when not requested: %v", err)
	}
	if !strings.Contains(note, "skipped") {
		t.Errorf("note = %q, want skipped", note)
	}
	if _, err := os.Stat(filepath.Join(e.SSHDConfD, "sparkbox-admin-port.conf")); err == nil {
		t.Error("gated-off admin-ssh must not write a drop-in")
	}
}

func TestStepEnableServicesReadsIsActive(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Run = runnerWith(map[string]string{"systemctl is-active sparkbox.service": "active\n"})
	if sat, _, _ := stepEnableServices().Satisfied(e); !sat {
		t.Error("enable-services should be satisfied when the unit is active")
	}
}

// TestStepEnableServicesRestartsAfterBinarySwap is the second half of F0: the
// running process keeps executing the OLD inode after the rename, so an active
// unit whose binary just changed is not "already satisfied" — it needs a
// restart, or the box goes on running the previous release.
func TestStepEnableServicesRestartsAfterBinarySwap(t *testing.T) {
	e, _ := testEnv(t, false)
	fr := runnerWith(map[string]string{
		"systemctl is-active sparkbox.service":        "active\n",
		"systemctl daemon-reload":                     "",
		"systemctl enable --now sparkbox-net.service": "",
		"systemctl enable --now sparkbox.service":     "",
		"systemctl restart sparkbox.service":          "",
	})
	e.Run = fr
	e.BinaryInstalled = true
	s := stepEnableServices()
	if sat, _, _ := s.Satisfied(e); sat {
		t.Fatal("an active unit running the OLD binary must not report satisfied")
	}
	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fr.calls, "systemctl restart sparkbox.service") {
		t.Errorf("apply should restart the unit onto the new binary; calls = %v", fr.calls)
	}
}

// TestStepSystemdUnitsRerendersOnConfigChange guards the other half of the
// --bin-path promise: install-binary writes the new build wherever the operator
// asked, so the unit has to follow. An existence-only Satisfied left ExecStart
// pointing at the previous path — setup then restarted the OLD binary and
// exited 0 — which is F0's silent-staleness shape one level up, and it applies
// to every unit fix shipped in a release, not just --bin-path.
func TestStepSystemdUnitsRerendersOnConfigChange(t *testing.T) {
	e, _ := testEnv(t, false)
	s := stepSystemdUnits()
	if sat, _, err := s.Satisfied(e); err != nil || sat {
		t.Fatalf("no units on a fresh host: sat=%v err=%v", sat, err)
	}
	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	if !e.UnitsChanged {
		t.Error("a unit write must be recorded so enable-services reloads and restarts")
	}
	sat, note, err := s.Satisfied(e)
	if err != nil || !sat {
		t.Fatalf("freshly written units should be satisfied: sat=%v err=%v", sat, err)
	}
	if !strings.Contains(note, "current") {
		t.Errorf("note = %q, want it to claim the units are current, not merely present", note)
	}

	// Now the operator re-runs with a different --bin-path.
	e.UnitsChanged = false
	e.Cfg.BinPath = filepath.Join(t.TempDir(), "opt", "sparkbox")
	if sat, note, _ := s.Satisfied(e); sat {
		t.Fatalf("a unit whose ExecStart names the OLD --bin-path must not be satisfied (note %q)", note)
	}
	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(e.SystemdDir, "sparkbox.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "ExecStart="+e.Cfg.BinPath+" serve") {
		t.Errorf("ExecStart did not follow --bin-path to %s:\n%s", e.Cfg.BinPath, unit)
	}

	// …and the same must hold for a unit shipped with the release: a template
	// change has to reach a host that was provisioned by an older sparkbox.
	e.UnitsChanged = false
	stale := filepath.Join(e.SystemdDir, "sparkbox-net.service")
	if err := os.WriteFile(stale, []byte("[Unit]\nDescription=an older release's unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sat, _, _ := s.Satisfied(e); sat {
		t.Fatal("a stale sparkbox-net.service must not report satisfied")
	}
	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(stale); !bytes.Equal(got, deploy.NetService) {
		t.Error("the stale unit survived the re-render")
	}
	// The restart is what turns a rewritten unit into a running one.
	fr := runnerWith(map[string]string{
		"systemctl is-active sparkbox.service":        "active\n",
		"systemctl daemon-reload":                     "",
		"systemctl enable --now sparkbox-net.service": "",
		"systemctl enable --now sparkbox.service":     "",
		"systemctl restart sparkbox.service":          "",
	})
	e.Run = fr
	es := stepEnableServices()
	if sat, _, _ := es.Satisfied(e); sat {
		t.Fatal("an active unit whose file just changed must not report satisfied")
	}
	if err := es.Apply(e); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"systemctl daemon-reload", "systemctl restart sparkbox.service"} {
		if !slices.Contains(fr.calls, want) {
			t.Errorf("a rewritten unit must be reloaded and restarted; missing %q in %v", want, fr.calls)
		}
	}
}

// TestStepInstallBinary is F0 proper: setup must put the binary it is running
// at the path the unit's ExecStart names.
func TestStepInstallBinary(t *testing.T) {
	s := stepInstallBinary()

	t.Run("fresh host installs the running binary", func(t *testing.T) {
		e, _ := testEnv(t, false)
		if sat, _, err := s.Satisfied(e); err != nil || sat {
			t.Fatalf("nothing at %s yet: sat=%v err=%v", e.Cfg.BinPath, sat, err)
		}
		if err := s.Apply(e); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(e.Cfg.BinPath)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := os.ReadFile(e.SelfPath)
		if !bytes.Equal(got, want) {
			t.Errorf("installed bytes differ from the running binary")
		}
		fi, err := os.Stat(e.Cfg.BinPath)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("installed binary is not executable (mode %v)", fi.Mode().Perm())
		}
		if !e.BinaryInstalled {
			t.Error("a swap must be recorded so enable-services restarts the unit")
		}
		if _, err := os.Stat(e.Cfg.BinPath + ".tmp"); err == nil {
			t.Error("the staging file must not survive a successful install")
		}
		if sat, note, _ := s.Satisfied(e); !sat || !strings.Contains(note, "identical") {
			t.Errorf("after apply: sat=%v note=%q, want satisfied/identical", sat, note)
		}
	})

	t.Run("byte-identical destination is satisfied and never rewritten", func(t *testing.T) {
		e, _ := testEnv(t, false)
		if err := installFile(e.SelfPath, e.Cfg.BinPath); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(e.Cfg.BinPath)
		if err != nil {
			t.Fatal(err)
		}
		sat, note, err := s.Satisfied(e)
		if err != nil || !sat {
			t.Fatalf("identical destination should be satisfied: sat=%v err=%v", sat, err)
		}
		if !strings.Contains(note, "identical") {
			t.Errorf("note = %q, want it to say the binary is already identical", note)
		}
		after, _ := os.Stat(e.Cfg.BinPath)
		if !after.ModTime().Equal(before.ModTime()) {
			t.Error("Satisfied must not touch the destination — it runs on every setup, live service or not")
		}
	})

	t.Run("stale destination is overwritten", func(t *testing.T) {
		e, _ := testEnv(t, false)
		// The DGX case: an older build already sitting at the install path.
		if err := os.MkdirAll(filepath.Dir(e.Cfg.BinPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.Cfg.BinPath, []byte("stale v0.3.0"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.Run = runnerWith(map[string]string{
			e.Cfg.BinPath + " version": "sparkbox v0.3.0 (linux/arm64)\n",
		})
		if sat, _, _ := s.Satisfied(e); sat {
			t.Fatal("a different binary at the destination must not report satisfied")
		}
		if err := s.Apply(e); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(e.Cfg.BinPath)
		if string(got) == "stale v0.3.0" {
			t.Error("the stale binary survived the install")
		}
	})

	t.Run("setup run from the install path is a no-op", func(t *testing.T) {
		e, _ := testEnv(t, false)
		if err := installFile(e.SelfPath, e.Cfg.BinPath); err != nil {
			t.Fatal(err)
		}
		e.SelfPath = e.Cfg.BinPath
		sat, note, err := s.Satisfied(e)
		if err != nil || !sat {
			t.Fatalf("running from the install path should be satisfied: sat=%v err=%v", sat, err)
		}
		if !strings.Contains(note, "running from") {
			t.Errorf("note = %q, want it to say we are already running from there", note)
		}
	})

	t.Run("refuses to downgrade a newer destination without --force", func(t *testing.T) {
		e, _ := testEnv(t, false) // Cfg.Version is v0.4.0
		if err := os.MkdirAll(filepath.Dir(e.Cfg.BinPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.Cfg.BinPath, []byte("newer"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.Run = runnerWith(map[string]string{
			e.Cfg.BinPath + " version": "sparkbox v0.5.0 (linux/arm64)\n",
		})
		err := s.Apply(e)
		if err == nil {
			t.Fatal("installing over a newer binary must fail without --force")
		}
		for _, want := range []string{"v0.5.0", "v0.4.0", "--force"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q: %v", want, err)
			}
		}
		if got, _ := os.ReadFile(e.Cfg.BinPath); string(got) != "newer" {
			t.Error("a refused install must leave the destination untouched")
		}
		if e.BinaryInstalled {
			t.Error("a refused install must not claim the binary changed")
		}

		// …and --force goes through.
		e.Cfg.Force = true
		if err := s.Apply(e); err != nil {
			t.Fatalf("--force should overwrite a newer binary: %v", err)
		}
		if got, _ := os.ReadFile(e.Cfg.BinPath); string(got) == "newer" {
			t.Error("--force did not overwrite the destination")
		}
	})

	t.Run("an unreadable destination version is reported, not fatal", func(t *testing.T) {
		// Whatever is at the path may not be sparkbox, or may not run at all
		// (wrong arch). "Cannot tell" must not strand the host.
		e, _ := testEnv(t, false)
		if err := os.MkdirAll(filepath.Dir(e.Cfg.BinPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.Cfg.BinPath, []byte("not sparkbox at all"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.Run = &fakeRunner{} // every command errors
		if err := s.Apply(e); err != nil {
			t.Fatalf("an uncomparable destination should be overwritten, not refused: %v", err)
		}
	})

	t.Run("skipped when --bin-path is empty", func(t *testing.T) {
		e, _ := testEnv(t, false)
		e.Cfg.BinPath = ""
		sat, note, err := s.Satisfied(e)
		if err != nil || !sat || !strings.Contains(note, "skipped") {
			t.Fatalf("empty --bin-path should skip: sat=%v note=%q err=%v", sat, note, err)
		}
	})
}

// TestProvisionFailsWhenVerificationFails is the other half of F7: the post-apply
// report used to be printed and then ignored, so setup announced success over a
// dead gateway. A FAIL there must reach the exit code.
func TestProvisionFailsWhenVerificationFails(t *testing.T) {
	e, _ := testEnv(t, false)
	buf := &bytes.Buffer{}
	e.Log = buf
	e.Fetch = mapFetcher{
		ManifestURL(e.Cfg.ArtifactBase, e.Cfg.Release): "RELEASE=r1\nARCH=" + runtime.GOARCH + "\n",
	}
	// Let every step run to completion so the run reaches the verify pass: no
	// swapfile, artifacts already on disk, a literal operator key.
	e.Cfg.SwapGB = 0
	e.Cfg.OperatorKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f op@laptop"
	if err := os.MkdirAll(e.Cfg.ImageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{e.Cfg.KernelPath, e.Cfg.rootfsPath()} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A host that passes preflight but whose gateway is crash-looping: the same
	// "active" both samples, with the restart counter climbing under it.
	p := newScriptedProbe(map[string][]string{
		showCmd: {
			unitState("active", "running", "3", "800", "50000"),
			unitState("active", "running", "5", "930", "70000"),
		},
		"journalctl -u " + serviceUnit + " -n 20 --no-pager": {"sparkbox: listen tcp 127.0.0.1:8080: bind: address already in use"},
	})
	p.fakeProbe = fakeProbe{
		goos: "linux", goarch: runtime.GOARCH, uid: 0,
		files:    map[string]string{"/dev/kvm": "", "/proc/cpuinfo": "flags: vmx"},
		writable: map[string]bool{"/dev/kvm": true},
	}
	e.Probe = p
	e.Run = runnerWith(map[string]string{
		"mountpoint -q " + e.Cfg.dataDir():            "",
		"sysctl -q --system":                          "",
		"systemctl daemon-reload":                     "",
		"systemctl enable --now sparkbox-net.service": "",
		"systemctl enable --now sparkbox.service":     "",
		"systemctl restart sparkbox.service":          "",
	})

	err := Provision(e)
	if err == nil {
		t.Fatal("Provision must return an error when the post-apply checks FAIL")
	}
	out := buf.String()
	if strings.Contains(out, "== sparkbox is provisioned ==") {
		t.Error("setup must not announce success over a failing verification")
	}
	if !strings.Contains(out, "bind: address already in use") {
		t.Errorf("the failing journal lines must be inlined in the report:\n%s", out)
	}
	if !strings.Contains(out, "crash-looping") {
		t.Errorf("the report should name the crash loop:\n%s", out)
	}
}

// runnerWith builds a fakeRunner whose listed commands succeed with the given
// output; anything else errors, which is how the real thing behaves for a
// missing binary.
func runnerWith(results map[string]string) *fakeRunner {
	m := map[string]struct {
		out []byte
		err error
	}{}
	for k, v := range results {
		m[k] = struct {
			out []byte
			err error
		}{out: []byte(v)}
	}
	return &fakeRunner{results: m}
}

func TestEnvFileContents(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Manifest = Manifest{RootfsLogin: "sparky"}
	e.Cfg.ProxyDomain = "example.tools"
	out := e.renderEnvFile()
	for _, want := range []string{"PROXY_DOMAIN=example.tools", "LOGIN_USER_FLAG=--default-login-user=sparky", "TLS_FLAGS=", "OVERCOMMIT_FLAGS="} {
		if !strings.Contains(out, want) {
			t.Errorf("env file missing %q\n%s", want, out)
		}
	}
}

func TestNodeEnvFileContents(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Cfg.Gateway = "gateway.example:2222"
	e.Cfg.NodeName = "mac-studio"
	out := e.renderEnvFile()
	if !strings.Contains(out, "GATEWAY_FLAG=--gateway gateway.example:2222 --node-name mac-studio") {
		t.Fatalf("node env file missing gateway flags:\n%s", out)
	}
}
