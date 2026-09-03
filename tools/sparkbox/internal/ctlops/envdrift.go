package ctlops

// Setup-script drift: whether an environment's recorded script still agrees
// with the .sparkbox/setup.sh in the repository it came from.
//
// WHY THIS IS NOT SIMPLY "DO THEY DIFFER". A row and a repository can disagree
// for two opposite reasons, and they want opposite handling:
//
//   - The repository moved ahead. Somebody committed a change to
//     .sparkbox/setup.sh and this environment has never been told. The right
//     answer is to take it, and — because a build's script comes from the row,
//     not from github — a person who does nothing will keep building the old
//     one forever without being told that is what is happening.
//   - The environment has its own version now. A repair pass rewrote the
//     script, or somebody piped one in. The row is the newer of the two and
//     taking the repository's would silently undo work.
//
// Nothing about the two scripts distinguishes these. The SEED does:
// envs.Environment.SetupSeedSHA records what the script hashed to when it was
// read out of a repository, so a row that still hashes to its seed is untouched
// and one that does not has been edited since. That is the whole mechanism, and
// it is why the seed is stamped by exactly one writer (envs.SetSeededScript).
//
// The consequence worth stating plainly: only the untouched case authorises a
// write. Anything unknown — an environment older than the seed column, a script
// somebody typed, github not answering — falls to `diverged`, which asks a
// person rather than acting.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
)

// The drift verdicts, as they reach a surface. The empty string is the fourth
// and it means "no answer" — no attached repository, none of them has the file,
// or github has not been reached yet — which every surface renders as nothing
// at all rather than as reassurance.
const (
	// ScriptDriftMatch: the row and the repository hold the same bytes.
	ScriptDriftMatch = "match"
	// ScriptDriftRepoAhead: the row is still a clean copy of what the
	// repository seeded, and the repository has since changed. The next build
	// takes the repository's version by itself (resolveSetupScript).
	ScriptDriftRepoAhead = "repo_ahead"
	// ScriptDriftDiverged: the row has been changed since it was seeded — or
	// was never seeded at all — and does not match the repository. Nothing
	// overwrites it; a person chooses.
	ScriptDriftDiverged = "diverged"
	// ScriptDriftRepoOnly: this environment has no script and an attached
	// repository has one. Not drift so much as a correction to what the
	// surfaces would otherwise say, which is that a build would ask an agent to
	// write a script — when in fact it will read this file.
	ScriptDriftRepoOnly = "repo_only"
)

// EnvScriptDriftTTL is how long a repository's setup script is believed without
// asking github again. Generous because the thing it tracks is somebody
// committing to a repository, which is a human-paced event, and because the
// cost of being ten minutes stale is a card that catches up on the next load.
const EnvScriptDriftTTL = 10 * time.Minute

// envDriftBudget is how long a LISTING waits for the answers it does not have
// cached. It is short on purpose: this is decoration on a page that must render
// either way, and a console that hung for as long as github felt like taking
// would have traded a working page for a badge.
//
// Refreshes that miss the budget are NOT cancelled — they keep running on their
// own timeout and land in the cache, so the next load has them.
const envDriftBudget = 4 * time.Second

// envDriftLookupTimeout bounds one environment's read, whoever is waiting.
const envDriftLookupTimeout = 20 * time.Second

// driftEntry is one environment's last look at github: what the repository's
// .sparkbox/setup.sh hashed to, and which repository it came out of. A zero sha
// means "asked, and no attached repository has the file", which is a real
// answer and distinct from never having asked.
type driftEntry struct {
	slug string
	sha  string
	at   time.Time
}

// repoScript is one attached repository's .sparkbox/setup.sh as it reads now.
type repoScript struct {
	Slug   string
	Script string
}

// canReadRepoScripts is the whole feature's precondition. A host with no
// GitHub App configured cannot answer any of this, and says nothing rather than
// reporting that every environment has diverged from a repository it cannot
// see.
func (o *Ops) canReadRepoScripts() bool {
	return o.envs != nil && o.repos != nil && o.ghApp != nil && o.repoFiles != nil
}

