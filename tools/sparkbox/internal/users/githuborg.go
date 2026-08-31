package users

// Reading an organization's membership, so a platform can decide that everyone
// in one GitHub org may have an account here.
//
// This needs a credential, and that is not an accident of the API design — it
// is the whole reason the operator-driven shape below exists. GitHub publishes
// an org's PUBLIC members without one, but public membership is opt-in and
// almost nobody opts in: on the org this was written for, neither the operator
// nor the colleague being onboarded was a public member, so the free check
// would have said "not a member" about two people who are. Any design that
// gates on org membership therefore has to authenticate, and the only question
// left is whose credential it uses.
//
// It uses the OPERATOR'S, passed in per call and never stored. The alternative
// — a long-lived read:org token in the gateway's configuration — buys live
// membership checks at signup time and costs a standing GitHub credential on an
// internet-facing host, which is a bad trade for a platform whose whole
// identity story is "no secret on this side". The operator already holds a
// token (`gh auth token` prints one with read:org in its default scope set);
// spending it for the length of one command and dropping it is strictly less
// exposure than keeping one.
//
// The other half of the trade is honest: this is a pull, so it is a snapshot.
// Somebody who leaves the org keeps their account until the next sync says
// otherwise. That is a deprovisioning question, and deprovisioning was already
// manual here — see users.StatusActive, which nothing but an operator sets.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// githubOrgMembersURL and githubTeamMembersURL are vars so tests can point them
// at an httptest server without touching github.com, matching githubKeysURL's
// precedent next door.
var (
	githubOrgMembersURL  = "https://api.github.com/orgs/%s/members"
	githubTeamMembersURL = "https://api.github.com/orgs/%s/teams/%s/members"
)

// ValidGitHubLogin reports whether login could be a GitHub account name. It is
// the same grammar orgs and teams are checked against — GitHub draws them from
// one namespace — and it exists as its own name so a caller that means "a
// person" does not have to say ValidGitHubOrg and leave a reader wondering
// whether that was a mistake.
func ValidGitHubLogin(login string) bool { return githubLoginOK(login) }

// ValidGitHubTeam reports whether slug could be a team slug. Teams are slugged
// by GitHub itself into the lower-case dashed form, which is a subset of what a
// login may be, so the login grammar is a safe superset to check against.
func ValidGitHubTeam(slug string) bool { return githubLoginOK(slug) }

// orgMemberPageSize is GitHub's maximum. Fewer, larger pages is the difference
// between four requests and forty for an org of a few hundred people.
const orgMemberPageSize = 100

// maxOrgPages bounds the pagination loop. An org large enough to hit this is
// not one anybody should be bulk-provisioning onto a single-node platform, and
// an unbounded loop against a paginated API is how a malformed Link header
// turns into a spin.
const maxOrgPages = 50

// ValidGitHubOrg reports whether org is a name GitHub could have issued. Same
// grammar as a login — GitHub draws both from one namespace, which is why an
// org and a user cannot share a name.
func ValidGitHubOrg(org string) bool { return githubLoginOK(org) }

// ListOrgMembers returns the login of every member of org that token can see.
//
// Membership here means what GitHub means by it: the token's owner must be a
// member of the org for private members to appear, and the token needs the
// `read:org` scope. A token that has neither still succeeds — it just reports
// the public members, which is usually none of them. That silence is the
// failure mode worth knowing about, so a caller that gets an empty list from a
// populated org should say so rather than reporting a clean run.
func ListOrgMembers(ctx context.Context, org, token string) ([]string, error) {
	return ListMembers(ctx, org, "", token)
}

// ListMembers returns the members of org, or of one team within it when team is
// non-empty.
//
// A team is the useful unit more often than an org is. A company org runs to
// hundreds of people, most of whom will never open a sandbox, and provisioning
// all of them fills the roster with accounts nobody uses. A team is the group
// that actually wants the thing.
func ListMembers(ctx context.Context, org, team, token string) ([]string, error) {
	if !ValidGitHubOrg(org) {
		return nil, fmt.Errorf("invalid github org %q", org)
	}
	if team != "" && !ValidGitHubTeam(team) {
		return nil, fmt.Errorf("invalid github team %q", team)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("a GitHub token with read:org is required to list %q's members", org)
	}
	var out []string
	seen := map[string]bool{}
	for page := 1; page <= maxOrgPages; page++ {
		logins, err := orgMemberPage(ctx, org, team, token, page)
		if err != nil {
			return nil, err
		}
		for _, l := range logins {
			// GitHub has handed us a login it invented, but it still goes
			// through the same shape check as one a person typed: it becomes a
			// URL we fetch and a handle we may claim, and "GitHub would never"
			// is not a property this process can check. Duplicates across page
			// boundaries are possible when the roster changes mid-walk.
			if githubLoginOK(l) && !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
		if len(logins) < orgMemberPageSize {
			return out, nil
		}
	}
	return out, fmt.Errorf("%q has more members than this command will walk (%d pages)", org, maxOrgPages)
}

// orgMemberPage fetches one page of the member list, of the org or of one team.
func orgMemberPage(ctx context.Context, org, team, token string, page int) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	endpoint := fmt.Sprintf(githubOrgMembersURL, url.PathEscape(org))
	if team != "" {
		endpoint = fmt.Sprintf(githubTeamMembersURL, url.PathEscape(org), url.PathEscape(team))
	}
	endpoint += fmt.Sprintf("?per_page=%d&page=%d", orgMemberPageSize, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("github rejected the token (%s) — it may be expired", resp.Status)
	case http.StatusForbidden:
		// Also what an org with SAML SSO enforcement answers for a token that
		// has not been authorized for the org, which is a different fix from a
		// missing scope and worth naming.
		return nil, fmt.Errorf("github refused the token for %q (%s) — it needs the read:org scope, "+
			"and if the org enforces SAML the token must be authorized for it", org, resp.Status)
	case http.StatusNotFound:
		if team != "" {
			return nil, fmt.Errorf("github has no team %q in %q visible to this token", team, org)
		}
		return nil, fmt.Errorf("github has no org %q visible to this token", org)
	default:
		return nil, fmt.Errorf("github returned %s listing %q's members", resp.Status, org)
	}
	var body []struct {
		Login string `json:"login"`
	}
	// 100 logins is a few KiB; the cap is the usual guard against an upstream
	// that decides to stream forever, not a real expectation.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(body))
	for _, m := range body {
		out = append(out, m.Login)
	}
	return out, nil
}

// HandleForGitHubLogin derives a sparkbox handle from a GitHub login, or "" if
// no valid handle can be made from it.
//
// The two namespaces nearly coincide and the gaps are all one-directional:
// logins may carry capitals (handles are lower-case only) and may run to 39
// characters (handles stop at 32). Lower-casing is safe because GitHub logins
// are case-insensitive and unique case-insensitively, so it cannot collide two
// distinct accounts. Length is not something this can paper over — a truncated
// handle would be a different person's name — so an over-long login is refused
// and the operator names a handle instead.
func HandleForGitHubLogin(login string) string {
	if !githubLoginOK(login) {
		return ""
	}
	h := strings.ToLower(login)
	if !ValidHandle(h) {
		return ""
	}
	return h
}
