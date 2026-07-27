package hostsetup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
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

// fakeListener answers the port preflight from a map instead of the network
// stack. Tests must never bind a real port: :2222 and :8081 belong to whatever
// the developer happens to be running, so a real listen would make the suite
// pass or fail depending on the machine — and on a live host it would briefly
// steal the gateway's own address.
type fakeListener struct {
	busy  map[string]bool  // "tcp/:8081" -> held by someone (EADDRINUSE)
	errs  map[string]error // any other bind failure, e.g. EADDRNOTAVAIL
	calls []string
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func (f *fakeListener) Listen(network, address string) (io.Closer, error) {
	key := network + "/" + address
	f.calls = append(f.calls, key)
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	if f.busy[key] {
		// Wrapped rather than a bare string: the preflight tells a conflict
		// from an address-not-on-this-host by errno, not by message text.
		return nil, &net.OpError{Op: "listen", Net: network, Err: syscall.EADDRINUSE}
	}
	return nopCloser{}, nil
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
	// Redirected for the same reason as BinPath: the default is the real
	// /usr/local/bin/firecracker, and fetch-artifacts now checks (and would
	// therefore download to) the destination rather than assuming a host with a
	// kernel and a rootfs must already have the hypervisor.
	cfg.FirecrackerBin = filepath.Join(root, "bin", "firecracker")
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
		Listen:     &fakeListener{},
		Log:        &bytes.Buffer{},
		SystemdDir: filepath.Join(root, "systemd"),
		SysctlDir:  filepath.Join(root, "sysctl"),
		SbinDir:    filepath.Join(root, "sbin"),
		// Redirected for the same reason as BinPath and FirecrackerBin: the
		// default is the real /usr/local/bin/sluice, and stepSluice would
		// download to it and chmod it +x.
		SluiceBinPath: filepath.Join(root, "bin", "sluice"),
		FstabPath:     filepath.Join(root, "fstab"),
		SwapPath:      filepath.Join(root, "swapfile"),
		SSHDConfD:     filepath.Join(root, "sshd_config.d"),
		HomeDir:       filepath.Join(root, "home"),
		SelfPath:      self,
	}
	return e, root
}