// readRepoSetupScript walks the repositories attached to an environment, in
// slug order, and returns the first usable .sparkbox/setup.sh.
//
// It is a PURE READ — it was lifted out of seedSetupScript precisely so that
// asking "what does the repository say" no longer implies writing the answer to
// the row. seedSetupScript now reads with this and then decides to store;
// drift detection reads with this and stores nothing.
//
// The error return is reserved for github being unreachable, because that is
// the one outcome a caller must not read as "there is no script": seeding an
// environment on that reading would start an agent build for a project that has
// a perfectly good committed script.
func (o *Ops) readRepoSetupScript(ctx context.Context, op, owner, env string) (repoScript, error) {
	if !o.canReadRepoScripts() {
		return repoScript{}, nil
	}
	list, err := o.repos.ListRepos(owner)
	if err != nil {
		o.log.Warn("could not read repo attachments while reading a setup script",
			"user", owner, "env", env, "err", err)
		return repoScript{}, nil
	}
	mine := make([]repos.Repo, 0, len(list))
	for _, r := range list {
		if slicesContainsFold(r.Tags, env) {
			mine = append(mine, r)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Slug < mine[j].Slug })

	var upstream error
	for _, r := range mine {
		owner, repoName, ok := repos.SplitSlug(r.Slug)
		if !ok {
			continue
		}
		inst, err := o.ghApp.InstallationFor(ctx, owner, repoName)
		if err != nil {
			if errors.Is(err, ghapp.ErrUpstream) {
				upstream = err
			}
			continue
		}
		// r.Ref may be empty, which ReadFile takes to mean the repository's
		// default branch — the same thing the checkout the script will run in
		// was made from.
		b, err := o.repoFiles.ReadFile(ctx, inst, owner, repoName, r.Ref, SetupScriptPath)
		switch {
		case errors.Is(err, ghapp.ErrNoSuchFile):
			continue
		case errors.Is(err, ghapp.ErrUpstream):
			upstream = err
			continue
		case err != nil:
			o.log.Warn("could not read a setup script from an attached repository",
				"user", owner, "env", env, "slug", r.Slug, "err", err)
			continue
		}
		script := string(b)
		if strings.TrimSpace(script) == "" {
			// A file that is there and says nothing is not a script. Treating
			// it as one would start a build that captures an untouched base
			// image and calls it an environment.
			continue
		}
		if len(script) > MaxSetupScript {
			// Refused, never truncated: half a script is what every future fork
			// of this environment would run.
			o.log.Warn("an attached repository's setup script is too large to use",
				"user", owner, "env", env, "slug", r.Slug, "bytes", len(script), "max", MaxSetupScript)
			continue
		}
		return repoScript{Slug: r.Slug, Script: script}, nil
	}
	if upstream != nil {
		return repoScript{}, &Error{
			Kind: KindUpstream, Op: op, Code: "github_unreachable",
			Msg: "github.com did not answer, so there is no way to tell whether " + env +
				" has a setup script in one of its repositories. Try again in a moment.",
			Verbatim: true, Err: upstream,
		}
	}
	return repoScript{}, nil
}

// classifyDrift is the verdict, and it is a pure function of three hashes so
// that the rule can be read in one place and tested without a store.
//
// The content comparison comes FIRST, ahead of the seed, and that ordering is
// what makes the platform's own advice work. An agent build's deliverable is a
// script the owner is told to commit back; when they do, the row and the
// repository hold identical bytes while the seed is still empty, because the
// row was never seeded from anywhere. Consulting the seed first would report
// that as `diverged` — telling somebody who just did exactly the right thing
// that their environment disagrees with their repository.
func classifyDrift(stored, seed, repoSHA string) string {
	if repoSHA == "" {
		// Nothing to compare against: no attached repository has the file.
		return ""
	}
	// TrimSpace decides EMPTINESS only, and never what gets hashed. Hashing a
	// trimmed copy would compare something neither side stores: every script
	// ends in a newline, so it made a row and the repository's byte-identical
	// file disagree with each other — reported as `diverged`, on every
	// environment, which is the loudest possible way to be wrong.
	if strings.TrimSpace(stored) == "" {
		return ScriptDriftRepoOnly
	}
	if envs.ScriptSHA(stored) == repoSHA {
		return ScriptDriftMatch
	}
	if seed != "" && envs.ScriptSHA(stored) == seed {
		return ScriptDriftRepoAhead
	}
	return ScriptDriftDiverged
}

