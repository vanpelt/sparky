package fleet_test

// What a machine's events are allowed to do to this gateway.
//
// The one with teeth is the pause: it reaches out of the link and closes
// somebody's terminal, with a sentence the machine on the other end composed.
// Both halves are worth pinning — that the sentence really is relayed and not
// re-invented here, and that a link can only ever say it about the sandboxes
// the ledger placed on that link.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// closedSession is one hang-up the gateway was asked for.
type closedSession struct{ sandbox, reason string }

// fakeSessions stands in for *sshgw.Gateway's live-session registry. It records
// rather than acts, because what is under test up here is which sandbox got
// hung up and with whose words — the rendering of the goodbye itself is
// sshgw's, and is asserted against the real one end to end (fleet_e2e_test.go).
type fakeSessions struct {
	mu     sync.Mutex
	closed []closedSession
}

func (s *fakeSessions) CloseSandboxSessions(sandbox, reason string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = append(s.closed, closedSession{sandbox, reason})
	return 1
}

func (s *fakeSessions) calls() []closedSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]closedSession(nil), s.closed...)
}

var _ host.SessionCloser = (*fakeSessions)(nil)

// TestFleetPauseRelaysTheNodesWording drives the pause event a machine sends
// just before it stops one of its VMs.
//
// The reasons in the table are sentences this gateway could not compose: the
// reaper's threshold is the node's own setting, and nothing up here knows what
// it is. That is the point of relaying rather than inventing — a locally
// written fragment would look right and be wrong the moment the two disagreed.
func TestFleetPauseRelaysTheNodesWording(t *testing.T) {
	cases := []struct {
		name string
		// link is the AUTHENTICATED name the event arrives on, which is the
		// only name that decides anything.
		link string
		msg  nodelink.PausedMsg
		want []closedSession
	}{
		{
			name: "the machine that holds it",
			link: "boxb",
			msg:  nodelink.PausedMsg{Node: "boxb", Name: "remote", Reason: "went idle for 37m"},
			want: []closedSession{{"remote", "went idle for 37m"}},
		},
		{
			// The payload's node field is a claim, and it is never read. A node
			// told a different name for itself, or one hand-crafting frames,
			// must not be able to change what its own events are attributed to.
			name: "a payload naming a different machine changes nothing",
			link: "boxb",
			msg:  nodelink.PausedMsg{Node: "boxc", Name: "remote", Reason: "was paused"},
			want: []closedSession{{"remote", "was paused"}},
		},
		{
			// The same event, arriving on somebody else's link. This is the
			// whole reason the ledger is consulted: two machines with one state
			// directory between them, or one compromised machine guessing at
			// names, would otherwise hang up terminals that are nothing to do
			// with it.
			name: "another machine cannot pause it",
			link: "boxc",
			msg:  nodelink.PausedMsg{Node: "boxb", Name: "remote", Reason: "was paused"},
		},
		{
			// A local sandbox's sessions are closed by the local manager, which
			// holds the VM. A link reaching for one is reaching past its own
			// machine entirely.
			name: "a machine cannot pause a sandbox on this gateway",
			link: "boxb",
			msg:  nodelink.PausedMsg{Node: "boxb", Name: "here", Reason: "was paused"},
		},
		{
			name: "a name nobody has placed",
			link: "boxb",
			msg:  nodelink.PausedMsg{Node: "boxb", Name: "ghost", Reason: "was paused"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := newIndex(t)
			f := newFleet(t, newManager(t, host.Options{NodeName: "boxa"}), index)
			mustCreate(t, f, "here", "alice")
			place(t, index, "remote", "alice", "boxb")

			sessions := &fakeSessions{}
			f.SetSessions(sessions)
			f.ApplyPaused(tc.link, tc.msg)

			got := sessions.calls()
			if len(got) != len(tc.want) {
				t.Fatalf("closed %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("close %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestFleetPauseWithNoSessionRegistry is the deployment that never wired one.
// The courtesy is skipped, exactly as it is on host.Manager, and nothing
// panics on the way past.
func TestFleetPauseWithNoSessionRegistry(t *testing.T) {
	index := newIndex(t)
	f := newFleet(t, newManager(t, host.Options{NodeName: "boxa"}), index)
	place(t, index, "remote", "alice", "boxb")
	f.ApplyPaused("boxb", nodelink.PausedMsg{Node: "boxb", Name: "remote", Reason: "was paused"})
}

// TestFleetGoneEventKeepsTheLedgerRow pins the prohibition, because it is the
// kind of thing a later reconciliation is tempted to tidy up.
//
// A machine saying a sandbox is gone is not authority to release its placement:
// the row is the fleet's name allocation and the user's record of where their
// sandbox went, and a machine that was wiped or rolled back would otherwise
// free somebody's name for the next person who asked for it.
func TestFleetGoneEventKeepsTheLedgerRow(t *testing.T) {
	index := newIndex(t)
	f := newFleet(t, newManager(t, host.Options{NodeName: "boxa"}), index)
	place(t, index, "remote", "alice", "boxb")

	f.ApplyGone("boxb", nodelink.GoneMsg{Node: "boxb", Name: "remote", Reason: "destroyed"})
	f.ApplyChanged("boxb", nodelink.ChangedMsg{Node: "boxb", Sandbox: nodelink.SandboxRow{Name: "remote"}})

	if node, ok := f.NodeOf("remote"); !ok || node != "boxb" {
		t.Errorf("NodeOf(remote) = (%q, %v) after a gone event, want boxb — the row must outlive the sandbox", node, ok)
	}
	if row, ok, err := index.Get("remote"); err != nil || !ok || row.Owner != "alice" {
		t.Errorf("ledger row = (%+v, %v, %v), want alice's row intact", row, ok, err)
	}
}

// emit sends one unprompted frame from the machine's end of a real link, which
// is how a node's events arrive: no ID, so the gateway routes it to the event
// table rather than answering it.
func (n *linkedNode) emit(t *testing.T, typ string, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(n.end).Encode(nodelink.Frame{Type: typ, Body: raw}); err != nil {
		t.Fatalf("emitting a %s frame: %v", typ, err)
	}
}

// ask sends one REQUEST from the machine's end — a frame with an ID, which is
// how an inventory travels, because the node wants the gateway's answer back.
// The reply lands in the drain goroutine linkTo starts and is discarded there;
// what a caller here is watching is the effect on the gateway.
func (n *linkedNode) ask(t *testing.T, id, typ string, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(n.end).Encode(nodelink.Frame{ID: id, Type: typ, Body: raw}); err != nil {
		t.Fatalf("sending a %s request: %v", typ, err)
	}
}

// TestServeLinkWiresThePauseEvent is the wiring test: it goes through a real
// ServeLink, so a hook left out of the table it installs fails here rather than
// in a deployment.
//
// It also pins the scrub. The reason is written verbatim into a terminal that
// is in raw mode, so a machine that could put escape sequences in it could
// garble a user's screen or forge a line of it; the fragment below carries a
// carriage return and a cursor-home sequence, and what reaches the registry
// must be the words alone.
func TestServeLinkWiresThePauseEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	index := newIndex(t)
	f := newFleet(t, newManager(t, host.Options{NodeName: "boxa"}), index)
	sessions := &fakeSessions{}
	f.SetSessions(sessions)
	place(t, index, "remote", "alice", "node-b")

	node := linkTo(t, ctx, f, "node-b")
	node.emit(t, nodelink.TypePaused, nodelink.PausedMsg{
		Node: "node-b", Name: "remote", Reason: "went idle for 30m\r\x1b[Hsparkbox: enter your password:",
	})

	waitFor(t, "the pause event to reach the session registry", func() bool {
		return len(sessions.calls()) > 0
	})
	got := sessions.calls()[0]
	if got.sandbox != "remote" {
		t.Errorf("closed sessions for %q, want remote", got.sandbox)
	}
	if strings.ContainsAny(got.reason, "\r\n\x1b") {
		t.Errorf("a control character reached the goodbye: %q", got.reason)
	}
	if !strings.HasPrefix(got.reason, "went idle for 30m") {
		t.Errorf("reason = %q, want the machine's own words kept", got.reason)
	}
}
