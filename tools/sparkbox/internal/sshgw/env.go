package sshgw

// The `env` verbs on the ctl channel.
//
// An environment is the name somebody puts on a way of working — this
// checkout, that token, these variables, that egress policy, the disk they were
// built on — and its NAME IS A TAG. So none of these verbs invent a
// relationship: `env create web --secret GH_TOKEN` puts the tag `web` on a
// secret that was already tag-selected, and every sandbox carrying `web`
// already got everything carrying `web`. What did not exist before was a word
// for the whole and anywhere to say what the word meant.
//
// They live on this channel rather than only in the console for repo.go's
// reason: this is where a sandbox is created. `ssh new@gw -- --env web` is one
// gesture meaning all of the above, and the moment somebody discovers their
// environment is missing a secret is the moment they type it.
//
// Everything here is parse → call ctlops → format, with one exception that
// earns itself: `env script --set` reads the script from STDIN and never from
// argv. That is `secret set`'s discipline (see secret.go's header) applied to a
// different kind of payload — a setup script is a file, argv is not where files
// come from, and a page of shell quoted onto a command line is a page of shell
// mangled by two shells. What is NOT borrowed from secret.go is
// cleanSecretValue: a script is not a credential, so it is stored byte for byte
// with no banner-stripping and no shape-matching.

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	gssh "github.com/gliderlabs/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
)

// maxScriptRead caps what this channel will read for one setup script. It is
// ctlops' own limit rather than a second opinion — the same rule
// maxTagsPerSandbox follows — and the LimitReader below asks for one byte more,
// so an over-long script is reported as too long instead of arriving silently
// truncated to a script that runs and does the wrong half of the job.
const maxScriptRead = ctlops.MaxSetupScript

const envUsage = "usage: ssh ctl@<gateway> env ls\r\n" +
	"       ssh ctl@<gateway> env show <name>\r\n" +
	"       ssh ctl@<gateway> env create <name> [<flag>…]\r\n" +
	"       ssh ctl@<gateway> env set <name> [<flag>…] [--unset <K>]…\r\n" +
	"       ssh ctl@<gateway> env rm <name>\r\n" +
	"       ssh ctl@<gateway> env script <name>\r\n" +
	"       cat setup.sh | ssh ctl@<gateway> env script <name> --set\r\n" +
	"       ssh ctl@<gateway> env script <name> --from-repo\r\n" +
	"       ssh ctl@<gateway> env build <name>\r\n" +
	"       ssh ctl@<gateway> env rebuild <name>\r\n" +
	"       ssh ctl@<gateway> env capture <name>\r\n" +
	"       ssh ctl@<gateway> env log <name>\r\n" +
	"\r\n" +
	"  flags: --repo <owner>/<name>  --secret <NAME>  --rule <name>  --var K=V\r\n" +
	"         --description <text>          every one of them may be repeated\r\n" +
	"         --open-egress                 on create: skip the default egress rules\r\n" +
	"         --adopt                       on create: take on a tag already in use\r\n" +
	"\r\n" +
	"an environment names a way of working: a checkout, the secrets it needs, the\r\n" +
	"egress it is allowed, some plain variables, and the disk they were built on.\r\n" +
	"its NAME IS A TAG, so it composes what that tag already selects — every\r\n" +
	"sandbox of yours carrying `web` gets everything `web` is on.\r\n" +
	"\r\n" +
	"attaching ADDS. a secret can belong to three environments at once, so `env rm`\r\n" +
	"deletes the environment, the variables it created, and the egress rule-set\r\n" +
	"`env create` made for it — nothing else: the secrets, repos and rule-sets you\r\n" +
	"wrote carry on and are listed on the way out, and so does anything that was\r\n" +
	"already on the tag when the environment adopted it.\r\n" +
	"\r\n" +
	"  ssh ctl@<gateway> env create web --repo wandb/hivemind --secret GITHUB_TOKEN\r\n" +
	"  ssh ctl@<gateway> env set web --var NODE_ENV=test --description \"the web box\"\r\n" +
	"  ssh new@<gateway> -- web\r\n" +
	"\r\n" +
	"`create` and `set` are one verb: naming an environment that exists adds to it.\r\n" +
	"--secret and --rule name things that must already exist — save a secret with\r\n" +
	"`secret set`, and write an egress rule-set in the user console.\r\n" +
	"\r\n" +
	"`ssh new@<gateway> -- --env web` boots a sandbox from the disk the environment\r\n" +
	"was built on, so it needs one that has been built. until then, naming it as a\r\n" +
	"plain tag — `ssh new@<gateway> -- web` — gives you its secrets, checkouts and\r\n" +
	"egress on the stock image: the tag is what composes those, and it works from\r\n" +
	"the moment the environment exists.\r\n" +
	"\r\n" +
	"the setup script is read from stdin, never from the command line, because it\r\n" +
	"is a file rather than an argument:\r\n" +
	"\r\n" +
	"  cat .sparkbox/setup.sh | ssh ctl@<gateway> env script web --set\r\n" +
	"\r\n" +
	"`env build` is what turns all of that into a disk. it boots one sandbox named\r\n" +
	"<name>-build from the stock image, runs the setup script in the checkout, and\r\n" +
	"— when the script succeeds — captures that sandbox as the image every later\r\n" +
	"`ssh new@<gateway> -- --env <name>` boots from. it RETURNS AS SOON AS THE\r\n" +
	"BUILD STARTS: the work takes minutes and keeps going after you disconnect, so\r\n" +
	"read the outcome with `env show <name>` rather than by waiting here.\r\n" +
	"\r\n" +
	"the script comes from the environment (`env script <name> --set`), or, when\r\n" +
	"there is none, from .sparkbox/setup.sh in one of its repositories — which is\r\n" +
	"then stored, so the next build is the same build. `sparkbox docs\r\n" +
	"dev-environment` describes what belongs in that file.\r\n" +
	"\r\n" +
	"a stored script that is still a CLEAN COPY of the one its repository gave it\r\n" +
	"is refreshed on every build, so committing a change to .sparkbox/setup.sh and\r\n" +
	"running `env rebuild` picks it up. once it has been changed here — by the\r\n" +
	"repair pass, or by `--set` — nothing overwrites it, `env show` says the two\r\n" +
	"disagree, and `env script <name> --from-repo` is how you choose the\r\n" +
	"repository's version.\r\n" +
	"\r\n" +
	"with NO script anywhere, `env build` runs an AGENT in the builder instead: it\r\n" +
	"reads `sparkbox docs dev-environment`, gets the project running, and writes\r\n" +
	".sparkbox/setup.sh — which is the deliverable, because the next build of the\r\n" +
	"same environment then runs that script instead of running an agent. commit the\r\n" +
	"file it leaves in your checkout and every later build is deterministic. this\r\n" +
	"needs a CLAUDE_CODE_OAUTH_TOKEN the builder will carry, and says so up front\r\n" +
	"rather than after booting a VM to find out.\r\n" +
	"\r\n" +
	"`env rebuild` is `env build` on an environment that already has a disk: always\r\n" +
	"from the stock image and the current script, never from its own last snapshot,\r\n" +
	"so an environment never accumulates. the old image stays bound until the new\r\n" +
	"one is captured, so a rebuild that fails costs you nothing but the time.\r\n" +
	"\r\n" +
	"each build leaves its capture as `<name>-<YYMMDD-HHMM>` in `snapshot ls`, and\r\n" +
	"the three newest are kept — the one bound now and two to roll back to with\r\n" +
	"`snapshot bind`. older ones are deleted, unless you bound one to a tag of its\r\n" +
	"own, which is how you keep a build forever.\r\n" +
	"\r\n" +
	"a new environment gets a default egress rule-set named after it, so its\r\n" +
	"sandboxes reach the package registries, github and the model API and not the\r\n" +
	"rest of the internet. widen it in the console's Network panel, or pass\r\n" +
	"--open-egress on create to have no rules at all.\r\n" +
	"\r\n" +
	"a SCRIPT build that fails leaves its builder sandbox PAUSED, with the\r\n" +
	"half-built disk and the log in it. ssh in, fix what was missing by hand, and\r\n" +
	"keep the result:\r\n" +
	"\r\n" +
	"  ssh ctl@<gateway> env build web\r\n" +
	"  ssh web-build@<gateway>              # only if it failed\r\n" +
	"  ssh ctl@<gateway> env capture web    # snapshot that box, bind it, done\r\n" +
	"\r\n" +
	"an AGENT build that overruns its budget has its builder DESTROYED instead of\r\n" +
	"paused: it holds an unattended agent with your credentials, and by definition\r\n" +
	"has not written the script that was the point.\r\n"

