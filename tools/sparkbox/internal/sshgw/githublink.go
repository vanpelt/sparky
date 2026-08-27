package sshgw

// Linking a GitHub account, in one dialog reachable from every door.
//
// This used to live inside the signup dialog and nowhere else, which meant the
// offer reached exactly the people who arrived through `ssh signup@` — not the
// operators seeded from users.conf, not anybody who skipped it the first time,
// not anybody whose key was not on GitHub that day. Everybody else had to know
// that `ctl keys verify-github <login>` existed and that their sandbox key
// happened to be published on GitHub. The offer being hard to find is why so
// few accounts carried one.
//
// So the dialog is a function, and signup, `ctl github link` and the nudges all
// enter it the same way.
//
// Two paths, and which one runs is a property of the HOST rather than a choice
// the user makes:
//
//   - The device flow, when the host has a GitHub app configured. It proves the
//     account with GitHub directly, needs nothing published anywhere, and — the
//     part that makes it fit here — needs no terminal. There is nothing to type
//     into this session, so it works over `ssh ctl@host github link` with no
//     PTY, which is what a plain non-interactive ssh invocation gives you.
//   - The key check otherwise, which is what shipped before and needs no
//     configuration at all: name a login, and one of your registered keys is
//     looked for among the ones github.com publishes for it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/term"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// deviceWait is how long a session will sit waiting for somebody to finish in
// their browser.
//
// GitHub's own code lives for fifteen minutes, which is far longer than anyone
// will watch a hung terminal, and an SSH session held open that long against a
// person who wandered off is a session the reaper cannot tell from work. Five
// minutes is long enough to find the tab, log in, and approve; past it the
// message says to run the command again, which costs one round trip.
const deviceWait = 5 * time.Minute

// linkGitHub runs whichever dialog this host supports and reports the login it
// linked, or "" when nothing was linked.
//
// Nothing here is fatal to its caller. Signup calls it with an account that
// already exists and email still to ask for; a failed or declined link must
// leave both alone. Every path therefore prints its own sentence — including
// the command to run later — and returns "".
func (g *Gateway) linkGitHub(s gssh.Session, w io.Writer, c ctlops.Caller, arg string, log *slog.Logger) string {
	if g.ops.Capabilities().GitHubDevice {
		return g.linkGitHubByDevice(s, w, c, log)
	}
	return g.linkGitHubByKey(s, w, c, arg, log)
}

// linkGitHubByDevice prints a code, waits, and reports what GitHub said.
func (g *Gateway) linkGitHubByDevice(s gssh.Session, w io.Writer, c ctlops.Caller, log *slog.Logger) string {
	dc, err := g.ops.StartGitHubLink(s.Context(), c)
	if err != nil {
		fmt.Fprintf(w, "couldn't start the GitHub sign-in: %s\r\n", linkMessage("github.link", err))
		return ""
	}
	// The code goes on its own line with nothing after it. People select it with
	// a double-click, and a trailing period lands inside the selection.
	fmt.Fprintf(w, "\r\nopen %s and enter this code:\r\n\r\n", dc.VerificationURI)
	fmt.Fprintf(w, "    %s\r\n\r\n", dc.UserCode)
	fmt.Fprint(w, "waiting for you to authorize it (ctrl-c to skip)…\r\n")

	// The session's own context is the outer bound — a disconnect must stop the
	// polling rather than leave a goroutine talking to github.com about a
	// terminal nobody is watching — and deviceWait is the inner one.
	ctx, cancel := context.WithTimeout(s.Context(), deviceWait)
	defer cancel()
	me, err := g.ops.FinishGitHubLink(ctx, c, dc)
	if err != nil {
		// A caller who gave up is not a failure and is not worth a scary line;
		// everything else gets ctlops' own sentence, which already distinguishes
		// declined from expired from unreachable.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(w, "no answer from GitHub yet — nothing linked. run this again when you're ready:\r\n")
		} else {
			fmt.Fprintf(w, "%s\r\n", linkMessage("github.link", err))
		}
		fmt.Fprintf(w, "  ssh %s@%s github link\r\n", ControlUser, g.sshHint())
		return ""
	}
	fmt.Fprintf(w, "✓ linked to github.com/%s\r\n", me.GitHubLogin)
	return me.GitHubLogin
}