func TestDryRunMutatesNothing(t *testing.T) {
	e, root := testEnv(t, true)
	buf := &bytes.Buffer{}
	e.Log = buf
	// Resolve works offline via the map fetcher. PLATFORM has to match this
	// host: resolve-release rejects a manifest built for another OS, and the
	// URL the fetcher is keyed by already carries the OS, so serving a linux
	// manifest here would be serving it at the darwin manifest's address.
	e.Fetch = mapFetcher{
		ManifestURL(e.Probe.GOOS(), e.Probe.GOARCH(), e.Cfg.ArtifactBase, e.Cfg.Release): "RELEASE=r1\nPLATFORM=" +
			e.Probe.GOOS() + "\nARCH=" + e.Probe.GOARCH() + "\n",
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
	// A dry run must not open a socket any more than it writes a file — on a
	// live host it would otherwise report the operator's own gateway as a wall
	// of conflicts — but it must still say which addresses it would check.
	if fl, ok := e.Listen.(*fakeListener); !ok || len(fl.calls) != 0 {
		t.Errorf("dry-run bound ports: %v", e.Listen)
	}
	if !strings.Contains(out, "would probe") {
		t.Errorf("the plan should report the port preflight it would run:\n%s", out)
	}
}

// (The swapfile step's idempotency now lives in TestStepSwapCountsExistingSwap,
// which drives it from /proc/swaps rather than from the existence of our own
// path — see F4b.)

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
	// The restart is what turns a rewritten unit into a running one — and for
	// sparkbox-net.service that is the ONLY thing that does. It is Type=oneshot
	// + RemainAfterExit=yes, so `enable --now` is a no-op on any host that has
	// booted with it, and a rewritten packet filter would otherwise sit on disk
	// until the next reboot.
	fr := runnerWith(map[string]string{
		"systemctl is-active sparkbox.service":        "active\n",
		"systemctl daemon-reload":                     "",
		"systemctl enable --now sparkbox-net.service": "",
		"systemctl enable --now sparkbox.service":     "",
		"systemctl restart sparkbox.service":          "",
		"systemctl restart sparkbox-net.service":      "",
	})
	e.Run = fr
	es := stepEnableServices()
	if sat, _, _ := es.Satisfied(e); sat {
		t.Fatal("an active unit whose file just changed must not report satisfied")
	}
	if err := es.Apply(e); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"systemctl daemon-reload", "systemctl restart sparkbox.service", "systemctl restart sparkbox-net.service"} {
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
	// The manifest URL and its PLATFORM key are built from the probe this test
	// installs below (linux/<this arch>), not from runtime.GOOS: the platform is
	// a parameter now, so the fixture describes the host the test is pretending
	// to be rather than the one it happens to run on.
	e.Fetch = mapFetcher{
		ManifestURL("linux", runtime.GOARCH, e.Cfg.ArtifactBase, e.Cfg.Release): "RELEASE=r1\nPLATFORM=linux" +
			"\nARCH=" + runtime.GOARCH + "\n",
	}
	// Let every step run to completion so the run reaches the verify pass: no
	// swapfile, artifacts already on disk, a literal operator key.
	e.Cfg.SwapGB = 0
	e.Cfg.OperatorKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f op@laptop"
	if err := os.MkdirAll(e.Cfg.ImageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(e.Cfg.FirecrackerBin), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{e.Cfg.KernelPath, e.Cfg.rootfsPath(), e.Cfg.FirecrackerBin} {
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
		"systemctl restart sparkbox-net.service":      "",
		"systemctl enable --now " + refreshToolsTimer: "",
		filepath.Join(e.SbinDir, refreshToolsScript):  ">> patching universal.ext4",
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

// TestStepNetAssetsRerendersStaleScript: these files ship with the release, so
// an existence check meant every packet-filter fix stopped at the first host
// that had ever been provisioned — the host most likely to need it.
func TestStepNetAssetsRerendersStaleScript(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Run = runnerWith(map[string]string{"sysctl -q --system": ""})
	s := stepNetAssets()

	if sat, _, _ := s.Satisfied(e); sat {
		t.Fatal("nothing installed yet")
	}
	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	sat, note, err := s.Satisfied(e)
	if err != nil || !sat {
		t.Fatalf("freshly written assets should be satisfied: sat=%v err=%v", sat, err)
	}
	if !strings.Contains(note, "current") {
		t.Errorf("note = %q, want it to claim the scripts are current, not merely present", note)
	}

	// A previous release's script at the same path.
	script := filepath.Join(e.SbinDir, "sparkbox-net.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n# an older release\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if sat, _, _ := s.Satisfied(e); sat {
		t.Fatal("a stale sparkbox-net.sh must not report satisfied")
	}
	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(script); !bytes.Equal(got, deploy.NetScript) {
		t.Error("the stale script survived the re-install")
	}
}

// TestStepFetchArtifactsMatchesRelease: "the files are there" is a weaker claim
// than "the files are THIS release's". A host holding the previous release's
// kernel read as satisfied, so an upgrade went on booting guests on the old one.
func TestStepFetchArtifactsMatchesRelease(t *testing.T) {
	s := stepFetchArtifacts()
	kernel := "kernel bytes"
	fc := "firecracker bytes"

	newHost := func(t *testing.T) *Env {
		t.Helper()
		e, _ := testEnv(t, false)
		e.Manifest = Manifest{
			Release:        "v0.4.0",
			SHA256Vmlinux:  sha256Of(kernel),
			SHA256Firecrkr: sha256Of(fc),
			RootfsName:     "universal",
		}
		mustWrite(t, e.Cfg.KernelPath, kernel)
		mustWrite(t, e.Cfg.FirecrackerBin, fc)
		mustWrite(t, e.Cfg.rootfsPath(), "rootfs")
		return e
	}

	t.Run("matching release is satisfied", func(t *testing.T) {
		e := newHost(t)
		sat, note, err := s.Satisfied(e)
		if err != nil || !sat {
			t.Fatalf("sat=%v err=%v", sat, err)
		}
		if !strings.Contains(note, "v0.4.0") {
			t.Errorf("note = %q, want it to name the release it matched", note)
		}
	})

	t.Run("a previous release's kernel is not satisfied", func(t *testing.T) {
		e := newHost(t)
		mustWrite(t, e.Cfg.KernelPath, "last month's kernel")
		if sat, _, _ := s.Satisfied(e); sat {
			t.Fatal("a kernel from another release must not report satisfied")
		}
	})

	t.Run("a missing firecracker is not satisfied", func(t *testing.T) {
		// It was never looked at here at all, so a host with a kernel and a
		// rootfs reported "present" and the step never ran.
		e := newHost(t)
		if err := os.Remove(e.Cfg.FirecrackerBin); err != nil {
			t.Fatal(err)
		}
		if sat, _, _ := s.Satisfied(e); sat {
			t.Fatal("a host with no hypervisor must not report the artifacts present")
		}
	})

	t.Run("a zero-length artifact is not satisfied", func(t *testing.T) {
		e := newHost(t)
		mustWrite(t, e.Cfg.rootfsPath(), "")
		if sat, _, _ := s.Satisfied(e); sat {
			t.Fatal("an interrupted download leaves a zero-length file; that is not 'present'")
		}
	})

	t.Run("no manifest means the shas are unchecked, and it says so", func(t *testing.T) {
		// --dry-run: resolve-release only ever runs its Apply.
		e := newHost(t)
		e.Manifest = Manifest{}
		sat, note, err := s.Satisfied(e)
		if err != nil || !sat {
			t.Fatalf("sat=%v err=%v", sat, err)
		}
		if !strings.Contains(note, "unchecked") {
			t.Errorf("note = %q, want it to admit the shas were not verified", note)
		}
	})
}

// TestStepEnvFileReconcilesManagedKeys: sparkbox.env used to be written once and
// never looked at again, so `setup --proxy-addr :443` moved the edge and left
// PROXY_PORT=8081 behind — and PROXY_PORT is where sparkbox-net.sh forwards
// every any-port connection.
func TestStepEnvFileReconcilesManagedKeys(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Manifest = Manifest{RootfsLogin: "sparky"}
	s := stepEnvFile()

	if sat, _, _ := s.Satisfied(e); sat {
		t.Fatal("no env file yet")
	}
	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	if sat, note, _ := s.Satisfied(e); !sat || !strings.Contains(note, "current") {
		t.Fatalf("freshly written env should be satisfied/current: sat=%v note=%q", sat, note)
	}

	// The operator's own settings, which must survive everything below.
	orig, err := os.ReadFile(e.Cfg.envPath())
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(orig), "EXTRA_FLAGS=", "EXTRA_FLAGS=--proxy-tls --tls-provider cloudflare", 1)
	edited += "SPARKBOX_CONSOLE_PASSWORD=hunter2\n"
	if err := os.WriteFile(e.Cfg.envPath(), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if sat, _, _ := s.Satisfied(e); !sat {
		t.Fatal("operator edits to unmanaged keys are not drift")
	}

	// Now move the edge. PROXY_PORT must follow it.
	e.Cfg.ProxyAddr = "10.66.0.1:443"
	sat, _, _ := s.Satisfied(e)
	if sat {
		t.Fatal("PROXY_PORT no longer matches --proxy-addr; that is not satisfied")
	}
	if plan := s.Plan(e); !strings.Contains(plan, "PROXY_PORT") || !strings.Contains(plan, "443") {
		t.Errorf("plan should name the drift it is fixing: %q", plan)
	}
	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(e.Cfg.envPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "PROXY_PORT=443") {
		t.Errorf("PROXY_PORT did not follow the edge:\n%s", got)
	}
	// …and the operator's file is otherwise intact. Re-rendering would be
	// simpler and would silently delete a live host's TLS configuration.
	for _, want := range []string{
		"EXTRA_FLAGS=--proxy-tls --tls-provider cloudflare",
		"SPARKBOX_CONSOLE_PASSWORD=hunter2",
		"# sparkbox host config",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the merge lost %q:\n%s", want, got)
		}
	}
	if sat, _, _ := s.Satisfied(e); !sat {
		t.Error("the merged file should be satisfied")
	}
}

// TestStepEnvFileAddsMissingManagedKey: a host provisioned by an older sparkbox
// has no line at all for a key this one manages.
func TestStepEnvFileAddsMissingManagedKey(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Manifest = Manifest{RootfsLogin: "sparky"}
	mustWrite(t, e.Cfg.envPath(), "PROXY_DOMAIN="+e.Cfg.ProxyDomain+"\nPROXY_PORT=8081\nEXTRA_FLAGS=\n")
	s := stepEnvFile()
	if sat, _, _ := s.Satisfied(e); sat {
		t.Fatal("LOGIN_USER_FLAG is missing entirely; that is drift")
	}
	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(e.Cfg.envPath())
	if !strings.Contains(string(got), "LOGIN_USER_FLAG=--default-login-user=sparky") {
		t.Errorf("the missing key was not added:\n%s", got)
	}
}

// dgxEnvFile is the live DGX gateway's sparkbox.env, in the shape that makes
// reconciliation dangerous: a domain no default agrees with, an edge on :443,
// and the pre-A2 workaround — real bind addresses smuggled into TLS_FLAGS,
// which the unit appends last and Go's flag package therefore lets win.
const dgxEnvFile = `# sparkbox host config
PROXY_DOMAIN=catnip.sh
PROXY_PORT=443
SPARKBOX_GUEST_SUBNET=172.30.0.0/16
GUEST_SUBNET_FLAG=--guest-subnet=172.30.0.0/16
SPARKBOX_EDGE_IP=10.66.0.1
SPARKBOX_EDGE_REDIRECT=0
OVERCOMMIT_FLAGS=--mem-reserve-mb 1024
EXTRA_FLAGS=
TLS_FLAGS=--ssh-addr 10.66.0.1:2222 --proxy-addr 10.66.0.1:443 --proxy-tls --tls-provider cloudflare --api-addr 127.0.0.1:8079
GATEWAY_FLAG=
`

// TestStepEnvFileLeavesAnUnaskedDomainAlone: a flag with a default is not an
// opinion. `sparkbox setup --adopt-legacy` to upgrade the DGX's binary passes no
// --proxy-domain, so Cfg.ProxyDomain carries the compiled-in hivemind.tools —
// and reconciling on that basis would rewrite PROXY_DOMAIN=catnip.sh, move every
// sandbox web route and the two consoles onto a domain the box does not serve,
// and send certmagic off to order *.hivemind.tools in a zone its token cannot
// touch. The upgrade also rewrites the unit, so the restart makes it live in the
// same run.
func TestStepEnvFileLeavesAnUnaskedDomainAlone(t *testing.T) {
	s := stepEnvFile()

	t.Run("an upgrade run that never mentions the domain is not drift", func(t *testing.T) {
		e, _ := testEnv(t, false)
		mustWrite(t, e.Cfg.envPath(), dgxEnvFile)
		sat, _, err := s.Satisfied(e)
		if err != nil || !sat {
			t.Fatalf("a live host's own domain must not read as drift: sat=%v err=%v (plan %q)", sat, err, s.Plan(e))
		}
		if err := s.Apply(e); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(e.Cfg.envPath())
		if !strings.Contains(string(got), "PROXY_DOMAIN=catnip.sh") {
			t.Errorf("the live domain was overwritten with the compiled-in default:\n%s", got)
		}
		if e.EnvChanged {
			t.Error("nothing changed, so nothing should be restarted")
		}
	})

	t.Run("an explicit --proxy-domain is reconciled", func(t *testing.T) {
		e, _ := testEnv(t, false)
		mustWrite(t, e.Cfg.envPath(), dgxEnvFile)
		e.Cfg.ProxyDomain = "new.example"
		e.Cfg.FlagsGiven = map[string]bool{"proxy-domain": true}
		if sat, _, _ := s.Satisfied(e); sat {
			t.Fatal("a domain the operator actually asked for must be applied")
		}
		if plan := s.Plan(e); !strings.Contains(plan, "catnip.sh") || !strings.Contains(plan, "new.example") {
			t.Errorf("plan should name the change it is making: %q", plan)
		}
		if err := s.Apply(e); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(e.Cfg.envPath())
		if !strings.Contains(string(got), "PROXY_DOMAIN=new.example") {
			t.Errorf("an explicit --proxy-domain did not land:\n%s", got)
		}
		if !e.EnvChanged {
			t.Error("a rewritten sparkbox.env must force the restart that makes it live")
		}
	})

	t.Run("a file with no PROXY_DOMAIN line at all gets one", func(t *testing.T) {
		// Not the same case: there is no live value to protect, and the unit
		// interpolates ${PROXY_DOMAIN} straight out of this file.
		e, _ := testEnv(t, false)
		mustWrite(t, e.Cfg.envPath(), "PROXY_PORT=8081\nEXTRA_FLAGS=\n")
		if sat, _, _ := s.Satisfied(e); sat {
			t.Fatal("a missing PROXY_DOMAIN is drift")
		}
		if err := s.Apply(e); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(e.Cfg.envPath())
		if !strings.Contains(string(got), "PROXY_DOMAIN="+e.Cfg.ProxyDomain) {
			t.Errorf("the missing key was not added:\n%s", got)
		}
	})
}

// TestManagedProxyPortFollowsTheEffectiveEdge: PROXY_PORT must be derived from
// the address the daemon ACTUALLY binds, not from the one this run templated
// into the unit. On the DGX the edge address lives in TLS_FLAGS, which the unit
// appends after the templated flags — so a run with no --proxy-addr would have
// "corrected" a working PROXY_PORT=443 to the default 8081 and pointed every
// any-port DNAT at a closed port. That is the exact breakage this reconcile
// exists to prevent, caused by the reconcile.
func TestManagedProxyPortFollowsTheEffectiveEdge(t *testing.T) {
	e, _ := testEnv(t, false)
	mustWrite(t, e.Cfg.envPath(), dgxEnvFile)
	s := stepEnvFile()

	if sat, _, _ := s.Satisfied(e); !sat {
		t.Fatalf("PROXY_PORT already matches the effective edge; that is not drift (plan %q)", s.Plan(e))
	}
	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(e.Cfg.envPath())
	if !strings.Contains(string(got), "PROXY_PORT=443") {
		t.Errorf("PROXY_PORT was rewritten away from the port the edge binds:\n%s", got)
	}

	// And the correction still happens when the skew is real: the bundle names
	// :9443 while the file still says 443.
	e2, _ := testEnv(t, false)
	mustWrite(t, e2.Cfg.envPath(), strings.Replace(dgxEnvFile, "--proxy-addr 10.66.0.1:443", "--proxy-addr 10.66.0.1:9443", 1))
	if sat, _, _ := s.Satisfied(e2); sat {
		t.Fatal("a PROXY_PORT that does not match the effective edge IS drift")
	}
	if err := s.Apply(e2); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(e2.Cfg.envPath())
	if !strings.Contains(string(got2), "PROXY_PORT=9443") {
		t.Errorf("PROXY_PORT did not follow the effective edge:\n%s", got2)
	}
}

// TestSSHPortReachesTheNetScript: --ssh-addr made the gateway's SSH port
// configurable and nothing carried it into deploy/sparkbox-net.sh, which
// hardcodes 2222 in three places. So `setup --ssh-addr :2200` produced a host
// whose any-port rules swallowed inbound 2200 (SSH gateway unreachable) while
// sparing an unused 2222, and whose edge-IP :22 DNAT pointed at 2222 where
// nothing listened — killing the bare `ssh ctl@<domain>` the banner advertises.
// Both failures are silent.
func TestSSHPortReachesTheNetScript(t *testing.T) {
	keys := func(ss []envSetting) map[string]string {
		out := map[string]string{}
		for _, s := range ss {
			out[s.key] = s.val
		}
		return out
	}

	t.Run("the script's own default needs no override", func(t *testing.T) {
		// At :2222 the script is already right, and writing the keys anyway
		// would overwrite an operator's hand-tuned exclude list to say exactly
		// what it said before.
		e, _ := testEnv(t, false)
		if got := keys(e.managedEnv(nil)); got["SPARKBOX_GATEWAY_PORT"] != "" || got["PROXY_REDIRECT_EXCLUDE"] != "" {
			t.Errorf("nothing should be written for the default SSH port: %v", got)
		}
	})

	t.Run("a moved SSH port is written for every rule that assumes 2222", func(t *testing.T) {
		e, _ := testEnv(t, false)
		e.Cfg.SSHAddr = ":2200"
		got := keys(e.managedEnv(nil))
		want := map[string]string{
			"SPARKBOX_GATEWAY_PORT":    "2200",
			"PROXY_REDIRECT_EXCLUDE":   "2200",
			"SPARKBOX_TAILNET_EXCLUDE": "22 2200 53 8443",
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q", k, got[k], v)
			}
		}
		// The fresh-host render and the reconcile must agree, or which one ran
		// decides where the packet filter sends SSH.
		fresh := e.renderEnvFile()
		for k, v := range want {
			if !strings.Contains(fresh, k+"="+v) {
				t.Errorf("fresh sparkbox.env missing %s=%s:\n%s", k, v, fresh)
			}
		}
	})

	t.Run("a dedicated edge IP spares only its own SSH port", func(t *testing.T) {
		// Dest-scoped rules never match host services, so the interface-scoped
		// exclude list would be wrong here — and on an interface-scoped host the
		// dest-scoped one un-protects sshd, :53 and a co-tenant serve.
		e, _ := testEnv(t, false)
		e.Cfg.SSHAddr = "10.66.0.1:2200"
		e.Cfg.EdgeIP = "10.66.0.1"
		if got := keys(e.managedEnv(nil)); got["SPARKBOX_TAILNET_EXCLUDE"] != "2200" {
			t.Errorf("SPARKBOX_TAILNET_EXCLUDE = %q, want just the gateway port", got["SPARKBOX_TAILNET_EXCLUDE"])
		}
	})

	t.Run("the address the daemon binds is the one that counts", func(t *testing.T) {
		// The DGX's --ssh-addr rides TLS_FLAGS, which wins over the templated
		// flag — and it lands on 2222, so nothing needs writing.
		e, _ := testEnv(t, false)
		kv, _ := parseEnv(strings.NewReader(dgxEnvFile))
		if got := keys(e.managedEnv(kv)); got["SPARKBOX_GATEWAY_PORT"] != "" {
			t.Errorf("the effective SSH port is 2222; nothing should be overridden: %v", got)
		}
	})

	t.Run("a fleet node has no SSH door", func(t *testing.T) {
		e, _ := testEnv(t, false)
		e.Cfg.SSHAddr = ":2200"
		e.Cfg.Gateway = "gw.example:2222"
		if got := keys(e.managedEnv(nil)); got["SPARKBOX_GATEWAY_PORT"] != "" {
			t.Errorf("a node serves no edge and no SSH gateway: %v", got)
		}
	})
}

// TestEnableServicesRestartsTheNetUnit: sparkbox-net.service is Type=oneshot +
// RemainAfterExit=yes, so `systemctl enable --now` is a no-op on any host that
// has booted with it. Nothing else restarted it, so a corrected PROXY_PORT, a
// new --edge-ip, or the F8 chain-name fix in a re-installed sparkbox-net.sh sat
// on disk doing nothing until the next reboot — under a green "sparkbox is
// provisioned" banner.
func TestEnableServicesRestartsTheNetUnit(t *testing.T) {
	newRunner := func() *fakeRunner {
		return runnerWith(map[string]string{
			"systemctl is-active sparkbox.service":        "active\n",
			"systemctl daemon-reload":                     "",
			"systemctl enable --now sparkbox-net.service": "",
			"systemctl enable --now sparkbox.service":     "",
			"systemctl restart sparkbox.service":          "",
			"systemctl restart sparkbox-net.service":      "",
		})
	}
	s := stepEnableServices()

	t.Run("rewritten packet-filter assets", func(t *testing.T) {
		e, _ := testEnv(t, false)
		fr := newRunner()
		e.Run = fr
		e.NetChanged = true
		if sat, _, _ := s.Satisfied(e); sat {
			t.Fatal("an active host whose rules just changed on disk is not satisfied")
		}
		if err := s.Apply(e); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(fr.calls, "systemctl restart sparkbox-net.service") {
			t.Errorf("the packet filter must be rebuilt in this run, not at the next reboot; calls = %v", fr.calls)
		}
		if slices.Contains(fr.calls, "systemctl restart sparkbox.service") {
			t.Errorf("the gateway itself did not change; restarting it drops live sessions for nothing: %v", fr.calls)
		}
	})

	t.Run("a rewritten sparkbox.env restarts both", func(t *testing.T) {
		// It is the EnvironmentFile= of both units, and systemd reads one only at
		// unit start: PROXY_PORT/SPARKBOX_EDGE_* feed the script, PROXY_DOMAIN
		// feeds the gateway.
		e, _ := testEnv(t, false)
		fr := newRunner()
		e.Run = fr
		e.EnvChanged = true
		if sat, _, _ := s.Satisfied(e); sat {
			t.Fatal("an active host whose env file just changed is not satisfied")
		}
		if err := s.Apply(e); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"systemctl restart sparkbox-net.service", "systemctl restart sparkbox.service"} {
			if !slices.Contains(fr.calls, want) {
				t.Errorf("missing %q in %v", want, fr.calls)
			}
		}
		// Order matters: the DNATs that carry traffic to the edge come up first.
		if slices.Index(fr.calls, "systemctl restart sparkbox-net.service") > slices.Index(fr.calls, "systemctl restart sparkbox.service") {
			t.Errorf("the packet filter should be restarted before the gateway: %v", fr.calls)
		}
	})

	t.Run("nothing changed restarts nothing", func(t *testing.T) {
		e, _ := testEnv(t, false)
		fr := newRunner()
		e.Run = fr
		if sat, _, _ := s.Satisfied(e); !sat {
			t.Fatal("an active unit with nothing changed is satisfied")
		}
		for _, call := range fr.calls {
			if strings.HasPrefix(call, "systemctl restart") {
				t.Errorf("nothing changed, yet %q ran", call)
			}
		}
	})
}

