package ctlops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// Three distinct real ed25519 keys, so "adopted every published key" and "a key
// another account already holds" are testable as different facts rather than
// as the same byte string twice.
const (
	ghKey1 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH2AdPYizcJR/KGFvpd84KgNtSjvsIec+tMkJ5lsYclN one"
	ghKey2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICjzzY7x3T9JHhzrBGUz8p/2pkaqY8S9hT31vfkxVqAS two"
	ghKey3 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGLI8qOcxEenuvAApPZ/nURB0LWV/2NHcQzblDttHkuX three"
)

func opsy() Caller { return Caller{Handle: "opsy"} }

// ghKeys parses the given authorized_keys lines and registers them as what
// github.com publishes for login.
func ghKeys(t *testing.T, r *rig, login string, lines ...string) []xssh.PublicKey {
	t.Helper()
	var ks []xssh.PublicKey
	for _, l := range lines {
		k, _ := mustKey(t, l)
		ks = append(ks, k)
	}
	r.github.keys[login] = ks
	return ks
}

// TestProvisioningIsOperatorGated is the gate the whole feature rests on: these
// verbs create accounts, so operator status is resolved from the account store
// inside each method and can never be asserted by a transport — Caller has no
// operator field to assert with.
func TestProvisioningIsOperatorGated(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ghKeys(t, r, "octocat", ghKey1)

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"user add", func() error {
			_, err := r.ops.ProvisionGitHubUsers(ctx, alice(), []string{"octocat"}, false)
			return err
		}},
		{"user sync-github-org", func() error {
			_, err := r.ops.ProvisionGitHubOrg(ctx, alice(), "wandb", "", "tok", false)
			return err
		}},
		{"user ls", func() error { _, err := r.ops.ListAccounts(alice()); return err }},
	} {
		err := tc.call()
		if !IsKind(err, KindDenied) {
			t.Errorf("%s by a non-operator = %v, want KindDenied", tc.name, err)
		}
		var e *Error
		if errors.As(err, &e) && e.Code != "not_operator" {
			t.Errorf("%s code = %q, want not_operator", tc.name, e.Code)
		}
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Errorf("a refused provisioning still wrote: %v", got)
	}
}

// THE security property of this feature. A provisioned colleague must be an
// ordinary user, because IsOperator() carries node administration and — via
// proxy.mayView — the ability to open any other user's private sandbox URLs.
// Seeding them into users.conf, the obvious shortcut, would do exactly that.
func TestProvisionedAccountsAreNotOperators(t *testing.T) {
	r := newRig(t)
	ghKeys(t, r, "octocat", ghKey1)

	if _, err := r.ops.ProvisionGitHubUsers(context.Background(), opsy(), []string{"octocat"}, false); err != nil {
		t.Fatalf("ProvisionGitHubUsers: %v", err)
	}
	u, err := r.accts.Get("octocat")
	if err != nil {
		t.Fatalf("the account was not created: %v", err)
	}
	if u.IsOperator() {
		t.Fatal("a provisioned account is an operator — it can administer nodes and read every private route")
	}
	if u.InvitedBy != "opsy" {
		t.Errorf("InvitedBy = %q, want the provisioning operator's handle", u.InvitedBy)
	}
}

// Every key GitHub publishes is adopted, not just the first: people have a
// laptop and a desktop, and an account that authenticates from only one of them
// sends the user back to an invite code.
func TestProvisioningAdoptsEveryPublishedKey(t *testing.T) {
	r := newRig(t)
	ghKeys(t, r, "octocat", ghKey1, ghKey2, ghKey3)

	res, err := r.ops.ProvisionGitHubUsers(context.Background(), opsy(), []string{"octocat"}, false)
	if err != nil {
		t.Fatalf("ProvisionGitHubUsers: %v", err)
	}
	if res.Created != 1 || res.Users[0].Outcome != OutcomeCreated {
		t.Fatalf("result = %+v, want one created", res)
	}
	keys, _ := r.accts.Keys("octocat")
	if len(keys) != 3 {
		t.Fatalf("account holds %d keys, want all 3 github lists", len(keys))
	}
}

// The link is recorded as github-keys, which is a statement of fact rather than
// a convenience: the account's keys ARE what github.com publishes for the
// login, so the provenance's claim is true by construction. It matters because
// StrongGitHubLink gates `keys import-github`, which is how the user picks up a
// new key later without an operator.
func TestProvisioningRecordsAStrongGitHubLink(t *testing.T) {
	r := newRig(t)
	ghKeys(t, r, "octocat", ghKey1)
	r.github.id = 583231

	if _, err := r.ops.ProvisionGitHubUsers(context.Background(), opsy(), []string{"octocat"}, false); err != nil {
		t.Fatalf("ProvisionGitHubUsers: %v", err)
	}
	u, _ := r.accts.Get("octocat")
	if u.GitHubLogin != "octocat" || u.GitHubID != 583231 {
		t.Errorf("link = %s/%d, want octocat/583231", u.GitHubLogin, u.GitHubID)
	}
	if !users.StrongGitHubLink(u.GitHubVia) {
		t.Errorf("provenance %q is not strong, so the user can never self-import a new key", u.GitHubVia)
	}
}

