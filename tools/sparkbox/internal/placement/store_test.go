package placement

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReserveAndGet(t *testing.T) {
	s := openTemp(t)
	if err := s.Reserve("myvm", "alice", "nodeb", "ubuntu", "arm64"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get("myvm")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Owner != "alice" || got.Node != "nodeb" || got.Image != "ubuntu" || got.Arch != "arm64" {
		t.Fatalf("unexpected row: %+v", got)
	}
	if got.State != StateOK {
		t.Fatalf("fresh row should start in StateOK, got %q", got.State)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not stamped: %+v", got)
	}

	if _, ok, err := s.Get("ghost"); ok || err != nil {
		t.Fatalf("Get(ghost) = ok %v, err %v; want false, nil", ok, err)
	}
}

func TestReserveRequiresNameOwnerNode(t *testing.T) {
	s := openTemp(t)
	cases := [][3]string{
		{"", "alice", "nodeb"},
		{"myvm", "", "nodeb"},
		{"myvm", "alice", ""},
	}
	for _, c := range cases {
		if err := s.Reserve(c[0], c[1], c[2], "", ""); err == nil {
			t.Fatalf("Reserve%v: expected a validation error", c)
		}
	}
}

// TestReserveIsAtomic is the whole point of the PRIMARY KEY: the allocator has
// no read-then-write window, so exactly one racer may win a name.
func TestReserveIsAtomic(t *testing.T) {
	s := openTemp(t)
	const racers = 64

	var wg sync.WaitGroup
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = s.Reserve("contested", "owner"+strconv.Itoa(i), "node"+strconv.Itoa(i), "", "")
		}()
	}
	close(start)
	wg.Wait()

	var won, taken int
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrTaken):
			taken++
		default:
			t.Fatalf("racer %d: unexpected error %v", i, err)
		}
	}
	if won != 1 || taken != racers-1 {
		t.Fatalf("won=%d taken=%d; want 1 and %d", won, taken, racers-1)
	}
	if _, ok, _ := s.Get("contested"); !ok {
		t.Fatal("the winning reservation left no row")
	}
}

