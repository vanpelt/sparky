package envsync

// The environment-build nudge, from the gateway's side of the channel.
//
// What is pinned here is what ctlops.BuildEnvironment depends on and cannot
// check for itself: that the call returns when the guest has ACCEPTED the job
// rather than when the setup script has finished, that a guest too old to have
// the payload is NAMED instead of silently reported as building, that the two
// skip rules (a box that is not running, a box mid-pack) never dial at all, and
// that a cancelled context ends the exec instead of hanging on it.
//
// The guest here is the mock driver's unprivileged /bin/sh with the sandbox
// workdir as its cwd, so the stand-ins below live in that workdir and the
// syncer's shell is given a PATH that starts there. That is the only thing the
// real /usr/local/sbin and this have to share: what is under test is what this
// package does with the guest's exit status and how long it waits, not where a
// payload lives.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// guestBin writes an executable stand-in into box's mock VM workdir and makes
// it findable by name: the syncer's shell is rewritten to put the workdir first
// on PATH, so `command -v systemctl` inside the nudge script resolves to what a
// test put there rather than to whatever the machine running `go test` happens
// to have installed. Without that the systemd branch would be exercised on a
// Linux workstation and skipped on a Mac.
func guestBin(t *testing.T, te *testEnv, box, name, body string) {
	t.Helper()
	path := filepath.Join(te.dir, "mock-vms", box, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	te.syncer.shell = `PATH="$PWD:$PATH" sh`
}

// installEnvSetup puts the guest's setup payload in place and points the script
// at it by a workdir-relative path, the way installUpdater does for the tool
// refresh next door.
func installEnvSetup(t *testing.T, te *testEnv, box, body string) {
	t.Helper()
	guestBin(t, te, box, "sparkbox-env-setup", body)
	prev := envSetupPath
	envSetupPath = "./sparkbox-env-setup"
	t.Cleanup(func() { envSetupPath = prev })
}

// noEnvSetupPayload points the script at a path the mock guest genuinely does
// not have, which is the condition a rootfs predating environment builds is in.
func noEnvSetupPayload(t *testing.T) {
	t.Helper()
	prev := envSetupPath
	envSetupPath = "./sparkbox-env-setup"
	t.Cleanup(func() { envSetupPath = prev })
}

// countingDialer routes the syncer's dials through a counter, so a test can
// assert that a skip was decided BEFORE the network — not that it merely did no
// damage after connecting.
func countingDialer(te *testEnv) func() int {
	var mu sync.Mutex
	n := 0
	te.syncer.SetDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		n++
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

func guestFile(te *testEnv, box, name string) string {
	return filepath.Join(te.dir, "mock-vms", box, name)
}

// waitFor polls for a file the detached half of the nudge writes. The whole
// point of this call is that it returns before that work is done, so the test
// cannot assert on it synchronously.
func waitFor(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared: the guest never started the setup run", filepath.Base(path))
}

// The property the whole orchestration rests on: the call returns when the
// guest has TAKEN the job, not when the setup script has finished.
//
// A setup script installs a toolchain and builds a dependency tree — minutes,
// and the reason the build has a 15-minute budget at all — while the caller is
// a person who typed `ctl env build` and is waiting at a prompt. The guest
// oneshot owns the long half, with the bounded TimeoutStartSec and journal it
// already has, and the outcome comes back over the metadata service instead.
func TestStartSetupReturnsAsSoonAsTheGuestAcceptsTheJob(t *testing.T) {
	t.Run("systemd restarts the oneshot", func(t *testing.T) {
		te := newTestEnv(t)
		box := te.create(t, "builder", "alice")
		// The payload must exist for the guard, but must NOT be what runs: with
		// systemd present the unit is the thing that runs it.
		installEnvSetup(t, te, "builder", "#!/bin/sh\n: > ran-in-the-foreground\n")
		guestBin(t, te, "builder", "systemctl", "#!/bin/sh\nprintf '%s ' \"$@\" > systemctl-args\n")

		if err := te.syncer.StartSetup(context.Background(), box); err != nil {
			t.Fatalf("StartSetup: %v", err)
		}
		args, err := os.ReadFile(guestFile(te, "builder", "systemctl-args"))
		if err != nil {
			t.Fatalf("the nudge never reached systemd: %v", err)
		}
		// --no-block is load-bearing, not decoration: without it systemctl
		// waits for the oneshot, and this call inherits the whole build.
		//
		// `restart` and not `start` for a second reason: the unit is
		// Type=oneshot RemainAfterExit=yes, so after one pass it is `active
		// (exited)` and a start is a no-op — a rebuild on a re-used builder
		// would silently do nothing.
		if got := string(args); !strings.Contains(got, "restart --no-block "+envSetupUnit) {
			t.Errorf("systemctl saw %q, want a non-blocking restart of %s", got, envSetupUnit)
		}
		if _, err := os.Stat(guestFile(te, "builder", "ran-in-the-foreground")); err == nil {
			t.Error("the nudge ran the setup payload itself; the unit owns the long half")
		}
	})

	t.Run("no systemd runs it detached", func(t *testing.T) {
		te := newTestEnv(t)
		box := te.create(t, "builder", "alice")
		// The setup payload blocks until this test releases it, so "did the
		// call wait for the script?" is settled by what the call observably
		// did and not by how long it took. A wall-clock budget cannot settle
		// it: the fixed cost of this nudge is a dial, a handshake and an exec,
		// and on a machine also running the rest of `go test ./...` that cost
		// is seconds — so any constant tight enough to catch an implementation
		// that waits is also tight enough to fail one that does not.
		installEnvSetup(t, te, "builder",
			"#!/bin/sh\n: > started\nwhile [ ! -f release ]; do sleep 0.05; done\n: > finished\n")
		// A guest with no systemd at all. Shadowed rather than assumed absent
		// so this branch is the one under test on every platform.
		guestBin(t, te, "builder", "systemctl", "#!/bin/sh\nexit 1\n")
		guestBin(t, te, "builder", "setsid", "#!/bin/sh\nexec \"$@\"\n")
		// Always released, so a failing assertion below leaves no guest loop
		// spinning behind it.
		release := guestFile(te, "builder", "release")
		t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o644) })

		done := make(chan error, 1)
		go func() { done <- te.syncer.StartSetup(context.Background(), box) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("StartSetup: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("StartSetup had not returned while the guest's setup was still running: it waited for the script")
		}
		if _, err := os.Stat(guestFile(te, "builder", "finished")); err == nil {
			t.Fatal("the setup script had already run to completion when StartSetup returned; the caller inherited the build")
		}
		// It returned early AND the work really started: a nudge that returns
		// fast by doing nothing is the failure this pair rules out.
		waitFor(t, guestFile(te, "builder", "started"), 10*time.Second)
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		waitFor(t, guestFile(te, "builder", "finished"), 10*time.Second)
	})
}

