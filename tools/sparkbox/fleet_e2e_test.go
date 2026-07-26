package sparkbox_test

// Two sparkboxes in one process: a gateway with its own mock-driver manager,
// and a second machine that dials in over a real SSH connection on loopback and
// becomes a member of its fleet. Nothing here is stubbed — a real node roster, a
// real placement ledger, a real nodelink supervisor, a real `ssh ctl@` session
// doing the approving — because the whole point of the item is to find out
// whether the pieces that were unit-tested apart actually meet.
//
// Both machines run the mock driver, and every mock guest reports HostIP
// "127.0.0.1" (internal/vmm/mock, Driver.instance) on an ephemeral port. So the
// two nodes are indistinguishable by address: a test here can only tell them
// apart by which manager holds a record and which link carried a message. That
// is not an inconvenience to work around, it is the property the synthetic
// "<sandbox>.<node>.sandbox.invalid" addressing exists to enforce — on a real
// fleet every node mints the same 172.30.<idx>.2, so an address is never a
// fleet-wide name, and anything up here that dialed one would reach the
// gateway's own VM instead of the one it meant.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// waitFor polls for a condition instead of sleeping a guessed interval, which
// is the idiom the rest of the tree uses (host/manager_test.go,
// sshgw/livesessions_test.go): a link that never comes up fails with a sentence
// naming what was being waited on, and one that comes up in a millisecond costs
// a millisecond.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---------------------------------------------------------------------------
// The gateway
// ---------------------------------------------------------------------------

// fleetStack is the gateway machine: everything newStack builds, plus the three
// things that make it a fleet — the roster a node's key is resolved against,
// the ledger that decides which machine holds a name, and the router that puts
// the two together.
type fleetStack struct {
	*testStack
	// gw is the SSH gateway itself, kept because it is also the live-session
	// registry: a test that wants to know what a pause does to an attached
	// terminal has to be able to attach one.
	gw     *sshgw.Gateway
	roster *nodes.Store
	index  *placement.Store
	flt    *fleet.Fleet
	// fleetLog is the router's own log, kept apart from the gateway's so a test
	// can read what the fleet made of a message without wading through
	// everything else the stack said. syncBuf is e2e_test.go's mutex-guarded
	// buffer, reused here because a slog handler writes from whichever goroutine
	// logged and this one is read from the test's.
	fleetLog *syncBuf
	// strangerKey is a second registered account. It exists so a test can ask
	// what somebody who does not own a sandbox is told about it.
	strangerKey xssh.Signer
	// upstreamPub is the one piece of gateway material that ever crosses a link.
	upstreamPub string

	// The durable half, kept so the volatile half can be rebuilt over it — see
	// restart. A gateway restarting replaces its router, its control plane and
	// its door; it does not replace the state directory, the keys in it or
	// sparkbox.db, and what a restart does to the records in those is the whole
	// question.
	dir         string
	log         *slog.Logger
	hostKey     xssh.Signer
	upstreamKey xssh.Signer
	srv         *gssh.Server
}

// nodeSide is the second machine. It holds no users, no secrets, no ledger and
// no roster: a node is a driver, a manager, and one outbound link.
type nodeSide struct {
	name string
	// dir is this machine's own state directory. The file in it is the point:
	// sandboxes.json is node-local truth, and "the sandbox really is on the
	// other machine" is an assertion about that file and nothing else.
	dir     string
	mgr     *host.Manager
	key     xssh.Signer
	emitter *nodelink.Emitter

	mu      sync.Mutex
	welcome nodelink.Welcome
}

func (n *nodeSide) lastWelcome() nodelink.Welcome {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.welcome
}

// newFleetStack stands up both machines, unlinked. The link is a separate step
// because joining a fleet is a ceremony an operator takes part in — see join —
// and a harness that did it silently at construction would test the parts of it
// that need no human and skip the one that does.
func newFleetStack(t *testing.T) (*fleetStack, *nodeSide) {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	hostKey, err := sshgw.LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatal(err)
	}
	upstreamKey, err := sshgw.LoadOrCreateKey(dir, "gateway_upstream_key")
	if err != nil {
		t.Fatal(err)
	}

	// One operator: an entry in users.conf is what blesses an account, and
	// approving a machine into the fleet is operator-only. And one ordinary
	// second account, because half of what a fleet has to get right is what
	// somebody else's sandbox looks like — see the masking assertions.
	userKey, userPub := newClientKey(t)
	strangerKey, strangerPub := newClientKey(t)
	usersPath := filepath.Join(dir, "users.conf")
	line := fmt.Sprintf("tester %sstranger %s",
		xssh.MarshalAuthorizedKey(userPub), xssh.MarshalAuthorizedKey(strangerPub))
	if err := os.WriteFile(usersPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "sparkbox.db")
	userStore, err := users.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { userStore.Close() })
	if err := users.SeedFile(usersPath, userStore, log); err != nil {
		t.Fatal(err)
	}

	driver := mock.New(dir, hostKey)
	t.Cleanup(func() { driver.Close() })
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver, Logger: log,
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey),
		NodeName:         "gw", Arch: "amd64", Release: "2026-07-22",
		HostMemMB: 16384, MemAdmissionPct: 80, HostVCPUs: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The roster is a further writer on the same sqlite file the identity store
	// already opened, which is how they ship. The placement ledger is opened by
	// the gateway plane below, because that is the half a restart rebuilds.
	roster, err := nodes.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roster.Close() })

	fs := &fleetStack{
		testStack: &testStack{
			mgr: mgr, userKey: userKey, users: userStore,
		},
		roster:      roster,
		strangerKey: strangerKey,
		upstreamPub: sshgw.PublicKeyLine(upstreamKey),
		dir:         dir,
		log:         log,
		hostKey:     hostKey,
		upstreamKey: upstreamKey,
	}
	fs.boot(t)
	return fs, newNodeSide(t, log, sshgw.PublicKeyLine(upstreamKey))
}

