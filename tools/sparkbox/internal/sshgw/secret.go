package sshgw

// The `secret` verbs on the ctl channel.
//
// The point of putting these here at all is that the value is usually already
// in the shell the user is standing in — `claude setup-token` just printed it,
// `gh auth token` will print it — and the previous shortest path from there to
// a sandbox went through a browser. A pipe is the whole feature:
//
//	claude setup-token | ssh ctl@<gateway> secret set CLAUDE_CODE_OAUTH_TOKEN
//
// # Why the value is never an argument
//
// `secret set NAME VALUE` would be the obvious grammar and it is the wrong one.
// The value would land in the user's shell history, in the argv of their local
// ssh process (world-readable in `ps` on a shared machine), and in any client
// or bastion that logs commands. None of those are places a long-lived
// credential should come to rest, and all of them outlive the command. Stdin
// has none of those properties, so stdin is the only way in — a value on argv
// is refused with a sentence explaining the pipe rather than quietly accepted.

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/term"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
)

// maxSecretRead caps what this channel will read for one value. The store's own
// limit is 4 KiB; reading a little more lets an over-long value be reported as
// too long rather than silently truncated to exactly the limit.
const maxSecretRead = 8 << 10

const secretUsage = "usage: ssh ctl@<gateway> secret ls\r\n" +
	"       <command printing the value> | ssh ctl@<gateway> secret set <NAME> [--tag <t>]…\r\n" +
	"       ssh ctl@<gateway> secret rm <NAME>\r\n" +
	"\r\n" +
	"the value is read from stdin, never from the command line, so it stays out\r\n" +
	"of your shell history. with a terminal (ssh -t) you are prompted instead.\r\n" +
	"\r\n" +
	"  gh auth token | ssh ctl@<gateway> secret set GITHUB_TOKEN --tag ci\r\n" +
	"  ssh -t ctl@<gateway> secret set CLAUDE_CODE_OAUTH_TOKEN   (paste; setup-token\r\n" +
	"                                                             prints a banner too)\r\n"

func (g *Gateway) controlSecret(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), secretUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	switch args[0] {
	case "ls", "list":
		metas, err := g.ops.ListSecrets(c)
		if err != nil {
			failCtl(s, log, "secret ls", err)
			return
		}
		for _, m := range metas {
			fmt.Fprintf(s, "%-32s %-24s v%-3d %s\r\n",
				m.Name, truncate(strings.Join(m.Tags, ","), 24), m.Version, m.UpdatedAt.Format("2006-01-02"))
		}
		if len(metas) == 0 {
			fmt.Fprint(s, "no secrets yet — add one with:\r\n"+
				"  ssh -t ctl@"+g.sshHint()+" secret set CLAUDE_CODE_OAUTH_TOKEN\r\n")
		}
		s.Exit(0) //nolint:errcheck

	case "set":
		g.secretSet(s, c, args[1:], log)

	case "rm", "delete":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), secretUsage)
			s.Exit(2) //nolint:errcheck
			return
		}
		affected, err := g.ops.DeleteSecret(s.Context(), c, args[1])
		if err != nil {
			failCtl(s, log, "secret rm", err)
			return
		}
		fmt.Fprintf(s, "removed %s%s\r\n", args[1], resyncNote(affected))
		s.Exit(0) //nolint:errcheck

	default:
		fmt.Fprintf(s.Stderr(), "unknown secret command %q\r\n%s", args[0], secretUsage)
		s.Exit(2) //nolint:errcheck
	}
}

func (g *Gateway) secretSet(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	name, tags, err := parseSecretSet(args)
	if err != nil {
		fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n%s", err, secretUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	value, err := readSecretValue(s, name)
	if err != nil {
		fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
		s.Exit(2) //nolint:errcheck
		return
	}
	res, err := g.ops.PutSecret(s.Context(), c, name, value, tags)
	if err != nil {
		failCtl(s, log, "secret set", err)
		return
	}
	fmt.Fprintf(s, "set %s  (tags: %s)%s\r\n", res.Name, strings.Join(res.Tags, ","), resyncNote(res.Resynced))
	if len(res.Resynced) == 0 {
		// The one mistake this feature invites, caught at the only moment the
		// user is still looking: a secret reaches a sandbox only if they share
		// a tag, so a write that touched nothing is worth a sentence rather
		// than a silent success the user discovers inside a broken sandbox.
		fmt.Fprintf(s, "note: no sandbox of yours carries %s, so nothing has it yet.\r\n"+
			"      new sandboxes get the `%s` tag automatically; retag an existing one with:\r\n"+
			"      ssh ctl@%s tags <name> %s\r\n",
			strings.Join(res.Tags, " or "), secrets.DefaultTag, g.sshHint(), strings.Join(res.Tags, " "))
	}
	s.Exit(0) //nolint:errcheck
}

// parseSecretSet reads `<NAME> [--tag t]… [--tags a,b]…`.
//
// A bare second word is rejected rather than read as the value. That is the
// grammar somebody will try first, and accepting it would put the credential
// in their shell history — see this file's header. Refusing loudly teaches the
// pipe; accepting quietly teaches nothing and leaks.
func parseSecretSet(args []string) (name string, tags []string, err error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("name the environment variable to set")
	}
	name = args[0]
	for i := 1; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--tag" || a == "--tags":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s needs a value", a)
			}
			i++
			tags = append(tags, args[i])
		case strings.HasPrefix(a, "--tag="), strings.HasPrefix(a, "--tags="):
			tags = append(tags, a[strings.Index(a, "=")+1:])
		default:
			return "", nil, fmt.Errorf(
				"the value is read from stdin, not from the command line — pipe it in instead:\r\n"+
					"       <command printing it> | ssh ctl@<gateway> secret set %s", name)
		}
	}
	return name, tags, nil
}

