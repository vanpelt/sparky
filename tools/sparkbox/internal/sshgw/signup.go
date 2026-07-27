package sshgw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// handleSignup serves the `signup@` door: a short interactive dialog that
// turns the connecting key into an account.
//
// This works over SSH alone — the only door users already have — because the
// gateway holds the client's public key before any session opens. So there is
// nothing to paste and no browser: the key you connect with is the key you
// register. Invite codes gate *who* may join (authorization); the key remains
// the sole credential (authentication).
func (g *Gateway) handleSignup(s gssh.Session, user string, log *slog.Logger) {
	key := sessionKey(s)
	if key == nil {
		fail(s, log, "signup", errors.New("no public key on this session"))
		return
	}
	fp := xssh.FingerprintSHA256(key)

	// Already registered: say so rather than leaving them to wonder why the
	// dialog is asking again. This is the common case of a re-run.
	if user != "" {
		fmt.Fprintf(s, "sparkbox: this key is already registered to %q.\r\n", user)
		fmt.Fprintf(s, "try:  ssh %s@%s\r\n", NewSandboxUser, g.domainHint())
		s.Exit(0) //nolint:errcheck
		return
	}

	// The dialog needs a terminal to echo and edit input. Without a PTY (`ssh
	// signup@host < /dev/null`, or a script) there is nothing sensible to do.
	if _, _, isPty := s.Pty(); !isPty {
		fmt.Fprintf(s.Stderr(), "sparkbox: signup is interactive — connect from a terminal, with no command or piped stdin:  ssh %s@%s\r\n", SignupUser, g.domainHint())
		s.Exit(2) //nolint:errcheck
		return
	}
	t := term.NewTerminal(s, "")
	fmt.Fprintf(t, "sparkbox: this key isn't registered yet (%s).\r\n", fp)

	// The invite is redeemed BEFORE the account is created: the database's
	// atomic update is what settles a race between two people racing the same
	// code. If anything downstream fails, the code goes back (release).
	invitedBy := users.OperatorInviter
	code := ""
	if !g.openSignup {
		var err error
		code, invitedBy, err = g.askInvite(t)
		if err != nil {
			signupAbort(s, t, log, err)
			return
		}
	}
	release := func() {
		if code == "" {
			return
		}
		if err := g.users.ReleaseInvite(code); err != nil {
			log.Error("could not release invite after abandoned signup", "err", err)
		}
	}

	handle, err := g.askHandle(t)
	if err != nil {
		release()
		signupAbort(s, t, log, err)
		return
	}
	if err := g.users.Create(handle, key, keyComment(s), "signup", invitedBy); err != nil {
		release()
		signupAbort(s, t, log, err)
		return
	}
	if code != "" {
		// Now that the handle exists, record who actually spent the code. The
		// reservation above left it blank.
		if err := g.users.AttributeInvite(code, handle); err != nil {
			log.Warn("could not attribute invite", "handle", handle, "err", err)
		}
	}
	log.Info("user registered", "handle", handle, "fp", fp, "invited_by", invitedBy)

	// GitHub linking and email are optional and never block registration: the
	// account exists from here on regardless of how these go.
	//
	// From here the new account acts for itself. Everything below goes through
	// ctlops as the user, which is what makes signup's GitHub step the same
	// dialog, the same provenance and the same audit line as `ctl github link` —
	// they were two implementations of one ceremony, and only one of them was
	// ever kept current.
	me := ctlops.Caller{Handle: handle, KeyFP: fp}
	emailPrefetch := g.askGitHub(s, t, me, log)
	g.askEmail(s.Context(), t, handle, emailPrefetch, log)

	fmt.Fprintf(t, "registered as %q. try:  ssh %s@%s\r\n", handle, NewSandboxUser, g.domainHint())
	s.Exit(0) //nolint:errcheck
}

// askInvite reads and redeems an invite code, returning the code (so it can be
// released if the rest of signup fails) and who issued it.
func (g *Gateway) askInvite(t *term.Terminal) (code, invitedBy string, err error) {
	t.SetPrompt("invite code: ")
	for attempt := 0; attempt < 3; attempt++ {
		line, err := t.ReadLine()
		if err != nil {
			return "", "", err
		}
		entered := strings.TrimSpace(line)
		// used_by is stamped once a handle exists (AttributeInvite); redeeming
		// here is the reservation that closes the race.
		by, err := g.users.RedeemInvite(entered, "")
		if err == nil {
			return entered, by, nil
		}
		fmt.Fprint(t, "that code isn't valid, or has already been used.\r\n")
	}
	return "", "", errors.New("no valid invite code")
}

