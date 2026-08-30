package fleet_test

// Placing a sandbox on a machine the caller named, and what everyone reads when
// that machine is not there.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// buildingNode is a fakeNode that can actually build, which fakeNode cannot: it
// exists for the tests that care where a create landed rather than what a
// machine does with one.
type buildingNode struct {
	*fakeNode
	createErr error
}

func newBuildingNode(name string) *buildingNode { return &buildingNode{fakeNode: newFakeNode(name)} }

func (n *buildingNode) Create(_ context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	n.record("create")
	if n.createErr != nil {
		return nil, n.createErr
	}
	// Stamped the way fleet.Remote stamps a record on its way off a link: the
	// node is the link's authenticated name and the addresses are synthetic.
	// That projection is remote.go's and is pinned there; a fake that skipped it
	// would let a test here pass on a record no real machine could produce.
	b := &host.Sandbox{
		Name: name, Owner: owner, Image: image, State: vmm.StateRunning,
		VCPUs: vcpus, MemMB: memMB, Node: n.Name(),
		HostIP:  fleet.Host(name, n.Name()),
		SSHAddr: fleet.Host(name, n.Name()) + ":" + fleet.SSHPort,
	}
	n.mu.Lock()
	n.boxes = append(n.boxes, b)
	n.mu.Unlock()
	return b, nil
}

func attachBuilder(t *testing.T, f *fleet.Fleet, n *buildingNode) {
	t.Helper()
	detach, err := f.Attach(n)
	if err != nil {
		t.Fatalf("attach %s: %v", n.Name(), err)
	}
	t.Cleanup(detach)
}

// The item in one test: a create that names a machine is built there, the
// ledger records it there, and the record the caller gets back says so.
func TestCreateOnANamedMachine(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	nodeb := newBuildingNode("boxb")
	attachBuilder(t, f, nodeb)

	b, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	if !nodeb.took("create") {
		t.Fatal("the named machine was never asked to build anything")
	}
	if _, here := mgr.Get("far-away"); here {
		t.Fatal("the gateway built it as well")
	}
	if b.Node != "boxb" {
		t.Errorf("the returned record says node %q, want the machine it was placed on", b.Node)
	}
	if b.HostIP != fleet.Host("far-away", "boxb") {
		t.Errorf("HostIP = %q, want the synthetic fleet address", b.HostIP)
	}
	row, ok, err := index.Get("far-away")
	if err != nil || !ok {
		t.Fatalf("no ledger row: ok=%v err=%v", ok, err)
	}
	if row.Node != "boxb" || row.Owner != "alice" || row.Arch != "amd64" {
		t.Errorf("row = %+v, want alice's sandbox on boxb (amd64, as boxb reports)", row)
	}
	// And it is now visible to every read, addressed to that machine.
	got, ok := f.Get("far-away")
	if !ok || got.Node != "boxb" {
		t.Fatalf("Get = (%+v, %v)", got, ok)
	}
	if list := f.ListByOwner("alice"); len(list) != 1 || list[0].Name != "far-away" {
		t.Fatalf("ListByOwner(alice) = %s", boxNames(list))
	}
}

// An unnamed create still lands here, whatever else is linked. A gateway that
// started spreading sandboxes the day a second machine joined would move a
// user's work without being asked.
func TestCreateWithoutAPreferenceStaysLocal(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	nodeb := newBuildingNode("boxb")
	attachBuilder(t, f, nodeb)

	b := mustCreate(t, f, "brave-otter", "alice")
	if b.Node != mgr.NodeName() {
		t.Fatalf("an unnamed create landed on %q", b.Node)
	}
	if nodeb.took("create") {
		t.Fatal("an unnamed create was sent to another machine")
	}
}

// Naming this machine is allowed and is the local path: its own manager decides
// whether it can take the sandbox, exactly as it does when nobody names it.
func TestCreateOnTheGatewayItself(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))

	b, err := f.CreateOn(context.Background(), mgr.NodeName(), "brave-otter", "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatalf("CreateOn(local): %v", err)
	}
	if b.Node != mgr.NodeName() {
		t.Fatalf("record says node %q", b.Node)
	}
	if _, ok := mgr.Get("brave-otter"); !ok {
		t.Fatal("the gateway's own manager does not hold it")
	}
}

