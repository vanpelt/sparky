package sshgw

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	gssh "github.com/gliderlabs/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// sessionTokenMaxTTL caps how long a minted edge session stays valid. Longer
// than the console cookie because re-minting means an ssh round-trip, but short
// enough that a leaked token self-heals within a week.
const sessionTokenMaxTTL = 7 * 24 * time.Hour

// controlEmail shows or sets the caller's email — the address the edge forwards
// to private apps as X-Forwarded-Email.
func (g *Gateway) controlEmail(s gssh.Session, user string, args []string, log *slog.Logger) {
	if len(args) == 0 {
		u, err := g.users.Get(user)
		if err != nil {
			fail(s, log, "email", err)
			return
		}
		if u.Email == "" {
			fmt.Fprintf(s, "no email set — add one with: ssh %s@%s email set you@example.com\r\n",
				ControlUser, g.domainHint())
		} else {
			fmt.Fprintf(s, "%s\r\n", u.Email)
		}
		s.Exit(0) //nolint:errcheck
		return
	}
	switch args[0] {
	case "set":
		if len(args) < 2 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> email set you@example.com\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		if err := g.users.SetEmail(user, args[1]); err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		log.Info("email set", "user", user)
		fmt.Fprintf(s, "email set to %s\r\n", args[1])
		s.Exit(0) //nolint:errcheck
	case "clear":
		if err := g.users.SetEmail(user, ""); err != nil {
			fail(s, log, "email clear", err)
			return
		}
		fmt.Fprint(s, "email cleared\r\n")
		s.Exit(0) //nolint:errcheck
	default:
		fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> email [set <addr>|clear]\r\n", ControlUser)
		s.Exit(2) //nolint:errcheck
	}
}

// controlShare shows or sets a sandbox's web-route visibility. Visibility is
// per-sandbox (every route pointing at it flips together), matching the "who
// can reach this VM" mental model. Owner-only.
func (g *Gateway) controlShare(s gssh.Session, user string, args []string, log *slog.Logger) {
	if g.routes == nil {
		fmt.Fprint(s.Stderr(), "sparkbox: the HTTP proxy isn't enabled on this host.\r\n")
		s.Exit(1) //nolint:errcheck
		return
	}
	if len(args) == 0 {
		fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> share <name> [public|private]\r\n", ControlUser)
		s.Exit(2) //nolint:errcheck
		return
	}
	name := args[0]
	box, ok := g.mgr.Get(name)
	if !ok || box.Owner != user {
		fmt.Fprintf(s.Stderr(), "sparkbox: no sandbox named %q\r\n", name)
		s.Exit(1) //nolint:errcheck
		return
	}
	rs, err := g.routes.ListBySandbox(name)
	if err != nil {
		fail(s, log, "share", err)
		return
	}

	// No visibility arg → report current state.
	if len(args) == 1 {
		if len(rs) == 0 {
			fmt.Fprintf(s, "%s has no web routes yet.\r\n", name)
			s.Exit(0) //nolint:errcheck
			return
		}
		for _, r := range rs {
			fmt.Fprintf(s, "%-8s https://%s.%s  → :%d\r\n", r.Visibility, r.Subdomain, g.domainHint(), r.Port)
		}
		s.Exit(0) //nolint:errcheck
		return
	}

	vis := args[1]
	if !routes.ValidVisibility(vis) {
		fmt.Fprintf(s.Stderr(), "sparkbox: visibility must be 'public' or 'private', not %q\r\n", vis)
		s.Exit(2) //nolint:errcheck
		return
	}
	n := 0
	for _, r := range rs {
		if err := g.routes.SetVisibility(r.Subdomain, vis); err != nil {
			fail(s, log, "share", err)
			return
		}
		n++
	}
	log.Info("route visibility changed", "user", user, "sandbox", name, "visibility", vis, "routes", n)
	if vis == routes.VisibilityPublic {
		fmt.Fprintf(s, "%s is now public — anyone with the URL can reach it (%d route(s)).\r\n", name, n)
	} else {
		fmt.Fprintf(s, "%s is now private — visitors must sign in and own it (%d route(s)).\r\n", name, n)
	}
	s.Exit(0) //nolint:errcheck
}

