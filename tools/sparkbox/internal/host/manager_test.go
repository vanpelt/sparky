package host_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// newTestManager wires a Manager onto the in-process mock driver with the given
// limits. Returned Manager persists to a temp dir; the driver is closed on
// cleanup so per-VM listeners don't leak between tests.
func newTestManager(t *testing.T, opts host.Options) *host.Manager {
	t.Helper()
	return newTestManagerWith(t, opts, nil)
}

// newTestManagerWith is newTestManager with a driver decorator: wrap (if
// non-nil) turns the mock into the driver the Manager sees, so tests can
// observe or hide driver calls.
func newTestManagerWith(t *testing.T, opts host.Options, wrap func(*mock.Driver) vmm.Driver) *host.Manager {
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
	opts.Driver = vmm.Driver(driver)
	if wrap != nil {
		opts.Driver = wrap(driver)
	}
	opts.GatewayPublicKey = string(xssh.MarshalAuthorizedKey(signer.PublicKey()))
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := host.NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

// newManagerInDir wires a manager on the mock driver over a caller-owned state
// dir, so a test can poke the mock's per-VM workdir (its "disk") or "restart
// the host" by building a second manager over the same state.
func newManagerInDir(t *testing.T, dir string, opts host.Options) *host.Manager {
	t.Helper()
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

type failingUnpackDriver struct {
	*mock.Driver
	fail atomic.Bool
}

func (d *failingUnpackDriver) UnpackRootfs(ctx context.Context, name, inPath string) error {
	if d.fail.Load() {
		return errors.New("injected unpack failure")
	}
	return d.Driver.UnpackRootfs(ctx, name, inPath)
}

func TestRestoreCheckpointFailureRestartsPreviouslyRunningSandbox(t *testing.T) {
	store := newMemStore()
	var driver *failingUnpackDriver
	m := newTestManagerWith(t, host.Options{Checkpoint: store}, func(base *mock.Driver) vmm.Driver {
		driver = &failingUnpackDriver{Driver: base}
		return driver
	})
	ctx := context.Background()
	mustCreate(t, m, "restore-recovery", "alice", 512)
	if err := m.Checkpoint(ctx, "restore-recovery"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	driver.fail.Store(true)
	if err := m.RestoreCheckpoint(ctx, "restore-recovery"); err == nil {
		t.Fatal("restore should fail")
	}
	box, ok := m.Get("restore-recovery")
	if !ok {
		t.Fatal("sandbox disappeared after failed restore")
	}
	if box.State != vmm.StateRunning {
		t.Fatalf("state after failed restore = %q, want running", box.State)
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
	_, err := m.EnsureReady(context.Background(), "a1")
	var limit *host.LimitError
	if !errors.As(err, &limit) {
		t.Fatalf("resume past cap: want *LimitError, got %v", err)
	}
}

type blockingResumeDriver struct {
	*mock.Driver
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	resumes atomic.Int64
}

type contextValueKey struct{}

type valueResumeDriver struct {
	*mock.Driver
	got any
}

func (d *valueResumeDriver) Resume(ctx context.Context, name string) (*vmm.Instance, error) {
	d.got = ctx.Value(contextValueKey{})
	return d.Driver.Resume(ctx, name)
}

func TestEnsureReadyPreservesCallerContextValues(t *testing.T) {
	var d *valueResumeDriver
	m := newTestManagerWith(t, host.Options{}, func(md *mock.Driver) vmm.Driver {
		d = &valueResumeDriver{Driver: md}
		return d
	})
	mustCreate(t, m, "shared", "alice", 512)
	if err := m.Pause(context.Background(), "shared"); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), contextValueKey{}, "operation-identity")
	if _, err := m.EnsureReady(ctx, "shared"); err != nil {
		t.Fatal(err)
	}
	if d.got != "operation-identity" {
		t.Fatalf("resume context value = %v, want operation identity", d.got)
	}
}

func (d *blockingResumeDriver) Resume(ctx context.Context, name string) (*vmm.Instance, error) {
	d.resumes.Add(1)
	d.once.Do(func() { close(d.entered) })
	select {
	case <-d.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.Driver.Resume(ctx, name)
}

func TestConcurrentEnsureReadyResumesOnce(t *testing.T) {
	var d *blockingResumeDriver
	m := newTestManagerWith(t, host.Options{}, func(md *mock.Driver) vmm.Driver {
		d = &blockingResumeDriver{
			Driver: md, entered: make(chan struct{}), release: make(chan struct{}),
		}
		return d
	})
	mustCreate(t, m, "shared", "alice", 512)
	if err := m.Pause(context.Background(), "shared"); err != nil {
		t.Fatal(err)
	}

	const callers = 100
	start := make(chan struct{})
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			<-start
			_, err := m.EnsureReady(context.Background(), "shared")
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	<-d.entered
	close(d.release)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("EnsureReady: %v", err)
		}
	}
	if got := d.resumes.Load(); got != 1 {
		t.Fatalf("driver Resume calls = %d, want 1", got)
	}
}

func TestCancelledEnsureReadyCallerDoesNotPoisonSharedResume(t *testing.T) {
	lifecycle, stop := context.WithCancel(context.Background())
	defer stop()
	var d *blockingResumeDriver
	m := newTestManagerWith(t, host.Options{Context: lifecycle}, func(md *mock.Driver) vmm.Driver {
		d = &blockingResumeDriver{
			Driver: md, entered: make(chan struct{}), release: make(chan struct{}),
		}
		return d
	})
	mustCreate(t, m, "shared", "alice", 512)
	if err := m.Pause(context.Background(), "shared"); err != nil {
		t.Fatal(err)
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() {
		_, err := m.EnsureReady(leaderCtx, "shared")
		leader <- err
	}()
	<-d.entered

	follower := make(chan error, 1)
	go func() {
		_, err := m.EnsureReady(context.Background(), "shared")
		follower <- err
	}()
	cancelLeader()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled leader = %v, want context.Canceled", err)
	}
	close(d.release)
	if err := <-follower; err != nil {
		t.Fatalf("live follower inherited leader cancellation: %v", err)
	}
	if got := d.resumes.Load(); got != 1 {
		t.Fatalf("driver Resume calls = %d, want 1", got)
	}
}

func TestManagerLifecycleCancelsSharedResume(t *testing.T) {
	lifecycle, stop := context.WithCancel(context.Background())
	var d *blockingResumeDriver
	m := newTestManagerWith(t, host.Options{Context: lifecycle}, func(md *mock.Driver) vmm.Driver {
		d = &blockingResumeDriver{
			Driver: md, entered: make(chan struct{}), release: make(chan struct{}),
		}
		return d
	})
	mustCreate(t, m, "shared", "alice", 512)
	if err := m.Pause(context.Background(), "shared"); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := m.EnsureReady(context.Background(), "shared")
		result <- err
	}()
	<-d.entered
	stop()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("resume after manager shutdown = %v, want context.Canceled", err)
	}
}

