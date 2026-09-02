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
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/templates"
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

	// The repo store is not in the optional set below: serve() opens one on
	// every gateway unconditionally, so a fixture without it would model a host
	// that does not exist and would let the wiring assertion pass vacuously.
	repoStore, err := repos.Open(db, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repoStore.Close() }) //nolint:errcheck

	// Nor is the template-bindings store, for the same reason and with the same
	// consequence: serve() opens one on every gateway unconditionally, so a
	// fixture without it would model a host that does not exist and would let
	// TestGatewayOpsBindsTemplates pass vacuously — the capability is false
	// either way when the field is dropped.
	templateStore, err := templates.Open(db, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { templateStore.Close() }) //nolint:errcheck

	// Nor is the environment store, for the third time and for the same
	// reason: serve() opens one on every gateway unconditionally, so a fixture
	// without it would model a host that does not exist and would let
	// TestGatewayOpsNamesEnvironments pass vacuously.
	envStore, err := envs.Open(db, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { envStore.Close() }) //nolint:errcheck

	// The optional stores a gateway also passes are left out: nothing here asks
	// the control plane about tags, schedules, routes or session tokens. The
	// GitHub App is left out too, and that one is optional in production as
	// well — its key is a fleet secret most hosts do not hold.
	return gatewayFixture{
		stores: gatewayStores{
			Fleet: flt, Placement: index, Roster: roster, Users: userStore,
			Repos:        repoStore,
			TemplateTags: templateStore,
			Environments: envStore,
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

// TestGatewayOpsAttachesRepos is the same wiring assertion as the one above,
// for the field next to it, and it exists for the same reason: ctlops decides
// whether repositories can be attached at all by comparing Config.Repos against
// nil, so a dropped line in newGatewayOps turns `ssh ctl@<gw> repo add` into
// "repo attachments are not enabled on this host" on a machine with the store
// plainly open — with nothing in the logs and no other test noticing.
//
// It does NOT assert Capabilities().GitHubApp. A host with no App key is a
// normal host, not a misconfigured one: attachments still record and public
// repositories still clone with no credential at all.
func TestGatewayOpsAttachesRepos(t *testing.T) {
	fx := newGatewayFixture(t)
	ops := newGatewayOps(fx.stores)
	t.Cleanup(ops.Close)

	if !ops.Capabilities().Repos {
		t.Fatal("a gateway with a repo store reports repo attachments disabled; ctlops.Config.Repos is unwired")
	}
}

// TestGatewayOpsBindsTemplates is the third of these wiring assertions, and it
// exists for the reason the two above do: ctlops decides whether a tag can name
// a base image at all by comparing Config.TemplateTags against nil, so a dropped
// line in newGatewayOps turns `ssh ctl@<gw> snapshot bind` into "template
// bindings are not enabled on this host" on a machine with the store plainly
// open — and every create silently takes the operator's default image.
//
// The field is declared on gatewayStores as the ctlops INTERFACE for a reason
// this test cannot see: a concrete *templates.Store holding a typed nil becomes
// a NON-nil interface when it is copied into ctlops.Config, which would make the
// capability report true on a host that has no store at all and panic on the
// first call. See the comment on gatewayStores.Repos.
func TestGatewayOpsBindsTemplates(t *testing.T) {
	fx := newGatewayFixture(t)
	ops := newGatewayOps(fx.stores)
	t.Cleanup(ops.Close)

	if !ops.Capabilities().TemplateTags {
		t.Fatal("a gateway with a template store reports bindings disabled; ctlops.Config.TemplateTags is unwired")
	}
}

// TestGatewayOpsWithoutATemplateStoreSaysSo is the other half, and it is what
// makes the assertion above mean something: an unset field must report false.
// Without this, a concrete-typed field holding a nil pointer would satisfy the
// test above on every host, including the ones that cannot bind anything.
func TestGatewayOpsWithoutATemplateStoreSaysSo(t *testing.T) {
	fx := newGatewayFixture(t)
	fx.stores.TemplateTags = nil
	ops := newGatewayOps(fx.stores)
	t.Cleanup(ops.Close)

	if ops.Capabilities().TemplateTags {
		t.Fatal("a gateway with no template store advertises bindings; the field is not the honest nil")
	}
}

// TestGatewayOpsNamesEnvironments is the fourth of these wiring assertions, and
// it exists for the reason the three above do: ctlops decides whether an
// environment can be named at all by comparing Config.Environments against nil,
// so a dropped line in newGatewayOps turns `ssh ctl@<gw> env ls` into
// "environments are not enabled on this host" on a machine with the store
// plainly open — and `create --env` into a refusal nobody can act on.
func TestGatewayOpsNamesEnvironments(t *testing.T) {
	fx := newGatewayFixture(t)
	ops := newGatewayOps(fx.stores)
	t.Cleanup(ops.Close)

	if !ops.Capabilities().Environments {
		t.Fatal("a gateway with an environment store reports environments disabled; ctlops.Config.Environments is unwired")
	}
}

// TestGatewayOpsWithoutAnEnvironmentStoreSaysSo is the other half, and it is
// what makes the assertion above mean something — it is also the typed-nil trap
// itself, asserted. gatewayStores.Environments is declared as the ctlops
// INTERFACE, so an unset field is a nil interface and the capability is false.
// Declared as a concrete *envs.Store it would be a NON-nil interface holding a
// nil pointer the moment newGatewayOps copied it into ctlops.Config: this test
// would fail, and on a real host with no store every env verb would panic
// instead of refusing.
func TestGatewayOpsWithoutAnEnvironmentStoreSaysSo(t *testing.T) {
	fx := newGatewayFixture(t)
	fx.stores.Environments = nil
	ops := newGatewayOps(fx.stores)
	t.Cleanup(ops.Close)

	if ops.Capabilities().Environments {
		t.Fatal("a gateway with no environment store advertises environments; the field is not the honest nil")
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

// A node advertises what it can boot from BOTH directories it reads templates
// from. Captures live in a writable dir separate from the operator's read-only
// image dir on a hardened node, and a machine that listed only the latter would
// tell the gateway it cannot boot disks it is holding.
func TestImageNamesMergesBothTemplateDirectories(t *testing.T) {
	images, templates := t.TempDir(), t.TempDir()
	write := func(dir, name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(images, "universal.ext4")
	write(images, "shared.ext4")
	write(images, "notes.txt") // not a template
	write(templates, "snap-alice-gold.ext4")
	write(templates, "shared.ext4") // the same name in both dirs

	got := map[string]int{}
	for _, n := range imageNames(images, templates) {
		got[n]++
	}
	for _, want := range []string{"universal", "shared", "snap-alice-gold"} {
		if got[want] == 0 {
			t.Errorf("%q is missing from the advertised images %v", want, got)
		}
	}
	if got["shared"] > 1 {
		t.Errorf("a name present in both directories was advertised %d times", got["shared"])
	}
	if got["notes"] != 0 || got["notes.txt"] != 0 {
		t.Errorf("a non-template file was advertised as an image: %v", got)
	}
	// The single-machine shape: one directory, an empty second one, unchanged.
	if len(imageNames(images, "")) != 2 {
		t.Errorf("imageNames with no template dir = %v, want the 2 in the image dir", imageNames(images, ""))
	}
}

// ---------------------------------------------------------------------------
// The environment build — Phase B's half of the wiring
// ---------------------------------------------------------------------------

// nudgeRecorder stands in for the envsync syncer in the assertions below. The
// real one runs a systemd unit inside a VM; what is under test here is whether
// this package hands ctlops anything at all.
type nudgeRecorder struct{ boxes []string }

func (n *nudgeRecorder) StartSetup(_ context.Context, box *host.Sandbox) error {
	n.boxes = append(n.boxes, box.Name)
	return nil
}

// buildableStores is the fixture plus the two stores an environment build needs
// that the base fixture leaves out: the secrets store, which newGatewayOps also
// wires as Tags (a builder is created WITH a tag, so a host with no tag store
// refuses the create), and a stand-in for the guest nudge.
func buildableStores(t *testing.T, fx gatewayFixture) (gatewayStores, *nudgeRecorder) {
	t.Helper()
	store, err := secrets.Open(filepath.Join(t.TempDir(), "secrets.db"),
		secrets.DeriveKEK([]byte("wiring-test-key-material")), fx.stores.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() }) //nolint:errcheck
	nudge := &nudgeRecorder{}
	s := fx.stores
	s.Secrets, s.SecretTags, s.EnvVars = store, store, store
	s.SetupStarter = nudge
	return s, nudge
}

// TestGatewayOpsBuildsEnvironmentsWithoutAGitHubApp is the typed-nil trap on
// the field Phase B added, asserted from the outside.
//
// gatewayStores.RepoFiles is declared as the ctlops INTERFACE, and this fixture
// leaves it unset — which is what a host with no App key looks like, and that
// is a normal host rather than a broken one: its App private key is a fleet
// secret most operators do not hold. Declared as a concrete *ghapp.App the
// field would be a NON-nil interface holding a nil pointer the moment
// newGatewayOps copied it into ctlops.Config, and the seed read of
// `.sparkbox/setup.sh` would panic inside a build instead of concluding there
// is no script to run.
//
// So the assertion is the refusal, not the panic: an environment nobody has
// written a script for is refused with the code that says exactly that.
func TestGatewayOpsBuildsEnvironmentsWithoutAGitHubApp(t *testing.T) {
	fx := newGatewayFixture(t)
	stores, nudge := buildableStores(t, fx)
	ops := newGatewayOps(stores)
	t.Cleanup(ops.Close)

	ctx := context.Background()
	alice := ctlops.Caller{Handle: "opsy"}
	if _, err := ops.PutEnvironment(ctx, alice, ctlops.EnvArgs{Name: "web"}); err != nil {
		t.Fatalf("env create: %v", err)
	}
	// No script anywhere is the AGENT path now, so what this fixture actually
	// proves has moved: it is not that a scriptless build is refused, it is
	// that the refusal it does get comes from the agent gate and not from a
	// typed-nil RepoFiles panicking on the seed read — which is this test's
	// real subject. This owner has no CLAUDE_CODE_OAUTH_TOKEN, so the gate
	// refuses, and the seed read has already run by then.
	_, err := ops.BuildEnvironment(ctx, alice, "web")
	if err == nil {
		t.Fatal("a build with no script and no agent credential was accepted")
	}
	if got := ctlops.AsError("env.build", err).Code; got != "env_no_agent_credential" {
		t.Fatalf("build refused with code %q, want env_no_agent_credential: %v", got, err)
	}
	if len(nudge.boxes) != 0 {
		t.Fatalf("a refused build nudged %v", nudge.boxes)
	}
}

// TestGatewayOpsStartsTheGuestSetupRun is the other half: with the seam wired,
// a build reaches a guest. Without ctlops.Config.SetupStarter in newGatewayOps
// this passes vacuously nowhere — the build refuses, no builder is created, and
// the only place the mistake shows is a production host where `env build`
// answers "environment builds are not enabled" on a machine plainly running
// one.
func TestGatewayOpsStartsTheGuestSetupRun(t *testing.T) {
	fx := newGatewayFixture(t)
	stores, nudge := buildableStores(t, fx)
	ops := newGatewayOps(stores)
	t.Cleanup(ops.Close)

	ctx := context.Background()
	alice := ctlops.Caller{Handle: "opsy"}
	if _, err := ops.PutEnvironment(ctx, alice, ctlops.EnvArgs{Name: "web"}); err != nil {
		t.Fatalf("env create: %v", err)
	}
	if err := ops.SetEnvScript(alice, "web", "echo hi\n", envs.SetupFromManual); err != nil {
		t.Fatalf("env script: %v", err)
	}
	info, err := ops.BuildEnvironment(ctx, alice, "web")
	if err != nil {
		t.Fatalf("env build: %v", err)
	}
	if info.State != string(envs.StateBuilding) || info.BuildBox != "web-build" {
		t.Fatalf("build returned %+v, want a building row naming web-build", info)
	}
	if len(nudge.boxes) != 1 || nudge.boxes[0] != "web-build" {
		t.Fatalf("the guest nudge went to %v, want [web-build]", nudge.boxes)
	}
}

// TestGatewayOpsWithoutBindingsRefusesABuild is the degraded host: a control
// plane that cannot point a tag at a disk cannot build an environment, and must
// say so instead of running somebody's setup script for ten minutes to find
// out. The refusal comes BEFORE the store is read, which is also why the name
// below is one nobody has.
func TestGatewayOpsWithoutBindingsRefusesABuild(t *testing.T) {
	fx := newGatewayFixture(t)
	stores, nudge := buildableStores(t, fx)
	stores.TemplateTags = nil
	ops := newGatewayOps(stores)
	t.Cleanup(ops.Close)

	_, err := ops.BuildEnvironment(context.Background(), ctlops.Caller{Handle: "opsy"}, "ghost")
	if err == nil {
		t.Fatal("a host that cannot bind a disk accepted a build")
	}
	if e := ctlops.AsError("env.build", err); e.Kind != ctlops.KindDisabled {
		t.Fatalf("build refused as %v (%q), want a disabled-feature refusal", e.Kind, e.Msg)
	}
	if len(nudge.boxes) != 0 {
		t.Fatalf("a refused build nudged %v", nudge.boxes)
	}
}

// TestEnvSetupDoorAnswersAnOrdinaryBox is the metadata adapter, asserted where
// it is built rather than where it is used.
//
// envSetupOps is the one bridge between metadata.SetupResult and
// ctlops.SetupReport — two structs declared field for field alike so the
// conversion in SetupDone compiles — and it is handed to the metadata server
// unconditionally. Every VM in the fleet therefore reaches SetupFor on boot,
// and all but the rare builder must be told "no job" without a store lookup
// going wrong and without a nil dereference. A host with no environment store
// at all has to answer the same way, which is the second case below.
func TestEnvSetupDoorAnswersAnOrdinaryBox(t *testing.T) {
	fx := newGatewayFixture(t)
	stores, _ := buildableStores(t, fx)
	box := &host.Sandbox{Name: "alice-box", Owner: "opsy"}

	for _, tc := range []struct {
		name  string
		build func() gatewayStores
	}{
		{"with an environment store", func() gatewayStores { return stores }},
		{"with none at all", func() gatewayStores {
			s := stores
			s.Environments = nil
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := newGatewayOps(tc.build())
			t.Cleanup(ops.Close)
			door := envSetupOps{ops: ops}
			job, ok, err := door.SetupFor(context.Background(), box)
			if err != nil {
				t.Fatalf("SetupFor on an ordinary sandbox: %v", err)
			}
			if ok || job.Payload != "" || job.Env != "" || job.Mode != "" {
				t.Fatalf("SetupFor handed %+v to a box with no build", job)
			}
		})
	}
}

// stubGitHubApp is the installation half of the App, answering for every
// repository. Only InstallationFor is reached by a seed read; the other two are
// on the interface for `repo check` and `github install`.
type stubGitHubApp struct{}

func (stubGitHubApp) InstallationFor(_ context.Context, owner, _ string) (ghapp.Installation, error) {
	return ghapp.Installation{ID: 1, AccountLogin: owner, AccountType: "Organization"}, nil
}

func (stubGitHubApp) Authorize(context.Context, ghapp.Installation, int64, string) error { return nil }
func (stubGitHubApp) InstallURL() string                                                 { return "" }

// stubRepoFiles is the file-reading half. It records what it was asked for, so
// the assertion below is that the seed read HAPPENED and not merely that a
// script appeared from somewhere.
type stubRepoFiles struct {
	asked []string
	body  string
}

func (s *stubRepoFiles) ReadFile(_ context.Context, _ ghapp.Installation, owner, name, _, path string) ([]byte, error) {
	s.asked = append(s.asked, owner+"/"+name+":"+path)
	if s.body == "" {
		return nil, ghapp.ErrNoSuchFile
	}
	return []byte(s.body), nil
}

// TestGatewayOpsSeedsASetupScriptFromARepo is the wiring assertion for
// ctlops.Config.RepoFiles, and it is the one that cannot be inferred from a
// capability flag: nothing in Capabilities() reports it, and a host with the
// line dropped behaves exactly like a host with no GitHub App — every build
// refuses "no setup script" at people whose repository has one committed, which
// is the single most confusing failure this feature can produce.
func TestGatewayOpsSeedsASetupScriptFromARepo(t *testing.T) {
	fx := newGatewayFixture(t)
	stores, nudge := buildableStores(t, fx)

	repoStore, err := repos.Open(filepath.Join(t.TempDir(), "repos.db"), fx.stores.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repoStore.Close() }) //nolint:errcheck
	if err := repoStore.PutRepo("opsy", repos.Repo{
		Host: "github.com", Slug: "wandb/hivemind", Access: repos.AccessRead,
	}, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	files := &stubRepoFiles{body: "#!/usr/bin/env bash\nnpm ci\n"}
	stores.Repos = repoStore
	stores.GitHubApp = stubGitHubApp{}
	stores.RepoFiles = files

	ops := newGatewayOps(stores)
	t.Cleanup(ops.Close)

	ctx := context.Background()
	opsy := ctlops.Caller{Handle: "opsy"}
	if _, err := ops.PutEnvironment(ctx, opsy, ctlops.EnvArgs{Name: "web"}); err != nil {
		t.Fatalf("env create: %v", err)
	}
	if _, err := ops.BuildEnvironment(ctx, opsy, "web"); err != nil {
		t.Fatalf("env build: %v", err)
	}
	if len(files.asked) != 1 || files.asked[0] != "wandb/hivemind:"+ctlops.SetupScriptPath {
		t.Fatalf("the seed read asked for %v, want one read of %s", files.asked, ctlops.SetupScriptPath)
	}
	if len(nudge.boxes) != 1 {
		t.Fatalf("the seeded build nudged %v", nudge.boxes)
	}
	// And it was RECORDED, which is what makes the next build of this
	// environment the same build rather than another trip to github.com.
	script, from, err := ops.EnvScript(opsy, "web")
	if err != nil {
		t.Fatal(err)
	}
	if script != files.body || from != envs.SetupFromRepo {
		t.Fatalf("stored script = %q from %q, want the repository's, from %q", script, from, envs.SetupFromRepo)
	}
}

// TestNodeMetadataCarriesEveryGuestDoorTheGatewayHas is a regression test for a
// bug that was invisible by construction: the node built its own metadata
// server, the environment-build pair was simply not among the collaborators it
// passed, and nothing anywhere failed. Both routes answered 501, a builder read
// that as "no job" and exited 0, and the environment sat in `building` until a
// 45-minute timeout reported a cause that was not the real one — on CKS, where
// the gateway holds no VMs, for every build there could ever be.
//
// So the assertion is the crude one, deliberately: each door the gateway hands
// its metadata server is present here too. metadata's own rule is that a guest
// must not be able to tell which machine its sandbox landed on from the status
// it got, and every nil below is exactly that leak.
func TestNodeMetadataCarriesEveryGuestDoorTheGatewayHas(t *testing.T) {
	dir := t.TempDir()
	mgr := testNodeManager(t, dir)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	uplink := nodelink.NewUplink()
	identity := newRelayIdentity(uplink, "ssh", log)
	t.Cleanup(func() { identity.Close() })
	opts := nodeOptions{
		guestSubnet:       testNodeGuestSubnet,
		toolsDir:          filepath.Join(dir, "tools"),
		guestSelfSnapshot: true,
	}
	o := nodeMetadataOptions(mgr, uplink, identity,
		newRelayRepos(uplink, identity.currentGRPC, log), opts, log)

	for name, door := range map[string]any{
		"Manager":        o.Manager,
		"Identity":       o.Identity,
		"RouteControl":   o.RouteControl,
		"Repos":          o.Repos,
		"RepoAuthorizer": o.RepoAuthorizer,
		"RepoStatus":     o.RepoStatus,
		"Vitals":         o.Vitals,
		"Tools":          o.Tools,
		"SelfLifecycle":  o.SelfLifecycle,
		"EnvSetup":       o.EnvSetup,
	} {
		if door == nil {
			t.Errorf("a node's metadata service has no %s, so those routes answer 501 "+
				"where the gateway's answer is a 2xx", name)
		}
	}
	// And the options this package builds are ones the service will actually
	// accept: an assertion about a struct nobody can construct is no assertion.
	if _, err := metadata.NewChecked(o); err != nil {
		t.Fatalf("the node's metadata options are not serviceable: %v", err)
	}
}
