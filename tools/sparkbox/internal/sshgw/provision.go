package sshgw

// The operator `user` verbs: admitting people from GitHub instead of by invite
// code. See internal/ctlops/user.go for why importing published keys is a sound
// way in, and for the operator-privilege trap this door exists to avoid.
//
// The org roster needs a GitHub token, and it arrives the same way a secret
// value does — on stdin, never on argv:
//
//	gh auth token | ssh ctl@<gateway> user sync-github-org wandb
//
// Same reasoning as internal/sshgw/secret.go, and more force behind it here:
// this one is a read:org credential for the operator's whole organization.

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	gssh "github.com/gliderlabs/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// maxTokenRead caps the token read. GitHub's are ~40-100 bytes; anything near
// this is not a token.
const maxTokenRead = 4 << 10

const userUsage = "usage: ssh ctl@<gateway> user ls\r\n" +
	"       ssh ctl@<gateway> user add <github-login>… [--dry-run]\r\n" +
	"       gh auth token | ssh ctl@<gateway> user sync-github-org <org> [--team <slug>] [--dry-run]\r\n" +
	"\r\n" +
	"`add` needs no token: github.com publishes an account's ssh keys. `sync-github-org`\r\n" +
	"reads the roster with YOUR token (read:org), on stdin, and never stores it.\r\n" +
	"\r\n" +
	"accounts are created with the keys github.com publishes for each login, so the\r\n" +
	"person signs in with `ssh new@<gateway>` and has nothing to type. they are\r\n" +
	"ordinary users, never operators.\r\n"

func (g *Gateway) controlUser(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), userUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	switch args[0] {
	case "ls", "list":
		list, err := g.ops.ListAccounts(c)
		if err != nil {
			failCtl(s, log, "user ls", err)
			return
		}
		for _, u := range list {
			gh := u.GitHubLogin
			if gh == "" {
				gh = "-"
			}
			role := "user"
			if u.IsOperator() {
				role = "operator"
			}
			fmt.Fprintf(s, "%-20s %-9s %-8s %-20s %s\r\n",
				u.Handle, u.Status, role, truncate(gh, 20), u.CreatedAt.Format("2006-01-02"))
		}
		fmt.Fprintf(s, "%d account(s)\r\n", len(list))
		s.Exit(0) //nolint:errcheck

	case "add":
		logins, dryRun := splitDryRun(args[1:])
		if len(logins) == 0 {
			fmt.Fprint(s.Stderr(), userUsage)
			s.Exit(2) //nolint:errcheck
			return
		}
		res, err := g.ops.ProvisionGitHubUsers(s.Context(), c, logins, dryRun)
		if err != nil {
			failCtl(s, log, "user add", err)
			return
		}
		g.printProvision(s, res)

	case "sync-github-org", "sync-org":
		rest, team, dryRun, err := parseSyncArgs(args[1:])
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n%s", err, userUsage)
			s.Exit(2) //nolint:errcheck
			return
		}
		if len(rest) != 1 {
			fmt.Fprint(s.Stderr(), userUsage)
			s.Exit(2) //nolint:errcheck
			return
		}
		token, err := readToken(s)
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(2) //nolint:errcheck
			return
		}
		res, err := g.ops.ProvisionGitHubOrg(s.Context(), c, rest[0], team, token, dryRun)
		if err != nil {
			failCtl(s, log, "user sync-github-org", err)
			return
		}
		g.printProvision(s, res)

	default:
		fmt.Fprintf(s.Stderr(), "unknown user command %q\r\n%s", args[0], userUsage)
		s.Exit(2) //nolint:errcheck
	}
}

// printProvision renders a run as one line per login plus a summary.
//
// Every examined login gets a line, including the ones nothing happened to. A
// sync that silently omitted the skips would leave the operator believing the
// people missing from the output were done, when they are exactly the ones
// still needing an invite.
func (g *Gateway) printProvision(s gssh.Session, res ctlops.ProvisionResult) {
	for _, u := range res.Users {
		line := fmt.Sprintf("%-20s %-12s", u.Login, u.Outcome)
		if u.Handle != "" && u.Handle != u.Login {
			line += fmt.Sprintf(" as %s", u.Handle)
		}
		if u.Note != "" {
			line += "  " + u.Note
		}
		fmt.Fprint(s, line+"\r\n")
	}
	verb := "created"
	if res.DryRun {
		verb = "would create"
	}
	fmt.Fprintf(s, "%d examined, %s %d, updated %d, skipped %d\r\n",
		res.Examined, verb, res.Created, res.Updated, res.Skipped)
	if res.DryRun {
		fmt.Fprint(s, "(dry run — nothing was written; re-run without --dry-run to apply)\r\n")
		s.Exit(0) //nolint:errcheck
		return
	}
	if res.Created > 0 {
		// The whole point of the feature, stated so the operator knows what to
		// send: there is no code to deliver and nothing for the new user to do
		// first.
		fmt.Fprintf(s, "\r\nthey can connect now, with no invite code:\r\n"+
			"  ssh new@%s\r\n", g.sshHint())
	}
	s.Exit(0) //nolint:errcheck
}

// parseSyncArgs pulls --team and --dry-run out, leaving the positional args.
//
// --team exists because a company org is not a guest list: the one this was
// built for has 617 members and a couple of dozen who want a sandbox. A team is
// the group that actually asked.
func parseSyncArgs(args []string) (rest []string, team string, dryRun bool, err error) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--dry-run" || a == "-n":
			dryRun = true
		case a == "--team":
			if i+1 >= len(args) {
				return nil, "", false, fmt.Errorf("--team needs a slug")
			}
			i++
			team = args[i]
		case strings.HasPrefix(a, "--team="):
			team = strings.TrimPrefix(a, "--team=")
		default:
			rest = append(rest, a)
		}
	}
	return rest, team, dryRun, nil
}

// splitDryRun pulls --dry-run out of an argument list wherever it appears.
func splitDryRun(args []string) (rest []string, dryRun bool) {
	for _, a := range args {
		if a == "--dry-run" || a == "-n" {
			dryRun = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, dryRun
}

// readToken reads the GitHub token from stdin.
//
// Unlike a secret value there is no interactive fallback: a token typed at a
// prompt is a token pasted from somewhere, and `gh auth token | ssh …` is
// already the shortest path for the only people who can run this. Refusing the
// PTY case keeps one way in rather than two.
func readToken(s gssh.Session) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(s, maxTokenRead+1))
	if err != nil {
		return "", fmt.Errorf("could not read the GitHub token from stdin: %v", err)
	}
	if len(raw) > maxTokenRead {
		return "", fmt.Errorf("that is too long to be a GitHub token")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("no GitHub token arrived on stdin — pipe one in:\r\n" +
			"       gh auth token | ssh ctl@<gateway> user sync-github-org <org>")
	}
	return token, nil
}