// boot builds everything a gateway restart rebuilds — the placement ledger's
// handle, the router, the control plane, the SSH door and the socket — and
// leaves the durable half alone. Calling it twice is a restart; see restart.
func (fs *fleetStack) boot(t *testing.T) {
	t.Helper()
	index, err := placement.Open(filepath.Join(fs.dir, "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })

	fleetLog := &syncBuf{}
	flt, err := fleet.New(fleet.Options{
		Local: fs.mgr, LocalName: "gw", LocalArch: "amd64", Index: index,
		Log: slog.New(slog.NewTextHandler(fleetLog, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { flt.Close() })

	// One control plane for the whole gateway, and deliberately one without a
	// node roster.
	//
	// The adapter that joins the roster to the running fleet is unexported
	// wiring in package main, so a harness out here can only supply a copy of
	// it — and a copy is worse than nothing. This file used to carry one, and
	// what it bought was a full milestone in which `ctl node rm` left the
	// removed machine linked on every real gateway: the copy implemented what
	// the copy needed, the shipped adapter was missing the method ctlops
	// asserts for, and the tests out here could not tell. The operator's half of
	// the fleet — enrol, `node ls`, `node approve`, `node rm` and the link it
	// closes — is therefore tested in cmd/sparkbox against the value serve()
	// actually passes (cmd/sparkbox/wiring_test.go). What is left here is what
	// only two machines and a real connection can show: the link, the
	// handshake, the inventory, the heartbeat and a name defended fleet-wide.
	ops := ctlops.New(ctlops.Config{
		Sandboxes: flt, Templates: flt, Accounts: fs.users,
		DefaultImage: "ubuntu", Domain: "hivemind.tools", Log: fs.log,
	})
	t.Cleanup(func() { ops.Close() })

	gw := sshgw.New(sshgw.GatewayOptions{
		Manager: fs.mgr, Fleet: flt, Dial: flt.DialContext,
		Users: fs.users, HostKey: fs.hostKey, UpstreamKey: fs.upstreamKey,
		DefaultImage: "ubuntu", Logger: fs.log, Domain: "hivemind.tools",
		Nodes: fs.roster, NodeJoiner: flt, NodeEnrol: true, Ops: ops,
	})
	// The one live-session registry, wired exactly as cmd/sparkbox wires it:
	// the manager calls it for a sandbox that pauses here, and the fleet calls
	// it for one that pauses on another machine. Two would mean a pause hanging
	// up half of somebody's terminals.
	fs.mgr.SetSessions(gw)
	flt.SetSessions(gw)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := gw.Server("")
	go srv.Serve(ln) //nolint:errcheck // returns on Close
	t.Cleanup(func() { srv.Close() })

	fs.index, fs.flt, fs.fleetLog, fs.gw, fs.srv = index, flt, fleetLog, gw, srv
	fs.addr = ln.Addr().String()
}

// restart is the gateway process going away and coming back.
//
// It stands the volatile half up again on a NEW socket rather than reclaiming
// the old one, because a test that rebinds an ephemeral port is a test that
// fails on somebody else's machine one day in twenty. From the node's side the
// difference does not matter: what it reconnects to is a gateway with no memory
// of the link it had, which is the whole point — every cached inventory, every
// capacity report and the router's own idea of what is where died with the
// process, and only sparkbox.db and the machines themselves are left.
func (fs *fleetStack) restart(t *testing.T) {
	t.Helper()
	fs.srv.Close() //nolint:errcheck // idempotent; the cleanup closes it again
	fs.boot(t)
}

// newNodeSide builds the second machine. Its state directory and its driver are
// its own — sharing either would make the two managers one machine wearing two
// names, since a mock driver defends a VM name only within itself and both
// managers keep their records in <state-dir>/sandboxes.json.
func newNodeSide(t *testing.T, log *slog.Logger, gatewayPub string) *nodeSide {
	t.Helper()
	dir := t.TempDir()
	key, _ := newClientKey(t)

	// The node's own key doubles as its fake guests' host key, exactly as
	// cmd/sparkbox does in node mode: a node holds no gateway host key, and
	// minting one would leave a gateway identity on a machine that must never
	// be one.
	driver := mock.New(dir, key)
	t.Cleanup(func() { driver.Close() })

	// The emitter is this machine's whole view of its gateway, installed as both
	// the manager's observer and its session closer.
	emitter := nodelink.NewEmitter(log)
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver, Logger: log,
		// The copy a node caches from its last welcome. A machine that has never
		// linked has none and cannot build a sandbox until the first welcome
		// arrives; seeding it here is what lets this node hold a sandbox before
		// the link comes up, which is the only way the handshake's inventory has
		// anything in it to carry.
		GatewayPublicKey: gatewayPub,
		NodeName:         "node-b", Arch: "arm64", Release: "2026-07-22",
		// Room for one default-sized sandbox and not two, which is what lets a
		// test watch this machine refuse the second one in its own words.
		HostMemMB: 16384, MemAdmissionPct: 75, HostVCPUs: 8,
		Observer: emitter,
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetSessions(emitter)
	return &nodeSide{name: "node-b", dir: dir, mgr: mgr, key: key, emitter: emitter}
}

// holds reports whether this machine's own state file names a sandbox. It reads
// the JSON rather than asking the manager because the manager is in this
// process and the file is what survives it — "the sandbox is on node-b" means
// node-b would still have it after a reboot.
func (n *nodeSide) holds(t *testing.T, name string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(n.dir, "sandboxes.json"))
	if err != nil {
		t.Fatalf("reading %s's state file: %v", n.name, err)
	}
	// The file is the manager's own map, keyed by sandbox name.
	var boxes map[string]json.RawMessage
	if err := json.Unmarshal(raw, &boxes); err != nil {
		t.Fatalf("parsing %s's state file: %v", n.name, err)
	}
	_, ok := boxes[name]
	return ok
}

// join runs the whole enrolment ceremony: the machine dials, is told it is
// pending, its row is approved, and the next attempt links. Nothing is hurried
// past — the retry loop is what an operator's delay looks like to a node, so the
// harness lives through it rather than pre-approving the key and skipping it.
//
// The approval is written straight to the roster because the operator's door on
// to it lives in cmd/sparkbox, whose adapter this package cannot import; what an
// operator sees and does is tested there, against the shipped one. All this
// harness needs from the approval is its effect on the row a node's next dial
// is resolved against, which is the same write either way.
// It returns the function that unplugs the machine: cancelling the supervisor
// is what a node being switched off looks like from up here, and it is the only
// honest way to test what a user is told when their machine is not there. The
// cleanup runs it too, so a test that never unplugs is unaffected.
func (fs *fleetStack) join(t *testing.T, n *nodeSide) (unplug func()) {
	t.Helper()
	unplug = fs.dial(t, n)

	// First contact records a pending row and buys nothing at all.
	waitFor(t, "node-b to enrol itself", func() bool {
		_, err := fs.roster.Get(n.name)
		return err == nil
	})
	if fs.flt.Online(n.name) {
		t.Fatal("an unapproved machine joined the fleet; enrolling must grant nothing")
	}

	// Approval is keyed on the key's fingerprint, not the name — which is the
	// operator ceremony too: read the fingerprint off the row, compare it to the
	// one the machine printed, approve that.
	row, err := fs.roster.Get(n.name)
	if err != nil {
		t.Fatalf("reading the pending row for %s: %v", n.name, err)
	}
	if err := fs.roster.ApproveFP(row.FP, "tester"); err != nil {
		t.Fatalf("approving %s: %v", n.name, err)
	}
	fs.awaitLink(t, n)
	return unplug
}

// relink is join for a machine whose key is already approved: a node
// reconnecting to a gateway that has restarted goes through no ceremony at all,
// because approval is a durable row and the operator did it once.
func (fs *fleetStack) relink(t *testing.T, n *nodeSide) (unplug func()) {
	t.Helper()
	unplug = fs.dial(t, n)
	fs.awaitLink(t, n)
	return unplug
}

func (fs *fleetStack) awaitLink(t *testing.T, n *nodeSide) {
	t.Helper()
	waitFor(t, "the fleet to see "+n.name+" online", func() bool { return fs.flt.Online(n.name) })
	// The gateway registers a link and only then writes the welcome
	// (nodelink.Accept), so "online" up here does not yet mean the node has
	// installed what the welcome carried. Wait for the node's own side of the
	// handshake as well, or anything reading lastWelcome races its last step.
	waitFor(t, n.name+" to install its welcome", func() bool { return n.lastWelcome().Node == n.name })
}

// dial starts the machine's link supervisor against whatever address the
// gateway is listening on right now, and returns the function that unplugs it.
func (fs *fleetStack) dial(t *testing.T, n *nodeSide) (unplug func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- nodelink.RunClient(ctx, nodelink.ClientOptions{
			Gateway: fs.addr, NodeName: n.name, Key: n.key,
			Manager: n.mgr, Emitter: n.emitter,
			Hello: func() nodelink.Hello {
				return nodelink.Hello{
					Arch: "arm64", Release: "2026-07-22", Version: "test",
					Driver: "mock", GuestSubnet: "172.30.0.0/16",
					Archiving: n.mgr.ArchivingEnabled(), Snapshots: n.mgr.Snapshotter(),
				}
			},
			OnWelcome: func(w nodelink.Welcome) error {
				n.mu.Lock()
				defer n.mu.Unlock()
				n.welcome = w
				return nil
			},
			// Real cadences measured in seconds would make this test a minute
			// long; the machinery under them is the same either way.
			BackoffMin: 10 * time.Millisecond, BackoffMax: 50 * time.Millisecond,
			Heartbeat: 50 * time.Millisecond,
			Log:       slog.New(slog.DiscardHandler),
		})
	}()
	stopped := false
	unplug = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the node supervisor returned %v, want context.Canceled", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the node supervisor did not stop when its context was cancelled")
		}
	}
	t.Cleanup(unplug)
	return unplug
}