func TestWarmEnsureReadyDefersPersistenceUntilActivityFlush(t *testing.T) {
	dir := t.TempDir()
	m := newManagerInDir(t, dir, host.Options{})
	mustCreate(t, m, "warm", "alice", 512)
	path := filepath.Join(dir, "sandboxes.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.EnsureReady(context.Background(), "warm"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("a warm EnsureReady synchronously rewrote manager state")
	}
	if err := m.FlushActivity(); err != nil {
		t.Fatal(err)
	}
	flushed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(flushed) == string(before) {
		t.Fatal("FlushActivity did not persist the warm activity timestamp")
	}
}

func TestSandboxIDPersistsAcrossRenameAndRestart(t *testing.T) {
	dir := t.TempDir()
	m := newManagerInDir(t, dir, host.Options{})
	box, err := m.Create(context.Background(), "before", "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatal(err)
	}
	if box.ID == "" {
		t.Fatal("created sandbox has no immutable ID")
	}
	if _, err := uuid.Parse(box.ID); err != nil {
		t.Fatalf("sandbox ID %q is not a UUID: %v", box.ID, err)
	}
	if err := m.Rename(context.Background(), "before", "after", "alice"); err != nil {
		t.Fatal(err)
	}
	renamed, ok := m.Get("after")
	if !ok || renamed.ID != box.ID {
		t.Fatalf("rename changed sandbox ID: got %q, want %q", renamed.ID, box.ID)
	}

	reloaded := newManagerInDir(t, dir, host.Options{})
	afterRestart, ok := reloaded.Get("after")
	if !ok || afterRestart.ID != box.ID {
		t.Fatalf("restart changed sandbox ID: got %q, want %q", afterRestart.ID, box.ID)
	}
}

