package sshgw

import (
	"fmt"
	"log/slog"
	"time"

	gssh "github.com/gliderlabs/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

// controlEmail shows or sets the caller's email — the address the edge forwards
// to private apps as X-Forwarded-Email.
func (g *Gateway) controlEmail(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	if len(args) == 0 {
		addr, err := g.ops.Email(s.Context(), c)
		if err != nil {
			failCtl(s, log, "email", wrapVerbatim(err, ctlops.KindInternal))
			return
		}
		if addr == "" {
			fmt.Fprintf(s, "no email set — add one with: ssh %s@%s email set you@example.com\r\n",
				ControlUser, g.domainHint())
		} else {
			fmt.Fprintf(s, "%s\r\n", addr)
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
		if _, err := g.ops.SetEmail(s.Context(), c, args[1]); err != nil {
			failCtl(s, log, "email", err)
			return
		}
		fmt.Fprintf(s, "email set to %s\r\n", args[1])
		s.Exit(0) //nolint:errcheck
	case "clear":
		if _, err := g.ops.SetEmail(s.Context(), c, ""); err != nil {
			failCtl(s, log, "email clear", wrapVerbatim(err, ctlops.KindInternal))
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
func (g *Gateway) controlShare(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	// The host-not-configured answer comes first, before the usage line: a box
	// with no proxy cannot share anything however the command was typed.
	if !g.ops.Capabilities().Routes {
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

	// No visibility arg → report current state.
	if len(args) == 1 {
		rs, err := g.ops.Visibility(s.Context(), c, name)
		if err != nil {
			failCtl(s, log, "share", err)
			return
		}
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
	res, err := g.ops.SetVisibility(s.Context(), c, name, vis)
	if err != nil {
		failCtl(s, log, "share", err)
		return
	}
	if vis == routes.VisibilityPublic {
		fmt.Fprintf(s, "%s is now public — anyone with the URL can reach it (%d route(s)).\r\n", name, res.Changed)
	} else {
		fmt.Fprintf(s, "%s is now private — visitors must sign in and own it (%d route(s)).\r\n", name, res.Changed)
	}
	s.Exit(0) //nolint:errcheck
}

// controlPasskey lists or removes the caller's WebAuthn passkeys. Enrollment
// happens in the browser (the login page offers it after a token sign-in);
// this is the audit-and-revoke end, deliberately on the SSH channel so a lost
// or stolen browser credential can be killed from any machine with your key.
func (g *Gateway) controlPasskey(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	usage := func() {
		fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> passkey [list|rm <id>]\r\n", ControlUser)
		s.Exit(2) //nolint:errcheck
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		pks, err := g.ops.ListPasskeys(s.Context(), c)
		if err != nil {
			failCtl(s, log, "passkey list", err)
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
		if err := g.ops.RemovePasskey(s.Context(), c, args[1]); err != nil {
			failCtl(s, log, "passkey rm", err)
			return
		}
		fmt.Fprintf(s, "removed — that passkey can no longer sign in\r\n")
		s.Exit(0) //nolint:errcheck
	default:
		usage()
	}
}

// controlSessionToken mints an edge session token for the caller. The ctl
// channel has already proven possession of a registered key, so this is
// unconditional — it is exactly "a signed token you create with your SSH key".
func (g *Gateway) controlSessionToken(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	// Before the flag parsing, so a host that cannot mint says so rather than
	// complaining about a --ttl it was never going to honour.
	if !g.ops.Capabilities().SessionTokens {
		fmt.Fprint(s.Stderr(), "sparkbox: authenticated forwarding isn't enabled on this host.\r\n")
		s.Exit(1) //nolint:errcheck
		return
	}
	var ttl time.Duration // 0 takes ctlops' default; a bad value never gets here
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
	tok, err := g.ops.MintSessionToken(s.Context(), c, ttl)
	if err != nil {
		failCtl(s, log, "session-token", err)
		return
	}
	// The token alone goes to stdout; every explanatory line goes to stderr, so
	// a caller can capture the credential without filtering. The line ending is
	// CRLF because this is an SSH channel, which means `$(…)` strips the \n and
	// keeps the \r — every doc that shows this command pipes through
	// `tr -d '\r\n'`, and a bare CR in an Authorization header is rejected by
	// Go's own HTTP/1.1 parser before any handler sees it.
	fmt.Fprintf(s, "%s\r\n", tok.Token)
	fmt.Fprintf(s.Stderr(), "# valid until %s\r\n", tok.ExpiresAt.Local().Format("2006-01-02 15:04 MST"))
	fmt.Fprint(s.Stderr(), "# browser: paste it into the sign-in page for a private URL\r\n")
	// Name the REST API first: it is the door this token was most likely minted
	// for, and it is the one a reader cannot guess from the sign-in page they
	// just came from. The CRLF note is not padding — capturing this command in
	// `$(…)` leaves a bare CR that Go's HTTP/1.1 parser rejects, and the
	// resulting 400 says nothing about why.
	fmt.Fprint(s.Stderr(), "# api:     curl -H \"Authorization: Bearer <token>\" https://api.<domain>/v1/sandboxes\r\n")
	fmt.Fprint(s.Stderr(), "#          docs at https://api.<domain>/docs; the same header opens https://<name>.<domain>:<port>\r\n")
	fmt.Fprint(s.Stderr(), "# shell:   TOKEN=$(ssh ctl@<domain> session-token | tr -d '\\r\\n')   # this channel sends CRLF\r\n")
	// The offer, for an account that has never taken it. This is the moment
	// somebody is most likely to be standing at: minting a token is what you do
	// on the way to signing into the browser for the first time, which is also
	// where a linked GitHub account starts paying off. On stderr with the rest
	// of the commentary, so `$(…)` still captures nothing but the credential.
	g.nudgeGitHub(s, s.Stderr(), c)
	s.Exit(0) //nolint:errcheck
}
