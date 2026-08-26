package sshgw

// What `ctl sessions` prints. The empty case is the one worth pinning: it is
// the answer a person gets right after wiring the feature up, and a bare "none"
// would leave them unable to tell a VM that has never run an agent from one
// whose daemon is broken.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

type stubHiveMind struct {
	snapshot host.HiveMindSessionSnapshot
	err      error
	asked    []string
}

func (s *stubHiveMind) Sessions(
	_ context.Context,
	box *host.Sandbox,
	_ int,
) (host.HiveMindSessionSnapshot, error) {
	s.asked = append(s.asked, box.Name)
	return s.snapshot, s.err
}

func sessionsStack(t *testing.T, hm ctlops.HiveMind) *ctlStack {
	t.Helper()
	st := newCtlStackWith(t, testRoster(), func(cfg *ctlops.Config) {
		cfg.HiveMind = hm
	})
	if _, err := st.mgr.Create(context.Background(), "alice-box", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSessionsRendersTheCatalog(t *testing.T) {
	now := time.Now()
	hm := &stubHiveMind{snapshot: host.HiveMindSessionSnapshot{
		Sessions: []host.HiveMindSession{
			{
				ID: "s1", Title: "Wire up presence", State: "active",
				AgentType: "claude", Model: "opus-5",
				URL:            "https://hivemind.example/sessions/s1",
				StartedAt:      now.Add(-2 * time.Hour),
				LastActivityAt: now.Add(-30 * time.Second),
			},
			{
				ID: "s2", State: "ended", AgentType: "codex",
				StartedAt:      now.Add(-72 * time.Hour),
				LastActivityAt: now.Add(-72 * time.Hour),
			},
		},
		TotalCount: 2,
	}}
	st := sessionsStack(t, hm)

	s := st.run(t, "alice", "sessions", "alice-box")
	out := s.out.String()
	if s.code != 0 {
		t.Fatalf("exit = %d, stderr %q", s.code, s.stderr.String())
	}
	for _, want := range []string{
		"Wire up presence", "active", "just now",
		"claude · opus-5", "https://hivemind.example/sessions/s1",
		"3d ago", "2 sessions from alice-box",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// A session with no title falls back to its ID rather than printing a blank
	// row that looks like a rendering bug.
	if !strings.Contains(out, "s2") {
		t.Errorf("untitled session did not fall back to its ID:\n%s", out)
	}
	if len(hm.asked) != 1 || hm.asked[0] != "alice-box" {
		t.Errorf("asked about %v, want [alice-box]", hm.asked)
	}
}

// TestSessionsEmptySaysWhereToLook — the count alone is not actionable, and
// this is precisely the moment someone needs to know what to check next.
func TestSessionsEmptySaysWhereToLook(t *testing.T) {
	st := sessionsStack(t, &stubHiveMind{})

	s := st.run(t, "alice", "sessions", "alice-box")
	out := s.out.String()
	if s.code != 0 {
		t.Fatalf("exit = %d, stderr %q", s.code, s.stderr.String())
	}
	if !strings.Contains(out, "no HiveMind sessions recorded from alice-box") {
		t.Errorf("output = %q", out)
	}
	if !strings.Contains(out, "systemctl --user status hivemind") {
		t.Errorf("empty answer named no next step:\n%s", out)
	}
}

// TestSessionsPartialPageIsHonest: a total that silently meant "the first page"
// would be worse than no total at all.
func TestSessionsPartialPageIsHonest(t *testing.T) {
	st := sessionsStack(t, &stubHiveMind{snapshot: host.HiveMindSessionSnapshot{
		Sessions:   []host.HiveMindSession{{ID: "s1", Title: "one", State: "ended"}},
		TotalCount: 40, HasMore: true,
	}})

	s := st.run(t, "alice", "sessions", "alice-box")
	if got := s.out.String(); !strings.Contains(got, "1 of 40 sessions from alice-box") {
		t.Errorf("output = %q", got)
	}
}

// TestSessionsMasksAStrangersSandbox: the same masked sentence as every other
// verb, and HiveMind is never asked.
func TestSessionsMasksAStrangersSandbox(t *testing.T) {
	hm := &stubHiveMind{}
	st := sessionsStack(t, hm)
	if _, err := st.mgr.Create(context.Background(), "mallory-box", "mallory", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}

	s := st.run(t, "alice", "sessions", "mallory-box")
	if s.code != 1 {
		t.Fatalf("exit = %d, want 1", s.code)
	}
	if got := s.stderr.String(); !strings.Contains(got, `no sandbox named "mallory-box"`) {
		t.Errorf("stderr = %q", got)
	}
	if len(hm.asked) != 0 {
		t.Errorf("a stranger's sandbox reached HiveMind: %v", hm.asked)
	}
}

// TestSessionsWithoutHiveMindSaysSo rather than printing an empty catalog on a
// host that never asked.
func TestSessionsWithoutHiveMindSaysSo(t *testing.T) {
	st := sessionsStack(t, nil)

	s := st.run(t, "alice", "sessions", "alice-box")
	if s.code != 1 {
		t.Fatalf("exit = %d, want 1", s.code)
	}
	if got := s.stderr.String(); !strings.Contains(got, "isn't enabled on this host") {
		t.Errorf("stderr = %q", got)
	}
}

func TestSessionsNeedsAName(t *testing.T) {
	st := sessionsStack(t, &stubHiveMind{})

	s := st.run(t, "alice", "sessions")
	if s.code != 2 {
		t.Fatalf("exit = %d, want 2", s.code)
	}
	if got := s.stderr.String(); !strings.Contains(got, "sessions <name>") {
		t.Errorf("stderr = %q", got)
	}
}
