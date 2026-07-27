package main

// Tests for the wiring itself — the assembly in this package rather than the
// behaviour of anything it assembles. A store that is opened and then not
// passed on is invisible: nothing fails, nothing logs, and the feature it backs
// simply answers "not configured" on a host that plainly is.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

const (
	testGatewayGuestSubnet = "10.200.0.0/20"
	testNodeGuestSubnet    = "10.201.0.0/20"
)

func TestPrivateAPIExposesMetricsWithoutShadowingControl(t *testing.T) {
	control := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := privateAPIHandler(control, fleetmetrics.New().Handler())

	metrics := httptest.NewRecorder()
	h.ServeHTTP(metrics, httptest.NewRequest("GET", "/metrics", nil))
	if metrics.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", metrics.Code)
	}
	if ct := metrics.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("metrics Content-Type = %q, want Prometheus text", ct)
	}

	api := httptest.NewRecorder()
	h.ServeHTTP(api, httptest.NewRequest("GET", "/healthz", nil))
	if api.Code != http.StatusTeapot {
		t.Fatalf("control route = %d, want %d", api.Code, http.StatusTeapot)
	}
}

// gatewayFixture is the set of stores serve() opens on a gateway, on a temp
// dir. It is deliberately built the long way rather than by calling serve():
// what is under test is what serve() then *does* with them.
//
// It keeps the machine and the two keys as well as the stores, because a test
// that wants to watch an operator command reach a live node has to be able to
// stand this gateway's own SSH listener up — with the same arguments serve()
// passes it, since a fixture that assembled its own control plane would be
// testing itself.
type gatewayFixture struct {
	stores gatewayStores
	roster *nodes.Store
	users  *users.Store

	mgr         *host.Manager
	hostKey     xssh.Signer
	upstreamKey xssh.Signer
	// opKey is the operator's client key. The account it belongs to is the only
	// one in the fixture, because every node command is operator-gated.
	opKey xssh.Signer
}

func newGatewayFixture(t *testing.T) gatewayFixture {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "sparkbox.db")
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	hostKey, err := sshgw.LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatal(err)
	}
	upstreamKey, err := sshgw.LoadOrCreateKey(dir, "gateway_upstream_key")
	if err != nil {
		t.Fatal(err)
	}
	driver := mock.New(dir, hostKey)
	t.Cleanup(func() { driver.Close() }) //nolint:errcheck
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver, NodeName: "gw", Arch: "arm64",
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	userStore, err := users.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { userStore.Close() }) //nolint:errcheck
	roster, err := nodes.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roster.Close() }) //nolint:errcheck
	index, err := placement.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() }) //nolint:errcheck
	flt, err := fleet.New(fleet.Options{
		Local: mgr, LocalName: "gw", LocalArch: "arm64", Index: index, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { flt.Close() }) //nolint:errcheck

	// An operator, because every node command is operator-gated: a caller who
	// is not one gets a refusal that would not tell these tests anything.
	opKey, err := sshgw.LoadOrCreateKey(dir, "operator_key")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := xssh.ParseAuthorizedKey([]byte(sshgw.PublicKeyLine(opKey)))
	if err != nil {
		t.Fatal(err)
	}
	if err := userStore.Create("opsy", pub, "opsy@example.test", "seed", users.OperatorInviter); err != nil {
		t.Fatal(err)
	}

	// The optional stores a gateway also passes are left out: nothing here asks
	// the control plane about tags, schedules, routes or session tokens.
	return gatewayFixture{
		stores: gatewayStores{
			Fleet: flt, Placement: index, Roster: roster, Users: userStore,
			DefaultImage: "ubuntu", Domain: "hivemind.tools", Log: log,
			GatewayGuestSubnet: testGatewayGuestSubnet,
		},
		roster:      roster,
		users:       userStore,
		mgr:         mgr,
		hostKey:     hostKey,
		upstreamKey: upstreamKey,
		opKey:       opKey,
	}
}