// capacityOf is the fleet's picture of one machine's resources.
func (fs *fleetStack) capacityOf(node string) (host.NodeCapacity, bool) {
	for _, c := range fs.flt.Capacities() {
		if c.Node == node {
			return c, true
		}
	}
	return host.NodeCapacity{}, false
}

// ---------------------------------------------------------------------------
// The tests
// ---------------------------------------------------------------------------

// TestFleetNodeJoinsAndReports is the milestone in one test: a second machine
// enrols, is approved, and from then on the gateway knows what that machine is,
// what it holds and what it has left — none of which it can observe any other
// way, because the machine is over a network and every address it could quote
// means something else up here.
func TestFleetNodeJoinsAndReports(t *testing.T) {
	fs, node := newFleetStack(t)
	ctx := context.Background()

	// A sandbox that exists before the link, so the handshake's inventory has
	// something to carry. This is also the ordinary case on a real fleet: a node
	// reboots, resumes its pinned VMs, and only then finds its gateway.
	if _, err := node.mgr.Create(ctx, "demo-b", "tester", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	fs.join(t, node)

	// The welcome is the only gateway material that ever crosses a link, and
	// installing it is the only thing a node does with one.
	if got := node.lastWelcome(); got.GatewayUpstreamPub != fs.upstreamPub {
		t.Errorf("welcome carried %q, want the gateway's upstream key %q", got.GatewayUpstreamPub, fs.upstreamPub)
	} else if got.Node != "node-b" || got.Protocol != nodelink.Protocol {
		t.Errorf("welcome = %+v, want node-b on protocol %d", got, nodelink.Protocol)
	}

	// Registration: the roster now records what the machine said about itself,
	// which is display-only and node-authored — nothing authorizes on it.
	row, err := fs.roster.Get("node-b")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != nodes.StatusApproved || row.ApprovedBy != "tester" {
		t.Errorf("roster row = %+v, want approved by tester", row)
	}
	if row.Arch != "arm64" || row.LastSeen == nil {
		t.Errorf("roster row = %+v, want arch arm64 and a last-seen stamp", row)
	}

	// And the fleet holds two machines, both answering. What an operator reads
	// off that — the rendered `node ls`, with the fingerprint they compared — is
	// asserted in cmd/sparkbox, where the roster adapter that produces it is the
	// shipped one rather than a copy written to satisfy this file.
	var linked []string
	for _, st := range fs.flt.Nodes() {
		if st.Online {
			linked = append(linked, st.Name)
		}
	}
	if len(linked) != 2 {
		t.Fatalf("the fleet holds %v, want this gateway and node-b both online", linked)
	}

	// Inventory: the node's whole picture arrived. The link caches it and the
	// fleet can now project it (fleet.Remote), but a listing still shows nothing
	// until a ledger row places one of those names here — the row is what every
	// authorization decision about a remote sandbox is made from. So the
	// gateway's own record of taking the inventory is the observable here.
	waitFor(t, "the gateway to take node-b's inventory", func() bool {
		logged := fs.fleetLog.String()
		return strings.Contains(logged, "node inventory") && strings.Contains(logged, "sandboxes=1")
	})

	// Capacity: what the machine has left, as its own manager computes it. The
	// two fields the gateway adds are the ones a machine cannot know about
	// itself — whether anyone can still hear it, and when it was last heard.
	waitFor(t, "node-b's capacity to arrive intact", func() bool {
		got, ok := fs.capacityOf("node-b")
		return ok && aggregated(got) == node.mgr.Capacity()
	})
	got, _ := fs.capacityOf("node-b")
	if got.LastSeenAt == nil {
		t.Error("node-b's capacity carries no last-seen stamp; the gateway stamps one on arrival")
	}
	if !got.Online {
		t.Error("node-b's capacity says offline while its link is up")
	}
	if want := node.mgr.Capacity(); aggregated(got) != want {
		t.Errorf("node-b reports %+v, its manager says %+v", aggregated(got), want)
	}

	// Heartbeat: capacity keeps following the machine after the handshake. A
	// pause changes what its manager reports, and only a heartbeat (or another
	// inventory) can carry that — a lifecycle event carries none.
	if err := node.mgr.Pause(ctx, "demo-b"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a heartbeat carrying node-b's pause", func() bool {
		c, ok := fs.capacityOf("node-b")
		return ok && c.Running == 0 && c.Sandboxes == 1
	})
}