// linkGitHubByKey is the original check: name a login, and one of the keys
// already on this account is looked for among the ones github.com publishes
// for it.
//
// arg is a login supplied on the command line. Without one this needs a
// terminal to ask, and says so rather than failing silently for a caller who
// ran `ssh ctl@host github link` with no PTY.
func (g *Gateway) linkGitHubByKey(s gssh.Session, w io.Writer, c ctlops.Caller, arg string, log *slog.Logger) string {
	login := strings.TrimSpace(arg)
	if login == "" {
		t, ok := w.(*term.Terminal)
		if !ok {
			fmt.Fprintf(w, "name your GitHub account:  ssh %s@%s github link <login>\r\n",
				ControlUser, g.sshHint())
			return ""
		}
		fmt.Fprint(t, "link a GitHub account? enter your GitHub username to verify\r\n")
		fmt.Fprint(t, "this key against github.com/<user>.keys (or blank to skip)\r\n")
		t.SetPrompt("github: ")
		line, err := t.ReadLine()
		if err != nil {
			return ""
		}
		login = strings.TrimSpace(line)
		if login == "" {
			return ""
		}
	}
	me, err := g.ops.VerifyGitHub(s.Context(), c, login, c.KeyFP)
	if err != nil {
		fmt.Fprintf(w, "%s\r\n", linkMessage("keys.verify-github", err))
		fmt.Fprintf(w, "add that key to github.com, then run:  ssh %s@%s github link %s\r\n",
			ControlUser, g.sshHint(), login)
		return ""
	}
	fmt.Fprintf(w, "✓ key %s is listed on github.com/%s — verified.\r\n", c.KeyFP, me.GitHubLogin)
	return me.GitHubLogin
}

// linkMessage renders a refusal as the one sentence this dialog prints.
//
// It joins the hint on purpose, unlike failCtl: every failure here is one a
// person can do something about — publish the key, run it again, ask an
// operator — and the sentence without the hint reads as a dead end. It never
// exits the session, because none of these are fatal to what the caller was
// doing.
func linkMessage(op string, err error) string {
	e := ctlops.AsError(op, err)
	if e.Hint != "" {
		return e.Msg + " " + e.Hint
	}
	return e.Msg
}

// controlGitHub serves `ctl github …`.
//
// `link` and `install` are the whole command surface on purpose, and they are
// two halves of the same sentence: `link` proves which GitHub account this
// handle is, `install` puts the App on the repositories that account wants in
// its sandboxes. Unlinking is not offered because a stale link is not a hazard
// worth a verb — re-linking overwrites it, and the account it names is the one
// GitHub last vouched for — and showing the current one is `whoami`'s job,
// which prints it beside everything else about the account.
func (g *Gateway) controlGitHub(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	arg := ""
	if len(args) > 1 {
		arg = args[1]
	}
	switch verb {
	case "link", "":
		if g.linkGitHub(s, s, c, arg, log) == "" {
			// Nothing linked. Exit 1 so a script can tell, while the sentences
			// explaining why have already gone to the same stream a person is
			// reading.
			s.Exit(1) //nolint:errcheck
			return
		}
		s.Exit(0) //nolint:errcheck
	case "install":
		g.controlGitHubInstall(s, c, log)
	default:
		fmt.Fprintf(s.Stderr(), "unknown github command %q\r\n%s", verb, pageFor("account"))
		s.Exit(2) //nolint:errcheck
	}
}

// nudgeGitHub prints the one-line offer to an account that has no GitHub link.
//
// It goes wherever somebody is already being told something else — minting a
// session token, creating a sandbox — because the offer's whole problem was
// that it lived at a door nobody walks through twice. One line, on the stream
// that is not carrying the credential, and only when there is genuinely nothing
// linked: an account that has already answered this question must never be
// asked again.
func (g *Gateway) nudgeGitHub(s gssh.Session, w io.Writer, c ctlops.Caller) {
	me, err := g.ops.Whoami(s.Context(), c)
	if err != nil || me.GitHubLogin != "" {
		return
	}
	fmt.Fprintf(w, "# tip: link your GitHub account —  ssh %s@%s github link\r\n",
		ControlUser, g.sshHint())
}
