package hostsetup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

// testEnv builds an Env rooted entirely in a tempdir so no real host path is touched.
func testEnv(t *testing.T, dry bool) (*Env, string) {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Root = filepath.Join(root, "srv")
	cfg.StateDir = filepath.Join(cfg.Root, "data", "state")
	cfg.ImageDir = filepath.Join(cfg.Root, "data", "images")
	cfg.KernelPath = filepath.Join(cfg.Root, "vmlinux")
	cfg.UsersPath = filepath.Join(cfg.Root, "users.conf")
	cfg.DryRun = dry
	e := &Env{
		Ctx: context.Background(), Cfg: cfg,
		Run:        &fakeRunner{},
		Fetch:      mapFetcher{},
		Log:        &bytes.Buffer{},
		SystemdDir: filepath.Join(root, "systemd"),
		SysctlDir:  filepath.Join(root, "sysctl"),
		SbinDir:    filepath.Join(root, "sbin"),
		FstabPath:  filepath.Join(root, "fstab"),
		SwapPath:   filepath.Join(root, "swapfile"),
		SSHDConfD:  filepath.Join(root, "sshd_config.d"),
		HomeDir:    filepath.Join(root, "home"),
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
	for _, name := range []string{"swapfile", "data-volume", "fetch-artifacts", "users.conf", "systemd-units", "enable-services"} {
		if !strings.Contains(out, name) {
			t.Errorf("plan missing step %q\n%s", name, out)
		}
	}
	if !strings.Contains(out, "dry run — nothing was changed") {
		t.Error("dry-run should announce it changed nothing")
	}
	// Nothing under any system dir should have been created.
	for _, d := range []string{"systemd", "sysctl", "sbin"} {
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
	fr := &fakeRunner{results: map[string]struct {
		out []byte
		err error
	}{
		"systemctl is-active sparkbox.service": {out: []byte("active\n")},
	}}
	e.Run = fr
	if sat, _, _ := stepEnableServices().Satisfied(e); !sat {
		t.Error("enable-services should be satisfied when the unit is active")
	}
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
