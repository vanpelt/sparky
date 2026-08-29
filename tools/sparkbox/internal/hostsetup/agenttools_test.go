package hostsetup

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/deploy"
)

// agentToolsEnv is a testEnv with a rootfs template on disk (the thing this
// step patches) and a runner that answers everything the step shells out to.
// stamped decides what the template's /etc/sparkbox/tools-rev reads back as.
func agentToolsEnv(t *testing.T, stamped bool) (*Env, *fakeRunner) {
	t.Helper()
	e, _ := testEnv(t, false)
	if err := os.MkdirAll(e.Cfg.ImageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Cfg.rootfsPath(), []byte("ext4 bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Both fixtures carry debugfs's real stderr banner, which combined output
	// puts in front of anything on stdout — see stampLine.
	stamp := "debugfs 1.47.0 (5-Feb-2023)\n" + templateToolsStamp + ": File not found by ext2_lookup"
	if stamped {
		stamp = "debugfs 1.47.0 (5-Feb-2023)\nclaude=2.1.212 codex=rust-v0.145.0 pi=v0.82.1 hivemind=1.0.5 identity=2 agentenv=1"
	}
	r := runnerWith(map[string]string{
		"systemctl daemon-reload":                                         "",
		"systemctl enable --now " + refreshToolsTimer:                     "",
		filepath.Join(e.SbinDir, refreshToolsScript):                      ">> done: 1 template(s) now ship claude",
		"debugfs -R cat " + templateToolsStamp + " " + e.Cfg.rootfsPath(): stamp,
	})
	e.Run = r
	return e, r
}

// TestAgentToolsAsksTheTemplateNotAStampFile is the regression for the bug this
// step exists to end. On the DGX every asset was installed and current — the
// refresher, its timer, and a host-side versions.env naming today's agent
// releases — while the template those versions were supposed to be IN had been
// replaced by a release upgrade an hour later and held no claude at all. Every
// sandbox created afterwards was empty, and nothing anywhere said so.
//
// So a fully installed, fully current set of assets over an unstamped template
// must NOT report satisfied.
func TestAgentToolsAsksTheTemplateNotAStampFile(t *testing.T) {
	e, _ := agentToolsEnv(t, false)
	s := stepAgentTools()

	if err := s.Apply(e); err != nil {
		t.Fatal(err)
	}
	// Every asset is now byte-for-byte what this release ships...
	for _, a := range mustAssets(t, e) {
		got, err := os.ReadFile(a.path)
		if err != nil || !bytes.Equal(got, a.body) {
			t.Fatalf("%s was not installed", a.path)
		}
	}
	// ...and the step still refuses to call itself done, because the template
	// says it never received them.
	if sat, note, err := s.Satisfied(e); sat || err != nil {
		t.Fatalf("assets current over an unstamped template must not be satisfied: sat=%v note=%q err=%v", sat, note, err)
	}

	// Once the template carries the stamp, and only then, it is satisfied.
	e2, _ := agentToolsEnv(t, true)
	if err := s.Apply(e2); err != nil {
		t.Fatal(err)
	}
	sat, note, err := s.Satisfied(e2)
	if err != nil || !sat {
		t.Fatalf("a stamped template with current assets should be satisfied: sat=%v err=%v", sat, err)
	}
	if !strings.Contains(note, "template") {
		t.Errorf("note = %q, want it to say the claim rests on the template", note)
	}
}

// TestAgentToolsApplyInstallsAndBakes: the assets, the timer, and one immediate
// bake — a host that finishes setup should be able to create a sandbox somebody
// can work in, not one that waits up to a day for the timer.
func TestAgentToolsApplyInstallsAndBakes(t *testing.T) {
	e, r := agentToolsEnv(t, false)
	if err := stepAgentTools().Apply(e); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(e.SbinDir, refreshToolsScript)
	if got, _ := os.ReadFile(script); !bytes.Equal(got, deploy.RefreshToolsScript) {
		t.Error("the refresher was not installed from the embedded asset")
	}
	if fi, err := os.Stat(script); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the refresher must be executable: %v", err)
	}
	if got, _ := os.ReadFile(script); !bytes.Contains(got, []byte("PI_REPO=${PI_REPO:-earendil-works/pi}")) ||
		!bytes.Contains(got, []byte("ln -sfn ../lib/pi/pi")) {
		t.Error("the refresher must download Pi's official bundle and expose the pi CLI")
	}
	if got, _ := os.ReadFile(filepath.Join(e.SbinDir, guestIdentityName)); !bytes.Equal(got, deploy.GuestIdentityScript) {
		t.Error("the guest-identity installer was not installed — the refresher calls it by that path and exits 1 without it")
	}
	unit, err := os.ReadFile(filepath.Join(e.SystemdDir, refreshToolsUnit))
	if err != nil {
		t.Fatal(err)
	}
	// The layout has to be baked into the unit: the script's own defaults point
	// at /srv/sparkbox/data/images, and a refresher aimed at an empty directory
	// exits 1 having patched nothing.
	if !strings.Contains(string(unit), "IMAGES_DIR="+e.Cfg.ImageDir) {
		t.Errorf("unit does not carry this host's image dir:\n%s", unit)
	}
	if !strings.Contains(string(unit), "TOOLS_DIR="+e.Cfg.toolsDir()) {
		t.Errorf("unit does not carry this host's tools dir:\n%s", unit)
	}
	if !strings.Contains(string(unit), "ExecStart="+script) {
		t.Errorf("unit does not run the script setup installed:\n%s", unit)
	}

	if !slices.Contains(r.calls, "systemctl enable --now "+refreshToolsTimer) {
		t.Errorf("the daily timer was never enabled: %q", r.calls)
	}
	if !slices.Contains(r.calls, script) {
		t.Errorf("setup did not bake the tools in this run: %q", r.calls)
	}
	if i, j := slices.Index(r.calls, "systemctl daemon-reload"), slices.Index(r.calls, "systemctl enable --now "+refreshToolsTimer); i > j {
		t.Error("enabling the timer before daemon-reload enables the text systemd was already holding")
	}
}

// TestCloudInitAgentToolsPathsMatchSetup keeps both cloud-init entry points —
// the daily unit and its immediate provision-time bake — aligned with the paths
// a later `sparkbox setup` renders. A mismatch moves the cache and versions.env
// on the first setup/timer run and needlessly downloads every agent CLI again.
func TestCloudInitAgentToolsPathsMatchSetup(t *testing.T) {
	got, err := os.ReadFile(filepath.Join("..", "..", "deploy", "cloud-init.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	if want := "TOOLS_DIR=${TOOLS_DIR:-" + cfg.toolsDir() + "}"; !bytes.Contains(deploy.RefreshToolsScript, []byte(want)) {
		t.Errorf("refresher default is not aligned with setup: missing %q", want)
	}
	for _, want := range []string{
		"Environment=IMAGES_DIR=" + cfg.ImageDir,
		"Environment=TOOLS_DIR=" + cfg.toolsDir(),
		"IMAGES_DIR=" + cfg.ImageDir + " TOOLS_DIR=" + cfg.toolsDir() + " \\",
		// The serve unit has to name the same directory the refresher fills, or
		// a host provisioned purely by cloud-init answers 501 to every
		// `sparkbox update-tools` until some later `sparkbox setup` rewrites the
		// unit from the template. This file is the OTHER provisioner (the dual
		// cloud-init/`sparkbox setup` split), and it is the half that has no
		// Go-rendered template to keep it honest.
		"--tools-dir " + cfg.toolsDir() + " \\",
	} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("cloud-init agent-tools path is not aligned with setup: missing %q", want)
		}
	}
}

