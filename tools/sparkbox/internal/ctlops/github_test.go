package ctlops

// Linking a GitHub account: which evidence is accepted, what it is recorded as,
// and what that recording then permits.
//
// The theme is that the three ways to prove a link are not interchangeable. Two
// of them are GitHub speaking about the person holding this account; the third
// is somebody else's word for it. Only the first two may adopt keys, because an
// adopted key authenticates — and a link that could name a stranger's account
// would then be a way to collect that stranger.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// fakeDevice is GitHub's device flow, scripted.
type fakeDevice struct {
	c        *calls
	profile  users.GitHubProfile
	startErr error
	waitErr  error
}

func (f *fakeDevice) Start(context.Context) (users.DeviceCode, error) {
	f.c.add("device.Start")
	if f.startErr != nil {
		return users.DeviceCode{}, f.startErr
	}
	return users.DeviceCode{UserCode: "WDJB-MJHT", Code: "secret-dc",
		VerificationURI: "https://github.com/login/device"}, nil
}

func (f *fakeDevice) Wait(_ context.Context, dc users.DeviceCode) (users.GitHubProfile, error) {
	f.c.add("device.Wait %s", dc.Code)
	if f.waitErr != nil {
		return users.GitHubProfile{}, f.waitErr
	}
	return f.profile, nil
}

// withDevice turns the flow on for one rig, the way --github-client-id does for
// a host. The default rig has it off, which is the state of every deployment
// that has not configured an app.
func withDevice(r *rig, p users.GitHubProfile) *fakeDevice {
	d := &fakeDevice{c: r.calls, profile: p}
	r.ops.ghDevice = d
	return d
}

// The item in one test: the login comes from GitHub, and the link records that
// GitHub is where it came from.
func TestDeviceFlowLinksWhomGitHubNames(t *testing.T) {
	r := newRig(t)
	withDevice(r, users.GitHubProfile{Login: "octocat", ID: 583231})
	c := Caller{Handle: "alice"}

	dc, err := r.ops.StartGitHubLink(context.Background(), c)
	if err != nil {
		t.Fatalf("StartGitHubLink: %v", err)
	}
	if dc.UserCode == "" {
		t.Fatal("nothing to show the user")
	}
	me, err := r.ops.FinishGitHubLink(context.Background(), c, dc)
	if err != nil {
		t.Fatalf("FinishGitHubLink: %v", err)
	}
	if me.GitHubLogin != "octocat" {
		t.Errorf("linked %q, want the account GitHub named", me.GitHubLogin)
	}
	if me.GitHubVia != users.GitHubViaDevice {
		t.Errorf("provenance = %q, want %q", me.GitHubVia, users.GitHubViaDevice)
	}
	// The number GitHub gave is recorded as-is, and no second lookup is made
	// for it: the flow already knows, and a profile fetch here would be a round
	// trip to learn something already in hand.
	if got := r.accts.users["alice"].GitHubID; got != 583231 {
		t.Errorf("github id = %d, want 583231", got)
	}
	for _, call := range r.calls.all() {
		if strings.HasPrefix(call, "github.Profile") {
			t.Errorf("looked up a profile the flow already had: %s", call)
		}
	}
}

