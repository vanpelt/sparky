package nodes

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"
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

func TestEnrollIsIdempotentForTheSameKey(t *testing.T) {
	s := openTemp(t)
	key := newKey(t)

	first, err := s.Enroll("node-b", key)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != StatusPending {
		t.Fatalf("fresh enrolment status = %q, want %q", first.Status, StatusPending)
	}
	if first.FP != xssh.FingerprintSHA256(key) {
		t.Errorf("FP = %q, want %q", first.FP, xssh.FingerprintSHA256(key))
	}

	again, err := s.Enroll("node-b", key)
	if err != nil {
		t.Fatalf("re-enrolling the same key: %v", err)
	}
	if again.Name != first.Name || again.Status != StatusPending {
		t.Errorf("re-enrolment = %+v, want the same pending row as %+v", again, first)
	}
	if n, err := s.PendingCount(); err != nil || n != 1 {
		t.Errorf("PendingCount = %d, %v; want 1, nil", n, err)
	}
}

// A reconnecting key gets the roster's name back, not the one it asked for:
// the operator approved a name they read off the roster, so a key cannot
// rename itself past them.
func TestEnrollKeepsTheRosterName(t *testing.T) {
	s := openTemp(t)
	key := newKey(t)
	if _, err := s.Enroll("node-b", key); err != nil {
		t.Fatal(err)
	}
	got, err := s.Enroll("node-c", key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "node-b" {
		t.Errorf("Name = %q, want node-b", got.Name)
	}
	if _, err := s.Get("node-c"); !errors.Is(err, ErrNoSuchNode) {
		t.Errorf("Get(node-c) = %v, want ErrNoSuchNode", err)
	}
}

func TestEnrollRefusesATakenName(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Enroll("node-b", newKey(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll("node-b", newKey(t)); !errors.Is(err, ErrNameTaken) {
		t.Errorf("second key under a taken name: %v, want ErrNameTaken", err)
	}
	// The incumbent is untouched.
	rows, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("List = %d rows, want 1", len(rows))
	}
}

func TestEnrollRejectsBadNames(t *testing.T) {
	s := openTemp(t)
	for _, name := range []string{"", "-lead", "Node", "node_b", "node.b", strings.Repeat("a", 64)} {
		if _, err := s.Enroll(name, newKey(t)); !errors.Is(err, ErrBadName) {
			t.Errorf("Enroll(%q) = %v, want ErrBadName", name, err)
		}
	}
	for _, name := range []string{"a", "0", "node-b", strings.Repeat("a", 63)} {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false, want true", name)
		}
	}
}

func TestEnrolmentIsBounded(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < MaxPending; i++ {
		if _, err := s.Enroll(fmt.Sprintf("node-%d", i), newKey(t)); err != nil {
			t.Fatalf("enrolment %d: %v", i, err)
		}
	}
	if _, err := s.Enroll("one-too-many", newKey(t)); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("enrolment %d: %v, want ErrTooManyPending", MaxPending+1, err)
	}
	// Approving one frees a slot: the ceiling is on the approval queue, not on
	// the fleet.
	if err := s.Approve("node-0", "vanpelt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll("one-too-many", newKey(t)); err != nil {
		t.Fatalf("after an approval freed a slot: %v", err)
	}
}

func TestApproveStampsTheOperator(t *testing.T) {
	s := openTemp(t)
	key := newKey(t)
	if _, err := s.Enroll("node-b", key); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Second)
	if err := s.Approve("node-b", "vanpelt"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("node-b")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusApproved || got.ApprovedBy != "vanpelt" {
		t.Errorf("after Approve: %+v, want approved by vanpelt", got)
	}
	if got.ApprovedAt == nil || got.ApprovedAt.Before(before) {
		t.Errorf("ApprovedAt = %v, want a stamp after %v", got.ApprovedAt, before)
	}
	if err := s.Approve("ghost", "vanpelt"); !errors.Is(err, ErrNoSuchNode) {
		t.Errorf("Approve(ghost) = %v, want ErrNoSuchNode", err)
	}
}

// Lookup answers for every status, because the door has to tell a pending node
// that it is pending and a disabled one that it is disabled — neither of which
// it can do if the store reports "unknown".
func TestLookupIgnoresStatus(t *testing.T) {
	s := openTemp(t)
	key := newKey(t)
	if _, err := s.Enroll("node-b", key); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{StatusPending, StatusApproved, StatusDisabled} {
		if _, err := s.db.Exec(`UPDATE nodes SET status = ? WHERE name = ?`, status, "node-b"); err != nil {
			t.Fatal(err)
		}
		got, ok := s.Lookup(key)
		if !ok || got.Status != status {
			t.Errorf("Lookup with status %q = %+v, %v; want the row back", status, got, ok)
		}
	}
	if _, ok := s.Lookup(newKey(t)); ok {
		t.Error("an unenrolled key resolved to a node")
	}
}

