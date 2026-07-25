package hostsetup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// legacyHost lays down the shape the DGX actually has: state, images and the
// kernel flat under <root>, a populated sqlite DB, and no <root>/data at all.
func legacyHost(t *testing.T, e *Env) (stateDir, imageDir string) {
	t.Helper()
	stateDir = filepath.Join(e.Cfg.Root, "state")
	imageDir = filepath.Join(e.Cfg.Root, "images")
	for _, d := range []string{stateDir, imageDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		filepath.Join(stateDir, "sparkbox.db"),
		filepath.Join(stateDir, "gateway_host_key.pem"),
		filepath.Join(imageDir, "universal.ext4"),
	} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "certmagic"), 0o755); err != nil {
		t.Fatal(err)
	}
	return stateDir, imageDir
}

func TestExecStartFlag(t *testing.T) {
	// The real unit spreads ExecStart over eight backslash-continued lines, so a
	// per-line scan finds the flag and the value on different lines.
	unit, err := renderService(DefaultConfigAt("/srv/sparkbox"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		unit string
		flag string
		want string
	}{
		{"rendered unit state-dir", unit, "--state-dir", "/srv/sparkbox/data/state"},
		{"rendered unit image-dir", unit, "--image-dir", "/srv/sparkbox/data/images"},
		{"absent flag", unit, "--sluice-socket", ""},
		{
			"equals form",
			"[Service]\nExecStart=/usr/local/bin/sparkbox serve --state-dir=/srv/legacy/state\n",
			"--state-dir", "/srv/legacy/state",
		},
		{
			"dash prefix (systemd ignore-failure)",
			"[Service]\nExecStart=-/usr/local/bin/sparkbox serve --state-dir /a/b\n",
			"--state-dir", "/a/b",
		},
		{
			// A flag whose value is missing must not swallow the next flag.
			"flag with no value",
			"[Service]\nExecStart=/x serve --state-dir --image-dir /i\n",
			"--state-dir", "",
		},
		{"no ExecStart at all", "[Unit]\nDescription=nothing\n", "--state-dir", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := execStartFlag(execStartLine(tc.unit), tc.flag); got != tc.want {
				t.Errorf("execStartFlag(%s) = %q, want %q", tc.flag, got, tc.want)
			}
		})
	}
}

func TestDetectLayout(t *testing.T) {
	cases := []struct {
		name string
		// setup mutates the tempdir host; want* describe the verdict.
		setup        func(t *testing.T, e *Env) (wantState string)
		wantFound    bool
		wantOwn      bool
		wantSource   string
		wantImageDir bool
	}{
		{
			name:      "fresh host",
			setup:     func(*testing.T, *Env) string { return "" },
			wantFound: false,
		},
		{
			name: "already on this layout",
			setup: func(t *testing.T, e *Env) string {
				mustWrite(t, filepath.Join(e.Cfg.StateDir, "sparkbox.db"), "x")
				return ""
			},
			wantFound: false,
			wantOwn:   true,
		},
		{
			name: "populated flat layout (the DGX)",
			setup: func(t *testing.T, e *Env) string {
				s, _ := legacyHost(t, e)
				return s
			},
			wantFound:    true,
			wantSource:   "flat layout",
			wantImageDir: true,
		},
		{
			name: "empty flat directories are not a layout",
			setup: func(t *testing.T, e *Env) string {
				// A stray <root>/state with nothing in it must not block a
				// perfectly ordinary provision.
				if err := os.MkdirAll(filepath.Join(e.Cfg.Root, "state"), 0o755); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			wantFound: false,
		},
		{
			name: "the installed unit names a state dir of its own",
			setup: func(t *testing.T, e *Env) string {
				// An operator who once passed --state-dir has a host whose real
				// layout matches no default. Only the unit knows.
				state := filepath.Join(t.TempDir(), "elsewhere", "state")
				mustWrite(t, filepath.Join(state, "sparkbox.db"), "x")
				cfg := e.Cfg
				cfg.StateDir = state
				unit, err := renderService(cfg)
				if err != nil {
					t.Fatal(err)
				}
				mustWrite(t, filepath.Join(e.SystemdDir, serviceUnit), unit)
				return state
			},
			wantFound:  true,
			wantSource: "installed sparkbox.service",
		},
		{
			name: "both roots populated",
			setup: func(t *testing.T, e *Env) string {
				s, _ := legacyHost(t, e)
				mustWrite(t, filepath.Join(e.Cfg.StateDir, "sparkbox.db"), "x")
				return s
			},
			wantFound: true,
			wantOwn:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := testEnv(t, false)
			wantState := tc.setup(t, e)
			found, own, err := detectLayout(e)
			if err != nil {
				t.Fatal(err)
			}
			if own != tc.wantOwn {
				t.Errorf("ownPopulated = %v, want %v", own, tc.wantOwn)
			}
			if (found != nil) != tc.wantFound {
				t.Fatalf("found = %v, want found=%v", found, tc.wantFound)
			}
			if found == nil {
				return
			}
			if found.stateDir != wantState {
				t.Errorf("stateDir = %q, want %q", found.stateDir, wantState)
			}
			if tc.wantSource != "" && !strings.Contains(found.source, tc.wantSource) {
				t.Errorf("source = %q, want it to mention %q", found.source, tc.wantSource)
			}
			if tc.wantImageDir && found.imageDir == "" {
				t.Error("a flat layout with an images/ dir should offer it for adoption")
			}
			if len(found.markers) == 0 {
				t.Error("a detected layout must say what made it look live")
			}
		})
	}
}

