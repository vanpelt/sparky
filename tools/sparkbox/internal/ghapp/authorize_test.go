package ghapp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

var (
	userInst = Installation{ID: 42, AccountID: 7, AccountLogin: "vanpelt", AccountType: "User"}
	orgInst  = Installation{ID: 99, AccountID: 800, AccountLogin: "wandb", AccountType: "Organization"}
)

// Zero is UNKNOWN, not an account number: it is what every link made before the
// profile fetch existed carries, and what ctlops records when api.github.com is
// slow at link time. Refused before anything else looks at anything, and
// without a request — an unknown identity is not a question github can answer.
func TestAuthorizeRefusesAnUnknownAccountNumber(t *testing.T) {
	s := newStub(t)
	app := newApp(t, s, nil)

	for _, inst := range []Installation{userInst, orgInst, {ID: 1, AccountID: 0, AccountType: "User"}} {
		err := app.Authorize(context.Background(), inst, 0, "vanpelt")
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s installation: err = %v, want ErrForbidden", inst.AccountType, err)
		}
		if !strings.Contains(err.Error(), "account number") {
			t.Errorf("error %q does not say why", err)
		}
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("made %d requests to decide something local", n)
	}
}

// A personal installation binds on the id and only the id. The login is
// renameable and, once released, re-registerable by somebody else.
func TestAuthorizeUserInstallation(t *testing.T) {
	s := newStub(t)
	app := newApp(t, s, nil)

	if err := app.Authorize(context.Background(), userInst, 7, "vanpelt"); err != nil {
		t.Fatalf("the linked account was refused: %v", err)
	}
	// Same login, different account number: the person who took the name after
	// it was released must not inherit the installation.
	err := app.Authorize(context.Background(), userInst, 8, "vanpelt")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("a mismatched account id was allowed: err = %v", err)
	}
	if !strings.Contains(err.Error(), "different github account") {
		t.Errorf("error %q does not say why", err)
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("made %d requests; a user installation is decided from the record in hand", n)
	}
}

// An org installation is decided by github, with a token that can read
// membership and nothing else.
func TestAuthorizeOrgInstallationChecksMembership(t *testing.T) {
	s := newStub(t)
	s.json("POST /app/installations/99/access_tokens", 200, tokenBody)
	s.json("GET /orgs/wandb/memberships/vanpelt", 200, `{"state":"active","role":"member"}`)
	app := newApp(t, s, nil)

	if err := app.Authorize(context.Background(), orgInst, 7, "vanpelt"); err != nil {
		t.Fatalf("an active member was refused: %v", err)
	}

	// The probe token is the one credential this package mints with no
	// repository list, and it is safe precisely because its only permission is
	// members:read — it cannot read a line of code.
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(s.last("access_tokens").body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Repositories) != 0 {
		t.Errorf("the membership probe asked for repositories: %v", body.Repositories)
	}
	if len(body.Permissions) != 1 || body.Permissions["members"] != "read" {
		t.Errorf("probe permissions = %v, want only members:read", body.Permissions)
	}
	if got := s.last("/memberships/").auth; got != "Bearer ghs_supersecret" {
		t.Errorf("the membership call presented %q, want the installation token", got)
	}

	// Both the token and the answer are cached, so a second decision is free.
	if err := app.Authorize(context.Background(), orgInst, 7, "vanpelt"); err != nil {
		t.Fatal(err)
	}
	if n := s.count("/memberships/"); n != 1 {
		t.Errorf("checked membership %d times, want 1", n)
	}
	if n := s.count("access_tokens"); n != 1 {
		t.Errorf("minted %d probe tokens, want 1", n)
	}
}

// The 422 branch is the same operator mistake caught one step earlier, and it
// is the one that actually fires in practice: an installation that was never
// granted `members: read` does not reach the membership call at all, because
// GitHub refuses to MINT a token carrying a permission it does not have. That
// refusal is a 422, which the mint path otherwise reads as "the installation
// does not cover what was asked for" — a sentence about repositories that would
// tell the operator to reinstall an app that is installed correctly.
func TestAuthorizeOrgMembersPermissionRefusedAtMint(t *testing.T) {
	s := newStub(t)
	s.json("POST /app/installations/99/access_tokens", 422,
		`{"message":"The permissions requested are not granted to this installation."}`)
	app := newApp(t, s, nil)

	err := app.Authorize(context.Background(), orgInst, 7, "vanpelt")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if errors.Is(err, ErrNotInstalled) {
		t.Errorf("error %q blames the installation for a missing permission", err)
	}
	if !strings.Contains(err.Error(), "Members: read") {
		t.Errorf("error %q does not name the missing permission", err)
	}
	if strings.Contains(err.Error(), "not a member") {
		t.Errorf("error %q reads as a membership answer, which it is not", err)
	}
	// Never reached the membership endpoint, so there is nothing to have cached.
	if n := s.count("/memberships/"); n != 0 {
		t.Errorf("asked about membership %d times with no usable token, want 0", n)
	}
}