// Re-running a sync is the mechanism for picking up a colleague's new laptop
// key, so it must be free the rest of the time.
func TestProvisioningIsIdempotentAndPicksUpNewKeys(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ghKeys(t, r, "octocat", ghKey1)
	if _, err := r.ops.ProvisionGitHubUsers(ctx, opsy(), []string{"octocat"}, false); err != nil {
		t.Fatalf("first run: %v", err)
	}

	res, err := r.ops.ProvisionGitHubUsers(ctx, opsy(), []string{"octocat"}, false)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Users[0].Outcome != OutcomeCurrent {
		t.Errorf("re-run outcome = %q, want %q", res.Users[0].Outcome, OutcomeCurrent)
	}

	// A key appears on GitHub; the next sync adopts it.
	ghKeys(t, r, "octocat", ghKey1, ghKey2)
	res, err = r.ops.ProvisionGitHubUsers(ctx, opsy(), []string{"octocat"}, false)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if res.Users[0].Outcome != OutcomeKeysAdded || res.Updated != 1 {
		t.Errorf("outcome = %q (updated %d), want %q", res.Users[0].Outcome, res.Updated, OutcomeKeysAdded)
	}
	if keys, _ := r.accts.Keys("octocat"); len(keys) != 2 {
		t.Errorf("account holds %d keys, want 2", len(keys))
	}
}

// The account-theft case. A handle that already belongs to somebody who is NOT
// this GitHub login must not have a stranger's keys adopted onto it — that
// would hand the existing account away to whoever holds the GitHub login.
func TestProvisioningRefusesToAdoptOntoAStrangersHandle(t *testing.T) {
	r := newRig(t)
	ghKeys(t, r, "alice", ghKey2) // github.com/alice is a different person from handle "alice"

	res, err := r.ops.ProvisionGitHubUsers(context.Background(), opsy(), []string{"alice"}, false)
	if err != nil {
		t.Fatalf("ProvisionGitHubUsers: %v", err)
	}
	if res.Users[0].Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %q, want %q", res.Users[0].Outcome, OutcomeSkipped)
	}
	keys, _ := r.accts.Keys("alice")
	for _, k := range keys {
		if k.Via == "github-import" {
			t.Fatal("a stranger's github key was adopted onto an existing account")
		}
	}
}

// Somebody with no published key is reported, not silently dropped: they are
// exactly the people who still need an invite, and an operator who cannot see
// them will assume the sync covered everyone.
func TestProvisioningReportsPeopleWithNoPublishedKey(t *testing.T) {
	r := newRig(t)
	ghKeys(t, r, "haskeys", ghKey1)
	r.github.keys["nokeys"] = nil

	res, err := r.ops.ProvisionGitHubUsers(context.Background(), opsy(), []string{"haskeys", "nokeys"}, false)
	if err != nil {
		t.Fatalf("ProvisionGitHubUsers: %v", err)
	}
	if len(res.Users) != 2 {
		t.Fatalf("reported %d logins, want both", len(res.Users))
	}
	byLogin := map[string]string{}
	for _, u := range res.Users {
		byLogin[u.Login] = u.Outcome
	}
	if byLogin["nokeys"] != OutcomeNoKeys {
		t.Errorf("nokeys outcome = %q, want %q", byLogin["nokeys"], OutcomeNoKeys)
	}
	if byLogin["haskeys"] != OutcomeCreated {
		t.Errorf("haskeys outcome = %q, want it created anyway", byLogin["haskeys"])
	}
}

// A dry run must report exactly what an apply would do, and write nothing.
func TestProvisioningDryRunWritesNothing(t *testing.T) {
	r := newRig(t)
	ghKeys(t, r, "octocat", ghKey1)
	r.calls.reset()

	res, err := r.ops.ProvisionGitHubUsers(context.Background(), opsy(), []string{"octocat"}, true)
	if err != nil {
		t.Fatalf("ProvisionGitHubUsers: %v", err)
	}
	if !res.DryRun || res.Created != 1 {
		t.Fatalf("result = %+v, want a dry run reporting one create", res)
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Fatalf("a dry run wrote: %v", got)
	}
	if _, err := r.accts.Get("octocat"); err == nil {
		t.Fatal("a dry run created the account")
	}
}