// TestAgentToolsBakeFailureIsNotFatal: the bake pulls several hundred MB from
// several third-party release channels, and it is the one thing in the pipeline
// allowed to fail without undoing a provisioning run whose gateway, network and
// units are all correct. It must say so, though — silence here is a host whose
// sandboxes are empty for a day with no explanation.
func TestAgentToolsBakeFailureIsNotFatal(t *testing.T) {
	e, _ := agentToolsEnv(t, false)
	buf := &bytes.Buffer{}
	e.Log = buf
	// Drop the script from the runner's map: an unlisted command errors, which
	// is how the real Runner behaves for a binary that fails.
	delete(e.Run.(*fakeRunner).results, filepath.Join(e.SbinDir, refreshToolsScript))

	if err := stepAgentTools().Apply(e); err != nil {
		t.Fatalf("a failed bake must not fail the provisioning run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Errorf("a failed bake must be reported:\n%s", out)
	}
	if !strings.Contains(out, refreshToolsTimer) {
		t.Errorf("the report should name what will retry:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(e.SbinDir, refreshToolsScript)) {
		t.Errorf("the report should name the command to retry by hand:\n%s", out)
	}
}

// TestAgentToolsOffIsNamed: --agent-tools=false is satisfied, and says what the
// host gives up. Nothing else about a green run would hint that the sandboxes
// this host creates have no agent in them.
func TestAgentToolsOffIsNamed(t *testing.T) {
	e, r := agentToolsEnv(t, false)
	e.Cfg.AgentTools = false

	sat, note, err := stepAgentTools().Satisfied(e)
	if err != nil || !sat {
		t.Fatalf("--agent-tools=false must be satisfied, not a failure: sat=%v err=%v", sat, err)
	}
	if !strings.Contains(note, "pi") {
		t.Errorf("note = %q, want it to name what the sandboxes will not have", note)
	}
	if len(r.calls) != 0 {
		t.Errorf("a disabled step must not touch the host: %q", r.calls)
	}
}

// TestAgentToolsOnByDefault pins the default, which is the whole answer to "was
// there a flag we forgot to set". There was not: nothing in the released binary
// installed the agent CLIs at all.
func TestAgentToolsOnByDefault(t *testing.T) {
	if !DefaultConfig().AgentTools {
		t.Error("agent tooling must be on by default — a sandbox with no agent in it is not a useful default")
	}
}

// TestCheckAgentToolsReportsAnEmptyTemplate: the diagnostic that was missing on
// the day it mattered. doctor was green over a template with no claude in it,
// because nothing on the box asked the template.
func TestCheckAgentToolsReportsAnEmptyTemplate(t *testing.T) {
	cfg := DefaultConfigAt(t.TempDir())
	cmd := "debugfs -R cat " + templateToolsStamp + " " + cfg.rootfsPath()
	onDisk := fakeProbe{files: map[string]string{cfg.rootfsPath(): "ext4 bytes"}}

	// A template that debugfs reads back with nothing in it: the file is not
	// there, which debugfs reports by printing nothing and exiting 0.
	empty := newScriptedProbe(map[string][]string{cmd: {""}})
	empty.fakeProbe = onDisk
	got := checkAgentTools(empty, cfg)
	if got.Status != Warn {
		t.Fatalf("an empty template must be reported, got %v (%s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "claude") {
		t.Errorf("detail = %q, want it to name what the sandboxes will be missing", got.Detail)
	}
	if !strings.Contains(got.Hint, refreshToolsScript) {
		t.Errorf("hint = %q, want the command that fixes it", got.Hint)
	}

	// A stamped one passes, and reports the versions it found rather than a bare
	// "ok" — the stamp is the evidence.
	//
	// The fixture is what debugfs REALLY prints, banner included. That banner
	// goes to stderr and both readers take combined output, so an implementation
	// that read "the first line" would find "debugfs 1.47.0" here and warn about
	// a template that is perfectly baked — which is how this was caught, on a
	// live box, after the check was written.
	stamp := "claude=2.1.212 codex=rust-v0.145.0 pi=v0.82.1 hivemind=1.0.5 identity=2 agentenv=1"
	ok := newScriptedProbe(map[string][]string{cmd: {"debugfs 1.47.0 (5-Feb-2023)\n" + stamp}})
	ok.fakeProbe = onDisk
	if got := checkAgentTools(ok, cfg); got.Status != Pass || !strings.Contains(got.Detail, "claude=") {
		t.Errorf("a stamped template should pass with its versions: %v %q", got.Status, got.Detail)
	}

	// A stamp from before Pi joined the baked tool set is intentionally stale.
	// Setup must retry the bake rather than treating the presence of Claude as
	// proof that every currently required CLI is present.
	legacyStamp := "claude=2.1.212 codex=rust-v0.145.0 hivemind=1.0.5 identity=2 agentenv=1"
	legacy := newScriptedProbe(map[string][]string{cmd: {"debugfs 1.47.0 (5-Feb-2023)\n" + legacyStamp}})
	legacy.fakeProbe = onDisk
	if got := checkAgentTools(legacy, cfg); got.Status != Warn || !strings.Contains(got.Detail, "pi") {
		t.Errorf("a pre-Pi stamp should warn and name the incomplete tool set: %v %q", got.Status, got.Detail)
	}
}

func mustAssets(t *testing.T, e *Env) []fileAsset {
	t.Helper()
	a, err := agentToolsAssets(e)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
