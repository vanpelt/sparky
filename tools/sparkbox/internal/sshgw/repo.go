package sshgw

// The `repo` verbs on the ctl channel, and the `github install` door beside
// them.
//
// They are here — rather than only in the user console, which is where
// internal/netrules stopped — because this is the channel a sandbox is created
// on. `ssh new@gw -- --tag hivemind` is one gesture that means these secrets,
// this egress policy and this checkout, and the moment somebody discovers that
// their tag has no repo on it is the moment they type that. A panel in a
// browser cannot be there for it.
//
// Nothing here holds a credential. An attachment is configuration — a slug, a
// tag, a ref — and the token that clones it is minted inside the guest's own
// request, an hour at a time, and never touches this channel. So unlike
// `secret set` there is no stdin ceremony: everything a repo attachment needs
// can safely be an argument.

import (
	"fmt"
	"log/slog"
	"strings"

	gssh "github.com/gliderlabs/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
)

const repoUsage = "usage: ssh ctl@<gateway> repo ls\r\n" +
	"       ssh ctl@<gateway> repo add <owner>/<name> [--tag <t>]… [--write] [--ref <r>] [--path <p>]\r\n" +
	"       ssh ctl@<gateway> repo rm <owner>/<name>\r\n" +
	"       ssh ctl@<gateway> repo check\r\n" +
	"\r\n" +
	"an attachment is carried by a tag: every sandbox of yours with that tag clones\r\n" +
	"the repo at boot, before you get there. with no --tag it goes on `default`,\r\n" +
	"which every new sandbox of yours has — so it lands in all of them.\r\n" +
	"\r\n" +
	"nothing here is a credential. the token that clones a private repo is minted\r\n" +
	"inside the sandbox, expires in an hour, and is never stored anywhere.\r\n" +
	"\r\n" +
	"the attachment's --ref is where a CLONE starts and pins nothing after that.\r\n" +
	"to start ONE sandbox on a different branch, say so when you create it:\r\n" +
	"\r\n" +
	"  ssh new+box@<gateway> hm --ref feat/x\r\n" +
	"  ssh ctl@<gateway> fork cuda-12 box --ref wandb/hivemind=feat/x\r\n" +
	"\r\n" +
	"inside a sandbox, `sparkbox repos sync` brings checkouts forward. it can only\r\n" +
	"fetch, fast-forward a clean tree, or tell you why it did neither — uncommitted\r\n" +
	"work, untracked files and unpushed commits are reported and left alone.\r\n" +
	"\r\n" +
	"  ssh ctl@<gateway> repo add wandb/hivemind --tag hm\r\n" +
	"  ssh ctl@<gateway> repo check\r\n"

// controlRepo serves `ctl repo …`.
func (g *Gateway) controlRepo(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	// The host-not-configured answer comes first, before the usage line: a host
	// with no repo store cannot attach anything however the command was typed.
	// The sentence is the one ctlops would have printed for the same condition,
	// deliberately — two wordings for one state is how a user ends up believing
	// they are two states.
	if !g.ops.Capabilities().Repos {
		fmt.Fprint(s.Stderr(), "sparkbox: repo attachments are not enabled on this host\r\n")
		s.Exit(1) //nolint:errcheck
		return
	}
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), repoUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	switch args[0] {
	case "ls", "list":
		list, err := g.ops.ListRepos(c)
		if err != nil {
			failCtl(s, log, "repo ls", err)
			return
		}
		for _, r := range list {
			fmt.Fprintf(s, "%-40s %-20s %-5s %s\r\n",
				truncate(r.Slug, 40), truncate(strings.Join(r.Tags, ","), 20), r.Access, repoWhere(r))
		}
		if len(list) == 0 {
			fmt.Fprint(s, "no repos attached — attach one with:\r\n"+
				"  ssh ctl@"+g.sshHint()+" repo add wandb/hivemind --tag hm\r\n")
		}
		s.Exit(0) //nolint:errcheck

	case "add":
		g.repoAdd(s, c, args[1:], log)

	case "rm", "delete":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), repoUsage)
			s.Exit(2) //nolint:errcheck
			return
		}
		affected, err := g.ops.DetachRepo(s.Context(), c, "", args[1])
		if err != nil {
			failCtl(s, log, "repo rm", err)
			return
		}
		fmt.Fprintf(s, "detached %s\r\n", args[1])
		if len(affected) > 0 {
			// Said plainly because the opposite is what people expect from a
			// verb called `rm`: a checkout is a directory somebody may be
			// working in, so detaching stops future clones and touches nothing
			// that already exists.
			fmt.Fprintf(s, "note: %s already has a clone of it — that is left alone; "+
				"new sandboxes just won't get it.\r\n", strings.Join(affected, ", "))
		}
		s.Exit(0) //nolint:errcheck

	case "check":
		g.repoCheck(s, c, log)

	default:
		fmt.Fprintf(s.Stderr(), "unknown repo command %q\r\n%s", args[0], repoUsage)
		s.Exit(2) //nolint:errcheck
	}
}