func TestManagerBackfillsLegacySandboxID(t *testing.T) {
	dir := t.TempDir()
	m := newManagerInDir(t, dir, host.Options{})
	mustCreate(t, m, "legacy", "alice", 512)

	statePath := filepath.Join(dir, "sandboxes.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var records map[string]map[string]any
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatal(err)
	}
	delete(records["legacy"], "id")
	raw, err = json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	reloaded := newManagerInDir(t, dir, host.Options{})
	backfilled, ok := reloaded.Get("legacy")
	if !ok || backfilled.ID == "" {
		t.Fatal("legacy sandbox ID was not backfilled")
	}
	restarted := newManagerInDir(t, dir, host.Options{})
	persisted, ok := restarted.Get("legacy")
	if !ok || persisted.ID != backfilled.ID {
		t.Fatalf("backfilled ID was not persisted: got %q, want %q", persisted.ID, backfilled.ID)
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

func TestReserveAdmissionMultipliesDensity(t *testing.T) {
	// Budget = 8192 MB. With a 1024 MB working-set reserve, each 8 GB-ceiling
	// sandbox is charged 1024 MB, so 8 fit where the old full-ceiling accounting
	// fit only 1.
	m := newTestManager(t, host.Options{
		MemAdmissionPct: 100, HostMemMB: 8192, MemReserveMB: 1024,
	})
	for i := 0; i < 8; i++ {
		mustCreate(t, m, fmt.Sprintf("v%d", i), "alice", 8192)
	}
	// The 9th would push effective usage to 9×1024 > 8192: refused.
	_, err := m.Create(context.Background(), "v8", "alice", "ubuntu", 1, 8192)
	var capErr *host.CapacityError
	if !errors.As(err, &capErr) {
		t.Fatalf("want *CapacityError past the reserve budget, got %v", err)
	}
	if capErr.RequestedMB != 1024 || capErr.UsedMB != 8192 || capErr.BudgetMB != 8192 {
		t.Fatalf("unexpected CapacityError: %+v", capErr)
	}
}

func TestOwnerMemoryPoolIsSharedAcrossVMs(t *testing.T) {
	// The node has ample room, but each owner gets one 8 GiB effective-memory
	// envelope. Four 8 GiB-ceiling VMs fit at a 2 GiB working-set charge.
	m := newTestManager(t, host.Options{
		MemAdmissionPct: 100, HostMemMB: 65536,
		MemReserveMB: 2048, OwnerMemoryPoolMB: 8192,
	})
	for i := 0; i < 4; i++ {
		mustCreate(t, m, fmt.Sprintf("alice-%d", i), "alice", 8192)
	}
	_, err := m.Create(context.Background(), "alice-4", "alice", "ubuntu", 2, 8192)
	var capErr *host.CapacityError
	if !errors.As(err, &capErr) {
		t.Fatalf("want owner *CapacityError, got %v", err)
	}
	if capErr.Owner != "alice" || capErr.UsedMB != 8192 || capErr.RequestedMB != 2048 || capErr.BudgetMB != 8192 {
		t.Fatalf("unexpected owner CapacityError: %+v", capErr)
	}

	// Bob owns an independent envelope on the same node.
	mustCreate(t, m, "bob-0", "bob", 8192)
	owner := m.CapacityForOwner("alice")
	if owner.MemoryPoolMB != 8192 || owner.EffectiveMemMB != 8192 ||
		owner.AllocatedMemMB != 4*8192 || owner.RunningSandboxes != 4 || owner.TotalSandboxes != 4 {
		t.Fatalf("unexpected owner capacity: %+v", owner)
	}

	// Pausing returns the charge to Alice's pool and admits another child.
	if err := m.Pause(context.Background(), "alice-0"); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, m, "alice-4", "alice", 8192)
}

func TestOwnerMemoryPoolWithoutOvercommitChargesCeilings(t *testing.T) {
	m := newTestManager(t, host.Options{OwnerMemoryPoolMB: 8192})
	mustCreate(t, m, "first", "alice", 8192)
	_, err := m.Create(context.Background(), "second", "alice", "ubuntu", 2, 8192)
	var capErr *host.CapacityError
	if !errors.As(err, &capErr) || capErr.Owner != "alice" {
		t.Fatalf("want Alice's pooled capacity error, got %v", err)
	}
}

func TestOwnerPoolsOverlapOnNodeCapacity(t *testing.T) {
	m := newTestManager(t, host.Options{
		HostMemMB: 4096, MemAdmissionPct: 100, MemReserveMB: 256,
		OwnerMemoryPoolMB: 8192, OwnerMemoryBurstMB: 16384,
	})
	mustCreate(t, m, "alice-box", "alice", 8192)
	mustCreate(t, m, "bob-box", "bob", 8192)

	c := m.Capacity()
	if c.ActiveOwners != 2 || c.EntitledMemMB != 16384 || c.EffectiveMemMB != 512 {
		t.Fatalf("overlapping capacity = %+v", c)
	}
	if c.EntitledMemMB <= c.BudgetMemMB {
		t.Fatalf("test did not prove overcommit: entitled %d <= physical budget %d",
			c.EntitledMemMB, c.BudgetMemMB)
	}
}

func TestOwnerBurstConfigurationValidation(t *testing.T) {
	for _, opts := range []host.Options{
		{OwnerMemoryBurstMB: 16384},
		{OwnerMemoryPoolMB: 8192, OwnerMemoryBurstMB: 4096},
	} {
		opts.StateDir = t.TempDir()
		if _, err := host.NewManager(opts); err == nil {
			t.Fatalf("invalid owner burst config accepted: %+v", opts)
		}
	}
}

func TestTotalSandboxLimitCountsPausedVMs(t *testing.T) {
	m := newTestManager(t, host.Options{MaxSandboxesPerOwner: 2})
	mustCreate(t, m, "first", "alice", 1024)
	if err := m.Pause(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, m, "second", "alice", 1024)
	_, err := m.Create(context.Background(), "third", "alice", "ubuntu", 2, 1024)
	var stateErr *host.StateError
	if !errors.As(err, &stateErr) || stateErr.Code != "sandbox_limit" {
		t.Fatalf("want sandbox_limit with a paused VM counted, got %v", err)
	}

	// The cap is per owner, and deleting an identity returns its slot.
	mustCreate(t, m, "bob-first", "bob", 1024)
	if err := m.Destroy(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, m, "third", "alice", 1024)
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
	// pauseAfter=0: every idle running sandbox is eligible on the first tick.
	// balloonAfter=0 disables the balloon stage (this test is about pausing).
	go m.RunReaper(ctx, 0, 0, time.Millisecond)

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
	if _, err := mgr.EnsureReady(ctx, "doorful"); err != nil {
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

func TestListByOwner(t *testing.T) {
	m := newTestManager(t, host.Options{})
	mustCreate(t, m, "b1", "bob", 512)
	mustCreate(t, m, "a2", "alice", 512)
	mustCreate(t, m, "a1", "alice", 512)

	got := m.ListByOwner("alice")
	if len(got) != 2 || got[0].Name != "a1" || got[1].Name != "a2" {
		t.Fatalf("ListByOwner(alice) = %v, want [a1 a2]", names(got))
	}
	if got := m.ListByOwner("carol"); len(got) != 0 {
		t.Fatalf("ListByOwner(carol) = %v, want empty", names(got))
	}
}

func names(boxes []*host.Sandbox) []string {
	var out []string
	for _, b := range boxes {
		out = append(out, b.Name)
	}
	return out
}

// recordingDriver decorates the mock driver to record lifecycle calls, so
// ordering (snapshots dropped before the VM dir moves; reboot cold-boots via a
// fresh Create) is assertable through the Manager.
type recordingDriver struct {
	*mock.Driver
	mu    sync.Mutex
	calls []string
}

func (d *recordingDriver) record(c string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, c)
}

func (d *recordingDriver) recorded() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func (d *recordingDriver) Create(ctx context.Context, cfg vmm.Config) (*vmm.Instance, error) {
	d.record("create " + cfg.Name)
	return d.Driver.Create(ctx, cfg)
}

func (d *recordingDriver) DropSnapshots(name string) error {
	d.record("drop " + name)
	return d.Driver.DropSnapshots(name)
}

func (d *recordingDriver) RenameVM(oldName, newName string) error {
	d.record("rename " + oldName + ">" + newName)
	return d.Driver.RenameVM(oldName, newName)
}

func indexOf(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

func TestRenameRunningBox(t *testing.T) {
	var rd *recordingDriver
	m := newTestManagerWith(t, host.Options{}, func(d *mock.Driver) vmm.Driver {
		rd = &recordingDriver{Driver: d}
		return rd
	})
	ctx := context.Background()
	mustCreate(t, m, "before", "alice", 512)

	if err := m.Rename(ctx, "before", "after", "alice"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// The running box was auto-paused and the record rekeyed.
	if _, ok := m.Get("before"); ok {
		t.Fatal("old name still present after rename")
	}
	b, ok := m.Get("after")
	if !ok || b.Name != "after" || b.Owner != "alice" {
		t.Fatalf("renamed record wrong: %+v ok=%v", b, ok)
	}
	if b.State != vmm.StatePaused {
		t.Fatalf("renamed box state = %s, want paused", b.State)
	}
	if b.RenamedFrom != "" {
		t.Fatalf("rename journal not cleared after commit: %q", b.RenamedFrom)
	}
	// Snapshots were dropped strictly before the dir moved (a firecracker
	// state.snap embeds absolute paths into the old dir).
	calls := rd.recorded()
	drop, ren := indexOf(calls, "drop before"), indexOf(calls, "rename before>after")
	if drop == -1 || ren == -1 || drop > ren {
		t.Fatalf("want drop before rename, got calls %v", calls)
	}
	// The next start cold-boots the moved rootfs under the new name.
	if _, err := m.EnsureReady(ctx, "after"); err != nil {
		t.Fatalf("resume after rename: %v", err)
	}
	if indexOf(rd.recorded(), "create after") == -1 {
		t.Fatalf("no cold boot under the new name: calls %v", rd.recorded())
	}
}

// simulateRenameCrash rewrites sandboxes.json into the journal state a crash
// mid-rename leaves behind: the record keyed by the new name with RenamedFrom
// set. When moved is true the VM dir has already moved (crash after the dir
// move, before the final save); otherwise it still sits under the old name.
func simulateRenameCrash(t *testing.T, dir, oldName, newName string, moved bool) {
	t.Helper()
	statePath := filepath.Join(dir, "sandboxes.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var boxes map[string]*host.Sandbox
	if err := json.Unmarshal(raw, &boxes); err != nil {
		t.Fatal(err)
	}
	b := boxes[oldName]
	if b == nil {
		t.Fatalf("no %q record in %s", oldName, statePath)
	}
	delete(boxes, oldName)
	b.Name = newName
	b.RenamedFrom = oldName
	boxes[newName] = b
	out, err := json.Marshal(boxes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	if moved {
		vms := filepath.Join(dir, "mock-vms")
		if err := os.Rename(filepath.Join(vms, oldName), filepath.Join(vms, newName)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRenameCrashReconciledOnLoad(t *testing.T) {
	for _, tc := range []struct {
		name  string
		moved bool
	}{
		{"crash before dir move", false},
		{"crash after dir move", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			m1 := newManagerInDir(t, dir, host.Options{})
			mustCreate(t, m1, "before", "alice", 512)
			// Drop a marker into the VM's "disk" to prove the box's real rootfs
			// — not a fresh one — comes back under the new name.
			marker := workdirFile(dir, "before", "keep.txt")
			if err := os.WriteFile(marker, []byte("real rootfs"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := m1.Pause(ctx, "before"); err != nil {
				t.Fatal(err)
			}

			simulateRenameCrash(t, dir, "before", "after", tc.moved)

			// "Host restart": a fresh manager over the same state reconciles
			// the journal, leaving the box intact under exactly one name.
			m2 := newManagerInDir(t, dir, host.Options{})
			if _, ok := m2.Get("before"); ok {
				t.Fatal("old name survived reconcile")
			}
			b, ok := m2.Get("after")
			if !ok {
				t.Fatal("renamed box missing after reconcile")
			}
			if b.RenamedFrom != "" {
				t.Fatalf("rename journal not cleared on load: %q", b.RenamedFrom)
			}
			if _, err := m2.EnsureReady(ctx, "after"); err != nil {
				t.Fatalf("resume after reconcile: %v", err)
			}
			got, err := os.ReadFile(workdirFile(dir, "after", "keep.txt"))
			if err != nil {
				t.Fatalf("rootfs lost across crashed rename: %v", err)
			}
			if string(got) != "real rootfs" {
				t.Fatalf("rootfs content = %q, want %q", got, "real rootfs")
			}
		})
	}
}

func TestRenameRefusals(t *testing.T) {
	m := newTestManager(t, host.Options{})
	ctx := context.Background()
	mustCreate(t, m, "vic", "alice", 512)
	mustCreate(t, m, "taken", "alice", 512)

	for _, tc := range []struct {
		name          string
		old, new, own string
	}{
		{"invalid new name", "vic", "Bad_Name", "alice"},
		{"reserved new name", "vic", "console", "alice"},
		{"collision", "vic", "taken", "alice"},
		{"wrong owner", "vic", "fresh", "bob"},
		{"missing box", "ghost", "fresh", "alice"},
	} {
		if err := m.Rename(ctx, tc.old, tc.new, tc.own); err == nil {
			t.Errorf("%s: rename %s->%s as %s succeeded, want error", tc.name, tc.old, tc.new, tc.own)
		}
	}
	// Nothing was renamed or paused by the refusals above.
	if b, ok := m.Get("vic"); !ok || b.State != vmm.StateRunning {
		t.Fatalf("vic disturbed by refused renames: %+v ok=%v", b, ok)
	}
}

func TestRenameArchivedRefused(t *testing.T) {
	store := newMemStore()
	m := newTestManager(t, host.Options{Archive: store})
	ctx := context.Background()
	mustCreate(t, m, "cold", "alice", 512)
	if err := m.Archive(ctx, "cold"); err != nil {
		t.Fatal(err)
	}
	err := m.Rename(ctx, "cold", "warm", "alice")
	if err == nil || !contains(err.Error(), "archived") {
		t.Fatalf("rename of archived box: want archived error, got %v", err)
	}
}

func TestRenameRouteCollision(t *testing.T) {
	dir := t.TempDir()
	rs, err := routes.Open(filepath.Join(dir, "routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	m := newTestManager(t, host.Options{Routes: rs})
	ctx := context.Background()
	mustCreate(t, m, "one", "alice", 512)
	mustCreate(t, m, "two", "bob", 512)
	// bob points a custom subdomain at his box; alice can't rename onto it.
	if err := rs.Upsert(routes.Route{Subdomain: "shiny", Sandbox: "two", Owner: "bob", Port: 3000}); err != nil {
		t.Fatal(err)
	}
	err = m.Rename(ctx, "one", "shiny", "alice")
	if err == nil || !contains(err.Error(), "taken") {
		t.Fatalf("rename onto a taken subdomain: want taken error, got %v", err)
	}
}

// TestRenameRepairsHalfMovedRoutes: a crash between the routes hook and the
// record commit leaves the routes already at the new name while the record
// still holds the old one. Renaming again must complete the move — the route
// check tolerates the box's own half-moved row and the store's RenameSandbox
// is idempotent — instead of erroring "taken" forever.
func TestRenameRepairsHalfMovedRoutes(t *testing.T) {
	dir := t.TempDir()
	rs, err := routes.Open(filepath.Join(dir, "routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	m := newTestManager(t, host.Options{Routes: rs})
	ctx := context.Background()
	mustCreate(t, m, "before", "alice", 512)

	// Simulate the crash state: routes moved, record not.
	if err := rs.RenameSandbox("before", "after"); err != nil {
		t.Fatal(err)
	}
	if err := m.Rename(ctx, "before", "after", "alice"); err != nil {
		t.Fatalf("rename over half-moved routes: %v", err)
	}
	if _, ok := m.Get("after"); !ok {
		t.Fatal("record did not move to the new name")
	}
	r, found, err := rs.GetBySubdomain("after")
	if err != nil || !found || r.Sandbox != "after" || r.Owner != "alice" {
		t.Fatalf("route after repair = %+v found=%v err=%v", r, found, err)
	}
	if _, found, _ := rs.GetBySubdomain("before"); found {
		t.Fatal("old subdomain still routed after repair")
	}
}

// A stale route row squatting the new subdomain under sandbox == newName but a
// DIFFERENT owner (orphan of a destroyed box whose cleanup warn-failed) must
// still refuse the rename: its Owner column gates private-route auth, so
// silently adopting it would authorize the wrong user.
func TestRenameStaleForeignRouteRefused(t *testing.T) {
	dir := t.TempDir()
	rs, err := routes.Open(filepath.Join(dir, "routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	m := newTestManager(t, host.Options{Routes: rs})
	ctx := context.Background()
	mustCreate(t, m, "one", "alice", 512)
	if err := rs.Upsert(routes.Route{Subdomain: "shiny", Sandbox: "shiny", Owner: "bob", Port: 3000}); err != nil {
		t.Fatal(err)
	}
	err = m.Rename(ctx, "one", "shiny", "alice")
	if err == nil || !contains(err.Error(), "taken") {
		t.Fatalf("rename onto a foreign stale route: want taken error, got %v", err)
	}
}

// failRenameDriver makes the dir move fail so the routes rollback is testable.
type failRenameDriver struct{ *mock.Driver }

func (d *failRenameDriver) RenameVM(oldName, newName string) error {
	return errors.New("disk on fire")
}

// The routes move fires before the record commit; if the dir move then fails,
// the routes must be rolled back so record and routes stay in step.
func TestRenameRollsBackRoutesOnVMFailure(t *testing.T) {
	dir := t.TempDir()
	rs, err := routes.Open(filepath.Join(dir, "routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	m := newTestManagerWith(t, host.Options{Routes: rs}, func(d *mock.Driver) vmm.Driver {
		return &failRenameDriver{Driver: d}
	})
	ctx := context.Background()
	mustCreate(t, m, "before", "alice", 512)

	err = m.Rename(ctx, "before", "after", "alice")
	if err == nil || !contains(err.Error(), "disk on fire") {
		t.Fatalf("rename with failing dir move: want driver error, got %v", err)
	}
	if _, ok := m.Get("before"); !ok {
		t.Fatal("record lost its old name after failed rename")
	}
	r, found, err := rs.GetBySubdomain("before")
	if err != nil || !found || r.Sandbox != "before" {
		t.Fatalf("route not rolled back: %+v found=%v err=%v", r, found, err)
	}
	if _, found, _ := rs.GetBySubdomain("after"); found {
		t.Fatal("new subdomain still routed after rollback")
	}
}

// contains reports whether s contains sub (no strings import in the original
// test file; keep the dependency footprint as-is).
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// tagRecorder records TagCleaner calls; fail makes every call error so the
// best-effort contract is testable.
type tagRecorder struct {
	mu      sync.Mutex
	deleted []string
	renamed []string
	fail    bool
}

func (c *tagRecorder) DeleteBySandbox(sandbox string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("tag store down")
	}
	c.deleted = append(c.deleted, sandbox)
	return nil
}

func (c *tagRecorder) RenameSandbox(old, new string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("tag store down")
	}
	c.renamed = append(c.renamed, old+">"+new)
	return nil
}

// schedRecorder is a ScheduleCleaner that also implements the rename hook, as
// schedule.Store will (RenameSandbox(old, new string) error).
type schedRecorder struct{ tagRecorder }

func TestRenameSideHooks(t *testing.T) {
	tags := &tagRecorder{}
	sched := &schedRecorder{}
	fd := &recordingFrontDoor{}
	m := newTestManager(t, host.Options{Tags: tags, Schedules: sched, FrontDoor: fd})
	ctx := context.Background()
	mustCreate(t, m, "before", "alice", 512)

	if err := m.Rename(ctx, "before", "after", "alice"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(tags.renamed) != 1 || tags.renamed[0] != "before>after" {
		t.Fatalf("tag rename calls = %v, want [before>after]", tags.renamed)
	}
	if len(sched.renamed) != 1 || sched.renamed[0] != "before>after" {
		t.Fatalf("schedule rename calls = %v, want [before>after]", sched.renamed)
	}
	// The front door drops the old address and plumbs the new one.
	if len(fd.removed) != 1 || fd.removed[0] != "before" {
		t.Fatalf("front door Remove calls = %v, want [before]", fd.removed)
	}
	if indexOf(fd.ensured, "after") == -1 {
		t.Fatalf("front door Ensure calls = %v, want to include after", fd.ensured)
	}
}

func TestRenameSurvivesHookFailure(t *testing.T) {
	tags := &tagRecorder{fail: true}
	m := newTestManager(t, host.Options{Tags: tags})
	ctx := context.Background()
	mustCreate(t, m, "before", "alice", 512)

	// Side plumbing is best-effort: a down tag store never fails the rename.
	if err := m.Rename(ctx, "before", "after", "alice"); err != nil {
		t.Fatalf("rename with failing hook: %v", err)
	}
	if _, ok := m.Get("after"); !ok {
		t.Fatal("rename did not commit despite best-effort hook contract")
	}
}

func TestDestroyCleansTags(t *testing.T) {
	tags := &tagRecorder{}
	m := newTestManager(t, host.Options{Tags: tags})
	ctx := context.Background()
	mustCreate(t, m, "gone", "alice", 512)
	if err := m.Destroy(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if len(tags.deleted) != 1 || tags.deleted[0] != "gone" {
		t.Fatalf("tag delete calls = %v, want [gone]", tags.deleted)
	}
}

func TestRebootColdBoots(t *testing.T) {
	var rd *recordingDriver
	m := newTestManagerWith(t, host.Options{}, func(d *mock.Driver) vmm.Driver {
		rd = &recordingDriver{Driver: d}
		return rd
	})
	ctx := context.Background()
	mustCreate(t, m, "loop", "alice", 512)

	// Accrue some CPU time so the cold boot's counter reset is observable.
	var before float64
	for i := 0; i < 3; i++ {
		var ok bool
		before, ok = m.CPUSeconds(ctx, "loop")
		if !ok {
			t.Fatal("CPUSeconds unavailable on a running mock box")
		}
	}

	if err := m.Reboot(ctx, "loop"); err != nil {
		t.Fatalf("reboot: %v", err)
	}
	b, ok := m.Get("loop")
	if !ok || b.State != vmm.StateRunning {
		t.Fatalf("box not running after reboot: %+v ok=%v", b, ok)
	}
	// The mock observed a fresh Create (snapshot dropped -> resume fails ->
	// recreate = cold boot), not a resume of the old VM.
	calls := rd.recorded()
	if got := indexOf(calls[1:], "create loop"); got == -1 {
		t.Fatalf("no cold-boot Create after reboot: calls %v", calls)
	}
	if indexOf(calls, "drop loop") == -1 {
		t.Fatalf("snapshots not dropped on reboot: calls %v", calls)
	}
	// Cold boot reset the cumulative CPU counter.
	after, ok := m.CPUSeconds(ctx, "loop")
	if !ok {
		t.Fatal("CPUSeconds unavailable after reboot")
	}
	if after >= before {
		t.Fatalf("CPU counter not reset by cold boot: before=%v after=%v", before, after)
	}
}

// rebootRaceDriver widens the window between Reboot's pause and its snapshot
// drop: the first Pause cues the test's competing EnsureRunning (resume chan),
// and DropSnapshots parks before delegating, so a resume-on-connect that is
// allowed to slip between the two reliably does — the auto-reconnect race
// Reboot must tolerate. With the drop serialized under the manager lock, the
// competing resume instead lands strictly before or after it.
type rebootRaceDriver struct {
	*mock.Driver
	once   sync.Once
	resume chan struct{}
}

func (d *rebootRaceDriver) Pause(ctx context.Context, name string) error {
	if err := d.Driver.Pause(ctx, name); err != nil {
		return err
	}
	d.once.Do(func() { close(d.resume) })
	return nil
}

func (d *rebootRaceDriver) DropSnapshots(name string) error {
	time.Sleep(50 * time.Millisecond)
	return d.Driver.DropSnapshots(name)
}

func TestRebootToleratesConcurrentResume(t *testing.T) {
	var sd *rebootRaceDriver
	m := newTestManagerWith(t, host.Options{}, func(d *mock.Driver) vmm.Driver {
		sd = &rebootRaceDriver{Driver: d, resume: make(chan struct{})}
		return sd
	})
	ctx := context.Background()
	mustCreate(t, m, "busy", "alice", 512)

	// A client reconnects the instant the reboot's pause kills its session:
	// its EnsureRunning resumes the box before Reboot gets to the snapshot
	// drop. Reboot must re-pause and finish rather than fail with the
	// driver's "vm is running; pause it first".
	done := make(chan error, 1)
	go func() {
		<-sd.resume
		_, err := m.EnsureReady(ctx, "busy")
		done <- err
	}()
	if err := m.Reboot(ctx, "busy"); err != nil {
		t.Fatalf("reboot with concurrent resume: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("concurrent resume: %v", err)
	}
	if b, _ := m.Get("busy"); b.State != vmm.StateRunning {
		t.Fatalf("state = %q after reboot, want running", b.State)
	}
}

func TestCPUSecondsUnavailableWhenPaused(t *testing.T) {
	m := newTestManager(t, host.Options{})
	ctx := context.Background()
	mustCreate(t, m, "nap", "alice", 512)
	if err := m.Pause(ctx, "nap"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.CPUSeconds(ctx, "nap"); ok {
		t.Fatal("CPUSeconds ok on a paused box")
	}
	if _, ok := m.CPUSeconds(ctx, "ghost"); ok {
		t.Fatal("CPUSeconds ok on a missing box")
	}
}

// bareDriver hides the mock's optional capabilities so the "not enabled"
// refusals are exercisable.
type bareDriver struct{ d *mock.Driver }

func (b bareDriver) Create(ctx context.Context, cfg vmm.Config) (*vmm.Instance, error) {
	return b.d.Create(ctx, cfg)
}
func (b bareDriver) Pause(ctx context.Context, name string) error { return b.d.Pause(ctx, name) }
func (b bareDriver) Resume(ctx context.Context, name string) (*vmm.Instance, error) {
	return b.d.Resume(ctx, name)
}
func (b bareDriver) Destroy(ctx context.Context, name string) error { return b.d.Destroy(ctx, name) }
func (b bareDriver) Close() error                                   { return b.d.Close() }

func TestRebootRenameNotEnabled(t *testing.T) {
	m := newTestManagerWith(t, host.Options{}, func(d *mock.Driver) vmm.Driver {
		return bareDriver{d: d}
	})
	ctx := context.Background()
	mustCreate(t, m, "plain", "alice", 512)

	if err := m.Reboot(ctx, "plain"); err == nil || !contains(err.Error(), "not enabled") {
		t.Fatalf("reboot without capability: want not-enabled error, got %v", err)
	}
	if err := m.Rename(ctx, "plain", "fancy", "alice"); err == nil || !contains(err.Error(), "not enabled") {
		t.Fatalf("rename without capability: want not-enabled error, got %v", err)
	}
	if _, ok := m.CPUSeconds(ctx, "plain"); ok {
		t.Fatal("CPUSeconds ok without capability")
	}
}

// envRecorder implements host.EnvPusher, recording which boxes were pushed.
type envRecorder struct {
	mu     sync.Mutex
	pushes []string
	err    error
}

func (p *envRecorder) PushEnv(_ context.Context, box *host.Sandbox) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushes = append(p.pushes, box.Name)
	return p.err
}

func (p *envRecorder) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.pushes {
		if s == name {
			n++
		}
	}
	return n
}

func TestEnvPushHook(t *testing.T) {
	m := newTestManager(t, host.Options{})
	pusher := &envRecorder{}
	m.SetEnvSync(pusher)
	ctx := context.Background()

	// Fires (asynchronously) after a create...
	mustCreate(t, m, "envy", "alice", 512)
	waitFor(t, func() bool { return pusher.count("envy") == 1 })

	// ...and again each time the box returns to running.
	if err := m.Pause(ctx, "envy"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureReady(ctx, "envy"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return pusher.count("envy") == 2 })

	// An already-running box gets no redundant push from EnsureRunning.
	if _, err := m.EnsureReady(ctx, "envy"); err != nil {
		t.Fatal(err)
	}
	if got := pusher.count("envy"); got != 2 {
		t.Fatalf("push count after no-op EnsureRunning = %d, want 2", got)
	}

	// A fork is a create: the forked rootfs carries the template's managed
	// block, and the create-time push is what rewrites it.
	if _, err := m.Snapshot(ctx, "envy", "base", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Fork(ctx, "base", "forked", "alice", 0, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return pusher.count("forked") == 1 })
}

// TestAwaitEnvIsSynchronous pins the one property the interactive doors need
// and the fire-and-forget hook cannot give them: when it returns, the guest
// already has the environment.
//
// Every test above has to poll (waitFor), because a fired push is a goroutine.
// This one asserts on the next line, and that difference is the whole fix: a
// shell opened one second before its secrets landed never saw them, because
// pam_env reads /etc/environment exactly once, at session setup.
func TestAwaitEnvIsSynchronous(t *testing.T) {
	m := newTestManager(t, host.Options{})
	pusher := &envRecorder{}
	m.SetEnvSync(pusher)
	ctx := context.Background()

	mustCreate(t, m, "envy", "alice", 512)
	waitFor(t, func() bool { return pusher.count("envy") == 1 }) // drain the create's own push

	if err := m.AwaitEnv(ctx, "envy"); err != nil {
		t.Fatalf("AwaitEnv: %v", err)
	}
	if got := pusher.count("envy"); got != 2 {
		t.Fatalf("push count = %d on the line after AwaitEnv, want 2: the delivery has to have finished before the call returned", got)
	}

	// A door that waited is owed the news that the wait failed — unlike the
	// lifecycle hook, whose failures are logged and dropped so no VM operation
	// ever fails over an SSH exec.
	bad := &envRecorder{err: errors.New("guest unreachable")}
	m.SetEnvSync(bad)
	if err := m.AwaitEnv(ctx, "envy"); err == nil {
		t.Fatal("AwaitEnv swallowed a delivery failure")
	}
	m.SetEnvSync(pusher)

	// And it is never a wake-up source: there is nothing to write into a paused
	// rootfs's running environment, and the next resume pushes anyway.
	if err := m.Pause(ctx, "envy"); err != nil {
		t.Fatal(err)
	}
	before := pusher.count("envy")
	if err := m.AwaitEnv(ctx, "envy"); err != nil {
		t.Fatalf("AwaitEnv on a paused sandbox: %v", err)
	}
	if got := pusher.count("envy"); got != before {
		t.Fatalf("AwaitEnv pushed into a paused sandbox (%d -> %d)", before, got)
	}
}

func TestEnvPushFailureNeverFailsLifecycle(t *testing.T) {
	m := newTestManager(t, host.Options{})
	pusher := &envRecorder{err: errors.New("guest unreachable")}
	m.SetEnvSync(pusher)
	ctx := context.Background()

	mustCreate(t, m, "flaky", "alice", 512)
	if err := m.Pause(ctx, "flaky"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureReady(ctx, "flaky"); err != nil {
		t.Fatalf("EnsureRunning failed over a push error: %v", err)
	}
	waitFor(t, func() bool { return pusher.count("flaky") == 2 })
}
