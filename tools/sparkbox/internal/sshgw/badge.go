package sshgw

// `ctl badge` — the one command on this channel whose output is meant to leave
// the terminal.
//
// Everything else here prints a fact somebody reads once and forgets. This
// prints markdown that gets pasted into a pull-request comment, where it
// outlives the terminal, the session, and quite possibly this deployment: a
// GitHub comment cannot be edited by us, so a URL minted wrong today is wrong
// in that comment forever. That is the whole reason for the shape of this file.
//
// Three consequences, each of which explains something below that would
// otherwise look like fussiness:
//
//   - The host gate runs before the argument check, so somebody on a host with
//     no launch door learns that fact rather than being corrected on a typo and
//     then told the same thing on their second try. That is repo.go's rule too.
//   - Both hostnames are built from the labels this gateway was actually
//     configured with, never from the literals "go" and "xterm". A host that
//     renamed its launch label and left a hardcoded one here would hand out
//     buttons that 404, in comments nobody can take back.
//   - The snippet is emitted as ONE line on stdout with everything else on
//     stderr, so `ssh ctl@<gateway> badge wandb/hivemind | pbcopy` puts exactly
//     the right bytes on the clipboard. It is also why the line is assembled by
//     concatenation rather than written as a raw string literal: a raw literal
//     is where a real "\n" gets in, and this channel's client is in raw mode —
//     see the CRLF assertion in control_golden_test.go.
//
// Nothing here writes. `badge` reads the caller's attachments to say true
// things about the link, and a missing attachment is a note rather than a
// refusal — the markdown is correct for whoever will click it, who may well
// have attached the repository even when the person printing the button has
// not.

import (
	"fmt"
	"log/slog"
	"strings"

	gssh "github.com/gliderlabs/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
)