// readSecretValue takes the value from a terminal prompt when there is one and
// from stdin when there is not.
//
// The PTY split is what makes both `ssh -t ctl@host secret set X` (type it,
// unechoed) and `cmd | ssh ctl@host secret set X` (pipe it) work with no flag
// to choose between them: a pipe never allocates a PTY, and `-t` always does.
func readSecretValue(s gssh.Session, name string) (string, error) {
	if _, _, isPty := s.Pty(); isPty {
		t := term.NewTerminal(s, "")
		v, err := t.ReadPassword(name + ": ")
		if err != nil {
			return "", fmt.Errorf("no value entered")
		}
		return cleanSecretValue(v, name)
	}
	// LimitReader is +1 so an oversize value is detectable rather than arriving
	// pre-truncated to something that would be stored as a plausible-looking
	// but wrong credential.
	raw, err := io.ReadAll(io.LimitReader(s, maxSecretRead+1))
	if err != nil {
		return "", fmt.Errorf("could not read the value from stdin: %v", err)
	}
	if len(raw) > maxSecretRead {
		return "", fmt.Errorf("that value is too long to be an environment variable")
	}
	return cleanSecretValue(string(raw), name)
}

// cleanSecretValue reduces what arrived to the one line that can be an env var,
// or explains why it cannot.
//
// The trailing newline is the ordinary case: every CLI that prints a token ends
// with one, and storing it would trip the store's own newline check — which
// reads to the user as "my token is invalid" rather than "your pipe added a
// byte". Blank lines around the value are the same mistake with more whitespace,
// so they are dropped too.
//
// What is NOT guessed at is which of several candidate lines is the secret.
// `claude setup-token` prints a banner, an ASCII-art logo, the token, and two
// sentences of advice; picking the token out of that would mean pattern-matching
// on another tool's output and silently storing the wrong line the day it
// changes. So more than one line of content is refused, and the refusal names
// the way through — a prompt, which is also the only way that keeps a pasted
// credential out of the shell's history.
func cleanSecretValue(v, name string) (string, error) {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(v, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	switch len(lines) {
	case 1:
		return lines[0], nil
	case 0:
		return "", fmt.Errorf("no value for %s arrived on stdin — pipe one in, "+
			"or use `ssh -t` to be prompted", name)
	default:
		return "", fmt.Errorf(
			"that is %d lines of output, and only one of them can be %s — "+
				"this refuses to guess which.\r\n"+
				"       paste it at a prompt instead (it will not be echoed, and stays out of your history):\r\n"+
				"         ssh -t ctl@<gateway> secret set %s",
			len(lines), name, name)
	}
}

// resyncNote renders the fan-out as a clause, or nothing when there was none.
func resyncNote(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return " — pushed to " + names[0]
	default:
		return fmt.Sprintf(" — pushed to %d sandboxes: %s", len(names), strings.Join(names, ", "))
	}
}

// nudgeAgentToken prints the one-line offer to an account with no secrets at
// all, beside the create banner.
//
// Same shape and the same reasoning as nudgeGitHub next door: a first sandbox
// is the moment this is worth something, and an account that has answered the
// question must never be asked again. The specific failure it heads off is the
// one every new user hits — an agent CLI in a fresh box that asks them to log
// in, with nothing anywhere connecting that to a secret they never set.
//
// It fires on having no secrets whatsoever rather than on a particular variable
// being absent: somebody using an API key instead of an OAuth token, or Codex
// instead of Claude, has answered this question, and naming one variable would
// keep nagging them forever.
func (g *Gateway) nudgeAgentToken(s gssh.Session, w io.Writer, c ctlops.Caller) {
	metas, err := g.ops.ListSecrets(c)
	if err != nil || len(metas) > 0 {
		return
	}
	fmt.Fprintf(w, "# tip: sign your agents in once —  ssh -t %s@%s secret set CLAUDE_CODE_OAUTH_TOKEN\r\n",
		ControlUser, g.sshHint())
}
