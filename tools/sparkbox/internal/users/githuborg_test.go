package users

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveOrg stands in for api.github.com's member listing, recording what it was
// asked and answering with pages of the given logins.
func serveOrg(t *testing.T, logins []string, gotAuth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		page := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		start := (page - 1) * orgMemberPageSize
		if start > len(logins) {
			start = len(logins)
		}
		end := start + orgMemberPageSize
		if end > len(logins) {
			end = len(logins)
		}
		var b strings.Builder
		b.WriteString("[")
		for i, l := range logins[start:end] {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"login":%q,"id":%d}`, l, i)
		}
		b.WriteString("]")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, b.String())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withOrgURL points the package at a test server for the duration of one test.
func withOrgURL(t *testing.T, base string) {
	t.Helper()
	prev := githubOrgMembersURL
	githubOrgMembersURL = base + "/orgs/%s/members"
	t.Cleanup(func() { githubOrgMembersURL = prev })
}

// TestListOrgMembersPaginates: an org larger than one page must be walked to
// the end. Stopping at the first page would silently onboard the alphabetical
// first hundred people and quietly omit everybody else, which reads as success.
func TestListOrgMembersPaginates(t *testing.T) {
	want := make([]string, 0, 250)
	for i := 0; i < 250; i++ {
		want = append(want, fmt.Sprintf("user%03d", i))
	}
	srv := serveOrg(t, want, nil)
	withOrgURL(t, srv.URL)

	got, err := ListOrgMembers(context.Background(), "wandb", "tok")
	if err != nil {
		t.Fatalf("ListOrgMembers: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d members, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("member %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The token has to actually reach GitHub as a bearer credential: without it the
// API answers with public members only, which for most orgs is nobody — and a
// short, clean, wrong answer is the worst possible failure here.
func TestListOrgMembersSendsTheToken(t *testing.T) {
	var auth string
	srv := serveOrg(t, []string{"octocat"}, &auth)
	withOrgURL(t, srv.URL)

	if _, err := ListOrgMembers(context.Background(), "wandb", "ghp_secret"); err != nil {
		t.Fatalf("ListOrgMembers: %v", err)
	}
	if auth != "Bearer ghp_secret" {
		t.Errorf("Authorization = %q, want a bearer token", auth)
	}
}

func TestListOrgMembersRefusesAnEmptyToken(t *testing.T) {
	// No server: this must fail before any request is made.
	if _, err := ListOrgMembers(context.Background(), "wandb", "  "); err == nil {
		t.Fatal("an empty token was accepted")
	}
}

// GitHub's three interesting refusals get three different sentences, because
// they need three different fixes: a bad token, a token missing read:org (or
// unauthorized for a SAML org), and a name that is not an org here.
func TestListOrgMembersExplainsRefusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "expired"},
		{http.StatusForbidden, "read:org"},
		{http.StatusNotFound, "no org"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		withOrgURL(t, srv.URL)
		_, err := ListOrgMembers(context.Background(), "wandb", "tok")
		if err == nil {
			t.Errorf("status %d was not an error", tc.status)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d said %q, want it to mention %q", tc.status, err, tc.want)
		}
		srv.Close()
	}
}

// A login GitHub itself returned still goes through the shape check: it becomes
// a URL this package fetches and a handle it may claim.
func TestListOrgMembersDropsUnusableLogins(t *testing.T) {
	srv := serveOrg(t, []string{"good", "bad login", "-nope", "alsogood"}, nil)
	withOrgURL(t, srv.URL)

	got, err := ListOrgMembers(context.Background(), "wandb", "tok")
	if err != nil {
		t.Fatalf("ListOrgMembers: %v", err)
	}
	if len(got) != 2 || got[0] != "good" || got[1] != "alsogood" {
		t.Fatalf("got %v, want the two well-formed logins", got)
	}
}

func TestHandleForGitHubLogin(t *testing.T) {
	for _, tc := range []struct {
		login, want, why string
	}{
		{"adrnswanberg", "adrnswanberg", "the ordinary case"},
		{"AdrnSwanberg", "adrnswanberg", "GitHub logins are case-insensitive, handles are lower-case"},
		{"a-b-1", "a-b-1", "dashes and digits are fine in both namespaces"},
		{strings.Repeat("a", 33), "", "33 chars exceeds a handle; truncating would name a different person"},
		{"console", "", "a reserved platform name is not claimable"},
		{"x", "", "one character is below the handle minimum"},
		{"has space", "", "not a login GitHub could have issued"},
		{"", "", "empty"},
	} {
		if got := HandleForGitHubLogin(tc.login); got != tc.want {
			t.Errorf("HandleForGitHubLogin(%q) = %q, want %q (%s)", tc.login, got, tc.want, tc.why)
		}
	}
}
