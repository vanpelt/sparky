package envsync

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// A guest with no repo payload is the state every sandbox created before this
// feature is in, and it is the exact confusion the feature shipped with: the
// box accepts a tag, reports the tag, and then checks nothing out. Reaching one
// must produce a named error a surface can print, never a silent success.
func TestSyncReposNamesASandboxTooOldToCheckOut(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "old-box", "alice")

	// The mock's guest is an unprivileged /bin/sh in the sandbox workdir, so
	// /usr/local/sbin/sparkbox-repos is genuinely absent — which is precisely
	// the condition an old rootfs is in.
	err := te.syncer.SyncRepos(context.Background(), box)
	if !errors.Is(err, host.ErrNoRepoSupport) {
		t.Fatalf("SyncRepos on a payload-less guest = %v, want ErrNoRepoSupport", err)
	}
	// The sandbox is still named, because the surface printing this has to say
	// WHICH box is too old when a fan-out reaches several.
	if got := err.Error(); !strings.Contains(got, "old-box") {
		t.Errorf("error %q does not name the sandbox", got)
	}
}

// Never a wake-up source, exactly like the env push: a paused box checks out at
// its next boot from whatever is attached then, and resuming somebody's machine
// is a bigger act than the one they asked for.
func TestSyncReposNeverWakesASandbox(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "napping", "alice")
	if err := te.mgr.Pause(context.Background(), "napping"); err != nil {
		t.Fatal(err)
	}
	paused, _ := te.mgr.Get("napping")
	if paused.State == vmm.StateRunning {
		t.Fatal("pause did not stop the box")
	}
	if err := te.syncer.SyncRepos(context.Background(), paused); err != nil {
		t.Fatalf("SyncRepos on a paused box = %v, want a silent no-op", err)
	}
	if now, _ := te.mgr.Get("napping"); now.State == vmm.StateRunning {
		t.Fatal("SyncRepos resumed a paused sandbox")
	}
	// A nil box is the same no-op rather than a panic: the fan-out reads the
	// ledger and a row can go away between the read and the nudge.
	if err := te.syncer.SyncRepos(context.Background(), (*host.Sandbox)(nil)); err != nil {
		t.Fatalf("SyncRepos(nil) = %v", err)
	}
	_ = box
}

// A box mid archive/snapshot is quiesced, and a checkout landing between the
// pre-pack strip and the pack would be packed in half-finished.
func TestSyncReposSkipsAQuiescedSandbox(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "packing", "alice")
	st := te.syncer.boxState("packing")
	st.mu.Lock()
	st.quiesced = true
	st.mu.Unlock()

	if err := te.syncer.SyncRepos(context.Background(), box); err != nil {
		t.Fatalf("SyncRepos on a quiesced box = %v, want a silent no-op", err)
	}
}