// TestProvisionRefusesSecondDataRoot is the invariant: never provision a second
// data root beside a live one. This is the DGX plan from F4 — 300G volume, fresh
// artifact download, units repointed — and it must not merely warn.
func TestProvisionRefusesSecondDataRoot(t *testing.T) {
	for _, dry := range []bool{false, true} {
		name := "real run"
		if dry {
			// The evidence in F4 is a --dry-run: a plan that describes a host
			// that does not exist is the failure, so the dry run has to refuse
			// too rather than print the wrong plan.
			name = "dry run"
		}
		t.Run(name, func(t *testing.T) {
			e, _ := testEnv(t, dry)
			buf := &strings.Builder{}
			e.Log = buf
			stateDir, imageDir := legacyHost(t, e)

			err := Provision(e)
			if err == nil {
				t.Fatal("Provision must refuse a host whose live state is somewhere else")
			}
			msg := err.Error()
			for _, want := range []string{stateDir, "sparkbox.db", "--adopt-legacy", "mv " + stateDir, "cp -a", e.Cfg.dataDir()} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal should mention %q:\n%s", want, msg)
				}
			}
			if !strings.Contains(msg, imageDir) {
				t.Errorf("refusal should mention the legacy images dir %q:\n%s", imageDir, msg)
			}
			// Nothing may have been built beside the live root.
			if _, statErr := os.Stat(e.Cfg.dataDir()); statErr == nil {
				t.Errorf("%s was created despite the refusal", e.Cfg.dataDir())
			}
			if _, statErr := os.Stat(filepath.Join(e.Cfg.Root, "data.img")); statErr == nil {
				t.Error("a second data volume image was created despite the refusal")
			}
			if fr, ok := e.Run.(*fakeRunner); ok {
				for _, call := range fr.calls {
					if strings.HasPrefix(call, "mkfs.xfs") || strings.HasPrefix(call, "mount ") {
						t.Errorf("the refusal must come before any volume work; ran %q", call)
					}
				}
			}
		})
	}
}

// TestProvisionRefusesTwoPopulatedRoots covers the half-migrated host: adoption
// cannot pick a winner, so --adopt-legacy must not paper over it either.
func TestProvisionRefusesTwoPopulatedRoots(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Cfg.AdoptLegacy = true
	stateDir, _ := legacyHost(t, e)
	mustWrite(t, filepath.Join(e.Cfg.StateDir, "sparkbox.db"), "x")

	err := Provision(e)
	if err == nil {
		t.Fatal("two populated state dirs must be refused even with --adopt-legacy")
	}
	for _, want := range []string{stateDir, e.Cfg.StateDir, "systemctl stop sparkbox"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q:\n%v", want, err)
		}
	}
}