// A host with no app configured says so rather than starting something it
// cannot finish, and says it in a way a transport renders as "not available
// here" instead of "you did something wrong".
func TestDeviceFlowRefusesWhenNoAppIsConfigured(t *testing.T) {
	r := newRig(t)
	_, err := r.ops.StartGitHubLink(context.Background(), Caller{Handle: "alice"})
	if err == nil {
		t.Fatal("a host with no GitHub app started a device flow")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindDisabled {
		t.Fatalf("err = %v, want a KindDisabled refusal", err)
	}
	if e.Hint == "" {
		t.Error("the refusal names no other way to link")
	}
}

// Somebody declining on github.com is a decision, not a platform fault, and it
// must not be rendered as one — nor as success.
func TestDeclinedAuthorizationLinksNothing(t *testing.T) {
	r := newRig(t)
	d := withDevice(r, users.GitHubProfile{Login: "octocat"})
	d.waitErr = users.ErrDeviceDenied

	_, err := r.ops.FinishGitHubLink(context.Background(), Caller{Handle: "alice"},
		users.DeviceCode{Code: "secret-dc"})
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindDenied {
		t.Fatalf("err = %v, want a KindDenied refusal", err)
	}
	if u := r.accts.users["alice"]; u.GitHubLogin != "" {
		t.Errorf("a declined authorization linked %q", u.GitHubLogin)
	}
}

// An app with the device flow switched off is an operator's problem, and the
// error has to say so — every user hits it, on the first try, forever.
func TestDeviceFlowDisabledOnTheAppIsAnOperatorFault(t *testing.T) {
	r := newRig(t)
	d := withDevice(r, users.GitHubProfile{})
	d.startErr = users.ErrDeviceUnsupported

	_, err := r.ops.StartGitHubLink(context.Background(), Caller{Handle: "alice"})
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindDisabled {
		t.Fatalf("err = %v, want a KindDisabled refusal", err)
	}
	if !strings.Contains(e.Hint, "operator") {
		t.Errorf("hint = %q, want it to say who can fix this", e.Hint)
	}
}

// The key check records its own provenance, and fills in the account number it
// could not learn from a key.
func TestKeyProofRecordsItsProvenanceAndTheAccountNumber(t *testing.T) {
	r := newRig(t)
	r.github.listed = true
	r.github.id = 4242
	_, fp := mustKey(t, testKey)

	me, err := r.ops.VerifyGitHub(context.Background(), Caller{Handle: "alice", KeyFP: fp}, "alice-gh", "")
	if err != nil {
		t.Fatalf("VerifyGitHub: %v", err)
	}
	if me.GitHubVia != users.GitHubViaKeys {
		t.Errorf("provenance = %q, want %q", me.GitHubVia, users.GitHubViaKeys)
	}
	if got := r.accts.users["alice"].GitHubID; got != 4242 {
		t.Errorf("github id = %d, want the profile lookup's 4242", got)
	}
}

// A profile lookup that fails must not undo a link that was already proved. The
// verification is the fact; the account number is a nicety fetched after it.
func TestAProvedLinkSurvivesAnUnreachableProfileLookup(t *testing.T) {
	r := newRig(t)
	r.github.listed = true
	r.github.id = 4242
	// Only the profile lookup fails. The key check has to pass first, or the
	// test would be asserting about a link that was never proved.
	r.github.profileErr = errors.New("api.github.com unreachable")
	_, fp := mustKey(t, testKey)
	me, err := r.ops.VerifyGitHub(context.Background(), Caller{Handle: "alice", KeyFP: fp}, "alice-gh", "")
	if err != nil {
		t.Fatalf("VerifyGitHub: %v", err)
	}
	if me.GitHubLogin != "alice-gh" {
		t.Errorf("linked %q, want alice-gh", me.GitHubLogin)
	}
	// Unknown, recorded as unknown, link intact.
	if got := r.accts.users["alice"].GitHubID; got != 0 {
		t.Errorf("github id = %d, want 0 when the lookup could not be made", got)
	}
	if r.accts.users["alice"].GitHubVia != users.GitHubViaKeys {
		t.Error("the link was not recorded")
	}
}

// The gate this whole column exists for: only evidence that came from GitHub
// about the person holding THIS account may adopt keys.
//
// An adopted key authenticates. If a link established by somebody else's signed
// word could reach this verb, a person could claim a stranger's login, pre-load
// the stranger's published keys onto their own account, and collect that
// stranger the next time they connected.
func TestOnlyAStrongLinkMayAdoptKeys(t *testing.T) {
	cases := []struct {
		via   string
		allow bool
	}{
		{users.GitHubViaKeys, true},
		{users.GitHubViaDevice, true},
		{users.GitHubViaAssertion, false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run("via="+tc.via, func(t *testing.T) {
			r := newRig(t)
			u := r.accts.users["alice"]
			u.GitHubLogin, u.GitHubVia = "alice-gh", tc.via
			r.accts.users["alice"] = u

			_, err := r.ops.ImportGitHubKeys(context.Background(), Caller{Handle: "alice"})
			switch {
			case tc.allow && err != nil:
				t.Fatalf("a %s link could not import keys: %v", tc.via, err)
			case !tc.allow && err == nil:
				t.Fatalf("a %q link adopted keys", tc.via)
			case !tc.allow:
				var e *Error
				if !errors.As(err, &e) || e.Kind != KindDenied {
					t.Fatalf("err = %v, want a KindDenied refusal", err)
				}
				// And nothing was fetched: the refusal comes before github.com
				// is asked for a stranger's keyring.
				for _, call := range r.calls.all() {
					if strings.HasPrefix(call, "github.Fetch") {
						t.Errorf("keys were fetched for a link too weak to adopt them: %s", call)
					}
				}
			}
		})
	}
}

// Capabilities is what a transport asks before offering a dialog, so it has to
// track the wiring rather than being set alongside it.
func TestCapabilitiesReportTheDeviceFlow(t *testing.T) {
	r := newRig(t)
	if r.ops.Capabilities().GitHubDevice {
		t.Error("a host with no app claims it can run the device flow")
	}
	withDevice(r, users.GitHubProfile{Login: "octocat"})
	if !r.ops.Capabilities().GitHubDevice {
		t.Error("a host with an app claims it cannot")
	}
}