// controlEnv serves `ctl env …`.
func (g *Gateway) controlEnv(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	// Before the usage line, exactly as `repo` does it: a host with no
	// environment store cannot compose anything however the command was typed,
	// and the sentence is the one ctlops would have printed for the same state
	// — two wordings for one condition is how a user comes to believe there are
	// two conditions.
	if !g.ops.Capabilities().Environments {
		fmt.Fprint(s.Stderr(), "sparkbox: environments are not enabled on this host\r\n")
		s.Exit(1) //nolint:errcheck
		return
	}
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), envUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	switch args[0] {
	case "ls", "list":
		g.envList(s, c, log)

	case "show", "get":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), envUsage)
			s.Exit(2) //nolint:errcheck
			return
		}
		g.envShow(s, c, args[1], log)

	// One verb under two names. ctlops.PutEnvironment is create-or-add, and
	// pretending otherwise would mean refusing `env create web --secret X` the
	// second time somebody ran it — which is the command they will run when
	// they want to add a secret, because it is the one they already know.
	case "create", "set", "add":
		g.envSet(s, c, args[0], args[1:], log)

	case "rm", "delete":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), envUsage)
			s.Exit(2) //nolint:errcheck
			return
		}
		g.envRemove(s, c, args[1], log)

	case "script":
		g.envScript(s, c, args[1:], log)

	// One verb under two names, and the second one is honest about it. `build`
	// on an environment that already has a disk IS a rebuild — it boots the
	// stock image and runs the current script, never the environment's own last
	// snapshot — so there is one code path and `rebuild` is the word for the
	// case where you know that is what you are asking for. Refusing to accept
	// it would leave `env rebuild`, which this platform's own refusals now
	// recommend, reading as a typo.
	case "build", "rebuild":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), envUsage)
			s.Exit(2) //nolint:errcheck
			return
		}
		g.envBuild(s, c, args[1], log)

	case "capture":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), envUsage)
			s.Exit(2) //nolint:errcheck
			return
		}
		g.envCapture(s, c, args[1], log)

	case "log":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), envUsage)
			s.Exit(2) //nolint:errcheck
			return
		}
		g.envBuildLog(s, c, args[1], log)

	default:
		fmt.Fprintf(s.Stderr(), "unknown env command %q\r\n%s", args[0], envUsage)
		s.Exit(2) //nolint:errcheck
	}
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// envList prints one row per environment: what it is called, where its build
// got to, what it composes, and what the owner said it was for.
//
// The columns are `repo ls`' and `secret ls`' — name first, fixed-width, the
// free text last so a long description runs off the right edge of a narrow
// terminal instead of pushing the useful columns off it.
func (g *Gateway) envList(s gssh.Session, c ctlops.Caller, log *slog.Logger) {
	list, err := g.ops.ListEnvironments(c)
	if err != nil {
		failCtl(s, log, "env ls", err)
		return
	}
	for _, e := range list {
		fmt.Fprintf(s, "%-20s %-8s %-26s %s\r\n",
			truncate(e.Name, 20), e.State, truncate(envSummary(e), 26), truncate(e.Description, 22))
	}
	if len(list) == 0 {
		fmt.Fprint(s, "no environments yet — create one with:\r\n"+
			"  ssh ctl@"+g.sshHint()+" env create web --repo wandb/hivemind\r\n")
	}
	s.Exit(0) //nolint:errcheck
}