// The 403 branch is the one the contract calls out: it is a permission the
// operator has not granted, and reporting it as "not a member" sends them to
// invite somebody who is already there.
func TestAuthorizeOrgMissingMembersPermission(t *testing.T) {
	s := newStub(t)
	s.json("POST /app/installations/99/access_tokens", 200, tokenBody)
	s.json("GET /orgs/wandb/memberships/vanpelt", 403, `{"message":"Resource not accessible by integration"}`)
	app := newApp(t, s, nil)

	err := app.Authorize(context.Background(), orgInst, 7, "vanpelt")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if !strings.Contains(err.Error(), "Members: read") {
		t.Errorf("error %q does not name the missing permission", err)
	}
	if strings.Contains(err.Error(), "not a member") {
		t.Errorf("error %q reads as a membership answer, which it is not: %q", err, err)
	}

	// A permission the operator can grant in the next minute is not an answer
	// worth remembering.
	if err := app.Authorize(context.Background(), orgInst, 7, "vanpelt"); err == nil {
		t.Fatal("the second call was allowed")
	}
	if n := s.count("/memberships/"); n != 2 {
		t.Errorf("checked membership %d times, want 2 — a 403 must not be cached", n)
	}
}

func TestAuthorizeOrgMembershipAnswers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		mention string
	}{
		{"not a member", 404, `{"message":"Not Found"}`, "is not a member of wandb"},
		{"invited but not accepted", 200, `{"state":"pending"}`, "has not accepted"},
		{"some other state", 200, `{"state":"billing_manager"}`, "is not a member of wandb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			s.json("POST /app/installations/99/access_tokens", 200, tokenBody)
			s.json("GET /orgs/wandb/memberships/vanpelt", tc.status, tc.body)
			app := newApp(t, s, nil)

			err := app.Authorize(context.Background(), orgInst, 7, "vanpelt")
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not mention %q", err, tc.mention)
			}

			// A negative answer IS an answer and is cached like one, or a user
			// who is not in the org turns a retry loop into a request loop.
			if err := app.Authorize(context.Background(), orgInst, 7, "vanpelt"); err == nil {
				t.Fatal("the second call was allowed")
			}
			if n := s.count("/memberships/"); n != 1 {
				t.Errorf("checked membership %d times, want 1", n)
			}
		})
	}
}

// github being down is not a membership answer, and must not be reported as
// one. It is also the class a caller may retry.
func TestAuthorizeOrgUpstreamFailure(t *testing.T) {
	s := newStub(t)
	s.json("POST /app/installations/99/access_tokens", 200, tokenBody)
	s.json("GET /orgs/wandb/memberships/vanpelt", 500, `{"message":"Server Error"}`)
	app := newApp(t, s, nil)

	err := app.Authorize(context.Background(), orgInst, 7, "vanpelt")
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if strings.Contains(err.Error(), "not a member") {
		t.Errorf("an outage was reported as a membership decision: %q", err)
	}
}

// A name that is not one github could have issued never becomes a URL segment,
// even though github is where both of these came from.
func TestAuthorizeOrgRefusesUnusableNames(t *testing.T) {
	s := newStub(t)
	app := newApp(t, s, nil)

	if err := app.Authorize(context.Background(), orgInst, 7, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("an empty login was accepted: %v", err)
	}
	if err := app.Authorize(context.Background(), orgInst, 7, "van pelt/../admin"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a login with a path in it was accepted: %v", err)
	}
	bad := Installation{ID: 5, AccountID: 800, AccountLogin: "wandb/evil", AccountType: "Organization"}
	if err := app.Authorize(context.Background(), bad, 7, "vanpelt"); !errors.Is(err, ErrForbidden) {
		t.Errorf("an org name with a path in it was accepted: %v", err)
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("made %d requests with names that never should have left this host", n)
	}
}

// Anything that is not a User or an Organization has no rule written for it, so
// it gets the refusal rather than the nearest rule.
func TestAuthorizeRefusesUnknownAccountTypes(t *testing.T) {
	s := newStub(t)
	app := newApp(t, s, nil)

	for _, kind := range []string{"", "Enterprise", "user", "ORGANIZATION"} {
		inst := Installation{ID: 3, AccountID: 7, AccountLogin: "wandb", AccountType: kind}
		if err := app.Authorize(context.Background(), inst, 7, "vanpelt"); !errors.Is(err, ErrForbidden) {
			t.Errorf("account type %q was allowed: %v", kind, err)
		}
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("made %d requests deciding something local", n)
	}
}
