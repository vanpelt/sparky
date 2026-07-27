package sshgw

// The GitHub linking dialog as a person meets it over SSH.
//
// What is pinned here is not the linking — that is ctlops' — but the shape of
// the conversation: that the code is printed where somebody can read and copy
// it, that a host with no GitHub app degrades to the check that needs none, and
// that the offer appears at the doors people actually walk through without
// appearing at them forever.

import (
	"context"
	"errors"
	"strings"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// scriptedDevice is GitHub's device flow with the human already decided.
type scriptedDevice struct {
	profile users.GitHubProfile
	waitErr error
}

func (d scriptedDevice) Start(context.Context) (users.DeviceCode, error) {
	return users.DeviceCode{
		UserCode:        "WDJB-MJHT",
		Code:            "secret-device-code",
		VerificationURI: "https://github.com/login/device",
	}, nil
}

func (d scriptedDevice) Wait(context.Context, users.DeviceCode) (users.GitHubProfile, error) {
	if d.waitErr != nil {
		return users.GitHubProfile{}, d.waitErr
	}
	return d.profile, nil
}

// listedKeys is github.com/<login>.keys answering yes to everything, which is
// what the key-proof path needs to succeed without a network.
type listedKeys struct{ listed bool }

func (l listedKeys) Fetch(context.Context, string) ([]xssh.PublicKey, error) { return nil, nil }
func (l listedKeys) Verify(context.Context, string, xssh.PublicKey) (bool, error) {
	return l.listed, nil
}
func (l listedKeys) Profile(_ context.Context, login string) (users.GitHubProfile, error) {
	return users.GitHubProfile{Login: login, ID: 4242}, nil
}

// The item in one test: the code and the place to type it are both on screen,
// and what comes back is linked to the account GitHub named.
func TestGitHubLinkShowsTheCodeAndLinksWhatGitHubSays(t *testing.T) {
	st := newCtlStackGitHub(t, scriptedDevice{
		profile: users.GitHubProfile{Login: "octocat", ID: 583231},
	}, nil)

	s := st.run(t, "alice", "github", "link")
	out := s.out.String()
	if s.code != 0 {
		t.Fatalf("exit = %d, stderr = %q", s.code, s.stderr.String())
	}
	if !strings.Contains(out, "WDJB-MJHT") {
		t.Errorf("the user code is not on screen:\n%s", out)
	}
	if !strings.Contains(out, "https://github.com/login/device") {
		t.Errorf("nowhere to enter the code:\n%s", out)
	}
	// The device code is the credential half of the flow. Anyone holding it can
	// collect the token, and a terminal is a place things get pasted into
	// issues.
	if strings.Contains(out, "secret-device-code") {
		t.Errorf("the device code was printed to the session:\n%s", out)
	}
	if !strings.Contains(out, "github.com/octocat") {
		t.Errorf("the linked account is not confirmed:\n%s", out)
	}
	u, err := st.users.Get("alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.GitHubLogin != "octocat" || u.GitHubVia != users.GitHubViaDevice {
		t.Errorf("stored link = %q via %q, want octocat via %q",
			u.GitHubLogin, u.GitHubVia, users.GitHubViaDevice)
	}
}

// Declining is not a crash and not a success. The session says what happened,
// names the command to run later, and exits non-zero so a script can tell.
func TestGitHubLinkDeclinedLeavesTheAccountAlone(t *testing.T) {
	st := newCtlStackGitHub(t, scriptedDevice{waitErr: users.ErrDeviceDenied}, nil)

	s := st.run(t, "alice", "github", "link")
	if s.code == 0 {
		t.Error("a declined authorization exited 0")
	}
	if !strings.Contains(s.out.String(), "github link") {
		t.Errorf("no way back is offered:\n%s", s.out.String())
	}
	u, err := st.users.Get("alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.GitHubLogin != "" {
		t.Errorf("a declined authorization linked %q", u.GitHubLogin)
	}
}

// A host with no GitHub app configured falls back to the check that needs none.
// With no PTY and no login on the command line there is nothing to ask with, so
// it says what to type instead of hanging on a prompt nobody can see.
func TestGitHubLinkWithoutAnAppAsksForALogin(t *testing.T) {
	st := newCtlStackGitHub(t, nil, listedKeys{listed: true})

	s := st.run(t, "alice", "github", "link")
	if s.code == 0 {
		t.Error("exited 0 having linked nothing")
	}
	if !strings.Contains(s.out.String(), "github link <login>") {
		t.Errorf("output does not say how to name the account:\n%s", s.out.String())
	}

	// Named on the command line, the same host links it — by the key check, and
	// recorded as such.
	s = st.run(t, "alice", "github", "link", "alice-gh")
	if s.code != 0 {
		t.Fatalf("exit = %d, out = %q", s.code, s.out.String())
	}
	u, err := st.users.Get("alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.GitHubLogin != "alice-gh" || u.GitHubVia != users.GitHubViaKeys {
		t.Errorf("stored link = %q via %q, want alice-gh via %q",
			u.GitHubLogin, u.GitHubVia, users.GitHubViaKeys)
	}
}

// A key that is not published on GitHub is refused with something to do about
// it, and the account is untouched.
func TestGitHubLinkByKeyRefusesAnUnlistedKey(t *testing.T) {
	st := newCtlStackGitHub(t, nil, listedKeys{listed: false})

	s := st.run(t, "alice", "github", "link", "alice-gh")
	if s.code == 0 {
		t.Error("an unlisted key linked an account")
	}
	if !strings.Contains(s.out.String(), "github.com") {
		t.Errorf("the refusal does not say where to add the key:\n%s", s.out.String())
	}
	if u, _ := st.users.Get("alice"); u.GitHubLogin != "" {
		t.Errorf("linked %q anyway", u.GitHubLogin)
	}
}

// The offer has to reach people who never went through signup@, which is what
// the nudge is for — and it has to stop once they have answered it.
func TestTheNudgeAppearsOnlyForAnUnlinkedAccount(t *testing.T) {
	st := newCtlStackGitHub(t, scriptedDevice{
		profile: users.GitHubProfile{Login: "octocat"},
	}, nil)

	// Before answering, an account is offered the link.
	sess := st.newSession("alice")
	st.gw.nudgeGitHub(sess, sess.Stderr(), ctlops.Caller{Handle: "alice"})
	if got := sess.stderr.String(); !strings.Contains(got, "github link") {
		t.Fatalf("an unlinked account was not offered the link: %q", got)
	}

	if s := st.run(t, "alice", "github", "link"); s.code != 0 {
		t.Fatalf("link failed: %q", s.out.String())
	}
	// Now that it is answered, the same door must stop asking.
	sess = st.newSession("alice")
	st.gw.nudgeGitHub(sess, sess.Stderr(), ctlops.Caller{Handle: "alice"})
	if got := sess.stderr.String(); got != "" {
		t.Errorf("a linked account was nudged again: %q", got)
	}

	// And an account that has not answered still is.
	sess = st.newSession("mallory")
	st.gw.nudgeGitHub(sess, sess.Stderr(), ctlops.Caller{Handle: "mallory"})
	if got := sess.stderr.String(); !strings.Contains(got, "github link") {
		t.Errorf("an unlinked account was not offered the link: %q", got)
	}
}

// whoami shows the provenance, because it is what decides whether
// `keys import-github` will work and somebody refused there needs to be able to
// read why.
func TestWhoamiShowsHowTheLinkWasProved(t *testing.T) {
	st := newCtlStackGitHub(t, scriptedDevice{
		profile: users.GitHubProfile{Login: "octocat"},
	}, nil)
	if s := st.run(t, "alice", "github", "link"); s.code != 0 {
		t.Fatalf("link failed: %q", s.out.String())
	}
	s := st.run(t, "alice", "whoami")
	if !strings.Contains(s.out.String(), "via "+users.GitHubViaDevice) {
		t.Errorf("whoami hides the provenance:\n%s", s.out.String())
	}
}

func TestGitHubUnknownSubcommandIsUsage(t *testing.T) {
	st := newCtlStackGitHub(t, scriptedDevice{}, nil)
	s := st.run(t, "alice", "github", "unlink")
	if s.code != 2 {
		t.Errorf("exit = %d, want 2 for a command that does not exist", s.code)
	}
	if !strings.Contains(s.stderr.String(), "unknown github command") {
		t.Errorf("stderr = %q", s.stderr.String())
	}
}

// An account is never left holding a link nobody proved: every path that fails
// leaves the column empty, which is what the nudge and `whoami` both read.
func TestUpstreamFailureLinksNothing(t *testing.T) {
	st := newCtlStackGitHub(t, scriptedDevice{waitErr: errors.New("github.com unreachable")}, nil)
	if s := st.run(t, "alice", "github", "link"); s.code == 0 {
		t.Error("exited 0 after github.com was unreachable")
	}
	if u, _ := st.users.Get("alice"); u.GitHubLogin != "" {
		t.Errorf("linked %q with no answer from GitHub", u.GitHubLogin)
	}
}