// repoAdd attaches one repository and then says every true thing about it that
// the user cannot see: which of their sandboxes it reaches, whether it went on
// the tag that reaches all of them, and whether the App can get at it at all.
func (g *Gateway) repoAdd(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	a, err := parseRepoAdd(args)
	if err != nil {
		fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n%s", err, repoUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	res, err := g.ops.AttachRepo(s.Context(), c, a)
	if err != nil {
		failCtl(s, log, "repo add", err)
		return
	}
	fmt.Fprintf(s, "attached %s  (tags: %s; %s)%s\r\n",
		res.Repo.Slug, strings.Join(res.Repo.Tags, ","), res.Repo.Access, sandboxNote(res.Sandboxes))

	if res.Defaulted {
		// The footgun the design (§2.2) says must be printed at attach time
		// rather than left in a doc: `default` is stamped on every new sandbox
		// too, so an untagged attachment is a standing instruction to clone
		// this into everything the user makes from now on. That is a legitimate
		// thing to want and a surprising thing to get by accident.
		fmt.Fprintf(s, "note: with no --tag this went on the `%s` tag, which every new sandbox of\r\n"+
			"      yours carries — so every sandbox you create from now on will clone it.\r\n"+
			"      narrow it with `repo add %s --tag <t>`, or undo it with `repo rm %s`.\r\n",
			secrets.DefaultTag, res.Repo.Slug, res.Repo.Slug)
	}
	if len(res.Sandboxes) > 0 {
		// The running ones have been nudged into checking it out; the paused and
		// archived ones will at their next boot. Both halves are said, because
		// "3 sandboxes select it" and "3 sandboxes now have it" are different
		// claims and only the first is true the moment this prints — a clone
		// runs on the guest's clock, not this command's.
		fmt.Fprintf(s, "note: the sandboxes above are checking it out now, or will at their next\r\n"+
			"      start — run `sparkbox repos` inside one to watch.\r\n")
	}
	// One line per box that could not be reached or is too old to check
	// anything out. Never an error: the attachment is stored either way.
	for _, n := range res.Notes {
		fmt.Fprintf(s, "note: %s\r\n", n)
	}
	switch {
	case !res.Check.Checked:
		fmt.Fprint(s, "note: this host has no GitHub App configured, so only public repos will clone.\r\n")
	case !res.Check.Reachable:
		// Attached, but it will not clone yet — and this is the only moment
		// anybody is looking. Without it the answer arrives inside a VM at boot,
		// in a log nobody reads.
		fmt.Fprintf(s, "but %s\r\n", res.Check.Reason)
		if res.Check.InstallURL != "" && !strings.Contains(res.Check.Reason, res.Check.InstallURL) {
			fmt.Fprintf(s, "      install it here: %s\r\n", res.Check.InstallURL)
		}
	}
	s.Exit(0) //nolint:errcheck
}

// repoCheck prints one line per attachment: whether the App can reach it, and
// why not when it cannot.
func (g *Gateway) repoCheck(s gssh.Session, c ctlops.Caller, log *slog.Logger) {
	checks, err := g.ops.CheckRepos(s.Context(), c)
	if err != nil {
		failCtl(s, log, "repo check", err)
		return
	}
	for _, rc := range checks {
		state := "ok"
		if !rc.Reachable {
			state = "unreachable"
		}
		fmt.Fprintf(s, "%-40s %-12s %s\r\n", truncate(rc.Slug, 40), state, rc.Reason)
	}
	if len(checks) == 0 {
		fmt.Fprint(s, "no repos attached — attach one with:\r\n"+
			"  ssh ctl@"+g.sshHint()+" repo add wandb/hivemind --tag hm\r\n")
	}
	s.Exit(0) //nolint:errcheck
}

// controlGitHubInstall prints where to install this host's GitHub App.
//
// It is a command rather than a line in the help text because the URL belongs
// to the App this host actually holds: one copied out of somebody's runbook
// installs a different App, which would look like it worked and then fail every
// mint.
func (g *Gateway) controlGitHubInstall(s gssh.Session, c ctlops.Caller, log *slog.Logger) {
	url, err := g.ops.GitHubInstallURL(c)
	if err != nil {
		failCtl(s, log, "github install", err)
		return
	}
	fmt.Fprintf(s, "install the GitHub App on the repositories you want in your sandboxes:\r\n  %s\r\n", url)
	fmt.Fprintf(s, "then attach one:  ssh %s@%s repo add <owner>/<name> --tag <t>\r\n", ControlUser, g.sshHint())
	s.Exit(0) //nolint:errcheck
}

// parseRepoAdd reads `<owner>/<name> [--tag t]… [--write] [--ref r] [--path p]`.
//
// The tag grammar is parseTags', unchanged, so `--tag`, `-t` and the `=` forms
// mean here exactly what they mean at the new@ door — and comma lists stay
// whole for ctlops.NormalizeTags to split, because a second splitter is a
// second chance for the two to disagree. --ref and --path follow splitNodeFlag:
// last one wins, since repeating a flag that names one thing is a correction
// rather than a list.
//
// It returns ctlops.RepoArgs rather than a handful of values because that is
// what the next line passes on; a parser that returns the argument struct
// cannot drop a field on the way.
func parseRepoAdd(args []string) (ctlops.RepoArgs, error) {
	tags, rest, err := parseTags(args)
	if err != nil {
		return ctlops.RepoArgs{}, err
	}
	a := ctlops.RepoArgs{Tags: tags}
	var slugs []string
	for i := 0; i < len(rest); i++ {
		word := rest[i]
		flag, value, attached := strings.Cut(word, "=")
		switch flag {
		case "--write":
			if attached {
				return ctlops.RepoArgs{}, fmt.Errorf("--write takes no value")
			}
			a.Write = true
		case "--ref", "--branch":
			if !attached {
				i++
				if i >= len(rest) {
					return ctlops.RepoArgs{}, fmt.Errorf("%s needs a value, e.g. --ref main", flag)
				}
				value = rest[i]
			}
			if strings.TrimSpace(value) == "" {
				return ctlops.RepoArgs{}, fmt.Errorf("%s needs a value, e.g. --ref main", flag)
			}
			a.Ref = value
		case "--path":
			if !attached {
				i++
				if i >= len(rest) {
					return ctlops.RepoArgs{}, fmt.Errorf("--path needs a value, e.g. --path src/hivemind")
				}
				value = rest[i]
			}
			if strings.TrimSpace(value) == "" {
				return ctlops.RepoArgs{}, fmt.Errorf("--path needs a value, e.g. --path src/hivemind")
			}
			a.Path = value
		default:
			if strings.HasPrefix(word, "-") {
				return ctlops.RepoArgs{}, fmt.Errorf("unknown flag %q", word)
			}
			slugs = append(slugs, word)
		}
	}
	switch len(slugs) {
	case 0:
		return ctlops.RepoArgs{}, fmt.Errorf("name the repository to attach, as <owner>/<name>")
	case 1:
		a.Slug = slugs[0]
	default:
		// Two bare words is almost always a forgotten `--tag`, and attaching the
		// first while ignoring the second would be the worst of the readings.
		return ctlops.RepoArgs{}, fmt.Errorf(
			"attach one repository at a time — %q and %q both look like repositories", slugs[0], slugs[1])
	}
	return a, nil
}

// repoWhere renders the two optional halves of an attachment: which ref it
// tracks and where in the home directory it lands. Both are empty for the
// ordinary case, which is the default branch in the default layout.
func repoWhere(r repos.Repo) string {
	var parts []string
	if r.Ref != "" {
		parts = append(parts, "@"+r.Ref)
	}
	if r.Path != "" {
		parts = append(parts, "→ ~/"+r.Path)
	}
	return strings.Join(parts, "  ")
}

// sandboxNote renders the fan-out as a clause. The empty case is the one that
// matters: an attachment no sandbox of theirs carries is the tag mistake this
// feature invites, and it is silent everywhere else.
func sandboxNote(names []string) string {
	switch len(names) {
	case 0:
		return " — no sandbox of yours carries that tag yet"
	case 1:
		return " — " + names[0] + " carries that tag"
	default:
		return fmt.Sprintf(" — %d sandboxes carry that tag: %s", len(names), strings.Join(names, ", "))
	}
}
