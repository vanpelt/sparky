package nodelink

// The egress verbs as seen across a real control channel: what a machine with
// a sluice answers, and what one without answers instead.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
)

// fakeSluice stands in for the daemon on the node's own machine.
type fakeSluice struct {
	on      bool
	applied map[string][]string
	usage   map[string]netpush.VMUsage
	// owner records what Usage was filtered by, which must always be "" — the
	// gateway holds the ledger that decides who may see what.
	owner   string
	applyEr error
}

func (f *fakeSluice) Enabled() bool { return f.on }

func (f *fakeSluice) Apply(_ context.Context, allow map[string][]string) error {
	f.applied = allow
	return f.applyEr
}

func (f *fakeSluice) Usage(_ context.Context, owner string) (map[string]netpush.VMUsage, error) {
	f.owner = owner
	return f.usage, nil
}

func netPair(t *testing.T, s NetControl) (*Conn, *fakeSluice) {
	t.Helper()
	fs, _ := s.(*fakeSluice)
	gw, _ := newPipePair(t, nil, func(c *Conn) { registerNetOps(c, "laptop", s) })
	return gw, fs
}

func TestNetPolicyReachesTheNodesSluice(t *testing.T) {
	gw, fs := netPair(t, &fakeSluice{on: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	want := map[string][]string{"there": {"pypi.org"}, "vault": {}}
	var resp EmptyResp
	if err := gw.Request(ctx, TypeNetPolicy, NetPolicyReq{Allow: want}, &resp); err != nil {
		t.Fatalf("push: %v", err)
	}
	if !reflect.DeepEqual(fs.applied, want) {
		t.Errorf("applied = %v, want %v", fs.applied, want)
	}
	// The empty list must survive as an empty list, not become nil: a governed
	// deny-all and an ungoverned sandbox are different policies, and JSON is
	// where that distinction is easiest to lose.
	if fs.applied["vault"] == nil {
		t.Error("a governed deny-all arrived as nil, which reads as ungoverned")
	}
}

func TestNetUsageIsReportedUnfilteredAndSorted(t *testing.T) {
	gw, fs := netPair(t, &fakeSluice{on: true, usage: map[string]netpush.VMUsage{
		"zeta":  {Name: "zeta", TxBytes: 1},
		"alpha": {Name: "alpha", TxBytes: 2},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var resp NetUsageResp
	if err := gw.Request(ctx, TypeNetUsage, struct{}{}, &resp); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if fs.owner != "" {
		t.Errorf("the node filtered by owner %q; ownership is the gateway's question", fs.owner)
	}
	if len(resp.VMs) != 2 || resp.VMs[0].Name != "alpha" || resp.VMs[1].Name != "zeta" {
		t.Errorf("vms = %+v, want alpha then zeta", resp.VMs)
	}
}

// The distinction the whole CodeNoSluice constant exists for. A machine with no
// meter must REFUSE both verbs: an empty success would tell the gateway a
// tagged sandbox is filtered when nothing filters it, and an empty report would
// render as an idle VM rather than an unmeasured one.
func TestEgressVerbsRefuseOnAMachineWithNoSluice(t *testing.T) {
	for _, tc := range []struct {
		name string
		net  NetControl
	}{
		{"no sluice wired at all", nil},
		{"a syncer with no socket", &fakeSluice{on: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, _ := netPair(t, tc.net)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			var empty EmptyResp
			perr := gw.Request(ctx, TypeNetPolicy, NetPolicyReq{}, &empty)
			var usage NetUsageResp
			uerr := gw.Request(ctx, TypeNetUsage, struct{}{}, &usage)

			for verb, err := range map[string]error{"policy": perr, "usage": uerr} {
				if err == nil {
					t.Fatalf("%s succeeded on a machine that meters nothing", verb)
				}
				var typed *ctlops.Error
				if !errors.As(err, &typed) || typed.Code != CodeNoSluice {
					t.Errorf("%s err = %v, want code %s", verb, err, CodeNoSluice)
				}
			}
		})
	}
}

// The sluice's own failure must arrive as a failure, not be swallowed into the
// same refusal a missing daemon produces — the two need different repairs.
func TestNetPolicyReportsTheSluicesOwnFailure(t *testing.T) {
	gw, _ := netPair(t, &fakeSluice{on: true, applyEr: errors.New("socket refused")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var resp EmptyResp
	err := gw.Request(ctx, TypeNetPolicy, NetPolicyReq{}, &resp)
	if err == nil {
		t.Fatal("a failing sluice was reported as a successful push")
	}
	var typed *ctlops.Error
	if errors.As(err, &typed) && typed.Code == CodeNoSluice {
		t.Errorf("a live-but-broken sluice was reported as an absent one: %v", err)
	}
}