// askHandle reads a handle until it's valid and unclaimed.
func (g *Gateway) askHandle(t *term.Terminal) (string, error) {
	t.SetPrompt("handle: ")
	for attempt := 0; attempt < 5; attempt++ {
		line, err := t.ReadLine()
		if err != nil {
			return "", err
		}
		handle := strings.ToLower(strings.TrimSpace(line))
		switch {
		case handle == "":
			fmt.Fprint(t, "a handle is required.\r\n")
		case !users.ValidHandle(handle):
			fmt.Fprint(t, "handles are 2-32 characters of a-z, 0-9 and dashes; some names are reserved.\r\n")
		default:
			if _, err := g.users.Get(handle); err == nil {
				fmt.Fprintf(t, "%q is taken.\r\n", handle)
				continue
			}
			return handle, nil
		}
	}
	return "", errors.New("no valid handle")
}

// askGitHub offers to link a GitHub account, through the dialog every other
// door uses.
//
// On a verified link it returns a channel that will deliver the profile's public
// email for askEmail's prefill; nil when the link was skipped or didn't verify.
// The fetch is started here rather than awaited because the next prompt is the
// one that wants it: the round trip hides behind the sentence confirming the
// link instead of stalling in front of the email question.
func (g *Gateway) askGitHub(s gssh.Session, t *term.Terminal, c ctlops.Caller, log *slog.Logger) <-chan string {
	login := g.linkGitHub(s, t, c, "", log)
	if login == "" {
		return nil
	}
	// Buffered so the goroutine finishes even when the channel is dropped.
	emailCh := make(chan string, 1)
	go func() {
		em, _ := users.FetchGitHubEmail(s.Context(), login)
		emailCh <- em
	}()
	return emailCh
}

// askEmail offers to record a contact email. The dialog is the only reliable
// capture point: the SSH wire protocol never carries the key's comment, so
// there is no address to read off the connecting key. When GitHub was just
// linked and the profile shows a public email (delivered via prefetch, started
// back in askGitHub), that address becomes an accept-with-Enter default.
func (g *Gateway) askEmail(ctx context.Context, t *term.Terminal, handle string, prefetch <-chan string, log *slog.Logger) {
	prefill := ""
	if prefetch != nil {
		select {
		case prefill = <-prefetch:
		case <-ctx.Done():
		}
	}
	fmt.Fprint(t, "add a contact email? apps behind the proxy see it as X-Forwarded-Email\r\n")
	if prefill != "" {
		fmt.Fprintf(t, "(enter to use %s from your GitHub profile, or \"skip\")\r\n", prefill)
	} else {
		fmt.Fprint(t, "(or blank to skip)\r\n")
	}
	t.SetPrompt("email: ")
	for attempt := 0; attempt < 3; attempt++ {
		line, err := t.ReadLine()
		if err != nil {
			return
		}
		email := strings.TrimSpace(line)
		switch {
		case email == "" && prefill != "":
			email = prefill
		case email == "" || email == "skip":
			return
		}
		if !users.ValidEmail(email) {
			fmt.Fprint(t, "that doesn't look like an email address (or \"skip\").\r\n")
			continue
		}
		if err := g.users.SetEmail(handle, email); err != nil {
			log.Error("set email at signup", "handle", handle, "err", err)
			return
		}
		fmt.Fprintf(t, "✓ email set to %s — change it with:  ssh %s@%s email set <addr>\r\n",
			email, ControlUser, g.domainHint())
		return
	}
}

// keyComment labels the key for `keys list`. SSH doesn't carry the client's
// authorized_keys comment over the wire, so fall back to the client's version
// string, which at least tells one machine's software from another's.
func keyComment(s gssh.Session) string {
	v := strings.TrimPrefix(s.Context().ClientVersion(), "SSH-2.0-")
	if i := strings.IndexByte(v, ' '); i > 0 {
		v = v[:i]
	}
	return v
}

func signupAbort(s gssh.Session, w io.Writer, log *slog.Logger, err error) {
	log.Info("signup abandoned", "err", err)
	fmt.Fprintf(w, "sparkbox: signup cancelled: %v\r\n", err)
	s.Exit(1) //nolint:errcheck
}