// Every way a named machine can refuse, and the one thing they have in common:
// nothing is built and no name is left reserved for a sandbox that was never
// made.
func TestCreateOnARefusingMachine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(*buildingNode)
		wantKind ctlops.Kind
		wantCode string
		wantMsg  string
		wantExit int
		wantHTTP int
	}{
		{
			name:     "no such machine",
			setup:    func(n *buildingNode) { n.fakeNode.name = "boxc" },
			wantKind: ctlops.KindNotFound,
			wantCode: "node_not_found",
			wantMsg:  `no node named "boxb"`,
			wantExit: 1,
			wantHTTP: 404,
		},
		{
			name:     "the machine is not answering",
			setup:    func(n *buildingNode) { n.setOnline(false) },
			wantKind: ctlops.KindCapacity,
			wantCode: "node_unreachable",
			wantMsg:  `node "boxb" is offline`,
			wantExit: 1,
			wantHTTP: 503,
		},
		{
			name: "the machine does not have the image",
			setup: func(n *buildingNode) {
				n.facts.Images = []string{"debian"}
			},
			wantKind: ctlops.KindConflict,
			wantCode: "node_cannot_place",
			wantMsg:  `node "boxb" does not have the "ubuntu" image`,
			wantExit: 1,
			wantHTTP: 409,
		},
		{
			name: "the machine is full",
			setup: func(n *buildingNode) {
				n.capacity = host.NodeCapacity{BudgetMemMB: 1024, EffectiveMemMB: 900}
			},
			wantKind: ctlops.KindCapacity,
			wantCode: "node_cannot_place",
			wantMsg:  `node "boxb" is at capacity (900/1024 MB allocated, this needs 512)`,
			wantExit: 1,
			wantHTTP: 503,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newManager(t, host.Options{})
			index := newIndex(t)
			f := newFleet(t, mgr, index)
			nodeb := newBuildingNode("boxb")
			tc.setup(nodeb)
			attachBuilder(t, f, nodeb)

			_, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512)
			var e *ctlops.Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v (%T), want a typed refusal", err, err)
			}
			if e.Kind != tc.wantKind || e.Code != tc.wantCode || e.Msg != tc.wantMsg {
				t.Errorf("refusal = %s/%s %q, want %s/%s %q",
					e.Kind, e.Code, e.Msg, tc.wantKind, tc.wantCode, tc.wantMsg)
			}
			if e.ExitCode() != tc.wantExit || e.HTTPStatus() != tc.wantHTTP {
				t.Errorf("exit %d / HTTP %d, want %d / %d",
					e.ExitCode(), e.HTTPStatus(), tc.wantExit, tc.wantHTTP)
			}
			if nodeb.took("create") {
				t.Error("a refused placement still asked the machine to build")
			}
			if _, ok, _ := index.Get("far-away"); ok {
				t.Error("a refused placement left the name reserved forever")
			}
			if _, ok := mgr.Get("far-away"); ok {
				t.Error("a refused placement fell back to the gateway")
			}
		})
	}
}

// A machine that says nothing about itself is not refused on that silence: an
// unknown is not a no, or a node whose first capacity report has not landed
// would be unusable for a reason nobody could see.
func TestPlacementDoesNotRefuseOnWhatItDoesNotKnow(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	nodeb := newBuildingNode("boxb")
	nodeb.facts = fleet.Facts{Node: "boxb"} // no arch, no image list
	nodeb.capacity = host.NodeCapacity{}    // no report yet
	attachBuilder(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn onto a machine that has reported nothing: %v", err)
	}
	if !nodeb.took("create") {
		t.Fatal("the machine was never asked")
	}
}

