package fleet_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// newManager wires a Manager onto the in-process mock driver, the same way
// internal/host's own tests do: a temp state dir, a throwaway gateway key and a
// driver closed on cleanup so per-VM listeners don't leak between tests.
func newManager(t *testing.T, opts host.Options) *host.Manager {
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
	opts.Logger = discardLog()
	if opts.NodeName == "" {
		opts.NodeName = "boxa"
	}
	mgr, err := host.NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newIndex opens a placement ledger in its own temp dir, under the same file
// name the gateway uses so a test exercises the shared-database path.
func newIndex(t *testing.T) *placement.Store {
	t.Helper()
	st, err := placement.Open(filepath.Join(t.TempDir(), "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newFleet(t *testing.T, mgr *host.Manager, index *placement.Store) *fleet.Fleet {
	t.Helper()
	f, err := fleet.New(fleet.Options{
		Local: mgr, LocalName: mgr.NodeName(), LocalArch: "arm64", Index: index, Log: discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func mustCreate(t *testing.T, f *fleet.Fleet, name, owner string) *host.Sandbox {
	t.Helper()
	b, err := f.Create(context.Background(), name, owner, "ubuntu", 1, 512)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return b
}

// fakeNode is another machine, in memory. Nothing in this package can reach a
// real one until the link lands, but the rules that make a remote record safe —
// the ledger's authority over owner and node, the synthesised addresses, the
// answer when the machine stops answering — are worth pinning before it does.
type fakeNode struct {
	name  string
	facts fleet.Facts

	mu        sync.Mutex
	online    bool
	boxes     []*host.Sandbox
	snaps     []*host.Snapshot
	renameErr error
	calls     []string
}

func newFakeNode(name string) *fakeNode {
	return &fakeNode{name: name, online: true, facts: fleet.Facts{Node: name, Arch: "amd64"}}
}

func (n *fakeNode) Name() string       { return n.name }
func (n *fakeNode) Facts() fleet.Facts { return n.facts }
func (n *fakeNode) Capacity() host.NodeCapacity {
	return host.NodeCapacity{Node: n.name, Arch: n.facts.Arch}
}

func (n *fakeNode) Online() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.online
}

func (n *fakeNode) setOnline(v bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.online = v
}

func (n *fakeNode) Snapshot() ([]*host.Sandbox, []*host.Snapshot) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.boxes, n.snaps
}

func (n *fakeNode) record(op string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, op)
}

func (n *fakeNode) took(op string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range n.calls {
		if c == op {
			return true
		}
	}
	return false
}

func (n *fakeNode) Create(context.Context, string, string, string, int64, int64) (*host.Sandbox, error) {
	n.record("create")
	return nil, errors.New("fakeNode cannot create")
}

func (n *fakeNode) EnsureRunning(_ context.Context, name string) (*host.Sandbox, error) {
	n.record("ensure_running")
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, b := range n.boxes {
		if b.Name == name {
			return b, nil
		}
	}
	return nil, errors.New("no such sandbox")
}

func (n *fakeNode) Pause(context.Context, string) error   { n.record("pause"); return nil }
func (n *fakeNode) Archive(context.Context, string) error { n.record("archive"); return nil }
func (n *fakeNode) Resize(context.Context, string, int64) error {
	n.record("resize")
	return nil
}
func (n *fakeNode) Reboot(context.Context, string) error { n.record("reboot"); return nil }

func (n *fakeNode) Rename(_ context.Context, _, _, _ string) error {
	n.record("rename")
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.renameErr
}

func (n *fakeNode) Destroy(context.Context, string) error { n.record("destroy"); return nil }
func (n *fakeNode) SetPinned(context.Context, string, bool) error {
	n.record("set_pinned")
	return nil
}
func (n *fakeNode) ResyncEnv(context.Context, string) error { n.record("resync_env"); return nil }
func (n *fakeNode) Touch(context.Context, string) error     { n.record("touch"); return nil }
func (n *fakeNode) RecordKey(context.Context, string, string) error {
	n.record("record_key")
	return nil
}

func (n *fakeNode) Snapshotter(context.Context, string, string, string) (*host.Snapshot, error) {
	n.record("snapshot")
	return nil, errors.New("fakeNode cannot snapshot")
}

func (n *fakeNode) DeleteSnapshot(context.Context, string, string) error {
	n.record("delete_snapshot")
	return nil
}

func (n *fakeNode) Fork(context.Context, string, string, string, int64, int64) (*host.Sandbox, error) {
	n.record("fork")
	return nil, errors.New("fakeNode cannot fork")
}

func (n *fakeNode) DialGuest(context.Context, string, string, int) (net.Conn, error) {
	n.record("dial")
	return nil, errors.New("fakeNode cannot dial")
}

var _ fleet.Node = (*fakeNode)(nil)

// attach links a fake machine and gives it an inventory. The ledger rows that
// place those sandboxes are written separately, by place, because who owns a
// sandbox and where it lives is the gateway's answer and not the machine's —
// several tests here turn on the two disagreeing.
func attach(t *testing.T, f *fleet.Fleet, n *fakeNode, boxes ...*host.Sandbox) {
	t.Helper()
	n.mu.Lock()
	n.boxes = append(n.boxes, boxes...)
	n.mu.Unlock()
	detach, err := f.Attach(n)
	if err != nil {
		t.Fatalf("attach %s: %v", n.name, err)
	}
	t.Cleanup(detach)
}

func place(t *testing.T, index *placement.Store, name, owner, node string) {
	t.Helper()
	if err := index.Reserve(name, owner, node, "ubuntu", "amd64"); err != nil {
		t.Fatalf("place %s: %v", name, err)
	}
}

func TestHostRoundTrip(t *testing.T) {
	cases := []struct {
		in            string
		sandbox, node string
		ok            bool
	}{
		{in: fleet.Host("brave-otter", "boxb"), sandbox: "brave-otter", node: "boxb", ok: true},
		{in: "172.30.4.2"},
		{in: "brave-otter.sandbox.invalid"},               // no node label
		{in: "a.b.c.sandbox.invalid"},                     // one label too many
		{in: ".boxb.sandbox.invalid"},                     // no sandbox
		{in: "brave-otter..sandbox.invalid", sandbox: ""}, // no node
		{in: "brave-otter.boxb.sandbox.example"},
	}
	for _, tc := range cases {
		sandbox, node, ok := fleet.SplitHost(tc.in)
		if ok != tc.ok || sandbox != tc.sandbox || node != tc.node {
			t.Errorf("SplitHost(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, sandbox, node, ok, tc.sandbox, tc.node, tc.ok)
		}
	}
}

func TestCreateTakesTheNameOnce(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)

	mustCreate(t, f, "brave-otter", "alice")

	// The ledger's PRIMARY KEY is the allocator, and its refusal must read
	// exactly like the manager's own: same noun, same problem, same rendering.
	_, err := f.Create(context.Background(), "brave-otter", "bob", "ubuntu", 1, 512)
	var name *host.NameError
	if !errors.As(err, &name) {
		t.Fatalf("second create: want *host.NameError, got %v", err)
	}
	if name.Problem != host.NameTaken || name.Noun != "sandbox" || name.Name != "brave-otter" {
		t.Fatalf("unexpected NameError: %+v", name)
	}
	if got := ctlops.AsError("new", err).Msg; got != ctlops.AsError("new", &host.NameError{
		Problem: host.NameTaken, Noun: "sandbox", Name: "brave-otter",
	}).Msg {
		t.Fatalf("collision does not render like a local one: %q", got)
	}

	row, ok, err := index.Get("brave-otter")
	if err != nil || !ok {
		t.Fatalf("ledger row: ok=%v err=%v", ok, err)
	}
	if row.Owner != "alice" || row.Node != "boxa" || row.Image != "ubuntu" || row.Arch != "arm64" {
		t.Fatalf("unexpected row: %+v", row)
	}
}

func TestFailedCreateReleasesTheName(t *testing.T) {
	// A budget of 512 MB admits the first sandbox and refuses the second, so the
	// second create fails inside the machine, after the name was reserved.
	mgr := newManager(t, host.Options{MemAdmissionPct: 100, HostMemMB: 512})
	index := newIndex(t)
	f := newFleet(t, mgr, index)

	mustCreate(t, f, "first", "alice")

	_, err := f.Create(context.Background(), "second", "alice", "ubuntu", 1, 512)
	var capacity *host.CapacityError
	if !errors.As(err, &capacity) {
		t.Fatalf("want *host.CapacityError, got %v", err)
	}
	if _, ok, err := index.Get("second"); err != nil || ok {
		t.Fatalf("a name reserved for a sandbox that was never built is stranded: ok=%v err=%v", ok, err)
	}

	// And the name is genuinely usable again once there is room for it.
	if err := f.Pause(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, f, "second", "alice")
}

func TestDestroyReleasesTheName(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)

	mustCreate(t, f, "brave-otter", "alice")
	if err := f.Destroy(context.Background(), "brave-otter"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := index.Get("brave-otter"); err != nil || ok {
		t.Fatalf("destroyed sandbox still holds its name: ok=%v err=%v", ok, err)
	}
	mustCreate(t, f, "brave-otter", "bob")
}

func TestBootReconcileFollowsTheLocalManager(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)

	// One sandbox this machine holds and never told the ledger about, one row
	// for a sandbox it no longer has, and one row belonging to another machine.
	if _, err := mgr.Create(context.Background(), "brave-otter", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := index.Reserve("ghost", "alice", "boxa", "ubuntu", "arm64"); err != nil {
		t.Fatal(err)
	}
	if err := index.Reserve("elsewhere", "bob", "boxb", "ubuntu", "amd64"); err != nil {
		t.Fatal(err)
	}

	newFleet(t, mgr, index)

	row, ok, err := index.Get("brave-otter")
	if err != nil || !ok {
		t.Fatalf("a local sandbox with no row was not adopted: ok=%v err=%v", ok, err)
	}
	if row.Node != "boxa" || row.Owner != "alice" {
		t.Fatalf("unexpected adopted row: %+v", row)
	}
	if _, ok, err := index.Get("ghost"); err != nil || ok {
		t.Fatalf("a row this machine no longer holds was left blocking the name: ok=%v err=%v", ok, err)
	}
	if _, ok, err := index.Get("elsewhere"); err != nil || !ok {
		t.Fatalf("another machine's row was released: ok=%v err=%v", ok, err)
	}
}

// A deployment that predates the ledger can hold a record with no owner. Boot
// has to survive it: refusing to serve because one sandbox is malformed turns
// an upgrade into an outage for every other sandbox on the machine.
func TestBootSurvivesAnOwnerlessSandbox(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)

	if _, err := mgr.Create(context.Background(), "legacy", "", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Create(context.Background(), "brave-otter", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := index.Reserve("ghost", "alice", "boxa", "ubuntu", "arm64"); err != nil {
		t.Fatal(err)
	}

	f := newFleet(t, mgr, index) // fatal here is the defect

	// The record the ledger will not accept is left unplaced, and skipping it
	// does not cost the rest of the reconciliation.
	if _, ok, err := index.Get("legacy"); err != nil || ok {
		t.Fatalf("an ownerless sandbox was placed anyway: ok=%v err=%v", ok, err)
	}
	if _, ok, err := index.Get("brave-otter"); err != nil || !ok {
		t.Fatalf("adoption stopped at the malformed record: ok=%v err=%v", ok, err)
	}
	if _, ok, err := index.Get("ghost"); err != nil || ok {
		t.Fatalf("the release pass never ran: ok=%v err=%v", ok, err)
	}

	// The sandbox is still fully usable — the local manager answers for it —
	// and its name is still defended, by the manager rather than the ledger.
	if _, ok := f.Get("legacy"); !ok {
		t.Fatal("an unplaced local sandbox is invisible to the fleet")
	}
	if err := f.Pause(context.Background(), "legacy"); err != nil {
		t.Fatalf("an unplaced local sandbox cannot be operated: %v", err)
	}
	_, err := f.Create(context.Background(), "legacy", "bob", "ubuntu", 1, 512)
	var name *host.NameError
	if !errors.As(err, &name) || name.Problem != host.NameTaken {
		t.Fatalf("an unplaced name was handed to somebody else: %v", err)
	}
	if _, ok, err := index.Get("legacy"); err != nil || ok {
		t.Fatalf("the refused create stranded a row: ok=%v err=%v", ok, err)
	}
}

// The gateway always builds a ledger, so "no other machine is linked" — the
// ordinary single-box deployment — must not pay for one. Listings are the
// highest-traffic reads in the system and there is nothing a row can produce
// without a node attached to render it against.
func TestListingsSkipTheLedgerWithNothingLinked(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	for i := range 5000 {
		if err := index.Reserve(fmt.Sprintf("remote-%04d", i), "bob", "boxb", "ubuntu", "amd64"); err != nil {
			t.Fatal(err)
		}
	}
	f := newFleet(t, mgr, index)
	mustCreate(t, f, "brave-otter", "alice")

	start := time.Now()
	const rounds = 200
	for range rounds {
		if got := f.List(); len(got) != 1 || got[0].Name != "brave-otter" {
			t.Fatalf("List = %+v", got)
		}
		if got := f.ListByOwner("bob"); got != nil {
			t.Fatalf("ListByOwner(bob) = %+v", got)
		}
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("%d listings against a 5000-row ledger with nothing linked took %v", rounds, elapsed)
	}

	// And the shortcut is exactly that: once a machine is linked, its rows are
	// served again.
	attach(t, f, newFakeNode("boxb"), &host.Sandbox{
		Name: "remote-0000", Owner: "bob", Image: "ubuntu", State: vmm.StateRunning,
	})
	if got := f.ListByOwner("bob"); len(got) != 1 || got[0].Name != "remote-0000" {
		t.Fatalf("ListByOwner(bob) after attach = %+v", got)
	}
	if got := f.List(); len(got) != 2 {
		t.Fatalf("List after attach = %+v", got)
	}
}

func TestGetIsNotSlowedByALargeLedger(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	for i := range 5000 {
		name := fmt.Sprintf("remote-%04d", i)
		if err := index.Reserve(name, "bob", "boxb", "ubuntu", "amd64"); err != nil {
			t.Fatal(err)
		}
	}
	f := newFleet(t, mgr, index)
	mustCreate(t, f, "brave-otter", "alice")

	// The local manager answers first and its answer is final, so the ledger's
	// size cannot show up on the hot path every authorization decision runs.
	start := time.Now()
	const rounds = 1000
	for range rounds {
		if _, ok := f.Get("brave-otter"); !ok {
			t.Fatal("local sandbox not found")
		}
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("%d local Gets against a 5000-row ledger took %v", rounds, elapsed)
	}
}

func TestRemoteRecordsComeFromTheLedger(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)

	nodeb := newFakeNode("boxb")
	// The machine claims the sandbox belongs to someone else, on somewhere else,
	// and hands back its own guest address. None of that may survive: the
	// gateway placed this name for bob, on boxb.
	attach(t, f, nodeb, &host.Sandbox{
		Name: "far-away", Owner: "mallory", Node: "boxc", Image: "ubuntu",
		State: vmm.StateRunning, HostIP: "172.30.4.2", SSHAddr: "172.30.4.2:22",
		GuestV6: "fd00::4:2", MemMB: 2048,
	})
	place(t, index, "far-away", "bob", "boxb")

	b, ok := f.Get("far-away")
	if !ok {
		t.Fatal("a placed sandbox on a linked machine is not visible")
	}
	if b.Owner != "bob" || b.Node != "boxb" {
		t.Fatalf("owner/node came from the machine, not the ledger: %+v", b)
	}
	if b.HostIP != fleet.Host("far-away", "boxb") {
		t.Fatalf("HostIP was relayed rather than synthesised: %q", b.HostIP)
	}
	if want := net.JoinHostPort(fleet.Host("far-away", "boxb"), fleet.SSHPort); b.SSHAddr != want {
		t.Fatalf("SSHAddr = %q, want %q", b.SSHAddr, want)
	}
	if b.GuestV6 != "" {
		t.Fatalf("GuestV6 was relayed: %q", b.GuestV6)
	}
	if b.Unreachable {
		t.Fatal("an online machine's sandbox is marked unreachable")
	}
	// Node-authored, display-only fields survive untouched.
	if b.MemMB != 2048 || b.State != vmm.StateRunning {
		t.Fatalf("display fields lost: %+v", b)
	}

	if got := f.ListByOwner("bob"); len(got) != 1 || got[0].Name != "far-away" {
		t.Fatalf("ListByOwner(bob) = %+v", got)
	}
	if got := f.ListByOwner("mallory"); got != nil {
		t.Fatalf("the machine's claimed owner can see it: %+v", got)
	}
	if got := f.List(); len(got) != 1 || got[0].Name != "far-away" {
		t.Fatalf("List = %+v", got)
	}

	nodeb.setOnline(false)
	b, ok = f.Get("far-away")
	if !ok || !b.Unreachable {
		t.Fatalf("an offline machine's sandbox is not flagged: ok=%v %+v", ok, b)
	}
}

func TestOperationsOnAnOfflineMachine(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	place(t, index, "far-away", "bob", "boxb")

	err := f.Pause(context.Background(), "far-away")
	if !fleet.IsNodeUnreachable(err) {
		t.Fatalf("want a node outage, got %v", err)
	}
	var ce *ctlops.Error
	if !errors.As(err, &ce) {
		t.Fatalf("want a *ctlops.Error, got %T", err)
	}
	if ce.Op != "pause" || ce.ExitCode() != 1 || ce.HTTPStatus() != 503 {
		t.Fatalf("unexpected outage error: op=%q exit=%d status=%d", ce.Op, ce.ExitCode(), ce.HTTPStatus())
	}
	if ce.Details["node"] != "boxb" {
		t.Fatalf("the outage does not name the machine: %+v", ce.Details)
	}
	if !fleet.IsNodeUnreachable(fmt.Errorf("wrapped: %w", err)) {
		t.Fatal("IsNodeUnreachable does not look through a wrap")
	}
	if fleet.IsNodeUnreachable(errors.New("plain")) {
		t.Fatal("IsNodeUnreachable said yes to an unrelated error")
	}

	// The reads that carry no error simply do nothing rather than blocking.
	f.Touch("far-away")
	f.RecordKey("far-away", "SHA256:whatever")
	f.ResyncEnv(context.Background(), "far-away")
}

func TestRenameMovesTheLedgerAndRollsBack(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	mustCreate(t, f, "brave-otter", "alice")

	if err := f.Rename(context.Background(), "brave-otter", "bold-otter", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := index.Get("brave-otter"); ok {
		t.Fatal("the old name still holds a row")
	}
	row, ok, err := index.Get("bold-otter")
	if err != nil || !ok || row.Node != "boxa" {
		t.Fatalf("the row did not move: ok=%v err=%v row=%+v", ok, err, row)
	}

	// A target another machine holds is a collision, and the machine must never
	// have been asked to do the rename.
	place(t, index, "taken", "bob", "boxb")
	err = f.Rename(context.Background(), "bold-otter", "taken", "alice")
	var name *host.NameError
	if !errors.As(err, &name) || name.Problem != host.NameTaken {
		t.Fatalf("want a taken-name error, got %v", err)
	}
	if _, ok := mgr.Get("bold-otter"); !ok {
		t.Fatal("the sandbox was renamed despite the collision")
	}
}

func TestRenameRollsTheLedgerBackWhenTheMachineRefuses(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)

	nodeb := newFakeNode("boxb")
	nodeb.renameErr = errors.New("no")
	attach(t, f, nodeb, &host.Sandbox{
		Name: "far-away", Owner: "bob", Image: "ubuntu", State: vmm.StateRunning,
	})
	place(t, index, "far-away", "bob", "boxb")

	if err := f.Rename(context.Background(), "far-away", "far-away-2", "bob"); err == nil {
		t.Fatal("a refused rename reported success")
	}
	if !nodeb.took("rename") {
		t.Fatal("the machine was never asked")
	}
	if _, ok, _ := index.Get("far-away-2"); ok {
		t.Fatal("the ledger kept the new name after the machine refused")
	}
	row, ok, err := index.Get("far-away")
	if err != nil || !ok || row.Node != "boxb" {
		t.Fatalf("the row was not rolled back: ok=%v err=%v row=%+v", ok, err, row)
	}
}

func TestAttachIsExclusive(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, nil)

	if _, err := f.Attach(newFakeNode("")); err == nil {
		t.Fatal("a nameless machine was attached")
	}
	if _, err := f.Attach(newFakeNode("boxa")); err == nil {
		t.Fatal("this machine was attached as if it were another one")
	}
	nodeb := newFakeNode("boxb")
	detach, err := f.Attach(nodeb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Attach(newFakeNode("boxb")); err == nil {
		t.Fatal("a second link for one machine was accepted")
	}
	if !f.Online("boxb") {
		t.Fatal("an attached machine is not online")
	}
	if got := f.Capacities(); len(got) != 2 || got[0].Node != "boxa" || got[1].Node != "boxb" {
		t.Fatalf("Capacities = %+v", got)
	}
	detach()
	detach() // idempotent
	if f.Online("boxb") {
		t.Fatal("a detached machine is still online")
	}
	if got := f.Capacities(); len(got) != 1 {
		t.Fatalf("Capacities after detach = %+v", got)
	}
}

func TestNodeOf(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	mustCreate(t, f, "brave-otter", "alice")
	place(t, index, "far-away", "bob", "boxb")

	for _, tc := range []struct {
		name, node string
		ok         bool
	}{
		{"brave-otter", "boxa", true},
		{"far-away", "boxb", true},
		{"nobody", "", false},
	} {
		node, ok := f.NodeOf(tc.name)
		if node != tc.node || ok != tc.ok {
			t.Errorf("NodeOf(%q) = (%q, %v), want (%q, %v)", tc.name, node, ok, tc.node, tc.ok)
		}
	}
}

func TestDialContextFallsThroughToTheHostNetwork(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()

	conn, err := f.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("an ordinary host address did not dial directly: %v", err)
	}
	conn.Close()
}

func TestDialContextReachesALocalSandbox(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, nil)
	mustCreate(t, f, "brave-otter", "alice")

	// The mock driver's guests listen on loopback, so the synthetic address
	// resolves node-side to a real sshd the same way firecracker's would.
	addr := net.JoinHostPort(fleet.Host("brave-otter", "boxa"), fleet.SSHPort)
	conn, err := f.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	conn.Close()

	if _, err := f.DialContext(context.Background(), "tcp",
		net.JoinHostPort(fleet.Host("nobody", "boxa"), fleet.SSHPort)); err == nil {
		t.Fatal("a sandbox this machine does not have was dialed anyway")
	}

	if err := f.Pause(context.Background(), "brave-otter"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.DialContext(context.Background(), "tcp", addr); err == nil {
		t.Fatal("a paused sandbox was dialed anyway")
	}
}

func TestDialContextOnAnOfflineMachine(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))

	_, err := f.DialContext(context.Background(), "tcp",
		net.JoinHostPort(fleet.Host("far-away", "boxb"), "8080"))
	if !fleet.IsNodeUnreachable(err) {
		t.Fatalf("want a node outage, got %v", err)
	}
	if _, err := f.DialContext(context.Background(), "tcp",
		net.JoinHostPort(fleet.Host("far-away", "boxb"), "http")); err == nil {
		t.Fatal("a port that is not a number and not the ssh name was accepted")
	}
}

func TestFleetNeedsALocalMachine(t *testing.T) {
	if _, err := fleet.New(fleet.Options{Log: discardLog()}); err == nil {
		t.Fatal("a fleet with nowhere to put anything was built")
	}
}

func TestLocalManagerRecoversTheConcreteManager(t *testing.T) {
	mgr := newManager(t, host.Options{})
	got, ok := fleet.LocalManager(fleet.Local("boxa", mgr))
	if !ok || got != mgr {
		t.Fatalf("LocalManager = (%p, %v), want (%p, true)", got, ok, mgr)
	}
	if _, ok := fleet.LocalManager(newFakeNode("boxb")); ok {
		t.Fatal("another machine was presented as this one")
	}
}