// aggregated strips the two fields an aggregator fills in, leaving what the
// machine itself reported. A manager describing its own resources is online by
// definition and leaves LastSeenAt nil, since "now" carries no information.
func aggregated(c host.NodeCapacity) host.NodeCapacity {
	c.LastSeenAt = nil
	c.Online = true
	return c
}

// TestFleetNameAllocationIsFleetWide is the regression a fleet makes possible
// and a single box cannot have. mock.Driver refuses a duplicate name only
// within one Driver, and firecracker only within one machine's directory, so
// two nodes will each happily build a `demo` — the name is defended fleet-wide
// by the placement ledger's PRIMARY KEY and by nothing else. A gateway that
// consulted only its own manager would hand one user's name to another user's
// sandbox on another machine, and the two would be reachable under one name.
func TestFleetNameAllocationIsFleetWide(t *testing.T) {
	fs, node := newFleetStack(t)
	ctx := context.Background()
	fs.join(t, node)

	if _, err := node.mgr.Create(ctx, "demo", "tester", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	// The row the gateway writes when it places a sandbox on another machine.
	// It is written by hand here because placing one is M2 and this is M1; the
	// columns are the ones fleet.reserve fills, and they are what every
	// authorization decision about that sandbox reads.
	if err := fs.index.Reserve("demo", "tester", "node-b", "ubuntu", "arm64"); err != nil {
		t.Fatal(err)
	}

	// Nothing on this machine has an opinion about the name: its manager has no
	// such record, and its driver would build one without complaint.
	if _, ok := fs.mgr.Get("demo"); ok {
		t.Fatal("the gateway's own manager holds a sandbox only node-b built")
	}

	_, err := fs.flt.Create(ctx, "demo", "tester", "ubuntu", 1, 512)
	if err == nil {
		t.Fatal("the fleet built a second `demo` on the gateway while node-b holds one")
	}
	var taken *host.NameError
	if !errors.As(err, &taken) || taken.Problem != host.NameTaken || taken.Noun != "sandbox" {
		t.Fatalf("create refused with %v (%T), want a taken sandbox name", err, err)
	}
	if _, ok := fs.mgr.Get("demo"); ok {
		t.Error("the name was refused but a VM was built anyway; a rejected reservation must build nothing")
	}

	// And the refusal reads exactly like a collision with one of this machine's
	// own sandboxes — same error type, same sentence, same exit code, same HTTP
	// status — because a user has no business knowing that a fleet is involved.
	if _, err := fs.mgr.Create(ctx, "demo", "tester", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	_, local := fs.mgr.Create(ctx, "demo", "tester", "ubuntu", 1, 512)
	if local == nil {
		t.Fatal("the local manager built two sandboxes called `demo`")
	}
	if local.Error() != err.Error() {
		t.Errorf("a remote collision reads %q, a local one %q", err, local)
	}
	remoteInfo, localInfo := ctlops.AsError("create", err), ctlops.AsError("create", local)
	if remoteInfo.Kind != localInfo.Kind || remoteInfo.Code != localInfo.Code {
		t.Errorf("remote collision classified as %s/%s, local as %s/%s",
			remoteInfo.Kind, remoteInfo.Code, localInfo.Kind, localInfo.Code)
	}
	if remoteInfo.ExitCode() != localInfo.ExitCode() || remoteInfo.HTTPStatus() != localInfo.HTTPStatus() {
		t.Errorf("remote collision exits %d/%d, local %d/%d",
			remoteInfo.ExitCode(), remoteInfo.HTTPStatus(), localInfo.ExitCode(), localInfo.HTTPStatus())
	}
}

// ---------------------------------------------------------------------------
// Placing on a named machine
// ---------------------------------------------------------------------------

// session runs one command over SSH as a given key and returns everything a
// user would see: stdout, stderr and the exit status. It does not fail on a
// non-zero exit, because half of what is under test here is which exit status a
// person gets — testStack.run cannot be used for that.
func (fs *fleetStack) session(t *testing.T, signer xssh.Signer, user, cmd string) (stdout, stderr string, code int) {
	t.Helper()
	client := fs.dialAs(t, signer, user)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// One buffer per stream: x/crypto copies the two on separate goroutines, so
	// a shared bare bytes.Buffer races and drops output (see syncBuf).
	var out, errs syncBuf
	sess.Stdout, sess.Stderr = &out, &errs
	code = 0
	if err := sess.Run(cmd); err != nil {
		var exit *xssh.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run %q as %s: %v (stderr: %s)", cmd, user, err, errs.String())
		}
		code = exit.ExitStatus()
	}
	return out.String(), errs.String(), code
}

func (fs *fleetStack) ctl(t *testing.T, cmd string) (string, string, int) {
	t.Helper()
	return fs.session(t, fs.userKey, sshgw.ControlUser, cmd)
}

// TestFleetPlacesOnANamedNode is the milestone: `ssh new@gw -- --node node-b`
// builds the sandbox on the other machine, and every command that follows finds
// it there.
//
// The absence of a tag store is load-bearing. This stack wires none, so any word
// that reaches the door as a tag fails the create with "tagging is not enabled".
// A `--node` that were merely unrecognised would arrive as the two bare words
// `--node` and `node-b`, which the door folds into the tag list — so the create
// succeeding at all is the assertion that the flag was understood and not
// silently swallowed.
func TestFleetPlacesOnANamedNode(t *testing.T) {
	fs, node := newFleetStack(t)
	fs.join(t, node)
	ctx := context.Background()

	// The door creates, prints its banner, and only then tries to open a shell
	// inside the guest — which cannot happen across a link yet (the data plane
	// is the next milestone), so the session ends non-zero. The banner is what
	// this test is here for.
	_, banner, _ := fs.session(t, fs.userKey, sshgw.NewSandboxUser+"+far-away", "--node node-b")
	if !strings.Contains(banner, `created sandbox "far-away"`) {
		t.Fatalf("the create did not happen: %q", banner)
	}
	if !strings.Contains(banner, "on node-b") {
		t.Errorf("the banner does not say where it landed: %q", banner)
	}

	// The three places that have to agree, and the one that must not.
	row, ok, err := fs.index.Get("far-away")
	if err != nil || !ok {
		t.Fatalf("no ledger row: ok=%v err=%v", ok, err)
	}
	if row.Node != "node-b" || row.Owner != "tester" || row.Arch != "arm64" {
		t.Errorf("row = %+v, want tester's sandbox on node-b (arm64)", row)
	}
	if !node.holds(t, "far-away") {
		t.Error("node-b's own sandboxes.json does not hold it")
	}
	if _, here := fs.mgr.Get("far-away"); here {
		t.Error("the gateway built it too")
	}

	// And it reads as an ordinary sandbox from the control plane.
	out, _, code := fs.ctl(t, "list")
	if code != 0 || !strings.Contains(out, "far-away") {
		t.Fatalf("ctl list = %q (exit %d)", out, code)
	}

	// Every lifecycle verb, round-tripped against the machine that holds it.
	// Each assertion is against node-b's OWN manager, which is the only thing
	// that can tell a command that travelled from one that was quietly handled
	// here.
	for _, step := range []struct {
		cmd   string
		check func() bool
		what  string
	}{
		{"pause far-away", func() bool { b, _ := node.mgr.Get("far-away"); return b.State == "paused" }, "paused on node-b"},
		{"restore far-away", func() bool { b, _ := node.mgr.Get("far-away"); return b.State == "running" }, "running on node-b"},
		{"pin far-away", func() bool { b, _ := node.mgr.Get("far-away"); return b.Pinned }, "pinned on node-b"},
		{"unpin far-away", func() bool { b, _ := node.mgr.Get("far-away"); return !b.Pinned }, "unpinned on node-b"},
		{"resize far-away 30G", func() bool { b, _ := node.mgr.Get("far-away"); return b.DiskTotalMB >= 30720 }, "grown on node-b"},
	} {
		if out, errs, code := fs.ctl(t, step.cmd); code != 0 {
			t.Fatalf("ctl %s exited %d: %s%s", step.cmd, code, out, errs)
		}
		if !step.check() {
			b, _ := node.mgr.Get("far-away")
			t.Fatalf("after `%s` the sandbox is not %s: %+v", step.cmd, step.what, b)
		}
	}

	// Rename has no ctl@ door — it is a REST verb — so it is driven through the
	// router itself. It is the one verb the gateway does half of: the ledger row
	// moves first and is rolled back if the machine refuses.
	if err := fs.flt.Rename(ctx, "far-away", "far-off", "tester"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, ok := node.mgr.Get("far-off"); !ok {
		t.Error("node-b did not rename it")
	}
	// The rename moved the ledger row with it, which is what keeps the name
	// routable at all.
	if _, ok, _ := fs.index.Get("far-away"); ok {
		t.Error("the old name still holds a row")
	}
	if row, ok, _ := fs.index.Get("far-off"); !ok || row.Node != "node-b" {
		t.Errorf("the row did not follow the rename: ok=%v row=%+v", ok, row)
	}

	// And rm releases the name only after the machine has let go of it.
	if out, errs, code := fs.ctl(t, "rm far-off"); code != 0 {
		t.Fatalf("ctl rm exited %d: %s%s", code, out, errs)
	}
	if node.holds(t, "far-off") {
		t.Error("node-b's state file still holds a destroyed sandbox")
	}
	if _, ok, _ := fs.index.Get("far-off"); ok {
		t.Error("the placement outlived the sandbox")
	}
}

// A machine this fleet does not have is a plain answer, not a masked one: node
// names are published so people can place work on them.
func TestFleetRefusesAnUnknownNode(t *testing.T) {
	fs, node := newFleetStack(t)
	fs.join(t, node)

	_, banner, code := fs.session(t, fs.userKey, sshgw.NewSandboxUser+"+ghosted", "--node ghost")
	if !strings.Contains(banner, `no node named "ghost"`) {
		t.Fatalf("stderr = %q, want the unknown-machine sentence", banner)
	}
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if _, ok, _ := fs.index.Get("ghosted"); ok {
		t.Error("a refused placement reserved the name")
	}

	// A flag with no value is a mistyped command, which exits 2 everywhere else
	// in this tree.
	_, banner, code = fs.session(t, fs.userKey, sshgw.NewSandboxUser, "--node")
	if code != 2 || !strings.Contains(banner, "--node needs a value") {
		t.Fatalf("`--node` with no value: exit %d, stderr %q", code, banner)
	}
	if got := len(fs.mgr.List()); got != 0 {
		t.Errorf("a mistyped flag built %d sandboxes here", got)
	}
}

// TestFleetSandboxSurvivesItsMachineGoingAway is the other half of the
// milestone, and the one with a security property in it.
//
// A machine being switched off must not look like a deletion to its owner, and
// must not become a way for anybody else to find out that the name exists. Both
// answers are asserted here against the shipped renderers, because "the same
// bytes" is a claim about what a person actually sees.
func TestFleetSandboxSurvivesItsMachineGoingAway(t *testing.T) {
	fs, node := newFleetStack(t)
	unplug := fs.join(t, node)

	_, banner, _ := fs.session(t, fs.userKey, sshgw.NewSandboxUser+"+far-away", "--node node-b")
	if !strings.Contains(banner, `created sandbox "far-away"`) {
		t.Fatalf("the create did not happen: %q", banner)
	}

	// The machine goes away. Its link ends, it stops being a member of the
	// fleet, and its sandbox goes on existing on a disk this gateway cannot
	// reach.
	unplug()
	waitFor(t, "the fleet to lose node-b", func() bool {
		for _, st := range fs.flt.Nodes() {
			if st.Name == "node-b" {
				return false
			}
		}
		return true
	})

	// The owner is told what happened, in one sentence, at exit 1.
	_, errs, code := fs.ctl(t, "pause far-away")
	if want := `sparkbox: sandbox "far-away" lives on node "node-b", which is offline` + "\r\n"; errs != want {
		t.Fatalf("the owner reads %q, want %q", errs, want)
	}
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	// And still sees it in their own listing rather than losing it.
	if out, _, _ := fs.ctl(t, "list"); !strings.Contains(out, "far-away") {
		t.Errorf("the owner's listing lost the sandbox: %q", out)
	}

	// Anybody else gets the answer they get for a name nobody ever used — the
	// same bytes and the same exit status, so an outage cannot be used to
	// enumerate other people's sandboxes.
	_, mine, mineCode := fs.session(t, fs.strangerKey, sshgw.ControlUser, "pause far-away")
	_, ghost, ghostCode := fs.session(t, fs.strangerKey, sshgw.ControlUser, "pause no-such-sandbox")
	if mine != `sparkbox: no sandbox named "far-away"`+"\r\n" {
		t.Fatalf("a stranger reads %q", mine)
	}
	if strings.Replace(mine, "far-away", "no-such-sandbox", 1) != ghost {
		t.Fatalf("a real name answers %q and an invented one %q", mine, ghost)
	}
	if mineCode != ghostCode {
		t.Fatalf("a real name exits %d and an invented one %d", mineCode, ghostCode)
	}
	if out, _, _ := fs.session(t, fs.strangerKey, sshgw.ControlUser, "list"); strings.Contains(out, "far-away") {
		t.Errorf("a stranger's listing shows it: %q", out)
	}
}

// TestFleetSurvivesAGatewayRestart is reconciliation end to end, and it is the
// case a fleet exists to make survivable: the gateway is the one process that
// can be restarted without stopping anybody's work, because the work is not on
// it.
//
// The sequence is the one an operator actually performs — deploy a new binary,
// which drops every link — with the machine's sandbox running throughout. What
// has to hold across it is that the ledger outlives the process, that the
// gateway's picture of a machine it has not heard from yet is honest rather
// than absent, and that the machine coming back converges the two without being
// asked to do anything to its VMs.
//
// The last clause is the second prohibition, and it is asserted rather than
// assumed: the sandbox is still RUNNING on node-b after the reconnect. A
// gateway that applied its own boot reconciliation to another machine's records
// would have paused it, and the user would find their work stopped by a deploy
// they were told was invisible to them.
func TestFleetSurvivesAGatewayRestart(t *testing.T) {
	fs, node := newFleetStack(t)
	unplug := fs.join(t, node)

	_, banner, _ := fs.session(t, fs.userKey, sshgw.NewSandboxUser+"+far-away", "--node node-b")
	if !strings.Contains(banner, `created sandbox "far-away"`) {
		t.Fatalf("the create did not happen: %q", banner)
	}
	if !node.holds(t, "far-away") {
		t.Fatal("far-away is not on node-b, so this test would prove nothing")
	}

	// The gateway goes away and comes back. The node's link dies with it and
	// its supervisor starts backing off; nothing on node-b stops.
	unplug()
	fs.restart(t)
	if b, ok := node.mgr.Get("far-away"); !ok || b.State != "running" {
		t.Fatalf("the restart reached node-b's VM: %+v", b)
	}

	// The new process has never spoken to node-b. What it knows is the ledger,
	// and the ledger is enough to tell the owner the truth: their sandbox
	// exists, it is theirs, and it cannot be reached right now.
	out, _, code := fs.ctl(t, "list")
	if code != 0 || !strings.Contains(out, "far-away") {
		t.Fatalf("the owner's listing after a restart = %q (exit %d)", out, code)
	}
	_, errs, code := fs.ctl(t, "pause far-away")
	if want := `sparkbox: sandbox "far-away" lives on node "node-b", which is offline` + "\r\n"; errs != want {
		t.Fatalf("the owner reads %q, want %q", errs, want)
	}
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	// And a stranger reads what they read about a name nobody ever used, so a
	// deploy cannot be used to enumerate other people's sandboxes either.
	_, mine, _ := fs.session(t, fs.strangerKey, sshgw.ControlUser, "pause far-away")
	if mine != `sparkbox: no sandbox named "far-away"`+"\r\n" {
		t.Fatalf("a stranger reads %q", mine)
	}

	// While the gateway was down, somebody worked on node-b directly — which is
	// also exactly what an interrupted create leaves behind, and is the reason
	// adoption exists.
	if _, err := node.mgr.Create(context.Background(), "made-offline", "tester", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}

	// node-b reconnects, with no ceremony: its approval is a durable row.
	fs.relink(t, node)

	waitFor(t, "the gateway to reconcile node-b's inventory", func() bool {
		b, ok := fs.flt.Get("far-away")
		return ok && !b.Unreachable
	})
	// The row was never released and never re-marked: it is the same placement
	// it was before the restart.
	row, ok, err := fs.index.Get("far-away")
	if err != nil || !ok {
		t.Fatalf("the placement did not survive: ok=%v err=%v", ok, err)
	}
	if row.Node != "node-b" || row.Owner != "tester" || row.State != "" {
		t.Fatalf("row = %+v, want tester's, on node-b, unmarked", row)
	}
	// The sandbox is still running. Nothing about a gateway restart may reach
	// another machine's VMs.
	if b, ok := node.mgr.Get("far-away"); !ok || b.State != "running" {
		t.Fatalf("node-b's sandbox = %+v, want still running", b)
	}
	if b, _ := fs.flt.Get("far-away"); b.State != "running" {
		t.Fatalf("the gateway shows it as %q, want what node-b reports", b.State)
	}
	// And it is operable again, which is the whole point of converging.
	if out, errs, code := fs.ctl(t, "pause far-away"); code != 0 {
		t.Fatalf("ctl pause after the restart exited %d: %s%s", code, out, errs)
	}
	if b, _ := node.mgr.Get("far-away"); b.State != "paused" {
		t.Fatalf("the pause did not reach node-b: %+v", b)
	}

	// The sandbox built while the gateway was down was adopted: a running VM
	// nobody could reach by name is the failure this converges away.
	adopted, ok, err := fs.index.Get("made-offline")
	if err != nil || !ok {
		t.Fatalf("a sandbox built while the gateway was down was not adopted: ok=%v err=%v", ok, err)
	}
	if adopted.Node != "node-b" || adopted.Owner != "tester" {
		t.Fatalf("adopted row = %+v, want tester's sandbox on node-b", adopted)
	}
	if out, _, _ := fs.ctl(t, "list"); !strings.Contains(out, "made-offline") {
		t.Errorf("the adopted sandbox is not in its owner's listing: %q", out)
	}
	if out, _, _ := fs.session(t, fs.strangerKey, sshgw.ControlUser, "list"); strings.Contains(out, "made-offline") {
		t.Errorf("an adopted sandbox is visible to a stranger: %q", out)
	}
}

// A machine refuses in its own words, and the user reads the sentence they
// would have read if the machine had been this one.
//
// This is the whole payoff of keeping the error typed across the link: the
// friendly capacity guidance in sshgw.failStart branches on errors.As against
// the concrete *host.CapacityError, so a refusal that arrived as a string, or
// as a generic 500, would silently fall through to `sparkbox: create sandbox
// failed: …` and lose the one line telling the user what to do about it.
func TestFleetRendersTheMachinesOwnRefusal(t *testing.T) {
	fs, node := newFleetStack(t)
	fs.join(t, node)

	// Both machines have room for exactly one default-sized sandbox, so the
	// second create on each is refused by that machine's own admission.
	fill := func(user, args string) string {
		t.Helper()
		_, errs, _ := fs.session(t, fs.userKey, user, args)
		return errs
	}
	if got := fill(sshgw.NewSandboxUser+"+first-there", "--node node-b"); !strings.Contains(got, "created sandbox") {
		t.Fatalf("the first create on node-b did not happen: %q", got)
	}
	remote := fill(sshgw.NewSandboxUser+"+second-there", "--node node-b")

	if got := fill(sshgw.NewSandboxUser+"+first-here", ""); !strings.Contains(got, "created sandbox") {
		t.Fatalf("the first create here did not happen: %q", got)
	}
	local := fill(sshgw.NewSandboxUser+"+second-here", "")

	if !strings.Contains(local, "host is at capacity") {
		t.Fatalf("the local refusal is not the capacity sentence: %q", local)
	}
	// The two machines have different budgets, so the numbers differ and
	// nothing else may. Stripping the digits is what "the identical sentence"
	// means here.
	if digits.ReplaceAllString(remote, "#") != digits.ReplaceAllString(local, "#") {
		t.Fatalf("node-b's refusal reads\n  %q\nand this machine's reads\n  %q", remote, local)
	}
	if node.holds(t, "second-there") {
		t.Error("the refused sandbox was built anyway")
	}
	if _, ok, _ := fs.index.Get("second-there"); ok {
		t.Error("the refused create left the name reserved")
	}
}

var digits = regexp.MustCompile(`[0-9]+`)

// fakeTerminal is a session attached to a sandbox, as the gateway's registry
// sees one: somewhere to write the goodbye and something to close. It is the
// same shape internal/xterm registers a browser tab through.
type fakeTerminal struct {
	mu     sync.Mutex
	buf    strings.Builder
	closed bool
}

func (f *fakeTerminal) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(p)
}

func (f *fakeTerminal) Read([]byte) (int, error) { return 0, io.EOF }
func (f *fakeTerminal) Stderr() io.ReadWriter    { return f }

func (f *fakeTerminal) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeTerminal) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeTerminal) written() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