// The credential is the key's wire form, not its fingerprint: a fingerprint is
// a digest an operator compares by eye, and keying authentication on it would
// authenticate anything that can produce a matching digest.
func TestLookupKeysOnTheWireForm(t *testing.T) {
	s := openTemp(t)
	key := newKey(t)
	n, err := s.Enroll("node-b", key)
	if err != nil {
		t.Fatal(err)
	}
	var wire, fp string
	if err := s.db.QueryRow(`SELECT wire, fp FROM nodes WHERE name = ?`, "node-b").Scan(&wire, &fp); err != nil {
		t.Fatal(err)
	}
	if wire != wireOf(key) || wire == fp {
		t.Fatalf("stored wire = %q, want base64(key.Marshal()) = %q", wire, wireOf(key))
	}
	// Corrupting the fingerprint alone leaves authentication working; the FP
	// column is display material.
	if _, err := s.db.Exec(`UPDATE nodes SET fp = 'SHA256:bogus' WHERE name = ?`, "node-b"); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Lookup(key); !ok || got.Name != n.Name {
		t.Errorf("Lookup after an FP change = %+v, %v; want the row back", got, ok)
	}
}

func TestSeenRecordsNodeAuthoredFacts(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Enroll("node-b", newKey(t)); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	if err := s.Seen("node-b", "arm64", "2026-07-17-2114", at); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("node-b")
	if err != nil {
		t.Fatal(err)
	}
	if got.Arch != "arm64" || got.Release != "2026-07-17-2114" {
		t.Errorf("after Seen: %+v", got)
	}
	if got.LastSeen == nil || !got.LastSeen.Equal(at) {
		t.Errorf("LastSeen = %v, want %v", got.LastSeen, at)
	}
	if err := s.Seen("ghost", "arm64", "", at); !errors.Is(err, ErrNoSuchNode) {
		t.Errorf("Seen(ghost) = %v, want ErrNoSuchNode", err)
	}
}

func TestRemoveLetsAKeyEnrolAgain(t *testing.T) {
	s := openTemp(t)
	key := newKey(t)
	if _, err := s.Enroll("node-b", key); err != nil {
		t.Fatal(err)
	}
	if err := s.Approve("node-b", "vanpelt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("node-b"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(key); ok {
		t.Error("a removed node still authenticates")
	}
	if err := s.Remove("node-b"); !errors.Is(err, ErrNoSuchNode) {
		t.Errorf("second Remove = %v, want ErrNoSuchNode", err)
	}
	n, err := s.Enroll("node-b", key)
	if err != nil {
		t.Fatal(err)
	}
	if n.Status != StatusPending {
		t.Errorf("re-enrolment after removal = %q, want pending", n.Status)
	}
}

func TestListIsNameSorted(t *testing.T) {
	s := openTemp(t)
	for _, name := range []string{"zeta", "alpha", "mid"} {
		if _, err := s.Enroll(name, newKey(t)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Name)
	}
	if strings.Join(got, ",") != "alpha,mid,zeta" {
		t.Errorf("List order = %v", got)
	}
}

// The credential never leaves the process in a rendering: consoles and the
// REST API marshal this struct straight out of the store.
func TestJSONOmitsTheCredential(t *testing.T) {
	s := openTemp(t)
	key := newKey(t)
	n, err := s.Enroll("node-b", key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), wireOf(key)) {
		t.Errorf("marshalled node leaks its key wire form: %s", b)
	}
	if !strings.Contains(string(b), n.FP) {
		t.Errorf("marshalled node omits the fingerprint operators compare: %s", b)
	}
}

// Enrolment races on one name settle in the database, not in a read-then-write
// window: exactly one key gets the name.
func TestEnrollIsAtomic(t *testing.T) {
	s := openTemp(t)
	const racers = 32
	keys := make([]xssh.PublicKey, racers)
	for i := range keys {
		keys[i] = newKey(t)
	}
	var wg sync.WaitGroup
	errs := make([]error, racers)
	wg.Add(racers)
	for i := range keys {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Enroll("node-b", keys[i])
		}(i)
	}
	wg.Wait()

	var won, taken int
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrNameTaken):
			taken++
		default:
			t.Errorf("racer %d: %v", i, err)
		}
	}
	if won != 1 || taken != racers-1 {
		t.Fatalf("won=%d taken=%d, want 1 and %d", won, taken, racers-1)
	}
	rows, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("List = %d rows, want 1", len(rows))
	}
}

// The roster shares sparkbox.db with the other stores, so a second Open on an
// existing file must be a no-op rather than a schema reset.
func TestReopenIsANoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	if _, err := first.Enroll("node-b", key); err != nil {
		t.Fatal(err)
	}
	if err := first.Approve("node-b", "vanpelt"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	got, ok := second.Lookup(key)
	if !ok || got.Status != StatusApproved || got.ApprovedBy != "vanpelt" {
		t.Fatalf("after reopen: %+v, %v", got, ok)
	}
}
