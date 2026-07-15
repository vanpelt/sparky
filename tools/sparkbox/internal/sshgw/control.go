package sshgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

const controlUsage = "usage: ssh ctl@<gateway> <command>\r\n" +
	"  list                     list your sandboxes and their state\r\n" +
	"  pause <name>             pause a running sandbox to free a slot\r\n" +
	"  whoami                   show your account and linked identities\r\n" +
	"  keys list                list the SSH keys on your account\r\n" +
	"  keys add \"<key line>\"    link another key\r\n" +
	"  keys rm <SHA256:...>     unlink a key (never the last one)\r\n" +
	"  keys import-github       adopt every key github.com lists for your login\r\n" +
	"  keys verify-github       re-check your GitHub link\r\n" +
	"  invite                   mint a single-use invite code\r\n"

// handleControl serves the `ctl@` out-of-band channel: managing sandboxes and
// your own account without dialing into a VM. It only ever touches the
// caller's own sandboxes and keys.
func (g *Gateway) handleControl(s gssh.Session, user string, log *slog.Logger) {
	args := s.Command()
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), controlUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	switch args[0] {
	case "list":
		n := 0
		for _, b := range g.mgr.List() {
			if b.Owner != user {
				continue
			}
			fmt.Fprintf(s, "%-24s %s\r\n", b.Name, b.State)
			n++
		}
		if n == 0 {
			fmt.Fprint(s, "no sandboxes yet — create one with: ssh new@<gateway>\r\n")
		}
		s.Exit(0) //nolint:errcheck
	case "pause":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), "usage: ssh ctl@<gateway> pause <name>\r\n")
			s.Exit(2) //nolint:errcheck
			return
		}
		name := args[1]
		box, ok := g.mgr.Get(name)
		if !ok || box.Owner != user {
			// Same message either way: don't leak other users' sandbox names.
			fmt.Fprintf(s.Stderr(), "sparkbox: no sandbox named %q\r\n", name)
			s.Exit(1) //nolint:errcheck
			return
		}
		ctx, cancel := context.WithTimeout(s.Context(), pauseTimeout)
		defer cancel()
		if err := g.mgr.Pause(ctx, name); err != nil {
			fail(s, log, "pause", err)
			return
		}
		fmt.Fprintf(s, "paused %s\r\n", name)
		s.Exit(0) //nolint:errcheck
	case "whoami":
		g.controlWhoami(s, user)
	case "keys":
		g.controlKeys(s, user, args[1:], log)
	case "invite":
		g.controlInvite(s, user, log)
	default:
		fmt.Fprintf(s.Stderr(), "unknown command %q\r\n%s", args[0], controlUsage)
		s.Exit(2) //nolint:errcheck
	}
}

func (g *Gateway) controlWhoami(s gssh.Session, user string) {
	u, err := g.users.Get(user)
	if err != nil {
		fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
		s.Exit(1) //nolint:errcheck
		return
	}
	fmt.Fprintf(s, "handle:  %s\r\n", u.Handle)
	fmt.Fprintf(s, "status:  %s\r\n", u.Status)
	if u.GitHubVerifiedAt != nil {
		fmt.Fprintf(s, "github:  %s (verified %s)\r\n", u.GitHubLogin, u.GitHubVerifiedAt.Format("2006-01-02"))
	} else {
		fmt.Fprintf(s, "github:  not linked — link it with: ssh %s@%s keys verify-github\r\n",
			ControlUser, g.domainHint())
	}
	fmt.Fprintf(s, "subject: %s\r\n", oidc.SubjectFor(user))
	fmt.Fprintf(s, "key:     %s\r\n", sessionKeyFP(s))
	s.Exit(0) //nolint:errcheck
}