// TestGatewayOpsIsAFleetGateway is the wiring, asserted against what this
// package actually constructs rather than against a roster a test supplied.
//
// Without it the milestone is inoperable and every test still passes: the SSH
// door enrols a machine, the roster row appears, and `ssh ctl@<gw> node
// approve` answers "this host is not a fleet gateway" — so nothing can ever be
// approved, and the only place the mistake shows is a production host.
func TestGatewayOpsIsAFleetGateway(t *testing.T) {
	fx := newGatewayFixture(t)
	ops := newGatewayOps(fx.stores)
	t.Cleanup(ops.Close)

	if !ops.Capabilities().Fleet {
		t.Fatal("a gateway with a roster does not report itself as a fleet gateway; ctlops.Config.Nodes is unwired")
	}

	opsy := ctlops.Caller{Handle: "opsy"}
	list, err := ops.ListNodes(context.Background(), opsy)
	if err != nil {
		t.Fatalf("node ls on a fleet gateway: %v", err)
	}
	if len(list) != 1 || list[0].Name != "gw" || !list[0].Local {
		t.Fatalf("node ls = %+v, want this gateway's own machine", list)
	}
}

// TestGatewayOpsApprovesAnEnrolledNode walks the operator's half of enrolment
// through the same Ops the SSH channel and the REST API drive: a row appears,
// they approve it, and the listing says so. This is the sequence M1 exists for.
func TestGatewayOpsApprovesAnEnrolledNode(t *testing.T) {
	fx := newGatewayFixture(t)
	ops := newGatewayOps(fx.stores)
	t.Cleanup(ops.Close)

	key, err := sshgw.LoadOrCreateKey(t.TempDir(), "node_key")
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := fx.roster.Enroll("node-b", key.PublicKey())
	if err != nil {
		t.Fatal(err)
	}

	opsy := ctlops.Caller{Handle: "opsy"}
	ctx := context.Background()
	// By fingerprint, which is what the production adapter has to key on: a node
	// names itself, so approving a name would trust a string a stranger chose.
	row, err := ops.ApproveNode(ctx, opsy, enrolled.FP, ctlops.NodeApprovalConfig{
		GuestSubnet: testNodeGuestSubnet,
	})
	if err != nil {
		t.Fatalf("approving an enrolled node: %v", err)
	}
	if _, err := ops.ApproveNode(ctx, opsy, "node-b"); err == nil {
		t.Error("the wired-up roster approved a machine by name")
	}
	if row.Status != nodes.StatusApproved || row.FP == "" {
		t.Errorf("approved row = %+v, want an approved status and the fingerprint the operator compared", row)
	}

	list, err := ops.ListNodes(ctx, opsy)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range list {
		if n.Name == "node-b" {
			found = n.Status == nodes.StatusApproved
		}
	}
	if !found {
		t.Errorf("node ls = %+v, want node-b approved in it", list)
	}
}

// ---------------------------------------------------------------------------
// The operator's ceremony, against the control plane this package assembles
// ---------------------------------------------------------------------------

// fleetWiring is a gateway with its SSH listener running and a second machine
// dialling into it over a real nodelink connection.
//
// It lives here, in package main, rather than beside the other fleet end-to-end
// tests, and that is the whole point of it. The adapter that joins the node
// roster to the running fleet is unexported wiring in this package: a harness
// anywhere else can only write its own, and a harness that writes its own can
// never discover that the shipped one is missing a method. That is exactly how
// `node rm` came to leave a removed machine linked — the copy implemented what
// the copy needed, the real one did not, and every test passed.
type fleetWiring struct {
	gatewayFixture
	ops  *ctlops.Ops
	addr string
	// node is the second machine: a driver, a manager and one outbound link,
	// which is all a node is.
	node *nodeMachine
}

type nodeMachine struct {
	name string
	mgr  *host.Manager
	key  xssh.Signer
}

