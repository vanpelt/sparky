package envsync

// The pre-capture agent-tool refresh, from the gateway's side of the channel.
//
// What is pinned here is the contract host.Manager.Snapshot depends on and
// cannot check for itself: that the call has FINISHED installing when it
// returns, that a guest which cannot update says so in a sentence instead of
// reporting success, and that neither the paused-box rule nor the quiesce flag
// is quietly reinterpreted by a later reader.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// installUpdater writes a stand-in for the guest's installer into the mock VM's
// workdir and points the script at it.
//
// The mock's "guest" is an unprivileged /bin/sh whose cwd is the sandbox
// workdir, so there is no /usr/local/sbin to install into and no sudo to do it
// with — the same reason the env tests relocate /etc/environment. A relative
// path resolves against that cwd, which is the only thing the real absolute
// path and this one need to have in common: what is under test is what this
// package does with the guest's exit status, not where the installer lives.
func installUpdater(t *testing.T, te *testEnv, box, body string) {
	t.Helper()
	const rel = "sparkbox-update-tools"
	path := filepath.Join(te.dir, "mock-vms", box, rel)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := toolUpdaterPath
	toolUpdaterPath = "./" + rel
	t.Cleanup(func() { toolUpdaterPath = prev })
}

// The property the whole placement of this call rests on: the install is over
// before RefreshTools returns.
//
// Its caller pauses the guest on the very next line. If this returned when the
// guest had merely accepted the job — which is exactly what the repo nudge next
// door does, on purpose — the pause would land mid-install and the template
// would be captured with a half-written executable in /usr/local/bin, which
// every fork of that template then copies byte-for-byte.
func TestRefreshToolsIsSynchronous(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "slowpoke", "alice")
	installUpdater(t, te, "slowpoke", "#!/bin/sh\nsleep 1\n: > installed\n")

	start := time.Now()
	if err := te.syncer.RefreshTools(context.Background(), box); err != nil {
		t.Fatalf("RefreshTools: %v", err)
	}
	elapsed := time.Since(start)
	if _, err := os.Stat(filepath.Join(te.dir, "mock-vms", "slowpoke", "installed")); err != nil {
		t.Fatalf("RefreshTools returned before the guest finished installing: %v", err)
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("RefreshTools returned in %s; the guest's install takes a second, so it cannot have waited", elapsed)
	}
}

// A guest that cannot update its tools must be NAMED, never reported as
// refreshed: a silent success here means a template captured with the tool
// versions of the day the sandbox was born, and nothing downstream can tell.
//
// Three exits, one sentence, because the same fact arrives three ways. 3 is the
// script's own guard — no installer in this rootfs. 2 is a template that has
// the in-guest `sparkbox` dispatcher but not the `update-tools` verb: that
// dispatcher is a POSIX-sh case statement whose unknown-verb branch exits 2, so
// EVERY sandbox created before the verb shipped answers 2, not 3. 127 is a
// shell that could not find the command at all.
func TestRefreshToolsNamesAGuestTooOldToUpdate(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string // empty installs nothing at all
	}{
		{name: "no installer in the rootfs"},
		{name: "a dispatcher with no update-tools verb", body: "#!/bin/sh\nexit 2\n"},
		{name: "no such command", body: "#!/bin/sh\nexit 127\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			box := te.create(t, "old-box", "alice")
			if tc.body == "" {
				// Nothing to install: the mock guest genuinely has no
				// installer, which is the condition an old rootfs is in.
				prev := toolUpdaterPath
				toolUpdaterPath = "./sparkbox-update-tools"
				t.Cleanup(func() { toolUpdaterPath = prev })
			} else {
				installUpdater(t, te, "old-box", tc.body)
			}

			err := te.syncer.RefreshTools(context.Background(), box)
			if !errors.Is(err, host.ErrNoToolRefresh) {
				t.Fatalf("RefreshTools = %v, want ErrNoToolRefresh", err)
			}
			// A capture names the sandbox in its WARN, and a fan-out would
			// reach several: the sentence has to say which one.
			if got := err.Error(); !strings.Contains(got, "old-box") {
				t.Errorf("error %q does not name the sandbox", got)
			}
		})
	}
}

