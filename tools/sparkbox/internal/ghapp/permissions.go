package ghapp

import (
	"maps"
	"slices"
)

// The repository permissions every token this platform mints asks for.
//
// This list lived as three identical map literals in internal/metadata and
// internal/userconsole, and they had already drifted: two of them hardcoded
// `write` for the consent screen while the third followed the attachment. A
// consent set that is wider than the mint set is not cosmetic — the scoped
// token is re-derived from the stored grant on every refresh, so the two
// disagreeing is how a user grant quietly turns back into a bot token an hour
// after it was authorized. One list, two tiers, no copies.
//
// Both tiers are still passed through Installation.Narrow before they reach
// GitHub. Asking for a permission the installation lacks is refused OUTRIGHT
// with a 422 rather than trimmed, so an App that predates any line below must
// keep working unchanged — which is the same reason it is safe to add to these
// lists without a coordinated redeploy of every installation.

// coreScope is what the credential has always carried: the permissions a clone,
// a fetch, a push, `gh pr` and `gh issue` need. Its level follows the
// attachment — a read attachment gets read, a write attachment gets write.
var coreScope = []string{"contents", "pull_requests", "issues"}

// readScope is what an agent standing in the checkout needs to answer the
// questions it is actually asked: "what is vulnerable in here", "why is CI
// red", "what shipped". Without it `gh api /repos/{o}/{r}/dependabot/alerts`
// 403s inside a sandbox whose token can push to that very repository, which is
// the same strange half-grant that put pull_requests on the list.
//
// Every entry is pinned to read at EVERY attachment level, deliberately. A
// write attachment says "this agent may push code"; it does not say "this agent
// may dismiss a security alert or cancel a production deploy". Those are
// different powers with different blast radii, and the token is handed to a
// model, so the tier that can only look is the tier that stays.
var readScope = map[string]string{
	"vulnerability_alerts": PermRead, // Dependabot alerts
	"security_events":      PermRead, // code scanning and secret scanning alerts
	"actions":              PermRead, // workflow runs, jobs and their logs
	"checks":               PermRead, // check runs and suites on a commit
	"statuses":             PermRead, // commit statuses
	"deployments":          PermRead, // deployments and deployment statuses
}

// MintPermissions is the full set, with the core tier at perm. Pass PermRead
// for a read attachment and PermWrite for a write one; nothing here ever asks
// for admin.
func MintPermissions(perm string) map[string]string {
	out := make(map[string]string, len(coreScope)+len(readScope))
	for _, name := range coreScope {
		out[name] = perm
	}
	maps.Copy(out, readScope)
	return out
}

// CoreMintPermissions is MintPermissions without the read tier: the set this
// platform minted before that tier existed.
//
// It exists for one job, in ghuser: a user authorization that was consented to
// under the old set cannot be re-scoped to the new one, because a GitHub App
// user grant carries the permissions the user actually approved and re-scoping
// beyond them fails. Retrying with this set turns that failure from "silently
// fall back to the bot, attribution lost" into "keep the token you already
// had, and tell the owner to re-authorize" — which is the difference between a
// widening that is safe to deploy and one that logs everybody out.
func CoreMintPermissions(perm string) map[string]string {
	out := make(map[string]string, len(coreScope))
	for _, name := range coreScope {
		out[name] = perm
	}
	return out
}

// IsCoreOnly reports whether want asks for nothing beyond the core tier, so a
// caller can skip a retry that would send the identical request twice.
func IsCoreOnly(want map[string]string) bool {
	for name := range want {
		if !slices.Contains(coreScope, name) {
			return false
		}
	}
	return true
}

// Covers reports whether have grants at least everything want asks for, at at
// least the level want asks for. A missing name is not covered; an "admin"
// grant covers a "write" request.
//
// The console asks this of a stored user grant to decide whether the row needs
// a re-authorize nudge. It is deliberately one-directional: have may be wider
// than want — a grant approved for a write attachment that has since been
// downgraded to read still covers it — and only a gap in the direction that
// costs the guest a capability is worth telling anybody about.
func Covers(have, want map[string]string) bool {
	for name, level := range want {
		held, ok := have[name]
		if !ok || permRank(held) < permRank(level) {
			return false
		}
	}
	return true
}

// Missing returns the names in want this installation cannot be asked for,
// sorted. It is Narrow's complement, and it exists because Narrow drops what it
// cannot grant SILENTLY: a permission that was never granted on github.com and
// a permission whose name is a typo produce the identical working-but-narrower
// token, and the only way to tell them apart is to look at what the
// installation actually reported holding.
func Missing(have, want map[string]string) []string {
	// An installation that reported NO permissions is one this code did not
	// learn about, not one that holds nothing: `metadata` is mandatory on every
	// GitHub App and always comes back, so an empty map is an absent answer.
	// Listing the entire request as missing would be a wall of text that is
	// wrong more often than it is right — and Narrow already refuses to widen
	// on the same evidence, so nothing is minted on the strength of a guess
	// either way.
	if len(have) == 0 {
		return nil
	}
	var out []string
	for name := range want {
		if _, ok := have[name]; !ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func permRank(level string) int {
	switch level {
	case PermRead:
		return 1
	case PermWrite:
		return 2
	case "admin":
		return 3
	}
	return 0
}