func (g *Gateway) controlKeys(s gssh.Session, user string, args []string, log *slog.Logger) {
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), controlUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	switch args[0] {
	case "list":
		keys, err := g.users.Keys(user)
		if err != nil {
			fail(s, log, "keys list", err)
			return
		}
		thisFP := sessionKeyFP(s)
		for _, k := range keys {
			marker := " "
			if k.FP == thisFP {
				marker = "*" // the key you're connected with right now
			}
			fmt.Fprintf(s, "%s %s  %-16s %-14s %s\r\n",
				marker, k.FP, truncate(k.Label, 16), k.Via, k.AddedAt.Format("2006-01-02"))
		}
		s.Exit(0) //nolint:errcheck

	case "add":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), "usage: ssh ctl@<gateway> keys add \"ssh-ed25519 AAAA... label\"\r\n")
			s.Exit(2) //nolint:errcheck
			return
		}
		key, comment, _, _, err := xssh.ParseAuthorizedKey([]byte(strings.Join(args[1:], " ")))
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: that isn't a valid authorized_keys line: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		if err := g.users.AddKey(user, key, comment, "ctl"); err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		log.Info("key added", "user", user, "fp", xssh.FingerprintSHA256(key))
		fmt.Fprintf(s, "added %s\r\n", xssh.FingerprintSHA256(key))
		s.Exit(0) //nolint:errcheck

	case "rm":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), "usage: ssh ctl@<gateway> keys rm <SHA256:...>\r\n")
			s.Exit(2) //nolint:errcheck
			return
		}
		if err := g.users.RemoveKey(user, args[1]); err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		log.Info("key removed", "user", user, "fp", args[1])
		fmt.Fprintf(s, "removed %s\r\n", args[1])
		s.Exit(0) //nolint:errcheck

	case "import-github":
		u, err := g.users.Get(user)
		if err != nil {
			fail(s, log, "keys import-github", err)
			return
		}
		if u.GitHubLogin == "" {
			fmt.Fprintf(s.Stderr(), "sparkbox: no GitHub account linked — link one with: ssh %s@%s keys verify-github\r\n",
				ControlUser, g.domainHint())
			s.Exit(1) //nolint:errcheck
			return
		}
		keys, err := users.FetchGitHubKeys(s.Context(), u.GitHubLogin)
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		added := 0
		for _, k := range keys {
			switch err := g.users.AddKey(user, k, "github:"+u.GitHubLogin, "github-import"); {
			case err == nil:
				added++
			case errors.Is(err, users.ErrKeyLinked):
				fmt.Fprintf(s.Stderr(), "skipped %s (linked to another account)\r\n", xssh.FingerprintSHA256(k))
			default:
				fail(s, log, "keys import-github", err)
				return
			}
		}
		// AddKey is a no-op for keys already on the account, so `added` counts
		// only genuinely new ones and re-running this is free.
		fmt.Fprintf(s, "imported %d new key(s) from github.com/%s (%d listed)\r\n", added, u.GitHubLogin, len(keys))
		s.Exit(0) //nolint:errcheck

	case "verify-github":
		var login string
		if len(args) >= 2 {
			login = args[1]
		} else if u, err := g.users.Get(user); err == nil {
			login = u.GitHubLogin
		}
		if login == "" {
			fmt.Fprint(s.Stderr(), "usage: ssh ctl@<gateway> keys verify-github <login>\r\n")
			s.Exit(2) //nolint:errcheck
			return
		}
		ok, err := users.VerifyGitHubKey(s.Context(), login, sessionKey(s))
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		if !ok {
			fmt.Fprintf(s.Stderr(),
				"sparkbox: %s isn't listed on github.com/%s.keys — add it there, then retry.\r\n",
				sessionKeyFP(s), login)
			s.Exit(1) //nolint:errcheck
			return
		}
		if err := g.users.LinkGitHub(user, login); err != nil {
			fail(s, log, "keys verify-github", err)
			return
		}
		log.Info("github verified", "user", user, "login", login)
		fmt.Fprintf(s, "verified: %s is listed on github.com/%s\r\n", sessionKeyFP(s), login)
		s.Exit(0) //nolint:errcheck

	default:
		fmt.Fprintf(s.Stderr(), "unknown keys command %q\r\n%s", args[0], controlUsage)
		s.Exit(2) //nolint:errcheck
	}
}

// controlInvite mints a single-use invite. Operators (the users seeded from
// users.conf) may always invite; everyone else is capped by --invites-per-user,
// which defaults to 0 — operator-only.
func (g *Gateway) controlInvite(s gssh.Session, user string, log *slog.Logger) {
	u, err := g.users.Get(user)
	if err != nil {
		fail(s, log, "invite", err)
		return
	}
	if !u.IsOperator() {
		if g.invitesPerUser <= 0 {
			fmt.Fprint(s.Stderr(), "sparkbox: only operators can mint invite codes here.\r\n")
			s.Exit(1) //nolint:errcheck
			return
		}
		n, err := g.users.InviteCount(user)
		if err != nil {
			fail(s, log, "invite", err)
			return
		}
		if n >= g.invitesPerUser {
			fmt.Fprintf(s.Stderr(), "sparkbox: you've used all %d of your invites.\r\n", g.invitesPerUser)
			s.Exit(1) //nolint:errcheck
			return
		}
	}
	code, err := g.users.NewInvite(user)
	if err != nil {
		fail(s, log, "invite", err)
		return
	}
	log.Info("invite minted", "by", user)
	fmt.Fprintf(s, "invite code: %s   (single use, expires in %d days)\r\n",
		code, int(users.InviteTTL.Hours()/24))
	fmt.Fprintf(s, "they run:    ssh %s@%s\r\n", SignupUser, g.domainHint())
	s.Exit(0) //nolint:errcheck
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