// TestFleetPauseClosesTheSessionWithTheNodesWording is the user-visible half of
// the fleet's event stream.
//
// A sandbox that pauses takes its sessions with it, and every session in a
// fleet terminates HERE — the machine holding the VM has none to close. Without
// the relay the user's terminal is simply left pointing at a VM that stopped
// answering: the connection stays open with nothing on the far end, the ssh
// client never exits, and whatever full-screen program was running keeps its
// mouse reporting and alternate screen latched on. That is the wedged terminal
// internal/sshgw exists to prevent locally, and a sandbox on another machine
// must not be a way to get it back.
//
// The wording is the second half, and it is why this is a relay rather than a
// message composed up here. "went idle for 0s" is the node's reaper describing
// its own threshold — a setting of that machine, which this gateway does not
// know and cannot restate. So it is proof of provenance as well as a sentence:
// nothing here could have written it.
//
// The two goodbyes are then compared with the name and the reason factored out,
// because "the same experience" is a claim about bytes — the escape sequences
// that undo mouse reporting and leave the alternate screen included.
func TestFleetPauseClosesTheSessionWithTheNodesWording(t *testing.T) {
	fs, node := newFleetStack(t)
	fs.join(t, node)

	// One sandbox on each machine, each with a terminal attached to it.
	for _, c := range []struct{ name, args string }{
		{"far-away", "--node node-b"},
		{"right-here", ""},
	} {
		_, banner, _ := fs.session(t, fs.userKey, sshgw.NewSandboxUser+"+"+c.name, c.args)
		if !strings.Contains(banner, `created sandbox "`+c.name+`"`) {
			t.Fatalf("creating %s: %q", c.name, banner)
		}
	}
	if !node.holds(t, "far-away") {
		t.Fatal("far-away is not on node-b, so this test would prove nothing")
	}
	remote, local := &fakeTerminal{}, &fakeTerminal{}
	t.Cleanup(fs.gw.TrackSession("far-away", remote, true))
	t.Cleanup(fs.gw.TrackSession("right-here", local, true))

	// node-b's own reaper, driven at a threshold of zero so the first tick
	// pauses. It is the machine's real idle path, not a poke at its manager:
	// what is under test is a pause this gateway did not ask for and only finds
	// out about because the machine said so.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go node.mgr.RunReaper(ctx, 0, 0, 5*time.Millisecond)

	waitFor(t, "the terminal attached to far-away to be released", remote.isClosed)
	if b, _ := node.mgr.Get("far-away"); b.State != "paused" {
		t.Fatalf("node-b did not pause it: %+v", b)
	}
	if _, here := fs.mgr.Get("far-away"); here {
		t.Fatal("the gateway holds a record for far-away, so the pause may have been local")
	}

	got := remote.written()
	if !strings.Contains(got, "went idle for 0s") {
		t.Fatalf("the goodbye does not carry node-b's own words: %q", got)
	}

	// The local pause, for comparison. Same registry, same renderer, a reason
	// this gateway's manager composes for itself.
	if err := fs.mgr.Pause(context.Background(), "right-here"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the terminal attached to right-here to be released", local.isClosed)

	normalise := func(s, name, reason string) string {
		s = strings.ReplaceAll(s, name, "<sandbox>")
		return strings.ReplaceAll(s, reason, "<reason>")
	}
	wantBytes := normalise(local.written(), "right-here", "was paused")
	gotBytes := normalise(got, "far-away", "went idle for 0s")
	if gotBytes != wantBytes {
		t.Fatalf("a pause on another machine reads differently:\n  remote %q\n  local  %q", gotBytes, wantBytes)
	}
	if !strings.Contains(gotBytes, "\x1b[?1000l") {
		t.Errorf("the goodbye left the terminal unrestored: %q", gotBytes)
	}
}
