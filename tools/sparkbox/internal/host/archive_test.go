package host_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// memStore is an in-memory host.ObjectStore for tests — it just copies bytes
// between local files and a map, so archive/restore can round-trip with no
// rclone, network, or object storage.
type memStore struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func newMemStore() *memStore { return &memStore{objs: map[string][]byte{}} }

func (s *memStore) Put(_ context.Context, key, localPath string) error {
	b, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = b
	return nil
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

// managerWithDir wires a manager on the mock driver but returns the state dir
// too, so a test can poke the mock's per-VM workdir (its "disk").
func managerWithDir(t *testing.T, opts host.Options) (*host.Manager, string) {
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
	return mgr, dir
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
	if _, err := m.EnsureRunning(ctx, "a1"); err != nil {
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