// cachedDrift returns this environment's last look at github and whether it is
// still inside the TTL.
func (o *Ops) cachedDrift(owner, env string) (driftEntry, bool) {
	o.scriptDriftMu.Lock()
	defer o.scriptDriftMu.Unlock()
	entry, ok := o.scriptDrift[envBuildKey(owner, env)]
	if !ok {
		return driftEntry{}, false
	}
	return entry, o.now().Sub(entry.at) < EnvScriptDriftTTL
}

// refreshScriptDrift asks github what this environment's repository says, and
// records it. Collapsed by singleflight on (owner, environment) for the reason
// every other use of it in this package is: a console that renders a dozen
// cards and a person running `env show` at the same moment are one question.
//
// github being unreachable KEEPS THE PREVIOUS ANSWER rather than recording an
// empty one. An empty sha means "no attached repository has this file", and
// letting a network blip say that would turn a badge that reads "your
// repository has moved ahead" into nothing at all, which is the wrong direction
// to fail in: the whole point is to stop a difference going unnoticed.
func (o *Ops) refreshScriptDrift(owner, env string) {
	key := envBuildKey(owner, env)
	o.scriptDriftSF.Do(key, func() (any, error) { //nolint:errcheck
		ctx, cancel := context.WithTimeout(context.Background(), envDriftLookupTimeout)
		defer cancel()
		found, err := o.readRepoSetupScript(ctx, "env.drift", owner, env)
		if err != nil {
			o.log.Debug("could not check an environment's setup script against its repository",
				"user", owner, "env", env, "err", err)
			return nil, nil
		}
		o.scriptDriftMu.Lock()
		defer o.scriptDriftMu.Unlock()
		if o.scriptDrift == nil {
			o.scriptDrift = make(map[string]driftEntry)
		}
		o.scriptDrift[key] = driftEntry{
			slug: found.Slug, sha: envs.ScriptSHA(found.Script), at: o.now(),
		}.normalized()
		return nil, nil
	})
}

// normalized keeps a slug out of an entry that has no script to attribute to
// it, so a surface can rely on "sha set implies slug set".
func (d driftEntry) normalized() driftEntry {
	if d.sha == "" {
		d.slug = ""
	}
	return d
}

// annotateScriptDrift decorates a listing in place. rows and out must be the
// same environments in the same order — they are, because both come out of the
// one pass in ListEnvironments.
func (o *Ops) annotateScriptDrift(rows []envs.Environment, out []EnvironmentInfo) {
	if !o.canReadRepoScripts() || len(rows) == 0 || len(rows) != len(out) {
		return
	}
	var wg sync.WaitGroup
	for _, e := range rows {
		if _, fresh := o.cachedDrift(e.Owner, e.Name); fresh {
			continue
		}
		wg.Add(1)
		go func(owner, name string) {
			defer wg.Done()
			o.refreshScriptDrift(owner, name)
		}(e.Owner, e.Name)
	}
	waitBounded(&wg, envDriftBudget)

	for i := range out {
		entry, ok := o.cachedDrift(rows[i].Owner, rows[i].Name)
		if !ok {
			continue
		}
		out[i].ScriptDrift = classifyDrift(rows[i].SetupScript, rows[i].SetupSeedSHA, entry.sha)
		if out[i].ScriptDrift != "" {
			out[i].ScriptDriftRepo = entry.slug
		}
	}
}

// forgetDrift drops a cached verdict because the row it was about has just
// changed underneath it. Called wherever this package writes a setup script:
// the alternative is a card that goes on saying "your repository has moved
// ahead" for ten minutes after the build that took the repository's version.
func (o *Ops) forgetDrift(owner, env string) {
	o.scriptDriftMu.Lock()
	defer o.scriptDriftMu.Unlock()
	delete(o.scriptDrift, envBuildKey(owner, env))
}

// waitBounded waits for wg, or for d to pass, whichever comes first. The
// goroutines it gives up on are not cancelled and not leaked: each one is
// bounded by envDriftLookupTimeout and its only effect is a map write.
func waitBounded(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}