// controlPasskey lists or removes the caller's WebAuthn passkeys. Enrollment
// happens in the browser (the login page offers it after a token sign-in);
// this is the audit-and-revoke end, deliberately on the SSH channel so a lost
// or stolen browser credential can be killed from any machine with your key.
func (g *Gateway) controlPasskey(s gssh.Session, user string, args []string, log *slog.Logger) {
	usage := func() {
		fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> passkey [list|rm <id>]\r\n", ControlUser)
		s.Exit(2) //nolint:errcheck
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		pks, err := g.users.Passkeys(user)
		if err != nil {
			fail(s, log, "passkey list", err)
			return
		}
		if len(pks) == 0 {
			fmt.Fprintf(s, "no passkeys — enroll one by signing in at https://login.%s\r\n", g.domainHint())
			s.Exit(0) //nolint:errcheck
			return
		}
		for _, p := range pks {
			lastUsed := "never used"
			if p.LastUsedAt != nil {
				lastUsed = "last used " + p.LastUsedAt.Format("2006-01-02")
			}
			label := p.Label
			if label == "" {
				label = "-"
			}
			fmt.Fprintf(s, "%-24s %-16s added %s  %s\r\n",
				truncate(p.ID, 24), truncate(label, 16), p.CreatedAt.Format("2006-01-02"), lastUsed)
		}
		s.Exit(0) //nolint:errcheck
	case "rm", "remove":
		if len(args) < 2 {
			usage()
			return
		}
		switch err := g.users.RemovePasskey(user, args[1]); {
		case errors.Is(err, users.ErrNoSuchPasskey):
			fmt.Fprintf(s.Stderr(), "sparkbox: no passkey matches %q — see `passkey list`\r\n", args[1])
			s.Exit(1) //nolint:errcheck
		case errors.Is(err, users.ErrAmbiguousPasskey):
			fmt.Fprintf(s.Stderr(), "sparkbox: %q matches more than one passkey — use more of the id\r\n", args[1])
			s.Exit(1) //nolint:errcheck
		case err != nil:
			fail(s, log, "passkey rm", err)
		default:
			log.Info("passkey removed", "user", user, "id", args[1])
			fmt.Fprintf(s, "removed — that passkey can no longer sign in\r\n")
			s.Exit(0) //nolint:errcheck
		}
	default:
		usage()
	}
}

// controlSessionToken mints an edge session token for the caller. The ctl
// channel has already proven possession of a registered key, so this is
// unconditional — it is exactly "a signed token you create with your SSH key".
func (g *Gateway) controlSessionToken(s gssh.Session, user string, args []string, log *slog.Logger) {
	if g.session == nil {
		fmt.Fprint(s.Stderr(), "sparkbox: authenticated forwarding isn't enabled on this host.\r\n")
		s.Exit(1) //nolint:errcheck
		return
	}
	ttl := 12 * time.Hour
	for i := 0; i < len(args); i++ {
		if args[i] == "--ttl" && i+1 < len(args) {
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d <= 0 {
				fmt.Fprintf(s.Stderr(), "sparkbox: bad --ttl %q (try 1h, 30m, 168h)\r\n", args[i+1])
				s.Exit(2) //nolint:errcheck
				return
			}
			ttl = d
			i++
		}
	}
	if ttl > sessionTokenMaxTTL {
		ttl = sessionTokenMaxTTL
	}
	u, err := g.users.Get(user)
	if err != nil {
		fail(s, log, "session-token", err)
		return
	}
	tok, exp, err := g.session.Mint(edgeauth.Identity{Handle: user, Email: u.Email}, ttl)
	if err != nil {
		fail(s, log, "session-token", err)
		return
	}
	log.Info("session token minted", "user", user, "exp", exp)
	fmt.Fprintf(s, "%s\r\n", tok)
	fmt.Fprintf(s.Stderr(), "# valid until %s\r\n", exp.Local().Format("2006-01-02 15:04 MST"))
	fmt.Fprint(s.Stderr(), "# browser: paste it into the sign-in page for a private URL\r\n")
	fmt.Fprint(s.Stderr(), "# api:     curl -H \"Authorization: Bearer <token>\" https://<name>.<domain>:<port>\r\n")
	s.Exit(0) //nolint:errcheck
}
