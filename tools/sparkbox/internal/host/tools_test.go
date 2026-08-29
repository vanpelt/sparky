package host_test

// The pre-capture agent-tool refresh: where it runs in the pack sequence, and
// what happens when it cannot.
//
// A template is the one packed disk other sandboxes boot FROM, so the versions
// frozen into it are the versions every fork of it starts with, possibly for
// months. That is the whole reason this step exists — and the reason it must
// never be able to fail a capture, because a snapshot that failed over a tool
// download is worse than a snapshot that is a few days behind.

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// packOrder is the single sequence the three actors in a capture append to, so
// a test can assert the ORDER rather than three separate "it happened" facts.
// The order is the contract: the strip is the only safe wake, the refresh needs
// a woken guest, and the pause takes the guest away.
type packOrder struct {
	mu    sync.Mutex
	steps []string
}

func (o *packOrder) add(step string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.steps = append(o.steps, step)
}

func (o *packOrder) taken() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.steps...)
}

// SandboxChanged makes the order log a host.Observer as well, which is how the
// pause gets into it: the manager reports it from inside its own lock, so this
// records and returns without touching the manager.
func (o *packOrder) SandboxChanged(b *host.Sandbox, reason string) {
	if reason == "paused" {
		o.add("pause:" + b.Name)
	}
}

func (o *packOrder) SandboxGone(string) {}

// orderStripper is an env syncer that only records. The real one's file
// rewriting is covered by TestSnapshotStripsManagedEnvBlock; what matters here
// is that the strip is the step that ran before the refresh.
type orderStripper struct{ order *packOrder }

func (s *orderStripper) PushEnv(context.Context, *host.Sandbox) error { return nil }

func (s *orderStripper) StripEnv(_ context.Context, b *host.Sandbox) error {
	s.order.add("strip:" + b.Name)
	return nil
}

// toolRecorder is the envsync syncer's RefreshTools, reduced to what it was
// asked to do and the state the box was in when it was asked.
type toolRecorder struct {
	order *packOrder

	mu    sync.Mutex
	boxes []host.Sandbox
	err   error
}

func (r *toolRecorder) RefreshTools(_ context.Context, b *host.Sandbox) error {
	r.order.add("refresh:" + b.Name)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.boxes = append(r.boxes, *b)
	return r.err
}

func (r *toolRecorder) refreshed() []host.Sandbox {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]host.Sandbox(nil), r.boxes...)
}

func (r *toolRecorder) countFor(name string) int {
	n := 0
	for _, b := range r.refreshed() {
		if b.Name == name {
			n++
		}
	}
	return n
}

