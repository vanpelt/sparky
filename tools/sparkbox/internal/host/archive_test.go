package host_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/envsync"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// memStore is an in-memory host.ObjectStore for tests — it just copies bytes
// between local files and a map, so archive/restore can round-trip with no
// rclone, network, or object storage.
type memStore struct {
	mu     sync.Mutex
	objs   map[string][]byte
	putErr error
}

type blockingStore struct {
	*memStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingStore) Put(ctx context.Context, key, localPath string) error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.memStore.Put(ctx, key, localPath)
}

func newMemStore() *memStore { return &memStore{objs: map[string][]byte{}} }

func (s *memStore) Put(_ context.Context, key, localPath string) error {
	s.mu.Lock()
	putErr := s.putErr
	s.mu.Unlock()
	if putErr != nil {
		return putErr
	}
	b, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = b
	return nil
}

func (s *memStore) failPuts(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putErr = err
}

func (s *memStore) Get(_ context.Context, key, localPath string) error {
	s.mu.Lock()
	b, ok := s.objs[key]
	s.mu.Unlock()
	if !ok {
		return errors.New("not found: " + key)
	}
	return os.WriteFile(localPath, b, 0o600)
}

func (s *memStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objs, key)
	return nil
}

func (s *memStore) Exists(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objs[key]
	return ok, nil
}

func (s *memStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objs)
}

func (s *memStore) object(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objs[key]
}

// managerWithDir wires a manager on the mock driver but returns the state dir
// too, so a test can poke the mock's per-VM workdir (its "disk").
func managerWithDir(t *testing.T, opts host.Options) (*host.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	return newManagerInDir(t, dir, opts), dir
}

func workdirFile(dir, box, rel string) string {
	return filepath.Join(dir, "mock-vms", box, rel)
}

func TestArchiveRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, dir := managerWithDir(t, host.Options{Archive: store})

	if _, err := m.Create(ctx, "a1", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	// Drop a marker into the VM's "disk" so we can prove it survives the trip.
	marker := workdirFile(dir, "a1", "notes.txt")
	if err := os.WriteFile(marker, []byte("hello archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.Archive(ctx, "a1"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	b, _ := m.Get("a1")
	if b.State != vmm.StateArchived {
		t.Fatalf("state = %q, want archived", b.State)
	}
	if b.ArchiveKey == "" {
		t.Fatal("archived box has no ArchiveKey")
	}
	if store.len() != 1 {
		t.Fatalf("object store has %d objects, want 1", store.len())
	}
	// Local disk was reclaimed: the mock destroys the workdir at archive time.
	if _, err := os.Stat(filepath.Dir(marker)); !os.IsNotExist(err) {
		t.Fatalf("local workdir still present after archive: %v", err)
	}

	// Resume-on-connect restores transparently.
	if _, err := m.EnsureReady(ctx, "a1"); err != nil {
		t.Fatalf("restore/resume: %v", err)
	}
	b, _ = m.Get("a1")
	if b.State != vmm.StateRunning {
		t.Fatalf("state = %q, want running after restore", b.State)
	}
	if b.ArchiveKey != "" {
		t.Fatal("ArchiveKey should be cleared after restore")
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker missing after restore: %v", err)
	}
	if string(got) != "hello archive" {
		t.Fatalf("marker = %q, want %q", got, "hello archive")
	}
	// Restore consumed the archive (a move, not a copy).
	if store.len() != 0 {
		t.Fatalf("object store still has %d objects after restore", store.len())
	}
}

func TestCheckpointRestoreRoundTripKeepsLocalAndDurableCopies(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, dir := managerWithDir(t, host.Options{Checkpoint: store})
	if _, err := m.Create(ctx, "c1", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	marker := workdirFile(dir, "c1", "notes.txt")
	if err := os.WriteFile(marker, []byte("checkpoint one"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.Checkpoint(ctx, "c1"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	box, _ := m.Get("c1")
	if box.CheckpointKey == "" || box.CheckpointAt.IsZero() {
		t.Fatalf("checkpoint metadata not committed: %+v", box)
	}
	if box.State != vmm.StateRunning {
		t.Fatalf("state after checkpoint = %q, want running", box.State)
	}
	if store.len() != 1 {
		t.Fatalf("durable objects = %d, want 1", store.len())
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "checkpoint one" {
		t.Fatalf("local disk was not retained: %q, %v", got, err)
	}

	if err := os.WriteFile(marker, []byte("new local writes"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := box.CheckpointKey
	if err := m.RestoreCheckpoint(ctx, "c1"); err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "checkpoint one" {
		t.Fatalf("restored marker = %q, %v", got, err)
	}
	box, _ = m.Get("c1")
	if box.CheckpointKey != key || store.len() != 1 {
		t.Fatalf("restore consumed durable checkpoint: key=%q objects=%d", box.CheckpointKey, store.len())
	}
}

func TestFailedSecondCheckpointPreservesPriorPointer(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, dir := managerWithDir(t, host.Options{Checkpoint: store})
	if _, err := m.Create(ctx, "c2", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	marker := workdirFile(dir, "c2", "notes.txt")
	if err := os.WriteFile(marker, []byte("committed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Checkpoint(ctx, "c2"); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Get("c2")

	if err := os.WriteFile(marker, []byte("not committed"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.failPuts(errors.New("VAST unavailable"))
	if err := m.Checkpoint(ctx, "c2"); err == nil {
		t.Fatal("second checkpoint should fail")
	}
	after, _ := m.Get("c2")
	if after.CheckpointKey != before.CheckpointKey || !after.CheckpointAt.Equal(before.CheckpointAt) {
		t.Fatalf("failed checkpoint moved pointer: before=%+v after=%+v", before, after)
	}
	if store.len() != 1 {
		t.Fatalf("failed checkpoint changed durable objects: %d", store.len())
	}

	store.failPuts(nil)
	if err := m.RestoreCheckpoint(ctx, "c2"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "committed" {
		t.Fatalf("prior checkpoint unusable after failed second attempt: %q, %v", got, err)
	}
}

func TestSuccessfulCheckpointsUseImmutableGenerationKeys(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, _ := managerWithDir(t, host.Options{Checkpoint: store})
	if _, err := m.Create(ctx, "generations", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := m.Checkpoint(ctx, "generations"); err != nil {
		t.Fatal(err)
	}
	first, _ := m.Get("generations")
	if err := m.Checkpoint(ctx, "generations"); err != nil {
		t.Fatal(err)
	}
	second, _ := m.Get("generations")
	if first.CheckpointKey == second.CheckpointKey {
		t.Fatalf("second checkpoint reused immutable key %q", first.CheckpointKey)
	}
	if store.len() != 2 {
		t.Fatalf("durable generations = %d, want 2", store.len())
	}
}

func TestMissingCheckpointedHotDiskRequiresExplicitRestore(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, dir := managerWithDir(t, host.Options{Checkpoint: store})
	if _, err := m.Create(ctx, "c3", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	marker := workdirFile(dir, "c3", "notes.txt")
	if err := os.WriteFile(marker, []byte("recover me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Checkpoint(ctx, "c3"); err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(ctx, "c3"); err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Dir(marker)
	if err := os.RemoveAll(workdir); err != nil {
		t.Fatal(err)
	}

	_, err := m.EnsureReady(ctx, "c3")
	var stateErr *host.StateError
	if !errors.As(err, &stateErr) || stateErr.Code != "checkpoint_restore_required" {
		t.Fatalf("EnsureReady missing hot disk: got %v, want checkpoint_restore_required", err)
	}
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Fatalf("EnsureReady silently recreated a base-image disk: %v", err)
	}

	if err := m.RestoreCheckpoint(ctx, "c3"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "recover me" {
		t.Fatalf("explicit restore did not recover marker: %q, %v", got, err)
	}
}

func TestMissingHotDiskWithoutCheckpointIsUnrecoverable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m := newManagerInDir(t, dir, host.Options{})
	if _, err := m.Create(ctx, "lost", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(ctx, "lost"); err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(dir, "mock-vms", "lost")
	if err := os.RemoveAll(workdir); err != nil {
		t.Fatal(err)
	}

	// A fresh manager models the host process restarting after the persisted
	// sandbox record outlived its local disk.
	restarted := newManagerInDir(t, dir, host.Options{})
	_, err := restarted.EnsureReady(ctx, "lost")
	var stateErr *host.StateError
	if !errors.As(err, &stateErr) || stateErr.Code != "sandbox_unrecoverable" {
		t.Fatalf("EnsureReady missing hot disk: got %v, want sandbox_unrecoverable", err)
	}
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Fatalf("EnsureReady silently recreated a base-image disk: %v", err)
	}
}

func TestCheckpointSerializesResumeUntilDiskWorkFinishes(t *testing.T) {
	ctx := context.Background()
	store := &blockingStore{
		memStore: newMemStore(),
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	m, _ := managerWithDir(t, host.Options{Checkpoint: store})
	if _, err := m.Create(ctx, "c4", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	checkpointDone := make(chan error, 1)
	go func() { checkpointDone <- m.Checkpoint(ctx, "c4") }()
	<-store.started

	resumeDone := make(chan error, 1)
	go func() {
		_, err := m.EnsureReady(ctx, "c4")
		resumeDone <- err
	}()
	select {
	case err := <-resumeDone:
		close(store.release)
		<-checkpointDone
		t.Fatalf("resume crossed active checkpoint disk work: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(store.release)
	if err := <-checkpointDone; err != nil {
		t.Fatal(err)
	}
	if err := <-resumeDone; err != nil {
		t.Fatal(err)
	}
}

func TestArchiveDisabledWithoutStore(t *testing.T) {
	m := newTestManager(t, host.Options{})
	mustCreate(t, m, "x1", "alice", 512)
	if err := m.Archive(context.Background(), "x1"); err == nil {
		t.Fatal("archive without a store should error")
	}
	if m.ArchivingEnabled() {
		t.Fatal("ArchivingEnabled should be false without a store")
	}
}

func TestDestroyArchivedDropsObject(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, _ := managerWithDir(t, host.Options{Archive: store})
	if _, err := m.Create(ctx, "a1", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := m.Archive(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if store.len() != 1 {
		t.Fatalf("want 1 object, got %d", store.len())
	}
	if err := m.Destroy(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if store.len() != 0 {
		t.Fatalf("destroy should have dropped the archive; %d objects remain", store.len())
	}
}

func TestSnapshotFork(t *testing.T) {
	ctx := context.Background()
	m, dir := managerWithDir(t, host.Options{})

	if _, err := m.Create(ctx, "base", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	// Customize the VM: this file should appear in every fork.
	if err := os.WriteFile(workdirFile(dir, "base", "custom.txt"), []byte("customized"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(ctx, "base", "golden", "alice")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Owner != "alice" || snap.FromBox != "base" {
		t.Fatalf("unexpected snapshot record: %+v", snap)
	}
	if got := m.Snapshots("alice"); len(got) != 1 {
		t.Fatalf("alice should have 1 snapshot, got %d", len(got))
	}
	// bob can't see or fork alice's snapshot.
	if _, err := m.Fork(ctx, "golden", "bobfork", "bob", 0, 0); err == nil {
		t.Fatal("bob forking alice's snapshot should fail")
	}

	if _, err := m.Fork(ctx, "golden", "clone", "alice", 0, 0); err != nil {
		t.Fatalf("fork: %v", err)
	}
	got, err := os.ReadFile(workdirFile(dir, "clone", "custom.txt"))
	if err != nil {
		t.Fatalf("fork missing customization: %v", err)
	}
	if string(got) != "customized" {
		t.Fatalf("fork content = %q, want %q", got, "customized")
	}

	// Deleting the snapshot leaves the fork intact.
	if err := m.DeleteSnapshot(ctx, "golden", "alice"); err != nil {
		t.Fatalf("delete snapshot: %v", err)
	}
	if got := m.Snapshots("alice"); len(got) != 0 {
		t.Fatalf("snapshot should be gone, got %d", len(got))
	}
	if _, err := os.Stat(workdirFile(dir, "clone", "custom.txt")); err != nil {
		t.Fatalf("fork should survive snapshot delete: %v", err)
	}
}

func TestDiskPoolAdmission(t *testing.T) {
	ctx := context.Background()
	// 4 MB pool per owner.
	m, dir := managerWithDir(t, host.Options{DiskPoolMBPerOwner: 4})

	if _, err := m.Create(ctx, "d1", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	// Fill d1's "disk" past the pool (6 MB), then refresh accounting.
	big := make([]byte, 6*1024*1024)
	if err := os.WriteFile(workdirFile(dir, "d1", "blob.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	m.RefreshDiskUsage(ctx)
	if b, _ := m.Get("d1"); b.DiskMB < 6 {
		t.Fatalf("d1 DiskMB = %d, want >= 6", b.DiskMB)
	}

	// A second sandbox for alice is now refused on the pooled-disk dimension.
	_, err := m.Create(ctx, "d2", "alice", "ubuntu", 1, 512)
	var q *host.DiskQuotaError
	if !errors.As(err, &q) {
		t.Fatalf("want *DiskQuotaError, got %v", err)
	}
	if q.Owner != "alice" || q.PoolMB != 4 {
		t.Fatalf("unexpected DiskQuotaError: %+v", q)
	}
	// The pool is per-owner: bob is unaffected.
	if _, err := m.Create(ctx, "b1", "bob", "ubuntu", 1, 512); err != nil {
		t.Fatalf("bob should be admitted: %v", err)
	}
}

// envFileRel is the stub syncer's stand-in for /etc/environment inside the
// mock VM's workdir ("rootfs").
const envFileRel = "etc-environment"

// bakedLine stands in for the toolchain PATH the image bakes outside the
// managed block; it must survive both push and strip.
const bakedLine = "PATH=/usr/local/bin\n"

const plaintextSecret = `HUSH="s3kr3t-plaintext"`

// stripSyncer is a stub envsync syncer: PushEnv writes a managed block holding
// a recognizable secret into the mock VM's workdir env file and StripEnv
// rewrites the block empty — the same file shapes the real Syncer produces
// over SSH. Both record their calls so tests can assert ordering and the box
// state they were handed.
type stripSyncer struct {
	dir string // manager state dir; mock workdirs live under it

	mu       sync.Mutex
	pushes   []string
	strips   []string
	stripErr error
	stripBox *host.Sandbox // copy handed to the most recent StripEnv
}

func (s *stripSyncer) envPath(box string) string { return workdirFile(s.dir, box, envFileRel) }

func (s *stripSyncer) PushEnv(_ context.Context, box *host.Sandbox) error {
	if box.State != vmm.StateRunning {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushes = append(s.pushes, box.Name)
	block := envsync.BlockBegin + "\n" + plaintextSecret + "\n" + envsync.BlockEnd + "\n"
	return os.WriteFile(s.envPath(box.Name), []byte(bakedLine+block), 0o644)
}

func (s *stripSyncer) StripEnv(_ context.Context, box *host.Sandbox) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strips = append(s.strips, box.Name)
	s.stripBox = box
	if s.stripErr != nil {
		return s.stripErr
	}
	block := envsync.BlockBegin + "\n" + envsync.BlockEnd + "\n"
	return os.WriteFile(s.envPath(box.Name), []byte(bakedLine+block), 0o644)
}

func (s *stripSyncer) pushCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.pushes {
		if b == name {
			n++
		}
	}
	return n
}

func (s *stripSyncer) stripCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.strips {
		if b == name {
			n++
		}
	}
	return n
}

func (s *stripSyncer) lastStripBox() *host.Sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stripBox
}

func TestArchiveStripsManagedEnvBlock(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, dir := managerWithDir(t, host.Options{Archive: store})
	syncer := &stripSyncer{dir: dir}
	m.SetEnvSync(syncer)

	if _, err := m.Create(ctx, "a1", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return syncer.pushCount("a1") == 1 })

	if err := m.Archive(ctx, "a1"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if got := syncer.stripCount("a1"); got != 1 {
		t.Fatalf("strip count = %d, want 1", got)
	}
	b, _ := m.Get("a1")
	obj := store.object(b.ArchiveKey)
	if len(obj) == 0 {
		t.Fatal("no archive object uploaded")
	}
	if bytes.Contains(obj, []byte(plaintextSecret)) {
		t.Fatal("packed archive still contains the plaintext secret")
	}
	if !bytes.Contains(obj, []byte(bakedLine)) {
		t.Fatal("strip clobbered content outside the managed block")
	}

	// Restore re-pushes via the EnsureRunning hook: the env comes back.
	if _, err := m.EnsureReady(ctx, "a1"); err != nil {
		t.Fatalf("restore/resume: %v", err)
	}
	waitFor(t, func() bool { return syncer.pushCount("a1") == 2 })
	got, err := os.ReadFile(syncer.envPath("a1"))
	if err != nil {
		t.Fatalf("env file missing after restore: %v", err)
	}
	if !bytes.Contains(got, []byte(plaintextSecret)) {
		t.Fatal("env block not restored after resume")
	}
}

func TestArchiveWakesPausedBoxToStrip(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, dir := managerWithDir(t, host.Options{Archive: store})
	syncer := &stripSyncer{dir: dir}
	m.SetEnvSync(syncer)

	if _, err := m.Create(ctx, "p1", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return syncer.pushCount("p1") == 1 })
	if err := m.Pause(ctx, "p1"); err != nil {
		t.Fatal(err)
	}

	// Archiving a paused box still strips: the guest is woken just long enough.
	if err := m.Archive(ctx, "p1"); err != nil {
		t.Fatalf("archive paused box: %v", err)
	}
	if got := syncer.stripCount("p1"); got != 1 {
		t.Fatalf("strip count = %d, want 1", got)
	}
	sb := syncer.lastStripBox()
	if sb.State != vmm.StateRunning || sb.SSHAddr == "" {
		t.Fatalf("strip saw state %q addr %q, want a reachable running box", sb.State, sb.SSHAddr)
	}
	// The wake was strip-scoped, never an EnsureRunning: no extra push fired.
	if got := syncer.pushCount("p1"); got != 1 {
		t.Fatalf("push count after wake-to-strip = %d, want 1", got)
	}
	b, _ := m.Get("p1")
	if b.State != vmm.StateArchived {
		t.Fatalf("state = %q, want archived", b.State)
	}
	if obj := store.object(b.ArchiveKey); bytes.Contains(obj, []byte(plaintextSecret)) {
		t.Fatal("packed archive of a paused box still contains the plaintext secret")
	}
}

func TestArchiveAbortsWhenStripFails(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, dir := managerWithDir(t, host.Options{Archive: store})
	syncer := &stripSyncer{dir: dir, stripErr: errors.New("guest wedged")}
	m.SetEnvSync(syncer)

	if _, err := m.Create(ctx, "w1", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return syncer.pushCount("w1") == 1 })

	err := m.Archive(ctx, "w1")
	if err == nil || !contains(err.Error(), "strip") {
		t.Fatalf("archive with failing strip: want strip error, got %v", err)
	}
	if store.len() != 0 {
		t.Fatalf("failed strip must not upload; store has %d objects", store.len())
	}
	// The box was running on entry, so the aborted archive leaves it running.
	if b, _ := m.Get("w1"); b.State != vmm.StateRunning {
		t.Fatalf("state = %q after aborted archive of a running box, want running", b.State)
	}
}

// A box the strip itself woke must not be left running when the strip fails:
// the caller only pauses after a successful strip, so the failure path has to
// undo its own wake.
func TestFailedStripRepausesWokenBox(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, dir := managerWithDir(t, host.Options{Archive: store})
	syncer := &stripSyncer{dir: dir, stripErr: errors.New("guest wedged")}
	m.SetEnvSync(syncer)

	if _, err := m.Create(ctx, "w2", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return syncer.pushCount("w2") == 1 })
	if err := m.Pause(ctx, "w2"); err != nil {
		t.Fatal(err)
	}

	if err := m.Archive(ctx, "w2"); err == nil {
		t.Fatal("archive with failing strip should error")
	}
	if b, _ := m.Get("w2"); b.State != vmm.StatePaused {
		t.Fatalf("state = %q after failed strip of a paused box, want paused", b.State)
	}
	if store.len() != 0 {
		t.Fatalf("failed strip must not upload; store has %d objects", store.len())
	}
}

// The strip-time wake must restart the idle clock: a box being archived is by
// definition long-idle, and a stale LastActive would let a reaper tick pause
// it again mid-strip.
func TestStripWakeBumpsLastActive(t *testing.T) {
	ctx := context.Background()
	m, dir := managerWithDir(t, host.Options{Archive: newMemStore()})
	syncer := &stripSyncer{dir: dir}
	m.SetEnvSync(syncer)

	if _, err := m.Create(ctx, "p2", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return syncer.pushCount("p2") == 1 })
	if err := m.Pause(ctx, "p2"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	t0 := time.Now()

	if err := m.Archive(ctx, "p2"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	sb := syncer.lastStripBox()
	if !sb.LastActive.After(t0) {
		t.Fatalf("strip saw LastActive %v, want after the wake at %v", sb.LastActive, t0)
	}
}

func TestSnapshotStripsManagedEnvBlock(t *testing.T) {
	ctx := context.Background()
	m, dir := managerWithDir(t, host.Options{})
	syncer := &stripSyncer{dir: dir}
	m.SetEnvSync(syncer)

	if _, err := m.Create(ctx, "base", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return syncer.pushCount("base") == 1 })

	if _, err := m.Snapshot(ctx, "base", "golden", "alice"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := syncer.stripCount("base"); got != 1 {
		t.Fatalf("strip count = %d, want 1", got)
	}
	tpl, err := os.ReadFile(filepath.Join(dir, "mock-templates", "snap-alice-golden", envFileRel))
	if err != nil {
		t.Fatalf("template env file: %v", err)
	}
	if bytes.Contains(tpl, []byte(plaintextSecret)) {
		t.Fatal("snapshot template still contains the plaintext secret")
	}
	if !bytes.Contains(tpl, []byte(bakedLine)) {
		t.Fatal("strip clobbered content outside the managed block")
	}

	// A fork boots from the stripped template and gets its own create-time push.
	if _, err := m.Fork(ctx, "golden", "clone", "alice", 0, 0); err != nil {
		t.Fatalf("fork: %v", err)
	}
	waitFor(t, func() bool { return syncer.pushCount("clone") == 1 })
	got, err := os.ReadFile(syncer.envPath("clone"))
	if err != nil {
		t.Fatalf("fork env file: %v", err)
	}
	if !bytes.Contains(got, []byte(plaintextSecret)) {
		t.Fatal("fork's env block not rewritten by the create-time push")
	}
}

// A pusher without the StripEnv capability must not block archiving — the
// strip is an optional extension detected by type assertion.
func TestArchiveWithoutStripperProceeds(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, _ := managerWithDir(t, host.Options{Archive: store})
	pusher := &envRecorder{}
	m.SetEnvSync(pusher)

	if _, err := m.Create(ctx, "n1", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return pusher.count("n1") == 1 })
	if err := m.Archive(ctx, "n1"); err != nil {
		t.Fatalf("archive with plain EnvPusher: %v", err)
	}
	if store.len() != 1 {
		t.Fatalf("object store has %d objects, want 1", store.len())
	}
}

// TestArchivedSandboxPaysFullFreight: archiving replaces DiskMB with the
// compressed artifact's size in object storage, where nothing is deduplicated
// against a local ext4 template. Subtracting a reflink baseline from that figure
// would discount bytes the owner genuinely occupies, so the pooled charge
// short-circuits to raw for an archived box — and a restore, which decompresses
// a whole fresh image, drops the baseline outright.
func TestArchivedSandboxPaysFullFreight(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m, dir := managerWithDir(t, host.Options{Archive: store})

	if _, err := m.Create(ctx, "base", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workdirFile(dir, "base", "tooling.bin"), make([]byte, 6*1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(ctx, "base", "golden", "alice"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := m.Fork(ctx, "golden", "clone", "alice", 0, 0); err != nil {
		t.Fatalf("fork: %v", err)
	}
	m.RefreshDiskUsage(ctx)

	clone, _ := m.Get("clone")
	if clone.BaseDiskMB == 0 {
		t.Fatal("fork has no template baseline; the archive short-circuit would be vacuous")
	}
	if got := m.CapacityForOwner("alice").UsedDiskMB; got != 6 {
		t.Fatalf("pooled disk before archive = %d MB, want 6 (the fork's blocks are shared)", got)
	}

	if err := m.Archive(ctx, "clone"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	archived, _ := m.Get("clone")
	if archived.DiskMB == 0 {
		t.Fatal("archived box recorded no artifact size")
	}
	if archived.BaseDiskMB == 0 {
		t.Fatal("archive cleared the baseline; the short-circuit, not the field, is what must handle this")
	}
	if got, want := m.CapacityForOwner("alice").UsedDiskMB, 6+archived.DiskMB; got != want {
		t.Fatalf("pooled disk after archive = %d MB, want %d — an archive is charged in full", got, want)
	}

	// EnsureReady restores and resumes; UnpackRootfs wrote every block fresh, so
	// the box shares nothing with the template any more.
	if _, err := m.EnsureReady(ctx, "clone"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, _ := m.Get("clone")
	if restored.BaseDiskMB != 0 {
		t.Fatalf("BaseDiskMB = %d immediately after restore, want 0", restored.BaseDiskMB)
	}
}