// newFleetWiring stands the gateway up and starts the node's supervisor. The
// node is left unapproved: joining a fleet is a ceremony an operator takes part
// in, and a harness that pre-approved the key would skip the only part of it
// that needs a human.
func newFleetWiring(t *testing.T) *fleetWiring {
	t.Helper()
	fx := newGatewayFixture(t)

	// The one control plane every transport on this host shares — built by the
	// same function serve() calls, from the same struct serve() fills in.
	ops := newGatewayOps(fx.stores)
	t.Cleanup(ops.Close)

	gw := sshgw.New(sshgw.GatewayOptions{
		Manager: fx.mgr, Fleet: fx.stores.Fleet, Dial: fx.stores.Fleet.DialContext,
		Users: fx.users, HostKey: fx.hostKey, UpstreamKey: fx.upstreamKey,
		DefaultImage: fx.stores.DefaultImage, Domain: fx.stores.Domain,
		Logger: fx.stores.Log, Ops: ops,
		Nodes: fx.roster, NodeJoiner: fx.stores.Fleet, NodeEnrol: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := gw.Server("")
	go srv.Serve(ln) //nolint:errcheck // returns on Close
	t.Cleanup(func() { srv.Close() })

	fw := &fleetWiring{gatewayFixture: fx, ops: ops, addr: ln.Addr().String()}
	fw.node = fw.startNode(t, "node-x")
	return fw
}

// startNode builds the second machine and runs its supervisor until the test
// ends. Its state directory and its driver are its own: sharing either would
// make the two managers one machine wearing two names.
func (fw *fleetWiring) startNode(t *testing.T, name string) *nodeMachine {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	key, err := sshgw.LoadOrCreateKey(dir, "node_key")
	if err != nil {
		t.Fatal(err)
	}
	// The node's own key doubles as its fake guests' host key, exactly as
	// cmd/sparkbox does in node mode: a node holds no gateway host key.
	driver := mock.New(dir, key)
	t.Cleanup(func() { driver.Close() }) //nolint:errcheck

	emitter := nodelink.NewEmitter(log)
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver, Logger: log,
		GatewayPublicKey: sshgw.PublicKeyLine(fw.upstreamKey),
		NodeName:         name, Arch: "amd64", Release: "2026-07-22",
		Observer: emitter,
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetSessions(emitter)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- nodelink.RunClient(ctx, nodelink.ClientOptions{
			Gateway: fw.addr, NodeName: name, Key: key,
			Manager: mgr, Emitter: emitter,
			Hello: func() nodelink.Hello {
				return nodelink.Hello{
					Arch: "amd64", Release: "2026-07-22", Version: "test",
					Driver: "mock", GuestSubnet: testNodeGuestSubnet,
				}
			},
			OnWelcome: func(nodelink.Welcome) error { return nil },
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
	return &nodeMachine{name: name, mgr: mgr, key: key}
}

// ctl runs one `ssh ctl@<gateway> …` command as the operator and returns what
// the gateway printed. Driving the real door rather than calling Ops directly
// is deliberate: it is the only way to see what an operator sees.
func (fw *fleetWiring) ctl(t *testing.T, cmd string) string {
	t.Helper()
	client, err := xssh.Dial("tcp", fw.addr, &xssh.ClientConfig{
		User:            sshgw.ControlUser,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(fw.opKey)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), //nolint:gosec // test
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial %s@%s: %v", sshgw.ControlUser, fw.addr, err)
	}
	defer client.Close() //nolint:errcheck
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	if err := sess.Run(cmd); err != nil {
		t.Fatalf("ctl %q: %v (stderr: %s)", cmd, err, stderr.String())
	}
	return stdout.String()
}

// join walks the enrolment ceremony to the point where the machine is linked.
func (fw *fleetWiring) join(t *testing.T) {
	t.Helper()
	name := fw.node.name
	waitFor(t, name+" to enrol itself", func() bool {
		_, err := fw.roster.Get(name)
		return err == nil
	})
	if fw.stores.Fleet.Online(name) {
		t.Fatal("an unapproved machine joined the fleet; enrolling must grant nothing")
	}
	// The operator ceremony, driven through the real ctl@ channel: read the
	// fingerprint off the pending row, compare it to the one the machine
	// printed, approve that. The name is never what is typed.
	row, err := fw.roster.Get(name)
	if err != nil {
		t.Fatalf("reading the pending row for %s: %v", name, err)
	}
	if out := fw.ctl(t, "node approve "+row.FP+" --guest-subnet "+testNodeGuestSubnet); !strings.Contains(out, "approved "+name) {
		t.Fatalf("ctl node approve said %q", out)
	}
	waitFor(t, "the fleet to see "+name+" online", func() bool { return fw.stores.Fleet.Online(name) })
}

// TestGatewayNodeRemovalClosesTheLink is the attacker's exploit as a test: a
// linked machine is removed by the operator, and everything it was granted must
// be gone by the time the command returns.
//
// The removal is durable either way — the roster row goes — so nothing here
// would fail if the eviction never happened. That is what makes it worth
// asserting: approval is read once, at the door (sshgw.admitNode), so a machine
// that is already connected keeps its control channel, keeps reporting capacity
// into the fleet's totals and keeps its data channels for as long as it likes,
// on a gateway whose operator has been told it is gone.
func TestGatewayNodeRemovalClosesTheLink(t *testing.T) {
	fw := newFleetWiring(t)
	fw.join(t)
	name := fw.node.name

	if out := fw.ctl(t, "node rm "+name); !strings.Contains(out, "removed node") {
		t.Fatalf("ctl node rm said %q", out)
	}

	// Synchronously, not eventually: the fleet drops the machine before its link
	// is told anything (fleet.EvictNode), so by the time the operator's command
	// has returned there is nothing left to race with.
	if fw.stores.Fleet.Online(name) {
		t.Error("the removed machine is still linked; its approval was taken away and it carried on regardless")
	}
	for _, c := range fw.stores.Fleet.Capacities() {
		if c.Node == name {
			t.Errorf("the removed machine still counts as fleet capacity: %+v", c)
		}
	}

	// And the operator's own listing agrees. The machine may well have
	// re-enrolled by now — that is allowed, removal is not a blacklist — but a
	// row it has to be approved into again is a different thing from a link.
	list, err := fw.ops.ListNodes(context.Background(), ctlops.Caller{Handle: "opsy"})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range list {
		if n.Name == name && n.Online {
			t.Errorf("node ls still shows %s online after it was removed: %+v", name, n)
		}
	}
}

// TestGatewayNodeListingIsWhatAnOperatorReads renders `ctl node ls` through the
// shipped adapter: the gateway's own machine, the machine that just joined, and
// the fingerprint an operator compares against the one the node printed on its
// own console before approving it.
//
// The rendering is only as good as what the adapter joins together — neither the
// roster nor the fleet can produce this listing alone — so it is asserted here,
// on the value newGatewayOps builds, rather than anywhere a harness would have
// to supply its own.
func TestGatewayNodeListingIsWhatAnOperatorReads(t *testing.T) {
	fw := newFleetWiring(t)

	// Before approval: a row that bought nothing. This is the listing an
	// operator reads to learn the name and fingerprint they are about to bless.
	waitFor(t, fw.node.name+" to enrol itself", func() bool {
		_, err := fw.roster.Get(fw.node.name)
		return err == nil
	})
	pending := fw.ctl(t, "node ls")
	if !strings.Contains(pending, "pending") {
		t.Errorf("node ls before approval reads %q, want the enrolled machine listed as pending", pending)
	}

	fw.join(t)

	out := fw.ctl(t, "node ls")
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(out, "\r\n", "\n")), "\n")
	if len(lines) != 2 {
		t.Fatalf("node ls listed %d machines, want this gateway and %s:\n%s", len(lines), fw.node.name, out)
	}
	if !strings.Contains(lines[0], "gw (this gateway)") || !strings.Contains(lines[0], "online") {
		t.Errorf("the gateway's own line reads %q", lines[0])
	}
	if !strings.Contains(lines[1], fw.node.name) || !strings.Contains(lines[1], "online") {
		t.Errorf("%s's line reads %q", fw.node.name, lines[1])
	}
	if fp := xssh.FingerprintSHA256(fw.node.key.PublicKey()); !strings.Contains(lines[1], fp) {
		t.Errorf("%s's line does not carry the fingerprint an operator compared: %q", fw.node.name, lines[1])
	}
}
