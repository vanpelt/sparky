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
		fmt.Fprint(s.Stderr(), "sparkbox: signup is interactive — connect without a command:  ssh signup@<gateway>\r\n")
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

	// GitHub linking is optional and never blocks registration: the account
	// exists from here on regardless of how this goes.
	g.askGitHub(s.Context(), t, handle, key, fp, log)

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

// askGitHub offers to link a GitHub account by checking the connecting key
// against github.com/<login>.keys. Possession of a key GitHub publishes for an
// account proves control of it — the same evidence GitHub itself accepts for a
// git push — so this needs no OAuth app, browser, or client secret.
func (g *Gateway) askGitHub(ctx context.Context, t *term.Terminal, handle string, key gssh.PublicKey, fp string, log *slog.Logger) {
	fmt.Fprint(t, "link a GitHub account? enter your GitHub username to verify\r\n")
	fmt.Fprint(t, "this key against github.com/<user>.keys (or blank to skip)\r\n")
	t.SetPrompt("github: ")
	line, err := t.ReadLine()
	if err != nil {
		return
	}
	login := strings.TrimSpace(line)
	if login == "" {
		return
	}
	ok, err := users.VerifyGitHubKey(ctx, login, key)
	if err != nil {
		fmt.Fprintf(t, "couldn't check github (%v) — skipping; link later with:\r\n", err)
		fmt.Fprintf(t, "  ssh %s@%s keys verify-github %s\r\n", ControlUser, g.domainHint(), login)
		return
	}
	if !ok {
		fmt.Fprintf(t, "%s isn't listed on github.com/%s.keys — skipping the link.\r\n", fp, login)
		fmt.Fprintf(t, "add it there, then run:  ssh %s@%s keys verify-github %s\r\n",
			ControlUser, g.domainHint(), login)
		return
	}
	if err := g.users.LinkGitHub(handle, login); err != nil {
		log.Error("github link failed after verifying", "handle", handle, "login", login, "err", err)
		return
	}
	log.Info("github verified at signup", "handle", handle, "login", login)
	fmt.Fprintf(t, "✓ key %s is listed on github.com/%s — verified.\r\n", fp, login)
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
