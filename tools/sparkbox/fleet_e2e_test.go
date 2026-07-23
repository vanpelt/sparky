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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	roster *nodes.Store
	index  *placement.Store
	flt    *fleet.Fleet
	// fleetLog is the router's own log, kept apart from the gateway's so a test
	// can read what the fleet made of a message without wading through
	// everything else the stack said. syncBuf is e2e_test.go's mutex-guarded
	// buffer, reused here because a slog handler writes from whichever goroutine
	// logged and this one is read from the test's.
	fleetLog *syncBuf
	// upstreamPub is the one piece of gateway material that ever crosses a link.
	upstreamPub string
}

// nodeSide is the second machine. It holds no users, no secrets, no ledger and
// no roster: a node is a driver, a manager, and one outbound link.
type nodeSide struct {
	name    string
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
	// approving a machine into the fleet is operator-only.
	userKey, userPub := newClientKey(t)
	usersPath := filepath.Join(dir, "users.conf")
	line := fmt.Sprintf("tester %s", xssh.MarshalAuthorizedKey(userPub))
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

	// Both fleet tables are further writers on the same sqlite file the identity
	// store already opened, which is how they ship.
	index, err := placement.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })
	roster, err := nodes.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roster.Close() })

	fleetLog := &syncBuf{}
	flt, err := fleet.New(fleet.Options{
		Local: mgr, LocalName: "gw", LocalArch: "amd64", Index: index,
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
		Sandboxes: flt, Templates: flt, Accounts: userStore,
		DefaultImage: "ubuntu", Domain: "hivemind.tools", Log: log,
	})
	t.Cleanup(func() { ops.Close() })

	gw := sshgw.New(sshgw.GatewayOptions{
		Manager: mgr, Fleet: flt, Dial: flt.DialContext,
		Users: userStore, HostKey: hostKey, UpstreamKey: upstreamKey,
		DefaultImage: "ubuntu", Logger: log, Domain: "hivemind.tools",
		Nodes: roster, NodeJoiner: flt, NodeEnrol: true, Ops: ops,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := gw.Server("")
	go srv.Serve(ln) //nolint:errcheck // returns on Close
	t.Cleanup(func() { srv.Close() })

	fs := &fleetStack{
		testStack: &testStack{
			mgr: mgr, addr: ln.Addr().String(), userKey: userKey, users: userStore,
		},
		roster: roster, index: index, flt: flt, fleetLog: fleetLog,
		upstreamPub: sshgw.PublicKeyLine(upstreamKey),
	}
	return fs, newNodeSide(t, log, sshgw.PublicKeyLine(upstreamKey))
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
		HostMemMB: 8192, MemAdmissionPct: 75, HostVCPUs: 8,
		Observer: emitter,
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetSessions(emitter)
	return &nodeSide{name: "node-b", mgr: mgr, key: key, emitter: emitter}
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
func (fs *fleetStack) join(t *testing.T, n *nodeSide) {
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
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the node supervisor returned %v, want context.Canceled", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the node supervisor did not stop when its context was cancelled")
		}
	})

	// First contact records a pending row and buys nothing at all.
	waitFor(t, "node-b to enrol itself", func() bool {
		_, err := fs.roster.Get(n.name)
		return err == nil
	})
	if fs.flt.Online(n.name) {
		t.Fatal("an unapproved machine joined the fleet; enrolling must grant nothing")
	}

	if err := fs.roster.Approve(n.name, "tester"); err != nil {
		t.Fatalf("approving %s: %v", n.name, err)
	}
	waitFor(t, "the fleet to see node-b online", func() bool { return fs.flt.Online(n.name) })
	// The gateway registers a link and only then writes the welcome
	// (nodelink.Accept), so "online" up here does not yet mean the node has
	// installed what the welcome carried. Wait for the node's own side of the
	// handshake as well, or anything reading lastWelcome races its last step.
	waitFor(t, n.name+" to install its welcome", func() bool { return n.lastWelcome().Node == n.name })
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

	// Inventory: the node's whole picture arrived. The fleet deliberately keeps
	// it out of every listing until a placement can authorize it
	// (fleet.linkNode.Snapshot returns nothing on purpose), so the gateway's own
	// record of taking it is the only observable there is at this milestone.
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
