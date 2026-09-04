package fleet_test

// The egress plane: every machine gets its own sandboxes' policy and nobody
// else's, and a bandwidth read reaches the machine that holds the VM.

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// tagRules governs the sandboxes it names and nothing else, which is the
// distinction the whole push turns on: an absent name means unrestricted, and a
// present-but-empty one means deny-all.
type tagRules map[string][]string

func (r tagRules) AllowForSandbox(sandbox, _ string) ([]string, bool, error) {
	allow, ok := r[sandbox]
	return allow, ok, nil
}

// localNet is a fake sluice for the gateway's own machine.
type localNet struct {
	on    bool
	allow map[string][]string
	usage map[string]netpush.VMUsage
}

func (n *localNet) Enabled() bool { return n.on }
func (n *localNet) Apply(_ context.Context, allow map[string][]string) error {
	n.allow = allow
	return nil
}
func (n *localNet) Usage(context.Context, string) (map[string]netpush.VMUsage, error) {
	return n.usage, nil
}

// The property that cannot be got wrong: sluice's PUT replaces the whole set it
// enforces, so a machine handed another machine's sandboxes would have nothing
// to bind them to — and, worse, anything genuinely its own that was omitted
// reverts to unrestricted. Each machine must receive exactly its own.
func TestPushNetGivesEachMachineOnlyItsOwnSandboxes(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	gwNet := &localNet{on: true}
	f, err := fleet.New(fleet.Options{
		Local: mgr, LocalName: mgr.NodeName(), LocalArch: "arm64",
		Index: index, Log: discardLog(), LocalNet: gwNet,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	f.SetRules(tagRules{
		"here":  {"github.com"},
		"there": {"pypi.org"},
		"vault": nil, // governed, resolves to nothing: a deliberate deny-all
	})

	// One running sandbox on the gateway, two on the node.
	mustCreate(t, f, "here", "alice")
	n := newFakeNode("laptop")
	n.metered = true
	attach(t, f, n,
		running(&host.Sandbox{Name: "there", Owner: "alice", Image: "universal"}),
		running(&host.Sandbox{Name: "vault", Owner: "alice", Image: "universal"}),
	)
	place(t, index, "there", "alice", "laptop")
	place(t, index, "vault", "alice", "laptop")

	if err := f.PushNet(context.Background()); err != nil {
		t.Fatalf("PushNet: %v", err)
	}
	if want := (map[string][]string{"here": {"github.com"}}); !reflect.DeepEqual(gwNet.allow, want) {
		t.Errorf("gateway policy = %v, want %v", gwNet.allow, want)
	}
	n.mu.Lock()
	got := n.netAllow
	n.mu.Unlock()
	want := map[string][]string{"there": {"pypi.org"}, "vault": {}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("node policy = %v, want %v", got, want)
	}
}

// A machine that runs no sluice is not a failure of the push: the rest of the
// fleet must still be reconciled, and nothing about it is worth an error every
// thirty seconds.
func TestPushNetToleratesAMachineWithoutSluice(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	gwNet := &localNet{on: true}
	f, err := fleet.New(fleet.Options{
		Local: mgr, LocalName: mgr.NodeName(), Index: index, Log: discardLog(), LocalNet: gwNet,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	f.SetRules(tagRules{"here": {"github.com"}})
	mustCreate(t, f, "here", "alice")

	n := newFakeNode("laptop") // metered false: no sluice there
	attach(t, f, n)

	if err := f.PushNet(context.Background()); err != nil {
		t.Fatalf("PushNet reported a machine with no sluice as a failure: %v", err)
	}
	if gwNet.allow == nil {
		t.Error("the gateway's own policy was not pushed")
	}
}

// A gateway with no sluice of its own must still police its nodes. Gating the
// push on the local socket — which is what the pre-fleet code did — left every
// node in the fleet permanently unfiltered.
func TestPushNetReachesNodesEvenWithNoSluiceOnTheGateway(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f, err := fleet.New(fleet.Options{
		Local: mgr, LocalName: mgr.NodeName(), Index: index, Log: discardLog(),
		// LocalNet deliberately nil.
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	f.SetRules(tagRules{"there": {"pypi.org"}})

	n := newFakeNode("laptop")
	n.metered = true
	attach(t, f, n, running(&host.Sandbox{Name: "there", Owner: "alice", Image: "universal"}))
	place(t, index, "there", "alice", "laptop")

	if err := f.PushNet(context.Background()); err != nil {
		t.Fatalf("PushNet: %v", err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if want := (map[string][]string{"there": {"pypi.org"}}); !reflect.DeepEqual(n.netAllow, want) {
		t.Errorf("node policy = %v, want %v", n.netAllow, want)
	}
}

// The bug this whole change exists to fix: a bandwidth read for a VM on a node
// used to consult the gateway's own meter, find no tap by that name, and answer
// zeroes — indistinguishable from an idle sandbox.
func TestNetUsageReadsTheMachineHoldingTheSandbox(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)

	n := newFakeNode("laptop")
	n.metered = true
	n.netUsage = map[string]netpush.VMUsage{
		"there": {Name: "there", TxBytes: 4096, RxBytes: 8192, Domains: []netpush.DomainUsage{
			{Domain: "pypi.org", Resolved: true, TxBytes: 4096, RxBytes: 8192},
		}},
	}
	attach(t, f, n, running(&host.Sandbox{Name: "there", Owner: "alice", Image: "universal"}))
	place(t, index, "there", "alice", "laptop")

	u, err := f.NetUsage(context.Background(), "there")
	if err != nil {
		t.Fatalf("NetUsage: %v", err)
	}
	if u.TxBytes != 4096 || u.RxBytes != 8192 {
		t.Errorf("usage = %+v, want the node's own numbers", u)
	}
	if len(u.Domains) != 1 || u.Domains[0].Domain != "pypi.org" {
		t.Errorf("domains = %+v", u.Domains)
	}
}

func TestBuildDenialsReachTheMachineHoldingTheSandbox(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)

	n := newFakeNode("laptop")
	n.metered = true
	n.netDenials = netpush.DenialCapture{
		CaptureID: "box-id",
		Domains:   []netpush.DeniedDomain{{Domain: "registry.npmjs.org", Queries: 2}},
	}
	attach(t, f, n, running(&host.Sandbox{Name: "there", Owner: "alice", Image: "universal"}))
	place(t, index, "there", "alice", "laptop")

	if err := f.BeginBuildDenials(context.Background(), "there"); err != nil {
		t.Fatalf("BeginBuildDenials: %v", err)
	}
	got, err := f.FinishBuildDenials(context.Background(), "there")
	if err != nil {
		t.Fatalf("FinishBuildDenials: %v", err)
	}
	if got.CaptureID != "box-id" || len(got.Domains) != 1 || got.Domains[0].Domain != "registry.npmjs.org" {
		t.Fatalf("capture = %+v", got)
	}
	want := []string{"net.denials there true", "net.denials there false"}
	n.mu.Lock()
	defer n.mu.Unlock()
	var calls []string
	for _, call := range n.calls {
		if strings.HasPrefix(call, "net.denials ") {
			calls = append(calls, call)
		}
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("denial calls = %v, want %v", calls, want)
	}
}

// "Metered and quiet" and "not metered at all" must not render the same. The
// first is an empty report; the second is a typed refusal the console turns
// into a 501.
func TestNetUsageDistinguishesQuietFromUnmetered(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)

	quiet := newFakeNode("quiet")
	quiet.metered = true // meters, has seen nothing
	attach(t, f, quiet, running(&host.Sandbox{Name: "silent", Owner: "alice", Image: "universal"}))
	place(t, index, "silent", "alice", "quiet")

	blind := newFakeNode("blind") // no sluice at all
	attach(t, f, blind, running(&host.Sandbox{Name: "unwatched", Owner: "alice", Image: "universal"}))
	place(t, index, "unwatched", "alice", "blind")

	u, err := f.NetUsage(context.Background(), "silent")
	if err != nil {
		t.Fatalf("a metered machine with no traffic must answer, not refuse: %v", err)
	}
	if u.Name != "silent" || len(u.Domains) != 0 {
		t.Errorf("quiet usage = %+v", u)
	}
	if _, err := f.NetUsage(context.Background(), "unwatched"); codeOf(err) != nodelink.CodeNoSluice {
		t.Errorf("unmetered usage = %v (code %s), want %s", err, codeOf(err), nodelink.CodeNoSluice)
	}
}

// running marks a fake node's sandbox as up, since only a running VM has a tap
// and the push filters on exactly that.
func running(b *host.Sandbox) *host.Sandbox {
	b.State = vmm.StateRunning
	return b
}
