package fleet_test

// What the gateway does with a machine's picture of itself.
//
// Every test here is written against the two prohibitions, because both are the
// kind of defect that costs somebody a sandbox at three in the morning and
// neither can happen on a single box:
//
//   - a placement is never released because a machine stopped reporting it;
//   - the running->paused downgrade a manager performs on its own records at
//     boot is never applied to another machine's.
//
// The fake machine is fleet_test.go's fakeNode and the ledger is a real sqlite
// store, because half of what is under test is which row is written and which
// is left alone.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// testClock is a clock the test moves by hand. Every timer in this file is a
// grace period measured in minutes, and the alternative to controlling the
// clock is a test that either sleeps out a real one or configures it so short
// that it proves nothing.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *testClock { return &testClock{at: time.Now()} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// newFleetOn is newFleet with the reconciliation grace and its clock in the
// test's hands.
func newFleetOn(t *testing.T, mgr *host.Manager, index *placement.Store, grace time.Duration, clock *testClock) *fleet.Fleet {
	t.Helper()
	f, err := fleet.New(fleet.Options{
		Local: mgr, LocalName: mgr.NodeName(), LocalArch: "arm64", Index: index,
		Log: discardLog(), ReconcileGrace: grace, Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// bootManager builds a manager over a NAMED state directory, so the same
// records can be loaded twice. That is the whole of what a gateway restart does
// to them, and it is what runs the downgrade the second prohibition is about.
func bootManager(t *testing.T, dir, node string) *host.Manager {
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
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver, NodeName: node, Logger: discardLog(),
		GatewayPublicKey: string(xssh.MarshalAuthorizedKey(signer.PublicKey())),
	})
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

// inventory is one machine's whole picture, as a node sends it. Several tests
// below put a deliberate lie in the Node field: it is a claim, and the
// authenticated link name is the only thing that decides anything.
func inventory(node string, boxes ...nodelink.SandboxRow) nodelink.InventoryMsg {
	return nodelink.InventoryMsg{Node: node, Sandboxes: boxes, At: time.Now()}
}

func reports(name, owner string) nodelink.SandboxRow {
	return nodelink.SandboxRow{
		Name: name, Owner: owner, Image: "ubuntu", State: string(vmm.StateRunning),
		VCPUs: 1, MemMB: 512, CreatedAt: time.Now(), LastActive: time.Now(),
	}
}

type ledgerRow struct{ name, owner, node string }

func seed(t *testing.T, index *placement.Store, rows []ledgerRow) {
	t.Helper()
	for _, r := range rows {
		if err := index.Reserve(r.name, r.owner, r.node, "ubuntu", "amd64"); err != nil {
			t.Fatalf("seeding %q: %v", r.name, err)
		}
	}
}

// stateOf reads the ledger's reconciliation marker, failing if the row is gone
// — which is the assertion in most of these tests rather than a lookup on the
// way to one.
func stateOf(t *testing.T, index *placement.Store, name string) string {
	t.Helper()
	r, ok, err := index.Get(name)
	if err != nil {
		t.Fatalf("reading the row for %q: %v", name, err)
	}
	if !ok {
		t.Fatalf("the placement for %q was released; a row is marked, never deleted", name)
	}
	return r.State
}

func sameNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The four cases
// ---------------------------------------------------------------------------

// TestReconcileTheFourCases is the blueprint's table, one row per case.
//
// Each case sets up a ledger and a machine that disagree in exactly one way,
// then asserts what the ledger holds afterwards, what the machine is told, and
// — the point of the whole exercise — that no row went away.
func TestReconcileTheFourCases(t *testing.T) {
	cases := []struct {
		name string
		// rows is what the ledger holds before the inventory arrives.
		rows []ledgerRow
		// node is the AUTHENTICATED name the inventory arrives on; inv is what
		// it reports.
		node string
		inv  []nodelink.SandboxRow
		// disclaimed is a machine that reports an empty inventory first. It is
		// how a row gets to be orphaned before somebody else claims its name.
		disclaimed string
		// want is the state every named row must be in afterwards. Any name
		// reported by the machine that is absent from this map must have no row
		// at all.
		want            map[string]string
		wantOrphaned    []string
		wantQuarantined []string
	}{
		{
			name: "the ledger and the machine agree",
			rows: []ledgerRow{{"far-away", "alice", "boxb"}},
			node: "boxb",
			inv:  []nodelink.SandboxRow{reports("far-away", "alice")},
			want: map[string]string{"far-away": placement.StateOK},
		},
		{
			name: "the machine no longer has it",
			rows: []ledgerRow{{"far-away", "alice", "boxb"}},
			node: "boxb",
			// Marked, and still there. Releasing the name is the one thing that
			// cannot be taken back.
			want:         map[string]string{"far-away": placement.StateOrphaned},
			wantOrphaned: []string{"far-away"},
		},
		{
			name: "the machine holds one nobody placed",
			node: "boxb",
			inv:  []nodelink.SandboxRow{reports("stray", "alice")},
			want: map[string]string{"stray": placement.StateOK},
		},
		{
			name: "a name another machine already holds",
			rows: []ledgerRow{{"far-away", "alice", "boxc"}},
			node: "boxb",
			inv:  []nodelink.SandboxRow{reports("far-away", "mallory")},
			// The incumbent's healthy row is not touched: a machine must not be
			// able to take another machine's sandbox out of service by
			// reporting its name, and the ledger's answer is still a usable one.
			want:            map[string]string{"far-away": placement.StateOK},
			wantQuarantined: []string{"far-away"},
		},
		{
			name:       "a name whose own machine has disclaimed it",
			rows:       []ledgerRow{{"far-away", "alice", "boxc"}},
			disclaimed: "boxc",
			node:       "boxb",
			inv:        []nodelink.SandboxRow{reports("far-away", "alice")},
			// Now the ledger's answer has been contradicted by the machine it
			// names, and neither machine is served.
			want:            map[string]string{"far-away": placement.StateQuarantine},
			wantQuarantined: []string{"far-away"},
		},
		{
			name: "rows for a machine that has not connected",
			rows: []ledgerRow{{"far-away", "alice", "boxb"}, {"elsewhere", "bob", "boxz"}},
			node: "boxb",
			inv:  []nodelink.SandboxRow{reports("far-away", "alice")},
			// boxz has said nothing, so nothing is concluded about it. There is
			// no timer that eventually gives up on a machine that is switched
			// off — its sandboxes are still on its disk.
			want: map[string]string{"far-away": placement.StateOK, "elsewhere": placement.StateOK},
		},
		{
			name: "a name this gateway itself holds",
			node: "boxb",
			inv:  []nodelink.SandboxRow{reports("right-here", "alice")},
			// The row is the local one, written when this gateway created it.
			want:            map[string]string{"right-here": placement.StateOK},
			wantQuarantined: []string{"right-here"},
		},
		{
			name: "a name the platform does not issue",
			node: "boxb",
			inv: []nodelink.SandboxRow{
				reports("Not-A-Label", "alice"),
				reports("console", "alice"), // reserved for a platform door
			},
			want:            map[string]string{},
			wantQuarantined: []string{"Not-A-Label", "console"},
		},
		{
			name: "an owner this gateway does not recognise",
			node: "boxb",
			inv: []nodelink.SandboxRow{
				reports("ownerless", ""),
				reports("shouty", "ALICE"),
			},
			want:            map[string]string{},
			wantQuarantined: []string{"ownerless", "shouty"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := newIndex(t)
			clock := newClock()
			f := newFleetOn(t, newManager(t, host.Options{NodeName: "boxa"}), index, time.Minute, clock)
			mustCreate(t, f, "right-here", "alice")
			seed(t, index, tc.rows)
			// Every row in this table is meant to be past the create window;
			// the grace has a test of its own.
			clock.advance(time.Hour)

			if tc.disclaimed != "" {
				f.Reconcile(tc.disclaimed, inventory(tc.disclaimed))
			}
			ack := f.Reconcile(tc.node, inventory(tc.node, tc.inv...))

			for name, want := range tc.want {
				if got := stateOf(t, index, name); got != want {
					t.Errorf("state of %q = %q, want %q", name, got, want)
				}
			}
			for _, b := range tc.inv {
				if _, expected := tc.want[b.Name]; expected {
					continue
				}
				if _, ok, _ := index.Get(b.Name); ok {
					t.Errorf("a refused claim on %q was written to the ledger", b.Name)
				}
			}
			if !sameNames(ack.Orphaned, tc.wantOrphaned) {
				t.Errorf("ack.Orphaned = %v, want %v", ack.Orphaned, tc.wantOrphaned)
			}
			if !sameNames(ack.Quarantined, tc.wantQuarantined) {
				t.Errorf("ack.Quarantined = %v, want %v", ack.Quarantined, tc.wantQuarantined)
			}
		})
	}
}

// TestReconcileAdoptsWhatTheLedgerLost is the case adoption exists for.
//
// A gateway that dies mid-create leaves the sandbox running on the machine and
// its job in the memory of a process that no longer exists (ctlops keeps jobs
// there deliberately). The row may or may not have been written. Without
// adoption the half where it was not is a VM burning somebody's RAM that no
// user can reach and no operator can find by name.
func TestReconcileAdoptsWhatTheLedgerLost(t *testing.T) {
	index := newIndex(t)
	f := newFleet(t, newManager(t, host.Options{NodeName: "boxa"}), index)

	nodeb := newFakeNode("boxb")
	attach(t, f, nodeb, &host.Sandbox{
		Name: "orphan-of-the-storm", Owner: "alice", Image: "ubuntu", State: vmm.StateRunning,
	})

	// Before: invisible. A name with no row is a name this gateway has not
	// placed anywhere, whatever a machine is holding under it.
	if _, ok := f.Get("orphan-of-the-storm"); ok {
		t.Fatal("a sandbox with no placement was served")
	}

	f.Reconcile("boxb", inventory("boxb", reports("orphan-of-the-storm", "alice")))

	r, ok, err := index.Get("orphan-of-the-storm")
	if err != nil || !ok {
		t.Fatalf("it was not adopted: ok=%v err=%v", ok, err)
	}
	if r.Owner != "alice" || r.Node != "boxb" || r.Image != "ubuntu" {
		t.Fatalf("adopted row = %+v, want alice's ubuntu sandbox on boxb", r)
	}
	// And from here it is an ordinary remote sandbox: its owner's, nobody
	// else's, and routable.
	b, ok := f.Get("orphan-of-the-storm")
	if !ok || b.Owner != "alice" || b.Node != "boxb" {
		t.Fatalf("Get after adoption = (%+v, %v)", b, ok)
	}
	if got := boxNames(f.ListByOwner("alice")); len(got) != 1 || got[0] != "orphan-of-the-storm" {
		t.Fatalf("ListByOwner(alice) = %v", got)
	}
	if got := f.ListByOwner("mallory"); got != nil {
		t.Fatalf("ListByOwner(mallory) = %v", boxNames(got))
	}
	if err := f.Pause(context.Background(), "orphan-of-the-storm"); err != nil {
		t.Fatalf("pausing an adopted sandbox: %v", err)
	}
	if !nodeb.took("pause") {
		t.Fatal("the pause never reached the machine holding it")
	}
}

// TestReconcileIgnoresTheNameInThePayload is invariant 1 at this layer: the
// authenticated link name decides, and the inventory's own Node field is never
// read.
//
// A machine that could reconcile as somebody else would only need one empty
// inventory in another machine's name to orphan every row on it.
func TestReconcileIgnoresTheNameInThePayload(t *testing.T) {
	index := newIndex(t)
	clock := newClock()
	f := newFleetOn(t, newManager(t, host.Options{NodeName: "boxa"}), index, time.Minute, clock)
	seed(t, index, []ledgerRow{{"far-away", "alice", "boxc"}})
	clock.advance(time.Hour)

	// boxb, claiming to be boxc, reporting nothing.
	ack := f.Reconcile("boxb", nodelink.InventoryMsg{Node: "boxc"})

	if got := stateOf(t, index, "far-away"); got != placement.StateOK {
		t.Fatalf("boxc's row is %q after boxb spoke in its name, want untouched", got)
	}
	if len(ack.Orphaned) != 0 {
		t.Fatalf("ack.Orphaned = %v, want nothing: boxb holds no rows", ack.Orphaned)
	}
}

// TestReconcileWillNotSpeakForTheLocalMachine is the same rule pointed at the
// one machine whose sandboxes are running in this very process.
func TestReconcileWillNotSpeakForTheLocalMachine(t *testing.T) {
	index := newIndex(t)
	clock := newClock()
	f := newFleetOn(t, newManager(t, host.Options{NodeName: "boxa"}), index, time.Minute, clock)
	mustCreate(t, f, "right-here", "alice")
	clock.advance(time.Hour)

	f.Reconcile("boxa", nodelink.InventoryMsg{Node: "boxa"})

	if got := stateOf(t, index, "right-here"); got != placement.StateOK {
		t.Fatalf("a local row is %q, want untouched", got)
	}
	if _, ok := f.Get("right-here"); !ok {
		t.Fatal("the local sandbox stopped being served")
	}
	if err := f.Pause(context.Background(), "right-here"); err != nil {
		t.Fatalf("the local sandbox stopped being operable: %v", err)
	}
}

// TestReconcileWithNoLedger is the single-box shape: nowhere to record a
// placement and nothing placed, so an inventory is a no-op rather than a nil
// dereference.
func TestReconcileWithNoLedger(t *testing.T) {
	f := newFleet(t, newManager(t, host.Options{NodeName: "boxa"}), nil)
	ack := f.Reconcile("boxb", inventory("boxb", reports("stray", "alice")))
	if len(ack.Orphaned) != 0 || len(ack.Quarantined) != 0 {
		t.Fatalf("ack = %+v, want empty", ack)
	}
}

// ---------------------------------------------------------------------------
// The graces
// ---------------------------------------------------------------------------

// TestReconcileGraceCoversACreateInFlight is the before/after-expiry pair for
// the grace reconciliation itself keeps.
//
// The name is reserved BEFORE the machine is asked to build anything — that
// ordering is what makes the ledger the fleet's name allocator — so there is a
// window in which the row is real and the sandbox honestly does not exist yet.
// An inventory crossing it (a reconnect, a queue overflow) must not conclude
// the sandbox is lost, because orphaning refuses the very operations that would
// finish it.
func TestReconcileGraceCoversACreateInFlight(t *testing.T) {
	index := newIndex(t)
	clock := newClock()
	f := newFleetOn(t, newManager(t, host.Options{NodeName: "boxa"}), index, time.Minute, clock)
	seed(t, index, []ledgerRow{{"being-built", "alice", "boxb"}})

	// Before expiry: the machine does not report it and nothing is concluded.
	clock.advance(59 * time.Second)
	ack := f.Reconcile("boxb", inventory("boxb"))
	if got := stateOf(t, index, "being-built"); got != placement.StateOK {
		t.Fatalf("a placement 59s old is %q, want untouched inside a 1m grace", got)
	}
	if len(ack.Orphaned) != 0 {
		t.Fatalf("ack.Orphaned = %v inside the grace, want nothing", ack.Orphaned)
	}
	// It is still operable, which is the point: the create that is finishing
	// has to be able to finish. The only complaint is that boxb is not linked.
	if err := f.Pause(context.Background(), "being-built"); !fleet.IsNodeUnreachable(err) {
		t.Fatalf("pausing inside the grace = %v, want only the not-linked answer", err)
	}

	// After expiry: the same silence means the sandbox is not coming.
	clock.advance(2 * time.Second)
	ack = f.Reconcile("boxb", inventory("boxb"))
	if got := stateOf(t, index, "being-built"); got != placement.StateOrphaned {
		t.Fatalf("a placement past the grace is %q, want orphaned", got)
	}
	if !sameNames(ack.Orphaned, []string{"being-built"}) {
		t.Fatalf("ack.Orphaned = %v, want the one name", ack.Orphaned)
	}
	// Marking it moved the row's updated_at. A grace measured from that would
	// re-arm on every marking and the disagreement would go quiet forever.
	ack = f.Reconcile("boxb", inventory("boxb"))
	if !sameNames(ack.Orphaned, []string{"being-built"}) {
		t.Fatalf("the second ack = %v; the disagreement is still live", ack.Orphaned)
	}
}

// TestNodeOfflineGraceBeforeAndAfterExpiry drives the other timer: how long a
// machine that has gone quiet still counts as answering.
//
// It runs over a real ServeLink so that the value configured on the fleet is
// the one the link is actually built with — the door assembles the
// ServerOptions and knows nothing about fleet policy, so a grace that never
// reached the link would be a setting with no effect and a passing unit test.
//
// Before expiry a quiet machine is simply a machine between heartbeats: its
// sandboxes are reachable and operations are sent. After it, they are flagged
// unreachable and refused with the offline sentence — and NOTHING else changes.
// No row is touched, no state is rewritten, nothing is rescheduled: the VMs are
// on a disk this gateway cannot see, which is not the same as gone.
func TestNodeOfflineGraceBeforeAndAfterExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	index := newIndex(t)
	clock := newClock()
	f, err := fleet.New(fleet.Options{
		Local: newManager(t, host.Options{NodeName: "boxa"}), LocalName: "boxa", LocalArch: "arm64",
		Index: index, Log: discardLog(), NodeGrace: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	seed(t, index, []ledgerRow{{"far-away", "alice", "node-b"}})

	// The link's own clock is the test's, so "the machine has said nothing for
	// 31 seconds" is a fact the test states rather than one it waits for.
	node := linkAt(t, ctx, f, "node-b", clock.now)

	// Before expiry.
	if !f.Online("node-b") {
		t.Fatal("a machine that has just connected is not online")
	}
	// Its inventory, over the real link — which is also the wiring assertion
	// for this file: a Reconcile that ServeLink never installed would leave the
	// row unadopted and this sandbox invisible.
	node.ask(t, "n2", nodelink.TypeInventory, inventory("node-b", reports("far-away", "alice")))
	waitFor(t, "node-b's inventory to reach the gateway", func() bool {
		b, ok := f.Get("far-away")
		return ok && !b.Unreachable
	})
	clock.advance(29 * time.Second)
	if !f.Online("node-b") {
		t.Fatal("a machine quiet for 29s of a 30s grace reads as offline")
	}
	if b, ok := f.Get("far-away"); !ok || b.Unreachable {
		t.Fatalf("Get inside the grace = (%+v, %v), want reachable", b, ok)
	}

	// After expiry.
	clock.advance(2 * time.Second)
	if f.Online("node-b") {
		t.Fatal("a machine quiet past its grace still reads as online")
	}
	b, ok := f.Get("far-away")
	if !ok {
		t.Fatal("a sandbox on a quiet machine vanished from its owner's view")
	}
	if !b.Unreachable || b.Owner != "alice" {
		t.Fatalf("Get past the grace = %+v, want alice's, unreachable", b)
	}
	if err := f.Pause(context.Background(), "far-away"); !fleet.IsNodeUnreachable(err) {
		t.Fatalf("pause past the grace = %v, want the offline answer", err)
	}
	if got := stateOf(t, index, "far-away"); got != placement.StateOK {
		t.Fatalf("the placement was marked %q because a machine went quiet", got)
	}
}

// ---------------------------------------------------------------------------
// What the markings look like from outside
// ---------------------------------------------------------------------------

// TestOrphanedSandboxIsKeptVisibleAndRefused is what an orphan looks like to
// the people involved, and it is deliberately not a not-found.
//
// The owner is told something true and specific, keeps the sandbox in their
// listing, and keeps the name. A stranger is told what they are told about a
// name nobody ever used — so a machine losing a sandbox cannot become a way to
// find out that somebody else has one.
func TestOrphanedSandboxIsKeptVisibleAndRefused(t *testing.T) {
	index := newIndex(t)
	clock := newClock()
	f := newFleetOn(t, newManager(t, host.Options{NodeName: "boxa"}), index, time.Minute, clock)
	nodeb := newFakeNode("boxb")
	attach(t, f, nodeb)
	seed(t, index, []ledgerRow{{"far-away", "alice", "boxb"}})

	clock.advance(time.Hour)
	f.Reconcile("boxb", inventory("boxb"))
	if got := stateOf(t, index, "far-away"); got != placement.StateOrphaned {
		t.Fatalf("state = %q, want orphaned", got)
	}

	b, ok := f.Get("far-away")
	if !ok {
		t.Fatal("an orphaned sandbox vanished from its owner's view")
	}
	if b.Owner != "alice" || !b.Unreachable {
		t.Fatalf("Get = %+v, want alice's, unreachable", b)
	}
	if got := boxNames(f.ListByOwner("alice")); len(got) != 1 || got[0] != "far-away" {
		t.Fatalf("ListByOwner(alice) = %v", got)
	}
	if got := f.ListByOwner("mallory"); got != nil {
		t.Fatalf("a stranger's listing shows it: %v", boxNames(got))
	}

	// Every mutation is refused, in words that say what happened, and the
	// machine is never asked to do something it has already said it cannot.
	err := f.Pause(context.Background(), "far-away")
	var ce *ctlops.Error
	if !errors.As(err, &ce) {
		t.Fatalf("pause = %v (%T), want a typed conflict", err, err)
	}
	if ce.Kind != ctlops.KindConflict || ce.HTTPStatus() != 409 || ce.ExitCode() != 1 {
		t.Fatalf("pause error = %s/%s, exit %d, status %d", ce.Kind, ce.Code, ce.ExitCode(), ce.HTTPStatus())
	}
	if !strings.Contains(ce.Msg, `"far-away"`) || !strings.Contains(ce.Msg, `"boxb"`) {
		t.Fatalf("the sentence names neither the sandbox nor the machine: %q", ce.Msg)
	}
	if nodeb.took("pause") {
		t.Fatal("the machine was asked to pause a sandbox it has already disclaimed")
	}

	// The name stays reserved: nobody else may take it while the question is
	// open, which is the difference between a marking and a release.
	if _, err := f.Create(context.Background(), "far-away", "mallory", "ubuntu", 1, 512); err == nil {
		t.Fatal("an orphaned name was handed to somebody else")
	}
	if row, ok, _ := index.Get("far-away"); !ok || row.Owner != "alice" {
		t.Fatalf("the row = (%+v, %v), want still alice's", row, ok)
	}

	// And it heals the moment the machine has it again.
	f.Reconcile("boxb", inventory("boxb", reports("far-away", "alice")))
	if got := stateOf(t, index, "far-away"); got != placement.StateOK {
		t.Fatalf("state after the machine reported it again = %q", got)
	}
	if err := f.Pause(context.Background(), "far-away"); err != nil {
		t.Fatalf("pausing after it came back: %v", err)
	}
}

// TestContestedSandboxIsServedToNobody is quarantine's user-visible half:
// routing a name two machines claim would be a coin flip with somebody's work
// on it, so nothing routes at all until an operator has decided.
func TestContestedSandboxIsServedToNobody(t *testing.T) {
	index := newIndex(t)
	clock := newClock()
	f := newFleetOn(t, newManager(t, host.Options{NodeName: "boxa"}), index, time.Minute, clock)
	boxb, boxc := newFakeNode("boxb"), newFakeNode("boxc")
	attach(t, f, boxb)
	attach(t, f, boxc)
	seed(t, index, []ledgerRow{{"far-away", "alice", "boxc"}})
	clock.advance(time.Hour)

	// boxc, which the ledger names, says it does not have it. Then boxb says it
	// does, and nothing knows which disk holds alice's work.
	f.Reconcile("boxc", inventory("boxc"))
	f.Reconcile("boxb", inventory("boxb", reports("far-away", "alice")))
	if got := stateOf(t, index, "far-away"); got != placement.StateQuarantine {
		t.Fatalf("state = %q, want quarantine", got)
	}

	if _, ok := f.Get("far-away"); ok {
		t.Error("a contested name is still served")
	}
	if got := f.ListByOwner("alice"); got != nil {
		t.Errorf("ListByOwner(alice) = %v, want nothing", boxNames(got))
	}
	err := f.Pause(context.Background(), "far-away")
	var ce *ctlops.Error
	if !errors.As(err, &ce) || ce.Kind != ctlops.KindConflict {
		t.Fatalf("pause = %v, want a typed conflict", err)
	}
	if boxb.took("pause") || boxc.took("pause") {
		t.Fatal("a contested name was routed to a machine anyway")
	}

	// It heals too: the machine the ledger names reports it again, the
	// contradiction is over, and the sandbox comes back exactly as it was.
	f.Reconcile("boxc", inventory("boxc", reports("far-away", "alice")))
	if got := stateOf(t, index, "far-away"); got != placement.StateOK {
		t.Fatalf("state after the contradiction ended = %q, want clear", got)
	}
	if b, ok := f.Get("far-away"); !ok || b.Owner != "alice" || b.Node != "boxc" {
		t.Fatalf("Get after healing = (%+v, %v)", b, ok)
	}
}

// ---------------------------------------------------------------------------
// The prohibitions, at the boundaries they are actually about
// ---------------------------------------------------------------------------

// TestReconcileSurvivesAMachineGoingAwayAndComingBack is the three-in-the-
// morning sequence in one test: a machine drops off the network, its rows and
// their last known state are left exactly as they were, and when it comes back
// with a changed inventory the ledger converges on what it now says.
//
// The middle step is the prohibition. Nothing happens to a row because its
// machine went quiet — there is no timer that eventually gives up on one, and
// there must not be.
func TestReconcileSurvivesAMachineGoingAwayAndComingBack(t *testing.T) {
	index := newIndex(t)
	clock := newClock()
	f := newFleetOn(t, newManager(t, host.Options{NodeName: "boxa"}), index, time.Minute, clock)
	nodeb := newFakeNode("boxb")
	attach(t, f, nodeb,
		&host.Sandbox{Name: "one", Owner: "alice", Image: "ubuntu", State: vmm.StateRunning},
		&host.Sandbox{Name: "two", Owner: "alice", Image: "ubuntu", State: vmm.StateRunning})
	seed(t, index, []ledgerRow{{"one", "alice", "boxb"}, {"two", "alice", "boxb"}})
	clock.advance(time.Hour)
	f.Reconcile("boxb", inventory("boxb", reports("one", "alice"), reports("two", "alice")))

	// The machine goes quiet. Not detached — just not answering, which is what
	// a laptop being shut looks like for the length of the grace.
	nodeb.setOnline(false)

	for _, name := range []string{"one", "two"} {
		if got := stateOf(t, index, name); got != placement.StateOK {
			t.Fatalf("%q went to %q while its machine was quiet", name, got)
		}
		b, ok := f.Get(name)
		if !ok {
			t.Fatalf("%q vanished while its machine was quiet", name)
		}
		if !b.Unreachable {
			t.Errorf("%q is not flagged unreachable while its machine is quiet", name)
		}
		// The last thing the machine said is still what is shown. This is the
		// second prohibition from the display side: nothing rewrites a running
		// sandbox as paused because the gateway lost touch with it.
		if b.State != vmm.StateRunning {
			t.Errorf("%q reads as %q, want the last state its machine reported", name, b.State)
		}
		if err := f.Pause(context.Background(), name); !fleet.IsNodeUnreachable(err) {
			t.Errorf("pausing %q while its machine is quiet = %v, want the offline answer", name, err)
		}
	}

	// It comes back having lost one of the two, which is what a disk restored
	// from a snapshot older than a create looks like.
	nodeb.setOnline(true)
	nodeb.forget("two")
	clock.advance(time.Hour)
	ack := f.Reconcile("boxb", inventory("boxb", reports("one", "alice")))

	if got := stateOf(t, index, "one"); got != placement.StateOK {
		t.Errorf("the sandbox that came back is %q", got)
	}
	if got := stateOf(t, index, "two"); got != placement.StateOrphaned {
		t.Errorf("the sandbox that did not is %q, want orphaned", got)
	}
	if !sameNames(ack.Orphaned, []string{"two"}) {
		t.Errorf("ack.Orphaned = %v", ack.Orphaned)
	}
	// Both are still the owner's: the one that came back works, and the one
	// that did not is still a row with her name on it.
	if got := boxNames(f.ListByOwner("alice")); len(got) != 2 {
		t.Errorf("ListByOwner(alice) = %v, want both", got)
	}
	if err := f.Pause(context.Background(), "one"); err != nil {
		t.Errorf("pausing the sandbox that came back: %v", err)
	}
}

// TestBootLeavesAnotherMachinesRecordsAlone is the second prohibition at the
// boundary it is actually about: a gateway restarting.
//
// host.NewManager downgrades every running record it loads to paused, because a
// manager coming up means the process that was running those VMs died with
// them. That is true of the machine it runs on and false of every other one —
// a gateway restarting stops nothing on a node — so the fleet's own boot
// reconciliation touches local rows only. Asserted here by giving the ledger a
// running remote row and rebuilding the whole gateway around it.
func TestBootLeavesAnotherMachinesRecordsAlone(t *testing.T) {
	index := newIndex(t)
	dir := t.TempDir()
	mgr := bootManager(t, dir, "boxa")
	f := newFleet(t, mgr, index)
	mustCreate(t, f, "right-here", "alice")
	seed(t, index, []ledgerRow{{"far-away", "bob", "boxb"}})
	before, ok, err := index.Get("far-away")
	if err != nil || !ok {
		t.Fatalf("seeding: ok=%v err=%v", ok, err)
	}

	// The gateway restarts: a new manager over the same state directory, which
	// is what runs the downgrade, and a new fleet over the same ledger.
	restarted := bootManager(t, dir, "boxa")
	f2 := newFleet(t, restarted, index)

	// The local sandbox took the downgrade, as it should have — this process
	// really did stop it.
	if b, ok := restarted.Get("right-here"); !ok || b.State != vmm.StatePaused {
		t.Fatalf("the local sandbox = (%+v, %v), want paused by its own manager's boot", b, ok)
	}
	// The remote row did not: not its state column, not its owner, not its
	// node, not even its updated_at.
	after, ok, err := index.Get("far-away")
	if err != nil || !ok {
		t.Fatalf("another machine's placement was released at boot: ok=%v err=%v", ok, err)
	}
	if after != before {
		t.Fatalf("another machine's row was rewritten at boot:\n  before %+v\n  after  %+v", before, after)
	}

	// And when that machine reconnects still running it, what it reports is
	// what the gateway serves: nothing downgraded it in the meantime, and
	// nothing asks it to catch up with a state this gateway invented.
	nodeb := newFakeNode("boxb")
	attach(t, f2, nodeb, &host.Sandbox{
		Name: "far-away", Owner: "bob", Image: "ubuntu", State: vmm.StateRunning,
	})
	f2.Reconcile("boxb", inventory("boxb", reports("far-away", "bob")))
	b, ok := f2.Get("far-away")
	if !ok || b.State != vmm.StateRunning {
		t.Fatalf("Get = (%+v, %v), want it still running on boxb", b, ok)
	}
	if got := stateOf(t, index, "far-away"); got != placement.StateOK {
		t.Fatalf("state = %q after the machine reported it running", got)
	}
	if nodeb.took("pause") {
		t.Fatal("the gateway asked another machine to pause a sandbox because IT had restarted")
	}
}