// controlBadge prints the launch button for one repository, plus every true
// thing about it the user cannot see from here: whether they have it attached
// at all, whether the ref they asked for is already the attachment's own, which
// tag the resulting sandbox would carry, and what it means to hand a stranger a
// branch of yours to run.
//
// args is already past the verb — the dispatch in control.go passes args[1:] —
// so the slug is args[0]. ownedBoxArg reads args[1] because it is handed the
// whole command; mixing the two conventions silently reads "badge" as the
// repository and then refuses it as a slug.
func (g *Gateway) controlBadge(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	// Before the usage line, deliberately, the way `repo` answers on a host
	// with no repo store: a person who typed the command wrong on a host that
	// cannot do the thing should learn the fact about the HOST, or they fix the
	// typo and get told the same thing on the second attempt.
	//
	// All three halves are load-bearing. Without g.domain there is no zone to
	// build a URL in — domainHint() would substitute the literal "<domain>",
	// which reads fine in usage prose and is a broken link inside markdown
	// somebody pastes into a permanent public comment. Without launchSubdomain
	// no door answers. Without xtermSubdomain there is no browser terminal for
	// a clicker to land in, which is the button's entire payoff, and it is also
	// exactly the condition cmd/sparkbox uses to decide not to mount the door.
	if g.domain == "" || g.launchSubdomain == "" || g.xtermSubdomain == "" {
		fmt.Fprint(s.Stderr(), "sparkbox: launch links are not enabled on this host\r\n")
		s.Exit(1) //nolint:errcheck
		return
	}
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), repoUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	slug, ref, err := parseBadge(args)
	if err != nil {
		fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n%s", err, repoUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	// repos.ValidSlug rather than a regexp written here. The owner half is a
	// GitHub login grammar and the name half allows dots, underscores and a
	// leading digit, so a hand-rolled pattern is the mistake that rejects
	// node.js — and this is also the same function the launch door validates
	// the path with, so a slug this accepts is one that door will accept back.
	if !repos.ValidSlug(slug) {
		fmt.Fprintf(s.Stderr(), "sparkbox: %q is not an owner/name repository\r\n%s", slug, repoUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	// Likewise repos.ValidRef, which is one authority over two rules: the
	// regexp, and the separate ".." check the regexp does not cover. Refusing
	// here rather than downstream matters because the ref ends up as the
	// argument of `git clone --branch <ref>` inside somebody else's sandbox,
	// where a leading '-' is an option and not a branch.
	if ref != "" && !repos.ValidRef(ref) {
		fmt.Fprintf(s.Stderr(),
			"sparkbox: %q is not a branch or tag name (it must start with a letter or digit, "+
				"and cannot contain \"..\")\r\n", ref)
		s.Exit(2) //nolint:errcheck
		return
	}

	// The lookup is advisory: it decides which notes to print and whether the
	// ref can be dropped, never whether a button is printed. A failure of the
	// call itself is a different thing from a repository that is simply not
	// attached, so it goes through failCtl — that is what keeps "this host has
	// no repo store" from being silently rendered as "you have nothing
	// attached", which is a sentence that would send somebody to run `repo add`
	// on a host where it cannot work.
	list, err := g.ops.ListRepos(c)
	if err != nil {
		failCtl(s, log, "badge", err)
		return
	}
	att, attached := findAttachment(list, slug)
	if attached {
		// The stored casing wins over what was typed, the way `repo add` echoes
		// res.Repo.Slug rather than the argument: the launch door matches the
		// path case-insensitively (the store's columns are COLLATE NOCASE), so
		// both work, and the one the user wrote down when they attached it is
		// the one that will match what GitHub shows them.
		slug = att.Slug
	}

	// The emitting half of the launch door's normalize rule, and the reason
	// that rule is symmetric: a sandbox sitting on the attachment's default ref
	// reports that ref as its effective one, so a button carrying `?ref=main`
	// against an attachment whose default is already `main` would fail to match
	// it and create a second sandbox on every click. Dropping the ref here is
	// what makes the button reuse instead. Compared byte for byte, never folded
	// — feat/X and feat/x are two branches, and nothing in this tree pretends
	// otherwise.
	suppressed := ""
	if attached && att.Ref != "" && ref == att.Ref {
		suppressed, ref = ref, ""
	}

	fmt.Fprintf(s, "%s\r\n", g.badgeMarkdown(slug, ref))

	// Everything from here is stderr, so the snippet above is the only thing a
	// pipe carries. The convention is `session-token`'s.
	branch := "its default branch"
	if ref != "" {
		branch = ref
	}
	fmt.Fprintf(s.Stderr(), "paste that into a pull request comment on %s. anyone who clicks it\r\n"+
		"signs in, then lands in a sandbox with the repo checked out on %s — theirs,\r\n"+
		"not yours, and one per person.\r\n", slug, branch)

	if !attached {
		// Not a refusal, and said in the order somebody acts on: what the
		// button will do as it stands, then the one command that changes it.
		//
		// It does NOT say the button builds an empty sandbox, because it does
		// not: the launch door refuses a create it cannot satisfy and shows the
		// clicker its own "attach it first" screen instead. Saying otherwise
		// here would misdescribe the feature in the one place whose output gets
		// written down permanently. The second sentence is the part authors get
		// wrong — an attachment is per person, so the clicker needs their own
		// whatever this account has.
		fmt.Fprintf(s.Stderr(), "note: nothing of yours is attached to %s, so clicking the button shows you\r\n"+
			"      the \"attach it first\" screen rather than building anything. attach it:\r\n"+
			"      ssh %s@%s repo add %s --tag hm\r\n"+
			"      attachments are per person — whoever clicks needs their own either way.\r\n",
			slug, ControlUser, g.sshHint(), slug)
	}
	if suppressed != "" {
		fmt.Fprintf(s.Stderr(), "note: %s is already the attachment's own branch, so the link leaves it out —\r\n"+
			"      a sandbox on it matches instead of a second one being created.\r\n", suppressed)
	}
	if attached {
		// UNCONDITIONAL, and the guard this used to carry was the bug: it only
		// fired when the ATTACHMENT itself was on `default`, but the consequence
		// has nothing to do with the attachment's tags. ctlops.Ops.Create stamps
		// `default` on every sandbox it builds (defaultTags), so the box this
		// button makes always carries it, and repos.ReposForSandbox joins
		// sandbox_tags to repo_tags — meaning it always also clones whatever the
		// CLICKER has on `default`, however narrowly this attachment is tagged.
		//
		// The old remedy was wrong for the same reason. `repo add --tag <t>`
		// narrows this attachment; it cannot remove `default` from a sandbox,
		// because nothing can. So the note now states the fact and stops, rather
		// than offering a command that does not do what it says.
		fmt.Fprintf(s.Stderr(), "note: every sandbox carries the `%s` tag, so the button's sandbox clones\r\n"+
			"      this repo AND everything the person clicking has on `%s`.\r\n"+
			"      that is their attachment list, not yours, and nothing narrows it away.\r\n",
			secrets.DefaultTag, secrets.DefaultTag)
	}
	// Last, and unconditional, because it is the only note that is about the
	// person who clicks rather than the person who typed. The clone runs no
	// hooks and initialises no submodules, so this is not remote code execution
	// — but an agent started in that checkout reads instructions written by
	// whoever chose the branch, with a credentialed `gh` on its PATH.
	fmt.Fprint(s.Stderr(), "note: whoever clicks runs that branch's code in their own sandbox, with a github\r\n"+
		"      token minted for this repo. only post a button for a branch you would run.\r\n")
	s.Exit(0) //nolint:errcheck
}

// badgeMarkdown renders the snippet, on ONE line with no leading whitespace.
//
// Both are decisions rather than formatting. Four leading spaces in a markdown
// comment makes an indented code block, so the button renders as literal text;
// zero is the only indent nobody can break by pasting it somewhere else. And
// because the repository sits in the PATH rather than in a query parameter, the
// no-ref form contains no `&` at all and the ref form contains exactly one — so
// `&amp;` is the only escaping a human retyping this can get wrong, in exactly
// one place.
//
// Nothing here is escaped, and nothing needs to be: repos.ValidSlug and
// repos.ValidRef have already refused every byte that would matter. Their
// grammars admit letters, digits, and `.`, `_`, `-` and `/` — no quote, no `<`,
// no `&`, no space, no `%`. The `/` inside a ref is left literal rather than
// percent-encoded, because it is legal in a query value per RFC 3986, no
// browser touches it, and `%2F` is one more thing a person copying this by hand
// gets wrong. If either validator ever loosens, this line needs an html escaper
// the same day.
//
// The URL is portless on purpose. SPARKBOX_EDGE_REDIRECT DNATs the whole
// uplink 1024-65535 range to the one edge listener, so a port here would work
// and would also pin a comment to a deployment detail.
func (g *Gateway) badgeMarkdown(slug, ref string) string {
	host := "https://" + g.launchSubdomain + "." + g.domain
	href := host + "/" + slug
	if ref != "" {
		href += "?ref=" + ref
	}
	// `<a>` and `<img>` are elements GitHub's markdown sanitizer reliably keeps,
	// and `align` and `height` are on its attribute allowlist where `style` is
	// not — all three confirmed by rendering this exact line through GitHub's
	// own /markdown API rather than from memory.
	//
	// `align="right"` goes on the IMG, which floats it, and that is the
	// difference between the two shapes that survive the sanitizer. Wrapped in
	// a `<div align="right">` the button is a block that STACKS above whatever
	// follows, taking a line of its own; floated, the next heading flows up
	// beside it and the button lands in the comment's top-right corner — which
	// is where a reader's eye already goes and where the affordance belongs on
	// a bot comment whose first line is a heading.
	//
	// Paste it as the FIRST thing in the comment, ahead of the heading, or the
	// float has nothing to sit beside. The doc says so; this comment says why
	// it matters.
	//
	// There is no `width`, and that asymmetry is load-bearing rather than an
	// oversight: GitHub injects `aspect-ratio: <width>/<height>` computed from
	// the attributes it finds, so declaring both would pin the ratio inside
	// every comment already posted and letterbox them all the day the badge is
	// redrawn. Height alone lets the SVG's own dimensions govern forever.
	//
	// The alt text reads as an action so a blocked or broken image degrades to
	// a sentence somebody can still act on rather than to a filename.
	return `<a href="` + href + `"><img align="right" src="` + host +
		`/badge.svg" alt="Open in Sparkbox" height="28"></a>`
}

// parseBadge reads `<owner>/<name> [--ref <r>]`.
//
// The --ref grammar is parseRepoAdd's, unchanged: `--ref x`, `--ref=x` and the
// `--branch` spelling all mean the same thing, and a repeated flag is a
// correction rather than a list, so the last one wins. Two dialects for one
// flag on one channel is how a person ends up believing the two commands take
// different arguments.
//
// It validates nothing about the values it returns. That is on purpose: the
// slug and the ref get two different refusal sentences — one that reprints the
// usage page and one that explains the ref grammar — and a parser handing back
// a single error cannot say which of the two the caller should print.
func parseBadge(args []string) (slug, ref string, err error) {
	var slugs []string
	for i := 0; i < len(args); i++ {
		word := args[i]
		flag, value, attached := strings.Cut(word, "=")
		switch flag {
		case "--ref", "--branch":
			if !attached {
				i++
				if i >= len(args) {
					return "", "", fmt.Errorf("%s needs a value, e.g. --ref feat/x", flag)
				}
				value = args[i]
			}
			if strings.TrimSpace(value) == "" {
				return "", "", fmt.Errorf("%s needs a value, e.g. --ref feat/x", flag)
			}
			ref = value
		default:
			if strings.HasPrefix(word, "-") {
				return "", "", fmt.Errorf("unknown flag %q", word)
			}
			slugs = append(slugs, word)
		}
	}
	switch len(slugs) {
	case 0:
		return "", "", fmt.Errorf("name the repository to make a button for, as <owner>/<name>")
	case 1:
		return slugs[0], ref, nil
	default:
		// Two bare words is almost always a forgotten `--ref`, and printing a
		// button for the first while ignoring the second is the worst of the
		// readings — it is the one that ends up in a comment.
		return "", "", fmt.Errorf(
			"one repository at a time — %q and %q both look like repositories", slugs[0], slugs[1])
	}
}

// findAttachment picks the caller's attachment for slug, case-insensitively.
//
// EqualFold rather than == because the store's owner and slug columns are
// COLLATE NOCASE, so `WANDB/Hivemind` and `wandb/hivemind` are one row there
// and would be two here. ctlops has its own copy of this for the same reason
// and keeps it unexported; there is no Ops.GetRepo to call instead.
func findAttachment(list []repos.Repo, slug string) (repos.Repo, bool) {
	for _, r := range list {
		if strings.EqualFold(r.Slug, slug) {
			return r, true
		}
	}
	return repos.Repo{}, false
}

// hasTag reports whether tags carries want. Tags are stored normalized, so a
// plain comparison is right here; it is a helper only so the condition it
// guards reads as the sentence it is.
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
