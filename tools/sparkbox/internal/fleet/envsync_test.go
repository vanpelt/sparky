package fleet_test

// The gateway's half of secret propagation: which moments fire a push for a
// sandbox on another machine, which must fire nothing at all, and whose secrets
// a push is allowed to select.
//
// The end-to-end proof that a value lands in a guest's env file lives in the
// two-machine rig (fleet_data_e2e_test.go). What is pinned here is the wiring —
// the set of triggers, and the rule that a local sandbox is never one of them,
// because a second pusher racing the manager's own over one guest's
// /etc/environment is a bug nothing downstream could diagnose.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// recordingPusher is the secret syncer, reduced to what it was asked to do.
type recordingPusher struct {
	mu    sync.Mutex
	boxes []host.Sandbox
}

func (p *recordingPusher) PushEnv(_ context.Context, b *host.Sandbox) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.boxes = append(p.boxes, *b)
	return nil
}

func (p *recordingPusher) pushed() []host.Sandbox {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]host.Sandbox(nil), p.boxes...)
}

// awaitPush waits for a push to a named sandbox and returns the record it
// carried. The push is detached on purpose — a lifecycle operation is never
// slowed or failed by an SSH exec into a guest — so a bare read after the call
// would be a race, not an assertion.
func awaitPush(t *testing.T, p *recordingPusher, name, what string) host.Sandbox {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, b := range p.pushed() {
			if b.Name == name {
				return b
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no secret-env push for %q after %s", name, what)
	return host.Sandbox{}
}

// noPush asserts nothing was pushed, and says why nothing should have been:
// the two reasons a push is wrong here are different failures with different
// fixes, and a shared sentence would name the wrong one half the time.
func noPush(t *testing.T, p *recordingPusher, name, what, why string) {
	t.Helper()
	// A push is a goroutine, so "it did not happen" needs a moment to be worth
	// asserting. This is short because the positive cases above prove the
	// dispatch is prompt: if a push were coming, it would already be here.
	time.Sleep(50 * time.Millisecond)
	for _, b := range p.pushed() {
		if b.Name == name {
			t.Fatalf("%s pushed a secret environment for %q — %s", what, name, why)
		}
	}
}

// TestRemoteLifecycleFiresTheGatewaysEnvPush walks the moments a sandbox on
// another machine becomes entitled to its environment.
//
// They are four different mechanisms and not one, which is why they are
// enumerated rather than sampled. Create and EnsureRunning fire from the call,
// because the caller is usually about to connect and waiting on an event would
// be a race the user loses. ResyncEnv fires from the gateway because relaying
// it is what the code used to do and it delivered nothing — the frame reaches a
// manager whose push hook is nil, by construction, on every node. And
// ApplyChanged fires for the transitions the MACHINE made on its own, which the
// gateway hears about no other way.
func TestRemoteLifecycleFiresTheGatewaysEnvPush(t *testing.T) {
	cases := []struct {
		name string
		// trigger runs the thing under test against a sandbox already placed
		// and running on boxb.
		trigger func(t *testing.T, f *fleet.Fleet, n *buildingNode)
	}{
		{
			name: "a resume",
			trigger: func(t *testing.T, f *fleet.Fleet, n *buildingNode) {
				// Parked first, because a resume is a TRANSITION and this test
				// is about the transition firing the push. EnsureRunning on a
				// box that never stopped is the case below.
				n.stopped("far-away", vmm.StatePaused)
				if _, err := f.EnsureReady(context.Background(), "far-away"); err != nil {
					t.Fatalf("EnsureRunning: %v", err)
				}
			},
		},
		{
			name: "a tag change",
			trigger: func(_ *testing.T, f *fleet.Fleet, _ *buildingNode) {
				f.ResyncEnv(context.Background(), "far-away")
			},
		},
		{
			name: "the machine bringing it up by itself",
			trigger: func(_ *testing.T, f *fleet.Fleet, n *buildingNode) {
				// What a node's emitter sends when its own ResumePinned, restore
				// or reboot puts a sandbox back into running. The gateway asked
				// for none of it.
				f.ApplyChanged(n.Name(), nodelink.ChangedMsg{
					Node:    n.Name(),
					Sandbox: nodelink.SandboxRow{Name: "far-away", State: string(vmm.StateRunning)},
					Reason:  "resumed",
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newManager(t, host.Options{})
			f := newFleet(t, mgr, newIndex(t))
			pusher := &recordingPusher{}
			f.SetEnvPusher(pusher)
			nodeb := newBuildingNode("boxb")
			attachBuilder(t, f, nodeb)

			if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
				t.Fatalf("CreateOn: %v", err)
			}
			// The create is a trigger in its own right, and asserting it here
			// also drains it so the case below is unambiguous.
			awaitPush(t, pusher, "far-away", "a create on another machine")

			pusher.mu.Lock()
			pusher.boxes = nil
			pusher.mu.Unlock()

			tc.trigger(t, f, nodeb)
			b := awaitPush(t, pusher, "far-away", tc.name)
			if b.Owner != "alice" {
				t.Errorf("pushed for owner %q, want the ledger's", b.Owner)
			}
			if !strings.HasSuffix(b.SSHAddr, ".sandbox.invalid:"+fleet.SSHPort) {
				t.Errorf("pushed with SSHAddr %q, want the synthetic fleet address the dialer resolves", b.SSHAddr)
			}
			if b.State != vmm.StateRunning {
				t.Errorf("pushed a %q sandbox; only a running one can be written to", b.State)
			}
		})
	}
}

// TestAwaitEnvDeliversBeforeItReturns is the difference between the push and
// the wait, and it is the entire reason the wait exists.
//
// awaitPush above has to poll, because a fired push is a goroutine. This one
// reads the record on the line after the call. An interactive attach needs that
// guarantee and cannot get it from the push: it starts the VM and opens a
// session on it in the same breath, pam_env reads /etc/environment exactly once
// at session setup, and the session — begun by a call that already returned —
// wins the race against a push that still has to dial the guest.
func TestAwaitEnvDeliversBeforeItReturns(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	pusher := &recordingPusher{}
	f.SetEnvPusher(pusher)
	nodeb := newBuildingNode("boxb")
	attachBuilder(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	// Drain the create's own asynchronous push so what follows is unambiguous.
	awaitPush(t, pusher, "far-away", "a create on another machine")
	pusher.mu.Lock()
	pusher.boxes = nil
	pusher.mu.Unlock()

	if err := f.AwaitEnv(context.Background(), "far-away"); err != nil {
		t.Fatalf("AwaitEnv: %v", err)
	}
	pushed := pusher.pushed()
	if len(pushed) != 1 {
		t.Fatalf("%d deliveries recorded on the line after AwaitEnv, want 1", len(pushed))
	}
	if pushed[0].Owner != "alice" {
		t.Errorf("delivered for owner %q, want the ledger's", pushed[0].Owner)
	}
	if pushed[0].State != vmm.StateRunning {
		t.Errorf("delivered to a %q sandbox; only a running one can be written to", pushed[0].State)
	}

	// A sandbox nobody has placed is not this fleet's to deliver into, and
	// saying so with an error would make an attach fail over a box the gateway
	// does not even hold.
	if err := f.AwaitEnv(context.Background(), "nobody-here"); err != nil {
		t.Errorf("AwaitEnv on an unknown sandbox: %v", err)
	}
}

// TestALocalSandboxIsNeverPushedByTheFleet is the other half of the split, and
// the one that is silent when it breaks.
//
// host.Manager fires its own push for a sandbox it holds, from inside its own
// lock, on every transition to running. If the router pushed as well, two
// goroutines would be rewriting one guest's /etc/environment with no ordering
// between them — and since both write the same content nearly all of the time,
// the day it mattered would be the day a tag had just changed.
func TestALocalSandboxIsNeverPushedByTheFleet(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	pusher := &recordingPusher{}
	f.SetEnvPusher(pusher)

	b := mustCreate(t, f, "brave-otter", "alice")
	if b.Node != mgr.NodeName() {
		t.Fatalf("an unnamed create landed on %q", b.Node)
	}
	noPush(t, pusher, "brave-otter", "a local create", "which is the local manager's job: two writers over one guest's /etc/environment")

	if _, err := f.EnsureReady(context.Background(), "brave-otter"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	noPush(t, pusher, "brave-otter", "a local resume", "which is the local manager's job: two writers over one guest's /etc/environment")

	// The wait obeys the same split as the push: it delegates to the manager,
	// whose hook owns this guest's env file, rather than becoming the second
	// writer the whole rule exists to prevent.
	if err := f.AwaitEnv(context.Background(), "brave-otter"); err != nil {
		t.Fatalf("AwaitEnv on a local sandbox: %v", err)
	}
	noPush(t, pusher, "brave-otter", "AwaitEnv on a local sandbox", "which the local manager delivers: two writers over one guest's /etc/environment")

	f.ResyncEnv(context.Background(), "brave-otter")
	noPush(t, pusher, "brave-otter", "a local resync", "which is the local manager's job: two writers over one guest's /etc/environment")
}

// TestAHeartbeatIsNotATransition is why the trigger is the reason and not the
// state.
//
// A node's manager reports every running sandbox as "touched" on every reaper
// tick. Pushing on "state is running" would mean an SSH exec into every guest
// in the fleet, on every tick, forever — and it would look like nothing at all
// until somebody counted the connections.
func TestAHeartbeatIsNotATransition(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	pusher := &recordingPusher{}
	f.SetEnvPusher(pusher)
	nodeb := newBuildingNode("boxb")
	attachBuilder(t, f, nodeb)
	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	awaitPush(t, pusher, "far-away", "a create on another machine")
	pusher.mu.Lock()
	pusher.boxes = nil
	pusher.mu.Unlock()

	for _, reason := range []string{"touched", "ballooned", "deflated", "disk", "pinned", "paused"} {
		f.ApplyChanged("boxb", nodelink.ChangedMsg{
			Node:    "boxb",
			Sandbox: nodelink.SandboxRow{Name: "far-away", State: string(vmm.StateRunning)},
			Reason:  reason,
		})
	}
	noPush(t, pusher, "far-away", "routine node chatter", "none of those reasons is a transition to running")
}

// TestEnsureRunningOnARunningSandboxIsNotATransition is the same rule as the
// heartbeat test, on the path that is hot.
//
// EnsureRunning is called on EVERY proxied HTTP request (proxy.Server.ServeHTTP),
// every ssh session, every browser-terminal attach and every scheduled job, and
// on a sandbox that is already up it does nothing at all. If the push were
// gated on the record it RETURNS, it would fire every time — because after a
// successful EnsureRunning the box is running by definition — and one page with
// thirty subresources would mean thirty SSH dials into the guest, each
// rewriting /etc/environment, queued on envsync's per-box mutex with a
// three-minute budget apiece. It would look like nothing at all until somebody
// counted the connections.
//
// The transition, when there is one, is covered above; when the gateway's
// picture is stale and the machine really did resume the box, the machine says
// so and ApplyChanged pushes.
func TestEnsureRunningOnARunningSandboxIsNotATransition(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	pusher := &recordingPusher{}
	f.SetEnvPusher(pusher)
	nodeb := newBuildingNode("boxb")
	attachBuilder(t, f, nodeb)
	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	awaitPush(t, pusher, "far-away", "a create on another machine")
	pusher.mu.Lock()
	pusher.boxes = nil
	pusher.mu.Unlock()

	// The sandbox is up and stays up. This is what a burst of traffic looks
	// like from here.
	for i := 0; i < 10; i++ {
		b, err := f.EnsureReady(context.Background(), "far-away")
		if err != nil {
			t.Fatalf("EnsureRunning %d: %v", i, err)
		}
		if b.State != vmm.StateRunning {
			t.Fatalf("EnsureRunning %d returned a %q sandbox", i, b.State)
		}
	}
	noPush(t, pusher, "far-away", "ten requests against a running sandbox",
		"a sandbox that never stopped needs nothing delivered, and this path runs on every request")
}

// TestAMachineCannotNameTheOwnerWhoseSecretsItGets is the security rule.
//
// The record a node hands back carries the owner the NODE claims — EnsureRunning
// does not overwrite it, because resume is not an ownership-changing operation
// and every caller has already passed the ownership gate on the ledger's
// column. If the push selected secrets on that string, a machine could name any
// handle it liked and be sent that account's decrypted values. The ledger is the
// only authorization input everywhere else in this package and it is the only
// one here.
func TestAMachineCannotNameTheOwnerWhoseSecretsItGets(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	pusher := &recordingPusher{}
	f.SetEnvPusher(pusher)
	nodeb := newBuildingNode("boxb")
	attachBuilder(t, f, nodeb)

	if _, err := f.CreateOn(context.Background(), "boxb", "far-away", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("CreateOn: %v", err)
	}
	awaitPush(t, pusher, "far-away", "a create on another machine")
	pusher.mu.Lock()
	pusher.boxes = nil
	pusher.mu.Unlock()

	// The machine relabels the sandbox as somebody else's and then reports it
	// coming back up.
	if b, ok := nodeb.Box("far-away"); ok {
		b.Owner = "mallory"
	}
	nodeb.stopped("far-away", vmm.StatePaused)
	if _, err := f.EnsureReady(context.Background(), "far-away"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	b := awaitPush(t, pusher, "far-away", "a resume of a relabelled sandbox")
	if b.Owner != "alice" {
		t.Fatalf("pushed for owner %q — a machine renamed the account whose secrets it receives", b.Owner)
	}
}