// envShow renders one environment's whole composition.
//
// The four lists are indented under their own headings rather than joined onto
// one line because this is the command somebody runs when a sandbox did not get
// something they expected, and the answer to that question is a name they have
// to read off a list — `secrets: GITHUB_TOKEN,NPM_TOKEN,…` truncated at the
// right margin is exactly the shape that hides the one they are looking for.
// A heading with nothing under it is omitted: a page of empty sections says
// less than a page of the sections that have something in them.
func (g *Gateway) envShow(s gssh.Session, c ctlops.Caller, name string, log *slog.Logger) {
	e, err := g.ops.GetEnvironment(c, name)
	if err != nil {
		failCtl(s, log, "env show", err)
		return
	}
	fmt.Fprintf(s, "%-20s %s\r\n", e.Name, e.State)
	if e.Description != "" {
		fmt.Fprintf(s, "  %-12s %s\r\n", "about", e.Description)
	}
	// The image line is printed even when there is none, because "this boots
	// the stock image" is the thing a person is surprised by, and a line that
	// only appears in the other case cannot tell them.
	image := "none — sandboxes on this tag boot the stock image"
	if e.Snapshot != "" {
		image = e.Snapshot
		if e.BuiltAt != nil {
			image += "  (built " + e.BuiltAt.Local().Format("2006-01-02 15:04") + ")"
		}
	}
	fmt.Fprintf(s, "  %-12s %s\r\n", "image", image)
	setup := "none"
	if e.HasSetup {
		setup = fmt.Sprintf("%d bytes", e.SetupBytes)
		if e.SetupFrom != "" {
			setup += ", from " + e.SetupFrom
		}
		setup += "  (read it with `env script " + e.Name + "`)"
	}
	fmt.Fprintf(s, "  %-12s %s\r\n", "setup", setup)
	// Whether that script is still the one in the repository. Printed only when
	// there is something to say — a match and an unknown both print nothing,
	// because this line exists to interrupt somebody and "your script is fine"
	// is not worth interrupting anybody for.
	if drift := envDriftLine(e); drift != "" {
		fmt.Fprintf(s, "  %-12s %s\r\n", "script", drift)
	}
	// The build. This is the half of `env show` somebody runs while something is
	// happening, so both live states say what to do next rather than only what
	// state they are in: a build in flight is watchable and a failed one is
	// RECOVERABLE — its builder is still there, paused, holding the half-built
	// disk and the log — and neither fact is discoverable from the word alone.
	//
	// The continuation lines are indented to the value column (two spaces, the
	// twelve-wide label, one space) so the commands read as belonging to the
	// row above them rather than as more rows.
	const envCont = "               " // 2 + 12 + 1: the column the values above start in
	if e.BuildBox != "" && e.State == string(envs.StateBuilding) {
		fmt.Fprintf(s, "  %-12s %s — running the setup script; this takes minutes\r\n",
			"building in", e.BuildBox)
		fmt.Fprintf(s, "%sssh %s@%s\r\n", envCont, e.BuildBox, g.sshHint())
	}
	// The agent's transcript, once the build has reported one. It is printed
	// for every state that has it rather than only for `ready`: on a FAILED
	// agent build it is the fastest account of what went wrong, and on a ready
	// one it is the only surviving record of why the setup script says what it
	// says — the box that wrote it was destroyed.
	if e.BuildSession != "" {
		fmt.Fprintf(s, "  %-12s %s\r\n", "agent", e.BuildSession)
	}
	if e.BuildError != "" {
		fmt.Fprintf(s, "  %-12s %s\r\n", "build error", e.BuildError)
	}
	// The run's own output, once a build has reported one. Printed for every
	// state that has it, for BuildSession's reason: on a failed build it is the
	// fastest account of what went wrong, and it is the only one a SCRIPT build
	// has — there is no HiveMind transcript unless an agent ran.
	if e.HasBuildLog {
		fmt.Fprintf(s, "  %-12s %d bytes  (read it with `env log %s`)\r\n", "log", e.BuildLogBytes, e.Name)
	}
	for i, denied := range e.BuildDenials {
		label := ""
		if i == 0 {
			label = "blocked DNS"
		}
		word := "queries"
		if denied.Queries == 1 {
			word = "query"
		}
		fmt.Fprintf(s, "  %-12s %s  (%d %s)\r\n", label, denied.Domain, denied.Queries, word)
	}
	if e.BuildDenialOverflow > 0 {
		fmt.Fprintf(s, "  %-12s %d more blocked queries were omitted\r\n", "", e.BuildDenialOverflow)
	}
	if e.BuildBox != "" && e.State != string(envs.StateBuilding) {
		fmt.Fprintf(s, "  %-12s %s — kept and paused, with the half-built disk in it\r\n",
			"builder", e.BuildBox)
		fmt.Fprintf(s, "%sfix it by hand:     ssh %s@%s\r\n", envCont, e.BuildBox, g.sshHint())
		fmt.Fprintf(s, "%skeep what you fix:  ssh %s@%s env capture %s\r\n",
			envCont, ControlUser, g.sshHint(), e.Name)
	}

	for _, sec := range []struct {
		head  string
		items []string
	}{
		{"repos", e.Repos},
		{"secrets", e.Secrets},
		{"rules", e.Rules},
	} {
		if len(sec.items) == 0 {
			continue
		}
		fmt.Fprintf(s, "  %s:\r\n", sec.head)
		for _, it := range sec.items {
			fmt.Fprintf(s, "    %s\r\n", it)
		}
	}
	if len(e.Vars) > 0 {
		fmt.Fprint(s, "  vars:\r\n")
		for _, v := range e.Vars {
			// The value IS printed. A var is a declaration that this string is
			// configuration rather than a credential — anything that must not
			// appear here is a secret, and `secret ls` never prints one.
			fmt.Fprintf(s, "    %s=%s\r\n", v.Name, v.Value)
		}
	}
	if len(e.Repos)+len(e.Secrets)+len(e.Rules)+len(e.Vars) == 0 {
		fmt.Fprintf(s, "\r\nnothing composed yet — add to it with:\r\n"+
			"  ssh ctl@%s env set %s --repo <owner>/<name> --secret <NAME> --var K=V\r\n",
			g.sshHint(), e.Name)
	}
	s.Exit(0) //nolint:errcheck
}