func TestReleaseThenReserve(t *testing.T) {
	s := openTemp(t)
	if err := s.Reserve("myvm", "alice", "nodea", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve("myvm", "bob", "nodeb", "", ""); !errors.Is(err, ErrTaken) {
		t.Fatalf("second Reserve = %v; want ErrTaken", err)
	}
	if err := s.Release("myvm"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("myvm"); ok {
		t.Fatal("row survived Release")
	}
	// Releasing again is the same end state, so it must not be an error.
	if err := s.Release("myvm"); err != nil {
		t.Fatalf("second Release = %v; want nil", err)
	}
	if err := s.Reserve("myvm", "bob", "nodeb", "", ""); err != nil {
		t.Fatalf("Reserve after Release = %v; want nil", err)
	}
	got, _, _ := s.Get("myvm")
	if got.Owner != "bob" || got.Node != "nodeb" {
		t.Fatalf("released name kept its old placement: %+v", got)
	}
}

func TestRename(t *testing.T) {
	s := openTemp(t)
	if err := s.Reserve("old", "alice", "nodeb", "ubuntu", "arm64"); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.Get("old")

	if err := s.Rename("old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("old"); ok {
		t.Fatal("the old name was not released")
	}
	got, ok, _ := s.Get("new")
	if !ok {
		t.Fatal("no row under the new name")
	}
	if got.Owner != "alice" || got.Node != "nodeb" || got.Image != "ubuntu" || got.Arch != "arm64" {
		t.Fatalf("rename lost placement fields: %+v", got)
	}
	if !got.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("rename reset CreatedAt: %v -> %v", before.CreatedAt, got.CreatedAt)
	}

	// The freed name is allocatable again.
	if err := s.Reserve("old", "bob", "nodec", "", ""); err != nil {
		t.Fatalf("Reserve of the freed name = %v; want nil", err)
	}
	if err := s.Rename("ghost", "somewhere"); !errors.Is(err, ErrNoSuchRow) {
		t.Fatalf("Rename of a missing row = %v; want ErrNoSuchRow", err)
	}
}

func TestRenameRefusesTakenTarget(t *testing.T) {
	s := openTemp(t)
	if err := s.Reserve("old", "alice", "nodea", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve("taken", "bob", "nodeb", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("old", "taken"); !errors.Is(err, ErrTaken) {
		t.Fatalf("Rename onto a taken name = %v; want ErrTaken", err)
	}
	// A refused rename must cost neither row: the caller still owns the name
	// it started with.
	old, ok, _ := s.Get("old")
	if !ok || old.Owner != "alice" || old.Node != "nodea" {
		t.Fatalf("source row damaged: ok=%v row=%+v", ok, old)
	}
	taken, ok, _ := s.Get("taken")
	if !ok || taken.Owner != "bob" || taken.Node != "nodeb" {
		t.Fatalf("target row damaged: ok=%v row=%+v", ok, taken)
	}
}

func TestSetNodeAndState(t *testing.T) {
	s := openTemp(t)
	if err := s.Reserve("myvm", "alice", "nodea", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNode("myvm", "nodeb"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRowState("myvm", StateOrphaned); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get("myvm")
	if got.Node != "nodeb" || got.State != StateOrphaned {
		t.Fatalf("unexpected row: %+v", got)
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Fatalf("UpdatedAt went backwards: %+v", got)
	}
	if err := s.SetRowState("myvm", StateOK); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Get("myvm"); got.State != StateOK {
		t.Fatalf("state not cleared: %+v", got)
	}

	if err := s.SetNode("ghost", "nodeb"); !errors.Is(err, ErrNoSuchRow) {
		t.Fatalf("SetNode on a missing row = %v; want ErrNoSuchRow", err)
	}
	if err := s.SetRowState("ghost", StateQuarantine); !errors.Is(err, ErrNoSuchRow) {
		t.Fatalf("SetRowState on a missing row = %v; want ErrNoSuchRow", err)
	}
	if err := s.SetNode("myvm", ""); err == nil {
		t.Fatal("SetNode with no node should be a validation error")
	}
}

func TestListing(t *testing.T) {
	s := openTemp(t)
	seed := []Row{
		{Name: "b", Owner: "alice", Node: "n1"},
		{Name: "a", Owner: "alice", Node: "n2"},
		{Name: "c", Owner: "bob", Node: "n1"},
	}
	for _, r := range seed {
		if err := s.Reserve(r.Name, r.Owner, r.Node, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	names := func(rows []Row) []string {
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.Name
		}
		return out
	}
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(all); len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("List = %v; want sorted a b c", got)
	}
	byNode, err := s.ByNode("n1")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(byNode); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("ByNode(n1) = %v; want b c", got)
	}
	byOwner, err := s.ByOwner("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(byOwner); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("ByOwner(alice) = %v; want a b", got)
	}
	if rows, err := s.ByNode("nowhere"); err != nil || len(rows) != 0 {
		t.Fatalf("ByNode(nowhere) = %v, %v; want empty, nil", rows, err)
	}
}

// TestReopenIsANoOp covers the second boot: CREATE TABLE IF NOT EXISTS must not
// fail and must not lose rows.
func TestReopenIsANoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Reserve("myvm", "alice", "nodeb", "ubuntu", "arm64"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	got, ok, err := second.Get("myvm")
	if err != nil || !ok {
		t.Fatalf("row lost across reopen: ok=%v err=%v", ok, err)
	}
	if got.Owner != "alice" || got.Node != "nodeb" || got.Arch != "arm64" {
		t.Fatalf("row changed across reopen: %+v", got)
	}
	if err := second.Reserve("myvm", "bob", "nodec", "", ""); !errors.Is(err, ErrTaken) {
		t.Fatalf("the name allocation did not survive reopen: %v", err)
	}
}

// TestSharesTheDatabase pins the reason this store copies routes' template
// instead of inventing its own: three packages hold their own *sql.DB on one
// sparkbox.db file, and WAL plus _txlock=immediate plus busy_timeout has to
// make concurrent writers wait rather than fail.
func TestSharesTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparkbox.db")

	place, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer place.Close()
	routeStore, err := routes.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer routeStore.Close()
	userStore, err := users.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer userStore.Close()

	const n = 24
	// Keys are minted up front: t.Fatal is only legal on the test's own
	// goroutine.
	keys := make([]xssh.PublicKey, n)
	for i := range keys {
		keys[i] = newKey(t)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 3*n)
	for i := range n {
		name := "vm" + strconv.Itoa(i)
		wg.Add(3)
		go func() {
			defer wg.Done()
			errs <- place.Reserve(name, "alice", "nodeb", "ubuntu", "arm64")
		}()
		go func() {
			defer wg.Done()
			errs <- routeStore.Upsert(routes.Route{Subdomain: name, Sandbox: name, Owner: "alice", Port: routes.DefaultPort})
		}()
		go func() {
			defer wg.Done()
			errs <- userStore.Create("user"+strconv.Itoa(i), keys[i], "laptop", "signup", "operator")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write across stores: %v", err)
		}
	}

	rows, err := place.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != n {
		t.Fatalf("placements = %d; want %d", len(rows), n)
	}
}

func newKey(t *testing.T) xssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := xssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