// The name is taken in the ledger before the machine is asked, and handed back
// when the machine says no. A name reserved for a sandbox that was never built
// is a name nobody can ever use again.
func TestCreateOnReleasesTheNameWhenTheMachineFails(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	nodeb := newBuildingNode("boxb")
	nodeb.createErr = errors.New("out of disk")
	attachBuilder(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err == nil {
		t.Fatal("CreateOn: want the machine's error")
	}
	if _, ok, _ := index.Get("far-away"); ok {
		t.Fatal("the failed create stranded a row")
	}
}

// countingPlacer is an M5 stand-in: proof that the decision is a seam and not
// a branch inside Create.
type countingPlacer struct {
	calls []fleet.Request
	pick  string
}

func (p *countingPlacer) Place(req fleet.Request, nodes []fleet.Candidate) (string, error) {
	p.calls = append(p.calls, req)
	if p.pick != "" {
		return p.pick, nil
	}
	return "", fmt.Errorf("no machine for %s", req.Owner)
}

func TestSetPlacerDecidesEveryCreate(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	nodeb := newBuildingNode("boxb")
	attachBuilder(t, f, nodeb)

	p := &countingPlacer{pick: "boxb"}
	f.SetPlacer(p)

	// An ordinary create — no --node anywhere — now goes wherever the placer
	// says. That is the whole point: a scheduler replaces this and Create is
	// not edited again.
	if _, err := f.Create(context.Background(), "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !nodeb.took("create") {
		t.Fatal("the placer's choice was ignored")
	}
	if len(p.calls) != 1 {
		t.Fatalf("the placer was consulted %d times", len(p.calls))
	}
	if got := p.calls[0]; got.Owner != "alice" || got.Image != "ubuntu" || got.MemMB != 512 || got.PreferNode != "" {
		t.Errorf("the placer was handed %+v", got)
	}

	// A --node arrives as a preference rather than as a decision, so a
	// scheduler with a reason to say no still can.
	if _, err := f.CreateOn(context.Background(), "boxb", "second", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	if got := p.calls[1]; got.PreferNode != "boxb" {
		t.Errorf("the placer was handed %+v, want PreferNode boxb", got)
	}

	// A placer that refuses is the answer, and nothing is built.
	f.SetPlacer(&countingPlacer{})
	if _, err := f.Create(context.Background(), "third", "alice", "ubuntu", 1, 512); err == nil {
		t.Fatal("a refusing placer still built a sandbox")
	}
	if _, ok, _ := index.Get("third"); ok {
		t.Fatal("a refused placement reserved the name")
	}

	// And nil puts the shipped policy back.
	f.SetPlacer(nil)
	b, err := f.Create(context.Background(), "fourth", "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatalf("Create after SetPlacer(nil): %v", err)
	}
	if b.Node != mgr.NodeName() {
		t.Fatalf("the default policy placed on %q", b.Node)
	}
}

func TestRemoteOnlyPlacerNeverUsesTheGateway(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	f.SetPlacer(fleet.RemoteOnlyPlacer{})

	if _, err := f.Create(context.Background(), "before-node", "alice", "ubuntu", 1, 512); err == nil {
		t.Fatal("a control-plane-only gateway created a local sandbox without a VM node")
	}
	if _, ok := mgr.Get("before-node"); ok {
		t.Fatal("the gateway manager holds a sandbox after a refused create")
	}

	nodec := newBuildingNode("node-c")
	nodeb := newBuildingNode("node-b")
	attachBuilder(t, f, nodec)
	attachBuilder(t, f, nodeb)

	b, err := f.Create(context.Background(), "remote", "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.Node != "node-b" || !nodeb.took("create") {
		t.Fatalf("unnamed create landed on %q; want first name-sorted VM node", b.Node)
	}
	if _, ok := mgr.Get("remote"); ok {
		t.Fatal("the gateway manager holds a remotely placed sandbox")
	}

	if _, err := f.CreateOn(context.Background(), mgr.NodeName(), "not-local", "alice", "ubuntu", 1, 512); err == nil {
		t.Fatal("an explicit request for the gateway bypassed remote-only placement")
	}
}

func TestRemoteOnlyPlacerSkipsOfflineAndRefusingNodes(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	f.SetPlacer(fleet.RemoteOnlyPlacer{})

	offline := newBuildingNode("node-a")
	offline.setOnline(false)
	wrongImage := newBuildingNode("node-b")
	wrongImage.facts.Images = []string{"debian"}
	ready := newBuildingNode("node-c")
	attachBuilder(t, f, offline)
	attachBuilder(t, f, wrongImage)
	attachBuilder(t, f, ready)

	b, err := f.Create(context.Background(), "placed", "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.Node != "node-c" {
		t.Fatalf("create landed on %q, want the first online node that fits", b.Node)
	}
}

// The candidates a placer is handed include machines that are not answering,
// so it can tell "asleep" from "no such machine" — and so can whoever reads
// the sentence it produces.
func TestCandidatesIncludeOfflineMachines(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	nodeb := newBuildingNode("boxb")
	nodeb.setOnline(false)
	attachBuilder(t, f, nodeb)

	var seen []fleet.Candidate
	f.SetPlacer(placerFunc(func(_ fleet.Request, nodes []fleet.Candidate) (string, error) {
		seen = nodes
		return mgr.NodeName(), nil
	}))
	if _, err := f.Create(context.Background(), "brave-otter", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("the placer saw %d machines, want this one and boxb", len(seen))
	}
	if !seen[0].Local || seen[0].Name != mgr.NodeName() || !seen[0].Online {
		t.Errorf("the local candidate is %+v", seen[0])
	}
	if seen[1].Local || seen[1].Name != "boxb" || seen[1].Online {
		t.Errorf("the remote candidate is %+v", seen[1])
	}
	// The local machine's architecture comes from the fleet when its manager was
	// never told one, so a placer cannot conclude that this machine runs nothing.
	if seen[0].Facts.Arch != "arm64" {
		t.Errorf("the local candidate reports arch %q", seen[0].Facts.Arch)
	}
}

type placerFunc func(fleet.Request, []fleet.Candidate) (string, error)

func (f placerFunc) Place(r fleet.Request, c []fleet.Candidate) (string, error) { return f(r, c) }

// Fits is the seam's vocabulary, and the memory arithmetic in it is the one
// number a gateway-side filter gets wrong by default: admission charges the
// working-set reserve under overcommit, not the ceiling.
func TestCandidateFits(t *testing.T) {
	base := fleet.Candidate{
		Name:  "boxb",
		Facts: fleet.Facts{Arch: "arm64", Images: []string{"ubuntu", "debian"}},
		Capacity: host.NodeCapacity{
			BudgetMemMB: 8192, EffectiveMemMB: 4096, ReserveMemMB: 1024,
		},
	}
	for _, tc := range []struct {
		name string
		req  fleet.Request
		want bool // does it fit
	}{
		{"a plain request", fleet.Request{Image: "ubuntu", MemMB: 2048}, true},
		{"an image it does not hold", fleet.Request{Image: "alpine", MemMB: 512}, false},
		{"an unstated image", fleet.Request{MemMB: 512}, true},
		{"the wrong architecture", fleet.Request{Image: "ubuntu", Arch: "amd64"}, false},
		{"the right architecture", fleet.Request{Image: "ubuntu", Arch: "arm64"}, true},
		// 8192 - 4096 = 4096 free. The ceiling is 16384, which does not fit; the
		// reserve is 1024, which does. Overcommit means the reserve is charged,
		// so this is a yes — and a filter comparing MemMB would under-pack the
		// machine by a factor of sixteen.
		{"more than the budget, less than the reserve", fleet.Request{Image: "ubuntu", MemMB: 16384}, true},
		// Under the reserve, the ceiling is charged.
		{"a small sandbox is charged whole", fleet.Request{Image: "ubuntu", MemMB: 512}, true},
		{"no size given", fleet.Request{Image: "ubuntu"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := base.Fits(tc.req)
			if (err == nil) != tc.want {
				t.Fatalf("Fits = %v, want fits=%v", err, tc.want)
			}
		})
	}

	// Overcommit off: the ceiling is what is charged, so a sandbox bigger than
	// what is left is refused.
	noOvercommit := base
	noOvercommit.Capacity.ReserveMemMB = 0
	if err := noOvercommit.Fits(fleet.Request{Image: "ubuntu", MemMB: 16384}); err == nil {
		t.Error("a machine with overcommit off accepted more than its whole budget")
	}
	// And a machine that has reported no budget at all is not refused on it.
	unreported := base
	unreported.Capacity = host.NodeCapacity{}
	if err := unreported.Fits(fleet.Request{Image: "ubuntu", MemMB: 16384}); err != nil {
		t.Errorf("a machine with no capacity report was refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The masking rule
// ---------------------------------------------------------------------------

// TestAnOfflineMachineIsNotAnExistenceOracle is the security half of the
// milestone, and it is two assertions that have to hold at once.
//
// The owner of a sandbox on a machine that is not answering must be told so —
// otherwise a laptop closing looks exactly like a deletion, and the honest
// answer is one a gateway restart used to lose. Anyone else must get the
// byte-identical "no sandbox named" they get for a name that never existed,
// because an error that only appears for real names is a way to enumerate other
// people's sandboxes.
func TestAnOfflineMachineIsNotAnExistenceOracle(t *testing.T) {
	for _, tc := range []struct {
		name string
		// linked says whether the machine is still attached (a heartbeat
		// timeout) or gone entirely (a gateway restart, or a link that dropped).
		linked bool
	}{
		{"the link is still attached but silent", true},
		{"the machine is not attached at all", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newManager(t, host.Options{})
			index := newIndex(t)
			// The row is written before the fleet is built, which is the
			// gateway-restart case exactly: the ledger outlives the process
			// that wrote it, and the sandbox on the other machine outlives
			// both.
			place(t, index, "far-away", "alice", "boxb")
			f := newFleet(t, mgr, index)
			if tc.linked {
				nodeb := newFakeNode("boxb")
				nodeb.setOnline(false)
				attach(t, f, nodeb, &host.Sandbox{
					Name: "far-away", Owner: "alice", Image: "ubuntu", State: vmm.StateRunning,
				})
			}

			ops := ctlops.New(ctlops.Config{
				Sandboxes: f, Templates: f, DefaultImage: "ubuntu", Log: discardLog(),
			})
			t.Cleanup(func() { ops.Close() })
			owner := ctlops.Caller{Handle: "alice"}
			stranger := ctlops.Caller{Handle: "mallory"}
			ctx := context.Background()

			// The owner is told what happened, on every verb.
			for _, op := range []struct {
				name string
				run  func() error
			}{
				{"pause", func() error { _, err := ops.Pause(ctx, owner, "far-away"); return err }},
				{"restore", func() error { _, err := ops.Resume(ctx, owner, "far-away"); return err }},
				{"resize", func() error { _, err := ops.Resize(ctx, owner, "far-away", 30720); return err }},
				{"rename", func() error { _, err := ops.Rename(ctx, owner, "far-away", "renamed"); return err }},
				{"rm", func() error { return ops.Destroy(ctx, owner, "far-away") }},
				{"pin", func() error { _, err := ops.SetPinned(ctx, owner, "far-away", true); return err }},
			} {
				err := op.run()
				if !fleet.IsNodeUnreachable(err) {
					t.Fatalf("%s as the owner = %v, want the machine-is-offline answer", op.name, err)
				}
				var e *ctlops.Error
				if !errors.As(err, &e) {
					t.Fatalf("%s: %T is not typed", op.name, err)
				}
				if want := `sandbox "far-away" lives on node "boxb", which is offline`; e.Msg != want {
					t.Errorf("%s says %q, want %q", op.name, e.Msg, want)
				}
				if e.ExitCode() != 1 || e.HTTPStatus() != 503 {
					t.Errorf("%s exits %d / HTTP %d, want 1 / 503", op.name, e.ExitCode(), e.HTTPStatus())
				}
			}

			// And a stranger gets the answer they get for a name that was never
			// used at all — the same bytes, the same code, the same status.
			ghost := ctlops.AsError("pause", mustFail(t, func() error {
				_, err := ops.Pause(ctx, stranger, "no-such-name")
				return err
			}))
			masked := ctlops.AsError("pause", mustFail(t, func() error {
				_, err := ops.Pause(ctx, stranger, "far-away")
				return err
			}))
			if masked.Msg != `no sandbox named "far-away"` {
				t.Fatalf("a stranger learned something: %q", masked.Msg)
			}
			if masked.Kind != ghost.Kind || masked.Code != ghost.Code ||
				masked.ExitCode() != ghost.ExitCode() || masked.HTTPStatus() != ghost.HTTPStatus() {
				t.Fatalf("a real name on an offline machine answers %s/%s (%d/%d), an invented one %s/%s (%d/%d)",
					masked.Kind, masked.Code, masked.ExitCode(), masked.HTTPStatus(),
					ghost.Kind, ghost.Code, ghost.ExitCode(), ghost.HTTPStatus())
			}
			// The two sentences differ only in the name, which is the caller's
			// own word in both cases.
			if got, want := masked.Msg, `no sandbox named "far-away"`; got != want {
				t.Fatalf("masked = %q, want %q", got, want)
			}

			// A stranger's listing shows nothing either way.
			if got := f.ListByOwner("mallory"); got != nil {
				t.Fatalf("ListByOwner(mallory) = %s", boxNames(got))
			}
			// The owner's listing still holds it, flagged.
			list := f.ListByOwner("alice")
			if len(list) != 1 || list[0].Name != "far-away" || !list[0].Unreachable {
				t.Fatalf("ListByOwner(alice) = %+v", list)
			}
			if list[0].Owner != "alice" || list[0].Node != "boxb" {
				t.Errorf("the record was not rendered from the ledger: %+v", list[0])
			}
		})
	}
}

// A ledger row nobody is answering for is served from the ledger alone, and
// invents nothing the ledger does not hold — in particular no state. There is
// no durable record of what the machine last said, and a state nobody observed
// would be a display that reads as fact.
func TestAnUnansweredRowInventsNothing(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	place(t, index, "far-away", "alice", "boxb")
	f := newFleet(t, mgr, index)

	b, ok := f.Get("far-away")
	if !ok {
		t.Fatal("a row for a machine that is not attached is invisible even to its owner")
	}
	if b.Owner != "alice" || b.Node != "boxb" || b.Image != "ubuntu" {
		t.Errorf("record = %+v, want the ledger's own columns", b)
	}
	if !b.Unreachable {
		t.Error("the record is not flagged unreachable")
	}
	if b.State != "" {
		t.Errorf("state = %q, want nothing at all — no machine has said", b.State)
	}
	if b.HostIP != fleet.Host("far-away", "boxb") || b.GuestV6 != "" {
		t.Errorf("addresses = %q / %q", b.HostIP, b.GuestV6)
	}

	// A quarantined row — a name two machines claim — is still served to
	// nobody, attached or not.
	if err := index.SetRowState("far-away", "boxb", placement.StateOK, placement.StateQuarantine); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Get("far-away"); ok {
		t.Fatal("a quarantined row was served")
	}
	if got := f.ListByOwner("alice"); got != nil {
		t.Fatalf("a quarantined row was listed: %s", boxNames(got))
	}
}

func mustFail(t *testing.T, run func() error) error {
	t.Helper()
	err := run()
	if err == nil {
		t.Fatal("want an error")
	}
	return err
}

// A template captured on a remote machine after its link came up is in that
// machine's live inventory and absent from the image listing it sent at hello.
// A binding-driven create aims at the machine holding the template, so the
// filter reading only the stale listing would refuse exactly that machine, for
// exactly the image it holds.
func TestPlacementSeesANodesLiveTemplates(t *testing.T) {
	const template = "snap-alice-cuda-base"
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	nodeb := newBuildingNode("boxb")
	// Spare capacity on purpose. If the union appended in place instead of
	// cloning, the write would land in this array — the node's own — and the
	// scribble is what the read below catches.
	images := make([]string, 1, 4)
	images[0] = "ubuntu"
	nodeb.facts.Images = images
	nodeb.snaps = []*host.Snapshot{{
		Name: "cuda-base", Owner: "alice", Image: template, FromBox: "elsewhere",
	}}
	attachBuilder(t, f, nodeb)

	b, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", template, 1, 512)
	if err != nil {
		t.Fatalf("create from a template that machine is holding: %v", err)
	}
	if b.Node != "boxb" || !nodeb.took("create") {
		t.Fatalf("the create landed on %q; want the machine with the template", b.Node)
	}
	if len(nodeb.facts.Images) != 1 || images[:2][1] != "" {
		t.Errorf("the union wrote into the node's own image list: %q", images[:2])
	}

	// The same fact as the placer sees it: the template is in the candidate's
	// image list, the hello-time entry survives beside it, and Fits agrees.
	var seen []fleet.Candidate
	f.SetPlacer(placerFunc(func(_ fleet.Request, nodes []fleet.Candidate) (string, error) {
		seen = nodes
		return "boxb", nil
	}))
	if _, err := f.Create(context.Background(), "second", "alice", template, 1, 512); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var remote fleet.Candidate
	for _, c := range seen {
		if c.Name == "boxb" {
			remote = c
		}
	}
	if !slices.Contains(remote.Facts.Images, template) || !slices.Contains(remote.Facts.Images, "ubuntu") {
		t.Fatalf("the candidate's images are %q", remote.Facts.Images)
	}
	if err := remote.Fits(fleet.Request{Image: template, MemMB: 512}); err != nil {
		t.Errorf("Fits refused the machine holding the template: %v", err)
	}
	if err := remote.Fits(fleet.Request{Image: "alpine", MemMB: 512}); err == nil {
		t.Error("the union stopped Fits refusing an image nobody has")
	}
}

// A machine that reported no image list at all is not made refusable by holding
// a template. Fits skips the check on an empty list because unknown must not
// read as refused; replacing that silence with the two names in the inventory
// would start refusing ordinary creates on a machine whose image directory
// merely failed to read.
func TestPlacementDoesNotTurnASilentImageListIntoARefusal(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	nodeb := newBuildingNode("boxb")
	nodeb.facts = fleet.Facts{Node: "boxb"} // no arch, no image list
	nodeb.snaps = []*host.Snapshot{{
		Name: "cuda-base", Owner: "alice", Image: "snap-alice-cuda-base", FromBox: "elsewhere",
	}}
	attachBuilder(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn onto a machine that reported no images: %v", err)
	}
	if !nodeb.took("create") {
		t.Fatal("the machine was never asked")
	}
}