// TestProvisionRefusesAHalfNamedLayout is the same invariant reached by a
// likelier route than forgetting the flag entirely: --state-dir and --image-dir
// are separate flags, every message here names them as a pair, and typing only
// the first is an ordinary slip. detectLayout compared state directories alone,
// so a run that already named the live state dir looked settled while ImageDir
// still pointed into <root>/data — and stepDataVolume duly planned a 300G volume
// and a full artifact re-download, leaving the live <root>/images orphaned.
func TestProvisionRefusesAHalfNamedLayout(t *testing.T) {
	e, _ := testEnv(t, true)
	buf := &strings.Builder{}
	e.Log = buf
	stateDir, imageDir := legacyHost(t, e)
	// The half-typed pair: the state dir is named, the images are not.
	e.Cfg.StateDir = stateDir

	found, own, err := detectLayout(e)
	if err != nil {
		t.Fatal(err)
	}
	if found == nil || !found.imagesOnly {
		t.Fatalf("a populated images dir outside --image-dir must be detected: found=%+v", found)
	}
	if found.imageDir != imageDir || !own {
		t.Errorf("imageDir = %q (want %q), ownPopulated = %v", found.imageDir, imageDir, own)
	}

	perr := Provision(e)
	if perr == nil {
		t.Fatal("setup must refuse to put the images on a new volume while the live ones sit elsewhere")
	}
	for _, want := range []string{imageDir, "--image-dir", "--adopt-legacy", e.Cfg.dataDir()} {
		if !strings.Contains(perr.Error(), want) {
			t.Errorf("refusal should mention %q:\n%v", want, perr)
		}
	}
	if strings.Contains(buf.String(), "data-volume") {
		t.Errorf("the refusal must come before the plan describes a volume:\n%s", buf.String())
	}

	// Naming the pair is the fix, and so is --adopt-legacy.
	e2, _ := testEnv(t, false)
	legacyHost(t, e2)
	e2.Cfg.StateDir = filepath.Join(e2.Cfg.Root, "state")
	e2.Cfg.ImageDir = filepath.Join(e2.Cfg.Root, "images")
	settled, _, err := detectLayout(e2)
	if err != nil {
		t.Fatal(err)
	}
	if settled != nil {
		t.Errorf("a host whose flags name both live directories is settled, not legacy: %+v", settled)
	}

	e3, _ := testEnv(t, false)
	_, images3 := legacyHost(t, e3)
	e3.Cfg.StateDir = filepath.Join(e3.Cfg.Root, "state")
	e3.Cfg.AdoptLegacy = true
	if err := reconcileLayout(e3); err != nil {
		t.Fatalf("--adopt-legacy should adopt the images too: %v", err)
	}
	if e3.Cfg.ImageDir != images3 {
		t.Errorf("ImageDir = %q, want the live %q", e3.Cfg.ImageDir, images3)
	}
}

// TestAdoptLegacyLayout is the other half: with the flag, setup uses what is
// there and builds no second root.
func TestAdoptLegacyLayout(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Cfg.AdoptLegacy = true
	stateDir, imageDir := legacyHost(t, e)

	if err := reconcileLayout(e); err != nil {
		t.Fatalf("--adopt-legacy should adopt, not refuse: %v", err)
	}
	if e.Cfg.StateDir != stateDir {
		t.Errorf("StateDir = %q, want the live %q", e.Cfg.StateDir, stateDir)
	}
	if e.Cfg.ImageDir != imageDir {
		t.Errorf("ImageDir = %q, want the live %q", e.Cfg.ImageDir, imageDir)
	}
	if !e.AdoptedLegacy {
		t.Error("adoption must be recorded so the connect banner can say which layout is live")
	}

	// The data volume is now beside the point: nothing this host uses lives
	// under it, so building one would be hundreds of gigabytes of nothing.
	sat, note, err := stepDataVolume().Satisfied(e)
	if err != nil || !sat {
		t.Fatalf("data-volume should be inapplicable after adoption: sat=%v err=%v", sat, err)
	}
	if !strings.Contains(note, "outside") {
		t.Errorf("note = %q, want it to say the volume is not applicable", note)
	}
	if fr, ok := e.Run.(*fakeRunner); ok && slices.ContainsFunc(fr.calls, func(c string) bool {
		return strings.HasPrefix(c, "mountpoint")
	}) {
		t.Error("an inapplicable data volume must not even be probed")
	}

	// And the unit follows the adopted paths — the whole point, since it is the
	// unit's ExecStart that decides what the running gateway opens.
	if err := stepSystemdUnits().Apply(e); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(e.SystemdDir, serviceUnit))
	if err != nil {
		t.Fatal(err)
	}
	if got := execStartFlag(execStartLine(string(unit)), "--state-dir"); got != stateDir {
		t.Errorf("unit --state-dir = %q, want the adopted %q", got, stateDir)
	}
	if got := execStartFlag(execStartLine(string(unit)), "--image-dir"); got != imageDir {
		t.Errorf("unit --image-dir = %q, want the adopted %q", got, imageDir)
	}
	// Re-detection on the next run must be a no-op, or every subsequent setup
	// would refuse a host it just adopted.
	found, own, err := detectLayout(e)
	if err != nil || found != nil {
		t.Errorf("an adopted host must look settled on re-detection: found=%v err=%v", found, err)
	}
	if !own {
		t.Error("the adopted state dir should now read as this config's own")
	}
}

