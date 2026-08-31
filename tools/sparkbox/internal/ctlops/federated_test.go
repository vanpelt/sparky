package ctlops

import (
	"context"
	"errors"
	"strings"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// errTestGitHub is what the fake github.com fails with when a test wants the
// upstream half of a sign-in to be the thing that goes wrong.
var errTestGitHub = errors.New("github.com: connection reset")

// The keys github.com publishes for the login these tests admit. They are not
// testKey, which the rig already has on alice's account: a key another account
// holds is ErrKeyLinked, which is a case worth testing on purpose rather than
// tripping over in every fixture.
const (
	newcomerKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHbUqT2leHVWXUiH57OnraZZmAmMRRB0Yh/NKALRYDY6 newcomer@laptop"
	newcomerKey2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG0yA+Aj2kUEAy1A4btcXwLlxqkIG3/AvQNdZS2aFNXD newcomer@desktop"
)

// publishes points the fake github.com at a login, so a test can say "this
// account publishes these keys" in one line.
func publishes(t *testing.T, r *rig, login string, lines ...string) {
	t.Helper()
	if r.github.keys == nil {
		r.github.keys = map[string][]xssh.PublicKey{}
	}
	for _, line := range lines {
		key, _ := mustKey(t, line)
		r.github.keys[login] = append(r.github.keys[login], key)
	}
}

// A login github.com publishes keys for gets an account holding exactly those
// keys — which is what makes `github-keys` a true statement about it, and what
// makes the account usable over ssh from the first click.
func TestAdmitCreatesFromPublishedKeys(t *testing.T) {
	r := newRig(t)
	r.github.id = 4242
	publishes(t, r, "newcomer", newcomerKey)

	got, err := r.ops.AdmitGitHubLogin(context.Background(), "newcomer", "new@wandb.com")
	if err != nil {
		t.Fatalf("AdmitGitHubLogin: %v", err)
	}
	if got.Handle != "newcomer" || !got.Created || !got.Strong || got.Keys != 1 {
		t.Fatalf("admission = %+v", got)
	}
	u, err := r.accts.Get("newcomer")
	if err != nil {
		t.Fatalf("account not created: %v", err)
	}
	if u.GitHubVia != users.GitHubViaKeys {
		t.Fatalf("provenance = %q, want %q", u.GitHubVia, users.GitHubViaKeys)
	}
	if !users.StrongGitHubLink(u.GitHubVia) {
		t.Fatal("an account built from published keys should hold a strong link")
	}
	if u.GitHubID != 4242 {
		t.Fatalf("github id = %d", u.GitHubID)
	}
	if u.Email != "new@wandb.com" {
		t.Fatalf("email = %q", u.Email)
	}
}

// A login with nothing published still gets in, and the account records what it
// actually is: a weak link that users.StrongGitHubLink refuses, so nothing
// downstream mistakes HiveMind's word for evidence from GitHub.
func TestAdmitCreatesKeylessWhenGitHubPublishesNothing(t *testing.T) {
	r := newRig(t)

	got, err := r.ops.AdmitGitHubLogin(context.Background(), "newcomer", "")
	if err != nil {
		t.Fatalf("AdmitGitHubLogin: %v", err)
	}
	if !got.Created || got.Strong || got.Keys != 0 {
		t.Fatalf("admission = %+v, want a created, weak, keyless account", got)
	}
	u, err := r.accts.Get("newcomer")
	if err != nil {
		t.Fatalf("account not created: %v", err)
	}
	if u.GitHubVia != users.GitHubViaAssertion {
		t.Fatalf("provenance = %q, want %q", u.GitHubVia, users.GitHubViaAssertion)
	}
	if users.StrongGitHubLink(u.GitHubVia) {
		t.Fatal("an asserted link must never be strong — it reaches key adoption and the `github` claim")
	}
	if keys, _ := r.accts.Keys("newcomer"); len(keys) != 0 {
		t.Fatalf("a keyless account holds %d keys", len(keys))
	}
}

// The account a federated sign-in makes must not be an operator: that inviter
// is what IsOperator() tests, and it carries node administration and the
// ability to open anybody's private sandbox URLs.
func TestAdmitNeverMintsAnOperator(t *testing.T) {
	r := newRig(t)
	publishes(t, r, "newcomer", newcomerKey)

	if _, err := r.ops.AdmitGitHubLogin(context.Background(), "newcomer", ""); err != nil {
		t.Fatalf("AdmitGitHubLogin: %v", err)
	}
	u, _ := r.accts.Get("newcomer")
	if u.IsOperator() {
		t.Fatal("a federated sign-in minted an operator")
	}
	if u.InvitedBy != FederatedInviter {
		t.Fatalf("invited by %q, want %q", u.InvitedBy, FederatedInviter)
	}
	// The inviter is not a claimable handle, so it can never collide with a
	// real account somebody could sign in to.
	if users.ValidHandle(FederatedInviter) {
		t.Fatalf("%q is a claimable handle", FederatedInviter)
	}
}

// A second click is a sign-in, not a refusal — and it is how a key published
// since last time reaches the platform.
func TestAdmitIsIdempotentAndAdoptsNewKeys(t *testing.T) {
	r := newRig(t)
	publishes(t, r, "newcomer", newcomerKey)
	if _, err := r.ops.AdmitGitHubLogin(context.Background(), "newcomer", ""); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	publishes(t, r, "newcomer", newcomerKey2)

	got, err := r.ops.AdmitGitHubLogin(context.Background(), "newcomer", "")
	if err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if got.Created {
		t.Fatal("a second sign-in reported the account as created")
	}
	if got.Keys != 2 {
		t.Fatalf("keys = %d, want the newly published one adopted", got.Keys)
	}
}

// An account that arrived keyless earns a strong link the first time its owner
// publishes a key — the assertion was an accelerator, and GitHub is still what
// the link ends up resting on.
func TestAdmitUpgradesAKeylessAccountOnceKeysAppear(t *testing.T) {
	r := newRig(t)
	if _, err := r.ops.AdmitGitHubLogin(context.Background(), "newcomer", ""); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if u, _ := r.accts.Get("newcomer"); u.GitHubVia != users.GitHubViaAssertion {
		t.Fatalf("setup: provenance = %q", u.GitHubVia)
	}
	publishes(t, r, "newcomer", newcomerKey)

	got, err := r.ops.AdmitGitHubLogin(context.Background(), "newcomer", "")
	if err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if !got.Strong {
		t.Fatal("published keys did not upgrade the link")
	}
	if u, _ := r.accts.Get("newcomer"); u.GitHubVia != users.GitHubViaKeys {
		t.Fatalf("provenance = %q, want %q", u.GitHubVia, users.GitHubViaKeys)
	}
}

// The handle-collision rule, which is the one that keeps a name clash from
// being an account takeover: alice exists and is not github.com/alice.
func TestAdmitRefusesAHandleThatBelongsToSomebodyElse(t *testing.T) {
	r := newRig(t)
	publishes(t, r, "alice", testKey)

	_, err := r.ops.AdmitGitHubLogin(context.Background(), "alice", "")
	if err == nil {
		t.Fatal("signed a stranger into an existing account")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindConflict {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if !strings.Contains(err.Error(), "belongs to somebody else") {
		t.Fatalf("unhelpful refusal: %v", err)
	}
	// And nothing was adopted onto the stranger's account.
	if keys, _ := r.accts.Keys("alice"); len(keys) != 1 {
		t.Fatalf("alice's key list changed: %d keys", len(keys))
	}
}

// A linked account whose login matches is theirs, collision rule or not.
func TestAdmitSignsInALinkedAccount(t *testing.T) {
	r := newRig(t)
	if err := r.accts.LinkGitHub("alice", "Alice", users.GitHubViaKeys, 1); err != nil {
		t.Fatal(err)
	}
	got, err := r.ops.AdmitGitHubLogin(context.Background(), "alice", "")
	if err != nil {
		t.Fatalf("AdmitGitHubLogin: %v", err)
	}
	if got.Handle != "alice" || got.Created {
		t.Fatalf("admission = %+v", got)
	}
}

// A disabled account cannot be signed into, however good the handoff was.
func TestAdmitRefusesADisabledAccount(t *testing.T) {
	r := newRig(t)
	u := r.accts.users["alice"]
	u.Status, u.GitHubLogin = "disabled", "alice"
	r.accts.users["alice"] = u

	_, err := r.ops.AdmitGitHubLogin(context.Background(), "alice", "")
	if err == nil {
		t.Fatal("a disabled account was signed into")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("unhelpful refusal: %v", err)
	}
}

// github.com being unreachable must not create a keyless account for somebody
// who publishes keys — that would record `assertion` permanently for a person
// who had earned `github-keys`.
func TestAdmitRefusesToGuessWhenGitHubIsUnreachable(t *testing.T) {
	r := newRig(t)
	r.github.err = errTestGitHub

	_, err := r.ops.AdmitGitHubLogin(context.Background(), "newcomer", "")
	if err == nil {
		t.Fatal("an unreachable github.com still created an account")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindUpstream {
		t.Fatalf("err = %v, want an upstream failure", err)
	}
	if _, err := r.accts.Get("newcomer"); err == nil {
		t.Fatal("an account was created anyway")
	}
}

// The same failure against an account that already exists is survivable: the
// key list is an upgrade, and refusing a sign-in over it would strand somebody
// because github.com was slow.
func TestAdmitSignsInDespiteAnUnreachableGitHub(t *testing.T) {
	r := newRig(t)
	if err := r.accts.LinkGitHub("alice", "alice", users.GitHubViaKeys, 1); err != nil {
		t.Fatal(err)
	}
	r.github.err = errTestGitHub

	got, err := r.ops.AdmitGitHubLogin(context.Background(), "alice", "")
	if err != nil {
		t.Fatalf("AdmitGitHubLogin: %v", err)
	}
	if got.Handle != "alice" || !got.Strong {
		t.Fatalf("admission = %+v", got)
	}
}

// An email already on the account is not replaced by whatever the identity
// provider currently thinks.
func TestAdmitNeverOverwritesAnEmail(t *testing.T) {
	r := newRig(t)
	if err := r.accts.LinkGitHub("alice", "alice", users.GitHubViaKeys, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.accts.SetEmail("alice", "alice@her-own-domain.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ops.AdmitGitHubLogin(context.Background(), "alice", "someone-else@wandb.com"); err != nil {
		t.Fatalf("AdmitGitHubLogin: %v", err)
	}
	if u, _ := r.accts.Get("alice"); u.Email != "alice@her-own-domain.test" {
		t.Fatalf("email was overwritten to %q", u.Email)
	}
}

// A login no handle can be derived from is named as an operator problem, not
// left to fail somewhere downstream.
func TestAdmitRefusesALoginWithNoDerivableHandle(t *testing.T) {
	r := newRig(t)
	for _, login := range []string{
		"a-login-far-too-long-to-be-a-sparkbox-handle",
		"console", // reserved: a door answers there
		"not a login",
		"",
	} {
		if _, err := r.ops.AdmitGitHubLogin(context.Background(), login, ""); err == nil {
			t.Errorf("login %q was admitted", login)
		}
	}
}