// The refresh is useless unless the guest can still be reached, and the pause
// on the next line is what takes that away. This pins both halves: the record
// handed to the hook is a RUNNING one, and the pause is recorded after it.
func TestSnapshotRefreshesToolsWhileTheGuestIsStillRunning(t *testing.T) {
	ctx := context.Background()
	order := &packOrder{}
	tools := &toolRecorder{order: order}
	m, _ := managerWithDir(t, host.Options{Observer: order})
	m.SetToolSync(tools)

	if _, err := m.Create(ctx, "base", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(ctx, "base", "golden", "alice"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	got := tools.refreshed()
	if len(got) != 1 {
		t.Fatalf("%d tool refreshes, want exactly 1", len(got))
	}
	if got[0].State != vmm.StateRunning {
		t.Errorf("refreshed a %q sandbox; only a running guest can be installed into", got[0].State)
	}
	if got[0].SSHAddr == "" {
		t.Error("refreshed with no SSH address; the syncer has nothing to dial")
	}
	if steps := order.taken(); len(steps) != 2 || steps[0] != "refresh:base" || steps[1] != "pause:base" {
		t.Fatalf("capture ran %v, want the refresh strictly before the pause", steps)
	}
}

// The strip is the only step allowed to wake a paused guest — it goes through
// resumeOrRecreate rather than EnsureRunning, whose asynchronous env push would
// land after the strip and write the secrets back into the disk about to be
// packed. So the refresh has to come after it, and it must not become a second
// wake-up path of its own.
func TestSnapshotRefreshesAfterTheEnvStrip(t *testing.T) {
	ctx := context.Background()
	order := &packOrder{}
	tools := &toolRecorder{order: order}
	m, _ := managerWithDir(t, host.Options{Observer: order})
	m.SetEnvSync(&orderStripper{order: order})
	m.SetToolSync(tools)

	if _, err := m.Create(ctx, "base", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	// Paused first, so the strip has to do the waking and the ordering is a
	// real claim rather than an accident of a box that never stopped.
	if err := m.Pause(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	order.mu.Lock()
	order.steps = nil
	order.mu.Unlock()

	if _, err := m.Snapshot(ctx, "base", "golden", "alice"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	steps := order.taken()
	if len(steps) != 3 || steps[0] != "strip:base" || steps[1] != "refresh:base" || steps[2] != "pause:base" {
		t.Fatalf("capture ran %v, want strip → refresh → pause", steps)
	}
}

// Best-effort, and this is the test that says so. The refresh reaches the open
// half of the world — a host cache that may be empty, a guest whose disk is
// full — and none of that is a reason to refuse somebody a template of the
// machine they just spent an afternoon setting up. A stale template still
// forks.
func TestSnapshotSurvivesAToolRefreshFailure(t *testing.T) {
	ctx := context.Background()
	order := &packOrder{}
	tools := &toolRecorder{order: order, err: errors.New("the host cache is empty")}
	m, dir := managerWithDir(t, host.Options{})
	m.SetToolSync(tools)

	if _, err := m.Create(ctx, "base", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workdirFile(dir, "base", "custom.txt"), []byte("customized"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(ctx, "base", "golden", "alice"); err != nil {
		t.Fatalf("snapshot after a failed tool refresh: %v", err)
	}
	if tools.countFor("base") != 1 {
		t.Fatalf("%d refreshes attempted, want 1", tools.countFor("base"))
	}
	// Forkable, not merely recorded: the failure must not have left the pack
	// half-done.
	if _, err := m.Fork(ctx, "golden", "clone", "alice", 0, 0); err != nil {
		t.Fatalf("fork from a template captured after a failed refresh: %v", err)
	}
	got, err := os.ReadFile(workdirFile(dir, "clone", "custom.txt"))
	if err != nil || string(got) != "customized" {
		t.Fatalf("fork content = %q (%v), want the captured customization", got, err)
	}
}

// Every deployment that has not wired a syncer — and every node, which has no
// signer to open a guest session with — runs with a nil hook. Capture is
// unchanged there.
func TestSnapshotWithoutAToolRefresherProceeds(t *testing.T) {
	ctx := context.Background()
	m, _ := managerWithDir(t, host.Options{})

	if _, err := m.Create(ctx, "base", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(ctx, "base", "golden", "alice"); err != nil {
		t.Fatalf("snapshot with no tool refresher: %v", err)
	}
	if len(m.Snapshots("alice")) != 1 {
		t.Fatal("no snapshot recorded")
	}
}

// A host with no EnvStripper never wakes the guest for a capture, and this step
// must not become the thing that does: waking a box fires the manager's
// asynchronous env push, which is precisely what the strip's own wake exists to
// avoid. So a paused sandbox on such a host is captured with the tools it has —
// honestly, and with a Debug line rather than silence.
func TestSnapshotDoesNotRefreshAPausedGuestOnAHostWithNoStripper(t *testing.T) {
	ctx := context.Background()
	order := &packOrder{}
	tools := &toolRecorder{order: order}
	m, _ := managerWithDir(t, host.Options{})
	m.SetToolSync(tools)

	if _, err := m.Create(ctx, "base", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(ctx, "base", "golden", "alice"); err != nil {
		t.Fatalf("snapshot of a paused box: %v", err)
	}
	if n := tools.countFor("base"); n != 0 {
		t.Fatalf("%d refreshes of a paused guest, want 0 — this step may not be a wake-up path", n)
	}
	if b, _ := m.Get("base"); b.State == vmm.StateRunning {
		t.Fatal("the capture woke a paused sandbox")
	}
	if len(m.Snapshots("alice")) != 1 {
		t.Fatal("the capture did not produce a template")
	}
}

// Only the template path pays for this. An archive and a checkpoint pack the
// same box's own disk to bring it back later, so newer tools inside them buy
// nothing and the minutes come straight out of ctlops.ArchiveTimeout's budget.
func TestArchiveAndCheckpointDoNotRefreshTools(t *testing.T) {
	ctx := context.Background()
	order := &packOrder{}
	tools := &toolRecorder{order: order}
	m, _ := managerWithDir(t, host.Options{Archive: newMemStore(), Checkpoint: newMemStore()})
	m.SetToolSync(tools)

	if _, err := m.Create(ctx, "n1", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := m.Checkpoint(ctx, "n1"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if n := tools.countFor("n1"); n != 0 {
		t.Fatalf("checkpoint refreshed tools %d times, want 0", n)
	}
	if _, err := m.EnsureReady(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Archive(ctx, "n1"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if n := tools.countFor("n1"); n != 0 {
		t.Fatalf("archive refreshed tools %d times, want 0", n)
	}
}