// This is not the code that may wake a box. The only safe wake before a capture
// is the pre-pack strip's, which goes through resumeOrRecreate precisely to
// avoid EnsureRunning's asynchronous env push landing after the strip and
// writing the secrets back into the disk about to be packed.
//
// An error rather than SyncRepos' silent no-op, because both callers have
// already checked the state: reaching here with a paused box means a check went
// missing, and the capture's WARN is where that should show up.
func TestRefreshToolsNeverWakesAPausedSandbox(t *testing.T) {
	te := newTestEnv(t)
	te.create(t, "napping", "alice")
	installUpdater(t, te, "napping", "#!/bin/sh\n: > installed\n")
	if err := te.mgr.Pause(context.Background(), "napping"); err != nil {
		t.Fatal(err)
	}
	paused, _ := te.mgr.Get("napping")
	if paused.State == vmm.StateRunning {
		t.Fatal("pause did not stop the box")
	}

	if err := te.syncer.RefreshTools(context.Background(), paused); err == nil {
		t.Fatal("RefreshTools on a paused box returned nil; an unreachable guest must be reported, not assumed refreshed")
	}
	if now, _ := te.mgr.Get("napping"); now.State == vmm.StateRunning {
		t.Fatal("RefreshTools resumed a paused sandbox")
	}
	if _, err := os.Stat(filepath.Join(te.dir, "mock-vms", "napping", "installed")); err == nil {
		t.Fatal("RefreshTools ran the installer in a paused sandbox")
	}
	// A nil box is an error and not a panic: the callers hold records read a
	// moment earlier and a sandbox can go away in between.
	if err := te.syncer.RefreshTools(context.Background(), nil); err == nil {
		t.Fatal("RefreshTools(nil) returned nil")
	}
}

// The quiesce flag stops a change-time SECRET push from rewriting values into
// a rootfs between the pre-pack strip and the pack. This refresh is a STEP of
// that same pack sequence, running after the strip and before the pause, and it
// writes /usr/local/bin rather than /etc/environment — so honouring the flag
// would mean the pack skipping its own step and silently capturing stale tools.
//
// The test exists so nobody "fixes" the asymmetry by copying SyncRepos' guard
// over here.
func TestRefreshToolsIgnoresTheQuiesceFlag(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "packing", "alice")
	installUpdater(t, te, "packing", "#!/bin/sh\n: > installed\n")

	// StripEnv is what sets the flag, and running it is the honest way to get
	// there: this asserts the real sequence, not a hand-set bit.
	if err := te.syncer.StripEnv(context.Background(), box); err != nil {
		t.Fatalf("StripEnv: %v", err)
	}
	st := te.syncer.boxState("packing")
	st.mu.Lock()
	quiesced := st.quiesced
	st.mu.Unlock()
	if !quiesced {
		t.Fatal("StripEnv did not quiesce the box; this test is no longer testing anything")
	}

	if err := te.syncer.RefreshTools(context.Background(), box); err != nil {
		t.Fatalf("RefreshTools on a quiesced box: %v", err)
	}
	if _, err := os.Stat(filepath.Join(te.dir, "mock-vms", "packing", "installed")); err != nil {
		t.Fatalf("the refresh skipped a quiesced box, so the capture would freeze stale tools into the template: %v", err)
	}
}

// The script ends by FLUSHING, and it does not exec.
//
// This is asserted against the script text rather than through the mock guest
// because the property is unobservable from outside: a missing sync costs
// nothing on any host that keeps running, and shows up only as a template whose
// /usr/local/bin is a version behind the sandbox it was captured from.
//
// The mechanism, which is why the two clauses below are one test: the pause on
// the very next line of host.Manager.Snapshot freezes dirty page cache into the
// MEMORY snapshot, while Driver.Snapshot reflinks the BLOCK DEVICE. An install
// that has been renamed into place but whose data blocks have not landed is
// therefore present when the sandbox resumes and absent from every fork of the
// template. `exec` would replace the shell and there would be nothing left to
// run the sync, so the two are the same requirement stated twice.
func TestRefreshToolsFlushesTheGuestBeforeTheCapture(t *testing.T) {
	script := toolRefreshScript("/usr/local/sbin/sparkbox-update-tools")

	lines := strings.Fields(strings.TrimSpace(script))
	if len(lines) == 0 || lines[len(lines)-1] != "sync" {
		t.Errorf("the refresh script does not end in a flush:\n%s\n"+
			"without it the capture reflinks a block device the install has not reached yet", script)
	}
	if strings.Contains(script, "exec ") {
		t.Errorf("the refresh script execs the updater:\n%s\n"+
			"exec replaces the shell, so nothing is left to run the sync that follows", script)
	}
}
