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
// That command prints a banner around its token, so the pipe carries more than
// the value. cleanSecretValue picks the credential out by its own shape rather
// than by anything about the banner, and refuses when the answer is not unique.
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
	"regexp"
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
	"a banner printed around the value is fine: the credential is picked out of it\r\n" +
	"by its own shape, and anything ambiguous is refused rather than guessed at.\r\n" +
	"\r\n" +
	"  gh auth token | ssh ctl@<gateway> secret set GITHUB_TOKEN --tag ci\r\n" +
	"  claude setup-token | ssh ctl@<gateway> secret set CLAUDE_CODE_OAUTH_TOKEN\r\n"

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
				"  claude setup-token | ssh ctl@"+g.sshHint()+" secret set CLAUDE_CODE_OAUTH_TOKEN\r\n")
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

// cleanSecretValue reduces what arrived to the one value that can be an env
// var, or explains why it cannot.
//
// The trailing newline is the ordinary case: every CLI that prints a token ends
// with one, and storing it would trip the store's own newline check — which
// reads to the user as "my token is invalid" rather than "your pipe added a
// byte". Blank lines and terminal styling around the value are the same mistake
// with more bytes, so they come off too.
//
// What is left may still be several lines, because the tool that printed the
// credential also printed a banner around it — which is exactly what `claude
// setup-token` does, and it is the command this whole channel exists to be the
// far end of. Those are resolved by looking for the credential ITSELF (see
// credentialShapes), never by learning the shape of somebody's banner. If that
// finds exactly one thing, it is the value; anything else is refused, because a
// wrong credential stored silently is worse than a pipe that did not work.
func cleanSecretValue(v, name string) (string, error) {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(v, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(ansiEscape.ReplaceAllString(l, "")); l != "" {
			lines = append(lines, l)
		}
	}
	switch len(lines) {
	case 0:
		return "", fmt.Errorf("no value for %s arrived on stdin — pipe one in, "+
			"or use `ssh -t` to be prompted", name)
	case 1:
		// One line of content is unambiguous whatever it looks like, and it has
		// to stay that way: most secrets are not issued by anyone who stamps a
		// recognisable prefix on them. A connection string, a webhook URL and a
		// password are all values this must keep taking verbatim.
		return lines[0], nil
	}

	found, split := credentialsIn(lines)
	switch {
	case split:
		return "", fmt.Errorf(
			"that output has a credential broken across two lines, and storing either half "+
				"would store the wrong value.%s", pasteInstead(name))
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return "", fmt.Errorf(
			"that output holds %d different credentials, and only one of them can be %s — "+
				"this refuses to guess which.%s", len(found), name, pasteInstead(name))
	default:
		return "", fmt.Errorf(
			"that is %d lines of output and none of them holds anything shaped like a "+
				"credential, so this cannot tell which one is %s.%s", len(lines), name, pasteInstead(name))
	}
}

// pasteInstead is the way through, shared by every refusal above so the three
// cannot drift into three different pieces of advice for one situation. The
// prompt is not just the fallback, it is also the only route that keeps a
// pasted credential out of the shell's history.
func pasteInstead(name string) string {
	return "\r\n       paste it at a prompt instead (it is not echoed, and stays out of your " +
		"history):\r\n         ssh -t ctl@<gateway> secret set " + name
}

// ansiEscape matches the CSI sequences a CLI writes around its output. Styling
// is presentation and never part of a credential, and a value carrying an ESC
// could not survive /etc/environment anyway, so it comes off before anything
// else looks at the text.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// credentialShapes recognise a token by its OWN structure — the prefix its
// issuer stamps on it and the alphabet it draws from — and never by where it
// sits in some tool's output.
//
// That distinction is the whole of why this is allowed to exist. Picking "the
// line after the one saying Your token" or "the fifth line" would be reading
// another program's presentation, which changes on its release schedule with no
// signal to us and whose next version this would silently store the wrong half
// of. `sk-ant-oat01-` is part of the credential. It changes only when the
// credential format changes, and on that day the old shape stops matching
// anything and the pipe is REFUSED rather than mis-stored: this fails closed.
//
// Adding one is two lines, and needs one property — the prefix must be specific
// enough that its presence alone identifies a secret. The length floors are
// what stop prose that merely mentions a prefix from matching.
var credentialShapes = []*regexp.Regexp{
	// Anthropic: `claude setup-token` mints oat, the console issues api.
	regexp.MustCompile(`sk-ant-(?:oat|api)[0-9]{2}-[A-Za-z0-9_-]{24,}`),
	// GitHub: the fine-grained PAT, then the classic/OAuth/app/refresh family
	// `gh auth token` prints.
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`),
	// OpenAI project keys, for a sandbox running codex.
	regexp.MustCompile(`sk-proj-[A-Za-z0-9_-]{24,}`),
}

// credentialsIn returns the distinct credential-shaped values in lines, and
// whether one of them appears to have been broken by a line wrap.
//
// Distinct is what makes the count meaningful: a tool that prints its token
// twice (once in a sentence, once on its own line) has printed one credential,
// and refusing that as "two" would be refusing the ordinary case.
func credentialsIn(lines []string) (found []string, split bool) {
	seen := map[string]bool{}
	for i, l := range lines {
		for _, shape := range credentialShapes {
			for _, at := range shape.FindAllStringIndex(l, -1) {
				// A match that runs to the end of its line, followed by a line
				// that is nothing but more credential characters, is one token
				// a terminal broke in two. Detected, never rejoined — a guess
				// about where a value continues is still a guess, and the whole
				// point here is that a plausible-looking wrong credential is the
				// one outcome worse than a pipe that refused.
				if at[1] == len(l) && i+1 < len(lines) && isCredentialRun(lines[i+1]) {
					split = true
				}
				if v := l[at[0]:at[1]]; !seen[v] {
					seen[v] = true
					found = append(found, v)
				}
			}
		}
	}
	return found, split
}

// isCredentialRun reports whether a line could be the tail of a token a
// terminal wrapped: nothing but credential characters, long enough not to be a
// short word, and mixing letters with digits.
//
// The mix is what keeps the two things that actually sit under a token line out
// of it. A closing sentence is words and punctuation; a rule or a line of
// ASCII-art logo is dashes and underscores. A wrapped tail of a base62 token is
// neither, and eight characters of it carry both a letter and a digit with
// overwhelming likelihood.
func isCredentialRun(l string) bool {
	if len(l) < 8 {
		return false
	}
	var letter, digit bool
	for _, r := range l {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letter = true
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return letter && digit
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
	fmt.Fprintf(w, "# tip: sign your agents in once —  claude setup-token | ssh %s@%s secret set CLAUDE_CODE_OAUTH_TOKEN\r\n",
		ControlUser, g.sshHint())
}