// TestDataVolumeRefusesToMountOverPayload is the second way a live root gets
// buried: not a different path, but a filesystem mounted on top of the one that
// already holds the state.
func TestDataVolumeRefusesToMountOverPayload(t *testing.T) {
	e, _ := testEnv(t, false)
	// A host provisioned once WITHOUT the loop volume: state written straight
	// into <root>/data/state.
	mustWrite(t, filepath.Join(e.Cfg.StateDir, "sparkbox.db"), "x")
	mustWrite(t, filepath.Join(e.Cfg.StateDir, "certmagic", "acme", "cert.pem"), "x")

	err := stepDataVolume().Apply(e)
	if err == nil {
		t.Fatal("mounting over a populated data dir hides it; that must be refused")
	}
	for _, want := range []string{e.Cfg.dataDir(), "state/", "--adopt-legacy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q:\n%v", want, err)
		}
	}
	if fr, ok := e.Run.(*fakeRunner); ok {
		for _, call := range fr.calls {
			if strings.HasPrefix(call, "mkfs.xfs") || strings.HasPrefix(call, "mount ") {
				t.Errorf("nothing may be formatted or mounted; ran %q", call)
			}
		}
	}

	// An empty leftover directory tree is not payload — setup has to stay
	// re-runnable after its own interrupted run.
	e2, _ := testEnv(t, false)
	if err := os.MkdirAll(e2.Cfg.ImageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e2.Run = runnerWith(map[string]string{
		"truncate -s 300G " + filepath.Join(e2.Cfg.Root, "data.img"):                       "",
		"mkfs.xfs -q -m reflink=1 " + filepath.Join(e2.Cfg.Root, "data.img"):               "",
		"mount -o loop " + filepath.Join(e2.Cfg.Root, "data.img") + " " + e2.Cfg.dataDir(): "",
	})
	if err := stepDataVolume().Apply(e2); err != nil {
		t.Fatalf("an empty leftover directory must not block the volume: %v", err)
	}
}

func TestUnderDir(t *testing.T) {
	cases := []struct {
		parent, child string
		want          bool
	}{
		{"/srv/sparkbox/data", "/srv/sparkbox/data/state", true},
		{"/srv/sparkbox/data", "/srv/sparkbox/data", true},
		{"/srv/sparkbox/data", "/srv/sparkbox/data/", true},
		{"/srv/sparkbox/data", "/srv/sparkbox/state", false},
		// The prefix trap: "/srv/sparkbox/database" is not inside "…/data".
		{"/srv/sparkbox/data", "/srv/sparkbox/database/state", false},
		{"/srv/sparkbox/data", "/mnt/big/state", false},
		{"", "/srv/sparkbox/data", false},
		{"/srv/sparkbox/data", "", false},
	}
	for _, tc := range cases {
		if got := underDir(tc.parent, tc.child); got != tc.want {
			t.Errorf("underDir(%q, %q) = %v, want %v", tc.parent, tc.child, got, tc.want)
		}
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
