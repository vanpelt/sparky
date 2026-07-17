package host_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// newTestManager wires a Manager onto the in-process mock driver with the given
// limits. Returned Manager persists to a temp dir; the driver is closed on
// cleanup so per-VM listeners don't leak between tests.
func newTestManager(t *testing.T, opts host.Options) *host.Manager {
	t.Helper()
	dir := t.TempDir()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	driver := mock.New(dir, signer)
	t.Cleanup(func() { driver.Close() })

	opts.StateDir = dir
	opts.Driver = driver
	opts.GatewayPublicKey = string(xssh.MarshalAuthorizedKey(signer.PublicKey()))
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := host.NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

func mustCreate(t *testing.T, m *host.Manager, name, owner string, memMB int64) {
	t.Helper()
	if _, err := m.Create(context.Background(), name, owner, "ubuntu", 1, memMB); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func TestPerOwnerRunningCap(t *testing.T) {
	m := newTestManager(t, host.Options{MaxRunningPerOwner: 2})

	mustCreate(t, m, "a1", "alice", 512)
	mustCreate(t, m, "a2", "alice", 512)

	// Third running sandbox for alice is refused with a typed, populated error.
	_, err := m.Create(context.Background(), "a3", "alice", "ubuntu", 1, 512)
	var limit *host.LimitError
	if !errors.As(err, &limit) {
		t.Fatalf("want *LimitError, got %v", err)
	}
	if limit.Max != 2 || len(limit.Running) != 2 {
		t.Fatalf("unexpected LimitError: %+v", limit)
	}
	if limit.Running[0] != "a1" || limit.Running[1] != "a2" {
		t.Fatalf("Running not sorted/complete: %v", limit.Running)
	}

	// The cap is per-owner: bob is unaffected.
	mustCreate(t, m, "b1", "bob", 512)

	// Pausing frees a slot for alice.
	if err := m.Pause(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, m, "a3", "alice", 512)
}

func TestResumeRespectsCap(t *testing.T) {
	m := newTestManager(t, host.Options{MaxRunningPerOwner: 2})
	mustCreate(t, m, "a1", "alice", 512)
	mustCreate(t, m, "a2", "alice", 512)
	if err := m.Pause(context.Background(), "a1"); err != nil { // a2 running, a1 paused
		t.Fatal(err)
	}
	mustCreate(t, m, "a3", "alice", 512) // a2,a3 running; a1 paused -> 2 running

	// Reconnecting to the paused a1 would make 3 running: refused.
	_, err := m.EnsureRunning(context.Background(), "a1")
	var limit *host.LimitError
	if !errors.As(err, &limit) {
		t.Fatalf("resume past cap: want *LimitError, got %v", err)
	}
}

func TestMemAdmission(t *testing.T) {
	// Budget = 2048 MB * 100% = 2048. Unlimited count so only RAM gates.
	m := newTestManager(t, host.Options{MemAdmissionPct: 100, HostMemMB: 2048})

	mustCreate(t, m, "a1", "alice", 1024) // used 0 -> 1024
	mustCreate(t, m, "a2", "alice", 1024) // used 1024 -> 2048 (== budget, allowed)

	_, err := m.Create(context.Background(), "a3", "alice", "ubuntu", 1, 1024)
	var capErr *host.CapacityError
	if !errors.As(err, &capErr) {
		t.Fatalf("want *CapacityError, got %v", err)
	}
	if capErr.BudgetMB != 2048 || capErr.UsedMB != 2048 || capErr.RequestedMB != 1024 {
		t.Fatalf("unexpected CapacityError: %+v", capErr)
	}

	// Freeing RAM (pause) lets the next start in.
	if err := m.Pause(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, m, "a3", "alice", 1024)
}

func TestNoLimitsWhenZero(t *testing.T) {
	m := newTestManager(t, host.Options{}) // all limits disabled
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5"} {
		mustCreate(t, m, n, "alice", 4096)
	}
	if got := len(m.List()); got != 5 {
		t.Fatalf("want 5 sandboxes, got %d", got)
	}
	// Sanity: they're all running (nothing refused).
	for _, b := range m.List() {
		if b.State != vmm.StateRunning {
			t.Fatalf("%s not running: %s", b.Name, b.State)
		}
	}
}

// waitFor polls cond until it returns true or ~2s elapses, failing the test on
// timeout. Used to observe an asynchronous loop (the reaper) without a fixed
// sleep.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

func TestPinnedSurvivesReaper(t *testing.T) {
	m := newTestManager(t, host.Options{})
	mustCreate(t, m, "keep", "alice", 512)
	mustCreate(t, m, "drop", "alice", 512)
	if err := m.SetPinned("keep", true); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// timeout=0: every idle running sandbox is eligible on the first tick.
	go m.RunReaper(ctx, 0, time.Millisecond)

	// The unpinned sandbox is paused by the reaper...
	waitFor(t, func() bool {
		b, _ := m.Get("drop")
		return b.State == vmm.StatePaused
	})
	// ...but the pinned one is never touched.
	if b, _ := m.Get("keep"); b.State != vmm.StateRunning {
		t.Fatalf("pinned sandbox was reaped: state=%s", b.State)
	}
	if b, _ := m.Get("keep"); !b.Pinned {
		t.Fatal("pin flag lost across the reaper run")
	}
}

func TestResumePinnedOnBoot(t *testing.T) {
	m := newTestManager(t, host.Options{})
	ctx := context.Background()
	mustCreate(t, m, "keep", "alice", 512)
	mustCreate(t, m, "drop", "alice", 512)
	if err := m.SetPinned("keep", true); err != nil {
		t.Fatal(err)
	}
	// Simulate a host restart: everything is paused.
	for _, n := range []string{"keep", "drop"} {
		if err := m.Pause(ctx, n); err != nil {
			t.Fatal(err)
		}
	}

	m.ResumePinned(ctx)

	if b, _ := m.Get("keep"); b.State != vmm.StateRunning {
		t.Fatalf("pinned sandbox not resumed on boot: state=%s", b.State)
	}
	if b, _ := m.Get("drop"); b.State != vmm.StatePaused {
		t.Fatalf("unpinned sandbox resumed unexpectedly: state=%s", b.State)
	}
}

func TestSetPinnedUnknown(t *testing.T) {
	m := newTestManager(t, host.Options{})
	if err := m.SetPinned("ghost", true); err == nil {
		t.Fatal("SetPinned on a missing sandbox should error")
	}
}

// recordingFrontDoor captures Ensure/Remove calls for assertions.
type recordingFrontDoor struct {
	ensured, removed []string
}

func (f *recordingFrontDoor) Ensure(_ context.Context, name string) {
	f.ensured = append(f.ensured, name)
}
func (f *recordingFrontDoor) Remove(_ context.Context, name string) {
	f.removed = append(f.removed, name)
}

func TestFrontDoorHook(t *testing.T) {
	fd := &recordingFrontDoor{}
	mgr := newTestManager(t, host.Options{FrontDoor: fd})
	ctx := context.Background()

	if _, err := mgr.Create(ctx, "doorful", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if len(fd.ensured) != 1 || fd.ensured[0] != "doorful" {
		t.Fatalf("Ensure calls = %v, want [doorful]", fd.ensured)
	}
	// Pause/resume must not touch the front door — the address is name-based
	// and stable for the sandbox's whole life.
	if err := mgr.Pause(ctx, "doorful"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.EnsureRunning(ctx, "doorful"); err != nil {
		t.Fatal(err)
	}
	if len(fd.ensured) != 1 || len(fd.removed) != 0 {
		t.Fatalf("pause/resume touched the front door: ensured=%v removed=%v", fd.ensured, fd.removed)
	}
	if err := mgr.Destroy(ctx, "doorful"); err != nil {
		t.Fatal(err)
	}
	if len(fd.removed) != 1 || fd.removed[0] != "doorful" {
		t.Fatalf("Remove calls = %v, want [doorful]", fd.removed)
	}
}