// envSummary is the listing's third column: what the environment holds, in
// words, with the empty parts left out. Zeros are omitted rather than printed
// as `0 repos` because a row is read by scanning it, and four zeros are four
// things to read past.
func envSummary(e ctlops.EnvironmentInfo) string {
	var parts []string
	add := func(n int, one, many string) {
		switch {
		case n == 1:
			parts = append(parts, "1 "+one)
		case n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	add(len(e.Repos), "repo", "repos")
	add(len(e.Secrets), "secret", "secrets")
	add(len(e.Rules), "rule", "rules")
	add(len(e.Vars), "var", "vars")
	if len(parts) == 0 {
		return "nothing composed yet"
	}
	return strings.Join(parts, " · ")
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// envSet creates an environment or adds to one, then applies any --unset.
func (g *Gateway) envSet(s gssh.Session, c ctlops.Caller, verb string, args []string, log *slog.Logger) {
	a, unset, err := parseEnvSet(args)
	if err != nil {
		fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n%s", err, envUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	info, err := g.ops.PutEnvironment(s.Context(), c, a)
	if err != nil {
		failCtl(s, log, "env "+verb, err)
		return
	}
	// The unsets run after the write, because until it lands there may be no
	// environment to unset a variable from. A failure here is reported and
	// stops the command: the environment IS written by then, which is why the
	// sentence says what was done rather than only what was not.
	for _, k := range unset {
		if err := g.ops.UnsetEnvVar(s.Context(), c, info.Name, k); err != nil {
			fmt.Fprintf(s, "%s %s\r\n", pastTense(verb), info.Name)
			failCtl(s, log, "env "+verb, err)
			return
		}
	}
	if len(unset) > 0 {
		// Re-read, so the summary counts the variables that are actually left
		// rather than the ones there were a moment ago.
		if e, err := g.ops.GetEnvironment(c, info.Name); err == nil {
			info = e
		}
	}
	fmt.Fprintf(s, "%s %s — %s  (%s)\r\n", pastTense(verb), info.Name, envSummary(info), info.State)
	if verb == "create" {
		// The one thing a fresh environment does not say for itself: it is
		// already usable as a tag, without a build and without --env. See the
		// usage text — this is the same sentence, at the moment it applies.
		fmt.Fprintf(s, "use it now with:  ssh %s@%s -- %s\r\n", NewSandboxUser, g.sshHint(), info.Name)
	}
	s.Exit(0) //nolint:errcheck
}

// pastTense renders the verb the user typed as the thing that happened, so
// `env create` and `env set` — one operation — still report themselves in the
// words of the command that was run.
func pastTense(verb string) string {
	if verb == "create" {
		return "created"
	}
	return "updated"
}

// envRemove deletes an environment and prints what outlived it.
func (g *Gateway) envRemove(s gssh.Session, c ctlops.Caller, name string, log *slog.Logger) {
	res, err := g.ops.DeleteEnvironment(s.Context(), c, name)
	if err != nil {
		failCtl(s, log, "env rm", err)
		return
	}
	fmt.Fprintf(s, "removed %s%s\r\n", res.Name, resyncNote(res.Resynced))
	if res.Unbound != "" {
		fmt.Fprintf(s, "note: the tag no longer boots from snapshot %q — the snapshot itself is kept.\r\n",
			res.Unbound)
	}
	// What an adopted environment gave back. These two would have been deleted
	// had the environment created them, so saying nothing would leave somebody
	// who remembers that rule assuming the worst about configuration that is
	// still there.
	if res.KeptSnapshot != "" {
		fmt.Fprintf(s, "note: the tag still boots from snapshot %q — it was bound before this\r\n"+
			"      environment adopted the tag, so it was left alone.\r\n", res.KeptSnapshot)
	}
	if len(res.KeptVars) > 0 {
		fmt.Fprintf(s, "note: these variables were on the tag before this environment adopted it\r\n"+
			"      and were kept: %s\r\n", strings.Join(res.KeptVars, ", "))
	}
	// Said out loud, because it is the one thing here that WAS deleted, and the
	// note below promises the opposite about everything else.
	if res.RemovedRule != "" {
		fmt.Fprintf(s, "note: the egress rule-set %q was created with this environment and went with it.\r\n",
			res.RemovedRule)
	}
	// The reassurance, and the map. `rm` on a grouping must never destroy the
	// things grouped, so the objects still carrying the tag are named — both so
	// nobody believes their token was deleted, and so somebody who DID want it
	// gone knows where to look.
	if len(res.Repos)+len(res.Secrets)+len(res.Rules) > 0 {
		fmt.Fprintf(s, "note: these still carry the tag %q and were not deleted:\r\n", res.Name)
		for _, sec := range []struct {
			head  string
			items []string
		}{
			{"repos", res.Repos},
			{"secrets", res.Secrets},
			{"rules", res.Rules},
		} {
			if len(sec.items) > 0 {
				fmt.Fprintf(s, "      %-8s %s\r\n", sec.head, strings.Join(sec.items, ", "))
			}
		}
	}
	s.Exit(0) //nolint:errcheck
}

// parseEnvSet reads `<name> [--repo o/n]… [--secret NAME]… [--rule NAME]…
// [--var K=V]… [--unset K]… [--description <text>]`.
//
// It returns the ctlops argument struct rather than a handful of slices, for
// parseRepoAdd's reason: a parser that returns the struct its caller passes on
// cannot drop a field on the way.
//
// An unknown flag is a refusal rather than a bare word. This door does not fold
// leftovers into anything the way new@ does, but a silently ignored
// `--secrets` (plural, which is what half the people who read the secret usage
// will type) would compose an environment missing the credential they named,
// discovered inside a sandbox with nothing pointing back at the typo.
func parseEnvSet(args []string) (ctlops.EnvArgs, []string, error) {
	var (
		a     ctlops.EnvArgs
		unset []string
		names []string
	)
	for i := 0; i < len(args); i++ {
		word := args[i]
		flag, value, attached := strings.Cut(word, "=")
		// value takes the next argument for the detached spelling, and the
		// consume-the-next-word rule is every flag's here, exactly as it is in
		// parseRepoAdd — a door whose flags each had their own dialect is a
		// door nobody can type at.
		take := func(example string) (string, error) {
			if !attached {
				i++
				if i >= len(args) {
					return "", fmt.Errorf("%s needs a value, e.g. %s", flag, example)
				}
				value = args[i]
			}
			if strings.TrimSpace(value) == "" {
				return "", fmt.Errorf("%s needs a value, e.g. %s", flag, example)
			}
			return value, nil
		}
		switch flag {
		case "--repo", "--repos":
			v, err := take("--repo wandb/hivemind")
			if err != nil {
				return ctlops.EnvArgs{}, nil, err
			}
			a.Repos = append(a.Repos, ctlops.RepoArgs{Slug: v})
		case "--secret", "--secrets":
			v, err := take("--secret GITHUB_TOKEN")
			if err != nil {
				return ctlops.EnvArgs{}, nil, err
			}
			a.Secrets = append(a.Secrets, v)
		case "--rule", "--rules":
			v, err := take("--rule npm-only")
			if err != nil {
				return ctlops.EnvArgs{}, nil, err
			}
			a.Rules = append(a.Rules, v)
		// Deliberately NOT aliased to `--env`: that flag means something else
		// three lines away, at the create door, and one word meaning two things
		// on one channel is worse than one spelling.
		case "--var", "--vars":
			// `--var K=V` is two '='s in one word for the attached spelling
			// (`--var=K=V`), so the value is whatever follows the FIRST one and
			// the pair is split at its own.
			v, err := take("--var NODE_ENV=test")
			if err != nil {
				return ctlops.EnvArgs{}, nil, err
			}
			k, val, ok := strings.Cut(v, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return ctlops.EnvArgs{}, nil, fmt.Errorf(
					"a variable is written NAME=value — %q has no name and value", v)
			}
			a.Vars = append(a.Vars, ctlops.EnvVar{Name: strings.TrimSpace(k), Value: val})
		case "--unset", "--rm-var":
			v, err := take("--unset NODE_ENV")
			if err != nil {
				return ctlops.EnvArgs{}, nil, err
			}
			if strings.Contains(v, "=") {
				return ctlops.EnvArgs{}, nil, fmt.Errorf(
					"--unset takes a variable's name, not a NAME=value pair — write `--unset %s`",
					strings.SplitN(v, "=", 2)[0])
			}
			unset = append(unset, strings.TrimSpace(v))
		case "--open-egress", "--no-egress-rules":
			// A gesture, not a value: it says "do not create the default
			// rule-set now". It is deliberately not stored, so it cannot become
			// a property of the environment that somebody later has to discover.
			//
			// The only flag here that takes no argument, so it is the only one
			// that must NOT touch i — the loop's own i++ is the whole advance.
			// `take` moves i for the others precisely because they consume the
			// next word; doing that here would silently eat the argument after
			// this one, which for `--open-egress --var K=V` is the var.
			if attached {
				return ctlops.EnvArgs{}, nil, fmt.Errorf("%s takes no value", flag)
			}
			a.OpenEgress = true
		case "--adopt":
			// The other valueless gesture, and the same i discipline applies:
			// consuming the next word here would eat the argument of whatever
			// follows.
			if attached {
				return ctlops.EnvArgs{}, nil, fmt.Errorf("%s takes no value", flag)
			}
			a.Adopt = true
		case "--description", "--desc":
			// The one flag whose value may legitimately be several words: the
			// shell is supposed to quote it, and somebody who forgets gets the
			// first word rather than a lecture. Last one wins — repeating a
			// flag that names one thing is a correction.
			v, err := take("--description \"the web box\"")
			if err != nil {
				return ctlops.EnvArgs{}, nil, err
			}
			a.Description = &v
		default:
			if strings.HasPrefix(word, "-") {
				return ctlops.EnvArgs{}, nil, fmt.Errorf("unknown flag %q", word)
			}
			names = append(names, word)
		}
	}
	switch len(names) {
	case 0:
		return ctlops.EnvArgs{}, nil, fmt.Errorf("name the environment, e.g. `env create web`")
	case 1:
		a.Name = names[0]
	default:
		// Two bare words is almost always a forgotten flag — `env set web
		// GITHUB_TOKEN` — and taking the first while ignoring the second would
		// silently drop the thing they were actually adding.
		return ctlops.EnvArgs{}, nil, fmt.Errorf(
			"one environment at a time — %q and %q both look like names; "+
				"attach things with --repo, --secret, --rule or --var", names[0], names[1])
	}
	// A name that is both set and unset in one command is a contradiction with
	// no reading that is obviously right, so neither is guessed at.
	for _, v := range a.Vars {
		for _, k := range unset {
			if v.Name == k {
				return ctlops.EnvArgs{}, nil, fmt.Errorf(
					"%s is both set and unset in the same command", k)
			}
		}
	}
	return a, unset, nil
}

// ---------------------------------------------------------------------------
// The setup script
// ---------------------------------------------------------------------------

// envScript prints an environment's setup script, or reads a new one from
// stdin under --set.
func (g *Gateway) envScript(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	name, set, fromRepo, err := parseEnvScript(args)
	if err != nil {
		fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n%s", err, envUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	if fromRepo {
		// The one path that deliberately overwrites this environment's script
		// with somebody else's copy of it, which is why it is a flag a person
		// types rather than anything a build decides.
		info, err := g.ops.AdoptRepoScript(s.Context(), c, name)
		if err != nil {
			failCtl(s, log, "env script", err)
			return
		}
		fmt.Fprintf(s, "%s now uses the %s in %s (%d bytes).\r\n",
			info.Name, ctlops.SetupScriptPath, info.ScriptDriftRepo, info.SetupBytes)
		fmt.Fprintf(s, "build it with:  ssh %s@%s env rebuild %s\r\n",
			ControlUser, g.sshHint(), info.Name)
		s.Exit(0) //nolint:errcheck
		return
	}
	if !set {
		script, from, err := g.ops.EnvScript(c, name)
		if err != nil {
			failCtl(s, log, "env script", err)
			return
		}
		if script == "" {
			// Not a silent success with an empty stdout: `env script web >
			// setup.sh` would otherwise truncate the file the user was about to
			// edit and tell them nothing. The sentence goes to stderr so it
			// cannot land in that file either.
			fmt.Fprintf(s.Stderr(), "sparkbox: %s has no setup script — set one with:\r\n"+
				"       cat setup.sh | ssh %s@%s env script %s --set\r\n",
				name, ControlUser, g.sshHint(), name)
			s.Exit(1) //nolint:errcheck
			return
		}
		writeScript(s, script)
		if from != "" {
			// On stderr, so it is visible to a person and absent from the file
			// a redirect writes.
			fmt.Fprintf(s.Stderr(), "sparkbox: %d bytes, from %s\r\n", len(script), from)
		}
		s.Exit(0) //nolint:errcheck
		return
	}

	script, err := readScript(s, name)
	if err != nil {
		fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
		s.Exit(2) //nolint:errcheck
		return
	}
	// SetupFromManual, always, from this door: a person piped this in. The
	// distinction is what a later build reads to decide whether it may
	// overwrite the script — one a person wrote is never overwritten — so
	// claiming any other origin here would let a rebuild throw their work away.
	if err := g.ops.SetEnvScript(c, name, script, envs.SetupFromManual); err != nil {
		failCtl(s, log, "env script", err)
		return
	}
	fmt.Fprintf(s, "set the setup script on %s (%d bytes)\r\n", name, len(script))
	fmt.Fprint(s, "it runs when the environment is built; nothing running now is touched.\r\n")
	s.Exit(0) //nolint:errcheck
}

// envBuildLog prints the most recent build's own output — the setup script's
// stdout/stderr in script mode, the runner's in agent mode.
//
// Read-only, unlike envScript: nothing a person types ever becomes this
// column, only a guest's report does, so there is no `--set` here.
func (g *Gateway) envBuildLog(s gssh.Session, c ctlops.Caller, name string, log *slog.Logger) {
	buildLog, err := g.ops.EnvBuildLog(c, name)
	if err != nil {
		failCtl(s, log, "env log", err)
		return
	}
	if buildLog == "" {
		fmt.Fprintf(s.Stderr(), "sparkbox: %s has no build log yet — it fills in once a build reports\r\n", name)
		s.Exit(1) //nolint:errcheck
		return
	}
	writeScript(s, buildLog)
	s.Exit(0) //nolint:errcheck
}

// parseEnvScript reads `<name> [--set]`.
func parseEnvScript(args []string) (name string, set, fromRepo bool, err error) {
	var names []string
	for _, a := range args {
		switch a {
		case "--set", "--save":
			set = true
		case "--from-repo", "--from-repository":
			fromRepo = true
		default:
			if strings.HasPrefix(a, "-") {
				return "", false, false, fmt.Errorf("unknown flag %q", a)
			}
			names = append(names, a)
		}
	}
	if set && fromRepo {
		// Two sources for one file. Refused rather than ranked, because either
		// ranking silently discards what the person meant by the other flag.
		return "", false, false, fmt.Errorf(
			"--set reads a script from stdin and --from-repo takes the one in the repository; pick one")
	}
	switch len(names) {
	case 0:
		return "", false, false, fmt.Errorf("name the environment, e.g. `env script web`")
	case 1:
		return names[0], set, fromRepo, nil
	default:
		// The grammar somebody will try first, and the one that must not be
		// read as a script: `env script web ./setup.sh` would otherwise store a
		// filename, or nothing at all. See this file's header.
		return "", false, false, fmt.Errorf(
			"the script is read from stdin, not from the command line — pipe it in instead:\r\n"+
				"       cat %s | ssh %s@<gateway> env script %s --set", names[1], ControlUser, names[0])
	}
}

// writeScript sends a stored script to the client.
//
// The bytes go out VERBATIM, newlines included, because the reason to run this
// command is `env script web > setup.sh` and a file whose every line ended
// \r\n is a file that shells and editors treat as damaged. That is the one
// place on this channel where a bare \n is correct: everything else it prints
// is a message to a terminal in raw mode, and this is a file.
func writeScript(s gssh.Session, script string) {
	fmt.Fprint(s, script)
	// Exactly one trailing newline, so a script stored without one still ends a
	// line rather than running into the next shell prompt.
	if !strings.HasSuffix(script, "\n") {
		fmt.Fprint(s, "\n")
	}
}

// readScript takes the script from stdin, and only from stdin.
//
// There is no terminal prompt half the way secret set has one: a value is one
// line somebody can be asked for, a shell script is a file. With a PTY this
// still works — type or paste it and press ctrl-D — and the refusal below names
// the pipe for anybody who gets nothing.
func readScript(s gssh.Session, name string) (string, error) {
	// +1 so an oversize script is detectable rather than arriving pre-truncated
	// into something that runs and does half the job.
	raw, err := io.ReadAll(io.LimitReader(s, maxScriptRead+1))
	if err != nil {
		return "", fmt.Errorf("could not read the script from stdin: %v", err)
	}
	if len(raw) > maxScriptRead {
		return "", fmt.Errorf("that script is larger than %d KiB — a setup script is a page of shell, "+
			"not an artifact; put the heavy lifting in a file the repo carries", maxScriptRead>>10)
	}
	// Only the outer whitespace goes, and only so a stray blank line at the end
	// of a heredoc is not stored as content. Nothing inside is touched: the
	// indentation of a shell script is part of the script.
	script := strings.TrimSpace(string(raw))
	if script == "" {
		return "", fmt.Errorf("no script arrived on stdin — pipe one in:\r\n"+
			"       cat setup.sh | ssh %s@<gateway> env script %s --set", ControlUser, name)
	}
	// \r\n would reach a guest as a script whose every line ends in a carriage
	// return, which /bin/sh reports as `command not found` on the command that
	// is plainly right there — the single most confusing failure this path can
	// produce, from a client that did nothing wrong.
	return strings.ReplaceAll(script, "\r\n", "\n"), nil
}

// ---------------------------------------------------------------------------
// The build
// ---------------------------------------------------------------------------

// failEnvBuild is failCtl plus the hint.
//
// failCtl prints Msg and nothing else, which is right for every refusal whose
// sentence IS the whole answer: `no sandbox named "x"` has nothing to add. The
// build's refusals are not that shape. ctlops splits them on purpose — the
// condition in Msg, what to do about it in Hint — and the commonest one by far
// is `env build web` on an environment nobody has written a setup script for,
// where the second half is the entire point: which file to write, which verb
// pipes one in, and that having an agent write it does not exist yet. Printing
// only the first half would leave a person reading "web has no setup script"
// with nowhere to go.
//
// It is two lines rather than one joined sentence because these hints are two
// or three sentences long; appended after the message they wrap somewhere
// arbitrary in the middle of a command the reader is meant to copy.
func failEnvBuild(s gssh.Session, log *slog.Logger, what string, err error) {
	e := ctlops.AsError(what, err)
	if !e.Verbatim || e.Hint == "" {
		failCtl(s, log, what, err)
		return
	}
	// failCtl's rule, restated rather than reached: a refusal the user is
	// already reading stays out of the operator's log unless something broke.
	switch e.Kind {
	case ctlops.KindInternal, ctlops.KindUpstream:
		log.Error(what+" failed", "err", err)
	default:
		log.Debug(what+" refused", "err", err, "kind", e.Kind.String())
	}
	fmt.Fprintf(s.Stderr(), "sparkbox: %s\r\n%s\r\n", e.Msg, e.Hint)
	s.Exit(e.ExitCode()) //nolint:errcheck
}

// envBuild starts a build and then gets out of the way.
//
// The whole of this function's job past the call is telling the truth about
// what it just did, because the command LOOKS synchronous and is not: ctlops
// returns once the builder sandbox exists and its guest has taken the job, and
// the setup script then runs for minutes inside that VM with nothing attached
// to this session. Somebody who believes otherwise waits, sees nothing, and
// presses ctrl-C — which does not stop the build but does teach them that the
// feature is broken. So the first line says it is running, the second and third
// say where to read the outcome and where to watch it happen, and none of them
// is a progress indicator this channel could not honour.
func (g *Gateway) envBuild(s gssh.Session, c ctlops.Caller, name string, log *slog.Logger) {
	info, err := g.ops.BuildEnvironment(s.Context(), c, name)
	if err != nil {
		failEnvBuild(s, log, "env build", err)
		return
	}
	if info.BuildBox == "" {
		// Unreachable from a successful build — the column is written before
		// the create — but the two hints below are worth nothing without a box
		// name, and printing `ssh @gateway` would be worse than printing less.
		fmt.Fprintf(s, "building %s — this takes minutes; read the outcome with `env show %s`.\r\n",
			info.Name, info.Name)
		s.Exit(0) //nolint:errcheck
		return
	}
	fmt.Fprintf(s, "building %s in %s. this takes minutes and keeps going if you disconnect.\r\n",
		info.Name, info.BuildBox)
	fmt.Fprintf(s, "  watch it      ssh %s@%s env show %s\r\n", ControlUser, g.sshHint(), info.Name)
	fmt.Fprintf(s, "  look inside   ssh %s@%s\r\n", info.BuildBox, g.sshHint())
	fmt.Fprintf(s, "when it succeeds, `ssh %s@%s -- --env %s` boots from the disk it built.\r\n",
		NewSandboxUser, g.sshHint(), info.Name)
	s.Exit(0) //nolint:errcheck
}

// envCapture adopts the builder exactly as it stands.
//
// Unlike `build` this one IS synchronous, and has to be: it pauses a VM and
// packs a disk, and the person who typed it is the person who just finished
// fixing that disk by hand. So it announces before it blocks, in `rename`'s
// shape, rather than going quiet for two minutes on a session with no output.
//
// The announcement needs the builder's name, which is read separately — and a
// failure of that read is deliberately ignored. CaptureEnvironment is the one
// place this command's refusals are decided; a second opinion from here is how
// one condition comes to have two wordings. All the read can do is make the
// sentence better, and when it cannot, nothing is printed before the refusal.
func (g *Gateway) envCapture(s gssh.Session, c ctlops.Caller, name string, log *slog.Logger) {
	if e, err := g.ops.GetEnvironment(c, name); err == nil && e.BuildBox != "" {
		fmt.Fprintf(s, "capturing %s (pause + pack; this takes minutes)…\r\n", e.BuildBox)
	}
	info, err := g.ops.CaptureEnvironment(s.Context(), c, name)
	if err != nil {
		failEnvBuild(s, log, "env capture", err)
		return
	}
	line := info.Name + " is " + info.State
	if info.Snapshot != "" {
		line += " — captured to " + info.Snapshot
	}
	fmt.Fprintf(s, "%s\r\n", line)
	fmt.Fprintf(s, "the builder is gone; sandboxes boot that disk now:\r\n"+
		"  ssh %s@%s -- --env %s\r\n", NewSandboxUser, g.sshHint(), info.Name)
	s.Exit(0) //nolint:errcheck
}

// envDriftLine is what `env show` says about an environment's setup script
// having moved away from the repository it came from, or "" when there is
// nothing to say.
//
// A MATCH PRINTS NOTHING, and so does an unknown. This line's whole job is to
// interrupt somebody who was about to build a script they did not mean to, and
// a row that appeared on every environment to report that everything was fine
// would train people to skip the place the warning lives.
//
// The two real verdicts get different sentences because they have different
// answers. ScriptDriftRepoAhead is handled BY the rebuild — the row is still a
// clean copy of what the repository seeded, so the next build takes the newer
// one on its own — and the sentence is an invitation. ScriptDriftDiverged is
// handled by nothing: this environment's script has been changed since it was
// seeded, by a repair pass or by somebody, so a person has to choose, and the
// sentence says plainly which of the two a rebuild would run.
func envDriftLine(e ctlops.EnvironmentInfo) string {
	switch e.ScriptDrift {
	case ctlops.ScriptDriftRepoAhead:
		return ctlops.SetupScriptPath + " in " + e.ScriptDriftRepo +
			" has changed since this was built — `env rebuild " + e.Name + "` picks it up"
	case ctlops.ScriptDriftDiverged:
		return "differs from " + ctlops.SetupScriptPath + " in " + e.ScriptDriftRepo +
			" and has been changed since it came from there, so a rebuild keeps THIS one. " +
			"Read it with `env script " + e.Name + "`, or take the repository's with `env script " +
			e.Name + " --from-repo`"
	case ctlops.ScriptDriftRepoOnly:
		return "none recorded yet — the next build reads " + ctlops.SetupScriptPath +
			" from " + e.ScriptDriftRepo
	default:
		return ""
	}
}