// One unreachable profile must not sink a sync of two hundred people.
func TestProvisioningReportsPerLoginFailuresWithoutAborting(t *testing.T) {
	r := newRig(t)
	ghKeys(t, r, "good", ghKey1)
	// A login too long to be a handle: refused locally, before github is asked.
	res, err := r.ops.ProvisionGitHubUsers(context.Background(), opsy(),
		[]string{"good", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, false)
	if err != nil {
		t.Fatalf("ProvisionGitHubUsers: %v", err)
	}
	if res.Created != 1 || res.Skipped != 1 || res.Examined != 2 {
		t.Fatalf("result = %+v, want 1 created / 1 skipped / 2 examined", res)
	}
}

// An org whose roster comes back empty is reported as a configuration problem,
// not as a clean run: a token without read:org sees only public members, and
// public membership is opt-in and rare, so "0 accounts, all good" would send
// the operator looking in the wrong place.
func TestProvisionGitHubOrgRejectsAnEmptyRoster(t *testing.T) {
	r := newRig(t)
	r.ops.orgMembers = func(ctx context.Context, org, team, token string) ([]string, error) {
		return nil, nil
	}
	_, err := r.ops.ProvisionGitHubOrg(context.Background(), opsy(), "wandb", "", "tok", false)
	if !IsKind(err, KindInvalid) {
		t.Fatalf("err = %v, want KindInvalid", err)
	}
	var e *Error
	if errors.As(err, &e) && e.Code != "no_members" {
		t.Errorf("code = %q, want no_members", e.Code)
	}
}

func TestProvisionGitHubOrgProvisionsTheRoster(t *testing.T) {
	r := newRig(t)
	ghKeys(t, r, "one", ghKey1)
	ghKeys(t, r, "two", ghKey2)
	r.ops.orgMembers = func(ctx context.Context, org, team, token string) ([]string, error) {
		if org != "wandb" || token != "ghp_x" {
			t.Errorf("org/token = %q/%q, want wandb/ghp_x", org, token)
		}
		return []string{"one", "two"}, nil
	}
	res, err := r.ops.ProvisionGitHubOrg(context.Background(), opsy(), "wandb", "", "ghp_x", false)
	if err != nil {
		t.Fatalf("ProvisionGitHubOrg: %v", err)
	}
	if res.Org != "wandb" || res.Created != 2 {
		t.Fatalf("result = %+v, want 2 created for wandb", res)
	}
}

func TestProvisionGitHubOrgNeedsAToken(t *testing.T) {
	r := newRig(t)
	_, err := r.ops.ProvisionGitHubOrg(context.Background(), opsy(), "wandb", "", "   ", false)
	if !IsKind(err, KindInvalid) {
		t.Fatalf("err = %v, want KindInvalid", err)
	}
}

// --team narrows the roster read to one team, which is the usable unit: a
// company org runs to hundreds of people, nearly all of whom will never open a
// sandbox, and provisioning all of them is what maxProvisionLogins refuses.
func TestProvisionGitHubOrgCanNarrowToATeam(t *testing.T) {
	r := newRig(t)
	ghKeys(t, r, "one", ghKey1)
	var gotTeam string
	r.ops.orgMembers = func(ctx context.Context, org, team, token string) ([]string, error) {
		gotTeam = team
		return []string{"one"}, nil
	}
	res, err := r.ops.ProvisionGitHubOrg(context.Background(), opsy(), "wandb", "infra", "tok", false)
	if err != nil {
		t.Fatalf("ProvisionGitHubOrg: %v", err)
	}
	if gotTeam != "infra" {
		t.Errorf("team = %q, want infra", gotTeam)
	}
	if res.Org != "wandb/infra" {
		t.Errorf("Org = %q, want it to name the team too", res.Org)
	}
}

// A roster bigger than the cap is refused, and the refusal has to name the way
// out — otherwise the operator's only visible option is to give up.
func TestProvisionGitHubOrgRefusesAnEntireCompany(t *testing.T) {
	r := newRig(t)
	big := make([]string, maxProvisionLogins+1)
	for i := range big {
		big[i] = fmt.Sprintf("user%04d", i)
	}
	r.ops.orgMembers = func(ctx context.Context, org, team, token string) ([]string, error) {
		return big, nil
	}
	_, err := r.ops.ProvisionGitHubOrg(context.Background(), opsy(), "wandb", "", "tok", false)
	if !IsKind(err, KindInvalid) {
		t.Fatalf("err = %v, want KindInvalid", err)
	}
	if !strings.Contains(err.Error(), "--team") {
		t.Errorf("refusal %q does not name the escape hatch", err)
	}
}