// TestCheckRoleSwitch: the gateway/node role lives in one variable setup does
// not rewrite, so provisioning across it would lay half of one role over the
// other and report success.
func TestCheckRoleSwitch(t *testing.T) {
	cases := []struct {
		name       string
		envGateway string // GATEWAY_FLAG on disk; "-" means no env file at all
		cfgGateway string
		wantErr    string
	}{
		{name: "fresh host", envGateway: "-", cfgGateway: ""},
		{name: "gateway stays a gateway", envGateway: ""},
		{name: "node stays a node", envGateway: "--gateway gw:2222 --node-name a", cfgGateway: "gw:2222"},
		{
			// A rename is not a role switch: the same host, a different label.
			name: "node renamed", envGateway: "--gateway gw:2222 --node-name a", cfgGateway: "gw:2222",
		},
		{
			name: "node re-run as a gateway", envGateway: "--gateway gw:2222 --node-name a",
			wantErr: "provisioned as a fleet NODE",
		},
		{
			name: "gateway re-run as a node", envGateway: "", cfgGateway: "gw:2222",
			wantErr: "provisioned as a standalone GATEWAY",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := testEnv(t, false)
			e.Cfg.Gateway = tc.cfgGateway
			if tc.envGateway != "-" {
				mustWrite(t, e.Cfg.envPath(), "PROXY_DOMAIN=x\nGATEWAY_FLAG="+tc.envGateway+"\n")
			}
			err := checkRoleSwitch(e)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a role switch in place must be refused")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "systemctl stop sparkbox") {
				t.Errorf("a refusal must say how to proceed deliberately: %v", err)
			}
		})
	}
}

// sha256Of is the test-side twin of sha256File.
func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestEnvFileContents(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Manifest = Manifest{RootfsLogin: "sparky"}
	e.Cfg.ProxyDomain = "example.tools"
	out := e.renderEnvFile()
	for _, want := range []string{
		"PROXY_DOMAIN=example.tools",
		"LOGIN_USER_FLAG=--default-login-user=sparky",
		"OVERCOMMIT_FLAGS=",
		"EXTRA_FLAGS=",
		// TLS_FLAGS keeps its line even though EXTRA_FLAGS supersedes it: the
		// unit still references it, hosts provisioned before EXTRA_FLAGS
		// existed carry real bind configuration in it, and sparkbox.env is
		// never rewritten once it exists — so dropping either half would
		// silently delete a live host's edge configuration.
		"TLS_FLAGS=",
		// The any-port mode sparkbox-net.sh reads, and doctor reads back.
		// Written on every fresh host so both knobs are visible and the mode is
		// not something an operator has to know to look for.
		"SPARKBOX_EDGE_IP=",
		"SPARKBOX_EDGE_REDIRECT=1",
	} {
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