// A builder booted from a template that predates environment builds must be
// NAMED. The alternative is the worst failure this feature can have: the nudge
// succeeds, nothing ever runs, and the row sits in `building` until the timeout
// reports "the builder never reported" — a sentence that names the wrong cause
// and sends the reader looking at the metadata service.
func TestStartSetupNamesAGuestTooOldToRunOne(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "old-box", "alice")
	noEnvSetupPayload(t)

	err := te.syncer.StartSetup(context.Background(), box)
	if !errors.Is(err, host.ErrNoEnvSetup) {
		t.Fatalf("StartSetup on a payload-less guest = %v, want ErrNoEnvSetup", err)
	}
	// The sandbox is still named: the surface printing this has to say WHICH
	// box, because the builder's name is also the repair instruction.
	if got := err.Error(); !strings.Contains(got, "old-box") {
		t.Errorf("error %q does not name the sandbox", got)
	}
}

// Never a wake-up source, exactly like the env push and the repo nudge. For a
// builder the case that gets here is a reconciler sweeping a build whose box
// the reaper paused: resuming it from here would put an unattended script back
// on somebody's credentials with nobody watching, and the decision to do that
// belongs to the caller that knows how old the build is.
//
// Asserted through the dialer, so this is "decided before the network" rather
// than "did no damage after connecting".
func TestStartSetupNeverWakesASandbox(t *testing.T) {
	te := newTestEnv(t)
	te.create(t, "napping", "alice")
	installEnvSetup(t, te, "napping", "#!/bin/sh\n: > started\n")
	if err := te.mgr.Pause(context.Background(), "napping"); err != nil {
		t.Fatal(err)
	}
	paused, _ := te.mgr.Get("napping")
	if paused.State == vmm.StateRunning {
		t.Fatal("pause did not stop the box")
	}
	dials := countingDialer(te)

	if err := te.syncer.StartSetup(context.Background(), paused); err != nil {
		t.Fatalf("StartSetup on a paused box = %v, want a silent no-op", err)
	}
	if now, _ := te.mgr.Get("napping"); now.State == vmm.StateRunning {
		t.Fatal("StartSetup resumed a paused sandbox")
	}
	if _, err := os.Stat(guestFile(te, "napping", "started")); err == nil {
		t.Fatal("StartSetup ran the setup payload in a paused sandbox")
	}
	// A nil box is the same no-op rather than a panic: a caller holds a record
	// read a moment earlier and a row can go away in between.
	if err := te.syncer.StartSetup(context.Background(), (*host.Sandbox)(nil)); err != nil {
		t.Fatalf("StartSetup(nil) = %v", err)
	}
	if n := dials(); n != 0 {
		t.Fatalf("the skip cost %d dial(s); an unreachable box must be decided before the network", n)
	}
}

// A box mid archive/snapshot is quiesced, and a setup run landing between the
// pre-pack strip and the pack would be packed into the image half-finished —
// which is then what every fork of that template copies.
//
// StripEnv is what sets the flag and running it is the honest way to get there:
// this asserts the real sequence, not a hand-set bit.
func TestStartSetupSkipsAQuiescedSandbox(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "packing", "alice")
	installEnvSetup(t, te, "packing", "#!/bin/sh\n: > started\n")
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
	dials := countingDialer(te)

	if err := te.syncer.StartSetup(context.Background(), box); err != nil {
		t.Fatalf("StartSetup on a quiesced box = %v, want a silent no-op", err)
	}
	if _, err := os.Stat(guestFile(te, "packing", "started")); err == nil {
		t.Fatal("StartSetup started a setup run inside a box being packed")
	}
	if n := dials(); n != 0 {
		t.Fatalf("the skip cost %d dial(s); a quiesced box must be decided before the network", n)
	}
}

// A cancelled context must END the exec, not merely stop caring about it.
//
// The nudge is meant to be instant, so the only way it hangs is a guest that
// never answers — a wedged systemd, a guest whose command never exits. Nothing
// in x/crypto/ssh watches a context, so without the AfterFunc that closes the
// client, Run blocks forever and takes the caller's goroutine with it. This is
// the gap sshgw.RunInSandbox has and this package deliberately does not.
func TestStartSetupStopsWaitingWhenTheContextIsCancelled(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "wedged", "alice")
	installEnvSetup(t, te, "wedged", "#!/bin/sh\n: > started\n")
	// A systemctl that never returns is the wedged guest, and it is the
	// foreground half of the script, so the exec cannot finish on its own.
	guestBin(t, te, "wedged", "systemctl", "#!/bin/sh\nsleep 30\n")

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- te.syncer.StartSetup(ctx, box) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("StartSetup on a cancelled context returned nil; a nudge nobody waited for must not report success")
		}
		if !strings.Contains(err.Error(), "wedged") {
			t.Errorf("error %q does not name the sandbox", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("StartSetup unblocked after %s; the cancellation should tear the transport down at once", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("StartSetup never returned after its context was cancelled: the exec is not bound to the context")
	}
}
